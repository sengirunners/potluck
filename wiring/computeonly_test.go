package wiring_test

// Stage 1a's end-to-end proof (zm-eakh.12.35 G8, zm-eakh.12.35.10): two
// daemons wired with KIT and compute only (wiring.Compute: no team, work,
// mail or sessions) lend and use a box across machines. The authority makes
// a team project and invites; the host joins, lends a box binding (the
// reference driver) to the project and heartbeats; a member at the
// authority lists the host's box, acquires it, runs commands in it, reads a
// background process's output and releases it, every driver verb relayed to
// the host over teamlink's op queue. internal/deps
// TestComputeWiringLinksNoOrch checks this test binary links no ORCH package.

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/computeclient"
	"github.com/sengirunners/potluck/conformance/refdriver"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/server"
	"github.com/sengirunners/potluck/kit/tailnet"
	"github.com/sengirunners/potluck/kit/tailnet/tailnettest"
	"github.com/sengirunners/potluck/kit/teamlink"
	"github.com/sengirunners/potluck/runtarget"
	"github.com/sengirunners/potluck/wiring"
)

func TestMain(m *testing.M) {
	if code, ok := refdriver.Main(); ok { // the reference driver runs as this binary
		os.Exit(code)
	}
	os.Exit(m.Run())
}

const refPin = "ref-image@sha256:0000000000000000000000000000000000000000000000000000000000000001"

// computeDaemon is a daemon wired with wiring.Compute in a temp home, its
// node on tn as id and up: its owner's socket client and its modules.
type computeDaemon struct {
	owner *client.Client
	dm    *server.Daemon
	home  string
}

func startCompute(t *testing.T, tn *tailnettest.Net, machine string, id tailnettest.Identity) *computeDaemon {
	t.Helper()
	return startComputeWith(t, tn, machine, id, io.Discard, nil)
}

// startComputeWith is startCompute logging at debug to logs, with tun as
// its tunnel config when set.
func startComputeWith(t *testing.T, tn *tailnettest.Net, machine string, id tailnettest.Identity, logs io.Writer, tun *config.Tunnel) *computeDaemon {
	t.Helper()
	home, err := os.MkdirTemp("", "zm")
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadFrom(home)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Human, cfg.Machine = machine+"-human", machine
	cfg.File.Admission = config.Admission{MinMemoryFreePercent: 1, MaxSwapUsedMB: 1 << 40, MinDiskFreeMB: 1}
	if tun != nil {
		cfg.File.Tunnel = *tun
	}
	ctx, cancel := context.WithCancel(context.Background())
	dm, err := server.New(ctx, cfg, slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})), wiring.Compute)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	for _, m := range dm.Modules() {
		if s, ok := m.(*tailnet.Service); ok {
			s.NewNode = tn.NodeFunc(id)
		}
	}
	done := make(chan error, 1)
	go func() { done <- dm.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Errorf("daemon %s did not stop", machine)
		}
		os.RemoveAll(home)
	})
	c := client.WithHandler(dm.SocketHandler())
	if err := client.NewUnix(cfg.SocketPath()).WaitReady(ctx, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if st, err := c.TailnetUp(ctx, api.TailnetUpRequest{}); err != nil || !st.Running {
		t.Fatalf("%s: tailnet up: %+v %v", machine, st, err)
	}
	return &computeDaemon{owner: c, dm: dm, home: home}
}

// module is d's module of type T.
func module[T core.Module](t *testing.T, d *computeDaemon) T {
	t.Helper()
	for _, m := range d.dm.Modules() {
		if x, ok := m.(T); ok {
			return x
		}
	}
	var zero T
	t.Fatalf("no %T module", zero)
	return zero
}

func TestComputeOnlyBoxesAcrossMachines(t *testing.T) {
	old := teamlink.BeatEvery
	teamlink.BeatEvery = time.Hour // the test beats by hand
	t.Cleanup(func() { teamlink.BeatEvery = old })
	ctx := context.Background()
	tn := tailnettest.New()
	a := startCompute(t, tn, "mini", tailnettest.Identity{NodeID: "nMINI", NodeKey: "nodekey:mini", Login: "judy@example.com", Host: "mini"})
	b := startCompute(t, tn, "box-host", tailnettest.Identity{NodeID: "nBOX", NodeKey: "nodekey:box-host", Login: "carl@example.com", Host: "box-host"})

	// A: a team project and an invite; B joins it.
	p, err := a.owner.CreateProject(ctx, api.NewProjectRequest{Slug: "orchid"})
	if err != nil {
		t.Fatal(err)
	}
	if mode, err := a.owner.ProjectTeam(ctx, "orchid"); err != nil || !mode.Team {
		t.Fatalf("team: %+v %v", mode, err)
	}
	inv, err := a.owner.Invite(ctx, api.InviteRequest{Project: "orchid", As: "carl", Role: api.RoleMember})
	if err != nil {
		t.Fatal(err)
	}
	j, err := b.owner.Join(ctx, api.JoinRequest{Code: inv.Code})
	if err != nil || j.Remote == nil || j.Remote.Project.ID != p.ID {
		t.Fatalf("join: %+v %v", j, err)
	}

	// B lends a box binding (the reference driver) to orchid.
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

	// A member at A sees B's box once B's heartbeat carried its advert.
	tok, err := a.owner.MintToken(ctx, api.MintTokenRequest{As: "ana", Project: "orchid", Role: "member"})
	if err != nil {
		t.Fatal(err)
	}
	ana := client.WithHandler(a.dm.Handler)
	ana.Token, ana.Project, ana.ProjectFrom = tok.Secret, "orchid", "flag"
	link := module[*teamlink.Service](t, b)
	var view computeapi.RunTargetView
	deadline := time.Now().Add(20 * time.Second)
	for view.Name == "" {
		if time.Now().After(deadline) {
			t.Fatal("B's box was never listed at A")
		}
		link.Heartbeat(ctx)
		l, err := computeclient.RunTargets(ctx, ana, true)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		for _, v := range l.Targets {
			if v.Name == "sandbox@box-host" {
				view = v
			}
		}
		if view.Name == "" {
			time.Sleep(100 * time.Millisecond)
		}
	}
	if view.Host != "box-host" || view.HostClass != api.MachineTrusted || view.Describe == nil || !view.Describe.Available {
		t.Fatalf("remote view: %+v", view)
	}
	stop := beat(link)
	defer stop()

	// Acquire, exec, a background process, status, release: each a driver
	// verb relayed to B.
	h, err := computeclient.AcquireRunTarget(ctx, ana, "sandbox@box-host", computeapi.AcquireRunTargetRequest{RequestID: "proof-1", TTLS: 120})
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if !strings.HasPrefix(h.Handle, "box:sandbox@box-host/") || h.State != "ready" {
		t.Fatalf("handle: %+v", h)
	}
	id := h.Handle[strings.LastIndex(h.Handle, "/")+1:]
	out, err := computeclient.ExecRunTarget(ctx, ana, "sandbox@box-host", id, computeapi.ExecRunTargetRequest{Argv: []string{"sh", "-c", "echo hi from $(hostname -s 2>/dev/null || echo box) > note && cat note && echo err >&2"}})
	if err != nil || out.Exit != 0 || !strings.HasPrefix(out.Stdout, "hi from ") || out.Stderr != "err\n" {
		t.Fatalf("exec: %+v %v", out, err)
	}
	again, err := computeclient.ExecRunTarget(ctx, ana, "sandbox@box-host", id, computeapi.ExecRunTargetRequest{Argv: []string{"cat", "note"}})
	if err != nil || again.Stdout != out.Stdout {
		t.Fatalf("the box kept no state between execs: %+v %v", again, err)
	}
	if fail, err := computeclient.ExecRunTarget(ctx, ana, "sandbox@box-host", id, computeapi.ExecRunTargetRequest{Argv: []string{"sh", "-c", "exit 7"}}); err != nil || fail.Exit != 7 {
		t.Fatalf("exit code: %+v %v", fail, err)
	}
	sp, err := computeclient.SpawnRunTarget(ctx, ana, "sandbox@box-host", id, computeapi.SpawnRunTargetRequest{RequestID: "proof-s1", Argv: []string{"sh", "-c", "echo one; echo two"}})
	if err != nil || sp.PID == "" {
		t.Fatalf("spawn: %+v %v", sp, err)
	}
	var o computeapi.RunTargetOutput
	for range 20 {
		if o, err = computeclient.RunTargetOutput(ctx, ana, "sandbox@box-host", id, computeapi.OutputRunTargetRequest{PID: sp.PID, WaitS: 2}); err != nil || !o.Running {
			break
		}
	}
	if err != nil || o.Running || o.Exit == nil || *o.Exit != 0 || o.Stdout != "one\ntwo\n" {
		t.Fatalf("background output: %+v %v", o, err)
	}
	if hs, err := computeclient.RunTargetHandles(ctx, ana, "sandbox@box-host", true); err != nil || len(hs) != 1 || hs[0].Handle != h.Handle {
		t.Fatalf("live handles: %+v %v", hs, err)
	}
	if rel, err := computeclient.ReleaseRunTarget(ctx, ana, "sandbox@box-host", id); err != nil || !rel.Released {
		t.Fatalf("release: %+v %v", rel, err)
	}
	if st, err := computeclient.RunTargetStatus(ctx, ana, "sandbox@box-host", id); err != nil || st.State != "released" {
		t.Fatalf("status after release: %+v %v", st, err)
	}
	// B's own list agrees: the box it lent is gone.
	if hs, err := computeclient.RunTargetHandles(ctx, b.owner, "sandbox", true); err == nil && len(hs) != 0 {
		t.Errorf("B still holds a live box: %+v", hs)
	}

	// Usage from the member machine (zm-eakh.12.49): B's owner asks its own
	// daemon for orchid's usage; B holds no orchid project, so the call must
	// go to A and answer as A answers carl (a member reads the project).
	carl := client.WithHandler(b.dm.SocketHandler())
	carl.Project, carl.ProjectFrom = "orchid", "flag"
	var u computeapi.UsageList
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(100 * time.Millisecond) {
		if u, err = computeclient.Usage(ctx, carl, ""); err != nil {
			t.Fatalf("usage from the member machine: %v", err)
		}
		if len(u.Boxes) > 0 || time.Now().After(deadline) {
			break
		}
	}
	if u.Project != "orchid" || len(u.Boxes) != 1 || u.Boxes[0].Handle != h.Handle || u.Boxes[0].EndState != "released" {
		t.Fatalf("usage from the member machine: %+v", u)
	}
	if at, err := computeclient.Usage(ctx, ana, ""); err != nil || !reflect.DeepEqual(at.Boxes, u.Boxes) {
		t.Fatalf("usage at the authority %+v %v differs from the member's %+v", at, err, u)
	}

	// zm-eakh.12.5.10.8: carl lowered to guest, with no rejoin: B is a
	// guest's machine at once. Its box is no longer listed, and it is given
	// no box, command or renewal; its live box can still be looked at and
	// ended.
	h2, err := computeclient.AcquireRunTarget(ctx, ana, "sandbox@box-host", computeapi.AcquireRunTargetRequest{RequestID: "proof-2", TTLS: 120})
	if err != nil {
		t.Fatalf("acquire before the demotion: %v", err)
	}
	id2 := h2.Handle[strings.LastIndex(h2.Handle, "/")+1:]
	if _, err := a.owner.Grant(ctx, api.GrantRequest{Identity: "carl", Project: "orchid", Role: api.RoleGuest}); err != nil {
		t.Fatal(err)
	}
	if l, err := computeclient.RunTargets(ctx, ana, true); err != nil || slices.ContainsFunc(l.Targets, func(v computeapi.RunTargetView) bool { return v.Host == "box-host" }) {
		t.Errorf("list after carl is lowered to guest: %+v %v; want no box on box-host", l, err)
	}
	if _, err := computeclient.AcquireRunTarget(ctx, ana, "sandbox@box-host", computeapi.AcquireRunTargetRequest{RequestID: "proof-3", TTLS: 120}); err == nil {
		t.Error("an acquire on a demoted member's machine")
	}
	if _, err := computeclient.ExecRunTarget(ctx, ana, "sandbox@box-host", id2, computeapi.ExecRunTargetRequest{Argv: []string{"true"}}); err == nil {
		t.Error("an exec on a demoted member's machine")
	}
	if _, err := computeclient.RenewRunTarget(ctx, ana, "sandbox@box-host", id2, 120); err == nil {
		t.Error("a renewal on a demoted member's machine")
	}
	if rel, err := computeclient.ReleaseRunTarget(ctx, ana, "sandbox@box-host", id2); err != nil || !rel.Released {
		t.Errorf("release on a demoted member's machine: %+v %v", rel, err)
	}
}

// beat heartbeats link every 200 ms until the returned func is called.
func beat(link *teamlink.Service) func() {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		for ctx.Err() == nil {
			link.Heartbeat(ctx)
			select {
			case <-ctx.Done():
			case <-time.After(200 * time.Millisecond):
			}
		}
	}()
	return func() { cancel(); <-done }
}
