package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/potluck/conformance/refdriver"
	"github.com/sengirunners/potluck/hostprobe"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/tailnet/tailnettest"
	"github.com/sengirunners/potluck/kit/teamlink"
	"github.com/sengirunners/potluck/runtarget"
)

// One machine is enough to try Potluck: with --via tunnel and no tunnel
// configured (no Tailscale), the machine that runs the pool shares itself
// and runs a sandbox on itself (the one-machine trial the README leads with).
func TestOneMachineTrialWithoutTailscale(t *testing.T) {
	oldBeat, oldRunner, oldDetect, oldDriver := teamlink.BeatEvery, shareRunner, detectHost, shareDriver
	t.Cleanup(func() {
		teamlink.BeatEvery, shareRunner, detectHost, shareDriver = oldBeat, oldRunner, oldDetect, oldDriver
	})
	teamlink.BeatEvery = time.Hour // beaten by hand, below
	shareRunner = runnerFunc(func(context.Context, string, ...string) (hostprobe.Result, error) {
		return hostprobe.Result{Stdout: "PASS"}, nil
	})
	detectHost = func(context.Context, string) hostprobe.Report {
		return hostprobe.Report{OS: "linux", Arch: "amd64", Cores: 8, RAMMB: 16 << 10, FreeDiskGB: 100, KVM: true,
			Layers: []hostprobe.Layer{{Name: "oci", Available: true, Supported: true, CLI: "docker", Runtime: "runsc", Runtimes: []string{"runsc"}}}}
	}
	shareDriver = "ref"

	m := startMachine(t, tailnettest.New(), "solo", "judy@example.com")
	fix, _ := filepath.EvalSymlinks(t.TempDir())
	state := filepath.Join(fix, "state")
	drv, err := refdriver.Install(fix, state, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { refdriver.KillAll(state) })
	module[*runtarget.Service](t, m).SetLookPath(func(string) (string, error) { return drv, nil })
	installed := filepath.Join(m.home, runtarget.DriversDir, "zm-run-ref")
	if err := os.MkdirAll(filepath.Dir(installed), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(drv, installed); err != nil {
		t.Fatal(err)
	}
	stop := beat(module[*teamlink.Service](t, m))
	defer stop()

	if out := m.ok(t, "create", "--via", "tunnel", "trial"); !strings.Contains(out, "pool trial is ready") {
		t.Fatalf("create:\n%s", out)
	}
	if out := m.ok(t, "host", "--image", refPin, "--sizes", "c1m2", "--name-prefix", "solo", "--reclaim-idle-s", "0", "--no-sudo"); !strings.Contains(out, "shared solo-c1m2") {
		t.Fatalf("host:\n%s", out)
	}
	out := m.ok(t, "sandbox", "create", "--ttl", "5m")
	id, _, _ := strings.Cut(strings.TrimSpace(out), " ")
	if !strings.Contains(out, " ready ") {
		t.Fatalf("sandbox create:\n%s", out)
	}
	if o := m.ok(t, "sandbox", "exec", id, "--", "echo", "hi"); o != "hi\n" {
		t.Fatalf("exec: %q", o)
	}
	m.ok(t, "sandbox", "rm", id)
	// None of it brought the tailnet node up: no Tailscale was needed.
	if st, err := client.WithHandler(m.dm.SocketHandler()).Tailnet(context.Background()); err == nil && st.Running {
		t.Error("the one-machine trial started the tailnet node")
	}
}
