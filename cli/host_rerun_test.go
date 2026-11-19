package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/conformance/refdriver"
	"github.com/sengirunners/potluck/hostprobe"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/tailnet/tailnettest"
	"github.com/sengirunners/potluck/kit/teamlink"
	"github.com/sengirunners/potluck/runtarget"
)

// Running potluck host again where this machine already shares sizes with
// the pool, with nothing that shapes the share, only prints what it shares
// and what the run would do: no pull, no conformance, no change. --yes, or
// a flag that says what to change, shares again as before.
func TestHostRerunAsksFirst(t *testing.T) {
	oldBeat, oldRunner, oldDetect, oldDriver := teamlink.BeatEvery, shareRunner, detectHost, shareDriver
	t.Cleanup(func() {
		teamlink.BeatEvery, shareRunner, detectHost, shareDriver = oldBeat, oldRunner, oldDetect, oldDriver
	})
	teamlink.BeatEvery = time.Hour // beaten by hand, below
	var ran atomic.Int32
	shareRunner = runnerFunc(func(context.Context, string, ...string) (hostprobe.Result, error) {
		ran.Add(1)
		return hostprobe.Result{Stdout: "PASS"}, nil
	})
	detectHost = func(context.Context, string) hostprobe.Report {
		return hostprobe.Report{OS: runtime.GOOS, Arch: "amd64", Cores: 8, RAMMB: 16 << 10, FreeDiskGB: 100, KVM: true,
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

	m.ok(t, "create", "--via", "tunnel", "trial")
	share := []string{"host", "--sizes", "c1m2", "--reclaim-idle-s", "0", "--no-sudo"}
	if out := m.ok(t, share...); !strings.Contains(out, "shared solo-c1m2") {
		t.Fatalf("host:\n%s", out)
	}

	// A bare re-run, with no flag at all, with the pool named, and with
	// only flags that change no share: the plan, then a refusal naming
	// --yes and host status; nothing ran.
	for _, args := range [][]string{{"host"}, {"host", "trial", "--no-sudo"}, {"host", "--detach", "--pull-timeout", "5m"}} {
		before := ran.Load()
		o, e, code := m.run(t, args...)
		if code != 1 || !strings.Contains(o, "this machine already shares solo-c1m2 with pool trial; potluck host again would do this") || !strings.Contains(o, "would share solo-c1m2") ||
			!strings.Contains(e, "nothing changed") || !strings.Contains(e, "--yes") || !strings.Contains(e, "potluck host status") {
			t.Fatalf("%v: exit %d\nstdout:\n%s\nstderr:\n%s", args, code, o, e)
		}
		if ran.Load() != before {
			t.Fatalf("%v pulled or ran conformance", args)
		}
	}
	// Under --json stdout stays empty: the refusal is the answer.
	o, e, code := m.run(t, "--json", "host")
	var answer api.ErrorResponse
	if code != 1 || o != "" || json.Unmarshal([]byte(e), &answer) != nil || answer.Error == nil || answer.Error.Code != api.CodeConflict ||
		!strings.Contains(answer.Error.Hint, "--yes") {
		t.Fatalf("--json host: exit %d\nstdout:\n%s\nstderr:\n%s", code, o, e)
	}
	// --yes shares again.
	before := ran.Load()
	if out := m.ok(t, "host", "--yes", "--no-sudo"); !strings.Contains(out, "shared solo-c1m2") || ran.Load() == before {
		t.Fatalf("host --yes:\n%s", out)
	}
	// A flag that shapes the share is the owner saying what to change.
	if out := m.ok(t, share...); !strings.Contains(out, "shared solo-c1m2") {
		t.Fatalf("host with flags again:\n%s", out)
	}
	// The old spelling, which programs run, never asks.
	before = ran.Load()
	if out := m.ok(t, "host", "share", "--no-sudo"); !strings.Contains(out, "shared solo-c1m2") || ran.Load() == before {
		t.Fatalf("host share:\n%s", out)
	}
}

// In a pool this machine joined, a size shared only with another pool is
// not shared again by a run for this one: that run is a first share there.
func TestSharedAgainIsThisPools(t *testing.T) {
	o := hostprobe.ShareOptions{NamePrefix: "solo", Earlier: []computeapi.RunTarget{
		{Name: "solo-c1m2", Kind: "box"}, {Name: "solo-c2m4", Kind: "box"}, {Name: "other-c1m2", Kind: "box"}, {Name: "solo-c1m2@far", Kind: "box", Host: "far"},
	}}
	shared := map[string][]string{"solo-c1m2": {"alpha"}, "solo-c2m4": {"01BETAID"}}
	for _, c := range []struct {
		sc   hostScope
		want []string
	}{
		{hostScope{pool: "alpha", machine: "m", poolID: "01ALPHAID", shared: shared}, []string{"solo-c1m2"}},
		{hostScope{pool: "beta", machine: "m", poolID: "01BETAID", shared: shared}, []string{"solo-c2m4"}},
		{hostScope{pool: "gamma", machine: "m", poolID: "01GAMMAID", shared: shared}, nil},
		{hostScope{pool: "home"}, []string{"solo-c1m2", "solo-c2m4"}}, // a pool this machine runs: its own sizes
	} {
		if got := c.sc.sharedAgain(o); !slices.Equal(got, c.want) {
			t.Errorf("%s: %v, want %v", c.sc.pool, got, c.want)
		}
	}
}
