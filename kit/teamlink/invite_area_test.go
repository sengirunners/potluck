package teamlink_test

import (
	"context"
	"slices"
	"testing"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/tailnet/tailnettest"
)

// An invite with add_area into an area its team project lacks makes the
// area first (a pool made before its "team" area was); without add_area the
// invite is refused as before, and a solo project gets no area.
func TestInviteAddsMissingArea(t *testing.T) {
	ctx := context.Background()
	tn := tailnettest.New()
	a := kitDaemon(t, tn, "mini", tailnettest.Identity{NodeID: "nMINI", NodeKey: "nodekey:mini", Login: "judy@example.com", Host: "mini"})
	for _, slug := range []string{"pool", "solo"} {
		if _, err := a.CreateProject(ctx, api.NewProjectRequest{Slug: slug}); err != nil {
			t.Fatal(err)
		}
		if _, err := a.AddArea(ctx, slug, api.AddAreaRequest{Name: "hosts"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := a.ProjectTeam(ctx, "pool"); err != nil {
		t.Fatal(err)
	}
	req := api.InviteRequest{Project: "pool", Area: "team", As: "ana", Role: api.RoleMember}
	if _, err := a.Invite(ctx, req); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("invite into a missing area without add_area: %v, want not_found", err)
	}
	req.AddArea = true
	inv, err := a.Invite(ctx, req)
	if err != nil {
		t.Fatalf("invite with add_area: %v", err)
	}
	if inv.Code == "" || inv.Invite.Area != "team" {
		t.Fatalf("invite: %+v", inv.Invite)
	}
	if !hasArea(t, a.GetProject, "pool", "team") {
		t.Fatal("pool has no team area after the invite")
	}
	// Again: the area is there, nothing is added twice.
	if _, err := a.Invite(ctx, api.InviteRequest{Project: "pool", Area: "team", As: "bo", Role: api.RoleMember, AddArea: true}); err != nil {
		t.Fatalf("second invite: %v", err)
	}
	if _, err := a.Invite(ctx, api.InviteRequest{Project: "solo", Area: "team", As: "ana", Role: api.RoleMember, AddArea: true}); err == nil {
		t.Fatal("an invite into a solo project was made")
	}
	if hasArea(t, a.GetProject, "solo", "team") {
		t.Fatal("a solo project got an area from a refused invite")
	}
	// Invites at once into an area a pool lacks all succeed: one adds it.
	errs := make(chan error, 4)
	for _, who := range []string{"cy", "di", "ed", "flo"} {
		go func() {
			_, err := a.Invite(ctx, api.InviteRequest{Project: "pool", Area: "crew", As: who, Role: api.RoleMember, AddArea: true})
			errs <- err
		}()
	}
	for range 4 {
		if err := <-errs; err != nil {
			t.Fatalf("concurrent invite: %v", err)
		}
	}
}

func hasArea(t *testing.T, get func(context.Context, string) (api.Project, error), slug, area string) bool {
	t.Helper()
	p, err := get(context.Background(), slug)
	if err != nil {
		t.Fatal(err)
	}
	return slices.ContainsFunc(p.Areas, func(x api.Area) bool { return x.Name == area })
}
