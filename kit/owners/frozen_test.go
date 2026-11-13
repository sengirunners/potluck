package owners_test

import (
	"context"
	"testing"
	"time"

	"github.com/sengirunners/potluck/kit/api"
)

// GitHub no longer answering for an owner's id (suspended or deleted)
// freezes its names: they resolve read-only, and nothing is bound, renamed
// or transferred under them, until GitHub answers again.
func TestFrozenOwner(t *testing.T) {
	x := newOX(t, "")
	clk := x.clock()
	x.projects(t, "orchid", "side")
	ana, _ := x.gh.AddUser("ana", "User")
	me := x.person(t, "judy", ana)
	acme, _ := x.gh.AddUser("acme", "Organization")
	bind := func(project, owner, name string) error {
		_, err := x.bind(t, me, ana, project, owner, name, true)
		return err
	}
	if err := bind("orchid", "acme", "orchid"); err != nil {
		t.Fatal(err)
	}
	orchid := x.id(t, "orchid")
	x.gh.Gone(acme.ID, true)
	if r := x.resolve(t, me, "acme", "orchid"); r.Frozen {
		t.Fatalf("frozen before the 1-hour recheck: %+v", r)
	}
	clk.add(2 * time.Hour)
	if r := x.resolve(t, me, "acme", "orchid"); r.Project != orchid || !r.Frozen || r.Path != "@acme/orchid" {
		t.Fatalf("a gone owner's name = %+v, want the project, frozen", r)
	}
	for what, err := range map[string]error{
		"a new name under the frozen owner": bind("side", "acme", "side"),
		"a transfer away from it":           bind("orchid", "ana", "orchid"),
	} {
		if !api.IsCode(err, api.CodeForbidden) {
			t.Errorf("%s: %v, want forbidden", what, err)
		}
	}
	// GitHub answers for the account again: the freeze lifts (same id,
	// nothing rebinds).
	x.gh.Gone(acme.ID, false)
	clk.add(2 * time.Hour)
	if r := x.resolve(t, me, "acme", "orchid"); r.Frozen || r.Project != orchid {
		t.Fatalf("after GitHub answers again = %+v", r)
	}
	if err := bind("side", "acme", "side"); err != nil {
		t.Fatalf("binding after the freeze lifted: %v", err)
	}
}

// A person's own account gone: their names resolve, frozen.
func TestFrozenPerson(t *testing.T) {
	x := newOX(t, "")
	clk := x.clock()
	ctx := context.Background()
	x.projects(t, "orchid")
	ana, _ := x.gh.AddUser("ana", "User")
	me := x.person(t, "judy", ana)
	if _, err := x.bind(t, me, ana, "orchid", "ana", "orchid", false); err != nil {
		t.Fatal(err)
	}
	x.gh.Gone(ana.ID, true)
	clk.add(2 * time.Hour)
	if r := x.resolve(t, me, "ana", "orchid"); !r.Frozen || r.Project != x.id(t, "orchid") {
		t.Fatalf("= %+v, want frozen", r)
	}
	names, err := x.svc.Names(ctx, me, "orchid")
	if err != nil || len(names.Names) != 1 || names.Names[0].Path != "@ana/orchid" {
		t.Fatalf("names = %+v %v", names, err)
	}
}
