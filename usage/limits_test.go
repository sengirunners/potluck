package usage_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/computeclient"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/project"
	"github.com/sengirunners/potluck/usage"
)

// TestAreaMinuteLimit: only a whole-project owner or admin sets an area's
// limit; once the area's minutes this period (a live box counted so far, an
// ended box whose row is not written yet counted from run targets' record)
// reach it, a new box there is refused in plain words that name how to raise
// it, and so is starting a stopped box; another area is not; a member reads
// only its own area's limit; raising it lets the next one through; a ledger
// that cannot be read refuses.
func TestAreaMinuteLimit(t *testing.T) {
	pl := newPool(t)
	ctx, owner := context.Background(), pl.owner
	ana, bo := pl.member("ana", "team1", "member"), pl.member("bo", "team2", "member")
	areaAdmin, wholeAdmin := pl.member("ad", "team1", "owner"), pl.member("wa", "", "owner")
	acquire := func(c *client.Client, id string) (computeapi.RunTargetHandle, error) {
		return computeclient.AcquireRunTarget(ctx, c, "sandbox", computeapi.AcquireRunTargetRequest{RequestID: id, TTLS: 600})
	}

	one := computeapi.SetAreaMinuteLimitRequest{Minutes: 1, Period: "day"}
	if _, err := computeclient.SetAreaMinuteLimit(ctx, ana, "team1", one); !api.IsCode(err, api.CodeForbidden) {
		t.Fatalf("a member set a limit: %v", err)
	}
	if _, err := computeclient.SetAreaMinuteLimit(ctx, areaAdmin, "team1", one); !api.IsCode(err, api.CodeForbidden) {
		t.Fatalf("the area's own owner set its limit: %v", err)
	}
	if _, err := computeclient.SetAreaMinuteLimit(ctx, owner, "nope", one); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("a limit on no area: %v", err)
	}
	if _, err := computeclient.SetAreaMinuteLimit(ctx, owner, "team1", computeapi.SetAreaMinuteLimitRequest{Minutes: 5, Period: "year"}); !api.IsCode(err, api.CodeBadRequest) {
		t.Fatalf("a limit per year: %v", err)
	}
	l, err := computeclient.SetAreaMinuteLimit(ctx, wholeAdmin, "team1", one)
	if err != nil || len(l.Limits) != 1 || l.Limits[0] != (computeapi.AreaMinuteLimit{Area: "team1", Minutes: 1, Period: "day"}) {
		t.Fatalf("another project owner sets it: %+v %v", l, err)
	}
	if _, err := computeclient.SetAreaMinuteLimit(ctx, owner, "team2", computeapi.SetAreaMinuteLimitRequest{Minutes: 1000}); err != nil {
		t.Fatal(err)
	}
	// A member reads its own area's limit, never another area's.
	if l, err := computeclient.AreaMinuteLimits(ctx, ana); err != nil || len(l.Limits) != 1 || l.Limits[0].Area != "team1" {
		t.Fatalf("a team1 member reads the limits: %+v %v", l, err)
	}
	if l, err := computeclient.AreaMinuteLimits(ctx, owner); err != nil || len(l.Limits) != 2 || l.Limits[1].Period != "month" {
		t.Fatalf("the owner reads the limits: %+v %v", l, err)
	}
	// A new number without a period keeps the area's period, never resets it to a month.
	if _, err := computeclient.SetAreaMinuteLimit(ctx, owner, "team2", computeapi.SetAreaMinuteLimitRequest{Minutes: 1000, Period: "week"}); err != nil {
		t.Fatal(err)
	}
	if l, err := computeclient.SetAreaMinuteLimit(ctx, owner, "team2", computeapi.SetAreaMinuteLimitRequest{Minutes: 900}); err != nil ||
		!slices.Contains(l.Limits, computeapi.AreaMinuteLimit{Area: "team2", Minutes: 900, Period: "week"}) {
		t.Fatalf("a limit set again without a period: %+v %v", l, err)
	}

	// A live box counts its minutes so far (a part of a minute is a minute).
	h, err := computeclient.AcquireRunTarget(ctx, ana, "sandbox", computeapi.AcquireRunTargetRequest{RequestID: "a1", TTLS: 600, Persist: true})
	if err != nil {
		t.Fatal(err)
	}
	refused := func(id string) {
		t.Helper()
		_, err := acquire(ana, id)
		var e *api.Error
		if !api.IsCode(err, api.CodeForbidden) || !errors.As(err, &e) ||
			!strings.Contains(e.Message, "area team1 has used 1 of its 1 sandbox minutes this day") ||
			!strings.Contains(e.Hint, "potluck usage limit team1 <minutes> --per day") {
			t.Fatalf("acquire %s at the limit: %v", id, err)
		}
	}
	refused("a2")
	// Renewing a running box adds no box: allowed at the limit (the minutes it
	// then runs are counted as it runs).
	if r, err := computeclient.RenewRunTarget(ctx, ana, "sandbox", h.Handle[strings.LastIndex(h.Handle, "/")+1:], 600); err != nil {
		t.Fatalf("renew at the limit: %+v %v", r, err)
	}
	if _, err := acquire(bo, "b1"); err != nil {
		t.Fatalf("another area has no limit: %v", err)
	}
	// A repeat of an acquire already made answers it, never asks again.
	if again, err := computeclient.AcquireRunTarget(ctx, ana, "sandbox", computeapi.AcquireRunTargetRequest{RequestID: "a1", TTLS: 600, Persist: true}); err != nil || again.Handle != h.Handle {
		t.Fatalf("a repeat at the limit: %+v %v", again, err)
	}

	// Starting a stopped box again is a new lease: refused at the limit too.
	id := h.Handle[strings.LastIndex(h.Handle, "/")+1:]
	if _, err := computeclient.StopRunTarget(ctx, ana, "sandbox", id); err != nil {
		t.Fatal(err)
	}
	if _, err := computeclient.StartRunTarget(ctx, ana, "sandbox", id, 0); !api.IsCode(err, api.CodeForbidden) || !strings.Contains(err.Error(), "sandbox minutes") {
		t.Fatalf("start of a stopped box at the limit: %v", err)
	}

	// Ended: its row counts; with the row lost, run targets' record counts.
	if _, err := computeclient.ReleaseRunTarget(ctx, ana, "sandbox", id); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		if u, err := computeclient.Usage(ctx, ana, ""); err == nil && len(u.Boxes) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no usage row")
		}
	}
	refused("a3")
	if err := pl.led.ForgetForTest(ctx, pl.pid, h.Handle); err != nil {
		t.Fatal(err)
	}
	refused("a4")

	// Raised, the next box goes through.
	if _, err := computeclient.SetAreaMinuteLimit(ctx, owner, "team1", computeapi.SetAreaMinuteLimitRequest{Minutes: 100, Period: "month"}); err != nil {
		t.Fatal(err)
	}
	if _, err := acquire(ana, "a5"); err != nil {
		t.Fatalf("after raising the limit: %v", err)
	}
	// Minutes that cannot be read refuse; an area without a limit is not asked.
	if _, err := computeclient.SetAreaMinuteLimit(ctx, owner, "team2", computeapi.SetAreaMinuteLimitRequest{}); err != nil {
		t.Fatal(err)
	}
	if err := pl.led.BreakLedgerForTest(ctx, pl.pid); err != nil {
		t.Fatal(err)
	}
	if _, err := acquire(ana, "a6"); !api.IsCode(err, api.CodeUnavailable) || !strings.Contains(err.Error(), "could not be read") {
		t.Fatalf("an unreadable ledger: %v", err)
	}
	if _, err := acquire(bo, "b2"); err != nil {
		t.Fatalf("an area without a limit, ledger unreadable: %v", err)
	}
	// 0 removes the limit.
	if l, err := computeclient.SetAreaMinuteLimit(ctx, owner, "team1", computeapi.SetAreaMinuteLimitRequest{}); err != nil || len(l.Limits) != 0 {
		t.Fatalf("removing: %+v %v", l, err)
	}
	if _, err := acquire(ana, "a7"); err != nil {
		t.Fatalf("after removing the limit: %v", err)
	}
}

// TestLimitsSection: [[box_minutes]] reads back from project.toml as written.
func TestLimitsSection(t *testing.T) {
	src := "id = \"p\"\nslug = \"acme\"\nname = \"acme\"\n\n[[areas]]\nname = \"team1\"\n\n[[box_minutes]]\narea = \"team1\"\nminutes = 600\nperiod = \"week\"\n"
	f, err := project.ParseFile(src)
	if err != nil {
		t.Fatal(err)
	}
	ls, _ := f.Section("box_minutes").([]computeapi.AreaMinuteLimit)
	if len(ls) != 1 || ls[0] != (computeapi.AreaMinuteLimit{Area: "team1", Minutes: 600, Period: "week"}) {
		t.Fatalf("parsed %+v", f.Section("box_minutes"))
	}
	if g, err := project.ParseFile(f.Encode()); err != nil || g.Encode() != f.Encode() {
		t.Fatalf("round trip: %v\n%s", err, f.Encode())
	}
	for _, bad := range []string{"minutes = 0\nperiod = \"day\"", "minutes = 5\nperiod = \"year\"", "minutes = 5\nperiod = \"day\"\nmax = 3"} {
		if _, err := project.ParseFile(strings.Replace(src, "minutes = 600\nperiod = \"week\"", bad, 1)); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

// TestSoFar: a live box counts its lease so far, whatever the driver last
// said (a figure as of acquire would hide hours), never past expires_at; a
// stopped box counts what it ran (not its time stopped), one started again
// the figure kept at that start plus its time since (a later status call's
// figure already holds that run: counting it would count the run twice); a
// timed-out acquire counts from its request.
func TestSoFar(t *testing.T) {
	at := time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC)
	ts := func(d time.Duration) string { return at.Add(d).Format(time.RFC3339Nano) }
	ran := func(m float64) *computeapi.RunTargetSpend {
		return &computeapi.RunTargetSpend{Unit: "slot_min", Amount: m}
	}
	later := 10 * time.Hour
	for _, tc := range []struct {
		name string
		h    computeapi.RunTargetHandle
		want int
	}{
		{"running, stale driver figure", computeapi.RunTargetHandle{State: "ready", Acquired: ts(0), Spend: ran(0.01)}, 600},
		{"running, past expires_at", computeapi.RunTargetHandle{State: "ready", Acquired: ts(0), ExpiresAt: ts(90 * time.Minute)}, 90},
		{"stopped after an hour", computeapi.RunTargetHandle{State: "stopped", Acquired: ts(0), ExpiresAt: ts(30 * 24 * time.Hour), Spend: ran(60)}, 60},
		{"stopped, no time figure", computeapi.RunTargetHandle{State: "stopped", Acquired: ts(0), Spend: &computeapi.RunTargetSpend{Unit: "cpu_s", Amount: 5}}, 600},
		{"started again 10 minutes ago", computeapi.RunTargetHandle{State: "ready", Acquired: ts(0), Started: ts(later - 10*time.Minute), Spend: ran(60)}, 70},
		{"started again, figure refreshed since", computeapi.RunTargetHandle{State: "ready", Acquired: ts(0), Started: ts(later - 10*time.Minute), StartedSpend: ran(60), Spend: ran(70)}, 70},
		{"acquire timed out", computeapi.RunTargetHandle{State: "ready", Created: ts(later - 5*time.Minute)}, 5},
	} {
		if n := usage.SoFar(tc.h, at.Add(later)); n != tc.want {
			t.Errorf("%s: %d minutes, want %d", tc.name, n, tc.want)
		}
	}
}

// TestUnwrittenEndCounts (zm-eakh.12.5.36): a box that ended long ago, its
// usage row never written, still counts toward its area's limit for the
// whole period (not just the last hour) until Reconcile writes it.
func TestUnwrittenEndCounts(t *testing.T) {
	pl := newPool(t)
	ctx, owner := context.Background(), pl.owner
	ana := pl.member("ana", "team1", "member")
	t0 := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	clock := t0
	pl.rts.SetNow(func() time.Time { return clock })
	pl.led.SetNowForTest(func() time.Time { return clock })
	defer pl.rts.SetNow(time.Now)
	if _, err := computeclient.SetAreaMinuteLimit(ctx, owner, "team1", computeapi.SetAreaMinuteLimitRequest{Minutes: 30, Period: "month"}); err != nil {
		t.Fatal(err)
	}
	h, err := computeclient.AcquireRunTarget(ctx, ana, "sandbox", computeapi.AcquireRunTargetRequest{RequestID: "o1", TTLS: 600})
	if err != nil {
		t.Fatal(err)
	}
	clock = t0.Add(30 * time.Minute)
	if _, err := computeclient.ReleaseRunTarget(ctx, ana, "sandbox", h.Handle[strings.LastIndex(h.Handle, "/")+1:]); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		if u, err := computeclient.Usage(ctx, ana, ""); err == nil && len(u.Boxes) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no usage row")
		}
	}
	if err := pl.led.ForgetForTest(ctx, pl.pid, h.Handle); err != nil {
		t.Fatal(err)
	}
	clock = t0.Add(5 * time.Hour) // far past an hour since the end
	if _, err := computeclient.AcquireRunTarget(ctx, ana, "sandbox", computeapi.AcquireRunTargetRequest{RequestID: "o2", TTLS: 600}); !api.IsCode(err, api.CodeForbidden) ||
		!strings.Contains(err.Error(), "used 30 of its 30") {
		t.Fatalf("a box that ended 4.5 hours ago, row unwritten: %v", err)
	}
	if n, err := pl.led.Reconcile(ctx); err != nil || n != 1 {
		t.Fatalf("reconcile wrote %d, %v", n, err)
	}
	if _, err := computeclient.AcquireRunTarget(ctx, ana, "sandbox", computeapi.AcquireRunTargetRequest{RequestID: "o3", TTLS: 600}); !api.IsCode(err, api.CodeForbidden) {
		t.Fatalf("with the row written: %v", err)
	}
}

// TestLimitOutlivesItsArea (zm-eakh.12.5.32): a limit on an area the project
// no longer defines is dropped by the next write of limits or the next
// reconcile pass, so it does not return with an area made again.
func TestLimitOutlivesItsArea(t *testing.T) {
	pl := newPool(t)
	ctx, owner := context.Background(), pl.owner
	ghost := computeapi.AreaMinuteLimit{Area: "ghost", Minutes: 5, Period: "day"}
	names := func() []string {
		t.Helper()
		l, err := computeclient.AreaMinuteLimits(ctx, owner)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, x := range l.Limits {
			out = append(out, x.Area)
		}
		return out
	}
	if err := pl.led.PlantLimitForTest(ctx, pl.pid, ghost); err != nil {
		t.Fatal(err)
	}
	if got := names(); !slices.Equal(got, []string{"ghost"}) {
		t.Fatalf("planted: %v", got)
	}
	if _, err := pl.led.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if got := names(); len(got) != 0 {
		t.Fatalf("after a reconcile pass: %v", got)
	}
	if err := pl.led.PlantLimitForTest(ctx, pl.pid, ghost); err != nil {
		t.Fatal(err)
	}
	if _, err := computeclient.SetAreaMinuteLimit(ctx, owner, "team1", computeapi.SetAreaMinuteLimitRequest{Minutes: 5, Period: "day"}); err != nil {
		t.Fatal(err)
	}
	if got := names(); !slices.Equal(got, []string{"team1"}) {
		t.Fatalf("after setting another area's limit: %v", got)
	}
}

// TestSoFarAgreesWithCompute (zm-eakh.12.5.32): a live box counted as if it
// ended at t is counted as the ledger bills it ending at t, whatever its
// expires_at says. A driver's time figure may be stale on a live box, so it
// is not counted live (the whole lease is, never less than the ledger bills).
func TestSoFarAgreesWithCompute(t *testing.T) {
	at := time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC)
	ts := func(d time.Duration) string { return at.Add(d).Format(time.RFC3339Nano) }
	end := 95*time.Minute + 20*time.Second
	for name, h := range map[string]computeapi.RunTargetHandle{
		"plain":        {Acquired: ts(0)},
		"past expiry":  {Acquired: ts(0), ExpiresAt: ts(40 * time.Minute)},
		"cpu seconds":  {Acquired: ts(0), Spend: &computeapi.RunTargetSpend{Unit: "cpu_s", Amount: 3}},
		"whole minute": {Acquired: ts(0), ExpiresAt: ts(60 * time.Minute)},
	} {
		h.State, h.Kind, h.Handle = "ready", "box", "box:x/"+name
		live := usage.SoFar(h, at.Add(end))
		h.State, h.Ended = "released", ts(end)
		if billed := usage.Compute(h).BillableMinutes; live != billed {
			t.Errorf("%s: counted %d live, billed %d ended", name, live, billed)
		}
	}
	h := computeapi.RunTargetHandle{State: "ready", Kind: "box", Handle: "box:x/fig", Acquired: ts(0), Spend: &computeapi.RunTargetSpend{Unit: "slot_min", Amount: 12.5}}
	live := usage.SoFar(h, at.Add(end))
	h.State, h.Ended = "released", ts(end)
	if billed := usage.Compute(h).BillableMinutes; live < billed {
		t.Errorf("a driver figure: counted %d live, billed %d ended", live, billed)
	}
}

// TestLimitReadTooLarge: an area whose ended boxes cannot all be read in one
// pass refuses a new box (fails closed) rather than reading as under its limit.
func TestLimitReadTooLarge(t *testing.T) {
	pl := newPool(t)
	ctx, ana := context.Background(), pl.member("ana", "team1", "member")
	if _, err := computeclient.SetAreaMinuteLimit(ctx, pl.owner, "team1", computeapi.SetAreaMinuteLimitRequest{Minutes: 1000, Period: "month"}); err != nil {
		t.Fatal(err)
	}
	defer usage.SetMaxEndPagesForTest(0)()
	if _, err := computeclient.AcquireRunTarget(ctx, ana, "sandbox", computeapi.AcquireRunTargetRequest{RequestID: "z1", TTLS: 600}); !api.IsCode(err, api.CodeUnavailable) ||
		!strings.Contains(err.Error(), "too many sandboxes ended") {
		t.Fatalf("a read that cannot finish: %v", err)
	}
}
