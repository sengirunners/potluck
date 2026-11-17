package project

import (
	"database/sql"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
)

var (
	testPin   = Pin{NodeID: "nDEVICE", NodeKey: "nodekey:device", Name: "device.example.ts.net"}
	atPin     = Caller{Remote: true, NodeID: "nDEVICE", NodeKey: "nodekey:device"}
	answerReq = api.MintTokenRequest{Limit: "answer", Project: "orchid", TTLSeconds: 3600}
)

func limitedMint() LimitedMint {
	return LimitedMint{Pin: testPin, MaxTTL: 24 * time.Hour, Admitted: func(l string) bool { return l == "answer" }}
}

func (f *fixture) count(t *testing.T, table string) int {
	t.Helper()
	var n int
	if err := f.st.Registry().QueryRowContext(f.ctx, `SELECT count(*) FROM `+table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestMintLimitedRefusals(t *testing.T) {
	f := newFixture(t)
	other, err := f.reg.Mint(f.ctx, f.local, api.MintTokenRequest{As: "helper", Project: "orchid", Role: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	remote, err := f.reg.FromToken(f.ctx, other.Secret, true)
	if err != nil {
		t.Fatal(err)
	}
	capped := f.local.ForSession("mini/abcd1234")
	capped.Scope = capped.Scope.Capped(f.orchid.ID, authz.Member)
	with := func(mut func(*api.MintTokenRequest, *LimitedMint)) (api.MintTokenRequest, LimitedMint) {
		req, m := answerReq, limitedMint()
		mut(&req, &m)
		return req, m
	}
	cases := map[string]struct {
		sub  *authz.Subject
		mut  func(*api.MintTokenRequest, *LimitedMint)
		code string
	}{
		"a token caller":           {remote, func(*api.MintTokenRequest, *LimitedMint) {}, api.CodeForbidden},
		"no caller":                {nil, func(*api.MintTokenRequest, *LimitedMint) {}, api.CodeForbidden},
		"a capped session":         {capped, func(*api.MintTokenRequest, *LimitedMint) {}, api.CodeForbidden},
		"a session":                {f.local.ForSession("mini/abcd1234"), func(*api.MintTokenRequest, *LimitedMint) {}, api.CodeForbidden},
		"another identity":         {f.local, func(r *api.MintTokenRequest, _ *LimitedMint) { r.As = "helper" }, api.CodeBadRequest},
		"own name as as":           {f.local, func(r *api.MintTokenRequest, _ *LimitedMint) { r.As = f.local.Identity.Name }, api.CodeBadRequest},
		"a box key":                {f.local, func(r *api.MintTokenRequest, _ *LimitedMint) { r.BoxKey = "zmkey:x" }, api.CodeBadRequest},
		"no project":               {f.local, func(r *api.MintTokenRequest, _ *LimitedMint) { r.Project = "" }, api.CodeBadRequest},
		"an area":                  {f.local, func(r *api.MintTokenRequest, _ *LimitedMint) { r.Area = "sprout" }, api.CodeBadRequest},
		"no ttl":                   {f.local, func(r *api.MintTokenRequest, _ *LimitedMint) { r.TTLSeconds = 0 }, api.CodeBadRequest},
		"ttl over the ceiling":     {f.local, func(r *api.MintTokenRequest, _ *LimitedMint) { r.TTLSeconds = 24*3600 + 1 }, api.CodeBadRequest},
		"huge ttl":                 {f.local, func(r *api.MintTokenRequest, _ *LimitedMint) { r.TTLSeconds = 1 << 62 }, api.CodeBadRequest},
		"no ceiling":               {f.local, func(_ *api.MintTokenRequest, m *LimitedMint) { m.MaxTTL = 0 }, api.CodeBadRequest},
		"a malformed limit":        {f.local, func(r *api.MintTokenRequest, _ *LimitedMint) { r.Limit = "an_swer" }, api.CodeBadRequest},
		"a limit no route admits":  {f.local, func(r *api.MintTokenRequest, _ *LimitedMint) { r.Limit = "other" }, api.CodeBadRequest},
		"no admitted list":         {f.local, func(_ *api.MintTokenRequest, m *LimitedMint) { m.Admitted = nil }, api.CodeBadRequest},
		"no device":                {f.local, func(_ *api.MintTokenRequest, m *LimitedMint) { m.Pin = Pin{} }, api.CodeBadRequest},
		"a device without its key": {f.local, func(_ *api.MintTokenRequest, m *LimitedMint) { m.Pin.NodeKey = "" }, api.CodeBadRequest},
		"a role below owner":       {f.local, func(r *api.MintTokenRequest, _ *LimitedMint) { r.Role = "member" }, api.CodeBadRequest},
		"an unknown role":          {f.local, func(r *api.MintTokenRequest, _ *LimitedMint) { r.Role = "boss" }, api.CodeBadRequest},
		"an unknown project":       {f.local, func(r *api.MintTokenRequest, _ *LimitedMint) { r.Project = "nope" }, api.CodeNotFound},
	}
	tokens, grants, identities := f.count(t, "tokens"), f.count(t, "grants"), f.count(t, "identities")
	for name, c := range cases {
		req, m := with(c.mut)
		if _, err := f.reg.MintLimited(f.ctx, c.sub, req, m); !api.IsCode(err, c.code) {
			t.Errorf("%s: %v, want %s", name, err, c.code)
		}
	}
	if f.count(t, "tokens") != tokens || f.count(t, "grants") != grants || f.count(t, "identities") != identities {
		t.Fatal("a refused mint wrote something")
	}
	// The plain mint never makes a limited token.
	for _, r := range []api.MintTokenRequest{{Project: "orchid", Role: "owner", Limit: "answer"}, {Project: "orchid", Role: "owner", Node: "device"}} {
		if _, err := f.reg.Mint(f.ctx, f.local, r); !api.IsCode(err, api.CodeBadRequest) {
			t.Errorf("plain mint of %+v: %v", r, err)
		}
	}
}

func TestMintLimitedNeedsTheIdentitysOwnGrant(t *testing.T) {
	f := newFixture(t)
	// The socket's scope is All, but the identity must own the project by
	// its own tuple: take it away.
	if _, err := f.st.Registry().ExecContext(f.ctx, `DELETE FROM grants WHERE subject = ? AND project = ?`, f.local.Identity.ID, f.orchid.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.reg.MintLimited(f.ctx, f.local, answerReq, limitedMint()); !api.IsCode(err, api.CodeForbidden) {
		t.Fatalf("no own grant: %v", err)
	}
	// An area grant is not the whole project.
	if _, err := f.st.Registry().ExecContext(f.ctx, `INSERT INTO grants (subject, relation, object, project, area, created) VALUES (?, 'owner', ?, ?, 'sprout', 0)`,
		f.local.Identity.ID, authz.AreaObject(f.orchid.ID, "sprout"), f.orchid.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.reg.MintLimited(f.ctx, f.local, answerReq, limitedMint()); !api.IsCode(err, api.CodeForbidden) {
		t.Fatalf("an area grant only: %v", err)
	}
}

func TestLimitedTokenUse(t *testing.T) {
	f := newFixture(t)
	grants, identities := f.count(t, "grants"), f.count(t, "identities")
	out, err := f.reg.MintLimited(f.ctx, f.local, answerReq, limitedMint())
	if err != nil {
		t.Fatal(err)
	}
	if f.count(t, "grants") != grants || f.count(t, "identities") != identities {
		t.Fatal("a limited mint added a grant or an identity")
	}
	if l, ok := authz.TokenLimit(out.Secret); !ok || l != "answer" || !strings.HasPrefix(out.Secret, "potluck_answer_") {
		t.Fatalf("secret %q", out.Secret)
	}
	tk := out.Token
	if tk.Limit != "answer" || tk.Device != testPin.Name || tk.Identity != f.local.Identity.Name || tk.Role != "owner" || tk.Project != "orchid" || tk.Area != "" || tk.Expires == nil {
		t.Fatalf("token %+v", tk)
	}
	sub, err := f.reg.FromTokenCaller(f.ctx, out.Secret, atPin)
	if err != nil {
		t.Fatal(err)
	}
	if sub.Limit() != "answer" || sub.Identity.ID != f.local.Identity.ID || sub.Via != authz.ViaToken || !sub.Remote || sub.Node != "" || sub.BoxKey != "" {
		t.Fatalf("subject %+v", sub)
	}
	listed, err := f.reg.Tokens(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if l := listed[len(listed)-1]; l.ID != tk.ID || l.Limit != "answer" || l.Device != testPin.Name {
		t.Fatalf("listed %+v", l)
	}

	refused := map[string]Caller{
		"the socket":                 {NodeID: "nDEVICE", NodeKey: "nodekey:device"},
		"the tunnel":                 {Remote: true, NodeID: "nDEVICE", NodeKey: "nodekey:device", Via: "tunnel"},
		"a listener without WhoIs":   {Remote: true},
		"another node":               {Remote: true, NodeID: "nOTHER", NodeKey: "nodekey:device"},
		"a re-keyed device":          {Remote: true, NodeID: "nDEVICE", NodeKey: "nodekey:new"},
		"no key":                     {Remote: true, NodeID: "nDEVICE"},
		"this host":                  {Remote: true, NodeID: "nDEVICE", NodeKey: "nodekey:device", Self: true},
		"a WhoIs with another value": {Remote: true, NodeID: "nDEVICE", NodeKey: "nodekey:device", Via: "other"},
	}
	for name, c := range refused {
		if _, err := f.reg.FromTokenCaller(f.ctx, out.Secret, c); err == nil || err.Error() != "unauthenticated: invalid or expired token" || !api.IsCode(err, api.CodeUnauthenticated) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// FromToken and FromTokenAt know no node key: a limited token fails.
	if _, err := f.reg.FromTokenAt(f.ctx, out.Secret, true, "nDEVICE"); !api.IsCode(err, api.CodeUnauthenticated) {
		t.Fatalf("FromTokenAt: %v", err)
	}
	if _, err := f.reg.FromToken(f.ctx, out.Secret, true); !api.IsCode(err, api.CodeUnauthenticated) {
		t.Fatalf("FromToken: %v", err)
	}
	// The accepted call recorded its use; refused calls write nothing.
	var used sql.NullInt64
	_ = f.st.Registry().QueryRowContext(f.ctx, `SELECT last_used FROM tokens WHERE id = ?`, tk.ID).Scan(&used)
	if !used.Valid {
		t.Fatal("the accepted call did not record its use")
	}
	if _, err := f.st.Registry().ExecContext(f.ctx, `UPDATE tokens SET last_used = NULL WHERE id = ?`, tk.ID); err != nil {
		t.Fatal(err)
	}
	for _, c := range refused {
		_, _ = f.reg.FromTokenCaller(f.ctx, out.Secret, c)
	}
	_ = f.st.Registry().QueryRowContext(f.ctx, `SELECT last_used FROM tokens WHERE id = ?`, tk.ID).Scan(&used)
	if used.Valid {
		t.Fatal("a refused call wrote last_used")
	}

	// The identity's whole-project grant below the stored role: 401.
	if _, err := f.st.Registry().ExecContext(f.ctx, `UPDATE grants SET relation = 'member' WHERE subject = ? AND project = ? AND area = ''`, f.local.Identity.ID, f.orchid.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.reg.FromTokenCaller(f.ctx, out.Secret, atPin); !api.IsCode(err, api.CodeUnauthenticated) {
		t.Fatalf("grant lowered: %v", err)
	}
	if _, err := f.st.Registry().ExecContext(f.ctx, `UPDATE grants SET relation = 'owner' WHERE subject = ? AND project = ? AND area = ''`, f.local.Identity.ID, f.orchid.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.reg.FromTokenCaller(f.ctx, out.Secret, atPin); err != nil {
		t.Fatalf("grant back: %v", err)
	}
	if _, err := f.st.Registry().ExecContext(f.ctx, `DELETE FROM grants WHERE subject = ? AND project = ?`, f.local.Identity.ID, f.orchid.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.reg.FromTokenCaller(f.ctx, out.Secret, atPin); !api.IsCode(err, api.CodeUnauthenticated) {
		t.Fatalf("grant gone: %v", err)
	}
}

func TestLimitedTokenRevokedOrExpired(t *testing.T) {
	f := newFixture(t)
	a, err := f.reg.MintLimited(f.ctx, f.local, answerReq, limitedMint())
	if err != nil {
		t.Fatal(err)
	}
	b, err := f.reg.MintLimited(f.ctx, f.local, answerReq, limitedMint())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.reg.RevokeToken(f.ctx, a.Token.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.reg.FromTokenCaller(f.ctx, a.Secret, atPin); !api.IsCode(err, api.CodeUnauthenticated) {
		t.Fatalf("revoked: %v", err)
	}
	if _, err := f.st.Registry().ExecContext(f.ctx, `UPDATE tokens SET expires = 1 WHERE id = ?`, b.Token.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.reg.FromTokenCaller(f.ctx, b.Secret, atPin); !api.IsCode(err, api.CodeUnauthenticated) {
		t.Fatalf("expired: %v", err)
	}
	// A limited row never lacks an expiry; one that does (written by
	// something else) is refused, not read as never expiring.
	c, err := f.reg.MintLimited(f.ctx, f.local, answerReq, limitedMint())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.Registry().ExecContext(f.ctx, `UPDATE tokens SET expires = NULL WHERE id = ?`, c.Token.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.reg.FromTokenCaller(f.ctx, c.Secret, atPin); !api.IsCode(err, api.CodeUnauthenticated) {
		t.Fatalf("no expiry: %v", err)
	}
}

// oldLooksLikeToken is a copy of the shape check every build before limits
// had (authz.LooksLikeToken at df74e20).
func oldLooksLikeToken(s string) bool {
	for _, p := range []string{"potluck_", "zm_"} {
		if strings.HasPrefix(s, p) && len(s) == len(p)+52 {
			return true
		}
	}
	return false
}

// A build without limits, or a restored backup read by one, never treats a
// limited row as a token: its shape check refuses the secret, and its
// lookup (HashToken of the secret) finds no row.
func TestLimitedTokenUnreadableByOlderCode(t *testing.T) {
	f := newFixture(t)
	out, err := f.reg.MintLimited(f.ctx, f.local, answerReq, limitedMint())
	if err != nil {
		t.Fatal(err)
	}
	if oldLooksLikeToken(out.Secret) {
		t.Fatalf("the older shape check takes %q", out.Secret)
	}
	var id string
	err = f.st.Registry().QueryRowContext(f.ctx, `SELECT id FROM tokens WHERE hash = ?`, authz.HashToken(out.Secret)).Scan(&id)
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("the older lookup found %q (%v)", id, err)
	}
	// Nor does the plain form of the same random part, under either prefix.
	body := out.Secret[len(out.Secret)-52:]
	for _, s := range []string{"potluck_" + body, "zm_" + body} {
		if _, err := f.reg.FromTokenCaller(f.ctx, s, atPin); !api.IsCode(err, api.CodeUnauthenticated) {
			t.Errorf("%s: %v", s, err)
		}
	}
	// A limited secret whose limit is changed finds no row either.
	if _, err := f.reg.FromTokenCaller(f.ctx, "potluck_other_"+body, atPin); !api.IsCode(err, api.CodeUnauthenticated) {
		t.Fatalf("relabelled: %v", err)
	}
	// A plain row given a limit by hand (a copy edited by something else)
	// is refused: the secret's shape and the row must agree.
	plain, err := f.reg.Mint(f.ctx, f.local, api.MintTokenRequest{As: "helper", Project: "orchid", Role: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.Registry().ExecContext(f.ctx, `UPDATE tokens SET route_limit = 'answer' WHERE id = ?`, plain.Token.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.reg.FromTokenCaller(f.ctx, plain.Secret, atPin); !api.IsCode(err, api.CodeUnauthenticated) {
		t.Fatalf("plain secret, limited row: %v", err)
	}
}

// Every reader of a token row other than FromTokenCaller refuses a limited
// one: each is read with no route or device to check.
func TestOtherTokenReadersRefuseALimitedRow(t *testing.T) {
	f := newFixture(t)
	out, err := f.reg.MintLimited(f.ctx, f.local, answerReq, limitedMint())
	if err != nil {
		t.Fatal(err)
	}
	id := out.Token.ID
	if _, ok := f.reg.TokenScope(f.ctx, id); ok {
		t.Error("TokenScope")
	}
	if f.reg.TokenLive(f.ctx, id) {
		t.Error("TokenLive")
	}
	if _, err := f.reg.TokenExpiry(f.ctx, id); !api.IsCode(err, api.CodeUnauthenticated) {
		t.Errorf("TokenExpiry: %v", err)
	}
	if _, err := f.reg.SubscriptionSubject(f.ctx, id); !api.IsCode(err, api.CodeUnauthenticated) {
		t.Errorf("SubscriptionSubject: %v", err)
	}
	if ok, err := f.reg.RevokeTokenHash(f.ctx, authz.HashToken(out.Secret)); ok || err != nil {
		t.Errorf("RevokeTokenHash by the plain hash: %v %v", ok, err)
	}
}

// An unlimited token's subject and refusal text, written out: its cap has
// no limit, its scope is its identity's grants under its role, and every
// refusal is the unknown token's text, from every caller shape the
// listeners now pass. (TestUnlimitedTokenGolden in kit/server compares
// whole answers with the code before limits.)
func TestUnlimitedTokenSubject(t *testing.T) {
	f := newFixture(t)
	plain, err := f.reg.Mint(f.ctx, f.local, api.MintTokenRequest{As: "helper", Project: "orchid", Role: "member"})
	if err != nil {
		t.Fatal(err)
	}
	callers := []Caller{
		{Remote: true}, {Remote: true, NodeID: "nX"}, {Remote: true, NodeID: "nX", NodeKey: "nodekey:x"},
		{Remote: true, NodeID: "nX", NodeKey: "nodekey:x", Self: true}, {Remote: true, NodeID: "k", Via: "tunnel"}, {},
	}
	want := &authz.Cap{Project: f.orchid.ID, Role: authz.Member}
	for _, c := range callers {
		sub, err := f.reg.FromTokenCaller(f.ctx, plain.Secret, c)
		if err != nil {
			t.Fatalf("%+v: %v", c, err)
		}
		if !reflect.DeepEqual(sub.Cap, want) || sub.Limit() != "" || sub.Via != authz.ViaToken || sub.Remote != c.Remote ||
			sub.Identity.Name != "helper" || sub.Address != "human:helper" || sub.Node != "" || sub.BoxKey != "" ||
			sub.Scope.Relation(authz.Object{Project: f.orchid.ID}) != authz.Member || sub.Scope.SeesProject(f.side.ID) {
			t.Fatalf("%+v: subject %+v cap %+v", c, sub, sub.Cap)
		}
		for _, secret := range []string{"potluck_" + strings.Repeat("a", 52), "zm_" + strings.Repeat("a", 52), "nope", ""} {
			if _, err := f.reg.FromTokenCaller(f.ctx, secret, c); errText(err) != "unauthenticated: invalid or expired token" {
				t.Errorf("%+v %q: %v", c, secret, err)
			}
		}
	}
	for _, tk := range mustTokens(t, f) {
		if tk.Limit != "" || tk.Device != "" {
			t.Fatalf("plain token listed with a limit: %+v", tk)
		}
	}
}

func mustTokens(t *testing.T, f *fixture) []api.Token {
	t.Helper()
	ts, err := f.reg.Tokens(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	return ts
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
