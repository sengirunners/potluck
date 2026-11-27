package menubar

import (
	"slices"
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
	// The menu is a person's: plain words and where to look, no command.
	if st.Menu.Title != "Setup stopped while testing that jobs run safely" || st.Menu.Note != "" {
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
	if st := Build(f); st.Menu.Title != "Setting up: download what jobs need" || st.Menu.Note != "image 1 of 2: pull a" || st.Setup.State != SetupRunning || st.Setup.Next != "" {
		t.Errorf("running: %q / %q, %s next %q", st.Menu.Title, st.Menu.Note, st.Setup.State, st.Setup.Next)
	}
	// Just started, no stage yet.
	f.Setup.Run = &hostsetup.Run{Started: "x", Again: "potluck host trial", Stages: []hostsetup.Stage{}}
	if st := Build(f); st.Menu.Title != "Setting up: check this Mac" || st.Setup.State != SetupRunning {
		t.Errorf("just started: %q %s", st.Menu.Title, st.Setup.State)
	}
	f.Setup.Run = &hostsetup.Run{Started: "x", Again: "potluck host trial", Stages: append(stages(hostsetup.Preflight),
		hostsetup.Stage{Name: hostsetup.Images, Result: hostsetup.Running, Detail: "image 1 of 2: pull a"})}
	// The run is gone without ending: cut off, and the way on is to run it again.
	f.Setup.RunLive = false
	if st := Build(f); st.Menu.Title != "Setup stopped while downloading what jobs need" || st.Setup.State != SetupInterrupted || st.Setup.Next != "potluck host trial" {
		t.Errorf("cut off: %q, %s next %q", st.Menu.Title, st.Setup.State, st.Setup.Next)
	}

	// While sizes from an earlier run still lend, a blocked re-run is a note.
	f.Setup.Run = &hostsetup.Run{Started: "x", Ended: "y", Again: "potluck host trial", Stages: blocked}
	f.Targets = []computeapi.RunTargetView{box("c2", "", "linux", 2, 2, nil, nil)}
	st = Build(f)
	if st.Menu.Title != "Lending 4 of 8 cores" || !strings.Contains(st.Menu.Note, "the last setup stopped while testing that jobs run safely") ||
		!strings.Contains(st.Menu.Note, "what was shared before still lends") {
		t.Errorf("lending, re-run blocked: %q / %q", st.Menu.Title, st.Menu.Note)
	}
	if c := check(st.Setup, "setup"); !strings.Contains(c.Detail, "the last setup run was blocked at validation (3 of 5): conformance failed at boot; what was shared before still lends") {
		t.Errorf("lending, re-run blocked, checklist: %+v", c)
	}
}

// A record from another Potluck may name a stage this one has no words for:
// the menu names it as the record does, and never fails.
func TestSetupMenuUnknownStage(t *testing.T) {
	f := Facts{Cores: 8, Host: &protocol.HostStatus{Mode: "auto", State: "accepting"}, Start: "potluck daemon --detach", Share: "potluck host"}
	f.Setup.Run = &hostsetup.Run{Started: "x", Ended: "y", Stages: []hostsetup.Stage{{Name: "warmup", Result: hostsetup.Blocked, Detail: "x"}}}
	if st := Build(f); st.Menu.Title != "Setup stopped at warmup" {
		t.Errorf("blocked: %q", st.Menu.Title)
	}
	f.Setup.Run = &hostsetup.Run{Started: "x", Stages: []hostsetup.Stage{{Name: "", Result: hostsetup.Running}}}
	f.Setup.RunLive = true
	if st := Build(f); !strings.HasPrefix(st.Menu.Title, "Setting up") {
		t.Errorf("running: %q", st.Menu.Title)
	}
}

// The run's again_args and needs_invite reach the setup view, and a join's
// login page shows only while the run goes on: a cut-off run's is a page
// nobody waits on.
func TestSetupCarriesAgainArgsAndOpenURL(t *testing.T) {
	const url = "https://login.example/a/1"
	f := Facts{Cores: 8, Host: &protocol.HostStatus{Mode: "auto", State: "accepting"}, Start: "potluck daemon --detach", Share: "potluck host"}
	f.Setup.Run = &hostsetup.Run{Started: "x", Again: "potluck host '<invite code>' --no-sudo=true", AgainArgs: []string{"host", "--no-sudo=true"}, NeedsInvite: true,
		Stages: append(stages(hostsetup.Preflight, hostsetup.Images, hostsetup.Validation), hostsetup.Stage{Name: hostsetup.Join, Result: hostsetup.Running, OpenURL: url})}
	f.Setup.RunLive = true
	st := Build(f)
	if st.Setup.Stages[3].OpenURL != url || !slices.Equal(st.Setup.AgainArgs, []string{"host", "--no-sudo=true"}) || !st.Setup.NeedsInvite {
		t.Errorf("running: %+v", st.Setup)
	}
	f.Setup.RunLive = false
	if st := Build(f); st.Setup.State != SetupInterrupted || st.Setup.Stages[3].OpenURL != "" || f.Setup.Run.Stages[3].OpenURL != url {
		t.Errorf("cut off: %s %+v (the record itself is left alone: %+v)", st.Setup.State, st.Setup.Stages, f.Setup.Run.Stages[3])
	}
}

// A setup blocked by a flag the layer this machine shares on does not take: the next step is
// the command without it (the record's Again, changed by the run), and it
// names the flag, never the refused command again.
func TestSetupBlockedByAFlagNamesIt(t *testing.T) {
	base := Facts{Cores: 8, Host: &protocol.HostStatus{Mode: "auto", State: "accepting"}, Start: "potluck daemon --detach", Share: "potluck host"}
	refused := hostsetup.Stage{Name: hostsetup.Preflight, Result: hostsetup.Blocked, Detail: "--docker is for the oci layer", Flag: "--docker", Fix: "without --docker=true"}
	f := base
	f.Setup.Run = &hostsetup.Run{Started: "x", Ended: "y", Again: "zm potluck host pool --linux-slots 8", Stages: []hostsetup.Stage{refused}}
	st := Build(f)
	want := "run again without --docker=true (the layer this machine shares on does not take --docker): zm potluck host pool --linux-slots 8"
	if st.Setup.Next != want || strings.Contains(st.Menu.Note, "potluck") {
		t.Errorf("next %q, note %q", st.Setup.Next, st.Menu.Note)
	}
	refused.Fix = ""
	f.Setup.Run.Stages = []hostsetup.Stage{refused}
	if got := Build(f).Setup.Next; got != "run again without --docker (the layer this machine shares on does not take it): zm potluck host pool --linux-slots 8" {
		t.Errorf("no fix: %q", got)
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
	f.Setup.Menu = &MenuApp{Path: "/Users/b/Applications/Potluck.app", Open: `open "/Users/b/Applications/Potluck.app"`}
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
	if d := check(Build(f).Setup, "daemon").Do; d == nil || d.Auto {
		t.Errorf("a daemon a person stopped: %+v", d)
	}
	f.Setup.AutoStart = true
	if d := check(Build(f).Setup, "daemon").Do; d == nil || !d.Auto {
		t.Errorf("no daemon after a restart: %+v", d)
	}
	// Cut off part way, by that restart too.
	done := *f.Setup.Run
	done.Ended, done.Stages = "", done.Stages[:2]
	f.Setup.Run = &done
	if st := Build(f); st.Setup.State != SetupInterrupted || check(st.Setup, "daemon").Do == nil || !check(st.Setup, "daemon").Do.Auto {
		t.Errorf("a setup a restart cut off: %s %+v", st.Setup.State, check(st.Setup, "daemon").Do)
	}
	// Never set up: share it.
	f.Daemon, f.Targets, f.Setup.Run = "", nil, nil
	if st := Build(f); st.Setup.State != SetupNone || st.Setup.Next != "potluck host" || st.Menu.Title != "Not lending" {
		t.Errorf("never set up: %s next %q %q", st.Setup.State, st.Setup.Next, st.Menu.Title)
	}
}

// The app never reads a check's words to know what to run: every check
// with an action has a do that says it, next_id names the check next is
// from, and no do's arguments hold the invite (it goes on stdin).
func TestEveryActionHasADo(t *testing.T) {
	t.Cleanup(func() { now = time.Now })
	cases := statusFixtures()
	at := now()
	base := cases["status-lending.json"]
	add := func(name string, edit func(f *Facts)) {
		f := base
		f.Setup.Menu, f.Setup.Updates = &MenuApp{}, &Updates{}
		if base.Setup.Run != nil {
			r := *base.Setup.Run
			f.Setup.Run = &r
		}
		edit(&f)
		cases[name] = f
	}
	blocked := func(flag, fix string) *hostsetup.Run {
		args := []string{"host"} // as hostsetup.End leaves it: with the fix, or unchanged when there is none
		if fix != "" {
			args = append(args, "--docker=false")
		}
		return &hostsetup.Run{Started: "x", Ended: "y", Again: "potluck host '<invite code>'", AgainArgs: args, NeedsInvite: true,
			Stages: append(stages(hostsetup.Preflight), hostsetup.Stage{Name: hostsetup.Images, Result: hostsetup.Blocked, Detail: "no", Flag: flag, Fix: fix})}
	}
	add("never, no daemon", func(f *Facts) { f.Setup.Run, f.Daemon, f.Targets = nil, "down", nil })
	add("never", func(f *Facts) { f.Setup.Run, f.Targets = nil, nil })
	add("blocked by a flag", func(f *Facts) { f.Setup.Run, f.Targets = blocked("--docker", "with --docker=false"), nil })
	add("blocked by a flag it keeps", func(f *Facts) { f.Setup.Run, f.Targets = blocked("--docker", ""), nil })
	add("blocked", func(f *Facts) { f.Setup.Run, f.Targets = blocked("", ""), nil })
	add("join on a page that is no login", func(f *Facts) {
		f.Setup.Run, f.Setup.RunLive, f.Targets = &hostsetup.Run{Started: "x", Again: "potluck host trial",
			Stages: []hostsetup.Stage{{Name: hostsetup.Join, Result: hostsetup.Running, OpenURL: "file:///etc/passwd"}}}, true, nil
	})
	add("interrupted", func(f *Facts) {
		f.Setup.Run, f.Targets = &hostsetup.Run{Started: "x", Again: "potluck host trial", AgainArgs: []string{"host", "trial"}, Stages: stages(hostsetup.Preflight)}, nil
	})
	add("interrupted, run with --yes", func(f *Facts) {
		f.Setup.Run, f.Targets = &hostsetup.Run{Started: "x", Again: "potluck host trial --yes", AgainArgs: []string{"host", "trial", "--yes=true"}, Stages: stages(hostsetup.Preflight)}, nil
	})
	add("old record", func(f *Facts) {
		f.Setup.Run, f.Targets = &hostsetup.Run{Started: "x", Again: "potluck host trial", Stages: stages(hostsetup.Preflight)}, nil
	})
	add("succeeded, nothing shared", func(f *Facts) { f.Targets = nil })
	add("unreadable", func(f *Facts) { f.Setup.Run, f.Setup.RunErr = nil, "bad json" })
	add("menu installed", func(f *Facts) { f.Setup.Menu = &MenuApp{Path: "/Applications/Potluck.app", Open: "open x"} })
	add("menu stale", func(f *Facts) {
		f.Setup.Menu = &MenuApp{Path: "/Applications/Potluck.app", Open: "open x", Seen: at.Add(-time.Hour)}
	})
	for _, l := range []string{LoginOn, LoginOff, LoginRefused} {
		add("login "+l, func(f *Facts) {
			f.Setup.Menu = &MenuApp{Path: "/Applications/Potluck.app", Open: "open x", Seen: at, Login: l}
		})
	}
	add("succeeded, no daemon", func(f *Facts) { f.Daemon, f.Targets = "down", nil })
	add("updates not loaded", func(f *Facts) { f.Setup.Updates = &Updates{Enabled: true} })

	ids := map[string]bool{}
	for name, f := range cases {
		s := Build(f).Setup
		if len(s.StageOrder) != len(hostsetup.Order) {
			t.Errorf("%s: stage_order %v", name, s.StageOrder)
		}
		for _, st := range s.Stages {
			if st.OpenURL != "" && !strings.HasPrefix(st.OpenURL, "https://") {
				t.Errorf("%s: stage %s opens %q", name, st.Name, st.OpenURL)
			}
		}
		for _, c := range s.Checks {
			if name == "blocked by a flag it keeps" && c.ID == "setup" { // running again fails the same way
				if c.Action == "" || c.Do != nil {
					t.Errorf("%s: %+v %+v", name, c, c.Do)
				}
				continue
			}
			if (c.Action != "") != (c.Do != nil) {
				t.Errorf("%s: %s: action %q, do %+v", name, c.ID, c.Action, c.Do)
				continue
			}
			if c.Do == nil {
				continue
			}
			ids[c.ID] = true
			n := 0
			for _, set := range []bool{len(c.Do.Args) > 0, c.Do.URL != "", c.Do.App != ""} {
				if set {
					n++
				}
			}
			if n != 1 || c.Do.Label == "" || len(c.Do.InviteArgs) > 0 && len(c.Do.Args) == 0 {
				t.Errorf("%s: %s: do %+v", name, c.ID, c.Do)
			}
			for _, a := range append(slices.Clone(c.Do.Args), c.Do.InviteArgs...) {
				if strings.Contains(a, "invite code") {
					t.Errorf("%s: %s: args hold the invite: %q", name, c.ID, c.Do.Args)
				}
			}
		}
		// next comes from next_id's check; next_id alone is a blocked check
		// with no action.
		if c := check(s, s.NextID); s.Next != "" && s.NextID == "" || s.NextID != "" && c.Action != s.Next ||
			s.NextID != "" && s.Next == "" && c.State != Blocked {
			t.Errorf("%s: next %q from %q", name, s.Next, s.NextID)
		}
	}
	for _, id := range []string{"daemon", "setup", "lending", "menu", "login", "updates"} {
		if !ids[id] {
			t.Errorf("no case gives %s a do", id)
		}
	}

	// Only a Mac whose setup was begun starts its daemon without a press, and
	// nothing else is done without one.
	for name, f := range cases {
		f.Setup.AutoStart = true
		for _, c := range Build(f).Setup.Checks {
			if want := f.Daemon != "" && Build(f).Setup.State != SetupNone && !notInstalled(f.Setup) && c.ID == "daemon"; c.Do != nil && c.Do.Auto != want {
				t.Errorf("%s: %s: auto %v", name, c.ID, c.Do.Auto)
			}
		}
	}

	// The login item: a button for each state, and one that turns it off.
	for l, want := range map[string]string{LoginOn: ActLoginOff, LoginOff: ActLoginOn, LoginRefused: ActLoginSettings} {
		s := Build(cases["login "+l]).Setup
		got := ""
		for _, r := range s.View.Sections[1].Rows {
			if r.Do != nil && r.Do.App != "" {
				got += r.Do.App
			}
		}
		if got != want || (check(s, "login").State == Done) != (l == LoginOn) {
			t.Errorf("login %s: acts %q, check %+v", l, got, check(s, "login"))
		}
	}

	// The setup button for each record.
	for name, want := range map[string]string{
		"never":                           "--json host --yes --no-sudo --detach --instead-of-invite= -- [+ --invite-stdin]",
		"blocked by a flag":               "--json host --docker=false --yes --no-sudo --detach --instead-of-invite= -- [+ --invite-stdin]",
		"interrupted":                     "--json host trial --yes --no-sudo --detach",
		"interrupted, run with --yes":     "--json host trial --yes=true --no-sudo --detach",
		"old record":                      "--json host --yes --no-sudo --detach --instead-of-invite= -- [+ --invite-stdin]",
		"join on a page that is no login": "",
	} {
		got := ""
		d := check(Build(cases[name]).Setup, "setup").Do
		if d != nil {
			got = strings.Join(d.Args, " ")
		}
		if d != nil && d.InviteArgs != nil {
			got += " [+" + strings.TrimPrefix(strings.Join(d.InviteArgs, " "), strings.TrimSuffix(strings.TrimSuffix(got, " --"), " --"+InsteadFlag+"=")) + "]" // an older app appends its paste after the --
		}
		if got != want {
			t.Errorf("%s: setup do %q, want %q", name, got, want)
		}
	}
	if s := Build(cases["status-join-sign-in.json"]).Setup; s.NextID != "setup" || check(s, "setup").Do.URL != "https://login.example/a/1" {
		t.Errorf("join: next_id %q, do %+v", s.NextID, check(s, "setup").Do)
	}
}
