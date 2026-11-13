package runtarget_test

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/computeclient"
	"github.com/sengirunners/potluck/conformance/refdriver"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/server"
	"github.com/sengirunners/potluck/protocol"
	"github.com/sengirunners/potluck/runtarget"
	"github.com/sengirunners/potluck/wiring"
)

// TestBoxJITConfig: a box acquire's jit_config reaches the driver and is
// kept nowhere else: not in zm's home (databases, logs) and not in the
// answer. It is a box's only, and a request_id reused with another
// jit_config is a different request.
func TestBoxJITConfig(t *testing.T) {
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
	ref, err := refdriver.Install(fix, state, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { refdriver.KillAll(state) })
	// The driver zm runs: it keeps each request it is sent, then answers as refdriver.
	seen := filepath.Join(fix, "requests")
	drv := filepath.Join(fix, "zm-run-tap")
	if err := os.WriteFile(drv, []byte(fmt.Sprintf("#!/bin/sh\ntee -a %q | exec %q\n", seen, ref)), 0o755); err != nil {
		t.Fatal(err)
	}
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
	box := computeapi.RunTarget{Name: "sandbox", Driver: "ref", Kind: "box", Isolation: "ephemeral", Images: []string{refPin},
		Repo: repo.Remote, Branch: "main", Config: map[string]string{"remote": repo.Remote, "jit": "1"}}
	nojit := computeapi.RunTarget{Name: "nojit", Driver: "ref", Kind: "box", Isolation: "ephemeral", Images: []string{refPin},
		Repo: repo.Remote, Branch: "main", Config: map[string]string{"remote": repo.Remote}}
	run := computeapi.RunTarget{Name: "runner", Driver: "ref", Kind: "run", Isolation: "ephemeral", Repo: repo.Remote, Branch: "main",
		Config: map[string]string{"remote": repo.Remote}}
	for _, b := range []computeapi.RunTarget{box, nojit, run} {
		if _, err := computeclient.BindRunTarget(ctx, owner, computeapi.BindRunTargetRequest{Target: b}); err != nil {
			t.Fatal(err)
		}
	}
	const jit = "eyJzZWtyaXQiOiJ6bS1qaXQtdGVzdC12YWx1ZSJ9"
	h, err := computeclient.AcquireRunTarget(ctx, owner, "sandbox", computeapi.AcquireRunTargetRequest{RequestID: "j1", TTLS: 120, JITConfig: jit})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = computeclient.ReleaseRunTarget(ctx, owner, "sandbox", idOf(h.Handle)) }()
	if b, _ := os.ReadFile(seen); !strings.Contains(string(b), `"jit_config":"`+jit+`"`) {
		t.Errorf("the driver was not sent jit_config: %s", b)
	}
	if strings.Contains(fmt.Sprintf("%+v", h), jit) {
		t.Errorf("the answer carries jit_config: %+v", h)
	}
	if _, err := computeclient.AcquireRunTarget(ctx, owner, "sandbox", computeapi.AcquireRunTargetRequest{RequestID: "j1", TTLS: 120, JITConfig: jit + "x"}); code(err) != api.CodeConflict {
		t.Errorf("request_id j1 again with another jit_config: %v, want conflict", err)
	}
	for _, bad := range []string{"short", "has a\x00nul in it here", strings.Repeat("a", 64<<10+1)} {
		if _, err := computeclient.AcquireRunTarget(ctx, owner, "sandbox", computeapi.AcquireRunTargetRequest{RequestID: "bad", TTLS: 120, JITConfig: bad}); code(err) != api.CodeBadRequest {
			t.Errorf("jit_config %.12q: %v, want bad_request", bad, err)
		}
	}
	// A driver that does not list github-jit is never sent a jit_config.
	before, _ := os.ReadFile(seen)
	if _, err := computeclient.AcquireRunTarget(ctx, owner, "nojit", computeapi.AcquireRunTargetRequest{RequestID: "n1", TTLS: 120, JITConfig: jit}); code(err) != api.CodeBadRequest ||
		!strings.Contains(err.Error(), protocol.CapGitHubJIT) {
		t.Errorf("jit_config to a driver without github-jit: %v, want bad_request", err)
	}
	if b, _ := os.ReadFile(seen); strings.Count(string(b), jit) != strings.Count(string(before), jit) {
		t.Errorf("a driver without github-jit was sent jit_config: %s", b[len(before):])
	}
	if _, err := computeclient.AcquireRunTarget(ctx, owner, "runner", computeapi.AcquireRunTargetRequest{RequestID: "r1", Ref: "main", Script: "probe.sh", JITConfig: jit}); code(err) != api.CodeBadRequest {
		t.Errorf("jit_config on a run: %v, want bad_request", err)
	}
	// jit_guard (which jobs the runner serves) reaches the driver with its
	// jit_config, is part of the request, and needs a driver that keeps it.
	one := &computeapi.JITGuard{Repo: "acme/app", Events: []string{"push", "pull_request"}}
	h2, err := computeclient.AcquireRunTarget(ctx, owner, "sandbox", computeapi.AcquireRunTargetRequest{RequestID: "j2", TTLS: 120, JITConfig: jit, JITGuard: one})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = computeclient.ReleaseRunTarget(ctx, owner, "sandbox", idOf(h2.Handle)) }()
	if b, _ := os.ReadFile(seen); !strings.Contains(string(b), `"jit_guard":{"repo":"acme/app","events":["push","pull_request"]}`) {
		t.Errorf("the driver was not sent jit_guard: %s", b)
	}
	if _, err := computeclient.AcquireRunTarget(ctx, owner, "sandbox", computeapi.AcquireRunTargetRequest{RequestID: "j2", TTLS: 120, JITConfig: jit,
		JITGuard: &computeapi.JITGuard{Repo: "acme/app"}}); code(err) != api.CodeConflict {
		t.Errorf("request_id j2 again with another guard: %v, want conflict", err)
	}
	for _, bad := range []computeapi.AcquireRunTargetRequest{{JITGuard: one}, {JITConfig: jit, JITGuard: &computeapi.JITGuard{Repo: "acme/app\n"}},
		{JITConfig: jit, JITGuard: &computeapi.JITGuard{Repo: "acme/app", Events: []string{"push'; x"}}}} {
		bad.RequestID, bad.TTLS = "badguard", 120
		if _, err := computeclient.AcquireRunTarget(ctx, owner, "sandbox", bad); code(err) != api.CodeBadRequest {
			t.Errorf("jit_guard %+v (jit_config %t): %v, want bad_request", bad.JITGuard, bad.JITConfig != "", err)
		}
	}
	// jit_guard.run (the one run whose job the runner serves, held for the
	// door's ready marker) reaches the driver with the guard, only as a run
	// of the guard's repository, and needs a driver whose hook holds it.
	held := &computeapi.JITGuard{Repo: "acme/app", Events: []string{"push"},
		Run: &protocol.JITRun{ID: 4242, WorkflowRef: "Acme/App/.github/workflows/ci.yml@refs/heads/main", Ref: "refs/heads/main"}}
	h3, err := computeclient.AcquireRunTarget(ctx, owner, "sandbox", computeapi.AcquireRunTargetRequest{RequestID: "j3", TTLS: 120, JITConfig: jit, JITGuard: held})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = computeclient.ReleaseRunTarget(ctx, owner, "sandbox", idOf(h3.Handle)) }()
	if b, _ := os.ReadFile(seen); !strings.Contains(string(b), `"run":{"id":4242,"workflow_ref":"Acme/App/.github/workflows/ci.yml@refs/heads/main","ref":"refs/heads/main"}`) {
		t.Errorf("the driver was not sent jit_guard.run: %s", b)
	}
	// An open guard (open_if_private) widens events, so it needs them, and reaches the driver.
	open := &computeapi.JITGuard{Repo: "acme/app", Events: []string{"push"}, OpenIfPrivate: true}
	h4, err := computeclient.AcquireRunTarget(ctx, owner, "sandbox", computeapi.AcquireRunTargetRequest{RequestID: "j4", TTLS: 120, JITConfig: jit, JITGuard: open})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = computeclient.ReleaseRunTarget(ctx, owner, "sandbox", idOf(h4.Handle)) }()
	if b, _ := os.ReadFile(seen); !strings.Contains(string(b), `"jit_guard":{"repo":"acme/app","events":["push"],"open_if_private":true}`) {
		t.Errorf("the driver was not sent jit_guard.open_if_private: %s", b)
	}
	if _, err := computeclient.AcquireRunTarget(ctx, owner, "sandbox", computeapi.AcquireRunTargetRequest{RequestID: "badopen", TTLS: 120, JITConfig: jit,
		JITGuard: &computeapi.JITGuard{Repo: "acme/app", OpenIfPrivate: true}}); code(err) != api.CodeBadRequest {
		t.Errorf("jit_guard.open_if_private with no events: %v, want bad_request", err)
	}
	for _, r := range []protocol.JITRun{
		{ID: 4242, WorkflowRef: "acme/other/.github/workflows/ci.yml@refs/heads/main", Ref: "refs/heads/main"},
		{ID: 4242, WorkflowRef: "acme/app/.github/workflows/ci.yml@refs/tags/main", Ref: "refs/tags/main"},
		{ID: 4242, WorkflowRef: "acme/app/.github/workflows/ci.yml@refs/heads/main", Ref: "refs/heads/dev"},
		{ID: 0, WorkflowRef: "acme/app/.github/workflows/ci.yml@refs/heads/main", Ref: "refs/heads/main"},
		{ID: 4242, WorkflowRef: "acme/app/.github/workflows/ci.yml@refs/heads/main", Ref: "refs/heads/main", HoldS: protocol.JITHoldMax + 1},
	} {
		g := &computeapi.JITGuard{Repo: "acme/app", Run: &r}
		if _, err := computeclient.AcquireRunTarget(ctx, owner, "sandbox", computeapi.AcquireRunTargetRequest{RequestID: "badrun", TTLS: 120, JITConfig: jit, JITGuard: g}); code(err) != api.CodeBadRequest {
			t.Errorf("jit_guard.run %+v: %v, want bad_request", r, err)
		}
	}
	for name, strip := range map[string]string{
		"zm-run-nohold":    `s/"github-jit-guard-run",*//`,     // a driver whose hook would ignore the run
		"zm-run-nosecrets": `s/"box-secrets",*//`,              // a driver with no box secrets to hold the job for
		"zm-run-noprivate": `s/"github-jit-guard-private",*//`, // a driver whose hook would not widen its events
	} {
		guard, want := held, "cannot hold a job for its secrets"
		if name == "zm-run-noprivate" {
			guard, want = open, "cannot check that a job's repository is private"
		}
		drv := filepath.Join(fix, name)
		if err := os.WriteFile(drv, []byte(fmt.Sprintf("#!/bin/sh\n%q | sed '%s'\n", ref, strip)), 0o755); err != nil {
			t.Fatal(err)
		}
		for _, m := range dm.Modules() {
			if s, ok := m.(*runtarget.Service); ok {
				s.SetLookPath(func(string) (string, error) { return drv, nil })
			}
		}
		_ = os.Remove(filepath.Join(home, runtarget.PinFile)) // the owner trusts the new driver: pinned again on its first use
		if _, err := computeclient.AcquireRunTarget(ctx, owner, "sandbox", computeapi.AcquireRunTargetRequest{RequestID: "h-" + name, TTLS: 120, JITConfig: jit, JITGuard: guard}); code(err) != api.CodeBadRequest ||
			!strings.Contains(err.Error(), want) {
			t.Errorf("%+v to %s: %v, want bad_request", guard, name, err)
		}
	}
	unheld := filepath.Join(fix, "zm-run-unheld") // a driver that would ignore jit_guard
	if err := os.WriteFile(unheld, []byte(fmt.Sprintf("#!/bin/sh\n%q | sed 's/\"github-jit-guard\",*//'\n", ref)), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, m := range dm.Modules() {
		if s, ok := m.(*runtarget.Service); ok {
			s.SetLookPath(func(string) (string, error) { return unheld, nil })
		}
	}
	_ = os.Remove(filepath.Join(home, runtarget.PinFile)) // the owner trusts the new driver: pinned again on its first use
	if _, err := computeclient.AcquireRunTarget(ctx, owner, "sandbox", computeapi.AcquireRunTargetRequest{RequestID: "u1", TTLS: 120, JITConfig: jit, JITGuard: one}); code(err) != api.CodeBadRequest ||
		!strings.Contains(err.Error(), "github-jit-guard") {
		t.Errorf("jit_guard to a driver without github-jit-guard: %v, want bad_request", err)
	}
	// A driver that echoes the config in a refusal: the caller never sees it.
	echo := filepath.Join(fix, "zm-run-echo")
	script := `#!/bin/sh
in=$(cat)
case $in in
*'"verb":"acquire"'*)
	j=$(printf %s "$in" | sed -n 's/.*"jit_config":"\([^"]*\)".*/\1/p')
	printf '{"ok":false,"error":{"code":"unavailable","message":"no: %s"}}\n' "$j" ;;
*) printf %s "$in" | exec ` + ref + ` ;;
esac
`
	if err := os.WriteFile(echo, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, m := range dm.Modules() {
		if s, ok := m.(*runtarget.Service); ok {
			s.SetLookPath(func(string) (string, error) { return echo, nil })
		}
	}
	_ = os.Remove(filepath.Join(home, runtarget.PinFile)) // the owner trusts the new driver: pinned again on its first use
	if _, err := computeclient.AcquireRunTarget(ctx, owner, "sandbox", computeapi.AcquireRunTargetRequest{RequestID: "e1", TTLS: 120, JITConfig: jit}); err == nil || strings.Contains(err.Error(), jit) || !strings.Contains(err.Error(), "[jit_config]") {
		t.Errorf("a refusal echoing jit_config: %v, want it scrubbed", err)
	}
	_ = filepath.WalkDir(home, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if b, _ := os.ReadFile(p); strings.Contains(string(b), jit) {
				t.Errorf("jit_config is kept in %s", p)
			}
		}
		return nil
	})
}
