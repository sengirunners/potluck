package issuers

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/config"
)

// A job's potluck-token outlives an authority restart (zm-3x35): Close
// leaves it valid, the next Start holds it again for a box that still
// lives, so the box's file keeps a working credential throughout, and the
// first Tick renews it (every check asked again), the old one revoked after
// the grace. A Start tried again does not revoke what it holds again.
func TestPotluckJobTokenKeptAcrossARestart(t *testing.T) {
	ctx := context.Background()
	p := newPotluckEnv(t)
	req := p.req(trustedBox)
	p.alive[req.Box.key()] = true
	p.mustIssue(req)
	first := p.bx.get(req.Box, "POTLUCK_TOKEN")
	if err := p.iss.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if !p.works(first) {
		t.Fatal("the job's token was revoked at Close")
	}
	p.clk.add(time.Minute)
	next := p.load()
	for range 2 { // the module tries Start again after an error
		if err := next.Start(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if !p.works(first) || p.bx.get(req.Box, "POTLUCK_TOKEN") != first {
		t.Fatalf("after the next start: the old token works %v, the box's file unchanged %v", p.works(first),
			p.bx.get(req.Box, "POTLUCK_TOKEN") == first)
	}
	if evs := strings.Join(p.events(), ";"); strings.Contains(evs, "revoked") || strings.Contains(evs, "expired_unrevoked") {
		t.Fatalf("the held-again token was settled: %s", evs)
	}
	next.Tick(ctx)
	second := p.bx.get(req.Box, "POTLUCK_TOKEN")
	if second == first || !p.works(second) || !p.works(first) {
		t.Fatalf("the first tick: renewed %v, new works %v, old works through the grace %v", second != first, p.works(second), p.works(first))
	}
	p.clk.add(6 * time.Minute)
	next.Tick(ctx)
	if p.works(first) || !p.works(second) {
		t.Fatalf("after the grace: old works %v, new works %v", p.works(first), p.works(second))
	}
}

// A token held again is ended like any other: the box's end revokes it
// (by its hash: its value is the box's alone) and forgets the assignment.
func TestPotluckTokenHeldAgainEndsWithItsBox(t *testing.T) {
	ctx := context.Background()
	p := newPotluckEnv(t)
	req := p.req(trustedBox)
	p.alive[req.Box.key()] = true
	p.mustIssue(req)
	secret := p.bx.get(req.Box, "POTLUCK_TOKEN")
	p.iss.Close(ctx)
	next := p.load()
	if err := next.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if err := next.End(ctx, req.Box, EndReleased); err != nil {
		t.Fatal(err)
	}
	if p.works(secret) {
		t.Fatal("the held-again token works after its box ended")
	}
	if a, _ := (SQLLedger{p.db}).Assigned(ctx, "gh"); len(a) != 0 {
		t.Fatalf("assignments %v after the box ended", a)
	}
}

// A token held again whose job no longer holds its box (it completed while
// the authority was down) is revoked at the first Tick, not renewed.
func TestPotluckTokenHeldAgainEndsWhenTheJobNoLongerHolds(t *testing.T) {
	ctx := context.Background()
	p := newPotluckEnv(t)
	req := p.req(trustedBox)
	p.alive[req.Box.key()] = true
	p.mustIssue(req)
	secret := p.bx.get(req.Box, "POTLUCK_TOKEN")
	p.iss.Close(ctx)
	p.opt.JobHolds = func(context.Context, Request) (bool, error) { return false, nil }
	next := p.load()
	if err := next.Start(ctx); err != nil {
		t.Fatal(err)
	}
	next.Tick(ctx)
	if p.works(secret) || p.bx.get(req.Box, "POTLUCK_TOKEN") != secret {
		t.Fatalf("after the first tick: the old token works %v, a new one delivered %v", p.works(secret),
			p.bx.get(req.Box, "POTLUCK_TOKEN") != secret)
	}
	if len(next.held) != 0 {
		t.Fatalf("still held: %d", len(next.held))
	}
}

// Only a job's potluck-token is kept: a person's is revoked at Close (its
// caller is kept in memory only, so it could not pass again).
func TestPotluckPersonTokenRevokedAtClose(t *testing.T) {
	ctx := context.Background()
	p := newPotluckEnv(t)
	reg := p.d.Projects
	caller := reg.Local("")
	box := trustedBox
	box.Project, box.Area = p.proj, ""
	p.alive[box.key()] = true
	p.mustIssue(Request{Kind: KindPotluckToken, Name: "POTLUCK_TOKEN", Subject: Subject{IdentityID: caller.Identity.ID}, Box: box,
		Role: "member", Caller: caller})
	secret := p.bx.get(box, "POTLUCK_TOKEN")
	if !p.works(secret) {
		t.Fatal("the person's token does not work")
	}
	p.iss.Close(ctx)
	if p.works(secret) {
		t.Fatal("a person's token still works after Close")
	}
}

// The module revokes a potluck-token an earlier run kept for an issuer that
// does not run now (its config removed): nothing else would take it back
// before its expiry.
func TestModuleRevokesTokensOfAnIssuerGone(t *testing.T) {
	ctx := context.Background()
	p := newPotluckEnv(t)
	req := p.req(trustedBox)
	p.alive[req.Box.key()] = true
	p.mustIssue(req)
	secret := p.bx.get(req.Box, "POTLUCK_TOKEN")
	p.iss.Close(ctx)
	if !p.works(secret) {
		t.Fatal("the job's token was revoked at Close")
	}
	p.d.Config.File.Secrets = config.Secrets{} // the next run has no issuer
	m := NewModule(p.d)
	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if p.works(secret) {
		t.Fatal("a token of an issuer that no longer runs still works after the module started")
	}
	if evs := strings.Join(p.events(), ";"); !strings.Contains(evs, "revoked:its issuer no longer runs") {
		t.Fatalf("ledger %s", evs)
	}
}

// assignedFails is a Ledger whose next Assigned errors when fail is set (a
// Start that cannot finish).
type assignedFails struct {
	Ledger
	fail bool
}

func (l *assignedFails) Assigned(ctx context.Context, issuer string) ([]Request, error) {
	if l.fail {
		l.fail = false
		return nil, errors.New("the ledger is unavailable")
	}
	return l.Ledger.Assigned(ctx, issuer)
}

// A kept token is revoked when its box, or its grant, ends before the next
// run's Start has succeeded (Start is what holds it again), not left valid
// until Start succeeds or it expires.
func TestPotluckKeptTokenEndsBeforeStart(t *testing.T) {
	ends := map[string]func(g *Minted, box Box, grant string) error{
		"box":   func(g *Minted, box Box, _ string) error { return g.End(context.Background(), box, EndReleased) },
		"grant": func(g *Minted, _ Box, grant string) error { return g.EndGrant(context.Background(), grant) },
	}
	for name, end := range ends {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			p := newPotluckEnv(t)
			req := p.req(trustedBox)
			p.alive[req.Box.key()] = true
			p.mustIssue(req)
			secret := p.bx.get(req.Box, "POTLUCK_TOKEN")
			out, err := (SQLLedger{p.db}).Outstanding(ctx, "gh")
			if err != nil || len(out) != 1 || out[0].Grant == "" {
				t.Fatalf("outstanding %+v, %v: want one entry naming its grant", out, err)
			}
			p.iss.Close(ctx)
			led := &assignedFails{Ledger: p.opt.Ledger, fail: true}
			p.opt.Ledger = led
			next := p.load()
			if err := next.Start(ctx); err == nil {
				t.Fatal("Start succeeded with the ledger failing")
			}
			if !p.works(secret) {
				t.Fatal("the kept token was revoked by a Start that failed")
			}
			if err := end(next, req.Box, out[0].Grant); err != nil {
				t.Fatal(err)
			}
			if p.works(secret) {
				t.Fatal("the kept token works after its end, before Start succeeded")
			}
			if err := next.Start(ctx); err != nil {
				t.Fatal(err)
			}
			if p.works(secret) || len(next.held) != 0 {
				t.Fatalf("after Start: token works %v, held %d", p.works(secret), len(next.held))
			}
		})
	}
}

// A kept token is revoked when its host is demoted, its host's swap turns
// unsafe, or its box is gone while the next run's Start keeps failing: the
// ends the module runs on a class change and on its sweep reach an earlier
// run's credentials too, not only what Start has taken back. While nothing
// has changed they leave it valid.
func TestPotluckKeptTokenEndsByCheckBeforeStart(t *testing.T) {
	ctx := context.Background()
	cases := map[string]func(p *potluckEnv, g *Minted, box Box){
		"demoted": func(_ *potluckEnv, g *Minted, _ Box) {
			g.o.ClassOf = func(context.Context, Box) (string, error) { return api.MachinePublic, nil }
			g.EndUntrusted(ctx)
		},
		"swap unsafe": func(_ *potluckEnv, g *Minted, _ Box) {
			g.o.BoxTakesSecrets = func(context.Context, Box) error {
				return api.Errorf(api.CodeForbidden, "lender swaps to a plain disk: fix swap there")
			}
			g.EndUntrusted(ctx)
		},
		"gone": func(p *potluckEnv, g *Minted, box Box) {
			p.alive[box.key()] = false
			g.Sweep(ctx, func(b Box) bool { return p.alive[b.key()] })
		},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			p := newPotluckEnv(t)
			req := p.req(trustedBox)
			p.alive[req.Box.key()] = true
			p.mustIssue(req)
			secret := p.bx.get(req.Box, "POTLUCK_TOKEN")
			p.iss.Close(ctx)
			p.opt.Ledger = &assignedFails{Ledger: p.opt.Ledger, fail: true}
			next := p.load()
			if err := next.Start(ctx); err == nil {
				t.Fatal("Start succeeded with the ledger failing")
			}
			next.o.BoxTakesSecrets = func(context.Context, Box) error {
				return api.Errorf(api.CodeUnavailable, "lender advertises no run target now")
			}
			next.EndUntrusted(ctx)
			next.Sweep(ctx, func(b Box) bool { return p.alive[b.key()] })
			if !p.works(secret) {
				t.Fatal("the kept token was revoked with its host trusted, its swap not known and its box live")
			}
			next.o.BoxTakesSecrets = nil
			change(p, next, req.Box)
			if p.works(secret) {
				t.Fatal("the kept token works after the change, before Start succeeded")
			}
			if err := next.Start(ctx); err != nil {
				t.Fatal(err)
			}
			if p.works(secret) || len(next.held) != 0 {
				t.Fatalf("after Start: token works %v, held %d", p.works(secret), len(next.held))
			}
		})
	}
}

// A token held again whose first checks cannot be answered is not kept on
// the earlier run's word: it stays through recheckGrace (a blip), then is
// revoked rather than kept to its expiry.
func TestPotluckTokenHeldAgainRevokedWhenItsChecksDoNotAnswer(t *testing.T) {
	ctx := context.Background()
	p := newPotluckEnv(t)
	req := p.req(trustedBox)
	p.alive[req.Box.key()] = true
	p.mustIssue(req)
	secret := p.bx.get(req.Box, "POTLUCK_TOKEN")
	p.iss.Close(ctx)
	p.clk.add(time.Minute)
	p.opt.JobHolds = func(context.Context, Request) (bool, error) { return false, errors.New("github is down") }
	next := p.load()
	if err := next.Start(ctx); err != nil {
		t.Fatal(err)
	}
	next.Tick(ctx)
	if !p.works(secret) {
		t.Fatal("revoked at the first unanswered check, inside the grace")
	}
	p.clk.add(recheckGrace)
	next.Tick(ctx)
	if p.works(secret) {
		t.Fatal("still works after the grace with no check answered")
	}
	if len(next.held) != 0 {
		t.Fatalf("still held: %d", len(next.held))
	}
}
