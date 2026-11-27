package menubar

import (
	"slices"
	"strings"
	"testing"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/hostprobe"
	"github.com/sengirunners/potluck/hostsetup"
	"github.com/sengirunners/potluck/protocol"
	"github.com/sengirunners/potluck/softnet"
)

// fineMac is a Mac with everything a host run needs: macOS 26.
func fineMac() hostprobe.MacParts {
	return hostprobe.MacParts{
		Report:  hostprobe.Report{OS: "darwin", Arch: "arm64", MacOS: "26.1", TartDir: "/Users/b/.tart", TartFreeGB: 200},
		HV:      true,
		Tart:    hostprobe.Layer{Name: "tart", CLI: "/opt/homebrew/bin/tart", Version: "2.30.0"},
		Softnet: hostprobe.Layer{Name: "tart", SoftnetPath: "/opt/homebrew/bin/softnet", SoftnetVersion: "0.25.0"},
		NeedGB:  60,
	}
}

// host status --probe's checks, each from a fact: what each one says, and
// the step the window offers first.
func TestProbeChecks(t *testing.T) {
	// A validated managed build for macOS 14, so a Mac the managed install
	// covers can be tested (today's only build is a macOS 15 candidate).
	old := softnet.Builds
	t.Cleanup(func() { softnet.Builds = old })
	softnet.Builds = append(slices.Clone(old), softnet.Build{ID: "test-14", Arch: "arm64", MinMacOS: 14, MaxMacOS: 14, Validated: true})

	base := Facts{Cores: 8, Host: &protocol.HostStatus{Mode: "auto", State: "accepting"}, Start: "potluck daemon --detach", Share: "potluck host"}
	base.Setup = SetupFacts{Version: "v1.2.3", SoftnetInstall: "potluck softnet install", SoftnetUninstall: "potluck softnet uninstall", Upgrade: "potluck upgrade enable", InstallMenu: "install-menu",
		Menu: &MenuApp{}, Updates: &Updates{}}
	notInstalled := []string{"Softnet is not installed: isolated Tart boxes need a helper"}

	type want struct {
		checks map[string]string // id -> state
		absent []string
		nextID string
		next   string
		doArgs string // the next check's do
		doURL  string
		detail string // in the next check's detail
	}
	cases := []struct {
		name string
		edit func(m *hostprobe.MacParts, s *SetupFacts)
		want want
	}{
		{"Intel", func(m *hostprobe.MacParts, _ *SetupFacts) { m.Report.Arch = "amd64" },
			want{checks: map[string]string{"mac": Blocked, "potluck": Done}, absent: []string{"tart", "softnet", "disk"}, nextID: "mac", detail: "Intel"}},
		{"macOS 12", func(m *hostprobe.MacParts, _ *SetupFacts) { m.Report.MacOS = "12.7.4" },
			want{checks: map[string]string{"mac": Blocked}, absent: []string{"tart"}, nextID: "mac", detail: "macOS 12.7.4"}},
		{"unknown macOS", func(m *hostprobe.MacParts, _ *SetupFacts) {
			m.Report.MacOS, m.MacOSUnread = "", "cannot determine macOS product version: no answer in time"
		}, want{checks: map[string]string{"mac": Unknown, "tart": Done, "softnet": Done, "disk": Done}, nextID: "setup"}},
		{"unknown macOS, no Softnet, an unsound managed copy", func(m *hostprobe.MacParts, _ *SetupFacts) {
			m.Report.MacOS, m.MacOSUnread = "", "cannot determine macOS product version: no answer in time"
			m.Softnet = hostprobe.Layer{Name: "tart", Problems: notInstalled}
			m.SoftnetStore = &softnet.Status{Current: &softnet.Installed{ID: "x", Problem: "gone"}}
		}, want{checks: map[string]string{"softnet": Unknown}, nextID: "setup"}},
		{"unknown processor", func(m *hostprobe.MacParts, _ *SetupFacts) {
			m.Report.Arch, m.ArchUnread = "unknown", "no answer in time"
		}, want{checks: map[string]string{"mac": Unknown, "softnet": Done}, nextID: "setup"}},
		{"Intel, macOS unread", func(m *hostprobe.MacParts, _ *SetupFacts) {
			m.Report.Arch, m.Report.MacOS, m.MacOSUnread = "amd64", "", "no answer in time"
		}, want{checks: map[string]string{"mac": Blocked}, absent: []string{"tart"}, nextID: "mac", detail: "Intel"}},
		{"Tart did not answer", func(m *hostprobe.MacParts, _ *SetupFacts) {
			m.Tart.CLI, m.Tart.Version, m.Tart.Problems, m.Tart.Unread = "/Applications/tart.app/Contents/MacOS/tart", "", []string{"no answer in time"}, "no answer in time"
		}, want{checks: map[string]string{"tart": Unknown}, nextID: "setup"}},
		{"Softnet did not answer", func(m *hostprobe.MacParts, _ *SetupFacts) {
			m.Softnet.Problems, m.Softnet.Unread = []string{"no answer in time"}, "no answer in time"
		}, want{checks: map[string]string{"softnet": Unknown}, nextID: "setup"}},
		{"low disk, Tart's images unlisted", func(m *hostprobe.MacParts, _ *SetupFacts) {
			m.Report.TartFreeGB, m.ImagesUnread = 20, "no answer in time"
		}, want{checks: map[string]string{"disk": Unknown}, nextID: "setup"}},
		{"no virtualization", func(m *hostprobe.MacParts, _ *SetupFacts) { m.HV = false },
			want{checks: map[string]string{"mac": Blocked}, absent: []string{"tart"}, nextID: "mac", detail: "virtualization"}},
		{"no Tart", func(m *hostprobe.MacParts, _ *SetupFacts) {
			m.Tart = hostprobe.Layer{Name: "tart", Problems: []string{"tart is not installed: download it"}}
		}, want{checks: map[string]string{"mac": Done, "tart": Todo, "softnet": Done}, nextID: "tart", doURL: hostprobe.TartReleases, next: "download tart.tar.gz from " + hostprobe.TartReleases + ", unpack it and drag tart.app into Applications"}},
		{"a tart.app with no program", func(m *hostprobe.MacParts, _ *SetupFacts) {
			m.Tart, m.Hollow = hostprobe.Layer{Name: "tart", Problems: []string{"/Applications/tart.app has no Contents/MacOS/tart in it"}}, []string{"/Applications/tart.app"}
		}, want{checks: map[string]string{"tart": Todo}, nextID: "tart", doURL: hostprobe.TartReleases, detail: "has no Contents/MacOS/tart"}},
		{"Tart in ~/Applications", func(m *hostprobe.MacParts, _ *SetupFacts) {
			m.Tart.CLI = "/Users/b/Applications/tart.app/Contents/MacOS/tart"
		}, want{checks: map[string]string{"tart": Done}, nextID: "setup"}},
		{"no Tart, and Potluck can install it", func(m *hostprobe.MacParts, s *SetupFacts) {
			m.Tart, s.TartInstall = hostprobe.Layer{Name: "tart", Problems: []string{"tart is not installed"}}, "potluck tart install"
		}, want{checks: map[string]string{"tart": Todo}, nextID: "tart", next: "potluck tart install", doArgs: "--json tart install"}},
		{"a tart.app with no program, and Potluck can install Tart", func(m *hostprobe.MacParts, s *SetupFacts) {
			m.Tart, m.Hollow = hostprobe.Layer{Name: "tart", Problems: []string{"/Applications/tart.app has no Contents/MacOS/tart in it"}}, []string{"/Applications/tart.app"}
			s.TartInstall = "potluck tart install"
		}, want{checks: map[string]string{"tart": Todo}, nextID: "tart", doArgs: "--json tart install", detail: "has no Contents/MacOS/tart"}},
		{"Potluck's own older Tart", func(m *hostprobe.MacParts, s *SetupFacts) {
			m.Tart.CLI, m.Tart.Problems = "/Users/b/Applications/tart.app/Contents/MacOS/tart", []string{"lacks required run option"}
			s.TartInstall = "potluck tart install"
		}, want{checks: map[string]string{"tart": Todo}, nextID: "tart", doArgs: "--json tart install", detail: "lacks required run option"}},
		{"a broken tart.app in the person's own Applications", func(m *hostprobe.MacParts, _ *SetupFacts) {
			m.Tart.CLI, m.Tart.Problems = "/Users/b/Applications/tart.app/Contents/MacOS/tart", []string{"cannot run on this host"}
		}, want{checks: map[string]string{"tart": Todo}, nextID: "tart", doURL: hostprobe.TartReleases, next: "download tart.tar.gz from " + hostprobe.TartReleases + ", unpack it and drag tart.app into Applications"}},
		{"an old tart.app", func(m *hostprobe.MacParts, _ *SetupFacts) {
			m.Tart.CLI, m.Tart.Problems = "/Applications/tart.app/Contents/MacOS/tart", []string{"Tart at /Applications/tart.app/Contents/MacOS/tart lacks required run option --net-softnet-block"}
		}, want{checks: map[string]string{"tart": Todo}, nextID: "tart", doURL: hostprobe.TartReleases, detail: "lacks required run option"}},
		{"an old Homebrew Tart", func(m *hostprobe.MacParts, _ *SetupFacts) {
			m.Tart.Problems = []string{"Tart at /opt/homebrew/bin/tart lacks required run option --net-softnet-block"}
		}, want{checks: map[string]string{"tart": Blocked}, nextID: "tart", detail: "brew upgrade tart"}},
		{"no Softnet, a managed build", func(m *hostprobe.MacParts, _ *SetupFacts) {
			m.Report.MacOS, m.Softnet = "14.6", hostprobe.Layer{Name: "tart", Problems: notInstalled}
		}, want{checks: map[string]string{"softnet": Todo}, nextID: "softnet", next: "potluck softnet install", doArgs: "--json softnet install --no-sudo", detail: "not installed"}},
		{"macOS 15, a candidate only", func(m *hostprobe.MacParts, _ *SetupFacts) {
			m.Report.MacOS, m.Softnet = "15.1", hostprobe.Layer{Name: "tart", Problems: notInstalled}
		}, want{checks: map[string]string{"softnet": Blocked}, nextID: "softnet", detail: "not installed"}},
		{"macOS 15, the candidate installed and changed since", func(m *hostprobe.MacParts, _ *SetupFacts) {
			m.Report.MacOS, m.Softnet = "15.1", hostprobe.Layer{Name: "tart", Problems: []string{"the managed Softnet cannot be used: it is not setuid"}}
			m.SoftnetStore = &softnet.Status{Current: &softnet.Installed{ID: "x", Problem: "it is not setuid"}}
		}, want{checks: map[string]string{"softnet": Blocked}, nextID: "softnet", detail: "it is not setuid"}},
		{"macOS 26, no Softnet", func(m *hostprobe.MacParts, _ *SetupFacts) {
			m.Softnet = hostprobe.Layer{Name: "tart", Problems: notInstalled}
		}, want{checks: map[string]string{"softnet": Todo}, nextID: "softnet", doArgs: "--json softnet install --no-sudo"}},
		{"macOS 27, Softnet not setuid", func(m *hostprobe.MacParts, _ *SetupFacts) {
			m.Softnet.Problems, m.Softnet.RootSteps = []string{"not setuid root"}, []string{"chown root /s"}
		}, want{checks: map[string]string{"softnet": Todo}, nextID: "softnet", doArgs: "--json softnet install --no-sudo"}},
		{"macOS 15, Softnet not setuid", func(m *hostprobe.MacParts, _ *SetupFacts) {
			m.Report.MacOS = "15.1"
			m.Softnet.Problems, m.Softnet.RootSteps = []string{"not setuid root"}, []string{"chown root /s"}
		}, want{checks: map[string]string{"softnet": Blocked}, nextID: "softnet", detail: "chown root /s"}},
		{"macOS 13, an old managed Softnet left", func(m *hostprobe.MacParts, _ *SetupFacts) {
			m.Report.MacOS = "13.6"
			m.Softnet.Problems = []string{"the managed Softnet x cannot be used (gone); remove it with `potluck softnet uninstall`"}
			m.SoftnetStore = &softnet.Status{Current: &softnet.Installed{ID: "x", Problem: "gone"}}
		}, want{checks: map[string]string{"softnet": Todo}, nextID: "softnet", next: "potluck softnet uninstall", doArgs: "--json softnet uninstall --no-sudo"}},
		{"low disk while the images are pulled", func(m *hostprobe.MacParts, s *SetupFacts) {
			m.Report.TartFreeGB = 20
			s.Run = &hostsetup.Run{Started: "x", Stages: append(stages(hostsetup.Preflight), hostsetup.Stage{Name: hostsetup.Images, Result: hostsetup.Running})}
			s.RunLive = true
		}, want{checks: map[string]string{"disk": Info, "setup": Running}, nextID: "menu"}},
		{"bundled release, no install directory", func(_ *hostprobe.MacParts, s *SetupFacts) {
			s.Exe, s.BundledRelease = "/Applications/Potluck.app/Contents/Resources/bin/potluck", "/x/release"
		}, want{checks: map[string]string{"potluck": Unknown}, nextID: "setup"}},
		{"macOS 13, no build at all", func(m *hostprobe.MacParts, _ *SetupFacts) {
			m.Report.MacOS, m.Softnet = "13.6", hostprobe.Layer{Name: "tart", Problems: notInstalled}
		}, want{checks: map[string]string{"mac": Done, "softnet": Blocked}, nextID: "softnet"}},
		{"low disk", func(m *hostprobe.MacParts, _ *SetupFacts) { m.Report.TartFreeGB = 20 },
			want{checks: map[string]string{"disk": Blocked}, nextID: "disk", detail: "free up 40 GB more"}},
		{"low disk, Tart has the image", func(m *hostprobe.MacParts, _ *SetupFacts) { m.Report.TartFreeGB, m.NeedGB = 20, 0 },
			want{checks: map[string]string{"disk": Done}, nextID: "setup"}},
		{"low disk by less than 1 GB", func(m *hostprobe.MacParts, _ *SetupFacts) { m.Report.TartFreeGB = 59.7 },
			want{checks: map[string]string{"disk": Blocked}, nextID: "disk", detail: "free up 1 GB more"}},
		{"low disk, a run in preflight", func(m *hostprobe.MacParts, s *SetupFacts) {
			m.Report.TartFreeGB = 20
			s.Run = &hostsetup.Run{Started: "x", Stages: []hostsetup.Stage{{Name: hostsetup.Preflight, Result: hostsetup.Running}}}
			s.RunLive = true
		}, want{checks: map[string]string{"disk": Info, "setup": Running}, nextID: "menu"}},
		{"low disk, the preflight refused", func(m *hostprobe.MacParts, s *SetupFacts) {
			m.Report.TartFreeGB = 20
			s.Run = &hostsetup.Run{Started: "x", Ended: "y", Stages: []hostsetup.Stage{{Name: hostsetup.Preflight, Result: hostsetup.Blocked, Detail: "no room"}}}
		}, want{checks: map[string]string{"disk": Blocked}, nextID: "disk"}},
		{"low disk, waiting for the join's sign-in", func(m *hostprobe.MacParts, s *SetupFacts) {
			m.Report.TartFreeGB = 20
			s.Run = &hostsetup.Run{Started: "x", Stages: append(stages(hostsetup.Preflight, hostsetup.Images, hostsetup.Validation), hostsetup.Stage{Name: hostsetup.Join, Result: hostsetup.Running, OpenURL: "https://login.tailscale.com/a/1"})}
			s.RunLive = true
		}, want{checks: map[string]string{"disk": Info}, nextID: "setup", doURL: "https://login.tailscale.com/a/1"}},
		{"low disk, the last run stopped at validation", func(m *hostprobe.MacParts, s *SetupFacts) {
			m.Report.TartFreeGB = 20
			s.Run = &hostsetup.Run{Started: "x", Ended: "y", Stages: append(stages(hostsetup.Preflight, hostsetup.Images), hostsetup.Stage{Name: hostsetup.Validation, Result: hostsetup.Blocked, Detail: "no"})}
		}, want{checks: map[string]string{"disk": Info}, nextID: "setup"}},
		{"low disk, the pull was cut off", func(m *hostprobe.MacParts, s *SetupFacts) {
			m.Report.TartFreeGB = 5
			s.Run = &hostsetup.Run{Started: "x", Stages: append(stages(hostsetup.Preflight), hostsetup.Stage{Name: hostsetup.Images, Result: hostsetup.Running})}
		}, want{checks: map[string]string{"disk": Info}, nextID: "setup"}},
		{"virtualization unread", func(m *hostprobe.MacParts, _ *SetupFacts) { m.HV, m.HVUnread = false, "timed out" },
			want{checks: map[string]string{"mac": Unknown, "tart": Done, "disk": Done}, nextID: "setup"}},
		{"disk unread", func(m *hostprobe.MacParts, _ *SetupFacts) { m.Report.TartDiskUnread = "statfs: no such file" },
			want{checks: map[string]string{"disk": Unknown}, nextID: "setup"}},
		{"unexplained problem", func(m *hostprobe.MacParts, _ *SetupFacts) {
			m.Rest.Problems = []string{"something Potluck reads went wrong"}
		},
			want{checks: map[string]string{"other": Blocked}, nextID: "other", detail: "something Potluck reads went wrong"}},
		{"bundled release, not installed", func(_ *hostprobe.MacParts, s *SetupFacts) {
			s.Exe, s.InstallDir, s.BundledRelease = "/Applications/Potluck.app/Contents/Resources/bin/potluck", "/Users/b/.local/bin", "/Applications/Potluck.app/Contents/Resources/release"
			s.InstallBundled = "potluck upgrade --release ..."
		}, want{checks: map[string]string{"potluck": Todo}, nextID: "potluck", next: "potluck upgrade --release ...",
			doArgs: "--json upgrade --release /Applications/Potluck.app/Contents/Resources/release --dir /Users/b/.local/bin"}},
		{"bundled release, installed", func(_ *hostprobe.MacParts, s *SetupFacts) {
			s.Exe, s.InstallDir, s.BundledRelease = "/Users/b/.local/bin/potluck", "/Users/b/.local/bin", "/x/release"
		}, want{checks: map[string]string{"potluck": Done}, nextID: "setup"}},
		{"everything fine", func(*hostprobe.MacParts, *SetupFacts) {},
			want{checks: map[string]string{"mac": Done, "potluck": Done, "tart": Done, "softnet": Done, "disk": Done}, absent: []string{"other"}, nextID: "setup"}},
	}
	order := []string{"mac", "potluck", "tart", "softnet", "disk", "other", "daemon", "setup", "lending", "menu", "login", "updates"}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := base
			m := fineMac()
			tc.edit(&m, &f.Setup)
			f.Setup.Mac = &m
			s := Build(f).Setup
			var ids []string
			for _, c := range s.Checks {
				if (c.Action != "") != (c.Do != nil) {
					t.Errorf("%s: action %q, do %+v", c.ID, c.Action, c.Do)
				}
				if slices.Contains(order, c.ID) {
					ids = append(ids, c.ID)
				}
			}
			if !slices.IsSortedFunc(ids, func(a, b string) int { return slices.Index(order, a) - slices.Index(order, b) }) {
				t.Errorf("order %v", ids)
			}
			for id, st := range tc.want.checks {
				if c := check(s, id); c.State != st {
					t.Errorf("%s: %s, want %s (%+v)", id, c.State, st, c)
				}
			}
			for _, id := range tc.want.absent {
				if c := check(s, id); c.ID != "" {
					t.Errorf("%s is there: %+v", id, c)
				}
			}
			if s.NextID != tc.want.nextID || tc.want.next != "" && s.Next != tc.want.next {
				t.Fatalf("next_id %q next %q", s.NextID, s.Next)
			}
			c := check(s, s.NextID)
			if c.State == Blocked && c.Action == "" && (s.Next != "" || c.Do != nil) {
				t.Errorf("a blocked check with no action: next %q do %+v", s.Next, c.Do)
			}
			got := ""
			if c.Do != nil {
				got = strings.Join(c.Do.Args, " ")
			}
			if tc.want.doArgs != "" && got != tc.want.doArgs {
				t.Errorf("do args %q", got)
			}
			if tc.want.doURL != "" && (c.Do == nil || c.Do.URL != tc.want.doURL) {
				t.Errorf("do %+v", c.Do)
			}
			if !strings.Contains(c.Detail, tc.want.detail) {
				t.Errorf("detail %q, want %q in it", c.Detail, tc.want.detail)
			}
		})
	}
}

// Potluck's build withdrawn: upstream's takes root by hand, so nothing to
// press, and the words do not say there is none.
func TestSoftnetBuildWithdrawn(t *testing.T) {
	old := softnet.Builds
	t.Cleanup(func() { softnet.Builds = old })
	softnet.Builds = slices.Clone(old)
	for i := range softnet.Builds {
		softnet.Builds[i].Withdrawn = "x"
	}
	m := fineMac()
	m.Softnet = hostprobe.Layer{Problems: []string{"install the latest upstream Softnet"}}
	c := softnetCheck(&m, SetupFacts{})
	if c.State != Blocked || c.Do != nil || c.Action != "" || !strings.Contains(c.Detail, "upstream") || !strings.Contains(c.words, "by hand") {
		t.Errorf("%+v, words %q", c, c.words)
	}
}

// A Mac that already shares boxes has its images: low disk is a fact there,
// and stops no next step. With the daemon down nothing can be listed, and
// the last run's record says it.
func TestLowDiskOnALendingMac(t *testing.T) {
	m := fineMac()
	m.Report.TartFreeGB = 20
	base := Facts{Cores: 8, Host: &protocol.HostStatus{Mode: "auto", State: "accepting"}, Start: "potluck daemon --detach", Share: "potluck host"}
	base.Setup = SetupFacts{Version: "v1.2.3", Mac: &m}
	up := base
	up.Targets = []computeapi.RunTargetView{box("c2", "", "macos", 2, 2, []string{"a"}, nil)}
	unlisted := base
	unlisted.ListErr = "timed out"
	unread := base
	unread.Setup.RunErr = "not JSON"
	down := base
	down.Daemon = "connection refused"
	down.Setup.Run = &hostsetup.Run{Started: "x", Ended: "y", Stages: stages(hostsetup.Order...)}
	downNoRecord := base
	downNoRecord.Daemon = "connection refused"
	downBlocked := downNoRecord
	downBlocked.Setup.Run = &hostsetup.Run{Started: "x", Ended: "y", Stages: []hostsetup.Stage{{Name: hostsetup.Preflight, Result: hostsetup.Blocked, Detail: "no room"}}}
	for name, f := range map[string]Facts{"up": up, "unlisted": unlisted, "unread record": unread, "down": down, "down, no record": downNoRecord, "down, stopped at preflight": downBlocked} {
		s := Build(f).Setup
		if c := check(s, "disk"); c.State != Info {
			t.Errorf("%s: disk %+v", name, c)
		}
		if s.NextID == "disk" || strings.HasPrefix(name, "down") && s.NextID != "daemon" {
			t.Errorf("%s: next_id %q", name, s.NextID)
		}
	}
}

// Without --probe the checklist is as it was: no mac, tart, softnet or
// disk check.
func TestNoProbeNoMacChecks(t *testing.T) {
	f := Facts{Cores: 8, Host: &protocol.HostStatus{Mode: "auto", State: "accepting"}, Start: "potluck daemon --detach", Share: "potluck host"}
	s := Build(f).Setup
	for _, id := range []string{"mac", "tart", "softnet", "disk", "other"} {
		if c := check(s, id); c.ID != "" {
			t.Errorf("%s: %+v", id, c)
		}
	}
	if s.Checks[0].ID != "potluck" || s.Checks[0].State != Done {
		t.Errorf("first %+v", s.Checks[0])
	}
}

// The setup button never comes before a Softnet the host run would need:
// whatever is wrong with Softnet, softnet is next_id and not setup, on every
// macOS Potluck lends from.
func TestSoftnetComesBeforeSetup(t *testing.T) {
	base := Facts{Cores: 8, Host: &protocol.HostStatus{Mode: "auto", State: "accepting"}, Start: "potluck daemon --detach", Share: "potluck host"}
	notRoot := []string{"chown root /s", "chmod u+s,go-w /s"}
	unusable := map[string]func(l *hostprobe.Layer){
		"not installed":   func(l *hostprobe.Layer) { l.SoftnetPath, l.Problems = "", []string{"not installed"} },
		"will not run":    func(l *hostprobe.Layer) { l.SoftnetVersion, l.Problems = "", []string{"cannot run"} },
		"refuses rules":   func(l *hostprobe.Layer) { l.Problems = []string{"cannot parse the rules"} },
		"not setuid root": func(l *hostprobe.Layer) { l.Problems, l.RootSteps = []string{"not setuid root"}, notRoot },
	}
	records := map[string]*hostsetup.Run{
		"never set up": nil,
		"stopped":      {Started: "x", Ended: "y", AgainArgs: []string{"host", "trial"}, Stages: []hostsetup.Stage{{Name: hostsetup.Preflight, Result: hostsetup.Blocked}}},
	}
	for _, macOS := range []string{"13.6", "14.6", "15.1", "26.1", "27.0"} {
		for what, edit := range unusable {
			for rec, run := range records {
				m := fineMac()
				m.Report.MacOS = macOS
				edit(&m.Softnet)
				f := base
				f.Setup = SetupFacts{Version: "v1.2.3", SoftnetInstall: "potluck softnet install", Mac: &m, Run: run}
				s := Build(f).Setup
				ids := []string{}
				for _, c := range s.Checks {
					ids = append(ids, c.ID)
				}
				if s.NextID != "softnet" || slices.Index(ids, "softnet") > slices.Index(ids, "setup") {
					t.Errorf("macOS %s, Softnet %s, %s: next_id %q of %v", macOS, what, rec, s.NextID, ids)
				}
			}
		}
	}
}

// Setup run from the app always hands back a root step, whatever the last
// run's own flags said.
func TestSetupNeverSkipsARootStep(t *testing.T) {
	for _, again := range [][]string{nil, {"host", "trial"}, {"host", "--no-sudo=true", "trial"}, {"host", "--no-sudo=false", "trial"}} {
		run := &hostsetup.Run{AgainArgs: again}
		last := ""
		for _, a := range againDo(run, "x").Args {
			if strings.HasPrefix(a, "--no-sudo") {
				last = a
			}
		}
		if last != "--no-sudo" && last != "--no-sudo=true" {
			t.Errorf("%q: %q", again, last)
		}
	}
}
