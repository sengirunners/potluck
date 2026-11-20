package ghapp

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

// An account token names no repository, asks only its permissions, is
// cached until 5 minutes before its hour ends, and is never minted at write.
func TestAccountToken(t *testing.T) {
	c, f, _ := setup(t, nil)
	ctx := context.Background()
	p := Perms{"members": "read", "metadata": "read"}
	tok, err := c.AccountToken(ctx, 7, p)
	if err != nil || tok != "tok-1" {
		t.Fatalf("first: %q, %v", tok, err)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(f.mint[0]), &body); err != nil {
		t.Fatal(err)
	}
	if _, ok := body["repositories"]; ok {
		t.Fatalf("an account token names a repository: %s", f.mint[0])
	}
	if got, _ := json.Marshal(body["permissions"]); string(got) != `{"members":"read","metadata":"read"}` {
		t.Fatalf("permissions %s", got)
	}
	if tok, _ := c.AccountToken(ctx, 7, p); tok != "tok-1" {
		t.Fatalf("not cached: %q", tok)
	}
	// A repository's token of the same installation and permissions is not it.
	if tok, _ := c.InstallationToken(ctx, 7, "o/r", p); tok == "tok-1" {
		t.Fatal("a repository's token came from the account token's cache entry")
	}
	f.mu.Lock()
	f.now = t0.Add(56 * time.Minute) // under 5 minutes left
	f.mu.Unlock()
	if tok, _ := c.AccountToken(ctx, 7, p); tok == "tok-1" {
		t.Fatal("not refreshed before expiry")
	}
	n := len(f.mint)
	if _, err := c.AccountToken(ctx, 7, Perms{"members": "write"}); err == nil {
		t.Fatal("an account token at write was not refused")
	}
	if _, err := c.AccountToken(ctx, 7, Perms{}); err == nil {
		t.Fatal("an account token with no permissions was not refused")
	}
	if len(f.mint) != n {
		t.Fatal("a refused account token asked GitHub")
	}
}

func TestAppOwner(t *testing.T) {
	c, _, _ := setup(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/app" {
			w.Write([]byte(`{"id":42,"owner":{"login":"acme"}}`))
			return
		}
		http.NotFound(w, r)
	})
	if o, err := c.AppOwner(context.Background()); err != nil || o != "acme" {
		t.Fatalf("owner %q, %v", o, err)
	}
	c, _, _ = setup(t, func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"id":42}`)) })
	if _, err := c.AppOwner(context.Background()); err == nil {
		t.Fatal("an App with no owner answered one")
	}
}
