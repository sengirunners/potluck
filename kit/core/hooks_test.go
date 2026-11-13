package core

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/project"
	"github.com/sengirunners/potluck/kit/swap"
)

// asSession is a socket request acting as as (Zm-As).
func asSession(d *Deps, as string) context.Context {
	return authed(d, func(r *http.Request) { r.Header.Set(api.HeaderAs, as) })
}

// A daemon with no module registered for a hook answers for itself: no
// health part, a notice only logged, every session unknown, and an unknown
// session never gets more than the caller had.
func TestHooksUnregistered(t *testing.T) {
	d, _ := newDeps(t)
	var logged bytes.Buffer
	d.Log = slog.New(slog.NewTextHandler(&logged, nil))
	ctx := context.Background()

	if p := d.HealthParts(ctx); p != nil {
		t.Fatalf("health parts with none registered: %v", p)
	}
	n := Notice{Project: api.ProjectRef{ID: mustID(t, d, "orchid"), Slug: "orchid"}, To: "owners", Body: "an unexpected join"}
	if err := d.Notify(ctx, n); !errors.Is(err, ErrNoNotifier) {
		t.Fatalf("notify with no notifier: %v", err)
	}
	// Project and recipient at Warn; the body only at Debug (off here).
	if !strings.Contains(logged.String(), "project=orchid to=owners") || strings.Contains(logged.String(), "an unexpected join") {
		t.Fatalf("the notice was not logged as it should be: %q", logged.String())
	}
	if d.SessionHooks() != nil {
		t.Fatal("session hooks with none registered")
	}
	if _, _, ok := d.SessionPlace(ctx, "mini/abcd1234"); ok {
		t.Fatal("an unknown session has a place")
	}
	d.ReapPresence(ctx) // no-op, no panic

	// Zm-As naming a session address: the caller's own scope, uncapped
	// (a cap only narrows, and nothing knows this session).
	sub := SubjectOf(asSession(d, "mini/abcd1234"))
	if sub == nil || sub.Session != "mini/abcd1234" || !sub.Scope.IsAll() {
		t.Fatalf("session address with no session module: %+v", sub)
	}
	// Zm-As naming a session by name: no name resolves; refused.
	if ctx := asSession(d, "mini/worker"); SubjectOf(ctx) != nil || authErrOf(ctx) == nil {
		t.Fatalf("a session name resolved with no session module: %+v", SubjectOf(ctx))
	}
	// Project resolution has no session rule: nothing places the session.
	if res := d.Resolve(asSession(d, "mini/abcd1234")); res.Project != nil {
		t.Fatalf("resolved %+v for an unknown session", res.Project)
	}
	// Visibility: an unknown session of this machine is hidden from a
	// scoped reader.
	guest := authz.Empty()
	if d.Visible(ctx, guest, "mini/abcd1234") {
		t.Fatal("an unknown session is visible to a scoped reader")
	}
}

func TestHooksRegistered(t *testing.T) {
	d, _ := newDeps(t)
	ctx := context.Background()
	orchid, side := mustID(t, d, "orchid"), mustID(t, d, "side")

	// Health parts: in key order, nil and typed-nil values left out.
	type part struct {
		N int `json:"n"`
	}
	d.AddHealthPart("zeta", func(context.Context) any { return part{2} })
	d.AddHealthPart("alpha", func(context.Context) any { return &part{1} })
	d.AddHealthPart("none", func(context.Context) any { return nil })
	d.AddHealthPart("typednil", func(context.Context) any { return (*part)(nil) })
	h := api.Health{OK: true, Protocol: "p", Version: "v", Commit: "c", Parts: d.HealthParts(ctx)}
	b, err := h.MarshalJSON()
	if err != nil || string(b) != `{"ok":true,"protocol":"p","version":"v","commit":"c","alpha":{"n":1},"zeta":{"n":2}}` {
		t.Fatalf("health with parts: %s %v", b, err)
	}
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("a health part added twice did not panic")
			}
		}()
		d.AddHealthPart("alpha", func(context.Context) any { return nil })
	}()

	// Notifiers: each gets the notice.
	var got []Notice
	d.AddNotifier(func(_ context.Context, n Notice) error { got = append(got, n); return nil })
	n := Notice{Project: api.ProjectRef{ID: orchid, Slug: "orchid"}, To: "human:judy", Body: "linked"}
	if err := d.Notify(ctx, n); err != nil || len(got) != 1 || got[0] != n {
		t.Fatalf("notify: %v %+v", err, got)
	}

	// Session hooks.
	reaped := 0
	d.SetSessionHooks(func() *SessionHooks {
		return &SessionHooks{
			Name: func(_ context.Context, ref string) (string, bool) {
				return "mini/abcd1234", ref == "mini/worker"
			},
			Cap: func(address string) (string, authz.Relation, bool) {
				return orchid, authz.Guest, address == "mini/abcd1234"
			},
			Place: func(_ context.Context, address string) (string, string, bool) {
				return side, "", address == "mini/abcd1234"
			},
			Visible: func(_ context.Context, _ authz.Scope, address string) bool { return address == "mini/abcd1234" },
			Reap:    func(context.Context) { reaped++ },
		}
	})
	sub := SubjectOf(asSession(d, "mini/worker"))
	if sub == nil || sub.Session != "mini/abcd1234" {
		t.Fatalf("named session: %+v", sub)
	}
	if sub.Scope.IsAll() || sub.Scope.SeesProject(side) || sub.Scope.Can(authz.Operate, authz.Object{Project: orchid}) {
		t.Fatal("the cap did not narrow the scope")
	}
	if ctx := asSession(d, "mini/no-such-name"); SubjectOf(ctx) != nil || !api.IsCode(authErrOf(ctx), api.CodeForbidden) {
		t.Fatalf("an unknown name: %v", authErrOf(ctx))
	}
	other := SubjectOf(asSession(d, "mini/efgh5678"))
	if other == nil || !other.Scope.IsAll() {
		t.Fatalf("an uncapped session: %+v", other)
	}
	if res := d.Resolve(asSession(d, "mini/efgh5678")); res.Project != nil {
		t.Fatalf("an unplaced session resolved to %+v", res.Project)
	}
	if !d.Visible(ctx, authz.Empty(), "mini/abcd1234") || d.Visible(ctx, authz.Empty(), "mini/efgh5678") {
		t.Fatal("visibility does not ask the hook")
	}
	d.ReapPresence(ctx)
	if reaped != 1 {
		t.Fatalf("reaped %d times", reaped)
	}
	// A nil field answers as for an unknown session; nil hooks as for no
	// session module.
	d.SetSessionHooks(func() *SessionHooks { return &SessionHooks{} })
	if ctx := asSession(d, "mini/worker"); !api.IsCode(authErrOf(ctx), api.CodeForbidden) {
		t.Fatalf("a name with no Name hook: %v", authErrOf(ctx))
	}
	d.SetSessionHooks(func() *SessionHooks { return nil })
	if d.SessionHooks() != nil {
		t.Fatal("hooks answered nil but SessionHooks is not")
	}
}

// replicaToken redeems an invite to orchid for machine rep (node nREP):
// a replica's node-bound token.
func replicaToken(t *testing.T, d *Deps) string {
	t.Helper()
	ctx := context.Background()
	orchid := mustID(t, d, "orchid")
	if err := d.Projects.SetTeam(ctx, orchid, true); err != nil {
		t.Fatal(err)
	}
	inv, err := d.Projects.CreateInvite(ctx, d.Projects.Local(""), project.InviteInput{
		InviteRequest: api.InviteRequest{Project: "orchid", As: "rhea", Role: "member"},
		Authority:     "mini", Fingerprint: project.NodeFingerprint("nMINI")})
	if err != nil {
		t.Fatal(err)
	}
	c, err := project.ParseJoinCode(inv.Code)
	if err != nil {
		t.Fatal(err)
	}
	out, err := d.Projects.Redeem(ctx, project.RedeemInput{Secret: c.Secret, Node: "nREP", Machine: "rep"})
	if err != nil {
		t.Fatal(err)
	}
	return out.Token
}

// With no hooks registered, no network caller (token, tailnet login,
// replica token) acts as a session it does not own through Zm-As.
func TestNoHooksNetworkCallersAsSession(t *testing.T) {
	d, secret := newDeps(t)
	ctx := context.Background()
	d.Net = &fakeNet{dns: "zm-mini.tail1234.ts.net."}
	if _, err := d.Projects.AddIdentity(ctx, api.NewIdentityRequest{Name: "ivy", TailscaleLogin: "ivy@example.com"}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Projects.Grant(ctx, d.Projects.Local(""), api.GrantRequest{Identity: "ivy", Role: "member", Project: "orchid"}); err != nil {
		t.Fatal(err)
	}
	rtok := replicaToken(t, d)
	token := func(as string) context.Context {
		return authed(d, func(r *http.Request) {
			r.Header.Set("Authorization", "Bearer "+secret)
			r.Header.Set(api.HeaderAs, as)
			*r = *r.WithContext(WithRemote(r.Context(), Remote{}))
		})
	}
	login := func(as string) context.Context {
		return authed(d, func(r *http.Request) {
			r.Host = "100.64.0.1:7433"
			r.Header.Set(api.HeaderAs, as)
			*r = *r.WithContext(WithRemote(r.Context(), Remote{Login: "ivy@example.com"}))
		})
	}
	replica := func(as string) context.Context {
		return authed(d, func(r *http.Request) {
			r.Header.Set("Authorization", "Bearer "+rtok)
			r.Header.Set(api.HeaderAs, as)
			*r = *r.WithContext(WithRemote(r.Context(), Remote{NodeID: "nREP"}))
		})
	}
	// The callers themselves authenticate (so a refusal below is Zm-As's).
	if SubjectOf(login("human:ivy")) == nil || SubjectOf(replica("rep/abcd1234")) == nil {
		t.Fatal("a caller did not authenticate at all")
	}
	for name, ctx := range map[string]context.Context{
		"token, this machine's session":   token("mini/abcd1234"),
		"token, a session name":           token("mini/worker"),
		"login, this machine's session":   login("mini/abcd1234"),
		"login, a session name":           login("mini/worker"),
		"replica, this machine's session": replica("mini/abcd1234"),
		"replica, a session name":         replica("mini/worker"),
		"replica, another machine's":      replica("other/abcd1234"),
	} {
		if sub := SubjectOf(ctx); sub != nil || authErrOf(ctx) == nil {
			t.Errorf("%s: acted as %+v", name, sub)
		}
	}
	// A replica's own machine's session is its own (SPEC 17.3), at its
	// token's scope: no hook widens it.
	own, base := SubjectOf(replica("rep/abcd1234")), SubjectOf(replica(""))
	if own == nil || base == nil || own.Session != "rep/abcd1234" || own.Scope.IsAll() || own.Scope.SeesProject(mustID(t, d, "side")) {
		t.Fatalf("replica's own session: %+v", own)
	}
}

// A hooks struct whose fields are nil answers as for an unknown session.
func TestNilHookFields(t *testing.T) {
	d, _ := newDeps(t)
	ctx := context.Background()
	d.SetSessionHooks(func() *SessionHooks { return &SessionHooks{} })
	sub := SubjectOf(asSession(d, "mini/abcd1234"))
	if sub == nil || !sub.Scope.IsAll() {
		t.Fatalf("nil Cap: %+v", sub)
	}
	if res := d.Resolve(asSession(d, "mini/abcd1234")); res.Project != nil {
		t.Fatalf("nil Place resolved %+v", res.Project)
	}
	if _, _, ok := d.SessionPlace(ctx, "mini/abcd1234"); ok {
		t.Fatal("nil Place placed a session")
	}
	if d.Visible(ctx, authz.Empty(), "mini/abcd1234") {
		t.Fatal("nil Visible showed a session")
	}
	d.ReapPresence(ctx) // nil Reap: no-op
}

// Visible on another machine's session with no hooks: only a whole-project
// reader of a project that machine joined sees it.
func TestNoHooksRemoteSessionVisible(t *testing.T) {
	d, _ := newDeps(t)
	ctx := context.Background()
	replicaToken(t, d) // machine rep joins orchid
	orchid := mustID(t, d, "orchid")
	whole := authz.All().Only(orchid)
	area := authz.FromTuples(nil, nil)
	if !d.Visible(ctx, whole, "rep/abcd1234") {
		t.Fatal("a whole-project reader of the joined project does not see the replica's session")
	}
	if d.Visible(ctx, area, "rep/abcd1234") || d.Visible(ctx, whole, "elsewhere/abcd1234") || d.Visible(ctx, authz.Empty(), "rep/abcd1234") {
		t.Fatal("a session shown beyond its machine's projects")
	}
	if !d.Visible(ctx, authz.All(), "elsewhere/abcd1234") {
		t.Fatal("the machine owner does not see every session")
	}
}

func TestHealthPartRefusesFieldNames(t *testing.T) {
	d, _ := newDeps(t)
	for _, key := range []string{"ok", "protocol", "version", "commit", "machine", "pid", "started", "hedges", "selfcheck", "last_swap", ""} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("health part %q took a health field's name", key)
				}
			}()
			d.AddHealthPart(key, func(context.Context) any { return 1 })
		}()
	}
	// The fields a part fills are free for it.
	for _, key := range []string{"backup", "heartbeat", "holders"} {
		d.AddHealthPart(key, func(context.Context) any { return nil })
	}
}

// A probe part may not take the name of a part every probe runs already
// (a base part, or one swap.RegisterProbe added): one of the two would go
// unchecked.
func TestAddProbeRefusesATakenName(t *testing.T) {
	swap.RegisterProbe("hooks-test-registered", func(*client.Client, *swap.Probing) {})
	for _, name := range []string{"health", "hooks-test-registered"} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("AddProbe(%q) was accepted", name)
				}
			}()
			d, _ := newDeps(t)
			d.AddProbe(name, func(*client.Client, *swap.Probing) {})
		}()
	}
}
