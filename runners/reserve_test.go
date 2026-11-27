package runners_test

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/runners"
)

// A host that takes a box back before its job started (a drain, a close,
// its owner coming back) leaves the job queued at GitHub: it is served again
// each time, past the cap on serves after a taken runner, until a box keeps
// it.
func TestBoxLostBeforeStartIsServedAgainEachTime(t *testing.T) {
	f := pinned(t, "a")
	ctx := context.Background()
	f.gh.mu.Lock()
	f.gh.status[9801] = "queued"
	f.gh.mu.Unlock()
	f.serveOn(t, 9801, 980, "a")
	for k := 1; k <= 3; k++ {
		key, runner := "gh-9801", "potluck-9801"
		if k > 1 {
			key, runner = fmt.Sprintf("gh-9801-%d", k), fmt.Sprintf("potluck-9801-%d", k)
		}
		if j := f.jobs(t)[9801]; j.State != "serving" || j.Runner != runner {
			t.Fatalf("job 9801 before loss %d: %+v; want serving on %s", k, j, runner)
		}
		f.takeBackKey(t, key)
		next := fmt.Sprintf("potluck-9801-%d", k+1)
		eventuallyBox(t, "9801 served again on "+next, func() bool {
			f.svc.Watch(ctx)
			j := f.jobs(t)[9801]
			return j.State == "serving" && j.Runner == next && f.svc.Undone(ctx) == 0
		})
	}
}

// A job that ended at GitHub while its host stayed away (unlisted, not
// answering a release) keeps its row only as long as the box's lease could
// still run: past it, the host has ended the box itself, so the row ends and
// no longer counts against the binding. The lease counts from its last
// renewal, and a daemon restart in between counts it again from the restart.
func TestHostAwayPastTheLeaseEndsAFinishedJobsRow(t *testing.T) {
	f := pinned(t, "a")
	ctx := context.Background()
	f.serveOn(t, 9811, 981, "a")
	f.started(t, 9811, 981, "acme/app", "potluck-9811")
	eventually(t, "the start applied", func() bool { return f.svc.Undone(ctx) == 0 })
	f.clock.advance(45 * time.Minute)
	f.svc.Sweep(ctx) // renews its lease: counted from here
	f.svc.HideTargets("a")
	f.svc.Watch(ctx) // notes the host unlisted
	f.gh.mu.Lock()
	f.gh.jobDone[9811], f.gh.conclusion[9811] = true, "success"
	f.gh.mu.Unlock()
	f.deliver(t, "workflow_job", endEvent(9811, 981, "acme/app", "success", 1, "pool:sa"))
	eventually(t, "the completed delivery settled", func() bool { return f.svc.Undone(ctx) == 0 })
	f.svc.Sweep(ctx)
	if j := f.jobs(t)[9811]; j.State != "serving" {
		t.Fatalf("job 9811 ended while its box's lease could still run: %+v", j)
	}
	f.clock.advance(40 * time.Minute) // past a lease from the acquire, not from the renewal
	f.svc.Sweep(ctx)
	if j := f.jobs(t)[9811]; j.State != "serving" {
		t.Fatalf("job 9811 ended within a lease of its last renewal: %+v", j)
	}
	f.restart(t)
	f.svc.HideTargets("a")
	f.svc.Sweep(ctx)
	if j := f.jobs(t)[9811]; j.State != "serving" {
		t.Fatalf("job 9811 ended just after a restart: %+v", j)
	}
	f.clock.advance(2 * time.Hour) // past the 3300 s lease and its slack, counted from the restart
	f.svc.Watch(ctx)
	f.svc.Sweep(ctx)
	if j := f.jobs(t)[9811]; j.State != "done" {
		t.Errorf("job 9811 after its host stayed away past the lease: %+v; want done", j)
	}
}

// Two jobs of a run on its last attempt lose their boxes. The first one's
// re-run item closes it (no attempts left) and is done; the second, ended
// later, is closed by that same item due again, never left waiting.
func TestSecondLostJobOfASettledRerunIsClosed(t *testing.T) {
	f := pinned(t, "a", "b")
	ctx := context.Background()
	f.gh.mu.Lock()
	f.gh.attempt[820] = 3
	f.gh.mu.Unlock()
	f.serveOn(t, 9821, 820, "a")
	f.serveOn(t, 9822, 820, "b")
	f.started(t, 9821, 820, "acme/app", "potluck-9821")
	f.started(t, 9822, 820, "acme/app", "potluck-9822")
	eventually(t, "the starts applied", func() bool { return f.svc.Undone(ctx) == 0 })
	f.takeBack(t, 9821)
	f.takeBack(t, 9822)
	eventually(t, "both losses recorded", func() bool {
		f.svc.Watch(ctx)
		jobs := f.jobs(t)
		return jobs[9821].State == "lost" && jobs[9822].State == "lost"
	})
	f.runIs(820, "completed")
	f.deliver(t, "workflow_job", endEvent(9821, 820, "acme/app", "failure", 3, "pool:sa"))
	eventually(t, "9821 closed", func() bool {
		return strings.Contains(f.jobs(t)[9821].Why, "attempt 3 of 3") && f.svc.Undone(ctx) == 0
	})
	f.deliver(t, "workflow_job", endEvent(9822, 820, "acme/app", "failure", 3, "pool:sb"))
	eventually(t, "9822 closed", func() bool {
		return strings.Contains(f.jobs(t)[9822].Why, "attempt 3 of 3") && f.svc.Undone(ctx) == 0
	})
	if got := f.reruns(); len(got) != 0 {
		t.Errorf("re-runs past the last attempt: %v", got)
	}
}

// A re-run is of one attempt's lost jobs. A job lost on attempt 1 whose re-run
// item is still backing off (GitHub did not answer) is not part of attempt
// 2's: attempt 2's one lost job is re-run alone, not with the run's failed
// jobs.
func TestRerunTakesOnlyItsAttemptsLostJobs(t *testing.T) {
	f := pinnedStarting(t, func(s *runners.Service) { s.Retry = time.Hour }, "a", "b")
	ctx := context.Background()
	f.serveOn(t, 9831, 830, "a")
	f.started(t, 9831, 830, "acme/app", "potluck-9831")
	eventually(t, "the start applied", func() bool { return f.svc.Undone(ctx) == 0 })
	f.takeBack(t, 9831)
	eventually(t, "9831 lost", func() bool { f.svc.Watch(ctx); return f.jobs(t)[9831].State == "lost" })
	failRun, runAsks := true, 0
	f.gh.mu.Lock()
	f.gh.extra = func(w http.ResponseWriter, r *http.Request) bool {
		if failRun && r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/actions/runs/830") {
			runAsks++
			http.Error(w, `{"message":"unavailable"}`, http.StatusBadGateway)
			return true
		}
		return false
	}
	f.gh.mu.Unlock()
	f.deliver(t, "workflow_job", endEvent(9831, 830, "acme/app", "failure", 1, "pool:sa"))
	eventually(t, "9831 ended", func() bool { return strings.Contains(f.jobs(t)[9831].Why, "GitHub failed it") })
	// Its re-run asks GitHub for the run once, and backs off an hour.
	eventually(t, "the re-run's ask refused", func() bool {
		f.gh.mu.Lock()
		defer f.gh.mu.Unlock()
		return runAsks > 0
	})
	f.gh.mu.Lock()
	failRun = false
	f.gh.attempt[830] = 2 // a person re-ran it meanwhile
	f.gh.mu.Unlock()
	f.deliver(t, "workflow_job", jobEvent("queued", 9832, 830, "acme/app", true, "pool:sb"))
	eventually(t, "job 9832 on box b", func() bool { return f.liveOn(t, "b") == 1 && f.svc.Undone(ctx) == 1 }) // 9831's re-run backs off
	f.started(t, 9832, 830, "acme/app", "potluck-9832")
	eventually(t, "the start applied", func() bool { return f.svc.Undone(ctx) <= 1 })
	f.takeBack(t, 9832)
	eventually(t, "9832 lost", func() bool { f.svc.Watch(ctx); return f.jobs(t)[9832].State == "lost" })
	f.runIs(830, "completed")
	f.deliver(t, "workflow_job", endEvent(9832, 830, "acme/app", "failure", 2, "pool:sb"))
	eventually(t, "attempt 2 re-run", func() bool { return len(f.reruns()) > 0 })
	if got := f.reruns(); !slices.Equal(got, []string{"job 9832"}) {
		t.Errorf("re-runs %v; want job 9832 alone", got)
	}
}

// GitHub not answering whether a job still runs on a box its host took back
// is asked again on later Watches, a bounded number of times; then the box is
// let go, the row says so, and nothing is re-run.
func TestLostBoxGitHubDoesNotAnswerForIsGivenUp(t *testing.T) {
	f := pinned(t, "a")
	ctx := context.Background()
	f.serveOn(t, 9841, 840, "a")
	f.started(t, 9841, 840, "acme/app", "potluck-9841")
	eventually(t, "the start applied", func() bool { return f.svc.Undone(ctx) == 0 })
	f.gh.mu.Lock()
	f.gh.extra = func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/actions/jobs/9841") {
			http.Error(w, `{"message":"unavailable"}`, http.StatusBadGateway)
			return true
		}
		return false
	}
	f.gh.mu.Unlock()
	f.takeBack(t, 9841)
	for range 9 {
		f.svc.Watch(ctx)
	}
	if j := f.jobs(t)[9841]; j.State != "serving" {
		t.Fatalf("job 9841 given up before the last ask: %+v", j)
	}
	f.svc.Watch(ctx)
	if j := f.jobs(t)[9841]; j.State != "done" || !strings.Contains(j.Why, "GitHub did not say whether job 9841 still ran (10 tries") {
		t.Errorf("job 9841 after GitHub left the question unanswered: %+v", j)
	}
	if got := f.reruns(); len(got) != 0 {
		t.Errorf("re-runs %v; want none", got)
	}
}

// A box that fails on its own before its job started (it ended by itself or
// was lost: its VM stopped, its supervisor died) points at its host: those
// count against the job's serves, and the job is served again only later after the third.
func TestBoxThatFailsOnItsOwnUsesUpTheServes(t *testing.T) {
	for i, state := range []string{"ended", "lost"} {
		t.Run(state, func(t *testing.T) {
			f := pinned(t, "a")
			ctx := context.Background()
			id := int64(9851 + i)
			f.gh.mu.Lock()
			f.gh.status[id] = "queued"
			f.gh.mu.Unlock()
			f.serveOn(t, id, 985, "a")
			f.svc.AnswerStatus(func(target string, h computeapi.RunTargetHandle, err error) (computeapi.RunTargetHandle, error) {
				if err == nil {
					h.State = state
				}
				return h, err
			})
			eventuallyBox(t, fmt.Sprintf("%d out of serves", id), func() bool {
				f.svc.Watch(ctx)
				return strings.Contains(f.jobs(t)[id].Why, "failed 3 times: served again at")
			})
			if j := f.jobs(t)[id]; j.State != "taken" || j.Runner != fmt.Sprintf("potluck-%d-3", id) {
				t.Errorf("job %d: %+v", id, j)
			}
		})
	}
}

// GitHub not answering the sweep's question about a running job is not the
// job ending: its box is renewed all the same.
func TestSweepRenewsWhenGitHubDoesNotAnswer(t *testing.T) {
	f := pinned(t, "a")
	ctx := context.Background()
	f.serveOn(t, 9861, 986, "a")
	f.started(t, 9861, 986, "acme/app", "potluck-9861")
	eventually(t, "the start applied", func() bool { return f.svc.Undone(ctx) == 0 })
	f.gh.mu.Lock()
	f.gh.extra = func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/actions/jobs/9861") {
			http.Error(w, `{"message":"unavailable"}`, http.StatusBadGateway)
			return true
		}
		return false
	}
	f.gh.mu.Unlock()
	f.svc.Sweep(ctx)
	if got := f.renews(t); len(got) != 1 {
		t.Errorf("renewals while GitHub did not answer: %v; want one", got)
	}
}
