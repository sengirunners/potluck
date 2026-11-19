package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/sengirunners/potluck/hostprobe"
	"github.com/sengirunners/potluck/hostswap"
	"github.com/sengirunners/potluck/kit/tailnet/tailnettest"
)

// A compatibility refusal reaches both command surfaces, runs no download or
// conformance, and leaves the invite usable for a later supported setup.
func TestHostCompatibilityRefusalLeavesInviteUnspent(t *testing.T) {
	oldDetect, oldRunner := detectHost, shareRunner
	t.Cleanup(func() { detectHost, shareRunner = oldDetect, oldRunner })
	const blocker = "Softnet at /checked/softnet cannot parse Potluck's required host DNS-only and SMTP-blocking isolation rules"
	detectHost = func(context.Context, string) hostprobe.Report {
		return hostprobe.Report{OS: "darwin", MacOS: "15.6", Arch: "arm64", Cores: 8, RAMMB: 16384, Swap: hostswap.Encrypted,
			Layers: []hostprobe.Layer{{Name: "tart", Supported: true, Problems: []string{blocker}}}}
	}
	shareRunner = runnerFunc(func(context.Context, string, ...string) (hostprobe.Result, error) {
		t.Fatal("incompatible host ran a download or conformance command")
		return hostprobe.Result{}, nil
	})
	net := tailnettest.New()
	owner := startMachine(t, net, "owner", "owner@example.com")
	lender := startMachine(t, net, "lender", "lender@example.com")
	owner.ok(t, "create", "friends")
	invite := hostCodeRe.FindStringSubmatch(owner.ok(t, "invite", "lender", "--host"))
	if invite == nil {
		t.Fatal("no host invite")
	}
	for _, args := range [][]string{{"doctor"}, {"host", invite[1], "--no-sudo"}} {
		out, errs, code := lender.run(t, args...)
		if code == 0 || !strings.Contains(out+errs, blocker) {
			t.Fatalf("%v: exit %d\n%s%s", args, code, out, errs)
		}
	}
	if out := lender.ok(t, "join", invite[1]); !strings.Contains(out, "joined pool friends") {
		t.Fatalf("invite was spent by refused host: %s", out)
	}
}
