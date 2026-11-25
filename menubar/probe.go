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

// tartInstallArgs install the Tart Potluck pins (no root).
var tartInstallArgs = []string{"--json", "tart", "install"}

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
		return Check{ID: "potluck", State: Unknown, Detail: "Potluck cannot tell where to install itself: there is no home directory and POTLUCK_INSTALL_DIR is not set"}.
			say("Couldn't tell whether Potluck is installed", "")
	}
	if s.BundledRelease != "" && filepath.Dir(s.Exe) != s.InstallDir {
		return Check{ID: "potluck", State: Todo,
			Detail: fmt.Sprintf("this copy of Potluck (%s) runs from %s, not from %s, where it installs itself", version, filepath.Dir(s.Exe), s.InstallDir),
			Action: s.InstallBundled,
			Do:     &Do{Label: "Install Potluck", Args: []string{"upgrade", "--release", s.BundledRelease, "--dir", s.InstallDir}}}.
			say("Potluck isn't installed yet", "First, install Potluck on this Mac. It takes a few seconds.")
	}
	return Check{ID: "potluck", State: Done, Detail: "Potluck " + version + " is installed"}.say("Potluck is installed", "")
}

// macCheck is this Mac: Apple silicon, macOS 13 or newer, and
// virtualization. A Mac without them cannot lend, and nothing Potluck runs
// changes that, so a blocked mac check has no action. What could not be
// read is unknown, no verdict: the walk goes on and the host run's own
// preflight decides.
func macCheck(m *hostprobe.MacParts) Check {
	r := m.Report
	major := hostprobe.MacOSMajor(r.MacOS)
	c := func(state, detail, words, help string) Check {
		return Check{ID: "mac", State: state, Detail: detail}.say(words, help)
	}
	unread := func(why string) Check {
		return c(Unknown, why, "Couldn't check this Mac's hardware just now", "")
	}
	switch {
	case r.Arch == "amd64":
		return c(Blocked, "this Mac has an Intel processor: Potluck lends through Tart's virtual machines, which need Apple silicon (M1 or newer), so this Mac cannot lend",
			"This Mac has an Intel processor", "Potluck needs a Mac with an Apple chip (M1 or newer). This one has an Intel processor, so it can't lend.")
	case m.MacOSUnread == "" && major < 13:
		return c(Blocked, fmt.Sprintf("this Mac runs macOS %s and Potluck needs macOS 13 or newer: if this Mac can update, update it in System Settings > General > Software Update, then check again", r.MacOS),
			"This Mac's macOS is too old", "Potluck needs macOS 13 or newer. If this Mac can be updated, do that in System Settings > General > Software Update, then come back here.")
	case m.HVUnread == "" && !m.HV:
		return c(Blocked, "macOS does not offer virtualization on this Mac (Virtualization.framework is not available), so it cannot run Potluck's virtual machines; a Mac that itself runs inside a virtual machine usually cannot",
			"This Mac can't run jobs", "macOS doesn't let apps run other systems safely on this Mac, which Potluck needs, so it can't lend.")
	case m.ArchUnread != "":
		return unread("Potluck could not read which processor this Mac has: " + m.ArchUnread)
	case m.MacOSUnread != "":
		return unread("Potluck could not read which macOS this Mac runs: " + m.MacOSUnread)
	case m.HVUnread != "":
		return unread("Potluck could not read whether this Mac offers virtualization: " + m.HVUnread)
	}
	return c(Done, fmt.Sprintf("Apple silicon, macOS %s, virtualization available", r.MacOS), "This Mac is the right kind", "")
}

// tartCheck is Tart: found, runs, and takes Potluck's run options; unknown
// when it did not answer in time. Where potluck tart install would put a
// Tart here (TartInstall) its do runs that, else it opens Tart's releases:
// an install leaves alone a tart.app Potluck did not install, so running it
// would change nothing.
func tartCheck(m *hostprobe.MacParts, s SetupFacts) Check {
	t := m.Tart
	words := strings.Join(t.Problems, "; ")
	const sealed = "Potluck runs each job in an app called Tart, sealed off from your own files. "
	const byHand = ` It needs no password. If that doesn't work, press "Open the download page" below, download the file named tart.tar.gz, double-click it, and drag the tart app into your Applications folder.`
	const getHelp = sealed + "Press the button, download the file named tart.tar.gz from the page that opens, double-click it, and drag the tart app into your Applications folder. This window notices when it is there."
	download := &Do{Label: "Open the download page", URL: hostprobe.TartReleases}
	get := "download tart.tar.gz from " + hostprobe.TartReleases + ", unpack it and drag tart.app into Applications"
	switch {
	case t.Unread != "":
		return Check{ID: "tart", State: Unknown, Detail: t.Unread}.say("Tart didn't answer just now", "")
	case s.TartInstall != "" && t.CLI == "" && len(m.Hollow) == 0:
		return Check{ID: "tart", State: Todo, Detail: "Tart is not installed: Potluck runs each job in a Tart virtual machine", Action: s.TartInstall, Do: &Do{Label: "Install Tart", Args: tartInstallArgs}}.
			say("Tart isn't installed yet", sealed+"Press the button to download and install it."+byHand).or(download)
	case s.TartInstall != "" && t.CLI == "":
		return Check{ID: "tart", State: Todo, Detail: words, Action: s.TartInstall, Do: &Do{Label: "Install Tart", Args: tartInstallArgs}}.
			say("The copy of Tart on this Mac is incomplete", sealed+"Press the button to download and install a whole copy."+byHand).or(download)
	case s.TartInstall != "" && len(t.Problems) > 0:
		return Check{ID: "tart", State: Todo, Detail: words, Action: s.TartInstall, Do: &Do{Label: "Update Tart", Args: tartInstallArgs}}.
			say("Tart needs updating", sealed+"Press the button to update it."+byHand).or(download)
	case t.CLI == "" && len(m.Hollow) == 0:
		return Check{ID: "tart", State: Todo, Detail: "Tart is not installed: Potluck runs each job in a Tart virtual machine", Action: get, Do: download}.
			say("Tart isn't installed yet", getHelp)
	case t.CLI == "":
		return Check{ID: "tart", State: Todo, Detail: words, Action: get, Do: download}.
			say("The copy of Tart on this Mac is incomplete", getHelp)
	case len(t.Problems) > 0 && strings.HasSuffix(t.CLI, tartInApp): // a dragged tart.app: a new download replaces it
		return Check{ID: "tart", State: Todo, Detail: words, Action: "replace tart.app in Applications with the latest: " + get, Do: download}.
			say("Tart needs replacing with a newer one", getHelp+" When it asks, replace the one that is there.")
	case len(t.Problems) > 0: // installed some other way (Homebrew, a link on PATH), which wins over a download
		return Check{ID: "tart", State: Blocked, Detail: words + "; update the Tart at " + t.CLI + " the way you installed it (brew upgrade tart, if Homebrew installed it)"}.
			say("The Tart on this Mac can't be used", "It was installed another way, so Potluck can't replace it for you. Whoever installed it needs to update it. If that wasn't you, press Copy details and send what it copies to the person who invited you.")
	}
	return Check{ID: "tart", State: Done, Detail: fmt.Sprintf("Tart %s at %s", t.Version, t.CLI)}.say("Tart is installed", "")
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
		return Check{ID: "softnet", State: Done, Detail: detail}.say("The network helper is installed", "")
	}
	if sn.Unread != "" {
		return Check{ID: "softnet", State: Unknown, Detail: sn.Unread}.say("The network helper didn't answer just now", "")
	}
	words := strings.Join(sn.Problems, "; ")
	if m.ArchUnread != "" || m.MacOSUnread != "" { // which Softnet fits is not known
		return Check{ID: "softnet", State: Unknown, Detail: words}.say("Couldn't check the network helper just now", "")
	}
	major := hostprobe.MacOSMajor(m.Report.MacOS)
	noHelper := fmt.Sprintf("Potluck needs a network helper to keep jobs away from your own network, and can't install one on macOS %d. If this Mac can be updated to macOS %d or newer, do that in System Settings > General > Software Update, then come back here. If it can't, it can't lend yet.", major, softnet.UpstreamMinMacOS)
	if major >= softnet.UpstreamMinMacOS {
		noHelper = "Potluck needs a network helper to keep jobs away from your own network, and can't set it up here. Press Copy details and send what it copies to the person who invited you."
	}
	a := softnet.For(major, m.Report.Arch)
	if st := m.SoftnetStore; st != nil && st.Current != nil && st.Current.Problem != "" && a.Kind != softnet.Managed { // an unsound managed copy this Mac does not use: removing it comes first
		return Check{ID: "softnet", State: Todo, Detail: words, Action: s.SoftnetUninstall, Do: &Do{Label: "Remove the old network helper", Args: softnetUninstallArgs}}.
			say("An old network helper needs removing", "An old network helper is left on this Mac and no longer works. Removing it asks for this Mac's password once.")
	}
	switch {
	case a.Kind == softnet.Managed && !a.Candidate:
		return Check{ID: "softnet", State: Todo, Detail: words, Action: s.SoftnetInstall, Do: &Do{Label: "Install the network helper", Args: softnetInstallArgs}}.
			say("The network helper isn't ready", "The network helper, called Softnet, keeps jobs away from your own network. Installing it asks for this Mac's password once.")
	case len(sn.RootSteps) > 0: // only its permissions are wrong: a new download has the same ones
		return Check{ID: "softnet", State: Blocked, Detail: words + "; to fix it, run in a terminal: " + hostprobe.RootCommand(sn.RootSteps)}.
			say("The network helper can't be used", "The network helper is installed, but it doesn't have the permission it needs. Press Copy details and send what it copies to the person who invited you.")
	case a.Kind == softnet.Upstream:
		return Check{ID: "softnet", State: Todo, Detail: words,
			Action: fmt.Sprintf("install the latest upstream Softnet (%s or newer) from %s", softnet.UpstreamMinVersion, softnet.UpstreamReleases),
			Do:     &Do{Label: "Open the download page", URL: softnet.UpstreamReleases}}.
			say("The network helper isn't installed", "The network helper, called Softnet, keeps jobs away from your own network. Press the button to open its download page.")
	default: // a candidate build only (the app never installs one), or no build for this Mac
		return Check{ID: "softnet", State: Blocked, Detail: words}.say("There is no network helper for this Mac's macOS yet", noHelper)
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
	c := func(state, detail, words, help string) Check {
		return Check{ID: "disk", State: state, Detail: detail}.say(words, help)
	}
	freeWords := fmt.Sprintf("%.0f GB of disk space free", free)
	where := fmt.Sprintf("%.0f GB free in %s, where Tart keeps images", free, r.TartDir)
	switch {
	case r.TartDiskUnread != "":
		return c(Unknown, "the free disk where Tart keeps images cannot be read: "+r.TartDiskUnread, "Couldn't check the free disk space just now", "")
	case m.ImagesUnread != "" && r.TartFreeGB < float64(m.NeedGB):
		return c(Unknown, fmt.Sprintf("%s; Potluck's image for this Mac needs about %d GB unless it is already there, and Tart's images could not be listed: %s", where, m.NeedGB, m.ImagesUnread), freeWords, "")
	case m.NeedGB == 0:
		return c(Done, where+"; Potluck's image for this Mac is already there", "Enough disk space", "")
	case r.TartFreeGB >= float64(m.NeedGB):
		return c(Done, fmt.Sprintf("%s (Potluck's image for this Mac needs about %d GB)", where, m.NeedGB), "Enough disk space", "")
	case shares, s.RunErr != "", runState(s) == SetupRunning, pastPreflight(s.Run):
		return c(Info, fmt.Sprintf("%s; Potluck's default image for this Mac would need about %d GB, and a host run checks the room for the image it pulls", where, m.NeedGB), freeWords, "")
	}
	return c(Blocked, fmt.Sprintf("only %s, and Potluck's image for this Mac needs about %d GB: free up %.0f GB more (empty the Trash, delete large files), then check again",
		where, m.NeedGB, float64(m.NeedGB)-free),
		"Not enough disk space", fmt.Sprintf("Potluck needs about %d GB of free disk space and this Mac has %.0f GB. Free up %.0f GB more (empty the Trash, delete large files you don't need), then come back here.", m.NeedGB, free, float64(m.NeedGB)-free))
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
	out := []Check{tartCheck(m, s), softnetCheck(m, s), diskCheck(m, s, shares)}
	if len(m.Rest.Problems) > 0 {
		out = append(out, Check{ID: "other", State: Blocked, Detail: "Potluck found a problem it has no button for: " + strings.Join(m.Rest.Problems, "; ")}.
			say("Something else on this Mac is in the way", "Potluck found a problem it can't fix from here. Press Copy details and send what it copies to the person who invited you."))
	}
	return out
}

// or is c with another way to do it.
func (c Check) or(d *Do) Check {
	c.other = d
	return c
}
