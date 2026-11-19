package runtarget

// Driver pinning (DRIVERS.md). A driver runs as the machine's owner and sees
// renters' secrets, so the daemon runs one only from a fixed set of
// directories (never from PATH), only when the file, any link to it and every
// directory above them are owned by the daemon's user or root and, by their
// mode bits, writable by no one else (ACLs are not read), and only when the
// file's sha256 is the one pinned for that driver in <home>/driver_pins.json.
// A driver with no pin is pinned the first time the daemon runs it (the bind
// that describes it, or the first call after an upgrade from a build without
// pins), with one log line. A changed or missing file is refused until the
// owner re-pins it (potluck driver pin <driver>, PinDriver) or the updater
// that placed it does (RepinPlaced).
//
// The pin is per driver, not per binding: every binding of a driver runs the
// same file, so one pin covers them all and an upgrade is re-pinned once.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"syscall"
	"time"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/protocol"
	"github.com/sengirunners/potluck/runtarget/rt"
)

// DriversDir is the directory under the daemon's home it also runs drivers
// from (protocol.DriversDir); PinFile records each driver's pin.
const (
	DriversDir = protocol.DriversDir
	PinFile    = "driver_pins.json"
)

// DriverPin is the file a driver was pinned to.
type DriverPin struct {
	Path   string `json:"path"`   // absolute, symlinks resolved
	SHA256 string `json:"sha256"` // hex
	Pinned string `json:"pinned"` // RFC 3339
}

// DriverDirs is protocol.DriverDirs: the daemon program's directory, then
// <home>/drivers.
func DriverDirs(home string) []string { return protocol.DriverDirs(home) }

// FindDriver is protocol.FindDriver with the daemon's error codes.
func FindDriver(dirs []string, driver string) (string, error) {
	p, err := protocol.FindDriver(dirs, driver)
	switch {
	case errors.Is(err, protocol.ErrDriverName):
		return "", api.Errorf(api.CodeBadRequest, "driver name %v", err)
	case err != nil:
		return "", api.Errorf(api.CodeUnavailable, "%v", err)
	}
	return p, nil
}

// repinHint is the command that re-pins driver.
func repinHint(driver string) string { return config.PotluckCommand("driver pin", driver) }

// short is a hash's first 12 characters, for messages.
func short(sum string) string { return sum[:min(len(sum), 12)] }

// fileID is what the hash cache keys on. A swap changes the inode (rename) or
// the ctime (a write in place; no one but root can set a ctime back).
type fileID struct {
	dev, ino            uint64
	size, mtime, ctimeN int64
}

func idOf(fi os.FileInfo) (fileID, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return fileID{}, false
	}
	c, ok := ctimeOf(st)
	return fileID{dev: uint64(st.Dev), ino: st.Ino, size: fi.Size(), mtime: fi.ModTime().UnixNano(), ctimeN: c}, ok
}

// pins checks driver files against their pins. Hashing a 30 MB driver takes
// tens of milliseconds, so a sum is cached by the file's identity: a file
// whose dev, inode, size, mtime and ctime are all unchanged is taken to hold
// the same bytes. What that trades: root can change a file without changing
// its ctime (by setting the clock), and a filesystem with coarse ctimes could
// hide a same-size write in the same tick as the hash; a file changed in the
// last two seconds is never cached for that reason. Root on the box is out of
// scope anyway (DRIVERS.md).
type pins struct {
	home string
	log  func(msg string, args ...any)
	mu   sync.Mutex
	sums map[string]cachedSum // resolved path -> its sum at an identity
	now  func() time.Time
	// noFirstPin refuses a driver with no pin instead of pinning it
	// (HostCommand: an owner's command never pins).
	noFirstPin bool
}

type cachedSum struct {
	id  fileID
	sum string
}

func newPins(home string, log func(string, ...any)) *pins {
	return &pins{home: home, log: log, sums: map[string]cachedSum{}, now: time.Now}
}

// warnUnpinned logs a warning, once (at Start), when the pins file holds no
// pins but run targets exist: every driver will be trusted again on its next
// use. Expected once after upgrading from a version without pins; otherwise
// someone removed or emptied the file.
func (s *Service) warnUnpinned() {
	m, err := readPins(s.d.Config.Home)
	if err != nil || len(m) > 0 || s.d.Log == nil {
		return
	}
	n := 0
	if hs, err := s.hostBindings(); err == nil {
		n += len(hs)
	}
	for _, p := range s.d.Store.Projects(s.d.System().Scope, authz.Write) {
		n += len(rt.ProjectTargets(s.d.Projects, p.ID))
	}
	if n > 0 {
		s.d.Log.Warn("driver pins file missing: re-trusting every driver on next use (expected once after upgrading from a version without pins)",
			"file", filepath.Join(s.d.Config.Home, PinFile), "run_targets", n)
	}
}

// verified is a driver file that matched its pin, held open from the hash to
// the exec (driver_exec_*.go).
type verified struct {
	path string
	f    *os.File
	id   fileID
}

func (v *verified) Close() error { return v.f.Close() }

// recheck refuses a file that is no longer the one hashed (by path).
func (v *verified) recheck() error {
	st, err := os.Stat(v.path)
	if err != nil {
		return err
	}
	if id, _ := idOf(st); id != v.id {
		return fmt.Errorf("%s changed after it was checked", v.path)
	}
	return nil
}

// open checks the driver file the daemon found (cand, or the error finding
// it) for binding's driver against its pin, pinning it on first use, and
// returns it open. Every refusal names the binding, the path and the fix.
func (p *pins) open(binding, driver, cand string, findErr error) (*verified, error) {
	pinned, hasPin, err := p.pin(driver)
	if err != nil {
		return nil, err
	}
	if findErr != nil {
		if hasPin {
			return nil, api.Errorf(api.CodeUnavailable, "run target %q: driver %s is pinned at %s (sha256 %s), which the daemon no longer finds (%v); reinstall it, or pin what is there now with: %s",
				binding, driver, pinned.Path, short(pinned.SHA256), findErr, repinHint(driver))
		}
		return nil, findErr
	}
	v, sum, err := p.check(cand)
	if err != nil {
		return nil, api.Errorf(api.CodeUnavailable, "run target %q: driver %s refused: %v", binding, driver, err)
	}
	if !hasPin && p.noFirstPin {
		v.Close()
		return nil, api.Errorf(api.CodeUnavailable, "run target %q: driver %s is not pinned yet: let the daemon run it once, or pin it with: %s", binding, driver, repinHint(driver))
	}
	if !hasPin {
		var first bool
		if err := updatePins(p.home, func(m map[string]DriverPin) {
			if pinned, hasPin = m[driver]; !hasPin { // another process may have pinned it meanwhile
				pinned = DriverPin{Path: v.path, SHA256: sum, Pinned: p.now().UTC().Format(time.RFC3339)}
				m[driver], first = pinned, true
			}
		}); err != nil {
			v.Close()
			return nil, err
		}
		if first && p.log != nil {
			p.log("run targets: driver pinned on first use; re-pin after a deliberate upgrade", "driver", driver, "path", v.path,
				"sha256", short(sum), "binding", binding, "repin", repinHint(driver))
		}
	}
	switch {
	case pinned.Path != v.path:
		v.Close()
		return nil, api.Errorf(api.CodeUnavailable, "run target %q: driver %s is pinned at %s, but the daemon now finds %s; if that is on purpose, pin it with: %s",
			binding, driver, pinned.Path, v.path, repinHint(driver))
	case pinned.SHA256 != sum:
		v.Close()
		return nil, api.Errorf(api.CodeUnavailable, "run target %q: driver %s at %s is not the file that was pinned (sha256 expected %s, found %s); if you upgraded it on purpose, re-pin with: %s",
			binding, driver, v.path, short(pinned.SHA256), short(sum), repinHint(driver))
	}
	return v, nil
}

// pin is driver's pin, if any.
func (p *pins) pin(driver string) (DriverPin, bool, error) {
	m, err := readPins(p.home)
	if err != nil {
		return DriverPin{}, false, err
	}
	d, ok := m[driver]
	return d, ok, nil
}

// check resolves cand, refuses it unless it is safe (safePath), opens it and
// hashes it (from the cache when its identity is unchanged).
func (p *pins) check(cand string) (*verified, string, error) {
	cand, err := filepath.Abs(cand)
	if err != nil {
		return nil, "", err
	}
	// The name as found, before any link is followed: whoever can write its
	// directory chooses what it points at, so that directory is held to the
	// driver's own rule (no sticky exemption), and a link must be owned by
	// the daemon's user or root.
	if err := safeDirs(filepath.Dir(cand)); err != nil {
		return nil, "", err
	}
	lfi, err := os.Lstat(cand)
	if err != nil {
		return nil, "", err
	}
	if lfi.Mode()&os.ModeSymlink != 0 {
		if err := safeOwner(cand, lfi); err != nil {
			return nil, "", err
		}
	}
	real, err := filepath.EvalSymlinks(cand)
	if err != nil {
		return nil, "", err
	}
	if real, err = filepath.Abs(real); err != nil {
		return nil, "", err
	}
	if err := safePath(real); err != nil {
		return nil, "", err
	}
	f, err := os.Open(real)
	if err != nil {
		return nil, "", err
	}
	fi, err := f.Stat()
	if err == nil {
		// The open file must be the one safePath checked.
		if lfi, lerr := os.Lstat(real); lerr != nil || !os.SameFile(fi, lfi) {
			err = fmt.Errorf("%s changed while it was checked", real)
		} else {
			err = safeMode(real, fi, false)
		}
	}
	if err != nil {
		f.Close()
		return nil, "", err
	}
	id, cacheable := idOf(fi)
	p.mu.Lock()
	c, hit := p.sums[real]
	p.mu.Unlock()
	if hit && cacheable && c.id == id {
		return &verified{path: real, f: f, id: id}, c.sum, nil
	}
	sum, err := hashFile(f)
	if err != nil {
		f.Close()
		return nil, "", err
	}
	if cacheable && p.now().Sub(fi.ModTime()) > 2*time.Second && p.now().UnixNano()-id.ctimeN > int64(2*time.Second) {
		p.mu.Lock()
		p.sums[real] = cachedSum{id: id, sum: sum}
		p.mu.Unlock()
	}
	return &verified{path: real, f: f, id: id}, sum, nil
}

func hashFile(f *os.File) (string, error) {
	h := sha256.New()
	if _, err := io.Copy(h, io.NewSectionReader(f, 0, 1<<62)); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// safePath refuses a driver file, its directory or any directory above them
// that someone other than the daemon's user or root could change: owned by
// another user, or writable by group or others (mode bits only: an ACL is
// not read). A directory above the driver's own may be writable by others
// when it is sticky (/tmp): no one can then rename what they do not own. On
// a platform whose owners cannot be read, only the modes are checked.
func safePath(p string) error {
	fi, err := os.Lstat(p)
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", p)
	}
	if err := safeMode(p, fi, false); err != nil {
		return err
	}
	return safeDirs(filepath.Dir(p))
}

// safeDirs checks dir (never sticky-exempt) and every directory above it
// (sticky ones exempt from the write rule). A directory reached through a
// link is checked where the link leads; the link itself sits in the next
// directory up, which is checked too.
func safeDirs(dir string) error {
	for first := true; ; dir, first = filepath.Dir(dir), false {
		fi, err := os.Stat(dir)
		if err != nil {
			return err
		}
		if err := safeMode(dir, fi, !first); err != nil {
			return err
		}
		if dir == filepath.Dir(dir) {
			return nil
		}
	}
}

func safeMode(p string, fi os.FileInfo, stickyOK bool) error {
	if fi.Mode().Perm()&0o022 != 0 && !(stickyOK && fi.Mode()&os.ModeSticky != 0) {
		return fmt.Errorf("%s is writable by group or others (fix: chmod go-w %s)", p, p)
	}
	return safeOwner(p, fi)
}

// safeOwner refuses p unless the daemon's user or root owns it.
func safeOwner(p string, fi os.FileInfo) error {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}
	if uid := os.Geteuid(); int(st.Uid) != uid && st.Uid != 0 {
		return fmt.Errorf("%s is owned by uid %d, not the daemon's user (uid %d) or root", p, st.Uid, uid)
	}
	return nil
}

// readPins reads <home>/driver_pins.json. A missing file, null, {} and
// {"drivers":null} all hold no pins (warnUnpinned).
func readPins(home string) (map[string]DriverPin, error) {
	var f struct {
		Drivers map[string]DriverPin `json:"drivers"`
	}
	b, err := os.ReadFile(filepath.Join(home, PinFile))
	if errors.Is(err, os.ErrNotExist) {
		return map[string]DriverPin{}, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("%s: %w (fix it, or remove it to pin every driver again on its next use)", PinFile, err)
	}
	if f.Drivers == nil {
		f.Drivers = map[string]DriverPin{}
	}
	return f.Drivers, nil
}

// updatePins changes the pins under a lock shared with every other process
// using this home (the daemon, potluck driver pin, an updater).
func updatePins(home string, fn func(map[string]DriverPin)) error {
	if err := os.MkdirAll(home, 0o700); err != nil {
		return err
	}
	lock, err := os.OpenFile(filepath.Join(home, PinFile+".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	m, err := readPins(home)
	if err != nil {
		return err
	}
	fn(m)
	b, err := json.MarshalIndent(struct {
		Drivers map[string]DriverPin `json:"drivers"`
	}{m}, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(home, "."+PinFile+".*")
	if err != nil {
		return err
	}
	_, err = tmp.Write(append(b, '\n'))
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), filepath.Join(home, PinFile))
	}
	if err != nil {
		os.Remove(tmp.Name())
	}
	return err
}

// PinDriver pins driver to the file found in dirs now (potluck driver pin):
// the owner's deliberate trust, after an upgrade or before a first use.
func PinDriver(home string, dirs []string, driver string) (DriverPin, error) {
	cand, err := FindDriver(dirs, driver)
	if err != nil {
		return DriverPin{}, err
	}
	v, sum, err := newPins(home, nil).check(cand)
	if err != nil {
		return DriverPin{}, api.Errorf(api.CodeUnavailable, "driver %s refused: %v", driver, err)
	}
	v.Close()
	d := DriverPin{Path: v.path, SHA256: sum, Pinned: time.Now().UTC().Format(time.RFC3339)}
	return d, updatePins(home, func(m map[string]DriverPin) { m[driver] = d })
}

// RepinPlaced re-pins the drivers an updater just placed in dir: sums are
// the sha256 of each file it placed (swap.Placed.Sums), computed while it
// copied them from the build it verified. Only a driver already pinned to a
// file in dir is re-pinned, only to a file the updater placed, and only when
// that file still has the sum it was placed with. Call it once the new build
// is final (not before a restore). It answers the drivers re-pinned.
func RepinPlaced(home, dir string, sums map[string]string) ([]string, error) {
	dir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return nil, err
	}
	var done []string
	var errs []error
	err = updatePins(home, func(m map[string]DriverPin) {
		for driver, old := range m {
			if filepath.Dir(old.Path) != dir {
				continue
			}
			cand, err := FindDriver([]string{dir}, driver)
			if err != nil {
				continue // removed by the update: the next call says so
			}
			want, ok := sums[filepath.Base(cand)]
			if !ok {
				continue
			}
			v, sum, err := newPins(home, nil).check(cand)
			if err != nil {
				errs = append(errs, fmt.Errorf("driver %s: %w", driver, err))
				continue
			}
			v.Close()
			if sum != want {
				errs = append(errs, fmt.Errorf("driver %s: %s changed after it was placed (sha256 %s, placed %s)", driver, v.path, short(sum), short(want)))
				continue
			}
			m[driver] = DriverPin{Path: v.path, SHA256: sum, Pinned: time.Now().UTC().Format(time.RFC3339)}
			done = append(done, driver)
		}
	})
	sort.Strings(done)
	return done, errors.Join(append(errs, err)...)
}

// PinOf is driver's pin, if it has one.
func PinOf(home, driver string) (DriverPin, bool, error) {
	return newPins(home, nil).pin(driver)
}

// PinnedDriver is driver's file as the daemon would run it now: found in
// dirs, safe, and the very file its pin names, with that sum. Unlike a bind
// it never pins: a driver with no pin is refused, since nothing then says
// which file this machine's owner trusts.
func PinnedDriver(home string, dirs []string, driver string) (DriverPin, error) {
	p := newPins(home, nil)
	pinned, ok, err := p.pin(driver)
	if err != nil {
		return DriverPin{}, err
	}
	if !ok {
		return DriverPin{}, fmt.Errorf("driver %s has no pin, so nothing says which file this machine trusts; pin it with: %s", driver, repinHint(driver))
	}
	cand, err := FindDriver(dirs, driver)
	if err != nil {
		return DriverPin{}, fmt.Errorf("driver %s is pinned at %s, which is not found (%v); reinstall it, or pin what is there now with: %s", driver, pinned.Path, err, repinHint(driver))
	}
	v, sum, err := p.check(cand)
	if err != nil {
		return DriverPin{}, fmt.Errorf("driver %s refused: %v", driver, err)
	}
	v.Close()
	switch {
	case v.path != pinned.Path:
		return DriverPin{}, fmt.Errorf("driver %s is pinned at %s, but %s is found now; if that is on purpose, pin it with: %s", driver, pinned.Path, v.path, repinHint(driver))
	case sum != pinned.SHA256:
		return DriverPin{}, fmt.Errorf("driver %s at %s is not the file that was pinned (sha256 expected %s, found %s); if you upgraded it on purpose, pin it with: %s", driver, v.path, short(pinned.SHA256), short(sum), repinHint(driver))
	}
	return pinned, nil
}
