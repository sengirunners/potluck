package vmhost

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sengirunners/potluck/conformance"
	"github.com/sengirunners/potluck/conformance/refdriver"
	"github.com/sengirunners/potluck/protocol"
)

const flagDriverTest = "-zm-run-tart-test-driver"

// The test binary is also the driver (installed by installDriver) and, through
// os.Executable, every run's supervisor.
// envHostOverride carries the test's host override file (hostmode.go) to the
// drivers and supervisors it starts, so none reads the host's real one.
const envHostOverride = "VMHOST_TEST_HOST_OVERRIDE"

func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "--fake-native-journal" {
		os.Exit(fakeNativeJournal(os.Args[2:]))
	}
	if len(os.Args) > 1 && os.Args[1] == "--api-socket" && os.Getenv(envFakeCloudHV) != "" {
		os.Exit(fakeCloudHV(os.Args[1:])) // cloudhv_test.go: the tests' hypervisor
	}
	cloudhvTestStubs() // no test, and no monitor a test starts, runs the root helper
	if len(os.Args) >= 4 && (os.Args[1] == flagCloudHVVM || os.Args[1] == flagCloudHVExec) {
		os.Exit(Main(os.Args[1:]))
	}
	// Test sockets are served by this test process, never a real dockerd.
	verifyDockerPeer = func(net.Conn) (dockerEngineOwner, error) { return dockerEngineOwner{}, nil }
	if hostOverridePath = os.Getenv(envHostOverride); hostOverridePath == "" {
		hostOverridePath = filepath.Join(os.TempDir(), fmt.Sprintf("vmhost-test-%d", os.Getpid()), "host-override.json") // none: auto
		os.Setenv(envHostOverride, hostOverridePath)
	}
	if len(os.Args) == 4 && (os.Args[1] == flagSupervise || os.Args[1] == flagHold || os.Args[1] == flagMeasure) {
		os.Exit(Main(os.Args[1:]))
	}
	if len(os.Args) == 3 && os.Args[1] == flagShelfFill { // a filler the tests' driver spawned: never the public registry
		if shelfRegistry = os.Getenv(envTestRegistry); shelfRegistry == "" {
			os.Exit(3)
		}
		os.Exit(Main(os.Args[1:]))
	}
	if len(os.Args) == 2 && os.Args[1] == flagDriverTest {
		os.Exit(Main(nil))
	}
	home, err := os.MkdirTemp("", "zmrt-home")
	if err != nil {
		panic(err)
	}
	os.Setenv("HOME", home)
	os.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	code := m.Run()
	os.RemoveAll(home)
	os.Exit(code)
}

func init() {
	layers["fake"] = func(c config) vmLayer { return dirLayer{filepath.Join(c.StateDir, "fakevms")} }
}

// dirLayer is a VM layer with no VMs, for tests: a "VM" is a directory, its
// "guest" a subdirectory that stands in for guestDir, its power a host sh
// process holding the boot pipe, and Exec runs on the host in its own process
// group, which powering off kills. An image named mac* is a macOS image; one
// named missing is not there. It ignores isolated: tests over it prove
// nothing about the network (the live oracle on Tart does).
type dirLayer struct{ root string }

func (l dirLayer) dir(vm string) string { return filepath.Join(l.root, vm) }

func (l dirLayer) OSOf(name string) (string, error) {
	if b, err := os.ReadFile(filepath.Join(l.dir(name), "os")); err == nil {
		return string(b), nil
	}
	switch {
	case name == "missing":
		return "", fmt.Errorf("no image %q here", name)
	case strings.HasPrefix(name, "mac"):
		return "darwin", nil
	}
	return "linux", nil
}

// fakeRoot stands in for a guest's shared roots (/tmp, /var/tmp, ...) in
// the conformance checks' leftover search (Options.LeftoverRoots): the fake
// guest is this machine, so Exec rewrites it to the VM's own "root"
// directory, which goes with the VM, as a real guest's /tmp does.
const fakeRoot = "/potluck-fake-guest-root"

// fakeNoSudo is a fake box's sudo: what a default box's says.
const fakeNoSudo = "#!/bin/sh\necho 'sudo: The \"no new privileges\" flag is set, which prevents sudo from running as root.' >&2\nexit 1\n"

func (l dirLayer) Clone(image, vm string, _, _ int) error {
	o, err := l.OSOf(image)
	if err != nil {
		return err
	}
	for _, d := range []string{"guest", "home", "tmp", "root", "bin"} { // each VM its own HOME, TMPDIR and fakeRoot, as a real one has
		if err := os.MkdirAll(filepath.Join(l.dir(vm), d), 0o700); err != nil {
			return err
		}
	}
	// The fake guest is this machine, whose sudo may need no password (a CI
	// runner's): this box lists no root, so its sudo refuses, as a default
	// box's does (no-new-privileges), first on its PATH.
	if err := os.WriteFile(filepath.Join(l.dir(vm), "bin", "sudo"), []byte(fakeNoSudo), 0o700); err != nil {
		return err
	}
	if err := l.memFS(vm); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(l.dir(vm), "os"), []byte(o), 0o600)
}

// memFS gives a fake VM its memory filesystem (secrets.go): a directory of
// its own, "shm", that its mount table ("mounts", read in place of
// guestMounts) lists as a tmpfs, with a swap table ("swaps") listing none, and that its stat, first on its PATH,
// calls one (as the conformance probe asks: stat -f -c %T). A real guest's
// is in its memory and goes when it powers off; the fake's goes then too
// (off, offKept). Each VM its own: guestMem is fixed in a real guest, and
// the fake's guests share this machine.
func (l dirLayer) memFS(vm string) error {
	shm := filepath.Join(l.dir(vm), "shm")
	if err := os.MkdirAll(shm, 0o700); err != nil {
		return err
	}
	real, err := filepath.EvalSymlinks(shm)
	if err != nil {
		return err
	}
	mounts := "/dev/disk1 / ext4 rw 0 0\ntmpfs " + strings.ReplaceAll(real, " ", `\040`) + " tmpfs rw,nosuid,nodev 0 0\n"
	if err := os.WriteFile(filepath.Join(l.dir(vm), "mounts"), []byte(mounts), 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(l.dir(vm), "swaps"), []byte(fakeSwaps), 0o600); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(l.dir(vm), "bin", "stat"), []byte(fakeStat(real)), 0o700)
}

// fakeSwaps is a fake guest's swap table: none (the header alone).
const fakeSwaps = "Filename\t\t\t\tType\t\tSize\t\tUsed\t\tPriority\n"

// fakeStat is a fake guest's stat: -f -c %T names a tmpfs under mem, a disk
// anywhere else; any other use is this machine's stat.
func fakeStat(mem string) string {
	realStat, _ := exec.LookPath("stat")
	return fmt.Sprintf(`#!/bin/sh
if [ $# = 4 ] && [ "$1" = -f ] && [ "$2" = -c ] && [ "$3" = %%T ]; then
	case $(cd "$4" 2>/dev/null && pwd -P) in %q|%q/*) echo tmpfs ;; *) echo ext2/ext3 ;; esac
	exit 0
fi
exec %q "$@"
`, mem, mem, realStat)
}

// off is powering off: kill every guest process group (a spawned command's
// own group too, which execWrap names in the guest's $TMPDIR), then the VM is
// gone.
const off = `for p in $(cat "$1/pids" "$1"/tmp/.zm-proc-p* 2>/dev/null); do kill -KILL -$p 2>/dev/null; done; chmod -R u+w "$1" 2>/dev/null; rm -rf "$1"`

// fakeBootMs, a file beside the fake VMs, holds the number of milliseconds
// each Boot takes, so a test can watch an acquire in flight
// (TestAcquireProgressOverAFakeLayer). A VM ended meanwhile stops the wait.
const fakeBootMs = "fake-boot-ms"

func (l dirLayer) slowBoot(vm string) {
	b, _ := os.ReadFile(filepath.Join(filepath.Dir(l.root), fakeBootMs))
	ms, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	for end := time.Now().Add(time.Duration(ms) * time.Millisecond); time.Now().Before(end); time.Sleep(25 * time.Millisecond) {
		if _, err := os.Stat(l.dir(vm)); err != nil {
			return
		}
	}
}

// fakeBootFail, a file beside the fake VMs, makes each Boot print the file to
// its log and exit 1, its guest never answering, as a tart run whose Softnet
// refuses a rule does (bootfail_test.go). The VM stays for Destroy to delete.
const fakeBootFail = "fake-boot-fail"

// Components names the fake layer's programs (componentNamer).
func (dirLayer) Components(isolated bool) string {
	return fmt.Sprintf("fake layer 1.0, isolated %v", isolated)
}

func (l dirLayer) Boot(vm string, _ bool, log io.Writer) (*exec.Cmd, io.WriteCloser, error) {
	l.slowBoot(vm)
	cmd := exec.Command("sh", "-c", `echo $$ >"$1/boot"; cat >/dev/null; `+off, "sh", l.dir(vm))
	if failing := filepath.Join(filepath.Dir(l.root), fakeBootFail); isFile(failing) {
		cmd = exec.Command("sh", "-c", `cat "$2" >&2; exit 1`, "sh", l.dir(vm), failing) // no boot file: the guest never answers
	}
	cmd.Stdout, cmd.Stderr = log, log
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	held, err := cmd.StdinPipe()
	if err != nil {
		return nil, nil, err
	}
	return cmd, held, cmd.Start()
}

func isFile(p string) bool { _, err := os.Stat(p); return err == nil }

func (l dirLayer) running(vm string) bool {
	b, err := os.ReadFile(filepath.Join(l.dir(vm), "boot"))
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	return err == nil && pid > 0 && syscall.Kill(pid, 0) == nil
}

func (l dirLayer) Exec(vm string, stdin io.Reader, stdout, stderr io.Writer, argv ...string) *exec.Cmd {
	guest := filepath.Join(l.dir(vm), "guest")
	a := []string{"-c", `d=$1; shift; [ -f "$d/boot" ] && kill -0 "$(cat "$d/boot")" 2>/dev/null || exit 255
echo $$ >>"$d/pids" || exit 255; exec "$@"`, "sh", l.dir(vm)}
	for _, s := range argv { // the fake guest's own checkout, memory filesystem, mount and swap tables
		s = strings.ReplaceAll(s, guestDir, guest)
		s = strings.ReplaceAll(s, guestMem, filepath.Join(l.dir(vm), "shm"))
		s = strings.ReplaceAll(s, guestSwaps, filepath.Join(l.dir(vm), "swaps"))
		s = strings.ReplaceAll(s, guestMounts, filepath.Join(l.dir(vm), "mounts"))
		if s == fakeRoot {
			s = filepath.Join(l.dir(vm), "root")
		}
		a = append(a, s)
	}
	cmd := exec.Command("sh", a...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
	cmd.Env = append(os.Environ(), "HOME="+filepath.Join(l.dir(vm), "home"), "TMPDIR="+filepath.Join(l.dir(vm), "tmp"), "XDG_RUNTIME_DIR=",
		"PATH="+filepath.Join(l.dir(vm), "bin")+string(os.PathListSeparator)+os.Getenv("PATH"))
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return cmd
}

func (l dirLayer) List() ([]vmInfo, error) {
	es, err := os.ReadDir(l.root)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	var vs []vmInfo
	for _, e := range es {
		vs = append(vs, vmInfo{Name: e.Name(), Running: l.running(e.Name()), Local: true})
	}
	return vs, nil
}

func (l dirLayer) Destroy(vm string, runPID int) {
	if runPID > 0 {
		_ = syscall.Kill(-runPID, syscall.SIGKILL)
	}
	if b, err := os.ReadFile(filepath.Join(l.dir(vm), "boot")); err == nil {
		if pid, _ := strconv.Atoi(strings.TrimSpace(string(b))); pid > 0 {
			_ = syscall.Kill(-pid, syscall.SIGKILL)
		}
	}
	_ = exec.Command("sh", "-c", off, "sh", l.dir(vm)).Run()
}

// The fake layer keeps disks too (persist.go): a kept power-off ends the
// guest's processes and keeps the directory.
const offKept = `for p in $(cat "$1/pids" "$1"/tmp/.zm-proc-p* 2>/dev/null); do kill -KILL -$p 2>/dev/null; done; rm -f "$1/pids" "$1/boot"; rm -rf "$1/shm"`

func (l dirLayer) CloneKept(image, vm string, cpu, memory int) error {
	return l.Clone(image, vm, cpu, memory)
}

func (l dirLayer) BootKept(vm string, _ bool, log io.Writer) (*exec.Cmd, io.WriteCloser, error) {
	if err := l.memFS(vm); err != nil { // a fresh, empty memory filesystem: the last boot's is gone
		return nil, nil, err
	}
	cmd := exec.Command("sh", "-c", `echo $$ >"$1/boot"; cat >/dev/null; `+offKept, "sh", l.dir(vm))
	cmd.Stdout, cmd.Stderr = log, log
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	held, err := cmd.StdinPipe()
	if err != nil {
		return nil, nil, err
	}
	return cmd, held, cmd.Start()
}

func (l dirLayer) PowerOff(vm string, runPID int) {
	if runPID > 0 {
		_ = syscall.Kill(-runPID, syscall.SIGKILL)
	}
	if b, err := os.ReadFile(filepath.Join(l.dir(vm), "boot")); err == nil {
		if pid, _ := strconv.Atoi(strings.TrimSpace(string(b))); pid > 0 {
			_ = syscall.Kill(-pid, syscall.SIGKILL)
		}
	}
	_ = exec.Command("sh", "-c", offKept, "sh", l.dir(vm)).Run()
}

func (dirLayer) KeepsRoot() bool { return true }

func (l dirLayer) DiskDir() string { return filepath.Dir(l.root) }

// FreeGiB is the fake host's free disk: "free-gib" beside the VMs, else this machine's.
func (l dirLayer) FreeGiB() float64 {
	if b, err := os.ReadFile(filepath.Join(filepath.Dir(l.root), "free-gib")); err == nil {
		if f, err := strconv.ParseFloat(strings.TrimSpace(string(b)), 64); err == nil {
			return f
		}
	}
	return freeGiB(l.DiskDir())
}

// The fake layer mounts volumes too (volume.go): a volume is a directory
// beside the VMs; rw and build are a symlink to it under the guest's HOME,
// ro a copy with no write bits, cow a copy of its own (all deleted with the
// VM). A mount asked sealed is never made: the volume must be there.
func (l dirLayer) vol(v string) string { return filepath.Join(filepath.Dir(l.root), "fakevols", v) }

func (l dirLayer) CloneVolumes(image, vm string, cpu, memory int, ms []mount) error {
	if err := l.Clone(image, vm, cpu, memory); err != nil {
		return err
	}
	home := filepath.Join(l.dir(vm), "home")
	for _, m := range ms {
		if filepath.IsAbs(m.Path) {
			return fmt.Errorf("the fake layer mounts under HOME only, not %s", m.Path)
		}
		src, dst := l.vol(m.Vol), filepath.Join(home, m.Path)
		if _, err := os.Stat(src); m.Sealed && err != nil {
			return fmt.Errorf("sealed volume %s is not there: %v", m.Vol, err)
		}
		if _, err := os.Stat(src); m.Mode == volBuild && err == nil {
			return fmt.Errorf("build volume %s is there already", m.Vol)
		}
		if err := os.MkdirAll(src, 0o700); err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			return err
		}
		var err error
		switch m.Mode {
		case volRW, volBuild:
			err = os.Symlink(src, dst)
		case volRO:
			err = exec.Command("sh", "-c", `cp -R "$1" "$2" && chmod -R a-w "$2"`, "sh", src, dst).Run()
		case volCOW:
			err = exec.Command("cp", "-R", src, dst).Run()
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func (dirLayer) OwnMounts(string, string, []mount) error { return nil }

func (l dirLayer) DeleteVolume(v string) error { return os.RemoveAll(l.vol(v)) }

// VolumeInUse: "inuse-<volume>" beside the VMs stands for a container that
// still references it.
func (l dirLayer) VolumeInUse(v string) bool {
	_, err := os.Stat(filepath.Join(filepath.Dir(l.root), "inuse-"+v))
	return err == nil
}

func (dirLayer) SweepLeaks(func(string) bool) {} // a cow copy is under its VM's directory: gone with it

func (l dirLayer) LayerVolumes() ([]string, error) {
	es, err := os.ReadDir(filepath.Join(filepath.Dir(l.root), "fakevols"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	var out []string
	for _, e := range es {
		out = append(out, e.Name())
	}
	return out, err
}

func (l dirLayer) COWVolumes(vm string, ms []mount) []string {
	var out []string
	for _, m := range ms {
		if m.Mode == volCOW {
			out = append(out, "cow:"+filepath.Join(l.dir(vm), "home", m.Path))
		}
	}
	return out
}

// The fake's measurements: "measure-delay" (seconds) beside the VMs slows
// each, "measure-fail" makes each answer nothing.
func (l dirLayer) VolumeBytes(_ string, vs []string) map[string]int64 {
	out := map[string]int64{}
	if b, err := os.ReadFile(filepath.Join(filepath.Dir(l.root), "measure-delay")); err == nil {
		if n, _ := strconv.Atoi(strings.TrimSpace(string(b))); n > 0 {
			time.Sleep(time.Duration(n) * time.Second)
		}
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(l.root), "measure-fail")); err == nil {
		return out
	}
	for _, v := range vs {
		var n int64
		dir := l.vol(v)
		if p, ok := strings.CutPrefix(v, "cow:"); ok {
			dir = p
		} else {
			// As Docker's run --mount type=volume does, the measuring
			// helper makes a named volume that is not there.
			_ = os.MkdirAll(dir, 0o700)
		}
		_ = filepath.Walk(dir, func(_ string, fi os.FileInfo, err error) error {
			if err == nil && fi.Mode().IsRegular() {
				n += fi.Size()
			}
			return nil
		})
		out[v] = n
	}
	return out
}

// fakeBinding installs the test binary as a driver over the fake layer and
// answers its path, a repo holding the conformance probe, and the config.
func fakeBinding(t *testing.T, image string) (string, refdriver.Repo, map[string]string) {
	t.Helper()
	dir, err := os.MkdirTemp("", "zmrt") // short: socket-free, but paths show in logs
	if err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(dir, "state")
	if err := (store{state}).create(); err != nil { // as the driver's first call does
		t.Fatal(err)
	}
	t.Cleanup(func() { // after endStarted: no supervisor boots a VM after the listing
		l := dirLayer{filepath.Join(state, "fakevms")}
		vs, _ := l.List()
		for _, v := range vs {
			l.Destroy(v.Name, 0) // a kept disk's: no supervisor ends it
		}
		os.RemoveAll(dir)
	})
	endAtCleanup(t, state)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	drv := filepath.Join(dir, "zm-run-tart")
	if err := os.WriteFile(drv, []byte(fmt.Sprintf("#!/bin/sh\nexec %q %s\n", self, flagDriverTest)), 0o755); err != nil {
		t.Fatal(err)
	}
	repo, err := refdriver.NewRepo(dir, conformance.ProbeScript)
	if err != nil {
		t.Fatal(err)
	}
	return drv, repo, map[string]string{"vm": "fake", "image": image, "remote": repo.Remote, "state_dir": state,
		"boot_timeout_s": "10", "persist_min_free_gb": "0"} // the floor is this machine's disk: TestKeptDiskFloor sets one
}

// guestForbid answers an address that opens on this machine but not from the
// fake layer's guest (which is this machine): a listener on this machine's
// LAN address whose port the guest's nc refuses (refdriver.Firewall, first on
// PATH). A --net-forbid that does not open here, or a loopback one (which
// means the guest itself inside a VM), would be a SKIP, proving nothing.
func guestForbid(t *testing.T) string {
	t.Helper()
	ip := refdriver.RequireLANAddr(t, "no forbidden address the suite tries")
	l, err := net.Listen("tcp", net.JoinHostPort(ip, "0"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for c, err := l.Accept(); err == nil; c, err = l.Accept() {
			c.Close()
		}
	}()
	bin := refdriver.FirewallCI(t, t.TempDir(), l.Addr().String())
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return l.Addr().String()
}

// reclaimIn is the fake layer's reclaim window: it must outlast an acquire
// and a status on a loaded CI runner (a 3 s window closed first there), and
// the layer reads it to the second, so it may close up to 1 s early.
const reclaimIn = 10 * time.Second

// The whole SPEC 20 suite against the driver over the fake layer: the
// supervisor's TTL, release, reclaim and process-group handling, proved in CI
// with no VM. (zm-run-conformance against Tart is the live oracle.)
func TestConformanceOverAFakeLayer(t *testing.T) {
	if testing.Short() {
		t.Skip("runs the full conformance suite")
	}
	drv, repo, m := fakeBinding(t, "linux-img")
	// The fake layer's guest is this machine: an open local port stands for
	// the internet it reaches, a closed one for what it must not.
	open, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer open.Close()
	go func() {
		for {
			c, err := open.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	forbid := guestForbid(t)
	opt := conformance.Options{Kind: "run", Target: "fake", Config: m, Ref: "main", Commit: repo.Commit,
		Repo: repo.Remote, Script: "probe.sh", ShortTTL: 2, Grace: 3 * time.Second, RunWait: 30 * time.Second,
		ReclaimConfig: map[string]string{"window": "{window}", "drain_deadline_s": "0"}, ReclaimIn: reclaimIn,
		NetAllow: []string{open.Addr().String()}, NetForbid: []string{forbid},
		NetResolve: []string{"localhost:" + strconv.Itoa(open.Addr().(*net.TCPAddr).Port)}} // hermetic: no github.com
	for _, g := range conformance.Groups { // acquire-progress needs a slow boot: TestAcquireProgressOverAFakeLayer
		if g != "boot" && g != "acquire-progress" {
			opt.Only = append(opt.Only, g)
		}
	}
	res := conformance.Run(context.Background(), drv, opt)
	out := conformance.Format(res)
	if n := conformance.Failed(res); n > 0 {
		t.Fatalf("%d failed:\n%s", n, out)
	}
	// Two checks skip, by name: the dirty-worktree check (runs never start
	// from a worktree) and the publish group (this driver serves boxes and
	// does not list run-publish). Any other skip is a check that stopped running.
	expected := map[string]string{
		"a dirty working tree does not leak in": "no --worktree",
		"a run's publish":                       "describe lists no run-publish capability",
	}
	for _, r := range conformance.SkippedResults(res) {
		if why, ok := expected[r.Name]; !ok || r.Detail != why {
			t.Fatalf("unexpected skip %q (%s):\n%s", r.Name, r.Detail, out)
		}
		delete(expected, r.Name)
	}
	if len(expected) > 0 {
		t.Fatalf("expected skips did not happen: %v:\n%s", expected, out)
	}
	t.Log("\n" + out)
}

// The acquire-progress group over the fake layer, each boot taking 6 s (past
// the suite's inFlightKnown), so every in-flight check runs: progress names
// the phase while the VM boots, and cancel ends it. No check may skip.
func TestAcquireProgressOverAFakeLayer(t *testing.T) {
	if testing.Short() {
		t.Skip("boots take 6 s")
	}
	drv, repo, m := fakeBinding(t, "linux-img")
	if err := os.WriteFile(filepath.Join(m["state_dir"], fakeBootMs), []byte("6000"), 0o600); err != nil {
		t.Fatal(err)
	}
	opt := conformance.Options{Kind: "run", Target: "fake", Config: m, Ref: "main", Commit: repo.Commit,
		Repo: repo.Remote, Script: "probe.sh", RunWait: 30 * time.Second, Only: []string{"acquire-progress"}}
	res := conformance.Run(context.Background(), drv, opt)
	out := conformance.Format(res)
	if n := conformance.Failed(res); n > 0 {
		t.Fatalf("%d failed:\n%s", n, out)
	}
	if n := conformance.Skipped(res); n > 0 {
		t.Fatalf("%d skipped, want none:\n%s", n, out)
	}
	if !strings.Contains(out, "PASS  progress of an acquire in flight answers starting") || !strings.Contains(out, "PASS  cancel of an acquire in flight") {
		t.Fatalf("the in-flight checks did not run:\n%s", out)
	}
	t.Log("\n" + out)
}

// Apple's cap is per host: a third macOS VM is refused, whoever runs the
// other two, and nothing is left behind.
func TestMacOSSlotsAreCappedPerHost(t *testing.T) {
	_, repo, m := fakeBinding(t, "mac-img")
	m["target"], m["macos_slots"] = "t", "5"
	acquire := func(key string) (map[string]any, error) {
		return call(t, m, "acquire", "", map[string]any{"request_id": key, "ttl_s": 60, "ref": "main",
			"commit": repo.Commit, "script": "probe.sh", "args": []string{"sleep", "60"}})
	}
	var handles []string
	for _, k := range []string{"a", "b"} {
		a, err := acquire(k)
		if err != nil {
			t.Fatalf("acquire %s: %v", k, err)
		}
		handles = append(handles, a["handle"].(string))
	}
	if _, err := acquire("c"); code(err) != "unavailable" {
		t.Fatalf("a third macOS run: %v, want unavailable", err)
	}
	d, _ := call(t, m, "describe", "", nil)
	if d["available"] != false || !strings.Contains(fmt.Sprint(d["why"]), "no free darwin slot") {
		t.Errorf("describe with both slots taken: %v", d)
	}
	for _, h := range handles {
		if rel, err := call(t, m, "release", h, nil); err != nil || rel["released"] != true {
			t.Errorf("release %s: %v %v", h, rel, err)
		}
	}
	if vs, _ := (dirLayer{filepath.Join(m["state_dir"], "fakevms")}).List(); len(vs) != 0 {
		t.Errorf("VMs left after release: %v", vs)
	}
}

// linux_slots is how many Linux boxes of the state directory run at once
// (potluck host --linux-slots): past it an acquire is unavailable and
// describe says so in the words potluck host reads (protocol.NoFreeSlot).
func TestLinuxSlotsCapConcurrentBoxes(t *testing.T) {
	_, repo, m := fakeBinding(t, "img")
	m["target"], m["linux_slots"] = "t", "3"
	acquire := func(key string) (map[string]any, error) {
		return call(t, m, "acquire", "", map[string]any{"request_id": key, "ttl_s": 60, "ref": "main",
			"commit": repo.Commit, "script": "probe.sh", "args": []string{"sleep", "60"}})
	}
	var handles []string
	for _, k := range []string{"a", "b", "c"} {
		a, err := acquire(k)
		if err != nil {
			t.Fatalf("acquire %s of 3 slots: %v", k, err)
		}
		handles = append(handles, a["handle"].(string))
	}
	_, err := acquire("d")
	if code(err) != "unavailable" || !strings.Contains(err.Error(), protocol.NoFreeSlot("linux")) {
		t.Fatalf("a fourth Linux run: %v, want unavailable: %s", err, protocol.NoFreeSlot("linux"))
	}
	// Marked as only full for now, so a caller waits and asks again (roomwait).
	if bz, ok := noRoomData(err); !ok || bz.Why != protocol.NoFreeSlot("linux")+" (3 in use)" || bz.RetryS != 0 {
		t.Errorf("a fourth Linux run's data: %+v %v, want no_room", bz, ok)
	}
	d, _ := call(t, m, "describe", "", nil)
	if d["available"] != false || !strings.Contains(fmt.Sprint(d["why"]), protocol.NoFreeSlot("linux")+" (3 in use)") {
		t.Errorf("describe with every slot taken: %v", d)
	}
	for _, h := range handles {
		if rel, err := call(t, m, "release", h, nil); err != nil || rel["released"] != true {
			t.Errorf("release %s: %v %v", h, rel, err)
		}
	}
	if d, _ := call(t, m, "describe", "", nil); d["available"] != true {
		t.Errorf("describe after release: %v", d)
	}
}

func TestUnknownLayerIsRefused(t *testing.T) {
	m := cfg(t)
	m["vm"] = "qemu"
	if _, err := call(t, m, "describe", "", nil); code(err) != "bad_request" || !strings.Contains(err.Error(), layerNames()) {
		t.Errorf("an unknown vm layer: %v, want bad_request naming the layers", err)
	}
}

// The SPEC 20 box suite over the fake layer: exec, its caps and timeout,
// files kept within a box and wiped between boxes, the checkout.
func TestBoxConformanceOverAFakeLayer(t *testing.T) {
	if testing.Short() {
		t.Skip("runs the full conformance suite")
	}
	drv, repo, m := fakeBinding(t, "linux-img")
	delete(m, "image")
	m["kind"], m["size"], m["cpu"], m["memory"] = "box", "small", "2", "2048"
	m["images"] = "fake/linux@sha256:" + strings.Repeat("ab", 32) + ",fake/linux2@sha256:" + strings.Repeat("cd", 32)
	// The net checks reach a box through exec (the fake guest is this machine).
	open, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer open.Close()
	go func() {
		for c, err := open.Accept(); err == nil; c, err = open.Accept() {
			c.Close()
		}
	}()
	forbid := guestForbid(t)
	opt := conformance.Options{Kind: "box", Target: "fake", Config: m, Ref: "main", Commit: repo.Commit,
		Repo: repo.Remote, LeftoverRoots: []string{fakeRoot}, ShortTTL: 2, Grace: 3 * time.Second, RunWait: 30 * time.Second,
		ReclaimConfig: map[string]string{"window": "{window}", "drain_deadline_s": "0"}, ReclaimIn: reclaimIn,
		NetAllow: []string{open.Addr().String()}, NetForbid: []string{forbid},
		NetResolve: []string{"localhost:" + strconv.Itoa(open.Addr().(*net.TCPAddr).Port)}} // hermetic: no github.com
	res := conformance.Run(context.Background(), drv, opt)
	out := conformance.Format(res)
	if n := conformance.Failed(res); n > 0 {
		t.Fatalf("%d failed:\n%s", n, out)
	}
	if !strings.Contains(out, "PASS  a box reaches "+open.Addr().String()) || !strings.Contains(out, "PASS  a box resolves localhost (DNS)") ||
		!strings.Contains(out, "PASS  a box cannot reach "+forbid) {
		t.Fatalf("the net checks did not run in a box:\n%s", out)
	}
	if !strings.Contains(out, "PASS  start brings the same disk back") || !strings.Contains(out, "PASS  a persistent box's deadline stops it") {
		t.Fatalf("the kept-disk checks did not run (zm-eakh.12.5.23):\n%s", out)
	}
	if !strings.Contains(out, "PASS  a volume outlives its box") || !strings.Contains(out, "PASS  a cow box's writes are its own") {
		t.Fatalf("the named-volume checks did not run (zm-eakh.12.32.1):\n%s", out)
	}
	if !strings.Contains(out, "PASS  a sealed volume refuses rw and build") || !strings.Contains(out, "PASS  a build box's processes end before its volume is sealed") {
		t.Fatalf("the sealed-volume checks did not run (zm-eakh.12.65.4):\n%s", out)
	}
	t.Log("\n" + out)
}

// A command that leaves a daemon holding its output (setsid escapes the
// process group) is answered as soon as it exits, with its own exit code; a
// NUL in argv is bad_request.
func TestBoxExecIsNotHeldByADaemon(t *testing.T) {
	_, _, m := fakeBinding(t, "linux-img")
	delete(m, "image")
	m["target"], m["kind"], m["size"], m["cpu"], m["memory"] = "t", "box", "small", "1", "512"
	m["images"] = "fake/linux@sha256:" + strings.Repeat("ab", 32)
	a, err := call(t, m, "acquire", "", map[string]any{"request_id": "k", "ttl_s": 120})
	if err != nil {
		t.Fatal(err)
	}
	h := a["handle"].(string)
	defer func() { _, _ = call(t, m, "release", h, nil) }()
	start := time.Now()
	pidf := filepath.Join(t.TempDir(), "daemon.pid")
	defer func() { // a real VM's end kills it; the fake has none
		if b, err := os.ReadFile(pidf); err == nil {
			if pid, _ := strconv.Atoi(strings.TrimSpace(string(b))); pid > 0 {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
	}()
	daemon := `perl -e 'use POSIX; fork and exit; POSIX::setsid(); open(my $f, ">", $ARGV[0]); print $f "$$\n"; close $f; sleep 30' "$1" & echo hi; exit 3`
	res, err := call(t, m, "exec", h, map[string]any{"argv": []string{"sh", "-c", daemon, "sh", pidf}, "timeout_s": 20})
	if err != nil || res["exit"] != 3.0 || res["stdout"] != "hi\n" {
		t.Fatalf("exec: %v %v, want exit 3, stdout hi", res, err)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("the answer waited %s on a daemon holding the output", took)
	}
	if _, err := call(t, m, "exec", h, map[string]any{"argv": []string{"echo", "a\x00b"}}); code(err) != "bad_request" {
		t.Errorf("a NUL in argv: %v, want bad_request", err)
	}
}

// Progress names the phase of a booting VM, and a cancel then ends that VM:
// the acquire answers conflict and no VM is left.
func TestCancelEndsABootingVM(t *testing.T) {
	for _, kind := range []string{"run", "box"} {
		t.Run(kind, func(t *testing.T) { cancelBooting(t, kind) })
	}
}

func cancelBooting(t *testing.T, kind string) {
	_, repo, m := fakeBinding(t, "linux-img")
	m["target"] = "t"
	acq := map[string]any{"request_id": "k", "ttl_s": 60, "ref": "main", "commit": repo.Commit, "script": "probe.sh"}
	if kind == "box" {
		delete(m, "image")
		m["kind"], m["size"], m["cpu"], m["memory"] = "box", "small", "1", "512"
		m["images"] = "fake/linux@sha256:" + strings.Repeat("ab", 32)
		delete(acq, "script")
	}
	if err := os.WriteFile(filepath.Join(m["state_dir"], fakeBootMs), []byte("20000"), 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := call(t, m, "acquire", "", acq)
		done <- err
	}()
	var phase string
	for end := time.Now().Add(10 * time.Second); phase != "booting"; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(end) {
			t.Fatalf("progress never said booting (last %q)", phase)
		}
		if p, err := call(t, m, "progress", "", map[string]any{"request_id": "k"}); err == nil {
			if p["state"] != "starting" {
				t.Fatalf("progress %v while booting", p)
			}
			phase, _ = p["phase"].(string)
		}
	}
	c, err := call(t, m, "cancel", "", map[string]any{"request_id": "k"})
	if err != nil || c["cancelled"] != true {
		t.Fatalf("cancel: %v %v", c, err)
	}
	select {
	case err := <-done:
		if e, ok := err.(refusal); !ok || e.code != "conflict" {
			t.Fatalf("the cancelled acquire answered %v, want conflict", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the cancelled acquire did not answer")
	}
	if p, err := call(t, m, "progress", "", map[string]any{"request_id": "k"}); err != nil || p["state"] != "cancelled" {
		t.Fatalf("progress after the cancel: %v %v", p, err)
	}
	for end := time.Now().Add(10 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		vs, _ := (dirLayer{filepath.Join(m["state_dir"], "fakevms")}).List()
		if len(vs) == 0 {
			break
		}
		if time.Now().After(end) {
			t.Fatalf("VMs left after the cancel: %v", vs)
		}
	}
}

// CleanSeal runs the driver's own sealCleanScript on the volume's directory,
// on the host. "keep-fail" beside the VMs makes it fail.
func (l dirLayer) CleanSeal(_, v string, keep []string) (int64, error) {
	if _, err := os.Stat(filepath.Join(filepath.Dir(l.root), "keep-fail")); err == nil {
		return 0, fmt.Errorf("keep-fail")
	}
	out, err := exec.Command("sh", append([]string{"-c", sealCleanScript, "sh", l.vol(v)}, sealCleanArgs(keep)...)...).CombinedOutput()
	if err != nil {
		return 0, fmt.Errorf("%v: %s", err, out)
	}
	b, ok := l.VolumeBytes("", []string{v})[v]
	if !ok {
		return 0, fmt.Errorf("measure-fail")
	}
	return b, nil
}
