package issuers

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"database/sql"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sengirunners/potluck/grants"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/events"
	"github.com/sengirunners/potluck/kit/secrets"
	"github.com/sengirunners/potluck/kit/store"
)

var t0 = time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)

// canary is in every token value the fake GitHub mints.
const canary = "ghs_CANARY7f3a"

type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// fakeGitHub mints tokens for installation 7 (account acme) and revokes them.
type fakeGitHub struct {
	mu        sync.Mutex
	clk       *clock
	calls     []string
	mints     []string // request bodies
	minted    []string // token values
	revoked   []string
	live      map[string]bool
	broaden   bool          // answer every mint with the whole installation
	mintFail  int           // status for every mint (0: 201)
	revFail   int           // status for every revoke (0: 204)
	expiresIn time.Duration // a token's life as GitHub answers it (0: an hour)
}

func (f *fakeGitHub) handler(t *testing.T) http.HandlerFunc {
	instRE := regexp.MustCompile(`^/repos/([^/]+)/([^/]+)/installation$`)
	return func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.calls = append(f.calls, r.Method+" "+r.URL.Path)
		switch {
		case r.Method == "POST" && r.URL.Path == "/app/installations/7/access_tokens":
			var b struct {
				Repositories []string          `json:"repositories"`
				Permissions  map[string]string `json:"permissions"`
			}
			_ = json.NewDecoder(r.Body).Decode(&b)
			raw, _ := json.Marshal(b)
			f.mints = append(f.mints, string(raw))
			if f.mintFail != 0 {
				w.WriteHeader(f.mintFail)
				fmt.Fprint(w, `{"message":"unavailable"}`)
				return
			}
			tok := fmt.Sprintf("%s_%d", canary, len(f.minted)+1)
			f.minted = append(f.minted, tok)
			f.live[tok] = true
			sel, repos := "selected", ""
			for i, n := range b.Repositories {
				if i > 0 {
					repos += ","
				}
				repos += fmt.Sprintf(`{"full_name":"acme/%s"}`, n)
			}
			if f.broaden {
				sel, repos = "all", `{"full_name":"acme/app"},{"full_name":"acme/infra"}`
			}
			life := f.expiresIn
			if life == 0 {
				life = time.Hour
			}
			perms, _ := json.Marshal(b.Permissions)
			w.WriteHeader(201)
			fmt.Fprintf(w, `{"token":%q,"expires_at":%q,"permissions":%s,"repository_selection":%q,"repositories":[%s]}`,
				tok, f.clk.Now().Add(life).Format(time.RFC3339), perms, sel, repos)
		case r.Method == "DELETE" && r.URL.Path == "/installation/token":
			if f.revFail != 0 {
				w.WriteHeader(f.revFail)
				return
			}
			tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if !f.live[tok] {
				w.WriteHeader(401)
				return
			}
			delete(f.live, tok)
			f.revoked = append(f.revoked, tok)
			w.WriteHeader(204)
		case r.Method == "GET" && instRE.MatchString(r.URL.Path):
			if instRE.FindStringSubmatch(r.URL.Path)[1] == "acme" {
				fmt.Fprint(w, `{"id":7}`)
				return
			}
			w.WriteHeader(404)
			fmt.Fprint(w, `{"message":"Not Found"}`)
		default:
			t.Errorf("unexpected GitHub call %s %s", r.Method, r.URL.Path)
			w.WriteHeader(404)
		}
	}
}

func (f *fakeGitHub) snapshot() (calls, mints, minted, revoked []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls), slices.Clone(f.mints), slices.Clone(f.minted), slices.Clone(f.revoked)
}

func (f *fakeGitHub) liveCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.live)
}

func (f *fakeGitHub) set(fn func(*fakeGitHub)) {
	f.mu.Lock()
	fn(f)
	f.mu.Unlock()
}

// boxes is what the fake Deliverer holds: name -> value, per box.
type boxes struct {
	mu      sync.Mutex
	vals    map[string]map[string]string
	fail    error
	pan     bool
	gate    chan struct{} // set: Deliver waits for it
	slow    time.Duration
	in, top int // deliveries running now, the most at once
}

func (b *boxes) Deliver(_ context.Context, box Box, name string, v secrets.Value) error {
	b.mu.Lock()
	gate, slow := b.gate, b.slow
	b.in++
	b.top = max(b.top, b.in)
	b.mu.Unlock()
	if gate != nil {
		<-gate
	}
	time.Sleep(slow)
	b.mu.Lock()
	defer b.mu.Unlock()
	b.in--
	if b.pan {
		panic("deliver blew up holding " + v.Reveal())
	}
	if b.fail != nil {
		return fmt.Errorf("%w: the box refused %s", b.fail, v.Reveal())
	}
	if b.vals[box.key()] == nil {
		b.vals[box.key()] = map[string]string{}
	}
	b.vals[box.key()][name] = v.Reveal()
	return nil
}

func (b *boxes) get(box Box, name string) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.vals[box.key()][name]
}

func (b *boxes) set(fn func(*boxes)) {
	b.mu.Lock()
	fn(b)
	b.mu.Unlock()
}

// bus is the fake event log.
type bus struct {
	mu  sync.Mutex
	ins []events.Input
}

func (p *bus) Publish(_ context.Context, in events.Input) (api.Event, error) {
	p.mu.Lock()
	p.ins = append(p.ins, in)
	p.mu.Unlock()
	return api.Event{}, nil
}

// list is every published event as "type:why".
func (p *bus) list() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []string
	for _, in := range p.ins {
		out = append(out, strings.TrimPrefix(in.Type, "dev.zm.secret.")+":"+in.Data.(SecretEvent).Why)
	}
	return out
}

type env struct {
	t     *testing.T
	clk   *clock
	gh    *fakeGitHub
	bx    *boxes
	bus   *bus
	db    *sql.DB
	logs  *bytes.Buffer
	cfg   config.File
	opt   Options
	iss   *Minted
	alive map[string]bool // box key -> the owner says it lives
	errs  []string        // every error any call answered, for the canary
	// grantOf is the grant the fake grant check names per box id (default
	// g1); grantMu guards it (renewals run concurrently).
	grantMu sync.Mutex
	grantOf map[string]string
}

// fakeCheck is the grant check the tests run with: every request passes,
// under e.grantOf's grant for its box.
func (e *env) fakeCheck(_ context.Context, req Request) (grants.Decision, error) {
	e.grantMu.Lock()
	defer e.grantMu.Unlock()
	g := e.grantOf[req.Box.ID]
	if g == "" {
		g = "g1"
	}
	return grants.Decision{Grant: g}, nil
}

var (
	trustedBox = Box{Project: "01POOL", Area: "ci", ID: "box-1", Host: "host-a"}
	otherBox   = Box{Project: "01POOL", Area: "ci", ID: "box-2", Host: "host-a"}
	thirdBox   = Box{Project: "01POOL", Area: "ci", ID: "box-3", Host: "host-a"}
	publicBox  = Box{Project: "01POOL", Area: "ci", ID: "box-9", Host: "lender"}
)

func newEnv(t *testing.T, ceiling ...config.SecretsCeiling) *env {
	t.Helper()
	dir := t.TempDir()
	k, _ := rsa.GenerateKey(rand.Reader, 2048)
	keyPath := filepath.Join(dir, "key.pem")
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)}), 0o600); err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, clk: &clock{now: t0}, bx: &boxes{vals: map[string]map[string]string{}}, bus: &bus{}, logs: &bytes.Buffer{},
		alive: map[string]bool{}, grantOf: map[string]string{}}
	e.gh = &fakeGitHub{clk: e.clk, live: map[string]bool{}}
	srv := httptest.NewServer(e.gh.handler(t))
	t.Cleanup(srv.Close)
	st, err := store.Open(context.Background(), filepath.Join(dir, "home"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.AddMigrations(context.Background(), Migrations...); err != nil {
		t.Fatal(err)
	}
	e.db = st.Registry()
	e.cfg = config.File{Secrets: config.Secrets{Issuers: map[string]config.SecretsIssuer{
		"gh": {Kind: KindGitHubApp, AppID: 42, PrivateKey: "file:" + keyPath, APIURL: srv.URL, Ceiling: ceiling}}}}
	var logMu sync.Mutex
	e.opt = Options{Deliver: e.bx, Ledger: SQLLedger{e.db}, Events: e.bus, HTTP: srv.Client(), Now: e.clk.Now,
		Sleep: func(context.Context, time.Duration) error { return nil },
		Log:   slog.New(slog.NewTextHandler(lockedWriter{&logMu, e.logs}, &slog.HandlerOptions{Level: slog.LevelDebug})),
		ClassOf: func(_ context.Context, b Box) (string, error) {
			switch b.Host {
			case "lender":
				return api.MachinePublic, nil
			case "unknown":
				return "", errors.New("no such machine")
			}
			return ClassTrusted, nil
		},
		Live:  func(_ context.Context, b Box) (bool, error) { return e.alive[b.key()], nil },
		Check: e.fakeCheck}
	e.iss = e.load()
	return e
}

type lockedWriter struct {
	mu *sync.Mutex
	b  *bytes.Buffer
}

func (w lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}

func (e *env) load() *Minted {
	e.t.Helper()
	g, err := Load(context.Background(), e.cfg, "gh", e.opt)
	if err != nil {
		e.t.Fatal(err)
	}
	return g
}

func (e *env) issue(req Request) (Issued, error) {
	out, err := e.iss.Issue(context.Background(), req)
	if err != nil {
		e.errs = append(e.errs, err.Error())
	}
	return out, err
}

func (e *env) mustIssue(req Request) Issued {
	e.t.Helper()
	out, err := e.issue(req)
	if err != nil {
		e.t.Fatal(err)
	}
	return out
}

// rows is the ledger as text, one row per line, every column.
func (e *env) rows() []string {
	e.t.Helper()
	var out []string
	for _, table := range []string{"issuers_ledger", "issuers_assignments"} {
		rs, err := e.db.Query(`SELECT * FROM ` + table)
		if err != nil {
			e.t.Fatal(err)
		}
		cols, _ := rs.Columns()
		for rs.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rs.Scan(ptrs...); err != nil {
				e.t.Fatal(err)
			}
			out = append(out, fmt.Sprint(vals...))
		}
		rs.Close()
	}
	return out
}

// events is the ledger's event and why columns.
func (e *env) events() []string {
	e.t.Helper()
	rs, err := e.db.Query(`SELECT event, why FROM issuers_ledger ORDER BY id`)
	if err != nil {
		e.t.Fatal(err)
	}
	defer rs.Close()
	var out []string
	for rs.Next() {
		var ev, why string
		_ = rs.Scan(&ev, &why)
		out = append(out, ev+":"+why)
	}
	return out
}

func (e *env) assigned() []string {
	e.t.Helper()
	reqs, err := SQLLedger{e.db}.Assigned(context.Background(), "gh")
	if err != nil {
		e.t.Fatal(err)
	}
	var out []string
	for _, r := range reqs {
		out = append(out, r.Box.ID)
	}
	slices.Sort(out)
	return out
}

var appCeiling = config.SecretsCeiling{Repo: "acme/app", Permissions: map[string]string{"contents": "write", "pull_requests": "write", "checks": "read"}}

func ident(box Box, perms map[string]string) Request {
	return Request{Name: "GH_TOKEN", Subject: Subject{IdentityID: "01IDCONTROLLER"}, Box: box, Repo: "acme/app", Permissions: perms,
		Reason: "task 12", Grant: "g1"}
}

func mainJob() *Job {
	return &Job{Repo: "acme/app", HeadRepo: "acme/app", RepoID: 9, HeadRepoID: 9, Workflow: ".github/workflows/lander.yml", Ref: "refs/heads/main", DefaultBranch: "main",
		Event: "push", RunID: 99}
}

var write = map[string]string{"contents": "write"}

func codeOf(err error) string {
	var ae *api.Error
	if errors.As(err, &ae) {
		return ae.Code
	}
	return ""
}

func TestSpecs(t *testing.T) {
	good := config.SecretsIssuer{Kind: KindGitHubApp, AppID: 1, PrivateKey: "file:/k.pem", Ceiling: []config.SecretsCeiling{appCeiling,
		{Repo: "acme/*", Permissions: map[string]string{"contents": "read"}, Workflows: []string{".github/workflows/ci.yml"}},
		{Repo: "acme/rel", Permissions: map[string]string{"workflows": "write", "actions": "read"}, Elevated: []string{"workflows"},
			Reason: "release tasks", Events: []string{"workflow_dispatch"}}}}
	broker := config.RunnersGitHub{AppID: 5, PrivateKey: "file:/broker.pem", APIURL: "https://ghe.example/api/v3/"}
	f := config.File{Runners: config.Runners{GitHub: broker}, Secrets: config.Secrets{Issuers: map[string]config.SecretsIssuer{
		"own": good, "shared": {Kind: KindGitHubApp, Use: UseRunners}, "none": {Kind: KindGitHubApp, AppID: 1, PrivateKey: "env:K"}}}}
	ss, err := Specs(f)
	if err != nil {
		t.Fatal(err)
	}
	if s := ss["shared"]; s.AppID != 5 || s.PrivateKey != "file:/broker.pem" || s.APIURL != "https://ghe.example/api/v3" {
		t.Errorf("use runners.github: %+v", s)
	}
	if s := ss["own"]; s.APIURL != "https://api.github.com" || len(s.Ceiling) != 3 || !slices.Equal(s.Ceiling[0].Events, []string{"push", "schedule"}) {
		t.Errorf("own: %+v", s)
	}
	if _, ok := ss["none"].match("acme/app", Subject{IdentityID: "x"}, map[string]string{"metadata": "read"}); ok {
		t.Error("an issuer with no ceiling grants something")
	}

	mod := func(fn func(*config.SecretsIssuer)) config.SecretsIssuer { c := good; fn(&c); return c }
	rule := func(r config.SecretsCeiling) config.SecretsIssuer {
		return mod(func(c *config.SecretsIssuer) { c.Ceiling = []config.SecretsCeiling{r} })
	}
	perm := func(p, lvl string) config.SecretsIssuer {
		return rule(config.SecretsCeiling{Repo: "acme/app", Permissions: map[string]string{p: lvl}})
	}
	cases := map[string]config.SecretsIssuer{
		"an unknown kind":         mod(func(c *config.SecretsIssuer) { c.Kind = "vault" }),
		"an inline key":           mod(func(c *config.SecretsIssuer) { c.PrivateKey = "-----BEGIN RSA PRIVATE KEY-----\nMII" }),
		"a key not a reference":   mod(func(c *config.SecretsIssuer) { c.PrivateKey = "MIIEowIBAAKCAQEA" }),
		"no key":                  mod(func(c *config.SecretsIssuer) { c.PrivateKey = "" }),
		"no app":                  mod(func(c *config.SecretsIssuer) { c.AppID = 0 }),
		"use with its own app":    mod(func(c *config.SecretsIssuer) { c.Use = UseRunners }),
		"an unknown use":          {Kind: KindGitHubApp, Use: "door.github"},
		"plain http":              mod(func(c *config.SecretsIssuer) { c.APIURL = "http://ghe.example" }),
		"admin level":             perm("contents", "admin"),
		"workflows read":          perm("workflows", "read"),
		"elevated write unlisted": rule(config.SecretsCeiling{Repo: "acme/app", Permissions: map[string]string{"checks": "write"}}),
		"elevated without a reason": rule(config.SecretsCeiling{Repo: "acme/app", Permissions: map[string]string{"checks": "write"},
			Elevated: []string{"checks"}}),
		"elevated listing a plain one": rule(config.SecretsCeiling{Repo: "acme/app", Permissions: map[string]string{"contents": "write"},
			Elevated: []string{"contents"}, Reason: "x"}),
		"elevated listed at read": rule(config.SecretsCeiling{Repo: "acme/app", Permissions: map[string]string{"checks": "read"},
			Elevated: []string{"checks"}, Reason: "x"}),
		"merge_group":          rule(config.SecretsCeiling{Repo: "acme/app", Permissions: write, Events: []string{"merge_group"}}),
		"release":              rule(config.SecretsCeiling{Repo: "acme/app", Permissions: write, Events: []string{"release"}}),
		"pull_request":         rule(config.SecretsCeiling{Repo: "acme/app", Permissions: write, Events: []string{"pull_request"}}),
		"no permissions":       rule(config.SecretsCeiling{Repo: "acme/app"}),
		"a bare owner":         rule(config.SecretsCeiling{Repo: "acme", Permissions: write}),
		"every owner":          rule(config.SecretsCeiling{Repo: "*/app", Permissions: write}),
		"a glob name":          rule(config.SecretsCeiling{Repo: "acme/app-*", Permissions: write}),
		"a workflow elsewhere": rule(config.SecretsCeiling{Repo: "acme/app", Permissions: write, Workflows: []string{"ci.yml"}}),
		"a workflow escaping":  rule(config.SecretsCeiling{Repo: "acme/app", Permissions: write, Workflows: []string{".github/workflows/../x.yml"}}),
	}
	// Denied: everything off the allowlist, GitHub's names and made-up ones.
	for _, p := range []string{"administration", "secrets", "environments", "repository_hooks", "members", "organization_secrets",
		"organization_administration", "dependabot_secrets", "codespaces", "codespaces_secrets", "repository_custom_properties",
		"team_discussions", "email_addresses", "gpg_keys", "actions_variables", "single_file", "repository_projects", "made_up"} {
		cases["deny "+p] = perm(p, "read")
	}
	for name, is := range cases {
		f := config.File{Secrets: config.Secrets{Issuers: map[string]config.SecretsIssuer{"x": is}}}
		if _, err := Specs(f); err == nil {
			t.Errorf("%s: accepted", name)
		} else if strings.Contains(err.Error(), "MII") {
			t.Errorf("%s: the error quotes the key: %v", name, err)
		}
	}
	if _, err := Specs(config.File{Secrets: config.Secrets{Issuers: map[string]config.SecretsIssuer{"x": {Kind: KindGitHubApp, Use: UseRunners}}}}); err == nil {
		t.Error("use runners.github without a broker App: accepted")
	}
	if _, err := Specs(config.File{Secrets: config.Secrets{Issuers: map[string]config.SecretsIssuer{"Bad Name": good}}}); err == nil {
		t.Error("a bad issuer name: accepted")
	}
}

// H1: a key the section does not know, at any level, refuses the section
// (naming the key) instead of being dropped, and Save keeps it as written.
func TestConfigIsDecodedStrictly(t *testing.T) {
	base := `{"kind":"github-app","app_id":1,"private_key":"file:/k.pem","ceiling":[{"repo":"acme/app","permissions":{"contents":"write"},%s}]}`
	for name, c := range map[string]struct{ doc, key string }{
		"a rule's typo":     {`{"issuers":{"gh":` + fmt.Sprintf(base, `"workflow":[".github/workflows/x.yml"]`) + `}}`, "workflow"},
		"an issuer's typo":  {`{"issuers":{"gh":{"kind":"github-app","app_id":1,"private_key":"file:/k.pem","ceilings":[]}}}`, "ceilings"},
		"the section's":     {`{"issuer":{}}`, "issuer"},
		"an elevated typo":  {`{"issuers":{"gh":` + fmt.Sprintf(base, `"elevate":["workflows"]`) + `}}`, "elevate"},
		"a reason misnamed": {`{"issuers":{"gh":` + fmt.Sprintf(base, `"why":"x"`) + `}}`, "why"},
	} {
		home := t.TempDir()
		doc := `{"human":"me","secrets":` + c.doc + `}`
		if err := os.WriteFile(filepath.Join(home, "config.json"), []byte(doc), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, err := config.LoadFrom(home)
		if err != nil {
			t.Fatalf("%s: the whole config failed: %v", name, err)
		}
		if _, err := Specs(cfg.File); err == nil || !strings.Contains(err.Error(), `"`+c.key+`"`) {
			t.Errorf("%s: %v, want a refusal naming %q", name, err, c.key)
		}
		if err := cfg.Save(); err != nil {
			t.Fatal(err)
		}
		b, _ := os.ReadFile(filepath.Join(home, "config.json"))
		if !strings.Contains(string(b), `"`+c.key+`"`) {
			t.Errorf("%s: Save dropped the section as written:\n%s", name, b)
		}
	}
	// A section that decodes is read as usual and saved back the same.
	home := t.TempDir()
	doc := `{"secrets":{"issuers":{"gh":` + fmt.Sprintf(base, `"workflows":[".github/workflows/x.yml"]`) + `}}}`
	_ = os.WriteFile(filepath.Join(home, "config.json"), []byte(doc), 0o600)
	cfg, err := config.LoadFrom(home)
	if err != nil {
		t.Fatal(err)
	}
	ss, err := Specs(cfg.File)
	if err != nil || len(ss["gh"].Ceiling[0].Workflows) != 1 {
		t.Fatalf("%v %+v", err, ss)
	}
}

func TestIssueMintsOneRepositoryWithOnlyThePermissionsAsked(t *testing.T) {
	e := newEnv(t, appCeiling)
	got := e.mustIssue(ident(trustedBox, write))
	_, mints, minted, _ := e.gh.snapshot()
	if len(mints) != 1 || mints[0] != `{"repositories":["app"],"permissions":{"contents":"write","metadata":"read"}}` {
		t.Fatalf("asked GitHub for %v", mints)
	}
	if v := e.bx.get(trustedBox, "GH_TOKEN"); v != minted[0] {
		t.Fatalf("the box holds %q, want the minted token", v)
	}
	if got.Repo != "acme/app" || !got.ExpiresAt.Equal(t0.Add(time.Hour)) || len(got.HashedToken) != 44 || got.Permissions["contents"] != "write" {
		t.Errorf("issued %+v", got)
	}
	if again, err := e.issue(ident(trustedBox, write)); err != nil || again.HashedToken != got.HashedToken {
		t.Errorf("the same request again: %+v, %v", again, err)
	}
	if _, err := e.issue(ident(trustedBox, map[string]string{"contents": "read"})); codeOf(err) != api.CodeConflict {
		t.Errorf("a second request for the same name: %v, want conflict", err)
	}
	if ev := e.events(); len(ev) != 1 || ev[0] != "issued:" {
		t.Errorf("ledger %v", ev)
	}
	if a := e.assigned(); !slices.Equal(a, []string{"box-1"}) {
		t.Errorf("assignments %v", a)
	}
	// A job on the default branch, its own repository, in a rule naming its
	// workflow; a dispatch only where the rule opts in.
	e2 := newEnv(t, config.SecretsCeiling{Repo: "acme/*", Permissions: write, Workflows: []string{".github/workflows/lander.yml"},
		Events: []string{"push", "workflow_dispatch"}})
	req := ident(trustedBox, write)
	req.Subject = Subject{Job: mainJob()}
	if _, err := e2.issue(req); err != nil {
		t.Fatalf("a main-branch push of a listed workflow: %v", err)
	}
	req.Box = otherBox
	req.Subject.Job.Event = "workflow_dispatch"
	if _, err := e2.issue(req); err != nil {
		t.Fatalf("an opted-in dispatch: %v", err)
	}
}

func TestElevatedPermissions(t *testing.T) {
	e := newEnv(t, appCeiling, config.SecretsCeiling{Repo: "acme/rel", Permissions: map[string]string{"workflows": "write", "contents": "write"},
		Elevated: []string{"workflows"}, Reason: "release tasks cleared by the dispatcher"})
	req := ident(trustedBox, map[string]string{"workflows": "write", "contents": "write"})
	req.Repo = "acme/rel"
	got := e.mustIssue(req)
	var elevated, ruleReason, reason string
	if err := e.db.QueryRow(`SELECT elevated, rule_reason, reason FROM issuers_ledger WHERE hashed_token = ?`, got.HashedToken).
		Scan(&elevated, &ruleReason, &reason); err != nil {
		t.Fatal(err)
	}
	if elevated != "workflows" || ruleReason != "release tasks cleared by the dispatcher" || reason != "task 12" {
		t.Errorf("ledger elevated %q, rule reason %q, reason %q", elevated, ruleReason, reason)
	}
	// Without the request's reason, or under a rule that does not list it: refused.
	req.Reason, req.Box = "", otherBox
	if _, err := e.issue(req); codeOf(err) != api.CodeForbidden {
		t.Errorf("an elevated write with no reason: %v", err)
	}
	req.Reason, req.Repo = "x", "acme/app"
	if _, err := e.issue(req); codeOf(err) != api.CodeForbidden {
		t.Errorf("workflows under a rule without it: %v", err)
	}
	// checks read is plain under the app rule.
	if _, err := e.issue(ident(thirdBox, map[string]string{"checks": "read"})); err != nil {
		t.Errorf("checks read: %v", err)
	}
}

func TestRefusalsComeBeforeAnyGitHubCall(t *testing.T) {
	e := newEnv(t, appCeiling, config.SecretsCeiling{Repo: "acme/web", Permissions: map[string]string{"contents": "read"},
		Workflows: []string{".github/workflows/deploy.yml"}})
	job := func(fn func(*Job)) Request {
		r := ident(trustedBox, write)
		j := mainJob()
		fn(j)
		r.Subject = Subject{Job: j}
		return r
	}
	for name, req := range map[string]Request{
		"an unknown repository":           func() Request { r := ident(trustedBox, write); r.Repo = "acme/infra"; return r }(),
		"another owner":                   func() Request { r := ident(trustedBox, write); r.Repo = "evil/app"; return r }(),
		"write over a read ceiling":       ident(trustedBox, map[string]string{"checks": "write"}),
		"a permission not in the ceiling": ident(trustedBox, map[string]string{"issues": "read"}),
		"one permission too many":         ident(trustedBox, map[string]string{"contents": "write", "workflows": "write"}),
		"administration":                  ident(trustedBox, map[string]string{"administration": "write"}),
		"a name off the allowlist":        ident(trustedBox, map[string]string{"made_up": "read"}),
		"workflows read":                  ident(trustedBox, map[string]string{"workflows": "read"}),
		"no permissions":                  ident(trustedBox, nil),
		"an admin level":                  ident(trustedBox, map[string]string{"contents": "admin"}),
		"a public host":                   ident(publicBox, write),
		"a host of no known class":        ident(Box{Project: "01POOL", ID: "b", Host: "unknown"}, write),
		"a bad name":                      func() Request { r := ident(trustedBox, write); r.Name = "gh_token"; return r }(),
		"no box":                          ident(Box{}, write),
		"no subject":                      func() Request { r := ident(trustedBox, write); r.Subject = Subject{}; return r }(),
		"two subjects":                    func() Request { r := ident(trustedBox, write); r.Subject.Job = mainJob(); return r }(),
		"a fork's run":                    job(func(j *Job) { j.HeadRepo = "stranger/app" }),
		"no head repository":              job(func(j *Job) { j.HeadRepo = "" }),
		"no run id":                       job(func(j *Job) { j.RunID = 0 }),
		"a pull request":                  job(func(j *Job) { j.Event, j.Ref = "pull_request", "refs/pull/4/merge" }),
		"a pull request on main":          job(func(j *Job) { j.Event = "pull_request_target" }),
		"a workflow_run":                  job(func(j *Job) { j.Event = "workflow_run" }),
		"a merge_group":                   job(func(j *Job) { j.Event = "merge_group" }),
		"a release":                       job(func(j *Job) { j.Event = "release" }),
		"a dispatch not opted in":         job(func(j *Job) { j.Event = "workflow_dispatch" }),
		"another branch":                  job(func(j *Job) { j.Ref = "refs/heads/feature" }),
		"a tag named main":                job(func(j *Job) { j.Ref = "refs/tags/main" }),
		"no default branch known":         job(func(j *Job) { j.DefaultBranch = "" }),
		"another repository's job":        job(func(j *Job) { j.Repo, j.HeadRepo = "acme/web", "acme/web" }),
		"a workflow the rule does not name": func() Request {
			r := job(func(j *Job) { j.Repo, j.HeadRepo, j.Workflow = "acme/web", "acme/web", ".github/workflows/ci.yml" })
			r.Repo, r.Permissions = "acme/web", map[string]string{"contents": "read"}
			return r
		}(),
		"an identity under a workflow rule": func() Request {
			r := ident(trustedBox, map[string]string{"contents": "read"})
			r.Repo = "acme/web"
			return r
		}(),
	} {
		if _, err := e.issue(req); codeOf(err) != api.CodeForbidden {
			t.Errorf("%s: %v, want forbidden", name, err)
		}
	}
	if calls, _, _, _ := e.gh.snapshot(); len(calls) != 0 {
		t.Errorf("refusals called GitHub: %v", calls)
	}
	if len(e.bx.vals) != 0 || len(e.events()) != 0 {
		t.Errorf("refusals delivered %v or wrote ledger rows %v", e.bx.vals, e.events())
	}
	for _, ev := range e.bus.list() {
		if !strings.HasPrefix(ev, "refused:") {
			t.Errorf("published %q, want only refusals", ev)
		}
	}
	// The grant check is asked too.
	e.iss.o.Check = func(context.Context, Request) (grants.Decision, error) {
		return grants.Decision{}, errors.New("no grant covers it")
	}
	if _, err := e.issue(ident(trustedBox, write)); codeOf(err) != api.CodeForbidden {
		t.Errorf("a request the grant check refuses: %v", err)
	}
	e.iss.o.Check = e.fakeCheck
	r := ident(trustedBox, map[string]string{"contents": "read"})
	r.Repo, r.Subject = "acme/web", Subject{Job: &Job{Repo: "acme/web", HeadRepo: "acme/web", RepoID: 9, HeadRepoID: 9, Workflow: ".github/workflows/deploy.yml",
		Ref: "refs/heads/trunk", DefaultBranch: "trunk", Event: "push", RunID: 1}}
	if _, err := e.issue(r); err != nil {
		t.Errorf("the listed workflow on the default branch: %v", err)
	}
}

// A box whose host's swap is not safe for secrets gets no token minted: the
// door would refuse the delivery, so GitHub is never asked.
func TestNoTokenIsMintedForAHostWithUnsafeSwap(t *testing.T) {
	e := newEnv(t, appCeiling, config.SecretsCeiling{Repo: "acme/web", Permissions: map[string]string{"contents": "read"}})
	e.iss.o.BoxTakesSecrets = func(context.Context, Box) error {
		return api.Errorf(api.CodeForbidden, "lender swaps to a plain disk: fix swap there")
	}
	_, err := e.issue(ident(trustedBox, write))
	if codeOf(err) != api.CodeForbidden || !strings.Contains(err.Error(), "plain disk") {
		t.Errorf("unsafe swap: %v, want forbidden naming it", err)
	}
	if calls, _, _, _ := e.gh.snapshot(); len(calls) != 0 {
		t.Errorf("unsafe swap called GitHub: %v", calls)
	}
	if len(e.bx.vals) != 0 {
		t.Errorf("unsafe swap delivered %v", e.bx.vals)
	}
	e.iss.o.BoxTakesSecrets = func(context.Context, Box) error { return nil }
	if _, err := e.issue(ident(trustedBox, write)); err != nil {
		t.Errorf("safe swap: %v", err)
	}
}

// An answer that is not a refusal (no host has said anything yet, right after
// a restart) is unavailable, never forbidden: a re-issue after a restart waits
// and tries again instead of dropping the box's assignment. A held token whose
// host's swap turns unsafe is not renewed (nothing is minted) and is ended.
func TestSwapUnknownIsNotARefusalAndRenewalStopsOnUnsafeSwap(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, appCeiling)
	e.iss.o.BoxTakesSecrets = func(context.Context, Box) error {
		return api.Errorf(api.CodeUnavailable, "lender advertises no run target now")
	}
	if _, err := e.issue(ident(trustedBox, write)); codeOf(err) != api.CodeUnavailable {
		t.Errorf("a host not heard from yet: %v, want unavailable", err)
	}
	if calls, _, _, _ := e.gh.snapshot(); len(calls) != 0 {
		t.Errorf("an unknown swap called GitHub: %v", calls)
	}
	e.iss.o.BoxTakesSecrets = func(context.Context, Box) error { return nil }
	e.mustIssue(ident(trustedBox, write))
	e.iss.o.BoxTakesSecrets = func(context.Context, Box) error {
		return api.Errorf(api.CodeForbidden, "lender swaps to a plain disk: fix swap there")
	}
	e.clk.add(40 * time.Minute)
	e.iss.Tick(ctx)
	if _, mints, _, revoked := e.gh.snapshot(); len(mints) != 1 || len(revoked) != 1 {
		t.Errorf("unsafe swap at renewal: %d mints, %d revoked, want 1 and the held token revoked", len(mints), len(revoked))
	}
}

// At renewal a host not heard from now (its advert went stale) is not an unsafe
// one: nothing is revoked, the assignment stays, and the renewal is tried
// again a minute later.
func TestRenewalWaitsWhenTheHostsSwapIsNotKnown(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, appCeiling)
	first := e.mustIssue(ident(trustedBox, write))
	e.iss.o.BoxTakesSecrets = func(context.Context, Box) error {
		return api.Errorf(api.CodeUnavailable, "lender advertises no run target now")
	}
	e.clk.add(40 * time.Minute)
	e.iss.Tick(ctx)
	if _, mints, _, revoked := e.gh.snapshot(); len(mints) != 1 || len(revoked) != 0 {
		t.Fatalf("an unknown swap at renewal: %d mints, %d revoked, want the token left alone", len(mints), len(revoked))
	}
	e.iss.o.BoxTakesSecrets = func(context.Context, Box) error { return nil }
	e.clk.add(time.Minute)
	e.iss.Tick(ctx)
	if _, mints, _, _ := e.gh.snapshot(); len(mints) != 2 {
		t.Errorf("the renewal was not tried again: %d mints", len(mints))
	}
	if again, err := e.issue(ident(trustedBox, write)); err != nil || again.HashedToken == "" || again.HashedToken == first.HashedToken {
		t.Errorf("the assignment did not survive: %v %v", again, err)
	}
}

func TestBroaderAnswerIsRevokedAndNothingDelivered(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, appCeiling)
	e.gh.set(func(f *fakeGitHub) { f.broaden = true })
	if _, err := e.issue(ident(trustedBox, write)); err == nil {
		t.Fatal("a broader answer was issued")
	}
	_, _, minted, revoked := e.gh.snapshot()
	if len(minted) != 1 || len(revoked) != 1 || revoked[0] != minted[0] || len(e.bx.vals) != 0 {
		t.Errorf("minted %d, revoked %d: the broader token must be revoked, never delivered", len(minted), len(revoked))
	}
	// L2: one whose revoke fails is retried, not tried once.
	e.gh.set(func(f *fakeGitHub) { f.revFail = 503 })
	if _, err := e.issue(ident(otherBox, write)); err == nil {
		t.Fatal("a broader answer was issued")
	}
	e.gh.set(func(f *fakeGitHub) { f.revFail = 0; f.broaden = false })
	e.clk.add(time.Minute)
	e.iss.Tick(ctx)
	if e.gh.liveCount() != 0 {
		t.Errorf("%d tokens live: the unrevoked broader token was not retried", e.gh.liveCount())
	}
	// M3: an expiry not within the next hour (GitHub's clock far off) is refused and revoked.
	for _, life := range []time.Duration{2 * time.Hour, -time.Minute} {
		e.gh.set(func(f *fakeGitHub) { f.expiresIn = life })
		if _, err := e.issue(ident(thirdBox, write)); err == nil || !strings.Contains(err.Error(), "expiry") {
			t.Errorf("a token living %v: %v", life, err)
		}
	}
	if e.gh.liveCount() != 0 || len(e.bx.vals) != 0 {
		t.Error("a token with a bad expiry was kept or delivered")
	}
}

func TestMintFailureFailsClosed(t *testing.T) {
	e := newEnv(t, appCeiling)
	e.gh.set(func(f *fakeGitHub) { f.mintFail = 502 })
	if _, err := e.issue(ident(trustedBox, write)); codeOf(err) != api.CodeUnavailable {
		t.Fatalf("got %v, want unavailable", err)
	}
	if _, mints, _, _ := e.gh.snapshot(); len(mints) != 3 {
		t.Errorf("%d mint attempts, want 3 (bounded retry)", len(mints))
	}
	if len(e.bx.vals) != 0 || len(e.iss.held) != 0 || len(e.assigned()) != 0 {
		t.Error("a failed mint left something behind")
	}
	e.gh.set(func(f *fakeGitHub) { f.mintFail = 0 })
	if _, err := e.issue(ident(trustedBox, write)); err != nil {
		t.Errorf("after the failure: %v", err)
	}
}

func TestDeliveryFailureRevokes(t *testing.T) {
	e := newEnv(t, appCeiling)
	e.bx.set(func(b *boxes) { b.fail = errors.New("no memory filesystem") })
	if _, err := e.issue(ident(trustedBox, write)); err == nil {
		t.Fatal("issued without delivering")
	}
	e.bx.set(func(b *boxes) { b.fail, b.pan = nil, true })
	if _, err := e.issue(ident(trustedBox, write)); err == nil || !strings.Contains(err.Error(), "panicked") {
		t.Fatalf("a panicking deliverer: %v", err)
	}
	_, _, minted, revoked := e.gh.snapshot()
	if len(minted) != 2 || len(revoked) != 2 {
		t.Errorf("minted %d, revoked %d: an undelivered token must be revoked", len(minted), len(revoked))
	}
}

// L5: an identical Issue while one is in flight waits for it.
func TestConcurrentIdenticalIssueWaits(t *testing.T) {
	e := newEnv(t, appCeiling)
	gate := make(chan struct{})
	e.bx.set(func(b *boxes) { b.gate = gate })
	type res struct {
		out Issued
		err error
	}
	ch := make(chan res, 2)
	for range 2 {
		go func() {
			out, err := e.iss.Issue(context.Background(), ident(trustedBox, write))
			ch <- res{out, err}
		}()
	}
	for {
		_, mints, _, _ := e.gh.snapshot()
		e.bx.mu.Lock()
		in := e.bx.in
		e.bx.mu.Unlock()
		if len(mints) == 1 && in == 1 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond) // the second is waiting, not minting
	e.bx.set(func(b *boxes) { b.gate = nil })
	close(gate)
	a, b := <-ch, <-ch
	if a.err != nil || b.err != nil || a.out.HashedToken != b.out.HashedToken {
		t.Fatalf("%v / %v: want both the one token", a, b)
	}
	if _, mints, _, _ := e.gh.snapshot(); len(mints) != 1 {
		t.Errorf("%d mints, want 1", len(mints))
	}
}

func TestRenewalAtTwoThirds(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, appCeiling)
	first := e.mustIssue(ident(trustedBox, write))
	if next := e.iss.Tick(ctx); !next.Equal(t0.Add(time.Minute)) {
		t.Errorf("next due %v, want capped at +1m", next)
	}
	e.clk.add(39 * time.Minute)
	e.iss.Tick(ctx)
	if _, mints, _, _ := e.gh.snapshot(); len(mints) != 1 {
		t.Fatalf("renewed before two thirds: %d mints", len(mints))
	}
	e.clk.add(time.Minute) // +40m
	e.iss.Tick(ctx)
	_, mints, minted, revoked := e.gh.snapshot()
	if len(mints) != 2 || mints[1] != mints[0] {
		t.Fatalf("renewal asked %v, want the same as the first", mints)
	}
	if e.bx.get(trustedBox, "GH_TOKEN") != minted[1] || len(revoked) != 0 {
		t.Error("the renewed token did not replace the old one, or the old was revoked before its grace")
	}
	e.clk.add(5 * time.Minute)
	e.iss.Tick(ctx)
	if _, _, _, revoked = e.gh.snapshot(); len(revoked) != 1 || revoked[0] != minted[0] {
		t.Errorf("after the grace revoked %d, want the old token", len(revoked))
	}
	if pub := strings.Join(e.bus.list(), " "); !strings.Contains(pub, "renewed:") || !strings.Contains(pub, "revoked:renewed") {
		t.Errorf("published %s", pub)
	}
	for _, in := range e.bus.ins {
		if in.Audience == nil || in.Audience.Need != authz.Operate || in.Project != "01POOL" || in.Area != "ci" || in.Subject != "box-1" {
			t.Errorf("event %s: audience %+v, project %s, area %s", in.Type, in.Audience, in.Project, in.Area)
		}
	}
	if again, _ := e.issue(ident(trustedBox, write)); again.HashedToken == first.HashedToken || again.HashedToken == "" {
		t.Error("Issue does not answer the renewed token")
	}

	// A renewal GitHub refuses keeps the old token and tries again a minute later.
	e.clk.add(35 * time.Minute)
	e.gh.set(func(f *fakeGitHub) { f.mintFail = 500 })
	e.iss.Tick(ctx)
	if e.bx.get(trustedBox, "GH_TOKEN") != minted[1] {
		t.Error("a failed renewal changed the box")
	}
	e.gh.set(func(f *fakeGitHub) { f.mintFail = 0 })
	e.clk.add(30 * time.Second)
	e.iss.Tick(ctx)
	if _, mints, _, _ = e.gh.snapshot(); len(mints) != 5 {
		t.Errorf("retried a renewal before a minute passed: %d mints", len(mints))
	}
	e.clk.add(30 * time.Second)
	e.iss.Tick(ctx)
	if _, _, minted, _ = e.gh.snapshot(); len(minted) != 3 || e.bx.get(trustedBox, "GH_TOKEN") != minted[2] {
		t.Errorf("the renewal was not retried: %d minted", len(minted))
	}

	// A renewal whose delivery fails revokes the new token and keeps the old.
	e.clk.add(41 * time.Minute)
	e.bx.set(func(b *boxes) { b.fail = errors.New("box busy") })
	e.iss.Tick(ctx)
	_, _, minted, revoked = e.gh.snapshot()
	if len(minted) != 4 || !slices.Contains(revoked, minted[3]) || e.bx.get(trustedBox, "GH_TOKEN") != minted[2] {
		t.Error("an undelivered renewal was not revoked, or the old token was lost")
	}

	// A box whose machine is no longer trusted is not renewed: its tokens are revoked.
	e.bx.set(func(b *boxes) { b.fail = nil })
	e.iss.o.ClassOf = func(context.Context, Box) (string, error) { return api.MachinePublic, nil }
	e.clk.add(time.Minute)
	e.iss.Tick(ctx)
	if len(e.iss.held) != 0 || e.gh.liveCount() != 0 || len(e.assigned()) != 0 {
		t.Errorf("a demoted host kept %d tokens, %d live at GitHub", len(e.iss.held), e.gh.liveCount())
	}
}

// L4: an owner that sleeps until Tick's answer renews a token issued while
// it slept.
func TestTickNeverSleepsPastARenewal(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, appCeiling)
	next := e.iss.Tick(ctx) // idle
	if !next.Equal(t0.Add(tickCap)) {
		t.Fatalf("idle Tick answered %v", next)
	}
	e.clk.add(30 * time.Second)
	e.mustIssue(ident(trustedBox, write))
	for i := 0; i < 100; i++ {
		e.clk.add(next.Sub(e.clk.Now()))
		next = e.iss.Tick(ctx)
		if _, mints, _, _ := e.gh.snapshot(); len(mints) == 2 {
			if at := e.clk.Now(); at.After(t0.Add(30*time.Second + 41*time.Minute)) {
				t.Errorf("renewed at %v, late", at.Sub(t0))
			}
			return
		}
	}
	t.Fatal("never renewed")
}

// L5: renewals run concurrently, bounded.
func TestRenewalsAreConcurrentAndBounded(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, appCeiling)
	e.iss.o.Renewers = 3
	for i := range 9 {
		e.mustIssue(ident(Box{Project: "01POOL", ID: fmt.Sprintf("b%d", i), Host: "h"}, write))
	}
	e.bx.set(func(b *boxes) { b.slow, b.top = 20*time.Millisecond, 0 })
	e.clk.add(40 * time.Minute)
	e.iss.Tick(ctx)
	_, mints, _, _ := e.gh.snapshot()
	if len(mints) != 18 {
		t.Fatalf("%d mints, want 9 renewals", len(mints))
	}
	if e.bx.top < 2 || e.bx.top > 3 {
		t.Errorf("%d renewals delivered at once, want 2 or 3", e.bx.top)
	}
}

func TestEveryEndRevokes(t *testing.T) {
	ctx := context.Background()
	for _, why := range []EndReason{EndReleased, EndExpired, EndReclaimed, EndLost, EndJobCompleted, EndJobTaken, EndHostDemoted, EndSwapUnsafe} {
		e := newEnv(t, appCeiling)
		e.mustIssue(ident(trustedBox, write))
		e.mustIssue(ident(otherBox, write))
		if err := e.iss.End(ctx, trustedBox, why); err != nil {
			t.Fatalf("%s: %v", why, err)
		}
		_, _, minted, revoked := e.gh.snapshot()
		if len(revoked) != 1 || revoked[0] != minted[0] {
			t.Errorf("%s: revoked %v, want the first box's token only", why, len(revoked))
		}
		if ev := e.events(); ev[len(ev)-1] != "revoked:"+string(why) {
			t.Errorf("%s: ledger %v", why, ev)
		}
		if a := e.assigned(); !slices.Equal(a, []string{"box-2"}) {
			t.Errorf("%s: assignments %v", why, a)
		}
	}

	// A revocation GitHub does not confirm is retried until it does.
	e := newEnv(t, appCeiling)
	e.mustIssue(ident(trustedBox, write))
	e.gh.set(func(f *fakeGitHub) { f.revFail = 503 })
	if err := e.iss.End(ctx, trustedBox, EndLost); err == nil {
		t.Fatal("an unconfirmed revocation answered nil")
	}
	e.gh.set(func(f *fakeGitHub) { f.revFail = 0 })
	e.clk.add(time.Minute)
	e.iss.Tick(ctx)
	if e.gh.liveCount() != 0 || len(e.iss.old) != 0 {
		t.Errorf("the retried revocation did not happen: %d live", e.gh.liveCount())
	}

	// A box ended during a renewal's grace takes the renewed-away token with it.
	e = newEnv(t, appCeiling)
	e.mustIssue(ident(trustedBox, write))
	e.clk.add(40 * time.Minute)
	e.iss.Tick(ctx)
	if err := e.iss.End(ctx, trustedBox, EndReleased); err != nil {
		t.Fatal(err)
	}
	if e.gh.liveCount() != 0 {
		t.Errorf("%d tokens live after the box ended mid-grace", e.gh.liveCount())
	}
}

func TestEndGrant(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, appCeiling)
	e.mustIssue(ident(trustedBox, write))
	e.grantOf[otherBox.ID] = "g2"
	r := ident(otherBox, write)
	r.Grant = "g1" // a caller's word is ignored: the grant check names g2
	e.mustIssue(r)
	if err := e.iss.EndGrant(ctx, "g1"); err != nil {
		t.Fatal(err)
	}
	if e.gh.liveCount() != 1 || !e.gh.live[e.bx.get(otherBox, "GH_TOKEN")] {
		t.Error("EndGrant(g1) did not revoke g1's token alone")
	}
	if a := e.assigned(); !slices.Equal(a, []string{"box-2"}) {
		t.Errorf("assignments %v", a)
	}
	if ev := e.events(); ev[len(ev)-1] != "revoked:grant_deleted" {
		t.Errorf("ledger %v", ev)
	}
}

// M3: a token renewal never replaced is still revoked at its expiry, and
// retried until expiry plus a margin.
func TestLapsedTokenIsRevoked(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, appCeiling)
	e.mustIssue(ident(trustedBox, write))
	e.gh.set(func(f *fakeGitHub) { f.mintFail = 500 })
	for e.clk.Now().Before(t0.Add(time.Hour)) {
		e.clk.add(time.Minute)
		e.iss.Tick(ctx)
	}
	if e.gh.liveCount() != 0 || len(e.iss.held) != 0 || len(e.assigned()) != 0 {
		t.Errorf("a lapsed token: %d live at GitHub, %d held", e.gh.liveCount(), len(e.iss.held))
	}
	if ev := strings.Join(e.events(), " "); !strings.Contains(ev, "revoked:lapsed") {
		t.Errorf("ledger %s", ev)
	}

	e = newEnv(t, appCeiling)
	e.mustIssue(ident(trustedBox, write))
	e.gh.set(func(f *fakeGitHub) { f.mintFail, f.revFail = 500, 503 })
	for e.clk.Now().Before(t0.Add(time.Hour + lapseMargin + 5*time.Minute)) {
		e.clk.add(time.Minute)
		e.iss.Tick(ctx)
	}
	if len(e.iss.old) != 0 {
		t.Errorf("still retrying %d revocations past the margin", len(e.iss.old))
	}
	ev := e.events()
	if !strings.HasPrefix(ev[len(ev)-1], "expired:revocation never succeeded") {
		t.Errorf("ledger %v", ev)
	}
	_, _, _, _ = e.gh.snapshot()
}

func TestSweepEndsGoneBoxes(t *testing.T) {
	e := newEnv(t, appCeiling)
	for _, b := range []Box{trustedBox, otherBox} {
		e.mustIssue(ident(b, write))
	}
	e.iss.Sweep(context.Background(), func(b Box) bool { return b == otherBox })
	if e.gh.liveCount() != 1 || !e.gh.live[e.bx.get(otherBox, "GH_TOKEN")] {
		t.Errorf("the sweep left %d live, want the live box's", e.gh.liveCount())
	}
}

// M1: nothing is minted after Close.
func TestNoIssueOrRenewalAfterClose(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, appCeiling)
	e.mustIssue(ident(trustedBox, write))
	if err := e.iss.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := e.issue(ident(otherBox, write)); codeOf(err) != api.CodeUnavailable {
		t.Errorf("Issue after Close: %v", err)
	}
	e.clk.add(41 * time.Minute)
	e.iss.Tick(ctx)
	if _, mints, _, _ := e.gh.snapshot(); len(mints) != 1 || e.gh.liveCount() != 0 {
		t.Errorf("after Close: %d mints, %d live", len(mints), e.gh.liveCount())
	}
	// Close kept the assignment, for the next run.
	if a := e.assigned(); !slices.Equal(a, []string{"box-1"}) {
		t.Errorf("assignments after Close %v", a)
	}
}

// M2: a restart issues again to live boxes, drops gone ones, and refuses
// what the config no longer allows; a crash's tokens are settled.
func TestRestart(t *testing.T) {
	ctx := context.Background()
	web := config.SecretsCeiling{Repo: "acme/web", Permissions: write}
	e := newEnv(t, appCeiling, web)
	e.mustIssue(ident(trustedBox, write))
	e.mustIssue(ident(otherBox, write))
	r := ident(thirdBox, write)
	r.Repo = "acme/web"
	e.mustIssue(r)
	if err := e.iss.Close(ctx); err != nil {
		t.Fatal(err)
	}
	_, _, minted, revoked := e.gh.snapshot()
	if len(revoked) != 3 {
		t.Fatalf("Close revoked %d of 3", len(revoked))
	}
	// The next run: box-1 and box-3 live, box-2 gone; acme/web out of the config.
	e.clk.add(time.Minute)
	e.alive = map[string]bool{trustedBox.key(): true, thirdBox.key(): true}
	is := e.cfg.Secrets.Issuers["gh"]
	is.Ceiling = []config.SecretsCeiling{appCeiling}
	e.cfg.Secrets.Issuers["gh"] = is
	e.iss = e.load()
	if err := e.iss.Start(ctx); err != nil {
		t.Fatal(err)
	}
	_, _, minted2, _ := e.gh.snapshot()
	if len(minted2) != 4 || e.bx.get(trustedBox, "GH_TOKEN") != minted2[3] || e.bx.get(trustedBox, "GH_TOKEN") == minted[0] {
		t.Fatalf("the live box did not get a fresh token: %d minted", len(minted2))
	}
	if e.bx.get(otherBox, "GH_TOKEN") != minted[1] || e.bx.get(thirdBox, "GH_TOKEN") != minted[2] {
		t.Error("a gone or refused box was delivered to")
	}
	if a := e.assigned(); !slices.Equal(a, []string{"box-1"}) {
		t.Errorf("assignments after Start %v, want box-1 alone", a)
	}
	if !slices.ContainsFunc(e.bus.list(), func(s string) bool { return strings.HasPrefix(s, "refused:outside") }) {
		t.Errorf("the refused re-issue was not published: %v", e.bus.list())
	}

	// A crash (no Close): the next Start settles the token as expired_unrevoked
	// and issues again.
	held := e.iss.issued(e.iss.held[heldKey(trustedBox, "GH_TOKEN")])
	e.clk.add(time.Second)
	e.iss = e.load()
	if err := e.iss.Start(ctx); err != nil {
		t.Fatal(err)
	}
	var hash string
	_ = e.db.QueryRow(`SELECT hashed_token FROM issuers_ledger WHERE event = 'expired_unrevoked'`).Scan(&hash)
	if hash != held.HashedToken {
		t.Error("the crashed run's token was not settled")
	}
	if out, _ := (SQLLedger{e.db}).Outstanding(ctx, "gh"); len(out) != 1 {
		t.Errorf("outstanding %d, want the new run's one token", len(out))
	}
	if err := e.iss.Start(ctx); err != nil {
		t.Fatal(err)
	}
}

// A GitHub failure at re-issue is retried by Tick.
func TestRestartRetriesAFailedReissue(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, appCeiling)
	e.mustIssue(ident(trustedBox, write))
	_ = e.iss.Close(ctx)
	e.alive[trustedBox.key()] = true
	e.gh.set(func(f *fakeGitHub) { f.mintFail = 502 })
	e.iss = e.load()
	if err := e.iss.Start(ctx); err != nil {
		t.Fatal(err)
	}
	e.gh.set(func(f *fakeGitHub) { f.mintFail = 0 })
	e.clk.add(time.Minute)
	e.iss.Tick(ctx)
	if len(e.iss.held) != 1 || len(e.assigned()) != 1 {
		t.Errorf("held %d after the retry", len(e.iss.held))
	}
}

func TestGrantCeiling(t *testing.T) {
	e := newEnv(t, appCeiling, config.SecretsCeiling{Repo: "acme/*", Permissions: map[string]string{"contents": "read"}},
		config.SecretsCeiling{Repo: "acme/rel", Permissions: map[string]string{"workflows": "write"}, Elevated: []string{"workflows"}, Reason: "r"})
	ok := []string{
		`{"repos":["acme/app"],"permissions":{"contents":"write","pull_requests":"read"}}`,
		`{"repos":["acme/x","acme/*"],"permissions":{"contents":"read"}}`,
		`{"repos":["acme/rel"],"permissions":{"workflows":"write"},"elevated":["workflows"],"reason":"cleared tasks"}`,
	}
	for _, c := range ok {
		if _, err := e.iss.Ceiling(json.RawMessage(c)); err != nil {
			t.Errorf("%s: %v", c, err)
		}
	}
	for _, c := range []string{
		`{"repo":["acme/app"],"permissions":{"contents":"read"}}`,                                    // typo
		`{"repos":["acme/app"],"permissions":{"contents":"read"},"workflow":"x"}`,                    // typo
		`{"repos":[],"permissions":{"contents":"read"}}`,                                             // nothing
		`{"repos":["acme/app"],"permissions":{"secrets":"read"}}`,                                    // denied
		`{"repos":["acme/*"],"permissions":{"contents":"write"}}`,                                    // wider than the config
		`{"repos":["other/app"],"permissions":{"contents":"read"}}`,                                  // not in the config
		`{"repos":["acme/rel"],"permissions":{"workflows":"write"}}`,                                 // elevated unlisted
		`{"repos":["acme/app"],"permissions":{"checks":"write"},"elevated":["checks"],"reason":"x"}`, // config lacks it
	} {
		if _, err := e.iss.Ceiling(json.RawMessage(c)); err == nil {
			t.Errorf("%s: accepted", c)
		}
	}
	ceil := json.RawMessage(ok[0])
	if err := e.iss.Within(ceil, json.RawMessage(`{"repo":"acme/app","permissions":{"contents":"write"}}`)); err != nil {
		t.Errorf("inside: %v", err)
	}
	for _, p := range []string{
		`{"repo":"acme/app","permissions":{"issues":"read"}}`,
		`{"repo":"acme/other","permissions":{"contents":"read"}}`,
		`{"repo":"acme/*","permissions":{"contents":"read"}}`,
		`{"repo":"acme/app","permission":{"contents":"read"}}`,
		`{"repo":"acme/app","permissions":{}}`,
	} {
		if err := e.iss.Within(ceil, json.RawMessage(p)); err == nil {
			t.Errorf("%s: within", p)
		}
	}
	// Params are Request's own JSON.
	b, _ := json.Marshal(ParamsOf(ident(trustedBox, write)))
	if err := e.iss.Within(ceil, b); err != nil {
		t.Errorf("a Request's params: %v", err)
	}
}

// TestCanary: a token value appears in no log line, error, ledger row,
// published event or panic message, whatever path it took.
func TestCanary(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, appCeiling)
	e.mustIssue(ident(trustedBox, write))
	e.bx.set(func(b *boxes) { b.fail = errors.New("refused") })
	e.issue(ident(otherBox, write))
	e.bx.set(func(b *boxes) { b.fail, b.pan = nil, true })
	e.issue(ident(otherBox, write))
	e.bx.set(func(b *boxes) { b.pan = false })
	e.gh.set(func(f *fakeGitHub) { f.broaden, f.revFail = true, 503 })
	e.issue(ident(otherBox, write))
	e.gh.set(func(f *fakeGitHub) { f.broaden, f.revFail = false, 0 })
	e.clk.add(40 * time.Minute)
	e.bx.set(func(b *boxes) { b.fail = errors.New("busy") })
	e.iss.Tick(ctx)
	e.bx.set(func(b *boxes) { b.fail = nil })
	e.clk.add(time.Minute)
	e.iss.Tick(ctx)
	e.gh.set(func(f *fakeGitHub) { f.revFail = 500 })
	if err := e.iss.End(ctx, trustedBox, EndLost); err != nil {
		e.errs = append(e.errs, err.Error())
	}
	e.clk.add(2 * time.Hour)
	e.iss.Tick(ctx)
	e.iss = e.load()
	e.iss.Start(ctx)

	_, _, minted, _ := e.gh.snapshot()
	if len(minted) < 5 {
		t.Fatalf("only %d tokens minted: the canary paths did not run", len(minted))
	}
	pub, _ := json.Marshal(e.bus.ins)
	hay := map[string]string{"logs": e.logs.String(), "errors": strings.Join(e.errs, "\n"), "ledger": strings.Join(e.rows(), "\n"),
		"events": string(pub)}
	for where, text := range hay {
		if strings.Contains(text, canary) {
			t.Errorf("a token value is in the %s:\n%s", where, text)
		}
	}
	if !strings.Contains(hay["errors"], "[secret]") {
		t.Errorf("the deliverer's errors were not scrubbed but dropped? %s", hay["errors"])
	}
}
