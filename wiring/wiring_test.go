package wiring_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/server"
	"github.com/sengirunners/potluck/kit/store"
	"github.com/sengirunners/potluck/kit/swap"
	"github.com/sengirunners/potluck/wiring"
)

// The daemon shell runs with fewer modules than zm's: the KIT base alone,
// and KIT plus compute. Each answers /v0/health on its socket, creates a
// project, honours a stop request and leaves no socket behind.
func TestSmallerWirings(t *testing.T) {
	for _, tc := range []struct {
		name string
		w    server.Wiring
	}{{"kit", wiring.Kit}, {"compute", wiring.Compute}} {
		t.Run(tc.name, func(t *testing.T) {
			home, err := os.MkdirTemp("", "zm")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { os.RemoveAll(home) })
			cfg, err := config.LoadFrom(home)
			if err != nil {
				t.Fatal(err)
			}
			cfg.Human, cfg.Machine = "ana", "kit"
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			dm, err := server.New(ctx, cfg, nil, tc.w)
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- dm.Serve(ctx) }()
			c := client.NewUnix(cfg.SocketPath())
			if err := c.WaitReady(ctx, 5*time.Second); err != nil {
				t.Fatal(err)
			}
			if h, err := c.Health(ctx); err != nil || !h.OK || h.Machine != "kit" || h.PID == 0 {
				t.Fatalf("health %+v %v", h, err)
			}
			if p, err := c.CreateProject(ctx, api.NewProjectRequest{Slug: "base"}); err != nil || p.Role != "owner" {
				t.Fatalf("create %+v %v", p, err)
			}
			if r, err := c.StopDaemon(ctx, api.DaemonStopRequest{}); err != nil || !r.Stopping {
				t.Fatalf("stop %+v %v", r, err)
			}
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("serve: %v", err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("the daemon did not stop")
			}
			if _, err := os.Stat(cfg.SocketPath()); !os.IsNotExist(err) {
				t.Fatalf("socket left behind: %v", err)
			}
		})
	}
}

// A module's migrations and self-check probe parts come with its wiring,
// not with being linked: in one process, a KIT daemon has none of compute's
// tables and never runs a part another daemon's wiring added.
func TestModulesBringTheirOwnSchemaAndProbes(t *testing.T) {
	ctx := context.Background()
	kit, err := server.ThisBuild(ctx, wiring.Kit)
	if err != nil {
		t.Fatal(err)
	}
	compute, err := server.ThisBuild(ctx, wiring.Compute)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range []struct{ kind, name string }{
		{swap.KindRegistry, "runners/0001_init"}, {swap.KindRegistry, "runtarget/0001_host_keys"}, {swap.KindRegistry, "issuers/0001_ledger"},
		{swap.KindProject, "runtarget/0001_handles"}, {swap.KindProject, "usage/0001_box_usage"}, {swap.KindProject, "grants/0001_secret_grants"},
	} {
		if !slices.Contains(compute.Schema[m.kind], m.name) || slices.Contains(kit.Schema[m.kind], m.name) {
			t.Errorf("%s %s: compute has it %v, kit has it %v", m.kind, m.name,
				slices.Contains(compute.Schema[m.kind], m.name), slices.Contains(kit.Schema[m.kind], m.name))
		}
	}
	for _, name := range []string{"owners/0001_links", "team/0001_remotes"} { // KIT's own modules
		if !slices.Contains(kit.Schema[swap.KindRegistry], name) {
			t.Errorf("kit's schema has no %s", name)
		}
	}

	selfCheck := func(w server.Wiring) api.SelfCheck {
		home, err := os.MkdirTemp("", "zm")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.RemoveAll(home) })
		cfg, err := config.LoadFrom(home)
		if err != nil {
			t.Fatal(err)
		}
		cfg.Human, cfg.Machine = "ana", "kit"
		dm, err := server.New(ctx, cfg, nil, w)
		if err != nil {
			t.Fatal(err)
		}
		defer dm.Close()
		return dm.SelfCheck(ctx)
	}
	withPart := func(d *core.Deps) ([]core.Module, error) {
		d.AddProbe("extra", func(_ *client.Client, p *swap.Probing) { p.Add("extra", "*", errors.New("extra is down")) })
		return wiring.Kit(d)
	}
	if sc := selfCheck(withPart); sc.OK || !slices.ContainsFunc(sc.Problems, func(p string) bool { return strings.Contains(p, "extra is down") }) {
		t.Fatalf("the wiring's part did not run: %+v", sc)
	}
	if sc := selfCheck(wiring.Kit); !sc.OK {
		t.Fatalf("another daemon's part ran here: %+v", sc)
	}
}

// A wiring's request gates see every request after authentication and
// before the team proxy and the router, in the order added: one refuses
// before anything is served, another serves through the rest.
func TestRequestGatesRunBeforeTheRouter(t *testing.T) {
	home, err := os.MkdirTemp("", "zm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	cfg, err := config.LoadFrom(home)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Human, cfg.Machine = "ana", "kit"
	var callers []string
	unauthenticated := 0
	w := func(d *core.Deps) ([]core.Module, error) {
		d.AddRequestGate(func(w http.ResponseWriter, r *http.Request, serve func(http.ResponseWriter, *http.Request)) bool {
			if sub := core.SubjectOf(r.Context()); sub != nil {
				callers = append(callers, sub.Address)
			} else {
				unauthenticated++
			}
			if r.Method == http.MethodPost && r.URL.Path == "/v0/projects" {
				api.WriteError(w, api.Errorf(api.CodeForbidden, "gated"))
				return true
			}
			return false
		})
		d.AddRequestGate(func(w http.ResponseWriter, r *http.Request, serve func(http.ResponseWriter, *http.Request)) bool {
			w.Header().Set("X-Second-Gate", "1")
			serve(w, r)
			return true
		})
		return wiring.Kit(d)
	}
	dm, err := server.New(context.Background(), cfg, nil, w)
	if err != nil {
		t.Fatal(err)
	}
	defer dm.Close()
	h := dm.SocketHandler()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v0/projects", nil))
	if rec.Code != http.StatusOK || rec.Header().Get("X-Second-Gate") != "1" {
		t.Fatalf("list through both gates: %d %v %s", rec.Code, rec.Header(), rec.Body)
	}
	rec = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v0/projects", strings.NewReader(`{"slug":"base"}`))
	api.SetHeader(req.Header, api.HeaderProtocol, api.WireProtocol)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden || rec.Header().Get("X-Second-Gate") != "" || !strings.Contains(rec.Body.String(), "gated") {
		t.Fatalf("create past the first gate: %d %v %s", rec.Code, rec.Header(), rec.Body)
	}
	if ps, err := client.WithHandler(h).Projects(context.Background()); err != nil || len(ps) != 0 {
		t.Fatalf("a refused create made a project: %+v %v", ps, err)
	}
	if len(callers) != 3 || callers[0] == "" {
		t.Fatalf("the gate saw callers %q, want the authenticated owner on each of 3 calls", callers)
	}
	// A network caller with no token: the gate sees it before the route's
	// auth class refuses it.
	rec = httptest.NewRecorder()
	dm.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v0/projects", nil))
	if rec.Code != http.StatusUnauthorized || unauthenticated != 1 {
		t.Fatalf("an unauthenticated call: %d, the gate saw %d unauthenticated", rec.Code, unauthenticated)
	}
}

// zm registers some of its migrations from init() that use a table a
// Potluck module's wiring makes: its team/0020_task_line indexes teamlink's
// team_task_refs (team/0005_machine_ops). This one stands in for it.
func init() {
	store.RegisterRegistry("team/0020_test_task_line", `CREATE INDEX test_task_refs_parent ON team_task_refs (parent);`)
}

// On a fresh home, a migration registered from init() runs after the module
// migration whose table it uses (one name order pass once the wiring has
// built), and the home opens again with nothing left to run.
func TestRegisteredMigrationsRunAfterTheModulesTheyUse(t *testing.T) {
	home, err := os.MkdirTemp("", "zm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	cfg, err := config.LoadFrom(home)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Human, cfg.Machine = "ana", "kit"
	ctx := context.Background()
	for range 2 {
		dm, err := server.New(ctx, cfg, nil, wiring.Compute)
		if err != nil {
			t.Fatal(err)
		}
		var n int
		err = dm.Deps.Store.Registry().QueryRowContext(ctx,
			`SELECT count(*) FROM sqlite_master WHERE type = 'index' AND name = 'test_task_refs_parent'`).Scan(&n)
		dm.Close()
		if err != nil || n != 1 {
			t.Fatalf("the registered migration's index: %d %v", n, err)
		}
	}
}
