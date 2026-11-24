package runners_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/runners"
)

func TestWorkflowLimitThroughBroker(t *testing.T) {
	for _, c := range []struct {
		name, event, yaml, wantSource string
		ttl                           int
	}{
		{"literal job key", "workflow_dispatch", "jobs:\n  Compile:\n    timeout-minutes: 12\n", "Compile", 720},
		{"duplicate literal names", "push", "jobs:\n  a:\n    name: Compile\n    timeout-minutes: 12\n  b:\n    name: Compile\n    timeout-minutes: 15\n", "ambiguous", 3000},
		{"matrix identity", "push", "jobs:\n  build:\n    name: Compile\n    strategy:\n      matrix:\n        go: [1, 2]\n    timeout-minutes: 12\n", "matrix", 3000},
		{"reusable workflow", "push", "jobs:\n  build:\n    uses: acme/other/.github/workflows/ci.yml@main\n", "reusable", 3000},
		{"omitted workflow timeout", "push", "jobs:\n  build:\n    name: Compile\n", "omits timeout-minutes", 3000},
		{"literal custom name", "push", "jobs:\n  build:\n    name: Compile\n    timeout-minutes: 12\n    runs-on: pool:sa\n", "build", 720},
		{"expression", "push", "jobs:\n  build:\n    name: Compile\n    timeout-minutes: ${{ inputs.minutes }}\n", "expression", 3000},
		{"ambiguous PR source", "pull_request", "jobs:\n  build:\n    name: Compile\n    timeout-minutes: 12\n", "pull_request", 3000},
		{"competing dynamic name", "push", "jobs:\n  build:\n    name: Compile\n    timeout-minutes: 12\n  other:\n    name: ${{ matrix.name }}\n    timeout-minutes: 30\n", "dynamic", 3000},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := pinned(t, "a")
			f.gh.mu.Lock()
			f.gh.workflow[980] = fmt.Sprintf(`{"id":980,"event":%q,"path":".github/workflows/ci.yml","head_sha":%q,"repository":{"full_name":"acme/app"},"head_repository":{"full_name":"acme/app"}}`, c.event, strings.Repeat("a", 40))
			f.gh.jobNames[9980] = "Compile"
			f.gh.contents = c.yaml
			f.gh.mu.Unlock()
			f.serveOn(t, 9980, 980, "a")
			eventually(t, "selected runtime reaches acquisition", func() bool {
				b, _ := os.ReadFile(f.seen)
				return strings.Contains(string(b), fmt.Sprintf(`"ttl_s":%d`, c.ttl+300))
			})
			var out struct {
				Jobs []struct {
					ID     int64  `json:"job_id"`
					TTL    int    `json:"limit_s"`
					Source string `json:"limit_source"`
				}
			}
			if err := f.owner.Do(context.Background(), "GET", "/v0/runners/jobs", nil, &out); err != nil {
				t.Fatal(err)
			}
			b, _ := json.Marshal(out)
			if len(out.Jobs) != 1 || out.Jobs[0].TTL != c.ttl || !strings.Contains(out.Jobs[0].Source, c.wantSource) {
				t.Fatalf("job limit/provenance: %s; want %d and %q", b, c.ttl, c.wantSource)
			}
		})
	}
}

func workflowFor(f *fixture, run, job int64, minutes int, start string) {
	f.gh.mu.Lock()
	defer f.gh.mu.Unlock()
	f.gh.workflow[run] = fmt.Sprintf(`{"id":%d,"event":"push","path":".github/workflows/ci.yml","head_sha":%q,"repository":{"full_name":"acme/app"},"head_repository":{"full_name":"acme/app"}}`, run, strings.Repeat("a", 40))
	f.gh.jobNames[job] = "Compile"
	f.gh.jobStarts[job] = start
	f.gh.contents = fmt.Sprintf("jobs:\n  build:\n    name: Compile\n    timeout-minutes: %d\n", minutes)
}

func TestRuntimeDeadlineStopsWithoutRetry(t *testing.T) {
	f := pinned(t, "a")
	ctx := context.Background()
	base := time.Now().Truncate(time.Second)
	var elapsed atomic.Int64
	f.svc.Now = func() time.Time { return base.Add(time.Duration(elapsed.Load()) * time.Second) }
	workflowFor(f, 981, 9981, 1, base.Format(time.RFC3339))
	f.serveOn(t, 9981, 981, "a")
	f.started(t, 9981, 981, "acme/app", "potluck-9981")
	eventually(t, "runtime deadline stored", func() bool { return f.jobs(t)[9981].Deadline == base.Unix()+60 })
	before := f.jobs(t)[9981]
	elapsed.Store(30)
	f.started(t, 9981, 981, "acme/app", "potluck-9981")
	eventually(t, "duplicate start applied", func() bool { return f.svc.Undone(ctx) == 0 })
	if got := f.jobs(t)[9981].Deadline; got != before.Deadline {
		t.Fatalf("duplicate moved deadline: %d -> %d", before.Deadline, got)
	}
	f.svc.Sweep(ctx)
	if got := f.renews(t); len(got) == 0 || got[len(got)-1] != 30 {
		t.Fatalf("renew must stop at absolute deadline: %v", got)
	}
	elapsed.Store(61)
	f.svc.Watch(ctx)
	eventuallyBox(t, "intentional runtime timeout", func() bool { return f.jobs(t)[9981].State == "done" && f.liveOn(t, "a") == 0 })
	if why := f.jobs(t)[9981].Why; !strings.Contains(why, "job time limit reached (60 s") || !strings.Contains(why, "not re-run") {
		t.Fatal(why)
	}
	f.gh.mu.Lock()
	f.gh.status[9981] = "completed"
	f.gh.conclusion[9981] = "failure"
	f.gh.runStatus[981] = "completed"
	f.gh.jobRun[9981] = 981
	f.gh.mu.Unlock()
	f.deliver(t, "workflow_job", endEvent(9981, 981, "acme/app", "failure", 1, "pool:sa"))
	eventually(t, "completion applied", func() bool { return f.svc.Undone(ctx) == 0 })
	f.svc.Sweep(ctx)
	f.gh.mu.Lock()
	defer f.gh.mu.Unlock()
	if len(f.gh.reruns) != 0 {
		t.Fatalf("intentional timeout retried: %v", f.gh.reruns)
	}
}

func TestExplicitWorkflowLimitDoesNotFitHost(t *testing.T) {
	f := pinned(t, "a")
	workflowFor(f, 982, 9982, 400, "")
	f.deliver(t, "workflow_job", jobEvent("queued", 9982, 982, "acme/app", true, "pool:sa"))
	eventually(t, "host ceiling refusal", func() bool { j := f.jobs(t)[9982]; return j.State == "waiting" && strings.Contains(j.Why, "24300") })
	j := f.jobs(t)[9982]
	if j.LimitS != 24000 || !strings.Contains(j.LimitSource, "job build") || len(f.minted()) != 0 {
		t.Fatalf("workflow limit silently shortened or acquired: %+v", j)
	}
}

func TestWorkflowPermissionFallbackIsVisible(t *testing.T) {
	f := pinned(t, "a")
	workflowFor(f, 983, 9983, 1, "")
	f.gh.mu.Lock()
	f.gh.noContents = true
	f.gh.mu.Unlock()
	f.serveOn(t, 9983, 983, "a")
	j := f.jobs(t)[9983]
	if j.LimitS != 3000 || !strings.Contains(j.LimitSource, "Contents: read") || !strings.Contains(j.LimitSource, "permission update") {
		t.Fatalf("missing permission hidden: %+v", j)
	}
}

func TestGitHubTimeoutConclusionHasReason(t *testing.T) {
	for _, conclusion := range []string{"timed_out", "success", "failure", "cancelled"} {
		t.Run(conclusion, func(t *testing.T) {
			f := pinned(t, "a")
			ctx := context.Background()
			workflowFor(f, 984, 9984, 1, time.Now().Add(-61*time.Second).UTC().Format(time.RFC3339))
			f.serveOn(t, 9984, 984, "a")
			f.started(t, 9984, 984, "acme/app", "potluck-9984")
			eventually(t, "start applied", func() bool { return f.svc.Undone(ctx) == 0 })
			f.gh.mu.Lock()
			f.gh.status[9984] = "completed"
			f.gh.conclusion[9984] = conclusion
			f.gh.mu.Unlock()
			f.svc.Watch(ctx)
			eventually(t, "completed", func() bool { return f.jobs(t)[9984].State == "done" })
			why := f.jobs(t)[9984].Why
			if conclusion == "timed_out" {
				if !strings.Contains(why, "GitHub stopped the job at its workflow time limit") {
					t.Fatal(why)
				}
			} else if strings.Contains(why, "time limit") {
				t.Fatalf("invented timeout for %s: %s", conclusion, why)
			}
		})
	}
}

func TestConcurrentHostLossCannotRetryIntentionalTimeout(t *testing.T) {
	f := pinned(t, "a")
	ctx := context.Background()
	base := time.Now().Truncate(time.Second)
	var elapsed atomic.Int64
	f.svc.Now = func() time.Time { return base.Add(time.Duration(elapsed.Load()) * time.Second) }
	workflowFor(f, 985, 9985, 1, base.Format(time.RFC3339))
	f.serveOn(t, 9985, 985, "a")
	f.started(t, 9985, 985, "acme/app", "potluck-9985")
	eventually(t, "start applied", func() bool { return f.jobs(t)[9985].Deadline == base.Unix()+60 })
	entered, unblock, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var unblockOnce sync.Once
	t.Cleanup(func() { unblockOnce.Do(func() { close(unblock) }) })
	var parked atomic.Bool
	f.svc.AnswerStatus(func(_ string, h computeapi.RunTargetHandle, err error) (computeapi.RunTargetHandle, error) {
		if parked.CompareAndSwap(false, true) {
			close(entered)
			<-unblock
			h.State = "expired"
		}
		return h, err
	})
	go func() { defer close(finished); f.svc.Watch(ctx) }()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("watch did not reach host")
	}
	var released atomic.Bool
	f.svc.AfterRelease(func() {
		if released.CompareAndSwap(false, true) {
			unblockOnce.Do(func() { close(unblock) })
			select {
			case <-finished:
			case <-time.After(3 * time.Second):
				t.Error("concurrent loss did not finish")
			}
		}
	})
	elapsed.Store(61)
	f.svc.Watch(ctx)
	eventually(t, "intentional stop", func() bool { return f.jobs(t)[9985].State == "done" })
	f.gh.mu.Lock()
	f.gh.status[9985] = "completed"
	f.gh.conclusion[9985] = "failure"
	f.gh.runStatus[985] = "completed"
	f.gh.jobRun[9985] = 985
	f.gh.mu.Unlock()
	f.deliver(t, "workflow_job", endEvent(9985, 985, "acme/app", "failure", 1, "pool:sa"))
	eventually(t, "failure applied", func() bool { return f.svc.Undone(ctx) == 0 })
	f.svc.Sweep(ctx)
	f.gh.mu.Lock()
	defer f.gh.mu.Unlock()
	if len(f.gh.reruns) != 0 {
		t.Fatalf("intentional stop became host-loss retry: %v", f.gh.reruns)
	}
}

func TestSwappedJobTimeoutRequeuesOriginalJob(t *testing.T) {
	f := pinned(t, "a")
	ctx := context.Background()
	base := time.Now().Truncate(time.Second)
	var elapsed atomic.Int64
	f.svc.Now = func() time.Time { return base.Add(time.Duration(elapsed.Load()) * time.Second) }
	// The original job is queued; a different trusted job takes its runner.
	f.gh.mu.Lock()
	f.gh.status[9986] = "queued"
	f.gh.mu.Unlock()
	f.serveOn(t, 9986, 986, "a")
	workflowFor(f, 987, 9987, 1, base.Format(time.RFC3339))
	f.gh.mu.Lock()
	f.gh.runnerOf[9987] = "potluck-9986"
	f.gh.jobRun[9987] = 987
	f.gh.mu.Unlock()
	f.deliver(t, "workflow_job", []byte(strings.ReplaceAll(string(startEvent(9987, 987, "acme/app", "potluck-9986")), `"sengi"`, `"pool:sa"`)))
	eventually(t, "actual job owns runtime", func() bool { j := f.jobs(t)[9987]; return j.LimitS == 60 && j.Deadline == base.Unix()+60 })
	elapsed.Store(61)
	f.svc.Watch(ctx)
	eventuallyBox(t, "original queued job served again", func() bool { return f.jobs(t)[9986].Runner == "potluck-9986-2" && f.liveOn(t, "a") == 1 })
	actual := f.jobs(t)[9987]
	if actual.State != "done" || actual.Run != 987 || !strings.Contains(actual.Why, "job time limit reached (60 s") {
		t.Fatalf("actual timeout missing or wrong run: %+v", actual)
	}
	original := f.jobs(t)[9986]
	if original.LimitS != 3000 || !strings.Contains(original.LimitSource, "pull_request") {
		t.Fatalf("original reused taker's runtime: %+v", original)
	}
}

func TestRuntimeTimeoutRetriesFailedRelease(t *testing.T) {
	f := pinned(t, "a")
	ctx := context.Background()
	base := time.Now().Truncate(time.Second)
	var elapsed atomic.Int64
	f.svc.Now = func() time.Time { return base.Add(time.Duration(elapsed.Load()) * time.Second) }
	workflowFor(f, 988, 9988, 1, base.Format(time.RFC3339))
	f.serveOn(t, 9988, 988, "a")
	f.started(t, 9988, 988, "acme/app", "potluck-9988")
	eventually(t, "start applied", func() bool { return f.jobs(t)[9988].Deadline == base.Unix()+60 })
	var refuse atomic.Bool
	refuse.Store(true)
	f.svc.RefuseRelease(refuse.Load)
	elapsed.Store(61)
	f.svc.Watch(ctx)
	if j := f.jobs(t)[9988]; j.State != "serving" || f.liveOn(t, "a") != 1 || !strings.Contains(j.Why, "job time limit") {
		t.Fatalf("failed release lost its retryable box: %+v", j)
	}
	refuse.Store(false)
	f.svc.Watch(ctx)
	eventuallyBox(t, "release retried", func() bool { return f.jobs(t)[9988].State == "done" && f.liveOn(t, "a") == 0 })
}

func TestSweepDiscoversActualExecutionStart(t *testing.T) {
	f := pinned(t, "a")
	ctx := context.Background()
	base := time.Now().Truncate(time.Second)
	var elapsed atomic.Int64
	f.svc.Now = func() time.Time { return base.Add(time.Duration(elapsed.Load()) * time.Second) }
	workflowFor(f, 989, 9989, 1, base.Add(200*time.Second).Format(time.RFC3339))
	f.serveOn(t, 9989, 989, "a")
	// No in_progress delivery arrives. The periodic GitHub check discovers the
	// real start, two hundred seconds after the box was acquired.
	elapsed.Store(200)
	f.gh.mu.Lock()
	f.gh.runnerOf[9989] = "potluck-9989"
	f.gh.mu.Unlock()
	f.svc.Sweep(ctx)
	j := f.jobs(t)[9989]
	if j.StartedAt != base.Unix()+200 || j.Deadline != base.Unix()+260 {
		t.Fatalf("deadline used acquisition instead of actual start: %+v", j)
	}
	elapsed.Store(250)
	f.started(t, 9989, 989, "acme/app", "potluck-9989")
	eventually(t, "late start delivery applied", func() bool { return f.svc.Undone(ctx) == 0 })
	if f.jobs(t)[9989].Deadline != j.Deadline {
		t.Fatal("late delivery moved discovered start")
	}
}

func TestSwappedJobCompletionOrLossRequeuesOwnSelectedLimit(t *testing.T) {
	for _, ending := range []string{"success", "reclaimed"} {
		t.Run(ending, func(t *testing.T) {
			f := pinned(t, "a")

			base := time.Now().Truncate(time.Second)
			var elapsed atomic.Int64
			f.svc.Now = func() time.Time { return base.Add(time.Duration(elapsed.Load()) * time.Second) }
			// The original job is queued; a different trusted job takes its runner.
			f.gh.mu.Lock()
			f.gh.status[9986] = "queued"
			f.gh.mu.Unlock()
			f.serveOn(t, 9986, 986, "a")
			workflowFor(f, 987, 9987, 1, base.Format(time.RFC3339))
			f.gh.mu.Lock()
			f.gh.runnerOf[9987] = "potluck-9986"
			f.gh.jobRun[9987] = 987
			f.gh.mu.Unlock()
			f.deliver(t, "workflow_job", []byte(strings.ReplaceAll(string(startEvent(9987, 987, "acme/app", "potluck-9986")), `"sengi"`, `"pool:sa"`)))
			eventually(t, "actual job owns runtime", func() bool { j := f.jobs(t)[9987]; return j.LimitS == 60 && j.Deadline == base.Unix()+60 })
			// The taker had no box of its own: it keeps this one, and the
			// original job, still queued, is served again at once.
			eventuallyBox(t, "original queued job served again", func() bool {
				return f.jobs(t)[9986].Runner == "potluck-9986-2" && f.liveOn(t, "a") == 2 && f.svc.Undone(context.Background()) == 0
			})
			if _, err := f.set("job_ttl_s", "1800"); err != nil {
				t.Fatal(err)
			}
			elapsed.Store(30)
			if ending == "success" {
				f.gh.mu.Lock()
				f.gh.status[9987] = "completed"
				f.gh.conclusion[9987] = "success"
				f.gh.mu.Unlock()
				f.deliver(t, "workflow_job", endEvent(9987, 987, "acme/app", "success", 1, "pool:sa"))
			} else {
				f.svc.AnswerStatus(func(_ string, h computeapi.RunTargetHandle, err error) (computeapi.RunTargetHandle, error) {
					if err == nil && strings.HasSuffix(h.RequestID, "gh-9986") { // the taker's box, not the original's new one
						h.State = "reclaimed"
					}
					return h, err
				})
				f.svc.Watch(context.Background())
				f.svc.AnswerStatus(nil)
			}
			eventuallyBox(t, "the taker's box ended", func() bool {
				return f.jobs(t)[9987].State != "serving" && f.jobs(t)[9986].Runner == "potluck-9986-2" && f.liveOn(t, "a") == 1
			})
			actual := f.jobs(t)[9987]
			if ending == "success" && (actual.State != "done" || actual.Run != 987) {
				t.Fatalf("actual timeout missing or wrong run: %+v", actual)
			}
			original := f.jobs(t)[9986]
			if original.LimitS != 3000 || !strings.Contains(original.LimitSource, "pull_request") {
				t.Fatalf("original reused taker's runtime: %+v", original)
			}
			elapsed.Store(40)
			f.gh.mu.Lock()
			f.gh.jobStarts[9986] = base.Add(40 * time.Second).Format(time.RFC3339)
			f.gh.mu.Unlock()
			f.started(t, 9986, 986, "acme/app", "potluck-9986-2")
			eventually(t, "original selected policy drives actual execution", func() bool {
				j := f.jobs(t)[9986]
				return j.StartedAt == base.Unix()+40 && j.Deadline == base.Unix()+3040 && j.LimitS == 3000
			})
			elapsed.Store(100)
			f.started(t, 9986, 986, "acme/app", "potluck-9986-2")
			eventually(t, "duplicate original start applied", func() bool { return f.svc.Undone(context.Background()) == 0 })
			if f.jobs(t)[9986].Deadline != base.Unix()+3040 {
				t.Fatal("duplicate moved original execution deadline")
			}
		})
	}
}

func startOn(t *testing.T, f *fixture, job, run int64, runner, label string) {
	t.Helper()
	f.gh.mu.Lock()
	f.gh.status[job] = "in_progress"
	f.gh.runnerOf[job] = runner
	f.gh.jobRun[job] = run
	f.gh.mu.Unlock()
	f.deliver(t, "workflow_job", []byte(strings.ReplaceAll(string(startEvent(job, run, "acme/app", runner)), `"sengi"`, fmt.Sprintf("%q", label))))
}

func TestExistingWaitingTakerKeepsItsPolicyAndClock(t *testing.T) {
	f := pinned(t, "a")
	ctx := context.Background()
	base := time.Now().Truncate(time.Second)
	var elapsed atomic.Int64
	f.svc.Now = func() time.Time { return base.Add(time.Duration(elapsed.Load()) * time.Second) }
	f.gh.mu.Lock()
	f.gh.status[99101], f.gh.status[99102] = "queued", "queued"
	f.gh.jobStarts[99102] = base.Format(time.RFC3339)
	f.gh.mu.Unlock()
	f.serveOn(t, 99101, 991, "a")
	f.svc.HideTargets("a")
	f.deliver(t, "workflow_job", jobEvent("queued", 99102, 992, "acme/app", true, "pool:sa"))
	eventually(t, "taker selected3000 before settings change", func() bool { j := f.jobs(t)[99102]; return j.State == "waiting" && j.LimitS == 3000 })
	if _, err := f.set("job_ttl_s", "1800"); err != nil {
		t.Fatal(err)
	}
	startOn(t, f, 99102, 992, "potluck-99101", "pool:sa")
	eventually(t, "actual taker has its own truthful clock", func() bool {
		j := f.jobs(t)[99102]
		return j.StartedAt == base.Unix() && j.Deadline == base.Unix()+3000 && j.LimitS == 3000
	})
	f.svc.HideTargets()
	elapsed.Store(1801)
	f.svc.Watch(ctx)
	// The taker had no box of its own: it carries the box it took.
	if j := f.jobs(t)[99102]; j.State != "serving" || strings.Contains(j.Why, "time limit") {
		t.Fatalf("taker stopped at changed fallback instead of selected limit: %+v", j)
	}
	elapsed.Store(3001)
	f.svc.Watch(ctx)
	eventually(t, "actual taker stopped at selected3000", func() bool {
		j := f.jobs(t)[99102]
		return j.State == "done" && strings.Contains(j.Why, "job time limit reached (3000 s")
	})
}

func TestTakersOwnTimeoutLeavesAnotherJobOnItsBoxRunning(t *testing.T) {
	for _, ending := range []string{"success", "host loss"} {
		t.Run(ending, func(t *testing.T) {
			f := pinned(t, "a", "b")
			ctx := context.Background()
			base := time.Now().Truncate(time.Second)
			var elapsed atomic.Int64
			f.svc.Now = func() time.Time { return base.Add(time.Duration(elapsed.Load()) * time.Second) }
			f.gh.mu.Lock()
			f.gh.status[99111] = "queued"
			f.gh.mu.Unlock()
			f.serveOn(t, 99111, 993, "a")
			workflowFor(f, 994, 99112, 1, base.Format(time.RFC3339))
			f.serveOn(t, 99112, 994, "b")
			f.gh.mu.Lock()
			f.gh.jobStarts[99113] = base.Format(time.RFC3339)
			f.gh.mu.Unlock()
			startOn(t, f, 99113, 995, "potluck-99112", "pool:sb")
			eventually(t, "C starts on B's box", func() bool { return f.jobs(t)[99113].Deadline == base.Unix()+3000 })
			startOn(t, f, 99112, 994, "potluck-99111", "pool:sa")
			eventually(t, "B starts on A's box using its own60 seconds", func() bool { return f.jobs(t)[99112].Deadline == base.Unix()+60 })
			elapsed.Store(61)
			f.svc.Watch(ctx)
			f.svc.Sweep(ctx)
			b, c := f.jobs(t)[99112], f.jobs(t)[99113]
			if b.Deadline != base.Unix()+60 || !strings.Contains(b.Why, "job time limit reached (60 s") {
				t.Fatalf("B's own timeout facts missing: %+v", b)
			}
			if f.liveOn(t, "b") != 1 || c.Deadline != base.Unix()+3000 || c.LimitS != 3000 {
				t.Fatalf("B's timeout changed C's active execution: B=%+v C=%+v live=%d", b, c, f.liveOn(t, "b"))
			}
			proven := b.Why
			f.gh.mu.Lock()
			f.gh.status[99112] = "completed"
			f.gh.conclusion[99112] = "timed_out"
			f.gh.mu.Unlock()
			f.deliver(t, "workflow_job", endEvent(99112, 994, "acme/app", "timed_out", 1, "pool:sa"))
			eventually(t, "late B timeout applied", func() bool { return f.svc.Undone(ctx) == 0 })
			if got := f.jobs(t)[99112].Why; got != proven {
				t.Fatalf("late GitHub conclusion overwrote B's proven timeout: before=%s after=%s", proven, got)
			}
			if ending == "success" {
				f.gh.mu.Lock()
				f.gh.status[99113] = "completed"
				f.gh.conclusion[99113] = "success"
				f.gh.mu.Unlock()
				f.deliver(t, "workflow_job", endEvent(99113, 995, "acme/app", "success", 1, "pool:sb"))
				eventuallyBox(t, "C completed independently", func() bool { return f.liveOn(t, "b") == 0 && f.svc.Undone(ctx) == 0 })
			} else {
				elapsed.Store(70)
				f.svc.AnswerStatus(func(target string, h computeapi.RunTargetHandle, err error) (computeapi.RunTargetHandle, error) {
					if err == nil && target == "b" {
						h.State = "lost"
					}
					return h, err
				})
				f.svc.Watch(ctx)
				f.svc.AnswerStatus(nil)
				eventuallyBox(t, "C lost its host independently", func() bool { return f.liveOn(t, "b") == 0 })
				f.gh.mu.Lock()
				f.gh.status[99113] = "completed"
				f.gh.conclusion[99113] = "failure"
				f.gh.runStatus[995] = "completed"
				f.gh.mu.Unlock()
				f.deliver(t, "workflow_job", endEvent(99113, 995, "acme/app", "failure", 1, "pool:sb"))
				eventually(t, "C unexpected loss is retried", func() bool {
					f.svc.Sweep(ctx)
					f.gh.mu.Lock()
					defer f.gh.mu.Unlock()
					return slices.Contains(f.gh.reruns, "job 99113")
				})
			}

			if got := f.jobs(t)[99112].Why; got != proven {
				t.Fatalf("C lifecycle overwrote B's proven timeout: %s", got)
			}
		})
	}
}

func TestEarlyHostLossWinsBeforeStaleTimeoutDecision(t *testing.T) {
	f := pinned(t, "a")
	ctx := context.Background()
	base := time.Now().Truncate(time.Second)
	var elapsed atomic.Int64
	f.svc.Now = func() time.Time { return base.Add(time.Duration(elapsed.Load()) * time.Second) }
	workflowFor(f, 996, 99114, 1, base.Format(time.RFC3339))
	f.serveOn(t, 99114, 996, "a")
	f.started(t, 99114, 996, "acme/app", "potluck-99114")
	eventually(t, "start applied", func() bool { return f.jobs(t)[99114].Deadline == base.Unix()+60 })
	gate := make(chan struct{})
	var open sync.Once
	t.Cleanup(func() {
		f.gh.mu.Lock()
		if f.gh.gate == gate {
			f.gh.gate = nil
		}
		f.gh.mu.Unlock()
		open.Do(func() { close(gate) })
	})
	f.gh.mu.Lock()
	f.gh.gate = gate
	f.gh.gateOn = "/actions/jobs/99114"
	f.gh.mu.Unlock()
	elapsed.Store(61)
	finished := make(chan struct{})
	go func() { defer close(finished); f.svc.Watch(ctx) }()
	eventually(t, "stale timeout waiting for GitHub", func() bool { f.gh.mu.Lock(); defer f.gh.mu.Unlock(); return f.gh.waiting > 0 })
	// A second observation of an earlier host loss commits before the stale
	// timeout decision may proceed. It must remain a recoverable host loss.
	elapsed.Store(30)
	f.gh.mu.Lock()
	f.gh.gate = nil
	f.gh.mu.Unlock()
	f.svc.AnswerStatus(func(_ string, h computeapi.RunTargetHandle, err error) (computeapi.RunTargetHandle, error) {
		if err == nil {
			h.State = "lost"
		}
		return h, err
	})
	f.svc.Watch(ctx)
	f.svc.AnswerStatus(nil)
	eventually(t, "genuine loss committed", func() bool { return f.jobs(t)[99114].State == "lost" })
	elapsed.Store(61)
	open.Do(func() { close(gate) })
	select {
	case <-finished:
	case <-time.After(3 * time.Second):
		t.Fatal("stale timeout did not finish")
	}
	if why := f.jobs(t)[99114].Why; strings.Contains(why, "job time limit reached") {
		t.Fatalf("stale timeout overwrote genuine host loss: %s", why)
	}
	f.gh.mu.Lock()
	f.gh.status[99114] = "completed"
	f.gh.conclusion[99114] = "failure"
	f.gh.runStatus[996] = "completed"
	delete(f.gh.workflow, 996)
	f.gh.mu.Unlock()
	f.deliver(t, "workflow_job", endEvent(99114, 996, "acme/app", "failure", 1, "pool:sa"))
	eventually(t, "genuine loss still retried", func() bool {
		f.svc.Sweep(ctx)
		f.gh.mu.Lock()
		defer f.gh.mu.Unlock()
		return slices.Contains(f.gh.reruns, "job 99114")
	})
}

func TestOtherJobsEarlierTimeoutDoesNotClaimThisJobTimedOut(t *testing.T) {
	f := pinned(t, "a", "b")
	ctx := context.Background()
	base := time.Now().Truncate(time.Second)
	var elapsed atomic.Int64
	f.svc.Now = func() time.Time { return base.Add(time.Duration(elapsed.Load()) * time.Second) }
	f.gh.mu.Lock()
	f.gh.status[99121] = "queued"
	f.gh.mu.Unlock()
	f.serveOn(t, 99121, 997, "a")
	workflowFor(f, 998, 99122, 2, base.Format(time.RFC3339))
	f.serveOn(t, 99122, 998, "b")
	workflowFor(f, 999, 99123, 1, base.Format(time.RFC3339))
	startOn(t, f, 99123, 999, "potluck-99122", "pool:sb")
	startOn(t, f, 99122, 998, "potluck-99121", "pool:sa")
	eventually(t, "independent actual clocks", func() bool {
		j := f.jobs(t)
		return j[99122].Deadline == base.Unix()+120 && j[99123].Deadline == base.Unix()+60
	})
	// C, without a box of its own, carries B's box; B runs on A's box.
	elapsed.Store(61)
	f.svc.Watch(ctx)
	eventuallyBox(t, "C stopped at its own limit", func() bool { return f.jobs(t)[99123].State == "done" && f.svc.Undone(ctx) == 0 })
	b, c := f.jobs(t)[99122], f.jobs(t)[99123]
	if b.LimitS != 120 || b.Deadline != base.Unix()+120 || strings.Contains(b.Why, "job time limit reached") {
		t.Fatalf("C's earlier timeout falsely described as B's: %+v", b)
	}
	if !strings.Contains(c.Why, "job time limit reached (60 s") {
		t.Fatalf("wrong execution stopped: B=%+v C=%+v", b, c)
	}
	elapsed.Store(121)
	f.svc.Watch(ctx)
	if why := f.jobs(t)[99122].Why; !strings.Contains(why, "job time limit reached (120 s") {
		t.Fatalf("B's later actual timeout missing: %s", why)
	}
}

// Two queued jobs with the same labels and different limits: the runner
// minted for the short one takes the long one. Its box lives for the long
// job's limit (renewed past the short one's, stopped only at the long one's),
// and the short job, still queued on GitHub, gets a new box.
//
// A reference-driver binding has room for any number of boxes, so the long
// job waits only while no binding is listed. Its waiting serve is retried
// only when the test makes it due (Nudge): a retry on its own clock, once
// time moves on, could give it a box of its own before its start is applied.
func TestTakenRunnerBoxLivesForTheTakenJobsLimit(t *testing.T) {
	f := newPoolFixtureStarting(t, func(r *config.Runners) { r.Pool.Sizes = map[string]string{} },
		func(s *runners.Service) { s.Retry, s.WaitMax = time.Hour, time.Hour },
		box("a", "sa", "linux", "1"), box("b", "sa", "linux", "1"))
	if w := f.link(t, inst, "good"); w.Code != http.StatusOK {
		t.Fatalf("link: %d %s", w.Code, w.Body)
	}
	ctx := context.Background()
	base := time.Now().Truncate(time.Second)
	var elapsed atomic.Int64
	f.svc.Now = func() time.Time { return base.Add(time.Duration(elapsed.Load()) * time.Second) }
	const short, long = 99301, 99302
	f.gh.mu.Lock()
	for run, job := range map[int64]int64{9931: short, 9932: long} {
		f.gh.workflow[run] = fmt.Sprintf(`{"id":%d,"event":"push","path":".github/workflows/ci.yml","head_sha":%q,"repository":{"full_name":"acme/app"},"head_repository":{"full_name":"acme/app"}}`, run, strings.Repeat("a", 40))
		f.gh.status[job] = "queued"
	}
	f.gh.jobNames[short], f.gh.jobNames[long] = "Verdict", "Land"
	f.gh.contents = "jobs:\n  verdict:\n    name: Verdict\n    timeout-minutes: 10\n  land:\n    name: Land\n    timeout-minutes: 20\n"
	f.gh.mu.Unlock()

	// The short job gets a box; the long one waits for room.
	f.deliver(t, "workflow_job", jobEvent("queued", short, 9931, "acme/app", true, "pool:sa"))
	eventuallyBox(t, "short job on a box", func() bool {
		j := f.jobs(t)[short]
		return j.State == "serving" && j.LimitS == 600 && j.Target != "" && f.liveOn(t, j.Target) == 1 && f.svc.Undone(ctx) == 0
	})
	live := func() int { return f.liveOn(t, "a") + f.liveOn(t, "b") }
	other := map[string]string{"a": "b", "b": "a"}[f.jobs(t)[short].Target]
	f.svc.HideTargets("a", "b")
	f.deliver(t, "workflow_job", jobEvent("queued", long, 9932, "acme/app", true, "pool:sa"))
	// Waiting is decided after asking the targets for room, through the driver: a box step.
	eventuallyBox(t, "long job waits", func() bool {
		j := f.jobs(t)[long]
		return j.State == "waiting" && j.LimitS == 1200 && strings.Contains(j.Why, "no box in the pool serves jobs yet")
	})

	// GitHub gives the short job's runner the long job.
	f.svc.HideTargets(other)
	elapsed.Store(60)
	f.gh.mu.Lock()
	f.gh.jobStarts[long] = base.Add(60 * time.Second).Format(time.RFC3339)
	f.gh.mu.Unlock()
	startOn(t, f, long, 9932, "potluck-99301", "pool:sa")
	eventuallyBox(t, "long job carries the box", func() bool {
		j := f.jobs(t)[long]
		return j.State == "serving" && j.Deadline == base.Unix()+60+1200
	})
	// Its own waiting serve, due now, finds it running and ends.
	f.svc.Nudge(ctx)
	eventuallyBox(t, "every delivery applied", func() bool { return f.svc.Undone(ctx) == 0 })
	if got := f.renews(t); len(got) == 0 || got[len(got)-1] != 1200 {
		t.Fatalf("box not renewed to the taken job's limit: %v", got)
	}

	// The short job, still queued, gets a new box.
	f.svc.HideTargets()
	elapsed.Store(61)
	f.svc.Nudge(ctx) // a serve that waited for room is due again
	f.svc.Watch(ctx)
	eventuallyBox(t, "short job served again", func() bool {
		j := f.jobs(t)[short]
		return j.State == "serving" && j.Runner == "potluck-99301-2" && live() == 2 && f.svc.Undone(ctx) == 0
	})

	// The short job starts on its new box.
	elapsed.Store(400)
	f.gh.mu.Lock()
	f.gh.jobStarts[short] = base.Add(400 * time.Second).Format(time.RFC3339)
	f.gh.mu.Unlock()
	startOn(t, f, short, 9931, "potluck-99301-2", "pool:sa")
	eventually(t, "short job runs on its new box", func() bool { return f.jobs(t)[short].Deadline == base.Unix()+400+600 })

	// Past the short job's limit and the lease its first box was acquired
	// with, measured from the long job's start, the long job's box lives on.
	elapsed.Store(60 + 600 + startupAllowanceS + 1)
	f.svc.Sweep(ctx)
	f.svc.Watch(ctx)
	if j := f.jobs(t)[long]; j.State != "serving" || live() != 2 {
		t.Fatalf("taken job's box stopped at the short job's limit: %+v (%d boxes)", j, live())
	}
	if got := f.renews(t); !slices.Contains(got, 1200-600-startupAllowanceS-1) {
		t.Fatalf("renewal not bounded by the taken job's deadline: %v", got)
	}
	// Each is stopped at its own limit.
	elapsed.Store(60 + 1200 + 1)
	f.svc.Watch(ctx)
	eventuallyBox(t, "both stopped at their own limits", func() bool {
		l, sh := f.jobs(t)[long], f.jobs(t)[short]
		return l.State == "done" && strings.Contains(l.Why, "job time limit reached (1200 s") &&
			sh.State == "done" && strings.Contains(sh.Why, "job time limit reached (600 s") && live() == 0
	})
}

const startupAllowanceS = 300
