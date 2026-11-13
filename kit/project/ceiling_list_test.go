package project

import (
	"database/sql"
	"slices"
	"testing"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/store"
)

func teamFixture(t *testing.T) *fixture {
	t.Helper()
	f := newFixture(t)
	if err := f.reg.SetTeam(f.ctx, f.orchid.ID, true); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *fixture) admit(who, node, area, account string) (Admitted, error) {
	return f.reg.Admit(f.ctx, AdmitInput{Node: node, Machine: node + "-mac", Identity: who, Project: f.orchid.ID, Area: area,
		Role: "member", By: "src", Account: account})
}

func (f *fixture) remember(t *testing.T, who, account string) {
	t.Helper()
	id, err := f.reg.Identity(f.ctx, who)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Tx(f.ctx, f.st.Registry(), func(tx *sql.Tx) error { return RememberAccount(f.ctx, tx, id.ID, account) }); err != nil {
		t.Fatal(err)
	}
}

// An area admin sees only the ceilings on its area, in the listing and on
// an account.
func TestAreaAdminSeesItsAreaCeilings(t *testing.T) {
	f := teamFixture(t)
	for _, area := range []string{"", "billing"} {
		if _, err := f.reg.Grant(f.ctx, f.local, api.GrantRequest{Identity: "ann", Project: "orchid", Area: area, Role: "member"}); err != nil {
			t.Fatal(err)
		}
	}
	f.remember(t, "ann", "github:1")
	for _, area := range []string{"", "billing"} {
		if _, err := f.reg.Revoke(f.ctx, f.local, api.GrantRequest{Identity: "ann", Project: "orchid", Area: area}); err != nil {
			t.Fatal(err)
		}
	}
	tok, err := f.reg.Mint(f.ctx, f.local, api.MintTokenRequest{As: "ada", Project: "orchid", Area: "billing", Role: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	ada, err := f.reg.FromToken(f.ctx, tok.Secret, true)
	if err != nil {
		t.Fatal(err)
	}
	if cs, err := f.reg.Ceilings(f.ctx, f.local, "orchid"); err != nil || len(cs) != 2 {
		t.Fatalf("the owner's ceilings: %+v, %v; want ann's two", cs, err)
	}
	cs, err := f.reg.Ceilings(f.ctx, ada, "orchid")
	if err != nil || len(cs) != 1 || cs[0].Area != "billing" || cs[0].Accounts != nil {
		t.Fatalf("billing admin's ceilings: %+v, %v; want only billing's, without accounts", cs, err)
	}
	cs, err = f.reg.AccountCeilings(f.ctx, ada, "github:1", f.orchid.ID)
	if err != nil || len(cs) != 1 || cs[0].Area != "billing" {
		t.Fatalf("billing admin's ceilings on ann's account: %+v, %v; want only billing's", cs, err)
	}
}

// The grants listing's ceilings show each lowering and removal, by whom
// and for which accounts (zm-eakh.12.23.28, zm-eakh.12.23.23).
func TestCeilingsListed(t *testing.T) {
	f := teamFixture(t)
	if _, err := f.admit("ann", "nann", "", "github:1"); err != nil {
		t.Fatal(err)
	}
	f.remember(t, "ann", "github:1")
	if _, err := f.reg.Revoke(f.ctx, f.local, api.GrantRequest{Identity: "ann", Project: "orchid"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.reg.Grant(f.ctx, f.local, api.GrantRequest{Identity: "bob", Project: "orchid", Role: "owner"}); err != nil {
		t.Fatal(err)
	}
	f.remember(t, "bob", "github:2")
	if _, err := f.reg.Revoke(f.ctx, f.local, api.GrantRequest{Identity: "bob", Project: "orchid"}); err != nil {
		t.Fatal(err)
	}
	cs, err := f.reg.Ceilings(f.ctx, f.local, "orchid")
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 2 || cs[0].Identity != "ann" || cs[0].Role != "" || cs[0].LoweredFrom != "member" || cs[0].SetBy != f.local.Address ||
		!slices.Equal(cs[0].Accounts, []string{"github:1"}) || cs[0].Project != "orchid" || cs[1].Identity != "bob" {
		t.Fatalf("ceilings: %+v; want ann's and bob's removals", cs)
	}

	if cs, err := f.reg.AccountCeilings(f.ctx, f.local, "github:2", f.orchid.ID); err != nil || len(cs) != 1 || cs[0].Identity != "bob" || cs[0].LoweredFrom != "owner" {
		t.Fatalf("github:2's ceilings: %+v, %v; want bob's removal only", cs, err)
	}
	if _, err := f.reg.AccountCeilings(f.ctx, f.local, "", f.orchid.ID); err == nil {
		t.Fatal("AccountCeilings answered for no account")
	}
	// A grant to the identity at what it was lowered from ends the ceiling.
	if _, err := f.reg.Grant(f.ctx, f.local, api.GrantRequest{Identity: "bob", Project: "orchid", Role: "owner"}); err != nil {
		t.Fatal(err)
	}
	if cs, _ := f.reg.Ceilings(f.ctx, f.local, "orchid"); len(cs) != 1 || cs[0].Identity != "ann" {
		t.Fatalf("ceilings after bob's re-grant: %+v; want ann's only", cs)
	}
}
