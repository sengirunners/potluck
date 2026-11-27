package runners_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/runners"
)

// lostBox and sameBox are AnswerStatus answers: every box lost, and each as
// its host says.
func lostBox(_ string, h computeapi.RunTargetHandle, err error) (computeapi.RunTargetHandle, error) {
	if err == nil {
		h.State = "lost"
	}
	return h, err
}

func sameBox(_ string, h computeapi.RunTargetHandle, err error) (computeapi.RunTargetHandle, error) {
	return h, err
}

// busy has GitHub list runner as running a job (or not): it refuses to
// delete a busy one.
func (f *fixture) busy(runner string, on bool) {
	f.gh.mu.Lock()
	f.gh.busy[runner] = on
	f.gh.mu.Unlock()
}

func (f *fixture) queued(jobs ...int64) {
	f.gh.mu.Lock()
	for _, j := range jobs {
		f.gh.status[j] = "queued"
	}
	f.gh.mu.Unlock()
}

// A granted job takes the runner of an ungranted one and is bounced: its box
// is released, so its runner is offline, and GitHub lists it busy until its
// lost-communication timeout (about 10 minutes). The ungranted job, still
// queued, is served again now, not once GitHub lets the runner go.
func TestBouncedRunnerGitHubCallsBusyIsReplacedAtOnce(t *testing.T) {
	df := newDoorFixture(t, true, grantGH)
	twoWorkflows(t, df)
	ctx := context.Background()
	df.served(t, 9401, 940, 1)
	df.served(t, 9421, 942, 2)
	df.busy("potluck-9421", true)
	df.applied(t, df.startOn(9401, 940, "acme/app", "potluck-9421"))
	eventuallyBox(t, "the ungranted job served again", func() bool {
		j := df.jobs(t)[9421]
		return j.State == "serving" && j.Runner == "potluck-9421-2" && len(df.guards()) >= 3 && df.svc.Undone(ctx) == 0
	})
}

// Granted jobs bounced off an ungranted job's boxes use up none of its serves.
func TestBouncesDoNotUseUpTheServes(t *testing.T) {
	df := newDoorFixture(t, true, grantGH)
	twoWorkflows(t, df)
	takers := []int64{9401, 9402, 9403}
	df.queued(takers...)
	for i, k := range takers {
		df.served(t, k, 940, i+1)
	}
	df.served(t, 9421, 942, 4)
	for i, k := range takers {
		df.applied(t, df.startOn(k, 940, "acme/app", df.jobs(t)[9421].Runner))
		eventuallyBox(t, fmt.Sprintf("the ungranted job served again after bounce %d", i+1), func() bool {
			j := df.jobs(t)[9421]
			return j.State == "serving" && j.Runner == fmt.Sprintf("potluck-9421-%d", i+2) && df.svc.Undone(context.Background()) == 0
		})
	}
}

// The same for a job the broker refuses (a fork's) that takes a runner.
func TestRefusedTakersRunnerGitHubCallsBusyIsReplacedAtOnce(t *testing.T) {
	f := pinnedStarting(t, func(s *runners.Service) { s.Retry = time.Hour }, "a")
	f.queued(9701)
	f.gh.mu.Lock()
	f.gh.forks[971] = true
	f.gh.mu.Unlock()
	f.serveOn(t, 9701, 970, "a")
	f.busy("potluck-9701", true)
	f.deliver(t, "workflow_job", startEvent(9702, 971, "acme/app", "potluck-9701"))
	eventuallyBox(t, "9701 served again", func() bool {
		j := f.jobs(t)[9701]
		return j.State == "serving" && j.Runner == "potluck-9701-2" && f.liveOn(t, "a") == 1
	})
}

// 9711 ran on another job's runner and ends. GitHub has just given 9711's own
// runner to 9713, whose in_progress is not applied yet: the runner is busy,
// and the box under it is not released.
func TestEndedJobsOwnBoxIsKeptWhileItsRunnerRunsAnother(t *testing.T) {
	f := pinned(t, "a", "b")
	ctx := context.Background()
	f.queued(9712)
	f.serveOn(t, 9711, 972, "a")
	f.serveOn(t, 9712, 972, "b")
	f.started(t, 9711, 972, "acme/app", "potluck-9712")
	eventually(t, "the start applied", func() bool { return f.svc.Undone(ctx) == 0 })
	f.busy("potluck-9711", true)
	f.gh.mu.Lock()
	f.gh.jobDone[9711] = true
	f.gh.mu.Unlock()
	f.deliver(t, "workflow_job", endEvent(9711, 972, "acme/app", "success", 1, "pool:sa"))
	eventually(t, "9711's end applied", func() bool { return f.jobs(t)[9712].Runner == "potluck-9712-2" })
	time.Sleep(300 * time.Millisecond)
	if f.liveOn(t, "a") != 1 {
		t.Fatalf("box a was released under the job GitHub gave its runner; 9711: %+v", f.jobs(t)[9711])
	}
	f.started(t, 9713, 973, "acme/app", "potluck-9711")
	f.busy("potluck-9711", false)
	eventually(t, "9713's start applied", func() bool { return f.svc.Undone(ctx) == 0 })
	f.gh.mu.Lock()
	f.gh.jobDone[9713] = true
	f.gh.mu.Unlock()
	f.deliver(t, "workflow_job", endEvent(9713, 973, "acme/app", "success", 1, "pool:sa"))
	eventually(t, "box a released with 9713", func() bool { return f.liveOn(t, "a") == 0 })
}

// Jobs the broker serves take job 9720's runner, one after another, and end
// while it is still queued. Its boxes never failed: that uses up none of its
// serves.
func TestSwapsDoNotUseUpTheServes(t *testing.T) {
	f := pinned(t, "a", "b")
	ctx := context.Background()
	f.queued(9720)
	f.serveOn(t, 9720, 974, "b")
	for i := range int64(4) {
		taker := 9721 + i
		runner := f.jobs(t)[9720].Runner
		f.serveOn(t, taker, 975+i, "a")
		f.started(t, taker, 975+i, "acme/app", runner)
		eventually(t, "the start applied", func() bool { return f.svc.Undone(ctx) == 0 })
		f.gh.mu.Lock()
		f.gh.jobDone[taker] = true
		f.gh.mu.Unlock()
		f.deliver(t, "workflow_job", endEvent(taker, 975+i, "acme/app", "success", 1, "pool:sa"))
		eventuallyBox(t, fmt.Sprintf("9720 served again after swap %d", i+1), func() bool {
			j := f.jobs(t)[9720]
			return j.State == "serving" && j.Runner == fmt.Sprintf("potluck-9720-%d", i+2) && f.liveOn(t, "a") == 0 && f.svc.Undone(ctx) == 0
		})
	}
}

// A job whose boxes failed three times is still queued on GitHub: it is
// served again later, not left there.
func TestJobOutOfServesIsServedAgainLater(t *testing.T) {
	f := pinned(t, "a")
	ctx := context.Background()
	f.queued(9731)
	f.serveOn(t, 9731, 980, "a")
	f.svc.AnswerStatus(lostBox)
	eventuallyBox(t, "its third box failed", func() bool {
		f.svc.Watch(ctx)
		j := f.jobs(t)[9731]
		return j.Runner == "potluck-9731-3" && j.State != "serving"
	})
	f.svc.AnswerStatus(sameBox)
	time.Sleep(500 * time.Millisecond)
	if n := len(f.minted()); n != 3 {
		t.Fatalf("%d runners minted at once, want 3 before the wait", n)
	}
	f.clock.advance(11 * time.Minute)
	f.svc.Nudge(ctx)
	eventuallyBox(t, "served again", func() bool {
		j := f.jobs(t)[9731]
		return j.State == "serving" && j.Runner == "potluck-9731-4"
	})
}

// A serve GitHub does not answer for a while is tried again once it does: the
// job is still queued there.
func TestServeOutlivesAGitHubOutage(t *testing.T) {
	f := pinnedStarting(t, func(s *runners.Service) { s.Retry = 5 * time.Millisecond }, "a")
	f.queued(9741)
	out, asks := true, 0
	f.gh.mu.Lock()
	f.gh.extra = func(w http.ResponseWriter, r *http.Request) bool {
		if out && r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/actions/jobs/9741") {
			asks++
			http.Error(w, `{"message":"unavailable"}`, http.StatusBadGateway)
			return true
		}
		return false
	}
	f.gh.mu.Unlock()
	f.deliver(t, "workflow_job", jobEvent("queued", 9741, 981, "acme/app", true, "pool:sa"))
	eventually(t, "six failures", func() bool { f.gh.mu.Lock(); defer f.gh.mu.Unlock(); return asks >= 6 })
	time.Sleep(100 * time.Millisecond)
	f.gh.mu.Lock()
	out = false
	f.gh.mu.Unlock()
	eventuallyBox(t, "served once GitHub answers", func() bool { return f.jobs(t)[9741].State == "serving" })
}

// 9751 ends before its in_progress was applied, and GitHub still lists its
// runner busy with its own last moments but does not say whose it is, or does
// not answer. Its box is kept, and freed once GitHub says, with no sweep.
func TestEndedJobsBoxIsFreedOnceGitHubSaysWhoseItsRunnerIs(t *testing.T) {
	for i, answer := range []string{"unavailable", "no runner yet"} {
		t.Run(answer, func(t *testing.T) {
			f := pinnedStarting(t, func(s *runners.Service) { s.Retry = 20 * time.Millisecond }, "a")
			ended := int64(9751 + i)
			f.serveOn(t, ended, 990, "a")
			runner, asked, out := fmt.Sprintf("potluck-%d", ended), 0, true
			f.busy(runner, true)
			f.gh.mu.Lock()
			f.gh.extra = func(w http.ResponseWriter, r *http.Request) bool {
				if r.Method != "GET" || !strings.HasSuffix(r.URL.Path, fmt.Sprintf("/actions/jobs/%d", ended)) {
					return false
				}
				asked++
				name := runner
				if out {
					if answer == "unavailable" {
						http.Error(w, `{"message":"unavailable"}`, http.StatusBadGateway)
						return true
					}
					name = ""
				}
				fmt.Fprintf(w, `{"id":%d,"run_id":990,"status":"completed","conclusion":"success","runner_name":%q}`, ended, name)
				return true
			}
			f.gh.mu.Unlock()
			f.deliver(t, "workflow_job", endEvent(ended, 990, "acme/app", "success", 1, "pool:sa"))
			// Asked twice: the first answer was applied and did not release it.
			eventuallyBox(t, "GitHub asked again", func() bool { f.gh.mu.Lock(); defer f.gh.mu.Unlock(); return asked >= 2 })
			if f.liveOn(t, "a") != 1 {
				t.Fatalf("the box was released while GitHub had its runner busy and did not say whose it is: ended %+v", f.jobs(t)[ended])
			}
			f.gh.mu.Lock()
			out = false
			f.gh.mu.Unlock()
			eventuallyBox(t, "the box freed", func() bool { return f.liveOn(t, "a") == 0 })
		})
	}
}

// GitHub's completed names the runner the job ran on, so a box's release does
// not wait for GitHub to answer a lookup. One that names another runner is of
// a job that ended elsewhere: a busy runner is another job's, and is kept.
func TestCompletedNamesTheEndedJobsRunner(t *testing.T) {
	for i, own := range []bool{true, false} {
		t.Run(fmt.Sprintf("own runner %v", own), func(t *testing.T) {
			f := pinnedStarting(t, func(s *runners.Service) { s.Retry = 20 * time.Millisecond }, "a")
			ended := int64(9771 + i)
			f.serveOn(t, ended, 991, "a")
			runner, asked := fmt.Sprintf("potluck-%d", ended), 0
			f.busy(runner, true)
			f.gh.mu.Lock()
			f.gh.extra = func(w http.ResponseWriter, r *http.Request) bool {
				if r.Method != "GET" || !strings.HasSuffix(r.URL.Path, fmt.Sprintf("/actions/jobs/%d", ended)) {
					return false
				}
				asked++
				http.Error(w, `{"message":"unavailable"}`, http.StatusBadGateway)
				return true
			}
			f.gh.mu.Unlock()
			name := runner
			if !own {
				name = "another-runner"
			}
			var m map[string]any
			_ = json.Unmarshal(endEvent(ended, 991, "acme/app", "success", 1, "pool:sa"), &m)
			m["workflow_job"].(map[string]any)["runner_name"] = name
			b, _ := json.Marshal(m)
			f.deliver(t, "workflow_job", b)
			if own {
				eventuallyBox(t, "its box freed", func() bool { return f.liveOn(t, "a") == 0 })
			} else {
				eventuallyBox(t, "the end applied", func() bool { return f.svc.Undone(context.Background()) == 0 })
				if f.liveOn(t, "a") != 1 {
					t.Fatal("the box was released under the other job its runner is busy with")
				}
			}
			f.gh.mu.Lock()
			defer f.gh.mu.Unlock()
			if asked != 0 {
				t.Fatalf("GitHub was asked %d times whose the runner is; its completed had said", asked)
			}
		})
	}
}
