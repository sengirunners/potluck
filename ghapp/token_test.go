package ghapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// tokenFake is a GitHub that answers token mints with what answer says, and
// records revocations and waits.
type tokenFake struct {
	mu      sync.Mutex
	answer  func(n int, repos []string, perms map[string]string) (status int, hdr map[string]string, body string)
	revoke  func(n int, tok string) int // status for the n-th DELETE
	mints   int
	revoked []string
	revokes int
	waits   []time.Duration
	other   http.HandlerFunc
}

func newTokenFake(t *testing.T) (*Client, *tokenFake) {
	t.Helper()
	_, pemb := newKey(t)
	f := &tokenFake{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/access_tokens"):
			var b struct {
				Repositories []string          `json:"repositories"`
				Permissions  map[string]string `json:"permissions"`
			}
			_ = json.NewDecoder(r.Body).Decode(&b)
			f.mints++
			st, hdr, body := 201, map[string]string(nil), ""
			if f.answer != nil {
				st, hdr, body = f.answer(f.mints, b.Repositories, b.Permissions)
			}
			if body == "" {
				body = exact(fmt.Sprintf("secret-token-%d", f.mints), "acme/"+b.Repositories[0], b.Permissions)
			}
			for k, v := range hdr {
				w.Header().Set(k, v)
			}
			w.WriteHeader(st)
			fmt.Fprint(w, body)
		case r.Method == "DELETE" && r.URL.Path == "/installation/token":
			f.revokes++
			st := 204
			if f.revoke != nil {
				st = f.revoke(f.revokes, r.Header.Get("Authorization"))
			}
			if st == 204 {
				f.revoked = append(f.revoked, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
			}
			w.WriteHeader(st)
		default:
			if f.other != nil {
				f.other(w, r)
				return
			}
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(srv.Close)
	c, err := New(Config{AppID: 42, PrivateKeyPEM: pemb, APIURL: srv.URL, HTTP: srv.Client(), Now: func() time.Time { return t0 },
		Sleep: func(_ context.Context, d time.Duration) error {
			f.mu.Lock()
			f.waits = append(f.waits, d)
			f.mu.Unlock()
			return nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	return c, f
}

// exact is GitHub's answer naming exactly repo and perms.
func exact(tok, repo string, perms map[string]string) string {
	p, _ := json.Marshal(perms)
	return fmt.Sprintf(`{"token":%q,"expires_at":%q,"permissions":%s,"repository_selection":"selected","repositories":[{"full_name":%q}]}`,
		tok, t0.Add(time.Hour).Format(time.RFC3339), p, repo)
}

func TestMintTokenExactAnswer(t *testing.T) {
	c, f := newTokenFake(t)
	p := Perms{"contents": "write", "metadata": "read"}
	tok, err := c.MintToken(context.Background(), 7, "acme/app", p)
	if err != nil {
		t.Fatal(err)
	}
	if tok.Reveal() != "secret-token-1" || tok.Repo != "acme/app" || !tok.ExpiresAt.Equal(t0.Add(time.Hour)) || tok.Permissions.key() != p.key() {
		t.Fatalf("token %v", tok)
	}
	for _, s := range []string{fmt.Sprint(tok), fmt.Sprintf("%+v", tok), fmt.Sprintf("%#v", tok), fmt.Sprintf("%s", tok),
		fmt.Sprintf("%v", []Token{tok}), fmt.Sprintf("%+v", struct{ T Token }{tok})} {
		if strings.Contains(s, "secret-token") {
			t.Errorf("printing a Token shows its value: %s", s)
		}
	}
	if b, _ := json.Marshal(struct{ T Token }{tok}); strings.Contains(string(b), "secret-token") {
		t.Errorf("JSON of a Token shows its value: %s", b)
	}
	if f.revokes != 0 {
		t.Errorf("an exact answer was revoked")
	}
}

func TestMintTokenBroaderAnswerIsRevokedAndRefused(t *testing.T) {
	asked := Perms{"contents": "read", "metadata": "read"}
	tok := "secret-broad"
	ans := func(sel, repos string, perms string) string {
		return fmt.Sprintf(`{"token":%q,"expires_at":%q,"permissions":%s,"repository_selection":%q,"repositories":[%s]}`,
			tok, t0.Add(time.Hour).Format(time.RFC3339), perms, sel, repos)
	}
	one := `{"full_name":"acme/app"}`
	for name, body := range map[string]string{
		"whole installation":   ans("all", one, `{"contents":"read","metadata":"read"}`),
		"two repositories":     ans("selected", one+`,{"full_name":"acme/other"}`, `{"contents":"read","metadata":"read"}`),
		"another repository":   ans("selected", `{"full_name":"acme/other"}`, `{"contents":"read","metadata":"read"}`),
		"no repositories":      ans("selected", ``, `{"contents":"read","metadata":"read"}`),
		"an extra permission":  ans("selected", one, `{"contents":"read","metadata":"read","issues":"write"}`),
		"write for read":       ans("selected", one, `{"contents":"write","metadata":"read"}`),
		"a missing permission": ans("selected", one, `{"metadata":"read"}`),
	} {
		t.Run(name, func(t *testing.T) {
			c, f := newTokenFake(t)
			f.answer = func(int, []string, map[string]string) (int, map[string]string, string) { return 201, nil, body }
			got, err := c.MintToken(context.Background(), 7, "acme/app", asked)
			if !errors.Is(err, ErrBroader) || got.Reveal() != "" {
				t.Fatalf("got %v, %v; want ErrBroader and no token", got, err)
			}
			if strings.Contains(err.Error(), tok) {
				t.Errorf("the error shows the token: %v", err)
			}
			if len(f.revoked) != 1 || f.revoked[0] != tok {
				t.Errorf("revoked %d tokens, want the broader one", len(f.revoked))
			}
		})
	}
}

func TestMintTokenRetriesBoundedAndFailsClosed(t *testing.T) {
	ctx := context.Background()
	p := Perms{"contents": "read", "metadata": "read"}

	c, f := newTokenFake(t)
	f.answer = func(n int, _ []string, _ map[string]string) (int, map[string]string, string) {
		if n <= 2 {
			return 502, nil, `{"message":"bad gateway"}`
		}
		return 201, nil, ""
	}
	if _, err := c.MintToken(ctx, 7, "acme/app", p); err != nil || f.mints != 3 {
		t.Fatalf("two 502s then a token: %v after %d mints", err, f.mints)
	}
	if fmt.Sprint(f.waits) != "[1s 2s]" {
		t.Errorf("waits %v, want [1s 2s]", f.waits)
	}

	c, f = newTokenFake(t)
	f.answer = func(int, []string, map[string]string) (int, map[string]string, string) {
		return 503, nil, `{"message":"down"}`
	}
	if _, err := c.MintToken(ctx, 7, "acme/app", p); err == nil || f.mints != retryTries {
		t.Fatalf("always 503: %v after %d mints, want an error after %d", err, f.mints, retryTries)
	}

	c, f = newTokenFake(t)
	f.answer = func(n int, _ []string, _ map[string]string) (int, map[string]string, string) {
		if n == 1 {
			return 429, map[string]string{"Retry-After": "4"}, `{"message":"slow down"}`
		}
		return 201, nil, ""
	}
	if _, err := c.MintToken(ctx, 7, "acme/app", p); err != nil || fmt.Sprint(f.waits) != "[4s]" {
		t.Fatalf("429 Retry-After 4: %v, waits %v", err, f.waits)
	}

	// A primary limit resetting in an hour: no waiting that long, fail now.
	c, f = newTokenFake(t)
	f.answer = func(int, []string, map[string]string) (int, map[string]string, string) {
		return 403, map[string]string{"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": fmt.Sprint(t0.Add(time.Hour).Unix())}, `{"message":"API rate limit exceeded"}`
	}
	if _, err := c.MintToken(ctx, 7, "acme/app", p); err == nil || f.mints != 1 || len(f.waits) != 0 {
		t.Fatalf("an hour's limit: %v after %d mints, waits %v", err, f.mints, f.waits)
	}

	// A plain 403 or 422 is GitHub's no: never retried.
	c, f = newTokenFake(t)
	f.answer = func(int, []string, map[string]string) (int, map[string]string, string) {
		return 422, nil, `{"message":"The permissions requested are not granted to this installation."}`
	}
	if _, err := c.MintToken(ctx, 7, "acme/app", p); err == nil || f.mints != 1 {
		t.Fatalf("422: %v after %d mints", err, f.mints)
	}

	// Refused before any call: a bad repo, no permission, a bad level.
	c, f = newTokenFake(t)
	for _, bad := range []struct {
		repo string
		p    Perms
	}{{"acme", p}, {"acme/../x", p}, {"acme/app", Perms{}}, {"acme/app", Perms{"contents": "admin"}}} {
		if _, err := c.MintToken(ctx, 7, bad.repo, bad.p); err == nil {
			t.Errorf("minted for %q %v", bad.repo, bad.p)
		}
	}
	if f.mints != 0 {
		t.Errorf("%d calls for refused mints", f.mints)
	}
}

func TestRevokeToken(t *testing.T) {
	ctx := context.Background()
	c, f := newTokenFake(t)
	tok, err := c.MintToken(ctx, 7, "acme/app", Perms{"metadata": "read"})
	if err != nil {
		t.Fatal(err)
	}
	f.revoke = func(n int, auth string) int {
		if auth != "Bearer "+tok.Reveal() {
			t.Errorf("revoke authenticated with %q", auth)
		}
		if n == 1 {
			return 500
		}
		return 204
	}
	if err := c.RevokeToken(ctx, tok); err != nil || len(f.revoked) != 1 {
		t.Fatalf("revoke after a 500: %v, revoked %d", err, len(f.revoked))
	}
	f.revoke = func(int, string) int { return 401 } // already revoked or expired
	if err := c.RevokeToken(ctx, tok); err != nil {
		t.Errorf("revoking a token GitHub no longer takes: %v", err)
	}
	f.revoke = func(int, string) int { return 404 }
	if err := c.RevokeToken(ctx, tok); err == nil || strings.Contains(err.Error(), tok.Reveal()) {
		t.Errorf("a 404: %v", err)
	}
	if err := c.RevokeToken(ctx, Token{}); err == nil {
		t.Error("revoked an empty token")
	}
}

func TestRevokeCachedEndsTheBrokersTokens(t *testing.T) {
	ctx := context.Background()
	c, f := newTokenFake(t)
	for _, repo := range []string{"acme/app", "acme/other"} {
		if _, err := c.InstallationToken(ctx, 7, repo, readPerms); err != nil {
			t.Fatal(err)
		}
	}
	n, err := c.RevokeCached(ctx)
	if err != nil || n != 2 || len(f.revoked) != 2 {
		t.Fatalf("revoked %d (%v), GitHub saw %d", n, err, len(f.revoked))
	}
	// The cache is empty: the next call mints a new token.
	if _, err := c.InstallationToken(ctx, 7, "acme/app", readPerms); err != nil || f.mints != 3 {
		t.Fatalf("after RevokeCached: %v, %d mints", err, f.mints)
	}
}

func TestRepoInstallation(t *testing.T) {
	c, f := newTokenFake(t)
	f.other = func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" && r.URL.Path == "/repos/acme/app/installation" && strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ey") {
			fmt.Fprint(w, `{"id":7}`)
			return
		}
		w.WriteHeader(404)
		fmt.Fprint(w, `{"message":"Not Found"}`)
	}
	if id, err := c.RepoInstallation(context.Background(), "acme/app"); err != nil || id != 7 {
		t.Fatalf("got %d, %v", id, err)
	}
	if _, err := c.RepoInstallation(context.Background(), "acme/none"); err == nil {
		t.Fatal("an App not installed on the repo answered an installation")
	}
}

func TestMintTokenExpiryWindow(t *testing.T) {
	asked := Perms{"contents": "read", "metadata": "read"}
	at := func(exp string) string {
		return fmt.Sprintf(`{"token":"secret-exp","expires_at":%s,"permissions":{"contents":"read","metadata":"read"},"repository_selection":"selected","repositories":[{"full_name":"acme/app"}]}`, exp)
	}
	q := func(d time.Duration) string { return fmt.Sprintf("%q", t0.Add(d).Format(time.RFC3339)) }
	for name, c := range map[string]struct {
		body string
		ok   bool
	}{
		"an hour":                      {at(q(time.Hour)), true},
		"GitHub's clock 5 min ahead":   {at(q(time.Hour + 5*time.Minute)), true},
		"GitHub's clock 50 min behind": {at(q(10 * time.Minute)), true},
		"no expiry":                    {at(`null`), false},
		"already expired":              {at(q(-time.Minute)), false},
		"expiring now":                 {at(q(0)), false},
		"two hours":                    {at(q(2 * time.Hour)), false},
		"GitHub's clock 6 min ahead":   {at(q(time.Hour + 6*time.Minute)), false},
	} {
		t.Run(name, func(t *testing.T) {
			c2, f := newTokenFake(t)
			f.answer = func(int, []string, map[string]string) (int, map[string]string, string) { return 201, nil, c.body }
			_, err := c2.MintToken(context.Background(), 7, "acme/app", asked)
			if c.ok != (err == nil) {
				t.Fatalf("got %v, want ok %v", err, c.ok)
			}
			if !c.ok && (!errors.Is(err, ErrBadExpiry) || len(f.revoked) != 1) {
				t.Errorf("%v, revoked %d: want ErrBadExpiry and the token revoked", err, len(f.revoked))
			}
		})
	}
}

func TestRefusedAnswerWhoseRevokeFailsIsHandedBack(t *testing.T) {
	c, f := newTokenFake(t)
	f.answer = func(int, []string, map[string]string) (int, map[string]string, string) {
		return 201, nil, exact("secret-wide", "acme/other", map[string]string{"metadata": "read"})
	}
	f.revoke = func(int, string) int { return 503 }
	_, err := c.MintToken(context.Background(), 7, "acme/app", Perms{"metadata": "read"})
	var ae *AnswerError
	if !errors.As(err, &ae) || !errors.Is(err, ErrBroader) || ae.Unrevoked.Reveal() != "secret-wide" {
		t.Fatalf("got %v; want an AnswerError holding the unrevoked token", err)
	}
	if strings.Contains(err.Error(), "secret-wide") || strings.Contains(fmt.Sprintf("%+v %#v", ae, ae), "secret-wide") {
		t.Errorf("the error shows the token: %v", err)
	}
}

func TestTokenCallsRefuseRedirects(t *testing.T) {
	c, f := newTokenFake(t)
	f.answer = func(int, []string, map[string]string) (int, map[string]string, string) {
		return 307, map[string]string{"Location": "/app/installations/7/access_tokens"}, `{}`
	}
	if _, err := c.MintToken(context.Background(), 7, "acme/app", Perms{"metadata": "read"}); err == nil || f.mints != 1 {
		t.Fatalf("a redirected mint: %v after %d calls", err, f.mints)
	}
}

func TestHashedTokenIsGitHubsFormat(t *testing.T) {
	// echo -n "ghs_x" | shasum -a 256 | xxd -r -p | base64 (the docs' recipe).
	if got := (Token{value: "ghs_x"}).HashedToken(); got != "nzHpBE+5Vsj//e27CI6v856CyTK2ZWvnFevqV7MueB8=" {
		t.Errorf("hashed_token %s", got)
	}
}

// A token GitHub refuses for the installation as a whole (a suspended
// installation, an App key it does not accept, an installation that is gone)
// is WholeInstallation: every repository of it fails alike. A rate limit is
// not, and nor is a refusal of one repository's permissions.
func TestInstallationTokenRefusalsOfTheWholeInstallation(t *testing.T) {
	for _, tt := range []struct {
		name   string
		status int
		hdr    map[string]string
		body   string
		whole  bool
	}{
		{"suspended", 403, nil, `{"message":"This installation has been suspended"}`, true},
		{"key not accepted", 401, nil, `{"message":"Bad credentials"}`, true},
		{"uninstalled", 404, nil, `{"message":"Not Found"}`, true},
		{"rate limit by message", 403, nil, `{"message":"API rate limit exceeded"}`, false},
		{"rate limit by header", 403, map[string]string{"Retry-After": "60"}, `{"message":"Forbidden"}`, false},
		{"permissions not granted", 422, nil, `{"message":"The permissions requested are not granted to this installation."}`, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c, f := newTokenFake(t)
			f.answer = func(int, []string, map[string]string) (int, map[string]string, string) {
				return tt.status, tt.hdr, tt.body
			}
			_, err := c.InstallationToken(context.Background(), 7, "acme/app", readPerms)
			if err == nil || WholeInstallation(err) != tt.whole {
				t.Fatalf("%d: %v, WholeInstallation %v, want %v", tt.status, err, WholeInstallation(err), tt.whole)
			}
		})
	}
}

// The token map does not keep an entry for every repository ever asked
// about: once it holds many, a new entry sweeps out the expired ones, and a
// live token stays (RevokeCached still ends it).
func TestInstallationTokenMapIsPruned(t *testing.T) {
	ctx := context.Background()
	c, f := newTokenFake(t)
	later := t0.Add(2 * time.Hour)
	f.mu.Lock()
	f.answer = func(n int, repos []string, perms map[string]string) (int, map[string]string, string) {
		if repos[0] != "live" {
			return 201, nil, ""
		}
		return 201, nil, strings.Replace(exact("live-token", "acme/live", perms), t0.Add(time.Hour).Format(time.RFC3339), later.Add(time.Hour).Format(time.RFC3339), 1)
	}
	f.mu.Unlock()
	if _, err := c.InstallationToken(ctx, 7, "acme/live", readPerms); err != nil {
		t.Fatal(err)
	}
	// The sweeps come when the map has doubled: 64 entries, then 128.
	for i := range 2*minPruneAt - 1 {
		if _, err := c.InstallationToken(ctx, 7, fmt.Sprintf("acme/r%03d", i), readPerms); err != nil {
			t.Fatal(err)
		}
	}
	c.now = func() time.Time { return later } // every token but the live one has expired
	if _, err := c.InstallationToken(ctx, 7, "acme/fresh", readPerms); err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	held := len(c.tokens)
	c.mu.Unlock()
	if held >= minPruneAt {
		t.Errorf("%d entries after every token but one expired, want the expired ones swept", held)
	}
	if got, err := c.RevokeCached(ctx); err != nil || got != 1 {
		t.Errorf("RevokeCached ended %d (%v), want the live token the sweep kept", got, err)
	}
}

// An entry a call holds (its token may be minting) is never swept, so the map
// always has every live token for RevokeCached.
func TestPruneKeepsHeldEntries(t *testing.T) {
	c, _ := newTokenFake(t)
	c.mu.Lock()
	for i := range 2 * minPruneAt {
		c.tokens[fmt.Sprint("k", i)] = &tokEntry{users: i % 2}
	}
	c.pruneTokens()
	held := len(c.tokens)
	c.mu.Unlock()
	if held != minPruneAt {
		t.Errorf("%d entries left, want the %d held ones", held, minPruneAt)
	}
}

// Calls for the same keys, while the map is being swept for other keys, end
// with one token per key, every one of them in the map to revoke.
func TestConcurrentTokensAreAllRevocable(t *testing.T) {
	ctx := context.Background()
	c, f := newTokenFake(t)
	var wg sync.WaitGroup
	for i := range 4 * minPruneAt {
		for range 3 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, _ = c.InstallationToken(ctx, 7, fmt.Sprintf("acme/r%03d", i), readPerms)
			}()
		}
	}
	wg.Wait()
	f.mu.Lock()
	minted := f.mints
	f.mu.Unlock()
	if got, err := c.RevokeCached(ctx); err != nil || got != minted {
		t.Errorf("%d tokens minted, %d revoked (%v)", minted, got, err)
	}
}
