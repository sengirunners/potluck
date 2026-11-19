package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/sengirunners/potluck/hostprobe"
)

// try on a machine in no pool makes the trial pool, shares the machine with
// the host command (no Tailscale), runs uname -a in a sandbox and removes it;
// run again it skips the pool and the share.
func TestTryOneMachine(t *testing.T) {
	m := soloMachine(t, "")
	stdout, stderr, code := m.run(t, "try", "--image", refPin)
	if code != 0 {
		t.Fatalf("try: exit %d\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	for _, want := range []string{
		"pool trial is ready",
		"running: potluck host trial --reclaim-idle-s 0 --image " + refPin + "\n",
		"shared with pool trial",
		" ready ",
		"$ potluck sandbox exec solo-",
		"removed solo-",
		"next:\n",
		"potluck invite <name>",
		"potluck host stop",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("try: no %q in\n%s\nstderr:\n%s", want, stdout, stderr)
		}
	}
	if strings.Contains(stdout, "macOS guests") || strings.Contains(stdout, "--with") {
		t.Errorf("a Linux machine was offered a macOS step or a named image:\n%s", stdout)
	}
	if o := m.ok(t, "sandbox", "ls", "-P", "trial"); strings.Contains(o, " ready ") {
		t.Errorf("try left its sandbox:\n%s", o)
	}

	again := m.ok(t, "try", "--image", refPin)
	if strings.Contains(again, "pool trial is ready") || strings.Contains(again, "running: potluck host") {
		t.Errorf("a second try made the pool or shared again:\n%s", again)
	}
	if !strings.Contains(again, "removed solo-") {
		t.Errorf("a second try made no sandbox:\n%s", again)
	}
}

// try prints words: --json is a usage error, before anything is made.
func TestTryRefusesJSON(t *testing.T) {
	m := soloMachine(t, "")
	_, stderr, code := m.run(t, "try", "--json")
	if code != 2 || !strings.Contains(stderr, "try prints words") {
		t.Fatalf("try --json: exit %d, stderr:\n%s", code, stderr)
	}
	if o := m.ok(t, "remotes"); strings.Contains(o, "trial") {
		t.Errorf("try --json made a pool:\n%s", o)
	}
}

// On a Mac whose layer lends macOS by default, try shares the machine with
// Potluck's small Linux image, the shipped table's published small-linux pin
// (hostprobe.TestChooseFirstImage covers the choice while it is unpublished).
func TestTryMacSmallImage(t *testing.T) {
	m := soloMachine(t, "")
	detectHost = func(context.Context, string) hostprobe.Report {
		return hostprobe.Report{OS: "darwin", MacOS: "26.0", Arch: "arm64", Cores: 8, RAMMB: 16 << 10, FreeDiskGB: 200,
			Layers: []hostprobe.Layer{{Name: "tart", Available: true, Supported: true, CLI: "tart"}}}
	}
	// The fixture's driver lends none of the image's boxes, so the run fails
	// after host: only what try chose and printed before it is checked.
	stdout, stderr, _ := m.run(t, "try")
	for _, want := range []string{
		"sharing this machine: Potluck's small Linux image (about 1 GB to pull), not macOS (about 27 GB to pull)\n",
		"running: potluck host trial --reclaim-idle-s 0 --with small-linux --root=false\n",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("try on a Mac: no %q in\n%s\nstderr:\n%s", want, stdout, stderr)
		}
	}
	if strings.Contains(stdout, "not published yet") {
		t.Errorf("try on a Mac called the published image unpublished:\n%s", stdout)
	}
}
