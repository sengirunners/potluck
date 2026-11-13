package runners_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/runners"
)

// fakeActions is what GitHub lists for the checker: the runs of
// installation inst's repositories (every one it has runs for, connected or
// not) and the runs' jobs, page by page (per_page, page), each answer with
// an ETag and a 304 to a request that names it. It also answers a job it
// lists by id. It has no list of the installation's repositories: the
// checker never asks for one (that takes a token for the whole
// installation, which the fake GitHub refuses as a misuse).
type fakeActions struct {
	runs    map[string][]*fakeRun // repo -> its runs, newest first
	churn   bool                  // every answer differs from the last (a very busy installation)
	n       int
	counted int            // answers that cost the installation's budget
	free    int            // 304s
	asked   map[string]int // counted answers per repository's runs lists
	// reqs counts requests, answered or refused: "runs <repo>", "jobs <run>",
	// "token" (every installation token).
	reqs map[string]int
	// refuse, when set, answers a request first (true: answered; a failure).
	refuse func(w http.ResponseWriter, r *http.Request) bool
	// listed, when set, runs after a counted answer for path.
	listed func(path string)
}

type fakeRun struct {
	id      int64
	status  string
	updated int // bumped when the run changes
	private bool
	jobs    []*fakeJob
}

type fakeJob struct {
	id                         int64
	status, runner, conclusion string
	labels                     []string
}

var (
	runsListRe = regexp.MustCompile(`^/repos/([^/]+/[^/]+)/actions/runs$`)
	runJobsRe  = regexp.MustCompile(`^/repos/([^/]+/[^/]+)/actions/runs/(\d+)/jobs$`)
	oneJobRe   = regexp.MustCompile(`^/repos/([^/]+/[^/]+)/actions/jobs/(\d+)$`)
)

// actions puts a fakeActions behind f's GitHub. Callers change it under
// f.gh.mu.
func (f *fixture) actions() *fakeActions {
	a := &fakeActions{runs: map[string][]*fakeRun{}, asked: map[string]int{}, reqs: map[string]int{}}
	f.gh.mu.Lock()
	defer f.gh.mu.Unlock()
	f.gh.extra = func(w http.ResponseWriter, r *http.Request) bool { return a.serve(f.gh, w, r) }
	return a
}

// page is items' page as r asks for it.
func page[T any](r *http.Request, items []T) []T {
	per, _ := strconv.Atoi(r.URL.Query().Get("per_page"))
	n, _ := strconv.Atoi(r.URL.Query().Get("page"))
	per, n = max(per, 1), max(n, 1)
	from := min((n-1)*per, len(items))
	return items[from:min(from+per, len(items))]
}

func (a *fakeActions) serve(g *fakeGitHub, w http.ResponseWriter, r *http.Request) bool {
	p := r.URL.Path
	switch {
	case r.Method == "POST" && p == fmt.Sprintf("/app/installations/%d/access_tokens", inst):
		a.reqs["token"]++
	case runsListRe.MatchString(p):
		a.reqs["runs "+runsListRe.FindStringSubmatch(p)[1]]++
	case runJobsRe.MatchString(p):
		a.reqs["jobs "+runJobsRe.FindStringSubmatch(p)[2]]++
	}
	if a.refuse != nil && a.refuse(w, r) {
		return true
	}
	if r.Method != "GET" {
		return false
	}
	var body map[string]any
	repo := ""
	switch {
	case runsListRe.MatchString(p):
		repo = runsListRe.FindStringSubmatch(p)[1]
		st := r.URL.Query().Get("status")
		runs := []map[string]any{}
		for _, run := range a.runs[repo] {
			if run.status == st {
				runs = append(runs, map[string]any{"id": run.id, "status": run.status, "event": "push",
					"updated_at": time.Unix(int64(1_800_000_000+run.updated), 0).UTC().Format(time.RFC3339),
					"repository": map[string]any{"id": repoID(repo), "full_name": repo, "private": run.private}})
			}
		}
		body = map[string]any{"total_count": len(runs), "workflow_runs": page(r, runs)}
	case runJobsRe.MatchString(p):
		m := runJobsRe.FindStringSubmatch(p)
		repo = ""
		jobs := []map[string]any{}
		for _, run := range a.runs[m[1]] {
			if fmt.Sprint(run.id) == m[2] {
				for _, j := range run.jobs {
					jobs = append(jobs, map[string]any{"id": j.id, "status": j.status, "runner_name": j.runner, "labels": j.labels,
						"conclusion": j.conclusion, "run_id": run.id, "run_attempt": 1})
				}
			}
		}
		body = map[string]any{"total_count": len(jobs), "jobs": page(r, jobs)}
	case oneJobRe.MatchString(p):
		m := oneJobRe.FindStringSubmatch(p)
		for _, run := range a.runs[m[1]] {
			for _, j := range run.jobs {
				if fmt.Sprint(j.id) == m[2] {
					fmt.Fprintf(w, `{"id":%d,"status":%q,"conclusion":%q,"runner_name":%q,"run_id":%d,"run_attempt":1}`, j.id, j.status, j.conclusion, j.runner, run.id)
					return true
				}
			}
		}
		return false
	default:
		return false
	}
	if a.churn {
		a.n++
		body["n"] = a.n
	}
	b, _ := json.Marshal(body)
	sum := sha256.Sum256(b)
	tag := `W/"` + hex.EncodeToString(sum[:8]) + `"`
	if r.Header.Get("If-None-Match") == tag {
		a.free++
		w.WriteHeader(http.StatusNotModified)
		return true
	}
	a.counted++
	if repo != "" {
		a.asked[repo]++
	}
	if a.churn { // a busy run changes between lists: its jobs are asked for again
		for _, run := range a.runs[repo] {
			run.updated++
		}
	}
	w.Header().Set("ETag", tag)
	_, _ = w.Write(b)
	if a.listed != nil {
		a.listed(p)
	}
	return true
}

// count is a.reqs[key], read under f.gh.mu.
func (f *fixture) count(a *fakeActions, key string) int {
	f.gh.mu.Lock()
	defer f.gh.mu.Unlock()
	return a.reqs[key]
}

func (f *fixture) setup(t *testing.T) runners.Setup {
	t.Helper()
	var out runners.Setup
	if err := f.owner.Do(context.Background(), "GET", "/v0/runners/setup", nil, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// checked is the checker's last round of the installation, as setup shows it.
func (f *fixture) checked(t *testing.T) runners.CheckedInstallation {
	t.Helper()
	st := f.setup(t)
	if st.Check == nil || len(st.Check.Installations) != 1 {
		t.Fatalf("setup's check: %+v", st.Check)
	}
	return st.Check.Installations[0]
}

// linked is a fixture with the installation linked, exactly repos connected
// (private), and GitHub's lists faked.
func linked(t *testing.T, f *fixture, repos ...string) *fakeActions {
	t.Helper()
	if w := f.link(t, inst, "good"); w.Code != http.StatusOK {
		t.Fatalf("link: %d %s", w.Code, w.Body)
	}
	for name := range f.repos(t) {
		if !slices.Contains(repos, name) {
			f.disconnect(t, name)
		}
	}
	have := f.repos(t)
	for _, r := range repos {
		if _, ok := have[r]; ok {
			continue
		}
		if _, err := f.allow(r, false); err != nil {
			t.Fatalf("allow %s: %v", r, err)
		}
	}
	return f.actions()
}

// disconnect is the pool owner's DELETE of repo's connection.
func (f *fixture) disconnect(t *testing.T, repo string) {
	t.Helper()
	if err := f.owner.Do(context.Background(), "DELETE", "/v0/runners/repos/"+repo, nil, nil); err != nil {
		t.Fatalf("disconnect %s: %v", repo, err)
	}
}

// unreachable restarts f's daemon with no public_url (linking needs one;
// serving does not): the checker is then how jobs arrive.
func (f *fixture) unreachable(t *testing.T) {
	t.Helper()
	f.restart(t, func(c *config.Config) { c.File.Runners.PublicURL = "" })
}

var ours = []string{"self-hosted", "sengi"}

// queue adds a run of repo with one queued job of the pool's, its run's
// event push.
func (f *fixture) queue(a *fakeActions, repo string, run, job int64) (*fakeRun, *fakeJob) {
	j := &fakeJob{id: job, status: "queued", labels: ours}
	r := &fakeRun{id: run, status: "queued", private: true, jobs: []*fakeJob{j}}
	f.gh.mu.Lock()
	a.runs[repo] = append(a.runs[repo], r)
	f.gh.events[run] = "push"
	f.gh.mu.Unlock()
	return r, j
}

// rounds runs n rounds of checking, the clock moved every apart.
func (f *fixture) rounds(n int, every time.Duration) {
	for range n {
		f.svc.Check(context.Background())
		f.clock.advance(every)
	}
}

// A job GitHub has queued is found by asking, with no webhook: the broker
// serves it, sees it start on its runner (its start recorded) and end, and
// releases its box. Asked again with nothing changed, every list is a 304
// (free) and nothing is stored twice. The round is recorded where doctor
// reads it.
func TestCheckServesJobsWithoutWebhooks(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	a := linked(t, f, "acme/app")
	run, job := f.queue(a, "acme/app", 501, 5001)

	f.svc.Check(ctx)
	eventually(t, "a runner for the job GitHub listed", func() bool { return slices.Contains(f.minted(), "potluck-5001") })
	eventually(t, "the job served", func() bool { return f.jobs(t)[5001].State == "serving" && f.svc.Undone(ctx) == 0 })
	if s := f.jobs(t)[5001].StartedAt; s != 0 {
		t.Fatalf("started before GitHub said so: %d", s)
	}

	f.gh.mu.Lock()
	counted, free := a.counted, a.free
	f.gh.mu.Unlock()
	f.svc.Check(ctx) // nothing changed
	f.gh.mu.Lock()
	if a.counted != counted || a.free != free+3 {
		t.Errorf("asked again with nothing changed: %d counted (want %d), %d free (want %d: one 304 per run status)", a.counted, counted, a.free, free+3)
	}
	f.gh.mu.Unlock()
	if m := f.minted(); len(m) != 1 {
		t.Errorf("runners minted: %v, want one", m)
	}

	// GitHub gives the job its runner, then it ends: no webhook says so.
	f.gh.mu.Lock()
	job.status, job.runner, run.status = "in_progress", "potluck-5001", "in_progress"
	run.updated++
	f.gh.mu.Unlock()
	f.svc.Check(ctx)
	eventually(t, "the start recorded", func() bool { return f.jobs(t)[5001].StartedAt != 0 })
	f.gh.mu.Lock()
	job.status, job.conclusion, run.status = "completed", "success", "completed"
	run.updated++
	f.gh.jobDone[5001] = true
	f.gh.mu.Unlock()
	f.svc.Check(ctx)
	eventually(t, "the job done, its runner deleted", func() bool {
		_, deleted := f.gh.snapshot()
		return f.jobs(t)[5001].State == "done" && slices.Contains(deleted, "potluck-5001")
	})

	st := f.setup(t)
	if st.Check == nil || st.Check.Mode != "webhook" || st.Check.EveryS != 300 || len(st.Check.Installations) != 1 {
		t.Fatalf("setup's check: %+v", st.Check)
	}
	in := st.Check.Installations[0]
	if ok, err := time.Parse(time.RFC3339, in.LastOK); err != nil || in.Installation != inst || in.Repos != 1 || in.Error != "" ||
		f.clock.now().Sub(ok) > time.Minute {
		t.Errorf("the installation's last round: %+v", in)
	}
}

// With a public_url the broker checks every five minutes by the wall clock,
// which runs on while a Mac sleeps: a job queued while it slept (no webhook
// reached it) is served at the first tick after it wakes.
func TestCheckPicksUpJobsQueuedWhileAsleep(t *testing.T) {
	f := newFixtureStarting(t, nil, func(s *runners.Service) { s.PollTick = 20 * time.Millisecond })
	a := linked(t, f, "acme/app")
	f.svc.Check(context.Background()) // the round at start, now with the link
	f.queue(a, "acme/app", 502, 5002)

	time.Sleep(300 * time.Millisecond) // many ticks, none due
	if slices.Contains(f.minted(), "potluck-5002") {
		t.Fatal("served before the check was due")
	}
	// The heartbeat (awake_since) beats apart from the rounds; a wake
	// starts it again.
	awake := func() string {
		if c := f.setup(t).Check; c != nil {
			return c.AwakeSince
		}
		return ""
	}
	before := awake()
	if before == "" {
		t.Fatal("no heartbeat after many ticks")
	}
	f.clock.advance(time.Hour) // asleep for an hour
	eventually(t, "the job queued while asleep served after the wake", func() bool { return slices.Contains(f.minted(), "potluck-5002") })
	eventually(t, "awake since the wake", func() bool { return awake() > before })
}

// A clock stepped back (by hand, by NTP) does not stop the checking: the next
// tick checks, and the hour stepped back costs the budget nothing.
func TestCheckAfterTheClockMovesBack(t *testing.T) {
	f := newFixtureStarting(t, nil, func(s *runners.Service) { s.PollTick = 20 * time.Millisecond })
	a := linked(t, f, "acme/app")
	f.svc.Check(context.Background())
	f.queue(a, "acme/app", 503, 5003)
	f.clock.advance(-time.Hour)
	eventually(t, "the job served after the clock moved back", func() bool { return slices.Contains(f.minted(), "potluck-5003") })
	if in := f.checked(t); in.Cut != "" || in.Error != "" {
		t.Errorf("the round after the clock moved back: %+v", in)
	}
}

// Twenty linked repositories: idle, every list after the first round is a
// 304 and costs nothing; busy (every answer new), the checker spends at most
// its own budget, PollBudget an hour plus five minutes' worth saved, well
// inside GitHub's 5000, and still reaches every repository.
func TestCheckStaysInsideTheRateBudgetFor20Repos(t *testing.T) {
	for _, busy := range []bool{false, true} {
		t.Run(map[bool]string{false: "idle", true: "busy"}[busy], func(t *testing.T) {
			f := newFixture(t)
			var repos []string
			for i := range 20 {
				repos = append(repos, fmt.Sprintf("acme/r%02d", i))
			}
			a := linked(t, f, repos...)
			f.gh.mu.Lock()
			a.churn = busy
			if busy { // each repository has a run going on someone else's runner
				for i, r := range repos {
					a.runs[r] = []*fakeRun{{id: int64(700 + i), status: "in_progress", private: true,
						jobs: []*fakeJob{{id: int64(7000 + i), status: "in_progress", runner: "theirs", labels: []string{"ubuntu-latest"}}}}}
				}
			}
			f.gh.mu.Unlock()
			f.rounds(240, 15*time.Second) // an hour of checking every 15 s
			f.gh.mu.Lock()
			defer f.gh.mu.Unlock()
			budget := 1500 + 1500/12
			switch {
			case !busy && a.counted != 20*3:
				t.Errorf("idle: %d counted answers in an hour, want %d (the first round's)", a.counted, 20*3)
			case !busy && a.free < 200*20*3:
				t.Errorf("idle: %d 304s, want a round every 15 s", a.free)
			case busy && (a.counted > budget || a.counted < 1500):
				t.Errorf("busy: %d counted answers in an hour, want the budget spent and no more (%d)", a.counted, budget)
			}
			for _, r := range repos {
				if a.asked[r] == 0 {
					t.Errorf("%s never listed", r)
				}
			}
		})
	}
}

// Every page of every list costs the budget: a repository with 90 queued
// runs (three pages) and a budget of three calls has those pages read and
// nothing more asked.
func TestCheckChargesEveryPage(t *testing.T) {
	f := newFixtureStarting(t, nil, func(s *runners.Service) { s.PollBudget = 3 * 12 })
	a := linked(t, f, "acme/app", "acme/many")
	f.gh.mu.Lock()
	for i := range 90 {
		a.runs["acme/many"] = append(a.runs["acme/many"], &fakeRun{id: int64(30000 + i), status: "queued", private: true})
	}
	f.gh.mu.Unlock()
	f.svc.Check(context.Background()) // acme/app first: its three run lists
	f.clock.advance(time.Hour)        // a full bucket again; the round goes on from acme/app
	f.gh.mu.Lock()
	counted := a.counted
	f.gh.mu.Unlock()
	f.svc.Check(context.Background())
	f.gh.mu.Lock()
	spent := a.counted - counted
	f.gh.mu.Unlock()
	if spent != 3 {
		t.Errorf("%d counted answers, want the bucket's three", spent)
	}
	if in := f.checked(t); !strings.Contains(in.Cut, "budget") || in.LastOK != "" || in.Repos != 2 {
		t.Errorf("the round: %+v", in)
	}
}

// A job found by checking is refused for the reasons a webhook's would be,
// in the same words: a fork's run, a public repository, and the events that
// run code the pool does not trust (pull_request_target, workflow_run,
// issue_comment).
func TestCheckRefusesWhatWebhooksRefuse(t *testing.T) {
	f := newFixture(t)
	a := linked(t, f, "acme/app")
	type pair struct {
		name            string
		event           string
		fork, private   bool
		checked, hooked int64 // job ids: found by checking, delivered by webhook
		cRun, hRun      int64
	}
	pairs := []pair{
		{"a fork's pull request", "pull_request", true, true, 6001, 6101, 601, 611},
		{"a public repository's push", "push", false, false, 6002, 6102, 602, 612},
		{"a pull_request_target run", "pull_request_target", false, true, 6003, 6103, 603, 613},
		{"a workflow_run run", "workflow_run", false, true, 6004, 6104, 604, 614},
		{"an issue_comment run", "issue_comment", false, true, 6005, 6105, 605, 615},
	}
	f.gh.mu.Lock()
	for _, p := range pairs {
		for _, run := range []int64{p.cRun, p.hRun} {
			f.gh.forks[run] = p.fork
			f.gh.events[run] = p.event
		}
		a.runs["acme/app"] = append(a.runs["acme/app"], &fakeRun{id: p.cRun, status: "queued", private: p.private,
			jobs: []*fakeJob{{id: p.checked, status: "queued", labels: ours}}})
	}
	f.gh.mu.Unlock()
	for _, p := range pairs {
		if code := f.deliver(t, "workflow_job", jobEvent("queued", p.hooked, p.hRun, "acme/app", p.private, ours...)); code != http.StatusAccepted {
			t.Fatalf("%s: delivery %d", p.name, code)
		}
	}
	f.svc.Check(context.Background())
	eventually(t, "every job refused", func() bool {
		js := f.jobs(t)
		for _, p := range pairs {
			if js[p.checked].State != "ignored" || js[p.hooked].State != "ignored" {
				return false
			}
		}
		return true
	})
	js := f.jobs(t)
	for _, p := range pairs {
		if c, h := js[p.checked].Why, js[p.hooked].Why; c != h || c == "" {
			t.Errorf("%s: checked refused with %q, webhook with %q", p.name, c, h)
		}
	}
	if m := f.minted(); len(m) != 0 {
		t.Errorf("runners minted for refused jobs: %v", m)
	}
}

// With no public_url the checker is how jobs arrive, every 15 s: a job queued
// after a round is served once 15 s have gone by, not before.
func TestCheckModeWithNoPublicAddress(t *testing.T) {
	f := newFixtureStarting(t, nil, func(s *runners.Service) { s.PollTick = 20 * time.Millisecond })
	a := linked(t, f, "acme/app")
	f.unreachable(t)
	if c := f.setup(t).Check; c == nil || c.Mode != "check" || c.EveryS != 15 {
		t.Fatalf("setup's check: %+v", c)
	}
	f.svc.Check(context.Background())
	f.queue(a, "acme/app", 504, 5004)
	time.Sleep(300 * time.Millisecond) // many ticks, none due
	if slices.Contains(f.minted(), "potluck-5004") {
		t.Fatal("served before the check was due")
	}
	f.clock.advance(16 * time.Second)
	eventually(t, "the job served at the next round", func() bool { return slices.Contains(f.minted(), "potluck-5004") })
}

// A repository whose runs cannot be listed does not hold up the others: the
// round records it and goes on, and it is asked again less and less often
// while it keeps failing, its failure in every round's record.
func TestCheckGoesOnPastAFailingRepo(t *testing.T) {
	f := newFixture(t)
	a := linked(t, f, "acme/bad", "acme/app")
	f.unreachable(t)
	f.gh.mu.Lock()
	a.refuse = func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/repos/acme/bad/actions/runs" {
			return false
		}
		w.WriteHeader(http.StatusBadGateway)
		fmt.Fprint(w, `{"message":"Server Error"}`)
		return true
	}
	f.gh.mu.Unlock()
	f.queue(a, "acme/app", 505, 5005)
	f.rounds(10, 15*time.Second)
	eventually(t, "the job of the repository after the failing one served", func() bool { return slices.Contains(f.minted(), "potluck-5005") })
	if bad, app := f.count(a, "runs acme/bad"), f.count(a, "runs acme/app"); app != 10*3 || bad > 4 || bad < 2 {
		t.Errorf("in 10 rounds: acme/bad asked %d times (want it backing off), acme/app %d (want every round's 3)", bad, app)
	}
	in := f.checked(t)
	if !strings.Contains(in.Error, "listing acme/bad's queued runs") || !strings.Contains(in.Error, "502") || in.LastOK != "" {
		t.Errorf("the round: %+v", in)
	}
}

// A run whose jobs cannot be listed (GitHub fails, the answer is over the
// read limit) leaves the round's record failed, naming the run and the list;
// it is asked again less and less often. A round that cannot begin (no App
// client) is recorded too.
func TestCheckRecordsFailedJobLists(t *testing.T) {
	f := newFixture(t)
	a := linked(t, f, "acme/app")
	f.unreachable(t)
	f.queue(a, "acme/app", 506, 5006)
	f.queue(a, "acme/app", 507, 5007)
	f.gh.mu.Lock()
	a.refuse = func(w http.ResponseWriter, r *http.Request) bool {
		switch r.URL.Path {
		case "/repos/acme/app/actions/runs/506/jobs":
			w.WriteHeader(http.StatusBadGateway)
			return true
		case "/repos/acme/app/actions/runs/507/jobs":
			fmt.Fprintf(w, `{"total_count":1,"jobs":[{"id":5007,"status":"queued","name":%q}]}`, strings.Repeat("x", 1<<20))
			return true
		}
		return false
	}
	f.gh.mu.Unlock()
	f.rounds(10, 15*time.Second)
	in := f.checked(t)
	if !strings.Contains(in.Error, "listing the jobs of acme/app's run 506") || !strings.Contains(in.Error, "502") ||
		!strings.Contains(in.Error, "listing the jobs of acme/app's run 507") || !strings.Contains(in.Error, "over 1 MiB") || in.LastOK != "" {
		t.Errorf("the round: %+v", in)
	}
	if n := f.count(a, "jobs 506"); n > 4 || n < 2 {
		t.Errorf("the failing jobs list asked %d times in 10 rounds, want it backing off", n)
	}
	if m := f.minted(); len(m) != 0 {
		t.Errorf("runners minted: %v", m)
	}

	g := newFixtureWith(t, func(r *config.Runners) { r.GitHub.PrivateKey = "file:/nonexistent/key.pem" })
	g.svc.Check(context.Background())
	if c := g.setup(t).Check; c == nil || !strings.Contains(c.Error, "the GitHub App's client") || c.ErrorAt == "" {
		t.Errorf("a round with no App client: %+v", c)
	}
}

// A job whose delivery could not be stored is listed again (its list's ETag
// is kept only once every store succeeded) and served once it can be.
func TestCheckStoresBeforeKeepingTheETag(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	a := linked(t, f, "acme/app")
	f.unreachable(t)
	f.queue(a, "acme/app", 508, 5008)
	reg := f.dm.Deps.Store.Registry()
	if _, err := reg.ExecContext(ctx, `CREATE TRIGGER full_disk BEFORE INSERT ON runners_inbox WHEN NEW.delivery LIKE 'check-%'
		BEGIN SELECT RAISE(ABORT, 'disk full'); END`); err != nil {
		t.Fatal(err)
	}
	f.svc.Check(ctx)
	if in := f.checked(t); !strings.Contains(in.Error, "storing what the jobs of acme/app's run 508 say") || !strings.Contains(in.Error, "disk full") {
		t.Errorf("the round: %+v", in)
	}
	if _, err := reg.ExecContext(ctx, `DROP TRIGGER full_disk`); err != nil {
		t.Fatal(err)
	}
	f.clock.advance(time.Minute)
	f.svc.Check(ctx)
	eventually(t, "the job served once it could be stored", func() bool { return slices.Contains(f.minted(), "potluck-5008") })
}

// Every page is read: a queued run older than the newest 30, and a job past
// the first page of a large run's jobs. Past the most pages read, the round
// says so.
func TestCheckReadsEveryPage(t *testing.T) {
	f := newFixtureStarting(t, nil, func(s *runners.Service) { s.PollBudget = 1 << 20 })
	a := linked(t, f, "acme/app", "acme/many")
	f.gh.mu.Lock()
	for i := range 40 { // newest first: the job's run is the oldest
		a.runs["acme/app"] = append(a.runs["acme/app"], &fakeRun{id: int64(1139 - i), status: "queued", private: true})
	}
	a.runs["acme/app"][39].jobs = []*fakeJob{{id: 11000, status: "queued", labels: ours}}
	big := &fakeRun{id: 1200, status: "in_progress", private: true}
	for i := range 120 {
		j := &fakeJob{id: int64(12001 + i), status: "in_progress", runner: "theirs", labels: []string{"ubuntu-latest"}}
		if i == 100 {
			j.status, j.runner, j.labels = "queued", "", ours
		}
		big.jobs = append(big.jobs, j)
	}
	a.runs["acme/app"] = append(a.runs["acme/app"], big)
	for i := range 301 {
		a.runs["acme/many"] = append(a.runs["acme/many"], &fakeRun{id: int64(20000 + i), status: "queued", private: true})
	}
	f.gh.events[1100], f.gh.events[1200] = "push", "push"
	f.gh.mu.Unlock()
	f.svc.Check(context.Background())
	eventually(t, "both jobs found", func() bool {
		js := f.jobs(t)
		_, old := js[11000]
		_, late := js[12101]
		return old && late
	})
	if in := f.checked(t); !strings.Contains(in.Cut, "only the newest 300 of acme/many's queued runs are read") {
		t.Errorf("the round: %+v", in)
	}
}

// After GitHub refuses for a rate limit (403 with Retry-After) the round
// stops for the installation, and no list is asked until Retry-After is over.
func TestCheckStopsForGitHubsRateLimit(t *testing.T) {
	f := newFixture(t)
	a := linked(t, f, "acme/app")
	f.unreachable(t)
	f.queue(a, "acme/app", 509, 5009)
	f.queue(a, "acme/app", 510, 5010)
	limited := true
	f.gh.mu.Lock()
	a.refuse = func(w http.ResponseWriter, r *http.Request) bool {
		if !limited || !runJobsRe.MatchString(r.URL.Path) {
			return false
		}
		w.Header().Set("Retry-After", "120")
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"message":"You have exceeded a secondary rate limit."}`)
		return true
	}
	f.gh.mu.Unlock()
	f.svc.Check(context.Background())
	if n := f.count(a, "jobs 509") + f.count(a, "jobs 510"); n != 1 {
		t.Errorf("job lists asked after the refusal in the same round: %d asked, want only the refused one", n)
	}
	if in := f.checked(t); !strings.Contains(in.Cut, "waiting for its reset") || !strings.Contains(in.Error, "403") {
		t.Errorf("the round: %+v", in)
	}
	f.gh.mu.Lock()
	limited = false
	before := len(f.gh.calls)
	f.gh.mu.Unlock()
	f.rounds(4, 15*time.Second) // a minute: inside Retry-After
	f.gh.mu.Lock()
	if calls := f.gh.calls[before:]; len(calls) != 0 {
		t.Errorf("asked GitHub inside Retry-After: %v", calls)
	}
	f.gh.mu.Unlock()
	f.clock.advance(2 * time.Minute)
	f.svc.Check(context.Background())
	eventually(t, "both jobs found after Retry-After", func() bool {
		js := f.jobs(t)
		_, one := js[5009]
		_, two := js[5010]
		return one && two
	})
}

// The checker's budget outlives the daemon: one restarting again and again
// does not start each time with a full bucket.
func TestCheckBudgetSurvivesRestarts(t *testing.T) {
	f := newFixtureStarting(t, nil, func(s *runners.Service) { s.PollBudget = 10 * 12 })
	var repos []string
	for i := range 10 {
		repos = append(repos, fmt.Sprintf("acme/r%02d", i))
	}
	a := linked(t, f, repos...)
	f.gh.mu.Lock()
	a.churn = true
	f.gh.mu.Unlock()
	f.svc.Check(context.Background())
	for range 3 {
		f.restart(t)
		f.svc.Check(context.Background())
	}
	f.gh.mu.Lock()
	defer f.gh.mu.Unlock()
	if a.counted > 11 {
		t.Errorf("%d counted answers over 4 starts, want the one bucket of 10 (and what refilled meanwhile)", a.counted)
	}
}

// A restarted broker checks again from nothing and serves nothing twice: a
// job it served before is known.
func TestCheckAfterARestart(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	a := linked(t, f, "acme/app")
	f.queue(a, "acme/app", 511, 5011)
	f.svc.Check(ctx)
	eventually(t, "the job served", func() bool { return f.jobs(t)[5011].State == "serving" && f.svc.Undone(ctx) == 0 })
	f.restart(t)
	f.svc.Check(ctx)
	if in := f.checked(t); in.LastOK == "" || in.Error != "" {
		t.Errorf("the round after the restart: %+v", in)
	}
	time.Sleep(200 * time.Millisecond)
	if m := f.minted(); len(m) != 1 || f.jobs(t)[5011].State != "serving" {
		t.Errorf("after the restart: minted %v, job %+v", m, f.jobs(t)[5011])
	}
}

// An installation GitHub no longer has (uninstalled: its tokens refused
// with a 404) backs off as a whole, not once per connected repository: it
// is asked again less and less often, up to the catch-up interval, its
// failure in the record once.
func TestCheckBacksOffAnUninstalledApp(t *testing.T) {
	f := newFixture(t)
	a := linked(t, f, "acme/app", "acme/lib", "acme/other")
	f.unreachable(t)
	f.gh.mu.Lock()
	a.refuse = func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method != "POST" || r.URL.Path != fmt.Sprintf("/app/installations/%d/access_tokens", inst) {
			return false
		}
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"message":"Not Found"}`)
		return true
	}
	f.gh.mu.Unlock()
	f.rounds(40, 15*time.Second) // ten minutes
	if n := f.count(a, "token"); n > 6 || n < 3 {
		t.Errorf("tokens asked for %d times in ten minutes for three repositories, want one installation backing off to every 5 min", n)
	}
	if in := f.checked(t); !strings.Contains(in.Error, "the App's installation") || !strings.Contains(in.Error, "404") ||
		strings.Count(in.Error, "404") != 1 {
		t.Errorf("the round: %+v", in)
	}
}

// A round cut short goes on from the repository it stopped at, by name,
// though a repository that sorts before it was connected meanwhile (by its
// old place, the round would begin with acme/app again).
func TestCheckResumesByName(t *testing.T) {
	f := newFixtureStarting(t, nil, func(s *runners.Service) { s.PollBudget = 4 * 12 })
	a := linked(t, f, "acme/app", "acme/lib")
	f.gh.mu.Lock()
	a.churn = true // every answer counted: no 304 makes a list free
	f.gh.mu.Unlock()
	f.svc.Check(context.Background()) // acme/app's three lists, acme/lib's first: the bucket
	if n := f.count(a, "runs acme/lib"); n != 1 {
		t.Fatalf("acme/lib's lists asked %d times in the first round, want one", n)
	}
	if _, err := f.allow("acme/aaa", false); err != nil { // sorts first
		t.Fatal(err)
	}
	f.clock.advance(15 * time.Minute) // the bucket full again: four calls
	f.svc.Check(context.Background())
	if n := f.count(a, "runs acme/lib"); n != 4 {
		t.Errorf("acme/lib's lists asked %d times after two rounds, want the second round to begin with it (4)", n)
	}
	if n := f.count(a, "runs acme/aaa"); n != 1 {
		t.Errorf("acme/aaa's lists asked %d times, want the one call left after acme/lib's three", n)
	}
}

// The same job delivered by webhook and found by checking, in either order,
// is served once.
func TestCheckAndWebhookServeAJobOnce(t *testing.T) {
	for _, hookFirst := range []bool{true, false} {
		t.Run(map[bool]string{true: "webhook first", false: "check first"}[hookFirst], func(t *testing.T) {
			f := newFixture(t)
			ctx := context.Background()
			a := linked(t, f, "acme/app")
			f.queue(a, "acme/app", 512, 5012)
			hook := func() {
				if code := f.deliver(t, "workflow_job", jobEvent("queued", 5012, 512, "acme/app", true, ours...)); code != http.StatusAccepted {
					t.Fatalf("delivery %d", code)
				}
			}
			first, second := hook, func() { f.svc.Check(ctx) }
			if !hookFirst {
				first, second = second, first
			}
			first()
			eventually(t, "the job served", func() bool { return f.jobs(t)[5012].State == "serving" && f.svc.Undone(ctx) == 0 })
			second()
			eventually(t, "the second copy applied", func() bool { return f.svc.Undone(ctx) == 0 })
			if m := f.minted(); len(m) != 1 || f.jobs(t)[5012].State != "serving" {
				t.Errorf("minted %v, job %+v", m, f.jobs(t)[5012])
			}
		})
	}
}

// A job GitHub finished between the list and the serve is not served.
func TestCheckJobCompletedBeforeItsServe(t *testing.T) {
	f := newFixture(t)
	a := linked(t, f, "acme/app")
	_, job := f.queue(a, "acme/app", 513, 5013)
	f.gh.mu.Lock()
	a.listed = func(p string) {
		if p == "/repos/acme/app/actions/runs/513/jobs" {
			job.status, job.conclusion = "completed", "success"
		}
	}
	f.gh.mu.Unlock()
	f.svc.Check(context.Background())
	eventually(t, "the job done unserved", func() bool { return f.jobs(t)[5013].State == "done" })
	if m := f.minted(); len(m) != 0 {
		t.Errorf("runners minted: %v", m)
	}
}

// A served job cancelled while still queued (it never started) ends as the
// checking finds it: its box released, its runner deleted.
func TestCheckJobCancelledWhileQueued(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	a := linked(t, f, "acme/app")
	run, job := f.queue(a, "acme/app", 514, 5014)
	f.svc.Check(ctx)
	eventually(t, "the job served", func() bool { return f.jobs(t)[5014].State == "serving" && f.svc.Undone(ctx) == 0 })
	f.gh.mu.Lock()
	job.status, job.conclusion, run.status = "completed", "cancelled", "completed"
	run.updated++
	f.gh.jobDone[5014] = true
	f.gh.mu.Unlock()
	f.svc.Check(ctx)
	eventually(t, "the cancelled job done, its runner deleted", func() bool {
		_, deleted := f.gh.snapshot()
		return f.jobs(t)[5014].State == "done" && slices.Contains(deleted, "potluck-5014")
	})
}

// A budget that runs out before a run's jobs are listed leaves the round
// short: the job is not served, and last_ok does not say every list was
// asked.
func TestCheckBudgetSpentBeforeTheJobsAreListed(t *testing.T) {
	f := newFixtureStarting(t, nil, func(s *runners.Service) { s.PollBudget = 3 * 12 })
	a := linked(t, f, "acme/app")
	f.queue(a, "acme/app", 515, 5015)
	f.svc.Check(context.Background()) // the three run lists: the bucket's three calls
	if n := f.count(a, "runs acme/app"); n != 3 || f.count(a, "jobs 515") != 0 {
		t.Fatalf("%d calls before the jobs list, %d job lists: want the three and none", n, f.count(a, "jobs 515"))
	}
	in := f.checked(t)
	if in.LastOK != "" || !strings.Contains(in.Cut, "budget") {
		t.Errorf("the round: %+v, want no last_ok and the budget named", in)
	}
	if m := f.minted(); len(m) != 0 {
		t.Errorf("runners minted with the job never listed: %v", m)
	}
}

// A repository that still fails stays in the record when a later round ends
// before reaching it (the budget).
func TestCheckKeepsAFailingRepoInTheRecord(t *testing.T) {
	f := newFixtureStarting(t, nil, func(s *runners.Service) { s.PollBudget = 4 * 12 })
	a := linked(t, f, "acme/app", "acme/bad")
	f.unreachable(t) // keeps PollBudget: a restart runs the same before-hook
	f.gh.mu.Lock()
	a.refuse = func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/repos/acme/bad/actions/runs" {
			return false
		}
		w.WriteHeader(http.StatusBadGateway)
		return true
	}
	f.gh.mu.Unlock()
	f.svc.Check(context.Background()) // 3 + 1 calls: the bucket
	if in := f.checked(t); !strings.Contains(in.Error, "acme/bad") {
		t.Fatalf("the failing round: %+v", in)
	}
	f.clock.advance(20 * time.Second) // too little to afford a call, inside bad's back-off
	f.svc.Check(context.Background())
	in := f.checked(t)
	if !strings.Contains(in.Cut, "budget") || !strings.Contains(in.Error, "listing acme/bad's queued runs") || in.ErrorAt == "" {
		t.Errorf("the round cut short by the budget: %+v, want acme/bad's failure still recorded", in)
	}
}

// A shutdown in the middle of a round stops it quietly: the repositories not
// reached are not counted as failures (no back-off, nothing recorded), and
// the next round asks them all.
func TestCheckStopsQuietlyAtShutdown(t *testing.T) {
	f := newFixture(t)
	a := linked(t, f, "acme/a", "acme/b", "acme/c")
	ctx, cancel := context.WithCancel(context.Background())
	f.gh.mu.Lock()
	a.refuse = func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path == "/repos/acme/a/actions/runs" {
			cancel()
		}
		return false
	}
	f.gh.mu.Unlock()
	f.svc.Check(ctx)
	if in := f.checked(t); in.Error != "" || in.LastOK != "" {
		t.Errorf("the round a shutdown ended: %+v, want nothing recorded", in)
	}
	f.gh.mu.Lock()
	a.refuse = nil
	f.gh.mu.Unlock()
	f.svc.Check(context.Background()) // no time has passed: a failure's back-off would skip a repository
	for _, r := range []string{"acme/a", "acme/b", "acme/c"} {
		if n := f.count(a, "runs "+r); n < 3 {
			t.Errorf("%s's run lists asked %d times, want all three of the next round's (a shutdown is no failure)", r, n)
		}
	}
	if in := f.checked(t); in.Error != "" || in.LastOK == "" {
		t.Errorf("the round after: %+v", in)
	}
}

// lowRate makes GitHub's answers to repo's queued runs say the
// installation's budget is spent (reset in an hour).
func lowRate(repo string) func(w http.ResponseWriter, r *http.Request) bool {
	return func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path == "/repos/"+repo+"/actions/runs" && r.URL.Query().Get("status") == "queued" {
			w.Header().Set("X-RateLimit-Limit", "5000")
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.Header().Set("X-RateLimit-Reset", fmt.Sprint(time.Now().Add(time.Hour).Unix()))
		}
		return false
	}
}

// GitHub's rate hold is read between the pages of a list: a run list of
// three pages whose first answer says the budget is spent is not read on.
func TestCheckHoldsBetweenThePagesOfAList(t *testing.T) {
	f := newFixtureStarting(t, nil, func(s *runners.Service) { s.PollBudget = 1 << 20 })
	a := linked(t, f, "acme/many")
	f.gh.mu.Lock()
	for i := range 90 {
		a.runs["acme/many"] = append(a.runs["acme/many"], &fakeRun{id: int64(30000 + i), status: "queued", private: true})
	}
	a.refuse = lowRate("acme/many")
	f.gh.mu.Unlock()
	f.svc.Check(context.Background())
	if n := f.count(a, "runs acme/many"); n != 1 {
		t.Errorf("%d run pages asked after GitHub said the budget was spent, want the first only", n)
	}
	if in := f.checked(t); !strings.Contains(in.Cut, "waiting for its reset") || in.LastOK != "" || in.Error != "" {
		t.Errorf("the round: %+v", in)
	}
}

// It is read after each list too: when one repository's list says the
// budget is spent, no other repository's runs are asked.
func TestCheckHoldsAfterAList(t *testing.T) {
	f := newFixture(t)
	a := linked(t, f, "acme/app", "acme/lib")
	f.gh.mu.Lock()
	a.refuse = lowRate("acme/app")
	f.gh.mu.Unlock()
	f.svc.Check(context.Background())
	if n := f.count(a, "runs acme/lib"); n != 0 {
		t.Errorf("%d run lists of acme/lib asked after acme/app's list said the budget was spent", n)
	}
	if in := f.checked(t); !strings.Contains(in.Cut, "waiting for its reset") || in.LastOK != "" {
		t.Errorf("the round: %+v", in)
	}
}

// Only connected repositories are asked about. With 500 repositories of the
// installation that have runs waiting and 2 connected, a round lists the 2;
// a repository connected a moment ago is listed in the next round; one
// disconnected is never listed again, and what the checker remembered of it
// (its lists' ETags) goes, so connecting it again reads it afresh.
func TestCheckListsOnlyConnectedRepos(t *testing.T) {
	f := newFixtureStarting(t, nil, func(s *runners.Service) { s.PollBudget = 1 << 20 })
	ctx := context.Background()
	a := linked(t, f, "acme/app", "acme/lib")
	f.gh.mu.Lock()
	for i := range 500 {
		repo := fmt.Sprintf("acme/r%03d", i)
		if i == 0 {
			repo = "acme/app"
		} else if i == 1 {
			repo = "acme/lib"
		}
		a.runs[repo] = []*fakeRun{{id: int64(40000 + i), status: "queued", private: true,
			jobs: []*fakeJob{{id: int64(400000 + i), status: "queued", labels: ours}}}}
		f.gh.events[int64(40000+i)] = "push"
	}
	f.gh.mu.Unlock()
	listedRepos := func() (out []string) {
		f.gh.mu.Lock()
		defer f.gh.mu.Unlock()
		for k := range a.reqs {
			if repo, ok := strings.CutPrefix(k, "runs "); ok {
				out = append(out, repo)
			}
		}
		slices.Sort(out)
		return out
	}

	f.svc.Check(ctx)
	if got := listedRepos(); !slices.Equal(got, []string{"acme/app", "acme/lib"}) {
		t.Fatalf("repositories listed: %v, want the two connected", got)
	}
	if in := f.checked(t); in.Repos != 2 || in.LastOK == "" {
		t.Errorf("the round: %+v", in)
	}
	eventually(t, "the connected repositories' jobs served", func() bool {
		m := f.minted()
		return slices.Contains(m, "potluck-400000") && slices.Contains(m, "potluck-400001") && f.svc.Undone(ctx) == 0
	})

	// Connected a moment ago: the next round lists it.
	if _, err := f.allow("acme/r007", false); err != nil {
		t.Fatal(err)
	}
	f.clock.advance(15 * time.Second)
	f.svc.Check(ctx)
	if got := listedRepos(); !slices.Equal(got, []string{"acme/app", "acme/lib", "acme/r007"}) {
		t.Fatalf("repositories listed after connecting acme/r007: %v", got)
	}
	if in := f.checked(t); in.Repos != 3 {
		t.Errorf("the round after connecting: %+v", in)
	}

	// Disconnected: never listed again.
	f.disconnect(t, "acme/lib")
	before := f.count(a, "runs acme/lib")
	for range 3 {
		f.clock.advance(15 * time.Second)
		f.svc.Check(ctx)
	}
	if n := f.count(a, "runs acme/lib") - before; n != 0 {
		t.Errorf("acme/lib's runs listed %d times after it was disconnected", n)
	}
	if in := f.checked(t); in.Repos != 2 {
		t.Errorf("the round after disconnecting: %+v", in)
	}
	if got := listedRepos(); slices.ContainsFunc(got, func(r string) bool { return r != "acme/app" && r != "acme/lib" && r != "acme/r007" }) {
		t.Errorf("an unconnected repository listed: %v", got)
	}

	// Connected again: its lists are read afresh (counted, not 304s on the
	// ETags of before).
	if _, err := f.allow("acme/lib", false); err != nil {
		t.Fatal(err)
	}
	f.gh.mu.Lock()
	asked := a.asked["acme/lib"]
	f.gh.mu.Unlock()
	f.clock.advance(15 * time.Second)
	f.svc.Check(ctx)
	f.gh.mu.Lock()
	again := a.asked["acme/lib"] - asked
	f.gh.mu.Unlock()
	if again != 3 {
		t.Errorf("acme/lib reconnected: %d counted run lists, want all three read afresh", again)
	}
}

// A suspended installation (403 on its token) or an App key GitHub does not
// accept (401) fails every repository alike: one back-off and one line in the
// round's record, not one per connected repository.
func TestCheckBacksOffARefusedInstallation(t *testing.T) {
	for _, tt := range []struct {
		status int
		msg    string
	}{{http.StatusForbidden, "This installation has been suspended"}, {http.StatusUnauthorized, "Bad credentials"}} {
		t.Run(fmt.Sprint(tt.status), func(t *testing.T) {
			f := newFixture(t)
			a := linked(t, f, "acme/app", "acme/lib", "acme/other")
			f.unreachable(t)
			f.gh.mu.Lock()
			a.refuse = func(w http.ResponseWriter, r *http.Request) bool {
				if r.Method != "POST" || r.URL.Path != fmt.Sprintf("/app/installations/%d/access_tokens", inst) {
					return false
				}
				w.WriteHeader(tt.status)
				fmt.Fprintf(w, `{"message":%q}`, tt.msg)
				return true
			}
			f.gh.mu.Unlock()
			f.rounds(40, 15*time.Second) // ten minutes
			if n := f.count(a, "token"); n > 6 || n < 3 {
				t.Errorf("tokens asked for %d times in ten minutes for three repositories, want one installation backing off to every 5 min", n)
			}
			if in := f.checked(t); !strings.Contains(in.Error, "the App's installation") || strings.Count(in.Error, fmt.Sprint(tt.status)) != 1 {
				t.Errorf("the round: %+v", in)
			}
		})
	}
}

// Once GitHub answers for the installation again, the error that it has no
// such installation goes from the record though no repository's lists all
// succeed (here each fails on its own).
func TestCheckForgetsAnInstallationErrorOnceItAnswers(t *testing.T) {
	f := newFixture(t)
	a := linked(t, f, "acme/app", "acme/lib")
	f.unreachable(t)
	f.gh.mu.Lock()
	a.refuse = func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method == "POST" && r.URL.Path == fmt.Sprintf("/app/installations/%d/access_tokens", inst) {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"message":"Not Found"}`)
			return true
		}
		return false
	}
	f.gh.mu.Unlock()
	f.svc.Check(context.Background())
	if in := f.checked(t); !strings.Contains(in.Error, "the App's installation") {
		t.Fatalf("the round: %+v", in)
	}
	f.gh.mu.Lock()
	a.refuse = func(w http.ResponseWriter, r *http.Request) bool {
		if runsListRe.MatchString(r.URL.Path) {
			w.WriteHeader(http.StatusInternalServerError)
			return true
		}
		return false
	}
	f.gh.mu.Unlock()
	f.clock.advance(time.Hour) // its back-off is over
	f.svc.Check(context.Background())
	if in := f.checked(t); strings.Contains(in.Error, "the App's installation") || !strings.Contains(in.Error, "listing acme/app's") {
		t.Errorf("the round: %+v", in)
	}
}

// The checker's pace counts the token each repository needs now and then: a
// round of n repositories asks up to n·(lists+1) times, and stays under
// pollPerMinute (300).
func TestCheckPaceCountsTokenMints(t *testing.T) {
	f := newFixture(t)
	var repos []string
	for i := range 30 {
		repos = append(repos, fmt.Sprintf("acme/r%02d", i))
	}
	linked(t, f, repos...)
	f.unreachable(t)
	f.svc.Check(context.Background())
	if got, want := f.svc.Every(), 30*4*time.Minute/300; got != want {
		t.Errorf("a round every %s for 30 repositories, want %s (three lists and a token each)", got, want)
	}
}

// The run lists leave part of the bucket for the jobs: with more repositories
// than the bucket's lists can cover, the jobs of the runs found are still
// listed.
func TestCheckListsJobsWhenTheRunListsCostTheBucket(t *testing.T) {
	f := newFixtureStarting(t, nil, func(s *runners.Service) { s.PollBudget = 8 * 12 }) // a bucket of 8
	a := linked(t, f, "acme/a", "acme/b", "acme/c")
	f.queue(a, "acme/a", 515, 5015)
	f.svc.Check(context.Background())
	if n := f.count(a, "jobs 515"); n != 1 {
		t.Errorf("%d job lists of the run found by the first repository's lists, want 1 (nine list calls do not fit the bucket)", n)
	}
	if in := f.checked(t); !strings.Contains(in.Cut, "budget") {
		t.Errorf("the round: %+v, want the budget named", in)
	}
}

// A failure to reach GitHub at all says nothing about the installation: its
// error stays in the record until GitHub answers.
func TestCheckKeepsAnInstallationErrorThroughANetworkFailure(t *testing.T) {
	f := newFixture(t)
	a := linked(t, f, "acme/app")
	f.unreachable(t)
	f.gh.mu.Lock()
	a.refuse = func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method == "POST" && r.URL.Path == fmt.Sprintf("/app/installations/%d/access_tokens", inst) {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"message":"Not Found"}`)
			return true
		}
		return false
	}
	f.gh.mu.Unlock()
	f.svc.Check(context.Background())
	f.gh.mu.Lock()
	a.refuse = func(w http.ResponseWriter, r *http.Request) bool {
		if runsListRe.MatchString(r.URL.Path) {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				conn.Close() // the answer never comes
			}
			return true
		}
		return false
	}
	f.gh.mu.Unlock()
	f.clock.advance(time.Hour)
	f.svc.Check(context.Background())
	if in := f.checked(t); !strings.Contains(in.Error, "the App's installation") || !strings.Contains(in.Error, "listing acme/app's") {
		t.Errorf("the round: %+v", in)
	}
}
