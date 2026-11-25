package menubar

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/hostprobe"
	"github.com/sengirunners/potluck/hostsetup"
	"github.com/sengirunners/potluck/protocol"
)

var update = flag.Bool("update", false, "rewrite menubar/testdata/status-*.json from Build")

// The menu lists the setup, jobs and uninstall windows, each with the
// arguments the app runs for it.
func TestBuildListsTheWindows(t *testing.T) {
	m := Build(Facts{Cores: 4}).Menu
	got := map[string]string{}
	for _, w := range m.Windows {
		got[w.ID] = strings.Join(w.Args, " ")
	}
	if len(m.Windows) != 3 || got["setup"] != "--json host status --probe" || got["jobs"] != "--json host jobs" || got["uninstall"] != "--json uninstall --dry-run" {
		t.Errorf("windows %+v", m.Windows)
	}
	if m.Windows[0].Label != "Set up and check…" {
		t.Errorf("label %q", m.Windows[0].Label)
	}
	if m.Windows[2].Label != "Uninstall Potluck…" {
		t.Errorf("label %q", m.Windows[2].Label)
	}
}

// statusFixtures are the host status answers the Swift window is checked
// against (macos/PotluckMenu/test.sh decodes each with the app's own
// structs). They are Build's output for fixed facts; `go test ./menubar
// -run TestStatusFixtures -update` rewrites them.
func statusFixtures() map[string]Facts {
	at := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	now = func() time.Time { return at }
	base := Facts{Cores: 8, Loc: time.UTC, Host: &protocol.HostStatus{Mode: "auto", State: "accepting"},
		Start: "potluck daemon --detach", Share: "potluck host"}
	base.Setup = SetupFacts{Version: "v1.2.3", InstallMenu: "install-menu", Upgrade: "potluck upgrade enable", Doctor: "potluck doctor",
		Menu: &MenuApp{Path: "/Users/b/Applications/Potluck Menu.app", Seen: at.Add(-5 * time.Second)}, Updates: &Updates{}}
	stage := func(name, result, detail string) hostsetup.Stage {
		return hostsetup.Stage{Name: name, Result: result, Detail: detail, At: "2026-11-19T11:58:00Z"}
	}
	run := func(ended string, stages ...hostsetup.Stage) *hostsetup.Run {
		return &hostsetup.Run{Started: "2026-11-19T11:57:00Z", Ended: ended, Again: "potluck host", Stages: stages}
	}

	never := base
	never.Daemon = "no daemon record"

	running := base
	running.Setup.Run = run("", stage(hostsetup.Preflight, hostsetup.Succeeded, ""), stage(hostsetup.Images, hostsetup.Running, "image 1 of 2: pull a"))
	running.Setup.RunLive = true
	running.Background = &Background{State: "running", Last: "images: image 1 of 2: pull a"}

	blocked := base
	blocked.Setup.Run = run("2026-11-19T11:59:00Z", stage(hostsetup.Preflight, hostsetup.Succeeded, ""), stage(hostsetup.Images, hostsetup.Succeeded, ""),
		stage(hostsetup.Validation, hostsetup.Blocked, "conformance failed at boot"))
	blocked.Background = &Background{State: "ended", Last: "validation: conformance failed at boot"}

	join := base // the join waits on a tailnet login
	join.Setup.Run = run("", stage(hostsetup.Preflight, hostsetup.Succeeded, ""), stage(hostsetup.Images, hostsetup.Succeeded, ""),
		stage(hostsetup.Validation, hostsetup.Succeeded, ""), stage(hostsetup.Join, hostsetup.Running, "waiting for a tailnet login"))
	join.Setup.Run.Stages[3].OpenURL = "https://login.example/a/1"
	join.Setup.Run.AgainArgs, join.Setup.Run.NeedsInvite = []string{"host"}, true
	join.Setup.RunLive = true
	join.Background = &Background{State: "running", Last: "join: waiting for a tailnet login"}

	lending := base
	var every []hostsetup.Stage // every stage, so a stage added to hostsetup.Order shows up in the Swift check
	for _, n := range hostsetup.Order {
		every = append(every, stage(n, hostsetup.Succeeded, ""))
	}
	lending.Setup.Run = run("2026-11-19T11:59:00Z", every...)
	lending.Targets = []computeapi.RunTargetView{box("c2", "", "macos", 2, 2, []string{"a"}, nil)}

	// A Mac that has nothing yet, as the app's setup window asks before
	// Potluck is installed (--probe, the app's release): Potluck to
	// install, no Tart, and on macOS 15 no Softnet the app may install.
	fresh := never
	fresh.Setup.Menu = &MenuApp{Path: "/Applications/Potluck.app", Seen: at.Add(-2 * time.Second)}
	fresh.Setup.SoftnetInstall = "potluck softnet install"
	fresh.Setup.TartInstall = "potluck tart install"
	fresh.Setup.Exe, fresh.Setup.InstallDir = "/Applications/Potluck.app/Contents/Resources/bin/potluck", "/Users/b/.local/bin"
	fresh.Setup.BundledRelease = "/Applications/Potluck.app/Contents/Resources/release"
	fresh.Setup.InstallBundled = "potluck upgrade --release /Applications/Potluck.app/Contents/Resources/release --dir /Users/b/.local/bin"
	fresh.Setup.Mac = &hostprobe.MacParts{
		Report: hostprobe.Report{OS: "darwin", Arch: "arm64", MacOS: "15.1", TartDir: "/Users/b/.tart", TartFreeGB: 180},
		HV:     true,
		Tart:   hostprobe.Layer{Name: "tart", Problems: []string{"tart is not installed: download tart.tar.gz from " + hostprobe.TartReleases + ", unpack it and drag tart.app into Applications"}},
		Softnet: hostprobe.Layer{Name: "tart", Problems: []string{"Softnet is not installed: isolated Tart boxes need a helper that enforces Potluck's protocol and port rules; " +
			"upstream Softnet 0.25.0 and newer need macOS 26, and Potluck's macOS 15 build is a candidate not yet validated on a real macOS 15 Mac"}},
		NeedGB: 60,
	}

	// The same on macOS 26, which has a Softnet to install.
	fresh26 := fresh
	mac26 := *fresh.Setup.Mac
	mac26.Report.MacOS, mac26.Softnet = "26.1", hostprobe.Layer{Name: "tart", Problems: []string{"Softnet is not installed"}}
	fresh26.Setup.Mac = &mac26

	// Potluck and Tart are there and macOS 15 has no network helper.
	noHelper := base
	noHelper.Setup.SoftnetInstall = "potluck softnet install"
	noHelper.Setup.Mac = &hostprobe.MacParts{Report: fresh.Setup.Mac.Report, HV: true, NeedGB: 60, Softnet: fresh.Setup.Mac.Softnet,
		Tart: hostprobe.Layer{Name: "tart", CLI: "/Applications/tart.app/Contents/MacOS/tart", Version: "2.30.0"}}

	// Everything is there: the invite is next.
	ready := base
	fine := fineMac()
	ready.Setup.Mac = &fine

	return map[string]Facts{
		"status-probe-fresh-mac-26.json": fresh26,
		"status-probe-no-helper.json":    noHelper,
		"status-probe-ready.json":        ready,
		"status-probe-fresh-mac.json":    fresh,
		"status-never-set-up.json":       never,
		"status-setup-running.json":      running,
		"status-blocked-validation.json": blocked,
		"status-join-sign-in.json":       join,
		"status-lending.json":            lending,
	}
}

func TestStatusFixtures(t *testing.T) {
	t.Cleanup(func() { now = time.Now })
	fixtures := statusFixtures()
	if have, err := filepath.Glob("testdata/status-*.json"); err == nil && !*update {
		for _, p := range have { // a fixture nothing writes any more would keep passing the Swift check
			if _, ok := fixtures[filepath.Base(p)]; !ok {
				t.Errorf("%s is not made by statusFixtures: delete it", p)
			}
		}
	}
	for name, f := range fixtures {
		got, err := json.MarshalIndent(Build(f), "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, '\n')
		path := filepath.Join("testdata", name)
		if *update {
			if err := os.MkdirAll("testdata", 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, got, 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		want, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("%v (go test ./menubar -run TestStatusFixtures -update writes it)", err)
		}
		if string(want) != string(got) {
			t.Errorf("%s is out of date: go test ./menubar -run TestStatusFixtures -update", name)
		}
	}
}
