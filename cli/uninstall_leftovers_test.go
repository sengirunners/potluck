package cli

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sengirunners/potluck/hostprobe"
	"github.com/sengirunners/potluck/hostsetup"
	"github.com/sengirunners/potluck/kit/tailnet/tailnettest"
	"github.com/sengirunners/potluck/protocol"
)

// freshTart is a lender's Mac whose setup stopped before any image: Potluck's
// tart.app is there, Tart's usual directory is dir, and the list makes it as
// Tart's does.
func (l *lender) freshTart(home string) (f *lenderTart, dir string) {
	l.t.Helper()
	f = l.lenderTart(home)
	f.entries = map[string]string{}
	if err := os.RemoveAll(os.Getenv("TART_HOME")); err != nil {
		l.t.Fatal(err)
	}
	l.t.Setenv("TART_HOME", "")
	uh, err := os.UserHomeDir()
	if err != nil || uh != l.home {
		l.t.Fatalf("home %s, not the test's %s (%v)", uh, l.home, err)
	}
	dir = filepath.Join(l.home, ".tart")
	was := tartRunner
	tartRunner = runnerFunc(func(ctx context.Context, name string, args ...string) (hostprobe.Result, error) {
		if args[0] == "list" {
			for _, n := range []string{"cache", "tmp"} {
				_ = os.MkdirAll(filepath.Join(dir, n), 0o755)
			}
		}
		return was.Run(ctx, name, args...)
	})
	return f, dir
}

// A Tart that Potluck installed and nobody used goes, with all that says so;
// a plan makes no directory of Tart's.
func TestUninstallRemovesAnUnusedTart(t *testing.T) {
	for name, listed := range map[string]bool{"no directory": false, "a directory Tart's list made": true} {
		t.Run(name, func(t *testing.T) {
			l := newLender(t)
			m := startMachine(t, tailnettest.New(), "mini", "judy@example.com")
			t.Setenv(EnvHome, m.home)
			f, dir := l.freshTart(m.home)
			if listed {
				for _, n := range []string{"cache", "tmp"} {
					if err := os.MkdirAll(filepath.Join(dir, n), 0o755); err != nil {
						t.Fatal(err)
					}
				}
			}
			plan, _ := l.uninstallJSON("--dry-run", "--with-tart")
			if !slices.Contains(plan.Remove.Args, "--with-tart=all") || l.exists(dir) != listed || !l.exists(f.place.App()) {
				t.Fatalf("%v, %s: %v", plan.Remove.Args, dir, l.exists(dir))
			}
			a, code := l.uninstallJSON("--yes", "--no-sudo", "--with-tart")
			if code != 0 || l.exists(f.place.App()) || l.exists(dir) || l.exists(filepath.Dir(f.place.RecordFile)) || l.exists(m.home) {
				t.Fatalf("%d %+v", code, a)
			}
		})
	}
}

// An empty directory proves nothing when Tart may keep what it holds in
// another, when it cannot be read, or when the Tart is not Potluck's.
func TestUninstallKeepsATartThatMayHoldSomething(t *testing.T) {
	must := func(t *testing.T, err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	for name, set := range map[string]func(t *testing.T, l *lender, home, dir string, f *lenderTart){
		"TART_HOME set, not there": func(t *testing.T, l *lender, _, _ string, _ *lenderTart) {
			t.Setenv("TART_HOME", filepath.Join(l.home, "disk", "tart"))
		},
		"TART_HOME set, empty": func(t *testing.T, l *lender, _, _ string, _ *lenderTart) { t.Setenv("TART_HOME", t.TempDir()) },
		"a link to an empty directory": func(t *testing.T, _ *lender, _, dir string, _ *lenderTart) {
			must(t, os.Symlink(t.TempDir(), dir))
		},
		"a dangling link": func(t *testing.T, l *lender, _, dir string, _ *lenderTart) {
			must(t, os.Symlink(filepath.Join(l.home, "disk", "tart"), dir))
		},
		"unreadable": func(t *testing.T, _ *lender, _, dir string, _ *lenderTart) {
			if os.Geteuid() == 0 {
				t.Skip("root reads it")
			}
			must(t, os.Mkdir(dir, 0))
			t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
		},
		"an unreadable cache": func(t *testing.T, _ *lender, _, dir string, _ *lenderTart) {
			if os.Geteuid() == 0 {
				t.Skip("root reads it")
			}
			must(t, os.MkdirAll(filepath.Join(dir, "cache"), 0o755))
			must(t, os.Chmod(filepath.Join(dir, "cache"), 0))
			t.Cleanup(func() { _ = os.Chmod(filepath.Join(dir, "cache"), 0o755) })
		},
		"a file": func(t *testing.T, _ *lender, _, dir string, _ *lenderTart) { must(t, os.WriteFile(dir, nil, 0o600)) },
		"a file in it": func(t *testing.T, _ *lender, _, dir string, _ *lenderTart) {
			must(t, os.MkdirAll(dir, 0o755))
			must(t, os.WriteFile(filepath.Join(dir, "mine"), nil, 0o600))
		},
		"a VM folder": func(t *testing.T, _ *lender, _, dir string, _ *lenderTart) {
			must(t, os.MkdirAll(filepath.Join(dir, "vms", "mine"), 0o755))
		},
		"something in its cache": func(t *testing.T, _ *lender, _, dir string, _ *lenderTart) {
			must(t, os.MkdirAll(filepath.Join(dir, "cache", "OCIs"), 0o755))
		},
		"VMs were made elsewhere": func(t *testing.T, l *lender, _, _ string, _ *lenderTart) {
			must(t, os.MkdirAll(protocol.DriverStateDir(l.home, "tart"), 0o755))
		},
		"images were pulled elsewhere": func(t *testing.T, _ *lender, home, _ string, _ *lenderTart) {
			must(t, os.WriteFile(filepath.Join(home, hostsetup.FileName), []byte(`{"stages":[{"name":"preflight"},{"name":"images"}]}`), 0o600))
		},
		"the setup's record is unreadable": func(t *testing.T, _ *lender, home, _ string, _ *lenderTart) {
			must(t, os.WriteFile(filepath.Join(home, hostsetup.FileName), []byte("{"), 0o600))
		},
		"the update job names another": func(t *testing.T, l *lender, _, _ string, _ *lenderTart) {
			t.Setenv("TART_HOME", "/Volumes/x")
			if out, code := l.job.potluck("enable"); code != 0 {
				t.Fatal(out)
			}
			t.Setenv("TART_HOME", "")
		},
		"a Tart the person installed": func(t *testing.T, _ *lender, _, _ string, f *lenderTart) {
			must(t, os.Remove(f.place.RecordFile))
		},
		"a running Tart": func(_ *testing.T, _ *lender, _, _ string, f *lenderTart) { f.ps = "/opt/homebrew/bin/tart\n" },
	} {
		t.Run(name, func(t *testing.T) {
			l := newLender(t)
			m := startMachine(t, tailnettest.New(), "mini", "judy@example.com")
			t.Setenv(EnvHome, m.home)
			f, dir := l.freshTart(m.home)
			set(t, l, m.home, dir, f)
			a, _ := l.uninstallJSON("--yes", "--no-sudo", "--with-tart")
			if !l.exists(f.place.Program()) || slices.ContainsFunc(a.View.Done, func(s viewStep) bool { return s.ID == tartAppStep }) {
				t.Fatalf("it went: %+v", a)
			}
		})
	}
}

// What appears in Tart's directory just before the removal keeps Tart.
func TestUninstallKeepsAnUnusedTartUsedMeanwhile(t *testing.T) {
	l := newLender(t)
	m := startMachine(t, tailnettest.New(), "mini", "judy@example.com")
	t.Setenv(EnvHome, m.home)
	f, dir := l.freshTart(m.home)
	was, n := tartRunner, 0
	tartRunner = runnerFunc(func(ctx context.Context, name string, args ...string) (hostprobe.Result, error) {
		if name == "/bin/ps" {
			if n++; n == 1 {
				defer func() { _ = os.MkdirAll(filepath.Join(dir, "vms", "mine"), 0o755) }()
			}
		}
		return was.Run(ctx, name, args...)
	})
	if a, code := l.uninstallJSON("--yes", "--no-sudo", "--with-tart"); code != 0 || !l.exists(f.place.Program()) || !l.exists(filepath.Join(dir, "vms", "mine")) {
		t.Fatalf("%d %+v", code, a)
	}
}

// A lock file with no record (an install cut off before it, a removal after)
// goes, with Tart left alone or not; a record keeps its lock.
func TestUninstallRemovesASpareTartLock(t *testing.T) {
	l := newLender(t)
	m := startMachine(t, tailnettest.New(), "mini", "judy@example.com")
	t.Setenv(EnvHome, m.home)
	place, _ := tartPlace()
	stage := filepath.Join(place.AppsDir, ".tart.app.potluck-1")
	for _, d := range []string{stage, filepath.Dir(place.RecordFile)} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(place.RecordFile+".lock", nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if a, code := l.uninstallJSON("--yes", "--no-sudo"); code != 0 || l.exists(filepath.Dir(place.RecordFile)) || l.exists(stage) {
		t.Fatalf("%d %+v", code, a)
	}

	if err := os.MkdirAll(filepath.Dir(place.RecordFile), 0o700); err != nil { // cut off between the two
		t.Fatal(err)
	}
	if a, code := l.uninstallJSON("--yes", "--no-sudo"); code != 0 || l.exists(filepath.Dir(place.RecordFile)) {
		t.Fatalf("%d %+v", code, a)
	}

	l = newLender(t)
	m = startMachine(t, tailnettest.New(), "mini2", "judy@example.com")
	t.Setenv(EnvHome, m.home)
	f := l.lenderTart(m.home)
	if a, code := l.uninstallJSON("--yes", "--no-sudo"); code != 0 || !l.exists(f.place.RecordFile) || !l.exists(f.place.RecordFile+".lock") || !strings.Contains(strings.Join(a.NotRemoved, "\n"), "tart-install.json.lock") {
		t.Fatalf("%d %+v", code, a)
	}
	if err := f.place.DropSpareLock(); err != nil || !l.exists(f.place.RecordFile+".lock") {
		t.Fatalf("a lock with its record: %v", err)
	}
	if _, err := f.place.Remove(); err != nil {
		t.Fatal(err)
	}
}

// The root step takes the folder the network helper was in when nothing else
// is there. One already empty asks for no password: it is named.
func TestUninstallRemovesTheHelpersFolder(t *testing.T) {
	for name, kept := range map[string]bool{"alone": false, "with a file": true, "already empty": true} {
		t.Run(name, func(t *testing.T) {
			l := newLender(t)
			mac := newSoftnetMac(t, "15.6")
			mac.store.Dir = filepath.Join(mac.store.Top, "Potluck", "Softnet")
			folder, empty := filepath.Dir(mac.store.Dir), name == "already empty"
			made := mac.store.Dir
			if empty {
				made = folder
			}
			if err := os.MkdirAll(made, 0o755); err != nil {
				t.Fatal(err)
			}
			if name == "with a file" {
				if err := os.WriteFile(filepath.Join(folder, "mine"), nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			asked := false
			softnetSudo = func(_ context.Context, _ string, args []string, _, _ io.Writer) error {
				asked, mac.asRoot = true, true
				defer func() { mac.asRoot = false }()
				if out, code := softnetRun(t, args...); code != 0 {
					t.Errorf("admin step: exit %d: %s", code, out)
				}
				return nil
			}
			m := startMachine(t, tailnettest.New(), "mini", "judy@example.com")
			t.Setenv(EnvHome, m.home)
			out, code := l.uninstall("", "--yes")
			if code != 0 || l.exists(folder) != kept || l.exists(mac.store.Dir) || asked == empty || strings.Contains(out, "sudo rmdir") != empty {
				t.Fatalf("exit %d asked %v\n%s", code, asked, out)
			}
		})
	}
}

// What shows Tart was used elsewhere goes with the home and the update job:
// the next run still keeps Tart.
func TestUninstallKeepsATartUsedElsewhereAgain(t *testing.T) {
	for _, how := range []string{"home", "job", "after the plan"} {
		job := how == "job"
		l := newLender(t)
		m := startMachine(t, tailnettest.New(), "mini", "judy@example.com")
		t.Setenv(EnvHome, m.home)
		f, _ := l.freshTart(m.home)
		args := []string{"--yes", "--no-sudo", "--with-tart"}
		if job {
			args = append(args, "--keep-home")
			t.Setenv("TART_HOME", "/Volumes/x")
			if out, code := l.job.potluck("enable"); code != 0 {
				t.Fatal(out)
			}
			t.Setenv("TART_HOME", "")
		}
		images := func() {
			if err := os.WriteFile(filepath.Join(m.home, hostsetup.FileName), []byte(`{"stages":[{"name":"preflight"},{"name":"images"}]}`), 0o600); err != nil {
				t.Error(err)
			}
		}
		switch how {
		case "home":
			images()
		case "after the plan":
			was, n := tartRunner, 0
			tartRunner = runnerFunc(func(ctx context.Context, name string, args ...string) (hostprobe.Result, error) {
				if n++; n == 1 {
					images()
				}
				return was.Run(ctx, name, args...)
			})
		}
		for range 2 {
			if a, code := l.uninstallJSON(args...); code != 0 || !l.exists(f.place.Program()) || l.exists(m.home) != job {
				t.Fatalf("%s: %d %+v", how, code, a)
			}
		}
	}
}

// What stays of the Mac app and its download is named, and never removed.
func TestUninstallNamesTheAppsFilesAndTheDownload(t *testing.T) {
	l := newLender(t)
	m := startMachine(t, tailnettest.New(), "mini", "judy@example.com")
	t.Setenv(EnvHome, m.home)
	prefs, zip := filepath.Join(l.home, "Library", "Preferences", "sh.potluck.menu.plist"), filepath.Join(l.home, "Downloads", "Potluck.zip")
	for _, p := range []string{prefs, zip} {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	a, code := l.uninstallJSON("--yes", "--no-sudo")
	stays := strings.Join(a.View.Stays, "\n")
	if code != 0 || !l.exists(prefs) || !l.exists(zip) || !strings.Contains(strings.Join(a.NotRemoved, "\n"), "sh.potluck.menu.plist") ||
		!strings.Contains(stays, "Potluck app's own settings") || !strings.Contains(stays, "Potluck.zip") {
		t.Fatalf("%d %+v", code, a)
	}
}
