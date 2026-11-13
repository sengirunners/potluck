package client

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sengirunners/potluck/kit/api"
)

// TestAutostart (SPEC 17.1): a request that finds no daemon calls Autostart
// once and is sent again (body intact); a failed start says why; a process
// that already reached a daemon never starts one behind the caller's back.
func TestAutostart(t *testing.T) {
	dir, err := os.MkdirTemp("", "zm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "s.sock")
	reset := func() {
		everConnected.Store(false)
		startAttempt.Lock()
		startAttempt.tried, startAttempt.err = false, nil
		startAttempt.Unlock()
	}
	reset()
	t.Cleanup(reset)

	var bodies []string
	var srv *http.Server
	start := func(context.Context) error {
		ln, err := net.Listen("unix", sock)
		if err != nil {
			return err
		}
		srv = &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var v map[string]string
			_ = json.NewDecoder(r.Body).Decode(&v)
			bodies = append(bodies, v["title"])
			w.Header().Set(api.HeaderProtocol, api.Protocol)
			api.WriteJSON(w, 200, map[string]string{"ok": "yes"})
		})}
		go func() { _ = srv.Serve(ln) }()
		return nil
	}
	t.Cleanup(func() {
		if srv != nil {
			srv.Close()
		}
	})

	// A failed start: unavailable, says the daemon did not start and why.
	// It is tried once per process: later requests get the same answer at
	// once, not another start each (a long-lived loop would retry 10 s starts).
	c := NewUnix(sock)
	failed := 0
	c.Autostart = func(context.Context) error { failed++; return errors.New("lock held") }
	for i := 0; i < 3; i++ {
		err = c.Do(context.Background(), http.MethodGet, "/v0/health", nil, nil)
		if !api.IsCode(err, api.CodeUnavailable) || !strings.Contains(err.Error(), "did not start: lock held") {
			t.Fatalf("failed start, request %d: %v", i, err)
		}
	}
	if failed != 1 {
		t.Fatalf("a failed start was tried %d times in one process, want 1", failed)
	}
	// An attempt cut short by the caller's context does not count.
	reset()
	ctx, cancel := context.WithCancel(context.Background())
	c.Autostart = func(context.Context) error { cancel(); return context.Canceled }
	_ = c.Do(ctx, http.MethodGet, "/v0/health", nil, nil)
	if startAttempt.tried {
		t.Fatal("a cancelled start counted as the attempt")
	}

	calls := 0
	c.Autostart = func(ctx context.Context) error { calls++; return start(ctx) }
	if err := c.Do(context.Background(), http.MethodPost, "/v0/work", map[string]string{"title": "first"}, nil); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || len(bodies) != 1 || bodies[0] != "first" {
		t.Fatalf("autostart calls %d, bodies %q", calls, bodies)
	}
	// The daemon goes away: this process reached one, so it is not restarted.
	srv.Close()
	_ = os.Remove(sock)
	c2 := NewUnix(sock) // a fresh connection pool: no kept-alive connection to the old one
	c2.Autostart = c.Autostart
	err = c2.Do(context.Background(), http.MethodGet, "/v0/health", nil, nil)
	if calls != 1 || !NotRunning(err) || !api.IsCode(err, api.CodeUnavailable) || !strings.Contains(err.Error(), "daemon not running") {
		t.Fatalf("after a daemon went away: calls %d, %v", calls, err)
	}
	// Without Autostart nothing is started (the default).
	everConnected.Store(false)
	if err := NewUnix(sock).Do(context.Background(), http.MethodGet, "/v0/health", nil, nil); !NotRunning(err) {
		t.Fatalf("no autostart: %v", err)
	}
}
