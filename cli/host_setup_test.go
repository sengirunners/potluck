package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
	if c := byID(s, "menu"); c.State != menubar.Todo || c.Action != installScript+" --menubar" {
		t.Errorf("no app: %+v", c)
	}
	if c := byID(s, "updates"); c.State != menubar.Todo || c.Action != "potluck upgrade enable" {
		t.Errorf("updates off: %+v", c)
	}
	if s.Next != "potluck daemon --detach" { // no daemon here: that comes first
		t.Errorf("next %q", s.Next)
	}

	app := filepath.Join(apps, "Potluck Menu.app")
	if err := os.MkdirAll(filepath.Join(app, "Contents", "MacOS"), 0o755); err != nil {
		t.Fatal(err)
	}
	if c := byID(status(), "menu"); c.State != menubar.Todo || !strings.Contains(c.Detail, "never read this status") || c.Action != "open "+shellQuote(app) {
		t.Errorf("installed, never asked: %+v", c)
	}
	// The app asks (it starts potluck): from then on it counts as running.
	menuParent = func() string { return filepath.Join(app, "Contents", "MacOS", "PotluckMenu") }
	status()
	menuParent = func() string { return "/bin/zsh" }
	if c := byID(status(), "menu"); c.State != menubar.Done || !strings.Contains(c.Detail, "is running") {
		t.Errorf("after the app asked: %+v", c)
	}
	if e.count("bootstrap") != 0 {
		t.Fatalf("host status loaded an update job: %v", e.calls)
	}
	if out, code := e.potluck("enable"); code != 0 {
		t.Fatalf("upgrade enable: %d\n%s", code, out)
	}
	if c := byID(status(), "updates"); c.State != menubar.Done {
		t.Errorf("updates on: %+v", c)
	}
}
