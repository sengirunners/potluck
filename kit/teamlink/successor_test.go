package teamlink_test

import (
	"bytes"
	"context"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/hometerm"
	"github.com/sengirunners/potluck/kit/project"
	"github.com/sengirunners/potluck/kit/server"
	"github.com/sengirunners/potluck/kit/store"
	"github.com/sengirunners/potluck/kit/tailnet"
	"github.com/sengirunners/potluck/kit/tailnet/tailnettest"
	"github.com/sengirunners/potluck/kit/teamlink"
)

// A member follows its project's home to another machine only by
// introduction: the home names the successor over the channel already
// pinned to its key, and the successor itself serves the project at a higher
// epoch. A home serving the project's data that was never named, and a named
// successor whose key is not the one named, are not followed; a manual
// follow needs the bound key. Following rebinds the project key and keeps the
// token file and the host state; the member's token verifies at the
// successor (its registry is a copy restored on another node); a restart
// loads the new address.
func TestMemberFollowsTheSuccessorItsHomeNamed(t *testing.T) {
	teamlink.BeatsOff.Store(true) // beats by hand only
	t.Cleanup(func() { teamlink.BeatsOff.Store(false) })
	silent := teamlink.HomeSilentAfter
	teamlink.HomeSilentAfter = 0 // a stopped home is followed at its first silent beat
	t.Cleanup(func() { teamlink.HomeSilentAfter = silent })
	ctx := context.Background()
	tn := tailnettest.New()
	miniID := tailnettest.Identity{NodeID: "nMINI", NodeKey: "nodekey:mini", Login: "judy@example.com", Host: "mini"}
	mini2ID := tailnettest.Identity{NodeID: "nMINI2", NodeKey: "nodekey:mini2", Login: "judy@example.com", Host: "mini2"}
	laptopID := tailnettest.Identity{NodeID: "nLAPTOP", NodeKey: "nodekey:laptop", Login: "ana@example.com", Host: "laptop"}
	homeA, homeC, homeM := tempHome(t), tempHome(t), tempHome(t)

	var dropped atomic.Bool
	repointed := make(chan api.Remote, 4)
	member := func(link *teamlink.Service) {
		link.Hooks.DropForgotten = append(link.Hooks.DropForgotten, func(context.Context, string) ([]string, error) {
			dropped.Store(true)
			return nil, nil
		})
		link.Hooks.Repointed = func(_ *teamlink.Remote, old api.Remote) { repointed <- old }
	}
	h := startHome(t, tn, homeA, "mini", "mini-human", miniID, nil)
	m := startHome(t, tn, homeM, "laptop", "laptop-human", laptopID, member)

	p, err := h.c.CreateProject(ctx, api.NewProjectRequest{Slug: "orchid"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.c.ProjectTeam(ctx, "orchid"); err != nil {
		t.Fatal(err)
	}
	inv, err := h.c.Invite(ctx, api.InviteRequest{Project: "orchid", As: "ana", Role: api.RoleMember})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.c.Join(ctx, api.JoinRequest{Code: inv.Code}); err != nil {
		t.Fatal(err)
	}
	x := m.link.ByName(p.ID)
	first := x.Snapshot()
	token, err := os.ReadFile(first.TokenFile)
	if err != nil {
		t.Fatal(err)
	}
	m.link.Heartbeat(ctx)
	if hv := x.Home(); hv.Epoch != 0 || hv.Heard.IsZero() || hv.Successor != nil {
		t.Fatalf("what the member knows of its home after a beat: %+v", hv)
	}
	unmoved := func(when string) {
		t.Helper()
		if r := x.Snapshot(); r.URL != first.URL || r.Fingerprint != first.Fingerprint {
			t.Fatalf("%s: the member repointed: %s %s", when, r.URL, r.Fingerprint)
		}
		if k, err := m.c.ProjectKey(ctx, p.ID); err != nil || k.Fingerprint != first.Fingerprint {
			t.Fatalf("%s: project key %+v %v", when, k, err)
		}
	}
	// The successor is the same home restored on mini2: its registry names
	// it mini, so its node takes the name the old home's had, at its own key.
	// The machine name the home designates it by is for display; the member
	// records the one the successor answers with (mini2).
	cand := teamlink.Successor{Machine: "studio", Peer: hometerm.Peer{Via: api.ViaTailnet, URL: first.URL,
		Fingerprint: project.NodeFingerprint(mini2ID.NodeID)}}

	// The home stops; mini2 restores its data and serves it at epoch 1.
	h.stop()
	restoreHome(t, homeA, homeC)
	pred := hometerm.Peer{Via: api.ViaTailnet, URL: first.URL, Fingerprint: first.Fingerprint}
	standby, err := hometerm.NewStandby(pred, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := hometerm.Create(homeC, standby); err != nil {
		t.Fatal(err)
	}
	if _, err := hometerm.Apply(homeC, func(t hometerm.Term) (hometerm.Term, error) { return t.Promote(1, 0) }); err != nil {
		t.Fatal(err)
	}
	c := startHome(t, tn, homeC, "mini2", "mini-human", mini2ID, nil)

	// Never named: the member stays, however silent its home.
	m.link.Heartbeat(ctx)
	unmoved("a successor its home never named")
	// The owner's follow names the bound key.
	if _, err := m.link.Follow(ctx, p.ID, cand, "", teamlink.FollowOptions{}); !api.IsCode(err, api.CodeBadRequest) {
		t.Fatalf("a follow that names no key: %v", err)
	}
	if _, err := m.link.Follow(ctx, p.ID, cand, "sha256:other", teamlink.FollowOptions{}); !api.IsCode(err, api.CodeConflict) {
		t.Fatalf("a follow at another key than the bound one: %v", err)
	}
	unmoved("a refused follow")

	// name has the old home (back for a while) name succ; then the old home
	// stops and mini2 serves again.
	name := func(succ teamlink.Successor) {
		t.Helper()
		c.stop()
		h = startHome(t, tn, homeA, "mini", "mini-human", miniID, nil)
		if _, err := h.link.Designate(ctx, succ); err != nil {
			t.Fatal(err)
		}
		m.link.Heartbeat(ctx)
		if hv := x.Home(); hv.Successor == nil || *hv.Successor != succ || hv.NamedAt.IsZero() {
			t.Fatalf("the named successor the member recorded: %+v", hv)
		}
		h.stop()
		c = startHome(t, tn, homeC, "mini2", "mini-human", mini2ID, nil)
	}

	// Named, but at a key that is not the one answering at that address.
	wrong := cand
	wrong.Fingerprint = project.NodeFingerprint("nEVIL")
	name(wrong)
	m.link.Heartbeat(ctx)
	unmoved("a successor whose key does not match the one named")

	// Named rightly: the member follows.
	name(cand)
	m.link.Heartbeat(ctx)
	r := x.Snapshot()
	if r.URL != cand.URL || r.Fingerprint != cand.Fingerprint || r.Authority != "mini2" || r.State != api.RemoteOK {
		t.Fatalf("the remote after the follow: %+v", r)
	}
	if hv := x.Home(); hv.Epoch != 1 || hv.Successor != nil {
		t.Fatalf("what the member knows of its new home: %+v", hv)
	}
	if k, err := m.c.ProjectKey(ctx, p.ID); err != nil || k.Fingerprint != cand.Fingerprint || k.Authority != "mini2" {
		t.Fatalf("the project key after the follow: %+v %v", k, err)
	}
	select {
	case old := <-repointed:
		if old.URL != first.URL || old.Fingerprint != first.Fingerprint {
			t.Fatalf("Repointed's old remote: %+v", old)
		}
	default:
		t.Fatal("Hooks.Repointed did not run")
	}
	if now, err := os.ReadFile(first.TokenFile); err != nil || !bytes.Equal(now, token) {
		t.Fatalf("the token file after the follow: %q %v", now, err)
	}
	if dropped.Load() {
		t.Fatal("the follow dropped the host state")
	}
	// The member's token verifies at mini2.
	var got api.Project
	if err := m.link.CallRemote(ctx, x, http.MethodGet, "/v0/projects/"+p.ID, nil, &got); err != nil || got.ID != p.ID {
		t.Fatalf("a call to the new home: %+v %v", got, err)
	}
	// Forget's rules hold at the new key.
	keep := api.ForgetProjectKeyRequest{HostState: api.ForgetKeepHostState, Fingerprint: cand.Fingerprint}
	if _, err := m.c.ForgetProjectKey(ctx, p.ID, keep); !api.IsCode(err, api.CodeConflict) {
		t.Fatalf("forget while joined, after a follow: %v", err)
	}

	// A restart loads the new home.
	m.stop()
	m = startHome(t, tn, homeM, "laptop", "laptop-human", laptopID, member)
	rs, err := m.c.Remotes(ctx)
	if err != nil || len(rs) != 1 || rs[0].URL != cand.URL || rs[0].Fingerprint != cand.Fingerprint {
		t.Fatalf("remotes after a restart: %+v %v", rs, err)
	}
	if hv := m.link.ByName(p.ID).Home(); hv.Epoch != 1 {
		t.Fatalf("the epoch after a restart: %+v", hv)
	}
	got = api.Project{}
	if err := m.link.CallRemote(ctx, m.link.ByName(p.ID), http.MethodGet, "/v0/projects/"+p.ID, nil, &got); err != nil || got.ID != p.ID {
		t.Fatalf("a call to the new home after a restart: %+v %v", got, err)
	}
}

func tempHome(t *testing.T) string {
	t.Helper()
	home, err := os.MkdirTemp("", "zm") // short: the socket lives in it
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	return home
}

// restoreHome copies the data of a stopped home to another home, as a
// restored backup set has it: the databases and the projects' files, not the
// machine's own state (config, node, term).
func restoreHome(t *testing.T, from, to string) {
	t.Helper()
	err := filepath.WalkDir(from, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(from, path)
		if d.IsDir() {
			if rel == "." || rel == "projects" || strings.HasPrefix(rel, "projects"+string(filepath.Separator)) {
				return os.MkdirAll(filepath.Join(to, rel), 0o700)
			}
			return filepath.SkipDir
		}
		if !strings.HasPrefix(rel, store.FileRegistry) && !strings.HasPrefix(rel, "projects"+string(filepath.Separator)) {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(to, rel), b, 0o600)
	})
	if err != nil {
		t.Fatal(err)
	}
}

type homeDaemon struct {
	c    *client.Client
	link *teamlink.Service
	stop func()
}

// startHome serves a daemon wired with tailnet and teamlink alone on home,
// its node on tn as id and up; setup runs on its teamlink before it starts.
// stop stops it (and is the test's cleanup too).
func startHome(t *testing.T, tn *tailnettest.Net, home, machine, human string, id tailnettest.Identity, setup func(*teamlink.Service)) homeDaemon {
	t.Helper()
	cfg, err := config.LoadFrom(home)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Human, cfg.Machine = human, machine
	var link *teamlink.Service
	wire := func(d *core.Deps) ([]core.Module, error) {
		net := tailnet.New(d)
		d.Net = net.Net()
		d.AddTransport(tailnet.TeamTransport(d, d.Net))
		link = teamlink.New(d)
		if setup != nil {
			setup(link)
		}
		return []core.Module{net, link}, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	dm, err := server.New(ctx, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), wire)
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
	stopped := false
	stop := func() {
		if stopped {
			return
		}
		stopped = true
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Errorf("daemon %s did not stop", machine)
		}
	}
	t.Cleanup(stop)
	c := client.WithHandler(dm.SocketHandler())
	if err := client.NewUnix(cfg.SocketPath()).WaitReady(ctx, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if st, err := c.TailnetUp(ctx, api.TailnetUpRequest{}); err != nil || !st.Running {
		t.Fatalf("%s: tailnet up: %+v %v", machine, st, err)
	}
	return homeDaemon{c: c, link: link, stop: stop}
}
