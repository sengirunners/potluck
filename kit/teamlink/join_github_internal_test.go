package teamlink

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/core"
)

// No GitHub join's watch starts once StopLoops ended them (a join still in
// discovery at shutdown would poll on with nobody to stop it).
func TestGitHubJoinRefusedAfterStop(t *testing.T) {
	s := &Service{}
	s.stopGitHubJoins()
	j := &ghJoin{project: "harbor", view: api.JoinGitHub{Login: "h1"}}
	if err := s.startGitHub(j, api.JoinRequest{}); !api.IsCode(err, api.CodeUnavailable) {
		t.Fatalf("start after stop: %v, want unavailable", err)
	}
	if _, ok := s.ghJoins.Load("h1"); ok {
		t.Fatal("a join refused at stop is kept")
	}
}

// A join finishes with the checkouts its runs named: a poll by handle alone
// keeps them, a rerun from outside any checkout keeps the first run's, and
// a rerun that names paths takes those.
func TestGitHubJoinKeepsCheckouts(t *testing.T) {
	repo, elsewhere := t.TempDir(), t.TempDir()
	if err := os.Mkdir(filepath.Join(repo, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	j := &ghJoin{}
	j.keepCheckouts(api.JoinRequest{Cwd: repo})
	j.keepCheckouts(api.JoinRequest{Login: "h1", WaitMS: 5000})
	j.keepCheckouts(api.JoinRequest{Cwd: elsewhere})
	if j.cwd != repo || len(j.paths) != 0 {
		t.Fatalf("kept %q %v, want the first run's checkout %q", j.cwd, j.paths, repo)
	}
	j = &ghJoin{}
	j.keepCheckouts(api.JoinRequest{Cwd: elsewhere})
	j.keepCheckouts(api.JoinRequest{Cwd: repo})
	if j.cwd != repo {
		t.Fatalf("kept %q, want the later run's checkout %q", j.cwd, repo)
	}
	j.keepCheckouts(api.JoinRequest{Paths: []string{"/src/harbor"}, Cwd: elsewhere})
	j.keepCheckouts(api.JoinRequest{Cwd: repo})
	if !slices.Equal(j.paths, []string{"/src/harbor"}) {
		t.Fatalf("kept %v, want the named paths", j.paths)
	}
}

// A join whose authority never answered until its code expired says so: the
// expiry is not the whole story.
func TestGitHubJoinExpirySaysTheAuthorityWasAway(t *testing.T) {
	old := GitHubPollGap
	GitHubPollGap = 50 * time.Millisecond
	t.Cleanup(func() { GitHubPollGap = old })
	s := &Service{d: &core.Deps{}}
	j := &ghJoin{project: "harbor", base: "http://x:7433", id: "i", via: core.ViaTailnet,
		hc:      &http.Client{Transport: errTransport{errors.New("dial: no route")}},
		expires: time.Now().Add(-time.Minute + 500*time.Millisecond)} // its watch ends in 0.5 s
	ctx, cancel := context.WithDeadline(context.Background(), j.expires.Add(time.Minute))
	defer cancel()
	_, err := s.collectGitHub(context.Background(), ctx, j)
	if !api.IsCode(err, api.CodeForbidden) || !strings.Contains(err.Error(), "expired") || !strings.Contains(err.Error(), "did not answer") {
		t.Fatalf("expiry: %v, want expired and the authority away", err)
	}
}
