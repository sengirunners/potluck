package project

import (
	"testing"
	"time"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/store"
)

// teamInvite makes orchid a team project and returns a fresh join secret
// (normalized) for a member invite of as.
func teamInvite(t *testing.T, f *fixture, as string) string {
	t.Helper()
	if !f.reg.IsTeam(f.orchid.ID) {
		if err := f.reg.SetTeam(f.ctx, f.orchid.ID, true); err != nil {
			t.Fatal(err)
		}
	}
	inv, err := f.reg.CreateInvite(f.ctx, f.local, InviteInput{InviteRequest: api.InviteRequest{Project: "orchid", As: as, Role: "member"},
		Authority: "mini", Fingerprint: NodeFingerprint("nMINI")})
	if err != nil {
		t.Fatal(err)
	}
	c, err := ParseJoinCode(inv.Code)
	if err != nil {
		t.Fatal(err)
	}
	return c.Secret
}

// tokenID is the id of the node-bound token node holds.
func tokenID(t *testing.T, f *fixture, node string) string {
	t.Helper()
	var id string
	if err := f.st.Registry().QueryRowContext(f.ctx, `SELECT id FROM tokens WHERE node = ? AND revoked IS NULL`, node).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// A replica may never redeem under the authority's own machine name: its
// token could then answer as every session on the authority (Zm-As
// mini/<s8>). The refusal does not spend the code.
func TestRedeemRefusesAuthorityMachineName(t *testing.T) {
	f := newFixture(t)
	secret := teamInvite(t, f, "mallory")
	if _, err := f.reg.Redeem(f.ctx, RedeemInput{Secret: secret, Node: "nMAL", Machine: "mini"}); !api.IsCode(err, api.CodeConflict) {
		t.Fatalf("redeem as the authority's machine: %v", err)
	}
	if ps := f.reg.MachineProjects(f.ctx, "mini"); len(ps) != 0 {
		t.Fatalf("machine mini holds %v", ps)
	}
	out, err := f.reg.Redeem(f.ctx, RedeemInput{Secret: secret, Node: "nMAL", Machine: "mal"})
	if err != nil || out.Machine != "mal" {
		t.Fatalf("the same code under its own name: %+v %v", out, err)
	}
}

// A machine name is held by its node only while that node holds a live
// node-bound token: after zm leave (or zm token revoke) a reimaged box with
// a new node ID rejoins under the same name, and the old node may join
// under another.
func TestMachineNameFreedWhenNodeLeaves(t *testing.T) {
	f := newFixture(t)
	if _, err := f.reg.Redeem(f.ctx, RedeemInput{Secret: teamInvite(t, f, "farm"), Node: "nBOX1", Machine: "box1"}); err != nil {
		t.Fatal(err)
	}
	// While the first node's token is live, the name is taken.
	again := teamInvite(t, f, "farm")
	if _, err := f.reg.Redeem(f.ctx, RedeemInput{Secret: again, Node: "nBOX1b", Machine: "box1"}); !api.IsCode(err, api.CodeConflict) {
		t.Fatalf("taken name: %v", err)
	}
	// zm leave: the replica revokes its own token.
	sub := &authz.Subject{Node: "nBOX1", TokenID: tokenID(t, f, "nBOX1")}
	if err := f.reg.RevokeOwn(f.ctx, sub); err != nil {
		t.Fatal(err)
	}
	out, err := f.reg.Redeem(f.ctx, RedeemInput{Secret: again, Node: "nBOX1b", Machine: "box1"})
	if err != nil || out.Machine != "box1" {
		t.Fatalf("rejoin from a new node under the same name: %+v %v", out, err)
	}
	// zm token revoke frees it too; the old node joins under a new name.
	if _, err := f.reg.RevokeToken(f.ctx, tokenID(t, f, "nBOX1b")); err != nil {
		t.Fatal(err)
	}
	if _, err := f.reg.Redeem(f.ctx, RedeemInput{Secret: teamInvite(t, f, "farm"), Node: "nBOX1", Machine: "box9"}); err != nil {
		t.Fatalf("old node under a new name: %v", err)
	}
	if _, err := f.reg.Redeem(f.ctx, RedeemInput{Secret: teamInvite(t, f, "farm"), Node: "nBOX1b", Machine: "box9"}); !api.IsCode(err, api.CodeConflict) {
		t.Fatalf("box9 is live again: %v", err)
	}
}

// MachineProjects (Visible asks it per remote address) is cached until a
// node-bound token is redeemed or revoked.
func TestMachineProjectsCached(t *testing.T) {
	f := newFixture(t)
	if _, err := f.reg.Redeem(f.ctx, RedeemInput{Secret: teamInvite(t, f, "carl"), Node: "nCARL", Machine: "host-b"}); err != nil {
		t.Fatal(err)
	}
	before := machineQueries.Load()
	for i := 0; i < 50; i++ {
		if ps := f.reg.MachineProjects(f.ctx, "host-b"); len(ps) != 1 || ps[0] != f.orchid.ID {
			t.Fatalf("MachineProjects %v", ps)
		}
	}
	if n := machineQueries.Load() - before; n != 1 {
		t.Fatalf("%d queries for 50 lookups", n)
	}
	if _, err := f.reg.RevokeToken(f.ctx, tokenID(t, f, "nCARL")); err != nil {
		t.Fatal(err)
	}
	if ps := f.reg.MachineProjects(f.ctx, "host-b"); len(ps) != 0 {
		t.Fatalf("after revoke: %v", ps)
	}
	if _, err := f.reg.Redeem(f.ctx, RedeemInput{Secret: teamInvite(t, f, "carl"), Node: "nCARL", Machine: "host-b"}); err != nil {
		t.Fatal(err)
	}
	if ps := f.reg.MachineProjects(f.ctx, "host-b"); len(ps) != 1 {
		t.Fatalf("after a new redemption: %v", ps)
	}
}

// An expired node-bound token is not a live one: MachineProjects drops its
// project once it expires, cache or no cache, so its replica's presence rows
// end (replica left) instead of living on.
func TestMachineProjectsExpiry(t *testing.T) {
	f := newFixture(t)
	if _, err := f.reg.Redeem(f.ctx, RedeemInput{Secret: teamInvite(t, f, "carl"), Node: "nCARL", Machine: "host-b"}); err != nil {
		t.Fatal(err)
	}
	exp := time.Now().Add(400 * time.Millisecond)
	if _, err := f.reg.st.Registry().ExecContext(f.ctx, `UPDATE tokens SET expires = ? WHERE node = 'nCARL'`, store.Millis(exp)); err != nil {
		t.Fatal(err)
	}
	f.reg.forgetMachines()
	if ps := f.reg.MachineProjects(f.ctx, "host-b"); len(ps) != 1 {
		t.Fatalf("before expiry: %v", ps)
	}
	time.Sleep(time.Until(exp) + 50*time.Millisecond)
	if ps := f.reg.MachineProjects(f.ctx, "host-b"); len(ps) != 0 {
		t.Fatalf("after expiry (cached since before it): %v", ps)
	}
}
