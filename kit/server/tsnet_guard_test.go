package server_test

import (
	"context"
	. "github.com/sengirunners/potluck/kit/server"
	"github.com/sengirunners/potluck/wiring"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/tailnet"
)

// A test daemon that was given no fake tailnet node can never start a real
// one: its owner's POST /v0/tailnet/up (as a route walk sends) fails, and
// nothing of tsnet is written to its home. A walk of a fresh daemon once
// reached Tailscale's control server this way.
func TestTestDaemonNeverStartsARealTailnetNode(t *testing.T) {
	if os.Getenv(tailnet.EnvTestTSNet) == "1" {
		t.Skip(tailnet.EnvTestTSNet + "=1 lets this binary start real nodes")
	}
	ctx := context.Background()
	home := shortHome(t)
	cfg, err := config.LoadFrom(home)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Human, cfg.Machine = "judy", "guard"
	dm, err := New(ctx, cfg, nil, wiring.Kit)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(dm.Close)
	st, err := client.WithHandler(dm.SocketHandler()).TailnetUp(ctx, api.TailnetUpRequest{})
	if err == nil && st.Running {
		t.Fatalf("a test daemon started a tailnet node: %+v", st)
	}
	if !(err != nil && strings.Contains(err.Error(), "test binary")) && !strings.Contains(st.Error, "test binary") {
		t.Fatalf("tailnet up was not refused as a test binary's: %v %+v", err, st)
	}
	st2, _ := client.WithHandler(dm.SocketHandler()).Tailnet(ctx)
	if st2.Running || st2.AuthURL != "" {
		t.Fatalf("tailnet status after up: %+v", st2)
	}
	if _, err := os.Stat(filepath.Join(home, tailnet.StateDir, "tailscaled.state")); err == nil {
		t.Fatal("tsnet wrote its state in the test daemon's home")
	}
}
