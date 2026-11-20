package wiring_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/server"
	"github.com/sengirunners/potluck/wiring"
)

// lookupGitHub is a GitHub that refuses unauthenticated lookups (as when
// the address's 60 an hour are spent) and hands out installation tokens of
// one App, owned by owner and installed on the org acme (installation 7),
// not on the org solo. A token in revoked is answered 401; with suspended,
// installation 7's tokens are refused (403).
type lookupGitHub struct {
	t   *testing.T
	srv *httptest.Server

	mu        sync.Mutex
	mints     []string          // each access_tokens body GitHub answered
	auth      map[string]string // route -> the Authorization it last carried
	hits      map[string]int    // route -> requests
	revoked   map[string]bool
	suspended bool
}

func newLookupGitHub(t *testing.T, owner string) *lookupGitHub {
	g := &lookupGitHub{t: t, auth: map[string]string{}, hits: map[string]int{}, revoked: map[string]bool{}}
	accounts := map[string]string{
		"ana":  `{"id":101,"login":"ana","type":"User"}`,
		"bob":  `{"id":102,"login":"bob","type":"User"}`,
		"acme": `{"id":200,"login":"acme","type":"Organization"}`,
		"solo": `{"id":300,"login":"solo","type":"Organization"}`,
	}
	g.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		g.mu.Lock()
		g.auth[r.Method+" "+r.URL.Path] = auth
		g.hits[r.Method+" "+r.URL.Path]++
		suspended := g.suspended
		g.mu.Unlock()
		path := r.URL.Path
		switch {
		case r.Method == http.MethodGet && path == "/app":
			fmt.Fprintf(w, `{"id":42,"owner":{"login":%q}}`, owner)
		case r.Method == http.MethodGet && path == "/orgs/acme/installation":
			fmt.Fprint(w, `{"id":7}`)
		case r.Method == http.MethodGet && strings.HasSuffix(path, "/installation"):
			http.NotFound(w, r)
		case r.Method == http.MethodPost && path == "/app/installations/7/access_tokens" && suspended:
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, `{"message":"This installation has been suspended"}`)
		case r.Method == http.MethodPost && path == "/app/installations/7/access_tokens":
			var body struct {
				Permissions map[string]string `json:"permissions"`
			}
			b := new(bytes.Buffer)
			b.ReadFrom(r.Body)
			g.mu.Lock()
			g.mints = append(g.mints, b.String())
			n := len(g.mints)
			g.mu.Unlock()
			json.Unmarshal(b.Bytes(), &body)
			tok := fmt.Sprintf("app-lookup-tok-%d", n)
			if body.Permissions["members"] == "read" {
				tok = fmt.Sprintf("app-members-tok-%d", n)
			}
			w.WriteHeader(http.StatusCreated)
			fmt.Fprintf(w, `{"token":%q,"expires_at":%q}`, tok, time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
		case r.Method == http.MethodGet && strings.HasPrefix(path, "/users/"):
			if auth == "" {
				w.Header().Set("X-RateLimit-Remaining", "0")
				w.Header().Set("X-RateLimit-Reset", fmt.Sprint(time.Now().Add(time.Hour).Unix()))
				w.WriteHeader(http.StatusForbidden)
				return
			}
			g.mu.Lock()
			revoked := g.revoked[strings.TrimPrefix(auth, "Bearer ")]
			g.mu.Unlock()
			if revoked {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			u, ok := accounts[strings.TrimPrefix(path, "/users/")]
			if !ok {
				http.NotFound(w, r)
				return
			}
			fmt.Fprint(w, u)
		case r.Method == http.MethodGet && path == "/orgs/acme/members/acme":
			g.mu.Lock()
			revoked := g.revoked[strings.TrimPrefix(auth, "Bearer ")]
			g.mu.Unlock()
			switch {
			case revoked:
				w.WriteHeader(http.StatusUnauthorized)
				return
			case !strings.HasPrefix(auth, "Bearer app-members-tok-"):
				w.WriteHeader(http.StatusFound) // not a token that sees acme's members
				return
			}
			w.WriteHeader(http.StatusNotFound) // seen: acme is not its own member
		case r.Method == http.MethodGet && strings.HasPrefix(path, "/orgs/"):
			w.WriteHeader(http.StatusFound)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(g.srv.Close)
	return g
}

func (g *lookupGitHub) hitsOf(route string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.hits[route]
}

func (g *lookupGitHub) authOf(route string) string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.auth[route]
}

// secretFile writes v to a mode 600 file in dir: a file: secret reference.
func secretFile(t *testing.T, dir, name string, v []byte) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, v, 0o600); err != nil {
		t.Fatal(err)
	}
	return "file:" + p
}

// startOwners runs a KIT daemon whose GitHub is gh, with edit applied to
// its config.json, and a project "base" its socket owns.
func startOwners(t *testing.T, gh *lookupGitHub, logs *bytes.Buffer, edit func(home string, f *config.File)) *client.Client {
	t.Helper()
	home, err := os.MkdirTemp("", "zm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	cfg, err := config.LoadFrom(home)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Human, cfg.Machine = "ana", "kit"
	cfg.File.GitHub.APIURL = gh.srv.URL
	edit(home, &cfg.File)
	ctx, cancel := context.WithCancel(context.Background())
	dm, err := server.New(ctx, cfg, slog.New(slog.NewTextHandler(logs, nil)), wiring.Kit)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- dm.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("the daemon did not stop")
		}
	})
	c := client.NewUnix(cfg.SocketPath())
	if err := c.WaitReady(ctx, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if _, err := c.CreateProject(ctx, api.NewProjectRequest{Slug: "base"}); err != nil {
		t.Fatal(err)
	}
	return c
}

// startWithApp runs startOwners with runners.github set to the App gh
// serves (a new key) and no github.lookup_token.
func startWithApp(t *testing.T, gh *lookupGitHub, logs *bytes.Buffer) *client.Client {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pemKey := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	return startOwners(t, gh, logs, func(home string, f *config.File) {
		f.Runners.GitHub = config.RunnersGitHub{AppID: 42, PrivateKey: secretFile(t, home, "app.pem", pemKey), APIURL: gh.srv.URL + "/"}
	})
}

// With no github.lookup_token, an authority that has the runner broker's
// App looks GitHub logins up with that App's installation tokens: metadata
// read from the App owner's installation for a login, members read from
// the org's own installation for an org's members. An org the App is not
// installed on cannot be granted, and says how to fix it. No token or key
// reaches the log.
func TestGrantsLookUpWithTheRunnersApp(t *testing.T) {
	gh := newLookupGitHub(t, "acme")
	var logs bytes.Buffer
	c := startWithApp(t, gh, &logs)
	ctx := context.Background()

	if g, err := c.GrantGitHub(ctx, api.GitHubGrantRequest{Owner: "@ana", Role: "member", Project: "base"}); err != nil {
		t.Fatalf("grant @ana: %+v, %v", g, err)
	}
	if a := gh.authOf("GET /users/ana"); !strings.HasPrefix(a, "Bearer app-lookup-tok-") {
		t.Fatalf("@ana was looked up with %q, want the App's lookup token", a)
	}
	if g, err := c.GrantGitHub(ctx, api.GitHubGrantRequest{Owner: "@acme", Role: "member", Project: "base"}); err != nil {
		t.Fatalf("grant @acme: %+v, %v", g, err)
	}
	if a := gh.authOf("GET /orgs/acme/members/acme"); !strings.HasPrefix(a, "Bearer app-members-tok-") {
		t.Fatalf("acme's members were read with %q, want the App's members token", a)
	}
	_, err := c.GrantGitHub(ctx, api.GitHubGrantRequest{Owner: "@solo", Role: "member", Project: "base"})
	if ae, ok := err.(*api.Error); !ok || ae.Code != api.CodeForbidden || !strings.Contains(ae.Hint, "runners.github's App installed on @solo") {
		t.Fatalf("grant @solo (App not installed): %v, want forbidden with the App hint", err)
	}

	gh.mu.Lock()
	mints := append([]string(nil), gh.mints...)
	gh.mu.Unlock()
	perms := map[string]bool{}
	for _, m := range mints {
		var body map[string]json.RawMessage
		if err := json.Unmarshal([]byte(m), &body); err != nil {
			t.Fatal(err)
		}
		if _, ok := body["repositories"]; ok {
			t.Errorf("a lookup token named repositories: %s", m)
		}
		perms[string(body["permissions"])] = true
	}
	if len(perms) != 2 || !perms[`{"metadata":"read"}`] || !perms[`{"members":"read","metadata":"read"}`] {
		t.Fatalf("minted with %v, want metadata read and members+metadata read", perms)
	}
	if len(mints) != 2 {
		t.Fatalf("%d tokens minted, want each cached after its first mint", len(mints))
	}
	for _, s := range []string{"app-lookup-tok", "app-members-tok", "PRIVATE KEY", "not installed on its owner"} {
		if strings.Contains(logs.String(), s) {
			t.Fatalf("the log has %q", s)
		}
	}
}

// github.lookup_token, when set, is the lookup token even when the runner
// broker's App is configured: the App is never asked for one.
func TestLookupTokenWinsOverTheRunnersApp(t *testing.T) {
	gh := newLookupGitHub(t, "acme")
	var logs bytes.Buffer
	c := startOwners(t, gh, &logs, func(home string, f *config.File) {
		f.GitHub.LookupToken = secretFile(t, home, "lookup", []byte("static-tok\n"))
		f.Runners.GitHub = config.RunnersGitHub{AppID: 42, PrivateKey: secretFile(t, home, "app.pem", []byte("unused")), APIURL: gh.srv.URL}
	})
	if g, err := c.GrantGitHub(context.Background(), api.GitHubGrantRequest{Owner: "@ana", Role: "member", Project: "base"}); err != nil {
		t.Fatalf("grant @ana: %+v, %v", g, err)
	}
	if a := gh.authOf("GET /users/ana"); a != "Bearer static-tok" {
		t.Fatalf("@ana was looked up with %q, want github.lookup_token's", a)
	}
	if gh.hitsOf("GET /app") != 0 || gh.hitsOf("POST /app/installations/7/access_tokens") != 0 {
		t.Fatal("the App was asked for a token although github.lookup_token is set")
	}
}

// The App is used only on the GitHub the lookups go to: one on another API
// URL leaves lookups unauthenticated, and a login GitHub then refuses for
// its rate limit is queued, not granted.
func TestRunnersAppOnAnotherGitHubIsNotUsed(t *testing.T) {
	gh := newLookupGitHub(t, "acme")
	var logs bytes.Buffer
	c := startOwners(t, gh, &logs, func(home string, f *config.File) {
		f.Runners.GitHub = config.RunnersGitHub{AppID: 42, PrivateKey: secretFile(t, home, "app.pem", []byte("unused")), APIURL: "https://ghe.example.com/api/v3"}
	})
	g, err := c.GrantGitHub(context.Background(), api.GitHubGrantRequest{Owner: "@ana", Role: "member", Project: "base"})
	if err != nil || !g.Queued {
		t.Fatalf("grant @ana: %+v, %v, want it queued behind the rate limit", g, err)
	}
	if a := gh.authOf("GET /users/ana"); a != "" {
		t.Fatalf("@ana was looked up with %q, want no token", a)
	}
	if !strings.Contains(logs.String(), "another GitHub") {
		t.Fatalf("no warning that the App is on another GitHub:\n%s", logs.String())
	}
}

// An App not installed on its own owner gives lookups no token: never
// another account's installation, whose API limit its runner jobs share. It
// says so once, and asks again only after a while.
func TestLookupsWhenTheAppOwnerHasNoInstallation(t *testing.T) {
	gh := newLookupGitHub(t, "someone")
	var logs bytes.Buffer
	c := startWithApp(t, gh, &logs)
	ctx := context.Background()
	for _, login := range []string{"@ana", "@bob"} {
		g, err := c.GrantGitHub(ctx, api.GitHubGrantRequest{Owner: login, Role: "member", Project: "base"})
		if err != nil || !g.Queued {
			t.Fatalf("grant %s: %+v, %v, want it queued behind the rate limit", login, g, err)
		}
	}
	if a := gh.authOf("GET /users/bob"); a != "" {
		t.Fatalf("@bob was looked up with %q, want no token", a)
	}
	if n := gh.hitsOf("POST /app/installations/7/access_tokens"); n != 0 {
		t.Fatalf("%d tokens minted, want none from another account's installation", n)
	}
	if n := gh.hitsOf("GET /users/someone/installation"); n != 1 {
		t.Fatalf("the owner's installation was asked for %d times, want once until the retry", n)
	}
	if n := strings.Count(logs.String(), "not installed on its owner"); n != 1 {
		t.Fatalf("the missing owner installation was warned %d times, want once:\n%s", n, logs.String())
	}
}

// An installation GitHub refuses tokens for (suspended) is not asked for a
// token on every lookup: one refusal, one warning, then lookups go without
// a token until the retry.
func TestSuspendedInstallationIsNotAskedEveryLookup(t *testing.T) {
	gh := newLookupGitHub(t, "acme")
	gh.mu.Lock()
	gh.suspended = true
	gh.mu.Unlock()
	var logs bytes.Buffer
	c := startWithApp(t, gh, &logs)
	ctx := context.Background()
	for _, login := range []string{"@ana", "@bob"} {
		g, err := c.GrantGitHub(ctx, api.GitHubGrantRequest{Owner: login, Role: "member", Project: "base"})
		if err != nil || !g.Queued {
			t.Fatalf("grant %s: %+v, %v, want it queued behind the rate limit", login, g, err)
		}
	}
	if n := gh.hitsOf("POST /app/installations/7/access_tokens"); n != 1 {
		t.Fatalf("a suspended installation was asked for %d tokens, want 1", n)
	}
	if n := strings.Count(logs.String(), "minting a GitHub lookup token"); n != 1 {
		t.Fatalf("the refusal was warned %d times, want once:\n%s", n, logs.String())
	}
}

// A minted token GitHub refuses (401) before its hour is up is named as the
// App's in the error, dropped, and the next lookup mints a new one.
func TestRefusedAppTokenIsMintedAgain(t *testing.T) {
	gh := newLookupGitHub(t, "acme")
	var logs bytes.Buffer
	c := startWithApp(t, gh, &logs)
	ctx := context.Background()
	if _, err := c.GrantGitHub(ctx, api.GitHubGrantRequest{Owner: "@ana", Role: "member", Project: "base"}); err != nil {
		t.Fatalf("grant @ana: %v", err)
	}
	first := strings.TrimPrefix(gh.authOf("GET /users/ana"), "Bearer ")
	gh.mu.Lock()
	gh.revoked[first] = true
	gh.mu.Unlock()
	if _, err := c.GrantGitHub(ctx, api.GitHubGrantRequest{Owner: "@bob", Role: "member", Project: "base"}); err == nil ||
		!strings.Contains(err.Error(), "runners.github's App") {
		t.Fatalf("grant @bob with a refused token: %v, want GitHub's refusal of the App's token", err)
	}
	if _, err := c.GrantGitHub(ctx, api.GitHubGrantRequest{Owner: "@bob", Role: "member", Project: "base"}); err != nil {
		t.Fatalf("grant @bob after the refusal: %v", err)
	}
	if a := strings.TrimPrefix(gh.authOf("GET /users/bob"), "Bearer "); a == first || !strings.HasPrefix(a, "app-lookup-tok-") {
		t.Fatalf("@bob was looked up with %q after %q was refused, want a new App token", a, first)
	}

	// The same for an org's members: the refusal says to try again (not to
	// install the App), and the next try mints a new token.
	if _, err := c.GrantGitHub(ctx, api.GitHubGrantRequest{Owner: "@acme", Role: "member", Project: "base"}); err != nil {
		t.Fatalf("grant @acme: %v", err)
	}
	members := strings.TrimPrefix(gh.authOf("GET /orgs/acme/members/acme"), "Bearer ")
	gh.mu.Lock()
	gh.revoked[members] = true
	gh.mu.Unlock()
	_, err := c.GrantGitHub(ctx, api.GitHubGrantRequest{Owner: "@acme", Role: "member", Project: "base"})
	if ae, ok := err.(*api.Error); !ok || ae.Code != api.CodeUnavailable || !strings.Contains(ae.Hint, "try again") {
		t.Fatalf("grant @acme with a refused members token: %v, want unavailable with a hint to try again", err)
	}
	if _, err := c.GrantGitHub(ctx, api.GitHubGrantRequest{Owner: "@acme", Role: "member", Project: "base"}); err != nil {
		t.Fatalf("grant @acme after the refusal: %v", err)
	}
}
