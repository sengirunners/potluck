package core

import (
	"context"
	"testing"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
)

// AreaRelation lifts a member machine's node-bound cap to its identity's
// current grants, for the project its cap names only; a minted token keeps
// its cap; an identity whose grants are gone has none.
func TestAreaRelation(t *testing.T) {
	ctx := context.Background()
	d, _ := newDeps(t)
	reg, local := d.Projects, d.Projects.Local("")
	orchid, side := mustID(t, d, "orchid"), mustID(t, d, "side")
	if _, err := reg.AddArea(ctx, local, "orchid", api.Area{Name: "bloom"}); err != nil {
		t.Fatal(err)
	}
	mint := func(as, area, role string) api.Identity {
		if _, err := reg.Mint(ctx, local, api.MintTokenRequest{As: as, Project: "orchid", Area: area, Role: role}); err != nil {
			t.Fatal(err)
		}
		id, err := reg.Identity(ctx, as)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	subject := func(id api.Identity, cap *authz.Cap, node bool) *authz.Subject {
		tuples, err := reg.Tuples(ctx, id.ID)
		if err != nil {
			t.Fatal(err)
		}
		s := &authz.Subject{Identity: id, Via: authz.ViaToken, Scope: authz.FromTuples(tuples, cap), Cap: cap}
		if node {
			s.Node, s.Machine = "n-"+id.Name, id.Name+"-mac"
		}
		return s
	}
	rel := func(s *authz.Subject, project, area string) authz.Relation {
		r, err := d.AreaRelation(ctx, s, project, area)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	// A guest machine (its link capped to one area, as a guest) whose
	// identity owns the pool.
	owen := mint("owen", "", "owner")
	owenLink := subject(owen, &authz.Cap{Project: orchid, Area: "sprout", Role: authz.Guest}, true)
	if r := rel(owenLink, orchid, ""); r != authz.Owner {
		t.Errorf("an owner through a guest machine: %q", r)
	}
	// A link capped to area sprout, its identity owning area bloom.
	ada := mint("ada", "sprout", "guest")
	if _, err := reg.Grant(ctx, local, api.GrantRequest{Identity: "ada", Role: "owner", Project: "orchid", Area: "bloom"}); err != nil {
		t.Fatal(err)
	}
	adaLink := subject(ada, &authz.Cap{Project: orchid, Area: "sprout", Role: authz.Member}, true)
	if r := rel(adaLink, orchid, "bloom"); r != authz.Owner {
		t.Errorf("an area owner through a link capped to another area: %q", r)
	}
	if r := rel(adaLink, orchid, ""); r != authz.None {
		t.Errorf("an area owner on the whole pool: %q", r)
	}
	// A cap naming another project lifts nothing here.
	if r := rel(subject(ada, &authz.Cap{Project: side, Role: authz.Member}, true), orchid, "bloom"); r != authz.None {
		t.Errorf("a link capped to another project: %q", r)
	}
	// A minted token (not node-bound) keeps its cap.
	if r := rel(subject(owen, &authz.Cap{Project: orchid, Role: authz.Guest}, false), orchid, ""); r != authz.Guest {
		t.Errorf("a minted guest token of an owner: %q", r)
	}
	// Once the identity's grants are gone, its link has nothing, whatever
	// the subject it was built with says.
	if _, err := reg.Revoke(ctx, local, api.GrantRequest{Identity: "owen", Project: "orchid"}); err != nil {
		t.Fatal(err)
	}
	if r := rel(owenLink, orchid, ""); r != authz.None {
		t.Errorf("a removed identity's link: %q", r)
	}
	// MemberSince, by area.
	if _, ok, _ := reg.MemberSince(ctx, owen.ID, orchid, ""); ok {
		t.Error("a removed identity is still a member")
	}
	if _, ok, _ := reg.MemberSince(ctx, ada.ID, orchid, "bloom"); !ok {
		t.Error("ada is not a member of bloom")
	}
	if _, ok, _ := reg.MemberSince(ctx, ada.ID, orchid, "other"); ok {
		t.Error("ada is a member of an area she holds nothing in")
	}
}
