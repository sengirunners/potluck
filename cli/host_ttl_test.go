package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/sengirunners/potluck/hostprobe"
)

// potluck host --ttl-max-s N: a plan binds ttl_max_s=N on every size and
// says so; a value out of range is refused; without the flag nothing is
// bound.
func TestHostTTLMaxSFlag(t *testing.T) {
	oldRunner, oldDetect, oldDriver := shareRunner, detectHost, shareDriver
	t.Cleanup(func() { shareRunner, detectHost, shareDriver = oldRunner, oldDetect, oldDriver })
	shareRunner = runnerFunc(func(context.Context, string, ...string) (hostprobe.Result, error) {
		t.Fatal("dry run executed a command")
		return hostprobe.Result{}, nil
	})
	detectHost = func(_ context.Context, dir string) hostprobe.Report {
		return hostprobe.Report{OS: "linux", Arch: "amd64", Cores: 8, RAMMB: 16 << 10, FreeDiskGB: 100, KVM: true, StateDir: dir,
			Layers: []hostprobe.Layer{{Name: "oci", Available: true, Supported: true, CLI: "docker", Runtime: "runsc", Runtimes: []string{"runsc"}}}}
	}
	shareDriver = "ref"
	t.Setenv(EnvHome, t.TempDir())
	host := func(flags ...string) (string, string, int) {
		args := append([]string{"host", "--dry-run", "--image", refPin, "--name-prefix", "lender", "--no-sudo", "--reclaim-idle-s", "0", "--runner-dir", "none", "--sizes", "c1m2"}, flags...)
		var out, errs bytes.Buffer
		code := Run(context.Background(), args, strings.NewReader(""), &out, &errs)
		return out.String(), errs.String(), code
	}

	out, errs, code := host("--ttl-max-s", "432300")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	if !strings.Contains(out, "note: boxes live at most 432300 s (5 days 0 h) on every size") {
		t.Errorf("no lifetime note in\n%s", out)
	}

	out, errs, code = host("--json", "--ttl-max-s", "432300")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	var res hostprobe.ShareResult
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatal(err)
	}
	if res.TTLMaxS != 432300 || len(res.Sizes) != 1 || res.Sizes[0].TTLMaxS != 432300 || res.Sizes[0].Config["ttl_max_s"] != "" {
		t.Errorf("JSON: %+v", res)
	}

	for _, v := range []string{"0", "899", "435601"} {
		if _, errs, code = host("--ttl-max-s", v); code == 0 || !strings.Contains(errs, "--ttl-max-s: want 900..435600") {
			t.Errorf("--ttl-max-s %s: exit %d: %s", v, code, errs)
		}
	}
	if out, _, code = host("--json"); code != 0 || strings.Contains(out, "ttl_max_s") {
		t.Errorf("no flag: exit %d: %s", code, out)
	}
}
