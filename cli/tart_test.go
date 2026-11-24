package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sengirunners/potluck/hostprobe"
	"github.com/sengirunners/potluck/tartpin"
	"github.com/sengirunners/potluck/tartpin/tartpintest"
	"github.com/sengirunners/potluck/vmhost"
)

// tartMac is a fake Mac for potluck tart: its macOS, a home whose
// Applications an install fills, a Tart elsewhere when other is set, and
// the release served from httptest.
type tartMac struct {
	home      string
	place     tartpin.Place
	other     string // a Tart program found before ~/Applications ("" for none)
	build     tartpin.Build
	body      []byte
	downloads int
}

func newTartMac(t *testing.T, macOS string) *tartMac {
	t.Helper()
	m := &tartMac{home: t.TempDir()}
	m.place = tartpin.Place{AppsDir: filepath.Join(m.home, "Applications"), RecordFile: filepath.Join(m.home, "support", "tart-install.json")}
	_, m.body, m.build = tartpintest.Release(t, "1.0.0", tartpintest.Whole())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.downloads++
		w.Write(m.body)
	}))
	t.Cleanup(srv.Close)
	m.build.URL = srv.URL + "/tart.tar.gz"

	oldBuilds, oldGOOS, oldRunner, oldSearch, oldPlace, oldHTTP := tartpin.Builds, tartGOOS, tartRunner, tartSearch, tartPlace, tartHTTP
	t.Cleanup(func() {
		tartpin.Builds, tartGOOS, tartRunner, tartSearch, tartPlace, tartHTTP = oldBuilds, oldGOOS, oldRunner, oldSearch, oldPlace, oldHTTP
	})
	tartpin.Builds = []tartpin.Build{m.build}
	tartGOOS = "darwin"
	tartRunner = runnerFunc(func(_ context.Context, name string, args ...string) (hostprobe.Result, error) {
		switch strings.Join(append([]string{name}, args...), " ") {
		case "/usr/bin/sw_vers -productVersion":
			return hostprobe.Result{Stdout: macOS + "\n"}, nil
		case "/usr/sbin/sysctl -n hw.optional.arm64":
			return hostprobe.Result{Stdout: "1\n"}, nil
		}
		return hostprobe.Result{Exit: 127}, nil
	})
	inHome := func(p string) bool { return strings.HasPrefix(p, m.home+string(filepath.Separator)) }
	tartSearch = func() vmhost.TartSearch {
		return vmhost.TartSearch{
			LookPath: func(string) (string, bool) { return m.other, m.other != "" },
			Executable: func(p string) bool {
				return inHome(p) && vmhost.IsExecutableFile(p)
			},
			Exists: func(p string) bool { _, err := os.Stat(p); return inHome(p) && err == nil },
			Home:   m.home,
		}
	}
	tartPlace = func() (tartpin.Place, error) { return m.place, nil }
	tartHTTP = srv.Client()
	return m
}

func tartRunJSON(t *testing.T, args ...string) (tartInstallAnswer, string, int) {
	t.Helper()
	var out, errs bytes.Buffer
	code := Run(context.Background(), append([]string{"--json"}, args...), strings.NewReader(""), &out, &errs)
	var a tartInstallAnswer
	if code == 0 {
		if err := json.Unmarshal(out.Bytes(), &a); err != nil {
			t.Fatalf("answer %q: %v", out.String(), err)
		}
	}
	return a, out.String() + errs.String(), code
}

func TestTartInstallPutsTartWhereItIsFound(t *testing.T) {
	m := newTartMac(t, "26.1")
	a, out, code := tartRunJSON(t, "tart", "install", "--dry-run")
	if code != 0 || !a.DryRun || a.Installed || len(a.Plan) == 0 || m.downloads != 0 {
		t.Fatalf("dry run: exit %d, %d downloads: %s", code, m.downloads, out)
	}
	if _, err := os.Stat(m.place.AppsDir); err == nil {
		t.Fatal("a dry run made Applications")
	}
	a, out, code = tartRunJSON(t, "tart", "install")
	if code != 0 || !a.Installed || a.Path != m.place.Program() || a.Version != "1.0.0" {
		t.Fatalf("install: exit %d: %s", code, out)
	}
	if got := vmhost.FindTart(tartSearch()); got.Path != m.place.Program() {
		t.Fatalf("FindTart found %+v after the install", got)
	}
	a, out, code = tartRunJSON(t, "tart", "install")
	if code != 0 || a.Installed || !a.AlreadyPresent || m.downloads != 1 {
		t.Fatalf("second install: exit %d, %d downloads: %s", code, m.downloads, out)
	}
	if out, code := softnetRun(t, "tart", "path"); code != 0 || strings.TrimSpace(out) != m.place.Program() {
		t.Fatalf("tart path: exit %d: %s", code, out)
	}
}

func TestTartInstallRefusesAWrongDownload(t *testing.T) {
	m := newTartMac(t, "26.1")
	m.body = append(append([]byte{}, m.body[:len(m.body)-1]...), m.body[len(m.body)-1]^1)
	if _, out, code := tartRunJSON(t, "tart", "install"); code == 0 || !strings.Contains(out, "SHA-256") || !strings.Contains(out, "Nothing was installed") {
		t.Fatalf("tampered download: exit %d: %s", code, out)
	}
	m.body = m.body[:len(m.body)-1]
	if _, out, code := tartRunJSON(t, "tart", "install"); code == 0 || !strings.Contains(out, "bytes") {
		t.Fatalf("short download: exit %d: %s", code, out)
	}
	if _, err := os.Stat(m.place.App()); err == nil {
		t.Fatal("a refused download was installed")
	}
}

func TestTartInstallLeavesTartThatIsThere(t *testing.T) {
	m := newTartMac(t, "26.1")
	m.other = "/opt/homebrew/bin/tart"
	a, out, code := tartRunJSON(t, "tart", "install")
	if code != 0 || a.Installed || !a.AlreadyPresent || a.Path != m.other || m.downloads != 0 {
		t.Fatalf("Tart on PATH: exit %d, %d downloads: %s", code, m.downloads, out)
	}

	// A tart.app in ~/Applications Potluck did not install: left alone.
	m = newTartMac(t, "26.1")
	if err := os.MkdirAll(m.place.App(), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, out, code := tartRunJSON(t, "tart", "install"); code == 0 || !strings.Contains(out, "Potluck did not install it") || m.downloads != 0 {
		t.Fatalf("a lender's own tart.app: exit %d, %d downloads: %s", code, m.downloads, out)
	}
}

func TestTartInstallRefusesAMacItCannotRunOn(t *testing.T) {
	m := newTartMac(t, "12.7")
	if _, out, code := tartRunJSON(t, "tart", "install"); code == 0 || !strings.Contains(out, "needs macOS 13") || m.downloads != 0 {
		t.Fatalf("macOS 12: exit %d: %s", code, out)
	}
	tartGOOS = "linux"
	if _, out, code := tartRunJSON(t, "tart", "install"); code == 0 || !strings.Contains(out, "on a Mac") {
		t.Fatalf("Linux: exit %d: %s", code, out)
	}
}

// Potluck's copy of an older release is replaced by the pinned one.
func TestTartInstallUpgradesItsOwnCopy(t *testing.T) {
	m := newTartMac(t, "26.1")
	old, _, ob := tartpintest.Release(t, "0.9.0", tartpintest.Whole())
	if _, err := m.place.Install(old, ob); err != nil {
		t.Fatal(err)
	}
	a, out, code := tartRunJSON(t, "tart", "install", "--dry-run")
	if code != 0 || a.Replaced != "0.9.0" || m.downloads != 0 {
		t.Fatalf("dry run: exit %d: %s", code, out)
	}
	a, out, code = tartRunJSON(t, "tart", "install")
	if code != 0 || !a.Installed || a.Replaced != "0.9.0" || a.Version != "1.0.0" {
		t.Fatalf("upgrade: exit %d: %s", code, out)
	}
	if rec, ok := m.place.Installed(); !ok || rec.Version != "1.0.0" {
		t.Fatalf("record %+v %v", rec, ok)
	}
}

// A link on PATH to Potluck's own copy does not stop its upgrade.
func TestTartInstallUpgradesThroughALink(t *testing.T) {
	m := newTartMac(t, "26.1")
	old, _, ob := tartpintest.Release(t, "0.9.0", tartpintest.Whole())
	if _, err := m.place.Install(old, ob); err != nil {
		t.Fatal(err)
	}
	m.other = filepath.Join(t.TempDir(), "tart")
	if err := os.Symlink(m.place.Program(), m.other); err != nil {
		t.Fatal(err)
	}
	if a, out, code := tartRunJSON(t, "tart", "install"); code != 0 || a.Replaced != "0.9.0" {
		t.Fatalf("exit %d: %s", code, out)
	}
}

// Potluck's own copy is there, but another Tart is found first: that one is
// reported, and Potluck's is neither upgraded nor claimed to be the one used.
func TestTartInstallReportsTheTartFoundFirst(t *testing.T) {
	for _, version := range []string{"0.9.0", "1.0.0"} {
		m := newTartMac(t, "26.1")
		own, _, ob := tartpintest.Release(t, version, tartpintest.Whole())
		if _, err := m.place.Install(own, ob); err != nil {
			t.Fatal(err)
		}
		m.other = filepath.Join(t.TempDir(), "tart")
		if err := os.WriteFile(m.other, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		a, out, code := tartRunJSON(t, "tart", "install")
		if code != 0 || a.Installed || !a.AlreadyPresent || a.Path != m.other || m.downloads != 0 || len(a.Notes) != 1 {
			t.Fatalf("Potluck's %s: exit %d, %d downloads: %s", version, code, m.downloads, out)
		}
		if rec, ok := m.place.Installed(); !ok || rec.Version != version {
			t.Fatalf("Potluck's %s became %+v %v", version, rec, ok)
		}
	}
}
