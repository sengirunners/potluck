package tunnel

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/core"
)

// Reachable is nil only for the home's own signed answer; an edge error,
// an unsigned answer, another home's key and no answer are each a RouteDown
// (unavailable) with its own reason and fix, and none of the requests it
// sends carries anything but the nonce.
func TestReachable(t *testing.T) {
	s, srv, _ := home(t)
	fp, err := s.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	if err := Reachable(context.Background(), srv.URL, fp); err != nil {
		t.Fatalf("the home itself: %v", err)
	}
	other := newService(t)
	ofp, err := other.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	var sawAuth bool
	edge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuth = sawAuth || r.Header.Get("Authorization") != ""
		w.WriteHeader(530)
	}))
	t.Cleanup(edge.Close)
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(401) }))
	t.Cleanup(plain.Close)
	dead := httptest.NewServer(http.NotFoundHandler())
	dead.Close()
	for name, c := range map[string]struct{ url, fp, why, fix string }{
		"edge error":   {edge.URL, fp, "Cloudflare cannot reach this machine's cloudflared (status 530)", "cloudflared is running"},
		"unsigned":     {plain.URL, fp, "does not answer as this home (status 401)", "forwards to this daemon's tunnel listener"},
		"another home": {srv.URL, ofp, "another", "points elsewhere"},
		"no answer":    {dead.URL, fp, "it did not answer", "online"},
	} {
		err := Reachable(context.Background(), c.url, c.fp)
		var rd core.RouteDown
		if !errors.As(err, &rd) || !api.IsCode(err, api.CodeUnavailable) || !strings.Contains(rd.Err.Message, c.why) || !strings.Contains(rd.Err.Hint, c.fix) {
			t.Errorf("%s: %v (hint %q), want a RouteDown saying %q and %q", name, err, func() string {
				if rd.Err != nil {
					return rd.Err.Hint
				}
				return ""
			}(), c.why, c.fix)
		}
	}
	if sawAuth {
		t.Error("the check sent an Authorization header")
	}
}
