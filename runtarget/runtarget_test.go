package runtarget_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
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
	"github.com/sengirunners/potluck/kit/project"
	"github.com/sengirunners/potluck/kit/server"
	"github.com/sengirunners/potluck/runtarget"
	"github.com/sengirunners/potluck/wiring"
)

func TestMain(m *testing.M) {
	if code, ok := refdriver.Main(); ok {
		os.Exit(code)
	}
	os.Exit(m.Run())
}

func code(err error) string {
	var e *api.Error
	if errors.As(err, &e) {
		return e.Code
	}
	if err != nil {
		return err.Error()
	}
	return ""
}

func idOf(h string) string { return h[strings.LastIndex(h, "/")+1:] }

// TestRunTargets drives SPEC 20's daemon side against the reference driver:
// owner-only binds, routing by the remote, the stricter class, local-only
// guarded acquire, the scripts allowlist, idempotent request ids, caps,
// status, release and unbind.
func TestRunTargets(t *testing.T) {
	home, err := os.MkdirTemp("", "zm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	t.Setenv("HOME", home) // drivers and git get a throwaway HOME
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
	repo, err := refdriver.NewRepo(fix, "#!/bin/sh\nexit 3\n")
	if err != nil {
		t.Fatal(err)
	}
	// A pushed branch at the older commit: not main's head.
	if out, err := exec.Command("git", "-C", repo.Worktree, "push", "-q", repo.Remote, repo.Commit+":refs/heads/feature").CombinedOutput(); err != nil {
		t.Fatalf("push feature: %v %s", err, out)
	}
	state := filepath.Join(fix, "state")
	drv, err := refdriver.Install(fix, state, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { refdriver.KillAll(state) })
	for _, m := range dm.Modules() {
		if s, ok := m.(*runtarget.Service); ok {
			s.SetLookPath(func(name string) (string, error) {
				if name != "ref" {
					return "", api.Errorf(api.CodeUnavailable, "no zm-run-%s", name)
				}
				return drv, nil
			})
		}
	}
	ctx := context.Background()
	owner := client.WithHandler(dm.SocketHandler())
	owner.Project, owner.ProjectFrom = "acme", "flag"
	if _, err := owner.CreateProject(ctx, api.NewProjectRequest{Slug: "acme", RepoRules: api.RepoRules{Remotes: []string{repo.Remote}}}); err != nil {
		t.Fatal(err)
	}
	tok, err := owner.MintToken(ctx, api.MintTokenRequest{As: "ana", Project: "acme", Role: "member"})
	if err != nil {
		t.Fatal(err)
	}
	ana := client.WithHandler(dm.Handler)
	ana.Token = tok.Secret
	ana.Project, ana.ProjectFrom = "acme", "flag"

	base := func(name string) computeapi.RunTarget {
		return computeapi.RunTarget{Name: name, Driver: "ref", Kind: "run", Isolation: "ephemeral", Repo: repo.Remote, Branch: "main",
			Config: map[string]string{"remote": repo.Remote}}
	}
	// Binds: owner only, checked, describe must answer, no secret values.
	if _, err := computeclient.BindRunTarget(ctx, ana, computeapi.BindRunTargetRequest{Target: base("vm")}); code(err) != api.CodeForbidden {
		t.Errorf("a token bind: %v", err)
	}
	if _, err := computeclient.BindRunTarget(ctx, owner, computeapi.BindRunTargetRequest{Target: base("vm")}); err != nil {
		t.Fatal(err)
	}
	if _, err := computeclient.BindRunTarget(ctx, owner, computeapi.BindRunTargetRequest{Target: base("vm")}); code(err) != api.CodeConflict {
		t.Errorf("rebind without replace: %v", err)
	}
	host := base("host")
	host.Isolation = "none"
	if _, err := computeclient.BindRunTarget(ctx, owner, computeapi.BindRunTargetRequest{Target: host}); code(err) != api.CodeBadRequest {
		t.Errorf("an unisolated run target without scripts: %v", err)
	}
	host.Scripts = []string{"probe.sh"}
	if _, err := computeclient.BindRunTarget(ctx, owner, computeapi.BindRunTargetRequest{Target: host}); err != nil {
		t.Fatal(err)
	}
	for name, mut := range map[string]func(*computeapi.RunTarget){
		"secret value": func(b *computeapi.RunTarget) { b.Config["api_token"] = "x" },
		"budget unit": func(b *computeapi.RunTarget) {
			b.Budget = &computeapi.RunTargetBudget{Unit: "usd", Limit: 5, Period: "day"}
		},
		"other remote":  func(b *computeapi.RunTarget) { b.Repo = "https://example.invalid/x.git" },
		"driver path":   func(b *computeapi.RunTarget) { b.Driver = "/bin/sh" },
		"shell script":  func(b *computeapi.RunTarget) { b.Scripts = []string{"probe.sh; rm -rf /"} },
		"no driver bin": func(b *computeapi.RunTarget) { b.Driver = "missing" },
		"url password":  func(b *computeapi.RunTarget) { b.Config["dsn"] = "postgres://u:pw@db:5432/x" },
	} {
		b := base("bad")
		mut(&b)
		if _, err := computeclient.BindRunTarget(ctx, owner, computeapi.BindRunTargetRequest{Target: b}); err == nil {
			t.Errorf("bind with %s: accepted", name)
		}
	}
	capped := base("capped")
	capped.HandlesMax = 1
	capped.Budget = &computeapi.RunTargetBudget{Unit: "cpu_s", Limit: 100, Period: "day"}
	if _, err := computeclient.BindRunTarget(ctx, owner, computeapi.BindRunTargetRequest{Target: capped}); err != nil {
		t.Fatal(err)
	}
	// The bindings are in project.toml and parse back.
	l, err := computeclient.RunTargets(ctx, owner, true)
	if err != nil || len(l.Targets) != 3 || l.Targets[0].Describe == nil || l.Targets[1].Effective.Isolation != "none" {
		t.Fatalf("list: %+v %v", l, err)
	}
	// A budgeted binding shows this period's recorded spend, for placement.
	if sp := l.Targets[2].BudgetSpent; sp == nil || *sp != 0 || l.Targets[0].BudgetSpent != nil {
		t.Errorf("budget_spent: capped %v, vm %v", sp, l.Targets[0].BudgetSpent)
	}
	if v, err := computeclient.DescribeRunTarget(ctx, ana, "capped"); err != nil || v.BudgetSpent == nil {
		t.Errorf("describe's budget_spent: %+v %v", v, err)
	}
	// Driver config is the owner's: a member sees the binding without it.
	if al, err := computeclient.RunTargets(ctx, ana, false); err != nil || len(al.Targets) != 3 || al.Targets[0].Config != nil {
		t.Errorf("a member's list shows driver config: %+v %v", al, err)
	}
	projs, _ := owner.Projects(ctx)
	b, err := os.ReadFile(filepath.Join(home, "projects", projs[0].ID, "project.toml"))
	if err != nil {
		t.Fatal(err)
	}
	f, err := project.ParseFile(string(b))
	rts, _ := f.Sections["run_targets"].([]computeapi.RunTarget)
	if err != nil || len(rts) != 3 || rts[2].Budget.Limit != 100 || rts[0].Config["remote"] != repo.Remote {
		t.Fatalf("project.toml: %+v %v\n%s", rts, err, b)
	}

	// Routing: a ref, resolved on the remote; the commit is zm's.
	run := computeapi.AcquireRunTargetRequest{RequestID: "k1", Ref: "main", Script: "probe.sh", TTLS: 60}
	h, err := computeclient.AcquireRunTarget(ctx, ana, "vm", run)
	if err != nil {
		t.Fatal(err)
	}
	if h.Commit != repo.Tip || !strings.HasPrefix(h.Handle, "run:vm/") || h.Actor != "human:ana" || h.ExpiresAt == "" {
		t.Fatalf("acquire: %+v", h)
	}
	again, err := computeclient.AcquireRunTarget(ctx, ana, "vm", run)
	if err != nil || again.Handle != h.Handle {
		t.Errorf("the same request_id: %+v %v", again, err)
	}
	tok2, err := owner.MintToken(ctx, api.MintTokenRequest{As: "bo", Project: "acme", Role: "member"})
	if err != nil {
		t.Fatal(err)
	}
	bo := client.WithHandler(dm.Handler)
	bo.Token = tok2.Secret
	bo.Project, bo.ProjectFrom = "acme", "flag"
	// Keys are per actor: bo's k1 is bo's own acquire, never ana's handle.
	if bh, err := computeclient.AcquireRunTarget(ctx, bo, "vm", computeapi.AcquireRunTargetRequest{RequestID: "k1", Ref: "main", Script: "probe.sh", TTLS: 60}); err != nil || bh.Handle == h.Handle {
		t.Errorf("bo reusing ana's key: %+v %v", bh, err)
	}
	changed := run
	changed.Args = []string{"x"}
	if _, err := computeclient.AcquireRunTarget(ctx, ana, "vm", changed); code(err) != api.CodeConflict {
		t.Errorf("a changed request under the same key: %v", err)
	}
	for name, c := range map[string]struct {
		req  computeapi.AcquireRunTargetRequest
		want string
	}{
		"no ref":           {computeapi.AcquireRunTargetRequest{RequestID: "e1", Script: "probe.sh"}, api.CodeBadRequest},
		"unpushed ref":     {computeapi.AcquireRunTargetRequest{RequestID: "e2", Ref: "nope", Script: "probe.sh"}, api.CodeBadRequest},
		"other commit":     {computeapi.AcquireRunTargetRequest{RequestID: "e3", Ref: "main", Commit: repo.Commit, Script: "probe.sh"}, api.CodeForbidden},
		"shell script":     {computeapi.AcquireRunTargetRequest{RequestID: "e4", Ref: "main", Script: "probe.sh;id"}, api.CodeBadRequest},
		"past the ttl":     {computeapi.AcquireRunTargetRequest{RequestID: "e5", Ref: "main", Script: "probe.sh", TTLS: 7200}, api.CodeBadRequest},
		"no request id":    {computeapi.AcquireRunTargetRequest{Ref: "main", Script: "probe.sh"}, api.CodeBadRequest},
		"actor not mine":   {computeapi.AcquireRunTargetRequest{RequestID: "e6", Ref: "main", Script: "probe.sh", Actor: "human:judy"}, api.CodeForbidden},
		"db fields":        {computeapi.AcquireRunTargetRequest{RequestID: "e7", Ref: "main", Script: "probe.sh", Name: "x"}, api.CodeBadRequest},
		"untrusted ref ok": {computeapi.AcquireRunTargetRequest{RequestID: "e8", Ref: "feature", Script: "probe.sh", TTLS: 60}, ""},
	} {
		if _, err := computeclient.AcquireRunTarget(ctx, ana, "vm", c.req); code(err) != c.want {
			t.Errorf("%s: got %v, want %q", name, err, c.want)
		}
	}
	// An unisolated target: local-only, the allowlist, and only main's head.
	// Each refusal says in plain words that the target cannot isolate untrusted code, and why.
	const plain = "run target host cannot isolate untrusted code (work on it is not walled off from this machine, its LAN and its tailnet)"
	if _, err := computeclient.AcquireRunTarget(ctx, ana, "host", computeapi.AcquireRunTargetRequest{RequestID: "h1", Ref: "main", Script: "probe.sh"}); code(err) != api.CodeForbidden || !strings.Contains(err.Error(), plain+", so only this machine's owner may use it") {
		t.Errorf("a token acquiring an unisolated target: %v", err)
	}
	if _, err := computeclient.AcquireRunTarget(ctx, owner, "host", computeapi.AcquireRunTargetRequest{RequestID: "h2", Ref: "feature", Script: "probe.sh"}); code(err) != api.CodeForbidden || !strings.Contains(err.Error(), "so it counts as untrusted, and "+plain) {
		t.Errorf("a non-head commit on an unisolated target: %v", err)
	}
	if _, err := computeclient.AcquireRunTarget(ctx, owner, "host", computeapi.AcquireRunTargetRequest{RequestID: "h3", Ref: "main", Script: "other.sh"}); code(err) != api.CodeForbidden {
		t.Errorf("a script outside the allowlist: %v", err)
	}
	hh, err := computeclient.AcquireRunTarget(ctx, owner, "host", computeapi.AcquireRunTargetRequest{RequestID: "h4", Ref: "main", Script: "probe.sh", TTLS: 60})
	if err != nil {
		t.Fatalf("the owner on main's head: %v", err)
	}
	// The owner acquiring for ana does not let ana repeat it: local-only holds on every request.
	forAna := computeapi.AcquireRunTargetRequest{RequestID: "h5", Ref: "main", Script: "probe.sh", TTLS: 60, Actor: "human:ana"}
	h5, err := computeclient.AcquireRunTarget(ctx, owner, "host", forAna)
	if err != nil {
		t.Fatalf("the owner acquiring for ana: %v", err)
	}
	forAna.Actor = ""
	if _, err := computeclient.AcquireRunTarget(ctx, ana, "host", forAna); code(err) != api.CodeForbidden {
		t.Errorf("ana repeating a guarded acquire: %v", err)
	}
	_, _ = computeclient.ReleaseRunTarget(ctx, owner, "host", idOf(h5.Handle))

	// Status: the run ends on its own with the script's exit.
	var st computeapi.RunTargetHandle
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		if st, err = computeclient.RunTargetStatus(ctx, ana, "vm", idOf(h.Handle)); err != nil || st.State == "ended" {
			break
		}
	}
	if err != nil || st.State != "ended" || st.Exit == nil || *st.Exit != 3 {
		t.Fatalf("status: %+v %v", st, err)
	}
	if _, err := computeclient.RunTargetStatus(ctx, ana, "vm", "r999"); code(err) != api.CodeNotFound {
		t.Errorf("an unknown handle: %v", err)
	}
	if _, err := computeclient.RunTargetStatus(ctx, ana, "host", idOf(h.Handle)); code(err) != api.CodeNotFound {
		t.Errorf("another binding's handle: %v", err)
	}
	// Release: the holder or the owner; twice answers released false.
	if _, err := computeclient.ReleaseRunTarget(ctx, ana, "host", idOf(hh.Handle)); code(err) != api.CodeForbidden {
		t.Errorf("ana releasing the owner's handle: %v", err)
	}
	rel, err := computeclient.ReleaseRunTarget(ctx, ana, "vm", idOf(h.Handle))
	if err != nil || rel.Released || rel.Exit == nil || *rel.Exit != 3 {
		t.Errorf("release of an ended run: %+v %v", rel, err)
	}

	// Caps: handles_max per actor.
	c1, err := computeclient.AcquireRunTarget(ctx, ana, "capped", computeapi.AcquireRunTargetRequest{RequestID: "c1", Ref: "main", Script: "probe.sh", TTLS: 60})
	if err != nil {
		t.Fatal(err)
	}
	_, err = computeclient.AcquireRunTarget(ctx, ana, "capped", computeapi.AcquireRunTargetRequest{RequestID: "c2", Ref: "main", Script: "probe.sh"})
	if why, ok := computeapi.RefusedBeforeRecord(err); code(err) != api.CodeForbidden || !ok || why != computeapi.RefusedHandlesMax {
		// Marked: nothing was made, so a sandbox create may ask the next binding with the same key.
		t.Errorf("past handles_max: %v (refused before record %q %v)", err, why, ok)
	}
	if err := computeclient.UnbindRunTarget(ctx, owner, "capped"); code(err) != api.CodeConflict {
		t.Errorf("unbind with a live handle: %v", err)
	}
	if rel, err := computeclient.ReleaseRunTarget(ctx, owner, "capped", idOf(c1.Handle)); err != nil || !rel.Released {
		// A run may already have ended (released false); either way it is no longer live.
		if err != nil {
			t.Fatalf("owner force release: %v", err)
		}
	}
	if rel, err := computeclient.ReleaseRunTarget(ctx, owner, "capped", idOf(c1.Handle)); err != nil || rel.Released {
		t.Errorf("a second release: %+v %v", rel, err)
	}
	hs, err := computeclient.RunTargetHandles(ctx, ana, "capped", true)
	if err != nil || len(hs) != 0 {
		t.Errorf("live handles after release: %+v %v", hs, err)
	}
	if _, err := computeclient.AcquireRunTarget(ctx, ana, "capped", computeapi.AcquireRunTargetRequest{RequestID: "c3", Ref: "main", Script: "probe.sh", TTLS: 60}); err != nil {
		t.Errorf("an acquire after release: %v", err)
	}
	_, _ = computeclient.ReleaseRunTarget(ctx, owner, "host", idOf(hh.Handle))
	if err := computeclient.UnbindRunTarget(ctx, owner, "host"); err != nil {
		t.Errorf("unbind: %v", err)
	}
	if _, err := computeclient.DescribeRunTarget(ctx, owner, "host"); code(err) != api.CodeNotFound {
		t.Errorf("describe after unbind: %v", err)
	}
}
