package runners_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/computeclient"
	"github.com/sengirunners/potluck/issuers"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/protocol"
)

// Job grants end to end (runners/jobsecrets.go): a real daemon, the broker,
// the github-token issuer on the broker's App, the reference box driver, and
// the fake GitHub answering what the door asks about a run.

// doorRun is a workflow run as GitHub describes it to the door.
type doorRun struct {
	event, branch, sha string
	path               string // the workflow file ("": .github/workflows/x.yml)
	headID             int64  // head_repository.id (0: the repository's own)
	refs               string // referenced_workflows JSON ("": none)
	attempt            int    // run_attempt (0: 1)
}

// doorGitHub is the fake GitHub's part for job grants: runs, the default
// branch, the compare of a commit with it, its tags, and the issuer's
// mints. Its fields are guarded by the fake's mu.
type doorGitHub struct {
	runs     map[int64]doorRun
	compare  map[string]string // head sha -> compare status (default identical)
	tags     map[string]bool   // tag names that exist (default none)
	branches map[string]bool   // branches besides main that exist (default none): a compare with another is 404
	fail     map[string]bool   // "run", "compare", "tag": GitHub answers 500
	refuse   string            // a permission GitHub refuses to mint (422)
	gate     chan struct{}     // non-nil: the next mint waits for it (the fake's lock let go meanwhile)
	gateFrom int               // the gate waits only once this many tokens are minted
	gated    chan struct{}     // closed once that mint waits
	issued   []fakeIssued      // the issuer's tokens, in order
	compares int
	tagCalls int
	// The default branch's rulesets: noRule, no pull_request rule on main;
	// bypass, ruleset 5's current_user_can_bypass (default never).
	noRule     bool
	bypass     string
	noApproval bool // the pull_request rule asks for no approval
	stale      bool // no approval of the last push by another is required (only stale reviews dismissed)
	// files are the repository's files at any commit, path -> body
	// (default: x.yml, every uses: pinned).
	files map[string]string
}

// pinnedWorkflow is x.yml with every uses: pinned, a local action included.
const pinnedWorkflow = `on: push
jobs:
  build:
    runs-on: [self-hosted, sengi]
    steps:
      - uses: actions/checkout@0123456789abcdef0123456789abcdef01234567
      - uses: ./.github/actions/setup
      - uses: docker://alpine@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
      - run: make
`

type fakeIssued struct{ token, repos, perms string }

var (
	mainSHA   = strings.Repeat("a", 40)
	doorRunRe = regexp.MustCompile(`^/repos/acme/app/actions/runs/(\d+)$`)
)

type doorFixture struct {
	*fixture
	door *doorGitHub
}

// newDoorFixture is a linked broker whose fake GitHub answers the door. With
// issuer, config.json names one github-app issuer on the broker's App (its
// ceiling acme/app contents and issues write), the issuer module is started,
// and grants (name -> permissions) are added as job grants of
// .github/workflows/x.yml on refs/heads/main for push.
func newDoorFixture(t *testing.T, issuer bool, grants ...[2]string) *doorFixture {
	return newDoorFixtureOn(t, newFixture(t), issuer, grants...)
}

// newDoorFixtureOn is newDoorFixture on fixture f.
func newDoorFixtureOn(t *testing.T, f *fixture, issuer bool, grants ...[2]string) *doorFixture {
	d := &doorGitHub{runs: map[int64]doorRun{}, compare: map[string]string{}, tags: map[string]bool{}, branches: map[string]bool{}, fail: map[string]bool{},
		files: map[string]string{".github/workflows/x.yml": pinnedWorkflow, ".github/actions/setup/action.yml": "runs:\n  using: composite\n  steps:\n    - run: true\n      shell: bash\n"}}
	df := &doorFixture{f, d}
	f.gh.mu.Lock()
	f.gh.extra = df.answer
	f.gh.mu.Unlock()
	if issuer {
		f.restart(t, func(c *config.Config) {
			c.File.Secrets = config.Secrets{Issuers: map[string]config.SecretsIssuer{"door": {Kind: issuers.KindGitHubApp, Use: issuers.UseRunners,
				Ceiling: []config.SecretsCeiling{{Repo: "acme/app", Permissions: map[string]string{"contents": "write", "issues": "write"}}}}}}
		})
		df.startIssuer(t)
	}
	if w := f.link(t, inst, "good"); w.Code != http.StatusOK {
		t.Fatalf("link: %d %s", w.Code, w.Body)
	}
	for _, g := range grants {
		if _, err := computeclient.AddSecretGrant(context.Background(), f.owner, computeapi.AddSecretGrantRequest{
			To:   computeapi.GrantTo{Job: &computeapi.GrantJob{Repo: "acme/app", Workflow: ".github/workflows/x.yml", Ref: "refs/heads/main", Events: []string{"push"}}},
			Kind: computeapi.SecretKindGitHubToken, Name: g[0], Scopes: []string{computeapi.SecretScopeBox},
			Ceiling: json.RawMessage(`{"repos":["acme/app"],"permissions":{"` + g[1] + `":"write"}}`)}); err != nil {
			t.Fatalf("grant %s: %v", g[0], err)
		}
	}
	return df
}

// ready waits until n boxes are ready.
func (df *doorFixture) ready(t *testing.T, n int) {
	t.Helper()
	eventuallyBox(t, "the box ready", func() bool {
		hs, _ := computeclient.RunTargetHandles(context.Background(), df.owner, "small", false)
		k := 0
		for _, h := range hs {
			if h.State == "ready" {
				k++
			}
		}
		return k >= n
	})
}

// startIssuer starts the daemon's issuer module (the fixture's daemon does
// not start modules itself).
func (df *doorFixture) startIssuer(t *testing.T) {
	t.Helper()
	started := false
	for _, m := range df.dm.Modules() {
		if im, ok := m.(*issuers.Module); ok {
			if err := im.Start(context.Background()); err != nil {
				t.Fatal(err)
			}
			started = true
		}
	}
	if !started || issuers.From(df.dm.Deps) == nil {
		t.Fatal("no issuer module running")
	}
}

// answer is the fake's extra hook (under its mu, after its token checks).
func (df *doorFixture) answer(w http.ResponseWriter, r *http.Request) bool {
	d, g, p := df.door, df.gh, r.URL.Path
	switch {
	case r.Method == "GET" && doorRunRe.MatchString(p):
		var id int64
		fmt.Sscan(doorRunRe.FindStringSubmatch(p)[1], &id)
		run, ok := d.runs[id]
		if !ok {
			return false
		}
		if d.fail["run"] {
			w.WriteHeader(500)
			return true
		}
		head := run.headID
		if head == 0 {
			head = repoID("acme/app")
		}
		path := run.path
		if path == "" {
			path = ".github/workflows/x.yml"
		}
		refs := ""
		if run.refs != "" {
			refs = `,"referenced_workflows":` + run.refs
		}
		attempt := run.attempt
		if attempt == 0 {
			attempt = 1
		}
		fmt.Fprintf(w, `{"id":%d,"event":%q,"status":"in_progress","run_attempt":%d,"path":%q,"head_sha":%q,"head_branch":%q,`+
			`"head_repository":{"id":%d,"full_name":"acme/app","fork":false},"repository":{"id":%d,"full_name":"acme/app","private":true}%s}`,
			id, run.event, attempt, path, run.sha, run.branch, head, repoID("acme/app"), refs)
		return true
	case r.Method == "GET" && p == "/repos/acme/app":
		fmt.Fprintf(w, `{"id":%d,"full_name":"acme/app","private":true,"visibility":"private","fork":false,"default_branch":"main"}`, repoID("acme/app"))
		return true
	case r.Method == "GET" && strings.HasPrefix(p, "/repos/acme/app/compare/"):
		d.compares++
		tok := g.tokens[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")]
		if tok.repo != "acme/app" || tok.perms != "map[contents:read metadata:read]" {
			g.misused = append(g.misused, fmt.Sprintf("compare with a token for %q %s", tok.repo, tok.perms))
			w.WriteHeader(403)
			return true
		}
		if d.fail["compare"] {
			w.WriteHeader(500)
			return true
		}
		branch, sha, _ := strings.Cut(strings.TrimPrefix(p, "/repos/acme/app/compare/"), "...")
		if branch != "main" && !d.branches[branch] {
			w.WriteHeader(404)
			fmt.Fprint(w, `{"message":"Not Found"}`)
			return true
		}
		st := d.compare[sha]
		if st == "" {
			st = "identical"
		}
		fmt.Fprintf(w, `{"status":%q}`, st)
		return true
	case r.Method == "GET" && strings.HasPrefix(p, "/repos/acme/app/git/ref/tags/"):
		d.tagCalls++
		tok := g.tokens[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")]
		if tok.repo != "acme/app" || tok.perms != "map[contents:read metadata:read]" {
			g.misused = append(g.misused, fmt.Sprintf("tag read with a token for %q %s", tok.repo, tok.perms))
			w.WriteHeader(403)
			return true
		}
		name := strings.TrimPrefix(p, "/repos/acme/app/git/ref/tags/")
		switch {
		case d.fail["tag"]:
			w.WriteHeader(500)
		case d.tags[name]:
			fmt.Fprintf(w, `{"ref":"refs/tags/%s","object":{"sha":%q,"type":"commit"}}`, name, mainSHA)
		default:
			w.WriteHeader(404)
			fmt.Fprint(w, `{"message":"Not Found"}`)
		}
		return true
	case r.Method == "GET" && strings.HasPrefix(p, "/repos/acme/app/contents/"):
		body, ok := d.files[strings.TrimPrefix(p, "/repos/acme/app/contents/")]
		if !ok || r.URL.Query().Get("ref") == "" {
			w.WriteHeader(404) // the workflow's time limit falls back
			fmt.Fprint(w, `{"message":"Not Found"}`)
			return true
		}
		fmt.Fprintf(w, `{"encoding":"base64","content":%q}`, base64.StdEncoding.EncodeToString([]byte(body)))
		return true
	case r.Method == "GET" && (p == "/repos/acme/app/rules/branches/main" || p == "/repos/acme/app/rulesets/5"):
		tok := g.tokens[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")]
		if tok.repo != "acme/app" || tok.perms != "map[metadata:read]" {
			g.misused = append(g.misused, fmt.Sprintf("rules read with a token for %q %s", tok.repo, tok.perms))
			w.WriteHeader(403)
			return true
		}
		switch {
		case p == "/repos/acme/app/rulesets/5":
			by := d.bypass
			if by == "" {
				by = "never"
			}
			fmt.Fprintf(w, `{"id":5,"enforcement":"active","current_user_can_bypass":%q}`, by)
		case d.noRule:
			fmt.Fprint(w, `[{"type":"deletion","ruleset_id":4}]`)
		default:
			n := 1
			if d.noApproval {
				n = 0
			}
			fmt.Fprintf(w, `[{"type":"deletion","ruleset_id":4},{"type":"pull_request","ruleset_id":5,"parameters":{"required_approving_review_count":%d,`+
				`"require_last_push_approval":%v,"dismiss_stale_reviews_on_push":true}}]`, n, !d.stale)
		}
		return true
	case r.Method == "POST" && p == fmt.Sprintf("/app/installations/%d/access_tokens", inst):
		body, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(body))
		var b struct {
			Repositories []string          `json:"repositories"`
			Permissions  map[string]string `json:"permissions"`
		}
		_ = json.Unmarshal(body, &b)
		if b.Permissions["contents"] != "write" && b.Permissions["issues"] != "write" {
			return false // the broker's own tokens
		}
		if d.refuse != "" && b.Permissions[d.refuse] != "" {
			w.WriteHeader(422)
			fmt.Fprint(w, `{"message":"The permissions requested are not granted to this installation."}`)
			return true
		}
		if ch := d.gate; ch != nil && len(d.issued) >= d.gateFrom {
			d.gate = nil
			close(d.gated)
			g.mu.Unlock()
			<-ch
			g.mu.Lock()
		}
		tok := fmt.Sprintf("ghs_door_%d", len(d.issued)+1)
		perms, _ := json.Marshal(b.Permissions)
		d.issued = append(d.issued, fakeIssued{tok, strings.Join(b.Repositories, ","), fmt.Sprint(b.Permissions)})
		g.tokens[tok] = fakeToken{"acme/" + strings.Join(b.Repositories, ","), fmt.Sprint(b.Permissions)}
		w.WriteHeader(201)
		var repos []string
		for _, n := range b.Repositories {
			repos = append(repos, fmt.Sprintf(`{"full_name":"acme/%s"}`, n))
		}
		fmt.Fprintf(w, `{"token":%q,"expires_at":%q,"permissions":%s,"repository_selection":"selected","repositories":[%s]}`,
			tok, time.Now().Add(time.Hour).UTC().Format(time.RFC3339), perms, strings.Join(repos, ","))
		return true
	}
	return false
}

func (df *doorFixture) set(fn func(d *doorGitHub)) {
	df.gh.mu.Lock()
	defer df.gh.mu.Unlock()
	fn(df.door)
}

// issued is the issuer's tokens and those of them GitHub revoked.
func (df *doorFixture) issued() (all []fakeIssued, revoked []string) {
	df.gh.mu.Lock()
	defer df.gh.mu.Unlock()
	all = slices.Clone(df.door.issued)
	for _, i := range all {
		if slices.Contains(df.gh.revoked, i.token) {
			revoked = append(revoked, i.token)
		}
	}
	return all, revoked
}

// driverCall is one request the box driver was sent.
type driverCall struct {
	Verb string          `json:"verb"`
	ID   string          `json:"id"`
	Args json.RawMessage `json:"args"`
}

func (df *doorFixture) calls() []driverCall {
	b, _ := os.ReadFile(df.seen)
	dec := json.NewDecoder(bytes.NewReader(b))
	var out []driverCall
	for {
		var c driverCall
		if dec.Decode(&c) != nil {
			return out
		}
		out = append(out, c)
	}
}

// guards are the jit_guards of every acquire, in order.
func (df *doorFixture) guards() []*computeapi.JITGuard {
	var out []*computeapi.JITGuard
	for _, c := range df.calls() {
		if c.Verb == "acquire" {
			var a struct {
				Guard *computeapi.JITGuard `json:"jit_guard"`
			}
			_ = json.Unmarshal(c.Args, &a)
			out = append(out, a.Guard)
		}
	}
	return out
}

// boxSecrets is every name and value written into a box, in order.
func (df *doorFixture) boxSecrets() [][2]string {
	var out [][2]string
	for _, c := range df.calls() {
		if c.Verb == "secrets" {
			var a protocol.BoxSecretsArgs
			_ = json.Unmarshal(c.Args, &a)
			for n, v := range a.Secrets {
				out = append(out, [2]string{n, v})
			}
		}
	}
	return out
}

func (df *doorFixture) marked() bool {
	for _, s := range df.boxSecrets() {
		if s[0] == protocol.SecretNameReady {
			return true
		}
	}
	return false
}

// served queues job of run and waits for its box to be acquired (the n-th
// acquire in all).
func (df *doorFixture) served(t *testing.T, job, run int64, n int) *computeapi.JITGuard {
	t.Helper()
	df.deliver(t, "workflow_job", jobEvent("queued", job, run, "acme/app", true, "self-hosted", "sengi"))
	eventuallyBox(t, fmt.Sprintf("job %d's box", job), func() bool { return len(df.guards()) >= n && df.svc.Undone(context.Background()) == 0 })
	g := df.guards()[n-1]
	if g == nil {
		t.Fatalf("job %d's box has no jit_guard", job)
	}
	return g
}

// startOn is the in_progress of job of run on runner, and GitHub answers
// asking for the job so from now on.
func (df *doorFixture) startOn(job, run int64, repo, runner string) []byte {
	df.gh.mu.Lock()
	df.gh.runnerOf[job], df.gh.jobRun[job] = runner, run
	delete(df.gh.status, job) // in_progress
	if df.gh.jobStarts[job] == "" {
		df.gh.jobStarts[job] = df.clock.now().UTC().Format(time.RFC3339)
	}
	df.gh.mu.Unlock()
	return startEvent(job, run, repo, runner)
}

// applied delivers body and waits until the broker has applied it, and
// what it queued (a job served again: a box step) too.
func (df *doorFixture) applied(t *testing.T, body []byte) {
	t.Helper()
	df.deliver(t, "workflow_job", body)
	eventuallyBox(t, "applied", func() bool { return df.svc.Undone(context.Background()) == 0 })
	time.Sleep(200 * time.Millisecond)
}

var grantGH = [2]string{"GH_TOKEN", "contents"}

// (a) A main push job named by a job grant: its box is guarded to its run
// and nothing is minted at serve; once GitHub says it runs on its own
// runner, one token for its repository alone is minted into the box, then
// the ready marker; its completion revokes the token at once.
func TestJobGrantMintsWhenTheJobRunsOnItsOwnBox(t *testing.T) {
	df := newDoorFixture(t, true, grantGH)
	ctx := context.Background()
	df.set(func(d *doorGitHub) { d.runs[700] = doorRun{event: "push", branch: "main", sha: mainSHA} })
	g := df.served(t, 7001, 700, 1)
	want := protocol.JITRun{ID: 700, WorkflowRef: "acme/app/.github/workflows/x.yml@refs/heads/main", Ref: "refs/heads/main", HoldS: 300}
	if g.Run == nil || *g.Run != want || g.Repo != "acme/app" {
		t.Fatalf("the guard: %+v run %+v, want run %+v", g, g.Run, want)
	}
	eventuallyBox(t, "the box ready", func() bool {
		hs, _ := computeclient.RunTargetHandles(ctx, df.owner, "small", false)
		return len(hs) == 1 && hs[0].State == "ready"
	})
	if all, _ := df.issued(); len(all) != 0 || len(df.boxSecrets()) != 0 {
		t.Fatalf("minted at serve: %+v %v", all, df.boxSecrets())
	}

	df.deliver(t, "workflow_job", df.startOn(7001, 700, "acme/app", "potluck-7001"))
	eventually(t, "the marker", df.marked)
	all, _ := df.issued()
	if len(all) != 1 || all[0].repos != "app" || all[0].perms != "map[contents:write metadata:read]" {
		t.Fatalf("issued %+v, want one token for app with contents write", all)
	}
	got := df.boxSecrets()
	if len(got) != 2 || got[0] != [2]string{"GH_TOKEN", all[0].token} || got[1] != [2]string{protocol.SecretNameReady, "700"} {
		t.Fatalf("written into the box: %v, want GH_TOKEN then the marker", got)
	}
	// A redelivered in_progress mints nothing more.
	df.applied(t, df.startOn(7001, 700, "acme/app", "potluck-7001"))
	if all, _ := df.issued(); len(all) != 1 {
		t.Fatalf("a redelivery minted again: %+v", all)
	}

	df.deliver(t, "workflow_job", jobEvent("completed", 7001, 700, "acme/app", true, "self-hosted", "sengi"))
	eventually(t, "the token revoked", func() bool { _, rev := df.issued(); return len(rev) == 1 })
	whys := df.revokedRecorded(t, 1)
	if len(whys) != 1 || whys[0] != string(issuers.EndJobCompleted) {
		t.Errorf("revoked for %q, want once for the job's completion", whys)
	}
}

// asked is how often GitHub was asked to compare a commit with main and
// whether a tag is named main.
func (df *doorFixture) asked() (compares, tags int) {
	df.gh.mu.Lock()
	defer df.gh.mu.Unlock()
	return df.door.compares, df.door.tagCalls
}

// (b) Runs a job grant does not name: a fork's (its head repository's id
// is another's, under the same name), a commit not on main though its
// head branch is called main, a tag named main whose commit is on main,
// another branch, one calling another repository's reusable workflow, a
// pull_request, and a main push of a workflow no grant names (GitHub not
// even asked about its ref). Each is served as before: no run in its
// guard, nothing minted, no marker.
func TestJobGrantNamesOnlyItsOwnMainRun(t *testing.T) {
	df := newDoorFixture(t, true, grantGH)
	other, onMain := strings.Repeat("b", 40), strings.Repeat("c", 40)
	cases := []struct {
		name  string
		run   doorRun
		tag   bool // a tag named main exists
		noAsk bool // GitHub is not asked about the ref
	}{
		{"a fork under the same name", doorRun{event: "push", branch: "main", sha: mainSHA, headID: 424242}, false, true},
		{"a tag named main, not on main", doorRun{event: "push", branch: "main", sha: other}, false, false},
		{"a tag named main, on main", doorRun{event: "push", branch: "main", sha: onMain}, true, false},
		{"a re-run of an older commit on main", doorRun{event: "push", branch: "main", sha: onMain, attempt: 2}, false, false},
		{"another branch", doorRun{event: "push", branch: "dev", sha: mainSHA}, false, true},
		{"another repository's reusable workflow", doorRun{event: "push", branch: "main", sha: mainSHA,
			refs: `[{"path":"evil/lib/.github/workflows/w.yml@main","sha":"` + mainSHA + `","ref":"refs/heads/main"}]`}, false, true},
		{"a pull_request", doorRun{event: "pull_request", branch: "main", sha: mainSHA}, false, true},
		{"a workflow no grant names", doorRun{event: "push", branch: "main", sha: mainSHA, path: ".github/workflows/other.yml"}, false, true},
	}
	df.set(func(d *doorGitHub) { d.compare[other], d.compare[onMain] = "diverged", "behind" })
	for i, c := range cases {
		job, run := int64(7101+i), int64(710+i)
		df.set(func(d *doorGitHub) { d.runs[run], d.tags["main"] = c.run, c.tag })
		c0, t0 := df.asked()
		if g := df.served(t, job, run, i+1); g.Run != nil {
			t.Errorf("%s: guarded to run %+v", c.name, g.Run)
		}
		df.applied(t, df.startOn(job, run, "acme/app", fmt.Sprintf("potluck-%d", job)))
		if all, _ := df.issued(); len(all) != 0 || df.marked() {
			t.Fatalf("%s: minted %+v, box secrets %v", c.name, all, df.boxSecrets())
		}
		if c1, t1 := df.asked(); c.noAsk && (c1 != c0 || t1 != t0) {
			t.Errorf("%s: GitHub asked %d compares, %d tag reads", c.name, c1-c0, t1-t0)
		}
		df.applied(t, jobEvent("completed", job, run, "acme/app", true, "self-hosted", "sengi"))
	}
	if c, tg := df.asked(); c == 0 || tg == 0 {
		t.Errorf("GitHub was asked %d compares, %d tag reads; want both", c, tg)
	}

	// Runs it does name: a re-run of the tip, and a first attempt of an
	// older commit on main.
	named := []struct {
		name string
		run  doorRun
	}{
		{"a re-run of the tip", doorRun{event: "push", branch: "main", sha: mainSHA, attempt: 2}},
		{"a first attempt of an older commit on main", doorRun{event: "push", branch: "main", sha: onMain, attempt: 1}},
	}
	df.set(func(d *doorGitHub) { d.tags["main"] = false })
	n := len(cases)
	for i, c := range named {
		job, run := int64(7151+i), int64(715+i)
		df.set(func(d *doorGitHub) { d.runs[run] = c.run })
		n++
		if g := df.served(t, job, run, n); g.Run == nil || g.Run.ID != run {
			t.Fatalf("%s: guard %+v, want run %d", c.name, g, run)
		}
		before, _ := df.issued()
		df.deliver(t, "workflow_job", df.startOn(job, run, "acme/app", fmt.Sprintf("potluck-%d", job)))
		eventually(t, c.name+": its token", func() bool { all, _ := df.issued(); return len(all) == len(before)+1 })
		df.applied(t, jobEvent("completed", job, run, "acme/app", true, "self-hosted", "sengi"))
	}
}

// (c) GitHub failing at in_progress (the run, the compare, the tag read)
// mints nothing and writes no marker; the tag read failing at serve holds
// the job (served once GitHub answers); a failure after one credential was
// minted (GitHub refusing the second grant's) revokes it and writes no
// marker.
func TestJobGrantFailsClosed(t *testing.T) {
	df := newDoorFixture(t, true, grantGH, [2]string{"ISSUES_TOKEN", "issues"})
	n := 0
	for i, what := range []string{"compare", "run", "tag"} {
		job, run := int64(7201+i), int64(720+i)
		df.set(func(d *doorGitHub) { d.runs[run] = doorRun{event: "push", branch: "main", sha: mainSHA} })
		n++
		if g := df.served(t, job, run, n); g.Run == nil || g.Run.ID != run {
			t.Fatalf("job %d's guard: %+v", job, g)
		}
		df.set(func(d *doorGitHub) { d.fail[what] = true })
		df.applied(t, df.startOn(job, run, "acme/app", fmt.Sprintf("potluck-%d", job)))
		df.set(func(d *doorGitHub) { d.fail[what] = false })
		if all, _ := df.issued(); len(all) != 0 || df.marked() {
			t.Fatalf("GitHub failing the %s: minted %+v, box secrets %v", what, all, df.boxSecrets())
		}
		df.applied(t, jobEvent("completed", job, run, "acme/app", true, "self-hosted", "sengi"))
	}

	// The tag read failing at serve: no box until GitHub answers.
	df.set(func(d *doorGitHub) {
		d.runs[723] = doorRun{event: "push", branch: "main", sha: mainSHA}
		d.fail["tag"] = true
	})
	_, before := df.asked()
	df.deliver(t, "workflow_job", jobEvent("queued", 7204, 723, "acme/app", true, "self-hosted", "sengi"))
	eventually(t, "the tag asked", func() bool { _, tg := df.asked(); return tg > before })
	time.Sleep(time.Second)
	if g := df.guards(); len(g) != n {
		t.Fatalf("served while GitHub did not answer the tag read: %d acquires, want %d", len(g), n)
	}
	df.set(func(d *doorGitHub) { d.fail["tag"] = false })
	n++
	eventuallyBox(t, "served once GitHub answers", func() bool { return len(df.guards()) == n })
	if g := df.guards()[n-1]; g == nil || g.Run == nil || g.Run.ID != 723 {
		t.Fatalf("the held job's guard: %+v", g)
	}
	if all, _ := df.issued(); len(all) != 0 {
		t.Fatalf("minted at serve: %+v", all)
	}
	df.applied(t, jobEvent("completed", 7204, 723, "acme/app", true, "self-hosted", "sengi"))

	df.set(func(d *doorGitHub) {
		d.runs[724] = doorRun{event: "push", branch: "main", sha: mainSHA}
		d.refuse = "issues"
	})
	n++
	df.served(t, 7205, 724, n)
	df.deliver(t, "workflow_job", df.startOn(7205, 724, "acme/app", "potluck-7205"))
	eventually(t, "the first token revoked", func() bool { all, rev := df.issued(); return len(all) == 1 && len(rev) == 1 })
	time.Sleep(300 * time.Millisecond)
	all, _ := df.issued()
	if len(all) != 1 || all[0].perms != "map[contents:write metadata:read]" || df.marked() {
		t.Fatalf("after a refused second grant: issued %+v, box secrets %v; want the first alone, revoked, no marker", all, df.boxSecrets())
	}
}

// boxWrites is what was written into each box, by handle.
func (df *doorFixture) boxWrites() map[string]map[string]string {
	out := map[string]map[string]string{}
	for _, c := range df.calls() {
		if c.Verb == "secrets" {
			var a protocol.BoxSecretsArgs
			_ = json.Unmarshal(c.Args, &a)
			if out[c.ID] == nil {
				out[c.ID] = map[string]string{}
			}
			for n, v := range a.Secrets {
				out[c.ID][n] = v
			}
		}
	}
	return out
}

// (d) Two jobs of one run swap runners: each box gets its token and the
// marker for the job holding it, and the completion of a job revokes the
// token of the box it held (the other job's). A job of another run taking
// a runner gets its own grant there, under its own run's re-guard marker,
// never the box's run's.
func TestJobGrantSwapWithinARun(t *testing.T) {
	df := newDoorFixture(t, true, grantGH)
	df.set(func(d *doorGitHub) {
		d.runs[730] = doorRun{event: "push", branch: "main", sha: mainSHA}
		d.runs[732] = doorRun{event: "push", branch: "main", sha: mainSHA}
		d.runs[733] = doorRun{event: "push", branch: "main", sha: mainSHA}
	})
	df.served(t, 7301, 730, 1)
	df.served(t, 7302, 730, 2)
	df.applied(t, df.startOn(7302, 730, "acme/app", "potluck-7301")) // B takes A's runner
	eventually(t, "B's token in A's box", df.marked)
	all, _ := df.issued()
	if len(all) != 1 || len(df.boxWrites()) != 1 {
		t.Fatalf("B on A's runner: issued %+v, writes %v; want one token, one box", all, df.boxWrites())
	}
	inA := all[0].token
	df.applied(t, df.startOn(7301, 730, "acme/app", "potluck-7302")) // A takes B's runner
	eventually(t, "A's token and the marker in B's box", func() bool {
		all, _ := df.issued()
		w := df.boxWrites()
		if len(all) != 2 || len(w) != 2 {
			return false
		}
		for _, s := range w {
			if s[protocol.SecretNameReady] == "" {
				return false
			}
		}
		return true
	})
	all, _ = df.issued()
	w := df.boxWrites()
	if len(all) != 2 || len(w) != 2 {
		t.Fatalf("after the swap: issued %+v, writes %v; want a token in each box", all, w)
	}
	for h, s := range w {
		if s[protocol.SecretNameReady] != "730" || (s["GH_TOKEN"] != all[0].token && s["GH_TOKEN"] != all[1].token) {
			t.Errorf("box %s holds %v", h, s)
		}
	}

	df.deliver(t, "workflow_job", jobEvent("completed", 7302, 730, "acme/app", true, "self-hosted", "sengi"))
	eventually(t, "the token B held revoked", func() bool { _, rev := df.issued(); return len(rev) >= 1 })
	time.Sleep(300 * time.Millisecond)
	if _, rev := df.issued(); !slices.Equal(rev, []string{inA}) {
		t.Fatalf("B's completion revoked %v, want only %s (A's box, which B held)", rev, inA)
	}
	whys := df.revokedRecorded(t, 1)
	if len(whys) != 1 || whys[0] != string(issuers.EndJobCompleted) {
		t.Errorf("revoked for %q, want once for B's completion", whys)
	}

	// A job of another run on a guarded box's runner: the box is re-guarded
	// to its run (733), and holds its own token under that run's marker.
	df.served(t, 7303, 732, 3)
	df.served(t, 7304, 733, 4)
	df.applied(t, df.startOn(7304, 733, "acme/app", "potluck-7303"))
	want := "733 acme/app/.github/workflows/x.yml@refs/heads/main refs/heads/main"
	eventually(t, "7304's token and its run's re-guard marker in 7303's box", func() bool {
		all, _ := df.issued()
		w := df.boxWrites()
		return len(all) == 3 && len(w) == 3 && w["box:small/b3"][protocol.SecretNameReady] == want
	})
	if all, _ := df.issued(); df.boxWrites()["box:small/b3"]["GH_TOKEN"] != all[2].token || len(df.boxWrites()["box:small/b3"]) != 2 {
		t.Fatalf("a job of another run on the runner: issued %+v, writes %v", all, df.boxWrites())
	}
}

// (e) With no issuer configured a main push job is served exactly as
// before: its guard names no run, GitHub is not asked about the commit or
// a tag, nothing is written into its box.
func TestNoIssuerServesAsBefore(t *testing.T) {
	df := newDoorFixture(t, false)
	if issuers.From(df.dm.Deps) != nil {
		t.Fatal("an issuer runs with none configured")
	}
	df.set(func(d *doorGitHub) { d.runs[740] = doorRun{event: "push", branch: "main", sha: mainSHA} })
	g := df.served(t, 7401, 740, 1)
	b, _ := json.Marshal(g)
	if string(b) != `{"repo":"acme/app","events":["push","pull_request","workflow_dispatch","schedule","merge_group","release","repository_dispatch"]}` {
		t.Errorf("the guard: %s", b)
	}
	df.applied(t, df.startOn(7401, 740, "acme/app", "potluck-7401"))
	c, tg := df.asked()
	if all, _ := df.issued(); len(all) != 0 || len(df.boxSecrets()) != 0 || c != 0 || tg != 0 {
		t.Fatalf("with no issuer: minted %+v, box secrets %v, %d compares, %d tag reads", all, df.boxSecrets(), c, tg)
	}
}

// (f) The job completes while its token is being minted: the token minted
// after the completion is revoked at once.
func TestJobGrantCompletedWhileIssuing(t *testing.T) {
	df := newDoorFixture(t, true, grantGH)
	gate, gated := make(chan struct{}), make(chan struct{})
	df.set(func(d *doorGitHub) {
		d.runs[750] = doorRun{event: "push", branch: "main", sha: mainSHA}
		d.gate, d.gated = gate, gated
	})
	df.served(t, 7501, 750, 1)
	df.deliver(t, "workflow_job", df.startOn(7501, 750, "acme/app", "potluck-7501"))
	select {
	case <-gated:
	case <-time.After(60 * time.Second):
		t.Fatal("the token was never asked for")
	}
	// The completion is applied while the mint waits: issuing no longer
	// holds up the lane completions go through.
	df.deliver(t, "workflow_job", jobEvent("completed", 7501, 750, "acme/app", true, "self-hosted", "sengi"))
	eventually(t, "the completion applied", func() bool { return df.svc.Undone(context.Background()) == 0 })
	close(gate)
	eventually(t, "the late token revoked", func() bool { all, rev := df.issued(); return len(all) == 1 && len(rev) == 1 })
}

// (g) A job grant no request could ever pass (here: for exec, not a box)
// names no job: the job is served as any other, unheld.
func TestJobGrantNotForABoxGuardsNothing(t *testing.T) {
	df := newDoorFixture(t, true)
	if _, err := computeclient.AddSecretGrant(context.Background(), df.owner, computeapi.AddSecretGrantRequest{
		To:   computeapi.GrantTo{Job: &computeapi.GrantJob{Repo: "acme/app", Workflow: ".github/workflows/x.yml", Ref: "refs/heads/main", Events: []string{"push"}}},
		Kind: computeapi.SecretKindGitHubToken, Name: "GH_TOKEN", Scopes: []string{computeapi.SecretScopeExec},
		Ceiling: json.RawMessage(`{"repos":["acme/app"],"permissions":{"contents":"write"}}`)}); err != nil {
		t.Fatalf("grant: %v", err)
	}
	df.set(func(d *doorGitHub) { d.runs[760] = doorRun{event: "push", branch: "main", sha: mainSHA} })
	if g := df.served(t, 7601, 760, 1); g.Run != nil {
		t.Fatalf("guarded to a run by a grant that cannot mint: %+v", g.Run)
	}
}

// MUSTS 10 and 11: a main push run gets its job grant only when a ruleset
// the App cannot bypass requires a pull request on main, and every uses:
// its workflow pulls in (through local actions too), read at its commit,
// is pinned. Each other case is served unguarded and gets nothing; GitHub
// is asked with read-only tokens (metadata for the rules, contents for the
// files), never more.
func TestJobGrantNeedsReview(t *testing.T) {
	df := newDoorFixture(t, true, grantGH)
	unpinned := strings.Replace(pinnedWorkflow, "actions/checkout@0123456789abcdef0123456789abcdef01234567", "actions/checkout@v4", 1)
	cases := []struct {
		name string
		set  func(d *doorGitHub)
	}{
		{"no ruleset requires a pull request", func(d *doorGitHub) { d.noRule = true }},
		{"the App can bypass the ruleset", func(d *doorGitHub) { d.bypass = "always" }},
		{"a pull request no one approves", func(d *doorGitHub) { d.noApproval = true }},
		{"stale reviews dismissed, but the pusher may approve", func(d *doorGitHub) { d.stale = true }},
		{"a job container by tag", func(d *doorGitHub) {
			d.files[".github/workflows/x.yml"] = strings.Replace(pinnedWorkflow, "    runs-on: [self-hosted, sengi]\n", "    runs-on: [self-hosted, sengi]\n    container: node:latest\n", 1)
		}},
		{"a service image by tag", func(d *doorGitHub) {
			d.files[".github/workflows/x.yml"] = strings.Replace(pinnedWorkflow, "    runs-on: [self-hosted, sengi]\n", "    runs-on: [self-hosted, sengi]\n    services:\n      db:\n        image: postgres:16\n", 1)
		}},
		{"a local Dockerfile action", func(d *doorGitHub) {
			d.files[".github/actions/setup/action.yml"] = "runs:\n  using: docker\n  image: Dockerfile\n"
		}},
		{"another repository's reusable workflow, pinned", func(d *doorGitHub) {
			d.files[".github/workflows/x.yml"] = "on: push\njobs:\n  call:\n    uses: evil/lib/.github/workflows/w.yml@0123456789abcdef0123456789abcdef01234567\n"
		}},
		{"a second YAML document", func(d *doorGitHub) {
			d.files[".github/workflows/x.yml"] = pinnedWorkflow + "---\njobs: {}\n"
		}},
		{"a Uses: key in another case", func(d *doorGitHub) {
			d.files[".github/workflows/x.yml"] = pinnedWorkflow + "      - Uses: evil/act@main\n"
		}},
		{"an action pinned to a tag", func(d *doorGitHub) { d.files[".github/workflows/x.yml"] = unpinned }},
		{"a local action pulling in a branch", func(d *doorGitHub) {
			d.files[".github/actions/setup/action.yml"] = "runs:\n  using: composite\n  steps:\n    - uses: evil/act@main\n"
		}},
		{"a docker image by tag", func(d *doorGitHub) {
			d.files[".github/workflows/x.yml"] = strings.Replace(pinnedWorkflow, "alpine@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", "alpine:3", 1)
		}},
		{"a uses: from an expression", func(d *doorGitHub) {
			d.files[".github/workflows/x.yml"] = pinnedWorkflow + "      - uses: ${{ inputs.action }}\n"
		}},
		{"a local action that is not there", func(d *doorGitHub) { delete(d.files, ".github/actions/setup/action.yml") }},
	}
	for i, c := range cases {
		job, run := int64(7801+i), int64(780+i)
		saved := map[string]string{}
		df.set(func(d *doorGitHub) {
			d.runs[run] = doorRun{event: "push", branch: "main", sha: mainSHA}
			d.noRule, d.bypass, d.noApproval, d.stale = false, "", false, false
			for k, v := range d.files {
				saved[k] = v
			}
			c.set(d)
		})
		if g := df.served(t, job, run, i+1); g.Run != nil {
			t.Errorf("%s: guarded to run %+v", c.name, g.Run)
		}
		df.applied(t, df.startOn(job, run, "acme/app", fmt.Sprintf("potluck-%d", job)))
		if all, _ := df.issued(); len(all) != 0 || df.marked() {
			t.Fatalf("%s: minted %+v, box secrets %v", c.name, all, df.boxSecrets())
		}
		df.applied(t, jobEvent("completed", job, run, "acme/app", true, "self-hosted", "sengi"))
		df.set(func(d *doorGitHub) { d.files = saved })
	}
	// Everything in place: granted.
	df.set(func(d *doorGitHub) {
		d.runs[790] = doorRun{event: "push", branch: "main", sha: mainSHA}
		d.noRule, d.bypass, d.noApproval, d.stale = false, "", false, false
	})
	if g := df.served(t, 7901, 790, len(cases)+1); g.Run == nil || g.Run.ID != 790 {
		t.Fatalf("a reviewed, pinned run: guard %+v", g)
	}
	df.ready(t, 1)
	df.deliver(t, "workflow_job", df.startOn(7901, 790, "acme/app", "potluck-7901"))
	eventually(t, "the marker", df.marked)
	df.gh.mu.Lock()
	misused := slices.Clone(df.gh.misused)
	df.gh.mu.Unlock()
	if len(misused) != 0 {
		t.Errorf("GitHub was asked with the wrong tokens: %v", misused)
	}
}

// MUST 6: after a daemon restart the issuer issues a job's token again only
// while the job still holds its box: a job GitHub says completed while the
// daemon was down gets nothing again (and its row is recorded ended, so no
// later restart asks again); one still running on its runner gets a fresh
// token.
func TestJobGrantRestart(t *testing.T) {
	for _, completed := range []bool{true, false} {
		t.Run(map[bool]string{true: "done", false: "running"}[completed], func(t *testing.T) {
			df := newDoorFixture(t, true, grantGH)
			df.set(func(d *doorGitHub) { d.runs[770] = doorRun{event: "push", branch: "main", sha: mainSHA} })
			df.served(t, 7701, 770, 1)
			df.ready(t, 1)
			df.deliver(t, "workflow_job", df.startOn(7701, 770, "acme/app", "potluck-7701"))
			eventually(t, "the box's secrets issued", func() bool { return df.svc.JobSecretsState(context.Background(), 7701) == "issued" })
			df.gh.mu.Lock()
			df.gh.runnerOf[7701], df.gh.jobRun[7701] = "potluck-7701", 770
			df.gh.jobDone[7701] = completed // no webhook: the daemon is down
			df.gh.mu.Unlock()
			df.restart(t)
			df.startIssuer(t)
			if completed {
				time.Sleep(time.Second)
				if all, _ := df.issued(); len(all) != 1 {
					t.Fatalf("a completed job's box was issued again after the restart: %+v", all)
				}
				return
			}
			eventually(t, "a fresh token for the running job", func() bool { all, _ := df.issued(); return len(all) == 2 })
		})
	}
}
