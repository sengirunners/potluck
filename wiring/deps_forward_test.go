package wiring_test

// Dependency layers across machines (zm-eakh.12.65.15): a member's acquire
// with deps goes member -> pool's machine -> host, over the tailnet and the
// tunnel alike. The member's proxy forwards a body over 1 MiB only when it
// carries deps, and never one over 16 MiB; the pool's machine keys the
// layer by the image digest the host advertises, builds it on the host,
// and the next acquire is a hit.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/computeclient"
	"github.com/sengirunners/potluck/conformance/refdriver"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/tailnet/tailnettest"
	"github.com/sengirunners/potluck/kit/teamlink"
	"github.com/sengirunners/potluck/kit/tunnel/tunneltest"
	"github.com/sengirunners/potluck/runtarget"
)

func TestDepsForwardedAcrossMachines(t *testing.T) {
	for _, via := range []string{api.ViaTailnet, api.ViaTunnel} {
		t.Run(via, func(t *testing.T) { depsForwarded(t, via) })
	}
}

func depsForwarded(t *testing.T, via string) {
	old := teamlink.BeatEvery
	teamlink.BeatEvery = time.Hour // the test beats by hand
	t.Cleanup(func() { teamlink.BeatEvery = old })
	// The reference driver's boxes run sh here: an npm on PATH stands in
	// for the real one, so the recipe builds node_modules from the lockfile.
	bin := t.TempDir()
	npm := "#!/bin/sh\ncase $1 in ci) mkdir -p node_modules && cp package-lock.json node_modules/.lock ;; esac\n"
	if err := os.WriteFile(filepath.Join(bin, "npm"), []byte(npm), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	ctx := context.Background()
	tn := tailnettest.New()
	var tun *config.Tunnel
	if via == api.ViaTunnel {
		listen := freeLoopback(t)
		p, err := tunneltest.New(listen)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { p.Close() })
		tun = &config.Tunnel{Listen: listen, URL: p.URL()}
	}
	logs := &syncBuf{}
	a := startComputeWith(t, tn, "mini", tailnettest.Identity{NodeID: "nMINI", NodeKey: "nodekey:mini", Login: "judy@example.com", Host: "mini"}, logs, tun)
	b := startComputeWith(t, tn, "box-host", tailnettest.Identity{NodeID: "nBOX", NodeKey: "nodekey:box-host", Login: "carl@example.com", Host: "box-host"}, logs, nil)
	m := startComputeWith(t, tn, "member-box", tailnettest.Identity{NodeID: "nMEM", NodeKey: "nodekey:member-box", Login: "dave@example.com", Host: "member-box"}, logs, nil)
	if _, err := a.owner.CreateProject(ctx, api.NewProjectRequest{Slug: "orchid"}); err != nil {
		t.Fatal(err)
	}
	if mode, err := a.owner.ProjectTeam(ctx, "orchid"); err != nil || !mode.Team {
		t.Fatalf("team: %+v %v", mode, err)
	}
	for _, j := range []struct {
		as string
		d  *computeDaemon
	}{{"carl", b}, {"dave", m}} {
		inv, err := a.owner.Invite(ctx, api.InviteRequest{Project: "orchid", As: j.as, Role: api.RoleMember, Via: via})
		if err != nil {
			t.Fatal(err)
		}
		if r, err := j.d.owner.Join(ctx, api.JoinRequest{Code: inv.Code}); err != nil || r.Remote == nil || r.Remote.Via != via {
			t.Fatalf("%s joins over %s: %+v %v", j.as, via, r, err)
		}
	}
	fix, _ := filepath.EvalSymlinks(t.TempDir())
	state := filepath.Join(fix, "state")
	drv, err := refdriver.Install(fix, state, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { refdriver.KillAll(state) })
	module[*runtarget.Service](t, b).SetLookPath(func(string) (string, error) { return drv, nil })
	if _, err := computeclient.BindRunTarget(ctx, b.owner, computeapi.BindRunTargetRequest{Host: true, Share: []string{"orchid"}, Target: computeapi.RunTarget{
		Name: "sandbox", Driver: "ref", Kind: "box", Isolation: "ephemeral", Images: []string{refPin}}}); err != nil {
		t.Fatalf("bind: %v", err)
	}
	link := module[*teamlink.Service](t, b)
	dave := client.WithHandler(m.dm.SocketHandler())
	dave.Project, dave.ProjectFrom = "orchid", "flag"
	for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(100 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("B's box was never listed at A with its pin")
		}
		link.Heartbeat(ctx)
		l, err := computeclient.RunTargets(ctx, dave, true)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(l.Targets) > 0 && l.Targets[0].Name == "sandbox@box-host" && l.Targets[0].HostClass == api.MachineTrusted &&
			l.Targets[0].Describe != nil && len(l.Targets[0].Describe.Images) == 1 {
			break
		}
	}
	stop := beat(link)
	defer stop()

	// Over 1 MiB of files (about 2.7 MiB once base64): forwarded, as deps.
	lock := `{"name":"app","lockfileVersion":3,"packages":{"":{"name":"app"}}}`
	pkg := `{"name":"app","description":"` + strings.Repeat("a", 2<<20) + `"}`
	deps := []computeapi.BoxDeps{{Kind: "npm", Path: "work", Files: []computeapi.DepsFile{{Path: "package-lock.json", Data: []byte(lock)}, {Path: "package.json", Data: []byte(pkg)}}}}
	h, err := computeclient.AcquireRunTarget(ctx, dave, "sandbox@box-host", computeapi.AcquireRunTargetRequest{RequestID: "d1", TTLS: 120, Deps: deps})
	if err != nil || len(h.Deps) != 1 || !h.Deps[0].Built {
		t.Fatalf("a member's acquire with deps over 1 MiB: %+v %v", h.Deps, err)
	}
	h2, err := computeclient.AcquireRunTarget(ctx, dave, "sandbox@box-host", computeapi.AcquireRunTargetRequest{RequestID: "d2", TTLS: 120, Deps: deps})
	if err != nil || len(h2.Deps) != 1 || !h2.Deps[0].Hit || h2.Deps[0].Key != h.Deps[0].Key {
		t.Fatalf("the next acquire: %+v %v, want a hit", h2.Deps, err)
	}
	id := h2.Handle[strings.LastIndex(h2.Handle, "/")+1:]
	if x, err := computeclient.ExecRunTarget(ctx, dave, "sandbox@box-host", id, computeapi.ExecRunTargetRequest{Argv: []string{"cat", "work/node_modules/.lock"}}); err != nil || x.Stdout != lock {
		t.Fatalf("the box's layer: %+v %v", x, err)
	}

	// Over 1 MiB without deps, and over 16 MiB with them: refused by the
	// member's machine, never forwarded.
	if _, err := computeclient.AcquireRunTarget(ctx, dave, "sandbox@box-host", computeapi.AcquireRunTargetRequest{RequestID: strings.Repeat("x", 2<<20)}); err == nil || !strings.Contains(err.Error(), "body over 1 MiB") {
		t.Errorf("an acquire over 1 MiB with no deps: %v, want refused", err)
	}
	huge := []computeapi.BoxDeps{{Kind: "npm", Path: "work", Files: []computeapi.DepsFile{{Path: "package-lock.json", Data: []byte(lock)}, {Path: "package.json", Data: []byte(strings.Repeat(" ", 13<<20) + "{}")}}}}
	if _, err := computeclient.AcquireRunTarget(ctx, dave, "sandbox@box-host", computeapi.AcquireRunTargetRequest{RequestID: "d3", Deps: huge}); err == nil || !strings.Contains(err.Error(), "body over 16 MiB") {
		t.Errorf("an acquire with deps over 16 MiB: %v, want refused", err)
	}
}
