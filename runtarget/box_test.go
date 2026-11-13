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
	"github.com/sengirunners/potluck/imagepins"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/server"
	"github.com/sengirunners/potluck/runtarget"
	"github.com/sengirunners/potluck/wiring"
)

const refPin = "ref-image@sha256:0000000000000000000000000000000000000000000000000000000000000001"

// TestBoxes: a box binding only on an untrusted-capable class, images by
// pinned name, exec for the holder only, a booted digest checked against the
// pin, and the end hook called once with the final status.
func TestBoxes(t *testing.T) {
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
	var mu sync.Mutex
	var ended []computeapi.RunTargetHandle
	var svc *runtarget.Service
	for _, m := range dm.Modules() {
		if s, ok := m.(*runtarget.Service); ok {
			svc = s
			s.SetLookPath(func(string) (string, error) { return drv, nil })
			s.OnEnd(func(_ context.Context, _ string, h computeapi.RunTargetHandle) {
				mu.Lock()
				ended = append(ended, h)
				mu.Unlock()
			})
		}
	}
	ctx := context.Background()
	owner := client.WithHandler(dm.SocketHandler())
	owner.Project, owner.ProjectFrom = "acme", "flag"
	if _, err := owner.CreateProject(ctx, api.NewProjectRequest{Slug: "acme", RepoRules: api.RepoRules{Remotes: []string{repo.Remote}}}); err != nil {
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
	ana, bo := mint("ana"), mint("bo")

	box := computeapi.RunTarget{Name: "sandbox", Driver: "ref", Kind: "box", Isolation: "ephemeral", Images: []string{refPin},
		Repo: repo.Remote, Branch: "main", Config: map[string]string{"remote": repo.Remote}}
	for name, mut := range map[string]func(*computeapi.RunTarget){
		"unisolated":      func(b *computeapi.RunTarget) { b.Isolation = "none" },
		"credentialed":    func(b *computeapi.RunTarget) { b.Credentials = true },
		"scripts":         func(b *computeapi.RunTarget) { b.Scripts = []string{"x"} },
		"unpinned":        func(b *computeapi.RunTarget) { b.Images = []string{"ref-image:latest"} },
		"no images":       func(b *computeapi.RunTarget) { b.Images = nil },
		"repo, no branch": func(b *computeapi.RunTarget) { b.Branch = "" },
	} {
		b := box
		b.Config = map[string]string{"remote": repo.Remote}
		mut(&b)
		_, err := computeclient.BindRunTarget(ctx, owner, computeapi.BindRunTargetRequest{Target: b})
		switch {
		case err == nil:
			t.Errorf("a %s box binding: accepted", name)
		case name == "unisolated" && !strings.Contains(err.Error(), "a box target cannot isolate untrusted code (work on it is not walled off from this machine, its LAN and its tailnet)"),
			name == "credentialed" && !strings.Contains(err.Error(), "a box target cannot isolate untrusted code (work on it could read the credentials it holds)"):
			t.Errorf("a %s box binding: the refusal does not say why in plain words: %v", name, err)
		}
	}
	if _, err := computeclient.BindRunTarget(ctx, owner, computeapi.BindRunTargetRequest{Target: box}); err != nil {
		t.Fatal(err)
	}
	drift := box
	drift.Name, drift.Images = "drift", []string{"ref-image@sha256:" + strings.Repeat("f", 64)}
	if _, err := computeclient.BindRunTarget(ctx, owner, computeapi.BindRunTargetRequest{Target: drift}); err != nil {
		t.Fatal(err)
	}

	for name, req := range map[string]computeapi.AcquireRunTargetRequest{
		"unknown image": {RequestID: "x1", Image: "other"},
		"a script":      {RequestID: "x2", Script: "probe.sh"},
		"unpushed ref":  {RequestID: "x3", Ref: "nope"},
	} {
		if _, err := computeclient.AcquireRunTarget(ctx, ana, "sandbox", req); code(err) != api.CodeBadRequest {
			t.Errorf("box acquire with %s: %v", name, err)
		}
	}
	// A template (zm-eakh.12.5.20) picks the binding's image of the template's name, any digest; the
	// binding's first image stays the default. The reference driver boots ref-image only, so it refusing the
	// agent image by name shows the template's image is the one sent.
	agentPin := "ghcr.io/sengirunners/potluck-box-agent@sha256:" + strings.Repeat("a", 64)
	agentBox := box
	agentBox.Name, agentBox.Images = "agentbox", []string{refPin, agentPin}
	if _, err := computeclient.BindRunTarget(ctx, owner, computeapi.BindRunTargetRequest{Target: agentBox}); err != nil {
		t.Fatal(err)
	}
	browserBox := box
	browserBox.Name = "browserbox"
	for _, p := range imagepins.DefaultPins() {
		if p.Template == "browser" {
			browserBox.Images = []string{refPin, p.Image}
		}
	}
	if _, err := computeclient.BindRunTarget(ctx, owner, computeapi.BindRunTargetRequest{Target: browserBox}); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct {
		target string
		req    computeapi.AcquireRunTargetRequest
		want   string
	}{
		"unknown template":                     {"agentbox", computeapi.AcquireRunTargetRequest{RequestID: "t1", Template: "nope"}, `no box template "nope" (zm's: agent, browser)`},
		"template and image":                   {"agentbox", computeapi.AcquireRunTargetRequest{RequestID: "t2", Template: "agent", Image: "ref-image"}, "template or an image, not both"},
		"binding without it":                   {"sandbox", computeapi.AcquireRunTargetRequest{RequestID: "t3", Template: "agent"}, "run target sandbox lends no agent template image"},
		"the template's name":                  {"agentbox", computeapi.AcquireRunTargetRequest{RequestID: "t4", Template: "agent"}, "ghcr.io/sengirunners/potluck-box-agent"},
		"browser missing":                      {"agentbox", computeapi.AcquireRunTargetRequest{RequestID: "browser-missing", Template: "browser"}, "run target agentbox lends no browser template image"},
		"published browser reaches the driver": {"browserbox", computeapi.AcquireRunTargetRequest{RequestID: "browser", Template: "browser"}, "ghcr.io/sengirunners/potluck-box-browser"},
	} {
		if _, err := computeclient.AcquireRunTarget(ctx, ana, c.target, c.req); code(err) != api.CodeBadRequest || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want a 400 saying %q", name, err, c.want)
		}
	}
	th, err := computeclient.AcquireRunTarget(ctx, ana, "agentbox", computeapi.AcquireRunTargetRequest{RequestID: "t5", TTLS: 60})
	if err != nil || th.ImageDigest != refPin[strings.LastIndex(refPin, "@")+1:] {
		t.Fatalf("no template: the first image: %+v %v", th, err)
	}
	if _, err := computeclient.ReleaseRunTarget(ctx, ana, "agentbox", idOf(th.Handle)); err != nil {
		t.Errorf("release: %v", err)
	}
	h, err := computeclient.AcquireRunTarget(ctx, ana, "sandbox", computeapi.AcquireRunTargetRequest{RequestID: "b1", Ref: "main", TTLS: 120})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(h.Handle, "box:sandbox/") || h.State != "ready" || h.Size != "micro" || h.Commit != repo.Tip || h.Acquired == "" ||
		h.ImageDigest != refPin[strings.LastIndex(refPin, "@")+1:] {
		t.Fatalf("box acquire: %+v", h)
	}
	out, err := computeclient.ExecRunTarget(ctx, ana, "sandbox", idOf(h.Handle), computeapi.ExecRunTargetRequest{Argv: []string{"sh", "-c", "echo hi; echo err >&2; exit 4"}})
	if err != nil || out.Exit != 4 || out.Stdout != "hi\n" || out.Stderr != "err\n" {
		t.Errorf("exec: %+v %v", out, err)
	}
	if _, err := computeclient.ExecRunTarget(ctx, bo, "sandbox", idOf(h.Handle), computeapi.ExecRunTargetRequest{Argv: []string{"id"}}); code(err) != api.CodeForbidden {
		t.Errorf("exec by another member: %v", err)
	}
	if _, err := computeclient.ExecRunTarget(ctx, ana, "sandbox", idOf(h.Handle), computeapi.ExecRunTargetRequest{}); code(err) != api.CodeBadRequest {
		t.Errorf("an empty argv: %v", err)
	}
	if _, err := computeclient.ExecRunTarget(ctx, ana, "sandbox", idOf(h.Handle), computeapi.ExecRunTargetRequest{Argv: []string{"id"}, TimeoutS: 601}); code(err) != api.CodeBadRequest {
		t.Errorf("timeout past 600: %v", err)
	}
	// A host that boots something other than the pin: refused and released.
	if _, err := computeclient.AcquireRunTarget(ctx, ana, "drift", computeapi.AcquireRunTargetRequest{RequestID: "d1", TTLS: 60}); code(err) != api.CodeInternal {
		t.Errorf("a digest off the pin: %v", err)
	}
	if hs, _ := computeclient.RunTargetHandles(ctx, ana, "drift", true); len(hs) != 0 {
		t.Errorf("the drifted box is still live: %+v", hs)
	}
	// Release ends it; the hook hears it once, and exec is then a conflict.
	if rel, err := computeclient.ReleaseRunTarget(ctx, ana, "sandbox", idOf(h.Handle)); err != nil || !rel.Released {
		t.Fatalf("release: %+v %v", rel, err)
	}
	_, _ = computeclient.RunTargetStatus(ctx, ana, "sandbox", idOf(h.Handle)) // a later status does not end it again
	if _, err := computeclient.ExecRunTarget(ctx, ana, "sandbox", idOf(h.Handle), computeapi.ExecRunTargetRequest{Argv: []string{"id"}}); code(err) != api.CodeConflict {
		t.Errorf("exec after release: %v", err)
	}
	// A box left to expire: the sweep asks status past the deadline and ends it.
	short, err := computeclient.AcquireRunTarget(ctx, ana, "sandbox", computeapi.AcquireRunTargetRequest{RequestID: "b2", TTLS: 2})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(3 * time.Second) // the driver ends it at its deadline
	svc.SetNow(func() time.Time { return time.Now().Add(10 * time.Second) })
	svc.Sweep(ctx)
	svc.SetNow(time.Now)
	if st, err := computeclient.RunTargetStatus(ctx, owner, "sandbox", idOf(short.Handle)); err != nil || st.State != "expired" || st.Ended == "" {
		t.Errorf("after the sweep: %+v %v", st, err)
	}
	// Expiry's one signal: dev.zm.run_target.expired keyed by the handle.
	var expired int
	_ = owner.Events(ctx, api.EventQuery{Types: computeapi.EventRunTargetExpired, Subject: short.Handle, Since: "0", NoLive: true}, func(api.Event) error {
		expired++
		return nil
	})
	if expired != 1 {
		t.Errorf("run_target.expired events for %s: %d", short.Handle, expired)
	}
	// The hook heard each ended handle exactly once: the drifted box, the released one, the expired one.
	want := map[string]string{h.Handle: "released", short.Handle: "expired"}
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		n := len(ended)
		mu.Unlock()
		if n >= 3 || time.Now().After(deadline) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond) // a duplicate would land by now
	mu.Lock()
	defer mu.Unlock()
	seen := map[string]int{}
	for _, e := range ended {
		seen[e.Handle]++
		if w, ok := want[e.Handle]; ok && e.State != w {
			t.Errorf("end hook for %s: state %s, want %s", e.Handle, e.State, w)
		}
	}
	if len(ended) != 4 || seen[h.Handle] != 1 || seen[short.Handle] != 1 || seen[th.Handle] != 1 {
		t.Errorf("end hook calls: %+v", ended)
	}
}
