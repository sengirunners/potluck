package runners_test

import (
	"cmp"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"hash/fnv"
	"io"
	"io/fs"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/computeclient"
	"github.com/sengirunners/potluck/conformance/refdriver"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/server"
	"github.com/sengirunners/potluck/runners"
	"github.com/sengirunners/potluck/runtarget"
	"github.com/sengirunners/potluck/wiring"
)

const (
	refPin   = "ref-image@sha256:0000000000000000000000000000000000000000000000000000000000000001"
	hookKey  = "webhook-secret-for-tests"
	inst     = 7
	jitValue = "eyJqaXQiOiJicm9rZXItdGVzdC1jb25maWcifQ=="
)

func TestMain(m *testing.M) {
	if code, ok := refdriver.Main(); ok { // this binary is also the box driver
		os.Exit(code)
	}
	os.Exit(m.Run())
}

// fakeGitHub is the GitHub the broker talks to: the App's endpoints, OAuth,
// and the manifest conversion.
type fakeGitHub struct {
	workflow  map[int64]string
	jobStarts map[int64]string
	jobNames  map[int64]string
	contents  string
	mu        sync.Mutex
	srv       *httptest.Server
	minted    []map[string]any
	deleted   []string
	runners   map[string]fakeRunner
	forks     map[int64]bool // run id -> from a fork
	// heads overrides a run's head repository ("" none: head_repository null);
	// repoFork marks the repositories that are themselves forks, whose own
	// runs GitHub answers with head_repository.fork true. A head's fork flag
	// is forks[id] (which also names the head stranger/app) or repoFork[head].
	heads    map[int64]*string
	repoFork map[string]bool
	jobDone  map[int64]bool
	userSee  []int64
	orgRole  string           // the signed-in person's role in acme
	events   map[int64]string // run id -> event (default pull_request)
	runRepo  map[int64]string // run id -> the run's repository (default the job's)
	status   map[int64]string // job id -> GitHub's status (default in_progress, completed when jobDone)
	tokens   map[string]fakeToken
	revoked  []string // tokens revoked (DELETE /installation/token)
	misused  []string // calls whose token was for another repository or lacked the permission
	// stopped is set when the broker is stopping (fixture.startBroker's stop):
	// its last requests, abandoned when its context ended, may reach this fake
	// after Close revoked their tokens, and are answered 401 as GitHub would
	// rather than counted as misuse.
	stopped  bool
	gate     chan struct{} // set: every call whose path holds gateOn waits for it to close
	gateOn   string        // default: mints (a serve stuck in a slow start)
	waiting  int           // calls waiting at the gate
	calls    []string
	rateLeft int             // set: every repository call answers X-RateLimit-Remaining this (of 5000, reset in an hour)
	busy     map[string]bool // runner name -> GitHub lists it busy (given a job)
	// Re-runs (requeue_test.go): the runner a job runs on and how it ended, a
	// run's status (default in_progress) and attempt (default 1), the re-runs
	// asked ("job <id>", "run <id>"), and the repositories whose installation
	// was not granted Actions: write.
	runnerOf   map[int64]string
	conclusion map[int64]string
	jobRun     map[int64]int64
	runStatus  map[int64]string
	runEnd     map[int64]string // a completed run's conclusion (default failure)
	attempt    map[int64]int
	reruns     []string
	noWrite    map[string]bool
	noContents bool
	// Repositories (repos_test.go): those that are public (default: acme/site
	// alone), each one's approval rule for outside contributors' runs
	// (default all_external_contributors; "error" answers 403), and the
	// installation on each account (acme: inst; any other: none).
	publicRepo map[string]bool
	// visibility: those GitHub answers private with this visibility
	// ("internal": visible to the whole enterprise).
	visibility map[string]string
	approval   map[string]string
	// repoIDs overrides a repository's id (default repoID); gone are the
	// repositories GitHub answers 404 for (deleted).
	repoIDs map[string]int64
	gone    map[string]bool
	// installReturn is where GitHub's install page sends the browser:
	// "setup_url" (the App's setup URL), "callback" (the App authorizes during
	// installation, GitHub's "Install & Authorize": the first callback URL with
	// a code) or "callback-no-state" (the same, the install URL's state lost).
	installReturn string
	// extra, when set, answers a request first (true: answered), under mu,
	// after the token checks (check_test.go: the lists the checker asks for).
	extra func(w http.ResponseWriter, r *http.Request) bool
}

// fakeToken is an installation token: the one repository it names and its
// permissions, as GitHub ties them.
type fakeToken struct{ repo, perms string }

func newFakeGitHub(t *testing.T) *fakeGitHub {
	g := &fakeGitHub{runners: map[string]fakeRunner{}, forks: map[int64]bool{}, heads: map[int64]*string{}, repoFork: map[string]bool{}, jobDone: map[int64]bool{}, userSee: []int64{inst},
		jobStarts: map[int64]string{}, workflow: map[int64]string{}, jobNames: map[int64]string{}, orgRole: "admin", events: map[int64]string{}, runRepo: map[int64]string{}, status: map[int64]string{}, tokens: map[string]fakeToken{},
		runnerOf: map[int64]string{}, conclusion: map[int64]string{}, jobRun: map[int64]int64{}, runStatus: map[int64]string{},
		attempt: map[int64]int{}, noWrite: map[string]bool{}, runEnd: map[int64]string{},
		publicRepo: map[string]bool{"acme/site": true}, visibility: map[string]string{}, approval: map[string]string{}, repoIDs: map[string]int64{}, gone: map[string]bool{},
		busy: map[string]bool{}}
	rerunRe := regexp.MustCompile(`^/repos/[^/]+/[^/]+/actions/(jobs|runs)/(\d+)/(rerun|rerun-failed-jobs)$`)
	runRe := regexp.MustCompile(`^/repos/([^/]+/[^/]+)/actions/runs/(\d+)$`)
	jobRe := regexp.MustCompile(`^/repos/[^/]+/[^/]+/actions/jobs/(\d+)$`)
	scope := `^/(orgs/acme|repos/[^/]+/[^/]+)/actions/runners`
	mintRe, listRe := regexp.MustCompile(scope+`/generate-jitconfig$`), regexp.MustCompile(scope+`$`)
	delRe := regexp.MustCompile(scope + `/(\d+)$`)
	repoRe := regexp.MustCompile(`^/repos/([^/]+/[^/]+)(?:/actions/(runs|jobs|runners|permissions/fork-pr-contributor-approval)|$)`)
	repoInstRe := regexp.MustCompile(`^/repos/([^/]+)/([^/]+)/installation$`)
	g.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		gate, on := g.gate, g.gateOn
		if on == "" {
			on = "/generate-jitconfig"
		}
		if gate != nil && strings.Contains(r.URL.Path, on) {
			g.waiting++
		} else {
			gate = nil
		}
		g.mu.Unlock()
		if gate != nil {
			<-gate
		}
		g.mu.Lock()
		defer g.mu.Unlock()
		g.calls = append(g.calls, r.Method+" "+r.URL.Path)
		p := r.URL.Path
		if bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "); g.stopped && strings.HasPrefix(bearer, "inst-token-") {
			if _, live := g.tokens[bearer]; !live {
				w.WriteHeader(401)
				fmt.Fprint(w, `{"message":"Bad credentials"}`)
				return
			}
		}
		if g.rateLeft > 0 && strings.HasPrefix(p, "/repos/") {
			w.Header().Set("X-RateLimit-Limit", "5000")
			w.Header().Set("X-RateLimit-Remaining", fmt.Sprint(g.rateLeft))
			w.Header().Set("X-RateLimit-Reset", fmt.Sprint(time.Now().Add(time.Hour).Unix()))
		}
		// A repository's endpoints take only a token for that repository with
		// the permission they need, as GitHub's do.
		if m := repoRe.FindStringSubmatch(p); m != nil {
			tok := g.tokens[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")]
			want := "map[actions:read metadata:read]"
			if m[2] == "runners" {
				want = "map[administration:write metadata:read]"
			} else if strings.HasPrefix(m[2], "permissions/") {
				want = "map[administration:read metadata:read]"
			} else if rerunRe.MatchString(p) {
				want = "map[actions:write metadata:read]"
			}
			if !strings.EqualFold(tok.repo, m[1]) || tok.perms != want {
				g.misused = append(g.misused, fmt.Sprintf("%s %s with a token for %q %s", r.Method, p, tok.repo, tok.perms))
				w.WriteHeader(403)
				fmt.Fprint(w, `{"message":"Resource not accessible by integration"}`)
				return
			}
		}
		if g.extra != nil && g.extra(w, r) {
			return
		}
		switch {
		case r.Method == "GET" && p == fmt.Sprintf("/app/installations/%d", inst):
			fmt.Fprintf(w, `{"id":%d,"account":{"login":"acme","type":"Organization"}}`, inst)
		case r.Method == "GET" && p == "/app/installations/11": // the signed-in person's own account
			fmt.Fprint(w, `{"id":11,"account":{"login":"Ana","type":"User"}}`)
		case r.Method == "GET" && p == "/app/installations/12": // someone else's account
			fmt.Fprint(w, `{"id":12,"account":{"login":"bob","type":"User"}}`)
		case r.Method == "GET" && strings.HasPrefix(p, "/app/installations/"):
			w.WriteHeader(404)
			fmt.Fprint(w, `{"message":"Not Found"}`)
		case r.Method == "POST" && p == fmt.Sprintf("/app/installations/%d/access_tokens", inst):
			// Every token names one repository and exactly one call's permissions:
			// the run and job checks, or one repo's runners (never the org's).
			var b struct {
				Repositories []string          `json:"repositories"`
				Permissions  map[string]string `json:"permissions"`
			}
			_ = json.NewDecoder(r.Body).Decode(&b)
			perms := fmt.Sprint(b.Permissions)
			if len(b.Repositories) == 0 { // a token for every repository of the installation: never asked for
				g.misused = append(g.misused, fmt.Sprintf("a token for the whole installation, %s", perms))
				w.WriteHeader(422)
				fmt.Fprint(w, `{"message":"no token for the whole installation in these tests"}`)
				return
			}
			if len(b.Repositories) != 1 || b.Repositories[0] == "" || (perms != "map[actions:read metadata:read]" &&
				perms != "map[administration:write metadata:read]" && perms != "map[actions:write metadata:read]" && perms != "map[contents:read metadata:read]" &&
				perms != "map[administration:read metadata:read]" && perms != "map[metadata:read]") {
				t.Errorf("an installation token asked for %+v", b)
			}
			if b.Permissions["contents"] == "read" && g.noContents {
				w.WriteHeader(422)
				fmt.Fprint(w, `{"message":"The permissions requested are not granted to this installation."}`)
				return
			}
			if b.Permissions["actions"] == "write" && g.noWrite["acme/"+strings.Join(b.Repositories, ",")] {
				w.WriteHeader(422)
				fmt.Fprint(w, `{"message":"The permissions requested are not granted to this installation."}`)
				return
			}
			tok := fmt.Sprintf("inst-token-%d", len(g.tokens)+1)
			g.tokens[tok] = fakeToken{"acme/" + strings.Join(b.Repositories, ","), perms} // installation 7 is acme's
			w.WriteHeader(201)
			fmt.Fprintf(w, `{"token":%q,"expires_at":%q}`, tok, time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
		case r.Method == "DELETE" && p == "/installation/token":
			tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if _, ok := g.tokens[tok]; !ok {
				w.WriteHeader(401)
				return
			}
			delete(g.tokens, tok)
			g.revoked = append(g.revoked, tok)
			w.WriteHeader(204)
		case r.Method == "GET" && strings.Contains(p, "/contents/"):
			tok := g.tokens[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")]
			if tok.repo != "acme/app" || tok.perms != "map[contents:read metadata:read]" {
				t.Errorf("workflow read token scope: %+v", tok)
			}
			if r.URL.Query().Get("ref") != strings.Repeat("a", 40) {
				t.Errorf("mutable workflow revision: %s", r.URL.String())
			}
			json.NewEncoder(w).Encode(map[string]string{"encoding": "base64", "content": base64.StdEncoding.EncodeToString([]byte(g.contents))})
		case r.Method == "GET" && repoInstRe.MatchString(p): // as the App
			if m := repoInstRe.FindStringSubmatch(p); m[1] != "acme" {
				w.WriteHeader(404)
				fmt.Fprint(w, `{"message":"Not Found"}`)
				return
			}
			fmt.Fprintf(w, `{"id":%d}`, inst)
		case r.Method == "GET" && strings.HasSuffix(p, "/actions/permissions/fork-pr-contributor-approval"):
			repo := repoRe.FindStringSubmatch(p)[1]
			if g.approval[repo] == "error" {
				w.WriteHeader(403)
				fmt.Fprint(w, `{"message":"Resource not accessible by integration"}`)
				return
			}
			fmt.Fprintf(w, `{"approval_policy":%q}`, cmp.Or(g.approval[repo], "all_external_contributors"))
		case r.Method == "GET" && repoRe.MatchString(p) && repoRe.FindStringSubmatch(p)[2] == "":
			repo, vis := repoRe.FindStringSubmatch(p)[1], "private"
			if g.gone[repo] {
				w.WriteHeader(404)
				fmt.Fprint(w, `{"message":"Not Found"}`)
				return
			}
			switch {
			case g.publicRepo[repo]:
				vis = "public"
			case g.visibility[repo] != "":
				vis = g.visibility[repo]
			}
			fmt.Fprintf(w, `{"id":%d,"full_name":%q,"private":%v,"visibility":%q,"fork":%v}`, cmp.Or(g.repoIDs[repo], repoID(repo)), repo,
				vis != "public", vis, g.repoFork[repo])
		case r.Method == "GET" && runRe.MatchString(p):
			m := runRe.FindStringSubmatch(p)
			var id int64
			fmt.Sscan(m[2], &id)
			repo, ev := m[1], "pull_request"
			if g.runRepo[id] != "" {
				repo = g.runRepo[id]
			}
			if g.events[id] != "" {
				ev = g.events[id]
			}
			head := repo
			if g.forks[id] {
				head = "stranger/app"
			}
			if h, ok := g.heads[id]; ok {
				head = *h
			}
			headJSON := "null"
			if head != "" {
				headJSON = fmt.Sprintf(`{"full_name":%q,"fork":%v}`, head, g.forks[id] || g.repoFork[head])
			}
			concl := ""
			if g.runStatus[id] == "completed" {
				concl = cmp.Or(g.runEnd[id], "failure")
			}
			if raw := g.workflow[id]; raw != "" {
				fmt.Fprint(w, raw)
				return
			}
			fmt.Fprintf(w, `{"id":%d,"event":%q,"status":%q,"conclusion":%q,"run_attempt":%d,"head_repository":%s,"repository":{"full_name":%q}}`,
				id, ev, cmp.Or(g.runStatus[id], "in_progress"), concl, max(g.attempt[id], 1), headJSON, repo)
		case r.Method == "GET" && jobRe.MatchString(p):
			var id int64
			fmt.Sscan(jobRe.FindStringSubmatch(p)[1], &id)
			st := "in_progress"
			if g.jobDone[id] {
				st = "completed"
			} else if g.status[id] != "" {
				st = g.status[id]
			}
			fmt.Fprintf(w, `{"started_at":%q,"name":%q,"id":%d,"status":%q,"conclusion":%q,"runner_name":%q,"run_id":%d,"run_attempt":%d}`, g.jobStarts[id], g.jobNames[id], id, st, g.conclusion[id],
				g.runnerOf[id], g.jobRun[id], max(g.attempt[g.jobRun[id]], 1))
		case r.Method == "POST" && rerunRe.MatchString(p):
			// GitHub re-runs only a completed run; the run's next attempt starts.
			m := rerunRe.FindStringSubmatch(p)
			var id int64
			fmt.Sscan(m[2], &id)
			run := id
			if m[1] == "jobs" {
				run = g.jobRun[id]
			}
			if g.runStatus[run] != "completed" {
				w.WriteHeader(403)
				fmt.Fprint(w, `{"message":"This workflow is already running"}`)
				return
			}
			g.reruns = append(g.reruns, strings.TrimSuffix(m[1], "s")+" "+m[2])
			g.runStatus[run], g.attempt[run] = "queued", max(g.attempt[run], 1)+1
			w.WriteHeader(201)
			fmt.Fprint(w, `{}`)
		case r.Method == "POST" && mintRe.MatchString(p):
			var b map[string]any
			_ = json.NewDecoder(r.Body).Decode(&b)
			b["scope"] = mintRe.FindStringSubmatch(p)[1]
			g.minted = append(g.minted, b)
			id := int64(100 + len(g.minted))
			var labels []string
			for _, l := range b["labels"].([]any) {
				labels = append(labels, fmt.Sprint(l))
			}
			g.runners[fmt.Sprint(b["name"])] = fakeRunner{id, b["scope"].(string), labels}
			w.WriteHeader(201)
			fmt.Fprintf(w, `{"runner":{"id":%d,"name":%q},"encoded_jit_config":%q}`, id, b["name"], jitValue)
		case r.Method == "GET" && listRe.MatchString(p):
			var rs []string
			for n, x := range g.runners {
				if x.scope == listRe.FindStringSubmatch(p)[1] {
					rs = append(rs, fmt.Sprintf(`{"id":%d,"name":%q,"status":"offline","busy":%t}`, x.id, n, g.busy[n]))
				}
			}
			fmt.Fprintf(w, `{"total_count":%d,"runners":[%s]}`, len(rs), strings.Join(rs, ","))
		case r.Method == "DELETE" && delRe.MatchString(p):
			m := delRe.FindStringSubmatch(p)
			for n, x := range g.runners {
				if fmt.Sprint(x.id) == m[2] && x.scope == m[1] {
					delete(g.runners, n)
					g.deleted = append(g.deleted, n)
				}
			}
			w.WriteHeader(204)
		case r.Method == "POST" && p == "/login/oauth/access_token":
			_ = r.ParseForm()
			if cs := r.Form.Get("client_secret"); r.Form.Get("code") != "good" || (cs != "client-secret" && cs != "new-client-secret") {
				fmt.Fprint(w, `{"error":"bad_verification_code","error_description":"bad code"}`)
				return
			}
			fmt.Fprint(w, `{"access_token":"user-token"}`)
		case r.Method == "GET" && p == "/user":
			fmt.Fprint(w, `{"login":"ana"}`)
		case r.Method == "GET" && p == "/user/memberships/orgs/acme":
			switch g.orgRole {
			case "no-members-read": // the App lacks Members read
				w.WriteHeader(403)
				fmt.Fprint(w, `{"message":"Resource not accessible by integration","status":"403"}`)
			case "none": // not affiliated with the org
				w.WriteHeader(404)
				fmt.Fprint(w, `{"message":"Not Found","status":"404"}`)
			default:
				fmt.Fprintf(w, `{"state":"active","role":%q}`, g.orgRole)
			}
		case r.Method == "GET" && p == "/user/installations":
			if r.Header.Get("Authorization") != "Bearer user-token" {
				w.WriteHeader(401)
				return
			}
			var is []string
			for _, id := range g.userSee {
				is = append(is, fmt.Sprintf(`{"id":%d}`, id))
			}
			fmt.Fprintf(w, `{"total_count":%d,"installations":[%s]}`, len(is), strings.Join(is, ","))
		case r.Method == "GET" && p == "/apps/z3-compute/installations/new": // the person installs on acme
			q := url.Values{"installation_id": {fmt.Sprint(inst)}, "setup_action": {"install"}}
			if st := r.URL.Query().Get("state"); st != "" && g.installReturn != "callback-no-state" {
				q.Set("state", st)
			}
			to := "https://pool.example/v0/runners/app/installed?"
			if g.installReturn != "setup_url" {
				q.Set("code", "install-code") // valid at GitHub; this fake accepts only "good"
				to = "https://pool.example/v0/runners/oauth?"
			}
			http.Redirect(w, r, to+q.Encode(), http.StatusFound)
		case r.Method == "GET" && p == "/login/oauth/authorize": // already authorized: straight back
			q := url.Values{"code": {"good"}, "state": {r.URL.Query().Get("state")}}
			http.Redirect(w, r, r.URL.Query().Get("redirect_uri")+"?"+q.Encode(), http.StatusFound)
		case r.Method == "POST" && p == "/app-manifests/mcode/conversions":
			w.WriteHeader(201)
			pk, _ := json.Marshal(appPEM)
			fmt.Fprintf(w, `{"id":99,"slug":"z3-compute","owner":{"login":"acme"},"client_id":"Iv1.new","client_secret":"new-client-secret",
				"webhook_secret":"new-webhook-secret","pem":%s}`, pk)
		default:
			w.WriteHeader(404)
			fmt.Fprintf(w, `{"message":"no fake for %s %s"}`, r.Method, p)
		}
	}))
	t.Cleanup(func() {
		g.mu.Lock()
		if g.gate != nil {
			close(g.gate) // never leave a mint waiting: Close waits for it
			g.gate = nil
		}
		misused := g.misused
		g.mu.Unlock()
		g.srv.Close()
		for _, m := range misused {
			t.Errorf("GitHub refused %s", m)
		}
	})
	return g
}

// appPEM is the key the fake manifest conversion hands over (a real one, so
// the broker can act as the new App).
var appPEM = func() string {
	k, _ := rsa.GenerateKey(rand.Reader, 2048)
	return string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)}))
}()

// fakeRunner is a registered runner: where it registered ("orgs/acme" or
// "repos/<owner>/<repo>") and its labels.
type fakeRunner struct {
	id     int64
	scope  string
	labels []string
}

// eligible is GitHub's own matching: the runners a queued job of repo with
// these labels may be given. An org runner (the Default group: every repo of
// the org) takes any of the org's repos, a repo runner only its own; and the
// runner has every label the job asks for.
func (g *fakeGitHub) eligible(repo string, labels []string) (names []string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for n, x := range g.runners {
		if x.scope != "repos/"+repo && !(x.scope == "orgs/acme" && strings.HasPrefix(repo, "acme/")) {
			continue
		}
		all := true
		for _, l := range labels {
			all = all && slices.Contains(x.labels, l)
		}
		if all {
			names = append(names, n)
		}
	}
	return names
}

func (g *fakeGitHub) snapshot() (minted []map[string]any, deleted []string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append(minted, g.minted...), append(deleted, g.deleted...)
}

type fixture struct {
	cfg     *config.Config
	tap     string                 // the box driver, as the run-target service runs it
	before  func(*runners.Service) // applied to the broker before each start
	stop    func()                 // stops the running daemon
	dm      *server.Daemon
	svc     *runners.Service
	gh      *fakeGitHub
	owner   *client.Client
	home    string
	seen    string // requests the box driver was sent
	gate    string // while this file exists, the box driver holds every acquire's answer (the box made)
	state   string // the box driver's state directory: a record per box
	project string
	remote  string // the pool project's repo remote (a box binding's config)
	clock   *clock // the broker's time
}

// clock is the broker's time in a fixture: the real time, plus what a test
// moved it ahead by.
type clock struct {
	mu    sync.Mutex
	ahead time.Duration
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return time.Now().Add(c.ahead)
}

// advance moves the broker's time ahead by d.
func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	c.ahead += d
	c.mu.Unlock()
}

func secretFile(t *testing.T, dir, name, v string) string {
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(v), 0o600); err != nil {
		t.Fatal(err)
	}
	return "file:" + p
}

func newFixture(t *testing.T) *fixture { return newFixtureWith(t, nil) }

// newFixtureWith is newFixture with its runners config changed by edit
// before the daemon starts, saved to config.json as a person would have.
// Its pool binds one box, small, named in sizes (label sengi).
func newFixtureWith(t *testing.T, edit func(*config.Runners)) *fixture {
	return newFixtureStarting(t, edit, nil)
}

// newFixtureStarting is newFixtureWith with before applied to the broker
// just before it starts.
func newFixtureStarting(t *testing.T, edit func(*config.Runners), before func(*runners.Service)) *fixture {
	return newPoolFixtureStarting(t, func(r *config.Runners) {
		r.Label, r.Pool.Sizes, r.Pool.DefaultSize = "sengi", map[string]string{"small": "small"}, "small"
		if edit != nil {
			edit(r)
		}
	}, before, box("small", "small", "linux", "1"))
}

// newPoolFixture is a broker whose config conf edits (from a pool "pool"
// with no label and no sizes, as zm runners setup leaves it) and whose pool
// project binds boxes (driver ref, each box's Config added to the fixture's).
func newPoolFixture(t *testing.T, conf func(*config.Runners), boxes ...computeapi.RunTarget) *fixture {
	return newPoolFixtureStarting(t, conf, nil, boxes...)
}

// newPoolFixtureStarting is newPoolFixture with before applied to the broker
// just before it starts. The checker's loop ticks once an hour unless before
// says otherwise: tests call Check.
func newPoolFixtureStarting(t *testing.T, conf func(*config.Runners), before func(*runners.Service), boxes ...computeapi.RunTarget) *fixture {
	home, err := os.MkdirTemp("", "zm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	t.Setenv("HOME", home)
	cfg, err := config.LoadFrom(home)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Human, cfg.Machine = "judy", "mini"
	gh := newFakeGitHub(t)
	keys := t.TempDir()
	k, _ := rsa.GenerateKey(rand.Reader, 2048)
	pemKey := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)}))
	cfg.File.Runners = config.Runners{JobTTLS: 3000, PublicURL: "https://pool.example",
		GitHub: config.RunnersGitHub{AppID: 1, ClientID: "Iv1.test", APIURL: gh.srv.URL, WebURL: gh.srv.URL,
			PrivateKey: secretFile(t, keys, "key.pem", pemKey), ClientSecret: secretFile(t, keys, "cs", "client-secret"),
			WebhookSecret: secretFile(t, keys, "ws", hookKey)},
		Pool: config.RunnersPool{Project: "pool"}}
	conf(&cfg.File.Runners)
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	fix, _ := filepath.EvalSymlinks(t.TempDir())
	repo, err := refdriver.NewRepo(fix, "#!/bin/sh\nexit 0\n")
	if err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(fix, "state")
	ref, err := refdriver.Install(fix, state, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { refdriver.KillAll(state) })
	seen := filepath.Join(fix, "requests")
	tap, gate := filepath.Join(fix, "zm-run-tap"), filepath.Join(fix, "acquire-gate")
	// The driver is pinned at first use, so the gate is in the tap from the
	// start; with no gate file the request goes straight through.
	if err := os.WriteFile(tap, []byte(fmt.Sprintf(`#!/bin/sh
if [ -e %[3]q ]; then
	IFS= read -r line
	printf '%%s\n' "$line" >> %[1]q
	case "$line" in
	*'"verb":"acquire"'*)
		out=$({ printf '%%s\n' "$line"; tee -a %[1]q; } | %[2]q)
		while [ -e %[3]q ]; do sleep 0.05; done
		printf '%%s\n' "$out"
		exit ;;
	esac
	{ printf '%%s\n' "$line"; tee -a %[1]q; } | %[2]q
	exit
fi
tee -a %[1]q | exec %[2]q
`, seen, ref, gate)), 0o755); err != nil {
		t.Fatal(err)
	}
	f := &fixture{gh: gh, home: home, seen: seen, gate: gate, state: state, cfg: cfg, tap: tap, before: before}
	t.Cleanup(func() {
		if f.stop != nil {
			f.stop()
		}
	})
	f.startDaemon(t)
	ctx := context.Background()
	if _, err := f.owner.CreateProject(ctx, api.NewProjectRequest{Slug: "pool", RepoRules: api.RepoRules{Remotes: []string{repo.Remote}}}); err != nil {
		t.Fatal(err)
	}
	for _, b := range boxes {
		box := computeapi.RunTarget{Name: b.Name, Driver: "ref", Kind: "box", Isolation: "ephemeral", Images: []string{refPin},
			Config: map[string]string{"remote": repo.Remote}}
		if len(b.Images) > 0 {
			box.Images = b.Images
		}
		maps.Copy(box.Config, b.Config)
		if _, err := computeclient.BindRunTarget(ctx, f.owner, computeapi.BindRunTargetRequest{Target: box}); err != nil {
			t.Fatal(err)
		}
	}
	f.remote = repo.Remote
	f.startBroker(t)
	return f
}

// startDaemon starts the machine's daemon from f's home, the broker not yet
// started (startBroker).
func (f *fixture) startDaemon(t *testing.T) {
	t.Helper()
	dm, err := server.New(context.Background(), f.cfg, nil, wiring.Compute)
	if err != nil {
		t.Fatal(err)
	}
	f.dm, f.stop = dm, dm.Close
	for _, m := range dm.Modules() {
		switch s := m.(type) {
		case *runtarget.Service:
			s.SetLookPath(func(string) (string, error) { return f.tap, nil })
		case *runners.Service:
			f.svc = s
		}
	}
	f.owner = client.WithHandler(dm.SocketHandler())
	f.owner.Project, f.owner.ProjectFrom = "pool", "flag"
}

// startBroker sets the test's broker timings and starts it.
func (f *fixture) startBroker(t *testing.T) {
	t.Helper()
	f.svc.Retry, f.svc.OrphanAge, f.svc.WaitMax, f.svc.WatchEvery = 50*time.Millisecond, 0, 100*time.Millisecond, time.Hour // Watch: called by tests
	f.svc.RefuseCool = 300 * time.Millisecond
	if f.clock == nil {
		f.clock = &clock{}
	}
	f.svc.Now, f.svc.PollTick = f.clock.now, time.Hour
	if f.before != nil {
		f.before(f.svc)
	}
	f.gh.mu.Lock()
	f.gh.stopped = false
	f.gh.mu.Unlock()
	sctx, cancel := context.WithCancel(context.Background())
	if err := f.svc.Start(sctx); err != nil {
		t.Fatal(err)
	}
	dm, svc, gh := f.dm, f.svc, f.gh
	f.stop = func() {
		gh.mu.Lock()
		gh.stopped = true
		gh.mu.Unlock()
		cancel()
		svc.Close()
		dm.Close()
	}
}

// restart stops the machine's daemon and starts it again from the same home
// (its store, its config): the broker's memory is gone, what it stored is not.
// edit, when given, changes the config while no daemon runs.
func (f *fixture) restart(t *testing.T, edit ...func(*config.Config)) {
	t.Helper()
	f.stop()
	f.stop = nil
	for _, e := range edit {
		e(f.cfg)
	}
	f.startDaemon(t)
	f.startBroker(t)
}

// public calls a public route as GitHub or a browser would (the network door).
func (f *fixture) public(t *testing.T, method, path string, body []byte, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(string(body)))
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	f.dm.Handler.ServeHTTP(w, req)
	return w
}

func sign(body []byte) string {
	m := hmac.New(sha256.New, []byte(hookKey))
	m.Write(body)
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

var delivery = 0

func (f *fixture) deliver(t *testing.T, event string, body []byte) int {
	t.Helper()
	delivery++
	return f.public(t, "POST", "/v0/runners/hook", body, map[string]string{"X-Hub-Signature-256": sign(body),
		"X-GitHub-Event": event, "X-GitHub-Delivery": fmt.Sprintf("d-%d", delivery), "Content-Type": "application/json"}).Code
}

// repoID is a repository's id as GitHub names it, unless the fake's repoIDs
// says another: one per name, whatever its case.
func repoID(repo string) int64 {
	h := fnv.New32a()
	h.Write([]byte(strings.ToLower(repo)))
	return int64(h.Sum32()) + 1
}

func jobEvent(action string, job, run int64, repo string, private bool, labels ...string) []byte {
	return jobEventOf(action, job, run, repo, repoID(repo), private, labels...)
}

// jobEventOf is jobEvent of the repository with id.
func jobEventOf(action string, job, run int64, repo string, id int64, private bool, labels ...string) []byte {
	b, _ := json.Marshal(map[string]any{"action": action, "installation": map[string]any{"id": inst},
		"repository":   map[string]any{"id": id, "full_name": repo, "private": private},
		"workflow_job": map[string]any{"id": job, "run_id": run, "labels": labels}})
	return b
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	waitFor(t, what, time.Now().Add(10*time.Second), ok)
}

// waitFor polls ok every 50 ms until it holds, failing the test at end.
func waitFor(t *testing.T, what string, end time.Time, ok func() bool) {
	t.Helper()
	for ; !ok(); time.Sleep(50 * time.Millisecond) {
		if time.Now().After(end) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

// boxWait is how long eventuallyBox waits at most: a box's start and release
// are several driver processes, which a loaded machine (or a -race test
// binary, which the reference driver runs again for each call and which takes
// a second to start on macOS) stretches from tenths of a second to many seconds.
const boxWait = 2 * time.Minute

// eventuallyBox is eventually for a step that waits on box starts or releases,
// ending at the test's own deadline (less a margin to clean up) or boxWait,
// whichever comes first. The condition is the same one eventually would check;
// only the time its slowest honest run may take differs.
func eventuallyBox(t *testing.T, what string, ok func() bool) {
	t.Helper()
	end := time.Now().Add(boxWait)
	if d, set := t.Deadline(); set && d.Add(-10*time.Second).Before(end) {
		end = d.Add(-10 * time.Second)
	}
	waitFor(t, what, end, ok)
}

// link runs the link flow: start as the owner, then GitHub's return.
func (f *fixture) link(t *testing.T, installation int64, code string) *httptest.ResponseRecorder {
	t.Helper()
	var out struct {
		URL string `json:"authorize_url"`
	}
	if err := f.owner.Do(context.Background(), "POST", "/v0/runners/links", map[string]any{"installation_id": installation}, &out); err != nil {
		t.Fatalf("link start: %v", err)
	}
	u, _ := url.Parse(out.URL)
	if u.Query().Get("redirect_uri") != "https://pool.example/v0/runners/oauth" || u.Query().Get("client_id") != "Iv1.test" {
		t.Fatalf("authorize url %s", out.URL)
	}
	w := f.linkAs(t, out.URL, code)
	if w.Code == http.StatusOK && installation == inst {
		// The private repositories the tests' jobs come from are connected.
		for _, repo := range []string{"acme/app", "acme/lib", "acme/other"} {
			if _, err := f.allow(repo, false); err != nil {
				t.Fatalf("allow %s: %v", repo, err)
			}
		}
	}
	return w
}

// linkAs is GitHub's return to the authorize URL, then the person's yes on
// the confirmation page.
func (f *fixture) linkAs(t *testing.T, authorize, code string) *httptest.ResponseRecorder {
	t.Helper()
	u, _ := url.Parse(authorize)
	w := f.public(t, "GET", "/v0/runners/oauth?state="+u.Query().Get("state")+"&code="+code, nil, nil)
	if w.Code != http.StatusOK {
		return w
	}
	m := regexp.MustCompile(`name="state" value="([0-9a-f]{48})"`).FindStringSubmatch(w.Body.String())
	if m == nil || !strings.Contains(w.Body.String(), "human:judy asked") {
		t.Fatalf("confirmation page: %s", w.Body)
	}
	return f.public(t, "POST", "/v0/runners/oauth", []byte("state="+m[1]), map[string]string{"Content-Type": "application/x-www-form-urlencoded"})
}

func TestBroker(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	// Linking proves the person can see the installation, and is single-use.
	f.gh.mu.Lock()
	f.gh.userSee = []int64{8}
	f.gh.mu.Unlock()
	if w := f.link(t, inst, "good"); w.Code != http.StatusForbidden {
		t.Fatalf("link of an installation the person cannot see: %d %s", w.Code, w.Body)
	}
	f.gh.mu.Lock()
	f.gh.userSee = []int64{inst}
	f.gh.mu.Unlock()
	if w := f.link(t, inst, "bad"); w.Code != http.StatusBadGateway {
		t.Fatalf("link with a bad code: %d", w.Code)
	}
	// Seeing an org's installation is not enough: an admin of the org links it.
	f.gh.mu.Lock()
	f.gh.orgRole = "member"
	f.gh.mu.Unlock()
	if w := f.link(t, inst, "good"); w.Code != http.StatusForbidden {
		t.Fatalf("link by an org member who is not an admin: %d %s", w.Code, w.Body)
	}
	f.gh.mu.Lock()
	f.gh.orgRole = "none"
	f.gh.mu.Unlock()
	if w := f.link(t, inst, "good"); w.Code != http.StatusForbidden {
		t.Fatalf("link by someone outside the org: %d %s", w.Code, w.Body)
	}
	// An App without Members read cannot prove an admin: the page says so,
	// never "not an admin".
	f.gh.mu.Lock()
	f.gh.orgRole = "no-members-read"
	f.gh.mu.Unlock()
	if w := f.link(t, inst, "good"); w.Code != http.StatusBadGateway ||
		!strings.Contains(w.Body.String(), "Members: read") || strings.Contains(w.Body.String(), "is not one") {
		t.Fatalf("link through an App without Members read: %d %s", w.Code, w.Body)
	}
	f.gh.mu.Lock()
	f.gh.orgRole, f.gh.userSee = "admin", []int64{inst, 11, 12}
	f.gh.mu.Unlock()
	// A user account's installation: only that user links it.
	if w := f.link(t, 12, "good"); w.Code != http.StatusForbidden {
		t.Fatalf("link of another user's installation: %d %s", w.Code, w.Body)
	}
	if w := f.link(t, 11, "good"); w.Code != http.StatusOK {
		t.Fatalf("link of the person's own installation: %d %s", w.Code, w.Body)
	}
	if err := f.owner.Do(ctx, "DELETE", "/v0/runners/links/11", nil, nil); err != nil {
		t.Fatal(err)
	}
	// The confirmation's state is single-use and its own kind.
	var start struct {
		URL string `json:"authorize_url"`
	}
	if err := f.owner.Do(ctx, "POST", "/v0/runners/links", map[string]any{"installation_id": inst}, &start); err != nil {
		t.Fatal(err)
	}
	su, _ := url.Parse(start.URL)
	form := map[string]string{"Content-Type": "application/x-www-form-urlencoded"}
	for _, st := range []string{su.Query().Get("state"), strings.Repeat("a", 48), "short"} {
		if w := f.public(t, "POST", "/v0/runners/oauth", []byte("state="+st), form); w.Code != http.StatusNotFound {
			t.Errorf("a confirm with state %.8q (the sign-in's, made up, malformed): %d, want 404", st, w.Code)
		}
	}
	w0 := f.public(t, "GET", "/v0/runners/oauth?state="+su.Query().Get("state")+"&code=good", nil, nil)
	cm := regexp.MustCompile(`name="state" value="([0-9a-f]{48})"`).FindStringSubmatch(w0.Body.String())
	if cm == nil {
		t.Fatalf("no confirmation: %s", w0.Body)
	}
	if w := f.public(t, "POST", "/v0/runners/oauth", []byte("state="+cm[1]), form); w.Code != http.StatusOK {
		t.Fatalf("confirm: %d %s", w.Code, w.Body)
	}
	if w := f.public(t, "POST", "/v0/runners/oauth", []byte("state="+cm[1]), form); w.Code != http.StatusNotFound {
		t.Errorf("the same confirmation twice: %d, want 404", w.Code)
	}
	w := f.link(t, inst, "good")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "acme") {
		t.Fatalf("link: %d %s", w.Code, w.Body)
	}
	var links struct {
		Links []runners.Link `json:"links"`
	}
	if err := f.owner.Do(ctx, "GET", "/v0/runners/links", nil, &links); err != nil || len(links.Links) != 1 || links.Links[0].Account != "acme" {
		t.Fatalf("links: %+v %v", links, err)
	}
	if err := f.owner.Do(ctx, "POST", "/v0/runners/links", map[string]any{"installation_id": 9}, nil); !api.IsCode(err, api.CodeNotFound) {
		t.Errorf("linking another App's installation: %v, want not_found", err)
	}

	// An admin of another area cannot move the installation there.
	if _, err := f.owner.AddArea(ctx, "pool", api.AddAreaRequest{Name: "b"}); err != nil {
		t.Fatal(err)
	}
	tok, err := f.owner.MintToken(ctx, api.MintTokenRequest{As: "bo", Project: "pool", Area: "b", Role: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	bo := client.WithHandler(f.dm.Handler)
	bo.Token, bo.Project, bo.ProjectFrom = tok.Secret, "pool", "flag"
	if err := bo.Do(ctx, "POST", "/v0/runners/links", map[string]any{"installation_id": inst, "area": "b"}, nil); !api.IsCode(err, api.CodeForbidden) {
		t.Errorf("an area-b admin re-linking area root's installation: %v, want forbidden", err)
	}

	// A bad signature is refused before anything else.
	body := jobEvent("queued", 1001, 55, "acme/app", true, "self-hosted", "sengi")
	if code := f.public(t, "POST", "/v0/runners/hook", body, map[string]string{"X-Hub-Signature-256": "sha256=" + strings.Repeat("0", 64),
		"X-GitHub-Event": "workflow_job", "X-GitHub-Delivery": "x"}).Code; code != http.StatusUnauthorized {
		t.Errorf("a bad signature: %d, want 401", code)
	}

	// A queued job for the pool: a runner minted, a box acquired with its config.
	if code := f.deliver(t, "workflow_job", body); code != http.StatusAccepted {
		t.Fatalf("delivery: %d", code)
	}
	eventually(t, "the box", func() bool {
		b, _ := os.ReadFile(f.seen)
		return strings.Contains(string(b), `"jit_config":"`+jitValue+`"`) && strings.Contains(string(b), `"verb":"acquire"`)
	})
	minted, _ := f.gh.snapshot()
	if len(minted) != 1 || minted[0]["name"] != "potluck-1001" || fmt.Sprint(minted[0]["labels"]) != "[self-hosted sengi]" ||
		minted[0]["scope"] != "repos/acme/app" {
		t.Fatalf("minted %v", minted)
	}
	eventually(t, "the box ready", func() bool {
		hs, err := computeclient.RunTargetHandles(ctx, f.owner, "small", false)
		return err == nil && len(hs) == 1 && hs[0].State == "ready" && hs[0].Actor == "human:"+runners.Identity
	})
	// A redelivery (and a second delivery of the same job) makes nothing new.
	f.deliver(t, "workflow_job", body)
	// Jobs not served: a fork's run, a public repo, another runner's labels, an unlinked installation.
	f.gh.mu.Lock()
	f.gh.forks[56] = true
	f.gh.mu.Unlock()
	f.deliver(t, "workflow_job", jobEvent("queued", 1002, 56, "acme/app", true, "self-hosted", "sengi"))
	f.deliver(t, "workflow_job", jobEvent("queued", 1003, 57, "acme/site", false, "self-hosted", "sengi"))
	f.deliver(t, "workflow_job", jobEvent("queued", 1004, 58, "acme/app", true, "self-hosted", "gpu"))
	f.deliver(t, "workflow_job", jobEvent("queued", 1005, 59, "acme/app", true, "self-hosted", "sengi:huge"))
	f.gh.mu.Lock()
	f.gh.events[61], f.gh.events[62], f.gh.events[65] = "pull_request_target", "workflow_run", "issue_comment"
	f.gh.runRepo[63] = "acme/other"
	f.gh.jobDone[1010] = true
	f.gh.mu.Unlock()
	f.deliver(t, "workflow_job", jobEvent("queued", 1007, 61, "acme/app", true, "self-hosted", "sengi"))
	f.deliver(t, "workflow_job", jobEvent("queued", 1008, 62, "acme/app", true, "self-hosted", "sengi"))
	f.deliver(t, "workflow_job", jobEvent("queued", 1009, 63, "acme/app", true, "self-hosted", "sengi"))
	f.deliver(t, "workflow_job", jobEvent("queued", 1010, 64, "acme/app", true, "self-hosted", "sengi"))
	f.deliver(t, "workflow_job", jobEvent("queued", 1011, 65, "acme/app", true, "self-hosted", "sengi"))
	time.Sleep(500 * time.Millisecond)
	if minted, _ := f.gh.snapshot(); len(minted) != 1 {
		t.Fatalf("minted %d runners, want 1 (a redelivery, a fork, a public repo, other labels, an unknown size, pull_request_target, workflow_run, another repo's run and a finished job serve nothing)", len(minted))
	}

	// Completed: the box released, the runner deleted.
	f.deliver(t, "workflow_job", jobEvent("completed", 1001, 55, "acme/app", true, "self-hosted", "sengi"))
	eventually(t, "the release", func() bool {
		hs, _ := computeclient.RunTargetHandles(ctx, f.owner, "small", false)
		_, del := f.gh.snapshot()
		return len(hs) == 1 && hs[0].State == "released" && len(del) == 1 && del[0] == "potluck-1001"
	})

	// The orphan sweep deletes only runners our records show launch minted
	// in this repository: a step 1 build's zm-<job>-<k> (H8) and an old
	// potluck-<job>; never one named like ours with no record here (someone
	// else's), nor anything while the records cannot be read.
	for _, j := range []struct {
		job          int64
		repo, runner string
		mints        int
	}{{999, "acme/app", "zm-999-2", 2}, {996, "acme/app", "potluck-996", 1}, {995, "acme/other", "potluck-995", 1}} {
		if err := f.svc.RecordJob(ctx, j.job, inst, j.repo, j.runner, j.mints); err != nil {
			t.Fatal(err)
		}
	}
	f.gh.mu.Lock()
	f.gh.runners["zm-999-2"] = fakeRunner{999, "repos/acme/app", nil}      // ours, from a step 1 build
	f.gh.runners["potluck-996"] = fakeRunner{996, "repos/acme/app", nil}   // ours, job ended long ago
	f.gh.runners["zm-build"] = fakeRunner{998, "repos/acme/app", nil}      // someone else's, named like ours
	f.gh.runners["potluck-build"] = fakeRunner{997, "repos/acme/app", nil} // the same under the new prefix
	f.gh.runners["potluck-123"] = fakeRunner{994, "repos/acme/app", nil}   // matches ourRunner, no record: someone else's
	f.gh.runners["zm-999-3"] = fakeRunner{993, "repos/acme/app", nil}      // a mint our record never made (mints 2)
	f.gh.runners["potluck-995"] = fakeRunner{992, "repos/acme/app", nil}   // our record is for another repository
	f.gh.mu.Unlock()
	if err := f.svc.BreakJobsLookup(ctx, true); err != nil {
		t.Fatal(err)
	}
	f.svc.Sweep(ctx)
	if _, del := f.gh.snapshot(); len(del) != 1 {
		t.Errorf("a sweep that could not read its records deleted %v", del)
	}
	if err := f.svc.BreakJobsLookup(ctx, false); err != nil {
		t.Fatal(err)
	}
	f.svc.Sweep(ctx)
	_, del := f.gh.snapshot()
	slices.Sort(del)
	if !slices.Equal(del, []string{"potluck-1001", "potluck-996", "zm-999-2"}) {
		t.Errorf("after the sweep deleted %v, want zm-999-2 and potluck-996 too, nothing else", del)
	}

	// Nothing kept a secret: no jit_config, token or webhook secret in zm's home.
	_ = filepath.WalkDir(f.home, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			b, _ := os.ReadFile(p)
			for _, s := range []string{jitValue, "user-token", "inst-token", hookKey, "client-secret"} {
				if strings.Contains(string(b), s) {
					t.Errorf("%s holds %q", p, s)
				}
			}
		}
		return nil
	})

	// Unlinking revokes the link; its jobs are no longer served.
	if err := f.owner.Do(ctx, "DELETE", fmt.Sprintf("/v0/runners/links/%d", inst), nil, nil); err != nil {
		t.Fatal(err)
	}
	f.deliver(t, "workflow_job", jobEvent("queued", 1006, 60, "acme/app", true, "self-hosted", "sengi"))
	time.Sleep(300 * time.Millisecond)
	if minted, _ := f.gh.snapshot(); len(minted) != 1 {
		t.Errorf("an unlinked installation's job minted a runner")
	}
}

// A job the broker refused and broker runners. The runner registers on the
// served job's repository, never the org (whose Default group every repo of
// the org may use), so a refused job of another repo is never given it.
// GitHub does give it to any job of that repository with its labels, so a
// refused one there can take it: the box's hook fails it before any step
// (TestTakenRunnerIsServedAgain), and in_progress releases its box at once; a
// job the broker would serve taking it is a swap.
func TestRefusedJobsAndBrokerRunners(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if w := f.link(t, inst, "good"); w.Code != http.StatusOK {
		t.Fatalf("link: %d %s", w.Code, w.Body)
	}
	deliver := func(body []byte) {
		t.Helper()
		if code := f.deliver(t, "workflow_job", body); code != http.StatusAccepted {
			t.Fatalf("delivery: %d", code)
		}
	}
	applied := func() { eventually(t, "the deliveries applied", func() bool { return f.svc.Undone(ctx) == 0 }) }
	labels := []string{"self-hosted", "sengi"}
	f.gh.mu.Lock()
	f.gh.forks[81], f.gh.events[82] = true, "pull_request_target"
	f.gh.mu.Unlock()
	deliver(jobEvent("queued", 3001, 80, "acme/app", true, labels...))
	deliver(jobEvent("queued", 3005, 85, "acme/app", true, labels...))
	ready := func(want int) func() bool {
		return func() bool {
			hs, err := computeclient.RunTargetHandles(ctx, f.owner, "small", false)
			n := 0
			for _, h := range hs {
				if h.State == "ready" {
					n++
				}
			}
			return err == nil && n == want
		}
	}
	eventually(t, "two boxes ready", ready(2))
	// Refused, with the same labels: a fork's run and a pull_request_target in
	// the repo, a public repo of the org.
	deliver(jobEvent("queued", 3002, 81, "acme/app", true, labels...))
	deliver(jobEvent("queued", 3003, 82, "acme/app", true, labels...))
	deliver(jobEvent("queued", 3004, 83, "acme/site", false, labels...))
	applied()
	minted, _ := f.gh.snapshot()
	if len(minted) != 2 {
		t.Fatalf("minted %d runners, want 2", len(minted))
	}
	for _, m := range minted {
		if m["scope"] != "repos/acme/app" {
			t.Errorf("runner %v registered on %v, want the job's repository", m["name"], m["scope"])
		}
	}
	if got := f.gh.eligible("acme/site", labels); len(got) != 0 {
		t.Errorf("GitHub may give the public repo's refused job runners %v", got)
	}
	// Jobs the broker would serve taking another's runner: a served one, and
	// one it has no record of yet (its queued delivery still to be retried).
	deliver(startEvent(3005, 85, "acme/app", "potluck-3001"))
	deliver(startEvent(3006, 86, "acme/app", "potluck-3001"))
	// A runner not ours (another repo's zm-<digits> too), or a job on its own runner.
	deliver(startEvent(3003, 82, "acme/app", "zm-build"))
	deliver(startEvent(3007, 87, "acme/other", "potluck-3005"))
	deliver(startEvent(3001, 80, "acme/app", "potluck-3001"))
	applied()
	if _, del := f.gh.snapshot(); !ready(2)() || len(del) != 0 {
		t.Fatalf("a swap, a foreign runner or a job on its own runner deleted %v or released a box", del)
	}
	// The repo's refused jobs may be given a runner (GitHub's rule); one that
	// takes it loses the box.
	if got := f.gh.eligible("acme/app", labels); len(got) != 2 {
		t.Fatalf("eligible in acme/app %v", got)
	}
	deliver(startEvent(3002, 81, "acme/app", "potluck-3005"))
	eventually(t, "the taken runner's box released and the runner deleted", func() bool {
		_, del := f.gh.snapshot()
		return ready(1)() && slices.Equal(del, []string{"potluck-3005"})
	})
	// The broker stops: its cached installation tokens are revoked, not left
	// to lapse within the hour.
	f.svc.Close()
	f.gh.mu.Lock()
	left, revoked := len(f.gh.tokens), len(f.gh.revoked)
	f.gh.mu.Unlock()
	if left != 0 || revoked == 0 {
		t.Errorf("after Close %d installation tokens live at GitHub, %d revoked", left, revoked)
	}
}

// A refused job of the served job's repository that takes its runner: the
// box is guarded by the broker's rules (jit_guard, which the box's
// job-started hook checks before any step; vmhost tests the hook), the box is released,
// and the job the runner was minted for is served again while GitHub still
// has it waiting, at most three runners in all.
func TestTakenRunnerIsServedAgain(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if w := f.link(t, inst, "good"); w.Code != http.StatusOK {
		t.Fatalf("link: %d %s", w.Code, w.Body)
	}
	labels := []string{"self-hosted", "sengi"}
	ready := func(want int) func() bool {
		return func() bool {
			hs, err := computeclient.RunTargetHandles(ctx, f.owner, "small", false)
			n := 0
			for _, h := range hs {
				if h.State == "ready" {
					n++
				}
			}
			return err == nil && n == want
		}
	}
	names := func() (ns []string) {
		minted, _ := f.gh.snapshot()
		for _, m := range minted {
			ns = append(ns, fmt.Sprint(m["name"]))
		}
		return ns
	}
	f.gh.mu.Lock()
	f.gh.events[91], f.gh.forks[92], f.gh.forks[93] = "pull_request_target", true, true
	f.gh.status[4001] = "queued" // waiting for a runner, as GitHub has it once its own was taken
	f.gh.mu.Unlock()
	f.deliver(t, "workflow_job", jobEvent("queued", 4001, 90, "acme/app", true, labels...))
	eventuallyBox(t, "the box", ready(1))
	guard := `"jit_guard":{"repo":"acme/app","events":["push","pull_request","workflow_dispatch","schedule","merge_group","release","repository_dispatch"]}`
	if b, _ := os.ReadFile(f.seen); !strings.Contains(string(b), guard) {
		t.Fatalf("the box was not guarded: %s", b)
	}
	f.deliver(t, "workflow_job", jobEvent("queued", 4002, 91, "acme/app", true, labels...))
	eventually(t, "the refusal", func() bool { return f.svc.Undone(ctx) == 0 })
	// The pull_request_target job takes the runner: its box goes, and job 4001
	// gets a new runner and box, guarded the same way.
	f.deliver(t, "workflow_job", startEvent(4002, 91, "acme/app", "potluck-4001"))
	eventuallyBox(t, "job 4001 served again", func() bool {
		_, del := f.gh.snapshot()
		return slices.Equal(names(), []string{"potluck-4001", "potluck-4001-2"}) && slices.Equal(del, []string{"potluck-4001"}) && ready(1)()
	})
	b, _ := os.ReadFile(f.seen)
	if !strings.Contains(string(b), `gh-4001-2"`) || strings.Count(string(b), guard) < 2 {
		t.Errorf("the second box: %s", b)
	}
	// Taken again (a fork's run), and again: three runners in all, then no more.
	f.deliver(t, "workflow_job", startEvent(4003, 92, "acme/app", "potluck-4001-2"))
	eventuallyBox(t, "a third runner", func() bool {
		return slices.Equal(names(), []string{"potluck-4001", "potluck-4001-2", "potluck-4001-3"}) && ready(1)()
	})
	f.deliver(t, "workflow_job", startEvent(4004, 93, "acme/app", "potluck-4001-3"))
	eventuallyBox(t, "the last box released", func() bool { _, del := f.gh.snapshot(); return len(del) == 3 && ready(0)() && f.svc.Undone(ctx) == 0 })
	if got := names(); len(got) != 3 {
		t.Errorf("minted %v, want three runners at most", got)
	}
	// A late in_progress for a runner already replaced does nothing.
	f.deliver(t, "workflow_job", startEvent(4002, 91, "acme/app", "potluck-4001"))
	eventually(t, "applied", func() bool { return f.svc.Undone(ctx) == 0 })
	if got := names(); len(got) != 3 {
		t.Errorf("a late in_progress minted %v", got)
	}
}

// A trusted job of another run (a push while a pull request runs) taking a
// runner passes the box's hook and is a swap: nothing is released, and the
// runner's own job takes the other's runner.
func TestAnotherTrustedRunTakingARunnerIsASwap(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if w := f.link(t, inst, "good"); w.Code != http.StatusOK {
		t.Fatalf("link: %d %s", w.Code, w.Body)
	}
	labels := []string{"self-hosted", "sengi"}
	f.gh.mu.Lock()
	f.gh.events[111] = "push"
	f.gh.mu.Unlock()
	f.deliver(t, "workflow_job", jobEvent("queued", 6001, 110, "acme/app", true, labels...))
	f.deliver(t, "workflow_job", jobEvent("queued", 6002, 111, "acme/app", true, labels...))
	eventually(t, "both served", func() bool { m, _ := f.gh.snapshot(); return len(m) == 2 && f.svc.Undone(ctx) == 0 })
	if b, _ := os.ReadFile(f.seen); strings.Count(string(b), `"jit_guard":{"repo":"acme/app","events":["push","pull_request","workflow_dispatch","schedule","merge_group","release","repository_dispatch"]}`) != 2 {
		t.Fatalf("the boxes were not guarded by the broker's rules: %s", b)
	}
	f.deliver(t, "workflow_job", startEvent(6002, 111, "acme/app", "potluck-6001"))
	f.deliver(t, "workflow_job", startEvent(6001, 110, "acme/app", "potluck-6002"))
	eventually(t, "applied", func() bool { return f.svc.Undone(ctx) == 0 })
	time.Sleep(300 * time.Millisecond)
	minted, del := f.gh.snapshot()
	hs, _ := computeclient.RunTargetHandles(ctx, f.owner, "small", false)
	ready := 0
	for _, h := range hs {
		if h.State == "ready" {
			ready++
		}
	}
	if len(minted) != 2 || len(del) != 0 || ready != 2 {
		t.Errorf("a swap between two trusted runs minted %d, deleted %v, left %d boxes; want 2, none, 2", len(minted), del, ready)
	}
}

// completed is applied in its own lane, so it can land while a serve of the
// same job is still checking it or starting its box: the serve then mints
// nothing, or releases the box and deletes the runner it just minted.
func TestCompletedDuringAServe(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if w := f.link(t, inst, "good"); w.Code != http.StatusOK {
		t.Fatalf("link: %d %s", w.Code, w.Body)
	}
	labels := []string{"self-hosted", "sengi"}
	ready := func() int {
		hs, _ := computeclient.RunTargetHandles(ctx, f.owner, "small", false)
		n := 0
		for _, h := range hs {
			if h.State == "ready" {
				n++
			}
		}
		return n
	}
	hold := func(on string) chan struct{} {
		gate := make(chan struct{})
		f.gh.mu.Lock()
		f.gh.gate, f.gh.gateOn, f.gh.waiting = gate, on, 0
		f.gh.mu.Unlock()
		return gate
	}
	open := func(gate chan struct{}) {
		f.gh.mu.Lock()
		f.gh.gate = nil
		f.gh.mu.Unlock()
		close(gate)
	}
	held := func() bool { f.gh.mu.Lock(); defer f.gh.mu.Unlock(); return f.gh.waiting == 1 }
	// Completed after GitHub said the job was waiting, before it was recorded
	// as serving (here: while its run is checked): nothing is minted.
	gate := hold("/actions/runs/")
	f.deliver(t, "workflow_job", jobEvent("queued", 7001, 120, "acme/app", true, labels...))
	eventually(t, "the serve at the run check", held)
	f.deliver(t, "workflow_job", jobEvent("completed", 7001, 120, "acme/app", true, labels...))
	eventually(t, "completed applied", func() bool { return f.svc.Undone(ctx) == 1 })
	open(gate)
	eventually(t, "the serve done", func() bool { return f.svc.Undone(ctx) == 0 })
	if minted, _ := f.gh.snapshot(); len(minted) != 0 {
		t.Fatalf("a job completed during its serve got runners %v", minted)
	}
	// Completed while its runner is minted: the box goes and so does the runner.
	gate = hold("/generate-jitconfig")
	f.deliver(t, "workflow_job", jobEvent("queued", 7002, 121, "acme/app", true, labels...))
	eventually(t, "the serve at the mint", held)
	f.deliver(t, "workflow_job", jobEvent("completed", 7002, 121, "acme/app", true, labels...))
	eventually(t, "completed applied", func() bool { return f.svc.Undone(ctx) == 1 })
	open(gate)
	eventually(t, "the box released and the runner deleted", func() bool {
		minted, del := f.gh.snapshot()
		return f.svc.Undone(ctx) == 0 && len(minted) == 1 && slices.Equal(del, []string{"potluck-7002"}) && ready() == 0
	})
}

// in_progress and completed release boxes: they never wait behind a serve
// stuck starting a box (here a slow mint), so a refused job's takeover ends
// at once.
func TestEndsAreNotHeldBehindASlowServe(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if w := f.link(t, inst, "good"); w.Code != http.StatusOK {
		t.Fatalf("link: %d %s", w.Code, w.Body)
	}
	labels := []string{"self-hosted", "sengi"}
	ready := func() int {
		hs, _ := computeclient.RunTargetHandles(ctx, f.owner, "small", false)
		n := 0
		for _, h := range hs {
			if h.State == "ready" {
				n++
			}
		}
		return n
	}
	f.gh.mu.Lock()
	f.gh.events[102] = "pull_request_target"
	f.gh.mu.Unlock()
	f.deliver(t, "workflow_job", jobEvent("queued", 5001, 100, "acme/app", true, labels...))
	eventually(t, "the box", func() bool { return ready() == 1 })
	gate := make(chan struct{})
	f.gh.mu.Lock()
	f.gh.gate = gate
	f.gh.mu.Unlock()
	f.deliver(t, "workflow_job", jobEvent("queued", 5002, 101, "acme/app", true, labels...))
	eventually(t, "the second serve stuck", func() bool { f.gh.mu.Lock(); defer f.gh.mu.Unlock(); return f.gh.waiting == 1 })
	f.deliver(t, "workflow_job", startEvent(5003, 102, "acme/app", "potluck-5001"))
	eventually(t, "the taken box released while the serve waits", func() bool {
		_, del := f.gh.snapshot()
		return ready() == 0 && slices.Equal(del, []string{"potluck-5001"})
	})
	f.gh.mu.Lock()
	f.gh.gate = nil
	f.gh.mu.Unlock()
	close(gate)
	eventually(t, "the second box", func() bool { return ready() == 1 })
}

// The fake GitHub ties each installation token to its repository and
// permissions: a token used on another repository, or for a call its
// permissions do not cover, is refused (so a broker mixing them fails).
func TestFakeGitHubTiesTokensToRepos(t *testing.T) {
	g := newFakeGitHub(t)
	mint := func(repo string, perms map[string]string) string {
		b, _ := json.Marshal(map[string]any{"repositories": []string{repo}, "permissions": perms})
		resp, err := http.Post(g.srv.URL+fmt.Sprintf("/app/installations/%d/access_tokens", inst), "application/json", strings.NewReader(string(b)))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out struct{ Token string }
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return out.Token
	}
	get := func(tok, path string) int {
		req, _ := http.NewRequest("GET", g.srv.URL+path, nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	read := mint("app", map[string]string{"actions": "read", "metadata": "read"})
	admin := mint("app", map[string]string{"administration": "write", "metadata": "read"})
	for _, c := range []struct {
		tok, path string
		want      int
	}{
		{read, "/repos/acme/app/actions/runs/1", 200},
		{read, "/repos/acme/site/actions/runs/1", 403}, // another repository
		{read, "/repos/acme/app/actions/runners", 403}, // runners need administration
		{admin, "/repos/acme/app/actions/runners", 200},
		{admin, "/repos/acme/site/actions/runners", 403},
		{admin, "/repos/acme/app/actions/jobs/1", 403},
		{"made-up", "/repos/acme/app/actions/jobs/1", 403},
	} {
		if got := get(c.tok, c.path); got != c.want {
			t.Errorf("GET %s with the %s token: %d, want %d", c.path, map[string]string{read: "read", admin: "admin"}[c.tok], got, c.want)
		}
	}
	g.mu.Lock()
	if len(g.misused) != 5 {
		t.Errorf("recorded %d misuses, want 5: %v", len(g.misused), g.misused)
	}
	g.misused = nil // refused on purpose here
	g.mu.Unlock()
}

func startEvent(job, run int64, repo, runner string) []byte {
	b, _ := json.Marshal(map[string]any{"action": "in_progress", "installation": map[string]any{"id": inst},
		"repository":   map[string]any{"id": repoID(repo), "full_name": repo, "private": true},
		"workflow_job": map[string]any{"id": job, "run_id": run, "labels": []string{"self-hosted", "sengi"}, "runner_name": runner}})
	return b
}

// The App is created from its manifest; GitHub's return hands its secrets to
// the authority directly, once.
func TestAppFromManifest(t *testing.T) {
	f := newFixture(t)
	var out struct{ URL string }
	if err := f.owner.Do(context.Background(), "POST", "/v0/runners/app", map[string]any{"org": "acme"}, &out); err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(out.URL)
	state := u.Query().Get("state")
	if !strings.HasPrefix(out.URL, "https://pool.example/v0/runners/app/new?state=") {
		t.Fatalf("app url %s", out.URL)
	}
	w := f.public(t, "GET", "/v0/runners/app/new?state="+state, nil, nil)
	b := w.Body.String()
	if w.Code != 200 || !strings.Contains(b, "/organizations/acme/settings/apps/new?state="+state) ||
		!strings.Contains(b, "https://pool.example/v0/runners/hook") || !strings.Contains(b, "workflow_job") {
		t.Fatalf("manifest page: %d %s", w.Code, b)
	}
	if w := f.public(t, "GET", "/v0/runners/app/created?state="+state+"&code=mcode", nil, nil); w.Code != 200 || !strings.Contains(w.Body.String(), "z3-compute") {
		t.Fatalf("created: %d %s", w.Code, w.Body)
	}
	if w := f.public(t, "GET", "/v0/runners/app/created?state="+state+"&code=mcode", nil, nil); w.Code != http.StatusNotFound {
		t.Errorf("the same state twice: %d, want 404", w.Code)
	}
	raw, err := os.ReadFile(filepath.Join(f.home, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cf struct {
		Runners config.Runners `json:"runners"`
	}
	_ = json.Unmarshal(raw, &cf)
	gh := cf.Runners.GitHub
	if gh.AppID != 99 || gh.ClientID != "Iv1.new" || !strings.HasPrefix(gh.WebhookSecret, "file:") || strings.Contains(string(raw), "new-webhook-secret") {
		t.Fatalf("config.json runners.github %+v", gh)
	}
	for _, ref := range []string{gh.PrivateKey, gh.ClientSecret, gh.WebhookSecret} {
		fi, err := os.Stat(strings.TrimPrefix(ref, "file:"))
		if err != nil || fi.Mode().Perm() != 0o600 {
			t.Errorf("%s: %v %v, want a 0600 file", ref, fi, err)
		}
	}
	// Not an owner: no page.
	guest := client.WithHandler(f.dm.Handler)
	if err := guest.Do(context.Background(), "POST", "/v0/runners/app", map[string]any{}, nil); err == nil {
		t.Error("an unauthenticated caller got an App page")
	}
	_ = io.Discard
}

// The public listener serves the broker's public routes and nothing else of
// the daemon.
func TestPublicHandlerServesOnlyThePublicRoutes(t *testing.T) {
	f := newFixture(t)
	srv := httptest.NewServer(f.svc.PublicHandler())
	defer srv.Close()
	for _, c := range []struct {
		method, path string
		want         int
	}{
		{"POST", "/v0/runners/hook", http.StatusUnauthorized}, // reached: no signature
		{"GET", "/v0/runners/oauth?state=x", http.StatusNotFound},
		{"POST", "/v0/runners/oauth", http.StatusNotFound},
		{"GET", "/v0/runners/app/new?state=x", http.StatusNotFound},
		{"GET", "/v0/runners/app/created?state=x", http.StatusNotFound},
		{"GET", "/v0/runners/app/installed?installation_id=7&state=x", http.StatusNotFound},
		{"GET", "/v0/runners/links", http.StatusNotFound},  // an authed route: not here
		{"POST", "/v0/runners/setup", http.StatusNotFound}, // a local route: never here
		{"GET", "/v0/runners/setup", http.StatusNotFound},
		{"POST", "/v0/runners/settings", http.StatusNotFound},
		{"POST", "/v0/runners/app", http.StatusNotFound},
		{"GET", "/v0/health", http.StatusNotFound},
		{"GET", "/v0/projects", http.StatusNotFound},
		{"POST", "/v0/run_targets/small/acquire", http.StatusNotFound},
		{"HEAD", "/v0/runners/oauth?state=x", http.StatusMethodNotAllowed}, // a preview never spends a state
		{"HEAD", "/v0/runners/app/created?state=x", http.StatusMethodNotAllowed},
		{"HEAD", "/v0/runners/app/installed?installation_id=7&state=x", http.StatusMethodNotAllowed},
	} {
		req, _ := http.NewRequest(c.method, srv.URL+c.path, strings.NewReader("{}"))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != c.want {
			t.Errorf("%s %s on the public listener: %d, want %d", c.method, c.path, resp.StatusCode, c.want)
		}
	}
	// A signed delivery is taken there like on the daemon's own listener.
	body := jobEvent("queued", 2001, 70, "acme/app", true, "self-hosted", "gpu")
	req, _ := http.NewRequest("POST", srv.URL+"/v0/runners/hook", strings.NewReader(string(body)))
	req.Header.Set("X-Hub-Signature-256", sign(body))
	req.Header.Set("X-GitHub-Event", "workflow_job")
	req.Header.Set("X-GitHub-Delivery", "pub-1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Errorf("a signed delivery on the public listener: %d, want 202", resp.StatusCode)
	}
}

// The broker's own acquire carries a jit_config, so it is under the pool's
// secret grants: with the defaults off a job gets no box until a
// github-jit grant names the link's identity; then it is served.
func TestBrokerUnderSecretGrants(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if w := f.link(t, inst, "good"); w.Code != http.StatusOK {
		t.Fatalf("link: %d %s", w.Code, w.Body)
	}
	if _, err := computeclient.SetSecretGrantDefault(ctx, f.owner, false); err != nil {
		t.Fatal(err)
	}
	ready := func() bool {
		hs, err := computeclient.RunTargetHandles(ctx, f.owner, "small", false)
		for _, h := range hs {
			if h.State == "ready" {
				return err == nil
			}
		}
		return false
	}
	f.deliver(t, "workflow_job", jobEvent("queued", 5001, 95, "acme/app", true, "self-hosted", "sengi"))
	eventually(t, "a refused box (its runner deleted)", func() bool {
		_, del := f.gh.snapshot()
		return slices.Contains(del, "potluck-5001")
	})
	if ready() {
		t.Fatal("a box was made with the defaults off and no grant")
	}
	if _, err := computeclient.AddSecretGrant(ctx, f.owner, computeapi.AddSecretGrantRequest{To: computeapi.GrantTo{Identity: runners.Identity},
		Kind: computeapi.SecretKindGitHubJIT, Name: computeapi.SecretNameJIT}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the job served under the grant", ready)
}
