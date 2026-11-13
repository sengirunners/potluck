package owners_test

import (
	"context"
	"strings"
	"testing"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/owners"
	"github.com/sengirunners/potluck/kit/store"
)

// An owner's removal or lowering holds against the GitHub account, not
// only the identity: unlinking and joining again as a new identity is
// refused (removed) or capped (lowered), naming the identity the owner
// removed; a grant to the new identity does not lift that, raising the
// removed identity again does (zm-eakh.12.23.27).
func TestUnlinkAndRejoinKeepsTheCeiling(t *testing.T) {
	x := newOX(t, "Iv1.fakeclient")
	ctx := context.Background()
	teamProject(t, x, "orchid")
	teamProject(t, x, "lily")
	carl, _ := x.gh.AddUser("carlexample", "User")
	for _, p := range []string{"orchid", "lily"} {
		if _, err := x.owner.GrantGitHub(ctx, api.GitHubGrantRequest{Owner: "@carlexample", Role: "member", Project: p}); err != nil {
			t.Fatal(err)
		}
	}
	n := carlNode(x)
	join := func(nd node, project, machine string) (*api.RedeemResponse, error) {
		t.Helper()
		x.gh.Approver(carl.ID, 0)
		out, err := nd.join(t, project, machine)
		return out.Join, err
	}
	for _, p := range []string{"orchid", "lily"} {
		if j, err := join(n, p, "host-b"); err != nil || j.Identity != "carlexample" || j.Cap.Role != "member" {
			t.Fatalf("carl's first join of %s: %+v, %v", p, j, err)
		}
	}
	// The owner removes carl from orchid and lowers him to guest in lily.
	if _, err := x.owner.Revoke(ctx, api.GrantRequest{Identity: "carlexample", Project: "orchid"}); err != nil {
		t.Fatal(err)
	}
	if _, err := x.owner.Grant(ctx, api.GrantRequest{Identity: "carlexample", Project: "lily", Role: "guest"}); err != nil {
		t.Fatal(err)
	}
	if _, err := x.owner.UnlinkGitHubIdentity(ctx, "carlexample", false); err != nil {
		t.Fatal(err)
	}
	// The same GitHub account joins again from another node: a new identity.
	n2 := node{x: x, rm: core.Remote{Login: "carl@example.com", Node: "host-c", NodeID: "nCARL2EXMPL", NodeKey: "nodekey:c2"}}
	if j, err := join(n2, "orchid", "host-c"); !api.IsCode(err, api.CodeForbidden) || !strings.Contains(err.Error(), "carlexample's grants") {
		t.Fatalf("removed carl unlinked and joined orchid again: %+v, %v; want refused, naming carlexample", j, err)
	}
	j, err := join(n2, "lily", "host-c")
	if err != nil || j.Identity != "carlexample-2" || j.Cap.Role != "guest" {
		t.Fatalf("lowered carl unlinked and joined lily again: %+v, %v; want carlexample-2 as guest", j, err)
	}
	// A grant to the new identity does not lift the old one's removal (the
	// account is the person); raising the identity the owner removed does.
	if _, err := x.owner.Grant(ctx, api.GrantRequest{Identity: "carlexample-2", Project: "orchid", Role: "member"}); err != nil {
		t.Fatal(err)
	}
	if j, err := join(n2, "orchid", "host-c"); !api.IsCode(err, api.CodeForbidden) {
		t.Fatalf("carlexample-2 joined orchid while carlexample's removal stands: %+v, %v; want refused", j, err)
	}
	if _, err := x.owner.Grant(ctx, api.GrantRequest{Identity: "carlexample", Project: "orchid", Role: "member"}); err != nil {
		t.Fatal(err)
	}
	if j, err := join(n2, "orchid", "host-c"); err != nil || j.Identity != "carlexample-2" || j.Cap.Role != "member" {
		t.Fatalf("carlexample-2 joined orchid after the owner raised carlexample back: %+v, %v; want member", j, err)
	}
}

// Unlinking before the removal does not help: the owner's removal of the
// first identity takes the grant the second already holds (the account is
// the person), names it, and its live token from its own node counts for
// nothing, so joining again from that node is refused as well as from a new
// one. Raising the first identity back does not lift the second one's
// removal (zm-eakh.12.23.31).
func TestUnlinkBeforeRemovalKeepsTheCeiling(t *testing.T) {
	x := newOX(t, "Iv1.fakeclient")
	ctx := context.Background()
	teamProject(t, x, "orchid")
	ann, _ := x.gh.AddUser("annexample", "User")
	if _, err := x.owner.GrantGitHub(ctx, api.GitHubGrantRequest{Owner: "@annexample", Role: "member", Project: "orchid"}); err != nil {
		t.Fatal(err)
	}
	join := func(id, machine string) (*api.RedeemResponse, error) {
		t.Helper()
		x.gh.Approver(ann.ID, 0)
		nd := node{x: x, rm: core.Remote{Login: "ann@example.com", Node: machine, NodeID: id, NodeKey: "nodekey:" + id}}
		out, err := nd.join(t, "orchid", machine)
		return out.Join, err
	}
	if j, err := join("nANN1", "ann-a"); err != nil || j.Identity != "annexample" {
		t.Fatalf("ann's first join: %+v, %v", j, err)
	}
	if _, err := x.owner.UnlinkGitHubIdentity(ctx, "annexample", false); err != nil {
		t.Fatal(err)
	}
	if j, err := join("nANN2", "ann-b"); err != nil || j.Identity != "annexample-2" || j.Cap.Role != "member" {
		t.Fatalf("ann's join after the unlink: %+v, %v; want annexample-2 as member", j, err)
	}
	// The owner removes the old identity: annexample-2's grant goes with it.
	g, err := x.owner.Revoke(ctx, api.GrantRequest{Identity: "annexample", Project: "orchid"})
	if err != nil || len(g.AlsoLowered) != 1 || g.AlsoLowered[0] != "annexample-2" {
		t.Fatalf("removing annexample: %+v, %v; want annexample-2 named as removed too", g, err)
	}
	grants, err := x.owner.Grants(ctx, "orchid")
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range grants {
		if strings.HasPrefix(g.Identity, "annexample") {
			t.Fatalf("after annexample's removal %s still holds %s", g.Identity, g.Role)
		}
	}
	if j, err := join("nANN2", "ann-b"); !api.IsCode(err, api.CodeForbidden) {
		t.Fatalf("annexample-2 joined again from its own node after annexample's removal: %+v, %v; want refused", j, err)
	}
	if j, err := join("nANN3", "ann-c"); !api.IsCode(err, api.CodeForbidden) {
		t.Fatalf("annexample-2 joined after annexample's removal: %+v, %v; want refused", j, err)
	}
	// The owner raises annexample back; then annexample-2 is unlinked and
	// the account joins as a third identity: annexample-2's removal holds.
	if _, err := x.owner.Grant(ctx, api.GrantRequest{Identity: "annexample", Project: "orchid", Role: "member"}); err != nil {
		t.Fatal(err)
	}
	if _, err := x.owner.UnlinkGitHubIdentity(ctx, "annexample-2", false); err != nil {
		t.Fatal(err)
	}
	if j, err := join("nANN4", "ann-d"); !api.IsCode(err, api.CodeForbidden) {
		t.Fatalf("the account joined again after annexample-2's removal: %+v, %v; want refused", j, err)
	}
}

// Migration project/0007 records every link made before links were
// recorded in ceiling_accounts, so an owner's removal reaches them too.
func TestMigrationRecordsOldLinks(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	st, err := store.Open(ctx, home)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.AddMigrations(ctx, owners.Migrations...); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO owner_links (identity, account, login, linked, seen) VALUES ('01ANN', 'github:1', 'ann', 1, 1)`,
		`DELETE FROM ceiling_accounts`,
		`DELETE FROM schema_migrations WHERE name = 'project/0007_linked_accounts'`,
	} {
		if _, err := st.Registry().ExecContext(ctx, q); err != nil {
			t.Fatal(q, err)
		}
	}
	st.Close()
	st, err = store.Open(ctx, home)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.AddMigrations(ctx, owners.Migrations...); err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var n int
	if err := st.Registry().QueryRowContext(ctx, `SELECT count(*) FROM ceiling_accounts WHERE subject = '01ANN' AND account = 'github:1'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("ceiling_accounts rows for an old link after 0007: %d, %v; want 1", n, err)
	}
}
