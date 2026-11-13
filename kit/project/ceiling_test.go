package project

import (
	"database/sql"
	"testing"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/store"
)

func inviteSecret(t *testing.T, f *fixture, who, area, role string) string {
	t.Helper()
	inv, err := f.reg.CreateInvite(f.ctx, f.local, InviteInput{InviteRequest: api.InviteRequest{Project: "orchid", Area: area, As: who, Role: role},
		Authority: "mini", Fingerprint: "0123456789abcdef"})
	if err != nil {
		t.Fatal(err)
	}
	jc, err := ParseJoinCode(inv.Code)
	if err != nil {
		t.Fatal(err)
	}
	return jc.Secret
}

// Invites an owner made after a lowering survive a later raise that keeps
// a ceiling: redeeming one invite, or a step-up grant, removes none.
func TestRaiseUnderACeilingKeepsLaterInvites(t *testing.T) {
	f := newFixture(t)
	if err := f.reg.SetTeam(f.ctx, f.orchid.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := f.reg.Admit(f.ctx, AdmitInput{Node: "nANN", Machine: "ann-mac", Identity: "ann", Project: f.orchid.ID, Role: "member", By: "src"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.reg.Revoke(f.ctx, f.local, api.GrantRequest{Identity: "ann", Project: "orchid"}); err != nil {
		t.Fatal(err)
	}
	guest, billing := inviteSecret(t, f, "ann", "", "guest"), inviteSecret(t, f, "ann", "billing", "member")
	if out, err := f.reg.Redeem(f.ctx, RedeemInput{Secret: guest, Node: "nANN2", Machine: "ann-mini"}); err != nil || out.Cap.Role != "guest" {
		t.Fatalf("ann's guest invite after the removal: %+v, %v; want guest", out.Cap, err)
	}
	if out, err := f.reg.Redeem(f.ctx, RedeemInput{Secret: billing, Node: "nANN3", Machine: "ann-air"}); err != nil || out.Cap.Role != "member" {
		t.Fatalf("ann's billing invite made after the removal, redeemed after her guest invite: %+v, %v; want member", out.Cap, err)
	}

	// bob, removed from owner, invited as member, then added back as
	// guest by hand (a raise that keeps a ceiling).
	if _, err := f.reg.Grant(f.ctx, f.local, api.GrantRequest{Identity: "bob", Project: "orchid", Role: "owner"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.reg.Revoke(f.ctx, f.local, api.GrantRequest{Identity: "bob", Project: "orchid"}); err != nil {
		t.Fatal(err)
	}
	member := inviteSecret(t, f, "bob", "", "member")
	if _, err := f.reg.Grant(f.ctx, f.local, api.GrantRequest{Identity: "bob", Project: "orchid", Role: "guest"}); err != nil {
		t.Fatal(err)
	}
	if out, err := f.reg.Redeem(f.ctx, RedeemInput{Secret: member, Node: "nBOB", Machine: "bob-mac"}); err != nil || out.Cap.Role != "member" {
		t.Fatalf("bob's member invite made after his removal, after a guest grant: %+v, %v; want member", out.Cap, err)
	}
}

// Migration 0005 carries a lowering made under 0004 (grants.lowered_by)
// into grant_ceilings, removes the lowered identity's unredeemed invites,
// and drops the column.
func TestMigrationCarriesLowerings(t *testing.T) {
	f := newFixture(t)
	if err := f.reg.SetTeam(f.ctx, f.orchid.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := f.reg.Grant(f.ctx, f.local, api.GrantRequest{Identity: "ann", Project: "orchid", Role: "guest"}); err != nil {
		t.Fatal(err)
	}
	stale := inviteSecret(t, f, "ann", "", "member")
	db := f.st.Registry()
	// Back to the schema as 0004 left it, with ann's grant lowered.
	for _, q := range []string{
		`DELETE FROM schema_migrations WHERE name = 'project/0005_grant_ceilings'`,
		`DROP TABLE grant_ceilings`,
		`ALTER TABLE grants ADD COLUMN lowered_by TEXT NOT NULL DEFAULT ''`,
		`UPDATE grants SET lowered_by = 'judy' WHERE subject = (SELECT id FROM identities WHERE name = 'ann')`,
	} {
		if _, err := db.ExecContext(f.ctx, q); err != nil {
			t.Fatal(q, err)
		}
	}
	cfg := f.reg.cfg
	f.st.Close()
	st, err := store.Open(f.ctx, cfg.Home)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	reg, err := Open(f.ctx, st, cfg)
	if err != nil {
		t.Fatal(err)
	}
	var cols int
	if err := st.Registry().QueryRowContext(f.ctx, `SELECT count(*) FROM pragma_table_info('grants') WHERE name = 'lowered_by'`).Scan(&cols); err != nil || cols != 0 {
		t.Fatalf("grants.lowered_by after 0005: %d columns, %v; want dropped", cols, err)
	}
	if out, err := reg.Redeem(f.ctx, RedeemInput{Secret: stale, Node: "nANN", Machine: "ann-mac"}); err == nil {
		t.Fatalf("an invite of a lowered identity redeemed after 0005: %+v; want refused", out.Cap)
	}
	if ad, err := reg.Admit(f.ctx, AdmitInput{Node: "nANN", Machine: "ann-mac", Identity: "ann", Project: f.orchid.ID, Role: "member", By: "src"}); err != nil || ad.Cap.Role != "guest" {
		t.Fatalf("ann's GitHub join after 0005: %+v, %v; want guest", ad.Cap, err)
	}
}

// A ceiling holds the GitHub account an identity was before an unlink
// (RememberAccount): the account's join under another identity is refused
// or capped across areas as the old identity's would be, and a join by
// another account, or by none, is not (zm-eakh.12.23.27).
func TestCeilingFollowsTheAccount(t *testing.T) {
	f := newFixture(t)
	if err := f.reg.SetTeam(f.ctx, f.orchid.ID, true); err != nil {
		t.Fatal(err)
	}
	admit := func(who, node, area, account string) (Admitted, error) {
		return f.reg.Admit(f.ctx, AdmitInput{Node: node, Machine: node + "-mac", Identity: who, Project: f.orchid.ID, Area: area,
			Role: "member", By: "src", Account: account})
	}
	remember := func(who, account string) {
		t.Helper()
		id, err := f.reg.Identity(f.ctx, who)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Tx(f.ctx, f.st.Registry(), func(tx *sql.Tx) error { return RememberAccount(f.ctx, tx, id.ID, account) }); err != nil {
			t.Fatal(err)
		}
	}
	// ann is removed from the whole project; bob is lowered to guest on
	// billing.
	if _, err := admit("ann", "nann", "", "github:1"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.reg.Revoke(f.ctx, f.local, api.GrantRequest{Identity: "ann", Project: "orchid"}); err != nil {
		t.Fatal(err)
	}
	if _, err := admit("bob", "nbob", "billing", "github:2"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.reg.Grant(f.ctx, f.local, api.GrantRequest{Identity: "bob", Project: "orchid", Area: "billing", Role: "guest"}); err != nil {
		t.Fatal(err)
	}
	remember("ann", "github:1")
	remember("bob", "github:2")
	if out, err := admit("ann-2", "nann2", "sprout", "github:1"); !api.IsCode(err, api.CodeForbidden) {
		t.Fatalf("ann's account joined an area as a new identity after her removal: %+v, %v; want refused", out.Cap, err)
	}
	if out, err := admit("bob-2", "nbob2", "", "github:2"); err != nil || out.Cap.Role != "guest" {
		t.Fatalf("bob's account joined the whole project as a new identity after his billing lowering: %+v, %v; want guest", out.Cap, err)
	}
	if out, err := admit("bob-3", "nbob3", "sprout", "github:2"); err != nil || out.Cap.Role != "member" {
		t.Fatalf("bob's account joined another area: %+v, %v; want member (the lowering was on billing)", out.Cap, err)
	}
	if out, err := admit("cat", "ncat", "", "github:3"); err != nil || out.Cap.Role != "member" {
		t.Fatalf("another account: %+v, %v; want member", out.Cap, err)
	}
	if out, err := admit("ann-3", "nann3", "", ""); err != nil || out.Cap.Role != "member" {
		t.Fatalf("a join with no account: %+v, %v; want member", out.Cap, err)
	}
	// An identity's own grant is no floor under another identity's
	// removal: dan holds member, and the account he joins with was dan-old,
	// whom the owner removed (the account linked back to dan).
	if _, err := admit("dan", "ndan", "", "github:4"); err != nil {
		t.Fatal(err)
	}
	if _, err := admit("dan-old", "ndanold", "", "github:4"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.reg.Revoke(f.ctx, f.local, api.GrantRequest{Identity: "dan-old", Project: "orchid"}); err != nil {
		t.Fatal(err)
	}
	remember("dan-old", "github:4")
	if out, err := admit("dan", "ndan2", "", "github:4"); !api.IsCode(err, api.CodeForbidden) {
		t.Fatalf("dan joined with the account removed as dan-old: %+v, %v; want refused", out.Cap, err)
	}
	if err := store.Tx(f.ctx, f.st.Registry(), func(tx *sql.Tx) error { return RememberAccount(f.ctx, tx, "", "github:1") }); err == nil {
		t.Fatal("RememberAccount stored an empty identity")
	}
}

// An owner's lowering or removal of one identity reaches every other
// identity the same GitHub account is or was (zm-eakh.12.23.31): their
// grant on that object comes down with a ceiling, their re-join from the
// node they already hold is held to it, their invites past it go, and an
// identity of another account is untouched.
func TestLoweringReachesTheAccountsOtherIdentities(t *testing.T) {
	f := newFixture(t)
	if err := f.reg.SetTeam(f.ctx, f.orchid.ID, true); err != nil {
		t.Fatal(err)
	}
	admit := func(who, node, account string) (Admitted, error) {
		return f.reg.Admit(f.ctx, AdmitInput{Node: node, Machine: node + "-mac", Identity: who, Project: f.orchid.ID,
			Role: "member", By: "src", Account: account})
	}
	remember := func(who, account string) {
		t.Helper()
		id, err := f.reg.Identity(f.ctx, who)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Tx(f.ctx, f.st.Registry(), func(tx *sql.Tx) error { return RememberAccount(f.ctx, tx, id.ID, account) }); err != nil {
			t.Fatal(err)
		}
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
		return ""
	}
	// ann was github:1 and unlinked; ann-2 is github:1 now. cat is github:3.
	for _, j := range []struct{ who, node, account string }{{"ann", "nann", "github:1"}, {"ann-2", "nann2", "github:1"}, {"cat", "ncat", "github:3"}} {
		if _, err := admit(j.who, j.node, j.account); err != nil {
			t.Fatal(err)
		}
		remember(j.who, j.account)
	}
	member := inviteSecret(t, f, "ann-2", "", "member")
	g, err := f.reg.Grant(f.ctx, f.local, api.GrantRequest{Identity: "ann", Project: "orchid", Role: "guest"})
	if err != nil || len(g.AlsoLowered) != 1 || g.AlsoLowered[0] != "ann-2" {
		t.Fatalf("lowering ann: %+v, %v; want ann-2 named", g, err)
	}
	if r := held("ann-2"); r != "guest" {
		t.Fatalf("ann-2 after ann's lowering holds %q; want guest", r)
	}
	if out, err := admit("ann-2", "nann2", "github:1"); err != nil || out.Cap.Role != "guest" {
		t.Fatalf("ann-2 joined again from its node after ann's lowering: %+v, %v; want guest", out.Cap, err)
	}
	if out, err := f.reg.Redeem(f.ctx, RedeemInput{Secret: member, Node: "nann3", Machine: "nann3-mac"}); err == nil {
		t.Fatalf("ann-2's member invite made before ann's lowering redeemed: %+v; want refused", out.Cap)
	}
	if _, err := f.reg.Revoke(f.ctx, f.local, api.GrantRequest{Identity: "ann", Project: "orchid"}); err != nil {
		t.Fatal(err)
	}
	if r := held("ann-2"); r != "" {
		t.Fatalf("ann-2 after ann's removal holds %q; want nothing", r)
	}
	if out, err := admit("ann-2", "nann2", ""); !api.IsCode(err, api.CodeForbidden) {
		t.Fatalf("ann-2 joined again from its node after ann's removal: %+v, %v; want refused", out.Cap, err)
	}
	if r := held("cat"); r != "member" {
		t.Fatalf("cat (another account) holds %q after ann's removal; want member", r)
	}
	// Raising ann back is not a lowering: ann-2 stays removed.
	if g, err := f.reg.Grant(f.ctx, f.local, api.GrantRequest{Identity: "ann", Project: "orchid", Role: "member"}); err != nil || len(g.AlsoLowered) != 0 || held("ann-2") != "" {
		t.Fatalf("raising ann back: %+v, %v, ann-2 holds %q; want ann-2 untouched", g, err, held("ann-2"))
	}
}

// The reach of an owner's act on another identity of the same account
// covers the project's areas for a whole-project act (removing the whole
// project takes their area grants too), and only the area for an area's
// act: an area owner's removal never takes their whole-project grant
// (zm-eakh.12.23.31).
func TestLoweringReachesAcrossAreas(t *testing.T) {
	f := newFixture(t)
	if err := f.reg.SetTeam(f.ctx, f.orchid.ID, true); err != nil {
		t.Fatal(err)
	}
	link := func(who, node, area, account string) {
		t.Helper()
		if _, err := f.reg.Admit(f.ctx, AdmitInput{Node: node, Machine: node + "-mac", Identity: who, Project: f.orchid.ID, Area: area,
			Role: "member", By: "src", Account: account}); err != nil {
			t.Fatal(err)
		}
		id, err := f.reg.Identity(f.ctx, who)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Tx(f.ctx, f.st.Registry(), func(tx *sql.Tx) error { return RememberAccount(f.ctx, tx, id.ID, account) }); err != nil {
			t.Fatal(err)
		}
	}
	holds := func(who string) []string {
		t.Helper()
		gs, err := f.reg.Grants(f.ctx, f.local, "orchid")
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, g := range gs {
			if g.Identity == who {
				out = append(out, g.Area+":"+g.Role)
			}
		}
		return out
	}
	// ann (whole project) and ann-2 (billing) are github:1.
	link("ann", "nann", "", "github:1")
	link("ann-2", "nann2", "billing", "github:1")
	if g, err := f.reg.Revoke(f.ctx, f.local, api.GrantRequest{Identity: "ann", Project: "orchid"}); err != nil || len(g.AlsoLowered) != 1 {
		t.Fatalf("removing ann from orchid: %+v, %v; want ann-2 named", g, err)
	}
	if h := holds("ann-2"); len(h) != 0 {
		t.Fatalf("ann-2 after ann's whole-project removal holds %v; want nothing", h)
	}
	// bob (billing) and bob-2 (whole project) are github:2. eve owns only
	// billing: her removal of bob there takes nothing outside billing, so
	// bob-2's whole-project grant stays (she holds no admin on it, and
	// bob's own would stay too).
	link("bob", "nbob", "billing", "github:2")
	link("bob-2", "nbob2", "", "github:2")
	link("bob-3", "nbob3", "billing", "github:2")
	if _, err := f.reg.Grant(f.ctx, f.local, api.GrantRequest{Identity: "eve", Project: "orchid", Area: "billing", Role: "owner"}); err != nil {
		t.Fatal(err)
	}
	eveID, err := f.reg.Identity(f.ctx, "eve")
	if err != nil {
		t.Fatal(err)
	}
	eve, err := f.reg.IdentitySubject(f.ctx, eveID.ID)
	if err != nil {
		t.Fatal(err)
	}
	if g, err := f.reg.Revoke(f.ctx, eve, api.GrantRequest{Identity: "bob", Project: "orchid", Area: "billing"}); err != nil || len(g.AlsoLowered) != 1 || g.AlsoLowered[0] != "bob-3" {
		t.Fatalf("eve removing bob from billing: %+v, %v; want bob-3 named", g, err)
	}
	if h := holds("bob-2"); len(h) != 1 || h[0] != ":member" {
		t.Fatalf("bob-2 after an area owner's billing removal of bob holds %v; want its whole-project member", h)
	}
	if h := holds("bob-3"); len(h) != 0 {
		t.Fatalf("bob-3 after bob's billing removal holds %v; want nothing", h)
	}
}
