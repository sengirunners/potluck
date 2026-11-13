package core

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPublicHTTPHostBoundary(t *testing.T) {
	d := &Deps{}
	domain := "example.test"
	ingress := PublicIngress(d)
	ingress.RegisterHostSource(func(host string) bool { return ValidPublicSuffix(domain) && strings.HasSuffix(host, "."+domain) }, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(201) }))
	h := ingress.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(401) }))
	for host, want := range map[string]int{"token.example.test": 201, "example.test": 401, "token.example.test.evil.test": 401, "tokenexample.test": 401, "token.other.test": 401} {
		r := httptest.NewRequest("GET", "http://"+host+"/v0/private", nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != want {
			t.Errorf("%s: %d want %d", host, w.Code, want)
		}
	}
	domain = ""
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "http://token.example.test/", nil))
	if w.Code != 401 {
		t.Fatal("disabled source still admits requests")
	}
	for _, bad := range []string{"localhost", "127.0.0.1", "UPPER.example", "foo.example:443", "foo.example/route", "-foo.example", "foo..example"} {
		if ValidPublicSuffix(bad) {
			t.Errorf("accepted domain %q", bad)
		}
	}
}
