// Package conformance is the owners brick's conformance suite: every
// owners.Accounts driver runs it against its provider (or a fake of it).
package conformance

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/potluck/kit/owners"
)

// Harness drives a provider for the suite.
type Harness struct {
	Accounts owners.Accounts
	// NewUser makes a person account named login and returns a token for it.
	NewUser func(t *testing.T, login string) (token string)
	// SecondToken returns another token for the account token belongs to.
	SecondToken func(t *testing.T, token string) string
	// Rename changes the login of the account token belongs to.
	Rename func(t *testing.T, token, login string)
	// Revoke makes the provider refuse token.
	Revoke func(t *testing.T, token string)

	// NewOrg makes an org account named login.
	NewOrg func(t *testing.T, login string)
	// Join makes token's holder a member of org: active, or a pending
	// invitation when active is false.
	Join func(t *testing.T, org, token string, active bool)
	// MakeAdmin makes token's holder an active admin of org.
	MakeAdmin func(t *testing.T, org, token string)
	// SetLookup sets the authority's own lookup token ("" for none).
	SetLookup func(t *testing.T, token string)
	// Suspend makes the provider stop answering for (true) or answer again
	// for (false) the account token belongs to.
	Suspend func(t *testing.T, token string, gone bool)

	// Device is the provider's device flow; Approve makes the account
	// holding token approve the next codes after pending polls, Decline
	// makes the person decline them, Expire makes them expire.
	Device  owners.Device
	Approve func(t *testing.T, token string, pending int)
	Decline func(t *testing.T)
	Expire  func(t *testing.T)
}

// Run checks the rules every driver keeps: ids are namespaced, stable
// across tokens and renames, distinct per account; a refused or empty
// token is owners.ErrBadToken.
func Run(t *testing.T, h Harness) {
	ctx := context.Background()
	verify := func(tok string) owners.Account {
		t.Helper()
		a, err := h.Accounts.Verify(ctx, tok)
		if err != nil {
			t.Fatalf("Verify: %v", err)
		}
		return a
	}
	ana := h.NewUser(t, "ana-conf")
	a := verify(ana)
	if a.Login != "ana-conf" || a.Kind != owners.KindUser {
		t.Fatalf("Verify = %+v, want login ana-conf, kind user", a)
	}
	if prefix, rest, ok := strings.Cut(a.ID, ":"); !ok || prefix == "" || rest == "" {
		t.Fatalf("id %q is not namespaced by driver (<driver>:<id>)", a.ID)
	}
	if b := verify(h.SecondToken(t, ana)); b.ID != a.ID {
		t.Fatalf("a second token of the same account answered id %q, want %q", b.ID, a.ID)
	}
	if b, err := h.Accounts.ByLogin(ctx, "ANA-conf"); err != nil || b != a {
		t.Fatalf("ByLogin (any case) = %+v %v, want %+v", b, err, a)
	}
	h.Rename(t, ana, "ana-renamed")
	if b := verify(ana); b.ID != a.ID || b.Login != "ana-renamed" {
		t.Fatalf("after a rename: %+v, want id %q and the new login", b, a.ID)
	}
	if _, err := h.Accounts.ByLogin(ctx, "ana-conf"); !errors.Is(err, owners.ErrNotFound) {
		t.Fatalf("the released login: %v, want ErrNotFound", err)
	}
	if b, err := h.Accounts.ByID(ctx, a.ID); err != nil || b.ID != a.ID || b.Login != "ana-renamed" {
		t.Fatalf("ByID after a rename = %+v %v", b, err)
	}
	for _, bad := range []string{"", "nope", a.ID + "x", "../user"} {
		if _, err := h.Accounts.ByID(ctx, bad); !errors.Is(err, owners.ErrNotFound) {
			t.Fatalf("ByID(%q): %v, want ErrNotFound", bad, err)
		}
	}
	for _, bad := range []string{"", "a/b", "../user", "-x"} {
		if _, err := h.Accounts.ByLogin(ctx, bad); !errors.Is(err, owners.ErrNotFound) {
			t.Fatalf("ByLogin(%q): %v, want ErrNotFound", bad, err)
		}
	}
	h.NewOrg(t, "acme-conf")
	if o, err := h.Accounts.ByLogin(ctx, "acme-conf"); err != nil || o.Kind != owners.KindOrg {
		t.Fatalf("org = %+v %v", o, err)
	}
	bob := h.NewUser(t, "bob-conf")
	for _, step := range []struct {
		join, active, want bool
	}{{false, false, false}, {true, false, false}, {true, true, true}} {
		if step.join {
			h.Join(t, "acme-conf", bob, step.active)
		}
		if ok, err := h.Accounts.Member(ctx, bob, "acme-conf"); err != nil || ok != step.want {
			t.Fatalf("Member (joined %v, active %v) = %v %v", step.join, step.active, ok, err)
		}
	}
	if ok, err := h.Accounts.Member(ctx, bob, "ana-renamed"); err != nil || ok {
		t.Fatalf("membership of a user account: %v %v", ok, err)
	}
	if role, err := h.Accounts.OrgRole(ctx, bob, "acme-conf"); err != nil || role != "member" {
		t.Fatalf("OrgRole of a member = %q %v", role, err)
	}
	h.MakeAdmin(t, "acme-conf", bob)
	if role, err := h.Accounts.OrgRole(ctx, bob, "acme-conf"); err != nil || role != "admin" {
		t.Fatalf("OrgRole of an admin = %q %v", role, err)
	}
	// Re-checking a membership with the authority's own token: only an org
	// member's token sees the members.
	carl := h.NewUser(t, "carl-conf")
	for _, tok := range []string{"", carl} {
		h.SetLookup(t, tok)
		if _, err := h.Accounts.OrgMember(ctx, "acme-conf", "bob-conf"); !errors.Is(err, owners.ErrNoView) {
			t.Fatalf("OrgMember with lookup token %q (no member's): %v, want ErrNoView", tok, err)
		}
	}
	h.SetLookup(t, bob)
	if ok, err := h.Accounts.OrgMember(ctx, "acme-conf", "bob-conf"); err != nil || !ok {
		t.Fatalf("OrgMember of a member: %v %v", ok, err)
	}
	if ok, err := h.Accounts.OrgMember(ctx, "acme-conf", "carl-conf"); err != nil || ok {
		t.Fatalf("OrgMember of a non-member: %v %v", ok, err)
	}
	h.SetLookup(t, "")
	h.Suspend(t, bob, true)
	if _, err := h.Accounts.ByLogin(ctx, "bob-conf"); !errors.Is(err, owners.ErrNotFound) {
		t.Fatalf("ByLogin of a suspended account: %v, want ErrNotFound", err)
	}
	h.Suspend(t, bob, false)
	b := verify(bob)
	h.Suspend(t, bob, true)
	if _, err := h.Accounts.ByID(ctx, b.ID); !errors.Is(err, owners.ErrNotFound) {
		t.Fatalf("ByID of a suspended account: %v, want ErrNotFound", err)
	}
	h.Suspend(t, bob, false)
	if c, err := h.Accounts.ByID(ctx, b.ID); err != nil || c.ID != b.ID {
		t.Fatalf("ByID after the suspension lifted: %+v %v", c, err)
	}
	if c := verify(h.NewUser(t, "ana-conf")); c.ID == a.ID {
		t.Fatalf("a new account under the released login got the old id %q", a.ID)
	}
	device(t, h, ana)
	h.Revoke(t, ana)
	if _, err := h.Accounts.Member(ctx, ana, "acme-conf"); !errors.Is(err, owners.ErrBadToken) {
		t.Fatalf("Member with a revoked token: %v, want ErrBadToken", err)
	}
	if _, err := h.Accounts.Verify(ctx, ana); !errors.Is(err, owners.ErrBadToken) {
		t.Fatalf("revoked token: %v, want ErrBadToken", err)
	}
	if _, err := h.Accounts.Verify(ctx, ""); !errors.Is(err, owners.ErrBadToken) {
		t.Fatalf("empty token: %v, want ErrBadToken", err)
	}
}

// device checks the device flow: an approved code answers a token of the
// approving account; a declined or expired one answers its error.
func device(t *testing.T, h Harness, holder string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	h.Approve(t, holder, 1)
	code, err := h.Device.Start(ctx)
	if err != nil || code.UserCode == "" || code.VerificationURI == "" || !code.Expires.After(time.Now()) {
		t.Fatalf("Start = %+v %v", code, err)
	}
	tok, err := h.Device.Wait(ctx, code)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	want, _ := h.Accounts.Verify(ctx, holder)
	if got, err := h.Accounts.Verify(ctx, tok); err != nil || got.ID != want.ID {
		t.Fatalf("the device token is %+v %v, want the approver %+v", got, err, want)
	}
	for _, c := range []struct {
		set  func(*testing.T)
		want error
	}{{h.Decline, owners.ErrDenied}, {h.Expire, owners.ErrExpired}} {
		c.set(t)
		code, err := h.Device.Start(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := h.Device.Wait(ctx, code); !errors.Is(err, c.want) {
			t.Fatalf("Wait: %v, want %v", err, c.want)
		}
	}
}
