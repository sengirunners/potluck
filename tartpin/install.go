package tartpin

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/sengirunners/potluck/kit/atomicfile"
)

// AppName is the bundle a Tart release unpacks to.
const AppName = "tart.app"

// ProgramInApp is the Tart program inside the bundle.
var ProgramInApp = filepath.Join("Contents", "MacOS", "tart")

// Place is where an install puts tart.app and keeps its record of having
// done so.
type Place struct {
	// AppsDir holds tart.app: the user's own Applications folder.
	AppsDir string
	// RecordFile is the installs' record (Record), outside the bundle,
	// whose code signature covers every file in it.
	RecordFile string
}

// UserPlace is this user's: ~/Applications, and the record in Potluck's
// folder of the user's config directory (on a Mac ~/Library/Application
// Support/potluck/tart-install.json).
func UserPlace() (Place, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Place{}, err
	}
	cfg, err := os.UserConfigDir()
	if err != nil {
		return Place{}, err
	}
	return Place{AppsDir: filepath.Join(home, "Applications"), RecordFile: filepath.Join(cfg, "potluck", "tart-install.json")}, nil
}

// App is the installed bundle's path.
func (p Place) App() string { return filepath.Join(p.AppsDir, AppName) }

// Program is the installed Tart program's path.
func (p Place) Program() string { return filepath.Join(p.App(), ProgramInApp) }

// Record is one bundle an install of Potluck's made.
type Record struct {
	Version string `json:"version"`
	// Inode is the bundle folder's: a rename keeps it, and a copy dragged
	// in has another, even of the same release.
	Inode uint64 `json:"inode"`
	// Tree is the SHA-256 of everything in the bundle (treeSHA256).
	Tree string `json:"tree"`
}

// recordFile is RecordFile's contents: the bundles Potluck made for Path.
// It is written before a bundle moves there, beside the one it replaces,
// so whichever is there after a crash is known.
type recordFile struct {
	Path    string   `json:"path"`
	Bundles []Record `json:"bundles"`
	// UsedElsewhere is MarkUsedElsewhere's fact.
	UsedElsewhere bool `json:"used_elsewhere,omitempty"`
}

func (p Place) read() (f recordFile) {
	data, err := os.ReadFile(p.RecordFile)
	if err != nil || json.Unmarshal(data, &f) != nil || f.Path != p.App() {
		return recordFile{}
	}
	return f
}

func (p Place) records() []Record { return p.read().Bundles }

// UsedElsewhere reports MarkUsedElsewhere's fact.
func (p Place) UsedElsewhere() bool { return p.read().UsedElsewhere }

// MarkUsedElsewhere notes in the installs' record, when there is one, that
// this Tart was used in a directory other than its usual one. The record
// outlives what showed it (a home, the update job).
func (p Place) MarkUsedElsewhere() error {
	if !lexists(p.RecordFile) {
		return nil
	}
	unlock, err := p.lock()
	if err != nil {
		return err
	}
	defer unlock()
	f := p.read()
	if len(f.Bundles) == 0 || f.UsedElsewhere {
		return nil
	}
	f.UsedElsewhere = true
	return p.write(f)
}

// made is the record of the bundle at app, when it is one Potluck made and
// nothing in it has changed since.
func (p Place) made(app string) (Record, bool) {
	recs := p.records()
	if len(recs) == 0 {
		return Record{}, false
	}
	ino, tree, err := identify(app)
	if err != nil {
		return Record{}, false
	}
	for _, r := range recs {
		if r.Inode == ino && r.Tree == tree {
			return r, true
		}
	}
	return Record{}, false
}

// Installed is Potluck's bundle at App(), when one is there unchanged.
// Anything else at App() (a lender's own copy, one changed since, a link)
// is not Potluck's to replace.
func (p Place) Installed() (Record, bool) { return p.made(p.App()) }

// identify is the bundle folder app's inode and treeSHA256; a link or a
// file is an error.
func identify(app string) (uint64, string, error) {
	fi, err := os.Lstat(app)
	if err != nil {
		return 0, "", err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !fi.IsDir() || !ok {
		return 0, "", fmt.Errorf("%s is not a folder", app)
	}
	tree, err := treeSHA256(app)
	return uint64(st.Ino), tree, err
}

// treeSHA256 is the SHA-256 over every name in dir with its kind, execute
// bit and contents. Anything but plain files and folders is an error.
func treeSHA256(dir string) (string, error) {
	h := sha256.New()
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		fi, err := d.Info()
		switch {
		case err != nil:
			return err
		case fi.IsDir():
			fmt.Fprintf(h, "dir %q\n", rel)
		case fi.Mode().IsRegular():
			sum, _, err := fileSHA256(path)
			if err != nil {
				return err
			}
			fmt.Fprintf(h, "file %q %v %s\n", rel, fi.Mode()&0o111 != 0, sum)
		default:
			return fmt.Errorf("%s is not a plain file or folder", path)
		}
		return nil
	})
	return hex.EncodeToString(h.Sum(nil)), err
}

// Check is nil when the file at path is b's tart.tar.gz: its size first,
// then its SHA-256.
func Check(path string, b Build) error {
	st, err := os.Stat(path)
	if err != nil {
		return err
	}
	if st.Size() != b.Bytes {
		return fmt.Errorf("the download is %d bytes, not the %d bytes of Tart %s's tart.tar.gz. Nothing was installed", st.Size(), b.Bytes, b.Version)
	}
	sum, _, err := fileSHA256(path)
	if err != nil {
		return err
	}
	if sum != b.SHA256 {
		return fmt.Errorf("the download has SHA-256 %s, not the %s of Tart %s's tart.tar.gz. Nothing was installed", sum, b.SHA256, b.Version)
	}
	return nil
}

// Fetch downloads b's tart.tar.gz into path (a new file; at most one byte
// more than b.Bytes is read) and checks it (Check). On any failure the file
// is removed.
func Fetch(ctx context.Context, c *http.Client, b Build, path string) (err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.URL, nil)
	if err != nil {
		return err
	}
	resp, err := c.Do(req)
	if err != nil {
		return fmt.Errorf("cannot download Tart %s: %w", b.Version, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("cannot download Tart %s: %s answered %s", b.Version, b.URL, resp.Status)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			os.Remove(path)
		}
	}()
	_, err = io.Copy(f, io.LimitReader(resp.Body, b.Bytes+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("cannot download Tart %s: %w", b.Version, err)
	}
	return Check(path, b)
}

// ErrNotOurs is returned by Install when App() holds a tart.app that
// Potluck did not install (Installed is false): it is left as it is.
var ErrNotOurs = errors.New("a tart.app Potluck did not install is there")

// Action is what an install of a build does at App().
type Action int

const (
	// Fresh: nothing is at App(); the build is moved there.
	Fresh Action = iota
	// Replace: Potluck's copy of an older (or withdrawn) release is there;
	// the build is swapped in.
	Replace
	// Keep: Potluck's copy of this release, or of a newer one that is not
	// withdrawn, is there; nothing changes. An older potluck never
	// downgrades the Tart a newer one installed.
	Keep
)

// Plan is what Install of a build would do.
type Plan struct {
	Action Action
	// Installed is Potluck's copy at App() (Replace and Keep).
	Installed Record
}

// Plan is what Install of b would do, or ErrNotOurs when a tart.app that
// Potluck did not install is at App(). The one home of that decision: the
// command's dry run and Install both read it.
func (p Place) Plan(b Build) (Plan, error) {
	rec, ours := p.Installed()
	switch {
	case !ours && lexists(p.App()):
		return Plan{}, fmt.Errorf("%s: %w", p.App(), ErrNotOurs)
	case !ours:
		return Plan{Action: Fresh}, nil
	case rec.Version == b.Version:
		return Plan{Action: Keep, Installed: rec}, nil
	}
	if old, pinned := ByVersion(rec.Version); pinned && old.Withdrawn != "" {
		return Plan{Action: Replace, Installed: rec}, nil
	}
	if c, ok := compareVersions(rec.Version, b.Version); !ok || c >= 0 {
		// Newer than b, or a version this potluck cannot order: kept.
		return Plan{Action: Keep, Installed: rec}, nil
	}
	return Plan{Action: Replace, Installed: rec}, nil
}

// compareVersions orders two dotted numeric versions ("2.42.0"): -1, 0 or
// 1, and ok false when either is not one.
func compareVersions(a, b string) (int, bool) {
	pa, oka := versionParts(a)
	pb, okb := versionParts(b)
	if !oka || !okb {
		return 0, false
	}
	for i := 0; i < max(len(pa), len(pb)); i++ {
		var x, y int
		if i < len(pa) {
			x = pa[i]
		}
		if i < len(pb) {
			y = pb[i]
		}
		if x != y {
			if x < y {
				return -1, true
			}
			return 1, true
		}
	}
	return 0, true
}

func versionParts(v string) ([]int, bool) {
	var out []int
	for _, f := range strings.Split(v, ".") {
		n, err := strconv.Atoi(f)
		if err != nil || n < 0 {
			return nil, false
		}
		out = append(out, n)
	}
	return out, len(out) > 0
}

// Result is what Install did.
type Result struct {
	Path     string // the installed bundle
	Version  string // the release now at Path
	Replaced string // the version of Potluck's copy it replaced, "" for none
	Already  bool   // Plan was Keep: nothing changed
}

// The hidden folders Install works in, beside App(). One with stagePrefix
// only ever holds what unpack wrote, so any old one is swept; it is renamed
// to swapPrefix before a swap puts the replaced bundle in it, and one of
// those is swept only when it holds a bundle Potluck made, or none.
const (
	stagePrefix = ".tart.app.potluck-"
	swapPrefix  = ".tart.app.swap-"
)

// staleStage is how old a work folder must be before it is taken for one an
// interrupted install left behind.
const staleStage = time.Hour

// Install puts b's tart.app from the tart.tar.gz at tarball into App(), as
// Plan says. It checks the tarball (Check) before unpacking it into a hidden
// folder beside App() (plain files and folders under tart.app only), checks
// the program's SHA-256, records the new bundle, and moves it into place in
// one rename: one that fails if anything appeared at App() meanwhile, or a
// swap with Potluck's own older bundle, checked after the swap and swapped
// back if what came out is not that bundle. A tart.app that Potluck did not
// make is never replaced (ErrNotOurs). On an error nothing at App() has
// changed, except when the lender changes it during a swap: then the error
// says where files that were there now are, if not put back.
func (p Place) Install(tarball string, b Build) (Result, error) {
	res := Result{Path: p.App(), Version: b.Version}
	if b.Withdrawn != "" {
		return res, fmt.Errorf("Tart %s is withdrawn: %s", b.Version, b.Withdrawn)
	}
	if err := Check(tarball, b); err != nil {
		return res, err
	}
	unlock, err := p.lock()
	if err != nil {
		return res, err
	}
	defer unlock()
	p.sweep(staleStage)
	plan, err := p.Plan(b)
	if err != nil {
		return res, err
	}
	switch plan.Action {
	case Keep:
		res.Already, res.Version = true, plan.Installed.Version
		return res, nil
	case Replace:
		res.Replaced = plan.Installed.Version
	}
	if err := os.MkdirAll(p.AppsDir, 0o755); err != nil {
		return res, err
	}
	stage, err := os.MkdirTemp(p.AppsDir, stagePrefix)
	if err != nil {
		return res, err
	}
	keepStage := false
	defer func() {
		if !keepStage {
			os.RemoveAll(stage)
		}
	}()
	if err := unpack(tarball, stage); err != nil {
		return res, fmt.Errorf("Tart %s's tart.tar.gz: %w. Nothing was installed", b.Version, err)
	}
	staged := filepath.Join(stage, AppName)
	sum, _, err := fileSHA256(filepath.Join(staged, ProgramInApp))
	switch {
	case err != nil:
		return res, fmt.Errorf("Tart %s's tart.tar.gz has no %s: %w. Nothing was installed", b.Version, filepath.Join(AppName, ProgramInApp), err)
	case sum != b.ProgramSHA256:
		return res, fmt.Errorf("Tart %s's program has SHA-256 %s, not %s. Nothing was installed", b.Version, sum, b.ProgramSHA256)
	}
	if err := syncTree(stage); err != nil {
		return res, err
	}
	rec := Record{Version: b.Version}
	if rec.Inode, rec.Tree, err = identify(staged); err != nil {
		return res, err
	}
	// Bundles made earlier stay known while a work folder may still hold one.
	if err := p.writeRecords(append([]Record{rec}, p.records()...)); err != nil {
		return res, fmt.Errorf("cannot record the install: %w. Nothing was installed", err)
	}
	if plan.Action == Fresh {
		beforeMove()
		if err := renameNew(staged, p.App()); err != nil {
			return res, fmt.Errorf("cannot move tart.app into %s: %w. Nothing was installed", p.AppsDir, err)
		}
		return res, nil
	}
	swapDir := filepath.Join(p.AppsDir, swapPrefix+strings.TrimPrefix(filepath.Base(stage), stagePrefix))
	if err := os.Rename(stage, swapDir); err != nil {
		return res, err
	}
	stage, staged = swapDir, filepath.Join(swapDir, AppName)
	beforeMove()
	if err := swapApps(staged, p.App()); err != nil {
		return res, fmt.Errorf("cannot move tart.app into %s: %w. Nothing was installed", p.AppsDir, err)
	}
	if out, ok := p.made(staged); !ok || out != plan.Installed {
		swapErr := swapApps(staged, p.App())
		// The folder goes only when it holds the bundle unpacked here; else
		// the lender's files are in it, put under a name they can see.
		if out, ok := p.made(staged); !ok || out != rec {
			keepStage = true
			kept := filepath.Join(p.AppsDir, "tart put aside "+strings.TrimPrefix(filepath.Base(stage), swapPrefix))
			if os.Rename(stage, kept) == nil {
				staged = filepath.Join(kept, AppName)
			}
			if swapErr != nil {
				return res, fmt.Errorf("a tart.app Potluck did not install appeared at %s during the install, and it could not be put back (%v): it is at %s", p.App(), swapErr, staged)
			}
			return res, fmt.Errorf("%s changed during the install: %w. Files that were there are at %s", p.App(), ErrNotOurs, staged)
		}
		return res, fmt.Errorf("%s changed during the install: %w. Nothing was installed", p.App(), ErrNotOurs)
	}
	return res, nil
}

// lock holds RecordFile's lock until unlock: one install or removal at a time.
func (p Place) lock() (unlock func(), err error) { return p.flock(syscall.LOCK_EX) }

// flock takes the lock as how says. A removal deletes the lock file: a run
// that waited on the deleted one takes the one now there.
func (p Place) flock(how int) (unlock func(), err error) {
	name := p.RecordFile + ".lock"
	for try := 0; ; try++ {
		if err := os.MkdirAll(filepath.Dir(p.RecordFile), 0o700); err != nil {
			return nil, err
		}
		f, err := os.OpenFile(name, os.O_RDWR|os.O_CREATE, 0o600)
		// A removal took the folder meanwhile (macOS: EINVAL while it goes).
		if (errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.EINVAL)) && try < 100 {
			time.Sleep(time.Millisecond)
			continue
		}
		if err != nil {
			return nil, err
		}
		if err := syscall.Flock(int(f.Fd()), how); err != nil {
			f.Close()
			return nil, err
		}
		held, err := f.Stat()
		if err != nil {
			f.Close()
			return nil, err
		}
		if now, err := os.Stat(name); err == nil && os.SameFile(held, now) {
			return func() { f.Close() }, nil
		}
		f.Close()
	}
}

// Sweep removes the work folders interrupted installs left in AppsDir.
func (p Place) Sweep() error {
	unlock, err := p.lock()
	if err != nil {
		return err
	}
	defer unlock()
	p.sweep(staleStage)
	return nil
}

// sweep removes the work folders older than age. Lock held.
func (p Place) sweep(age time.Duration) {
	ents, err := os.ReadDir(p.AppsDir)
	if err != nil {
		return
	}
	for _, e := range ents {
		swapped := strings.HasPrefix(e.Name(), swapPrefix)
		if !e.IsDir() || !swapped && !strings.HasPrefix(e.Name(), stagePrefix) {
			continue
		}
		if fi, err := e.Info(); err != nil || time.Since(fi.ModTime()) <= age {
			continue
		}
		dir := filepath.Join(p.AppsDir, e.Name())
		if app := filepath.Join(dir, AppName); swapped && lexists(app) {
			if _, ok := p.made(app); !ok {
				continue
			}
			// Half deleted, it is still known as Potluck's under this name.
			known := filepath.Join(p.AppsDir, stagePrefix+strings.TrimPrefix(e.Name(), swapPrefix))
			if os.Rename(dir, known) == nil {
				dir = known
			}
		}
		os.RemoveAll(dir)
	}
}

// Test seams: swapApps is swap, and beforeMove runs just before a bundle
// moves to App() (Install) or away from it (Remove).
var (
	swapApps   = swap
	beforeMove = func() {}
)

// maxUnpacked bounds what unpack writes, far above any Tart release.
const maxUnpacked = 1 << 30

// unpack writes the gzipped tar at tarball into dir, through an os.Root so
// nothing lands outside it. Only regular files and folders under tart.app/
// are taken (the release's top-level LICENSE is skipped); a link, a device,
// an absolute or ".." name, or a name twice refuses the whole file.
func unpack(tarball, dir string) error {
	f, err := os.Open(tarball)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	tr := tar.NewReader(gz)
	var total int64
	seen := map[string]bool{}
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		name := strings.TrimSuffix(strings.TrimPrefix(h.Name, "./"), "/")
		if name == "LICENSE" && h.Typeflag == tar.TypeReg {
			continue
		}
		if !fs.ValidPath(name) || path.Clean(name) != name || (name != AppName && !strings.HasPrefix(name, AppName+"/")) {
			return fmt.Errorf("it holds %q, which is not a file in %s", h.Name, AppName)
		}
		if seen[name] {
			return fmt.Errorf("it holds %q twice", h.Name)
		}
		seen[name] = true
		local := filepath.FromSlash(name)
		switch h.Typeflag {
		case tar.TypeDir:
			if err := root.MkdirAll(local, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if total += h.Size; h.Size < 0 || total > maxUnpacked {
				return errors.New("it unpacks to more than 1 GiB")
			}
			if err := root.MkdirAll(filepath.Dir(local), 0o755); err != nil {
				return err
			}
			mode := fs.FileMode(0o644)
			if h.Mode&0o111 != 0 {
				mode = 0o755
			}
			out, err := root.OpenFile(local, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
			if err != nil {
				return err
			}
			_, err = io.Copy(out, io.LimitReader(tr, h.Size))
			if err == nil {
				err = out.Sync()
			}
			if cerr := out.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return err
			}
		default:
			return fmt.Errorf("it holds %q, which is not a plain file or folder (tar type %q)", h.Name, h.Typeflag)
		}
	}
	return nil
}

// writeRecords replaces RecordFile, synced.
func (p Place) writeRecords(recs []Record) error {
	return p.write(recordFile{Path: p.App(), Bundles: recs, UsedElsewhere: p.UsedElsewhere()})
}

func (p Place) write(f recordFile) error {
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	return atomicfile.WritePrivate(p.RecordFile, append(data, '\n'))
}

// syncTree syncs every folder under dir, so a bundle is on the disk before
// it moves into place (unpack syncs each file).
func syncTree(dir string) error {
	return filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return err
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		return f.Sync()
	})
}

func lexists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

func fileSHA256(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", 0, err
	}
	if !st.Mode().IsRegular() {
		return "", 0, fmt.Errorf("%s is not a regular file", path)
	}
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}
