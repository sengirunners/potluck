package wiring_test

import (
	"context"
	"testing"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/tailnet/tailnettest"
	"github.com/sengirunners/potluck/runners"
)

// A lending member reads the authority's pool settings through its own
// daemon, even though its local broker has never been configured.
func TestRunnerPoolThroughMember(t *testing.T) {
	ctx := context.Background()
	tn := tailnettest.New()
	a := startCompute(t, tn, "mini", tailnettest.Identity{NodeID: "nMINI", NodeKey: "nodekey:mini", Login: "judy@example.com", Host: "mini"})
	m := startCompute(t, tn, "laptop", tailnettest.Identity{NodeID: "nLAPTOP", NodeKey: "nodekey:laptop", Login: "ana@example.com", Host: "laptop"})
	if _, err := a.owner.CreateProject(ctx, api.NewProjectRequest{Slug: "orchid"}); err != nil {
		t.Fatal(err)
	}
	if err := a.owner.Do(ctx, "POST", "/v0/runners/setup", runners.SetupRequest{Pool: "orchid", PublicURL: "https://pool.example"}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := a.owner.ProjectTeam(ctx, "orchid"); err != nil {
		t.Fatal(err)
	}
	inv, err := a.owner.Invite(ctx, api.InviteRequest{Project: "orchid", As: "ana", Role: api.RoleMember})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.owner.Join(ctx, api.JoinRequest{Code: inv.Code}); err != nil {
		t.Fatal(err)
	}
	m.owner.Project, m.owner.ProjectFrom = "orchid", "flag"
	var pool runners.Setup
	if err := m.owner.Do(ctx, "GET", "/v0/runners/pool", nil, &pool); err != nil {
		t.Fatal(err)
	}
	if pool.Pool != "orchid" || pool.Label != "orchid" || pool.JobTTLS <= 0 {
		t.Fatalf("authority's pool: %+v", pool)
	}
	if pool.App != nil || pool.Listen != "" || pool.PublicURL != "" {
		t.Fatalf("local setup details exposed: %+v", pool)
	}
}
