package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/sengirunners/potluck/hostprobe"
)

// potluck host --linux-slots N: a plan binds linux_slots=N on every size and
// says so; a size that does not fit N times inside the share, or 0, is
// refused; without the flag nothing is bound.
func TestHostLinuxSlotsFlag(t *testing.T) {
	oldRunner, oldDetect, oldDriver := shareRunner, detectHost, shareDriver
	t.Cleanup(func() { shareRunner, detectHost, shareDriver = oldRunner, oldDetect, oldDriver })
	shareRunner = runnerFunc(func(context.Context, string, ...string) (hostprobe.Result, error) {
		t.Fatal("dry run executed a command")
		return hostprobe.Result{}, nil
	})
	detectHost = func(_ context.Context, dir string) hostprobe.Report { // half of it: 4 cores, 8 GiB
		return hostprobe.Report{OS: "linux", Arch: "amd64", Cores: 8, RAMMB: 16 << 10, FreeDiskGB: 100, KVM: true, StateDir: dir,
			Layers: []hostprobe.Layer{{Name: "oci", Available: true, Supported: true, CLI: "docker", Runtime: "runsc", Runtimes: []string{"runsc"}}}}
	}
	shareDriver = "ref"
	t.Setenv(EnvHome, t.TempDir())
	host := func(flags ...string) (string, string, int) {
		args := append([]string{"host", "--dry-run", "--image", refPin, "--name-prefix", "lender", "--no-sudo", "--reclaim-idle-s", "0", "--runner-dir", "none"}, flags...)
		var out, errs bytes.Buffer
		code := Run(context.Background(), args, strings.NewReader(""), &out, &errs)
		return out.String(), errs.String(), code
	}

	out, errs, code := host("--sizes", "c1m2", "--linux-slots", "4")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	if !strings.Contains(out, "note: up to 4 Linux boxes at once on this machine, across every size: every size lent fits 4 times inside 50% of this machine") {
		t.Errorf("no slots note in\n%s", out)
	}

	out, errs, code = host("--json", "--sizes", "c1m2", "--linux-slots", "4")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	var res hostprobe.ShareResult
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatal(err)
	}
	if res.LinuxSlots != 4 || len(res.Sizes) != 1 || res.Sizes[0].Config["linux_slots"] != "4" {
		t.Errorf("JSON: %+v", res)
	}

	if _, errs, code = host("--sizes", "c2m4", "--linux-slots", "4"); code == 0 || !strings.Contains(errs, "size c2m4 does not fit 4 times inside the share") {
		t.Errorf("c2m4 four times in 4 cores: exit %d: %s", code, errs)
	}
	if _, errs, code = host("--linux-slots", "0"); code == 0 || !strings.Contains(errs, "--linux-slots: want 1..") {
		t.Errorf("--linux-slots 0: exit %d: %s", code, errs)
	}
	if out, _, code = host("--json", "--sizes", "c1m2"); code != 0 || strings.Contains(out, "linux_slots") {
		t.Errorf("no flag: exit %d: %s", code, out)
	}
}
