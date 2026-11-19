package menubar

import (
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/hostsetup"
	"github.com/sengirunners/potluck/protocol"
)

func stages(names ...string) []hostsetup.Stage {
	var out []hostsetup.Stage
	for _, n := range names {
		out = append(out, hostsetup.Stage{Name: n, Result: hostsetup.Succeeded})
	}
	return out
}

func check(s Setup, id string) Check {
	for _, c := range s.Checks {
		if c.ID == id {
			return c
		}
	}
	return Check{}
}

// A blocked or cut-off setup: the menu's title and note and the checklist
// say the same stage, and the next step is the one way on.
func TestSetupBlockedMenuAndChecklistAgree(t *testing.T) {
	now = func() time.Time { return time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC) }
	t.Cleanup(func() { now = time.Now })
	blocked := stages(hostsetup.Preflight, hostsetup.Images)
	blocked = append(blocked, hostsetup.Stage{Name: hostsetup.Validation, Result: hostsetup.Blocked, Detail: "conformance failed at boot"})
	base := Facts{Cores: 8, Host: &protocol.HostStatus{Mode: "auto", State: "accepting"}, Start: "potluck daemon --detach", Share: "potluck host"}

	f := base
	f.Setup.Run = &hostsetup.Run{Started: "x", Ended: "y", Again: "potluck host trial", Stages: blocked}
	st := Build(f)
	if st.Menu.Title != "Setup blocked at validation (3 of 5)" || st.Menu.Note != "conformance failed at boot; then run again: potluck host trial" {
		t.Errorf("menu: %q / %q", st.Menu.Title, st.Menu.Note)
	}
	if c := check(st.Setup, "setup"); st.Setup.State != SetupBlocked || c.State != Blocked || !strings.Contains(c.Detail, "setup blocked at validation (3 of 5): conformance failed at boot") {
		t.Errorf("setup check: %s %+v", st.Setup.State, c)
	}
	if st.Setup.Next != "fix what the validation stage says, then run again: potluck host trial" {
		t.Errorf("next %q", st.Setup.Next)
	}
	if c := check(st.Setup, "lending"); c.State != Todo || c.Action != "" {
		t.Errorf("a blocked setup's lending check is not a second next step: %+v", c)
	}

	// Going on now: the menu names the stage and its words.
	f.Setup.Run = &hostsetup.Run{Started: "x", Again: "potluck host trial", Stages: append(stages(hostsetup.Preflight),
		hostsetup.Stage{Name: hostsetup.Images, Result: hostsetup.Running, Detail: "image 1 of 2: pull a"})}
	f.Setup.RunLive = true
	if st := Build(f); st.Menu.Title != "Setting up: images (2 of 5)" || st.Menu.Note != "image 1 of 2: pull a" || st.Setup.State != SetupRunning || st.Setup.Next != "" {
		t.Errorf("running: %q / %q, %s next %q", st.Menu.Title, st.Menu.Note, st.Setup.State, st.Setup.Next)
	}
	// Just started, no stage yet.
	f.Setup.Run = &hostsetup.Run{Started: "x", Again: "potluck host trial", Stages: []hostsetup.Stage{}}
	if st := Build(f); st.Menu.Title != "Setting up: preflight (1 of 5)" || st.Setup.State != SetupRunning {
		t.Errorf("just started: %q %s", st.Menu.Title, st.Setup.State)
	}
	f.Setup.Run = &hostsetup.Run{Started: "x", Again: "potluck host trial", Stages: append(stages(hostsetup.Preflight),
		hostsetup.Stage{Name: hostsetup.Images, Result: hostsetup.Running, Detail: "image 1 of 2: pull a"})}
	// The run is gone without ending: cut off, and the way on is to run it again.
	f.Setup.RunLive = false
	if st := Build(f); st.Menu.Title != "Setup blocked at images (2 of 5)" || st.Setup.State != SetupInterrupted || st.Setup.Next != "potluck host trial" {
		t.Errorf("cut off: %q, %s next %q", st.Menu.Title, st.Setup.State, st.Setup.Next)
	}

	// While sizes from an earlier run still lend, a blocked re-run is a note.
	f.Setup.Run = &hostsetup.Run{Started: "x", Ended: "y", Again: "potluck host trial", Stages: blocked}
	f.Targets = []computeapi.RunTargetView{box("c2", "", "linux", 2, 2, nil, nil)}
	if st := Build(f); st.Menu.Title != "Lending 4 of 8 cores" || !strings.Contains(st.Menu.Note, "the last setup run was blocked at validation (3 of 5)") {
		t.Errorf("lending, re-run blocked: %q / %q", st.Menu.Title, st.Menu.Note)
	}
}

// The checklist on a Mac: install, launch (only the app's own ask counts),
// Login Items Potluck cannot read, and the daily update, which covers
// Potluck alone; each gives the next step in turn, and none is done for the
// owner.
func TestSetupChecklistOnAMac(t *testing.T) {
	at := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	now = func() time.Time { return at }
	t.Cleanup(func() { now = time.Now })
	lending := box("c2", "", "macos", 2, 2, nil, nil)
	lending.Config["reclaim_idle_s"] = "300"
	f := Facts{Cores: 8, Host: &protocol.HostStatus{Mode: "auto", State: "holding"}, Start: "potluck daemon --detach", Share: "potluck host",
		Targets: []computeapi.RunTargetView{lending}}
	f.Setup = SetupFacts{Version: "v1.2.3", InstallMenu: "install-menu", Upgrade: "potluck upgrade enable", Doctor: "potluck doctor",
		Run:     &hostsetup.Run{Started: "x", Ended: "y", Stages: stages(hostsetup.Order...)},
		Menu:    &MenuApp{},
		Updates: &Updates{}}
	st := Build(f)
	if st.Setup.State != SetupSucceeded || check(st.Setup, "setup").State != Done || check(st.Setup, "lending").State != Done {
		t.Fatalf("set up: %+v", st.Setup)
	}
	if w := check(st.Setup, "when"); !strings.Contains(w.Detail, "only while you are away: a job starts after 300 s") || !strings.Contains(w.Detail, "now it waits") {
		t.Errorf("when: %q", w.Detail)
	}
	if st.Setup.Next != "install-menu" || check(st.Setup, "login").ID != "" {
		t.Errorf("no app: next %q, %+v", st.Setup.Next, st.Setup.Checks)
	}
	// Installed but it never asked: not running, whatever opening it said.
	f.Setup.Menu = &MenuApp{Path: "/Users/b/Applications/Potluck Menu.app", Open: `open "/Users/b/Applications/Potluck Menu.app"`}
	st = Build(f)
	if c := check(st.Setup, "menu"); c.State != Todo || !strings.Contains(c.Detail, "never read this status") || st.Setup.Next != c.Action || c.Action != f.Setup.Menu.Open {
		t.Errorf("installed, never seen: %+v next %q", c, st.Setup.Next)
	}
	if c := check(st.Setup, "login"); c.State != Unknown || !strings.Contains(c.Detail, "System Settings > General > Login Items") {
		t.Errorf("login: %+v", c)
	}
	// Seen two minutes ago: not running now.
	f.Setup.Menu.Seen = at.Add(-2 * time.Minute)
	if c := check(Build(f).Setup, "menu"); c.State != Todo || !strings.Contains(c.Detail, "not running now") {
		t.Errorf("seen long ago: %+v", c)
	}
	// It asked ten seconds ago: running. Next is the daily update, the
	// owner's choice; its coverage names what it does not update.
	f.Setup.Menu.Seen = at.Add(-10 * time.Second)
	st = Build(f)
	if c := check(st.Setup, "menu"); c.State != Done || !strings.Contains(c.Detail, "read this status at 11:59:50") {
		t.Errorf("seen: %+v", c)
	}
	if u := check(st.Setup, "updates"); u.State != Todo || !strings.Contains(u.Detail, "never turns it on by itself") || st.Setup.Next != "potluck upgrade enable" {
		t.Errorf("updates off: %+v next %q", u, st.Setup.Next)
	}
	if c := check(st.Setup, "coverage"); !strings.Contains(c.Detail, "covers Potluck itself only") || !strings.Contains(c.Detail, "install-menu") || !strings.Contains(c.Detail, "Tart and Softnet") {
		t.Errorf("coverage: %+v", c)
	}
	// All on: nothing Potluck can read is left (Login Items it cannot read
	// is never the next step).
	f.Setup.Updates = &Updates{Enabled: true, Loaded: true}
	if st := Build(f); st.Setup.Next != "" || check(st.Setup, "updates").State != Done {
		t.Errorf("all done: next %q %+v", st.Setup.Next, st.Setup.Checks)
	}
	// Installed but not loaded is not on.
	f.Setup.Updates = &Updates{Enabled: true}
	if st := Build(f); check(st.Setup, "updates").State != Todo || st.Setup.Next != "potluck upgrade enable" {
		t.Errorf("not loaded: %+v", st.Setup.Checks)
	}
	// No daemon: start it first.
	f.Daemon = "connection refused"
	if st := Build(f); st.Setup.Next != "potluck daemon --detach" || check(st.Setup, "when").ID != "" {
		t.Errorf("no daemon: next %q %+v", st.Setup.Next, st.Setup.Checks)
	}
	// Never set up: share it.
	f.Daemon, f.Targets, f.Setup.Run = "", nil, nil
	if st := Build(f); st.Setup.State != SetupNone || st.Setup.Next != "potluck host" || st.Menu.Title != "Not lending" {
		t.Errorf("never set up: %s next %q %q", st.Setup.State, st.Setup.Next, st.Menu.Title)
	}
}
