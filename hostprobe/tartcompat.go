package hostprobe

import (
	"context"
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/sengirunners/potluck/imagepins"
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

func executableVersion(ctx context.Context, run Runner, path, label string, l *Layer) string {
	res, err := runBounded(ctx, run, path, "--version")
	version := strings.TrimSpace(res.Stdout)
	if err != nil || res.Exit != 0 || version == "" {
		l.Problems = append(l.Problems, fmt.Sprintf("%s at %s cannot run on this host: %s; use a build compatible with this macOS and architecture", label, path, commandFailure(res, err)))
		return ""
	}
	return firstVersion(version)
}

func detectTartArguments(ctx context.Context, run Runner, path string, l *Layer) {
	res, err := runBounded(ctx, run, path, "run", "--help")
	if err != nil || res.Exit != 0 {
		l.Problems = append(l.Problems, "cannot read Tart run capabilities at "+path+": "+commandFailure(res, err))
		return
	}
	for _, arg := range vmhost.TartBootArguments(true) {
		flag, _, _ := strings.Cut(arg, "=")
		if !regexp.MustCompile(`(?:^|[\s,])` + regexp.QuoteMeta(flag) + `(?:[=\s]|$)`).MatchString(res.Stdout) {
			l.Problems = append(l.Problems, "Tart at "+path+" lacks required run option "+flag+"; use a compatible Tart build that supports Potluck's isolation policy")
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

func detectMacOS(ctx context.Context, run Runner, r *Report, l *Layer) {
	// runtime.GOARCH describes the program under Rosetta, not the hardware.
	arch, archErr := runBounded(ctx, run, "/usr/sbin/sysctl", "-n", "hw.optional.arm64")
	switch {
	case archErr == nil && arch.Exit == 0 && strings.TrimSpace(arch.Stdout) == "1":
		r.Arch = "arm64"
	case archErr == nil && arch.Exit == 0 && strings.TrimSpace(arch.Stdout) == "0":
		r.Arch = "amd64"
	default:
		r.Arch = "unknown"
		l.Problems = append(l.Problems, "cannot determine this Mac's hardware architecture: "+commandFailure(arch, archErr))
	}
	if r.Arch != "arm64" {
		l.Problems = append(l.Problems, "tart needs Apple silicon")
	}
	res, err := runBounded(ctx, run, "/usr/bin/sw_vers", "-productVersion")
	r.MacOS = strings.TrimSpace(res.Stdout)
	if err != nil || res.Exit != 0 || macOSMajor(r.MacOS) == 0 {
		r.MacOS = ""
		l.Problems = append(l.Problems, "cannot determine macOS product version: "+commandFailure(res, err))
	} else if macOSMajor(r.MacOS) < 13 {
		l.Problems = append(l.Problems, "Tart needs macOS 13 or newer; this host runs "+r.MacOS)
	}
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
