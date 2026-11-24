package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/menubar"
)

// On a Mac, host status's checklist reads the menu bar item and the daily
// update as they are: installed is not running until the app itself asks,
// and reading them never turns either on.
func TestHostStatusChecklistReadsMenuAndUpdates(t *testing.T) {
	e := newJobEnv(t, "darwin")
	apps := t.TempDir()
	oldGOOS, oldParent, oldDirs := setupGOOS, menuParent, menuAppDirs
	t.Cleanup(func() { setupGOOS, menuParent, menuAppDirs = oldGOOS, oldParent, oldDirs })
	setupGOOS, menuParent, menuAppDirs = "darwin", func() string { return "" }, func() []string { return []string{apps} }
	status := func() menubar.Setup {
		t.Helper()
		var o, er bytes.Buffer
		if code := Run(context.Background(), []string{"--json", "host", "status"}, strings.NewReader(""), &o, &er); code != 0 {
			t.Fatalf("host status: exit %d\n%s", code, er.String())
		}
		var st menubar.Status
		if err := json.Unmarshal(o.Bytes(), &st); err != nil {
			t.Fatal(err)
		}
		return st.Setup
	}
	byID := func(s menubar.Setup, id string) menubar.Check {
		for _, c := range s.Checks {
			if c.ID == id {
				return c
			}
		}
		return menubar.Check{}
	}

	s := status()
	if c := byID(s, "menu"); c.State != menubar.Todo || c.Action != installScript+" --menubar" || c.Do == nil || !strings.HasPrefix(c.Do.URL, "https://") {
		t.Errorf("no app: %+v", c)
	}
	if c := byID(s, "updates"); c.State != menubar.Todo || c.Action != "potluck upgrade enable" {
		t.Errorf("updates off: %+v", c)
	}
	if s.Next != "potluck daemon --detach" || s.NextID != "daemon" { // no daemon here: that comes first
		t.Errorf("next %q (%s)", s.Next, s.NextID)
	}
	if c := byID(s, "daemon"); c.Do == nil || strings.Join(c.Do.Args, " ") != "daemon --detach" {
		t.Errorf("daemon: %+v", c.Do)
	}
	upgrade := byID(s, "updates").Do

	app := filepath.Join(apps, "Potluck Menu.app")
	if err := os.MkdirAll(filepath.Join(app, "Contents", "MacOS"), 0o755); err != nil {
		t.Fatal(err)
	}
	if c := byID(status(), "menu"); c.State != menubar.Todo || !strings.Contains(c.Detail, "never read this status") || c.Action != "open "+shellQuote(app) ||
		c.Do == nil || c.Do.URL != (&url.URL{Scheme: "file", Path: app}).String() {
		t.Errorf("installed, never asked: %+v %+v", c, c.Do)
	}
	// The app asks (it starts potluck): it counts as running, and with no
	// home (before setup, or after an uninstall) none is made to note it in.
	asApp := func() string { return filepath.Join(app, "Contents", "MacOS", "PotluckMenu") }
	menuParent = asApp
	home, err := config.HomeDir()
	if err != nil {
		t.Fatal(err)
	}
	if c := byID(status(), "menu"); c.State != menubar.Done {
		t.Errorf("the app asks, no home: %+v", c)
	}
	if lexists(home) {
		t.Fatalf("host status made the home %s", home)
	}
	// With a home it counts as running for a while after its ask.
	if err := os.Mkdir(home, 0o700); err != nil {
		t.Fatal(err)
	}
	status()
	menuParent = func() string { return "/bin/zsh" }
	if c := byID(status(), "menu"); c.State != menubar.Done || !strings.Contains(c.Detail, "is running") {
		t.Errorf("after the app asked: %+v", c)
	}
	if e.count("bootstrap") != 0 {
		t.Fatalf("host status loaded an update job: %v", e.calls)
	}
	// The updates check's button is what turns it on.
	if upgrade == nil || len(upgrade.Args) == 0 {
		t.Fatalf("updates off: no do: %+v", upgrade)
	}
	var o, er bytes.Buffer
	if code := Run(context.Background(), upgrade.Args, strings.NewReader(""), &o, &er); code != 0 {
		t.Fatalf("%q: exit %d\n%s%s", upgrade.Args, code, o.String(), er.String())
	}
	if c := byID(status(), "updates"); c.State != menubar.Done {
		t.Errorf("updates on: %+v", c)
	}
}
