package teamlink_test

import (
	"context"
	"testing"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/tailnet/tailnettest"
)

// A member's daemon forwards an invite into the joined project to its
// authority, where only an identity that owns or administers the whole
// project by its own grants makes one: a plain member is refused, the
// same member made admin gets a code, and the authority keeps the invite,
// made by that member.
func TestInviteFromJoinedMachine(t *testing.T) {
	ctx := context.Background()
	tn := tailnettest.New()
	a := kitDaemon(t, tn, "mini", tailnettest.Identity{NodeID: "nMINI", NodeKey: "nodekey:mini", Login: "judy@example.com", Host: "mini"})
	m := kitDaemon(t, tn, "laptop", tailnettest.Identity{NodeID: "nLAPTOP", NodeKey: "nodekey:laptop", Login: "ana@example.com", Host: "laptop"})
	if _, err := a.CreateProject(ctx, api.NewProjectRequest{Slug: "orchid"}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.AddArea(ctx, "orchid", api.AddAreaRequest{Name: "hosts"}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.ProjectTeam(ctx, "orchid"); err != nil {
		t.Fatal(err)
	}
	inv, err := a.Invite(ctx, api.InviteRequest{Project: "orchid", As: "ana", Role: api.RoleMember})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Join(ctx, api.JoinRequest{Code: inv.Code}); err != nil {
		t.Fatal(err)
	}
	mp := *m
	mp.Project = "orchid"
	req := api.InviteRequest{Project: "orchid", Area: "team", As: "bo", Role: api.RoleMember, AddArea: true}
	if _, err := mp.Invite(ctx, req); !api.IsCode(err, api.CodeForbidden) {
		t.Fatalf("a member's invite: %v, want forbidden", err)
	}
	if hasArea(t, a.GetProject, "orchid", "team") {
		t.Fatal("a member's refused invite added an area")
	}
	if _, err := a.Grant(ctx, api.GrantRequest{Identity: "ana", Role: "owner", Project: "orchid"}); err != nil {
		t.Fatal(err)
	}
	got, err := mp.Invite(ctx, req)
	if err != nil {
		t.Fatalf("an owner's invite from the joined machine: %v", err)
	}
	if got.Code == "" || got.Invite.As != "bo" || got.Invite.Area != "team" {
		t.Fatalf("invite: %+v", got.Invite)
	}
	if !hasArea(t, a.GetProject, "orchid", "team") {
		t.Fatal("the owner's invite did not complete the pool")
	}
	ls, err := a.Invites(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, x := range ls {
		if x.ID == got.Invite.ID {
			found = x.CreatedBy == "human:ana"
			if !found {
				t.Fatalf("made by %q, want human:ana", x.CreatedBy)
			}
		}
	}
	if !found {
		t.Fatalf("the authority has no invite %s: %+v", got.Invite.ID, ls)
	}
	// The authority's own socket still makes invites as before; a token on
	// that socket, even an owner's, never does.
	if _, err := a.Invite(ctx, api.InviteRequest{Project: "orchid", As: "cy", Role: api.RoleMember}); err != nil {
		t.Fatal(err)
	}
	tok, err := a.MintToken(ctx, api.MintTokenRequest{As: "agent", Project: "orchid", Role: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	at := *a
	at.Token = tok.Secret
	if _, err := at.Invite(ctx, api.InviteRequest{Project: "orchid", As: "di", Role: api.RoleMember}); !api.IsCode(err, api.CodeForbidden) {
		t.Fatalf("a minted owner token on the socket: %v, want forbidden", err)
	}
}

// An owner whose machine joined one area of the project (as a pool's
// members join its team area) makes an invite from that machine; the
// token's area does not narrow the owner's right, as for an owner's other
// rights (core.Deps.ProjectAdmin).
func TestInviteFromAreaMember(t *testing.T) {
	ctx := context.Background()
	tn := tailnettest.New()
	a := kitDaemon(t, tn, "mini", tailnettest.Identity{NodeID: "nMINI", NodeKey: "nodekey:mini", Login: "judy@example.com", Host: "mini"})
	m := kitDaemon(t, tn, "laptop", tailnettest.Identity{NodeID: "nLAPTOP", NodeKey: "nodekey:laptop", Login: "ana@example.com", Host: "laptop"})
	if _, err := a.CreateProject(ctx, api.NewProjectRequest{Slug: "orchid"}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.AddArea(ctx, "orchid", api.AddAreaRequest{Name: "team"}); err != nil {
		t.Fatal(err)
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
	req := api.InviteRequest{Project: "orchid", Area: "team", As: "bo", Role: api.RoleMember}
	if _, err := mp.Invite(ctx, req); !api.IsCode(err, api.CodeForbidden) && !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("an area member's invite: %v, want refused", err)
	}
	if _, err := a.Grant(ctx, api.GrantRequest{Identity: "ana", Role: "owner", Project: "orchid"}); err != nil {
		t.Fatal(err)
	}
	if _, err := mp.Invite(ctx, req); err != nil {
		t.Fatalf("an owner's invite from a machine that joined an area: %v", err)
	}
}
