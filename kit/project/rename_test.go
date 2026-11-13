package project

import (
	"strings"
	"testing"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
)

// Rename step 2 mints potluck_ tokens; a zm_ token (minted before, its hash
// in the registry) still authenticates.
func TestTokenPrefixes(t *testing.T) {
	f := newFixture(t)
	tok, err := f.reg.Mint(f.ctx, f.local, api.MintTokenRequest{As: "victor", Project: "orchid", Role: "guest"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(tok.Secret, "potluck_") || len(tok.Secret) != 8+52 {
		t.Fatalf("minted %q", tok.Secret)
	}
	if v, err := f.reg.FromToken(f.ctx, tok.Secret, true); err != nil || v.Identity.Name != "victor" {
		t.Fatalf("potluck_ token: %+v %v", v, err)
	}
	// The same row under a zm_ secret, as a token a step 1 daemon minted
	// (the hash covers the prefix).
	newSecret := "zm_" + strings.TrimPrefix(tok.Secret, "potluck_")
	if _, err := f.st.Registry().ExecContext(f.ctx, `UPDATE tokens SET hash = ? WHERE id = ?`, authz.HashToken(newSecret), tok.Token.ID); err != nil {
		t.Fatal(err)
	}
	v, err := f.reg.FromToken(f.ctx, newSecret, true)
	if err != nil || v.Identity.Name != "victor" {
		t.Fatalf("zm_ token: %+v %v", v, err)
	}
	// Each prefix is its own token: the old secret no longer matches.
	if _, err := f.reg.FromToken(f.ctx, tok.Secret, true); !api.IsCode(err, api.CodeUnauthenticated) {
		t.Fatalf("old secret after the rehash: %v", err)
	}
	for _, bad := range []string{"potluck_short", "potluck-" + strings.Repeat("a", 52), "zm_" + strings.Repeat("a", 53)} {
		if authz.LooksLikeToken(bad) {
			t.Errorf("LooksLikeToken(%q)", bad)
		}
	}
}

// Join codes name the authority's node as zm-<machine> (made today) or
// potluck-<machine> (a later daemon's); both parse and dial their own node.
func TestJoinCodeNodePrefixes(t *testing.T) {
	for _, c := range []struct{ code, machine, host string }{
		{"zm-mini:abcdefghijklmnop:ABCD-EFGH", "mini", "zm-mini"},
		{"potluck-mini:abcdefghijklmnop:ABCD-EFGH", "mini", "potluck-mini"},
		{"potluck-mini.example.ts.net:abcdefghijklmnop:ABCD-EFGH", "mini", "potluck-mini.example.ts.net"},
	} {
		jc, err := ParseJoinCode(c.code)
		if err != nil || jc.Machine != c.machine || jc.AuthorityHost() != c.host {
			t.Errorf("%s: %+v (%s) %v", c.code, jc, jc.AuthorityHost(), err)
		}
		if !LooksLikeJoinCode(c.code) {
			t.Errorf("LooksLikeJoinCode(%q) = false", c.code)
		}
	}
	if !LooksLikeJoinCode("zm-mini") {
		t.Error("LooksLikeJoinCode(zm-mini) = false (as on main)")
	}
	// potluck-foo is a project's slug (a GitHub join), not a malformed code:
	// only a potluck- text with the code's ':' separators is a code.
	for _, s := range []string{"potluck-foo", "potluck-mini"} {
		if LooksLikeJoinCode(s) {
			t.Errorf("LooksLikeJoinCode(%q) = true: zm join %s is a GitHub join", s, s)
		}
	}
	if _, err := ParseJoinCode("other-mini:abcdefghijklmnop:ABCD-EFGH"); err == nil {
		t.Fatal("a code without a daemon node parsed")
	}
	// A code made by hand (no prefix recorded) still dials zm-<machine>.
	if h := (JoinCode{Machine: "mini"}).AuthorityHost(); h != "zm-mini" {
		t.Fatalf("default host %q", h)
	}
}
