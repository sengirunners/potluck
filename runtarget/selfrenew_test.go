package runtarget_test

import (
	"context"
	"os"
	"path/filepath"
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
	"github.com/sengirunners/potluck/wiring"
)

// TestSweepLeavesSelfRenewed: with the holder live, the sweep renews a
// plain held box near its deadline but never one acquired with self_renew
// (its client renews it; a dead client lets it end at its TTL). A repeat of
// the key with self_renew flipped is a different acquire.
func TestSweepLeavesSelfRenewed(t *testing.T) {
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
	var svc *runtarget.Service
	for _, m := range dm.Modules() {
		if s, ok := m.(*runtarget.Service); ok {
			svc = s
			s.SetLookPath(func(string) (string, error) { return drv, nil })
		}
	}
	svc.AddHolderCheck(func(context.Context, api.Address) (bool, bool) { return true, true }) // every holder's session lives
	t.Cleanup(func() { svc.SetNow(time.Now) })
	ctx := context.Background()
	owner := client.WithHandler(dm.SocketHandler())
	owner.Project, owner.ProjectFrom = "acme", "flag"
	if _, err := owner.CreateProject(ctx, api.NewProjectRequest{Slug: "acme", RepoRules: api.RepoRules{Remotes: []string{repo.Remote}}}); err != nil {
		t.Fatal(err)
	}
	tok, err := owner.MintToken(ctx, api.MintTokenRequest{As: "ana", Project: "acme", Role: "member"})
	if err != nil {
		t.Fatal(err)
	}
	ana := client.WithHandler(dm.Handler)
	ana.Token, ana.Project, ana.ProjectFrom = tok.Secret, "acme", "flag"
	box := computeapi.RunTarget{Name: "sandbox", Driver: "ref", Kind: "box", Isolation: "ephemeral", Images: []string{refPin},
		Repo: repo.Remote, Branch: "main", Config: map[string]string{"remote": repo.Remote}}
	if _, err := computeclient.BindRunTarget(ctx, owner, computeapi.BindRunTargetRequest{Target: box}); err != nil {
		t.Fatal(err)
	}

	plain, err := computeclient.AcquireRunTarget(ctx, ana, "sandbox", computeapi.AcquireRunTargetRequest{RequestID: "plain", TTLS: 60})
	if err != nil {
		t.Fatal(err)
	}
	self, err := computeclient.AcquireRunTarget(ctx, ana, "sandbox", computeapi.AcquireRunTargetRequest{RequestID: "self", TTLS: 60, SelfRenew: true})
	if err != nil {
		t.Fatal(err)
	}
	if plain.SelfRenew || !self.SelfRenew {
		t.Errorf("acquire answers self_renew %v (plain) and %v (self_renew)", plain.SelfRenew, self.SelfRenew)
	}
	if _, err := computeclient.AcquireRunTarget(ctx, ana, "sandbox", computeapi.AcquireRunTargetRequest{RequestID: "self", TTLS: 60}); code(err) != api.CodeConflict {
		t.Errorf("the key again without self_renew: %v, want a conflict", err)
	}

	// 45s on: 15s of the 60s TTL left, under a third, and before the deadline.
	svc.SetNow(func() time.Time { return time.Now().Add(45 * time.Second) })
	svc.Sweep(ctx)
	svc.SetNow(time.Now)
	after := func(h computeapi.RunTargetHandle) computeapi.RunTargetHandle {
		t.Helper()
		st, err := computeclient.RunTargetStatus(ctx, ana, "sandbox", idOf(h.Handle))
		if err != nil {
			t.Fatal(err)
		}
		return st
	}
	if st := after(plain); !later(t, st.ExpiresAt, plain.ExpiresAt) {
		t.Errorf("a plain held box was not renewed: expires %s, was %s", st.ExpiresAt, plain.ExpiresAt)
	}
	if st := after(self); st.ExpiresAt != self.ExpiresAt || !st.SelfRenew {
		t.Errorf("the sweep renewed a self_renew box: expires %s, was %s (self_renew %v)", st.ExpiresAt, self.ExpiresAt, st.SelfRenew)
	}
	// Its own client still renews it.
	if _, err := computeclient.RenewRunTarget(ctx, ana, "sandbox", idOf(self.Handle), 60); err != nil {
		t.Errorf("the holder's own renew: %v", err)
	}
}

func later(t *testing.T, a, b string) bool {
	t.Helper()
	ta, err := time.Parse(time.RFC3339Nano, a)
	if err != nil {
		t.Fatalf("expires_at %q: %v", a, err)
	}
	tb, err := time.Parse(time.RFC3339Nano, b)
	if err != nil {
		t.Fatalf("expires_at %q: %v", b, err)
	}
	return ta.After(tb)
}
