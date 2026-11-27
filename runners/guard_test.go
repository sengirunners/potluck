package runners_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/potluck/computeclient"
	"github.com/sengirunners/potluck/kit/config"
)

// The guards a box's job-started hook checks (jit_guard): the trusted events
// (a pull_request* run's head must be the repository; vmhost's hook tests
// show it stops any other before its first step), or those and any run
// whose event payload says the repository is private (open).
const (
	strictGuard = `"jit_guard":{"repo":"acme/app","events":["push","pull_request","workflow_dispatch","schedule","merge_group","release","repository_dispatch"]}`
	openGuard   = `"jit_guard":{"repo":"acme/app","events":["push","pull_request","workflow_dispatch","schedule","merge_group","release","repository_dispatch"],"open_if_private":true}`
)

// While fork serving holds for a repository (private, allow_fork_prs), every
// runner minted for it is guarded by the repository alone, whatever run it
// was minted for: GitHub gives a runner to any queued job of the repository
// with its labels, so a fork's job (or a pull_request_target's) queued
// beside an own branch's may take the own branch's runner, and runs there,
// recorded as running on it; the own branch's job is served again only if
// its box ends without it.
func TestForkJobRunsOnOwnBranchRunnerWhileForksAreServed(t *testing.T) {
	for _, taker := range []string{"a fork's job", "a pull_request_target job"} {
		t.Run(taker, func(t *testing.T) {
			f := newFixtureWith(t, func(c *config.Runners) { c.AllowForkPRs = true })
			ctx := context.Background()
			if w := f.link(t, inst, "good"); w.Code != http.StatusOK {
				t.Fatalf("link: %d %s", w.Code, w.Body)
			}
			labels := []string{"self-hosted", "sengi"}
			f.gh.mu.Lock()
			f.gh.events[731] = "push"
			if taker == "a fork's job" {
				f.gh.forks[732] = true
			} else {
				f.gh.events[732] = "pull_request_target"
			}
			f.gh.mu.Unlock()
			f.deliver(t, "workflow_job", jobEvent("queued", 7301, 731, "acme/app", true, labels...))
			eventually(t, "served", func() bool { return f.jobs(t)[7301].State == "serving" && f.svc.Undone(ctx) == 0 })
			if b, _ := os.ReadFile(f.seen); !strings.Contains(string(b), openGuard) || strings.Contains(string(b), strictGuard) {
				t.Fatalf("an own branch's runner while forks are served is not guarded by the repository alone: %s", b)
			}
			f.deliver(t, "workflow_job", startEvent(7302, 732, "acme/app", "potluck-7301"))
			eventually(t, "recorded running on the runner", func() bool {
				j := f.jobs(t)
				return j[7302].State == "running" && strings.Contains(j[7302].Why, "running on runner potluck-7301") &&
					j[7301].State == "serving" && f.svc.Undone(ctx) == 0
			})
			time.Sleep(200 * time.Millisecond)
			if _, del := f.gh.snapshot(); len(del) != 0 || f.ready(t) != 1 || len(f.minted()) != 1 {
				t.Fatalf("the taker on the runner: deleted %v, %d boxes, minted %v; want none, 1, one", del, f.ready(t), f.minted())
			}
		})
	}
}

// Once fork serving stops holding for a repository, the broker retires its
// idle runners guarded by the repository alone as soon as it learns of it:
// allow_fork_prs switched off (read at a restart, or at a settings save,
// which reads config.json as it is on disk), the repository public on
// GitHub (read when its owner switches it on, by the hourly check, or before
// any mint for it). Each one's box is released and its runner deleted; its
// job is decided again: an own branch's is served on a runner with the
// trusted events' guard (a public one's only after its owner's yes), a
// fork's is not served. A fork's job queued later gets no runner.
func TestOpenRunnersAreRetiredWhenForkServingStops(t *testing.T) {
	for _, learned := range []string{"restart", "settings save", "owner's yes", "hourly check", "a mint's read"} {
		for _, minted := range []string{"an own branch's job", "a fork's job"} {
			t.Run(learned+"/"+minted, func(t *testing.T) {
				f := newFixtureWith(t, func(c *config.Runners) { c.AllowForkPRs = true })
				ctx := context.Background()
				if w := f.link(t, inst, "good"); w.Code != http.StatusOK {
					t.Fatalf("link: %d %s", w.Code, w.Body)
				}
				labels := []string{"self-hosted", "sengi"}
				own := minted == "an own branch's job"
				f.gh.mu.Lock()
				f.gh.events[741], f.gh.events[743] = "push", "push"
				f.gh.forks[741], f.gh.forks[742] = !own, true
				f.gh.status[7401] = "queued"
				f.gh.mu.Unlock()
				f.deliver(t, "workflow_job", jobEvent("queued", 7401, 741, "acme/app", true, labels...))
				eventually(t, "served", func() bool { return f.jobs(t)[7401].State == "serving" && f.svc.Undone(ctx) == 0 })
				public := false
				switch learned {
				case "restart":
					f.restart(t, func(c *config.Config) { c.File.Runners.AllowForkPRs = false })
				case "settings save":
					// A hand edit of config.json, then a save of another setting.
					c, err := config.LoadFrom(f.home)
					if err != nil {
						t.Fatal(err)
					}
					c.File.Runners.AllowForkPRs = false
					if err := c.Save(); err != nil {
						t.Fatal(err)
					}
					if _, err := f.set("job_ttl_s", "3000"); err != nil {
						t.Fatal(err)
					}
				case "owner's yes":
					public = true
					f.change(t, "made public")
				case "hourly check":
					public = true
					f.gh.mu.Lock()
					f.gh.publicRepo["acme/app"] = true
					f.gh.mu.Unlock()
					f.svc.RepoCheckEvery = 0
					f.svc.Sweep(ctx)
				case "a mint's read":
					public = true
					f.gh.mu.Lock()
					f.gh.publicRepo["acme/app"] = true
					f.gh.mu.Unlock()
					// Another job of the repository: ready reads it before a mint.
					f.deliver(t, "workflow_job", jobEvent("queued", 7403, 743, "acme/app", true, labels...))
				}
				// No job started on it: only the retire deletes it.
				eventually(t, "retired", func() bool { _, del := f.gh.snapshot(); return slices.Contains(del, "potluck-7401") })
				// A public repository serves its own branches once its owner said yes.
				if own && (!public || learned == "owner's yes") {
					// The old box is released in the background: the new one's
					// acquire is what shows it served again.
					eventually(t, "served again, strictly", func() bool {
						b, _ := os.ReadFile(f.seen)
						return f.jobs(t)[7401].State == "serving" && f.ready(t) == 1 && slices.Contains(f.minted(), "potluck-7401-2") &&
							strings.Contains(string(b), strictGuard)
					})
				} else {
					eventually(t, "not served again", func() bool {
						j := f.jobs(t)[7401]
						return j.State == "ignored" && f.ready(t) == 0 && f.svc.Undone(ctx) == 0
					})
				}
				n := len(f.minted())
				f.deliver(t, "workflow_job", jobEvent("queued", 7402, 742, "acme/app", true, labels...))
				eventually(t, "a later fork's job is ignored", func() bool { return f.jobs(t)[7402].State == "ignored" && f.svc.Undone(ctx) == 0 })
				if m := f.minted(); len(m) != n {
					t.Errorf("a runner was minted for a later fork's job: %v", m)
				}
			})
		}
	}
}

// A retire never ends a runner a job has started on: the broker's record
// says it started (decided then, as things were), or GitHub lists it busy.
// The job runs to its end; a GitHub runner takes one job only, so the runner
// serves nothing after it. An idle one beside it is retired. One GitHub lists busy but whose start the broker
// has not seen is left to the decision at that start (the backstop below).
func TestRetireLeavesBusyRunners(t *testing.T) {
	for _, busy := range []string{"started", "busy on GitHub"} {
		t.Run(busy, func(t *testing.T) {
			f := newFixtureWith(t, func(c *config.Runners) { c.AllowForkPRs = true })
			ctx := context.Background()
			if w := f.link(t, inst, "good"); w.Code != http.StatusOK {
				t.Fatalf("link: %d %s", w.Code, w.Body)
			}
			labels := []string{"self-hosted", "sengi"}
			f.gh.mu.Lock()
			f.gh.forks[751] = true
			f.gh.events[752] = "push"
			f.gh.mu.Unlock()
			f.deliver(t, "workflow_job", jobEvent("queued", 7501, 751, "acme/app", true, labels...))
			f.deliver(t, "workflow_job", jobEvent("queued", 7502, 752, "acme/app", true, labels...))
			eventually(t, "served", func() bool {
				j := f.jobs(t)
				return j[7501].State == "serving" && j[7502].State == "serving" && f.ready(t) == 2 && f.svc.Undone(ctx) == 0
			})
			if busy == "started" {
				f.deliver(t, "workflow_job", startEvent(7501, 751, "acme/app", "potluck-7501"))
				eventually(t, "started", func() bool { return f.svc.Undone(ctx) == 0 })
			} else {
				f.gh.mu.Lock()
				f.gh.busy["potluck-7501"] = true
				f.gh.mu.Unlock()
			}
			f.restart(t, func(c *config.Config) { c.File.Runners.AllowForkPRs = false })
			eventually(t, "the idle one retired", func() bool {
				_, del := f.gh.snapshot()
				return slices.Equal(del, []string{"potluck-7502"}) && f.ready(t) == 1 && f.svc.Undone(ctx) == 0
			})
			time.Sleep(300 * time.Millisecond)
			if _, del := f.gh.snapshot(); !slices.Equal(del, []string{"potluck-7502"}) || f.ready(t) != 1 || f.jobs(t)[7501].State != "serving" {
				t.Fatalf("a busy runner after the switch went off: deleted %v, %d boxes, %+v; want potluck-7502 alone, 1, serving", del, f.ready(t), f.jobs(t)[7501])
			}
			if busy == "started" {
				f.gh.mu.Lock()
				f.gh.jobDone[7501] = true
				f.gh.mu.Unlock()
				f.deliver(t, "workflow_job", jobEvent("completed", 7501, 751, "acme/app", true, labels...))
				eventually(t, "released at its end", func() bool {
					_, del := f.gh.snapshot()
					return slices.Equal(del, []string{"potluck-7502", "potluck-7501"}) && f.ready(t) == 0 && f.jobs(t)[7501].State == "done"
				})
			}
		})
	}
}

// What the retire misses (a runner GitHub had already given a job when it
// ran) is decided again when that job starts: every job that starts on a
// runner guarded by the repository alone, its own too, from GitHub's answer
// then and allow_fork_prs as it is then. Once the repository went public,
// or the switch is off, a fork's job (another's, or its own) has its box
// released and says why; an own branch's job still runs there.
func TestOpenRunnerIsDecidedAgainAtJobStart(t *testing.T) {
	for _, change := range []string{"made public", "fork serving switched off"} {
		why := map[string]string{"made public": "a run from a fork: never served for a public repository",
			"fork serving switched off": "a run from a fork: this pool serves only its repositories' own branches"}[change]
		for _, who := range []string{"another fork's job", "its own job", "an own branch's job"} {
			t.Run(change+"/"+who, func(t *testing.T) {
				f := newFixtureWith(t, func(c *config.Runners) { c.AllowForkPRs = true })
				ctx := context.Background()
				if w := f.link(t, inst, "good"); w.Code != http.StatusOK {
					t.Fatalf("link: %d %s", w.Code, w.Body)
				}
				labels := []string{"self-hosted", "sengi"}
				f.gh.mu.Lock()
				f.gh.forks[711], f.gh.forks[712] = true, true
				f.gh.events[713] = "push"
				f.gh.status[7101] = "queued"
				f.gh.mu.Unlock()
				f.deliver(t, "workflow_job", jobEvent("queued", 7101, 711, "acme/app", true, labels...))
				eventually(t, "served", func() bool { return f.jobs(t)[7101].State == "serving" && f.svc.Undone(ctx) == 0 })
				if b, _ := os.ReadFile(f.seen); !strings.Contains(string(b), openGuard) {
					t.Fatalf("a fork's run's runner is not guarded by the repository alone: %s", b)
				}
				f.gh.mu.Lock()
				f.gh.busy["potluck-7101"] = true // GitHub gave it the job: the retire leaves it, and GitHub refuses to delete it
				f.gh.mu.Unlock()
				f.change(t, change)
				switch who {
				case "another fork's job":
					f.deliver(t, "workflow_job", startEvent(7102, 712, "acme/app", "potluck-7101"))
					// Refused, its box released; job 7101, a fork's run too,
					// is not served again.
					eventually(t, "refused, its box released", func() bool {
						_, del := f.gh.snapshot()
						j := f.jobs(t)
						return len(del) == 0 && f.ready(t) == 0 &&
							j[7101].State == "ignored" && strings.Contains(j[7101].Why, why) &&
							j[7102].State == "ignored" && strings.Contains(j[7102].Why, "refused when it started on runner potluck-7101 ("+why+")")
					})
				case "its own job":
					f.deliver(t, "workflow_job", startEvent(7101, 711, "acme/app", "potluck-7101"))
					eventually(t, "refused, its box released", func() bool {
						_, del := f.gh.snapshot()
						j := f.jobs(t)[7101]
						return len(del) == 0 && f.ready(t) == 0 && j.State == "ignored" &&
							strings.Contains(j.Why, "refused when it started on its runner potluck-7101 ("+why+")")
					})
				case "an own branch's job":
					f.deliver(t, "workflow_job", startEvent(7103, 713, "acme/app", "potluck-7101"))
					eventually(t, "applied", func() bool { return f.svc.Undone(ctx) == 0 })
					time.Sleep(200 * time.Millisecond)
					if _, del := f.gh.snapshot(); len(del) != 0 || f.ready(t) != 1 {
						t.Fatalf("an own branch's job on the runner: deleted %v, %d boxes; want none, 1", del, f.ready(t))
					}
					return
				}
				if m := f.minted(); !slices.Equal(m, []string{"potluck-7101"}) {
					t.Errorf("runners minted: %v", m)
				}
			})
		}
	}
}

// A runner minted while fork serving did not hold has the trusted events'
// guard, which holds whatever changes. A job that takes it and passes only
// under fork serving (switched on since) is stopped by the box's hook before
// its first step whatever the broker would say: the broker does not count it
// a swap, releases the box and serves the runner's own job again. A served
// taker is told apart by its recorded level, one not served yet by GitHub's
// answer.
func TestStrictRunnerTakenByForkJob(t *testing.T) {
	for _, taker := range []string{"served", "not served yet"} {
		t.Run(taker, func(t *testing.T) {
			f := newFixture(t)
			ctx := context.Background()
			if w := f.link(t, inst, "good"); w.Code != http.StatusOK {
				t.Fatalf("link: %d %s", w.Code, w.Body)
			}
			labels := []string{"self-hosted", "sengi"}
			f.gh.mu.Lock()
			f.gh.events[761] = "push"
			f.gh.forks[762] = true
			f.gh.status[7601] = "queued"
			f.gh.mu.Unlock()
			f.deliver(t, "workflow_job", jobEvent("queued", 7601, 761, "acme/app", true, labels...))
			eventually(t, "served", func() bool { return f.jobs(t)[7601].State == "serving" && f.svc.Undone(ctx) == 0 })
			if b, _ := os.ReadFile(f.seen); !strings.Contains(string(b), strictGuard) {
				t.Fatalf("not guarded by the trusted events: %s", b)
			}
			f.restart(t, func(c *config.Config) { c.File.Runners.AllowForkPRs = true })
			if taker == "served" {
				f.deliver(t, "workflow_job", jobEvent("queued", 7602, 762, "acme/app", true, labels...))
				eventually(t, "the fork's job served", func() bool { return f.jobs(t)[7602].State == "serving" && f.svc.Undone(ctx) == 0 })
			}
			f.deliver(t, "workflow_job", startEvent(7602, 762, "acme/app", "potluck-7601"))
			eventually(t, "stopped by the guard; 7601 served again", func() bool {
				_, del := f.gh.snapshot()
				j := f.jobs(t)
				return slices.Contains(del, "potluck-7601") && slices.Contains(f.minted(), "potluck-7601-2") &&
					j[7601].State == "serving" && j[7602].State != "running" && f.svc.Undone(ctx) == 0
			})
		})
	}
}

// A job starting on a runner whose guard is the repository alone is refused
// when GitHub does not answer how the repository is now, after a few tries
// within a couple of seconds: never let through on the delivery's word. One
// failed answer is tried again. A refusal while the installation's API
// budget is nearly spent says so. A runner's own job whose run passed at the
// full rules (an own branch's push) is not asked about again: it runs.
func TestOpenRunnerRefusesWhenGitHubDoesNotAnswer(t *testing.T) {
	for _, answer := range []string{"never", "on the second try", "budget spent", "never, to an own branch's job"} {
		t.Run(answer, func(t *testing.T) {
			f := newFixtureWith(t, func(c *config.Runners) { c.AllowForkPRs = true })
			ctx := context.Background()
			if w := f.link(t, inst, "good"); w.Code != http.StatusOK {
				t.Fatalf("link: %d %s", w.Code, w.Body)
			}
			labels := []string{"self-hosted", "sengi"}
			own := answer == "never, to an own branch's job"
			f.gh.mu.Lock()
			f.gh.forks[721], f.gh.forks[722] = !own, true
			if own {
				f.gh.events[721] = "push"
			}
			f.gh.mu.Unlock()
			f.deliver(t, "workflow_job", jobEvent("queued", 7201, 721, "acme/app", true, labels...))
			eventually(t, "served", func() bool { return f.jobs(t)[7201].State == "serving" && f.svc.Undone(ctx) == 0 })
			fails := 0
			f.gh.mu.Lock()
			f.gh.extra = func(w http.ResponseWriter, r *http.Request) bool {
				if r.URL.Path == "/repos/acme/app" && (answer != "on the second try" || fails == 0) {
					fails++
					w.WriteHeader(http.StatusBadGateway)
					return true
				}
				return false
			}
			if answer == "budget spent" {
				f.gh.rateLeft = 10
			}
			f.gh.mu.Unlock()
			if own {
				f.deliver(t, "workflow_job", startEvent(7201, 721, "acme/app", "potluck-7201"))
				eventually(t, "applied", func() bool { return f.svc.Undone(ctx) == 0 })
				time.Sleep(200 * time.Millisecond)
				if _, del := f.gh.snapshot(); len(del) != 0 || f.ready(t) != 1 || f.jobs(t)[7201].State != "serving" {
					t.Fatalf("an own branch's job on its runner: deleted %v, %d boxes, %+v; want none, 1, serving", del, f.ready(t), f.jobs(t)[7201])
				}
				return
			}
			f.deliver(t, "workflow_job", startEvent(7202, 722, "acme/app", "potluck-7201"))
			if answer == "on the second try" {
				eventually(t, "runs", func() bool { return f.jobs(t)[7202].State == "running" && f.svc.Undone(ctx) == 0 })
				if _, del := f.gh.snapshot(); len(del) != 0 || f.ready(t) != 1 {
					t.Fatalf("let through on the second try: deleted %v, %d boxes; want none, 1", del, f.ready(t))
				}
				return
			}
			want := "(its check failed)"
			if answer == "budget spent" {
				want = "(its check failed: GitHub's API budget for this installation is nearly spent)"
			}
			eventually(t, "refused, its box released", func() bool {
				_, del := f.gh.snapshot()
				j := f.jobs(t)[7202]
				return slices.Equal(del, []string{"potluck-7201"}) && f.ready(t) == 0 &&
					j.State == "ignored" && strings.Contains(j.Why, want)
			})
			f.gh.mu.Lock()
			defer f.gh.mu.Unlock()
			if fails < 3 {
				t.Errorf("GitHub asked %d times before the refusal; want 3", fails)
			}
		})
	}
}

// change makes acme/app public on GitHub (its owner saying yes to its own
// branches), or switches allow_fork_prs off (a restart: the hand edit).
func (f *fixture) change(t *testing.T, change string) {
	t.Helper()
	switch change {
	case "made public":
		f.gh.mu.Lock()
		f.gh.publicRepo["acme/app"] = true
		f.gh.mu.Unlock()
		if _, err := f.allow("acme/app", true); err != nil {
			t.Fatal(err)
		}
	case "fork serving switched off":
		f.restart(t, func(c *config.Config) { c.File.Runners.AllowForkPRs = false })
	default:
		t.Fatalf("change %q", change)
	}
}

// ready counts the pool's boxes that are up.
func (f *fixture) ready(t *testing.T) int {
	t.Helper()
	hs, err := computeclient.RunTargetHandles(context.Background(), f.owner, "small", false)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, h := range hs {
		if h.State == "ready" {
			n++
		}
	}
	return n
}

// A job's start on a runner guarded by the repository alone is a read of the
// repository too: when GitHub says it is public, the repository's other idle
// open runners are retired at once, with nothing else reading it.
func TestJobStartRetiresWhenRepositoryIsPublic(t *testing.T) {
	f := newFixtureWith(t, func(c *config.Runners) { c.AllowForkPRs = true })
	ctx := context.Background()
	if w := f.link(t, inst, "good"); w.Code != http.StatusOK {
		t.Fatalf("link: %d %s", w.Code, w.Body)
	}
	labels := []string{"self-hosted", "sengi"}
	f.gh.mu.Lock()
	f.gh.forks[771], f.gh.forks[772] = true, true
	f.gh.status[7702] = "queued"
	f.gh.mu.Unlock()
	f.deliver(t, "workflow_job", jobEvent("queued", 7701, 771, "acme/app", true, labels...))
	f.deliver(t, "workflow_job", jobEvent("queued", 7702, 772, "acme/app", true, labels...))
	eventually(t, "served", func() bool {
		j := f.jobs(t)
		return j[7701].State == "serving" && j[7702].State == "serving" && f.ready(t) == 2 && f.svc.Undone(ctx) == 0
	})
	f.gh.mu.Lock()
	f.gh.publicRepo["acme/app"] = true
	f.gh.mu.Unlock()
	// Its own job, refused, is not served again: no mint reads the repository.
	f.deliver(t, "workflow_job", startEvent(7701, 771, "acme/app", "potluck-7701"))
	eventually(t, "both released", func() bool {
		_, del := f.gh.snapshot()
		j := f.jobs(t)
		return slices.Contains(del, "potluck-7701") && slices.Contains(del, "potluck-7702") && f.ready(t) == 0 &&
			j[7701].State == "ignored" && j[7702].State == "ignored" && f.svc.Undone(ctx) == 0
	})
}

// runnerLists counts the broker's reads of acme/app's runners on GitHub.
func (f *fixture) runnerLists() int {
	f.gh.mu.Lock()
	defer f.gh.mu.Unlock()
	n := 0
	for _, c := range f.gh.calls {
		if c == "GET /repos/acme/app/actions/runners" {
			n++
		}
	}
	return n
}

// A switch-off retire GitHub keeps failing (its runner list does not answer)
// is dropped after its tries, but not for good: while allow_fork_prs is off
// and an idle runner guarded by the repository alone is left, the sweep
// queues it again, and the runner is retired once GitHub answers.
func TestSweepRetiresWhatADroppedRetireLeft(t *testing.T) {
	f := newFixtureWith(t, func(c *config.Runners) { c.AllowForkPRs = true })
	ctx := context.Background()
	if w := f.link(t, inst, "good"); w.Code != http.StatusOK {
		t.Fatalf("link: %d %s", w.Code, w.Body)
	}
	f.gh.mu.Lock()
	f.gh.forks[781] = true
	f.gh.status[7801] = "queued"
	f.gh.mu.Unlock()
	f.deliver(t, "workflow_job", jobEvent("queued", 7801, 781, "acme/app", true, "self-hosted", "sengi"))
	eventually(t, "served", func() bool { return f.jobs(t)[7801].State == "serving" && f.svc.Undone(ctx) == 0 })
	failing := true
	f.gh.mu.Lock()
	f.gh.extra = func(w http.ResponseWriter, r *http.Request) bool {
		if failing && r.Method == "GET" && r.URL.Path == "/repos/acme/app/actions/runners" {
			w.WriteHeader(http.StatusBadGateway)
			return true
		}
		return false
	}
	f.gh.mu.Unlock()
	f.restart(t, func(c *config.Config) { c.File.Runners.AllowForkPRs = false })
	eventually(t, "the retire dropped", func() bool { return f.runnerLists() >= 6 && f.svc.Undone(ctx) == 0 })
	if _, del := f.gh.snapshot(); len(del) != 0 || f.jobs(t)[7801].State != "serving" {
		t.Fatalf("while GitHub failed: deleted %v, %+v", del, f.jobs(t)[7801])
	}
	f.gh.mu.Lock()
	failing = false
	f.gh.mu.Unlock()
	f.svc.Sweep(ctx)
	eventually(t, "retired by the sweep's retire", func() bool {
		_, del := f.gh.snapshot()
		return slices.Contains(del, "potluck-7801") && f.ready(t) == 0 && f.jobs(t)[7801].State == "ignored" && f.svc.Undone(ctx) == 0
	})
}

// A retire whose requeue fails after it deleted the runner leaves the job
// serving (its end and its requeue are one transaction), and finishes it
// when it is tried again: the job is served again.
func TestRetireQueuesAgainWhatItReleased(t *testing.T) {
	f := newFixtureWith(t, func(c *config.Runners) { c.AllowForkPRs = true })
	ctx := context.Background()
	if w := f.link(t, inst, "good"); w.Code != http.StatusOK {
		t.Fatalf("link: %d %s", w.Code, w.Body)
	}
	f.gh.mu.Lock()
	f.gh.events[791] = "push"
	f.gh.status[7901] = "queued"
	f.gh.mu.Unlock()
	f.deliver(t, "workflow_job", jobEvent("queued", 7901, 791, "acme/app", true, "self-hosted", "sengi"))
	eventually(t, "served", func() bool { return f.jobs(t)[7901].State == "serving" && f.svc.Undone(ctx) == 0 })
	if _, err := f.dm.Deps.Store.Registry().ExecContext(ctx, `CREATE TRIGGER full_disk BEFORE INSERT ON runners_inbox WHEN NEW.delivery LIKE 'requeue-%'
		BEGIN SELECT RAISE(ABORT, 'disk full'); END`); err != nil {
		t.Fatal(err)
	}
	f.restart(t, func(c *config.Config) { c.File.Runners.AllowForkPRs = false })
	reg := f.dm.Deps.Store.Registry()
	eventually(t, "runner deleted, not queued again", func() bool {
		_, del := f.gh.snapshot()
		return slices.Contains(del, "potluck-7901") && f.runnerLists() >= 2
	})
	if j := f.jobs(t)[7901]; j.State != "serving" {
		t.Fatalf("a retire whose requeue failed ended the serve: %+v", j)
	}
	if _, err := reg.ExecContext(ctx, `DROP TRIGGER full_disk`); err != nil {
		t.Fatal(err)
	}
	eventually(t, "served again", func() bool {
		j := f.jobs(t)[7901]
		return j.State == "serving" && slices.Contains(f.minted(), "potluck-7901-2") && f.svc.Undone(ctx) == 0
	})
}

// A runner GitHub lists busy whose job's start the broker has not seen is
// asked about once: later reads of the public repository queue no retire
// for it, so they spend no more of the API budget on its runner list.
func TestBusyRunnerIsListedOnce(t *testing.T) {
	f := newFixtureWith(t, func(c *config.Runners) { c.AllowForkPRs = true })
	ctx := context.Background()
	if w := f.link(t, inst, "good"); w.Code != http.StatusOK {
		t.Fatalf("link: %d %s", w.Code, w.Body)
	}
	labels := []string{"self-hosted", "sengi"}
	f.gh.mu.Lock()
	f.gh.forks[801], f.gh.forks[802], f.gh.forks[803], f.gh.forks[804] = true, true, true, true
	f.gh.mu.Unlock()
	f.deliver(t, "workflow_job", jobEvent("queued", 8001, 801, "acme/app", true, labels...))
	eventually(t, "served", func() bool { return f.jobs(t)[8001].State == "serving" && f.svc.Undone(ctx) == 0 })
	f.gh.mu.Lock()
	f.gh.busy["potluck-8001"] = true
	f.gh.publicRepo["acme/app"] = true
	f.gh.mu.Unlock()
	before := f.runnerLists()
	for i, job := range []int64{8002, 8003, 8004} {
		f.deliver(t, "workflow_job", jobEvent("queued", job, 802+int64(i), "acme/app", true, labels...))
		eventually(t, "ignored", func() bool { return f.jobs(t)[job].State == "ignored" && f.svc.Undone(ctx) == 0 })
	}
	if n := f.runnerLists() - before; n != 1 {
		t.Errorf("acme/app's runners listed %d times for three reads; want 1", n)
	}
	if _, del := f.gh.snapshot(); len(del) != 0 || f.jobs(t)[8001].State != "serving" {
		t.Errorf("the busy runner: deleted %v, %+v", del, f.jobs(t)[8001])
	}
}

// A retire goes on past a job GitHub fails for: the idle runners of the
// other repositories are retired in the same try. A runner list GitHub
// refuses the App (404 for the list: the repository gone; 422 for the
// repository's token: it left the installation) lists no busy runner: that
// job's box is released too.
func TestRetireGoesOnPastAFailingJob(t *testing.T) {
	for _, answer := range []string{"502", "404", "422 token"} {
		t.Run(answer, func(t *testing.T) {
			f := newFixtureWith(t, func(c *config.Runners) { c.AllowForkPRs = true })
			ctx := context.Background()
			if w := f.link(t, inst, "good"); w.Code != http.StatusOK {
				t.Fatalf("link: %d %s", w.Code, w.Body)
			}
			labels := []string{"self-hosted", "sengi"}
			f.gh.mu.Lock()
			f.gh.forks[761], f.gh.forks[762] = true, true
			f.gh.status[7601], f.gh.status[7602] = "queued", "queued"
			f.gh.mu.Unlock()
			f.deliver(t, "workflow_job", jobEvent("queued", 7601, 761, "acme/app", true, labels...))
			eventually(t, "served", func() bool { return f.jobs(t)[7601].State == "serving" && f.svc.Undone(ctx) == 0 })
			f.deliver(t, "workflow_job", jobEvent("queued", 7602, 762, "acme/lib", true, labels...))
			eventually(t, "served", func() bool {
				return f.jobs(t)[7602].State == "serving" && f.ready(t) == 2 && f.svc.Undone(ctx) == 0
			})
			f.gh.mu.Lock()
			f.gh.extra = func(w http.ResponseWriter, r *http.Request) bool {
				switch {
				case answer != "422 token" && r.Method == "GET" && r.URL.Path == "/repos/acme/app/actions/runners":
					w.WriteHeader(map[string]int{"502": http.StatusBadGateway, "404": http.StatusNotFound}[answer])
					fmt.Fprint(w, `{"message":"no"}`)
					return true
				case answer == "422 token" && r.Method == "POST" && r.URL.Path == fmt.Sprintf("/app/installations/%d/access_tokens", inst):
					b, _ := io.ReadAll(r.Body)
					r.Body = io.NopCloser(bytes.NewReader(b))
					var in struct {
						Repositories []string          `json:"repositories"`
						Permissions  map[string]string `json:"permissions"`
					}
					_ = json.Unmarshal(b, &in)
					if slices.Equal(in.Repositories, []string{"app"}) && in.Permissions["administration"] != "" {
						w.WriteHeader(http.StatusUnprocessableEntity)
						fmt.Fprint(w, `{"message":"There is at least one repository that does not exist or is not accessible to the parent installation."}`)
						return true
					}
				}
				return false
			}
			f.gh.mu.Unlock()
			f.restart(t, func(c *config.Config) { c.File.Runners.AllowForkPRs = false })
			eventuallyBox(t, "acme/lib's runner retired", func() bool {
				_, del := f.gh.snapshot()
				return slices.Contains(del, "potluck-7602") && f.jobs(t)[7602].State == "ignored"
			})
			if answer != "502" {
				eventuallyBox(t, "acme/app's box released", func() bool {
					return f.jobs(t)[7601].State == "ignored" && f.ready(t) == 0 && f.svc.Undone(ctx) == 0
				})
			} else if f.jobs(t)[7601].State != "serving" {
				t.Errorf("acme/app's job while GitHub fails: %+v; want serving", f.jobs(t)[7601])
			}
		})
	}
}

// GitHub's busy answer holds until the next sweep only: a runner it listed
// busy whose job was then cancelled before it started is idle again, and
// the sweep's retire retires it.
func TestBusyAnswerExpiresAtTheSweep(t *testing.T) {
	f := newFixtureWith(t, func(c *config.Runners) { c.AllowForkPRs = true })
	ctx := context.Background()
	if w := f.link(t, inst, "good"); w.Code != http.StatusOK {
		t.Fatalf("link: %d %s", w.Code, w.Body)
	}
	f.gh.mu.Lock()
	f.gh.forks[771] = true
	f.gh.status[7701] = "queued"
	f.gh.busy["potluck-7701"] = true
	f.gh.mu.Unlock()
	f.deliver(t, "workflow_job", jobEvent("queued", 7701, 771, "acme/app", true, "self-hosted", "sengi"))
	eventually(t, "served", func() bool { return f.jobs(t)[7701].State == "serving" && f.svc.Undone(ctx) == 0 })
	f.restart(t, func(c *config.Config) { c.File.Runners.AllowForkPRs = false })
	eventually(t, "listed busy", func() bool { return f.runnerLists() >= 1 && f.svc.Undone(ctx) == 0 })
	if _, del := f.gh.snapshot(); len(del) != 0 || f.jobs(t)[7701].State != "serving" {
		t.Fatalf("a runner GitHub lists busy: deleted %v, %+v", del, f.jobs(t)[7701])
	}
	f.gh.mu.Lock()
	delete(f.gh.busy, "potluck-7701")
	f.gh.mu.Unlock()
	f.svc.Sweep(ctx)
	eventuallyBox(t, "retired by the sweep's retire", func() bool {
		_, del := f.gh.snapshot()
		return slices.Contains(del, "potluck-7701") && f.ready(t) == 0 && f.jobs(t)[7701].State == "ignored" && f.svc.Undone(ctx) == 0
	})
}
