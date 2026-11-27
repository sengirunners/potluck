package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sengirunners/potluck/kit/tailnet/tailnettest"
)

// potluckHome makes a home with a registry and files files at dir.
func potluckHome(t *testing.T, dir string, files int) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for i := range files {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%05d", i)), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for n, s := range map[string]string{"registry.db": "x", "program": "potluck\n"} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// A removal killed while it deletes the home leaves no part of it under its
// name, and uninstall run again finishes. The kill is a real one, at several
// moments.
func TestUninstallKilledInTheHome(t *testing.T) {
	if h := os.Getenv("POTLUCK_TEST_REMOVE_HOME"); h != "" {
		_ = removeHome(h)
		os.Exit(0)
	}
	cut := 0
	for _, wait := range []time.Duration{0, 2, 5, 10, 20, 40} {
		l := newLender(t)
		home := filepath.Join(l.home, "potluck-home")
		const files = 4000
		potluckHome(t, home, files)
		t.Setenv(EnvHome, home)
		c := exec.Command(os.Args[0], "-test.run=^TestUninstallKilledInTheHome$")
		c.Env = append(os.Environ(), "POTLUCK_TEST_REMOVE_HOME="+home)
		if err := c.Start(); err != nil {
			t.Fatal(err)
		}
		for deadline := time.Now().Add(20 * time.Second); l.exists(home) && time.Now().Before(deadline); {
			time.Sleep(time.Millisecond)
		}
		time.Sleep(wait * time.Millisecond)
		_ = c.Process.Kill()
		_ = c.Wait()
		if l.exists(home) {
			t.Fatal("the home was not renamed")
		}
		if l.exists(home + ".removing") {
			cut++
			if out, code := l.uninstall("", "--dry-run"); code != 0 || !strings.Contains(out, "did not finish removing") {
				t.Fatalf("after a kill: exit %d\n%s", code, out)
			}
		}
		if out, code := l.uninstall("", "--yes"); code != 0 || l.exists(home) || l.exists(home+".removing") {
			t.Fatalf("exit %d, home %v, leftover %v\n%s", code, l.exists(home), l.exists(home+".removing"), out)
		}
	}
	if cut == 0 {
		t.Error("no kill landed inside the removal: nothing was proved")
	}
}

// A removal that cannot delete all of the home (a daemon's, or one no daemon
// claimed) says so and leaves what says it is Potluck's: the next one goes on.
func TestUninstallStoppedInTheHome(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root deletes anything")
	}
	for _, claimed := range []bool{true, false} {
		l := newLender(t)
		home := filepath.Join(l.home, "potluck-home")
		potluckHome(t, home, 1)
		if !claimed {
			if err := os.Remove(filepath.Join(home, "program")); err != nil {
				t.Fatal(err)
			}
		}
		stuck := filepath.Join(home, "stuck") // nothing can be taken from it
		if err := os.MkdirAll(stuck, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(stuck, "x"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(stuck, 0o500); err != nil {
			t.Fatal(err)
		}
		stuck = filepath.Join(home+".removing", filepath.Base(stuck))
		t.Cleanup(func() { _ = os.Chmod(stuck, 0o700) })
		t.Setenv(EnvHome, home)
		if out, code := l.uninstall("", "--yes"); code == 0 || l.exists(home) || !l.exists(stuck) {
			t.Fatalf("claimed %v: exit %d, home %v, what stays %v\n%s", claimed, code, l.exists(home), l.exists(stuck), out)
		}
		if err := os.Chmod(stuck, 0o700); err != nil {
			t.Fatal(err)
		}
		if out, code := l.uninstall("", "--yes"); code != 0 || l.exists(home+".removing") {
			t.Fatalf("claimed %v: exit %d, leftover %v\n%s", claimed, code, l.exists(home+".removing"), out)
		}
	}
}

// Set up again after a removal cut off: a later uninstall takes both the new
// home and what was left of the old one.
func TestUninstallTakesTheHomeAndAnEarlierLeftover(t *testing.T) {
	l := newLender(t)
	home := filepath.Join(l.home, "potluck-home")
	potluckHome(t, home, 1)
	potluckHome(t, home+".removing", 1)
	t.Setenv(EnvHome, home)
	if out, code := l.uninstall("", "--yes"); code != 0 || l.exists(home) || l.exists(home+".removing") {
		t.Fatalf("exit %d, home %v, leftover %v\n%s", code, l.exists(home), l.exists(home+".removing"), out)
	}
}

// A directory beside the home that does not say it is Potluck's is never
// taken, and the home goes all the same.
func TestUninstallLeavesAnothersDirectoryBesideTheHome(t *testing.T) {
	l := newLender(t)
	home := filepath.Join(l.home, "potluck-home")
	potluckHome(t, home, 1)
	mine := filepath.Join(home+".removing", "mine.txt")
	if err := os.MkdirAll(filepath.Dir(mine), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mine, []byte("mine"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvHome, home)
	if out, code := l.uninstall("", "--yes"); code != 0 || l.exists(home) || !l.exists(mine) {
		t.Fatalf("exit %d, home %v, theirs %v\n%s", code, l.exists(home), l.exists(mine), out)
	}
}

// A first setup cut off before the daemon first started leaves a home with a
// host run's files alone, or none: it can be removed.
func TestUninstallTakesAHomeNoDaemonClaimed(t *testing.T) {
	for _, names := range [][]string{nil,
		{"host.lock", "host.pid", "host-setup.json", ".host-setup.json.123", "menubar-seen.json", ".menubar-seen.json.new"},
		{"autostart.lock", "daemon.log", "daemon.lock", "writer.lock", "daemon-stopped"},
		{"host.lock", "host-setup.json", "swap.lock", "update-release.json", "update.log"},
		{"program", "daemon.lock", "update-release.json.tmp", "swap-pending.json.tmp"}, // a full disk
	} {
		l := newLender(t)
		home := filepath.Join(l.home, "potluck-home")
		if err := os.MkdirAll(home, 0o700); err != nil {
			t.Fatal(err)
		}
		for _, n := range names {
			if err := os.WriteFile(filepath.Join(home, n), nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		t.Setenv(EnvHome, home)
		if out, code := l.uninstall("", "--yes"); code != 0 || l.exists(home) {
			t.Fatalf("%v: exit %d, home %v\n%s", names, code, l.exists(home), out)
		}
	}
	// Names like those are not those, and another program's home is its own.
	l := newLender(t)
	home := filepath.Join(l.home, "potluck-home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "program"), []byte("other\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvHome, home)
	if out, code := l.uninstall("", "--yes"); code == 0 || !l.exists(filepath.Join(home, "program")) {
		t.Fatalf("another program's: exit %d\n%s", code, out)
	}
	l = newLender(t)
	home = filepath.Join(l.home, "potluck-home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"host.log.2026-01-01", "host.lock"} {
		if err := os.WriteFile(filepath.Join(home, n), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv(EnvHome, home)
	if out, code := l.uninstall("", "--yes"); code == 0 || !l.exists(filepath.Join(home, "host.lock")) {
		t.Fatalf("exit %d, home %v\n%s", code, l.exists(home), out)
	}
}

// What an install or a removal of the programs cut off left there (no
// potluck, copies not yet in place) is removed by the copy the Mac app
// carries.
func TestUninstallProgramsCutOff(t *testing.T) {
	l := newLender(t)
	_, names := installedProgramFiles()
	if len(names) < 2 || filepath.Base(names[0]) != "potluck" {
		t.Fatalf("potluck is not first: %v", names)
	}
	if err := os.Remove(filepath.Join(l.bin, "potluck")); err != nil {
		t.Fatal(err)
	}
	left := []string{filepath.Join(l.bin, "potluck-driver-tart"), filepath.Join(l.bin, ".potluck.new"), filepath.Join(l.bin, ".placing")}
	for _, p := range left {
		if err := os.WriteFile(p, nil, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cache := filepath.Join(l.home, "Library", "Caches", "app", "potluck")
	if err := os.MkdirAll(cache, 0o755); err != nil {
		t.Fatal(err)
	}
	mine := filepath.Join(cache, "potluck")
	if err := os.WriteFile(mine, nil, 0o755); err != nil {
		t.Fatal(err)
	}
	old := executable
	executable = func() (string, error) { return mine, nil }
	t.Cleanup(func() { executable = old })
	t.Setenv(EnvBundledRelease, filepath.Join(l.home, "release"))
	t.Setenv("POTLUCK_INSTALL_DIR", l.bin)
	if out, code := l.uninstall("", "--yes"); code != 0 || !l.exists(mine) || !l.exists(l.other) {
		t.Fatalf("exit %d, the app's copy %v, theirs %v\n%s", code, l.exists(mine), l.exists(l.other), out)
	}
	for _, p := range left {
		if l.exists(p) {
			t.Errorf("%s is still there", p)
		}
	}
}

// --keep-home keeps what a removal cut off left too, and without it the
// person is told what may be in it.
func TestUninstallLeftoverIsKeptOrWarned(t *testing.T) {
	l := newLender(t)
	home := filepath.Join(l.home, "potluck-home")
	potluckHome(t, home+".removing", 1)
	t.Setenv(EnvHome, home)
	if out, code := l.uninstall("", "--keep-home", "--yes"); code != 0 || !l.exists(home+".removing") {
		t.Fatalf("exit %d, leftover %v\n%s", code, l.exists(home+".removing"), out)
	}
	if out, code := l.uninstall("", "--dry-run"); code != 0 || !strings.Contains(out, "may hold this machine's identity") {
		t.Fatalf("exit %d\n%s", code, out)
	}
	// A daemon runs on it: someone recovers what is in it.
	f, err := os.Create(filepath.Join(home+".removing", "daemon.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	if out, code := l.uninstall("", "--yes"); code != 0 || !l.exists(home+".removing") {
		t.Fatalf("with a daemon on it: exit %d, leftover %v\n%s", code, l.exists(home+".removing"), out)
	}
}

// What a removal cut off left is potluck's own to remove, also on a machine
// whose Potluck daemon is another program's.
func TestUninstallLeftoverBesideAnotherProgramsDaemon(t *testing.T) {
	l := newLender(t)
	tmp, err := os.MkdirTemp("", "pl") // sockets have short paths
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmp)
	p := startProgram(t, tailnettest.New(), filepath.Join(tmp, ".supper"))
	t.Setenv(EnvHome, "")
	if out, code := l.uninstall("", "--yes"); code == 0 || !strings.Contains(out, "supper") {
		t.Fatalf("the other program's home: exit %d\n%s", code, out)
	}
	left := filepath.Join(l.home, ".potluck.removing")
	potluckHome(t, left, 1)
	if out, code := l.uninstall("", "--yes"); code != 0 || l.exists(left) || !l.exists(filepath.Join(p.home, "registry.db")) {
		t.Fatalf("exit %d, leftover %v, theirs %v\n%s", code, l.exists(left), l.exists(filepath.Join(p.home, "registry.db")), out)
	}
}
