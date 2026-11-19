package hostsetup_test

import (
	"errors"
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
