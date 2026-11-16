package issuers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/grants"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/project"
	"github.com/sengirunners/potluck/kit/store"
)

// New refuses an issuer with no grant check.
func TestNewNeedsCheck(t *testing.T) {
	e := newEnv(t, appCeiling)
	o := e.opt
	o.Check = nil
	if _, err := Load(context.Background(), e.cfg, "gh", o); err == nil {
		t.Fatal("an issuer with no grant check was built")
	}
}

// Register makes the issuer the grants check's github-token kind and ends
// a removed grant's tokens; GrantCheck judges the caller as authenticated
// (a capped token gets nothing its cap does not allow), names the covering
// grant, and refuses a request with no caller.
func TestRegisterAndGrantCheck(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, appCeiling)
	home := t.TempDir()
	st, err := store.Open(ctx, home)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cfg := &config.Config{Home: home, Human: "judy", Machine: "mini"}
	reg, err := project.Open(ctx, st, cfg)
	if err != nil {
		t.Fatal(err)
	}
	d := &core.Deps{Config: cfg, Store: st, Projects: reg}
	local := reg.Local("")
	if _, err := reg.Create(ctx, local, api.NewProjectRequest{Slug: "orchid"}); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Mint(ctx, local, api.MintTokenRequest{As: "ctl", Project: "orchid", Role: "member"}); err != nil {
		t.Fatal(err)
	}
	ref, _ := reg.Lookup("orchid")
	ctl, err := reg.Identity(ctx, "ctl")
	if err != nil {
		t.Fatal(err)
	}
	tuples, err := reg.Tuples(ctx, ctl.ID)
	if err != nil {
		t.Fatal(err)
	}
	caller := func(role authz.Relation) *authz.Subject {
		cap := &authz.Cap{Project: ref.ID, Role: role}
		return &authz.Subject{Identity: ctl, Address: "human:ctl", Via: authz.ViaToken, Scope: authz.FromTuples(tuples, cap), Cap: cap}
	}

	Register(d, e.iss)
	if grants.IssuerOf(d, computeapi.SecretKindGitHubToken) != e.iss {
		t.Fatal("Register did not provide the issuer to grants")
	}
	// A grant for ctl, added through the grants routes (its ceiling read by
	// the issuer).
	r := core.NewRouter()
	grants.New(d).Routes(r)
	call := func(method, path string, body any) *httptest.ResponseRecorder {
		b, _ := json.Marshal(body)
		req := httptest.NewRequest(method, path, bytes.NewReader(b))
		c := core.WithResolveInput(core.WithSubject(req.Context(), local), project.ResolveInput{Explicit: "orchid", ExplicitFrom: project.RuleFlag})
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req.WithContext(c))
		return rec
	}
	rec := call(http.MethodPost, "/v0/secret_grants", computeapi.AddSecretGrantRequest{To: computeapi.GrantTo{Identity: "ctl"},
		Kind: computeapi.SecretKindGitHubToken, Name: "GH_TOKEN", Scopes: []string{"box"},
		Ceiling: json.RawMessage(`{"repos":["acme/app"],"permissions":{"contents":"write"}}`)})
	var g computeapi.SecretGrant
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &g) != nil || g.ID == "" {
		t.Fatalf("add a github-token grant: %d %s", rec.Code, rec.Body)
	}

	check := GrantCheck(d, func(context.Context, Box) (string, error) { return ClassTrusted, nil }, KindGitHubToken)
	box := Box{Project: ref.ID, ID: "box:b/1"}
	req := Request{Name: "GH_TOKEN", Subject: Subject{IdentityID: ctl.ID}, Box: box, Repo: "acme/app", Permissions: write}
	req.Caller = caller(authz.Member)
	if dec, err := check(ctx, req); err != nil || dec.Grant != g.ID {
		t.Fatalf("the covered request: %+v %v", dec, err)
	}
	for name, mut := range map[string]func(*Request){
		"no caller":          func(r *Request) { r.Caller = nil },
		"a capped caller":    func(r *Request) { r.Caller = caller(authz.Guest) },
		"another identity":   func(r *Request) { r.Caller = local },
		"wider than granted": func(r *Request) { r.Permissions = map[string]string{"contents": "write", "pull_requests": "write"} },
	} {
		x := req
		mut(&x)
		if _, err := check(ctx, x); err == nil {
			t.Errorf("%s: allowed", name)
		}
	}

	// A token issued under the grant ends when the grant is removed.
	e.grantOf[trustedBox.ID] = g.ID
	e.mustIssue(ident(trustedBox, write))
	if e.gh.liveCount() != 1 {
		t.Fatal("no token issued")
	}
	if rec := call(http.MethodDelete, "/v0/secret_grants/"+g.ID, nil); rec.Code != http.StatusOK {
		t.Fatalf("rm: %d %s", rec.Code, rec.Body)
	}
	for end := time.Now().Add(5 * time.Second); e.gh.liveCount() != 0; {
		if time.Now().After(end) {
			t.Fatal("removing the grant did not end its token")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
