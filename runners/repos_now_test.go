package runners_test

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/computeclient"
	"github.com/sengirunners/potluck/kit/config"
)

// What is served is decided on GitHub's answer just before each runner is
// minted, not on the queued delivery: a job that waits (up to a day) while
// its repository is made public is served only as a public repository's job
// would be. Each case queues a job of private acme/app while the pool has no
// box, changes the repository on GitHub, then lends a box.
func TestVisibilityIsAskedBeforeEachRunner(t *testing.T) {
	for _, c := range []struct {
		name    string
		forkPRs bool
		event   string
		fork    bool
		yes     bool // the owner says yes to the public repository while the job waits
		why     string
	}{
		// With allow_fork_prs a fork's run of a private repository passes;
		// made public (and switched on), it is a fork's run of a public one.
		{"a fork's run, made public", true, "pull_request_target", true, true, "a run from a fork: never served for a public repository"},
		// Its own push, made public with no yes from the owner.
		{"its own push, made public without the yes", false, "push", false, false,
			"a public repository its owner has not switched on: its own branches are served after potluck runners allow acme/app --public"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newPoolFixture(t, func(r *config.Runners) { r.AllowForkPRs = c.forkPRs })
			if w := f.link(t, inst, "good"); w.Code != http.StatusOK {
				t.Fatalf("link: %d %s", w.Code, w.Body)
			}
			ctx := context.Background()
			const job, run = 8601, 8601
			f.gh.mu.Lock()
			f.gh.events[run], f.gh.forks[run], f.gh.status[job] = c.event, c.fork, "queued"
			f.gh.mu.Unlock()
			f.deliver(t, "workflow_job", jobEvent("queued", job, run, "acme/app", true, "pool"))
			eventually(t, "the job waits for a box", func() bool {
				j := f.jobs(t)[job]
				return j.State == "waiting" && j.Why != "being checked with GitHub"
			})
			f.gh.mu.Lock()
			f.gh.publicRepo["acme/app"] = true
			f.gh.mu.Unlock()
			if c.yes {
				if _, err := f.allow("acme/app", true); err != nil {
					t.Fatal(err)
				}
			}
			b := box("z", "c1m2", "linux", "1")
			b.Driver, b.Kind, b.Isolation, b.Images = "ref", "box", "ephemeral", []string{refPin}
			b.Config["remote"] = f.remote
			if _, err := computeclient.BindRunTarget(ctx, f.owner, computeapi.BindRunTargetRequest{Target: b}); err != nil {
				t.Fatal(err)
			}
			eventually(t, "refused", func() bool {
				j := f.jobs(t)[job]
				return j.State == "ignored" && strings.Contains(j.Why, c.why)
			})
			if m := f.minted(); len(m) != 0 {
				t.Errorf("runners minted: %v", m)
			}
			if f.liveOn(t, "z") != 0 {
				t.Error("a box was acquired")
			}
			if c.yes {
				// Its own push is served, its box guarded with the trusted events.
				f.gh.mu.Lock()
				f.gh.events[8602] = "push"
				f.gh.mu.Unlock()
				f.deliver(t, "workflow_job", jobEvent("queued", 8602, 8602, "acme/app", false, "pool"))
				guard := `"jit_guard":{"repo":"acme/app","events":["push","pull_request","workflow_dispatch","schedule","merge_group","release","repository_dispatch"]}`
				eventually(t, "the push served, its box guarded", func() bool {
					b, _ := os.ReadFile(f.seen)
					return f.jobs(t)[8602].State == "serving" && strings.Contains(string(b), guard)
				})
			}
		})
	}
}

// A job served again after a refused job took its runner (reserve) is
// checked as GitHub has its repository then: made public without the
// owner's yes, or disconnected, it is not served again.
func TestReserveAsksGitHubAgain(t *testing.T) {
	for _, change := range []string{"made public", "disconnected"} {
		t.Run(change, func(t *testing.T) {
			f := newFixture(t)
			ctx := context.Background()
			if w := f.link(t, inst, "good"); w.Code != http.StatusOK {
				t.Fatalf("link: %d %s", w.Code, w.Body)
			}
			labels := []string{"self-hosted", "sengi"}
			f.gh.mu.Lock()
			f.gh.events[91], f.gh.status[4001] = "pull_request_target", "queued"
			f.gh.mu.Unlock()
			f.deliver(t, "workflow_job", jobEvent("queued", 4001, 90, "acme/app", true, labels...))
			eventually(t, "served", func() bool { return f.jobs(t)[4001].State == "serving" && f.svc.Undone(ctx) == 0 })
			switch change {
			case "made public":
				f.gh.mu.Lock()
				f.gh.publicRepo["acme/app"] = true
				f.gh.mu.Unlock()
			case "disconnected":
				if err := f.owner.Do(ctx, "DELETE", "/v0/runners/repos/acme/app", nil, nil); err != nil {
					t.Fatal(err)
				}
			}
			// A pull_request_target job takes the runner: the box goes, and
			// job 4001 is not served again.
			f.deliver(t, "workflow_job", startEvent(4002, 91, "acme/app", "potluck-4001"))
			want := map[string]string{"made public": "a public repository its owner has not switched on", "disconnected": "acme/app is not connected"}[change]
			eventually(t, "not served again", func() bool {
				j := f.jobs(t)[4001]
				return j.State == "ignored" && strings.Contains(j.Why, want)
			})
			if m := f.minted(); !slices.Equal(m, []string{"potluck-4001"}) {
				t.Errorf("runners minted: %v", m)
			}
		})
	}
}

// A connection is of one repository, by GitHub's id: another repository
// that takes a connected one's name (it was deleted, or renamed away) is not
// connected, nor is a connected one under a new name. A connection the
// upgrade made has no id until GitHub's API names it; a delivery's id never
// fills it.
func TestConnectionIsOfOneRepositoryID(t *testing.T) {
	f := pinned(t, "a", "b")
	ctx := context.Background()
	f.gh.mu.Lock()
	for _, id := range []int64{7301, 7302, 7303, 7304, 7305, 7306} {
		f.gh.events[id] = "push"
	}
	f.gh.mu.Unlock()
	refused := func(job int64, body []byte, want string) {
		t.Helper()
		f.deliver(t, "workflow_job", body)
		eventually(t, fmt.Sprintf("job %d refused (%s)", job, want), func() bool {
			j := f.jobs(t)[job]
			return j.State == "ignored" && strings.Contains(j.Why, want)
		})
	}

	// acme/lib deleted and a new acme/lib made: its delivery is refused at
	// once, and so it is when GitHub's answer (not the delivery) says so.
	const newLib = 99001
	refused(7301, jobEventOf("queued", 7301, 7301, "acme/lib", newLib, true, "pool:sa"),
		"acme/lib is another repository than the one connected under that name (that one was renamed or deleted since): potluck runners allow acme/lib connects this one")
	f.gh.mu.Lock()
	f.gh.repoIDs["acme/lib"] = newLib
	f.gh.mu.Unlock()
	refused(7302, jobEvent("queued", 7302, 7302, "acme/lib", true, "pool:sa"), "another repository than the one connected")
	if f.liveOn(t, "a") != 0 {
		t.Fatal("a box was acquired")
	}
	// The owner connects the new one.
	if _, err := f.allow("acme/lib", false); err != nil {
		t.Fatal(err)
	}
	f.deliver(t, "workflow_job", jobEventOf("queued", 7303, 7303, "acme/lib", newLib, true, "pool:sa"))
	eventually(t, "the new acme/lib served", func() bool { return f.jobs(t)[7303].State == "serving" })

	// acme/other renamed acme/renamed (the same id): not connected under the
	// new name until its owner says so; the old name's record goes then.
	f.gh.mu.Lock()
	f.gh.repoIDs["acme/renamed"] = repoID("acme/other")
	f.gh.mu.Unlock()
	refused(7304, jobEventOf("queued", 7304, 7304, "acme/renamed", repoID("acme/other"), true, "pool:sb"),
		"acme/renamed was connected as acme/other, a name it no longer has (potluck runners allow acme/renamed)")
	if _, err := f.allow("acme/renamed", false); err != nil {
		t.Fatal(err)
	}
	if rs := f.repos(t); rs["acme/other"].Repo != "" || rs["acme/renamed"].Repo == "" {
		t.Errorf("after allowing the new name: %v", rs)
	}

	// An upgraded connection without an id: a delivery naming another id is
	// let through to the check before the mint, which asks GitHub, refuses
	// it there and records GitHub's id; from then on that delivery's id is
	// refused at once.
	if err := f.svc.ForgetRepoID(ctx, "acme/app"); err != nil {
		t.Fatal(err)
	}
	f.gh.mu.Lock()
	f.gh.repoIDs["acme/app"] = 55501
	f.gh.mu.Unlock()
	f.deliver(t, "workflow_job", jobEventOf("queued", 7305, 7305, "acme/app", 55502, true, "pool:sb"))
	eventually(t, "served on GitHub's word", func() bool { return f.jobs(t)[7305].State == "serving" })
	refused(7306, jobEventOf("queued", 7306, 7306, "acme/app", 55502, true, "pool:sb"), "another repository than the one connected")
}

// A connected repository GitHub no longer answers for (deleted) is read
// again with backoff, never every sweep, and logged once a read; an
// installation whose API budget is nearly spent is not read at all.
func TestRepoCheckBacksOff(t *testing.T) {
	f := pinned(t, "a")
	ctx := context.Background()
	reads := func() (n int) {
		f.gh.mu.Lock()
		defer f.gh.mu.Unlock()
		for _, c := range f.gh.calls {
			if c == "GET /repos/acme/lib" {
				n++
			}
		}
		return n
	}
	f.gh.mu.Lock()
	f.gh.gone["acme/lib"] = true
	f.gh.mu.Unlock()
	at := func(d time.Duration) int {
		f.clock.advance(d)
		f.svc.Sweep(ctx)
		return reads()
	}
	base := reads()
	if n := at(time.Hour + time.Second); n != base+1 {
		t.Fatalf("first due read: %d reads, want %d", n, base+1)
	}
	// Failed once: not read again for an hour, then two, then four.
	for i, d := range []time.Duration{5 * time.Minute, 50 * time.Minute, 10 * time.Minute, 2*time.Hour + time.Second, 3 * time.Hour, time.Hour + time.Second} {
		want := base + 1 + map[int]int{0: 0, 1: 0, 2: 1, 3: 2, 4: 2, 5: 3}[i]
		if n := at(d); n != want {
			t.Errorf("sweep %d (+%s): %d reads, want %d", i, d, n, want)
		}
	}
	// A low API budget: nothing is read, due or not, until it resets.
	g := pinned(t, "a")
	g.svc.RepoCheckEvery = 0 // every connection due
	g.gh.mu.Lock()
	g.gh.rateLeft = 40
	g.gh.mu.Unlock()
	if _, err := g.allow("acme/app", false); err != nil { // answered with the low budget
		t.Fatal(err)
	}
	g.gh.mu.Lock()
	n := len(g.gh.calls)
	g.gh.mu.Unlock()
	g.svc.Sweep(ctx)
	g.gh.mu.Lock()
	defer g.gh.mu.Unlock()
	for _, c := range g.gh.calls[n:] {
		if strings.HasPrefix(c, "GET /repos/acme/") && !strings.Contains(c, "/actions/") {
			t.Errorf("read with the budget nearly spent: %s", c)
		}
	}
}
