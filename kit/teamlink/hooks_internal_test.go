package teamlink

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/project"
	"github.com/sengirunners/potluck/kit/store"
)

// hooksService is a teamlink Service over a fresh registry with project
// orchid (one guest, victor, in area sprout) and a joined team project
// "remote" (ULID 01REMOTE).
func hooksService(t *testing.T) (*Service, *core.Deps) {
	t.Helper()
	ctx := context.Background()
	home := t.TempDir()
	st, err := store.Open(ctx, home)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.AddMigrations(ctx, Migrations...); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cfg := &config.Config{Home: home, Human: "judy", Machine: "mini"}
	reg, err := project.Open(ctx, st, cfg)
	if err != nil {
		t.Fatal(err)
	}
	local := reg.Local("")
	if _, err := reg.Create(ctx, local, api.NewProjectRequest{Slug: "orchid"}); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.AddArea(ctx, local, "orchid", api.Area{Name: "sprout"}); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Mint(ctx, local, api.MintTokenRequest{As: "victor", Project: "orchid", Area: "sprout", Role: "guest"}); err != nil {
		t.Fatal(err)
	}
	d := &core.Deps{Config: cfg, Store: st, Projects: reg}
	s := &Service{d: d, remotes: map[string]*Remote{
		"01REMOTE": {R: api.Remote{Project: api.ProjectRef{ID: "01REMOTE", Slug: "remote"}, Authority: "auth", State: api.RemoteOK}},
	}}
	return s, d
}

// A session's own team project (TeamWhy's last rule) comes only from the
// session hooks: with none, or a nil Place, no session rule applies.
func TestTeamWhySessionRuleNeedsHooks(t *testing.T) {
	s, d := hooksService(t)
	ctx := core.WithSubject(context.Background(), d.Projects.Local("").ForSession("mini/abcd1234"))
	if x, _, why := s.TeamWhy(ctx, ""); x != nil {
		t.Fatalf("no hooks: %s by %s", x.Slug(), why)
	}
	d.SetSessionHooks(func() *core.SessionHooks { return &core.SessionHooks{} })
	if x, _, why := s.TeamWhy(ctx, ""); x != nil {
		t.Fatalf("nil Place: %s by %s", x.Slug(), why)
	}
	d.SetSessionHooks(func() *core.SessionHooks {
		return &core.SessionHooks{Place: func(context.Context, string) (string, string, bool) { return "01REMOTE", "a1", true }}
	})
	if x, area, why := s.TeamWhy(ctx, ""); x == nil || x.Slug() != "remote" || area != "a1" || why != project.RuleSession {
		t.Fatalf("Place: %v %q %q", x, area, why)
	}
}

// The people list's machines and seen come only from the session hooks.
func TestPeopleWithoutSessionHooks(t *testing.T) {
	s, d := hooksService(t)
	ctx := context.Background()
	for name, h := range map[string]*core.SessionHooks{"no hooks": nil, "nil Seen": {}} {
		if h != nil {
			d.SetSessionHooks(func() *core.SessionHooks { return h })
		}
		out, err := s.people(ctx, d.Projects.Local(""), authz.All())
		if err != nil || len(out.People) != 2 || out.People[1].Name != "victor" || len(out.People[1].Machines) != 0 || out.People[1].Seen != nil {
			t.Fatalf("%s: %+v %v", name, out, err)
		}
	}
	at := time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)
	d.SetSessionHooks(func() *core.SessionHooks {
		return &core.SessionHooks{Seen: func(context.Context, authz.Scope) ([]core.SessionSeen, error) {
			return []core.SessionSeen{{Person: "victor", Machine: "m2", Seen: at}, {Person: "nobody", Machine: "m3", Seen: at}}, nil
		}}
	})
	out, err := s.people(ctx, d.Projects.Local(""), authz.All())
	if err != nil || len(out.People) != 2 || len(out.People[1].Machines) != 1 || out.People[1].Machines[0] != "m2" || out.People[1].Seen == nil || !out.People[1].Seen.Equal(at) {
		t.Fatalf("Seen: %+v %v", out, err)
	}
}

// Two identities linked to one GitHub account are one person in the
// authority's own answer (zm-mpb3r.10): one row named by the least name,
// listing both names and each identity's project row, answers from the one
// that owns the project, presence from either name, and the caller's own
// person is flagged you whichever identity asks. An area guest sees no one
// answer.
func TestPeopleFoldsByAccount(t *testing.T) {
	s, d := hooksService(t)
	ctx := context.Background()
	local := d.Projects.Local("")
	for _, g := range []api.GrantRequest{
		{Identity: "ivo", Project: "orchid", Role: "owner"},
		{Identity: "bea", Project: "orchid", Role: "member"},
		{Identity: "bea", Project: "orchid", Area: "sprout", Role: "guest"},
	} {
		if _, err := d.Projects.Grant(ctx, local, g); err != nil {
			t.Fatal(err)
		}
	}
	ids := map[string]api.Identity{}
	all, err := d.Projects.Identities(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range all {
		ids[id.Name] = id
	}
	d.LinkedAccount = func(_ context.Context, identity string) (string, string) {
		if identity == ids["bea"].ID || identity == ids["ivo"].ID {
			return "github:7", "beagh"
		}
		return "", ""
	}
	at := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	d.SetSessionHooks(func() *core.SessionHooks {
		return &core.SessionHooks{Seen: func(context.Context, authz.Scope) ([]core.SessionSeen, error) {
			return []core.SessionSeen{{Person: "ivo", Machine: "box-2", Seen: at}, {Person: "bea", Machine: "beamac", Seen: at.Add(-time.Hour)}}, nil
		}}
	})
	bea := func(out api.PeopleList) api.Person {
		t.Helper()
		var rows []api.Person
		for _, p := range out.People {
			if p.ID == "github:7" {
				rows = append(rows, p)
			}
		}
		if len(rows) != 1 {
			t.Fatalf("bea's rows: %+v, want one", out.People)
		}
		return rows[0]
	}
	for _, as := range []string{"bea", "ivo"} {
		sub := &authz.Subject{Identity: ids[as], Address: "human:" + as, Scope: authz.All()}
		out, err := s.people(ctx, sub, authz.All())
		if err != nil {
			t.Fatal(err)
		}
		p := bea(out)
		if p.Name != "bea" || !slices.Equal(p.Names, []string{"bea", "ivo"}) || !p.You || p.GitHub != "beagh" {
			t.Fatalf("as %s: %+v", as, p)
		}
		if !slices.Equal(p.Machines, []string{"beamac", "box-2"}) || p.Seen == nil || !p.Seen.Equal(at) {
			t.Fatalf("as %s: presence %+v %v", as, p.Machines, p.Seen)
		}
		answers := map[string]bool{}
		for _, pp := range p.Projects {
			answers[pp.Name] = pp.Answers
		}
		if len(p.Projects) != 2 || !answers["ivo"] || answers["bea"] {
			t.Fatalf("as %s: projects %+v, want bea's and ivo's, ivo answering", as, p.Projects)
		}
		for _, o := range out.People {
			if o.ID != "github:7" && o.You {
				t.Fatalf("as %s: %s flagged you", as, o.Name)
			}
		}
	}
	// The machine owner asking: bea is not it, judy is, and judy answers.
	out, err := s.people(ctx, local, authz.All())
	if err != nil {
		t.Fatal(err)
	}
	if p := bea(out); p.You {
		t.Fatalf("judy asking: bea flagged you: %+v", p)
	}
	for _, o := range out.People {
		if o.Name == "judy" && (!o.You || len(o.Projects) != 1 || !o.Projects[0].Answers) {
			t.Fatalf("judy asking: judy's row %+v", o)
		}
	}
	// An area guest of sprout sees bea (their area grant) and no one answering.
	orchid, ok := d.Projects.Lookup("orchid")
	if !ok {
		t.Fatal("no orchid")
	}
	guest := authz.FromTuples([]authz.Tuple{{Subject: ids["victor"].ID, Relation: authz.Guest, Object: authz.AreaObject(orchid.ID, "sprout")}}, nil)
	out, err = s.people(ctx, &authz.Subject{Identity: ids["victor"], Scope: guest}, guest)
	if err != nil {
		t.Fatal(err)
	}
	p := bea(out)
	if !slices.Equal(p.Names, []string{"bea"}) || len(p.Projects) != 1 || p.Projects[0].Answers {
		t.Fatalf("an area guest: %+v", p)
	}
	for _, o := range out.People {
		if o.Name == "judy" || o.Name == "ivo" {
			t.Fatalf("an area guest learned %s", o.Name)
		}
	}
}

// A member's merge of several authorities folds by id: names union, you
// and answers carry over, two identities in one project stay two rows.
func TestMergePeopleFolds(t *testing.T) {
	rows := []api.Person{
		{ID: "github:7", Name: "ivo", Projects: []api.PersonProject{{Project: "P1", Identity: "I1", Name: "ivo", Answers: true}}},
		{ID: "github:7", Name: "bea", Names: []string{"bea", "bw"}, You: true, Projects: []api.PersonProject{{Project: "P1", Identity: "I2", Name: "bea"}, {Project: "P2", Identity: "I3", Name: "bw"}}},
		{ID: "github:7", Name: "ivo", Projects: []api.PersonProject{{Project: "P1", Identity: "I1", Name: "ivo"}}},
	}
	out := MergePeople(rows)
	if len(out) != 1 {
		t.Fatalf("merged: %+v", out)
	}
	p := out[0]
	if p.Name != "bea" || !p.You || !slices.Equal(p.Names, []string{"bea", "bw", "ivo"}) || len(p.Projects) != 3 || !p.Projects[0].Answers {
		t.Fatalf("merged: %+v", p)
	}
}

// With no session hooks (or nil Machines) machine numbers stay in memory
// and this machine reports only its build.
func TestMachineStoreWithoutSessionHooks(t *testing.T) {
	s, d := hooksService(t)
	ctx := context.Background()
	for name, h := range map[string]*core.SessionHooks{"no hooks": nil, "nil Machines": {}} {
		if h != nil {
			d.SetSessionHooks(func() *core.SessionHooks { return h })
		}
		if s.machineStore() != nil {
			t.Fatalf("%s: a machine store", name)
		}
		if st := s.localStats(ctx); st.MemoryTotalMB != -1 {
			t.Fatalf("%s: local stats %+v", name, st)
		}
		if rows, err := s.machineRows(ctx); err != nil || len(rows) != 0 {
			t.Fatalf("%s: rows %v %v", name, rows, err)
		}
	}
}
