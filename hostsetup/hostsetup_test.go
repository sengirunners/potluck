package hostsetup_test

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
	st := hostsetup.Start(home, "potluck host '<invite code>'", code)
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
	st := hostsetup.Start(home, "potluck host trial")
	for _, s := range hostsetup.Order {
		st.Begin(s, "")
	}
	if b := st.End(nil); b != nil {
		t.Fatalf("blocked %+v", b)
	}
	if r, _ := hostsetup.Read(home); !r.Succeeded() || len(r.Stages) != len(hostsetup.Order) {
		t.Fatalf("run: %+v", r)
	}
	st = hostsetup.Start(home, "potluck host trial")
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

// A flag this machine never accepts: the blocked stage names it, and the
// command that runs the setup again leaves it out, or, when an earlier share
// kept it, turns it off. Running the same command again would fail the same
// way.
func TestTrackerEndOptionError(t *testing.T) {
	given := map[string]string{"docker": " --docker=true"}
	refused := &hostsetup.OptionError{Flag: "docker", Off: "--docker=false", Err: errors.New("--docker is for the oci layer")}
	for _, tc := range []struct {
		name, again, wantAgain, wantFix string
		failure                         error
	}{
		{"given", "zm potluck host pool --docker=true --linux-slots 8", "zm potluck host pool --linux-slots 8", "without --docker=true", refused},
		{"kept from an earlier share", "potluck host pool --linux-slots 8", "potluck host pool --linux-slots 8 --docker=false", "with --docker=false", refused},
		{"wrapped", "potluck host pool --docker=true", "potluck host pool", "without --docker=true", fmt.Errorf("share: %w", refused)},
	} {
		home := t.TempDir()
		st := hostsetup.Start(home, tc.again)
		if tc.name != "kept from an earlier share" {
			st.SetFlagArgs(func(n string) string { return given[n] })
		}
		b := st.End(tc.failure)
		r, _ := hostsetup.Read(home)
		if b == nil || b.Flag != "--docker" || b.Fix != tc.wantFix || r.Again != tc.wantAgain || r.Blocked().Fix != tc.wantFix {
			t.Errorf("%s: stage %+v, again %q", tc.name, b, r.Again)
		}
	}
	// A flag with no way off that the command did not give: named, Again as it was.
	st := hostsetup.Start(t.TempDir(), "potluck host pool")
	if b := st.End(&hostsetup.OptionError{Flag: "root", Err: errors.New("--root is for the tart layer")}); b == nil || b.Flag != "--root" || b.Fix != "" || st.Run().Again != "potluck host pool" {
		t.Errorf("no way off: %+v %q", b, st.Run().Again)
	}
	// A stage blocked earlier for another reason keeps its word.
	st = hostsetup.Start(t.TempDir(), "potluck host pool --docker=true")
	st.SetFlagArgs(func(n string) string { return given[n] })
	st.Block(hostsetup.Preflight, "disk full")
	if b := st.End(refused); b.Flag != "" || b.Detail != "disk full" || st.Run().Again != "potluck host pool --docker=true" {
		t.Errorf("earlier block: %+v %q", b, st.Run().Again)
	}
	// Any other failure leaves both alone.
	st = hostsetup.Start(t.TempDir(), "potluck host pool --docker=true")
	st.SetFlagArgs(func(n string) string { return given[n] })
	if b := st.End(errors.New("disk full")); b.Flag != "" || st.Run().Again != "potluck host pool --docker=true" {
		t.Errorf("other failure: %+v %q", b, st.Run().Again)
	}
}
