package softnet

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
)

// testStore is a store in a temporary directory, owned by this test's
// user (a real one is root's).
func testStore(t *testing.T) Store {
	top := t.TempDir()
	return Store{Dir: filepath.Join(top, "Library", "Potluck", "Softnet"), Owner: os.Geteuid(), Group: os.Getegid(), Top: top}
}

// pin adds a build whose executable is content to Builds for this test,
// and writes that file; it returns the build and the file's path.
func pin(t *testing.T, id, content string) (Build, string) {
	t.Helper()
	sum := sha256.Sum256([]byte(content))
	b := Build{ID: id, Version: "softnet " + id, SHA256: hex.EncodeToString(sum[:]), Bytes: int64(len(content)),
		Arch: "arm64", MinMacOS: 15, MaxMacOS: 15, LicenseSHA256: licenseSHA256}
	old := Builds
	Builds = append(append([]Build{}, Builds...), b)
	t.Cleanup(func() { Builds = old })
	src := filepath.Join(t.TempDir(), "softnet-download")
	if err := os.WriteFile(src, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return b, src
}

// checks counts the checks run and fails them with fail.
type checks struct {
	n    int
	fail error
}

func (c *checks) check(path string, b Build) error {
	c.n++
	if sum, _, err := sha256File(path); err != nil || sum != b.SHA256 {
		return errors.New("checked a file that is not the build")
	}
	return c.fail
}

func current(t *testing.T, s Store) (cur, prev string) {
	t.Helper()
	st := s.Status()
	if st.Current != nil {
		cur = st.Current.ID
		if !st.Current.Pinned {
			t.Fatalf("current %s is not sound: %s", cur, st.Current.Problem)
		}
	}
	if st.Previous != nil {
		prev = st.Previous.ID
	}
	return cur, prev
}

func TestInstallPlacesARootStyleSetuidBuildWithItsLicense(t *testing.T) {
	s := testStore(t)
	b, src := pin(t, "1.0-test", "build one")
	var c checks
	r, err := s.Install(src, b, c.check)
	if err != nil {
		t.Fatal(err)
	}
	if r.ID != b.ID || r.Previous != "" || r.Unchanged || c.n != 1 {
		t.Fatalf("result %+v after %d checks", r, c.n)
	}
	fi, err := os.Stat(s.Current())
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&fs.ModeSetuid == 0 || fi.Mode().Perm() != 0o755 {
		t.Fatalf("installed mode %s, want setuid 0755", fi.Mode())
	}
	lic, err := os.ReadFile(filepath.Join(s.Dir, b.ID, "LICENSE"))
	if err != nil || string(lic) != License {
		t.Fatalf("license beside the build: %v", err)
	}
	var p Provenance
	data, _ := os.ReadFile(filepath.Join(s.Dir, b.ID, "provenance.json"))
	if err := json.Unmarshal(data, &p); err != nil || p.SHA256 != b.SHA256 || p.ID != b.ID || p.LicenseSHA256 != licenseSHA256 {
		t.Fatalf("provenance %s: %v", data, err)
	}
	for _, d := range []string{filepath.Dir(s.Dir), s.Dir, filepath.Join(s.Dir, b.ID)} {
		if fi, err := os.Stat(d); err != nil || fi.Mode().Perm() != 0o755 {
			t.Fatalf("%s: %v %v", d, fi.Mode(), err)
		}
	}
	if es, _ := filepath.Glob(filepath.Join(s.Dir, ".staging-*")); len(es) != 0 {
		t.Fatalf("staging left behind: %v", es)
	}
}

func TestInstallIsIdempotentAndKeepsThePreviousBuildForRollback(t *testing.T) {
	s := testStore(t)
	b1, src1 := pin(t, "1.0-test", "build one")
	b2, src2 := pin(t, "2.0-test", "build two")
	var c checks
	if _, err := s.Install(src1, b1, c.check); err != nil {
		t.Fatal(err)
	}
	r, err := s.Install(src1, b1, c.check)
	if err != nil || !r.Unchanged || c.n != 1 {
		t.Fatalf("second install of the same build: %+v %v after %d checks", r, err, c.n)
	}
	if r, err := s.Install(src2, b2, c.check); err != nil || r.Previous != b1.ID {
		t.Fatalf("upgrade: %+v %v", r, err)
	}
	if cur, prev := current(t, s); cur != b2.ID || prev != b1.ID {
		t.Fatalf("after upgrade: current %s previous %s", cur, prev)
	}
	if r, err := s.Rollback(c.check); err != nil || r.ID != b1.ID || r.Previous != b2.ID {
		t.Fatalf("rollback: %+v %v", r, err)
	}
	if cur, prev := current(t, s); cur != b1.ID || prev != b2.ID {
		t.Fatalf("after rollback: current %s previous %s", cur, prev)
	}
	// Installing the newer build again puts its kept copy back in use: no copy, one check.
	before := c.n
	if r, err := s.Install(filepath.Join(t.TempDir(), "absent"), b2, c.check); err != nil || r.ID != b2.ID || c.n != before+1 {
		t.Fatalf("re-install of a kept build: %+v %v", r, err)
	}
	if cur, prev := current(t, s); cur != b2.ID || prev != b1.ID {
		t.Fatalf("after re-install: current %s previous %s", cur, prev)
	}
}

func TestInstallRefusesAFileThatIsNotThePinnedBuild(t *testing.T) {
	s := testStore(t)
	b1, src1 := pin(t, "1.0-test", "build one")
	b2, _ := pin(t, "2.0-test", "build two")
	var c checks
	if _, err := s.Install(src1, b1, c.check); err != nil {
		t.Fatal(err)
	}
	bad := filepath.Join(t.TempDir(), "softnet")
	os.WriteFile(bad, []byte("build twO"), 0o755)
	if _, err := s.Install(bad, b2, c.check); err == nil || !strings.Contains(err.Error(), "is not Softnet 2.0-test") {
		t.Fatalf("wrong file: %v", err)
	}
	long := filepath.Join(t.TempDir(), "softnet")
	os.WriteFile(long, []byte("build two and more"), 0o755)
	if _, err := s.Install(long, b2, c.check); err == nil {
		t.Fatal("a longer file was installed")
	}
	if cur, _ := current(t, s); cur != b1.ID || c.n != 1 {
		t.Fatalf("current %s after refused installs, %d checks", cur, c.n)
	}
	if _, err := os.Lstat(filepath.Join(s.Dir, b2.ID)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("refused build was placed: %v", err)
	}
}

func TestAFailedCheckLeavesTheBuildInUse(t *testing.T) {
	s := testStore(t)
	b1, src1 := pin(t, "1.0-test", "build one")
	b2, src2 := pin(t, "2.0-test", "build two")
	var c checks
	s.Install(src1, b1, c.check)
	c.fail = errors.New("dyld: missing symbol")
	if _, err := s.Install(src2, b2, c.check); err == nil || !strings.Contains(err.Error(), "dyld: missing symbol") {
		t.Fatalf("install past a failed check: %v", err)
	}
	if cur, prev := current(t, s); cur != b1.ID || prev != "" {
		t.Fatalf("current %s previous %s after a failed check", cur, prev)
	}
	if _, err := os.Lstat(filepath.Join(s.Dir, b2.ID)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("failed build was placed: %v", err)
	}
}

func TestRollbackNeedsASoundPinnedPreviousBuild(t *testing.T) {
	s := testStore(t)
	b1, src1 := pin(t, "1.0-test", "build one")
	var c checks
	if _, err := s.Rollback(c.check); err == nil {
		t.Fatal("rollback with nothing installed")
	}
	s.Install(src1, b1, c.check)
	if _, err := s.Rollback(c.check); err == nil || !strings.Contains(err.Error(), "no earlier Softnet") {
		t.Fatalf("rollback with one build: %v", err)
	}
	b2, src2 := pin(t, "2.0-test", "build two")
	s.Install(src2, b2, c.check)
	// A build this potluck no longer pins (withdrawn) is never put back in use.
	Builds = []Build{b2}
	if _, err := s.Rollback(c.check); err == nil || !strings.Contains(err.Error(), "not a build this potluck pins") {
		t.Fatalf("rollback to an unpinned build: %v", err)
	}
	if cur, _ := current(t, s); cur != b2.ID {
		t.Fatalf("current %s after refused rollback", cur)
	}
}

func TestAChangedInstalledBuildIsReportedAndReplaced(t *testing.T) {
	s := testStore(t)
	b, src := pin(t, "1.0-test", "build one")
	var c checks
	s.Install(src, b, c.check)
	bin := filepath.Join(s.Dir, b.ID, "softnet")
	os.Chmod(bin, 0o755)
	os.WriteFile(bin, []byte("build 0ne"), 0o755)
	st := s.Status()
	if st.Current == nil || st.Current.Pinned || !strings.Contains(st.Current.Problem, "not setuid") && !strings.Contains(st.Current.Problem, "SHA-256") {
		t.Fatalf("status of a changed build: %+v", st.Current)
	}
	if r, err := s.Install(src, b, c.check); err != nil || r.Unchanged {
		t.Fatalf("repair: %+v %v", r, err)
	}
	if cur, _ := current(t, s); cur != b.ID {
		t.Fatal("not repaired")
	}
	if es, _ := filepath.Glob(filepath.Join(s.Dir, ".old-*")); len(es) != 0 {
		t.Fatalf("set-aside copy left behind: %v", es)
	}
}

func TestInstallRefusesADirectoryOthersCanWrite(t *testing.T) {
	b, src := pin(t, "1.0-test", "build one")
	var c checks

	s := testStore(t)
	lib := filepath.Join(s.Top, "Library")
	os.Mkdir(lib, 0o755)
	os.Chmod(lib, 0o775)
	if _, err := s.Install(src, b, c.check); err == nil || !strings.Contains(err.Error(), "writable by group or others") {
		t.Fatalf("group-writable parent: %v", err)
	}

	s = testStore(t)
	real := filepath.Join(s.Top, "elsewhere")
	os.Mkdir(real, 0o755)
	os.Symlink(real, filepath.Join(s.Top, "Library"))
	if _, err := s.Install(src, b, c.check); err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("linked parent: %v", err)
	}

	s = testStore(t)
	s.Owner = os.Geteuid() + 1
	if _, err := s.Install(src, b, c.check); err == nil || !strings.Contains(err.Error(), "needs uid") {
		t.Fatalf("wrong user: %v", err)
	}
	if c.n != 0 {
		t.Fatalf("%d checks ran on refused installs", c.n)
	}
}

func TestStatusRefusesALinkOutOfTheStore(t *testing.T) {
	s := testStore(t)
	b, src := pin(t, "1.0-test", "build one")
	var c checks
	s.Install(src, b, c.check)
	os.Remove(filepath.Join(s.Dir, "current"))
	os.Symlink("../../elsewhere", filepath.Join(s.Dir, "current"))
	if st := s.Status(); st.Current == nil || st.Current.Pinned || st.Current.Problem == "" {
		t.Fatalf("a link out of the store: %+v", st.Current)
	}
}

// A replaced build that is neither current nor previous is disarmed and
// removed: a hard link someone made to it loses its mode bits too.
func TestARetiredBuildIsDisarmedBeforeItIsRemoved(t *testing.T) {
	s := testStore(t)
	b1, src1 := pin(t, "1.0-test", "build one")
	b2, src2 := pin(t, "2.0-test", "build two")
	b3, src3 := pin(t, "3.0-test", "build three")
	var c checks
	s.Install(src1, b1, c.check)
	held := filepath.Join(s.Top, "held-link")
	if err := os.Link(filepath.Join(s.Dir, b1.ID, binaryName), held); err != nil {
		t.Fatal(err)
	}
	s.Install(src2, b2, c.check)
	if _, err := os.Stat(filepath.Join(s.Dir, b1.ID)); err != nil {
		t.Fatalf("the previous build went: %v", err)
	}
	if _, err := s.Install(src3, b3, c.check); err != nil {
		t.Fatal(err)
	}
	if cur, prev := current(t, s); cur != b3.ID || prev != b2.ID {
		t.Fatalf("current %s previous %s", cur, prev)
	}
	if _, err := os.Lstat(filepath.Join(s.Dir, b1.ID)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("retired build kept: %v", err)
	}
	if fi, err := os.Stat(held); err != nil || fi.Mode().Perm() != 0 || fi.Mode()&fs.ModeSetuid != 0 {
		t.Fatalf("a hard link to the retired build keeps mode %v (%v)", fi.Mode(), err)
	}
}

// A withdrawn build is never installed, fails its check where it is
// installed, and is removed as soon as another build replaces it.
func TestAWithdrawnBuildIsRefusedAndRemoved(t *testing.T) {
	s := testStore(t)
	b1, src1 := pin(t, "1.0-test", "build one")
	b2, src2 := pin(t, "2.0-test", "build two")
	var c checks
	s.Install(src1, b1, c.check)
	Builds[len(Builds)-2].Withdrawn = "it lets a box reach the LAN"
	b1.Withdrawn = "it lets a box reach the LAN"
	if _, err := s.Install(src1, b1, c.check); err == nil || !strings.Contains(err.Error(), "withdrawn: it lets a box reach the LAN") {
		t.Fatalf("install of a withdrawn build: %v", err)
	}
	if st := s.Status(); st.Current == nil || st.Current.Pinned || !strings.Contains(st.Current.Problem, "withdrawn") {
		t.Fatalf("status of a withdrawn build in use: %+v", st.Current)
	}
	if r, err := s.Install(src2, b2, c.check); err != nil || r.Previous != b1.ID {
		t.Fatalf("replacing it: %+v %v", r, err)
	}
	if st := s.Status(); st.Previous != nil {
		t.Fatalf("withdrawn build kept as previous: %+v", st.Previous)
	}
	if _, err := os.Lstat(filepath.Join(s.Dir, b1.ID)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("withdrawn build kept: %v", err)
	}
}

func TestUninstallDisarmsEveryBuildAndRemovesTheStore(t *testing.T) {
	s := testStore(t)
	b1, src1 := pin(t, "1.0-test", "build one")
	b2, src2 := pin(t, "2.0-test", "build two")
	var c checks
	s.Install(src1, b1, c.check)
	s.Install(src2, b2, c.check)
	var held []string
	for _, b := range []Build{b1, b2} {
		h := filepath.Join(s.Top, "held-"+b.ID)
		if err := os.Link(filepath.Join(s.Dir, b.ID, binaryName), h); err != nil {
			t.Fatal(err)
		}
		held = append(held, h)
	}
	if err := s.Uninstall(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(s.Dir); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("store kept: %v", err)
	}
	for _, h := range held {
		if fi, err := os.Stat(h); err != nil || fi.Mode().Perm() != 0 || fi.Mode()&fs.ModeSetuid != 0 {
			t.Fatalf("%s keeps mode %v (%v)", h, fi.Mode(), err)
		}
	}
}

// The link to the build in use and the link to the previous one never
// name the same build.
func TestPreviousIsNeverTheBuildInUse(t *testing.T) {
	s := testStore(t)
	b1, src1 := pin(t, "1.0-test", "build one")
	b2, src2 := pin(t, "2.0-test", "build two")
	var c checks
	s.Install(src1, b1, c.check)
	s.Install(src2, b2, c.check)
	os.Remove(filepath.Join(s.Dir, currentLink)) // current lost, previous names b1
	if _, err := s.Install(src1, b1, c.check); err != nil {
		t.Fatal(err)
	}
	if cur, prev := current(t, s); cur != b1.ID || prev != "" {
		t.Fatalf("current %s previous %s", cur, prev)
	}
}

// An ACL that lets anyone change a directory on the way, or the build, is
// refused; deny entries are not.
func TestInstallRefusesAnACLThatGrantsWrites(t *testing.T) {
	b, src := pin(t, "1.0-test", "build one")
	var c checks
	s := testStore(t)
	lib := filepath.Join(s.Top, "Library")
	old := aclEntries
	t.Cleanup(func() { aclEntries = old })
	aclEntries = func(p string) ([]string, error) {
		if p == lib {
			return []string{"group:everyone deny delete", "user:mallory allow add_file,add_subdirectory,file_inherit"}, nil
		}
		return nil, nil
	}
	if _, err := s.Install(src, b, c.check); err == nil || !strings.Contains(err.Error(), "lets user:mallory change it") {
		t.Fatalf("allow-write ACL on a parent: %v", err)
	}
	aclEntries = func(p string) ([]string, error) {
		if p == lib {
			return []string{"group:everyone deny delete"}, nil
		}
		return nil, nil
	}
	if _, err := s.Install(src, b, c.check); err != nil {
		t.Fatalf("a deny-only ACL: %v", err)
	}
	bin := filepath.Join(s.Dir, b.ID, binaryName)
	aclEntries = func(p string) ([]string, error) {
		switch p {
		case bin:
			return []string{"user:mallory inherited allow write"}, nil
		case lib:
			return []string{"something Potluck cannot read"}, nil
		}
		return nil, nil
	}
	if st := s.Status(); st.Current == nil || st.Current.Pinned || !strings.Contains(st.Current.Problem, "lets user:mallory change it") {
		t.Fatalf("allow-write ACL on the build: %+v", st.Current)
	}
	if _, err := s.Install(src, b, c.check); err == nil || !strings.Contains(err.Error(), "cannot read") {
		t.Fatalf("an unreadable ACL entry: %v", err)
	}
}

// On a Mac, the real ACL reader sees an ACL chmod set.
func TestTheACLReaderReadsARealACL(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS ACLs")
	}
	d := filepath.Join(t.TempDir(), "d")
	os.Mkdir(d, 0o755)
	if out, err := exec.Command("/bin/chmod", "+a", "group:staff allow add_file", d).CombinedOutput(); err != nil {
		t.Fatalf("chmod +a: %v %s", err, out)
	}
	t.Cleanup(func() { exec.Command("/bin/chmod", "-N", d).Run() })
	if err := aclSafe(d); err == nil || !strings.Contains(err.Error(), "group:staff") {
		t.Fatalf("aclSafe: %v", err)
	}
	if err := aclSafe(filepath.Dir(d)); err != nil {
		t.Fatalf("no ACL: %v", err)
	}
}

// The root step never blocks on a FIFO (or device) put where the file
// should be: it reads only a regular file.
func TestInstallRefusesAFileThatIsNotRegular(t *testing.T) {
	s := testStore(t)
	b, _ := pin(t, "1.0-test", "build one")
	fifo := filepath.Join(t.TempDir(), "softnet")
	if err := syscall.Mkfifo(fifo, 0o644); err != nil {
		t.Skip(err)
	}
	var c checks
	if _, err := s.Install(fifo, b, c.check); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("a FIFO: %v", err)
	}
}
