package menubar

// The checks a never-set-up Mac needs before a host run can work, which
// host status adds when asked with --probe (the setup window asks; the
// menu's 15 s ask does not, since reading them runs Tart and Softnet):
// this Mac (Apple silicon, macOS, virtualization), Tart, Softnet and the
// disk where Tart keeps images. Each is made from a fact hostprobe reads
// (MacParts), never from the words of a problem; the words go in Detail.
// And the potluck check: done, or, when the app carries a release and the
// program answering is not the installed one, the step that installs it.

import (
	"fmt"
	"math"
	"path/filepath"
	"slices"
	"strings"

	"github.com/sengirunners/potluck/hostprobe"
	"github.com/sengirunners/potluck/hostsetup"
	"github.com/sengirunners/potluck/softnet"
)

// softnetInstallArgs install this Mac's Softnet build without sudo: the
// answer names the one admin step (admin_step) for the app to run.
var softnetInstallArgs = []string{"--json", "softnet", "install", "--no-sudo"}

// softnetUninstallArgs remove a managed Softnet this Mac does not use, the
// same way (admin_step).
var softnetUninstallArgs = []string{"--json", "softnet", "uninstall", "--no-sudo"}

// tartInApp is the end of a Tart program's path inside a dragged tart.app.
var tartInApp = string(filepath.Separator) + filepath.Join("tart.app", "Contents", "MacOS", "tart")

// potluckCheck is the potluck check: todo when the app carries a release
// (BundledRelease) and this program does not run from the install
// directory, so installing it is the first step.
func potluckCheck(s SetupFacts) Check {
	version := s.Version
	if version == "" {
		version = "(version unknown)"
	}
	if s.BundledRelease != "" && s.InstallDir == "" {
		return Check{ID: "potluck", State: Unknown, Detail: "Potluck cannot tell where to install itself: there is no home directory and POTLUCK_INSTALL_DIR is not set"}
	}
	if s.BundledRelease != "" && filepath.Dir(s.Exe) != s.InstallDir {
		return Check{ID: "potluck", State: Todo,
			Detail: fmt.Sprintf("this copy of Potluck (%s) runs from %s, not from %s, where it installs itself", version, filepath.Dir(s.Exe), s.InstallDir),
			Action: s.InstallBundled,
			Do:     &Do{Label: "Install Potluck", Args: []string{"upgrade", "--release", s.BundledRelease, "--dir", s.InstallDir}}}
	}
	return Check{ID: "potluck", State: Done, Detail: "Potluck " + version + " is installed"}
}

// macCheck is this Mac: Apple silicon, macOS 13 or newer, and
// virtualization. A Mac without them cannot lend, and nothing Potluck runs
// changes that, so a blocked mac check has no action. What could not be
// read is unknown, no verdict: the walk goes on and the host run's own
// preflight decides.
func macCheck(m *hostprobe.MacParts) Check {
	r := m.Report
	major := hostprobe.MacOSMajor(r.MacOS)
	c := func(state, detail string) Check { return Check{ID: "mac", State: state, Detail: detail} }
	switch {
	case r.Arch == "amd64":
		return c(Blocked, "this Mac has an Intel processor: Potluck lends through Tart's virtual machines, which need Apple silicon (M1 or newer), so this Mac cannot lend")
	case m.MacOSUnread == "" && major < 13:
		return c(Blocked, fmt.Sprintf("this Mac runs macOS %s and Potluck needs macOS 13 or newer: if this Mac can update, update it in System Settings > General > Software Update, then check again", r.MacOS))
	case m.HVUnread == "" && !m.HV:
		return c(Blocked, "macOS does not offer virtualization on this Mac (Virtualization.framework is not available), so it cannot run Potluck's virtual machines; a Mac that itself runs inside a virtual machine usually cannot")
	case m.ArchUnread != "":
		return c(Unknown, "Potluck could not read which processor this Mac has: "+m.ArchUnread)
	case m.MacOSUnread != "":
		return c(Unknown, "Potluck could not read which macOS this Mac runs: "+m.MacOSUnread)
	case m.HVUnread != "":
		return c(Unknown, "Potluck could not read whether this Mac offers virtualization: "+m.HVUnread)
	}
	return c(Done, fmt.Sprintf("Apple silicon, macOS %s, virtualization available", r.MacOS))
}

// tartCheck is Tart: found, runs, and takes Potluck's run options; unknown
// when it did not answer in time.
func tartCheck(m *hostprobe.MacParts) Check {
	t := m.Tart
	words := strings.Join(t.Problems, "; ")
	download := &Do{Label: "Download Tart", URL: hostprobe.TartReleases}
	get := "download tart.tar.gz from " + hostprobe.TartReleases + ", unpack it and drag tart.app into Applications"
	switch {
	case t.Unread != "":
		return Check{ID: "tart", State: Unknown, Detail: t.Unread}
	case t.CLI == "" && len(m.Hollow) == 0:
		return Check{ID: "tart", State: Todo, Detail: "Tart is not installed: Potluck runs each job in a Tart virtual machine", Action: get, Do: download}
	case t.CLI == "":
		return Check{ID: "tart", State: Todo, Detail: words, Action: get, Do: download}
	case len(t.Problems) > 0 && strings.HasSuffix(t.CLI, tartInApp): // a dragged tart.app: a new download replaces it
		return Check{ID: "tart", State: Todo, Detail: words, Action: "replace tart.app in Applications with the latest: " + get, Do: download}
	case len(t.Problems) > 0: // installed some other way (Homebrew, a link on PATH), which wins over a download
		return Check{ID: "tart", State: Blocked, Detail: words + "; update the Tart at " + t.CLI + " the way you installed it (brew upgrade tart, if Homebrew installed it)"}
	}
	return Check{ID: "tart", State: Done, Detail: fmt.Sprintf("Tart %s at %s", t.Version, t.CLI)}
}

// softnetCheck is Softnet, which Tart starts for each box's isolated
// network: usable, else what softnet.For says this Mac can install.
func softnetCheck(m *hostprobe.MacParts, s SetupFacts) Check {
	sn := m.Softnet
	if len(sn.Problems) == 0 && sn.SoftnetPath != "" {
		detail := fmt.Sprintf("Softnet %s at %s", sn.SoftnetVersion, sn.SoftnetPath)
		if sn.SoftnetBuild != "" {
			detail += " (Potluck's build " + sn.SoftnetBuild + ")"
		}
		return Check{ID: "softnet", State: Done, Detail: detail}
	}
	if sn.Unread != "" {
		return Check{ID: "softnet", State: Unknown, Detail: sn.Unread}
	}
	words := strings.Join(sn.Problems, "; ")
	if m.ArchUnread != "" || m.MacOSUnread != "" { // which Softnet fits is not known
		return Check{ID: "softnet", State: Unknown, Detail: words}
	}
	major := hostprobe.MacOSMajor(m.Report.MacOS)
	a := softnet.For(major, m.Report.Arch)
	if st := m.SoftnetStore; st != nil && st.Current != nil && st.Current.Problem != "" && a.Kind != softnet.Managed { // an unsound managed copy this Mac does not use: removing it comes first
		return Check{ID: "softnet", State: Todo, Detail: words, Action: s.SoftnetUninstall, Do: &Do{Label: "Remove the old Softnet", Args: softnetUninstallArgs}}
	}
	switch {
	case a.Kind == softnet.Managed && !a.Candidate:
		return Check{ID: "softnet", State: Todo, Detail: words, Action: s.SoftnetInstall, Do: &Do{Label: "Install Softnet", Args: softnetInstallArgs}}
	case len(sn.RootSteps) > 0: // only its permissions are wrong: a new download has the same ones
		return Check{ID: "softnet", State: Blocked, Detail: words + "; to fix it, run in a terminal: " + hostprobe.RootCommand(sn.RootSteps)}
	case a.Kind == softnet.Upstream:
		return Check{ID: "softnet", State: Todo, Detail: words,
			Action: fmt.Sprintf("install the latest upstream Softnet (%s or newer) from %s", softnet.UpstreamMinVersion, softnet.UpstreamReleases),
			Do:     &Do{Label: "Get Softnet", URL: softnet.UpstreamReleases}}
	default: // a candidate build only (the app never installs one), or no build for this Mac
		return Check{ID: "softnet", State: Blocked, Detail: words}
	}
}

// diskCheck is the free disk where Tart keeps images against what
// Potluck's default image for this Mac needs (nothing once Tart has it). It
// stops the walk only for a Mac whose image is not chosen yet: one that
// shares boxes (shares: it does, or what it shares cannot be listed, as
// when the daemon is not running: starting it comes first) has its images,
// and a run going on now or past its preflight was checked against the image
// it pulls, which may be smaller. What cannot be read does not stop it either.
func diskCheck(m *hostprobe.MacParts, s SetupFacts, shares bool) Check {
	r := m.Report
	free := math.Floor(r.TartFreeGB)
	c := func(state, detail string) Check { return Check{ID: "disk", State: state, Detail: detail} }
	where := fmt.Sprintf("%.0f GB free in %s, where Tart keeps images", free, r.TartDir)
	switch {
	case r.TartDiskUnread != "":
		return c(Unknown, "the free disk where Tart keeps images cannot be read: "+r.TartDiskUnread)
	case m.ImagesUnread != "" && r.TartFreeGB < float64(m.NeedGB):
		return c(Unknown, fmt.Sprintf("%s; Potluck's image for this Mac needs about %d GB unless it is already there, and Tart's images could not be listed: %s", where, m.NeedGB, m.ImagesUnread))
	case m.NeedGB == 0:
		return c(Done, where+"; Potluck's image for this Mac is already there")
	case r.TartFreeGB >= float64(m.NeedGB):
		return c(Done, fmt.Sprintf("%s (Potluck's image for this Mac needs about %d GB)", where, m.NeedGB))
	case shares, s.RunErr != "", runState(s) == SetupRunning, pastPreflight(s.Run):
		return c(Info, fmt.Sprintf("%s; Potluck's default image for this Mac would need about %d GB, and a host run checks the room for the image it pulls", where, m.NeedGB))
	}
	return c(Blocked, fmt.Sprintf("only %s, and Potluck's image for this Mac needs about %d GB: free up %.0f GB more (empty the Trash, delete large files), then check again",
		where, m.NeedGB, float64(m.NeedGB)-free))
}

// pastPreflight is that the last host run's preflight, which checks the
// disk for the images that run pulls, went through.
func pastPreflight(run *hostsetup.Run) bool {
	if run == nil {
		return false
	}
	return slices.ContainsFunc(run.Stages, func(st hostsetup.Stage) bool {
		return st.Name == hostsetup.Preflight && st.Result == hostsetup.Succeeded
	})
}

// probeChecks are the checks after potluck: Tart, Softnet, the disk, and
// one blocked check for a problem none of them is. They need a Mac that can
// lend at all, so a blocked mac check leaves them out.
func probeChecks(m *hostprobe.MacParts, s SetupFacts, shares bool) []Check {
	out := []Check{tartCheck(m), softnetCheck(m, s), diskCheck(m, s, shares)}
	if len(m.Rest.Problems) > 0 {
		out = append(out, Check{ID: "other", State: Blocked, Detail: "Potluck found a problem it has no button for: " + strings.Join(m.Rest.Problems, "; ")})
	}
	return out
}
