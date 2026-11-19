package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sengirunners/potluck/conformance/refdriver"
	"github.com/sengirunners/potluck/hostprobe"
	"github.com/sengirunners/potluck/hostsetup"
	"github.com/sengirunners/potluck/kit/tailnet/tailnettest"
	"github.com/sengirunners/potluck/kit/teamlink"
	"github.com/sengirunners/potluck/runtarget"
)

// When no size passes conformance, host's last words say what to do next:
// run it again, and that the images it pulled stay, so the next run starts at
// the checks. On a re-run after a share, they say the earlier sizes still
// lend and name the flags given, which did not take effect; never that this
// machine did not join the pool.
func TestHostNoSizePassedSaysWhatNext(t *testing.T) {
	oldBeat, oldRunner, oldDetect, oldDriver, oldGOOS := teamlink.BeatEvery, shareRunner, detectHost, shareDriver, setupGOOS
	t.Cleanup(func() {
		teamlink.BeatEvery, shareRunner, detectHost, shareDriver, setupGOOS = oldBeat, oldRunner, oldDetect, oldDriver, oldGOOS
	})
	teamlink.BeatEvery = time.Hour
	setupGOOS = "linux" // the host below is Linux: no menu bar item to set up, on any machine the test runs on
	var pass atomic.Bool
	shareRunner = runnerFunc(func(_ context.Context, name string, args ...string) (hostprobe.Result, error) {
		if strings.Contains(name, "conformance") && !pass.Load() {
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
	out, errOut, code := m.run(t, "host", "--sizes", "c1m2", "--name-prefix", "solo", "--reclaim-idle-s", "0", "--no-sudo")
	if code == 0 {
		t.Fatalf("host passed with every check failing:\n%s", out)
	}
	if !strings.Contains(out, "guest port probe did not listen") {
		t.Errorf("host does not name the failed check:\n%s", out)
	}
	for _, want := range []string{"this machine shares nothing with pool trial.", "Next: run the same potluck host command again", "pulled stay on this machine", "starts at the checks"} {
		if !strings.Contains(errOut, want) {
			t.Errorf("host's last words lack %q:\n%s", want, errOut)
		}
	}
	if strings.Contains(errOut, "did not take effect") {
		t.Errorf("a first share names flags as not taking effect:\n%s", errOut)
	}
	// The stages say where it stopped; host status says the same, with the
	// way on.
	for _, want := range []string{"[1/5] preflight: succeeded", "[2/5] images: succeeded", "[3/5] validation: blocked: what happened and what to do next are below"} {
		if !strings.Contains(errOut, want) {
			t.Errorf("host's stages lack %q:\n%s", want, errOut)
		}
	}
	if st := m.ok(t, "host", "status"); !strings.Contains(st, "Setup blocked at validation (3 of 5)") ||
		!strings.Contains(st, "next: fix what the validation stage says, then run again: potluck host trial --name-prefix solo --no-sudo=true --reclaim-idle-s 0 --sizes c1m2\n") {
		t.Errorf("host status after the blocked run:\n%s", st)
	}

	// Shared once, a re-run whose checks fail leaves solo-c1m2 lending.
	pass.Store(true)
	out, errOut, code = m.run(t, "host", "--sizes", "c1m2", "--name-prefix", "solo", "--reclaim-idle-s", "0", "--no-sudo")
	if code != 0 || !strings.Contains(out, "shared solo-c1m2") {
		t.Fatalf("host: exit %d\n%s%s", code, out, errOut)
	}
	for _, want := range []string{"[3/5] validation: succeeded: passed: solo-c1m2", "[4/5] join: succeeded: pool trial runs on this machine",
		"[5/5] usable capacity: succeeded: pool trial can use solo-c1m2 (1 vCPU, 2048 MB)", "\nsetup is complete: nothing Potluck can check is left"} {
		if !strings.Contains(errOut, want) {
			t.Errorf("a run that went through lacks %q:\n%s", want, errOut)
		}
	}
	if strings.Contains(out, "ran: ") {
		t.Errorf("a run without --show-commands printed raw command lines:\n%s", out)
	}
	if r, err := hostsetup.Read(m.home); err != nil || !r.Succeeded() {
		t.Errorf("record after the run went through: %+v %v", r, err)
	}
	pass.Store(false)
	out, errOut, code = m.run(t, "host", "--sizes", "c1m2", "--name-prefix", "solo", "--reclaim-idle-s", "0", "--no-sudo")
	if code == 0 {
		t.Fatalf("re-run passed with every check failing:\n%s", out)
	}
	for _, want := range []string{"nothing changed: the earlier sizes still lend to pool trial (solo-c1m2), as they were: --name-prefix, --reclaim-idle-s, --sizes did not take effect.", "Next: run the same potluck host command again"} {
		if !strings.Contains(errOut, want) {
			t.Errorf("re-run's last words lack %q:\n%s", want, errOut)
		}
	}
	if strings.Contains(errOut, "did not join") || strings.Contains(errOut, "--no-sudo") {
		t.Errorf("re-run says it did not join, or names a flag that shapes nothing:\n%s", errOut)
	}
}
