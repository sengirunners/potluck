package teamlink_test

import (
	"context"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/server"
	"github.com/sengirunners/potluck/kit/tailnet"
	"github.com/sengirunners/potluck/kit/tailnet/tailnettest"
	"github.com/sengirunners/potluck/wiring"
)

// kitDaemon serves a daemon wired with the KIT base only (no team, work,
// mail or sessions) in a temp home, its node on tn as id and up.
func kitDaemon(t *testing.T, tn *tailnettest.Net, machine string, id tailnettest.Identity) *client.Client {
	t.Helper()
	return kitDaemonWith(t, tn, machine, id, nil)
}

// kitDaemonWith is kitDaemon with mut applied to its config first.
func kitDaemonWith(t *testing.T, tn *tailnettest.Net, machine string, id tailnettest.Identity, mut func(*config.Config)) *client.Client {
	t.Helper()
	home, err := os.MkdirTemp("", "zm")
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadFrom(home)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Human, cfg.Machine = machine+"-human", machine
	if mut != nil {
		mut(cfg)
	}
	ctx, cancel := context.WithCancel(context.Background())
	dm, err := server.New(ctx, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), wiring.Kit)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	for _, m := range dm.Modules() {
		if s, ok := m.(*tailnet.Service); ok {
			s.NewNode = tn.NodeFunc(id)
		}
	}
	done := make(chan error, 1)
	go func() { done <- dm.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Errorf("daemon %s did not stop", machine)
		}
		os.RemoveAll(home)
	})
	c := client.WithHandler(dm.SocketHandler())
	if err := client.NewUnix(cfg.SocketPath()).WaitReady(ctx, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if st, err := c.TailnetUp(ctx, api.TailnetUpRequest{}); err != nil || !st.Running {
		t.Fatalf("%s: tailnet up: %+v %v", machine, st, err)
	}
	return c
}

// A KIT-only daemon makes a team project and an invite; another KIT-only
// daemon joins with the code and leaves, and the authority revokes it.
func TestKitJoinLeave(t *testing.T) {
	ctx := context.Background()
	tn := tailnettest.New()
	a := kitDaemon(t, tn, "mini", tailnettest.Identity{NodeID: "nMINI", NodeKey: "nodekey:mini", Login: "judy@example.com", Host: "mini"})
	m := kitDaemon(t, tn, "laptop", tailnettest.Identity{NodeID: "nLAPTOP", NodeKey: "nodekey:laptop", Login: "ana@example.com", Host: "laptop"})

	p, err := a.CreateProject(ctx, api.NewProjectRequest{Slug: "orchid"})
	if err != nil {
		t.Fatal(err)
	}
	if mode, err := a.ProjectTeam(ctx, "orchid"); err != nil || !mode.Team {
		t.Fatalf("team: %+v %v", mode, err)
	}
	inv, err := a.Invite(ctx, api.InviteRequest{Project: "orchid", As: "ana", Role: api.RoleMember})
	if err != nil {
		t.Fatal(err)
	}
	j, err := m.Join(ctx, api.JoinRequest{Code: inv.Code})
	if err != nil || j.Remote == nil || j.Remote.Project.ID != p.ID || j.Remote.State != api.RemoteOK {
		t.Fatalf("join: %+v %v", j, err)
	}
	rs, err := m.Remotes(ctx)
	if err != nil || len(rs) != 1 || rs[0].Project.Slug != "orchid" || rs[0].Role != api.RoleMember {
		t.Fatalf("remotes: %+v %v", rs, err)
	}
	if _, err := m.Join(ctx, api.JoinRequest{Code: inv.Code}); err == nil {
		t.Fatal("a used code joined again")
	}
	if ls, err := a.Invites(ctx); err != nil || len(ls) != 1 || ls[0].Revoked {
		t.Fatalf("invites while joined: %+v %v", ls, err)
	}
	l, err := m.Leave(ctx, "orchid")
	if err != nil || !l.Revoked || l.Project.ID != p.ID {
		t.Fatalf("leave: %+v %v", l, err)
	}
	if rs, err := m.Remotes(ctx); err != nil || len(rs) != 0 {
		t.Fatalf("remotes after leave: %+v %v", rs, err)
	}
	// The authority's invite list says its token is revoked, not in use.
	ls, err := a.Invites(ctx)
	if err != nil || len(ls) != 1 || ls[0].State != api.InviteRedeemed || !ls[0].Revoked || ls[0].Used {
		t.Fatalf("invites after leave: %+v %v", ls, err)
	}
}
