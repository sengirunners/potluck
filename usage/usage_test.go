package usage_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/computeclient"
	"github.com/sengirunners/potluck/conformance/refdriver"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/server"
	"github.com/sengirunners/potluck/runtarget"
	"github.com/sengirunners/potluck/usage"
	"github.com/sengirunners/potluck/wiring"
)

// TestMain lets the reference driver run as this test binary.
func TestMain(m *testing.M) {
	if code, ok := refdriver.Main(); ok {
		os.Exit(code)
	}
	os.Exit(m.Run())
}

func amount(f float64) *computeapi.RunTargetSpend {
	return &computeapi.RunTargetSpend{Unit: "slot_min", Amount: f}
}

// TestCompute: the clock runs from acquire's answer to the recorded end; the
// driver's minutes lower the bill, never raise it; reclaimed and lost boxes
// are not billed; a box that never answered acquire bills nothing.
func TestCompute(t *testing.T) {
	const at, end5 = "2026-11-06T10:00:00Z", "2026-11-06T10:05:00Z"
	for _, tc := range []struct {
		name   string
		h      computeapi.RunTargetHandle
		min    int
		billed bool
	}{
		{"released, driver agrees", computeapi.RunTargetHandle{State: "released", Acquired: at, Ended: end5, Spend: amount(5)}, 5, true},
		{"driver lower", computeapi.RunTargetHandle{State: "released", Acquired: at, Ended: end5, Spend: amount(2.1)}, 3, true},
		{"driver higher is capped by the lease", computeapi.RunTargetHandle{State: "expired", Acquired: at, Ended: end5, Spend: amount(500)}, 5, true},
		{"part of a minute rounds up", computeapi.RunTargetHandle{State: "released", Acquired: at, Ended: "2026-11-06T10:00:01Z"}, 1, true},
		{"unit says nothing about time", computeapi.RunTargetHandle{State: "released", Acquired: at, Ended: end5, Spend: &computeapi.RunTargetSpend{Unit: "cpu_s", Amount: 1}}, 5, true},
		{"seconds", computeapi.RunTargetHandle{State: "released", Acquired: at, Ended: end5, Spend: &computeapi.RunTargetSpend{Unit: "s", Amount: 61}}, 2, true},
		{"reclaimed", computeapi.RunTargetHandle{State: "reclaimed", Acquired: at, Ended: end5, Spend: amount(5)}, 5, false},
		{"lost", computeapi.RunTargetHandle{State: "lost", Acquired: at, Ended: end5}, 5, false},
		{"never acquired", computeapi.RunTargetHandle{State: "lost", Ended: end5}, 0, false},
		{"negative driver figure", computeapi.RunTargetHandle{State: "released", Acquired: at, Ended: end5, Spend: amount(-3)}, 0, false},
		{"a zero driver figure bills nothing", computeapi.RunTargetHandle{State: "released", Acquired: at, Ended: end5, Spend: amount(0)}, 0, false},
		{"noticed late: never past expires_at", computeapi.RunTargetHandle{State: "expired", Acquired: at, ExpiresAt: "2026-11-06T10:02:00Z", Ended: end5}, 2, true},
		{"end before start (skew)", computeapi.RunTargetHandle{State: "released", Acquired: end5, Ended: at}, 0, false},
	} {
		u := usage.Compute(tc.h)
		if u.BillableMinutes != tc.min || u.Billed != tc.billed {
			t.Errorf("%s: %d minutes billed=%v, want %d %v", tc.name, u.BillableMinutes, u.Billed, tc.min, tc.billed)
		}
	}
}

// pool is a daemon with the reference driver bound as box target
// "sandbox" in project acme, areas team1 and team2.
type pool struct {
	owner *client.Client
	led   *usage.Service
	rts   *runtarget.Service
	pid   string
	// member is a token client for as in area with role.
	member func(as, area, role string) *client.Client
}

func newPool(t *testing.T) pool {
	t.Helper()
	home, err := os.MkdirTemp("", "zm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	t.Setenv("HOME", home)
	cfg, err := config.LoadFrom(home)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Human, cfg.Machine = "judy", "mini"
	dm, err := server.New(context.Background(), cfg, nil, wiring.Compute)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { dm.Close() })
	fix, _ := filepath.EvalSymlinks(t.TempDir())
	repo, err := refdriver.NewRepo(fix, "#!/bin/sh\nexit 0\n")
	if err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(fix, "state")
	drv, err := refdriver.Install(fix, state, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { refdriver.KillAll(state) })
	var led *usage.Service
	var rts *runtarget.Service
	for _, m := range dm.Modules() {
		switch s := m.(type) {
		case *runtarget.Service:
			rts = s
			s.SetLookPath(func(string) (string, error) { return drv, nil })
		case *usage.Service:
			led = s
		}
	}
	if led == nil {
		t.Fatal("the usage module is not wired")
	}
	ctx := context.Background()
	owner := client.WithHandler(dm.SocketHandler())
	owner.Project, owner.ProjectFrom = "acme", "flag"
	p, err := owner.CreateProject(ctx, api.NewProjectRequest{Slug: "acme", RepoRules: api.RepoRules{Remotes: []string{repo.Remote}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range []string{"team1", "team2"} {
		if _, err := owner.AddArea(ctx, "acme", api.Area{Name: a}); err != nil {
			t.Fatal(err)
		}
	}
	member := func(as, area, role string) *client.Client {
		tok, err := owner.MintToken(ctx, api.MintTokenRequest{As: as, Project: "acme", Area: area, Role: role})
		if err != nil {
			t.Fatal(err)
		}
		c := client.WithHandler(dm.Handler)
		c.Token, c.Project, c.ProjectFrom = tok.Secret, "acme/"+area, "flag"
		return c
	}
	box := computeapi.RunTarget{Name: "sandbox", Driver: "ref", Kind: "box", Isolation: "ephemeral",
		Images: []string{"ref-image@sha256:0000000000000000000000000000000000000000000000000000000000000001"},
		Config: map[string]string{"remote": repo.Remote}}
	if _, err := computeclient.BindRunTarget(ctx, owner, computeapi.BindRunTargetRequest{Target: box}); err != nil {
		t.Fatal(err)
	}
	return pool{owner: owner, led: led, rts: rts, pid: p.ID, member: member}
}

// TestLedger: a released box in one team's area leaves exactly one row
// (a repeated end report writes nothing), its own area reads it, another
// team's area does not, and a run handle leaves none.
func TestLedger(t *testing.T) {
	pl := newPool(t)
	ctx, owner, led, pid := context.Background(), pl.owner, pl.led, pl.pid
	ana, bo, gus := pl.member("ana", "team1", "member"), pl.member("bo", "team2", "member"), pl.member("gus", "team1", "guest")
	h, err := computeclient.AcquireRunTarget(ctx, ana, "sandbox", computeapi.AcquireRunTargetRequest{RequestID: "u1", TTLS: 120})
	if err != nil {
		t.Fatal(err)
	}
	id := h.Handle[strings.LastIndex(h.Handle, "/")+1:]
	if _, err := computeclient.ReleaseRunTarget(ctx, ana, "sandbox", id); err != nil {
		t.Fatal(err)
	}
	var got computeapi.UsageList
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		if got, err = computeclient.Usage(ctx, ana, ""); err != nil {
			t.Fatal(err)
		}
		if len(got.Boxes) > 0 || time.Now().After(deadline) {
			break
		}
	}
	if len(got.Boxes) != 1 {
		t.Fatalf("ana's usage: %+v", got)
	}
	u := got.Boxes[0]
	if u.Handle != h.Handle || u.Area != "team1" || u.Actor == "" || u.EndState != "released" || !u.Billed ||
		u.BillableMinutes != 1 || u.Size != "micro" || u.Acquired == "" || got.BilledMinutes != 1 {
		t.Fatalf("row: %+v (list %+v)", u, got)
	}
	// A row lost (a failed write, a stop inside the retries) comes back from
	// run targets' record of ended handles; a second reconcile writes nothing.
	if err := led.ForgetForTest(ctx, pid, h.Handle); err != nil {
		t.Fatal(err)
	}
	if got, err := computeclient.Usage(ctx, ana, ""); err != nil || len(got.Boxes) != 0 {
		t.Fatalf("after forgetting: %+v %v", got, err)
	}
	if n, err := led.Reconcile(ctx); err != nil || n != 1 {
		t.Fatalf("reconcile wrote %d, %v; want 1", n, err)
	}
	if got, err := computeclient.Usage(ctx, ana, ""); err != nil || len(got.Boxes) != 1 || got.Boxes[0].Handle != h.Handle || !got.Boxes[0].Billed {
		t.Fatalf("after reconcile: %+v %v", got, err)
	}
	if n, err := led.Reconcile(ctx); err != nil || n != 0 {
		t.Fatalf("a second reconcile wrote %d, %v; want 0", n, err)
	}
	// Start's first pass reads the whole record; Close stops it and waits.
	if err := led.ForgetForTest(ctx, pid, h.Handle); err != nil {
		t.Fatal(err)
	}
	sctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if err := led.Start(sctx); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		if got, err := computeclient.Usage(ctx, ana, ""); err == nil && len(got.Boxes) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Start's first pass did not bring the row back")
		}
	}
	closed := make(chan error, 1)
	go func() { closed <- led.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return")
	}
	// The end report again (the hook is once per handle, but a row must survive a repeat).
	final, err := computeclient.RunTargetStatus(ctx, owner, "sandbox", id)
	if err != nil {
		t.Fatal(err)
	}
	led.Record(ctx, pid, final)
	if got, err := computeclient.Usage(ctx, owner, ""); err != nil || len(got.Boxes) != 1 {
		t.Fatalf("after a repeated report: %+v %v", got, err)
	}
	if got, err := computeclient.Usage(ctx, gus, ""); err != nil || len(got.Boxes) != 1 {
		t.Errorf("a guest of the box's area: %+v %v", got, err)
	}
	if got, err := computeclient.Usage(ctx, bo, ""); err != nil || len(got.Boxes) != 0 || got.BilledMinutes != 0 {
		t.Errorf("another team's area sees: %+v %v", got, err)
	}
	if got, err := computeclient.Usage(ctx, ana, time.Now().Add(time.Hour).UTC().Format(time.RFC3339)); err != nil || len(got.Boxes) != 0 {
		t.Errorf("since the future: %+v %v", got, err)
	}
	if _, err := computeclient.Usage(ctx, ana, "yesterday"); !api.IsCode(err, api.CodeBadRequest) {
		t.Errorf("a bad since: %v", err)
	}
	// Project-level rows (area "") and sub-second order: the owner reads every
	// area; a team member never the project's own rows; ended orders by time,
	// not by text (10:00:00.5Z is after 10:00:00Z), and since is inclusive.
	ended := func(id, area, ended string) computeapi.RunTargetHandle {
		return computeapi.RunTargetHandle{Handle: "box:sandbox/" + id, Kind: "box", Target: "sandbox", Area: area, Actor: "human:judy",
			State: "released", Acquired: "2026-11-06T09:00:00Z", Ended: ended}
	}
	led.Record(ctx, pid, ended("p1", "", "2026-11-06T10:00:00.5Z"))
	led.Record(ctx, pid, ended("p2", "", "2026-11-06T10:00:00Z"))
	all, err := computeclient.Usage(ctx, owner, "2026-11-06T10:00:00Z") // b1 ended now, so it leads
	if err != nil || len(all.Boxes) != 3 || all.Boxes[1].Handle != "box:sandbox/p1" || all.Boxes[2].Handle != "box:sandbox/p2" {
		t.Fatalf("owner since 10:00:00Z: %+v %v", all, err)
	}
	if got, err := computeclient.Usage(ctx, owner, "2026-11-06T10:00:00.4Z"); err != nil || len(got.Boxes) != 2 {
		t.Errorf("since 10:00:00.4Z keeps p1, drops p2: %+v %v", got, err)
	}
	if got, err := computeclient.Usage(ctx, ana, ""); err != nil || len(got.Boxes) != 1 {
		t.Errorf("a team member sees project-level rows: %+v %v", got, err)
	}
	// A run handle is not a box: no row.
	led.Record(ctx, pid, computeapi.RunTargetHandle{Handle: "run:x/r1", Kind: "run", State: "ended", Ended: u.Ended, Acquired: u.Acquired})
	if got, err := computeclient.Usage(ctx, owner, ""); err != nil || len(got.Boxes) != 3 {
		t.Fatalf("a run left a row: %+v %v", got, err)
	}
	// Past 1000 rows: the list stops, says so, and the total still counts all.
	for i := range 1000 {
		led.Record(ctx, pid, ended(fmt.Sprintf("m%04d", i), "team2", "2026-11-06T09:30:00Z"))
	}
	got, err = computeclient.Usage(ctx, bo, "")
	if err != nil || len(got.Boxes) != 1000 || got.Truncated || got.BilledMinutes != 1000*30 {
		t.Fatalf("1000 rows: %d truncated=%v billed=%d %v", len(got.Boxes), got.Truncated, got.BilledMinutes, err)
	}
	got, err = computeclient.Usage(ctx, owner, "")
	if err != nil || len(got.Boxes) != 1000 || !got.Truncated || got.BilledMinutes != 1000*30+61+60+u.BillableMinutes {
		t.Fatalf("1003 rows: %d truncated=%v billed=%d %v", len(got.Boxes), got.Truncated, got.BilledMinutes, err)
	}
}
