package teamlink_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/tailnet/tailnettest"
	"github.com/sengirunners/potluck/kit/teamlink"
)

// A KIT-only daemon (teamlink is its d.Team, no internal/team) joined to
// another's project forwards the KIT routes for it to the authority: the
// projects list, people, the owner right. A route with no row is not
// forwarded: this machine's router answers it as any unknown route.
func TestKitProxyForwardsKitRoutes(t *testing.T) {
	ctx := context.Background()
	tn := tailnettest.New()
	a := kitDaemon(t, tn, "mini", tailnettest.Identity{NodeID: "nMINI", NodeKey: "nodekey:mini", Login: "judy@example.com", Host: "mini"})
	m := kitDaemon(t, tn, "laptop", tailnettest.Identity{NodeID: "nLAPTOP", NodeKey: "nodekey:laptop", Login: "ana@example.com", Host: "laptop"})
	p, err := a.CreateProject(ctx, api.NewProjectRequest{Slug: "orchid"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.ProjectTeam(ctx, "orchid"); err != nil {
		t.Fatal(err)
	}
	inv, err := a.Invite(ctx, api.InviteRequest{Project: "orchid", As: "ana", Role: api.RoleMember})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Join(ctx, api.JoinRequest{Code: inv.Code}); err != nil {
		t.Fatal(err)
	}

	// GET /v0/projects lists the joined project, as its authority shows it.
	var pl api.ProjectList
	if err := m.Do(ctx, http.MethodGet, "/v0/projects", nil, &pl); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, x := range pl.Projects {
		found = found || x.ID == p.ID
	}
	if !found || len(pl.Skipped) != 0 {
		t.Fatalf("projects on the member: %+v", pl)
	}
	// GET /v0/people for the joined project is the authority's answer: its
	// owner, whom the member's own registry does not know, answering the
	// project, and the member itself, flagged you.
	var people api.PeopleList
	if err := m.Do(ctx, http.MethodGet, "/v0/people?project=orchid", nil, &people); err != nil {
		t.Fatal(err)
	}
	if len(people.People) != 2 || people.People[1].Name != "mini-human" || people.People[1].You || !people.People[1].Projects[0].Answers ||
		!people.People[0].You || people.People[0].Projects[0].Answers {
		t.Fatalf("people of orchid on the member: %+v", people)
	}
	// GET /v0/owner (the owners module's) is judged at the authority.
	var owner map[string]any
	if err := m.Do(ctx, http.MethodGet, "/v0/owner?project=orchid", nil, &owner); err != nil {
		t.Fatalf("owner through the member: %v", err)
	}
	// No row: the member's router answers (a KIT daemon has no work routes).
	err = m.Do(ctx, http.MethodGet, "/v0/work?project=orchid", nil, nil)
	if !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("GET /v0/work for the joined project: %v, want not_found from this machine's router", err)
	}
	if err == nil || strings.Contains(err.Error(), "mini") {
		t.Fatalf("answered by the authority: %v", err)
	}
}

// A forwarded row needs a hedge decision, and one pattern one owner: the
// daemon does not start otherwise.
func TestRouteRowsChecked(t *testing.T) {
	s := teamlink.New(&core.Deps{Config: &config.Config{Machine: "mini"}})
	if err := s.CheckRoutes(); err != nil {
		t.Fatalf("teamlink's own rows: %v", err)
	}
	s.RegisterRoutes("x", teamlink.RouteRule{Pattern: "GET /v0/thing", Kind: teamlink.KindItem})
	if err := s.CheckRoutes(); err == nil || !strings.Contains(err.Error(), "no hedge decision") {
		t.Fatalf("a row with no hedge decision: %v", err)
	}
	s = teamlink.New(&core.Deps{Config: &config.Config{Machine: "mini"}})
	s.RegisterRoutes("x", teamlink.RouteRule{Pattern: "GET /v0/people", Kind: teamlink.KindList, Hedge: teamlink.Hedged})
	if err := s.CheckRoutes(); err == nil || !strings.Contains(err.Error(), "rows from teamlink and x") {
		t.Fatalf("a pattern registered twice: %v", err)
	}
}
