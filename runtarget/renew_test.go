package runtarget_test

import (
	"context"
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
	"github.com/sengirunners/potluck/runtarget"
	"github.com/sengirunners/potluck/wiring"
)

// fakeSetupDriver answers describe (with setup) and setup, logging each
// setup's dry_run to log and each envelope to the envelope log. Its
// describe has only the old key ("zm"), as a step 1 driver's.
const fakeSetupDriver = `#!/bin/sh
in=$(cat)
printf '%s\n' "$in" >>"$ENVLOG"
case "$in" in
*'"verb":"describe"'*) echo '{"ok":true,"result":{"zm":"zm/v0","name":"fake","kind":"run","version":"1","verbs":["describe","acquire","status","release","list","setup"],"available":true,"isolation":"ephemeral","credentials":false,"cost":{"unit":"s","amount":0},"reports_spend":false,"ttl_max_s":60,"capabilities":[]}}' ;;
*'"verb":"setup"'*'"dry_run":true'*) echo dry >>"$LOG"; echo '{"ok":true,"result":{"notes":["would make a pool"]}}' ;;
*'"verb":"setup"'*) echo real >>"$LOG"; echo '{"ok":true,"result":{"notes":["made a pool"]}}' ;;
*) echo '{"ok":false,"error":{"code":"not_implemented","message":"no"}}' ;;
esac
`

// TestBindSetup: bind --setup runs a dry run then the real setup; a
// refused bind runs none; a driver without the verb is refused. The
// session-held renewal and the lease-key refusal need zm's sessions and
// work: internal/wiring/fulltest's TestRunTargetRenewalFollowsSession.
func TestBindSetup(t *testing.T) {
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
	repo, err := refdriver.NewRepo(fix, "#!/bin/sh\nsleep 60\n")
	if err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(fix, "state")
	ref, err := refdriver.Install(fix, state, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { refdriver.KillAll(state) })
	fake := filepath.Join(fix, "zm-run-fake")
	setupLog := filepath.Join(fix, "setup.log")
	envLog := filepath.Join(fix, "envelopes.log")
	if err := os.WriteFile(fake, []byte(strings.NewReplacer(`"$LOG"`, `"`+setupLog+`"`, `"$ENVLOG"`, `"`+envLog+`"`).Replace(fakeSetupDriver)), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, m := range dm.Modules() {
		if s, ok := m.(*runtarget.Service); ok {
			s.SetLookPath(func(name string) (string, error) {
				if name == "fake" {
					return fake, nil
				}
				return ref, nil
			})
		}
	}
	ctx := context.Background()
	owner := client.WithHandler(dm.SocketHandler())
	owner.Project, owner.ProjectFrom = "acme", "flag"
	if _, err := owner.CreateProject(ctx, api.NewProjectRequest{Slug: "acme", RepoRules: api.RepoRules{Paths: []string{fix}, Remotes: []string{repo.Remote}}}); err != nil {
		t.Fatal(err)
	}

	// bind --setup: a dry run, then the real one, then the binding.
	v, err := computeclient.BindRunTarget(ctx, owner, computeapi.BindRunTargetRequest{Setup: true, Target: computeapi.RunTarget{Name: "pool", Driver: "fake", Kind: "run",
		Isolation: "ephemeral", Repo: repo.Remote, Branch: "main"}})
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(setupLog); string(b) != "dry\nreal\n" || len(v.SetupNotes) != 2 || !strings.HasPrefix(v.SetupNotes[0], "plan: ") {
		t.Errorf("setup: log %q notes %v", b, v.SetupNotes)
	}
	// Rename step 2: every envelope carries the protocol under both keys,
	// so a step 1 (or older) driver reading "zm" and one reading "potluck"
	// both find it; this driver's zm-only describe was taken.
	envs, _ := os.ReadFile(envLog)
	for _, l := range strings.Split(strings.TrimSpace(string(envs)), "\n") {
		if !strings.Contains(l, `"potluck":"potluck/1"`) || !strings.Contains(l, `"zm":"zm/v0"`) {
			t.Errorf("envelope %s", l)
		}
	}
	if !strings.Contains(string(envs), `"verb":"describe"`) {
		t.Errorf("no describe sent: %s", envs)
	}
	// A refused bind (bound already, no replace) runs no setup at all.
	if _, err := computeclient.BindRunTarget(ctx, owner, computeapi.BindRunTargetRequest{Setup: true, Target: computeapi.RunTarget{Name: "pool", Driver: "fake", Kind: "run",
		Isolation: "ephemeral", Repo: repo.Remote, Branch: "main"}}); code(err) != api.CodeConflict {
		t.Errorf("rebind without replace: %v", err)
	}
	if b, _ := os.ReadFile(setupLog); string(b) != "dry\nreal\n" {
		t.Errorf("setup ran for a refused bind: %q", b)
	}
	if _, err := computeclient.BindRunTarget(ctx, owner, computeapi.BindRunTargetRequest{Setup: true, Target: computeapi.RunTarget{Name: "vm0", Driver: "ref", Kind: "run",
		Isolation: "ephemeral", Repo: repo.Remote, Branch: "main", Config: map[string]string{"remote": repo.Remote}}}); code(err) != api.CodeBadRequest {
		t.Errorf("setup on a driver without the verb: %v", err)
	}
	// An acquire past the lifetime cap is refused before it is recorded,
	// and says so (a sandbox create asks the next binding); the driver's own
	// refusal is not marked: the daemon cannot know what it made.
	run := computeapi.AcquireRunTargetRequest{RequestID: "l1", Ref: "main", Script: "probe.sh", TTLS: 120}
	_, err = computeclient.AcquireRunTarget(ctx, owner, "pool", run)
	if why, ok := computeapi.RefusedBeforeRecord(err); !ok || why != computeapi.RefusedLifetime {
		t.Errorf("past the lifetime cap: %v (refused before record %q %v)", err, why, ok)
	}
	run.TTLS = 30
	_, err = computeclient.AcquireRunTarget(ctx, owner, "pool", run)
	if why, ok := computeapi.RefusedBeforeRecord(err); err == nil || ok {
		t.Errorf("the driver's refusal: %v (refused before record %q %v), want an unmarked error", err, why, ok)
	}
}
