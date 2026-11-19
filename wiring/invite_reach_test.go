package wiring_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/project"
	"github.com/sengirunners/potluck/kit/tailnet/tailnettest"
	"github.com/sengirunners/potluck/kit/tunnel"
	"github.com/sengirunners/potluck/kit/tunnel/tunneltest"
)

// An invite names the tunnel only while its URL answers from outside as
// this home: a tunnel that is configured but down (cloudflared stopped, a
// quick tunnel's name gone) or that reaches another home gives the
// tailnet's code with the reason, or refuses when the tunnel was asked for.
// A refused route makes no invite, and the check carries nothing of one.
func TestInviteNamesTheTunnelOnlyWhileItAnswers(t *testing.T) {
	old := tunnel.ReachWithin
	tunnel.ReachWithin = 2 * time.Second
	t.Cleanup(func() { tunnel.ReachWithin = old })
	ctx := context.Background()
	tn := tailnettest.New()
	listen := freeLoopback(t)
	edge, err := tunneltest.New(listen) // stands in for Cloudflare's edge
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { edge.Close() })
	tap := &syncBuf{}
	edge.Tap(tap)
	a := startComputeWith(t, tn, "mini", tailnettest.Identity{NodeID: "nMINI", NodeKey: "nodekey:mini", Login: "judy@example.com", Host: "mini"},
		&syncBuf{}, &config.Tunnel{Listen: listen, URL: edge.URL()})
	if _, err := a.owner.CreateProject(ctx, api.NewProjectRequest{Slug: "orchid"}); err != nil {
		t.Fatal(err)
	}
	if mode, err := a.owner.ProjectTeam(ctx, "orchid"); err != nil || !mode.Team {
		t.Fatalf("team: %+v %v", mode, err)
	}
	invites := func() int {
		t.Helper()
		is, err := a.owner.Invites(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return len(is)
	}
	invite := func(via string) (api.InviteResponse, error) {
		return a.owner.Invite(ctx, api.InviteRequest{Project: "orchid", As: "ana", Role: api.RoleMember, Via: via})
	}
	secret := func(code string) string {
		t.Helper()
		jc, err := project.ParseJoinCode(code)
		if err != nil {
			t.Fatal(err)
		}
		return jc.Secret
	}

	// Up: the default is the tunnel's code, with nothing left out.
	r, err := invite("")
	if err != nil || !strings.HasPrefix(r.Code, edge.URL()+"#") || len(r.NotUsed) != 0 {
		t.Fatalf("tunnel up: %+v %v, want an https code naming %s", r, err, edge.URL())
	}
	if tap.has(secret(r.Code)) {
		t.Fatal("the invite's secret crossed the tunnel")
	}

	// Down: the tailnet's code, saying why the tunnel was not used.
	edge.Offline(true)
	n := invites()
	r, err = invite("")
	if err != nil || strings.Contains(r.Code, "://") {
		t.Fatalf("tunnel down: %+v %v, want the tailnet's code", r, err)
	}
	if len(r.NotUsed) != 1 || r.NotUsed[0].Via != api.ViaTunnel || !strings.Contains(r.NotUsed[0].Reason, "does not reach this machine now") || r.NotUsed[0].Hint == "" {
		t.Fatalf("tunnel down: not used %+v, want the tunnel with its reason and a fix", r.NotUsed)
	}
	if got := invites(); got != n+1 {
		t.Fatalf("one invite asked, %d made", got-n)
	}

	// Asked for by name while down: refused, and nothing made.
	n = invites()
	if _, err := invite(api.ViaTunnel); !api.IsCode(err, api.CodeUnavailable) || !strings.Contains(err.Error(), "does not reach this machine now") {
		t.Fatalf("--via tunnel while down: %v, want unavailable saying so", err)
	}
	if got := invites(); got != n {
		t.Fatalf("a refused invite made %d", got-n)
	}

	// Another home behind the URL (a hostname pointing elsewhere): not this
	// home's tunnel, so not offered.
	other := freeLoopback(t)
	startComputeWith(t, tn, "other", tailnettest.Identity{NodeID: "nOTHER", NodeKey: "nodekey:other", Login: "eve@example.com", Host: "other"},
		&syncBuf{}, &config.Tunnel{Listen: other, URL: "https://other.example"})
	edge.SetTarget(other)
	edge.Offline(false)
	if _, err := invite(api.ViaTunnel); !api.IsCode(err, api.CodeUnavailable) || !strings.Contains(err.Error(), "another") {
		t.Fatalf("another home behind the URL: %v, want unavailable naming another home", err)
	}

	// Neither route works: one answer naming both, nothing made.
	if _, err := a.owner.TailnetDown(ctx); err != nil {
		t.Fatal(err)
	}
	n = invites()
	_, err = invite("")
	if !api.IsCode(err, api.CodeUnavailable) || !strings.Contains(err.Error(), "tunnel") || !strings.Contains(err.Error(), "tailnet") {
		t.Fatalf("both down: %v, want unavailable naming both routes", err)
	}
	if got := invites(); got != n {
		t.Fatalf("a refused invite made %d", got-n)
	}
}
