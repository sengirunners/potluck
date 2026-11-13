package issuers

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/teamlink"
	"github.com/sengirunners/potluck/runtarget/rt"
)

// classes makes e's machine classes switchable: host -> class (default
// trusted), and counts ClassOf calls per box.
type classes struct {
	mu    sync.Mutex
	of    map[string]string
	calls int
	// after, when set, is the class every call after the first answers.
	after string
}

func (c *classes) set(host, class string) { c.mu.Lock(); c.of[host] = class; c.mu.Unlock() }

func switchable(e *env) *classes {
	c := &classes{of: map[string]string{}}
	e.opt.ClassOf = func(_ context.Context, b Box) (string, error) {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.calls++
		if c.after != "" && c.calls > 1 {
			return c.after, nil
		}
		if v, ok := c.of[b.Host]; ok {
			return v, nil
		}
		return ClassTrusted, nil
	}
	e.iss = e.load()
	return c
}

// MUST 5: a host's demotion ends its boxes' tokens at once (EndUntrusted),
// not at their next renewal; a box on another host keeps its token.
func TestDemotionEndsAtOnce(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, appCeiling)
	c := switchable(e)
	elsewhere := Box{Project: "01POOL", Area: "ci", ID: "box-5", Host: "host-b"}
	e.mustIssue(ident(trustedBox, write))
	e.mustIssue(ident(otherBox, write))
	e.mustIssue(ident(elsewhere, write))
	e.iss.EndUntrusted(ctx)
	if _, _, _, revoked := e.gh.snapshot(); len(revoked) != 0 {
		t.Fatalf("revoked %d with every host trusted", len(revoked))
	}
	c.set("host-a", api.MachinePublic)
	e.iss.EndUntrusted(ctx)
	_, _, minted, revoked := e.gh.snapshot()
	// One host's boxes end in no set order.
	if got, want := slices.Sorted(slices.Values(revoked)), slices.Sorted(slices.Values(minted[:2])); !slices.Equal(got, want) || e.gh.liveCount() != 1 {
		t.Fatalf("after host-a's demotion revoked %v of %v, want host-a's two", revoked, minted)
	}
	if a := e.assigned(); !slices.Equal(a, []string{"box-5"}) {
		t.Errorf("assignments %v, want host-b's alone", a)
	}
	if ev := e.events(); ev[len(ev)-1] != "revoked:"+string(EndHostDemoted) {
		t.Errorf("ledger %v", ev)
	}
}

// MUST 5: a demotion while a token is being issued: the issue revokes what
// it minted and the box keeps nothing.
func TestDemotionWhileIssuing(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, appCeiling)
	c := switchable(e)
	gate := make(chan struct{})
	e.bx.set(func(b *boxes) { b.gate = gate })
	done := make(chan error, 1)
	go func() { _, err := e.iss.Issue(ctx, ident(trustedBox, write)); done <- err }()
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		e.bx.mu.Lock()
		in := e.bx.in
		e.bx.mu.Unlock()
		if in > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the delivery never started")
		}
	}
	c.set("host-a", api.MachinePublic)
	e.iss.EndUntrusted(ctx)
	close(gate)
	if err := <-done; err == nil {
		t.Fatal("an issue on a host demoted meanwhile succeeded")
	}
	if e.gh.liveCount() != 0 || len(e.iss.held) != 0 || len(e.assigned()) != 0 {
		t.Errorf("after the demotion: %d live, %d held, assigned %v", e.gh.liveCount(), len(e.iss.held), e.assigned())
	}
}

// MUST 5: a demotion between the admission's class check and the issue
// holding the box's name is seen: nothing is minted.
func TestDemotionBeforeTheMint(t *testing.T) {
	e := newEnv(t, appCeiling)
	c := switchable(e)
	c.after = api.MachinePublic
	if _, err := e.issue(ident(trustedBox, write)); codeOf(err) != api.CodeForbidden {
		t.Fatalf("issue after a demotion: %v, want forbidden", err)
	}
	if _, mints, _, _ := e.gh.snapshot(); len(mints) != 0 {
		t.Fatalf("minted for a box whose host was demoted: %v", mints)
	}
}

// MUST 5: the module checks every box as soon as a machine's class changes
// (teamlink's ClassChanged), within seconds.
func TestModuleEndsDemotedBoxesOnClassChange(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, appCeiling)
	e.clk.mu.Lock()
	e.clk.now = time.Now()
	e.clk.mu.Unlock()
	d, rts, proj := moduleDeps(t, e.cfg.Secrets.Issuers)
	link := teamlink.New(d)
	d.Provide(teamlink.ServiceName, link)
	m := NewModule(d)
	t.Cleanup(func() { m.Close() })
	m.SweepEvery = time.Hour // only the class change can end it
	if len(link.Hooks.ClassChanged) != 1 {
		t.Fatalf("%d class hooks, want the issuer's", len(link.Hooks.ClassChanged))
	}
	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	ictx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if _, err := m.Issue(ictx, jobGrantRequest(t, d, proj, Box{Project: proj, ID: "box:small/1", Host: "lender"})); err != nil {
		t.Fatal(err)
	}
	rts.setClass(api.MachinePublic)
	link.Hooks.ClassChanged[0](ctx, proj, "lender", api.MachineClass{Machine: "lender", Class: api.MachinePublic})
	for deadline := time.Now().Add(5 * time.Second); e.gh.liveCount() != 0; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the demoted host's token is still live 5s after the class change")
		}
	}
}

// jobs is a switchable JobHolds: box id -> whether its job holds it, and
// every box it was asked about.
type jobs struct {
	mu    sync.Mutex
	holds map[string]bool
	fail  error
	asked []string
}

func (j *jobs) JobHolds(_ context.Context, req Request) (bool, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.asked = append(j.asked, req.Box.ID)
	return j.holds[req.Box.ID], j.fail
}

func jobReq(box Box) Request {
	r := ident(box, write)
	r.Subject = Subject{Job: mainJob()}
	return r
}

// MUST 6: after a restart a job's credential is issued again only while
// JobHolds says the job still holds its box (a job that completed never
// gets one again); an identity's is not asked about. GitHub not answering
// JobHolds is retried, never taken as a yes.
func TestRestartReissuesOnlyToAJobThatStillHoldsItsBox(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, appCeiling)
	j := &jobs{holds: map[string]bool{}}
	e.opt.JobHolds = j.JobHolds
	e.iss = e.load()
	e.mustIssue(jobReq(trustedBox))
	e.mustIssue(jobReq(otherBox))
	e.mustIssue(ident(thirdBox, write))
	if len(j.asked) != 0 {
		t.Fatalf("a first issue asked JobHolds: %v", j.asked)
	}
	if err := e.iss.Close(ctx); err != nil {
		t.Fatal(err)
	}
	e.alive = map[string]bool{trustedBox.key(): true, otherBox.key(): true, thirdBox.key(): true}
	j.holds[otherBox.ID] = true // trustedBox's job completed
	e.clk.add(time.Minute)
	e.iss = e.load()
	if err := e.iss.Start(ctx); err != nil {
		t.Fatal(err)
	}
	_, mints, _, _ := e.gh.snapshot()
	if len(mints) != 5 || e.bx.get(trustedBox, "GH_TOKEN") == "" {
		t.Fatalf("%d mints after the restart, want 5 (the completed job's box gets none)", len(mints))
	}
	if _, ok := e.iss.held[heldKey(trustedBox, "GH_TOKEN")]; ok {
		t.Error("the completed job's box holds a token again")
	}
	if a := e.assigned(); !slices.Equal(a, []string{"box-2", "box-3"}) {
		t.Errorf("assignments %v, want the live job's and the identity's", a)
	}
	slices.Sort(j.asked)
	if !slices.Equal(j.asked, []string{"box-1", "box-2"}) {
		t.Errorf("JobHolds asked about %v, want the two jobs' boxes", j.asked)
	}

	// JobHolds failing: retried by Tick, not issued.
	_ = e.iss.Close(ctx)
	j.mu.Lock()
	j.fail = errors.New("GitHub is down")
	j.mu.Unlock()
	e.clk.add(time.Minute)
	e.iss = e.load()
	if err := e.iss.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok := e.iss.held[heldKey(otherBox, "GH_TOKEN")]; ok || len(e.iss.reissue) != 1 {
		t.Fatalf("with JobHolds failing: held %v, %d retries pending", ok, len(e.iss.reissue))
	}
	j.mu.Lock()
	j.fail = nil
	j.mu.Unlock()
	e.clk.add(time.Minute)
	e.iss.Tick(ctx)
	if _, ok := e.iss.held[heldKey(otherBox, "GH_TOKEN")]; !ok {
		t.Error("the retried re-issue did not happen once JobHolds answered")
	}
}

// MUST 6: a job whose box JobHolds no longer gives it (it completed, its
// completion lost) loses its token at renewal; with no JobHolds at all a
// job's credential is never renewed.
func TestJobRenewalAsksJobHolds(t *testing.T) {
	ctx := context.Background()
	for _, name := range []string{"completed", "no JobHolds"} {
		e := newEnv(t, appCeiling)
		j := &jobs{holds: map[string]bool{}}
		if name != "no JobHolds" {
			e.opt.JobHolds = j.JobHolds
		}
		e.iss = e.load()
		e.mustIssue(jobReq(trustedBox))
		e.clk.add(40 * time.Minute)
		e.iss.Tick(ctx)
		_, mints, minted, revoked := e.gh.snapshot()
		if len(mints) != 1 || !slices.Equal(revoked, minted) || len(e.iss.held) != 0 || len(e.assigned()) != 0 {
			t.Errorf("%s: %d mints, revoked %v of %v, %d held: want no renewal and the token revoked", name, len(mints), revoked, minted, len(e.iss.held))
		}
	}
	// Still held: renewed.
	e := newEnv(t, appCeiling)
	j := &jobs{holds: map[string]bool{trustedBox.ID: true}}
	e.opt.JobHolds = j.JobHolds
	e.iss = e.load()
	e.mustIssue(jobReq(trustedBox))
	e.clk.add(40 * time.Minute)
	e.iss.Tick(ctx)
	if _, mints, _, _ := e.gh.snapshot(); len(mints) != 2 {
		t.Errorf("a job still holding its box: %d mints, want a renewal", len(mints))
	}
}

// MUST 14: a delivery that may have reached the box is its own outcome: the
// error wraps rt.ErrMaybeDelivered, the ledger records maybe_delivered (then
// the revocation), and a re-issue after a restart is not tried again.
func TestMaybeDeliveredIsItsOwnOutcome(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, appCeiling)
	e.bx.set(func(b *boxes) { b.fail = fmt.Errorf("%w: driver timeout", rt.ErrMaybeDelivered) })
	_, err := e.issue(ident(trustedBox, write))
	if !errors.Is(err, rt.ErrMaybeDelivered) || codeOf(err) != api.CodeUnavailable {
		t.Fatalf("a maybe-delivered issue answered %v, want unavailable wrapping ErrMaybeDelivered", err)
	}
	ev := e.events()
	if len(ev) < 3 || !strings.HasPrefix(ev[len(ev)-2], EventMaybeDelivered+":") || !strings.HasPrefix(ev[len(ev)-1], "revoked:") {
		t.Fatalf("ledger %v, want maybe_delivered then revoked", ev)
	}
	for _, s := range ev {
		if strings.Contains(s, "ghs_") {
			t.Fatalf("the ledger quotes a token: %q", s)
		}
	}
	// A plain failure is not reported as maybe delivered.
	e.bx.set(func(b *boxes) { b.fail = errors.New("the driver refused") })
	if _, err := e.issue(ident(trustedBox, write)); err == nil || errors.Is(err, rt.ErrMaybeDelivered) {
		t.Fatalf("a refused delivery answered %v", err)
	}

	// After a restart: one maybe-delivered re-issue, then no retry.
	e = newEnv(t, appCeiling)
	e.mustIssue(ident(trustedBox, write))
	_ = e.iss.Close(ctx)
	e.alive[trustedBox.key()] = true
	e.bx.set(func(b *boxes) { b.fail = fmt.Errorf("%w: driver timeout", rt.ErrMaybeDelivered) })
	e.iss = e.load()
	if err := e.iss.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if len(e.iss.reissue) != 0 || len(e.assigned()) != 0 {
		t.Errorf("a maybe-delivered re-issue is retried: %d pending, assigned %v", len(e.iss.reissue), e.assigned())
	}
}
