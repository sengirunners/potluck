package menubar

import (
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/potluck/hostprobe"
	"github.com/sengirunners/potluck/hostsetup"
	"github.com/sengirunners/potluck/protocol"
)

// viewCases are the window's states: the fixtures and what they leave out.
func viewCases(t *testing.T) map[string]Facts {
	t.Cleanup(func() { now = time.Now })
	cases := statusFixtures()
	lending := cases["status-lending.json"]
	add := func(name string, f Facts, edit func(f *Facts)) {
		m := fineMac()
		f.Setup.Mac = &m
		edit(&f)
		cases[name] = f
	}
	never := lending
	never.Setup.Run, never.Targets = nil, nil
	mac := func(edit func(m *hostprobe.MacParts)) func(f *Facts) { return func(f *Facts) { edit(f.Setup.Mac) } }
	add("intel", never, mac(func(m *hostprobe.MacParts) { m.Report.Arch = "amd64" }))
	add("old macOS", never, mac(func(m *hostprobe.MacParts) { m.Report.MacOS = "12.1" }))
	add("no virtualization", never, mac(func(m *hostprobe.MacParts) { m.HV = false }))
	add("a virtual machine", never, mac(func(m *hostprobe.MacParts) { m.HV, m.InVM = false, true }))
	add("unread", never, mac(func(m *hostprobe.MacParts) {
		m.HVUnread, m.Tart.Unread, m.Softnet.Unread, m.Report.TartDiskUnread = "x /y", "x /y", "x /y", "x /y"
		m.Tart.Problems, m.Softnet.Problems = []string{"x"}, []string{"x"}
	}))
	add("no Tart", never, mac(func(m *hostprobe.MacParts) { m.Tart = hostprobe.Layer{} }))
	add("old Homebrew Tart", never, mac(func(m *hostprobe.MacParts) { m.Tart.Problems = []string{"old"} }))
	add("no Softnet", never, mac(func(m *hostprobe.MacParts) { m.Softnet = hostprobe.Layer{Problems: []string{"none"}} }))
	add("low disk", never, mac(func(m *hostprobe.MacParts) { m.Report.TartFreeGB = 12 }))
	add("other", never, mac(func(m *hostprobe.MacParts) { m.Rest.Problems = []string{"x"} }))
	add("ready", never, func(*Facts) {})
	add("stopped daemon", lending, func(f *Facts) { f.Daemon = "connection refused" })
	add("lending", lending, func(*Facts) {})
	add("paused", lending, func(f *Facts) { f.Host = &protocol.HostStatus{Mode: protocol.HostClosed, State: "closed"} })
	add("in use", lending, func(f *Facts) { f.Host = &protocol.HostStatus{Mode: "auto", State: "holding"} })
	add("winding down", lending, func(f *Facts) { f.Host = &protocol.HostStatus{Mode: protocol.HostDrain, State: "draining"} })
	add("switch unread", lending, func(f *Facts) { f.Host, f.HostErr = nil, "x /y" })
	add("limits unread", lending, func(f *Facts) { f.LendErr = "x /y" })
	add("lending, no record", lending, func(f *Facts) { f.Setup.Run = nil })
	add("stopped, no record", lending, func(f *Facts) { f.Setup.Run, f.Targets, f.Daemon = nil, nil, "refused" })
	add("Tart to install", never, func(f *Facts) {
		f.Setup.Mac.Tart, f.Setup.TartInstall = hostprobe.Layer{Problems: []string{"none"}}, "potluck tart install"
	})
	add("unlisted, no record, no Tart", never, func(f *Facts) { f.ListErr, f.Setup.Mac.Tart = "timed out", hostprobe.Layer{} })
	add("unlisted, no record", never, func(f *Facts) { f.ListErr = "timed out" })
	add("set up, stopped, Tart gone", lending, func(f *Facts) {
		f.Targets, f.Daemon, f.Setup.Mac.Tart = nil, "refused", hostprobe.Layer{}
	})
	add("helper without permission", never, mac(func(m *hostprobe.MacParts) {
		m.Report.MacOS = "15.1"
		m.Softnet = hostprobe.Layer{Problems: []string{"no"}, RootSteps: []string{"chown /x"}}
	}))
	add("a refused setting with a fix", never, func(f *Facts) {
		f.Setup.Run = &hostsetup.Run{Started: "x", Ended: "y", AgainArgs: []string{"host"}, NeedsInvite: true, Stages: []hostsetup.Stage{{Name: hostsetup.Preflight, Result: hostsetup.Blocked, Detail: "x", Flag: "--docker=true", Fix: "without it"}}}
	})
	add("a run that kept no record", never, func(f *Facts) { f.Background = &Background{State: "ended", Last: "x /y"} })
	add("unlisted", lending, func(f *Facts) { f.Targets, f.ListErr = nil, "timed out" })
	add("unreadable record", never, func(f *Facts) { f.Setup.RunErr = "not JSON" })
	add("nothing shared", lending, func(f *Facts) { f.Targets = nil })
	add("lending, Tart gone", lending, mac(func(m *hostprobe.MacParts) { m.Tart = hostprobe.Layer{} }))
	for _, n := range hostsetup.Order {
		add("blocked at "+n, never, func(f *Facts) {
			f.Setup.Run = &hostsetup.Run{Started: "x", Ended: "y", AgainArgs: []string{"host"}, NeedsInvite: true, Stages: []hostsetup.Stage{{Name: n, Result: hostsetup.Blocked, Detail: "--x /y `z`"}}}
		})
		add("cut off at "+n, never, func(f *Facts) {
			f.Setup.Run = &hostsetup.Run{Started: "x", AgainArgs: []string{"host"}, Stages: []hostsetup.Stage{{Name: n, Result: hostsetup.Running}}}
		})
	}
	add("a refused setting", never, func(f *Facts) {
		f.Setup.Run = &hostsetup.Run{Started: "x", Ended: "y", Stages: []hostsetup.Stage{{Name: hostsetup.Preflight, Result: hostsetup.Blocked, Detail: "x", Flag: "--docker=true"}}}
	})
	add("lending, last setup blocked", lending, func(f *Facts) {
		f.Setup.Run = &hostsetup.Run{Started: "x", Ended: "y", Stages: []hostsetup.Stage{{Name: hostsetup.Images, Result: hostsetup.Blocked, Detail: "x"}}}
	})
	return cases
}

// Nothing the window shows is for a programmer: no command, path, flag or
// inside word. Only Details, which it copies and never shows, may hold them.
func TestSetupViewIsInPlainWords(t *testing.T) {
	// Go, not the app, makes these, so this is a check of syntax and fixed
	// words, never of meaning.
	bad := regexp.MustCompile(`(?i)potluck (host|softnet|doctor|upgrade|daemon)\b|daemon|--|/|` + "`" + `|sudo|terminal|preflight|conformance|softnet install|setuid|virtuali|run target|\bCLI\b|\bVM\b`)
	for name, f := range viewCases(t) {
		v := Build(f).Setup.View
		texts := []string{v.Heading, v.Lead, v.Progress.Title}
		do := func(d *Do) {
			if d != nil {
				texts = append(texts, d.Label)
			}
		}
		do(v.Do)
		for _, s := range append(v.Sections, v.Progress) {
			texts = append(texts, s.Title)
			for _, r := range s.Rows {
				if r.Text == "" {
					t.Errorf("%s: an empty line in %q", name, s.Title)
				}
				texts = append(texts, r.Text)
				do(r.Do)
			}
		}
		for _, text := range texts {
			if m := bad.FindString(text); m != "" {
				t.Errorf("%s: %q in %q", name, m, text)
			}
		}
		if v.Heading == "" || v.Lead == "" {
			t.Errorf("%s: heading %q, lead %q", name, v.Heading, v.Lead)
		}
		// The lead names only buttons that are there.
		if strings.Contains(v.Lead, "Copy details") && v.Details == "" {
			t.Errorf("%s: nothing to copy: %q", name, v.Lead)
		}
		if regexp.MustCompile(`(?i)press the button`).MatchString(v.Lead) && v.Do == nil {
			t.Errorf("%s: no button: %q", name, v.Lead)
		}
		if v.Do != nil && v.Do.InviteArgs != nil && !regexp.MustCompile(`(?i)paste (your|the) invite`).MatchString(v.Lead) {
			t.Errorf("%s: an invite field the lead does not name: %q", name, v.Lead)
		}
	}
}

// The heading is true, and there is at most one clear thing to press.
func TestSetupViewHeadings(t *testing.T) {
	cases := viewCases(t)
	for name, want := range map[string]struct{ heading, button string }{
		"status-probe-fresh-mac.json":    {"This Mac isn't set up yet", "Install Potluck"}, // the helper it waits on is not needed to install Potluck
		"status-probe-fresh-mac-26.json": {"This Mac isn't set up yet", "Install Potluck"},
		"status-probe-no-helper.json":    {"This Mac isn't set up yet", ""},
		"status-probe-ready.json":        {"This Mac isn't set up yet", "Set up this Mac"},
		"status-setup-running.json":      {"Setting up this Mac", ""},
		"status-join-sign-in.json":       {"Sign in to finish setting up", "Sign in"},
		"status-blocked-validation.json": {"Setup stopped", "Try again"},
		"status-lending.json":            {"This Mac is lending", ""},
		"intel":                          {"This Mac can't lend", ""},
		"a virtual machine":              {"This Mac can't lend", ""},
		"low disk":                       {"This Mac isn't set up yet", ""},
		"no Softnet":                     {"This Mac isn't set up yet", "Install the network helper"},
		"stopped daemon":                 {"Potluck isn't running", "Start Potluck"},
		"paused":                         {"Lending is paused", ""},
		"in use":                         {"This Mac is ready and waiting", ""},
		"unlisted":                       {"Potluck can't tell what this Mac lends", ""},
		"unlisted, no record":            {"Potluck can't tell what this Mac lends", ""},
		"unlisted, no record, no Tart":   {"This Mac needs attention", "Open the download page"},
		"Tart to install":                {"This Mac isn't set up yet", "Install Tart"},
		"winding down":                   {"Lending is winding down", ""},
		"switch unread":                  {"Lending is on hold", ""},
		"limits unread":                  {"Lending is on hold", ""},
		"lending, no record":             {"This Mac is lending", ""},
		"stopped, no record":             {"Potluck isn't running", "Start Potluck"},
		"set up, stopped, Tart gone":     {"This Mac needs attention", "Open the download page"},
		"helper without permission":      {"This Mac isn't set up yet", ""},
		"unreadable record":              {"Potluck can't tell how setup went", "Set up again"},
		"nothing shared":                 {"This Mac isn't lending", "Lend this Mac"},
		"lending, Tart gone":             {"This Mac needs attention", "Open the download page"},
		"cut off at images":              {"Setup stopped", "Continue setup"},
		"a refused setting":              {"Setup stopped", ""},
		"lending, last setup blocked":    {"This Mac is set up, but the last change didn't finish", "Try again"},
	} {
		f, ok := cases[name]
		if !ok {
			t.Fatalf("no case %s", name)
		}
		v := Build(f).Setup.View
		button := ""
		if v.Do != nil {
			button = v.Do.Label
		}
		if v.Heading != want.heading || button != want.button {
			t.Errorf("%s: %q [%s], want %q [%s]", name, v.Heading, button, want.heading, want.button)
		}
	}
}

// A setup that goes on is the window's step even when a check before it is
// open, and an optional step never is its button.
func TestSetupViewStep(t *testing.T) {
	cases := viewCases(t)
	f := cases["status-setup-running.json"]
	m := fineMac()
	m.Softnet = hostprobe.Layer{Problems: []string{"not installed"}}
	f.Setup.Mac = &m
	if v := Build(f).Setup.View; v.Heading != "Setting up this Mac" || v.Do != nil {
		t.Errorf("running: %q %+v", v.Heading, v.Do)
	}
	s := Build(cases["status-lending.json"]).Setup
	if s.NextID != "updates" || s.View.Do != nil {
		t.Errorf("lending: the terminal's next_id %q, the window's button %+v", s.NextID, s.View.Do)
	}
	optional := s.View.Sections[len(s.View.Sections)-1]
	if optional.Title != "Optional" || optional.Rows[len(optional.Rows)-1].Do == nil {
		t.Errorf("optional %+v", optional)
	}
	for _, r := range optional.Rows {
		if r.State == Todo {
			t.Errorf("an optional line drawn as left to do: %+v", r)
		}
	}
}

// Key is the same with and without the Mac's own checks, and changes with
// the steps.
func TestSetupViewKey(t *testing.T) {
	seen := map[string]string{}
	for name, f := range viewCases(t) {
		probed := Build(f).Setup.View.Key
		f.Setup.Mac = nil
		if plain := Build(f).Setup.View.Key; plain != probed {
			t.Errorf("%s: %q with --probe, %q without", name, probed, plain)
		}
		seen[name] = probed
	}
	for _, pair := range [][2]string{{"ready", "status-setup-running.json"}, {"status-setup-running.json", "status-join-sign-in.json"},
		{"status-setup-running.json", "status-blocked-validation.json"}, {"status-setup-running.json", "lending"}, {"lending", "stopped daemon"},
		{"ready", "a run that kept no record"}, {"lending", "winding down"}, {"lending", "paused"}, {"lending", "in use"}, {"lending", "switch unread"}, {"lending", "limits unread"}} {
		if seen[pair[0]] == seen[pair[1]] {
			t.Errorf("%v: the same key %q", pair, seen[pair[0]])
		}
	}
}

// Details holds what a helper needs and is empty where nothing is wrong.
func TestSetupViewDetails(t *testing.T) {
	cases := viewCases(t)
	for name, want := range map[string]string{"blocked at images": "--x /y", "a run that kept no record": "x /y", "switch unread": "x /y", "limits unread": "x /y", "helper without permission": "chown /x", "intel": "Intel", "ready": "", "lending": "", "status-never-set-up.json": ""} {
		d := Build(cases[name]).Setup.View.Details
		if want == "" && d != "" || !strings.Contains(d, want) {
			t.Errorf("%s: details %q", name, d)
		}
	}
}

// The Tart line keeps the download page for when installing fails.
func TestSetupViewOtherWay(t *testing.T) {
	for _, r := range Build(viewCases(t)["Tart to install"]).Setup.View.Sections[0].Rows {
		if r.Text == "Tart isn't installed yet" {
			if r.Do == nil || r.Do.URL != hostprobe.TartReleases {
				t.Errorf("%+v", r)
			}
			return
		}
	}
	t.Error("no Tart line")
}

// A Mac that is itself a virtual machine is told so, not that the check did
// not answer.
func TestSetupViewVirtualMachine(t *testing.T) {
	v := Build(viewCases(t)["a virtual machine"]).Setup.View
	if !strings.Contains(v.Lead, "virtual machine") || strings.Contains(v.Lead, "answer") || v.Do != nil {
		t.Errorf("lead %q, button %v", v.Lead, v.Do)
	}
}

// An invite from a link is kept for the step that takes it while one is to
// come, even when the step in front waits on Potluck's network helper; a Mac
// that lends, or cannot lend at all, takes none.
func TestSetupViewInviteLater(t *testing.T) {
	cases := viewCases(t)
	for name, want := range map[string]bool{
		"status-probe-fresh-mac.json": true,
		"status-probe-no-helper.json": true,
		"status-probe-ready.json":     true,
		"status-never-set-up.json":    true,
		"intel":                       false,
		"a virtual machine":           false,
		"status-lending.json":         false,
		"lending":                     false,
		"status-daemon-down.json":     false, // its button starts Potluck, which takes no invite
	} {
		f, ok := cases[name]
		if !ok {
			t.Fatalf("no case %s", name)
		}
		if got := Build(f).Setup.View.InviteLater; got != want {
			t.Errorf("%s: invite_later %t, want %t", name, got, want)
		}
	}
}
