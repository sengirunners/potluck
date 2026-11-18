package runners_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/internal/forkguard"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/protocol"
	"github.com/sengirunners/potluck/runners"
)

// doctorTransport answers the App calls the fake GitHub does not: GET /app,
// the owner's installation (ownerInst, default inst; owners' for others),
// and leaves acme/web out of the installation unless covered. A repository
// renamed (old name to new) answers its old name with its new one.
type doctorTransport struct {
	down      atomic.Bool // no route to GitHub: every call fails
	next      http.RoundTripper
	covered   bool
	ownerInst int64
	owners    map[string]int64
	renamed   map[string]string
}

func (d *doctorTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	answer := func(code int, body string) (*http.Response, error) {
		return &http.Response{StatusCode: code, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	}
	if d.down.Load() {
		return nil, errors.New("dial tcp: connect: no route to host")
	}
	if to := d.renamed[strings.TrimPrefix(r.URL.Path, "/repos/")]; to != "" {
		return answer(200, fmt.Sprintf(`{"id":%d,"full_name":%q,"private":true,"visibility":"private"}`, repoID(strings.TrimPrefix(r.URL.Path, "/repos/")), to))
	}
	if owner, ok := strings.CutPrefix(r.URL.Path, "/users/"); ok && d.owners[strings.TrimSuffix(owner, "/installation")] != 0 {
		return answer(200, fmt.Sprintf(`{"id":%d}`, d.owners[strings.TrimSuffix(owner, "/installation")]))
	}
	switch r.URL.Path {
	case "/app":
		return answer(200, `{"id":1}`)
	case "/users/acme/installation":
		return answer(200, fmt.Sprintf(`{"id":%d}`, max(d.ownerInst, inst)))
	case "/repos/acme/web/installation":
		if !d.covered {
			return answer(404, `{"message":"Not Found"}`)
		}
	}
	return d.next.RoundTrip(r)
}

// doctorReader is the owner's gh: one workflow file ci.yml in every
// repository, and (waiting) one in-progress run 901 whose job 902 for the
// pool waits.
func doctorReader(workflow string, waiting bool) runners.PersonRead {
	return func(ctx context.Context, path string, out any) error {
		var data any
		switch {
		case strings.HasSuffix(path, "/contents/.github/workflows"):
			data = []any{map[string]any{"name": "ci.yml", "path": ".github/workflows/ci.yml", "type": "file"}}
		case strings.HasSuffix(path, "/contents/.github/workflows/ci.yml"):
			data = map[string]any{"encoding": "base64", "content": base64.StdEncoding.EncodeToString([]byte(workflow))}
		case strings.Contains(path, "/actions/runs?"):
			runs := []any{}
			if waiting && strings.Contains(path, "status=in_progress") {
				runs = append(runs, map[string]any{"id": 901})
			}
			data = map[string]any{"workflow_runs": runs}
		case strings.Contains(path, "/actions/runs/901/jobs?"):
			data = map[string]any{"jobs": []any{map[string]any{"id": 902, "status": "queued", "labels": []string{"self-hosted", "sengi", "macos"}}}}
		case strings.HasPrefix(path, "/repos/") && strings.Count(path, "/") == 3:
			data = map[string]any{"id": 4242, "full_name": strings.ToLower(strings.TrimPrefix(path, "/repos/")), "private": true, "fork": false}
		default:
			return fmt.Errorf("unexpected GitHub read: %s", path)
		}
		b, _ := json.Marshal(data)
		return json.Unmarshal(b, out)
	}
}

// guardedDoctorWorkflow is goodDoctorWorkflow on pull requests with potluck ci pr's fork-guarded runs-on: forks on
// ubuntu-latest, own runs on self-hosted and these labels.
func guardedDoctorWorkflow(labels ...string) string {
	e, err := forkguard.Expression("ubuntu-latest", append([]string{"self-hosted"}, labels...))
	if err != nil {
		panic(err)
	}
	return strings.ReplaceAll(strings.ReplaceAll(goodDoctorWorkflow, "on: push", "on: pull_request"), "[self-hosted, sengi, linux]", e)
}

const goodDoctorWorkflow = "on: push\njobs:\n  build:\n    runs-on: [self-hosted, sengi, linux]\n    timeout-minutes: 10\n    steps:\n      - run: echo ok\n"

// doctorFixture is a broker with label sengi, a linux box "small", the App
// installed on acme and linked (acme/app, acme/lib and acme/other
// connected), and conf applied.
// gh is the App's GitHub; its fields may change while the test runs.
func doctorFixture(t *testing.T, gh *doctorTransport, conf ...func(*config.Runners)) *fixture {
	t.Helper()
	f := newPoolFixture(t, func(c *config.Runners) {
		c.Label = "sengi"
		for _, fn := range conf {
			fn(c)
		}
	}, box("small", "c1m2", "linux", "1"))
	gh.next = http.DefaultTransport
	f.svc.HTTP = &http.Client{Transport: gh}
	if w := f.link(t, inst, "good"); w.Code != http.StatusOK {
		t.Fatal(w.Body)
	}
	return f
}

// doctorView is a trusted runner box binding on a host this machine lists.
func doctorView(name, os string, rs ...protocol.RunnerStatus) computeapi.RunTargetView {
	return computeapi.RunTargetView{RunTarget: computeapi.RunTarget{Name: name, Kind: computeapi.RunTargetKindBox}, HostClass: api.MachineTrusted,
		Describe: &computeapi.RunTargetDescribe{Capabilities: []string{"github-jit", "github-jit-guard"}, Available: true,
			Size: &computeapi.RunTargetSize{Name: "c4m8", OS: os, CPU: 4}, Runners: rs}}
}

func (f *fixture) doctor(t *testing.T, repo string) runners.DoctorReport {
	t.Helper()
	var out struct {
		Reports []runners.DoctorReport `json:"reports"`
	}
	if err := f.owner.Do(context.Background(), "GET", "/v0/runners/doctor?repo="+repo, nil, &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Reports) != 1 || len(out.Reports[0].Checks) != 17 {
		t.Fatalf("incomplete report: %+v", out)
	}
	return out.Reports[0]
}

// noticesOf counts the notices of check n the owners get.
func (f *fixture) noticesOf(n int) *int {
	count := new(int)
	f.dm.Deps.AddNotifier(func(_ context.Context, no core.Notice) error {
		if strings.Contains(no.Body, fmt.Sprintf("check %d:", n)) {
			*count++
		}
		return nil
	})
	return count
}

// Today's faults, each named by the right check.
func TestDoctorFaults(t *testing.T) {
	type fault struct {
		name     string
		check    int
		repo     string
		workflow string
		setup    func(*fixture, *doctorTransport)
		waiting  bool
		status   string
		text     string
	}
	for _, tt := range []fault{
		{name: "installation covers only the other repo", check: 3, repo: "acme/web", status: "fail", text: "not included"},
		{name: "forked repository's own branches", check: 6, setup: func(f *fixture, _ *doctorTransport) { f.gh.repoFork["acme/app"] = true }, status: "pass", text: "own branches"},
		{name: "not connected", check: 5, repo: "acme/web", setup: func(_ *fixture, tr *doctorTransport) { tr.covered = true }, status: "fail", text: "acme/web is not connected"},
		{name: "public repository not switched on", check: 5, setup: func(f *fixture, _ *doctorTransport) { f.gh.publicRepo["acme/app"] = true }, status: "fail", text: "public repository its owner has not switched on"},
		{name: "connected", check: 5, status: "pass", text: "connected"},
		{name: "internal repository: no approval rule", check: 7, setup: func(f *fixture, _ *doctorTransport) { f.gh.visibility["acme/app"] = "internal" }, status: "pass", text: "does not apply to this internal repo"},
		{name: "a visibility this version does not know", check: 7, setup: func(f *fixture, _ *doctorTransport) { f.gh.visibility["acme/app"] = "secret" }, status: "warn", text: "does not know"},
		{name: "unquoted colon", check: 8, workflow: strings.ReplaceAll(goodDoctorWorkflow, "echo ok", "echo hello: world"), status: "fail", text: "colon"},
		{name: "duplicate workflow keys", check: 8, workflow: goodDoctorWorkflow + "on: pull_request\n", status: "fail", text: "already defined"},
		{name: "runs-on written twice in two spellings", check: 8, workflow: strings.ReplaceAll(goodDoctorWorkflow, "    timeout-minutes", "    Runs-On: ubuntu-latest\n    timeout-minutes"), status: "fail", text: "written twice"},
		{name: "runs-on written twice leaves no passing job check", check: 10, workflow: strings.ReplaceAll(goodDoctorWorkflow, "    timeout-minutes", "    Runs-On: ubuntu-latest\n    timeout-minutes"), status: "warn", text: "could not be checked"},
		{name: "top-level key written twice in two spellings", check: 8, workflow: goodDoctorWorkflow + "ON: pull_request\n", status: "fail", text: "written twice"},
		{name: "a job written twice in two spellings", check: 8, workflow: goodDoctorWorkflow + "  Build:\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo ok\n", status: "fail", text: "written twice"},
		{name: "no OS label: linux", check: 9, workflow: strings.ReplaceAll(goodDoctorWorkflow, ", linux", ""), status: "warn", text: "places it on linux"},
		{name: "no OS label, no linux machine", check: 9, workflow: strings.ReplaceAll(goodDoctorWorkflow, ", linux", ""), status: "fail", text: "places it on linux",
			setup: func(f *fixture, _ *doctorTransport) {
				f.svc.AnswerTargets([]computeapi.RunTargetView{doctorView("mac", "macos")})
			}},
		{name: "a label the pool does not serve", check: 9, workflow: strings.ReplaceAll(goodDoctorWorkflow, "linux", "linux, gpu"), status: "fail", text: "gpu"},
		{name: "GitHub's default labels only", check: 9, workflow: strings.ReplaceAll(goodDoctorWorkflow, "sengi, linux", "linux, x64"), status: "pass", text: "passed"},
		{name: "another fleet", check: 9, workflow: strings.ReplaceAll(goodDoctorWorkflow, "sengi, linux", "gpu, linux"), status: "warn", text: "another self-hosted fleet"},
		{name: "dynamic runs-on", check: 10, workflow: strings.ReplaceAll(goodDoctorWorkflow, "[self-hosted, sengi, linux]", "${{ vars.RUNS_ON }}"), status: "warn", text: "dynamic runs-on"},
		{name: "unserved issues event", check: 10, workflow: strings.ReplaceAll(goodDoctorWorkflow, "on: push", "on: issues"), status: "fail", text: "issues"},
		{name: "reusable workflow inherits its caller's event", check: 10, workflow: strings.ReplaceAll(goodDoctorWorkflow, "on: push", "on: workflow_call"), status: "pass", text: "passed"},
		{name: "pull request without a fork guard", check: 10, workflow: strings.ReplaceAll(goodDoctorWorkflow, "on: push", "on: pull_request"), status: "fail", text: "no fork guard"},
		{name: "fork-guarded runs-on on pull requests", check: 10, workflow: guardedDoctorWorkflow("sengi", "linux"), status: "pass", text: "passed"},
		{name: "fork-guarded runs-on is judged by its own labels", check: 9, workflow: guardedDoctorWorkflow("sengi", "linux", "gpu"), status: "fail", text: "gpu"},
		{name: "fork-guarded runs-on, other fleet", check: 9, workflow: guardedDoctorWorkflow("gpu", "linux"), status: "warn", text: "another self-hosted fleet"},
		{name: "routed fork guard stays dynamic", check: 10, workflow: strings.ReplaceAll(strings.ReplaceAll(goodDoctorWorkflow, "on: push", "on: pull_request"), "[self-hosted, sengi, linux]",
			"${{ "+forkguard.Guard+" && 'ubuntu-latest' || needs.route.outputs.label || 'sengi' }}"), status: "warn", text: "dynamic runs-on"},
		{name: "On is read as on", check: 10, workflow: strings.ReplaceAll(goodDoctorWorkflow, "on: push", "On: issues"), status: "fail", text: "issues"},
		{name: "no on", check: 8, workflow: strings.ReplaceAll(goodDoctorWorkflow, "on: push\n", ""), status: "fail", text: "no `on` key"},
		{name: "timeout expression", check: 11, workflow: strings.ReplaceAll(goodDoctorWorkflow, "10", "${{ inputs.minutes }}"), status: "warn", text: "expression is unresolved"},
		{name: "matrix job's timeout is not used", check: 11, workflow: strings.ReplaceAll(goodDoctorWorkflow, "    steps:", "    strategy:\n      matrix:\n        go: [1, 2]\n    steps:"), status: "warn", text: "matrix"},
		{name: "another job's matrix", check: 11, workflow: goodDoctorWorkflow + "  test:\n    runs-on: ubuntu-latest\n    strategy:\n      matrix:\n        go: [1, 2]\n    steps:\n      - run: echo ok\n", status: "warn", text: "matrix"},
		{name: "pull request runs get job_ttl_s", check: 11, workflow: strings.ReplaceAll(goodDoctorWorkflow, "on: push", "on: [push, pull_request]"), status: "warn", text: "on pull_request"},
		{name: "timeout past job_ttl_s is honoured", check: 11, workflow: strings.ReplaceAll(goodDoctorWorkflow, "10", "600"), status: "pass", text: "passed"},
		{name: "job waits in partially running workflow", check: 16, waiting: true, status: "fail", text: "never told"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			tr := &doctorTransport{}
			f := doctorFixture(t, tr)
			if tt.setup != nil {
				tt.setup(f, tr)
			}
			f.svc.DoctorRead = doctorReader(cmpOr(tt.workflow, goodDoctorWorkflow), tt.waiting)
			r := f.doctor(t, cmpOr(tt.repo, "acme/app"))
			got := r.Checks[tt.check-1]
			if got.Number != tt.check || got.Status != tt.status || !strings.Contains(got.Message, tt.text) {
				t.Fatalf("wrong diagnosis: %+v", got)
			}
			for _, c := range r.Checks {
				if c.Status == "fail" && c.Fix == "" {
					t.Errorf("failure without a next step: %+v", c)
				}
			}
		})
	}
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// Check 5's next step is the refusal's own command, and the doctor asks
// admit with the repository's id (a delivery without one is refused).
func TestDoctorConnectionFix(t *testing.T) {
	f := doctorFixture(t, &doctorTransport{covered: true})
	f.svc.DoctorRead = doctorReader(goodDoctorWorkflow, false)
	if c := f.doctor(t, "acme/web").Checks[4]; c.Fix != "potluck runners allow acme/web" {
		t.Fatalf("%+v", c)
	}
	f.gh.publicRepo["acme/web"] = true
	if c := f.doctor(t, "acme/web").Checks[4]; c.Fix != "potluck runners allow acme/web --public" {
		t.Fatalf("%+v", c)
	}
	if _, err := f.allow("acme/web", true); err != nil {
		t.Fatal(err)
	}
	if c := f.doctor(t, "acme/web").Checks[4]; c.Status != "pass" {
		t.Fatalf("%+v", c)
	}
}

// Check 10 is the broker's event rule: allow_fork_prs serves every event
// of a private repository, never of a public one.
func TestDoctorEventsFollowBroker(t *testing.T) {
	for _, tt := range []struct {
		event  string
		public bool
		status string
	}{
		{"issues", false, "pass"},
		{"pull_request_target", false, "pass"},
		{"pull_request", false, "pass"},
		{"issues", true, "fail"},
		{"pull_request_target", true, "fail"},
		{"workflow_run", true, "fail"},
		{"pull_request", true, "fail"}, // no fork guard: an outside pull request's job waits
		{"push", true, "pass"},
	} {
		t.Run(fmt.Sprintf("%s public=%v", tt.event, tt.public), func(t *testing.T) {
			f := doctorFixture(t, &doctorTransport{covered: true}, func(c *config.Runners) { c.AllowForkPRs = true })
			repo := "acme/app"
			if tt.public {
				repo = "acme/web"
				f.gh.publicRepo[repo] = true
				if _, err := f.allow(repo, true); err != nil {
					t.Fatal(err)
				}
			}
			f.svc.DoctorRead = doctorReader(strings.ReplaceAll(goodDoctorWorkflow, "on: push", "on: "+tt.event), false)
			if c := f.doctor(t, repo).Checks[9]; c.Status != tt.status {
				t.Fatalf("%+v", c)
			}
		})
	}
}

// Check 7 reads the approval rule through the App and judges it as the
// broker's record does.
func TestDoctorApprovalRule(t *testing.T) {
	for rule, status := range map[string]string{"all_external_contributors": "pass", "first_time_contributors": "fail", "error": "warn"} {
		t.Run(rule, func(t *testing.T) {
			f := doctorFixture(t, &doctorTransport{covered: true})
			f.gh.publicRepo["acme/web"] = true
			if _, err := f.allow("acme/web", true); err != nil {
				t.Fatal(err)
			}
			f.gh.mu.Lock()
			f.gh.approval["acme/web"] = rule
			f.gh.mu.Unlock()
			f.svc.DoctorRead = doctorReader(goodDoctorWorkflow, false)
			if c := f.doctor(t, "acme/web").Checks[6]; c.Status != status {
				t.Fatalf("%+v", c)
			}
		})
	}
}

// Check 13 is what the boxes' runner refresh reported (describe.runners).
func TestDoctorRunnerRefresh(t *testing.T) {
	now := time.Now()
	for _, tt := range []struct {
		name   string
		rs     []protocol.RunnerStatus
		status string
		text   string
	}{
		{"runner 2.332.0 not replaced", []protocol.RunnerStatus{{Image: "img", ImageVersion: "2.332.0", Refresh: protocol.RunnerFailed, Why: protocol.WhyDownloadFailed, At: now}}, "fail", "download failed"},
		{"refreshed", []protocol.RunnerStatus{{Image: "img", Version: "2.333.0", ImageVersion: "2.332.0", Refresh: protocol.RunnerRefreshed, At: now}}, "pass", "2.333.0"},
		{"no report yet", nil, "warn", "unavailable"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := doctorFixture(t, &doctorTransport{covered: true})
			f.svc.AnswerTargets([]computeapi.RunTargetView{doctorView("small", "linux", tt.rs...)})
			f.svc.DoctorRead = doctorReader(goodDoctorWorkflow, false)
			if c := f.doctor(t, "acme/app").Checks[12]; c.Status != tt.status || !strings.Contains(c.Message, tt.text) {
				t.Fatalf("%+v", c)
			}
		})
	}
}

func TestDoctorUnavailableNeverPasses(t *testing.T) {
	f := newPoolFixture(t, func(c *config.Runners) { c.Label = "sengi" })
	f.svc.DoctorRead = func(context.Context, string, any) error { return fmt.Errorf("not signed in") }
	report := f.svc.Doctor(context.Background(), "acme/web")
	for _, n := range []int{5, 6, 7, 8, 9, 10, 11, 13, 14, 15, 16, 17} {
		c := report.Checks[n-1]
		if c.Status != "warn" || !strings.Contains(c.Message, "unavailable") {
			t.Errorf("check %d lacks evidence: %+v", n, c)
		}
	}
}

func TestDoctorSelfCheckNewFailureNotice(t *testing.T) {
	f := doctorFixture(t, &doctorTransport{covered: true})
	waiting, unavailable := true, false
	f.svc.DoctorRead = func(ctx context.Context, path string, out any) error {
		if unavailable && strings.Contains(path, "/actions/") {
			return fmt.Errorf("GitHub unavailable")
		}
		if !strings.Contains(path, "/acme/app/") {
			return doctorReader(goodDoctorWorkflow, false)(ctx, path, out)
		}
		return doctorReader(goodDoctorWorkflow, waiting)(ctx, path, out)
	}
	notices := f.noticesOf(16)
	f.svc.DoctorSelfCheck(context.Background())
	f.svc.DoctorSelfCheck(context.Background())
	if *notices != 1 {
		t.Fatalf("persistent failure produced %d notices, want 1", *notices)
	}
	unavailable = true
	f.svc.DoctorSelfCheck(context.Background())
	unavailable = false
	f.svc.DoctorSelfCheck(context.Background())
	if *notices != 1 {
		t.Fatalf("missing evidence reset the notification latch: %d", *notices)
	}
	waiting = false
	f.svc.DoctorSelfCheck(context.Background())
	waiting = true
	f.svc.DoctorSelfCheck(context.Background())
	if *notices != 2 {
		t.Fatalf("new failure after recovery produced %d notices, want 2", *notices)
	}
}

// A result nobody checked again for a week goes: a repository disconnected
// and connected again later is told about afresh.
func TestDoctorSelfCheckForgetsOldResults(t *testing.T) {
	f := doctorFixture(t, &doctorTransport{covered: true})
	f.svc.DoctorRead = func(ctx context.Context, path string, out any) error {
		return doctorReader(goodDoctorWorkflow, strings.Contains(path, "/acme/lib/"))(ctx, path, out)
	}
	notices := f.noticesOf(16)
	f.svc.DoctorSelfCheck(context.Background())
	if err := f.owner.Do(context.Background(), "DELETE", "/v0/runners/repos/acme/lib", nil, nil); err != nil {
		t.Fatal(err)
	}
	f.clock.advance(8 * 24 * time.Hour)
	f.svc.DoctorSelfCheck(context.Background())
	if _, err := f.allow("acme/lib", false); err != nil {
		t.Fatal(err)
	}
	f.svc.DoctorSelfCheck(context.Background())
	if *notices != 2 {
		t.Fatalf("notices %d, want 2 (one before, one after reconnecting)", *notices)
	}
}

// Check 4 is about the installation: told once, not once per repository
// of it, whether nobody linked it or its link's token is gone.
func TestDoctorInstallationNoticeOnce(t *testing.T) {
	for _, revoked := range []bool{false, true} {
		t.Run(fmt.Sprint("revoked=", revoked), func(t *testing.T) {
			tr := &doctorTransport{covered: true}
			f := doctorFixture(t, tr)
			if !revoked { // the owner's installation is one nobody linked
				tr.ownerInst = 8
			} else {
				tokens, err := f.owner.Tokens(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				for _, token := range tokens {
					if _, err := f.owner.RevokeToken(context.Background(), token.ID); err != nil {
						t.Fatal(err)
					}
				}
			}
			f.svc.DoctorRead = doctorReader(goodDoctorWorkflow, false)
			notices := f.noticesOf(4)
			f.svc.DoctorSelfCheck(context.Background())
			f.svc.DoctorSelfCheck(context.Background())
			if *notices != 1 {
				t.Fatalf("installation failure emitted %d notices", *notices)
			}
		})
	}
}

// Check 12 is about the pool: two installations' repositories on one pool
// with no machine left get one notice, not one per installation.
func TestDoctorPoolNoticeOnce(t *testing.T) {
	tr := &doctorTransport{covered: true, owners: map[string]int64{"bob": 12}}
	f := doctorFixture(t, tr)
	if err := f.svc.LinkAlso(context.Background(), inst, 12, "bob", "bob/app"); err != nil {
		t.Fatal(err)
	}
	f.svc.AnswerTargets([]computeapi.RunTargetView{})
	f.svc.DoctorRead = doctorReader(goodDoctorWorkflow, false)
	notices := f.noticesOf(12)
	f.svc.DoctorSelfCheck(context.Background())
	f.svc.DoctorSelfCheck(context.Background())
	if *notices != 1 {
		t.Fatalf("an empty pool emitted %d notices, want 1", *notices)
	}
}

// Check 16's cause is the installation's: waiting jobs missing in two of
// its repositories are told once; a recovery of both and a new failure
// are told again.
func TestDoctorWaitingNoticeOncePerInstallation(t *testing.T) {
	f := doctorFixture(t, &doctorTransport{covered: true})
	waiting := true
	f.svc.DoctorRead = func(ctx context.Context, path string, out any) error {
		return doctorReader(goodDoctorWorkflow, waiting && !strings.Contains(path, "/acme/other/"))(ctx, path, out)
	}
	notices := f.noticesOf(16)
	f.svc.DoctorSelfCheck(context.Background())
	f.svc.DoctorSelfCheck(context.Background())
	if *notices != 1 {
		t.Fatalf("one installation's missing deliveries emitted %d notices, want 1", *notices)
	}
	waiting = false
	f.svc.DoctorSelfCheck(context.Background())
	waiting = true
	f.svc.DoctorSelfCheck(context.Background())
	if *notices != 2 {
		t.Fatalf("new failure after recovery: %d notices, want 2", *notices)
	}
}

// The self-check moves on past what each run checked: a connected list
// longer than one run's gh budget is covered in turn, wherever it ends.
func TestDoctorSelfCheckRotation(t *testing.T) {
	f := doctorFixture(t, &doctorTransport{covered: true})
	all := []string{"acme/app", "acme/lib", "acme/other"}
	for i := range 67 {
		repo := fmt.Sprintf("acme/r%02d", i)
		if _, err := f.allow(repo, false); err != nil {
			t.Fatal(err)
		}
		all = append(all, repo)
	}
	var runs [][]string
	f.svc.DoctorRead = func(ctx context.Context, path string, out any) error {
		if strings.Contains(path, "status=queued") {
			repo := strings.Split(strings.TrimPrefix(path, "/repos/"), "/actions/")[0]
			runs[len(runs)-1] = append(runs[len(runs)-1], repo)
		}
		return doctorReader(goodDoctorWorkflow, false)(ctx, path, out)
	}
	seen := map[string]bool{}
	for range 3 {
		runs = append(runs, nil)
		f.svc.DoctorSelfCheck(context.Background())
		for _, r := range runs[len(runs)-1] {
			seen[r] = true
		}
	}
	for _, r := range all {
		if !seen[r] {
			t.Errorf("%s never checked in 3 runs: %v", r, runs)
		}
	}
	if len(runs[0]) == 0 || len(runs[0]) >= len(all) || slices.Contains(runs[0], runs[1][0]) {
		t.Fatalf("second run did not start after the first: %v", runs)
	}
}

func TestDoctorConnectionFailureKeepsApp(t *testing.T) {
	f := newPoolFixture(t, func(c *config.Runners) { c.Label = "sengi" })
	f.svc.HTTP = &http.Client{Transport: doctorOffline{}}
	f.svc.DoctorRead = doctorReader(goodDoctorWorkflow, false)
	c := f.svc.Doctor(context.Background(), "acme/web").Checks[0]
	if c.Status != "warn" || strings.Contains(c.Fix, "setup") {
		t.Fatalf("%+v", c)
	}
}

type doctorOffline struct{}

func (doctorOffline) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, fmt.Errorf("network unavailable")
}

func TestDoctorCountsPendingWebhook(t *testing.T) {
	f := doctorFixture(t, &doctorTransport{covered: true})
	f.svc.DoctorRead = doctorReader(goodDoctorWorkflow, true)
	f.svc.Close() // the signed delivery stays in the inbox
	if code := f.deliver(t, "workflow_job", jobEvent("queued", 902, 901, "acme/app", true, "self-hosted", "sengi", "macos")); code != http.StatusAccepted {
		t.Fatalf("hook: %d", code)
	}
	if c := f.svc.Doctor(context.Background(), "acme/app").Checks[15]; c.Status != "pass" {
		t.Fatalf("pending delivery was lost: %+v", c)
	}
}

func TestDoctorMappedSizeUsesPlacement(t *testing.T) {
	f := doctorFixture(t, &doctorTransport{covered: true}, func(c *config.Runners) { c.Pool.Sizes = map[string]string{"c8m32": "small"} })
	f.svc.DoctorRead = doctorReader(strings.ReplaceAll(goodDoctorWorkflow, ", sengi,", ", sengi:c8m32,"), false)
	r := f.doctor(t, "acme/app")
	if r.Checks[8].Status != "pass" || r.Checks[11].Status != "pass" {
		t.Fatalf("%+v", r)
	}
}

func TestDoctorCoolingMappedBinding(t *testing.T) {
	f := newPoolFixture(t, func(c *config.Runners) { c.Label = "sengi"; c.Pool.Sizes = map[string]string{"c8m32": "small"} }, box("small", "c1m2", "linux", "1", "refuse", "1"))
	f.svc.RefuseCool = time.Hour
	f.svc.HTTP = &http.Client{Transport: &doctorTransport{next: http.DefaultTransport, covered: true}}
	if w := f.link(t, inst, "good"); w.Code != http.StatusOK {
		t.Fatal(w.Body)
	}
	f.deliver(t, "workflow_job", jobEvent("queued", 8001, 130, "acme/app", true, "sengi:c8m32", "linux"))
	eventually(t, "mapped binding cooling", func() bool { return strings.Contains(f.jobs(t)[8001].Why, "refused or failed a job moments ago") })
	j := f.jobs(t)[8001]
	if j.State != "waiting" || !strings.Contains(j.Why, "no box: ") || !strings.Contains(j.Why, "no capacity") {
		t.Fatalf("cooldown lost the driver's refusal: %+v", j)
	}
	_, retry, ok := strings.Cut(j.Why, "retry after ")
	if !ok {
		t.Fatalf("cooldown has no retry time: %+v", j)
	}
	until, err := time.Parse(time.RFC3339, strings.TrimSuffix(retry, ")"))
	if err != nil || until.Before(f.clock.now()) || until.After(f.clock.now().Add(f.svc.RefuseCool)) {
		t.Fatalf("invalid cooldown retry time %q: %v", retry, err)
	}
	// Further placement attempts keep the reason and deadline without stacking notes.
	time.Sleep(3 * f.svc.WaitMax)
	if later := f.jobs(t)[8001]; later.Why != j.Why || len(f.minted()) != 1 {
		t.Fatalf("cooldown changed or minted another runner: before=%+v after=%+v", j, later)
	}
	f.svc.DoctorRead = doctorReader(strings.ReplaceAll(goodDoctorWorkflow, ", sengi,", ", sengi:c8m32,"), false)
	if c := f.doctor(t, "acme/app").Checks[11]; c.Status != "warn" || !strings.Contains(c.Message, "refused or failed a job moments ago") {
		t.Fatalf("%+v", c)
	}
}

func TestDoctorWaitingDefaultLabelsIgnored(t *testing.T) {
	for _, labels := range []string{`["self-hosted","macos"]`, `["self-hosted","linux","x64"]`} {
		f := doctorFixture(t, &doctorTransport{covered: true})
		waiting := true
		f.svc.DoctorRead = func(ctx context.Context, path string, out any) error {
			if strings.Contains(path, "/actions/runs/901/jobs?") {
				return json.Unmarshal([]byte(`{"jobs":[{"id":902,"status":"queued","labels":`+labels+`}]}`), out)
			}
			return doctorReader(strings.ReplaceAll(goodDoctorWorkflow, "sengi, linux", "linux, x64"), waiting)(ctx, path, out)
		}
		r := f.doctor(t, "acme/app")
		if r.Checks[8].Status != "pass" || r.Checks[15].Status != "pass" {
			t.Fatalf("%s: %+v", labels, r)
		}
		notices := f.noticesOf(16)
		f.svc.DoctorSelfCheck(context.Background())
		waiting = false
		f.svc.DoctorSelfCheck(context.Background())
		waiting = true
		f.svc.DoctorSelfCheck(context.Background())
		if *notices != 0 {
			t.Fatalf("%s: default fleet raised %d notices", labels, *notices)
		}
	}
}

func TestDoctorIgnoresWaitingOtherFleet(t *testing.T) {
	f := doctorFixture(t, &doctorTransport{covered: true})
	f.svc.DoctorRead = func(ctx context.Context, path string, out any) error {
		if strings.Contains(path, "/actions/runs/901/jobs?") {
			return json.Unmarshal([]byte(`{"jobs":[{"id":902,"status":"queued","labels":["self-hosted","gpu","linux"]}]}`), out)
		}
		return doctorReader(strings.ReplaceAll(goodDoctorWorkflow, "sengi, linux", "gpu, linux"), true)(ctx, path, out)
	}
	report := f.doctor(t, "acme/app")
	if report.Checks[8].Status != "warn" || report.Checks[15].Status != "pass" {
		t.Fatalf("%+v", report)
	}
	notices := f.noticesOf(16)
	f.svc.DoctorSelfCheck(context.Background())
	if *notices != 0 {
		t.Fatalf("another fleet generated %d notices", *notices)
	}
}

// A size or OS no host offers fails check 12; one a host offers when free
// does not.
func TestDoctorPermanentPlacementFailure(t *testing.T) {
	for labels, status := range map[string]string{"sengi:c64m128, linux": "fail", "sengi, windows": "fail", "sengi:c1m2, linux": "pass"} {
		t.Run(labels, func(t *testing.T) {
			f := doctorFixture(t, &doctorTransport{covered: true})
			f.svc.DoctorRead = doctorReader(strings.ReplaceAll(goodDoctorWorkflow, "sengi, linux", labels), false)
			if r := f.doctor(t, "acme/app"); r.Checks[11].Status != status || (status == "fail") != r.Failed() {
				t.Fatalf("%+v", r)
			}
		})
	}
}

// A sleeping Mac is unavailable, not missing for good: its describe
// failed, or its advert lapsed and it is not listed, though it served a
// macos job before.
func TestDoctorSleepingMacUnavailable(t *testing.T) {
	for _, tt := range []struct {
		name   string
		views  []computeapi.RunTargetView
		served bool
		status string
	}{
		{"describe failed", []computeapi.RunTargetView{{RunTarget: computeapi.RunTarget{Name: "mac", Kind: computeapi.RunTargetKindBox}, HostClass: api.MachineTrusted, Error: "asleep"}}, false, "warn"},
		{"advert lapsed", []computeapi.RunTargetView{doctorView("small", "linux")}, true, "warn"},
		{"no Mac ever", []computeapi.RunTargetView{doctorView("small", "linux")}, false, "fail"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := doctorFixture(t, &doctorTransport{covered: true})
			if tt.served {
				if err := f.svc.RecordServed(context.Background(), 9901, inst, "acme/app", "mac", "self-hosted", "sengi", "macos"); err != nil {
					t.Fatal(err)
				}
			}
			f.svc.AnswerTargets(tt.views)
			f.svc.DoctorRead = doctorReader(strings.ReplaceAll(goodDoctorWorkflow, "linux", "macos"), false)
			if c := f.doctor(t, "acme/app").Checks[11]; c.Status != tt.status {
				t.Fatalf("%+v", c)
			}
		})
	}
}

// A binding whose budget for the period is spent waits, as a busy one does:
// check 12 warns, never fails (the budget clears with its period).
func TestDoctorSpentBudgetWaits(t *testing.T) {
	spent := 500.0
	v := doctorView("small", "linux")
	v.Budget, v.BudgetSpent = &computeapi.RunTargetBudget{Unit: "vm_s", Limit: 300, Period: "day"}, &spent
	f := doctorFixture(t, &doctorTransport{covered: true})
	f.svc.AnswerTargets([]computeapi.RunTargetView{v})
	f.svc.DoctorRead = doctorReader(goodDoctorWorkflow, false)
	if c := f.doctor(t, "acme/app").Checks[11]; c.Status != "warn" || !strings.Contains(c.Message, "(budget)") {
		t.Fatalf("%+v", c)
	}
}

func TestDoctorGitHubCallBudget(t *testing.T) {
	f := doctorFixture(t, &doctorTransport{covered: true})
	calls := 0
	f.svc.DoctorRead = func(ctx context.Context, path string, out any) error {
		calls++
		if strings.Contains(path, "/actions/runs?") {
			runs := make([]map[string]any, 100)
			for i := range runs {
				runs[i] = map[string]any{"id": i + 1}
			}
			b, _ := json.Marshal(map[string]any{"workflow_runs": runs})
			return json.Unmarshal(b, out)
		}
		if strings.Contains(path, "/jobs?") {
			return json.Unmarshal([]byte(`{"jobs":[]}`), out)
		}
		return doctorReader(goodDoctorWorkflow, false)(ctx, path, out)
	}
	if r := f.svc.Doctor(context.Background(), "acme/app"); calls != 60 || r.Checks[15].Status != "warn" {
		t.Fatalf("calls=%d check16=%+v", calls, r.Checks[15])
	}
	calls = 0
	f.svc.DoctorSelfCheck(context.Background())
	if calls != 60 {
		t.Fatalf("self-check calls=%d", calls)
	}
	calls = 0
	var out struct {
		Reports  []runners.DoctorReport
		Warnings []string
		Skipped  []string
	}
	if err := f.owner.Do(context.Background(), "GET", "/v0/runners/doctor", nil, &out); err != nil {
		t.Fatal(err)
	}
	if calls != 60 || len(out.Skipped) == 0 {
		t.Fatalf("no-arg doctor calls=%d skipped=%v", calls, out.Skipped)
	}
}

// Without a repository the doctor checks the connected ones, then the
// others whose jobs came here (refused: check 5 says why).
func TestDoctorInventory(t *testing.T) {
	f := doctorFixture(t, &doctorTransport{covered: true})
	if err := f.svc.RecordJob(context.Background(), 9901, inst, "acme/zzz", "old", 1); err != nil {
		t.Fatal(err)
	}
	f.svc.DoctorRead = doctorReader(goodDoctorWorkflow, false)
	var out struct{ Reports []runners.DoctorReport }
	if err := f.owner.Do(context.Background(), "GET", "/v0/runners/doctor", nil, &out); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range out.Reports {
		got = append(got, r.Repo)
	}
	if !slices.Equal(got, []string{"acme/app", "acme/lib", "acme/other", "acme/zzz"}) {
		t.Fatalf("%v", got)
	}
	if c := out.Reports[3].Checks[4]; c.Status != "fail" || !strings.Contains(c.Message, "not connected") {
		t.Fatalf("%+v", c)
	}
}

func TestDoctorNothingConnected(t *testing.T) {
	f := newPoolFixture(t, func(c *config.Runners) { c.Label = "sengi" })
	calls, notices := 0, 0
	f.svc.DoctorRead = func(ctx context.Context, path string, out any) error {
		calls++
		return doctorReader(goodDoctorWorkflow, true)(ctx, path, out)
	}
	f.dm.Deps.AddNotifier(func(context.Context, core.Notice) error { notices++; return nil })
	var out struct {
		Reports  []runners.DoctorReport
		Warnings []string
	}
	if err := f.owner.Do(context.Background(), "GET", "/v0/runners/doctor", nil, &out); err != nil {
		t.Fatal(err)
	}
	f.svc.DoctorSelfCheck(context.Background())
	if len(out.Reports) != 0 || len(out.Warnings) == 0 || calls != 0 || notices != 0 {
		t.Fatalf("reports=%+v calls=%d notices=%d", out, calls, notices)
	}
}

// A delivery that arrives while the doctor reads GitHub for a later
// repository counts as told.
func TestDoctorInboxArrivingDuringLaterRepo(t *testing.T) {
	f := doctorFixture(t, &doctorTransport{covered: true})
	f.svc.Close()
	if code := f.deliver(t, "workflow_job", jobEvent("queued", 902, 901, "acme/app", true, "sengi", "linux")); code != http.StatusAccepted {
		t.Fatal(code)
	}
	delivered := map[string]bool{"acme/app": true}
	f.svc.DoctorRead = func(ctx context.Context, path string, out any) error {
		repo := strings.Split(strings.TrimPrefix(path, "/repos/"), "/actions/")[0]
		if strings.Contains(path, "/actions/runs/901/jobs?") && !delivered[repo] {
			delivered[repo] = true
			if code := f.deliver(t, "workflow_job", jobEvent("queued", 902, 901, repo, true, "sengi", "linux")); code != http.StatusAccepted {
				t.Fatal(code)
			}
		}
		return doctorReader(goodDoctorWorkflow, true)(ctx, path, out)
	}
	var result struct{ Reports []runners.DoctorReport }
	if err := f.owner.Do(context.Background(), "GET", "/v0/runners/doctor", nil, &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Reports) != 3 {
		t.Fatalf("%+v", result)
	}
	for _, r := range result.Reports {
		if r.Checks[15].Status != "pass" {
			t.Fatalf("%s: %+v", r.Repo, r.Checks[15])
		}
	}
}

// The pool's only machine, a Mac, asleep (its advert lapsed, it served a
// macos job before): check 12 for the pool is a warning, never a failure,
// and the self-check tells nobody.
func TestDoctorPoolAsleepMac(t *testing.T) {
	f := doctorFixture(t, &doctorTransport{covered: true})
	if err := f.svc.RecordServed(context.Background(), 9901, inst, "acme/app", "mac", "self-hosted", "sengi", "macos"); err != nil {
		t.Fatal(err)
	}
	f.svc.AnswerTargets([]computeapi.RunTargetView{})
	f.svc.DoctorRead = doctorReader(strings.ReplaceAll(goodDoctorWorkflow, "linux", "macos"), false)
	r := f.doctor(t, "acme/app")
	if c := r.Checks[11]; c.Status != "warn" || !strings.Contains(c.Message, "mac served such a job before") || r.Failed() {
		t.Fatalf("%+v", r)
	}
	notices := f.noticesOf(12)
	f.svc.DoctorSelfCheck(context.Background())
	if *notices != 0 {
		t.Fatalf("an asleep Mac raised %d notices", *notices)
	}
}

// Checks 11 and 12 use the broker's runtime limit (workflowLimit): where it
// gives job_ttl_s instead of timeout-minutes (a matrix in the file, a pull
// request run), placement asks for job_ttl_s.
func TestDoctorLimitIsTheBrokers(t *testing.T) {
	long := strings.ReplaceAll(goodDoctorWorkflow, "timeout-minutes: 10", "timeout-minutes: 600")
	for _, tt := range []struct {
		name, workflow, check12 string
	}{
		{"literal timeout past the host's cap", long, "fail"},
		{"matrix", strings.ReplaceAll(long, "    steps:", "    strategy:\n      matrix:\n        go: [1, 2]\n    steps:"), "pass"},
		{"pull request", strings.ReplaceAll(long, "on: push", "on: pull_request"), "pass"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := doctorFixture(t, &doctorTransport{covered: true}, func(c *config.Runners) { c.JobTTLS = 3600 })
			v := doctorView("small", "linux")
			v.Describe.TTLMaxS = 2 * 3600
			f.svc.AnswerTargets([]computeapi.RunTargetView{v})
			f.svc.DoctorRead = doctorReader(tt.workflow, false)
			r := f.doctor(t, "acme/app")
			if r.Checks[11].Status != tt.check12 {
				t.Fatalf("check 12: %+v", r.Checks[11])
			}
			if tt.check12 == "pass" && (r.Checks[10].Status != "warn" || !strings.Contains(r.Checks[10].Message, "3600s")) {
				t.Fatalf("check 11: %+v", r.Checks[10])
			}
		})
	}
}

// Check 5 asks admission with GitHub's name of the repository, as its
// deliveries carry it, not as it was typed: renamed since it was
// connected, its jobs are refused.
func TestDoctorRepositoryNameAsGitHubSpellsIt(t *testing.T) {
	tr := &doctorTransport{covered: true}
	f := doctorFixture(t, tr)
	tr.renamed = map[string]string{"acme/app": "acme/renamed"}
	f.svc.DoctorRead = doctorReader(goodDoctorWorkflow, false)
	if c := f.doctor(t, "acme/app").Checks[4]; c.Status != "fail" || !strings.Contains(c.Message, "a name it no longer has") {
		t.Fatalf("%+v", c)
	}
}

// A job queued moments ago may have its delivery on the way: check 16
// does not count it yet.
func TestDoctorWaitingGrace(t *testing.T) {
	for ago, status := range map[time.Duration]string{10 * time.Second: "pass", 10 * time.Minute: "fail"} {
		t.Run(ago.String(), func(t *testing.T) {
			f := doctorFixture(t, &doctorTransport{covered: true})
			created := f.clock.now().Add(-ago).UTC().Format(time.RFC3339)
			f.svc.DoctorRead = func(ctx context.Context, path string, out any) error {
				if strings.Contains(path, "/actions/runs/901/jobs?") {
					return json.Unmarshal([]byte(`{"jobs":[{"id":902,"status":"queued","labels":["self-hosted","sengi","linux"],"created_at":"`+created+`"}]}`), out)
				}
				return doctorReader(goodDoctorWorkflow, true)(ctx, path, out)
			}
			if c := f.doctor(t, "acme/app").Checks[15]; c.Status != status {
				t.Fatalf("%+v", c)
			}
		})
	}
}

// One doctor run reads the pool's machines once, however many
// repositories it checks.
func TestDoctorReadsPoolOnce(t *testing.T) {
	f := doctorFixture(t, &doctorTransport{covered: true})
	f.svc.DoctorRead = doctorReader(goodDoctorWorkflow, false)
	lists := f.svc.CountLists()
	var out runners.DoctorResult
	if err := f.owner.Do(context.Background(), "GET", "/v0/runners/doctor", nil, &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Reports) != 3 || lists.Load() != 1 {
		t.Fatalf("%d reports, %d listings", len(out.Reports), lists.Load())
	}
	lists.Store(0)
	f.svc.DoctorSelfCheck(context.Background())
	if lists.Load() != 1 {
		t.Fatalf("self-check listings: %d", lists.Load())
	}
}
