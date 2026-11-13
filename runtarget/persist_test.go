package runtarget_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/computeclient"
	"github.com/sengirunners/potluck/conformance/refdriver"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/server"
	"github.com/sengirunners/potluck/runtarget"
	"github.com/sengirunners/potluck/wiring"
)

// keptPool is a daemon with the project acme, a ref driver and two members.
type keptPool struct {
	svc     *runtarget.Service
	owner   *client.Client
	ana, bo *client.Client
}

func newKeptPool(t *testing.T) keptPool {
	t.Helper()
	ctx := context.Background()
	home, err := os.MkdirTemp("", "zm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	t.Setenv("HOME", home)
	cfg, err := config.LoadFrom(home)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Human, cfg.Machine = "judy", "mini"
	dm, err := server.New(context.Background(), cfg, nil, wiring.Compute)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { dm.Close() })
	fix, _ := filepath.EvalSymlinks(t.TempDir())
	state := filepath.Join(fix, "state")
	drv, err := refdriver.Install(fix, state, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { refdriver.KillAll(state) })
	var svc *runtarget.Service
	for _, m := range dm.Modules() {
		if s, ok := m.(*runtarget.Service); ok {
			svc = s
			s.SetLookPath(func(string) (string, error) { return drv, nil })
		}
	}
	owner := client.WithHandler(dm.SocketHandler())
	owner.Project, owner.ProjectFrom = "acme", "flag"
	if _, err := owner.CreateProject(ctx, api.NewProjectRequest{Slug: "acme"}); err != nil {
		t.Fatal(err)
	}
	mint := func(as string) *client.Client {
		tok, err := owner.MintToken(ctx, api.MintTokenRequest{As: as, Project: "acme", Role: "member"})
		if err != nil {
			t.Fatal(err)
		}
		c := client.WithHandler(dm.Handler)
		c.Token, c.Project, c.ProjectFrom = tok.Secret, "acme", "flag"
		return c
	}
	return keptPool{svc: svc, owner: owner, ana: mint("ana"), bo: mint("bo")}
}

// TestKeptDisks (zm-eakh.12.5.23): a box acquired with persist stops with its
// disk kept and starts again with the same files, for its holder only; a
// lease's end stops it rather than ending it; release deletes it, and the
// end hook (the usage ledger) fires once, at that end.
func TestKeptDisks(t *testing.T) {
	pool := newKeptPool(t)
	ctx, svc, owner, ana, bo := context.Background(), pool.svc, pool.owner, pool.ana, pool.bo
	var mu sync.Mutex
	var ended []computeapi.RunTargetHandle
	svc.OnEnd(func(_ context.Context, _ string, h computeapi.RunTargetHandle) {
		mu.Lock()
		ended = append(ended, h)
		mu.Unlock()
	})
	for name, conf := range map[string]map[string]string{"box": {"jit": "1"}, "wiped": {"persist": "0"}} {
		if _, err := computeclient.BindRunTarget(ctx, owner, computeapi.BindRunTargetRequest{Target: computeapi.RunTarget{Name: name, Driver: "ref", Kind: "box",
			Isolation: "ephemeral", Images: []string{refPin}, Config: conf}}); err != nil {
			t.Fatal(err)
		}
	}

	// Refusals: a driver that keeps no disks, a runner box, stop of a throwaway box.
	if _, err := computeclient.AcquireRunTarget(ctx, ana, "wiped", computeapi.AcquireRunTargetRequest{RequestID: "w1", Persist: true}); code(err) != api.CodeBadRequest {
		t.Errorf("persist on a driver without the capability: %v", err)
	}
	if _, err := computeclient.AcquireRunTarget(ctx, ana, "box", computeapi.AcquireRunTargetRequest{RequestID: "j1", Persist: true, JITConfig: "QUJDREVGR0hJSktMTU5PUA=="}); code(err) != api.CodeBadRequest ||
		!strings.Contains(err.Error(), "never persistent") {
		t.Errorf("persist with a jit_config: %v", err)
	}
	plain, err := computeclient.AcquireRunTarget(ctx, ana, "box", computeapi.AcquireRunTargetRequest{RequestID: "p1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := computeclient.StopRunTarget(ctx, ana, "box", idOf(plain.Handle)); code(err) != api.CodeBadRequest {
		t.Errorf("stop of a box without persist: %v", err)
	}
	if _, err := computeclient.ReleaseRunTarget(ctx, ana, "box", idOf(plain.Handle)); err != nil {
		t.Fatal(err)
	}

	h, err := computeclient.AcquireRunTarget(ctx, ana, "box", computeapi.AcquireRunTargetRequest{RequestID: "k1", TTLS: 600, Persist: true})
	if err != nil || !h.Persist {
		t.Fatalf("acquire with persist: %+v %v", h, err)
	}
	if _, err := computeclient.AcquireRunTarget(ctx, ana, "box", computeapi.AcquireRunTargetRequest{RequestID: "k1", TTLS: 600}); code(err) != api.CodeConflict {
		t.Errorf("the same request_id without persist: %v", err)
	}
	id := idOf(h.Handle)
	if _, err := computeclient.ExecRunTarget(ctx, ana, "box", id, computeapi.ExecRunTargetRequest{Argv: []string{"sh", "-c", `echo kept > "$HOME/f"`}}); err != nil {
		t.Fatal(err)
	}
	if _, err := computeclient.StopRunTarget(ctx, bo, "box", id); code(err) != api.CodeForbidden {
		t.Errorf("stop by another member: %v", err)
	}
	st, err := computeclient.StopRunTarget(ctx, ana, "box", id)
	if err != nil || st.State != computeapi.StateStopped || st.Ended != "" {
		t.Fatalf("stop: %+v %v", st, err)
	}
	if exp, err := time.Parse(time.RFC3339Nano, st.ExpiresAt); err != nil || exp.Before(time.Now().Add(29*24*time.Hour)) {
		t.Errorf("a stopped box's expires_at is its disk's, 30 days out: %s", st.ExpiresAt)
	}
	if again, err := computeclient.StopRunTarget(ctx, ana, "box", id); err != nil || again.State != computeapi.StateStopped {
		t.Errorf("a second stop: %+v %v", again, err)
	}
	if _, err := computeclient.ExecRunTarget(ctx, ana, "box", id, computeapi.ExecRunTargetRequest{Argv: []string{"true"}}); code(err) != api.CodeConflict {
		t.Errorf("exec in a stopped box: %v", err)
	}
	if _, err := computeclient.RenewRunTarget(ctx, ana, "box", id, 60); code(err) != api.CodeConflict {
		t.Errorf("renew of a stopped box: %v", err)
	}
	if _, err := computeclient.StartRunTarget(ctx, bo, "box", id, 0); code(err) != api.CodeForbidden {
		t.Errorf("start by another member: %v", err)
	}
	if _, err := computeclient.StartRunTarget(ctx, owner, "box", id, 0); code(err) != api.CodeForbidden {
		t.Errorf("start by the machine owner (not the holder): %v", err)
	}
	if _, err := computeclient.StartRunTarget(ctx, ana, "box", id, 7200); code(err) != api.CodeBadRequest {
		t.Errorf("start past the lifetime cap: %v", err)
	}
	up, err := computeclient.StartRunTarget(ctx, ana, "box", id, 600)
	if err != nil || up.State != "ready" || up.Started == "" {
		t.Fatalf("start: %+v %v", up, err)
	}
	out, err := computeclient.ExecRunTarget(ctx, ana, "box", id, computeapi.ExecRunTargetRequest{Argv: []string{"cat", "f"}})
	if err != nil || out.Stdout != "kept\n" {
		t.Fatalf("the file after start: %+v %v", out, err)
	}
	// The start keeps the spend figure as of it (what ran before); a status
	// call later raises Spend with the run since, never that figure.
	if up.StartedSpend == nil || st.Spend == nil || up.StartedSpend.Amount < st.Spend.Amount {
		t.Fatalf("the figure kept at start: %+v, stopped at %+v", up.StartedSpend, st.Spend)
	}
	time.Sleep(1100 * time.Millisecond)
	cur, err := computeclient.RunTargetStatus(ctx, ana, "box", id)
	if err != nil || cur.Spend == nil || cur.StartedSpend == nil || *cur.StartedSpend != *up.StartedSpend || cur.Spend.Amount < up.StartedSpend.Amount+1 {
		t.Fatalf("status after the start: spend %+v, kept %+v (at start %+v) %v", cur.Spend, cur.StartedSpend, up.StartedSpend, err)
	}

	// A lease's end stops a persistent box; the sweep records it stopped and live.
	short, err := computeclient.AcquireRunTarget(ctx, ana, "box", computeapi.AcquireRunTargetRequest{RequestID: "k2", TTLS: 1, Persist: true})
	if err != nil {
		t.Fatal(err)
	}
	lagged, err := computeclient.AcquireRunTarget(ctx, ana, "box", computeapi.AcquireRunTargetRequest{RequestID: "k3", TTLS: 1, Persist: true})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(1500 * time.Millisecond)
	// Before the sweep asks, the row still says ready: start asks the driver, which stopped it.
	if up, err := computeclient.StartRunTarget(ctx, ana, "box", idOf(lagged.Handle), 600); err != nil || up.State != "ready" || up.Started == "" {
		t.Errorf("start right after a lease-end stop: %+v %v, want a fresh start", up, err)
	} else if exp, _ := time.Parse(time.RFC3339Nano, up.ExpiresAt); !exp.After(time.Now().Add(time.Minute)) {
		t.Errorf("start right after a lease-end stop answered expires_at %s: the stale lease", up.ExpiresAt)
	}
	svc.SetNow(func() time.Time { return time.Now().Add(10 * time.Second) }) // past the sweep's grace
	svc.Sweep(ctx)
	svc.SetNow(time.Now)
	hs, err := computeclient.RunTargetHandles(ctx, ana, "box", true)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, x := range hs {
		if x.Handle == short.Handle {
			found = true
			if x.State != computeapi.StateStopped || x.Ended != "" {
				t.Errorf("a persistent box past its lease: %+v, want stopped and live", x)
			}
		}
	}
	if !found {
		t.Errorf("the stopped box is not among the live handles: %+v", hs)
	}

	mu.Lock()
	if len(ended) != 1 { // the throwaway box alone
		t.Errorf("end hook before release: %+v", ended)
	}
	mu.Unlock()
	for _, x := range []string{h.Handle, short.Handle, lagged.Handle} {
		rel, err := computeclient.ReleaseRunTarget(ctx, ana, "box", idOf(x))
		if err != nil || !rel.Released {
			t.Fatalf("release %s: %+v %v", x, rel, err)
		}
	}
	if _, err := computeclient.StartRunTarget(ctx, ana, "box", idOf(short.Handle), 0); code(err) != api.CodeConflict {
		t.Errorf("start after release: %v", err)
	}
	time.Sleep(200 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if len(ended) != 4 || ended[1].State != "released" || ended[3].State != "released" || !ended[3].Persist {
		t.Errorf("end hook: %+v", ended)
	}
}

// TestStartChecksBudget (zm-eakh.12.5.37): a start is a new lease, so a
// binding whose budget for the period is spent refuses it with the reason an
// acquire gives.
func TestStartChecksBudget(t *testing.T) {
	pool := newKeptPool(t)
	ctx, ana := context.Background(), pool.ana
	if _, err := computeclient.BindRunTarget(ctx, pool.owner, computeapi.BindRunTargetRequest{Target: computeapi.RunTarget{Name: "capped", Driver: "ref", Kind: "box",
		Isolation: "ephemeral", Images: []string{refPin}, Config: map[string]string{"jit": "1"},
		Budget: &computeapi.RunTargetBudget{Unit: "cpu_s", Limit: 1, Period: "day"}}}); err != nil {
		t.Fatal(err)
	}
	h, err := computeclient.AcquireRunTarget(ctx, ana, "capped", computeapi.AcquireRunTargetRequest{RequestID: "k1", TTLS: 600, Persist: true})
	if err != nil {
		t.Fatalf("acquire under budget: %v", err)
	}
	id := idOf(h.Handle)
	time.Sleep(1200 * time.Millisecond) // the box runs past its one cpu_s
	if _, err := computeclient.StopRunTarget(ctx, ana, "capped", id); err != nil {
		t.Fatal(err)
	}
	_, err = computeclient.AcquireRunTarget(ctx, ana, "capped", computeapi.AcquireRunTargetRequest{RequestID: "k2", TTLS: 600})
	if code(err) != api.CodeForbidden || !strings.Contains(err.Error(), "(budget)") {
		t.Fatalf("acquire over budget: %v", err)
	}
	_, err = computeclient.StartRunTarget(ctx, ana, "capped", id, 0)
	if code(err) != api.CodeForbidden || !strings.Contains(err.Error(), "run target capped spent") || !strings.Contains(err.Error(), "(budget)") {
		t.Fatalf("start over budget: %v", err)
	}
}
