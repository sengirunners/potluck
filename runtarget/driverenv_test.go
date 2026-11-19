package runtarget_test

import (
	"context"
	"errors"
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

// A driver gets the daemon's Potluck prefix (config.EnvPrefix), so its
// hints name Potluck's commands as the person types them, and none of the
// daemon's other variables.
func TestDriverGetsThePrefix(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("DRIVERENV_SECRET", "s3cret")
	old := config.Prefix
	t.Cleanup(func() { config.Prefix = old })
	config.Prefix = "supper pool"
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
	drv := filepath.Join(fix, "zm-run-env")
	body := "#!/bin/sh\ncat >/dev/null\nprintf '{\"ok\":false,\"error\":{\"code\":\"conflict\",\"message\":\"prefix=%s secret=%s\"}}\\n' \"$POTLUCK_PREFIX\" \"$DRIVERENV_SECRET\"\n"
	if err := os.WriteFile(drv, []byte(body), 0o755); err != nil {
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
	if _, err := owner.CreateProject(ctx, api.NewProjectRequest{Slug: "acme", RepoRules: api.RepoRules{Paths: []string{fix}, Remotes: []string{repo.Remote}}}); err != nil {
		t.Fatal(err)
	}
	_, err = computeclient.BindRunTarget(ctx, owner, computeapi.BindRunTargetRequest{Target: computeapi.RunTarget{
		Name: "env", Driver: "env", Kind: "run", Isolation: "ephemeral", Repo: repo.Remote, Branch: "main"}})
	var e *api.Error
	if !errors.As(err, &e) || e.Code != api.CodeConflict {
		t.Fatalf("bind: %v, want the driver's conflict", err)
	}
	if !strings.Contains(e.Message, "prefix=supper pool secret=") || strings.Contains(e.Message, "s3cret") {
		t.Errorf("the driver saw %q, want the prefix and no other variable", e.Message)
	}
}
