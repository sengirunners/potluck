package runtarget_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

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

// TestAcquireFallThrough (zm-eakh.12.5.30): a sandbox whose first-ranked
// binding refuses before recording (handles_max here) lands on the second
// with the same request id; a retry of that id once the first has room is
// sent to the second and answered the same box, never a second one.
func TestAcquireFallThrough(t *testing.T) {
	ctx := context.Background()
	owner := boxDaemon(t, nil, // the same size: ranked by name, a first
		computeapi.RunTarget{Name: "a", Driver: "ref", Kind: "box", Isolation: "ephemeral", Images: []string{refPin}, HandlesMax: 1},
		computeapi.RunTarget{Name: "b", Driver: "refb", Kind: "box", Isolation: "ephemeral", Images: []string{refPin}})
	hold, err := computeclient.AcquireRunTarget(ctx, owner, "a", computeapi.AcquireRunTargetRequest{RequestID: "hold", TTLS: 120})
	if err != nil {
		t.Fatal(err)
	}
	req := computeapi.AcquireRunTargetRequest{RequestID: "k", TTLS: 120}
	target, h, err := computeclient.AcquireBox(ctx, owner, computeclient.BoxWant{}, req, nil)
	if err != nil || target != "b" {
		t.Fatalf("a busy first choice: %q %+v %v, want b", target, h, err)
	}
	t.Cleanup(func() { _, _ = computeclient.ReleaseRunTarget(ctx, owner, "b", idOf(h.Handle)) })
	if _, err := computeclient.ReleaseRunTarget(ctx, owner, "a", idOf(hold.Handle)); err != nil {
		t.Fatal(err)
	}
	// a has room now: the retry is still b's.
	target, again, err := computeclient.AcquireBox(ctx, owner, computeclient.BoxWant{}, req, nil)
	if err != nil || target != "b" || again.Handle != h.Handle {
		t.Errorf("the retry: %q %+v %v, want b's %s", target, again, err, h.Handle)
	}
	// Asked of a directly, the daemon names b and makes nothing.
	_, err = computeclient.AcquireRunTarget(ctx, owner, "a", req)
	if on, ok := computeapi.RequestUsedOn(err); !ok || on != "b" {
		t.Errorf("k on a: %v, want refused naming b", err)
	}
	if hs, err := computeclient.RunTargetHandles(ctx, owner, "a", true); err != nil || len(hs) != 0 {
		t.Errorf("a's live handles: %+v %v, want none", hs, err)
	}
	// b still bound, its box ended: the id stays b's (it answers the ended box).
	if _, err := computeclient.ReleaseRunTarget(ctx, owner, "b", idOf(h.Handle)); err != nil {
		t.Fatal(err)
	}
	if _, err := computeclient.AcquireRunTarget(ctx, owner, "a", req); !api.IsCode(err, api.CodeConflict) {
		t.Errorf("k on a, b's box ended: %v, want conflict", err)
	}
	// b removed: its ended box answers nothing, so the id is free on a.
	if err := computeclient.UnbindRunTarget(ctx, owner, "b"); err != nil {
		t.Fatal(err)
	}
	na, err := computeclient.AcquireRunTarget(ctx, owner, "a", req)
	if err != nil {
		t.Fatalf("k on a, b removed: %v", err)
	}
	_, _ = computeclient.ReleaseRunTarget(ctx, owner, "a", idOf(na.Handle))
}

// boxDaemon starts a daemon with the owner's project acme and binds
// targets, each driver (by its Driver name) a reference driver of its own,
// which keeps its own request ids. wrap, when set, answers the program the
// daemon runs for a driver, given the reference driver's path (a script
// standing in front of it).
func boxDaemon(t *testing.T, wrap func(driver, ref string) string, targets ...computeapi.RunTarget) *client.Client {
	t.Helper()
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
	drivers := map[string]string{}
	for _, b := range targets {
		dir, state := filepath.Join(fix, b.Driver), filepath.Join(fix, b.Driver, "state")
		p, err := refdriver.Install(dir, state, "")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { refdriver.KillAll(state) })
		if wrap != nil {
			p = wrap(b.Driver, p)
		}
		drivers[b.Driver] = p
	}
	for _, m := range dm.Modules() {
		if s, ok := m.(*runtarget.Service); ok {
			s.SetLookPath(func(name string) (string, error) { return drivers[name], nil })
		}
	}
	ctx := context.Background()
	owner := client.WithHandler(dm.SocketHandler())
	owner.Project, owner.ProjectFrom = "acme", "flag"
	if _, err := owner.CreateProject(ctx, api.NewProjectRequest{Slug: "acme"}); err != nil {
		t.Fatal(err)
	}
	for _, b := range targets {
		if _, err := computeclient.BindRunTarget(ctx, owner, computeapi.BindRunTargetRequest{Target: b}); err != nil {
			t.Fatal(err)
		}
	}
	return owner
}
