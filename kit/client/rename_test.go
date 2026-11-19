package client

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sengirunners/potluck/kit/api"
)

// stepOneDaemonTakes is a step 1 daemon's (potluck 358a1e8c) check of a
// request: the protocol under Potluck-Protocol, else Zm-Protocol, refused
// only when set and of another major than zm/v0 (potluck/1 is one).
func stepOneDaemonTakes(h http.Header) bool {
	p := api.GetHeader(h, api.HeaderProtocol)
	return p == "" || api.SameProtocol(p, "zm/v0")
}

// What a client of this build sends (rename step 2): Potluck-* names and
// potluck/1, no Zm-* name, which a step 1 daemon reads. It takes an answer
// from a step 1 daemon (Zm-Protocol: zm/v0) and from one of this build.
func TestClientSendsNewNamesReadsBoth(t *testing.T) {
	var got atomic.Pointer[http.Header]
	answer := func(w http.ResponseWriter) {}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := r.Header.Clone()
		got.Store(&h)
		answer(w)
		api.WriteJSON(w, 200, api.Health{OK: true, Protocol: api.Protocol})
	}))
	defer srv.Close()
	c := NewRemote(srv.URL, nil)
	c.As, c.Project, c.ProjectFrom, c.Cwd, c.Token = "mini/abcd1234", "side", "env", "/tmp", "potluck_x"
	// A step 1 daemon answers under the old name.
	answer = func(w http.ResponseWriter) { w.Header().Set("Zm-Protocol", "zm/v0") }
	if _, err := c.Health(context.Background()); err != nil {
		t.Fatal(err)
	}
	h := *got.Load()
	want := map[string]string{"Potluck-Protocol": "potluck/1", "Potluck-As": "mini/abcd1234", "Potluck-Project": "side",
		"Potluck-Project-From": "env", "Authorization": "Bearer potluck_x"} // Potluck-Cwd: socket only
	for k, v := range want {
		if h.Get(k) != v {
			t.Errorf("sent %s = %q, want %q", k, h.Get(k), v)
		}
	}
	for k := range h {
		if strings.HasPrefix(strings.ToLower(k), "zm-") {
			t.Errorf("sent an old name: %s", k)
		}
	}
	if !stepOneDaemonTakes(h) || api.GetHeader(h, api.HeaderAs) != "mini/abcd1234" || api.GetHeader(h, api.HeaderProject) != "side" {
		t.Fatalf("a step 1 daemon would not read it: %v", h)
	}
	// A caller's old-name header (zm's code may set one) never wins over
	// what the client sets.
	resp, err := c.send(context.Background(), http.MethodGet, "/v0/health", nil, http.Header{"Zm-Project": {"stale"}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if h := *got.Load(); api.GetHeader(h, api.HeaderProject) != "side" || h.Get("Zm-Project") != "" {
		t.Fatalf("a stale Zm-Project went: %v", h)
	}

	// This build's daemon answers under Potluck-Protocol: potluck/1.
	answer = func(w http.ResponseWriter) { w.Header().Set("Potluck-Protocol", "potluck/1") }
	if _, err := c.Health(context.Background()); err != nil {
		t.Fatalf("potluck/1 answer: %v", err)
	}
	answer = func(w http.ResponseWriter) { w.Header().Set("Potluck-Protocol", "potluck/2") }
	_, err = c.Health(context.Background())
	var ae *api.Error
	if !errors.As(err, &ae) || ae.Code != api.CodeUnavailable || ae.Message != "the daemon speaks potluck/2; this potluck speaks potluck/1" ||
		ae.Hint != "use the same potluck version for the daemon and the CLI (potluck daemon stop; potluck daemon --detach)" {
		t.Fatalf("potluck/2 answer: %v", err)
	}
	answer = func(w http.ResponseWriter) {}
	if _, err := c.Health(context.Background()); !api.IsCode(err, api.CodeUnavailable) {
		t.Fatalf("an answer with no protocol: %v", err)
	}
}

func TestHostURLNodePrefixes(t *testing.T) {
	for in, want := range map[string]string{
		"mini":         "http://zm-mini:7433",
		"zm-mini":      "http://zm-mini:7433",
		"potluck-mini": "http://potluck-mini:7433",
	} {
		if got := HostURL(in); got != want {
			t.Errorf("HostURL(%q) = %q, want %q", in, got, want)
		}
	}
}

// With the default program the client's messages name potluck.
func TestClientMessagesUnchanged(t *testing.T) {
	e := cutOff("/s/zm.sock", http.MethodPost)
	if e.Message != "the potluck daemon at /s/zm.sock stopped or restarted during this request" ||
		e.Hint != "it may or may not have taken effect: check, then retry once the daemon is back (potluck status; potluck daemon --detach starts it)" {
		t.Fatalf("cutOff: %+v", e)
	}
	if e := cutOff("/s/zm.sock", http.MethodGet); e.Hint != "run it again once the daemon is back (potluck status; potluck daemon --detach starts it)" {
		t.Fatalf("cutOff GET: %+v", e)
	}
}
