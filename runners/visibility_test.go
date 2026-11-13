package runners_test

import (
	"context"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/config"
)

// An internal repository (GitHub answers private: true, visibility
// internal: its whole enterprise sees it and can propose code to it) counts
// as public, as potluck ci doctor --probe counts it (ghapp.Public): it is
// connected only with its owner's yes, its jobs are refused without that yes though
// their deliveries say private, and allow_fork_prs never serves a fork's run
// of it.
func TestInternalRepositoryCountsAsPublic(t *testing.T) {
	f := newFixtureWith(t, func(c *config.Runners) { c.AllowForkPRs = true })
	if w := f.link(t, inst, "good"); w.Code != http.StatusOK {
		t.Fatalf("link: %d %s", w.Code, w.Body)
	}
	// acme/app was connected while private (link), then made internal.
	f.gh.mu.Lock()
	f.gh.visibility["acme/app"] = "internal"
	f.gh.events[901], f.gh.events[902], f.gh.events[903] = "push", "push", "push"
	f.gh.forks[903] = true
	f.gh.mu.Unlock()
	ctx := context.Background()

	f.deliver(t, "workflow_job", jobEvent("queued", 9001, 901, "acme/app", true, "self-hosted", "sengi"))
	eventually(t, "an internal repository's job waits for the yes", func() bool {
		j := f.jobs(t)[9001]
		return j.State == "ignored" && strings.Contains(j.Why, "potluck runners allow acme/app --public")
	})
	if _, err := f.allow("acme/app", false); !api.IsCode(err, api.CodeBadRequest) || !strings.Contains(err.Error(), "acme/app is internal: everyone in its enterprise") {
		t.Fatalf("allow an internal repository without --public: %v", err)
	}
	if rp, err := f.allow("acme/app", true); err != nil || !rp.Public || rp.Visibility != "internal" {
		t.Fatalf("allow acme/app --public: %+v, %v", rp, err)
	}

	f.deliver(t, "workflow_job", jobEvent("queued", 9002, 902, "acme/app", true, "self-hosted", "sengi"))
	eventually(t, "its own branch served with the yes", func() bool { return f.jobs(t)[9002].State == "serving" && f.svc.Undone(ctx) == 0 })
	if b, _ := os.ReadFile(f.seen); strings.Contains(string(b), openGuard) || !strings.Contains(string(b), strictGuard) {
		t.Fatalf("an internal repository's runner is open to forks: %s", b)
	}
	f.deliver(t, "workflow_job", jobEvent("queued", 9003, 903, "acme/app", true, "self-hosted", "sengi"))
	eventually(t, "a fork's run refused", func() bool {
		j := f.jobs(t)[9003]
		return j.State == "ignored" && strings.Contains(j.Why, "a run from a fork: never served for a public repository")
	})
}

// A box whose driver cannot check at a job's start that the repository is
// still private (no github-jit-guard-private) never gets an open guard:
// while fork serving holds, its runner has the trusted events' guard, so a
// fork's job that takes it is stopped by the box, and the broker says so.
func TestNoOpenGuardOnADriverThatCannotCheckPrivate(t *testing.T) {
	f := newPoolFixture(t, func(r *config.Runners) {
		r.Label, r.Pool.Sizes, r.Pool.DefaultSize, r.AllowForkPRs = "sengi", map[string]string{"small": "small"}, "small", true
	}, box("small", "small", "linux", "1", "jit_guard_private", "0"))
	ctx := context.Background()
	if w := f.link(t, inst, "good"); w.Code != http.StatusOK {
		t.Fatalf("link: %d %s", w.Code, w.Body)
	}
	f.gh.mu.Lock()
	f.gh.events[911] = "push"
	f.gh.forks[912] = true
	f.gh.status[9101] = "queued"
	f.gh.mu.Unlock()
	f.deliver(t, "workflow_job", jobEvent("queued", 9101, 911, "acme/app", true, "self-hosted", "sengi"))
	eventually(t, "served", func() bool { return f.jobs(t)[9101].State == "serving" && f.svc.Undone(ctx) == 0 })
	if b, _ := os.ReadFile(f.seen); strings.Contains(string(b), "open_if_private") || !strings.Contains(string(b), strictGuard) {
		t.Fatalf("a runner on a driver without github-jit-guard-private got an open guard: %s", b)
	}
	f.deliver(t, "workflow_job", startEvent(9102, 912, "acme/app", "potluck-9101"))
	// Where the guard is open the fork's job is a swap, recorded running on
	// the runner with no new mint (TestForkJobRunsOnOwnBranchRunnerWhileForksAreServed);
	// here the box stops it, and the own branch's job gets a runner again.
	eventually(t, "the own branch's job served again", func() bool { return len(f.minted()) == 2 && f.svc.Undone(ctx) == 0 })
	if j := f.jobs(t)[9102]; j.State == "running" {
		t.Fatalf("the fork's job is recorded running on a runner whose guard stops it: %+v", j)
	}
}

// A fork's job (served only under fork serving) is never placed on a box
// whose guard cannot be open: there the box would stop it and it would fail
// on GitHub. It waits, saying why, with no runner minted.
func TestForkJobWaitsForABoxThatCanCheckPrivate(t *testing.T) {
	f := newPoolFixture(t, func(r *config.Runners) {
		r.Label, r.Pool.Sizes, r.Pool.DefaultSize, r.AllowForkPRs = "sengi", map[string]string{"small": "small"}, "small", true
	}, box("small", "small", "linux", "1", "jit_guard_private", "0"))
	if w := f.link(t, inst, "good"); w.Code != http.StatusOK {
		t.Fatalf("link: %d %s", w.Code, w.Body)
	}
	f.gh.mu.Lock()
	f.gh.forks[921] = true
	f.gh.mu.Unlock()
	f.deliver(t, "workflow_job", jobEvent("queued", 9201, 921, "acme/app", true, "self-hosted", "sengi"))
	eventually(t, "the fork's job waits for a box that can check private", func() bool {
		j := f.jobs(t)[9201]
		return j.State == "waiting" && strings.Contains(j.Why, "cannot check at a job's start that the repository is still private")
	})
	if m := f.minted(); len(m) != 0 {
		t.Fatalf("a runner was minted for a fork's job on a box that cannot check private: %v", m)
	}
}

// Boxes that cannot check private never crowd a fork's job out of one that
// can: with more of them than are tried at once, ranked first, the job still
// goes to the box that can.
func TestForkJobFindsTheBoxThatCanCheckPrivate(t *testing.T) {
	f := newPoolFixture(t, func(r *config.Runners) {
		r.Label, r.AllowForkPRs = "sengi", true
	}, box("a", "small", "linux", "1", "jit_guard_private", "0"), box("b", "small", "linux", "1", "jit_guard_private", "0"),
		box("c", "small", "linux", "1", "jit_guard_private", "0"), box("d", "small", "linux", "1"))
	if w := f.link(t, inst, "good"); w.Code != http.StatusOK {
		t.Fatalf("link: %d %s", w.Code, w.Body)
	}
	f.gh.mu.Lock()
	f.gh.forks[931] = true
	f.gh.mu.Unlock()
	f.deliver(t, "workflow_job", jobEvent("queued", 9301, 931, "acme/app", true, "self-hosted", "sengi"))
	eventually(t, "the fork's job served on the box that can check private, its runner open", func() bool {
		b, _ := os.ReadFile(f.seen)
		return f.jobs(t)[9301].State == "serving" && f.liveOn(t, "d") == 1 && strings.Contains(string(b), openGuard)
	})
}
