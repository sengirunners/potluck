package project

import (
	"testing"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
)

// Only an owner of the whole project makes a member (and only a member) an
// inviter; a
// project-wide inviter invites guests into any area; any other role on the
// grant ends the permission, and the grant's removal takes it too.
func TestInviterGrant(t *testing.T) {
	f := newFixture(t)
	if err := f.reg.SetTeam(f.ctx, f.orchid.ID, true); err != nil {
		t.Fatal(err)
	}
	areaOwner, err := f.reg.Mint(f.ctx, f.local, api.MintTokenRequest{As: "olga", Project: "orchid", Area: "sprout", Role: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	olga, err := f.reg.FromToken(f.ctx, areaOwner.Secret, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.reg.Grant(f.ctx, olga, api.GrantRequest{Identity: "pat", Role: "inviter", Project: "orchid", Area: "sprout"}); !api.IsCode(err, api.CodeForbidden) {
		t.Fatalf("an area owner's inviter grant: %v, want forbidden", err)
	}
	if _, err := f.reg.Grant(f.ctx, f.local, api.GrantRequest{Identity: "nemo", Role: "inviter", Project: "orchid"}); !api.IsCode(err, api.CodeConflict) {
		t.Fatalf("an inviter grant to no member: %v, want conflict", err)
	}
	tok, err := f.reg.Mint(f.ctx, f.local, api.MintTokenRequest{As: "pat", Project: "orchid", Role: "member"})
	if err != nil {
		t.Fatal(err)
	}
	minted, err := f.reg.FromToken(f.ctx, tok.Secret, true)
	if err != nil {
		t.Fatal(err)
	}
	// pat as the network door judges a member's machine: by pat's own
	// grants, uncapped (teamlink's inviter).
	pat := func() *authz.Subject {
		tuples, err := f.reg.Tuples(f.ctx, minted.Identity.ID)
		if err != nil {
			t.Fatal(err)
		}
		s := *minted
		s.Scope, s.Cap = authz.FromTuples(tuples, nil), nil
		return &s
	}
	inviteAs := func(sub *authz.Subject, as, area string) error {
		_, err := f.reg.CreateInvite(f.ctx, sub, InviteInput{InviteRequest: api.InviteRequest{Project: "orchid", Area: area, As: as, Role: "guest"},
			Authority: "mini", Fingerprint: "0123456789abcdef"})
		return err
	}
	invite := func(as, area string) error { return inviteAs(pat(), as, area) }
	if err := invite("quinn", "sprout"); !api.IsCode(err, api.CodeForbidden) {
		t.Fatalf("a member's guest invite: %v, want forbidden", err)
	}
	g, err := f.reg.Grant(f.ctx, f.local, api.GrantRequest{Identity: "pat", Role: "inviter", Project: "orchid"})
	if err != nil || !g.Inviter || g.Role != "member" {
		t.Fatalf("inviter grant: %+v, %v", g, err)
	}
	if err := inviteAs(minted, "quinn", "sprout"); !api.IsCode(err, api.CodeForbidden) {
		t.Fatalf("an inviter's capped (minted) token's guest invite: %v, want forbidden", err)
	}
	for area, as := range map[string]string{"sprout": "quinn", "billing": "quill", "": "quince"} {
		if err := invite(as, area); err != nil {
			t.Fatalf("a project-wide inviter's guest invite into %q: %v", area, err)
		}
	}
	// Minting pat a member token keeps pat an inviter.
	if _, err := f.reg.Mint(f.ctx, f.local, api.MintTokenRequest{As: "pat", Project: "orchid", Role: "member"}); err != nil {
		t.Fatal(err)
	}
	if err := invite("quilla", "sprout"); err != nil {
		t.Fatalf("after a member token was minted: %v", err)
	}
	// A name the daemon keeps for its own identities is no guest's.
	for _, as := range []string{"zm", "zm-runners", "potluck-broker"} {
		if err := invite(as, "sprout"); !api.IsCode(err, api.CodeBadRequest) {
			t.Fatalf("an inviter's guest named %s: %v, want bad request", as, err)
		}
	}
	// The guest a code of pat's made stays a guest against the daemon's
	// own grants (a runner link's) and any join; an owner's grant raises it.
	inv, err := f.reg.CreateInvite(f.ctx, pat(), InviteInput{InviteRequest: api.InviteRequest{Project: "orchid", Area: "sprout", As: "robo", Role: "guest"},
		Authority: "mini", Fingerprint: "0123456789abcdef"})
	if err != nil {
		t.Fatal(err)
	}
	jc, err := ParseJoinCode(inv.Code)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.reg.Redeem(f.ctx, RedeemInput{Secret: jc.Secret, Node: "nSQUAT", Machine: "squat"}); err != nil {
		t.Fatal(err)
	}
	sys := authz.System("mini")
	if _, err := f.reg.Grant(f.ctx, sys, api.GrantRequest{Identity: "robo", Role: "member", Project: "orchid", Area: "sprout"}); !api.IsCode(err, api.CodeConflict) {
		t.Fatalf("the daemon's grant raising an inviter's guest: %v, want conflict", err)
	}
	if _, err := f.reg.Admit(f.ctx, AdmitInput{Node: "nOTHER", Machine: "other", Identity: "robo", Project: f.orchid.ID, Area: "sprout", Role: "member", By: "test"}); !api.IsCode(err, api.CodeConflict) {
		t.Fatalf("a join raising an inviter's guest: %v, want conflict", err)
	}
	if _, err := f.reg.Grant(f.ctx, f.local, api.GrantRequest{Identity: "robo", Role: "member", Project: "orchid", Area: "sprout"}); err != nil {
		t.Fatalf("an owner raising an inviter's guest: %v", err)
	}
	if _, err := f.reg.Grant(f.ctx, sys, api.GrantRequest{Identity: "robo", Role: "member", Project: "orchid", Area: "billing"}); err != nil {
		t.Fatalf("the daemon's grant once an owner raised them: %v", err)
	}
	// An area's owner mints a member token for that area's inviter: the
	// inviter stays one, though the area owner could not have made them so.
	if _, err := f.reg.Grant(f.ctx, f.local, api.GrantRequest{Identity: "sid", Role: "member", Project: "orchid", Area: "sprout"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.reg.Grant(f.ctx, f.local, api.GrantRequest{Identity: "sid", Role: "inviter", Project: "orchid", Area: "sprout"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.reg.Mint(f.ctx, olga, api.MintTokenRequest{As: "sid", Project: "orchid", Area: "sprout", Role: "member"}); err != nil {
		t.Fatalf("an area owner's member token for the area's inviter: %v", err)
	}
	gs, err := f.reg.Grants(f.ctx, f.local, "orchid")
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range gs {
		if g.Identity == "sid" && (!g.Inviter || g.Role != "member") {
			t.Fatalf("sid after the mint: %+v", g)
		}
	}
	// Lowered to guest and raised to member again: no inviter.
	for _, role := range []string{"guest", "member"} {
		if g, err := f.reg.Grant(f.ctx, f.local, api.GrantRequest{Identity: "pat", Role: role, Project: "orchid"}); err != nil || g.Inviter {
			t.Fatalf("grant %s: %+v, %v", role, g, err)
		}
	}
	if err := invite("rhea", "sprout"); !api.IsCode(err, api.CodeForbidden) {
		t.Fatalf("after the role changed: %v, want forbidden", err)
	}
	// Revoking the grant takes the permission with it.
	if _, err := f.reg.Grant(f.ctx, f.local, api.GrantRequest{Identity: "pat", Role: "inviter", Project: "orchid"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.reg.Revoke(f.ctx, f.local, api.GrantRequest{Identity: "pat", Project: "orchid"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.reg.Grant(f.ctx, f.local, api.GrantRequest{Identity: "pat", Role: "member", Project: "orchid"}); err != nil {
		t.Fatal(err)
	}
	if err := invite("rhea", "sprout"); !api.IsCode(err, api.CodeForbidden) {
		t.Fatalf("after a revoke and a plain member grant: %v, want forbidden", err)
	}
}
