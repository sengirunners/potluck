package cli

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/tailnet/tailnettest"
)

// A pool made before create gave it a team area (only "hosts") still takes
// a member's invite: the invite completes the pool first.
func TestInviteCompletesOldPool(t *testing.T) {
	ctx := context.Background()
	a := startMachine(t, tailnettest.New(), "mini", "judy@example.com")
	c := client.WithHandler(a.dm.SocketHandler())
	if st, err := c.TailnetUp(ctx, api.TailnetUpRequest{}); err != nil || !st.Running {
		t.Fatalf("tailnet up: %+v %v", st, err)
	}
	if _, err := c.CreateProject(ctx, api.NewProjectRequest{Slug: "old"}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.AddArea(ctx, "old", api.AddAreaRequest{Name: HostsArea}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ProjectTeam(ctx, "old"); err != nil {
		t.Fatal(err)
	}
	if out := a.ok(t, "invite", "ana"); !strings.Contains(out, "potluck join '") {
		t.Fatalf("member invite:\n%s", out)
	}
	p, err := c.GetProject(ctx, "old")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(p.Areas, func(x api.Area) bool { return x.Name == TeamArea }) {
		t.Fatalf("areas after the invite: %+v", p.Areas)
	}
}
