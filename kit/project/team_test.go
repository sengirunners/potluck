package project

import (
	"slices"
	"testing"

	"github.com/sengirunners/potluck/kit/api"
)

// A failed registry UPDATE leaves the project solo in memory and in its
// pool too: SetTeam writes the row before it switches the pool.
func TestSetTeamFailedUpdateKeepsPool(t *testing.T) {
	f := newFixture(t)
	if _, err := f.st.Registry().ExecContext(f.ctx, `CREATE TRIGGER no_team BEFORE UPDATE OF team ON projects
		BEGIN SELECT RAISE(ABORT, 'refused'); END`); err != nil {
		t.Fatal(err)
	}
	if err := f.reg.SetTeam(f.ctx, f.orchid.ID, true); err == nil {
		t.Fatal("SetTeam succeeded through a refused UPDATE")
	}
	if f.reg.IsTeam(f.orchid.ID) || f.st.IsTeam(f.orchid.ID) {
		t.Fatalf("after a failed UPDATE: registry team=%v, pool team=%v", f.reg.IsTeam(f.orchid.ID), f.st.IsTeam(f.orchid.ID))
	}
	if _, err := f.st.Registry().ExecContext(f.ctx, `DROP TRIGGER no_team`); err != nil {
		t.Fatal(err)
	}
	if err := f.reg.SetTeam(f.ctx, f.orchid.ID, true); err != nil || !f.reg.IsTeam(f.orchid.ID) || !f.st.IsTeam(f.orchid.ID) {
		t.Fatalf("SetTeam: %v", err)
	}
}

// An invite's operator class (SPEC 8, zm-eakh.12.23.13) goes onto the token
// its redemption mints, and the redeemer cannot loosen it afterwards: a
// later join from the same node (a GitHub join, Admit) keeps the stricter
// class, and a class counts only while the grant its token is for holds.
func TestInviteClassTravelsAndHolds(t *testing.T) {
	f := newFixture(t)
	if err := f.reg.SetTeam(f.ctx, f.orchid.ID, true); err != nil {
		t.Fatal(err)
	}
	redeem := func(as, area, class, node, machine string) {
		t.Helper()
		inv, err := f.reg.CreateInvite(f.ctx, f.local, InviteInput{InviteRequest: api.InviteRequest{Project: "orchid", Area: area, As: as, Role: "guest", Class: class},
			Authority: "mini", Fingerprint: "0123456789abcdef"})
		if err != nil {
			t.Fatal(err)
		}
		jc, err := ParseJoinCode(inv.Code)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.reg.Redeem(f.ctx, RedeemInput{Secret: jc.Secret, Node: node, Machine: machine}); err != nil {
			t.Fatal(err)
		}
	}
	join := func(machine string) Joined {
		t.Helper()
		j, err := f.reg.MachineJoin(f.ctx, f.orchid.ID, machine)
		if err != nil {
			t.Fatal(err)
		}
		return j
	}

	redeem("bob", "sprout", api.MachinePublic, "nBOB", "bob-mac")
	if j := join("bob-mac"); j.Class != api.MachinePublic || j.Member {
		t.Fatalf("a public invite's machine: %+v", j)
	}
	// A stronger role from a later join keeps the invite's public class.
	if _, err := f.reg.Admit(f.ctx, AdmitInput{Node: "nBOB", Machine: "bob-mac", Identity: "bob", Project: f.orchid.ID, Role: "member", By: "test"}); err != nil {
		t.Fatal(err)
	}
	if j := join("bob-mac"); j.Class != api.MachinePublic || !j.Member {
		t.Fatalf("after a member join from the same node: %+v", j)
	}

	// A trusted host who also rents: revoking the hosts grant ends the
	// class, though the token stays live through the other grant.
	redeem("carol", "sprout", api.MachineTrusted, "nCAROL", "carol-mac")
	if _, err := f.reg.Grant(f.ctx, f.local, api.GrantRequest{Identity: "carol", Project: "orchid", Area: "billing", Role: "member"}); err != nil {
		t.Fatal(err)
	}
	if j := join("carol-mac"); j.Class != api.MachineTrusted {
		t.Fatalf("a trusted invite's machine: %+v", j)
	}
	if ms, _ := f.reg.InvitedMachines(f.ctx, f.orchid.ID); !slices.Contains(ms, "carol-mac") {
		t.Fatalf("invited machines %v", ms)
	}
	if _, err := f.reg.Revoke(f.ctx, f.local, api.GrantRequest{Identity: "carol", Project: "orchid", Area: "sprout"}); err != nil {
		t.Fatal(err)
	}
	if j := join("carol-mac"); len(j.Nodes) != 1 || j.Class != "" {
		t.Fatalf("after its hosts grant is revoked: %+v", j)
	}
	if ms, _ := f.reg.InvitedMachines(f.ctx, f.orchid.ID); slices.Contains(ms, "carol-mac") {
		t.Fatalf("invited machines after the revoke %v", ms)
	}
	// Nor does a later join from that node bring the ended class back.
	if _, err := f.reg.Admit(f.ctx, AdmitInput{Node: "nCAROL", Machine: "carol-mac", Identity: "carol", Project: f.orchid.ID, Area: "billing", Role: "guest", By: "test"}); err != nil {
		t.Fatal(err)
	}
	if j := join("carol-mac"); j.Class != "" || j.Member {
		t.Fatalf("after a later join: %+v", j)
	}

	// Losing a grant never makes a machine more trusted: a member invite
	// with class public, the member grant revoked and a guest grant given,
	// stays public and is no member's.
	inv, err := f.reg.CreateInvite(f.ctx, f.local, InviteInput{InviteRequest: api.InviteRequest{Project: "orchid", As: "dan", Role: "member", Class: api.MachinePublic},
		Authority: "mini", Fingerprint: "0123456789abcdef"})
	if err != nil {
		t.Fatal(err)
	}
	jc, _ := ParseJoinCode(inv.Code)
	if _, err := f.reg.Redeem(f.ctx, RedeemInput{Secret: jc.Secret, Node: "nDAN", Machine: "dan-mac"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.reg.Revoke(f.ctx, f.local, api.GrantRequest{Identity: "dan", Project: "orchid"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.reg.Grant(f.ctx, f.local, api.GrantRequest{Identity: "dan", Project: "orchid", Area: "sprout", Role: "guest"}); err != nil {
		t.Fatal(err)
	}
	if j := join("dan-mac"); len(j.Nodes) != 1 || j.Class != api.MachinePublic || j.Member {
		t.Fatalf("a demoted public member's machine: %+v", j)
	}
}

// zm-eakh.12.5.10.8: a machine's class follows the grant its token is for as
// it is now. Lowering a member to guest makes the machine a guest's at once
// (no rejoin), and a trusted invite's class ends with it; raising the grant
// back restores both, and an owner lowered to member stays a member's.
func TestLoweredGrantReclassesMachine(t *testing.T) {
	f := newFixture(t)
	if err := f.reg.SetTeam(f.ctx, f.orchid.ID, true); err != nil {
		t.Fatal(err)
	}
	redeem := func(as, role, class, node, machine string) {
		t.Helper()
		inv, err := f.reg.CreateInvite(f.ctx, f.local, InviteInput{InviteRequest: api.InviteRequest{Project: "orchid", As: as, Role: role, Class: class},
			Authority: "mini", Fingerprint: "0123456789abcdef"})
		if err != nil {
			t.Fatal(err)
		}
		jc, err := ParseJoinCode(inv.Code)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.reg.Redeem(f.ctx, RedeemInput{Secret: jc.Secret, Node: node, Machine: machine}); err != nil {
			t.Fatal(err)
		}
	}
	join := func(machine string) Joined {
		t.Helper()
		j, err := f.reg.MachineJoin(f.ctx, f.orchid.ID, machine)
		if err != nil {
			t.Fatal(err)
		}
		return j
	}
	grant := func(as, role string) {
		t.Helper()
		if _, err := f.reg.Grant(f.ctx, f.local, api.GrantRequest{Identity: as, Project: "orchid", Role: role}); err != nil {
			t.Fatal(err)
		}
	}

	redeem("erin", "member", "", "nERIN", "erin-mac")
	if j := join("erin-mac"); !j.Member {
		t.Fatalf("a member's machine: %+v", j)
	}
	grant("erin", "guest")
	if j := join("erin-mac"); len(j.Nodes) != 1 || j.Member || j.Class != "" {
		t.Fatalf("after the member is lowered to guest: %+v; want a guest's machine, still joined", j)
	}
	grant("erin", "member")
	if j := join("erin-mac"); !j.Member {
		t.Fatalf("after the grant is raised back: %+v", j)
	}

	redeem("finn", "member", api.MachineTrusted, "nFINN", "finn-mac")
	if j := join("finn-mac"); !j.Member || j.Class != api.MachineTrusted {
		t.Fatalf("a trusted member invite's machine: %+v", j)
	}
	grant("finn", "guest")
	if j := join("finn-mac"); j.Member || j.Class != "" {
		t.Fatalf("a trusted invite's machine after its member is lowered to guest: %+v; want neither", j)
	}

	// A member raised to owner, then lowered to member again, keeps a
	// member's machine throughout.
	redeem("gus", "member", "", "nGUS", "gus-mac")
	grant("gus", "owner")
	grant("gus", "member")
	if j := join("gus-mac"); !j.Member {
		t.Fatalf("an owner lowered to member: %+v; want still a member's machine", j)
	}

	// A guest token stays a guest's whatever the grant becomes.
	redeem("hal", "guest", "", "nHAL", "hal-mac")
	grant("hal", "member")
	if j := join("hal-mac"); len(j.Nodes) != 1 || j.Member {
		t.Fatalf("a guest token's machine after a member grant: %+v", j)
	}

	// Joining again (a GitHub join) after the demotion carries over what
	// the old token counts for now, not its old role: still a guest's
	// machine, and the trusted class does not come back.
	ad, err := f.reg.Admit(f.ctx, AdmitInput{Node: "nFINN", Machine: "finn-mac", Identity: "finn", Project: f.orchid.ID, Role: "guest", By: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if ad.Cap.Role != "guest" {
		t.Fatalf("a demoted member joining again got role %q; want guest", ad.Cap.Role)
	}
	if j := join("finn-mac"); j.Member || j.Class != "" {
		t.Fatalf("a demoted trusted member's machine after joining again: %+v; want a guest's", j)
	}

	// A trusted member invite whose grant is raised to owner and lowered
	// back to member keeps its class: the grant still allows the token's
	// role.
	redeem("ida", "member", api.MachineTrusted, "nIDA", "ida-mac")
	grant("ida", "owner")
	grant("ida", "member")
	if j := join("ida-mac"); !j.Member || j.Class != api.MachineTrusted {
		t.Fatalf("a trusted member's machine after owner and back: %+v; want a member's, trusted", j)
	}
}
