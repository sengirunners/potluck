package hostprobe

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/sengirunners/potluck/imagepins"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/vmhost"
)

func commandFailure(res Result, err error) string {
	if err != nil {
		return err.Error()
	}
	return fmt.Sprintf("exit %d: %s", res.Exit, strings.TrimSpace(res.Stderr+" "+res.Stdout))
}

func absoluteExecutable(path string, l *Layer) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		l.Problems = append(l.Problems, "cannot resolve executable "+path+": "+err.Error())
		return ""
	}
	return abs
}

// unread records on l that its program did not answer in time, as a problem
// and, with no other problem known, as Unread.
func (l *Layer) unread(why string) {
	if len(l.Problems) == 0 {
		l.Unread = why
	}
	l.Problems = append(l.Problems, why)
}

func executableVersion(ctx context.Context, run Runner, path, label string, l *Layer) string {
	res, err := runBounded(ctx, run, path, "--version")
	version := strings.TrimSpace(res.Stdout)
	if errors.Is(err, errNoAnswer) {
		l.unread(fmt.Sprintf("%s at %s could not be asked its version: %v", label, path, err))
		return ""
	}
	if err != nil || res.Exit != 0 || version == "" {
		l.Problems = append(l.Problems, fmt.Sprintf("%s at %s cannot run on this host: %s; use a build compatible with this macOS and architecture", label, path, commandFailure(res, err)))
		return ""
	}
	return firstVersion(version)
}

// resolveTart is the Tart program to run when the report's tart layer names
// none: vmhost's one lookup (a test gives its own).
var resolveTart = vmhost.ResolveTart

// tartCLI is the Tart program a command for layer runs: the one the
// report found, else resolveTart's.
func tartCLI(layer Layer) string {
	if layer.CLI != "" {
		return layer.CLI
	}
	return resolveTart()
}

// tartInstallWords is how a lender gets Tart, without root.
func tartInstallWords() string {
	return "run `" + config.PotluckCommand("tart install") + "`, or download tart.tar.gz from " + TartReleases + ", unpack it and drag tart.app into Applications (no Terminal or root needed; guide: https://tart.run/quick-start/)"
}

// detectTartCLI finds Tart (vmhost.FindTart: PATH, the usual bin
// directories, then tart.app in Applications) and checks it runs and has
// the run options Potluck boots a box with. A tart.app with no program in it
// is a problem of its own, never taken for "not installed"; they are
// returned.
func detectTartCLI(ctx context.Context, run Runner, f Facts, l *Layer) []string {
	found := vmhost.FindTart(vmhost.TartSearch{LookPath: f.LookPath, Executable: executableOn(f), Exists: f.Exists, Home: hostHome(f)})
	path := found.Path
	if path == "" {
		for _, app := range found.Hollow {
			l.Problems = append(l.Problems, fmt.Sprintf("%s has no Contents/MacOS/tart in it, so it is not a whole Tart download: delete it and %s", app, tartInstallWords()))
		}
		if len(found.Hollow) == 0 {
			l.Problems = append(l.Problems, "tart is not installed: "+tartInstallWords())
		}
		return found.Hollow
	}
	path = absoluteExecutable(path, l)
	l.CLI = path
	if path != "" {
		l.Version = executableVersion(ctx, run, path, "Tart", l)
	}
	if l.Version != "" {
		detectTartArguments(ctx, run, path, l)
	}
	return nil
}

// CheckMac is the Mac preflight's host part alone: this Mac's hardware
// architecture and macOS version (in a Report), with Tart's path and
// version and what stops Tart here (no Apple silicon, too old a macOS, no
// Tart or one without Potluck's run options).
func CheckMac(ctx context.Context, run Runner, f Facts) (Report, Layer) {
	r := Report{OS: "darwin"}
	l := Layer{Name: "tart", Isolation: "vm"}
	detectMacOS(ctx, run, &r, &l)
	detectTartCLI(ctx, run, f, &l)
	return r, l
}

// MacRelease is this Mac's hardware architecture and macOS version alone
// (sysctl and sw_vers at their system paths; nothing on PATH runs).
func MacRelease(ctx context.Context, run Runner) Report {
	r := Report{OS: "darwin"}
	detectMacOS(ctx, run, &r, &Layer{})
	return r
}

// MacOSMajor is a macOS product version's major number, 0 when it is not
// one.
func MacOSMajor(version string) int { return macOSMajor(version) }

func detectTartArguments(ctx context.Context, run Runner, path string, l *Layer) {
	res, err := runBounded(ctx, run, path, "run", "--help")
	if errors.Is(err, errNoAnswer) {
		l.unread(fmt.Sprintf("Tart at %s could not be asked what it can do: %v", path, err))
		return
	}
	if err != nil || res.Exit != 0 {
		l.Problems = append(l.Problems, "cannot read Tart run capabilities at "+path+": "+commandFailure(res, err))
		return
	}
	args, err := vmhost.TartBootArguments(true)
	if err != nil {
		l.Problems = append(l.Problems, err.Error())
		return
	}
	for _, arg := range args {
		flag, _, _ := strings.Cut(arg, "=")
		if !vmhost.TartHelpHas(res.Stdout, flag) {
			l.Problems = append(l.Problems, "Tart at "+path+" lacks required run option "+flag+"; use a compatible Tart build that supports Potluck's isolation policy")
		}
	}
	if len(l.Problems) > 0 {
		return // a Tart refused above is not lent; a warning on top is noise
	}
	for _, h := range vmhost.TartHardeningArguments() {
		if !vmhost.TartHelpHas(res.Stdout, h.Flag) {
			l.Warnings = append(l.Warnings, fmt.Sprintf("Tart %s at %s has no %s (Tart %s or newer), so boxes keep %s; to drop it, %s", l.Version, path, h.Flag, h.Since, h.Without, vmhost.TartUpdateHint()))
		}
	}
}

func macOSMajor(version string) int {
	parts := strings.Split(version, ".")
	if len(parts) < 2 || len(parts) > 3 {
		return 0
	}
	for _, p := range parts {
		if n, err := strconv.Atoi(p); err != nil || n < 0 {
			return 0
		}
	}
	n, _ := strconv.Atoi(parts[0])
	return n
}

// detectMacOS reads this Mac's hardware architecture and macOS version. A
// read that did not answer, or answered nothing it knows, is named in
// archUnread or macOSUnread ("": read), and is a problem too.
func detectMacOS(ctx context.Context, run Runner, r *Report, l *Layer) (archUnread, macOSUnread string) {
	// runtime.GOARCH describes the program under Rosetta, not the hardware.
	arch, archErr := runBounded(ctx, run, "/usr/sbin/sysctl", "-n", "hw.optional.arm64")
	switch {
	case archErr == nil && arch.Exit == 0 && strings.TrimSpace(arch.Stdout) == "1":
		r.Arch = "arm64"
	case archErr == nil && arch.Exit == 0 && strings.TrimSpace(arch.Stdout) == "0",
		archErr == nil && strings.Contains(arch.Stderr, "unknown oid"): // an Intel Mac's kernel has no such name
		r.Arch = "amd64"
	default:
		r.Arch = "unknown"
		archUnread = "cannot determine this Mac's hardware architecture: " + commandFailure(arch, archErr)
		l.Problems = append(l.Problems, archUnread)
	}
	if r.Arch != "arm64" {
		l.Problems = append(l.Problems, "tart needs Apple silicon")
	}
	res, err := runBounded(ctx, run, "/usr/bin/sw_vers", "-productVersion")
	r.MacOS = strings.TrimSpace(res.Stdout)
	if err != nil || res.Exit != 0 || macOSMajor(r.MacOS) == 0 {
		r.MacOS = ""
		macOSUnread = "cannot determine macOS product version: " + commandFailure(res, err)
		l.Problems = append(l.Problems, macOSUnread)
	} else if macOSMajor(r.MacOS) < 13 {
		l.Problems = append(l.Problems, "Tart needs macOS 13 or newer; this host runs "+r.MacOS)
	}
	return archUnread, macOSUnread
}

// CheckTartImages refuses missing compatibility metadata as well as known
// mismatches. A digest outside the supported table needs verified metadata;
// neither its repository name nor a cached copy proves host compatibility.
func CheckTartImages(r Report, images []string) error {
	major := macOSMajor(r.MacOS)
	if r.OS != "darwin" || r.Arch != "arm64" || major < 13 {
		return fmt.Errorf("Tart image preflight needs Apple silicon and a known macOS 13+ product version (got %s/%s macOS %q)", r.OS, r.Arch, r.MacOS)
	}
	for _, im := range images {
		p, ok := PinForImage(im)
		if !ok || p.Layer != "tart" || p.MinHostMacOS == 0 || p.Arch == "" {
			return fmt.Errorf("Tart image %s has unknown host compatibility: add verified architecture and min_host_macos metadata for this digest before lending it", im)
		}
		if p.Arch != r.Arch || major < p.MinHostMacOS {
			return fmt.Errorf("Potluck's compatibility policy supports Tart image %s on %s macOS %d+; this host is %s macOS %s. Choose a compatible published image", im, p.Arch, p.MinHostMacOS, r.Arch, r.MacOS)
		}
	}
	return nil
}

// HostDefaultImage selects the newest compatible default in the pin table.
// Named host_default alternatives preserve older hosts' guest OS. No fallback
// is guessed when its digest has no compatibility data.
func HostDefaultImage(r Report, layer string) (DefaultPin, error) {
	p, err := DefaultImage(layer)
	if err != nil || layer != "tart" {
		return p, err
	}
	var best DefaultPin
	for _, candidate := range pinTable() {
		if candidate.Layer != layer || candidate.Template != "" || candidate.Name != "" && !candidate.HostDefault || candidate.Image == "" {
			continue
		}
		if CheckTartImages(r, []string{candidate.Image}) == nil && (best.Image == "" || candidate.MinHostMacOS > best.MinHostMacOS) {
			best = candidate
		}
	}
	if best.Image != "" {
		return best, nil
	}
	if p, ok := namedSmallLinux(); ok && CheckTartImages(r, []string{p.Image}) == nil {
		return p, nil
	}
	return DefaultPin{}, fmt.Errorf("no supported default Tart image for %s macOS %s; compatibility metadata must be verified before pulling an image", r.Arch, r.MacOS)
}

func namedSmallLinux() (DefaultPin, bool) {
	return imagepins.NamedPin(pinTable(), "tart", SmallLinux)
}
