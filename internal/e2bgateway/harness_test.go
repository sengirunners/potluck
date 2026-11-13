package e2bgateway

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

// TestMain lets the reference driver run as this test binary.
func TestMain(m *testing.M) {
	if code, ok := refdriver.Main(); ok {
		os.Exit(code)
	}
	os.Exit(m.Run())
}

const testKey = "e2b_0123456789abcdef0123456789abcdef"

type harness struct {
	g     *gateway
	owner *client.Client
	dm    *server.Daemon
	home  string
}

// newHarness starts a daemon in a temp ZM_HOME with one refdriver box
// binding ("sandbox", ttl_max_s 3600) in project acme, and a gateway over
// the daemon's socket handler with testKey as its one key.
func newHarness(t *testing.T) *harness {
	t.Helper()
	home, err := os.MkdirTemp("", "zm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	t.Setenv("HOME", home)
	t.Setenv("ZM_HOME", home)
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
	box := computeapi.RunTarget{Name: "sandbox", Driver: "ref", Kind: "box", Isolation: "ephemeral", TTLMaxS: 3600,
		Images: []string{"ref-image@sha256:0000000000000000000000000000000000000000000000000000000000000001"},
		Config: map[string]string{"remote": repo.Remote}}
	if _, err := computeclient.BindRunTarget(ctx, owner, computeapi.BindRunTargetRequest{Target: box}); err != nil {
		t.Fatal(err)
	}
	stateDir := filepath.Join(home, "e2b")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	keys := filepath.Join(stateDir, "keys")
	if err := os.WriteFile(keys, []byte("# test\n"+testKey+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	g, err := newGateway(gatewayConfig{Domain: "e2b.localhost", Advertise: "e2b.localhost:49982", StateDir: stateDir, KeysFile: keys}, owner)
	if err != nil {
		t.Fatal(err)
	}
	return &harness{g: g, owner: owner, dm: dm, home: home}
}
