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

// What the last run's button does now depends on the pools this machine is in
// now: an invite is taken as spent only for a pool joined at its own machine
// once the run had begun to join.
func TestRunAgainFollowsThePools(t *testing.T) {
	began := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	joining := &hostsetup.Joining{Fingerprint: "ABCDEFGHIJKLMNOP", Machine: "owner", At: began.Format(time.RFC3339)}
	at := func(slug string, when time.Duration) Pool {
		return Pool{Slug: slug, ID: "01" + slug, Joined: began.Add(when), Machine: "owner", Fingerprint: "abcdefghijklmnop"}
	}
	other := at("other", time.Second)
	other.Machine, other.Fingerprint = "elsewhere", "qqqqqqqqqqqqqqqq"
	lends := Pool{Slug: "lent", ID: "01lent", Joined: began, Lends: true}
	for name, c := range map[string]struct {
		joining *hostsetup.Joining
		pool    string // the record's own ("": the invite's)
		id      string
		pools   []Pool
		want    string // the pool the button names ("": it asks for the invite)
		els     string // the pool it goes on with when none is given ("": it refuses)
	}{
		"killed before it joined":                    {nil, "", "", nil, "", ""},
		"not tried, then another invite joined":      {nil, "", "", []Pool{at("friends", time.Minute)}, "", "friends"},
		"not tried, and two pools":                   {nil, "", "", []Pool{other, at("friends", time.Minute)}, "", ""},
		"not tried, and two pools, one to lend":      {nil, "", "", []Pool{other, lends}, "", "lent"},
		"joined, not noted":                          {joining, "", "", []Pool{at("friends", time.Second)}, "friends", ""},
		"joined, not noted, beside an older pool":    {joining, "", "", []Pool{at("old", -time.Hour), at("friends", time.Second)}, "friends", ""},
		"trying, then a pool elsewhere joined":       {joining, "", "", []Pool{other}, "", "other"},
		"trying, and a pool of this machine's own":   {joining, "", "", []Pool{{Slug: "mine", ID: "01mine"}}, "", "mine"},
		"trying, then two pools of its machine":      {joining, "", "", []Pool{at("a", time.Second), at("b", time.Minute)}, "", ""},
		"trying, only an older pool of its machine":  {joining, "", "", []Pool{at("old", -time.Hour)}, "", "old"},
		"trying, and one of its machine much later":  {joining, "", "", []Pool{at("late", joinMost+2*joinSlack)}, "", "late"},
		"still in its pool":                          {nil, "friends", "01friends", []Pool{at("friends", 0)}, "friends", ""},
		"its pool is called something else now":      {nil, "friends", "01friends", []Pool{{Slug: "work", ID: "01friends"}}, "work", ""},
		"left its pool":                              {nil, "friends", "01friends", []Pool{other}, "", ""},
		"left it, and in another of the same name":   {nil, "friends", "01friends", []Pool{{Slug: "friends", ID: "01else"}}, "", ""},
		"a record from before it held the pool's id": {nil, "friends", "", []Pool{at("friends", 0)}, "friends", ""},
	} {
		r := &hostsetup.Run{Started: began.Add(-time.Hour).Format(time.RFC3339), NeedsInvite: c.pool == "", Joining: c.joining, PoolID: c.id,
			Again: "potluck host '<invite code>' --cpu 4", AgainArgs: []string{"host", "--cpu=4"}, Stages: stages(hostsetup.Preflight)}
		if c.pool != "" {
			r.Again, r.AgainArgs = "potluck host "+c.pool+" --cpu 4", []string{"host", c.pool, "--cpu=4"}
		}
		f := Facts{Host: &protocol.HostStatus{Mode: "auto", State: "accepting"}, Share: "potluck host"}
		f.Setup.Run, f.Setup.Pools, f.Setup.PoolsRead = r, c.pools, true
		s := Build(f).Setup
		want, word := []string{"--json", "host"}, hostsetup.InviteWord
		if c.want != "" {
			want, word = append(want, c.want), c.want
		}
		want = append(want, "--cpu=4", "--yes", "--no-sudo", "--detach")
		if c.want == "" {
			want = append(want, "--instead-of-invite="+c.els)
		}
		do := check(s, "setup").Do
		if do == nil || do.Invite != (c.want == "") || !slices.Equal(slices.DeleteFunc(slices.Clone(do.Args), func(a string) bool { return a == "--" }), want) {
			t.Errorf("%s: button %+v, want %q", name, do, want)
		}
		if s.Next != "potluck host "+word+" --cpu 4" {
			t.Errorf("%s: next %q", name, s.Next)
		}
		if asks := strings.Contains(s.View.Lead, "aste "); asks != (c.want == "") {
			t.Errorf("%s: lead %q", name, s.View.Lead)
		}
		named := c.els
		if c.joining != nil && c.want != "" {
			named = c.want
		}
		if named != "" && !strings.Contains(s.View.Lead, "lend to the group "+named+".") && !strings.Contains(s.View.Lead, "joined "+named+".") {
			t.Errorf("%s: the pool is not named: %q", name, s.View.Lead)
		}
	}
}

// A refused invite on a machine that joined a pool where it is from: another
// is asked for, and with none given it goes on with that pool while it is in it.
func TestRunAgainAfterARefusedInvite(t *testing.T) {
	for _, c := range []struct {
		id   string
		in   []Pool
		want string
	}{
		{"01friends", []Pool{{Slug: "friends", ID: "01friends", Joined: time.Now()}}, "friends"},
		{"01friends", []Pool{{Slug: "work", ID: "01friends", Joined: time.Now()}}, "work"},
		{"", []Pool{{Slug: "friends", ID: "01friends", Joined: time.Now()}}, "friends"},
		{"01friends", []Pool{{Slug: "friends", ID: "01else", Joined: time.Now()}}, ""}, // left since: no other stands in for it
		{"01friends", nil, ""},
	} {
		f := Facts{Host: &protocol.HostStatus{Mode: "auto", State: "accepting"}, Share: "potluck host"}
		f.Setup.Run = &hostsetup.Run{Started: "x", Again: "potluck host '<invite code>'", AgainArgs: []string{"host", "--cpu=4"}, NeedsInvite: true, InviteRefused: true, Else: "friends", ElseID: c.id, Stages: stages(hostsetup.Preflight)}
		f.Setup.Pools, f.Setup.PoolsRead = c.in, true
		args := []string{"--json", "host", "--cpu=4", "--yes", "--no-sudo", "--detach"}
		s := Build(f).Setup
		do := check(s, "setup").Do
		if do == nil || !slices.Equal(do.Args, append(slices.Clone(args), "--instead-of-invite="+c.want, "--")) || !slices.Equal(do.InviteArgs, append(args, "--invite-stdin")) ||
			(c.want != "") != strings.Contains(s.View.Lead, "lend to the group "+c.want+".") || !s.View.InviteRefused ||
			(c.want == "") != strings.Contains(s.View.Lead, "The invite wasn't accepted") {
			t.Errorf("%+v: %+v, %q", c, do, s.View.Lead)
		}
		if f.Setup.RunLive = true; Build(f).Setup.View.InviteRefused {
			t.Error("a run going on may have been given another invite")
		}
	}
}

// A record that cannot be read: the one pool joined to lend is what it goes on
// with when no invite is given, never one it is in for another reason.
func TestUnreadableRecordGoesOnWithTheLentPool(t *testing.T) {
	lent, mine := Pool{Slug: "friends", ID: "01friends", Joined: time.Now(), Lends: true}, Pool{Slug: "mine", ID: "01mine"}
	for name, c := range map[string]struct {
		pools []Pool
		read  bool
		want  string
	}{
		"joined to lend":      {[]Pool{lent}, true, "friends"},
		"and one of its own":  {[]Pool{mine, lent}, true, "friends"},
		"only one of its own": {[]Pool{mine}, true, "mine"},
		"two of its own":      {[]Pool{mine, {Slug: "b"}}, true, ""},
		"two joined to lend":  {[]Pool{lent, {Slug: "b", Lends: true}}, true, ""},
		"not read":            {[]Pool{lent}, false, ""},
		"none":                {nil, true, ""},
	} {
		f := Facts{Host: &protocol.HostStatus{Mode: "auto", State: "accepting"}, Share: "potluck host"}
		f.Setup.RunErr, f.Setup.Pools, f.Setup.PoolsRead = "not JSON", c.pools, c.read
		do := check(Build(f).Setup, "setup").Do
		want := []string{"--json", "host", "--yes", "--no-sudo", "--detach", "--instead-of-invite=" + c.want, "--"}
		if do == nil || !slices.Equal(do.Args, want) || len(do.InviteArgs) == 0 {
			t.Errorf("%s: %+v", name, do)
		}
	}
	var lends computeapi.RunTargetView
	lends.Kind = "box"
	for name, f := range map[string]Facts{
		"what it shares could not be read": {ListErr: "no answer"},
		"the daemon is not running":        {Daemon: "not running"},
		"it lends":                         {Targets: []computeapi.RunTargetView{lends}},
	} {
		f.Host, f.Share, f.Setup.RunErr = &protocol.HostStatus{Mode: "auto", State: "accepting"}, "potluck host", "not JSON"
		if c := check(Build(f).Setup, "setup"); c.Do != nil {
			t.Errorf("%s, and it offers %+v", name, c.Do)
		}
	}
}

// With no daemon the pools are not known, so the record stands as it is.
func TestRunAgainWithPoolsUnread(t *testing.T) {
	f := Facts{Share: "potluck host"}
	f.Setup.Run = &hostsetup.Run{Started: "x", Again: "potluck host friends", AgainArgs: []string{"host", "friends"}, Stages: stages(hostsetup.Preflight)}
	if do := check(Build(f).Setup, "setup").Do; do == nil || do.Invite {
		t.Errorf("%+v", do)
	}
}

// After a restart the app starts the daemon by itself on a Mac whose setup was
// begun, however it ended, and not on one never set up.
func TestDaemonStartsItself(t *testing.T) {
	blocked := append(stages(hostsetup.Preflight), hostsetup.Stage{Name: hostsetup.Images, Result: hostsetup.Blocked})
	for name, c := range map[string]struct {
		run  *hostsetup.Run
		err  string
		want bool
	}{
		"never":      {nil, "", false},
		"blocked":    {&hostsetup.Run{Started: "x", Ended: "y", Stages: blocked}, "", true},
		"cut off":    {&hostsetup.Run{Started: "x", Stages: stages(hostsetup.Preflight)}, "", true},
		"unreadable": {nil, "not JSON", true},
	} {
		for _, auto := range []bool{true, false} {
			f := Facts{Daemon: "not running", Start: "potluck daemon --detach", Share: "potluck host"}
			f.Setup.Run, f.Setup.RunErr, f.Setup.AutoStart = c.run, c.err, auto
			if do := check(Build(f).Setup, "daemon").Do; do == nil || do.Auto != (c.want && auto) {
				t.Errorf("%s, auto start %v: %+v", name, auto, do)
			}
		}
	}
}

// A size shared only with pools this machine is in no more lends nothing: the
// window asks for an invite, and never says that this Mac lends.
func TestPoolsThatEnded(t *testing.T) {
	size := func(shared ...string) computeapi.RunTargetView {
		v := computeapi.RunTargetView{Shared: shared, Describe: &computeapi.RunTargetDescribe{Size: &computeapi.RunTargetSize{CPU: 1, MemoryMB: 2048}}}
		v.Name, v.Kind = "solo-c1m2", "box"
		return v
	}
	for name, c := range map[string]struct {
		shared []string
		pools  []Pool
		read   bool
		sizes  int
	}{
		"in its pool":                {[]string{"friends"}, []Pool{{Slug: "friends", ID: "01friends"}}, true, 1},
		"in its pool, by id":         {[]string{"01friends"}, []Pool{{Slug: "friends", ID: "01friends"}}, true, 1},
		"in one of its pools":        {[]string{"gone", "friends"}, []Pool{{Slug: "friends", ID: "01friends"}}, true, 1},
		"its pool ended":             {[]string{"friends"}, nil, true, 0},
		"its pool ended, in another": {[]string{"friends"}, []Pool{{Slug: "mine", ID: "01mine"}}, true, 0},
		"pools not read":             {[]string{"friends"}, nil, false, 1},
		"a pool it runs":             {nil, []Pool{{Slug: "mine", ID: "01mine"}}, true, 1},
	} {
		f := Facts{Host: &protocol.HostStatus{Mode: "auto", State: "accepting"}, Share: "potluck host", Targets: []computeapi.RunTargetView{size(c.shared...)}}
		f.Setup.Run = &hostsetup.Run{Started: "x", Ended: "y", Again: "potluck host friends", AgainArgs: []string{"host", "friends"}, PoolID: "01friends", Stages: stages(hostsetup.Order...)}
		f.Setup.Pools, f.Setup.PoolsRead = c.pools, c.read
		st := Build(f)
		v := st.Setup.View
		if st.Lending.Sizes != c.sizes || (v.Heading == "This Mac isn't lending") != (c.sizes == 0) || c.sizes == 0 && (v.Do == nil || len(v.Do.InviteArgs) == 0 || !strings.Contains(v.Lead, "no longer in its group") || st.Setup.Next != "potluck host '<invite code>'") || st.Menu.Attention != (c.sizes == 0) {
			t.Errorf("%s: %d sizes, %q %q %+v, next %q, attention %t", name, st.Lending.Sizes, v.Heading, v.Lead, v.Do, st.Setup.Next, st.Menu.Attention)
		}
	}
}

// A job still running on a size whose pool ended is counted, and ending it
// is asked about.
func TestJobOnASizeWhosePoolEnded(t *testing.T) {
	size := computeapi.RunTargetView{Shared: []string{"friends"}, Live: []string{"a"},
		Describe: &computeapi.RunTargetDescribe{Size: &computeapi.RunTargetSize{CPU: 4, MemoryMB: 1024}}}
	size.Name, size.Kind = "c4", "box"
	f := Facts{Host: &protocol.HostStatus{Mode: "auto", State: "accepting"}, Share: "potluck host", Targets: []computeapi.RunTargetView{size}}
	f.Setup.PoolsRead = true
	st := Build(f)
	if l := st.Lending; l.Sizes != 0 || l.Lent != 0 || l.Jobs != 1 || l.InUse != 4 || l.MemoryInUse != 1024 {
		t.Errorf("%+v", l)
	}
	if strings.Contains(st.Menu.Note, "until they are under it") {
		t.Errorf("no job starts after it either: %s", st.Menu.Note)
	}
	if !strings.Contains(st.Menu.Items[2].Confirm, "1 running job") {
		t.Errorf("%+v", st.Menu.Items)
	}
}

// With no record to go by, a press with no invite goes on with the pool the
// words name, or refuses.
func TestSetupWithNoRecordNamesThePool(t *testing.T) {
	mine, work := Pool{Slug: "mine", ID: "01mine"}, Pool{Slug: "work", ID: "01work", Joined: time.Now()}
	for name, run := range map[string]*hostsetup.Run{"no record": nil,
		"one from before again_args": {Started: "x", Ended: "y", Stages: []hostsetup.Stage{{Name: hostsetup.Preflight, Result: hostsetup.Blocked, Detail: "x"}}}} {
		for els, pools := range map[string][]Pool{"work": {work}, "mine": {mine}, "": {mine, work}} {
			f := Facts{Host: &protocol.HostStatus{Mode: "auto", State: "accepting"}, Share: "potluck host"}
			f.Setup.Run, f.Setup.Pools, f.Setup.PoolsRead = run, pools, true
			v := Build(f).Setup.View
			if v.Do == nil || !slices.Contains(v.Do.Args, "--"+InsteadFlag+"="+els) || len(v.Do.InviteArgs) == 0 ||
				(els != "") != strings.Contains(strings.ToLower(v.Lead), "leave the invite field empty to lend to the group "+els+".") || slices.ContainsFunc(v.Do.InviteArgs, func(a string) bool { return strings.Contains(a, InsteadFlag) }) {
				t.Errorf("%s, %q: %q %+v", name, els, v.Lead, v.Do)
			}
		}
	}
}

// A record that cannot be read never hides a setup that goes on.
func TestUnreadableRecordOfARunGoingOn(t *testing.T) {
	f := Facts{Host: &protocol.HostStatus{Mode: "auto", State: "accepting"}, Share: "potluck host"}
	f.Setup.RunErr, f.Setup.RunLive = "not JSON", true
	s := Build(f).Setup
	if c := check(s, "setup"); c.State != Running || c.Do != nil || s.View.Heading != "Setting up this Mac" || s.View.Do != nil {
		t.Errorf("%+v, %q %+v", c, s.View.Heading, s.View.Do)
	}
}

// A run that ended has joined nothing since: a pool joined later, at the same
// machine, is another invite's.
func TestJoinedAfterTheRunEnded(t *testing.T) {
	began := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	for after, want := range map[time.Duration]bool{0: true, joinSlack: true, 2 * joinSlack: false} {
		r := &hostsetup.Run{NeedsInvite: true, Joining: &hostsetup.Joining{Fingerprint: "a", At: began.Format(time.RFC3339)}, Ended: began.Format(time.RFC3339)}
		if got := joinedBy(r, []Pool{{Slug: "friends", Fingerprint: "a", Joined: began.Add(after)}}) != nil; got != want {
			t.Errorf("%s after it ended: %v", after, got)
		}
	}
}
