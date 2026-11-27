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

// The menu bar's picture is marked when lending stopped and a person has to
// see to it, not on a Mac nobody set up, a paused one or one that still lends.
func TestMenuAttention(t *testing.T) {
	cases := viewCases(t)
	for name, want := range map[string]bool{
		"stopped daemon": true, "switch unread": true, "limits unread": true, "unlisted": true, "unreadable record": false,
		"blocked at images": true, "cut off at images": true, "status-blocked-validation.json": true,
		"status-probe-fresh-mac.json": false, "stopped, no record": false, "ready": false, "nothing shared": false,
		"status-setup-running.json": false, "lending": false, "paused": false, "in use": false, "winding down": false,
	} {
		f, ok := cases[name]
		if !ok {
			t.Fatalf("no case %q", name)
		}
		if got := Build(f).Menu.Attention; got != want {
			t.Errorf("%s: attention %t, want %t", name, got, want)
		}
	}

	lending := cases["lending"]
	for name, c := range map[string]struct {
		edit func(f *Facts)
		want bool
	}{
		"down, first run going on":  {func(f *Facts) { f.Daemon, f.Setup = "refused", cases["status-setup-running.json"].Setup }, false},
		"down, first run cut off":   {func(f *Facts) { f.Daemon, f.Setup = "refused", cases["cut off at images"].Setup }, true},
		"down, first run blocked":   {func(f *Facts) { f.Daemon, f.Setup = "refused", cases["blocked at images"].Setup }, true},
		"down, record unreadable":   {func(f *Facts) { f.Daemon, f.Setup = "refused", cases["unreadable record"].Setup }, true},
		"down, a person stopped it": {func(f *Facts) { f.Daemon, f.Setup.Stopped = "refused", true }, false},
		"down, a person stopped it after a blocked run": {func(f *Facts) {
			f.Daemon, f.Setup, f.Setup.Stopped = "refused", cases["blocked at images"].Setup, true
		}, false},
		"down, paused":                {func(f *Facts) { f.Daemon, f.Host = "refused", cases["paused"].Host }, false},
		"limits unread, paused":       {func(f *Facts) { f.LendErr, f.Host = "x", cases["paused"].Host }, false},
		"limits unread, never set up": {func(f *Facts) { f.LendErr, f.Targets, f.Setup.Run = "x", nil, nil }, false},
		"blocked again, still lends":  {func(f *Facts) { f.Setup.Run = cases["blocked at images"].Setup.Run }, false},
		"no driver answers": {func(f *Facts) {
			f.Targets = []computeapi.RunTargetView{{RunTarget: computeapi.RunTarget{Name: "c2", Kind: "box"}, Error: "dead"}}
		}, true},
		"some drivers answer": {func(f *Facts) {
			f.Targets = append(slices.Clone(f.Targets), computeapi.RunTargetView{RunTarget: computeapi.RunTarget{Name: "c4", Kind: "box"}, Error: "dead"})
		}, false},
		"winding down, limits unread": {func(f *Facts) {
			f.LendErr, f.Host = "x", &protocol.HostStatus{Mode: protocol.HostDrain, State: "draining"}
		}, false},
		"unlisted, no record":      {func(f *Facts) { *f = cases["unlisted, no record"] }, true},
		"limits unread, no record": {func(f *Facts) { f.LendErr, f.Setup.Run = "x", nil }, true},
		"refusing new work": {func(f *Facts) {
			f.Host = &protocol.HostStatus{Mode: "auto", State: "accepting", Refused: &protocol.HostRefusal{Reason: "busy", At: now().Format(time.RFC3339), Until: now().Add(time.Minute).Format(time.RFC3339)}}
		}, false},
	} {
		f := lending
		c.edit(&f)
		if got := Build(f).Menu.Attention; got != c.want {
			t.Errorf("%s: attention %t, want %t", name, got, c.want)
		}
	}

	f := lending
	f.Daemon, f.Setup, f.Setup.Stopped = "refused", cases["blocked at images"].Setup, true
	// The note is the one for sizes that still lend, not the one for a daemon that went down.
	if m := Build(f).Menu; !strings.Contains(m.Note, "what was shared before still lends") {
		t.Errorf("a person stopped the daemon after a blocked run: note %q", m.Note)
	}
	f.Host = &protocol.HostStatus{Mode: protocol.HostUnreadable}
	if m := Build(f).Menu; m.Title != "Potluck is not running" {
		t.Errorf("down, the switch unreadable: title %q, though the dot follows the daemon", m.Title)
	}

	f = lending
	f.Targets = []computeapi.RunTargetView{{RunTarget: computeapi.RunTarget{Name: "c2", Kind: "box"}, Error: "dead"}}
	f.Setup.Run = cases["blocked at images"].Setup.Run
	m := Build(f).Menu
	if m.Title != "Cannot reach what this Mac lends" || !strings.Contains(m.Note, "not counted (no answer): c2") || strings.Contains(m.Note, "still lends") {
		t.Errorf("no driver answers after a blocked run: title %q, note %q", m.Title, m.Note)
	}

	f = lending
	f.Daemon, f.Setup.Run = "refused", cases["cut off at images"].Setup.Run
	blocked := f
	blocked.Setup.Run = cases["blocked at images"].Setup.Run
	if n := Build(blocked).Menu.Note; !strings.Contains(n, "the last setup stopped while downloading what jobs need") {
		t.Errorf("down after a blocked setup: the note %q does not say so", n)
	}
	if n := Build(f).Menu.Note; !strings.Contains(n, "the last setup stopped while downloading what jobs need") {
		t.Errorf("down after a setup cut off: the note %q does not say so", n)
	}
}

// A Mac whose group is gone is marked, unless a person paused it or a new
// setup is running.
func TestMenuAttentionWhenItsPoolEnded(t *testing.T) {
	size := computeapi.RunTargetView{Shared: []string{"friends"}, Live: []string{"a"},
		Describe: &computeapi.RunTargetDescribe{Size: &computeapi.RunTargetSize{CPU: 4, MemoryMB: 1024}}}
	size.Name, size.Kind = "c4", "box"
	done := &hostsetup.Run{Started: "x", Ended: "y", Stages: stages(hostsetup.Order...)}
	running := &hostsetup.Run{Started: "x", Stages: stages(hostsetup.Order[0])}
	for name, c := range map[string]struct {
		mode string
		run  *hostsetup.Run
		want bool
	}{
		"lending":                      {"auto", done, true},
		"paused":                       {protocol.HostClosed, done, false},
		"setup running":                {"auto", running, false},
		"setup running, limits unread": {"auto", running, true},
		"setup running, list unread":   {"auto", running, true},
		"setup running, switch unread": {protocol.HostUnreadable, running, true},
	} {
		f := Facts{Host: &protocol.HostStatus{Mode: c.mode, State: "accepting"}, Share: "potluck host", Targets: []computeapi.RunTargetView{size}}
		f.Setup.Run, f.Setup.PoolsRead, f.Setup.RunLive = c.run, true, c.run == running
		switch name {
		case "setup running, limits unread":
			f.LendErr = "x"
		case "setup running, list unread":
			f.ListErr = "x"
		}
		st := Build(f)
		if got := st.Menu.Attention; got != c.want {
			t.Errorf("%s: attention %t, want %t", name, got, c.want)
		}
		if c.want && name == "lending" && !strings.Contains(st.Menu.Note, "the group this Mac lent to is gone") {
			t.Errorf("%s: note %q", name, st.Menu.Note)
		}
	}
}
