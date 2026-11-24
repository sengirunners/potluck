package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/sengirunners/potluck/hostprobe"
	"github.com/sengirunners/potluck/softnet"
)

// A Mac whose Softnet was fine when the window probed it and needs a root
// step when the Set up button runs host: the answer is the managed install's
// root step, or a refusal that names it, and nothing is started either way.
func TestHostDetachHandsBackSoftnetsRootStep(t *testing.T) {
	m := onePoolMachine(t, refPin)
	useSwitch(t)
	oldSpawn, oldRoot := hostSpawn, hostRoot
	t.Cleanup(func() { hostSpawn, hostRoot = oldSpawn, oldRoot })
	hostRoot.tty = func() bool { t.Error("asked for a terminal"); return false }
	spawned := 0
	hostSpawn = func([]string, string, *os.File, *os.File) (int, error) { spawned++; return 4242, nil }

	sm := newSoftnetMac(t, "15.1")
	detectHost = func(context.Context, string) hostprobe.Report {
		return hostprobe.Report{OS: "darwin", Arch: "arm64", MacOS: sm.macOS, Cores: 8, RAMMB: 16 << 10, FreeDiskGB: 500, TartFreeGB: 500,
			Layers: []hostprobe.Layer{{Name: "tart", Isolation: "vm", Supported: true, CLI: "/fake/tart", SoftnetAlone: true,
				Problems: []string{"Softnet is at /fake/softnet but is not setuid root"}, RootSteps: []string{"chown root '/fake/softnet'", "chmod u+s,go-w '/fake/softnet'"}}}}
	}
	body, err := os.ReadFile(sm.file)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write(body) }))
	t.Cleanup(srv.Close)
	softnet.Builds[0].URL = srv.URL
	args := []string{"--json", "host", "trial", "--yes", "--no-sudo", "--detach", "--reclaim-idle-s", "0"}

	// A validated managed build: its install's root step.
	softnet.Builds[0].Validated = true
	out, errs, code := m.run(t, args...)
	var ans map[string]adminStep
	if code != 0 || json.Unmarshal([]byte(out), &ans) != nil || ans["admin_step"].Why == "" || !slices.Contains(ans["admin_step"].Argv, "--admin-step") || spawned != 0 {
		t.Fatalf("exit %d, spawned %d\n%s%s", code, spawned, out, errs)
	}
	if len(sm.sudo) != 0 {
		t.Errorf("sudo ran: %v", sm.sudo)
	}
	// The lock is free again.
	if out, errs, code := m.run(t, args...); code != 0 {
		t.Fatalf("again: exit %d\n%s%s", code, out, errs)
	}
	// A candidate, which the app never installs, and a Mac with no build:
	// refused with the root line.
	softnet.Builds[0].Validated = false
	for _, macOS := range []string{"15.1", "13.6"} {
		sm.macOS = macOS
		out, errs, code := m.run(t, args...)
		if code == 0 || spawned != 0 || !strings.Contains(out+errs, "chown root ") || strings.Contains(out, "admin_step") {
			t.Errorf("macOS %s: exit %d, spawned %d\n%s%s", macOS, code, spawned, out, errs)
		}
	}
	// No root step and no room for the image: refused before any password is
	// asked for.
	softnet.Builds[0].Validated = true
	sm.macOS = "15.1"
	fine := detectHost
	detectHost = func(ctx context.Context, d string) hostprobe.Report {
		r := fine(ctx, d)
		r.Layers[0].RootSteps, r.TartFreeGB, r.TartDir = nil, 1, "/x"
		return r
	}
	if out, errs, code := m.run(t, args...); code == 0 || !strings.Contains(out+errs, "not enough disk") || strings.Contains(out, "admin_step") {
		t.Errorf("low disk: exit %d\n%s%s", code, out, errs)
	}
	detectHost = func(ctx context.Context, d string) hostprobe.Report { // missing, so no root step, and room
		r := fine(ctx, d)
		r.Layers[0].RootSteps = nil
		return r
	}
	if out, errs, code := m.run(t, args...); code != 0 || !strings.Contains(out, "admin_step") {
		t.Errorf("Softnet missing: exit %d\n%s%s", code, out, errs)
	}
	detectHost = fine
	// Without --json a person reads the root line.
	softnet.Builds[0].Validated = true
	sm.macOS = "15.1"
	if out, errs, code := m.run(t, args[1:]...); code == 0 || spawned != 0 || !strings.Contains(out+errs, "chown root") {
		t.Errorf("no --json: exit %d\n%s%s", code, out, errs)
	}
}

// host status --probe adds the Mac's own checks, read while the daemon is
// asked.
func TestHostStatusProbeAddsTheMac(t *testing.T) {
	m := onePoolMachine(t, refPin)
	useSwitch(t)
	oldProbe, oldOS := probeMac, setupGOOS
	t.Cleanup(func() { probeMac, setupGOOS = oldProbe, oldOS })
	setupGOOS = "darwin"
	probeMac = func(context.Context) hostprobe.MacParts {
		return hostprobe.MacParts{Report: hostprobe.Report{Arch: "amd64"}}
	}
	if out := m.ok(t, "--json", "host", "status", "--probe"); !strings.Contains(out, "Intel processor") {
		t.Errorf("no probe in\n%s", out)
	}
}
