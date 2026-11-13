package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sengirunners/potluck/hostprobe"
)

func TestHostShareStateDir(t *testing.T) {
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
	home := t.TempDir()
	t.Setenv(EnvHome, home)
	explicit := filepath.Join(t.TempDir(), "driver records")
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	relative, err := filepath.Rel(cwd, explicit)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, flag, want string }{
		{"explicit", explicit, explicit},
		{"relative", relative, explicit},
		{"default", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := []string{"--json", "host", "share", "--dry-run", "--image", refPin, "--sizes", "c1m2,c2m4", "--name-prefix", "lender", "--no-sudo", "--reclaim-idle-s", "0"}
			if tc.flag != "" {
				args = append(args, "--state-dir", tc.flag)
			}
			var out, stderr bytes.Buffer
			if code := Run(context.Background(), args, strings.NewReader(""), &out, &stderr); code != 0 {
				t.Fatalf("host share: exit %d: %s", code, &stderr)
			}
			var result hostprobe.ShareResult
			if err := json.Unmarshal(out.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if len(result.Sizes) != 2 {
				t.Fatalf("sizes = %d, want 2", len(result.Sizes))
			}
			for _, size := range result.Sizes {
				if got := size.Config["state_dir"]; got != tc.want {
					t.Errorf("%s binding state_dir = %q, want %q", size.Binding, got, tc.want)
				}
				if tc.want != "" && !slices.Contains(size.Conformance, "state_dir="+tc.want) {
					t.Errorf("%s conformance lacks state_dir=%s: %v", size.Binding, tc.want, size.Conformance)
				}
			}
		})
	}
}
