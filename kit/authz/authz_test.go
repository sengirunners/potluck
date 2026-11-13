package authz

import (
	"strings"
	"testing"

	"github.com/sengirunners/potluck/kit/api"
)

const (
	orchid = "01ORCHID0000000000000000000"
	side   = "01SIDE00000000000000000000"
)

func TestRelationMatrix(t *testing.T) {
	cases := []struct {
		r    Relation
		a    Action
		want bool
	}{
		{Owner, Admin, true}, {Owner, Operate, true}, {Owner, Read, true},
		{Member, Admin, false}, {Member, Operate, true}, {Member, Write, true},
		{Guest, Operate, false}, {Guest, Write, true}, {Guest, Read, true},
		{None, Read, false},
	}
	for _, c := range cases {
		if got := c.r.Allows(c.a); got != c.want {
			t.Errorf("%q allows %s = %v, want %v", c.r, c.a, got, c.want)
		}
	}
}

// The plan's example: Judy owns all of orchid; Victor is a guest of orchid/sprout only.
func TestGuestOfOneArea(t *testing.T) {
	victor := FromTuples([]Tuple{{Subject: "V", Relation: Guest, Object: AreaObject(orchid, "sprout")}}, nil)
	judy := FromTuples([]Tuple{{Subject: "J", Relation: Owner, Object: ProjectObject(orchid)}}, nil)

	checks := []struct {
		s    Scope
		a    Action
		o    Object
		want bool
	}{
		{victor, Read, Object{orchid, "sprout"}, true},
		{victor, Write, Object{orchid, "sprout"}, true},
		{victor, Operate, Object{orchid, "sprout"}, false},
		{victor, Read, Object{orchid, "billing"}, false},
		{victor, Read, Object{orchid, ""}, false}, // area-less project objects
		{victor, Read, Object{side, ""}, false},
		{victor, Read, Object{api.ProjectNone, ""}, false},
		{judy, Admin, Object{orchid, "billing"}, true},
		{judy, Read, Object{side, ""}, false},
		{judy, Read, Object{"", ""}, false},
		{All(), Read, Object{"", ""}, true},
		{Empty(), Read, Object{orchid, ""}, false},
	}
	for i, c := range checks {
		if got := c.s.Can(c.a, c.o); got != c.want {
			t.Errorf("#%d %s %+v = %v, want %v", i, c.a, c.o, got, c.want)
		}
	}
	if all, ps := victor.Projects(); all || len(ps) != 1 || ps[0] != orchid {
		t.Errorf("projects %v %v", all, ps)
	}
	if whole, as := victor.Areas(orchid); whole || len(as) != 1 || as[0] != "sprout" {
		t.Errorf("areas %v %v", whole, as)
	}
	if victor.Role(orchid) != Guest || victor.SeesProject(side) {
		t.Error("role/sees")
	}
}

func TestCapNarrowsAndNeverWidens(t *testing.T) {
	tuples := []Tuple{
		{Relation: Owner, Object: ProjectObject(orchid)},
		{Relation: Owner, Object: ProjectObject(side)},
	}
	// A guest token for orchid/sprout from an owner: guest, sprout only.
	s := FromTuples(tuples, &Cap{Project: orchid, Area: "sprout", Role: Guest})
	if !s.Can(Write, Object{orchid, "sprout"}) || s.Can(Operate, Object{orchid, "sprout"}) ||
		s.Can(Read, Object{orchid, "billing"}) || s.Can(Read, Object{side, ""}) {
		t.Fatalf("cap did not narrow: %+v", s)
	}
	// A cap can never add what the tuples lack.
	g := FromTuples([]Tuple{{Relation: Guest, Object: AreaObject(orchid, "sprout")}}, &Cap{Role: Owner})
	if g.Can(Operate, Object{orchid, "sprout"}) || g.Can(Read, Object{orchid, "x"}) {
		t.Fatal("cap widened")
	}
}

func TestCheckHidesExistence(t *testing.T) {
	sub := &Subject{Scope: FromTuples([]Tuple{{Relation: Guest, Object: AreaObject(orchid, "sprout")}}, nil)}
	hidden := sub.Check(Read, Object{orchid, "billing"}, "mail", "01X")
	missing := api.NotFound("mail", "01X")
	if hidden.Error() != missing.Error() {
		t.Fatalf("hidden %q != missing %q", hidden, missing)
	}
	if err := sub.Check(Operate, Object{orchid, "sprout"}, "session", "m/12345678"); !api.IsCode(err, api.CodeForbidden) {
		t.Fatalf("visible but not allowed: %v", err)
	}
}

func TestSQL(t *testing.T) {
	s := FromTuples([]Tuple{
		{Relation: Member, Object: ProjectObject(side)},
		{Relation: Guest, Object: AreaObject(orchid, "sprout")},
		{Relation: Guest, Object: AreaObject(orchid, "api")},
	}, nil)
	where, args := s.SQL("project", "area", Read)
	if !strings.Contains(where, "area IN (?,?)") || len(args) != 4 {
		t.Fatalf("%s %v", where, args)
	}
	where, _ = s.SQL("project", "area", Operate)
	if where != "(project = ?)" {
		t.Fatalf("operate: %s", where)
	}
	if w, _ := Empty().SQL("p", "a", Read); w != "0=1" {
		t.Fatal(w)
	}
	if w, _ := All().SQL("p", "a", Read); w != "1=1" {
		t.Fatal(w)
	}
	if w, a := s.AreaSQL(orchid, "area", Read); w != "area IN (?,?)" || len(a) != 2 {
		t.Fatal(w, a)
	}
}

func TestTokenSecret(t *testing.T) {
	s := NewTokenSecret()
	if !LooksLikeToken(s) || HashToken(s) == HashToken(NewTokenSecret()) || len(HashToken(s)) != 64 {
		t.Fatalf("token %q", s)
	}
	if BearerToken("Bearer "+s) != s || BearerToken("Basic x") != "" || BearerToken(s) != "" {
		t.Fatal("bearer")
	}
}

func TestOnly(t *testing.T) {
	g := FromTuples([]Tuple{{Relation: Guest, Object: AreaObject(orchid, "sprout")}, {Relation: Owner, Object: ProjectObject(side)}}, nil)
	o := g.Only(orchid)
	if !o.Can(Read, Object{orchid, "sprout"}) || o.Can(Read, Object{side, ""}) || o.Machine() {
		t.Fatal("only orchid")
	}
	a := All().Only(side)
	if !a.Can(Admin, Object{side, "x"}) || a.Can(Read, Object{orchid, ""}) || a.Machine() || a.IsAll() {
		t.Fatal("all only side")
	}
	if n := All().Only(api.ProjectNone); !n.Machine() || n.Can(Read, Object{side, ""}) {
		t.Fatal("only none")
	}
}

func TestTokenPrefixesShape(t *testing.T) {
	s := NewTokenSecret() // rename step 2: mints potluck_
	if !strings.HasPrefix(s, "potluck_") || !LooksLikeToken(s) {
		t.Fatalf("minted %q", s)
	}
	if n := "zm_" + strings.TrimPrefix(s, "potluck_"); !LooksLikeToken(n) || HashToken(n) == HashToken(s) {
		t.Fatalf("zm_ form (a token minted before) %q", n)
	}
}
