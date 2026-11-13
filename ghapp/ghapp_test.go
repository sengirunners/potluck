package ghapp

import (
	"context"
	"crypto"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

var t0 = time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)

type fake struct {
	mu   sync.Mutex
	now  time.Time
	hits map[string]int
	h    http.HandlerFunc
	mint []string // each access_tokens request body, as sent
}

func newKey(t *testing.T) (*rsa.PrivateKey, []byte) {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return k, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)})
}

// setup returns a client against a fake GitHub; h handles non-token routes.
func setup(t *testing.T, h http.HandlerFunc) (*Client, *fake, *rsa.PrivateKey) {
	t.Helper()
	key, pemb := newKey(t)
	f := &fake{now: t0, hits: map[string]int{}, h: h}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mint := strings.HasSuffix(r.URL.Path, "/access_tokens")
		f.mu.Lock()
		if mint {
			body, _ := io.ReadAll(r.Body)
			f.mint = append(f.mint, string(body))
		}
		f.hits[r.Method+" "+r.URL.Path]++
		n := f.hits[r.Method+" "+r.URL.Path]
		now := f.now
		f.mu.Unlock()
		if r.Header.Get("Accept") != "application/vnd.github+json" || r.Header.Get("X-GitHub-Api-Version") != "2022-11-28" {
			t.Errorf("missing headers on %s", r.URL.Path)
		}
		if mint {
			w.WriteHeader(201)
			fmt.Fprintf(w, `{"token":"tok-%d","expires_at":%q}`, n, now.Add(time.Hour).Format(time.RFC3339))
			return
		}
		if f.h != nil {
			f.h(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	c, err := New(Config{AppID: 42, PrivateKeyPEM: pemb, APIURL: srv.URL + "/", HTTP: srv.Client(),
		Now: func() time.Time { f.mu.Lock(); defer f.mu.Unlock(); return f.now }})
	if err != nil {
		t.Fatal(err)
	}
	return c, f, key
}

func TestJWT(t *testing.T) {
	c, _, key := setup(t, nil)
	tok, err := c.jwt()
	if err != nil {
		t.Fatal(err)
	}
	p := strings.Split(tok, ".")
	if len(p) != 3 {
		t.Fatal("not 3 parts")
	}
	sig, _ := base64.RawURLEncoding.DecodeString(p[2])
	sum := sha256.Sum256([]byte(p[0] + "." + p[1]))
	if err := rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, sum[:], sig); err != nil {
		t.Fatalf("bad signature: %v", err)
	}
	var hdr map[string]string
	hb, _ := base64.RawURLEncoding.DecodeString(p[0])
	json.Unmarshal(hb, &hdr)
	if hdr["alg"] != "RS256" {
		t.Fatalf("alg %v", hdr)
	}
	var cl map[string]int64
	cb, _ := base64.RawURLEncoding.DecodeString(p[1])
	json.Unmarshal(cb, &cl)
	if cl["iss"] != 42 || cl["iat"] != t0.Add(-time.Minute).Unix() || cl["exp"] != t0.Add(9*time.Minute).Unix() {
		t.Fatalf("claims %v", cl)
	}
}

func TestNewKeyErrors(t *testing.T) {
	_, err := New(Config{AppID: 1, APIURL: "http://x", PrivateKeyPEM: []byte("not pem SECRETBYTES")})
	if err == nil || strings.Contains(err.Error(), "SECRETBYTES") {
		t.Fatalf("err %v", err)
	}
	k, _ := newKey(t)
	der, _ := x509.MarshalPKCS8PrivateKey(k)
	if _, err := New(Config{AppID: 1, APIURL: "http://x", PrivateKeyPEM: pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})}); err != nil {
		t.Fatalf("pkcs8: %v", err)
	}
}

var rp = Perms{"actions": "read", "metadata": "read"}

func TestTokenCache(t *testing.T) {
	c, f, _ := setup(t, nil)
	ctx := context.Background()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); c.InstallationToken(ctx, 7, "acme/web", rp) }()
	}
	wg.Wait()
	if n := f.hits["POST /app/installations/7/access_tokens"]; n != 1 {
		t.Fatalf("concurrent fetches: %d", n)
	}
	f.mu.Lock()
	f.now = t0.Add(54 * time.Minute) // still 6 min left
	f.mu.Unlock()
	if tok, _ := c.InstallationToken(ctx, 7, "acme/web", Perms{"metadata": "read", "actions": "read"}); tok != "tok-1" {
		t.Fatalf("expected cached, got %s", tok)
	}
	f.mu.Lock()
	f.now = t0.Add(56 * time.Minute) // under 5 min left
	f.mu.Unlock()
	if tok, _ := c.InstallationToken(ctx, 7, "acme/web", rp); tok != "tok-2" {
		t.Fatalf("expected refresh, got %s", tok)
	}
	if tok, _ := c.InstallationToken(ctx, 8, "acme/web", rp); tok != "tok-1" {
		t.Fatalf("separate installation: %s", tok)
	}
	// Another repository or other permissions is another token, never the cached one.
	if tok, _ := c.InstallationToken(ctx, 7, "acme/api", rp); tok != "tok-3" {
		t.Fatalf("separate repo: %s", tok)
	}
	if tok, _ := c.InstallationToken(ctx, 7, "acme/web", Perms{"administration": "write", "metadata": "read"}); tok != "tok-4" {
		t.Fatalf("separate permissions: %s", tok)
	}
	for _, bad := range []struct {
		repo string
		p    Perms
	}{{"", rp}, {"acme", rp}, {"acme/../x", rp}, {"acme/web", nil}, {"acme/web", Perms{"actions": "admin"}}, {"acme/web", Perms{"a b": "read"}}} {
		if _, err := c.InstallationToken(ctx, 7, bad.repo, bad.p); err == nil {
			t.Errorf("minted for %q %v", bad.repo, bad.p)
		}
	}
	if n := f.hits["POST /app/installations/7/access_tokens"]; n != 4 {
		t.Fatalf("a refused request reached GitHub: %d mints", n)
	}
}

// Every token the client mints names exactly the call's one repository (by
// name, as GitHub wants it) and only the permissions that call needs: a leaked
// token never reaches the installation's other repositories, and only a repo
// runner's token carries Administration write (F3, zm-eakh.12.7.3.1).
func TestTokenRequestBody(t *testing.T) {
	c, f, _ := setup(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/generate-jitconfig"):
			w.WriteHeader(201)
			fmt.Fprint(w, `{"runner":{"id":1},"encoded_jit_config":"E"}`)
		case r.Method == "DELETE":
			w.WriteHeader(204)
		case strings.HasSuffix(r.URL.Path, "/actions/runners"):
			fmt.Fprint(w, `{"total_count":0,"runners":[]}`)
		default:
			fmt.Fprint(w, `{"id":1}`)
		}
	})
	ctx := context.Background()
	read := `{"permissions":{"actions":"read","metadata":"read"},"repositories":["web"]}`
	repoRunners := `{"permissions":{"administration":"write","metadata":"read"},"repositories":["app"]}`
	user := Scope{Repo: "bob/app"}
	cases := []struct {
		name string
		inst int64
		call func(inst int64) error
		want string
	}{
		{"repo", 1, func(i int64) error { _, err := c.Repo(ctx, i, "acme/web"); return err }, read},
		{"run", 2, func(i int64) error { _, err := c.Run(ctx, i, "acme/web", 9); return err }, read},
		{"job", 3, func(i int64) error { _, err := c.Job(ctx, i, "acme/web", 3); return err }, read},
		{"repo jit", 7, func(i int64) error { _, _, err := c.JITConfig(ctx, i, user, "zm-1", nil, 0); return err }, repoRunners},
		{"repo list", 8, func(i int64) error { _, err := c.Runners(ctx, i, user, "zm-"); return err }, repoRunners},
		{"repo delete", 9, func(i int64) error { return c.DeleteRunner(ctx, i, user, 1) }, repoRunners},
	}
	for _, tc := range cases {
		f.mu.Lock()
		f.mint = nil
		f.mu.Unlock()
		if err := tc.call(tc.inst); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		f.mu.Lock()
		got := f.mint
		f.mu.Unlock()
		if len(got) != 1 || got[0] != tc.want {
			t.Errorf("%s: minted with %q, want exactly %s", tc.name, got, tc.want)
		}
	}
}

func TestReads(t *testing.T) {
	c, _, _ := setup(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/repos/acme/web") && r.Header.Get("Authorization") != "Bearer tok-1" {
			t.Errorf("auth %q", r.Header.Get("Authorization"))
		}
		switch r.URL.Path {
		case "/app/installations/5":
			if strings.Count(r.Header.Get("Authorization"), ".") != 2 {
				t.Error("expected JWT")
			}
			fmt.Fprint(w, `{"id":5,"account":{"login":"acme","type":"Organization"}}`)
		case "/repos/acme/web":
			fmt.Fprint(w, `{"id":4401,"full_name":"acme/web","private":true,"visibility":"private","fork":false}`)
		case "/repos/acme/noid":
			fmt.Fprint(w, `{"full_name":"acme/noid","private":true}`)
		case "/repos/acme/web/actions/runs/9":
			fmt.Fprint(w, `{"id":9,"event":"pull_request","status":"completed","conclusion":"failure","run_attempt":2,"head_repository":{"full_name":"bob/web","fork":true},"repository":{"full_name":"acme/web"}}`)
		case "/repos/acme/web/actions/jobs/3":
			fmt.Fprint(w, `{"id":3,"status":"completed","conclusion":null,"runner_name":"zm-1","run_id":9,"run_attempt":2}`)
		default:
			w.WriteHeader(404)
		}
	})
	ctx := context.Background()
	if in, err := c.Installation(ctx, 5); err != nil || in != (Installation{5, "acme", "Organization"}) {
		t.Fatalf("%v %+v", err, in)
	}
	if r, err := c.Repo(ctx, 5, "acme/web"); err != nil || r != (Repo{4401, "acme/web", true, "private", false, ""}) {
		t.Fatalf("%v %+v", err, r)
	}
	run, err := c.Run(ctx, 5, "acme/web", 9)
	if err != nil || run != (Run{ID: 9, Event: "pull_request", Status: "completed", Conclusion: "failure", Attempt: 2, HeadRepo: "bob/web", Repo: "acme/web", CallsOwnOnly: true}) || !run.FromFork() {
		t.Fatalf("%v %+v", err, run)
	}
	if j, err := c.Job(ctx, 5, "acme/web", 3); err != nil || j != (Job{ID: 3, Status: "completed", RunnerName: "zm-1", RunID: 9, RunAttempt: 2}) {
		t.Fatalf("%v %+v", err, j)
	}
	if _, err := c.Repo(ctx, 5, "acme/noid"); err == nil || !strings.Contains(err.Error(), "no repository id") {
		t.Fatalf("an answer without an id: %v", err)
	}
	if _, err := c.Repo(ctx, 5, "acme/../x"); err == nil {
		t.Fatal("expected bad repo name error")
	}
}

// FromFork asks whether the head repository is the repository, never whether
// it is a fork: a repository that is itself a fork (its every run's head
// repository has fork true, which Run no longer reads) is served for its own
// runs, of any event; a run whose head is another repository, or a
// pull_request* run with none, is from outside. Events are trusted's, not
// FromFork's.
func TestFromFork(t *testing.T) {
	cases := []struct {
		r    Run
		want bool
	}{
		{Run{Event: "push", HeadRepo: "a/b", Repo: "a/b"}, false},
		{Run{Event: "workflow_dispatch", HeadRepo: "a/b", Repo: "a/b"}, false},
		{Run{Event: "schedule", HeadRepo: "a/b", Repo: "a/b"}, false},
		{Run{Event: "pull_request", HeadRepo: "a/b", Repo: "a/b"}, false},
		{Run{Event: "pull_request", HeadRepo: "A/B", Repo: "a/b"}, false},
		{Run{Event: "pull_request", HeadRepo: "c/b", Repo: "a/b"}, true},
		{Run{Event: "push", HeadRepo: "c/b", Repo: "a/b"}, true},
		{Run{Event: "pull_request_target", HeadRepo: "c/b", Repo: "a/b"}, true},
		{Run{Event: "pull_request_target", Repo: "a/b"}, true},
		{Run{Event: "pull_request", Repo: "a/b"}, true},
		{Run{Event: "push", Repo: "a/b"}, false},
	}
	for i, tc := range cases {
		if got := tc.r.FromFork(); got != tc.want {
			t.Errorf("case %d %+v: %v", i, tc.r, got)
		}
	}
}

// Run reads a repository that is itself a fork: GitHub's head_repository
// says fork true for its own runs, and they are not from a fork.
func TestRunOfARepoThatIsAFork(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		event, head string
		want        bool
	}{
		{"push", `{"full_name":"alice/widget","fork":true}`, false},
		{"workflow_dispatch", `{"full_name":"alice/widget","fork":true}`, false},
		{"pull_request", `{"full_name":"ALICE/Widget","fork":true}`, false},
		{"pull_request", `{"full_name":"stranger/worm","fork":true}`, true},
		{"pull_request", `{"full_name":"CapSoftware/Cap","fork":false}`, true},
		{"pull_request", `null`, true},
	} {
		c, _, _ := setup(t, func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/actions/runs/9") {
				fmt.Fprintf(w, `{"id":9,"event":%q,"status":"in_progress","run_attempt":1,"head_repository":%s,"repository":{"full_name":"alice/widget"}}`, tc.event, tc.head)
			}
		})
		run, err := c.Run(ctx, 5, "alice/widget", 9)
		if err != nil {
			t.Fatal(err)
		}
		if run.FromFork() != tc.want {
			t.Errorf("%s with head %s: FromFork %v, want %v", tc.event, tc.head, !tc.want, tc.want)
		}
	}
}

func TestJITConfig(t *testing.T) {
	var paths []string
	var body map[string]any
	c, _, _ := setup(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			return
		}
		paths = append(paths, r.URL.Path)
		if strings.Contains(r.URL.Path, "repos/acme/web") {
			json.NewDecoder(r.Body).Decode(&body)
			w.WriteHeader(201)
			fmt.Fprint(w, `{"runner":{"id":77},"encoded_jit_config":"ENC"}`)
			return
		}
		w.WriteHeader(403)
		fmt.Fprint(w, `{"message":"Resource not accessible by integration"}`)
	})
	ctx := context.Background()
	id, enc, err := c.JITConfig(ctx, 1, Scope{Repo: "acme/web"}, "zm-1", []string{"self-hosted", "zm"}, 0)
	if err != nil || id != 77 || enc != "ENC" {
		t.Fatalf("%v %d %s", err, id, enc)
	}
	if body["name"] != "zm-1" || body["runner_group_id"] != float64(1) || body["work_folder"] != "_work" ||
		fmt.Sprint(body["labels"]) != "[self-hosted zm]" {
		t.Fatalf("body %v", body)
	}
	if paths[0] != "/repos/acme/web/actions/runners/generate-jitconfig" {
		t.Fatalf("path %v", paths)
	}
	_, _, err = c.JITConfig(ctx, 1, Scope{Repo: "bob/web"}, "zm-2", nil, 3)
	if err == nil || !strings.Contains(err.Error(), "403 Resource not accessible by integration") ||
		!strings.Contains(err.Error(), "/repos/bob/web/actions/runners/generate-jitconfig") || strings.Contains(err.Error(), "tok-") {
		t.Fatalf("err %v", err)
	}
}

func TestRunnersAndDelete(t *testing.T) {
	c, _, _ := setup(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "GET" && r.URL.Path == "/repos/acme/web/actions/runners":
			var rs []string
			if r.URL.Query().Get("page") == "1" {
				for i := 0; i < 100; i++ {
					n := "other"
					if i%2 == 0 {
						n = "zm-a"
					}
					rs = append(rs, fmt.Sprintf(`{"id":%d,"name":"%s%d","status":"online","busy":true}`, i, n, i))
				}
			} else if r.URL.Query().Get("page") == "2" {
				rs = append(rs, `{"id":200,"name":"zm-last","status":"offline","busy":false}`)
			}
			fmt.Fprintf(w, `{"total_count":101,"runners":[%s]}`, strings.Join(rs, ","))
		case r.Method == "DELETE" && r.URL.Path == "/repos/acme/web/actions/runners/5":
			w.WriteHeader(404)
			fmt.Fprint(w, `{"message":"Not Found"}`)
		case r.Method == "DELETE" && r.URL.Path == "/repos/acme/web/actions/runners/6":
			w.WriteHeader(204)
		case r.Method == "DELETE":
			w.WriteHeader(422)
		}
	})
	ctx := context.Background()
	rs, err := c.Runners(ctx, 1, Scope{Repo: "acme/web"}, "zm-")
	if err != nil || len(rs) != 51 || rs[50] != (Runner{200, "zm-last", "offline", false}) || !rs[0].Busy {
		t.Fatalf("%v %d", err, len(rs))
	}
	for _, id := range []int64{5, 6} {
		if err := c.DeleteRunner(ctx, 1, Scope{Repo: "acme/web"}, id); err != nil {
			t.Fatalf("delete %d: %v", id, err)
		}
	}
	if err := c.DeleteRunner(ctx, 1, Scope{Repo: "acme/web"}, 7); err == nil {
		t.Fatal("expected 422 error")
	}
}

func TestScopeValidation(t *testing.T) {
	good := map[Scope]string{
		{Repo: "acme-co/web"}: "repos/acme-co/web",
		{Repo: "bob/web.1"}:   "repos/bob/web.1",
	}
	for s, want := range good {
		if s.validate() != nil || s.path() != want {
			t.Errorf("%+v: %q", s, s.path())
		}
	}
	bad := []Scope{{}, {Repo: "../c"}, {Repo: "a b/c"}, {Repo: "a..b/c"},
		{Repo: "x"}, {Repo: "a/.."}, {Repo: "../b"}, {Repo: "a/b/c"}, {Repo: "a/b?x=1"}}
	for _, s := range bad {
		if s.validate() == nil || s.path() != "" {
			t.Errorf("accepted %+v", s)
		}
	}
	c, _, _ := setup(t, nil)
	if _, _, err := c.JITConfig(context.Background(), 1, Scope{Repo: "../x/y"}, "n", nil, 0); err == nil {
		t.Fatal("JITConfig accepted bad scope")
	}
}

func TestUserInstallations(t *testing.T) {
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		var ids []string
		if r.URL.Query().Get("page") == "1" {
			for i := 1; i <= 100; i++ {
				ids = append(ids, fmt.Sprintf(`{"id":%d}`, i))
			}
		} else {
			ids = append(ids, `{"id":500}`)
		}
		fmt.Fprintf(w, `{"total_count":101,"installations":[%s]}`, strings.Join(ids, ","))
	}))
	defer srv.Close()
	ids, err := UserInstallations(context.Background(), nil, srv.URL, "utok")
	if err != nil || len(ids) != 101 || ids[100] != 500 || auth != "Bearer utok" {
		t.Fatalf("%v %d %q", err, len(ids), auth)
	}
}

func TestUserOrgAdmin(t *testing.T) {
	// GitHub's answers for GET /user/memberships/orgs/{org} (docs: 404 when
	// the user is not affiliated; 403 when the App may not ask).
	for _, tc := range []struct {
		name, body string
		status     int
		admin      bool
		noMembers  bool // ErrNoMembersRead
		err        bool
	}{
		{"active admin", `{"state":"active","role":"admin"}`, 200, true, false, false},
		{"active member", `{"state":"active","role":"member"}`, 200, false, false, false},
		{"pending admin", `{"state":"pending","role":"admin"}`, 200, false, false, false},
		{"not a member", `{"message":"Not Found","status":"404"}`, 404, false, false, false},
		{"App lacks Members read", `{"message":"Resource not accessible by integration","status":"403"}`, 403, false, true, true},
		{"org blocks the App", `{"message":"Forbidden","status":"403"}`, 403, false, false, true},
		{"GitHub down", `{}`, 502, false, false, true},
	} {
		var path, auth string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			path, auth = r.URL.Path, r.Header.Get("Authorization")
			w.WriteHeader(tc.status)
			fmt.Fprint(w, tc.body)
		}))
		admin, err := UserOrgAdmin(context.Background(), nil, srv.URL, "utok", "acme")
		srv.Close()
		if admin != tc.admin || (err != nil) != tc.err || errors.Is(err, ErrNoMembersRead) != tc.noMembers {
			t.Errorf("%s: admin %v err %v", tc.name, admin, err)
		}
		if path != "/user/memberships/orgs/acme" || auth != "Bearer utok" {
			t.Errorf("%s: asked %s with %q", tc.name, path, auth)
		}
		if tc.noMembers && !strings.Contains(err.Error(), "Members: Read-only") {
			t.Errorf("%s: error does not say how to add the permission: %v", tc.name, err)
		}
	}
	if _, err := UserOrgAdmin(context.Background(), nil, "http://127.0.0.1:1", "utok", "../x"); err == nil {
		t.Error("an invalid org name was asked")
	}
}

func TestManifestJSON(t *testing.T) {
	// Runners register per repository, so every App asks for Administration
	// (each token limits it to one repository's runners) and never for the
	// org's self-hosted runners. Members read is the link proof's (an org
	// admin's GET /user/memberships/orgs/{org}).
	for _, tc := range []struct {
		public bool
		runner []string
	}{
		{false, []string{"administration"}},
		{true, []string{"administration"}},
	} {
		b, err := Manifest{Name: "zm", URL: "https://u", HookURL: "https://u/h", Public: tc.public}.JSON()
		if err != nil {
			t.Fatal(err)
		}
		var m struct {
			Perms map[string]string `json:"default_permissions"`
		}
		_ = json.Unmarshal(b, &m)
		want := map[string]string{"actions": "write", "metadata": "read", "members": "read", "contents": "read"}
		for _, p := range tc.runner {
			want[p] = "write"
		}
		if fmt.Sprint(m.Perms) != fmt.Sprint(want) {
			t.Errorf("public=%v: perms %v, want %v", tc.public, m.Perms, want)
		}
	}
	b, err := Manifest{Name: "zm", URL: "https://zm.example", HookURL: "https://zm.example/hook",
		RedirectURL: "https://zm.example/r", CallbackURLs: []string{"https://zm.example/cb"}}.JSON()
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	hook := m["hook_attributes"].(map[string]any)
	if hook["url"] != "https://zm.example/hook" || hook["active"] != true ||
		fmt.Sprint(m["default_events"]) != "[workflow_job]" ||
		m["request_oauth_on_install"] != false || m["setup_on_update"] != false || m["public"] != false ||
		m["name"] != "zm" || fmt.Sprint(m["callback_urls"]) != "[https://zm.example/cb]" {
		t.Fatalf("manifest %s", b)
	}
	if _, err := (Manifest{Name: "x"}).JSON(); err == nil {
		t.Fatal("expected error for missing fields")
	}
}

func TestConvert(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("convert must be unauthenticated")
		}
		if r.Method == "POST" && r.URL.Path == "/app-manifests/goodcode/conversions" {
			w.WriteHeader(201)
			fmt.Fprint(w, `{"id":9,"slug":"zm-app","owner":{"login":"acme"},"client_id":"cid","client_secret":"cs","webhook_secret":"ws","pem":"PEMDATA"}`)
			return
		}
		w.WriteHeader(404)
		fmt.Fprint(w, `{"message":"Not Found"}`)
	}))
	defer srv.Close()
	ctx := context.Background()
	got, err := Convert(ctx, srv.Client(), srv.URL, "goodcode")
	if err != nil || got.ID != 9 || got.Slug != "zm-app" || got.Owner != "acme" || got.ClientID != "cid" ||
		got.ClientSecret != "cs" || got.WebhookSecret != "ws" || string(got.PEM) != "PEMDATA" {
		t.Fatalf("%v %+v", err, got)
	}
	if _, err := Convert(ctx, srv.Client(), srv.URL, "unknown"); err == nil || !strings.Contains(err.Error(), "404 Not Found") {
		t.Fatalf("err %v", err)
	}
	if _, err := Convert(ctx, srv.Client(), srv.URL, "../x"); err == nil {
		t.Fatal("bad code accepted")
	}
}

func TestVerifyHook(t *testing.T) {
	secret, body := []byte("s3cret"), []byte(`{"action":"queued"}`)
	m := hmac.New(sha256.New, secret)
	m.Write(body)
	sig := hex.EncodeToString(m.Sum(nil))
	if !VerifyHook(secret, body, "sha256="+sig) {
		t.Error("good signature rejected")
	}
	for name, h := range map[string]string{"bad": "sha256=" + strings.Repeat("0", 64), "noprefix": sig,
		"sha1": "sha1=" + sig, "empty": "", "nothex": "sha256=zz"} {
		if VerifyHook(secret, body, h) {
			t.Errorf("%s accepted", name)
		}
	}
	if VerifyHook([]byte("other"), body, "sha256="+sig) || VerifyHook(secret, []byte("x"), "sha256="+sig) || VerifyHook(nil, body, "sha256="+sig) {
		t.Error("wrong secret/body accepted")
	}
}

// A re-run is one POST with a token for Actions write on the job's
// repository; GitHub refusing the permission (the token, or the call) is
// ErrNoActionsWrite, anything else its own error.
func TestRerun(t *testing.T) {
	granted := true
	var posts []string
	_, pemb := newKey(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/app/installations/5/access_tokens":
			var b struct {
				Permissions  map[string]string `json:"permissions"`
				Repositories []string          `json:"repositories"`
			}
			_ = json.NewDecoder(r.Body).Decode(&b)
			if len(b.Repositories) == 1 && b.Repositories[0] == "gone" {
				w.WriteHeader(422)
				fmt.Fprint(w, `{"message":"There is at least one repository that does not exist or is not accessible to the parent installation."}`)
				return
			}
			if b.Permissions["actions"] == "write" && !granted {
				w.WriteHeader(422)
				fmt.Fprint(w, `{"message":"The permissions requested are not granted to this installation."}`)
				return
			}
			w.WriteHeader(201)
			fmt.Fprintf(w, `{"token":"tok","expires_at":%q}`, time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
		case r.Method == "POST" && r.URL.Path == "/repos/acme/web/actions/jobs/4/rerun":
			w.WriteHeader(403)
			fmt.Fprint(w, `{"message":"Resource not accessible by integration"}`)
		case r.Method == "POST" && r.URL.Path == "/repos/acme/web/actions/jobs/5/rerun":
			w.WriteHeader(403)
			fmt.Fprint(w, `{"message":"This workflow is already running"}`)
		case r.Method == "POST":
			posts = append(posts, r.URL.Path)
			w.WriteHeader(201)
			fmt.Fprint(w, `{}`)
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(srv.Close)
	c, err := New(Config{AppID: 42, PrivateKeyPEM: pemb, APIURL: srv.URL, HTTP: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := c.RerunJob(ctx, 5, "acme/web", 3); err != nil {
		t.Fatal(err)
	}
	if err := c.RerunFailedJobs(ctx, 5, "acme/web", 9); err != nil {
		t.Fatal(err)
	}
	if want := []string{"/repos/acme/web/actions/jobs/3/rerun", "/repos/acme/web/actions/runs/9/rerun-failed-jobs"}; fmt.Sprint(posts) != fmt.Sprint(want) {
		t.Errorf("posted %v, want %v", posts, want)
	}
	if err := c.RerunJob(ctx, 5, "acme/web", 4); !errors.Is(err, ErrNoActionsWrite) {
		t.Errorf("a call GitHub refuses the integration: %v", err)
	}
	if err := c.RerunJob(ctx, 5, "acme/web", 5); err == nil || errors.Is(err, ErrNoActionsWrite) {
		t.Errorf("a run still running: %v", err)
	}
	if err := c.RerunJob(ctx, 5, "acme/gone", 3); err == nil || errors.Is(err, ErrNoActionsWrite) {
		t.Errorf("a repository outside the installation: %v", err)
	}
	if _, err := c.Job(ctx, 5, "acme/web", 404); !NotFound(err) {
		t.Errorf("a job GitHub does not have: %v", err)
	}
	granted = false
	if err := c.RerunJob(ctx, 5, "acme/lib", 3); !errors.Is(err, ErrNoActionsWrite) {
		t.Errorf("a token GitHub does not grant: %v", err)
	}
}

// Public: only a private repository whose visibility is private (or unsaid)
// does not count; internal, public and any visibility this version does not
// know do.
func TestPublic(t *testing.T) {
	for _, c := range []struct {
		private    bool
		visibility string
		want       bool
	}{
		{true, "private", false},
		{true, "", false},
		{true, "internal", true},
		{false, "public", true},
		{false, "", true},
		{false, "private", true},
		{true, "restricted", true},
	} {
		if got := Public(c.private, c.visibility); got != c.want {
			t.Errorf("Public(%v, %q) = %v, want %v", c.private, c.visibility, got, c.want)
		}
	}
}

func TestAudience(t *testing.T) {
	for _, c := range []struct {
		private          bool
		visibility, kind string
		who              string
	}{
		{true, "private", "private", ""},
		{true, "internal", "internal", "everyone in its enterprise"},
		{false, "public", "public", "anyone"},
		{false, "", "public", "anyone"},
		{true, "restricted", "restricted", "anyone"},
	} {
		if kind, who := Audience(c.private, c.visibility); kind != c.kind || who != c.who {
			t.Errorf("Audience(%v, %q) = %q, %q; want %q, %q", c.private, c.visibility, kind, who, c.kind, c.who)
		}
	}
}

func TestArticle(t *testing.T) {
	for kind, want := range map[string]string{"public": "a", "private": "a", "internal": "an", "unlisted": "an"} {
		if got := Article(kind); got != want {
			t.Errorf("Article(%q) = %q, want %q", kind, got, want)
		}
	}
}
