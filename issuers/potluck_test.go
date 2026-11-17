package issuers

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/grants"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/project"
	"github.com/sengirunners/potluck/kit/tunnel"
)

// poolIssuer is a potluck issuer whose jobs' tokens act as ci, at most
// member on project orchid.
var poolIssuer = config.SecretsIssuer{Kind: KindPotluck, As: "ci", Ceiling: []config.SecretsCeiling{{Project: "orchid", Role: "member"}}}

func TestPotluckSpec(t *testing.T) {
	good := map[string]config.SecretsIssuer{
		"bare":     {Kind: KindPotluck},
		"pool":     poolIssuer,
		"area":     {Kind: KindPotluck, Ceiling: []config.SecretsCeiling{{Project: "orchid", Area: "ci", Role: "guest"}}},
		"workflow": {Kind: KindPotluck, As: "ci", Ceiling: []config.SecretsCeiling{{Project: "orchid", Role: "member", Workflows: []string{".github/workflows/nightly.yml"}, Events: []string{"schedule"}}}},
	}
	for name, is := range good {
		if _, err := Specs(config.File{Secrets: config.Secrets{Issuers: map[string]config.SecretsIssuer{name: is}}}); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	bad := map[string]config.SecretsIssuer{
		"owner":       {Kind: KindPotluck, Ceiling: []config.SecretsCeiling{{Project: "orchid", Role: "owner"}}},
		"no-role":     {Kind: KindPotluck, Ceiling: []config.SecretsCeiling{{Project: "orchid"}}},
		"no-project":  {Kind: KindPotluck, Ceiling: []config.SecretsCeiling{{Role: "member"}}},
		"repo":        {Kind: KindPotluck, Ceiling: []config.SecretsCeiling{{Project: "orchid", Role: "member", Repo: "acme/app"}}},
		"app-key":     {Kind: KindPotluck, PrivateKey: "file:/k.pem"},
		"bad-as":      {Kind: KindPotluck, As: "Not A Name"},
		"pr-event":    {Kind: KindPotluck, Ceiling: []config.SecretsCeiling{{Project: "orchid", Role: "member", Events: []string{"pull_request"}}}},
		"gh-with-as":  {Kind: KindGitHubApp, As: "ci", AppID: 1, PrivateKey: "file:/k.pem", Ceiling: []config.SecretsCeiling{appCeiling}},
		"gh-with-pro": {Kind: KindGitHubApp, AppID: 1, PrivateKey: "file:/k.pem", Ceiling: []config.SecretsCeiling{{Repo: "acme/app", Permissions: write, Project: "orchid"}}},
	}
	for name, is := range bad {
		if _, err := Specs(config.File{Secrets: config.Secrets{Issuers: map[string]config.SecretsIssuer{name: is}}}); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// addJobGrant adds a job grant for mainJob through the grants routes,
// answering the HTTP status.
func addJobGrant(t *testing.T, d *core.Deps, kind, name, ceiling string) int {
	t.Helper()
	r := core.NewRouter()
	grants.New(d).Routes(r)
	b, _ := json.Marshal(computeapi.AddSecretGrantRequest{
		To:   computeapi.GrantTo{Job: &computeapi.GrantJob{Repo: "acme/app", Workflow: ".github/workflows/lander.yml", Ref: "refs/heads/main", Events: []string{"push"}}},
		Kind: kind, Name: name, Scopes: []string{"box"}, Ceiling: json.RawMessage(ceiling)})
	req := httptest.NewRequest(http.MethodPost, "/v0/secret_grants", bytes.NewReader(b))
	c := core.WithResolveInput(core.WithSubject(req.Context(), d.Projects.Local("")), project.ResolveInput{Explicit: "orchid", ExplicitFrom: project.RuleFlag})
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req.WithContext(c))
	return rec.Code
}

// potluckModule is a started module with the pool issuer, identity ci
// holding member on orchid, and a job grant of POTLUCK_TOKEN for mainJob.
func potluckModule(t *testing.T, grantCI bool) (*core.Deps, *fakeRunTargets, *Module, string, Request) {
	t.Helper()
	ctx := context.Background()
	d, rts, proj := moduleDeps(t, map[string]config.SecretsIssuer{"pool": poolIssuer})
	if !grantCI {
		if _, err := d.Projects.AddIdentity(ctx, api.NewIdentityRequest{Name: "ci", Kind: api.IdentityAgent}); err != nil {
			t.Fatal(err)
		}
	} else {
		if _, err := d.Projects.Grant(ctx, d.Projects.Local(""), api.GrantRequest{Identity: "ci", Role: "member", Project: "orchid"}); err != nil {
			t.Fatal(err)
		}
	}
	m := NewModule(d)
	t.Cleanup(func() { m.Close() })
	if From(d) == nil || grants.IssuerOf(d, KindPotluckToken) == nil || grants.IssuerOf(d, KindGitHubToken) != nil {
		t.Fatal("the pool issuer is not the one running")
	}
	if code := addJobGrant(t, d, KindPotluckToken, "POTLUCK_TOKEN", `{"role":"member"}`); code != http.StatusOK {
		t.Fatalf("add a potluck-token job grant: %d", code)
	}
	gs, err := grants.JobGrants(ctx, d, proj, "", "acme/app")
	if err != nil || len(gs) != 1 {
		t.Fatalf("the job grants: %+v %v", gs, err)
	}
	jr, err := JobRequest(gs[0], *mainJob(), Box{Project: proj, ID: "box:small/1"})
	if err != nil || jr.Kind != KindPotluckToken || jr.Role != "member" {
		t.Fatalf("JobRequest: %+v %v", jr, err)
	}
	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	return d, rts, m, proj, jr
}

func issueSoon(t *testing.T, m *Module, req Request) (Issued, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return m.Issue(ctx, req)
}

// A job's potluck-token is a token of the pool's own: it authenticates as
// the issuer's identity, capped to member on the box's project, expires no
// later than the box's lease, is in the ledger by its hash only, and stops
// working when the box ends.
func TestPotluckTokenIssuedCappedAndRevokedAtEnd(t *testing.T) {
	ctx := context.Background()
	d, rts, m, proj, jr := potluckModule(t, true)
	lease := time.Now().Add(20 * time.Minute)
	rts.mu.Lock()
	rts.lease = lease
	rts.mu.Unlock()

	out, err := issueSoon(t, m, jr)
	if err != nil {
		t.Fatal(err)
	}
	if out.Permissions["role"] != "member" || out.ExpiresAt.After(lease) || out.ExpiresAt.Before(lease.Add(-time.Minute)) {
		t.Fatalf("issued %+v, want role member expiring at the lease %v", out, lease)
	}
	secret := rts.value(jr.Box.ID, "POTLUCK_TOKEN")
	if secret == "" {
		t.Fatal("nothing delivered")
	}
	sub, err := boxAuth(d.Projects, secret)
	if err != nil {
		t.Fatalf("the delivered token does not authenticate: %v", err)
	}
	if sub.Identity.Name != "ci" || sub.Cap == nil || sub.Cap.Project != proj || sub.Cap.Role != "member" {
		t.Fatalf("token subject %s, cap %+v", sub.Identity.Name, sub.Cap)
	}
	toks, _ := d.Projects.Tokens(ctx)
	if len(toks) != 1 || toks[0].Expires == nil || toks[0].Expires.After(lease.Add(time.Second)) {
		t.Fatalf("token rows %+v: want one expiring by the lease", toks)
	}

	rts.onEnd[0](ctx, proj, computeapi.RunTargetHandle{Handle: jr.Box.ID, State: "released"})
	if _, err := boxAuth(d.Projects, secret); err == nil {
		t.Fatal("the token still authenticates after its box ended")
	}
	db := d.Store.Registry()
	rows, err := db.Query(`SELECT event, kind, hashed_token, why, subject, name, permissions FROM issuers_ledger ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var evs []string
	for rows.Next() {
		var ev, kind, hash, why, subject, name, perms string
		_ = rows.Scan(&ev, &kind, &hash, &why, &subject, &name, &perms)
		all := strings.Join([]string{ev, kind, hash, why, subject, name, perms}, " ")
		if strings.Contains(all, secret) || strings.Contains(all, mustCred(t, secret).Token) {
			t.Fatalf("the ledger holds the token: %s", all)
		}
		if kind != KindPotluckToken || hash == "" || name != "POTLUCK_TOKEN" || perms != "role=member" {
			t.Errorf("ledger row %s", all)
		}
		evs = append(evs, ev)
	}
	if strings.Join(evs, ",") != "issued,revoked" {
		t.Fatalf("ledger events %v", evs)
	}
}

// Refused before anything is minted: an identity that does not hold the
// role, an owner grant ceiling, and a box whose lease is about to end.
func TestPotluckTokenRefusals(t *testing.T) {
	ctx := context.Background()
	d, _, m, _, jr := potluckModule(t, false)
	if _, err := issueSoon(t, m, jr); codeOf(err) != api.CodeForbidden || !strings.Contains(err.Error(), "does not hold member") {
		t.Fatalf("ci holds no grant: %v, want forbidden", err)
	}
	if toks, _ := d.Projects.Tokens(ctx); len(toks) != 0 {
		t.Fatalf("minted %d tokens for a refused request", len(toks))
	}
	if code := addJobGrant(t, d, KindPotluckToken, "OWNER_TOKEN", `{"role":"owner"}`); code == http.StatusOK {
		t.Fatal("an owner potluck-token grant was added")
	}

	d2, rts2, m2, _, jr2 := potluckModule(t, true)
	rts2.mu.Lock()
	rts2.lease = time.Now().Add(30 * time.Second)
	rts2.mu.Unlock()
	if _, err := issueSoon(t, m2, jr2); codeOf(err) != api.CodeUnavailable || !strings.Contains(err.Error(), "within a minute") {
		t.Fatalf("a lease ending in 30s: %v, want unavailable", err)
	}
	if toks, _ := d2.Projects.Tokens(ctx); len(toks) != 0 {
		t.Fatalf("minted %d tokens for a box about to end", len(toks))
	}
}

// potluckEnv is an issuer of the pool kind on a real registry, with a fake
// clock, the boxes and grant check of env, and identity ci holding member
// on orchid.
type potluckEnv struct {
	*env
	d     *core.Deps
	proj  string
	mu    sync.Mutex
	lease time.Time // every box's lease end (zero: none)
}

func newPotluckEnv(t *testing.T) *potluckEnv { return newPotluckEnvWith(t, poolIssuer) }

// newPotluckEnvWith is newPotluckEnv with issuer is, on the real clock (the
// registry's token expiries are real time).
func newPotluckEnvWith(t *testing.T, is config.SecretsIssuer) *potluckEnv {
	t.Helper()
	ctx := context.Background()
	d, _, proj := moduleDeps(t, nil)
	if err := d.Store.AddMigrations(ctx, Migrations...); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Projects.Grant(ctx, d.Projects.Local(""), api.GrantRequest{Identity: "ci", Role: "member", Project: "orchid"}); err != nil {
		t.Fatal(err)
	}
	e := newEnv(t, appCeiling)
	e.db = d.Store.Registry()
	e.clk.mu.Lock()
	e.clk.now = time.Now()
	e.clk.mu.Unlock()
	e.cfg = config.File{Secrets: config.Secrets{Issuers: map[string]config.SecretsIssuer{"gh": is}}}
	e.opt.Ledger = SQLLedger{e.db}
	e.opt.Tokens = registryTokens{d}
	e.opt.Home = func() (string, string, error) { return testTunnelURL, testHomeFP, nil }
	p := &potluckEnv{d: d, proj: proj}
	e.opt.BoxExpires = func(context.Context, Box) (time.Time, error) {
		p.mu.Lock()
		defer p.mu.Unlock()
		return p.lease, nil
	}
	e.opt.JobHolds = func(context.Context, Request) (bool, error) { return true, nil }
	e.iss = e.load()
	p.env = e
	return p
}

func (p *potluckEnv) setLease(t time.Time) { p.mu.Lock(); p.lease = t; p.mu.Unlock() }

func (p *potluckEnv) req(box Box) Request {
	box.Project, box.Area = p.proj, ""
	return Request{Kind: KindPotluckToken, Name: "POTLUCK_TOKEN", Subject: Subject{Job: mainJob()}, Box: box, Role: "member"}
}

func (p *potluckEnv) works(secret string) bool {
	_, err := boxAuth(p.d.Projects, secret)
	return err == nil
}

// What lands in the box is one credential file: the token, a key made for
// it, and the authority's tunnel URL and key fingerprint. The token works
// only on a request its own key signed over the tunnel: not as a bearer
// token (the socket, the tailnet), not with another key. A renewal brings a
// new token with a new key. With no tunnel URL nothing is minted.
func TestPotluckTokenBoundToItsBoxKey(t *testing.T) {
	ctx := context.Background()
	p := newPotluckEnv(t)
	req := p.req(trustedBox)
	p.mustIssue(req)
	first := mustCred(t, p.bx.get(req.Box, "POTLUCK_TOKEN"))
	if first.URL != testTunnelURL || first.Home != testHomeFP {
		t.Fatalf("the credential names %s %s, want the authority's tunnel", first.URL, first.Home)
	}
	reg := p.d.Projects
	if _, err := reg.FromTokenAt(ctx, first.Token, true, tunnel.BoxKeyID(first.Key)); err != nil {
		t.Fatalf("with its own key: %v", err)
	}
	other, _ := tunnel.NewBoxKey()
	for name, node := range map[string]string{"another key": tunnel.BoxKeyID(other), "a tailnet node": "nTAILNET1CNTRL", "no node": ""} {
		if _, err := reg.FromTokenAt(ctx, first.Token, true, node); codeOf(err) != api.CodeUnauthenticated {
			t.Errorf("%s: %v, want unauthenticated", name, err)
		}
	}
	if _, err := reg.FromTokenAt(ctx, first.Token, false, tunnel.BoxKeyID(first.Key)); codeOf(err) != api.CodeUnauthenticated {
		t.Errorf("on the socket: %v, want unauthenticated", err)
	}
	toks, _ := reg.Tokens(ctx)
	if len(toks) != 1 || toks[0].Node != "" {
		t.Fatalf("token rows %+v: a box's token is never node-bound (a replica's)", toks)
	}
	p.clk.add(41 * time.Minute)
	p.iss.Tick(ctx)
	second := mustCred(t, p.bx.get(req.Box, "POTLUCK_TOKEN"))
	if second.Token == first.Token || tunnel.BoxKeyID(second.Key) == tunnel.BoxKeyID(first.Key) {
		t.Fatal("a renewal kept the token or the key")
	}
	if _, err := reg.FromTokenAt(ctx, second.Token, true, tunnel.BoxKeyID(first.Key)); err == nil {
		t.Fatal("the renewed token works with the old key")
	}

	q := newPotluckEnv(t)
	q.opt.Home = func() (string, string, error) {
		return "", "", api.Errorf(api.CodeUnavailable, "this daemon has no tunnel url (tunnel.url)")
	}
	q.iss = q.load()
	if _, err := q.issue(q.req(trustedBox)); codeOf(err) != api.CodeForbidden || !strings.Contains(err.Error(), "could not reach this authority") {
		t.Fatalf("no tunnel url: %v, want forbidden", err)
	}
	if toks, _ := q.d.Projects.Tokens(ctx); len(toks) != 0 {
		t.Fatalf("minted %d tokens with no tunnel url", len(toks))
	}
}

// A delivery error quoting the credential's token or key alone, not the
// whole file, is scrubbed of both before it reaches the caller or the
// ledger.
func TestPotluckTokenPartsScrubbed(t *testing.T) {
	p := newPotluckEnv(t)
	var parts []string
	p.bx.set(func(b *boxes) {
		b.fail = errors.New("box busy")
		b.quote = func(v string) string {
			c, err := tunnel.ParseBoxCredential(v)
			if err != nil {
				return "unparsed"
			}
			parts = []string{c.Token, hex.EncodeToString(c.Key.Seed())}
			return "token " + parts[0] + " key " + parts[1]
		}
	})
	_, err := p.issue(p.req(trustedBox))
	if err == nil || len(parts) != 2 {
		t.Fatalf("issue: %v (quoted %d parts)", err, len(parts))
	}
	rows, qerr := p.db.Query(`SELECT why FROM issuers_ledger`)
	if qerr != nil {
		t.Fatal(qerr)
	}
	defer rows.Close()
	all := err.Error()
	for rows.Next() {
		var why string
		_ = rows.Scan(&why)
		all += "\n" + why
	}
	for _, v := range parts {
		if strings.Contains(all, v) {
			t.Fatalf("a part of the credential leaked:\n%s", all)
		}
	}
	if !strings.Contains(all, "[secret]") {
		t.Fatalf("nothing scrubbed:\n%s", all)
	}
}

func mustCred(t *testing.T, delivered string) tunnel.BoxCredential {
	t.Helper()
	c, err := tunnel.ParseBoxCredential(delivered)
	if err != nil {
		t.Fatalf("the delivered value: %v", err)
	}
	return c
}

// A potluck-token is renewed at two thirds of its hour: the new one is
// delivered in place of the old, which works through the grace and is
// revoked after it.
func TestPotluckTokenRenewal(t *testing.T) {
	p := newPotluckEnv(t)
	req := p.req(trustedBox)
	p.mustIssue(req)
	box := req.Box
	first := p.bx.get(box, "POTLUCK_TOKEN")
	p.clk.add(41 * time.Minute)
	p.iss.Tick(context.Background())
	second := p.bx.get(box, "POTLUCK_TOKEN")
	if second == "" || second == first || !p.works(second) || !p.works(first) {
		t.Fatalf("after renewal: renewed %v, new works %v, old works through the grace %v", second != first, p.works(second), p.works(first))
	}
	p.clk.add(6 * time.Minute)
	p.iss.Tick(context.Background())
	if p.works(first) || !p.works(second) {
		t.Fatalf("after the grace: old works %v, new works %v", p.works(first), p.works(second))
	}
}

// After a crash, the next run revokes every potluck-token the earlier run
// held (the tokens table outlives the run): none is left to lapse.
func TestPotluckTokenRevokedAfterCrash(t *testing.T) {
	ctx := context.Background()
	p := newPotluckEnv(t)
	req := p.req(trustedBox)
	p.mustIssue(req)
	secret := p.bx.get(req.Box, "POTLUCK_TOKEN")
	p.clk.add(time.Minute)
	next := p.load() // the earlier issuer is never closed: a crash
	if err := next.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if p.works(secret) {
		t.Fatal("an earlier run's token still works after the next start")
	}
	evs := strings.Join(p.events(), ";")
	if !strings.Contains(evs, "revoked:the authority restarted") || strings.Contains(evs, "expired_unrevoked") {
		t.Fatalf("ledger %s", evs)
	}
}

// Every minted kind keeps the door's promises the same way (the kind
// conformance case): a box on a machine that is not trusted gets nothing;
// a trusted box's credential works until any of its ends (the box's end,
// its grant's removal, the authority stopping); the ledger and every error
// never hold the value.
func TestEveryMintedKind(t *testing.T) {
	type kind struct {
		e     *env
		req   func(Box) Request
		works func(string) bool
	}
	kinds := map[string]func(t *testing.T) kind{
		KindGitHubToken: func(t *testing.T) kind {
			e := newEnv(t, appCeiling)
			return kind{e: e, req: func(b Box) Request { return ident(b, write) }, works: func(v string) bool {
				e.gh.mu.Lock()
				defer e.gh.mu.Unlock()
				return e.gh.live[v]
			}}
		},
		KindPotluckToken: func(t *testing.T) kind {
			p := newPotluckEnv(t)
			return kind{e: p.env, req: p.req, works: p.works}
		},
	}
	for name, mk := range kinds {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			k := mk(t)
			e := k.e
			if _, err := e.issue(k.req(publicBox)); codeOf(err) != api.CodeForbidden {
				t.Fatalf("a public machine's box: %v, want forbidden", err)
			}
			var vals []string
			ends := []func(r Request){
				func(r Request) { e.iss.End(ctx, r.Box, EndReleased) },
				func(r Request) { e.iss.EndGrant(ctx, "g1") },
				func(r Request) { e.iss.Close(ctx) },
			}
			for i, end := range ends {
				r := k.req([]Box{trustedBox, otherBox, thirdBox}[i])
				e.mustIssue(r)
				v := e.bx.get(r.Box, r.Name)
				if v == "" || !k.works(v) {
					t.Fatalf("end %d: the delivered credential does not work", i)
				}
				vals = append(vals, v)
				end(r)
				if k.works(v) {
					t.Fatalf("end %d: the credential still works after the end", i)
				}
			}
			for _, v := range vals {
				for _, row := range e.rows() {
					if strings.Contains(row, v) {
						t.Fatalf("the ledger holds a value: %s", row)
					}
				}
				for _, msg := range e.errs {
					if strings.Contains(msg, v) {
						t.Fatalf("an error holds a value: %s", msg)
					}
				}
			}
		})
	}
}

// A person's token acts as that person, capped like a job's, and never
// outlives the token the person authenticated with (so a box's token cannot
// be chained into a longer-lived one). A person who holds only guest gets no
// member token.
func TestPotluckTokenForAPerson(t *testing.T) {
	ctx := context.Background()
	p := newPotluckEnv(t)
	reg := p.d.Projects
	callerTok, err := reg.Mint(ctx, reg.Local(""), api.MintTokenRequest{As: "ci", Project: "orchid", Role: "member", TTLSeconds: 600, NoGrant: true})
	if err != nil {
		t.Fatal(err)
	}
	caller, err := reg.FromToken(ctx, callerTok.Secret, true)
	if err != nil {
		t.Fatal(err)
	}
	box := trustedBox
	box.Project, box.Area = p.proj, ""
	req := Request{Kind: KindPotluckToken, Name: "POTLUCK_TOKEN", Subject: Subject{IdentityID: caller.Identity.ID}, Box: box, Role: "member", Caller: caller}
	out := p.mustIssue(req)
	if out.ExpiresAt.After(*callerTok.Token.Expires) {
		t.Fatalf("issued until %v, past the caller's token (%v)", out.ExpiresAt, *callerTok.Token.Expires)
	}
	sub, err := boxAuth(reg, p.bx.get(box, "POTLUCK_TOKEN"))
	if err != nil || sub.Identity.Name != "ci" || sub.Cap.Role != authz.Member || sub.Cap.Project != p.proj {
		t.Fatalf("the person's token: %+v %v", sub, err)
	}

	if _, err := reg.Grant(ctx, reg.Local(""), api.GrantRequest{Identity: "viewer", Role: "guest", Project: "orchid"}); err != nil {
		t.Fatal(err)
	}
	viewer, _ := reg.Identity(ctx, "viewer")
	g := req
	g.Subject, g.Caller, g.Box.ID = Subject{IdentityID: viewer.ID}, nil, "box-guest"
	if _, err := p.issue(g); codeOf(err) != api.CodeForbidden || !strings.Contains(err.Error(), "does not hold member") {
		t.Fatalf("a guest asking for member: %v, want forbidden", err)
	}
}

// A rule for one area does not cover a box of another area or of no area.
func TestPotluckTokenAreaRule(t *testing.T) {
	p := newPotluckEnvWith(t, config.SecretsIssuer{Kind: KindPotluck, As: "ci", Ceiling: []config.SecretsCeiling{{Project: "orchid", Area: "ci", Role: "member"}}})
	if _, err := p.issue(p.req(trustedBox)); codeOf(err) != api.CodeForbidden || !strings.Contains(err.Error(), "outside the issuer's ceiling") {
		t.Fatalf("a whole-project box under an area rule: %v, want forbidden", err)
	}
}

// Each renewal reads the lease again: a lease cut short caps the new token,
// and one ending within a minute gets no new token (the old one stays until
// it expires).
func TestPotluckTokenRenewalReadsTheLease(t *testing.T) {
	ctx := context.Background()
	p := newPotluckEnv(t)
	req := p.req(trustedBox)
	p.mustIssue(req)
	first := p.bx.get(req.Box, "POTLUCK_TOKEN")
	p.clk.add(41 * time.Minute)
	lease := p.clk.Now().Add(5 * time.Minute)
	p.setLease(lease)
	p.iss.Tick(ctx)
	second := p.bx.get(req.Box, "POTLUCK_TOKEN")
	var exp int64
	if err := p.db.QueryRow(`SELECT expires FROM issuers_ledger WHERE event = 'renewed' ORDER BY id DESC LIMIT 1`).Scan(&exp); err != nil {
		t.Fatal(err)
	}
	capped := exp <= lease.Unix() && exp > lease.Add(-time.Minute).Unix()
	if second == first || !capped {
		t.Fatalf("renewed %v, a live token capped at the shortened lease %v", second != first, capped)
	}
	p.clk.add(4*time.Minute + 30*time.Second) // 30 s of lease left at the next renewal
	p.iss.Tick(ctx)
	if got := p.bx.get(req.Box, "POTLUCK_TOKEN"); got != second {
		t.Fatal("renewed with less than a minute of lease left")
	}
}

// With both kinds configured, the module routes each request to its kind's
// issuer, answers Mints for both, and a box's end revokes what either issued.
func TestModuleRunsBothKinds(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, appCeiling)
	e.clk.mu.Lock()
	e.clk.now = time.Now()
	e.clk.mu.Unlock()
	d, rts, proj := moduleDeps(t, map[string]config.SecretsIssuer{"gh": e.cfg.Secrets.Issuers["gh"], "pool": poolIssuer})
	if _, err := d.Projects.Grant(ctx, d.Projects.Local(""), api.GrantRequest{Identity: "ci", Role: "member", Project: "orchid"}); err != nil {
		t.Fatal(err)
	}
	m := NewModule(d)
	t.Cleanup(func() { m.Close() })
	if !m.Mints(KindGitHubToken) || !m.Mints(KindPotluckToken) || m.Mints("value") || len(rts.onEnd) != 1 {
		t.Fatalf("Mints github %v potluck %v value %v; %d end hooks", m.Mints(KindGitHubToken), m.Mints(KindPotluckToken), m.Mints("value"), len(rts.onEnd))
	}
	box := Box{Project: proj, ID: "box:small/1"}
	ghReq := jobGrantRequest(t, d, proj, box)
	if code := addJobGrant(t, d, KindPotluckToken, "POTLUCK_TOKEN", `{"role":"member"}`); code != http.StatusOK {
		t.Fatalf("add a potluck-token job grant: %d", code)
	}
	gs, _ := grants.JobGrants(ctx, d, proj, "", "acme/app")
	var plReq Request
	for _, g := range gs {
		if g.Kind == KindPotluckToken {
			plReq, _ = JobRequest(g, *mainJob(), box)
		}
	}
	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := issueSoon(t, m, ghReq); err != nil {
		t.Fatalf("github-token: %v", err)
	}
	if _, err := issueSoon(t, m, plReq); err != nil {
		t.Fatalf("potluck-token: %v", err)
	}
	if _, err := issueSoon(t, m, Request{Kind: "value", Name: "X", Box: box}); codeOf(err) != api.CodeUnavailable {
		t.Fatalf("a kind no issuer mints: %v, want unavailable", err)
	}
	secret := rts.value(box.ID, "POTLUCK_TOKEN")
	if e.gh.liveCount() != 1 || rts.value(box.ID, "GH_TOKEN") == "" || secret == "" {
		t.Fatalf("delivered: %v", rts.writes)
	}
	rts.onEnd[0](ctx, proj, computeapi.RunTargetHandle{Handle: box.ID, State: "released"})
	if _, err := boxAuth(d.Projects, secret); err == nil || e.gh.liveCount() != 0 {
		t.Fatalf("after the box's end: potluck-token works %v, %d GitHub tokens live", err == nil, e.gh.liveCount())
	}
}

// A renewal asks the kind's check again: an identity that lost its role has
// its token ended, not renewed.
func TestPotluckTokenRenewalRechecks(t *testing.T) {
	ctx := context.Background()
	p := newPotluckEnv(t)
	req := p.req(trustedBox)
	p.mustIssue(req)
	first := p.bx.get(req.Box, "POTLUCK_TOKEN")
	if _, err := p.d.Projects.Revoke(ctx, p.d.Projects.Local(""), api.GrantRequest{Identity: "ci", Role: "member", Project: "orchid"}); err != nil {
		t.Fatal(err)
	}
	p.clk.add(41 * time.Minute)
	p.iss.Tick(ctx)
	toks, _ := p.d.Projects.Tokens(ctx)
	if p.bx.get(req.Box, "POTLUCK_TOKEN") != first || len(toks) != 1 || toks[0].Revoked == nil {
		t.Fatalf("after ci lost member: renewed %v, %d tokens, revoked %v", p.bx.get(req.Box, "POTLUCK_TOKEN") != first, len(toks), toks[0].Revoked != nil)
	}
	if !strings.Contains(strings.Join(p.events(), ";"), "refused at renewal") {
		t.Fatalf("ledger %v", p.events())
	}
}

// flakyTokens is a token mint whose grant reads fail while down is set.
type flakyTokens struct {
	TokenMint
	mu   sync.Mutex
	down bool
}

func (f *flakyTokens) set(down bool) { f.mu.Lock(); f.down = down; f.mu.Unlock() }
func (f *flakyTokens) Tuples(ctx context.Context, id string) ([]authz.Tuple, error) {
	f.mu.Lock()
	down := f.down
	f.mu.Unlock()
	if down {
		return nil, errors.New("database is locked")
	}
	return f.TokenMint.Tuples(ctx, id)
}

// A registry that cannot answer at renewal is tried again (the old token
// stays), not taken as a refusal that ends the token.
func TestPotluckTokenRenewalRetriesARegistryError(t *testing.T) {
	ctx := context.Background()
	p := newPotluckEnv(t)
	flaky := &flakyTokens{TokenMint: registryTokens{p.d}}
	p.opt.Tokens = flaky
	p.iss = p.load()
	req := p.req(trustedBox)
	p.mustIssue(req)
	first := p.bx.get(req.Box, "POTLUCK_TOKEN")
	flaky.set(true)
	p.clk.add(41 * time.Minute)
	p.iss.Tick(ctx)
	if p.bx.get(req.Box, "POTLUCK_TOKEN") != first || !p.works(first) {
		t.Fatal("a registry error at renewal ended the token")
	}
	flaky.set(false)
	p.clk.add(2 * time.Minute)
	p.iss.Tick(ctx)
	if second := p.bx.get(req.Box, "POTLUCK_TOKEN"); second == first || !p.works(second) {
		t.Fatal("the renewal was not tried again once the registry answered")
	}
}

// testTunnelURL and testHomeFP are the authority a test box's credential
// names.
const (
	testTunnelURL = "https://authority.example"
	testHomeFP    = "abcdefghijklmnopqrstuvwxyz"
)

// boxAuth authenticates a delivered box credential as the tunnel would: its
// token, on a request its own key signed.
func boxAuth(reg *project.Registry, delivered string) (*authz.Subject, error) {
	c, err := tunnel.ParseBoxCredential(delivered)
	if err != nil {
		return nil, err
	}
	return reg.FromTokenAt(context.Background(), c.Token, true, tunnel.BoxKeyID(c.Key))
}
