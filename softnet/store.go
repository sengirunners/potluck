package softnet

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
)

// Dir is where a Mac's managed Softnet lives. Each build has its own
// directory, Dir/<id>, holding the executable (softnet, root owned and
// setuid), the upstream LICENSE and provenance.json; Dir/current links to
// the build in use and Dir/previous to the one it replaced.
const Dir = "/Library/Application Support/Potluck/Softnet"

// File names inside a build's directory, and the links in Dir.
const (
	binaryName     = "softnet"
	licenseName    = "LICENSE"
	provenanceName = "provenance.json"
	currentLink    = "current"
	previousLink   = "previous"
	lockName       = ".lock"
	stagingPrefix  = ".staging-"
	oldPrefix      = ".old-"
)

// Store is a managed install directory and who must own it.
type Store struct {
	Dir string
	// Owner and Group own every file and directory in it: root and wheel
	// (0, 0) on a Mac.
	Owner, Group int
	// Top is the highest directory whose owner and mode are checked: "/" on
	// a Mac. Every directory from Top down to Dir must be owned by Owner or
	// root and writable by no one else, so no other user can swap a file
	// root runs.
	Top string
}

// System is the Mac's managed install.
func System() Store { return Store{Dir: Dir, Owner: 0, Group: 0, Top: "/"} }

// Current is the path of the managed executable in use: the one Potluck
// pins for Tart when it exists.
func (s Store) Current() string { return filepath.Join(s.Dir, currentLink, binaryName) }

// Build is the path of installed build id's executable.
func (s Store) Build(id string) string { return filepath.Join(s.Dir, id, binaryName) }

// Provenance is what provenance.json records beside an installed build.
type Provenance struct {
	ID            string    `json:"id"`
	Version       string    `json:"version"`
	SHA256        string    `json:"sha256"`
	Bytes         int64     `json:"bytes"`
	Arch          string    `json:"arch"`
	Source        Source    `json:"source"`
	License       string    `json:"license"`
	LicenseSHA256 string    `json:"license_sha256"`
	Installed     time.Time `json:"installed"`
}

// Installed is one build in the store, as Status reads it.
type Installed struct {
	ID   string `json:"id"`
	Path string `json:"path"`
	// Pinned is true when this potluck pins the build (Builds) and the
	// file has its pinned SHA-256.
	Pinned bool `json:"pinned"`
	// Problem is why the build cannot be used ("" when it can).
	Problem string `json:"problem,omitempty"`
}

// Status is the store's state: the build in use and the one before it
// (nil when there is none).
type Status struct {
	Dir      string     `json:"dir"`
	Current  *Installed `json:"current,omitempty"`
	Previous *Installed `json:"previous,omitempty"`
}

// Status reads the store without changing it; any user can.
func (s Store) Status() Status {
	st := Status{Dir: s.Dir}
	for _, l := range []struct {
		name string
		into **Installed
	}{{currentLink, &st.Current}, {previousLink, &st.Previous}} {
		id, err := s.link(l.name)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		in := &Installed{ID: id, Path: filepath.Join(s.Dir, id, binaryName)}
		if err != nil {
			in.Problem = err.Error()
		} else if _, err := s.verify(id); err != nil {
			in.Problem = err.Error()
		} else {
			in.Pinned = true
		}
		*l.into = in
	}
	return st
}

// CheckCurrent verifies the build in use, as Status does, and returns its
// ID. A boot runs it before it starts the managed Softnet, so a build
// withdrawn or changed since preflight is never started.
func (s Store) CheckCurrent() (string, error) {
	id, err := s.link(currentLink)
	if err != nil {
		return "", err
	}
	if _, err := s.verify(id); err != nil {
		return id, err
	}
	return id, nil
}

// Result is what Install or Rollback did.
type Result struct {
	ID       string // the build now in use
	Previous string // the build it replaced ("" when none)
	// Unchanged is true when ID was in use already and sound.
	Unchanged bool
}

// Check runs a staged or installed executable before it is put in use
// (its version, the rules it must accept): an error stops the change and
// leaves the build in use as it was.
type Check func(path string, b Build) error

// Install puts build b in use from the file at src, which must have b's
// SHA-256 and size. It must run as Owner (root on a Mac). It is
// idempotent: b already in use and sound changes nothing; b installed
// before but not in use is checked and put back in use. The build it
// replaces becomes previous, for Rollback. Nothing outside Dir changes.
func (s Store) Install(src string, b Build, check Check) (Result, error) {
	if err := b.valid(); err != nil {
		return Result{}, err
	}
	if err := s.prepare(); err != nil {
		return Result{}, err
	}
	unlock, err := s.lock()
	if err != nil {
		return Result{}, err
	}
	defer unlock()
	s.sweep()
	cur, _ := s.link(currentLink)
	target := filepath.Join(s.Dir, b.ID)
	if _, err := s.verify(b.ID); err == nil {
		if cur == b.ID {
			return Result{ID: b.ID, Unchanged: true}, nil
		}
		if err := check(filepath.Join(target, binaryName), b); err != nil {
			return Result{}, fmt.Errorf("installed Softnet %s fails its check: %w", b.ID, err)
		}
		return s.activate(b.ID, cur)
	}
	staging, err := s.stage(src, b)
	if staging != "" {
		defer s.remove(staging)
	}
	if err != nil {
		return Result{}, err
	}
	if err := check(filepath.Join(staging, binaryName), b); err != nil {
		return Result{}, fmt.Errorf("Softnet %s fails its check on this Mac, so it was not put in use: %w", b.ID, err)
	}
	// A copy under this name that did not verify (changed, or left half
	// made) is set aside, and the sweep removes it.
	if _, err := os.Lstat(target); err == nil {
		if err := os.Rename(target, filepath.Join(s.Dir, oldPrefix+b.ID+"-"+randomSuffix())); err != nil {
			return Result{}, fmt.Errorf("cannot set aside the unsound %s: %w", target, err)
		}
	}
	if err := os.Rename(staging, target); err != nil {
		return Result{}, fmt.Errorf("cannot place %s: %w", target, err)
	}
	syncDir(s.Dir)
	if _, err := s.verify(b.ID); err != nil {
		return Result{}, fmt.Errorf("placed Softnet %s does not verify: %w", b.ID, err)
	}
	r, err := s.activate(b.ID, cur)
	s.sweep()
	return r, err
}

// Rollback puts the previous build back in use, after checking it as
// Install would, and makes the build it replaces previous.
func (s Store) Rollback(check Check) (Result, error) {
	if err := s.prepare(); err != nil {
		return Result{}, err
	}
	unlock, err := s.lock()
	if err != nil {
		return Result{}, err
	}
	defer unlock()
	prev, err := s.link(previousLink)
	if errors.Is(err, fs.ErrNotExist) {
		return Result{}, errors.New("there is no earlier Softnet to roll back to")
	} else if err != nil {
		return Result{}, err
	}
	if _, err := s.verify(prev); err != nil {
		return Result{}, fmt.Errorf("cannot roll back to Softnet %s: %w", prev, err)
	}
	b, _ := ByID(prev) // verify found it
	if err := check(filepath.Join(s.Dir, prev, binaryName), b); err != nil {
		return Result{}, fmt.Errorf("cannot roll back to Softnet %s: it fails its check: %w", prev, err)
	}
	cur, _ := s.link(currentLink)
	return s.activate(prev, cur)
}

// activate points current at id and previous at cur (when cur is another
// build), each link replaced in one rename. The link that names the build
// to keep moves first, so a stop between the two renames leaves both links
// naming builds that are still wanted: on install previous (the old
// build) moves first, on rollback (id is previous) current does.
func (s Store) activate(id, cur string) (Result, error) {
	setPrev := func() error {
		if cur == "" || cur == id {
			return nil
		}
		return s.setLink(previousLink, cur)
	}
	setCur := func() error { return s.setLink(currentLink, id) }
	steps := []func() error{setPrev, setCur}
	if prev, err := s.link(previousLink); err == nil && prev == id {
		steps = []func() error{setCur, setPrev}
	}
	for _, step := range steps {
		if err := step(); err != nil {
			return Result{}, err
		}
	}
	if prev, err := s.link(previousLink); err == nil && prev == id {
		os.Remove(filepath.Join(s.Dir, previousLink))
	}
	syncDir(s.Dir)
	r := Result{ID: id}
	if cur != id {
		r.Previous = cur
	}
	return r, nil
}

// stage copies src into a new private directory in Dir, hashing what it
// copies (never src again, which its owner could change), and adds the
// license and provenance. It returns the directory, ready to rename.
func (s Store) stage(src string, b Build) (string, error) {
	dir, err := os.MkdirTemp(s.Dir, stagingPrefix)
	if err != nil {
		return "", fmt.Errorf("cannot make a staging directory in %s: %w", s.Dir, err)
	}
	// Non-blocking, so a FIFO or device put at src cannot hold root and the
	// lock; then only a regular file is read.
	if fi, err := os.Stat(src); err != nil || !fi.Mode().IsRegular() {
		return dir, fmt.Errorf("%s is not a regular file", src)
	}
	in, err := os.OpenFile(src, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return dir, fmt.Errorf("cannot read %s: %w", src, err)
	}
	defer in.Close()
	if fi, err := in.Stat(); err != nil || !fi.Mode().IsRegular() {
		return dir, fmt.Errorf("%s is not a regular file", src)
	}
	bin := filepath.Join(dir, binaryName)
	out, err := os.OpenFile(bin, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o700)
	if err != nil {
		return dir, err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(out, h), io.LimitReader(in, b.Bytes+1))
	if err == nil {
		err = out.Sync()
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return dir, fmt.Errorf("cannot copy %s: %w", src, err)
	}
	// The error names no size or hash of what was read: this runs as root
	// and src is the caller's choice.
	if got := hex.EncodeToString(h.Sum(nil)); n != b.Bytes || got != b.SHA256 {
		return dir, fmt.Errorf("%s is not Softnet %s: the pinned build has %d bytes and SHA-256 %s", src, b.ID, b.Bytes, b.SHA256)
	}
	prov, _ := json.MarshalIndent(Provenance{ID: b.ID, Version: b.Version, SHA256: b.SHA256, Bytes: b.Bytes, Arch: b.Arch,
		Source: b.Source, License: "FSL-1.1-ALv2 (upstream Softnet, OpenAI; see LICENSE)", LicenseSHA256: b.LicenseSHA256,
		Installed: time.Now().UTC().Truncate(time.Second)}, "", "  ")
	for _, f := range []struct {
		name string
		data []byte
	}{{licenseName, []byte(License)}, {provenanceName, append(prov, '\n')}} {
		if err := writeNew(filepath.Join(dir, f.name), f.data); err != nil {
			return dir, err
		}
	}
	// Owner first: a change of owner clears the setuid bit.
	for _, f := range []struct {
		name string
		mode fs.FileMode
	}{{binaryName, 0o755 | fs.ModeSetuid}, {licenseName, 0o444}, {provenanceName, 0o444}} {
		p := filepath.Join(dir, f.name)
		if err := os.Lchown(p, s.Owner, s.Group); err != nil {
			return dir, fmt.Errorf("cannot make %s owned by uid %d: %w", p, s.Owner, err)
		}
		if err := os.Chmod(p, f.mode); err != nil {
			return dir, err
		}
	}
	if err := os.Lchown(dir, s.Owner, s.Group); err != nil {
		return dir, err
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		return dir, err
	}
	syncDir(dir)
	return dir, nil
}

// verify checks the installed build id: its directory, executable and
// provenance owned by Owner and writable by no one else, the executable
// setuid, and the executable the build this potluck pins under id.
func (s Store) verify(id string) (Provenance, error) {
	b, ok := ByID(id)
	if !ok {
		return Provenance{}, fmt.Errorf("Softnet %s is not a build this potluck pins", id)
	}
	if b.Withdrawn != "" {
		return Provenance{}, fmt.Errorf("Softnet %s is withdrawn: %s", id, b.Withdrawn)
	}
	dir := filepath.Join(s.Dir, id)
	if err := s.owned(dir, true); err != nil {
		return Provenance{}, err
	}
	bin := filepath.Join(dir, binaryName)
	if err := s.owned(bin, false); err != nil {
		return Provenance{}, err
	}
	if fi, err := os.Lstat(bin); err != nil {
		return Provenance{}, err
	} else if fi.Mode()&fs.ModeSetuid == 0 {
		return Provenance{}, fmt.Errorf("%s is not setuid", bin)
	}
	for _, name := range []string{provenanceName, licenseName} {
		if err := s.owned(filepath.Join(dir, name), false); err != nil {
			return Provenance{}, err
		}
	}
	if sum, _, err := sha256File(filepath.Join(dir, licenseName)); err != nil {
		return Provenance{}, err
	} else if sum != b.LicenseSHA256 {
		return Provenance{}, fmt.Errorf("%s is not the upstream license (SHA-256 %s)", filepath.Join(dir, licenseName), sum)
	}
	var p Provenance
	data, err := os.ReadFile(filepath.Join(dir, provenanceName))
	if err == nil {
		err = json.Unmarshal(data, &p)
	}
	if err != nil {
		return Provenance{}, fmt.Errorf("cannot read %s: %w", filepath.Join(dir, provenanceName), err)
	}
	sum, n, err := sha256File(bin)
	if err != nil {
		return Provenance{}, err
	}
	if sum != b.SHA256 || n != b.Bytes || p.SHA256 != b.SHA256 || p.ID != id {
		return Provenance{}, fmt.Errorf("%s has SHA-256 %s, not Softnet %s's %s", bin, sum, id, b.SHA256)
	}
	return p, nil
}

// owned checks that path (a directory when dir, else a regular file, never
// a link) is owned by Owner and not writable by group or others.
func (s Store) owned(path string, dir bool) error {
	fi, err := os.Lstat(path)
	if err != nil {
		return err
	}
	switch {
	case dir && !fi.IsDir(), !dir && !fi.Mode().IsRegular():
		return fmt.Errorf("%s is not a plain %s", path, map[bool]string{true: "directory", false: "file"}[dir])
	case uidOf(fi) != s.Owner:
		return fmt.Errorf("%s is owned by uid %d, not %d", path, uidOf(fi), s.Owner)
	case fi.Mode().Perm()&0o022 != 0:
		return fmt.Errorf("%s is writable by group or others (%s)", path, fi.Mode().Perm())
	}
	return aclSafe(path)
}

// prepare checks the caller and every directory from Top to Dir, making
// the missing ones (owned by Owner, mode 0755).
func (s Store) prepare() error {
	if os.Geteuid() != s.Owner {
		return fmt.Errorf("changing %s needs uid %d (root); this runs as uid %d", s.Dir, s.Owner, os.Geteuid())
	}
	if !filepath.IsAbs(s.Dir) || !filepath.IsAbs(s.Top) {
		return errors.New("the store's directories must be absolute")
	}
	rel, err := filepath.Rel(s.Top, s.Dir)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return fmt.Errorf("%s is not below %s", s.Dir, s.Top)
	}
	p := s.Top
	for _, part := range append([]string{""}, strings.Split(rel, string(filepath.Separator))...) {
		p = filepath.Join(p, part)
		if part != "" {
			if err := os.Mkdir(p, 0o755); err == nil {
				if err := os.Lchown(p, s.Owner, s.Group); err != nil {
					return err
				}
				if err := os.Chmod(p, 0o755); err != nil {
					return err
				}
			} else if !errors.Is(err, fs.ErrExist) {
				return fmt.Errorf("cannot make %s: %w", p, err)
			}
		}
		fi, err := os.Lstat(p)
		if err != nil {
			return err
		}
		switch {
		case !fi.IsDir():
			return fmt.Errorf("%s is not a directory (a link is refused: every directory on the way to %s must be a real one)", p, s.Dir)
		case uidOf(fi) != s.Owner && uidOf(fi) != 0:
			return fmt.Errorf("%s is owned by uid %d: every directory on the way to %s must be owned by root, so no other user can swap the setuid file", p, uidOf(fi), s.Dir)
		case fi.Mode().Perm()&0o022 != 0:
			return fmt.Errorf("%s is writable by group or others (%s): every directory on the way to %s must be writable by root alone", p, fi.Mode().Perm(), s.Dir)
		}
		if err := aclSafe(p); err != nil {
			return err
		}
	}
	return nil
}

// lock holds the store's lock until the returned func runs.
func (s Store) lock() (func(), error) {
	f, err := os.OpenFile(filepath.Join(s.Dir, lockName), os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, fmt.Errorf("cannot open the lock in %s: %w", s.Dir, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	// An Uninstall that ran while this waited removed the lock file: this
	// lock then guards nothing, and another install may hold a new one.
	held, err1 := f.Stat()
	now, err2 := os.Lstat(filepath.Join(s.Dir, lockName))
	if err1 != nil || err2 != nil || !os.SameFile(held, now) {
		f.Close()
		return nil, fmt.Errorf("%s changed while this waited for its lock (removed by an uninstall?); run it again", s.Dir)
	}
	return func() { f.Close() }, nil
}

// sweep removes what a stopped Install left (staging directories, copies
// set aside) and every build that is neither current nor a sound previous
// one, so a withdrawn or replaced setuid file does not stay on the Mac.
// Only under the lock.
func (s Store) sweep() {
	cur, _ := s.link(currentLink)
	prev, err := s.link(previousLink)
	if err == nil && prev != cur {
		if _, err := s.verify(prev); err != nil {
			prev = ""
		}
	} else {
		prev = ""
	}
	if prev == "" {
		if _, err := os.Lstat(filepath.Join(s.Dir, previousLink)); err == nil {
			os.Remove(filepath.Join(s.Dir, previousLink))
		}
	}
	es, _ := os.ReadDir(s.Dir)
	for _, e := range es {
		name := e.Name()
		switch {
		case name == lockName || name == currentLink || name == previousLink:
			continue
		case strings.HasPrefix(name, stagingPrefix) || strings.HasPrefix(name, oldPrefix):
		case name == cur || name == prev:
			continue
		}
		s.remove(filepath.Join(s.Dir, name))
	}
}

// remove disarms and deletes a build directory (or a staging one, or a
// stray file). Its executable loses every mode bit first: another user
// may hold a hard link to it, which would otherwise stay a setuid root
// file after the directory is gone.
func (s Store) remove(path string) {
	if fi, err := os.Lstat(path); err == nil && fi.IsDir() {
		bin := filepath.Join(path, binaryName)
		if fi, err := os.Lstat(bin); err == nil && fi.Mode().IsRegular() {
			os.Chmod(bin, 0)
		}
	}
	os.RemoveAll(path)
}

// parent is the folder Dir is in when the store made it ("": Top, or above).
func (s Store) parent() string {
	p := filepath.Dir(s.Dir)
	if rel, err := filepath.Rel(s.Top, p); s.Top == "" || err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return ""
	}
	return p
}

// EmptyParent is the folder the store was in, when it is there with nothing in
// it ("": not so). Only root can remove it.
func (s Store) EmptyParent() string {
	p := s.parent()
	if p == "" {
		return ""
	}
	if fi, err := os.Lstat(p); err != nil || !fi.IsDir() || uidOf(fi) != s.Owner {
		return ""
	}
	if es, err := os.ReadDir(p); err != nil || len(es) > 0 {
		return ""
	}
	return p
}

// Uninstall disarms every build and removes the store, and the folder it was
// in when nothing else is there. A box already
// running keeps its Softnet process; the next one uses the softnet on
// PATH, if any.
func (s Store) Uninstall() error {
	if err := s.prepare(); err != nil {
		return err
	}
	unlock, err := s.lock()
	if err != nil {
		return err
	}
	defer unlock()
	es, err := os.ReadDir(s.Dir)
	if err != nil {
		return err
	}
	for _, e := range es {
		if e.Name() != lockName {
			s.remove(filepath.Join(s.Dir, e.Name()))
		}
	}
	if err := os.RemoveAll(s.Dir); err != nil {
		return fmt.Errorf("cannot remove %s: %w", s.Dir, err)
	}
	if p := s.parent(); p != "" {
		_ = syscall.Rmdir(p) // not empty: it stays
	}
	return nil
}

// idRE is what a build ID and a link's target may be: one plain name.
var idRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,127}$`)

// link reads Dir/name, which must hold a build ID.
func (s Store) link(name string) (string, error) {
	t, err := os.Readlink(filepath.Join(s.Dir, name))
	if err != nil {
		return "", err
	}
	if !idRE.MatchString(t) || strings.Contains(t, "..") {
		return "", fmt.Errorf("%s points at %q, not a build in %s", filepath.Join(s.Dir, name), t, s.Dir)
	}
	return t, nil
}

// setLink points Dir/name at id in one rename.
func (s Store) setLink(name, id string) error {
	tmp := filepath.Join(s.Dir, stagingPrefix+name+"-"+randomSuffix())
	if err := os.Symlink(id, tmp); err != nil {
		return err
	}
	if err := os.Lchown(tmp, s.Owner, s.Group); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, filepath.Join(s.Dir, name)); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

func (b Build) valid() error {
	switch {
	case !idRE.MatchString(b.ID) || strings.Contains(b.ID, ".."):
		return fmt.Errorf("Softnet build ID %q is not a plain name", b.ID)
	case len(b.SHA256) != 64 || b.Bytes <= 0:
		return fmt.Errorf("Softnet %s has no pinned SHA-256 and size", b.ID)
	case b.Archive != nil && (len(b.Archive.SHA256) != 64 || b.Archive.Bytes <= 0 || b.Archive.Member == ""):
		return fmt.Errorf("Softnet %s has no pinned archive SHA-256, size and member", b.ID)
	case b.LicenseSHA256 != licenseSHA256:
		return fmt.Errorf("Softnet %s names license SHA-256 %s; the license this potluck carries has %s", b.ID, b.LicenseSHA256, licenseSHA256)
	}
	p, ok := ByID(b.ID)
	if !ok || p.SHA256 != b.SHA256 || p.Bytes != b.Bytes {
		return fmt.Errorf("Softnet %s is not a build this potluck pins", b.ID)
	}
	if p.Withdrawn != "" {
		return fmt.Errorf("Softnet %s is withdrawn: %s", b.ID, p.Withdrawn)
	}
	return nil
}

// sha256File is a regular file's SHA-256 and size, never through a link.
func sha256File(path string) (string, int64, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// FileSHA256 is a file's SHA-256 and size (a link is refused).
func FileSHA256(path string) (string, int64, error) { return sha256File(path) }

func writeNew(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

func syncDir(dir string) {
	if f, err := os.Open(dir); err == nil {
		f.Sync()
		f.Close()
	}
}

func randomSuffix() string {
	b := make([]byte, 6)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func uidOf(fi fs.FileInfo) int {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return int(st.Uid)
	}
	return -1
}
