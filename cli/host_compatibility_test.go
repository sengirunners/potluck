package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sengirunners/potluck/hostprobe"
	"github.com/sengirunners/potluck/hostsetup"
	"github.com/sengirunners/potluck/hostswap"
	"github.com/sengirunners/potluck/kit/tailnet/tailnettest"
	"github.com/sengirunners/potluck/menubar"
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
	// The run's record says preflight blocked it, never holds the invite,
	// and the menu and the terminal say the same.
	r, err := hostsetup.Read(lender.home)
	if err != nil || r == nil || r.Blocked() == nil || r.Blocked().Name != hostsetup.Preflight || !strings.Contains(r.Blocked().Detail, blocker) ||
		r.Again != "potluck host '<invite code>' --no-sudo=true" {
		t.Fatalf("setup record: %+v %v", r, err)
	}
	if raw, _ := os.ReadFile(filepath.Join(lender.home, hostsetup.FileName)); strings.Contains(string(raw), invite[1]) {
		t.Fatalf("the setup record holds the invite code:\n%s", raw)
	}
	var st menubar.Status
	if err := json.Unmarshal([]byte(lender.ok(t, "--json", "host", "status")), &st); err != nil {
		t.Fatal(err)
	}
	text := lender.ok(t, "host", "status")
	if st.Menu.Title != "Setup stopped while checking this Mac" || !strings.HasPrefix(text, st.Menu.Title+"\n") ||
		st.Setup.Next != "fix what the preflight stage says, then run again: potluck host '<invite code>' --no-sudo=true" || !strings.Contains(text, "\nnext: "+st.Setup.Next) {
		t.Fatalf("host status: %+v\n%s", st.Menu, text)
	}
	if out := lender.ok(t, "join", invite[1]); !strings.Contains(out, "joined pool friends") {
		t.Fatalf("invite was spent by refused host: %s", out)
	}
}

// A flag this machine never accepts (--docker on a Mac's tart layer): the
// next step is the command without it, never the refused command again.
func TestHostBlockedByAFlagDropsIt(t *testing.T) {
	m := soloMachine(t, "")
	detectHost = func(context.Context, string) hostprobe.Report {
		return hostprobe.Report{OS: "darwin", MacOS: "26.0", Arch: "arm64", Cores: 8, RAMMB: 16 << 10, FreeDiskGB: 500, Swap: hostswap.Encrypted,
			Layers: []hostprobe.Layer{{Name: "tart", Available: true, Supported: true, CLI: "tart"}}}
	}
	shareRunner = runnerFunc(func(context.Context, string, ...string) (hostprobe.Result, error) {
		t.Error("a refused flag ran a download or conformance command")
		return hostprobe.Result{}, nil
	})
	m.ok(t, "create", "trial")
	out, errs, code := m.run(t, "host", "trial", "--docker", "--linux-slots", "2", "--no-sudo")
	if code == 0 || !strings.Contains(out+errs, "--docker is for the oci layer") ||
		!strings.Contains(errs, "next: run again without --docker=true (the layer this machine shares on does not take --docker): potluck host trial --linux-slots 2 --no-sudo=true") {
		t.Fatalf("exit %d\n%s%s", code, out, errs)
	}
	r, err := hostsetup.Read(m.home)
	if err != nil || r.Blocked() == nil || r.Blocked().Flag != "--docker" || r.Again != "potluck host trial --linux-slots 2 --no-sudo=true" {
		t.Fatalf("setup record: %+v %v", r, err)
	}
	var st menubar.Status
	if err := json.Unmarshal([]byte(m.ok(t, "--json", "host", "status")), &st); err != nil {
		t.Fatal(err)
	}
	if want := "run again without --docker=true (the layer this machine shares on does not take --docker): potluck host trial --linux-slots 2 --no-sudo=true"; st.Setup.Next != want {
		t.Fatalf("next %q, want %q", st.Setup.Next, want)
	}
}

// The old spelling (host share -P <pool>) records the command that runs a
// setup again as host's: the pool its argument, not --pool beside it, and
// again_args say the same command as again.
func TestHostShareOldSpellingAgainArgs(t *testing.T) {
	m := soloMachine(t, "")
	detectHost = func(context.Context, string) hostprobe.Report {
		return hostprobe.Report{OS: "darwin", MacOS: "26.0", Arch: "arm64", Cores: 8, RAMMB: 16 << 10, FreeDiskGB: 500, Swap: hostswap.Encrypted,
			Layers: []hostprobe.Layer{{Name: "tart", Available: true, Supported: true, CLI: "tart"}}}
	}
	shareRunner = runnerFunc(func(context.Context, string, ...string) (hostprobe.Result, error) {
		t.Error("a refused flag ran a download or conformance command")
		return hostprobe.Result{}, nil
	})
	m.ok(t, "create", "trial")
	if _, _, code := m.run(t, "host", "share", "-P", "trial", "--docker", "--linux-slots", "2", "--no-sudo"); code == 0 {
		t.Fatal("--docker on tart went through")
	}
	r, err := hostsetup.Read(m.home)
	if want := []string{"host", "trial", "--linux-slots=2", "--no-sudo=true"}; err != nil || r == nil || !slices.Equal(r.AgainArgs, want) || r.NeedsInvite ||
		r.Again != "potluck host trial --linux-slots 2 --no-sudo=true" {
		t.Fatalf("setup record: %+v %v", r, err)
	}
	// Run as given, it is host's own command again: blocked no more by --docker.
	if _, errs, _ := m.run(t, r.AgainArgs...); strings.Contains(errs, "--docker") || strings.Contains(errs, "unknown") || strings.Contains(errs, "accepts") {
		t.Fatalf("again_args did not run as host:\n%s", errs)
	}
}
