package cli

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/hometerm"
	"github.com/sengirunners/potluck/kit/swap"
)

// shortTemp is a temp home short enough for its socket's path (macOS allows
// 103 bytes; its t.TempDir is long).
func shortTemp(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "pl")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// With no daemon running, freeze and unfreeze change the home's term under
// its locks, at the same epoch; a move the term refuses, or a home another
// process holds, changes nothing.
func TestFreezeAndUnfreezeWithNoDaemon(t *testing.T) {
	home := shortTemp(t)
	if err := hometerm.Create(home, hometerm.Term{Epoch: 3, State: hometerm.Serving}); err != nil {
		t.Fatal(err)
	}
	term := func() hometerm.Term {
		t.Helper()
		got, err := hometerm.Load(home)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}

	if out := okAs(t, home, "daemon", "freeze"); !strings.Contains(out, "frozen at epoch 3") {
		t.Fatalf("freeze said %q", out)
	}
	if got := term(); got.State != hometerm.Frozen || got.Epoch != 3 {
		t.Fatalf("after freeze: %+v", got)
	}
	if _, e, code := runAs(t, home, "daemon", "freeze"); code == 0 || !strings.Contains(e, "frozen") {
		t.Fatalf("a second freeze: exit %d %q", code, e)
	}

	// Another process changing the home, or a daemon whose socket is gone:
	// refused, the term as it was.
	for name, lock := range map[string]func(string) (func(), error){"writer": config.LockWriter, "daemon": config.LockHome} {
		release, err := lock(home)
		if err != nil {
			t.Fatal(err)
		}
		_, e, code := runAs(t, home, "daemon", "unfreeze")
		release()
		if code == 0 || !strings.Contains(e, "pid") {
			t.Fatalf("unfreeze while the %s lock is held: exit %d %q", name, code, e)
		}
		if got := term(); got.State != hometerm.Frozen {
			t.Fatalf("a refused unfreeze (%s lock) changed the term: %+v", name, got)
		}
	}

	if out := okAs(t, home, "daemon", "unfreeze"); !strings.Contains(out, "serves again at epoch 3") {
		t.Fatalf("unfreeze said %q", out)
	}
	if got := term(); got.State != hometerm.Serving || got.Epoch != 3 {
		t.Fatalf("after unfreeze: %+v", got)
	}
}

// With a daemon running, freeze and unfreeze start it again with the
// listeners it ran with: one that joined the tailnet (its health says so)
// is started with --tailnet, so a home whose move is called off is
// reachable on its tailnet again. The daemon here is a fake on the home's
// socket that stops after its answer, and the start is recorded, not run.
func TestFreezeAndUnfreezeKeepTheTailnet(t *testing.T) {
	for _, what := range []string{"freeze", "unfreeze"} {
		for _, tailnet := range []bool{true, false} {
			home := shortTemp(t)
			cfg, err := config.LoadClientFrom(home)
			if err != nil {
				t.Fatal(err)
			}
			ln, err := net.Listen("unix", cfg.SocketPath())
			if err != nil {
				t.Fatal(err)
			}
			srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				api.SetHeader(w.Header(), api.HeaderProtocol, api.WireProtocol)
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/v0/health":
					api.WriteJSON(w, http.StatusOK, api.Health{OK: true, Protocol: api.WireProtocol, Machine: "host", Tailnet: tailnet})
				case r.Method == http.MethodPost && r.URL.Path == "/v0/authority/"+what:
					api.WriteJSON(w, http.StatusOK, api.AuthorityTerm{State: map[string]string{"freeze": api.HQFrozen, "unfreeze": api.HQServing}[what], Epoch: 3})
					go ln.Close() // the daemon stops after its answer
				default:
					http.NotFound(w, r)
				}
			})}
			srv.SetKeepAlivesEnabled(false) // a closed listener ends the daemon
			go func() { _ = srv.Serve(ln) }()
			t.Cleanup(func() { _ = srv.Close() })

			var argv []string
			was := spawnDetached
			spawnDetached = func(_ context.Context, d swap.Detach) (*swap.Detached, error) {
				argv = d.Argv
				return nil, errors.New("not started in a test")
			}
			_, e, code := runAs(t, home, "daemon", what)
			spawnDetached = was
			if code == 0 || !strings.Contains(e, "did not start again") {
				t.Fatalf("%s (tailnet %v): exit %d %q, want the recorded start's refusal", what, tailnet, code, e)
			}
			if len(argv) < 2 || argv[1] != "daemon" || slices.Contains(argv, "--tailnet") != tailnet {
				t.Fatalf("%s of a daemon with tailnet %v started %q", what, tailnet, argv)
			}
		}
	}
}
