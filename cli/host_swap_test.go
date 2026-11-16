package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/sengirunners/potluck/hostprobe"
	"github.com/sengirunners/potluck/hostswap"
	"github.com/sengirunners/potluck/kit/tailnet/tailnettest"
)

// potluck doctor fails on swap that the pool would refuse box secrets for,
// and names the fix; safe swap passes, with --json too.
func TestHostDoctorFailsOnUnsafeSwap(t *testing.T) {
	oldDetect := detectHost
	t.Cleanup(func() { detectHost = oldDetect })
	m := startMachine(t, tailnettest.New(), "lender", "carl@example.com")
	for _, c := range []struct {
		swap string
		ok   bool
	}{
		{hostswap.PlainDisk, false}, {hostswap.Unknown, false}, {"", false},
		{hostswap.Off, true}, {hostswap.Zram, true}, {hostswap.Encrypted, true},
	} {
		detectHost = func(context.Context, string) hostprobe.Report {
			r := hostprobe.Report{OS: "linux", Arch: "amd64", Cores: 8, RAMMB: 16 << 10, FreeDiskGB: 100, KVM: true, Swap: c.swap,
				Layers: []hostprobe.Layer{{Name: "oci", Available: true, Supported: true, CLI: "docker", Runtime: "runsc", Runtimes: []string{"runsc"}}}}
			if c.swap == hostswap.PlainDisk {
				r.SwapNote = "swap on /dev/sda2 is not encrypted; " + hostswap.Fix
			}
			return r
		}
		for _, json := range []bool{false, true} {
			args := []string{"doctor"}
			if json {
				args = append(args, "--json")
			}
			out, errs, code := m.run(t, args...)
			if (code == 0) != c.ok {
				t.Errorf("swap %q json=%t: exit %d, want ok=%t\n%s%s", c.swap, json, code, c.ok, out, errs)
			}
			if !c.ok && !json && !strings.Contains(out+errs, "zram") { // the note (stdout) or the failure names it
				t.Errorf("swap %q: the failure does not name the fix: %s", c.swap, errs)
			}
			if c.ok && !json && !strings.Contains(out, "next: potluck host") {
				t.Errorf("swap %q: no next step: %s", c.swap, out)
			}
		}
	}
}
