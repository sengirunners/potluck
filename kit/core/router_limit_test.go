package core

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
)

// The router's own guard refuses a limited subject on a route that does not
// admit its limit, for a router served without the server's choke point,
// and a public route sees no subject and no token.
func TestRouterGuardRechecksLimit(t *testing.T) {
	r := NewRouter()
	reached := map[string]bool{}
	var publicSaw []string
	h := func(w http.ResponseWriter, req *http.Request) { reached[req.Method+" "+req.URL.Path] = true }
	r.HandleFunc("GET /a", Authed, "read", h)
	r.HandleFunc("GET /b", Authed, "read", h)
	r.HandleFunc("GET /p", Public, "none", func(w http.ResponseWriter, req *http.Request) {
		if SubjectOf(req.Context()) != nil {
			publicSaw = append(publicSaw, "subject")
		}
		if req.Header.Get(api.HeaderAuthorization) != "" {
			publicSaw = append(publicSaw, "token")
		}
	})
	r.Admit("answer", "GET /a")
	r.Admit(AnyLimit, "GET /b")
	if err := r.CheckAdmits(); err != nil {
		t.Fatal(err)
	}
	limited := &authz.Subject{Via: authz.ViaToken, Remote: true, Cap: &authz.Cap{Role: authz.Owner, Limit: "answer"}}
	other := &authz.Subject{Via: authz.ViaToken, Remote: true, Cap: &authz.Cap{Role: authz.Owner, Limit: "other"}}
	call := func(sub *authz.Subject, path string) int {
		req := httptest.NewRequest("GET", path, nil)
		req.Header.Set(api.HeaderAuthorization, "Bearer x")
		req = req.WithContext(WithSubject(req.Context(), sub))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w.Code
	}
	if c := call(limited, "/a"); c != http.StatusOK || !reached["GET /a"] {
		t.Fatalf("admitted: %d", c)
	}
	if c := call(other, "/a"); c != http.StatusForbidden {
		t.Fatalf("another limit: %d", c)
	}
	if c := call(other, "/b"); c != http.StatusOK {
		t.Fatalf("any limit: %d", c)
	}
	if c := call(&authz.Subject{Via: authz.ViaToken, Remote: true, Cap: &authz.Cap{Role: authz.Owner}}, "/a"); c != http.StatusOK {
		t.Fatalf("no limit: %d", c)
	}
	if call(limited, "/p"); len(publicSaw) != 0 {
		t.Fatalf("public saw %v", publicSaw)
	}
	if got := r.Limits(); len(got) != 1 || got[0] != "answer" {
		t.Fatalf("Limits %v", got)
	}
	for _, rt := range r.Routes() {
		if rt.Pattern == "GET /a" && (len(rt.Limits) != 1 || rt.Limits[0] != "answer") {
			t.Fatalf("route %+v", rt)
		}
	}
}
