package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/menubar"
)

// The app is found under its name of now and the one it had before: the one
// that last asked is the one there is, else the new name.
func TestHostStatusFindsTheAppUnderEitherName(t *testing.T) {
	newJobEnv(t, "darwin")
	apps := t.TempDir()
	oldGOOS, oldParent, oldDirs := setupGOOS, menuParent, menuAppDirs
	t.Cleanup(func() { setupGOOS, menuParent, menuAppDirs = oldGOOS, oldParent, oldDirs })
	parent := ""
	setupGOOS, menuParent, menuAppDirs = "darwin", func() string { return parent }, func() []string { return []string{apps} }
	home, err := config.HomeDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	setup := func() menubar.Setup {
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
	check := func(id string) menubar.Check {
		t.Helper()
		for _, c := range setup().Checks {
			if c.ID == id {
				return c
			}
		}
		t.Fatalf("no %s check", id)
		return menubar.Check{}
	}
	app := func(name string) string { return filepath.Join(apps, name) }
	open := func(name string) string { return "open " + shellQuote(app(name)) }
	mkFor := func(name, program string) {
		t.Helper()
		macos := filepath.Join(app(name), "Contents", "MacOS")
		if err := os.MkdirAll(macos, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(macos, program), nil, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	mk := func(name string) { t.Helper(); mkFor(name, "PotluckMenu") }
	mkFor("Potluck.app", "Recipes")
	if c := check("menu"); !strings.Contains(c.Detail, "is not installed") {
		t.Errorf("another program's Potluck.app: %+v", c)
	}
	mk("Potluck Menu.app")
	if c := check("menu"); c.Action != open("Potluck Menu.app") {
		t.Errorf("only the old name: %+v", c)
	}
	if c := check("login"); !strings.Contains(c.Detail, "add Potluck Menu in") {
		t.Errorf("only the old name: %+v", c)
	}
	mk("Potluck.app")
	if c := check("menu"); c.Action != open("Potluck.app") {
		t.Errorf("both, none asked: %+v", c)
	}
	if c := check("login"); !strings.Contains(c.Detail, "add Potluck in") {
		t.Errorf("both, none asked: %+v", c)
	}
	for _, name := range []string{"Potluck Menu.app", "Potluck.app"} {
		parent = filepath.Join(app(name), "Contents", "MacOS", "PotluckMenu")
		check("menu")
		parent = "/bin/zsh"
		if c := check("menu"); c.State != menubar.Done || !strings.Contains(c.Detail, app(name)) {
			t.Errorf("%s asked last: %+v", name, c)
		}
	}
	// What the app says of its login item counts only from the app, and the
	// old app's place is offered for removal only while it is there.
	login := func(say, from string) string {
		t.Helper()
		t.Setenv(EnvLoginItem, say)
		parent = from
		return check("login").State
	}
	inApp := filepath.Join(app("Potluck.app"), "Contents", "MacOS", "PotluckMenu")
	for say, want := range map[string]string{"on": menubar.Done, "off": menubar.Todo, "refused": menubar.Todo, "yes": menubar.Unknown, "": menubar.Unknown} {
		if got := login(say, inApp); got != want {
			t.Errorf("the app says %q: %s, want %s", say, got, want)
		}
	}
	login("off", inApp)
	if got := login("on", "/bin/zsh"); got != menubar.Todo {
		t.Errorf("another program says on: %s", got)
	}
	if got := setup().OldApps; len(got) != 1 || got[0] != app("Potluck Menu.app") {
		t.Errorf("old apps %q", got)
	}
	if err := os.Rename(app("Potluck Menu.app"), app("Away.app")); err != nil {
		t.Fatal(err)
	}
	if got := setup().OldApps; len(got) != 0 {
		t.Errorf("gone, old apps %q", got)
	}
	if err := os.Rename(app("Away.app"), app("Potluck Menu.app")); err != nil {
		t.Fatal(err)
	}
	// The one that asked was renamed away: the other is found.
	if err := os.RemoveAll(app("Potluck.app")); err != nil {
		t.Fatal(err)
	}
	if c := check("menu"); c.Action != open("Potluck Menu.app") {
		t.Errorf("the one that asked is gone: %+v", c)
	}
}

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
	if c := byID(s, "daemon"); c.Do == nil || strings.Join(c.Do.Args, " ") != "--json daemon --detach" {
		t.Errorf("daemon: %+v", c.Do)
	}
	upgrade := byID(s, "updates").Do

	app := filepath.Join(apps, menubar.AppName)
	if err := os.MkdirAll(filepath.Join(app, "Contents", "MacOS"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(app, "Contents", "MacOS", "PotluckMenu"), nil, 0o755); err != nil {
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

// `potluck daemon stop` marks the home: the app then neither starts the daemon
// by itself nor asks for attention because it is down.
func TestHostSetupReadsThatThePersonStoppedTheDaemon(t *testing.T) {
	home := t.TempDir()
	t.Setenv(EnvHome, home)
	a := programApp(Program{}, nil, io.Discard, io.Discard)
	if f := a.setupFacts(); f.Stopped || !f.AutoStart {
		t.Fatalf("not stopped: Stopped %t, AutoStart %t", f.Stopped, f.AutoStart)
	}
	if err := markStopped(home); err != nil {
		t.Fatal(err)
	}
	if f := a.setupFacts(); !f.Stopped || f.AutoStart {
		t.Errorf("stopped: Stopped %t, AutoStart %t", f.Stopped, f.AutoStart)
	}
}
