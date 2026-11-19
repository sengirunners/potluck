package teamlink_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/owners/githubfake"
	"github.com/sengirunners/potluck/kit/tailnet/tailnettest"
)

// ghHarbor is an authority mini that runs GitHub logins, with team project
// harbor where @beagh holds a member grant, and Bea's laptop on its tailnet.
func ghHarbor(t *testing.T) (gh *githubfake.GitHub, bea githubfake.User, a, m *client.Client) {
	t.Helper()
	ctx := context.Background()
	gh = githubfake.New(t)
	tn := tailnettest.New()
	a = kitDaemonWith(t, tn, "mini", tailnettest.Identity{NodeID: "nMINI", NodeKey: "nodekey:mini", Login: "ivo@example.com", Host: "mini"}, func(c *config.Config) {
		c.File.GitHub = config.GitHub{ClientID: gh.ClientID, APIURL: gh.URL, WebURL: gh.URL}
	})
	m = kitDaemon(t, tn, "laptop", tailnettest.Identity{NodeID: "nLAPTOP", NodeKey: "nodekey:laptop", Login: "bea@example.com", Host: "laptop"})
	if _, err := a.CreateProject(ctx, api.NewProjectRequest{Slug: "harbor"}); err != nil {
		t.Fatal(err)
	}
	if mode, err := a.ProjectTeam(ctx, "harbor"); err != nil || !mode.Team {
		t.Fatalf("team: %+v %v", mode, err)
	}
	bea, _ = gh.AddUser("beagh", "User")
	if _, err := a.GrantGitHub(ctx, api.GitHubGrantRequest{Owner: "@beagh", Role: api.RoleMember, Project: "harbor"}); err != nil {
		t.Fatal(err)
	}
	return gh, bea, a, m
}

// A GitHub join is the daemon's, not its client's (zm join killed by a
// wrapper's timeout while the person types the code): zm join asked again the
// same way answers the same code, the daemon finishes the join itself once
// the person approves, with no client polling, and the next rerun answers
// the joined project. GitHub is asked for one code.
func TestGitHubJoinSurvivesAKilledClient(t *testing.T) {
	ctx := context.Background()
	gh, bea, _, m := ghHarbor(t)
	gh.Approver(bea.ID, 3) // approves on GitHub's fourth poll, a few seconds on

	j, err := m.Join(ctx, api.JoinRequest{Project: "harbor", From: "mini"})
	g := j.GitHub
	if err != nil || g == nil || g.UserCode == "" || g.Resumed || gh.Starts() != 1 {
		t.Fatalf("first answer: %+v %v (GitHub asked %d times)", j, err, gh.Starts())
	}
	// The client is killed while it waits for the person.
	killed, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	_, err = m.Join(killed, api.JoinRequest{Project: "harbor", Login: g.Login, WaitMS: 5000})
	cancel()
	if err == nil {
		t.Fatal("a poll whose caller went away answered no error")
	}
	// Run again the same way (--from in any case, the machine's own name
	// spelled out): the same code, still waiting; GitHub asked once.
	r, err := m.Join(ctx, api.JoinRequest{Project: "harbor", From: "MINI", Machine: "laptop"})
	if err != nil || r.GitHub == nil || !r.GitHub.Resumed || r.GitHub.UserCode != g.UserCode || r.GitHub.Login != g.Login || r.Remote != nil || gh.Starts() != 1 {
		t.Fatalf("rerun: %+v %v (GitHub asked %d times), want code %s resumed", r, err, gh.Starts(), g.UserCode)
	}
	// Nobody polls now: the person approves and the daemon joins by itself.
	deadline := time.Now().Add(30 * time.Second)
	for {
		rs, err := m.Remotes(ctx)
		if err == nil && len(rs) == 1 && rs[0].Project.Slug == "harbor" {
			if x := rs[0]; x.Identity != "beagh" || x.Cap.Role != api.RoleMember {
				t.Fatalf("remote: %+v", x)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the daemon never finished the join: %+v %v", rs, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	// The rerun that comes later answers it, once.
	if r, err = m.Join(ctx, api.JoinRequest{Project: "harbor", From: "mini"}); err != nil || r.Remote == nil || r.Remote.Project.Slug != "harbor" || gh.Starts() != 1 {
		t.Fatalf("rerun after the approval: %+v %v (GitHub asked %d times)", r, err, gh.Starts())
	}
	if _, err := m.Join(ctx, api.JoinRequest{Project: "harbor", From: "mini"}); !api.IsCode(err, api.CodeConflict) || !strings.Contains(err.Error(), "already joined") {
		t.Fatalf("after the join: %v, want already joined", err)
	}
	if _, err := m.Join(ctx, api.JoinRequest{Project: "harbor", Login: g.Login}); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("the spent handle: %v, want not found", err)
	}
}

// A join the authority ended (here GitHub would not say who approved) is
// never offered again as the same code: the rerun answers how it ended,
// once, and the next one asks GitHub for a new code.
func TestGitHubJoinThatEndedIsNoResume(t *testing.T) {
	ctx := context.Background()
	gh, bea, _, m := ghHarbor(t)
	gh.Approver(bea.ID, 0)
	gh.Gone(bea.ID, true) // GET /user refuses the approver's token

	j, err := m.Join(ctx, api.JoinRequest{Project: "harbor", From: "mini"})
	if err != nil || j.GitHub == nil {
		t.Fatalf("first answer: %+v %v", j, err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		r, err := m.Join(ctx, api.JoinRequest{Project: "harbor", From: "mini"})
		if err != nil {
			if !strings.Contains(err.Error(), "who approved") {
				t.Fatalf("the rerun: %v, want why it ended", err)
			}
			break
		}
		if r.GitHub == nil || !r.GitHub.Resumed || time.Now().After(deadline) {
			t.Fatalf("the rerun: %+v, want the same code until the join ends", r)
		}
		time.Sleep(200 * time.Millisecond)
	}
	r, err := m.Join(ctx, api.JoinRequest{Project: "harbor", From: "mini"})
	if err != nil || r.GitHub == nil || r.GitHub.Resumed || r.GitHub.Login == j.GitHub.Login || gh.Starts() != 2 {
		t.Fatalf("after it ended: %+v %v (GitHub asked %d times), want a new code", r, err, gh.Starts())
	}
}
