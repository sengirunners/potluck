package owners_test

import (
	"context"
	"slices"
	"testing"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/owners"
	"github.com/sengirunners/potluck/kit/store"
)

// identity is the identity named name, granted role in project first when
// project is set, made when it does not exist.
func (x *ox) identity(t *testing.T, name, project, role string) api.Identity {
	t.Helper()
	ctx := context.Background()
	if project != "" {
		if _, err := x.owner.Grant(ctx, api.GrantRequest{Identity: name, Project: project, Role: role}); err != nil {
			t.Fatal(err)
		}
	}
	ids, err := x.owner.Identities(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		if id.Name == name {
			return id
		}
	}
	id, err := x.owner.AddIdentity(ctx, api.NewIdentityRequest{Name: name, Kind: api.IdentityHuman})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// A GitHub join of an account that owns several identities joins as the
// one it linked first, whichever that is.
func TestJoinPicksTheEarliestLink(t *testing.T) {
	x := newOX(t, "Iv1.fakeclient")
	ctx := context.Background()
	teamProject(t, x, "orchid")
	ana, _ := x.gh.AddUser("ana", "User")
	if _, err := x.owner.GrantGitHub(ctx, api.GitHubGrantRequest{Owner: "@ana", Role: "member", Project: "orchid"}); err != nil {
		t.Fatal(err)
	}
	acc := owners.Account{ID: "github:" + jsonNum(ana.ID), Login: "ana", Kind: owners.KindUser}
	for _, name := range []string{"carl", "dave"} {
		if err := owners.LinkNow(x.svc, ctx, x.identity(t, name, "", ""), acc); err != nil {
			t.Fatalf("linking %s: %v", name, err)
		}
	}
	db := x.dm.Deps.Store.Registry()
	order := func(carl, dave int) {
		t.Helper()
		for name, at := range map[string]int{"carl": carl, "dave": dave} {
			if _, err := db.Exec(`UPDATE owner_links SET linked = ? WHERE identity = (SELECT id FROM identities WHERE name = ?)`, at, name); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, c := range []struct {
		carl, dave int
		want       string
	}{{200, 100, "dave"}, {100, 200, "carl"}} {
		order(c.carl, c.dave)
		x.gh.Approver(ana.ID, 0)
		j := mustJoin(t, carlNode(x), "orchid", "host-b")
		if j.Identity != c.want {
			t.Fatalf("carl linked at %d, dave at %d: joined as %s, want %s", c.carl, c.dave, j.Identity, c.want)
		}
	}
}

// An owner's removal or lowering of one identity reaches the other
// identities linked to the same account (lowerSameAccount), never the
// machine owner's own.
func TestCeilingAcrossFoldedIdentities(t *testing.T) {
	x := newOX(t, "Iv1.fakeclient")
	ctx := context.Background()
	teamProject(t, x, "orchid")
	teamProject(t, x, "lily")
	ana, _ := x.gh.AddUser("ana", "User")
	acc := owners.Account{ID: "github:" + jsonNum(ana.ID), Login: "ana", Kind: owners.KindUser}
	for _, name := range []string{"carl", "dave"} {
		for _, p := range []string{"orchid", "lily"} {
			x.identity(t, name, p, "member")
		}
	}
	for _, id := range []api.Identity{x.identity(t, "carl", "", ""), x.identity(t, "dave", "", ""), x.dm.Deps.Projects.LocalIdentity()} {
		if err := owners.LinkNow(x.svc, ctx, id, acc); err != nil {
			t.Fatalf("linking %s: %v", id.Name, err)
		}
	}
	roles := func(project string) map[string]string {
		t.Helper()
		gs, err := x.owner.Grants(ctx, project)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]string{}
		for _, g := range gs {
			out[g.Identity] = g.Role
		}
		return out
	}
	if _, err := x.owner.Revoke(ctx, api.GrantRequest{Identity: "carl", Project: "orchid"}); err != nil {
		t.Fatal(err)
	}
	if r := roles("orchid"); r["dave"] != "" || r["carl"] != "" || r["judy"] != "owner" {
		t.Fatalf("orchid after carl's removal: %v; want dave gone, judy kept", r)
	}
	if _, err := x.owner.Grant(ctx, api.GrantRequest{Identity: "dave", Project: "lily", Role: "guest"}); err != nil {
		t.Fatal(err)
	}
	if r := roles("lily"); r["carl"] != "guest" || r["dave"] != "guest" || r["judy"] != "owner" {
		t.Fatalf("lily after dave's lowering: %v; want carl guest, judy kept", r)
	}
}

// An owner who is not the machine owner removing their own second identity
// keeps their own grant: the actor is never lowered with its siblings.
func TestRemovingYourOwnSecondIdentity(t *testing.T) {
	x := newOX(t, "Iv1.fakeclient")
	ctx := context.Background()
	teamProject(t, x, "orchid")
	ana, _ := x.gh.AddUser("ana", "User")
	acc := owners.Account{ID: "github:" + jsonNum(ana.ID), Login: "ana", Kind: owners.KindUser}
	for name, role := range map[string]string{"carl": "owner", "carl-laptop": "member"} {
		if err := owners.LinkNow(x.svc, ctx, x.identity(t, name, "orchid", role), acc); err != nil {
			t.Fatal(err)
		}
	}
	mint, err := x.owner.MintToken(ctx, api.MintTokenRequest{As: "carl", Role: "owner", Project: "orchid"})
	if err != nil {
		t.Fatal(err)
	}
	carl := client.WithHandler(x.dm.SocketHandler())
	carl.Token = mint.Secret
	g, err := carl.Revoke(ctx, api.GrantRequest{Identity: "carl-laptop", Project: "orchid"})
	if err != nil || len(g.AlsoLowered) != 0 {
		t.Fatalf("carl removing carl-laptop: %+v, %v; want nothing else lowered", g, err)
	}
	gs, err := x.owner.Grants(ctx, "orchid")
	if err != nil {
		t.Fatal(err)
	}
	roles := map[string]string{}
	for _, g := range gs {
		roles[g.Identity] = g.Role
	}
	if roles["carl"] != "owner" || roles["carl-laptop"] != "" {
		t.Fatalf("orchid after carl removed carl-laptop: %v; want carl still owner", roles)
	}
}

// Migration owners/0005 rebuilds owner_links without UNIQUE(account),
// keeping every row, so a second identity of an account can link.
func TestMigrationManyPerAccount(t *testing.T) {
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
		`DROP TABLE owner_links`,
		`CREATE TABLE owner_links (identity TEXT PRIMARY KEY, account TEXT NOT NULL UNIQUE, login TEXT NOT NULL, linked INTEGER NOT NULL, seen INTEGER NOT NULL)`,
		`INSERT INTO owner_links (identity, account, login, linked, seen) VALUES ('01ANN', 'github:1', 'ann', 10, 11), ('01BOB', 'github:2', 'bob', 20, 21)`,
		`DELETE FROM schema_migrations WHERE name = 'owners/0005_many_per_account'`,
	} {
		if _, err := st.Registry().ExecContext(ctx, q); err != nil {
			t.Fatal(q, err)
		}
	}
	if _, err := st.Registry().ExecContext(ctx, `INSERT INTO owner_links VALUES ('01ANN2', 'github:1', 'ann', 30, 30)`); !store.IsUnique(err) {
		t.Fatalf("the old table took a second identity of an account: %v", err)
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
	db := st.Registry()
	if _, err := db.ExecContext(ctx, `INSERT INTO owner_links VALUES ('01ANN2', 'github:1', 'ann', 30, 30)`); err != nil {
		t.Fatalf("a second identity of an account after 0005: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO owner_links VALUES ('01ANN', 'github:3', 'x', 40, 40)`); !store.IsUnique(err) {
		t.Fatalf("an identity linked twice after 0005: %v", err)
	}
	rows, err := db.QueryContext(ctx, `SELECT identity || ' ' || account || ' ' || login || ' ' || linked || ' ' || seen FROM owner_links ORDER BY identity`)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for rows.Next() {
		var r string
		if err := rows.Scan(&r); err != nil {
			t.Fatal(err)
		}
		got = append(got, r)
	}
	rows.Close()
	if want := []string{"01ANN github:1 ann 10 11", "01ANN2 github:1 ann 30 30", "01BOB github:2 bob 20 21"}; !slices.Equal(got, want) {
		t.Fatalf("rows after 0005: %q, want %q", got, want)
	}
	var idx int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type = 'index' AND name = 'owner_links_account'`).Scan(&idx); err != nil || idx != 1 {
		t.Fatalf("owner_links_account index: %d, %v", idx, err)
	}
}
