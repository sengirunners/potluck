package runners_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/runners"
)

// endEvent is a completed delivery: how the job ended, in which attempt of
// its run.
func endEvent(job, run int64, repo, conclusion string, attempt int, labels ...string) []byte {
	b, _ := json.Marshal(map[string]any{"action": "completed", "installation": map[string]any{"id": inst},
		"repository":   map[string]any{"id": repoID(repo), "full_name": repo, "private": true},
		"workflow_job": map[string]any{"id": job, "run_id": run, "run_attempt": attempt, "conclusion": conclusion, "labels": labels}})
	return b
}

// started tells the fake GitHub and the broker that job runs on runner.
func (f *fixture) started(t *testing.T, job, run int64, repo, runner string) {
	t.Helper()
	f.gh.mu.Lock()
	f.gh.runnerOf[job], f.gh.jobRun[job] = runner, run
	f.gh.mu.Unlock()
	f.deliver(t, "workflow_job", startEvent(job, run, repo, runner))
}

// takeBack is the lending machine taking back the box that serves job's
// first runner (its record's key ends in the request id, gh-<job>).
func (f *fixture) takeBack(t *testing.T, job int64) {
	t.Helper()
	f.takeBackKey(t, fmt.Sprintf("gh-%d", job))
}

// takeBackKey takes back the box acquired with request id key: the box's
// reclaim window, which its supervisor reads from the box's record every
// 25 ms, is set to one that is closed now, as when the machine's owner comes
// back. It waits until the box is reclaimed.
func (f *fixture) takeBackKey(t *testing.T, key string) {
	t.Helper()
	later := time.Now().Add(time.Hour)
	window := later.Format("15:04:05") + "-" + later.Add(time.Second).Format("15:04:05")
	eventually(t, "the box of request "+key+" taken back", func() bool {
		ps, _ := filepath.Glob(filepath.Join(f.state, "h-*.json"))
		for _, p := range ps {
			var r map[string]any
			if b, err := os.ReadFile(p); err != nil || json.Unmarshal(b, &r) != nil || !strings.HasSuffix(fmt.Sprint(r["Key"]), "-"+key) {
				continue
			}
			if r["State"] != "ready" {
				return r["State"] == "reclaimed"
			}
			if r["Window"] != window { // the supervisor may save over it: set again until it is read
				r["Window"] = window
				b, _ := json.Marshal(r)
				if os.WriteFile(p+".test", b, 0o600) == nil {
					_ = os.Rename(p+".test", p)
				}
			}
		}
		return false
	})
}

func (f *fixture) reruns() []string {
	f.gh.mu.Lock()
	defer f.gh.mu.Unlock()
	return slices.Clone(f.gh.reruns)
}

func (f *fixture) runIs(run int64, status string) {
	f.gh.mu.Lock()
	f.gh.runStatus[run] = status
	f.gh.mu.Unlock()
}

// A host takes its boxes back while their jobs run. GitHub cannot
// give such a job to another runner: it fails it once the vanished runner
// misses its timeout. The broker records each loss while GitHub still has the
// job running, and re-runs the job once GitHub ended it and its run, on a new
// box, at most twice per run:
//
//   - run 300: job 9101 loses box a, is re-run as job 9111 (attempt 2), which
//     loses box b and is re-run again (attempt 3 of 3);
//   - run 301: job 9102 fails on its own on a box nobody took: not re-run;
//   - run 302: job 9105, already on attempt 3, loses its box: not re-run, and
//     its completed delivery never comes (the sweep finds it);
//   - run 303: jobs 9106 and 9107 run on each other's runner (GitHub gives a
//     runner to any job of the repository with its labels) and both lose
//     their box: one re-run of the run's failed jobs;
//   - run 304, of a repository whose installation lacks Actions: write: the
//     loss is recorded and logged, nothing is re-run, nothing retries;
//   - run 305: job 9109 loses its box and is then cancelled by a person: not
//     re-run.
func TestBoxTakenBackMidJobIsRerun(t *testing.T) {
	// Each job names its box's size, pinned in sizes: placement asks no box to
	// describe itself (eight driver runs a serve, slow under the race detector).
	f := newPoolFixture(t, func(r *config.Runners) {
		r.Pool.Sizes = map[string]string{}
		for _, b := range []string{"a", "b", "c", "d", "e", "f", "g", "h"} {
			r.Pool.Sizes["s"+b] = b
		}
	},
		box("a", "sa", "linux", "1"), box("b", "sb", "linux", "1"), box("c", "sc", "linux", "1"), box("d", "sd", "linux", "1"),
		box("e", "se", "linux", "1"), box("f", "sf", "linux", "1"), box("g", "sg", "linux", "1"), box("h", "sh", "linux", "1"))
	if w := f.link(t, inst, "good"); w.Code != http.StatusOK {
		t.Fatalf("link: %d %s", w.Code, w.Body)
	}
	ctx := context.Background()
	f.gh.mu.Lock()
	f.gh.attempt[302] = 3
	f.gh.noWrite["acme/lib"] = true
	f.gh.mu.Unlock()
	type serve struct {
		job, run  int64
		repo, box string
	}
	first := []serve{{9101, 300, "acme/app", "a"}, {9102, 301, "acme/app", "d"}, {9105, 302, "acme/app", "c"}, {9106, 303, "acme/app", "f"},
		{9107, 303, "acme/app", "g"}, {9108, 304, "acme/lib", "e"}, {9109, 305, "acme/app", "h"}}
	for batch := range slices.Chunk(first, 3) { // three serves at once (Serves): a slow machine boots three boxes per wait
		for _, s := range batch {
			f.deliver(t, "workflow_job", jobEvent("queued", s.job, s.run, s.repo, true, "pool:s"+s.box))
		}
		eventually(t, "the jobs on their boxes", func() bool {
			for _, s := range batch {
				if f.liveOn(t, s.box) != 1 {
					return false
				}
			}
			return f.svc.Undone(ctx) == 0
		})
	}
	for _, s := range first {
		runner := fmt.Sprintf("potluck-%d", s.job)
		switch s.job { // run 303's two jobs each take the other's runner
		case 9106:
			runner = "potluck-9107"
		case 9107:
			runner = "potluck-9106"
		}
		f.started(t, s.job, s.run, s.repo, runner)
	}
	eventually(t, "the starts applied", func() bool { return f.svc.Undone(ctx) == 0 })
	// A job that fails on its own, on a box nobody took, is not re-run.
	f.runIs(301, "completed")
	f.deliver(t, "workflow_job", endEvent(9102, 301, "acme/app", "failure", 1, "pool:sd"))
	eventually(t, "9102 done", func() bool { return f.jobs(t)[9102].State == "done" })
	f.svc.Watch(ctx) // every box is still there: nothing is lost
	if j := f.jobs(t)[9101]; j.State != "serving" {
		t.Fatalf("job 9101 before its box was taken back: %+v", j)
	}

	// Boxes a, e, f and g are taken back mid-job.
	for _, job := range []int64{9101, 9108, 9106, 9107} {
		f.takeBack(t, job)
	}
	lost := func(ids ...int64) func() bool {
		return func() bool {
			f.svc.Watch(ctx)
			jobs := f.jobs(t)
			for _, id := range ids {
				// Run 303's rows are of the jobs whose runners 9106 and 9107 took:
				// their boxes went, and GitHub has those jobs running elsewhere.
				j := jobs[id]
				if j.State == "serving" || (id != 9106 && id != 9107 && !strings.Contains(j.Why, "took its box back")) {
					return false
				}
			}
			return true
		}
	}
	eventually(t, "the losses recorded while GitHub still has the jobs running", lost(9101, 9106, 9107, 9108))
	if got := f.reruns(); len(got) != 0 {
		t.Fatalf("re-run before GitHub ended the jobs: %v", got)
	}
	if j := f.jobs(t)[9101]; j.State != "lost" {
		t.Errorf("job 9101 once its box was taken back: %+v", j)
	}
	why := func(id int64, parts ...string) func() bool {
		return func() bool {
			j := f.jobs(t)[id]
			for _, p := range parts {
				if !strings.Contains(j.Why, p) {
					return false
				}
			}
			return true
		}
	}
	// GitHub fails 9101 (the runner's lost-communication timeout); its run
	// still has a job running, so the re-run waits for it.
	f.deliver(t, "workflow_job", endEvent(9101, 300, "acme/app", "failure", 1, "pool:sa"))
	eventually(t, "9101 waiting for its run", why(9101, "took its box back", "once the rest of its run ends"))
	time.Sleep(300 * time.Millisecond) // a few retries of the waiting re-run
	if got := f.reruns(); len(got) != 0 {
		t.Fatalf("re-run while the run still ran: %v", got)
	}
	f.runIs(300, "completed")
	eventually(t, "9101 re-queued", func() bool { return slices.Equal(f.reruns(), []string{"job 9101"}) })
	eventually(t, "9101's row says so", why(9101, "took its box back", "re-queued: attempt 2 of 3"))
	if j := f.jobs(t)[9101]; j.State != "done" {
		t.Errorf("job 9101 after its re-run: %+v", j)
	}
	// Run 303: both jobs end failed, then the run: one call for the run.
	f.deliver(t, "workflow_job", endEvent(9106, 303, "acme/app", "failure", 1, "pool:sf"))
	f.deliver(t, "workflow_job", endEvent(9107, 303, "acme/app", "failure", 1, "pool:sg"))
	eventually(t, "9106 and 9107 waiting for their run", func() bool {
		return why(9106, "once the rest of its run ends")() && why(9107, "once the rest of its run ends")()
	})
	f.runIs(303, "completed")
	eventually(t, "run 303's failed jobs re-queued", func() bool { return slices.Equal(f.reruns(), []string{"job 9101", "run 303"}) })
	eventually(t, "both rows say so", func() bool {
		return why(9106, "re-queued: attempt 2 of 3")() && why(9107, "re-queued: attempt 2 of 3")()
	})
	// Run 304: the installation was never granted Actions: write.
	f.runIs(304, "completed")
	f.deliver(t, "workflow_job", endEvent(9108, 304, "acme/lib", "failure", 1, "pool:se"))
	eventually(t, "9108 not re-run, saying what to add", why(9108, "took its box back", "not re-run", "Actions: write"))

	// Attempt 2 of run 300 is a new job, served on box b like any queued job.
	f.gh.mu.Lock()
	f.gh.runStatus[300] = "in_progress"
	f.gh.mu.Unlock()
	f.deliver(t, "workflow_job", jobEvent("queued", 9111, 300, "acme/app", true, "pool:sb"))
	eventually(t, "attempt 2 on box b", func() bool { return f.liveOn(t, "b") == 1 })
	f.started(t, 9111, 300, "acme/app", "potluck-9111")
	eventually(t, "applied", func() bool { return f.svc.Undone(ctx) == 0 })

	// Boxes b, c and h are taken back.
	for _, job := range []int64{9111, 9105, 9109} {
		f.takeBack(t, job)
	}
	eventually(t, "the second losses recorded", lost(9111, 9105, 9109))
	f.runIs(300, "completed")
	f.deliver(t, "workflow_job", endEvent(9111, 300, "acme/app", "failure", 2, "pool:sb"))
	eventually(t, "9111 re-queued for the last time", why(9111, "re-queued: attempt 3 of 3"))
	// 9105 was on attempt 3 already; GitHub's completed never arrives.
	f.gh.mu.Lock()
	f.gh.jobDone[9105], f.gh.conclusion[9105], f.gh.runStatus[302] = true, "failure", "completed"
	f.gh.mu.Unlock()
	f.svc.Sweep(ctx)
	eventually(t, "9105 not re-run after attempt 3", why(9105, "took its box back", "attempt 3 of 3", "not re-run"))
	// 9109 was cancelled by a person after its box went.
	f.runIs(305, "completed")
	f.deliver(t, "workflow_job", endEvent(9109, 305, "acme/app", "cancelled", 1, "pool:sh"))
	eventually(t, "9109 not re-run: cancelled", why(9109, "took its box back", "cancelled", "not re-run"))
	eventually(t, "nothing left to apply", func() bool { return f.svc.Undone(ctx) == 0 })
	time.Sleep(300 * time.Millisecond)
	if got, want := f.reruns(), []string{"job 9101", "run 303", "job 9111"}; !slices.Equal(got, want) {
		t.Errorf("re-runs asked %v, want %v", got, want)
	}
	for _, box := range []string{"a", "b", "c", "e", "f", "g", "h"} {
		if n := f.liveOn(t, box); n != 0 {
			t.Errorf("box %s still holds %d handle(s)", box, n)
		}
	}
}

// pinned is a pool of boxes a, b, ..., each the one box of size s<name>, so
// a job asking pool:s<name> lands on it.
func pinned(t *testing.T, names ...string) *fixture {
	t.Helper()
	return pinnedStarting(t, nil, names...)
}

// pinnedStarting is pinned with before applied to the broker just before it
// starts.
func pinnedStarting(t *testing.T, before func(*runners.Service), names ...string) *fixture {
	t.Helper()
	var boxes []computeapi.RunTarget
	for _, n := range names {
		boxes = append(boxes, box(n, "s"+n, "linux", "1"))
	}
	f := newPoolFixtureStarting(t, func(r *config.Runners) {
		r.Pool.Sizes = map[string]string{}
		for _, n := range names {
			r.Pool.Sizes["s"+n] = n
		}
	}, before, boxes...)
	if w := f.link(t, inst, "good"); w.Code != http.StatusOK {
		t.Fatalf("link: %d %s", w.Code, w.Body)
	}
	return f
}

// serveOn queues job of run for box and waits until the box holds it.
func (f *fixture) serveOn(t *testing.T, job, run int64, box string) {
	t.Helper()
	f.deliver(t, "workflow_job", jobEvent("queued", job, run, "acme/app", true, "pool:s"+box))
	eventuallyBox(t, fmt.Sprintf("job %d on box %s", job, box), func() bool {
		return f.liveOn(t, box) == 1 && f.svc.Undone(context.Background()) == 0
	})
}

// A host that stops listing its bindings (its daemon restarts, the network
// to it drops) has not said any box ended: a job running there stays served,
// its box is never released, and if it then fails on its own it is not
// re-run. So too for a job GitHub says runs there whose in_progress the
// broker has not applied yet.
func TestUnlistedHostDoesNotLoseARunningJob(t *testing.T) {
	f := pinned(t, "a", "b")
	ctx := context.Background()
	f.svc.HostGrace = 0
	f.serveOn(t, 9201, 400, "a")
	f.serveOn(t, 9202, 401, "b")
	f.started(t, 9201, 400, "acme/app", "potluck-9201")
	eventually(t, "the start applied", func() bool { return f.svc.Undone(ctx) == 0 })
	f.gh.mu.Lock() // 9202 runs too, as GitHub has it; its in_progress is still on its way
	f.gh.runnerOf[9202], f.gh.jobRun[9202] = "potluck-9202", 401
	f.gh.mu.Unlock()
	f.svc.HideTargets("a", "b")
	for range 3 {
		f.svc.Watch(ctx)
	}
	f.svc.Sweep(ctx)
	f.svc.HideTargets()
	time.Sleep(300 * time.Millisecond) // a release would be in the background
	jobs := f.jobs(t)
	for _, id := range []int64{9201, 9202} {
		if j := jobs[id]; j.State != "serving" || j.Why != "" {
			t.Errorf("job %d while its host was unlisted: %+v", id, j)
		}
	}
	if a, b := f.liveOn(t, "a"), f.liveOn(t, "b"); a != 1 || b != 1 {
		t.Fatalf("boxes live after the host was unlisted: a %d, b %d; want both", a, b)
	}
	// It fails on its own afterwards: not re-run.
	f.runIs(400, "completed")
	f.deliver(t, "workflow_job", endEvent(9201, 400, "acme/app", "failure", 1, "pool:sa"))
	eventually(t, "9201 done", func() bool { return f.jobs(t)[9201].State == "done" && f.svc.Undone(ctx) == 0 })
	time.Sleep(300 * time.Millisecond)
	if got := f.reruns(); len(got) != 0 {
		t.Errorf("a job that failed on its own was re-run: %v", got)
	}
}

// Two jobs of one run lose their boxes. GitHub's completed for the second
// arrives after the run is already completed: the re-run waits for it, then
// asks once for the run's failed jobs, never for the first job alone (GitHub
// would refuse the second while that re-run runs).
func TestTwoLostJobsOfARunAreRerunTogether(t *testing.T) {
	f := pinned(t, "a", "b")
	ctx := context.Background()
	f.serveOn(t, 9301, 500, "a")
	f.serveOn(t, 9302, 500, "b")
	f.started(t, 9301, 500, "acme/app", "potluck-9301")
	f.started(t, 9302, 500, "acme/app", "potluck-9302")
	eventually(t, "the starts applied", func() bool { return f.svc.Undone(ctx) == 0 })
	f.takeBack(t, 9301)
	f.takeBack(t, 9302)
	eventually(t, "both losses recorded", func() bool {
		f.svc.Watch(ctx)
		jobs := f.jobs(t)
		return jobs[9301].State == "lost" && jobs[9302].State == "lost"
	})
	f.runIs(500, "completed")
	f.deliver(t, "workflow_job", endEvent(9301, 500, "acme/app", "failure", 1, "pool:sa"))
	eventually(t, "9301 ended", func() bool { return strings.Contains(f.jobs(t)[9301].Why, "GitHub failed it") })
	time.Sleep(500 * time.Millisecond) // several tries of the waiting re-run
	if got := f.reruns(); len(got) != 0 {
		t.Fatalf("re-run before the run's other lost job ended: %v", got)
	}
	f.deliver(t, "workflow_job", endEvent(9302, 500, "acme/app", "failure", 1, "pool:sb"))
	eventually(t, "the run's failed jobs re-queued", func() bool { return len(f.reruns()) > 0 && f.svc.Undone(ctx) == 0 })
	if got := f.reruns(); !slices.Equal(got, []string{"run 500"}) {
		t.Errorf("re-runs asked %v, want one for the run's failed jobs", got)
	}
	jobs := f.jobs(t)
	for _, id := range []int64{9301, 9302} {
		if j := jobs[id]; j.State != "done" || !strings.Contains(j.Why, "re-queued: attempt 2 of 3") {
			t.Errorf("job %d: %+v", id, j)
		}
	}
}

// A run cancelled after its lost job failed (a person, or a newer run under
// concurrency: cancel-in-progress) is not re-run: the re-run could cancel
// the newer run.
func TestCancelledRunIsNotRerun(t *testing.T) {
	f := pinned(t, "a")
	ctx := context.Background()
	f.serveOn(t, 9401, 600, "a")
	f.started(t, 9401, 600, "acme/app", "potluck-9401")
	eventually(t, "the start applied", func() bool { return f.svc.Undone(ctx) == 0 })
	f.takeBack(t, 9401)
	eventually(t, "the loss recorded", func() bool { f.svc.Watch(ctx); return f.jobs(t)[9401].State == "lost" })
	f.gh.mu.Lock()
	f.gh.runStatus[600], f.gh.runEnd[600] = "completed", "cancelled"
	f.gh.mu.Unlock()
	f.deliver(t, "workflow_job", endEvent(9401, 600, "acme/app", "failure", 1, "pool:sa"))
	eventually(t, "applied", func() bool { return f.svc.Undone(ctx) == 0 })
	if got := f.reruns(); len(got) != 0 {
		t.Errorf("a cancelled run was re-run: %v", got)
	}
	if j := f.jobs(t)[9401]; !strings.Contains(j.Why, "its run ended cancelled") || !strings.Contains(j.Why, "not re-run") {
		t.Errorf("job 9401: %+v", j)
	}
}

// A lost job of a repository disconnected since it ran is not re-run: the
// broker asks GitHub for a re-run only of a repository it would serve.
func TestLostJobOfADisconnectedRepositoryIsNotRerun(t *testing.T) {
	f := pinned(t, "a")
	ctx := context.Background()
	f.serveOn(t, 9501, 610, "a")
	f.started(t, 9501, 610, "acme/app", "potluck-9501")
	eventually(t, "the start applied", func() bool { return f.svc.Undone(ctx) == 0 })
	f.takeBack(t, 9501)
	eventually(t, "the loss recorded", func() bool { f.svc.Watch(ctx); return f.jobs(t)[9501].State == "lost" })
	if err := f.owner.Do(ctx, "DELETE", "/v0/runners/repos/acme/app", nil, nil); err != nil {
		t.Fatal(err)
	}
	f.runIs(610, "completed")
	f.deliver(t, "workflow_job", endEvent(9501, 610, "acme/app", "failure", 1, "pool:sa"))
	eventually(t, "applied", func() bool { return f.svc.Undone(ctx) == 0 })
	if got := f.reruns(); len(got) != 0 {
		t.Errorf("a disconnected repository's run was re-run: %v", got)
	}
	if j := f.jobs(t)[9501]; !strings.Contains(j.Why, "not re-run: acme/app is not connected") {
		t.Errorf("job 9501: %+v", j)
	}
}

// Two jobs run on each other's runners. The one that ends first frees the
// box it ran on, never the box its own runner was minted on, where the other
// job still runs.
func TestSwappedJobEndingReleasesTheBoxItRanOn(t *testing.T) {
	f := pinned(t, "a", "b")
	ctx := context.Background()
	f.serveOn(t, 9501, 700, "a")
	f.serveOn(t, 9502, 700, "b")
	f.started(t, 9501, 700, "acme/app", "potluck-9502") // 9501 runs on box b
	f.started(t, 9502, 700, "acme/app", "potluck-9501") // 9502 runs on box a
	eventually(t, "the starts applied", func() bool { return f.svc.Undone(ctx) == 0 })
	f.gh.mu.Lock()
	f.gh.jobDone[9501] = true
	f.gh.mu.Unlock()
	f.deliver(t, "workflow_job", endEvent(9501, 700, "acme/app", "success", 1, "pool:sa"))
	eventually(t, "9501's end applied", func() bool { return f.svc.Undone(ctx) == 0 && f.liveOn(t, "a")+f.liveOn(t, "b") == 1 })
	if a, b := f.liveOn(t, "a"), f.liveOn(t, "b"); a != 1 || b != 0 {
		t.Fatalf("after 9501 (on box b) ended: box a %d, box b %d; want a kept for 9502, b released", a, b)
	}
	f.svc.Sweep(ctx) // 9502 still runs on box a: kept and renewed
	if a := f.liveOn(t, "a"); a != 1 {
		t.Fatalf("the sweep released box a under job 9502")
	}
	f.gh.mu.Lock()
	f.gh.jobDone[9502] = true
	f.gh.mu.Unlock()
	f.deliver(t, "workflow_job", endEvent(9502, 700, "acme/app", "success", 1, "pool:sb"))
	eventually(t, "box a released when 9502 ended", func() bool { return f.liveOn(t, "a") == 0 && f.svc.Undone(ctx) == 0 })
	if _, del := f.gh.snapshot(); len(del) != 2 {
		t.Errorf("runners deleted %v, want both", del)
	}
}

// A job that took another's runner ends while that other job is still
// queued: the other job is served again on a new runner, and nothing later
// (a sweep, a second completed delivery for the ended job) takes that new box
// for the ended job's.
func TestReservedRowForgetsWhoRanOnItsOldRunner(t *testing.T) {
	f := pinned(t, "a", "b")
	ctx := context.Background()
	f.gh.mu.Lock()
	f.gh.status[9602] = "queued" // GitHub has it waiting for a runner throughout
	f.gh.mu.Unlock()
	f.serveOn(t, 9601, 800, "a")
	f.serveOn(t, 9602, 800, "b")
	f.started(t, 9601, 800, "acme/app", "potluck-9602") // 9601 runs on box b; 9602 stays queued
	eventually(t, "the start applied", func() bool { return f.svc.Undone(ctx) == 0 })
	f.gh.mu.Lock()
	f.gh.jobDone[9601] = true
	f.gh.mu.Unlock()
	f.deliver(t, "workflow_job", endEvent(9601, 800, "acme/app", "success", 1, "pool:sa"))
	served := func() bool {
		j := f.jobs(t)[9602]
		return j.State == "serving" && j.Runner == "potluck-9602-2" && f.liveOn(t, "b") == 1 && f.svc.Undone(ctx) == 0
	}
	eventually(t, "9602 served again on a new runner", served)
	f.svc.Sweep(ctx)
	f.deliver(t, "workflow_job", endEvent(9601, 800, "acme/app", "success", 1, "pool:sa")) // GitHub delivers it twice
	eventually(t, "applied", func() bool { return f.svc.Undone(ctx) == 0 })
	f.svc.Sweep(ctx)
	time.Sleep(300 * time.Millisecond)
	if !served() {
		t.Errorf("job 9602 after the sweeps: %+v, %d box(es) on b, minted %v; want it still serving on potluck-9602-2",
			f.jobs(t)[9602], f.liveOn(t, "b"), f.minted())
	}
}

// A box that ended by itself was not taken back (zm-eakh.12.53 item 8). No
// conforming box answers "ended" (docs/protocol.md: a runner's exit leaves
// its box ready), so a driver that does is saying the box's own process
// finished. A Watch that lands between a job's end and its completed
// delivery finds such a box: the job is not recorded as lost, whether GitHub
// still has it in_progress on that runner for a moment (9501) or has it
// completed already (9502), its row gets no "took its box back", and a job
// that then concludes failure for its own reasons is not re-run. A job that
// had not started on such a box (9503) is served again.
func TestBoxThatEndedByItselfIsNotALoss(t *testing.T) {
	f := pinned(t, "a", "b", "c")
	ctx := context.Background()
	f.serveOn(t, 9501, 500, "a")
	f.serveOn(t, 9502, 501, "b")
	f.serveOn(t, 9503, 502, "c")
	f.started(t, 9501, 500, "acme/app", "potluck-9501")
	f.started(t, 9502, 501, "acme/app", "potluck-9502")
	eventually(t, "the starts applied", func() bool { return f.svc.Undone(ctx) == 0 })
	f.gh.mu.Lock()
	f.gh.jobDone[9502], f.gh.conclusion[9502] = true, "failure"
	f.gh.status[9503] = "queued"
	f.gh.mu.Unlock()
	ended := map[string]bool{"a": true, "b": true, "c": true}
	f.svc.AnswerStatus(func(target string, h computeapi.RunTargetHandle, err error) (computeapi.RunTargetHandle, error) {
		if err == nil && ended[target] {
			h.State = "ended"
		}
		return h, err
	})
	f.svc.Watch(ctx)
	eventually(t, "9503, not started, served again", func() bool {
		j := f.jobs(t)[9503]
		return j.State == "serving" && j.Runner == "potluck-9503-2" && f.svc.Undone(ctx) == 0
	})
	delete(ended, "c") // its new box
	for range 2 {
		f.svc.Watch(ctx)
	}
	time.Sleep(300 * time.Millisecond) // a release would be in the background
	jobs := f.jobs(t)
	for _, id := range []int64{9501, 9502} {
		if j := jobs[id]; j.State != "serving" || j.Why != "" {
			t.Errorf("job %d after its box ended by itself: %+v", id, j)
		}
	}
	f.runIs(500, "completed")
	f.runIs(501, "completed")
	f.deliver(t, "workflow_job", endEvent(9501, 500, "acme/app", "failure", 1, "pool:sa"))
	f.deliver(t, "workflow_job", endEvent(9502, 501, "acme/app", "failure", 1, "pool:sb"))
	eventually(t, "both done", func() bool {
		jobs := f.jobs(t)
		return jobs[9501].State == "done" && jobs[9502].State == "done" && f.svc.Undone(ctx) == 0
	})
	time.Sleep(300 * time.Millisecond)
	for _, id := range []int64{9501, 9502} {
		if j := f.jobs(t)[id]; strings.Contains(j.Why, "took its box back") {
			t.Errorf("job %d ended on its own and its row says: %s", id, j.Why)
		}
	}
	if got := f.reruns(); len(got) != 0 {
		t.Errorf("a job that failed on its own was re-run: %v", got)
	}
}

// A listed host that answers not_found for anything but the handle ("this
// machine is not in that project": it dropped out of the pool, or is mid
// restart) has said nothing about the box (zm-eakh.12.53 item 9): a job
// running there stays served, its box is not released, and if it then fails
// on its own it is not re-run.
func TestHostOutOfItsProjectDoesNotLoseARunningJob(t *testing.T) {
	f := pinned(t, "a")
	ctx := context.Background()
	f.serveOn(t, 9601, 600, "a")
	f.started(t, 9601, 600, "acme/app", "potluck-9601")
	eventually(t, "the start applied", func() bool { return f.svc.Undone(ctx) == 0 })
	f.svc.AnswerStatus(func(string, computeapi.RunTargetHandle, error) (computeapi.RunTargetHandle, error) {
		return computeapi.RunTargetHandle{}, api.Errorf(api.CodeNotFound, "this machine is not in that project")
	})
	for range 3 {
		f.svc.Watch(ctx)
	}
	f.svc.AnswerStatus(nil)
	time.Sleep(300 * time.Millisecond) // a release would be in the background
	if j := f.jobs(t)[9601]; j.State != "serving" || j.Why != "" {
		t.Errorf("job 9601 while its host was out of the project: %+v", j)
	}
	if n := f.liveOn(t, "a"); n != 1 {
		t.Fatalf("box a holds %d handle(s) after its host was out of the project; want 1", n)
	}
	f.runIs(600, "completed")
	f.deliver(t, "workflow_job", endEvent(9601, 600, "acme/app", "failure", 1, "pool:sa"))
	eventually(t, "9601 done", func() bool { return f.jobs(t)[9601].State == "done" && f.svc.Undone(ctx) == 0 })
	time.Sleep(300 * time.Millisecond)
	if got := f.reruns(); len(got) != 0 {
		t.Errorf("a job that failed on its own was re-run: %v", got)
	}
}

// A host on a build that cannot say "no such handle" answers a plain
// not_found for a box that is gone, which the pool's machine reports as
// unavailable (nobody spoke about the box): a job that has not started stays
// where it is. The run-target sweep records the box lost 15 minutes past its
// lease, its status then answers that row, and Watch serves the job again.
func TestBoxTheSweepMarkedLostIsServedAgain(t *testing.T) {
	f := pinned(t, "a")
	ctx := context.Background()
	f.gh.mu.Lock()
	f.gh.status[9701] = "queued"
	f.gh.mu.Unlock()
	f.serveOn(t, 9701, 700, "a")
	swept := false
	f.svc.AnswerStatus(func(target string, h computeapi.RunTargetHandle, err error) (computeapi.RunTargetHandle, error) {
		if !swept {
			return computeapi.RunTargetHandle{}, api.Errorf(api.CodeUnavailable, "run target a could not be asked about its box: no handle")
		}
		h.State = "lost"
		return h, err
	})
	for range 3 {
		f.svc.Watch(ctx)
	}
	if j := f.jobs(t)[9701]; j.State != "serving" || j.Runner != "potluck-9701" {
		t.Fatalf("job 9701 while its host could not say: %+v", j)
	}
	swept = true
	f.svc.Watch(ctx)
	f.svc.AnswerStatus(nil)
	eventually(t, "9701 served again", func() bool {
		j := f.jobs(t)[9701]
		return j.State == "serving" && j.Runner == "potluck-9701-2" && f.svc.Undone(ctx) == 0
	})
}
