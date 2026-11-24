package hostsetup_test

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sengirunners/potluck/hostsetup"
)

// A run's stages are written as they change: reaching a stage ends the one
// before as succeeded, a failure blocks the stage running, and the invite
// code never reaches the record or a line.
func TestTrackerRecordsStagesAndNeverTheInvite(t *testing.T) {
	home := t.TempDir()
	const code = "pk1-secretcode"
	st := hostsetup.Start(home, cmd("", " --no-sudo=true"), code)
	if r, err := hostsetup.Read(home); err != nil || r == nil || len(r.Stages) != 0 || r.Ended != "" {
		t.Fatalf("a run just started: %+v %v", r, err)
	}
	if got := st.Begin(hostsetup.Preflight, "checking"); got != "[1/5] preflight: checking" {
		t.Errorf("line %q", got)
	}
	st.Begin(hostsetup.Images, "image 1 of 2: pull a")
	st.Begin(hostsetup.Images, "image 2 of 2: reuse b")
	r, _ := hostsetup.Read(home)
	if len(r.Stages) != 2 || r.Stages[0].Result != hostsetup.Succeeded || r.Stages[1].Detail != "image 2 of 2: reuse b" || r.Current().Name != hostsetup.Images {
		t.Fatalf("stages: %+v", r.Stages)
	}
	st.Succeed(hostsetup.Images, "2 of 2 ready")
	st.Begin(hostsetup.Validation, "size 1 of 1")
	st.Succeed(hostsetup.Validation, "passed: h-c1m2")
	st.Begin(hostsetup.Join, "joining")
	b := st.End(errors.New("the invite " + code + " was spent"))
	if b == nil || b.Name != hostsetup.Join || b.Result != hostsetup.Blocked {
		t.Fatalf("blocked %+v", b)
	}
	raw, err := os.ReadFile(filepath.Join(home, hostsetup.FileName))
	if err != nil || strings.Contains(string(raw), code) {
		t.Fatalf("the record holds the invite code (%v):\n%s", err, raw)
	}
	r, _ = hostsetup.Read(home)
	if r.Ended == "" || r.Blocked() == nil || r.Blocked().Detail != "the invite <invite code> was spent" || r.Succeeded() {
		t.Fatalf("ended run: %+v", r)
	}
	if fi, err := os.Stat(filepath.Join(home, hostsetup.FileName)); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("record mode: %v %v", fi, err)
	}
}

// A run that went through every stage succeeded; one that failed before
// any stage ran is blocked at preflight.
func TestTrackerEnd(t *testing.T) {
	home := t.TempDir()
	st := hostsetup.Start(home, cmd("trial"))
	for _, s := range hostsetup.Order {
		st.Begin(s, "")
	}
	if b := st.End(nil); b != nil {
		t.Fatalf("blocked %+v", b)
	}
	if r, _ := hostsetup.Read(home); !r.Succeeded() || len(r.Stages) != len(hostsetup.Order) {
		t.Fatalf("run: %+v", r)
	}
	st = hostsetup.Start(home, cmd("trial"))
	if b := st.End(errors.New("no layer")); b == nil || b.Name != hostsetup.Preflight || b.Detail != "no layer" {
		t.Fatalf("blocked %+v", b)
	}
	// No home records nothing, but still answers lines.
	var none *hostsetup.Tracker
	if got := none.Block(hostsetup.Capacity, "x"); got != "[5/5] usable capacity: blocked: x" || none.End(errors.New("y")) != nil {
		t.Errorf("nil tracker: %q", got)
	}
	if r, err := hostsetup.Read(t.TempDir()); r != nil || err != nil {
		t.Errorf("no record: %+v %v", r, err)
	}
}

// cmd is "potluck host" for pool ("" for an invite) with flags, each given
// as it stands in Again (" --docker=true", " --linux-slots 8").
func cmd(pool string, flags ...string) hostsetup.Command {
	c := hostsetup.Command{Prefix: "potluck host", Path: []string{"host"}, Pool: pool}
	for _, f := range flags {
		name, val, ok := strings.Cut(strings.TrimPrefix(f, " --"), "=")
		if !ok {
			name, val, _ = strings.Cut(strings.TrimPrefix(f, " --"), " ")
		}
		c.Flags = append(c.Flags, hostsetup.Flag{Name: name, Text: f, Args: []string{"--" + name + "=" + val}})
	}
	return c
}

// The command that runs a setup again, as a program's arguments: an invite
// run's need the invite appended until it joined, then name the pool; the
// invite code is in neither form, nor anywhere in the file, even when a flag
// carries it.
func TestTrackerAgainArgsNeverHoldTheInvite(t *testing.T) {
	home := t.TempDir()
	const code = "potluck-owner:ABCDEFGHIJKLMNOP:SECR-ETXX"
	c := cmd("", " --no-sudo=true", " --linux-slots 8")
	c.Flags = append(c.Flags, hostsetup.Flag{Name: "note", Text: " --note " + code, Args: []string{"--note=" + code}})
	st := hostsetup.Start(home, c, code)
	r, _ := hostsetup.Read(home)
	if want := []string{"host", "--no-sudo=true", "--linux-slots=8", "--note=<invite code>"}; !slices.Equal(r.AgainArgs, want) || !r.NeedsInvite ||
		r.Again != "potluck host '<invite code>' --no-sudo=true --linux-slots 8 --note <invite code>" {
		t.Fatalf("before the join: %q %v %q", r.AgainArgs, r.NeedsInvite, r.Again)
	}
	st.Begin(hostsetup.Join, "joining")
	st.Joined("friends")
	r, _ = hostsetup.Read(home)
	if want := []string{"host", "friends", "--no-sudo=true", "--linux-slots=8", "--note=<invite code>"}; !slices.Equal(r.AgainArgs, want) || r.NeedsInvite ||
		r.Again != "potluck host friends --no-sudo=true --linux-slots 8 --note <invite code>" {
		t.Fatalf("after the join: %q %v %q", r.AgainArgs, r.NeedsInvite, r.Again)
	}
	st.End(errors.New("no room for " + code))
	raw, err := os.ReadFile(filepath.Join(home, hostsetup.FileName))
	if err != nil || strings.Contains(string(raw), code) || strings.Contains(string(raw), "SECR-ETXX") {
		t.Fatalf("the record holds the invite code (%v):\n%s", err, raw)
	}
}

// A page the running stage waits on (a join's tailnet login) is on the
// record while it waits, and gone once the stage changes; one for a stage
// that is not running is not written.
func TestTrackerOpenURL(t *testing.T) {
	const url = "https://login.tailscale.com/a/abc"
	openOf := func(home string) []string {
		r, _ := hostsetup.Read(home)
		var out []string
		for _, s := range r.Stages {
			out = append(out, s.OpenURL)
		}
		return out
	}
	for _, tc := range []struct {
		name string
		end  func(st *hostsetup.Tracker)
	}{
		{"succeeded", func(st *hostsetup.Tracker) { st.Succeed(hostsetup.Join, "joined pool friends") }},
		{"blocked", func(st *hostsetup.Tracker) { st.End(errors.New("no tailnet login within 10 minutes")) }},
		{"next stage", func(st *hostsetup.Tracker) { st.Begin(hostsetup.Capacity, "sharing") }},
		{"cleared", func(st *hostsetup.Tracker) { st.Open(hostsetup.Join, "") }},
	} {
		home := t.TempDir()
		st := hostsetup.Start(home, cmd(""))
		st.Begin(hostsetup.Validation, "size 1 of 1")
		st.Open(hostsetup.Join, url) // not the stage running
		if got := openOf(home); !slices.Equal(got, []string{""}) {
			t.Fatalf("%s: a stage not running got a page: %q", tc.name, got)
		}
		st.Begin(hostsetup.Join, "joining the pool of your invite")
		st.Open(hostsetup.Join, url)
		if got := openOf(home); !slices.Equal(got, []string{"", url}) {
			t.Fatalf("%s: waiting: %q", tc.name, got)
		}
		tc.end(st)
		for _, u := range openOf(home) {
			if u != "" {
				t.Errorf("%s: page still on the record: %q", tc.name, openOf(home))
			}
		}
	}
}

// A flag this machine never accepts: the blocked stage names it, and the
// command that runs the setup again leaves it out, or, when an earlier share
// kept it, turns it off, in both its forms. Running the same command again
// would fail the same way.
func TestTrackerEndOptionError(t *testing.T) {
	refused := &hostsetup.OptionError{Flag: "docker", Off: "--docker=false", Err: errors.New("--docker is for the oci layer")}
	zm := cmd("pool", " --docker=true", " --linux-slots 8")
	zm.Prefix, zm.Path = "zm potluck host", []string{"potluck", "host"}
	for _, tc := range []struct {
		name               string
		c                  hostsetup.Command
		wantAgain, wantFix string
		wantArgs           []string
		failure            error
	}{
		{"given", zm, "zm potluck host pool --linux-slots 8", "without --docker=true", []string{"potluck", "host", "pool", "--linux-slots=8"}, refused},
		{"kept from an earlier share", cmd("pool", " --linux-slots 8"), "potluck host pool --linux-slots 8 --docker=false", "with --docker=false", []string{"host", "pool", "--linux-slots=8", "--docker=false"}, refused},
		{"wrapped", cmd("pool", " --docker=true"), "potluck host pool", "without --docker=true", []string{"host", "pool"}, fmt.Errorf("share: %w", refused)},
	} {
		home := t.TempDir()
		st := hostsetup.Start(home, tc.c)
		b := st.End(tc.failure)
		r, _ := hostsetup.Read(home)
		if b == nil || b.Flag != "--docker" || b.Fix != tc.wantFix || r.Again != tc.wantAgain || r.Blocked().Fix != tc.wantFix || !slices.Equal(r.AgainArgs, tc.wantArgs) {
			t.Errorf("%s: stage %+v, again %q %q", tc.name, b, r.Again, r.AgainArgs)
		}
	}
	// A flag with no way off that the command did not give: named, Again as it was.
	st := hostsetup.Start(t.TempDir(), cmd("pool"))
	if b := st.End(&hostsetup.OptionError{Flag: "root", Err: errors.New("--root is for the tart layer")}); b == nil || b.Flag != "--root" || b.Fix != "" || st.Run().Again != "potluck host pool" {
		t.Errorf("no way off: %+v %q", b, st.Run().Again)
	}
	// A stage blocked earlier for another reason keeps its word.
	st = hostsetup.Start(t.TempDir(), cmd("pool", " --docker=true"))
	st.Block(hostsetup.Preflight, "disk full")
	if b := st.End(refused); b.Flag != "" || b.Detail != "disk full" || st.Run().Again != "potluck host pool --docker=true" {
		t.Errorf("earlier block: %+v %q", b, st.Run().Again)
	}
	// Any other failure leaves both alone.
	st = hostsetup.Start(t.TempDir(), cmd("pool", " --docker=true"))
	if b := st.End(errors.New("disk full")); b.Flag != "" || st.Run().Again != "potluck host pool --docker=true" || !slices.Equal(st.Run().AgainArgs, []string{"host", "pool", "--docker=true"}) {
		t.Errorf("other failure: %+v %q", b, st.Run().Again)
	}
}
