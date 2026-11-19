package teamlink_test

import (
	"context"
	"strings"
	"testing"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/tailnet/tailnettest"
)

// An owner makes a member an inviter (role inviter on the member's area):
// from the member's joined machine they make a guest invite into that
// area, which a friend redeems; a member or owner invite, a class, a
// tagged node, an existing name or another object stay refused; a plain
// member is refused as before; a name an owner invited stays the owner's;
// and once the owner takes the permission back, the inviter is refused and
// their unredeemed code is gone for good.
func TestInviterMakesGuestInvitesOnly(t *testing.T) {
	ctx := context.Background()
	tn := tailnettest.New()
	a := kitDaemon(t, tn, "mini", tailnettest.Identity{NodeID: "nMINI", NodeKey: "nodekey:mini", Login: "judy@example.com", Host: "mini"})
	m := kitDaemon(t, tn, "laptop", tailnettest.Identity{NodeID: "nLAPTOP", NodeKey: "nodekey:laptop", Login: "ana@example.com", Host: "laptop"})
	g := kitDaemon(t, tn, "studio", tailnettest.Identity{NodeID: "nSTUDIO", NodeKey: "nodekey:studio", Login: "bo@example.com", Host: "studio"})
	g2 := kitDaemon(t, tn, "den", tailnettest.Identity{NodeID: "nDEN", NodeKey: "nodekey:den", Login: "cy@example.com", Host: "den"})
	if _, err := a.CreateProject(ctx, api.NewProjectRequest{Slug: "orchid"}); err != nil {
		t.Fatal(err)
	}
	for _, area := range []string{"team", "hosts"} {
		if _, err := a.AddArea(ctx, "orchid", api.AddAreaRequest{Name: area}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := a.ProjectTeam(ctx, "orchid"); err != nil {
		t.Fatal(err)
	}
	inv, err := a.Invite(ctx, api.InviteRequest{Project: "orchid", Area: "team", As: "ana", Role: api.RoleMember})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Join(ctx, api.JoinRequest{Code: inv.Code}); err != nil {
		t.Fatal(err)
	}
	mp := *m
	mp.Project = "orchid"
	guest := func(as string) api.InviteRequest {
		return api.InviteRequest{Project: "orchid", Area: "team", As: as, Role: api.RoleGuest}
	}

	// A plain member: refused, as before.
	if _, err := mp.Invite(ctx, guest("bo")); !api.IsCode(err, api.CodeForbidden) {
		t.Fatalf("a plain member's guest invite: %v, want forbidden", err)
	}

	if _, err := a.Grant(ctx, api.GrantRequest{Identity: "ana", Role: "inviter", Project: "orchid", Area: "team"}); err != nil {
		t.Fatal(err)
	}
	gs, err := a.Grants(ctx, "orchid")
	if err != nil {
		t.Fatal(err)
	}
	if g := findGrant(gs, "ana", "team"); g == nil || !g.Inviter || g.Role != api.RoleMember {
		t.Fatalf("ana's grant after role inviter: %+v", g)
	}

	got, err := mp.Invite(ctx, guest("bo"))
	if err != nil {
		t.Fatalf("an inviter's guest invite: %v", err)
	}
	if got.Invite.Role != api.RoleGuest || got.Invite.Area != "team" || got.Invite.As != "bo" {
		t.Fatalf("invite: %+v", got.Invite)
	}
	if _, err := g.Join(ctx, api.JoinRequest{Code: got.Code}); err != nil {
		t.Fatalf("redeeming an inviter's guest invite: %v", err)
	}
	if g := findGrant(mustGrants(t, a), "bo", "team"); g == nil || g.Role != api.RoleGuest {
		t.Fatalf("bo's grant after joining: %+v", g)
	}

	// No owner's invite makes the inviter's guest more than a guest; an
	// owner's own grant does, and then their invites do too.
	if _, err := a.Invite(ctx, api.InviteRequest{Project: "orchid", Area: "team", As: "bo", Role: api.RoleMember}); !api.IsCode(err, api.CodeConflict) {
		t.Fatalf("an owner's member invite for an inviter's guest: %v, want conflict", err)
	}
	if _, err := a.Grant(ctx, api.GrantRequest{Identity: "bo", Role: api.RoleMember, Project: "orchid", Area: "team"}); err != nil {
		t.Fatalf("an owner raising an inviter's guest by hand: %v", err)
	}
	if _, err := a.Invite(ctx, api.InviteRequest{Project: "orchid", Area: "team", As: "bo", Role: api.RoleMember}); err != nil {
		t.Fatalf("an owner's member invite once they raised the guest: %v", err)
	}

	for name, req := range map[string]api.InviteRequest{
		"a member invite":        {Project: "orchid", Area: "team", As: "dee", Role: api.RoleMember},
		"an owner invite":        {Project: "orchid", Area: "team", As: "dee", Role: api.RoleOwner},
		"a classed invite":       {Project: "orchid", Area: "team", As: "dee", Role: api.RoleGuest, Class: api.MachinePublic},
		"a tagged invite":        {Project: "orchid", Area: "team", As: "dee", Role: api.RoleGuest, AllowTagged: true},
		"an existing guest":      guest("bo"),
		"another area":           {Project: "orchid", Area: "hosts", As: "dee", Role: api.RoleGuest},
		"the whole project":      {Project: "orchid", As: "dee", Role: api.RoleGuest},
		"a new area of its own":  {Project: "orchid", Area: "garden", As: "dee", Role: api.RoleGuest, AddArea: true},
		"an existing member too": guest("ana"),
	} {
		if _, err := mp.Invite(ctx, req); err == nil {
			t.Errorf("%s from an inviter: made, want refused", name)
		}
	}
	if hasArea(t, a.GetProject, "orchid", "garden") {
		t.Fatal("an inviter's refused invite added an area")
	}
	_, err = mp.Invite(ctx, api.InviteRequest{Project: "orchid", Area: "team", As: "dee", Role: api.RoleMember})
	if !api.IsCode(err, api.CodeForbidden) || !strings.Contains(err.Error(), "guest invites only") {
		t.Fatalf("an inviter's member invite: %v, want the guest-only refusal", err)
	}

	// A name an owner's live invite holds is not an inviter's to take, and
	// an inviter's code for a name an owner invites later no longer joins.
	if _, err := a.Invite(ctx, api.InviteRequest{Project: "orchid", Area: "team", As: "fay", Role: api.RoleMember}); err != nil {
		t.Fatal(err)
	}
	if _, err := mp.Invite(ctx, guest("fay")); !api.IsCode(err, api.CodeConflict) {
		t.Fatalf("an inviter's guest invite under an owner's invited name: %v, want conflict", err)
	}
	taken, err := mp.Invite(ctx, guest("gil"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Invite(ctx, api.InviteRequest{Project: "orchid", Area: "team", As: "gil", Role: api.RoleMember}); err != nil {
		t.Fatal(err)
	}
	if _, err := g2.Join(ctx, api.JoinRequest{Code: taken.Code}); err == nil {
		t.Fatal("an inviter's code joined under a name an owner invited since")
	}

	// An unredeemed code from before the permission goes stops working,
	// and giving the permission back does not revive it.
	pending, err := mp.Invite(ctx, guest("cy"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Grant(ctx, api.GrantRequest{Identity: "ana", Role: api.RoleMember, Project: "orchid", Area: "team"}); err != nil {
		t.Fatal(err)
	}
	ls, err := a.Invites(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range ls {
		if x.ID == pending.Invite.ID {
			t.Fatalf("a former inviter's unredeemed invite is still listed: %+v", x)
		}
	}
	if g := findGrant(mustGrants(t, a), "ana", "team"); g == nil || g.Inviter || g.Role != api.RoleMember {
		t.Fatalf("ana's grant after role member: %+v", g)
	}
	if _, err := mp.Invite(ctx, guest("eve")); !api.IsCode(err, api.CodeForbidden) {
		t.Fatalf("a former inviter's guest invite: %v, want forbidden", err)
	}
	if _, err := a.Grant(ctx, api.GrantRequest{Identity: "ana", Role: "inviter", Project: "orchid", Area: "team"}); err != nil {
		t.Fatal(err)
	}
	if _, err := g2.Join(ctx, api.JoinRequest{Code: pending.Code}); err == nil {
		t.Fatal("a former inviter's code joined once the permission came back")
	}
}

func mustGrants(t *testing.T, c interface {
	Grants(context.Context, string) ([]api.Grant, error)
}) []api.Grant {
	t.Helper()
	gs, err := c.Grants(context.Background(), "orchid")
	if err != nil {
		t.Fatal(err)
	}
	return gs
}

func findGrant(gs []api.Grant, identity, area string) *api.Grant {
	for i := range gs {
		if gs[i].Identity == identity && gs[i].Area == area {
			return &gs[i]
		}
	}
	return nil
}
