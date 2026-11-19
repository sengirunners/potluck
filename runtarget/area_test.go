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

// TestHandleAreas: a handle records the area it was acquired in; another
// area's member sees it as missing, in lists and by id; the owner sees all.
func TestHandleAreas(t *testing.T) {
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
	repo, err := refdriver.NewRepo(fix, "#!/bin/sh\nexit 0\n")
	if err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(fix, "state")
	drv, err := refdriver.Install(fix, state, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { refdriver.KillAll(state) })
	for _, m := range dm.Modules() {
		if s, ok := m.(*runtarget.Service); ok {
			s.SetLookPath(func(string) (string, error) { return drv, nil })
		}
	}
	ctx := context.Background()
	owner := client.WithHandler(dm.SocketHandler())
	owner.Project, owner.ProjectFrom = "acme", "flag"
	if _, err := owner.CreateProject(ctx, api.NewProjectRequest{Slug: "acme", RepoRules: api.RepoRules{Remotes: []string{repo.Remote}}}); err != nil {
		t.Fatal(err)
	}
	for _, a := range []string{"team1", "team2"} {
		if _, err := owner.AddArea(ctx, "acme", api.Area{Name: a}); err != nil {
			t.Fatal(err)
		}
	}
	member := func(as, area string) *client.Client {
		tok, err := owner.MintToken(ctx, api.MintTokenRequest{As: as, Project: "acme", Area: area, Role: "member"})
		if err != nil {
			t.Fatal(err)
		}
		c := client.WithHandler(dm.Handler)
		c.Token, c.Project, c.ProjectFrom = tok.Secret, "acme/"+area, "flag"
		return c
	}
	ana, bo := member("ana", "team1"), member("bo", "team2")
	box := computeapi.RunTarget{Name: "sandbox", Driver: "ref", Kind: "box", Isolation: "ephemeral", Images: []string{refPin},
		Config: map[string]string{"remote": repo.Remote}}
	if _, err := computeclient.BindRunTarget(ctx, owner, computeapi.BindRunTargetRequest{Target: box}); err != nil {
		t.Fatal(err)
	}
	h, err := computeclient.AcquireRunTarget(ctx, ana, "sandbox", computeapi.AcquireRunTargetRequest{RequestID: "a1", TTLS: 120})
	if err != nil {
		t.Fatal(err)
	}
	if h.Area != "team1" {
		t.Fatalf("acquired in area %q, want team1", h.Area)
	}
	if hs, err := computeclient.RunTargetHandles(ctx, bo, "sandbox", false); err != nil || len(hs) != 0 {
		t.Errorf("another area's list: %+v %v", hs, err)
	}
	for name, call := range map[string]func() error{
		"status": func() error { _, err := computeclient.RunTargetStatus(ctx, bo, "sandbox", idOf(h.Handle)); return err },
		"exec": func() error {
			_, err := computeclient.ExecRunTarget(ctx, bo, "sandbox", idOf(h.Handle), computeapi.ExecRunTargetRequest{Argv: []string{"id"}})
			return err
		},
		"spawn": func() error {
			_, err := computeclient.SpawnRunTarget(ctx, bo, "sandbox", idOf(h.Handle), computeapi.SpawnRunTargetRequest{Argv: []string{"id"}, RequestID: "x"})
			return err
		},
		"output": func() error {
			_, err := computeclient.RunTargetOutput(ctx, bo, "sandbox", idOf(h.Handle), computeapi.OutputRunTargetRequest{PID: "p000000000000"})
			return err
		},
		"signal": func() error {
			_, err := computeclient.SignalRunTarget(ctx, bo, "sandbox", idOf(h.Handle), computeapi.SignalRunTargetRequest{PID: "p000000000000", Signal: "KILL"})
			return err
		},
		"release": func() error { _, err := computeclient.ReleaseRunTarget(ctx, bo, "sandbox", idOf(h.Handle)); return err },
	} {
		if err := call(); code(err) != api.CodeNotFound {
			t.Errorf("another area's %s: %v", name, err)
		}
	}
	if hs, err := computeclient.RunTargetHandles(ctx, ana, "sandbox", false); err != nil || len(hs) != 1 {
		t.Errorf("own area's list: %+v %v", hs, err)
	}
	if hs, err := computeclient.RunTargetHandles(ctx, owner, "sandbox", false); err != nil || len(hs) != 1 {
		t.Errorf("the owner's list: %+v %v", hs, err)
	}
	// Another member of the same area never sees ana's sandbox, with or without mine=1: only the machine owner
	// and a whole-project owner or admin list another member's.
	cy := member("cy", "team1")
	mine := computeclient.HandleFilter{Mine: true}
	if hs, err := computeclient.ListHandles(ctx, cy, "sandbox", mine); err != nil || len(hs) != 0 {
		t.Errorf("another member's own list: %+v %v", hs, err)
	}
	if hs, err := computeclient.RunTargetHandles(ctx, cy, "sandbox", false); err != nil || len(hs) != 0 {
		t.Errorf("another member's full list: %+v %v", hs, err)
	}
	if _, err := computeclient.RunTargetStatus(ctx, cy, "sandbox", idOf(h.Handle)); code(err) != api.CodeNotFound {
		t.Errorf("another member reads ana's sandbox by id: %v", err)
	}
	if hs, err := computeclient.ListHandles(ctx, ana, "sandbox", mine); err != nil || len(hs) != 1 || hs[0].Handle != h.Handle {
		t.Errorf("the holder's own list: %+v %v", hs, err)
	}
	if rel, err := computeclient.ReleaseRunTarget(ctx, ana, "sandbox", idOf(h.Handle)); err != nil || !rel.Released {
		t.Errorf("release by the holder: %+v %v", rel, err)
	}
}
