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
	"github.com/sengirunners/potluck/kit/tailnet/tailnettest"
	"github.com/sengirunners/potluck/kit/teamlink"
	"github.com/sengirunners/potluck/runtarget"
)

// When no size passes conformance, host's last words say what to do next:
// run it again, and that the images it pulled stay, so the next run starts at
// the checks.
func TestHostNoSizePassedSaysWhatNext(t *testing.T) {
	oldBeat, oldRunner, oldDetect, oldDriver := teamlink.BeatEvery, shareRunner, detectHost, shareDriver
	t.Cleanup(func() {
		teamlink.BeatEvery, shareRunner, detectHost, shareDriver = oldBeat, oldRunner, oldDetect, oldDriver
	})
	teamlink.BeatEvery = time.Hour
	shareRunner = runnerFunc(func(_ context.Context, name string, args ...string) (hostprobe.Result, error) {
		if strings.Contains(name, "conformance") {
			return hostprobe.Result{Exit: 1, Stdout: "FAIL  port transfers binary data: guest port probe did not listen\n"}, nil
		}
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
	installed := filepath.Join(m.home, runtarget.DriversDir, "zm-run-ref")
	if err := os.MkdirAll(filepath.Dir(installed), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(drv, installed); err != nil {
		t.Fatal(err)
	}
	if out := m.ok(t, "create", "--via", "tunnel", "trial"); !strings.Contains(out, "pool trial is ready") {
		t.Fatalf("create:\n%s", out)
	}
	out, errOut, code := m.run(t, "host", "--sizes", "c1m2", "--name-prefix", "solo", "--reclaim-idle-s", "0", "--no-sudo")
	if code == 0 {
		t.Fatalf("host passed with every check failing:\n%s", out)
	}
	if !strings.Contains(out, "guest port probe did not listen") {
		t.Errorf("host does not name the failed check:\n%s", out)
	}
	for _, want := range []string{"did not join the pool", "Next: run the same potluck host command again", "pulled stay on this machine", "starts at the checks"} {
		if !strings.Contains(errOut, want) {
			t.Errorf("host's last words lack %q:\n%s", want, errOut)
		}
	}
}
