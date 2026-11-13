package project

import (
	"testing"

	"github.com/sengirunners/potluck/kit/api"
)

// An owner's demotion holds over a GitHub join: the GitHub mapping still
// saying member does not give member back (zm-eakh.12.5.10.14), while the
// same source may still raise what it gave itself.
func TestAdmitKeepsAnOwnersDemotion(t *testing.T) {
	f := newFixture(t)
	if err := f.reg.SetTeam(f.ctx, f.orchid.ID, true); err != nil {
		t.Fatal(err)
	}
	const src = "github:1 grant:github:1:"
	admit := func(who, node, machine, role string) Admitted {
		t.Helper()
		ad, err := f.reg.Admit(f.ctx, AdmitInput{Node: node, Machine: machine, Identity: who, Project: f.orchid.ID, Role: role, By: src})
		if err != nil {
			t.Fatal(err)
		}
		return ad
	}
	held := func(who string) string {
		t.Helper()
		gs, err := f.reg.Grants(f.ctx, f.local, "orchid")
		if err != nil {
			t.Fatal(err)
		}
		for _, g := range gs {
			if g.Identity == who && g.Area == "" {
				return g.Role
			}
		}
		t.Fatalf("no grant for %s", who)
		return ""
	}
	machineOf := func(machine string) Joined {
		t.Helper()
		j, err := f.reg.MachineJoin(f.ctx, f.orchid.ID, machine)
		if err != nil {
			t.Fatal(err)
		}
		return j
	}

	admit("ann", "nANN", "ann-mac", "member")
	if _, err := f.reg.Grant(f.ctx, f.local, api.GrantRequest{Identity: "ann", Project: "orchid", Role: "guest"}); err != nil {
		t.Fatal(err)
	}
	// From the same node and from a new one.
	for _, n := range []struct{ node, machine string }{{"nANN", "ann-mac"}, {"nANN2", "ann-mini"}} {
		ad := admit("ann", n.node, n.machine, "member")
		if ad.Cap.Role != "guest" {
			t.Fatalf("a GitHub join from %s after the owner lowered ann to guest got role %q; want guest", n.node, ad.Cap.Role)
		}
		if g := held("ann"); g != "guest" {
			t.Fatalf("ann's grant after the join from %s: %q; want guest", n.node, g)
		}
		if j := machineOf(n.machine); j.Member {
			t.Fatalf("%s after the join: %+v; want a guest's machine", n.machine, j)
		}
	}
	// The owner raising ann again is the owner's to do.
	if _, err := f.reg.Grant(f.ctx, f.local, api.GrantRequest{Identity: "ann", Project: "orchid", Role: "member"}); err != nil {
		t.Fatal(err)
	}
	if ad := admit("ann", "nANN", "ann-mac", "member"); ad.Cap.Role != "member" || held("ann") != "member" {
		t.Fatalf("after the owner raised ann back: role %q, grant %q; want member", ad.Cap.Role, held("ann"))
	}

	// A join first as guest and then as member, with no owner setting the
	// grant in between, still raises it: from the same GitHub grant, and
	// from another one (an org grant's guest, then a user grant's member).
	admit("bea", "nBEA", "bea-mac", "guest")
	if ad := admit("bea", "nBEA", "bea-mac", "member"); ad.Cap.Role != "member" || held("bea") != "member" {
		t.Fatalf("bea joining again as member: role %q, grant %q; want member", ad.Cap.Role, held("bea"))
	}
	if _, err := f.reg.Admit(f.ctx, AdmitInput{Node: "nCAL", Machine: "cal-mac", Identity: "cal", Project: f.orchid.ID, Role: "guest",
		By: "github:3 grant:github:99:"}); err != nil {
		t.Fatal(err)
	}
	if ad := admit("cal", "nCAL", "cal-mac", "member"); ad.Cap.Role != "member" || held("cal") != "member" {
		t.Fatalf("cal joining by a second GitHub grant as member: role %q, grant %q; want member", ad.Cap.Role, held("cal"))
	}

	// A guest invite, then a GitHub grant's member: raised.
	inv, err := f.reg.CreateInvite(f.ctx, f.local, InviteInput{InviteRequest: api.InviteRequest{Project: "orchid", As: "dee", Role: "guest"},
		Authority: "mini", Fingerprint: "0123456789abcdef"})
	if err != nil {
		t.Fatal(err)
	}
	jc, err := ParseJoinCode(inv.Code)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.reg.Redeem(f.ctx, RedeemInput{Secret: jc.Secret, Node: "nDEE", Machine: "dee-mac"}); err != nil {
		t.Fatal(err)
	}
	if ad := admit("dee", "nDEE", "dee-mac", "member"); ad.Cap.Role != "member" || held("dee") != "member" {
		t.Fatalf("dee joining by GitHub after a guest invite: role %q, grant %q; want member", ad.Cap.Role, held("dee"))
	}

	// An owner adding someone by hand, or setting the role a grant already
	// has, is no demotion: a stronger GitHub grant still raises it.
	if _, err := f.reg.Grant(f.ctx, f.local, api.GrantRequest{Identity: "eve", Project: "orchid", Role: "member"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.reg.Grant(f.ctx, f.local, api.GrantRequest{Identity: "eve", Project: "orchid", Role: "member"}); err != nil {
		t.Fatal(err)
	}
	if ad := admit("eve", "nEVE", "eve-mac", "owner"); ad.Cap.Role != "owner" || held("eve") != "owner" {
		t.Fatalf("eve, added by hand as member, joining by a GitHub owner grant: role %q, grant %q; want owner", ad.Cap.Role, held("eve"))
	}
	if ad, err := f.reg.Admit(f.ctx, AdmitInput{Node: "nEVE2", Machine: "eve-mini", Identity: "eve", Project: f.orchid.ID, Area: "billing",
		Role: "owner", By: src}); err != nil || ad.Cap.Role != "owner" {
		t.Fatalf("eve's billing join as owner: %+v, %v; want owner", ad.Cap, err)
	}

	// A join scoped to an area does not get around a whole-project
	// demotion.
	admit("fay", "nFAY", "fay-mac", "member")
	if _, err := f.reg.Grant(f.ctx, f.local, api.GrantRequest{Identity: "fay", Project: "orchid", Role: "guest"}); err != nil {
		t.Fatal(err)
	}
	ad, err := f.reg.Admit(f.ctx, AdmitInput{Node: "nFAY2", Machine: "fay-mini", Identity: "fay", Project: f.orchid.ID, Area: "billing",
		Role: "member", By: src})
	if err != nil {
		t.Fatal(err)
	}
	if ad.Cap.Role != "guest" {
		t.Fatalf("fay's area join after the owner lowered her: role %q; want guest", ad.Cap.Role)
	}

	// A whole-project demotion never caps an area join below the area's
	// own grant, and a join never lowers what a node's live token still
	// counts for: gil holds a billing member token and grant (from a
	// GitHub join) when the owner lowers his whole-project grant.
	admit("gil", "nGIL", "gil-mac", "member")
	if ad, err := f.reg.Admit(f.ctx, AdmitInput{Node: "nGIL", Machine: "gil-mac", Identity: "gil", Project: f.orchid.ID, Area: "billing",
		Role: "member", By: src}); err != nil || ad.Cap.Role != "member" {
		t.Fatalf("gil's billing join: %+v, %v; want member", ad.Cap, err)
	}
	if _, err := f.reg.Grant(f.ctx, f.local, api.GrantRequest{Identity: "gil", Project: "orchid", Role: "guest"}); err != nil {
		t.Fatal(err)
	}
	if ad, err := f.reg.Admit(f.ctx, AdmitInput{Node: "nGIL3", Machine: "gil-air", Identity: "gil", Project: f.orchid.ID, Area: "billing",
		Role: "member", By: src}); err != nil || ad.Cap.Role != "member" || ad.Cap.Area != "billing" {
		t.Fatalf("gil's billing join from a new node, his billing grant member: %+v, %v; want member on billing", ad.Cap, err)
	}
	if ad := admit("gil", "nGIL", "gil-mac", "member"); ad.Cap.Role != "member" || ad.Cap.Area != "billing" {
		t.Fatalf("gil's whole-project join from a node holding a billing member token: %+v; want member on billing kept", ad.Cap)
	}
	if ad := admit("gil", "nGIL2", "gil-mini", "member"); ad.Cap.Role != "guest" {
		t.Fatalf("gil's whole-project join from a new node: %+v; want guest", ad.Cap)
	}

	// An owner lowering the area grant itself caps a join to that area.
	if _, err := f.reg.Grant(f.ctx, f.local, api.GrantRequest{Identity: "gil", Project: "orchid", Area: "billing", Role: "guest"}); err != nil {
		t.Fatal(err)
	}
	if ad, err := f.reg.Admit(f.ctx, AdmitInput{Node: "nGIL4", Machine: "gil-pro", Identity: "gil", Project: f.orchid.ID, Area: "billing",
		Role: "member", By: src}); err != nil || ad.Cap.Role != "guest" {
		t.Fatalf("gil's billing join after the owner lowered billing: %+v, %v; want guest", ad.Cap, err)
	}

	// hue holds only an area grant; the owner lowers it.
	hue := AdmitInput{Node: "nHUE", Machine: "hue-mac", Identity: "hue", Project: f.orchid.ID, Area: "billing", Role: "member", By: src}
	if _, err := f.reg.Admit(f.ctx, hue); err != nil {
		t.Fatal(err)
	}
	if _, err := f.reg.Grant(f.ctx, f.local, api.GrantRequest{Identity: "hue", Project: "orchid", Area: "billing", Role: "guest"}); err != nil {
		t.Fatal(err)
	}
	if ad, err := f.reg.Admit(f.ctx, hue); err != nil || ad.Cap.Role != "guest" {
		t.Fatalf("hue's billing join after the owner lowered billing: %+v, %v; want guest", ad.Cap, err)
	}

	// A whole-project join, which would cover billing, does not get
	// around hue's lowered billing grant.
	if ad := admit("hue", "nHUE2", "hue-mini", "member"); ad.Cap.Role != "guest" {
		t.Fatalf("hue's whole-project join after the owner lowered billing: %+v; want guest", ad.Cap)
	}

	// A lowered area grant never caps a whole-project join below the
	// whole-project grant itself: kim's billing grant lowered to guest
	// takes nothing from her whole-project member grant.
	admit("kim", "nKIM", "kim-mac", "member")
	if _, err := f.reg.Grant(f.ctx, f.local, api.GrantRequest{Identity: "kim", Project: "orchid", Area: "billing", Role: "member"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.reg.Grant(f.ctx, f.local, api.GrantRequest{Identity: "kim", Project: "orchid", Area: "billing", Role: "guest"}); err != nil {
		t.Fatal(err)
	}
	if ad := admit("kim", "nKIM2", "kim-mini", "member"); ad.Cap.Role != "member" {
		t.Fatalf("kim's whole-project join from a new node, her whole-project grant member: %+v; want member", ad.Cap)
	}
	if j := machineOf("kim-mini"); !j.Member {
		t.Fatalf("kim-mini after the join: %+v; want a member's machine", j)
	}
	// A GitHub owner join still gets no more than member over the
	// lowered billing grant, and never below member, her own grant.
	if ad := admit("kim", "nKIM3", "kim-air", "owner"); ad.Cap.Role != "member" {
		t.Fatalf("kim's whole-project owner join: %+v; want member", ad.Cap)
	}

	// An invite raising a lowered grant lifts the lowering.
	if _, err := f.reg.Grant(f.ctx, f.local, api.GrantRequest{Identity: "ann", Project: "orchid", Role: "guest"}); err != nil {
		t.Fatal(err)
	}
	inv, err = f.reg.CreateInvite(f.ctx, f.local, InviteInput{InviteRequest: api.InviteRequest{Project: "orchid", As: "ann", Role: "member"},
		Authority: "mini", Fingerprint: "0123456789abcdef"})
	if err != nil {
		t.Fatal(err)
	}
	if jc, err = ParseJoinCode(inv.Code); err != nil {
		t.Fatal(err)
	}
	if _, err := f.reg.Redeem(f.ctx, RedeemInput{Secret: jc.Secret, Node: "nANN3", Machine: "ann-air"}); err != nil {
		t.Fatal(err)
	}
	if ad := admit("ann", "nANN", "ann-mac", "owner"); ad.Cap.Role != "owner" {
		t.Fatalf("ann's GitHub owner join after an invite raised her back: %+v; want owner", ad.Cap)
	}

	// Revoking the GitHub grant that gave the demoted grant still removes
	// it: the owner's change does not adopt it.
	if gone, err := f.reg.RevokeAdmitted(f.ctx, identityID(t, f, "ann"), f.orchid.ID, "", src); err != nil || !gone {
		t.Fatalf("RevokeAdmitted after an owner's change: gone=%v err=%v; want gone", gone, err)
	}
}

// A rejoin after the grant was revoked takes the role the new join gives,
// not the old token's (zm-eakh.12.23.21). The revoke itself holds: the
// GitHub join is refused until an owner adds cy back (zm-eakh.12.23.22).
func TestAdmitAfterRevokedGrantTakesTheNewRole(t *testing.T) {
	f := newFixture(t)
	if err := f.reg.SetTeam(f.ctx, f.orchid.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := f.reg.Admit(f.ctx, AdmitInput{Node: "nCY", Machine: "cy-mac", Identity: "cy", Project: f.orchid.ID, Role: "member", By: "src"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.reg.Revoke(f.ctx, f.local, api.GrantRequest{Identity: "cy", Project: "orchid"}); err != nil {
		t.Fatal(err)
	}
	if ad, err := f.reg.Admit(f.ctx, AdmitInput{Node: "nCY", Machine: "cy-mac", Identity: "cy", Project: f.orchid.ID, Role: "member", By: "src"}); !api.IsCode(err, api.CodeForbidden) {
		t.Fatalf("a GitHub join after the owner removed cy: %+v, %v; want forbidden", ad.Cap, err)
	}
	if _, err := f.reg.Grant(f.ctx, f.local, api.GrantRequest{Identity: "cy", Project: "orchid", Role: "guest"}); err != nil {
		t.Fatal(err)
	}
	ad, err := f.reg.Admit(f.ctx, AdmitInput{Node: "nCY", Machine: "cy-mac", Identity: "cy", Project: f.orchid.ID, Role: "guest", By: "src"})
	if err != nil {
		t.Fatal(err)
	}
	if ad.Cap.Role != "guest" {
		t.Fatalf("a rejoin after the grant was revoked got role %q; want guest", ad.Cap.Role)
	}
	if j, err := f.reg.MachineJoin(f.ctx, f.orchid.ID, "cy-mac"); err != nil || j.Member {
		t.Fatalf("cy-mac after the rejoin: %+v, %v; want a guest's machine", j, err)
	}
}

// An owner's lowering outlives the grant row (zm-eakh.12.23.22): removing
// a member and adding them back as guest, or a GitHub grant's row going
// while another GitHub grant still covers them, leaves the GitHub join at
// guest.
func TestOwnersLoweringOutlivesTheGrant(t *testing.T) {
	f := newFixture(t)
	if err := f.reg.SetTeam(f.ctx, f.orchid.ID, true); err != nil {
		t.Fatal(err)
	}
	const user, org = "github:1 grant:github:1:", "github:1 grant:github:org:"
	admit := func(who, node, machine, role, by string) (Admitted, error) {
		return f.reg.Admit(f.ctx, AdmitInput{Node: node, Machine: machine, Identity: who, Project: f.orchid.ID, Role: role, By: by})
	}
	grant := func(who, role string) {
		t.Helper()
		if _, err := f.reg.Grant(f.ctx, f.local, api.GrantRequest{Identity: who, Project: "orchid", Role: role}); err != nil {
			t.Fatal(err)
		}
	}

	// Removed, then added back as guest.
	if _, err := admit("ann", "nANN", "ann-mac", "member", user); err != nil {
		t.Fatal(err)
	}
	if _, err := f.reg.Revoke(f.ctx, f.local, api.GrantRequest{Identity: "ann", Project: "orchid"}); err != nil {
		t.Fatal(err)
	}
	grant("ann", "guest")
	if ad, err := admit("ann", "nANN2", "ann-mini", "member", user); err != nil || ad.Cap.Role != "guest" {
		t.Fatalf("ann's GitHub join after the owner removed her and added her back as guest: %+v, %v; want guest", ad.Cap, err)
	}

	// Lowered, then the row's GitHub grant went; another still covers her.
	if _, err := admit("bea", "nBEA", "bea-mac", "member", user); err != nil {
		t.Fatal(err)
	}
	grant("bea", "guest")
	if gone, err := f.reg.RevokeAdmitted(f.ctx, identityID(t, f, "bea"), f.orchid.ID, "", user); err != nil || !gone {
		t.Fatalf("RevokeAdmitted: gone=%v err=%v; want gone", gone, err)
	}
	if ad, err := admit("bea", "nBEA2", "bea-mini", "member", org); err != nil || ad.Cap.Role != "guest" {
		t.Fatalf("bea's org join after the owner lowered her and her user grant went: %+v, %v; want guest", ad.Cap, err)
	}

	// Lowered, removed, and added back at the lowered role: still a lowering.
	if _, err := admit("cal", "nCAL", "cal-mac", "member", user); err != nil {
		t.Fatal(err)
	}
	grant("cal", "guest")
	if _, err := f.reg.Revoke(f.ctx, f.local, api.GrantRequest{Identity: "cal", Project: "orchid"}); err != nil {
		t.Fatal(err)
	}
	grant("cal", "guest")
	if ad, err := admit("cal", "nCAL2", "cal-mini", "member", user); err != nil || ad.Cap.Role != "guest" {
		t.Fatalf("cal's GitHub join after lowered, removed and re-added as guest: %+v, %v; want guest", ad.Cap, err)
	}

	// Removed from an area: a whole-project join, which would cover it, is
	// refused while she holds no whole-project grant.
	if _, err := f.reg.Admit(f.ctx, AdmitInput{Node: "nDEE", Machine: "dee-mac", Identity: "dee", Project: f.orchid.ID, Area: "billing",
		Role: "member", By: user}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.reg.Revoke(f.ctx, f.local, api.GrantRequest{Identity: "dee", Project: "orchid", Area: "billing"}); err != nil {
		t.Fatal(err)
	}
	if ad, err := admit("dee", "nDEE2", "dee-mini", "member", user); !api.IsCode(err, api.CodeForbidden) {
		t.Fatalf("dee's whole-project join after the owner removed her billing grant: %+v, %v; want forbidden", ad.Cap, err)
	}

	// The owner raising her back lifts the ceiling.
	grant("ann", "member")
	if ad, err := admit("ann", "nANN3", "ann-air", "owner", user); err != nil || ad.Cap.Role != "owner" {
		t.Fatalf("ann's GitHub owner join after the owner raised her back: %+v, %v; want owner", ad.Cap, err)
	}
}

// An invite made before an owner lowers the grant does not undo the
// lowering when redeemed (zm-eakh.12.23.24); one made after it does raise.
func TestStaleInviteDoesNotUndoALowering(t *testing.T) {
	f := newFixture(t)
	if err := f.reg.SetTeam(f.ctx, f.orchid.ID, true); err != nil {
		t.Fatal(err)
	}
	const src = "github:1 grant:github:1:"
	invite := func(who, area, role string) string { return inviteSecret(t, f, who, area, role) }
	if _, err := f.reg.Admit(f.ctx, AdmitInput{Node: "nANN", Machine: "ann-mac", Identity: "ann", Project: f.orchid.ID, Role: "member", By: src}); err != nil {
		t.Fatal(err)
	}
	stale, staleArea, guest := invite("ann", "", "member"), invite("ann", "billing", "member"), invite("ann", "", "guest")
	if _, err := f.reg.Grant(f.ctx, f.local, api.GrantRequest{Identity: "ann", Project: "orchid", Role: "guest"}); err != nil {
		t.Fatal(err)
	}
	for _, sec := range []string{stale, staleArea} {
		if out, err := f.reg.Redeem(f.ctx, RedeemInput{Secret: sec, Node: "nANN2", Machine: "ann-mini"}); err == nil {
			t.Fatalf("an invite made before the owner lowered ann redeemed: %+v; want refused", out.Cap)
		}
	}
	// An invite at or below the new role is left alone.
	if out, err := f.reg.Redeem(f.ctx, RedeemInput{Secret: guest, Node: "nANN2", Machine: "ann-mini"}); err != nil || out.Cap.Role != "guest" {
		t.Fatalf("ann's guest invite: %+v, %v; want guest", out.Cap, err)
	}
	if ad, err := f.reg.Admit(f.ctx, AdmitInput{Node: "nANN3", Machine: "ann-air", Identity: "ann", Project: f.orchid.ID, Role: "member", By: src}); err != nil || ad.Cap.Role != "guest" {
		t.Fatalf("ann's GitHub join after the stale invites: %+v, %v; want guest", ad.Cap, err)
	}
	fresh := invite("ann", "", "member")
	if out, err := f.reg.Redeem(f.ctx, RedeemInput{Secret: fresh, Node: "nANN4", Machine: "ann-pro"}); err != nil || out.Cap.Role != "member" {
		t.Fatalf("an invite made after the lowering: %+v, %v; want member", out.Cap, err)
	}
	if ad, err := f.reg.Admit(f.ctx, AdmitInput{Node: "nANN3", Machine: "ann-air", Identity: "ann", Project: f.orchid.ID, Role: "member", By: src}); err != nil || ad.Cap.Role != "member" {
		t.Fatalf("ann's GitHub join after a fresh invite raised her: %+v, %v; want member", ad.Cap, err)
	}
}

func identityID(t *testing.T, f *fixture, name string) string {
	t.Helper()
	id, err := f.reg.Identity(f.ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	return id.ID
}
