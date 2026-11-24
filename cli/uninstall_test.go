package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/tailnet/tailnettest"
	"github.com/sengirunners/potluck/softnet"
)

// lender is a person's machine as install.sh and a few commands leave it: a
// temp HOME with a startup file that holds potluck's PATH line among the
// person's own, an install directory with the programs (and one file that
// is not Potluck's), and no real update job, host override file or daemon
// record.
type lender struct {
	t     *testing.T
	home  string // the user's HOME
	bin   string
	rc    string
	job   *jobEnv
	other string // a file in the install directory that is not Potluck's
}

func newLender(t *testing.T) *lender {
	t.Helper()
	e := newJobEnv(t, "darwin")
	l := &lender{t: t, home: e.home, bin: e.bin, job: e}
	l.rc = filepath.Join(l.home, ".zshrc")
	rc := "export EDITOR=vim\n\n# Potluck (install.sh): put potluck on PATH\nexport PATH=\"$HOME/.local/bin:$PATH\"\nalias ll='ls -l'\n"
	if err := os.WriteFile(l.rc, []byte(rc), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, n := range append(InstalledPrograms, "potluck.prev", "potluck-driver-tart.prev") {
		if err := os.WriteFile(filepath.Join(l.bin, n), []byte("x"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	l.other = filepath.Join(l.bin, "potluck-mine")
	if err := os.WriteFile(l.other, []byte("mine"), 0o755); err != nil {
		t.Fatal(err)
	}
	oldOverride := hostOverrideFile
	override := filepath.Join(t.TempDir(), "host-override.json")
	hostOverrideFile = func() string { return override }
	t.Cleanup(func() { hostOverrideFile = oldOverride })
	t.Setenv("POTLUCK_DAEMONS_DIR", filepath.Join(l.home, "records"))
	// No Softnet of this Mac's: a test that wants one makes it (newSoftnetMac).
	oldStore, oldGOOS := softnetStore, softnetGOOS
	oldDownloads := softnetDownloadDir
	softnetStore = func() softnet.Store { return softnet.Store{Dir: filepath.Join(t.TempDir(), "Softnet")} }
	softnetDownloadDir = func() (string, error) { return filepath.Join(l.home, "Library", "Caches", "potluck", "softnet"), nil }
	softnetGOOS = "darwin"
	t.Cleanup(func() { softnetStore, softnetGOOS, softnetDownloadDir = oldStore, oldGOOS, oldDownloads })
	oldGone := processGone
	processGone = func(int) bool { return true } // the daemon under test shares this process
	t.Cleanup(func() { processGone = oldGone })
	return l
}

func (l *lender) exists(p string) bool {
	l.t.Helper()
	_, err := os.Lstat(p)
	return err == nil
}

// uninstall runs potluck uninstall with stdin.
func (l *lender) uninstall(stdin string, args ...string) (string, int) {
	l.t.Helper()
	var o, e bytes.Buffer
	code := Run(context.Background(), append([]string{"uninstall"}, args...), strings.NewReader(stdin), &o, &e)
	return o.String() + e.String(), code
}

// A run that only prints says what it would do, in order, and what it
// leaves, and changes nothing.
func TestUninstallDryRunChangesNothing(t *testing.T) {
	l := newLender(t)
	tn := tailnettest.New()
	m := startMachine(t, tn, "mini", "judy@example.com")
	m.ok(t, "create", "friends")
	t.Setenv(EnvHome, m.home)
	if out, code := l.job.potluck("enable"); code != 0 {
		t.Fatalf("enable: %s", out)
	}

	out, code := l.uninstall("", "--dry-run")
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	order := []string{"stop sharing", "stop the daemon", "remove the daily update job", "remove the lines install.sh added", "remove the home", "remove the programs in"}
	at := -1
	for _, w := range order {
		i := strings.Index(out, w)
		if i < 0 || i < at {
			t.Fatalf("want %q after the earlier steps:\n%s", w, out)
		}
		at = i
	}
	for _, w := range []string{"friends", "this one last", "not removed by potluck uninstall:", "Tailscale"} {
		if !strings.Contains(out, w) {
			t.Errorf("output lacks %q:\n%s", w, out)
		}
	}
	if _, err := client.NewUnix(filepath.Join(m.home, "potluck.sock")).Health(context.Background()); err != nil {
		t.Errorf("the daemon stopped on a dry run: %v", err)
	}
	for _, p := range []string{filepath.Join(l.bin, "potluck"), l.job.plist(), m.home} {
		if !l.exists(p) {
			t.Errorf("%s is gone after a dry run", p)
		}
	}
	if b, _ := os.ReadFile(l.rc); !strings.Contains(string(b), "put potluck on PATH") {
		t.Errorf("the startup file changed on a dry run:\n%s", b)
	}
}

// --dry-run --json is the plan as data: the steps, the pools this machine
// runs, what is lost for good, and the arguments that run it.
func TestUninstallDryRunJSON(t *testing.T) {
	newLender(t)
	m := startMachine(t, tailnettest.New(), "mini", "judy@example.com")
	m.ok(t, "create", "friends")
	t.Setenv(EnvHome, m.home)
	run := func(args ...string) uninstallAnswer {
		t.Helper()
		answer, words, code := softnetRunJSON(t, append([]string{"uninstall", "--dry-run"}, args...)...)
		var a uninstallAnswer
		if err := json.Unmarshal(answer, &a); code != 0 || err != nil {
			t.Fatalf("exit %d (%v): %s\n%s", code, err, answer, words)
		}
		return a
	}
	a := run()
	if len(a.Done) != 0 || len(a.Left) == 0 || !slices.Equal(a.PoolsRun, []string{"friends"}) || !strings.Contains(a.Warning, "friends") ||
		slices.ContainsFunc(a.NotRemoved, func(s string) bool { return strings.Contains(s, "friends") }) {
		t.Fatalf("plan: %+v", a)
	}
	if a.Remove == nil || a.Remove.Detail != a.Warning ||
		!slices.Equal(a.Remove.Args, []string{"--json", "uninstall", "--yes", "--no-sudo"}) {
		t.Fatalf("remove: %+v", a.Remove)
	}
	if k := run("--keep-home"); k.Warning != "" || k.Remove == nil || !slices.Contains(k.Remove.Args, "--keep-home") {
		t.Fatalf("--keep-home: %+v", k)
	}
	// A daemon that does not run cannot name the pools: the warning says so.
	home, err := os.MkdirTemp("", "pl")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(home)
	if err := os.WriteFile(filepath.Join(home, "registry.db"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvHome, home)
	if n := run(); len(n.PoolsRun) != 0 || !strings.Contains(n.Warning, "none can be named") {
		t.Fatalf("no daemon: %+v", n)
	}
}

// A setup run going on is stopped first, so it cannot share again or write
// the home again after.
func TestUninstallStopsASetupRun(t *testing.T) {
	l := newLender(t)
	home, err := os.MkdirTemp("", "pl")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(home)
	if err := os.WriteFile(filepath.Join(home, "registry.db"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	lock, err := os.Create(filepath.Join(home, hostLockName))
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	// A run and a program it started, in a session of their own; the run
	// holds the lock as it does in hostSpawn.
	run := exec.Command("sh", "-c", "sleep 60 & wait")
	run.ExtraFiles = []*os.File{lock}
	run.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := run.Start(); err != nil {
		t.Fatal(err)
	}
	lock.Close()
	ended := make(chan struct{})
	go func() { _ = run.Wait(); close(ended) }()
	t.Cleanup(func() { _ = syscall.Kill(-run.Process.Pid, syscall.SIGKILL); <-ended })
	for _, n := range []string{hostPIDName, hostBgPIDName} {
		if err := os.WriteFile(filepath.Join(home, n), []byte(strconv.Itoa(run.Process.Pid)+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv(EnvHome, home)
	answer, words, code := softnetRunJSON(t, "uninstall", "--yes", "--no-sudo")
	var a uninstallAnswer
	if err := json.Unmarshal(answer, &a); code != 0 || err != nil || len(a.Done) == 0 || !strings.Contains(a.Done[0], "stop the setup run") {
		t.Fatalf("exit %d (%v): %s\n%s", code, err, answer, words)
	}
	select {
	case <-ended:
	case <-time.After(5 * time.Second):
		t.Fatal("the run goes on")
	}
	if l.exists(home) {
		t.Error("the home is still there")
	}
}

// The host lock is uninstall's while its steps run; a pid a killed run left
// is gone, so a second uninstall never signals that process.
func TestUninstallHoldsTheHostLock(t *testing.T) {
	l := newLender(t)
	m := startMachine(t, tailnettest.New(), "mini", "judy@example.com")
	t.Setenv(EnvHome, m.home)
	if err := os.WriteFile(filepath.Join(m.home, hostPIDName), []byte(strconv.Itoa(os.Getppid())), 0o600); err != nil {
		t.Fatal(err)
	}
	// Asked in the step that stops the daemon.
	asked := false
	processGone = func(int) bool {
		asked = true
		if !hostBusy(m.home) {
			t.Error("a host run could start during the removal")
		}
		if l.exists(filepath.Join(m.home, hostPIDName)) {
			t.Error("the stale pid is still there")
		}
		return true
	}
	if out, code := l.uninstall("", "--yes"); code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if !asked {
		t.Fatal("no step looked")
	}
}

// A run that holds the lock without naming itself, and a daemon that came
// up after the plan, stop the removal before it touches the home.
func TestHoldHostRefuses(t *testing.T) {
	lockFile := func(home, name string) {
		t.Helper()
		f, err := os.Create(filepath.Join(home, name))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { f.Close() })
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
			t.Fatal(err)
		}
	}
	for name, want := range map[string]string{hostLockName: "another host run", config.FileHomeLock: "started the daemon"} {
		home := t.TempDir()
		lockFile(home, name)
		release, err := holdHost(context.Background(), uninstallPlan{home: home})
		release()
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// A pool joined after the plan would keep this machine's token.
	p := uninstallPlan{home: t.TempDir(), running: true, joined: []string{"studio"},
		joinedNow: func(context.Context) ([]string, error) { return []string{"studio", "friends"}, nil }}
	release, err := holdHost(context.Background(), p)
	release()
	if err == nil || !strings.Contains(err.Error(), "joined friends meanwhile") || hostBusy(p.home) {
		t.Errorf("a pool joined meanwhile: %v", err)
	}
}

// A host run that waited for the lock while uninstall removed the home gets
// none.
func TestTakeLockRefusesARemovedHome(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, hostLockName)
	first, err := takeLock(path)
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan error, 1)
	go func() {
		f, err := takeLock(path)
		if err == nil {
			f.Close()
		}
		got <- err
	}()
	time.Sleep(200 * time.Millisecond)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	first.Close()
	if err := <-got; err == nil {
		// The waiter may have opened the path only after its removal.
		if _, serr := os.Stat(path); serr != nil {
			t.Fatal("the lock of a removed file was given")
		}
	}
}

// What a step could not do though it went on goes with it in done: a startup
// file that cannot be written.
func TestUninstallSaysWhatAStepCouldNotDo(t *testing.T) {
	l := newLender(t)
	t.Setenv(EnvHome, filepath.Join(l.home, "none"))
	if err := os.Chmod(l.home, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(l.home, 0o700) })
	if f, err := os.CreateTemp(l.home, "x"); err == nil { // root writes anywhere
		f.Close()
		t.Skip("a read-only directory does not stop this user")
	}
	answer, words, code := softnetRunJSON(t, "uninstall", "--yes", "--no-sudo")
	var a uninstallAnswer
	if err := json.Unmarshal(answer, &a); code != 0 || err != nil || !slices.ContainsFunc(a.Done, func(s string) bool { return strings.Contains(s, "could not edit") }) {
		t.Fatalf("exit %d (%v): %s\n%s", code, err, answer, words)
	}
}

// The answers the Mac app's decoder is checked against (macos/PotluckMenu/test.sh)
// have this command's fields, and no others.
func TestUninstallAppFixtures(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "macos", "PotluckMenu", "test", "answers", "uninstall-*.json"))
	if err != nil || len(files) < 4 {
		t.Fatalf("fixtures %v (%v)", files, err)
	}
	for _, p := range files {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		d := json.NewDecoder(bytes.NewReader(b))
		d.DisallowUnknownFields()
		var a uninstallAnswer
		if err := d.Decode(&a); err != nil || a.Done == nil || a.Left == nil || a.NotRemoved == nil || a.PoolsRun == nil {
			t.Errorf("%s: %v %+v", p, err, a)
		}
	}
}

// It asks first: anything but yes removes nothing; --yes does not ask.
func TestUninstallAsksFirst(t *testing.T) {
	l := newLender(t)
	m := startMachine(t, tailnettest.New(), "mini", "judy@example.com")
	t.Setenv(EnvHome, m.home)
	for _, in := range []string{"", "no\n", "y\n"} {
		out, code := l.uninstall(in)
		if code == 0 || !strings.Contains(out, "nothing was removed") {
			t.Fatalf("stdin %q: exit %d\n%s", in, code, out)
		}
	}
	for _, p := range []string{filepath.Join(l.bin, "potluck"), m.home} {
		if !l.exists(p) {
			t.Errorf("%s was removed without a yes", p)
		}
	}
}

// One run takes off what install.sh and the first commands put on the
// machine, and only that: the programs and their .prev copies, the PATH line
// (not the person's own lines), the update job, the home and the daemon.
func TestUninstallRemovesWhatPotluckPut(t *testing.T) {
	l := newLender(t)
	m := startMachine(t, tailnettest.New(), "mini", "judy@example.com")
	m.ok(t, "create", "friends")
	t.Setenv(EnvHome, m.home)
	if out, code := l.job.potluck("enable"); code != 0 {
		t.Fatalf("enable: %s", out)
	}

	driverState := filepath.Join(l.home, ".potluck-driver-tart")
	if err := os.MkdirAll(filepath.Join(driverState, "failed"), 0o755); err != nil {
		t.Fatal(err)
	}
	out, code := l.uninstall("yes\n")
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if !l.exists(driverState) || !strings.Contains(out, "rm -r") {
		t.Errorf("the driver's records are kept, with the command to remove them:\n%s", out)
	}
	if !strings.Contains(out, "potluck daemon stopped") || !strings.Contains(out, "daily update removed") {
		t.Errorf("output:\n%s", out)
	}
	for _, n := range append(InstalledPrograms, "potluck.prev", "potluck-driver-tart.prev") {
		if l.exists(filepath.Join(l.bin, n)) {
			t.Errorf("%s is still installed", n)
		}
	}
	if !l.exists(l.other) {
		t.Error("it removed a file that is not Potluck's")
	}
	if l.exists(m.home) {
		t.Error("the home is still there")
	}
	if l.exists(l.job.plist()) {
		t.Error("the update job file is still there")
	}
	b, err := os.ReadFile(l.rc)
	if err != nil {
		t.Fatal(err)
	}
	if want := "export EDITOR=vim\nalias ll='ls -l'\n"; string(b) != want {
		t.Errorf("startup file:\n%q\nwant\n%q", b, want)
	}
	if _, err := client.NewUnix(filepath.Join(m.home, "potluck.sock")).Health(context.Background()); err == nil {
		t.Error("the daemon still answers")
	}

	// Run again: nothing left of Potluck's, and no error.
	if out, code := l.uninstall("", "--yes"); code != 0 || !strings.Contains(out, "nothing of potluck's is on this machine") {
		t.Errorf("second run: exit %d\n%s", code, out)
	}
}

// --keep-home leaves the home (the pools this machine runs) and the daemon's
// data; the daemon still stops.
func TestUninstallKeepHome(t *testing.T) {
	l := newLender(t)
	m := startMachine(t, tailnettest.New(), "mini", "judy@example.com")
	m.ok(t, "create", "friends")
	t.Setenv(EnvHome, m.home)
	out, code := l.uninstall("", "--yes", "--keep-home")
	if code != 0 || strings.Contains(out, "remove the home") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if !l.exists(filepath.Join(m.home, "registry.db")) {
		t.Error("--keep-home removed the home")
	}
	if l.exists(filepath.Join(l.bin, "potluck")) {
		t.Error("the programs are still installed")
	}
	if _, err := client.NewUnix(filepath.Join(m.home, "potluck.sock")).Health(context.Background()); err == nil {
		t.Error("the daemon still answers")
	}
}

// A machine that joined a pool leaves it, and the pool's machine revokes it.
func TestUninstallLeavesJoinedPools(t *testing.T) {
	l := newLender(t)
	tn := tailnettest.New()
	a := startMachine(t, tn, "mini", "judy@example.com")
	b := startMachine(t, tn, "laptop", "ana@example.com")
	a.ok(t, "create", "friends")
	code := memberCodeRe.FindStringSubmatch(a.ok(t, "invite", "ana"))
	if code == nil {
		t.Fatal("no member code")
	}
	b.ok(t, "join", code[1])
	if out := b.ok(t, "remotes"); !strings.Contains(out, "friends") {
		t.Fatalf("remotes:\n%s", out)
	}
	t.Setenv(EnvHome, b.home)

	out, c := l.uninstall("", "--yes")
	if c != 0 || !strings.Contains(out, "left friends") || strings.Contains(out, "did not answer") {
		t.Fatalf("exit %d:\n%s", c, out)
	}
}

// With no daemon running it starts none: it says what it could not do, and
// still removes the files.
func TestUninstallWithNoDaemonStartsNone(t *testing.T) {
	l := newLender(t)
	home, err := os.MkdirTemp("", "pl")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(home)
	if err := os.WriteFile(filepath.Join(home, "registry.db"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvHome, home)
	out, code := l.uninstall("", "--yes")
	if code != 0 || !strings.Contains(out, "potluck daemon is not running") || !strings.Contains(out, "ask each pool's owner to revoke") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if l.exists(home) || l.exists(filepath.Join(l.bin, "potluck")) {
		t.Error("the files were not removed")
	}
	if l.exists(filepath.Join(l.home, "records")) {
		t.Error("it started a daemon, which wrote its record")
	}
}

// A POTLUCK_HOME that names a folder that is not a Potluck home, or the
// person's own home folder, is never removed.
func TestUninstallRefusesAFolderThatIsNotAPotluckHome(t *testing.T) {
	l := newLender(t)
	for name, dir := range map[string]string{"user's home": l.home, "a folder of files": filepath.Join(l.home, "Documents")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "thesis.txt"), []byte("mine"), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Setenv(EnvHome, dir)
		out, code := l.uninstall("", "--yes")
		if code == 0 || !strings.Contains(out, "not removing it") {
			t.Errorf("%s: exit %d\n%s", name, code, out)
		}
		if !l.exists(filepath.Join(dir, "thesis.txt")) || !l.exists(filepath.Join(l.bin, "potluck")) {
			t.Errorf("%s: something was removed", name)
		}
	}
}

// On a Mac the managed Softnet goes first, through the admin step, before the
// program that runs that step is removed.
func TestUninstallRemovesTheManagedSoftnetBeforeThePrograms(t *testing.T) {
	l := newLender(t)
	mac := newSoftnetMac(t, "15.6")
	mac.asRoot = true
	if out, code := softnetRun(t, "softnet", "install", "--admin-step", "--candidate", "--from", mac.file); code != 0 {
		t.Fatalf("install: exit %d: %s", code, out)
	}
	mac.asRoot = false
	softnetSudo = func(_ context.Context, _ string, args []string, _, _ io.Writer) error {
		if !l.exists(filepath.Join(l.bin, "potluck")) {
			t.Error("the admin step ran after the programs were removed")
		}
		mac.asRoot = true
		defer func() { mac.asRoot = false }()
		if out, code := softnetRun(t, args...); code != 0 {
			t.Errorf("admin step: exit %d: %s", code, out)
		}
		return nil
	}
	home, err := os.MkdirTemp("", "pl")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(home)
	if err := os.WriteFile(filepath.Join(home, "registry.db"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvHome, home) // a home with no daemon running

	out, code := l.uninstall("", "--yes")
	if code != 0 || !strings.Contains(out, "remove the managed Softnet") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if l.exists(mac.store.Dir) {
		t.Error("the managed Softnet is still there")
	}
	if l.exists(filepath.Join(l.bin, "potluck")) {
		t.Error("the programs are still installed")
	}
	if strings.Contains(out, "run potluck host again") {
		t.Errorf("it told a person who is uninstalling to run host again:\n%s", out)
	}
}

// A config.json is in every project folder: it is not a Potluck home's mark.
func TestUninstallRefusesAFolderWithOnlyAConfigFile(t *testing.T) {
	l := newLender(t)
	proj := filepath.Join(l.home, "proj")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"config.json", "main.go"} {
		if err := os.WriteFile(filepath.Join(proj, f), []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv(EnvHome, proj)
	out, code := l.uninstall("", "--yes")
	if code == 0 || !strings.Contains(out, "does not look like a Potluck home") || !l.exists(filepath.Join(proj, "main.go")) {
		t.Fatalf("exit %d:\n%s", code, out)
	}
}

// The program file named in a home's `program` file marks it too.
func TestUninstallTakesAHomeNamedForPotluck(t *testing.T) {
	l := newLender(t)
	home := filepath.Join(l.home, "elsewhere")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "program"), []byte("potluck\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvHome, home)
	if out, code := l.uninstall("", "--yes"); code != 0 || l.exists(home) {
		t.Fatalf("exit %d, home still there %v:\n%s", code, l.exists(home), out)
	}
}

// The line under the marker goes only when it is the one install.sh wrote;
// the file keeps its mode and its other lines, a symlinked startup file is
// edited through, and a ZDOTDIR file under HOME is found.
func TestUninstallPathLine(t *testing.T) {
	l := newLender(t)
	home, err := os.MkdirTemp("", "pl")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(home)
	if err := os.WriteFile(filepath.Join(home, "registry.db"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvHome, home)

	// .zshrc: the person removed the export line and wrote their own under the comment.
	if err := os.WriteFile(l.rc, []byte("a=1\n\n# Potluck (install.sh): put potluck on PATH\nalias mine=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(l.rc, 0o640); err != nil {
		t.Fatal(err)
	}
	// .bashrc: the person edited the export line (more directories): it is theirs now.
	bashrc := filepath.Join(l.home, ".bashrc")
	if err := os.WriteFile(bashrc, []byte("b=1\n\n# Potluck (install.sh): put potluck on PATH\nexport PATH=\"$HOME/bin:$HOME/.local/bin:$PATH\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// ZDOTDIR's .zshrc, reached through a symlink.
	zdir := filepath.Join(l.home, ".config", "zsh")
	if err := os.MkdirAll(zdir, 0o755); err != nil {
		t.Fatal(err)
	}
	real := filepath.Join(l.home, "dotfiles-zshrc")
	if err := os.WriteFile(real, []byte("x=1\n\n# Potluck (install.sh): put potluck on PATH\nexport PATH=\"$HOME/.local/bin:$PATH\"\ny=2"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, filepath.Join(zdir, ".zshrc")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ZDOTDIR", zdir)
	// fish: the file is only Potluck's line.
	fish := filepath.Join(l.home, ".config", "fish", "conf.d", "potluck.fish")
	if err := os.MkdirAll(filepath.Dir(fish), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fish, []byte("\n# Potluck (install.sh): put potluck on PATH\nfish_add_path \"$HOME/.local/bin\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if out, code := l.uninstall("", "--yes"); code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if b, _ := os.ReadFile(l.rc); string(b) != "a=1\nalias mine=1\n" {
		t.Errorf(".zshrc:\n%q", b)
	}
	if b, _ := os.ReadFile(bashrc); string(b) != "b=1\nexport PATH=\"$HOME/bin:$HOME/.local/bin:$PATH\"\n" {
		t.Errorf(".bashrc:\n%q", b)
	}
	if fi, err := os.Stat(l.rc); err != nil || fi.Mode().Perm() != 0o640 {
		t.Errorf(".zshrc mode: %v %v", fi, err)
	}
	if b, _ := os.ReadFile(real); string(b) != "x=1\ny=2" {
		t.Errorf("the symlinked ZDOTDIR .zshrc:\n%q", b)
	}
	if fi, err := os.Lstat(filepath.Join(zdir, ".zshrc")); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("the symlink was replaced: %v %v", fi, err)
	}
	if l.exists(fish) {
		t.Error("an empty fish file is left")
	}
}

// Programs are taken from the directory the person ran potluck from, never
// from where a link to it leads.
func TestUninstallLeavesWhereASymlinkLeads(t *testing.T) {
	l := newLender(t)
	build := t.TempDir() // a dev build with its own potluck programs
	for _, n := range []string{"potluck", "potluck-e2b"} {
		if err := os.WriteFile(filepath.Join(build, n), []byte("dev"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Remove(filepath.Join(l.bin, "potluck")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(build, "potluck"), filepath.Join(l.bin, "potluck")); err != nil {
		t.Fatal(err)
	}
	home, err := os.MkdirTemp("", "pl")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(home)
	os.WriteFile(filepath.Join(home, "registry.db"), nil, 0o600)
	t.Setenv(EnvHome, home)
	if out, code := l.uninstall("", "--yes"); code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if l.exists(filepath.Join(l.bin, "potluck")) {
		t.Error("the link is still there")
	}
	for _, n := range []string{"potluck", "potluck-e2b"} {
		if !l.exists(filepath.Join(build, n)) {
			t.Errorf("%s in the build the link led to was removed", n)
		}
	}
}

// The Tart driver's records say which VMs are Potluck's sandboxes, so uninstall
// never removes them: it names them and the command. The dry run names the
// override file; --force removes the override file too and keeps the records.
func TestUninstallKeepsTheDriverRecords(t *testing.T) {
	l := newLender(t)
	state := filepath.Join(l.home, ".potluck-driver-tart")
	if err := os.MkdirAll(state, 0o755); err != nil {
		t.Fatal(err)
	}
	override := hostOverrideFile()
	if err := os.WriteFile(override, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	m := startMachine(t, tailnettest.New(), "mini", "judy@example.com")
	t.Setenv(EnvHome, m.home)

	out, code := l.uninstall("", "--dry-run")
	if code != 0 || !strings.Contains(out, "remove the host override file") || strings.Contains(out, "remove the Tart driver") {
		t.Fatalf("dry run, exit %d:\n%s", code, out)
	}
	if !l.exists(state) || !l.exists(override) {
		t.Error("the dry run removed something")
	}

	out, code = l.uninstall("", "--yes", "--force")
	if code != 0 || !l.exists(state) || !strings.Contains(out, "rm -r") {
		t.Fatalf("--force, exit %d, records there %v:\n%s", code, l.exists(state), out)
	}
	if l.exists(override) {
		t.Error("the override file is still there")
	}
}

// A daemon whose socket is gone but whose process holds the home's lock is
// running: nothing is removed.
func TestUninstallRefusesWhileTheHomeLockIsHeld(t *testing.T) {
	l := newLender(t)
	home, err := os.MkdirTemp("", "pl")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(home)
	os.WriteFile(filepath.Join(home, "registry.db"), nil, 0o600)
	release, err := config.LockHome(home)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvHome, home)
	out, code := l.uninstall("", "--yes")
	release()
	if code == 0 || !strings.Contains(out, "still holds") || !l.exists(home) || !l.exists(filepath.Join(l.bin, "potluck")) {
		t.Fatalf("exit %d:\n%s", code, out)
	}
}

// A daemon that is there but does not answer is not "not running": nothing is
// removed.
func TestUninstallRefusesWhenTheDaemonDoesNotAnswer(t *testing.T) {
	l := newLender(t)
	old := uninstallHealthWait
	uninstallHealthWait = 200 * time.Millisecond
	t.Cleanup(func() { uninstallHealthWait = old })
	home, err := os.MkdirTemp("", "pl")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(home)
	os.WriteFile(filepath.Join(home, "registry.db"), nil, 0o600)
	ln, err := net.Listen("unix", filepath.Join(home, SocketName))
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() { // accepts and says nothing
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			defer c.Close()
		}
	}()
	t.Setenv(EnvHome, home)
	out, code := l.uninstall("", "--yes")
	if code == 0 || !strings.Contains(out, "did not answer") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if !l.exists(home) || !l.exists(filepath.Join(l.bin, "potluck")) {
		t.Error("something was removed")
	}
}

// uninstall --yes --no-sudo --json runs the steps before the root step,
// stops there and answers it as data, running no sudo; after the root step
// it goes on to the end.
func TestUninstallNoSudoStopsAtTheRootStepThenFinishes(t *testing.T) {
	l := newLender(t)
	m := startMachine(t, tailnettest.New(), "mini", "judy@example.com")
	m.ok(t, "create", "friends")
	t.Setenv(EnvHome, m.home)
	mac := newSoftnetMac(t, "15.6")
	mac.downloads = filepath.Join(l.home, "Library", "Caches", "potluck", "softnet")
	if err := os.MkdirAll(mac.downloads, 0o700); err != nil {
		t.Fatal(err)
	}
	mac.asRoot = true
	if out, code := softnetRun(t, "softnet", "install", "--admin-step", "--candidate", "--from", mac.file); code != 0 {
		t.Fatalf("install: exit %d: %s", code, out)
	}
	mac.asRoot = false

	run := func() uninstallAnswer {
		t.Helper()
		answer, words, code := softnetRunJSON(t, "uninstall", "--yes", "--no-sudo")
		var a uninstallAnswer
		if err := json.Unmarshal(answer, &a); code != 0 || err != nil {
			t.Fatalf("exit %d (%v): %s\n%s", code, err, answer, words)
		}
		return a
	}
	first := run()
	if first.AdminStep == nil || len(first.Done) == 0 || !strings.HasPrefix(first.Done[0], "stop sharing") ||
		len(first.Left) == 0 || !strings.Contains(first.Left[0], "managed Softnet") || len(first.NotRemoved) == 0 {
		t.Fatalf("first run: %+v", first)
	}
	if !slices.Equal(first.AdminStep.Argv[1:], []string{"softnet", "uninstall", "--admin-step"}) {
		t.Fatalf("argv %q", first.AdminStep.Argv)
	}
	for _, p := range []string{filepath.Join(l.bin, "potluck"), m.home, mac.store.Dir, mac.downloads} {
		if !l.exists(p) {
			t.Errorf("%s went before the root step", p)
		}
	}
	mac.rootHalf(t, *first.AdminStep)

	second := run()
	if second.AdminStep != nil || len(second.Left) != 0 || !slices.ContainsFunc(second.Done, func(s string) bool { return strings.HasPrefix(s, "remove the programs") }) {
		t.Fatalf("second run: %+v", second)
	}
	for _, p := range []string{filepath.Join(l.bin, "potluck"), m.home, mac.downloads} {
		if l.exists(p) {
			t.Errorf("%s is still there", p)
		}
	}
	if len(mac.sudo) != 0 {
		t.Fatalf("sudo ran: %v", mac.sudo)
	}
}

// Under --json a step that fails still answers what was done and what is
// left, with the error, and exits non-zero.
func TestUninstallJSONAnswersAFailedStep(t *testing.T) {
	l := newLender(t)
	mac := newSoftnetMac(t, "15.6")
	mac.asRoot = true
	if out, code := softnetRun(t, "softnet", "install", "--admin-step", "--candidate", "--from", mac.file); code != 0 {
		t.Fatalf("install: exit %d: %s", code, out)
	}
	mac.asRoot = false
	softnetSudo = func(context.Context, string, []string, io.Writer, io.Writer) error { return io.ErrUnexpectedEOF }
	t.Setenv(EnvHome, filepath.Join(t.TempDir(), "none"))
	answer, words, code := softnetRunJSON(t, "uninstall", "--yes")
	var a uninstallAnswer
	if err := json.Unmarshal(answer, &a); code == 0 || err != nil || a.Error == "" || len(a.Left) == 0 || !strings.Contains(a.Left[0], "managed Softnet") {
		t.Fatalf("exit %d (%v): %s\n%s", code, err, answer, words)
	}
	if !l.exists(filepath.Join(l.bin, "potluck")) {
		t.Error("a step after the failed one ran")
	}
}
