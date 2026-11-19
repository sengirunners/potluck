package menubar

import (
	"slices"
	"testing"
	"time"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/protocol"
)

func box(name, stateDir, os string, cpu, slots int, live, stopped []string) computeapi.RunTargetView {
	v := computeapi.RunTargetView{RunTarget: computeapi.RunTarget{Name: name, Kind: "box", Config: map[string]string{}}, Live: live, Stopped: stopped}
	if stateDir != "" {
		v.Config["state_dir"] = stateDir
	}
	v.Describe = &computeapi.RunTargetDescribe{Size: &computeapi.RunTargetSize{Name: name, OS: os, CPU: cpu}}
	if slots > 0 {
		v.Describe.Slots = &computeapi.RunTargetSlots{OS: os, Total: slots}
	}
	return v
}

func TestLend(t *testing.T) {
	// Two sizes of one state directory share its two Linux slots: the
	// larger size twice is the most they use at once.
	got := Lend([]computeapi.RunTargetView{
		box("c2", "", "linux", 2, 2, []string{"a"}, nil),
		box("c3", "", "linux", 3, 2, []string{"b", "c"}, []string{"c"}),
	})
	if got.Lent != 6 || got.InUse != 5 || got.Jobs != 2 || len(got.Unknown) != 0 {
		t.Errorf("shared slots: %+v", got)
	}
	// macOS slots and another state directory are pools of their own; a
	// remote binding, a run binding and a driver with no answer add nothing.
	failed := box("down", "", "linux", 4, 2, []string{"x"}, nil)
	failed.Error = "driver did not answer"
	remote := box("far", "", "linux", 8, 2, []string{"y"}, nil)
	remote.RunTarget.Host = "other"
	run := box("ci", "", "linux", 8, 2, nil, nil)
	run.Kind = "run"
	got = Lend([]computeapi.RunTargetView{
		box("c2", "", "linux", 2, 2, nil, nil),
		box("mac", "", "macos", 4, 1, []string{"m"}, nil),
		box("oci", "/var/lib/p", "linux", 1, 0, nil, nil), // no slots: one
		failed, remote, run,
	})
	if got.Lent != 4+4+1 || got.InUse != 4 || got.Jobs != 1 || !slices.Equal(got.Unknown, []string{"down"}) {
		t.Errorf("pools: %+v", got)
	}
}

func TestBuild(t *testing.T) {
	loc := time.FixedZone("here", -8*3600)
	now = func() time.Time { return time.Date(2026, 11, 14, 12, 0, 0, 0, loc) }
	t.Cleanup(func() { now = time.Now })
	lending := []computeapi.RunTargetView{box("c3", "", "linux", 3, 2, []string{"a", "b"}, nil)}
	host := func(mode, state string) *protocol.HostStatus { return &protocol.HostStatus{Mode: mode, State: state} }
	refusing := []computeapi.RunTargetView{box("c3", "", "linux", 3, 2, []string{"a", "b"}, nil), box("c1", "/var/lib/p", "linux", 1, 1, nil, nil), box("c2", "/var/lib/q", "linux", 0, 0, nil, nil)}
	refusing[1].Describe.Host = &protocol.HostStatus{Mode: "auto", State: "accepting",
		Refused: &protocol.HostRefusal{Reason: "no room on this host now: CPU busy", At: "2026-11-14T20:00:00Z", Until: "2026-11-14T20:10:00Z"}}
	refusing[2].Describe.Host = &protocol.HostStatus{Mode: "auto", State: "accepting",
		Refused: &protocol.HostRefusal{Reason: "no room on this host now: an older answer", At: "2026-11-14T19:00:00Z", Until: "2026-11-14T20:30:00Z"}}
	checked := func(s Status) string {
		for _, it := range s.Menu.Items {
			if it.Checked {
				return it.ID
			}
		}
		return ""
	}
	for _, c := range []struct {
		name          string
		f             Facts
		title, detail string
		note          string
		checked       string
		cores         string
	}{
		{"the site's picture", Facts{Cores: 12, Targets: lending, Host: host("auto", "accepting")},
			"Lending 6 of 12 cores", "6 cores in use by 2 jobs", "", "keep", "●●●●●●······"},
		{"open", Facts{Cores: 12, Targets: lending, Host: &protocol.HostStatus{Mode: "open", State: "accepting", Until: "2026-11-14T22:00:00Z"}},
			"Lending 6 of 12 cores", "6 cores in use by 2 jobs", "until 14:00, then back to your rules", "keep", "●●●●●●······"},
		{"winding down", Facts{Cores: 12, Targets: lending, Host: &protocol.HostStatus{Mode: "drain", State: "draining", DrainSince: "2026-11-14T19:00:00Z", DrainDeadline: "2026-11-15T19:30:00Z"}},
			"Winding down 6 of 12 cores", "6 cores in use by 2 jobs", "jobs still running end at Nov 15 11:30", "wind", "●●●●●●······"},
		{"paused", Facts{Cores: 12, Targets: []computeapi.RunTargetView{box("c3", "", "linux", 3, 2, nil, nil)}, Host: host("closed", "closed")},
			"Paused: lending nothing", "nothing running", "", "stop", "○○○○○○······"},
		{"paused for a while, sharing in the background", Facts{Cores: 12, Targets: []computeapi.RunTargetView{box("c3", "", "linux", 3, 2, nil, nil)},
			Host: &protocol.HostStatus{Mode: "closed", State: "closed", Until: "2026-11-14T22:00:00Z"}, Background: &Background{State: "running", Last: "pulling img: 40%"}},
			"Paused: lending nothing", "nothing running", "sharing goes on in the background: pulling img: 40%; until 14:00, then back to your rules", "stop", "○○○○○○······"},
		{"a background run that ended says nothing in the menu", Facts{Cores: 12, Targets: lending, Host: host("auto", "accepting"), Background: &Background{State: "ended", Last: "done"}},
			"Lending 6 of 12 cores", "6 cores in use by 2 jobs", "", "keep", "●●●●●●······"},
		{"the owner's rules say no", Facts{Cores: 12, Targets: lending, Host: host("auto", "closed")},
			"Paused by your lending rules", "6 cores in use by 2 jobs", "", "keep", "●●●●●●······"},
		{"one job", Facts{Cores: 4, Targets: []computeapi.RunTargetView{box("c1", "", "linux", 1, 2, []string{"a"}, nil)}, Host: host("auto", "accepting")},
			"Lending 2 of 4 cores", "1 core in use by 1 job", "", "keep", "●○··"},
		{"not lending", Facts{Cores: 4, Host: host("auto", "accepting")},
			"Not lending", "nothing running", "share this Mac: potluck host", "keep", "····"},
		{"no daemon", Facts{Cores: 4, Daemon: "not running", Host: host("drain", "draining")},
			"Potluck is not running", "", "start it: potluck daemon --detach", "wind", "····"},
		{"unreadable switch", Facts{Cores: 4, Targets: lending, Host: host("unreadable", "holding"), HostErr: "bad JSON"},
			"Held: the host switch cannot be read", "6 cores in use by 2 jobs", "bad JSON", "", "●●●●"},
		{"a pool not listed", Facts{Cores: 12, Targets: lending, ListErr: "pool acme: timeout", Host: host("auto", "accepting")},
			"Lending 6 of 12 cores", "6 cores in use by 2 jobs", "not everything shared could be read: pool acme: timeout", "keep", "●●●●●●······"},
		{"nothing listed", Facts{Cores: 4, ListErr: "timeout", Host: host("auto", "accepting")},
			"Cannot read what this Mac lends", "nothing running", "not everything shared could be read: timeout", "keep", "····"},
		{"only a run binding", Facts{Cores: 4, Targets: []computeapi.RunTargetView{{RunTarget: computeapi.RunTarget{Name: "ci", Kind: "run"}}}, Host: host("auto", "accepting")},
			"Not lending", "nothing running", "share this Mac: potluck host", "keep", "····"},
		{"past the drain deadline", Facts{Cores: 12, Targets: lending, Host: &protocol.HostStatus{Mode: "drain", State: "draining", DrainSince: "2026-11-14T19:00:00Z", DrainDeadline: "2026-11-14T19:30:00Z"}},
			"Winding down 6 of 12 cores", "6 cores in use by 2 jobs", "jobs still running are being ended", "wind", "●●●●●●······"},
		{"no answer", Facts{Cores: 4, HostErr: "permission denied"},
			"Held: the host switch cannot be read", "nothing running", "permission denied", "", "····"},
		// The switch accepts, but the host's admission controller refused a
		// sandbox: any size's driver may hold it, the newest counts.
		{"refused by the admission controller", Facts{Cores: 12, Targets: refusing, Host: host("auto", "accepting")},
			"Refusing new work now", "6 cores in use by 2 jobs", "no room on this host now: CPU busy (said at 12:00, retry after 12:10)", "keep", "●●●●●●○·····"},
		{"refused, and the owner's rules say no too", Facts{Cores: 12, Targets: refusing, Host: host("auto", "closed")},
			"Paused by your lending rules", "6 cores in use by 2 jobs", "", "keep", "●●●●●●○·····"},
	} {
		c.f.Loc = loc
		c.f.Start, c.f.Share = "potluck daemon --detach", "potluck host"
		s := Build(c.f)
		m := s.Menu
		if m.Title != c.title || m.Detail != c.detail || m.Note != c.note || m.Cores != c.cores || checked(s) != c.checked {
			t.Errorf("%s: got %q / %q / %q / %q, checked %q", c.name, m.Title, m.Detail, m.Note, m.Cores, checked(s))
		}
		// Every item not in force sets its mode; the one in force runs nothing.
		want := map[string]string{"keep": protocol.HostAuto, "wind": protocol.HostDrain, "stop": protocol.HostClosed}
		for _, it := range m.Items {
			switch {
			case it.Checked && it.Args != nil:
				t.Errorf("%s: %s is in force but runs %v", c.name, it.ID, it.Args)
			case !it.Checked && !slices.Equal(it.Args, []string{"host", "set", want[it.ID]}):
				t.Errorf("%s: %s runs %v", c.name, it.ID, it.Args)
			}
		}
	}
}

func TestStopNowAsksFirstWhileJobsRun(t *testing.T) {
	running := []computeapi.RunTargetView{box("c3", "", "linux", 3, 2, []string{"a", "b"}, nil)}
	idle := []computeapi.RunTargetView{box("c3", "", "linux", 3, 2, nil, nil)}
	for _, c := range []struct {
		targets []computeapi.RunTargetView
		mode    string
		want    string
	}{
		{running, "auto", "End 2 running jobs now? Their work is lost."},
		{idle, "auto", ""},
		{running, "closed", ""}, // already stopped: nothing to run
	} {
		s := Build(Facts{Cores: 12, Targets: c.targets, Host: &protocol.HostStatus{Mode: c.mode, State: "accepting"}})
		for _, it := range s.Menu.Items {
			if want := map[bool]string{true: c.want}[it.ID == "stop"]; it.Confirm != want {
				t.Errorf("%s with %d jobs, %s: confirm %q", c.mode, s.Lending.Jobs, it.ID, it.Confirm)
			}
		}
	}
}

func TestBuildLabels(t *testing.T) {
	for mode, keep := range map[string]string{"auto": "Keep lending", "drain": "Keep lending", "closed": "Resume lending"} {
		s := Build(Facts{Cores: 2, Host: &protocol.HostStatus{Mode: mode, State: "accepting"}})
		var got [][2]string
		for _, it := range s.Menu.Items {
			got = append(got, [2]string{it.Label, it.Sub})
		}
		want := [][2]string{{keep, ""}, {"Wind down", "finish jobs, take no new ones"}, {"Stop now", "end jobs now"}}
		if !slices.Equal(got, want) {
			t.Errorf("%s: items: %v", mode, got)
		}
	}
}

// A paused machine (host pause: the switch closed) says so, its way back
// is Resume lending (the switch back to auto), and the status carries a
// background host run.
func TestBuildPaused(t *testing.T) {
	bg := &Background{State: "running", Last: "conformance on solo-c1m2"}
	s := Build(Facts{Cores: 4, Targets: []computeapi.RunTargetView{box("c1", "", "linux", 1, 2, nil, nil)}, Host: &protocol.HostStatus{Mode: "closed", State: "closed"}, Background: bg})
	if s.Mode != "closed" || s.Menu.Title != "Paused: lending nothing" || s.Background != bg {
		t.Fatalf("paused: %+v", s)
	}
	if it := s.Menu.Items[0]; it.Label != "Resume lending" || it.Checked || !slices.Equal(it.Args, []string{"host", "set", "auto"}) {
		t.Errorf("resume item: %+v", it)
	}
}

// Status carries the newest refusal for the JSON reader (the menu bar app).
func TestBuildCarriesTheNewestRefusal(t *testing.T) {
	now = func() time.Time { return time.Date(2026, 11, 14, 20, 5, 0, 0, time.UTC) }
	t.Cleanup(func() { now = time.Now })
	older := box("c1", "/a", "linux", 1, 1, nil, nil)
	newer := box("c2", "/b", "linux", 2, 1, nil, nil)
	older.Describe.Host = &protocol.HostStatus{Mode: "auto", State: "accepting", Refused: &protocol.HostRefusal{Reason: "old", At: "2026-11-14T19:00:00Z", Until: "2026-11-14T21:00:00Z"}}
	newer.Describe.Host = &protocol.HostStatus{Mode: "auto", State: "accepting", Refused: &protocol.HostRefusal{Reason: "new", At: "2026-11-14T20:00:00Z", Until: "2026-11-14T20:10:00Z"}}
	s := Build(Facts{Cores: 4, Targets: []computeapi.RunTargetView{older, newer}, Host: &protocol.HostStatus{Mode: "auto", State: "accepting"}})
	if s.Refused == nil || s.Refused.Reason != "new" {
		t.Fatalf("refused: %+v", s.Refused)
	}
	if s := Build(Facts{Cores: 4, Targets: []computeapi.RunTargetView{box("c1", "", "linux", 1, 1, nil, nil)}, Host: &protocol.HostStatus{Mode: "auto", State: "accepting"}}); s.Refused != nil {
		t.Fatalf("no refusal: %+v", s.Refused)
	}
	// A cached describe past its refusal's Until: the refusal no longer stands.
	now = func() time.Time { return time.Date(2026, 11, 14, 20, 10, 0, 0, time.UTC) }
	s = Build(Facts{Cores: 4, Targets: []computeapi.RunTargetView{older, newer}, Host: &protocol.HostStatus{Mode: "auto", State: "accepting"}})
	if s.Refused == nil || s.Refused.Reason != "old" {
		t.Fatalf("past the newer one's until: %+v", s.Refused)
	}
	now = func() time.Time { return time.Date(2026, 11, 14, 21, 0, 0, 0, time.UTC) }
	if s := Build(Facts{Cores: 4, Targets: []computeapi.RunTargetView{older, newer}, Host: &protocol.HostStatus{Mode: "auto", State: "accepting"}}); s.Refused != nil || s.Menu.Title != "Lending 3 of 4 cores" {
		t.Fatalf("past every until: %+v %q", s.Refused, s.Menu.Title)
	}
}
