package cli

import (
	"bytes"
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
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
	softnetStore = func() softnet.Store { return softnet.Store{Dir: filepath.Join(t.TempDir(), "Softnet")} }
	softnetGOOS = "darwin"
	t.Cleanup(func() { softnetStore, softnetGOOS = oldStore, oldGOOS })
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
