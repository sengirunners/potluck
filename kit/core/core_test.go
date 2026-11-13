package core

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/project"
	"github.com/sengirunners/potluck/kit/store"
)

func newDeps(t *testing.T) (*Deps, string) {
	t.Helper()
	ctx := context.Background()
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
	d := &Deps{Config: cfg, Store: st, Projects: reg}
	local := reg.Local("")
	if _, err := reg.Create(ctx, local, api.NewProjectRequest{Slug: "orchid"}); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.AddArea(ctx, local, "orchid", api.Area{Name: "sprout"}); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Create(ctx, local, api.NewProjectRequest{Slug: "side"}); err != nil {
		t.Fatal(err)
	}
	tok, err := reg.Mint(ctx, local, api.MintTokenRequest{As: "victor", Project: "orchid", Area: "sprout", Role: "guest"})
	if err != nil {
		t.Fatal(err)
	}
	return d, tok.Secret
}

// authed builds a socket request (WithLocal); mut may add WithRemote or strip it.
func authed(d *Deps, mut func(r *http.Request)) context.Context {
	r := httptest.NewRequest("GET", "/x", nil)
	r = r.WithContext(WithLocal(r.Context()))
	mut(r)
	return d.Authenticate(r)
}

func TestAuthenticate(t *testing.T) {
	d, secret := newDeps(t)
	cases := []struct {
		name    string
		mut     func(*http.Request)
		via     string
		address string
		err     string
	}{
		{"socket", func(*http.Request) {}, authz.ViaLocal, "human:judy", ""},
		{"socket session", func(r *http.Request) { r.Header.Set(api.HeaderAs, "mini/abcd1234") }, authz.ViaLocal, "mini/abcd1234", ""},
		{"socket other human", func(r *http.Request) { r.Header.Set(api.HeaderAs, "human:carl") }, "", "", api.CodeForbidden},
		{"socket token", func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+secret) }, authz.ViaToken, "human:victor", ""},
		{"remote no creds", func(r *http.Request) { *r = *r.WithContext(WithRemote(r.Context(), Remote{})) }, "", "", api.CodeUnauthenticated},
		{"remote token", func(r *http.Request) {
			r.Header.Set("Authorization", "Bearer "+secret)
			*r = *r.WithContext(WithRemote(r.Context(), Remote{Login: "x@y"}))
		}, authz.ViaToken, "human:victor", ""},
		{"remote token spoofs As", func(r *http.Request) {
			r.Header.Set("Authorization", "Bearer "+secret)
			r.Header.Set(api.HeaderAs, "human:judy")
			*r = *r.WithContext(WithRemote(r.Context(), Remote{}))
		}, "", "", api.CodeForbidden},
		{"bad token", func(r *http.Request) { r.Header.Set("Authorization", "Bearer zm_x") }, "", "", api.CodeUnauthenticated},
		// Fail closed: a listener that marks nothing is network, not the owner.
		{"unmarked transport", func(r *http.Request) { *r = *r.WithContext(context.Background()) }, "", "", api.CodeUnauthenticated},
		{"unmarked transport with token", func(r *http.Request) {
			*r = *r.WithContext(context.Background())
			r.Header.Set("Authorization", "Bearer "+secret)
		}, authz.ViaToken, "human:victor", ""},
		{"unmarked claims the human", func(r *http.Request) {
			*r = *r.WithContext(context.Background())
			r.Header.Set(api.HeaderAs, "human:judy")
		}, "", "", api.CodeUnauthenticated},
	}
	for _, c := range cases {
		ctx := authed(d, c.mut)
		if c.err != "" {
			if !api.IsCode(authErrOf(ctx), c.err) || SubjectOf(ctx) != nil {
				t.Errorf("%s: want %s, got %v", c.name, c.err, authErrOf(ctx))
			}
			continue
		}
		s := SubjectOf(ctx)
		if s == nil || s.Via != c.via || s.Address != c.address {
			t.Errorf("%s: got %+v (%v)", c.name, s, authErrOf(ctx))
		}
	}
}

func TestWriteTargetAndReadScope(t *testing.T) {
	d, secret := newDeps(t)
	guest := func(project string) context.Context {
		return authed(d, func(r *http.Request) {
			r.Header.Set("Authorization", "Bearer "+secret)
			if project != "" {
				r.Header.Set(api.HeaderProject, project)
			}
			*r = *r.WithContext(WithRemote(r.Context(), Remote{}))
		})
	}
	tg, err := d.WriteTarget(guest("orchid"), "", authz.Write)
	if err != nil || tg.Project.Slug != "orchid" || tg.Area != "sprout" {
		t.Fatalf("guest write target %+v %v", tg, err)
	}
	if _, err := d.WriteTarget(guest("orchid"), "", authz.Operate); !api.IsCode(err, api.CodeForbidden) {
		t.Fatalf("guest operate: %v", err)
	}
	_, hidden := d.WriteTarget(guest("side"), "", authz.Write)
	_, missing := d.WriteTarget(guest("nope"), "", authz.Write)
	if !api.IsCode(hidden, api.CodeNotFound) || !api.IsCode(missing, api.CodeNotFound) {
		t.Fatalf("hidden %v missing %v", hidden, missing)
	}
	if _, err := d.WriteTarget(guest(""), "", authz.Write); !api.IsCode(err, api.CodeBadRequest) {
		t.Fatalf("no project: %v", err)
	}
	sc, err := d.ReadScope(guest(""), "")
	if err != nil || sc.SeesProject(mustID(t, d, "side")) || !sc.SeesProject(mustID(t, d, "orchid")) {
		t.Fatalf("read scope %v", err)
	}
	if _, err := d.ReadScope(guest(""), "side"); !api.IsCode(err, api.CodeNotFound) {
		t.Fatal(err)
	}
	// $ZM_PROJECT does not narrow lists; --project does.
	local := authed(d, func(r *http.Request) {
		r.Header.Set(api.HeaderProject, "side")
		r.Header.Set(api.HeaderProjectFrom, "env")
	})
	if sc, _ := d.ReadScope(local, ""); !sc.IsAll() {
		t.Fatal("env narrowed a list")
	}
	local = authed(d, func(r *http.Request) { r.Header.Set(api.HeaderProject, "side") })
	if sc, _ := d.ReadScope(local, ""); sc.IsAll() || sc.SeesProject(mustID(t, d, "orchid")) {
		t.Fatal("flag did not narrow")
	}
}

func mustID(t *testing.T, d *Deps, slug string) string {
	ref, ok := d.Projects.Lookup(slug)
	if !ok {
		t.Fatal(slug)
	}
	return ref.ID
}

func TestRouterGuards(t *testing.T) {
	d, secret := newDeps(t)
	r := NewRouter()
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) })
	r.Handle("GET /pub", Public, "none", ok)
	r.Handle("GET /authed", Authed, "x", ok)
	r.Handle("GET /local", Local, "x", ok)
	call := func(path string, mut func(*http.Request)) int {
		req := httptest.NewRequest("GET", path, nil)
		req = req.WithContext(WithLocal(req.Context()))
		mut(req)
		req = req.WithContext(d.Authenticate(req))
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec.Code
	}
	remote := func(req *http.Request) { *req = *req.WithContext(WithRemote(req.Context(), Remote{})) }
	tokenOnSocket := func(req *http.Request) { req.Header.Set("Authorization", "Bearer "+secret) }
	for _, c := range []struct {
		path string
		mut  func(*http.Request)
		want int
	}{
		{"/pub", remote, 204}, {"/authed", remote, 401}, {"/local", remote, 401},
		{"/authed", tokenOnSocket, 204}, {"/local", tokenOnSocket, 403},
		{"/local", func(*http.Request) {}, 204},
	} {
		if got := call(c.path, c.mut); got != c.want {
			t.Errorf("%s: %d, want %d", c.path, got, c.want)
		}
	}
	if len(r.Routes()) != 3 {
		t.Fatal(r.Routes())
	}
}
