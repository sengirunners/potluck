package hostprobe

import (
	"context"
	"strings"
	"time"

	"github.com/sengirunners/potluck/softnet"
)

// TartReleases is the page a lender downloads Tart from (tart.tar.gz, which
// unpacks to tart.app).
const TartReleases = "https://github.com/openai/tart/releases/latest"

// MacParts is the Mac preflight (the tart layer) read part by part, so a
// caller can tell from facts which part stops this Mac, never from the
// words of a problem. Each part's Layer holds only that part's problems and
// warnings; the tart layer Detect reports is the parts merged, in this
// order.
type MacParts struct {
	// Report is the Mac as the parts read it: Arch, MacOS, TartDir,
	// TartFreeGB and TartDiskUnread.
	Report Report
	// HV is whether macOS offers virtualization to apps
	// (kern.hv_support).
	HV bool
	// HVUnread is why that could not be read ("": it was); ArchUnread and
	// MacOSUnread the same for Report.Arch ("unknown" then) and
	// Report.MacOS ("" then).
	HVUnread, ArchUnread, MacOSUnread string
	// Mac is the hardware and macOS part: virtualization, Apple silicon,
	// macOS 13 or newer.
	Mac Layer
	// Tart is Tart itself: CLI is its path ("" when none was found) and
	// Version its version ("" when it would not run; Unread when it did not
	// answer in time); Hollow are tart.app
	// folders with no program in them.
	Tart   Layer
	Hollow []string
	// Softnet is the Softnet part: SoftnetPath, SoftnetVersion and
	// SoftnetBuild when it is usable.
	Softnet Layer
	// SoftnetStore is the managed Softnet install's state when one is
	// installed (nil: none).
	SoftnetStore *softnet.Status
	// NeedGB is the free disk Potluck's default image for this Mac needs
	// where Tart keeps images (its pin's min_free_gb); 0 when the disk is
	// shorter than that but Tart already has the image.
	NeedGB int
	// ImagesUnread is why Tart's images could not be listed on a disk
	// shorter than NeedGB, or which image this Mac needs is not known, so
	// whether Tart has it is not either
	// ("": they were, or the disk has the room).
	ImagesUnread string
	// image is that default image ("" when this Mac has none).
	image string
	// Rest is everything else the preflight reads (the default image, the
	// disk, sleep). Its problems are ones no other part explains.
	Rest Layer
}

// macSetupTimeout is the most CheckMacSetup takes, whatever wedges: well
// under the 25 s a menu bar ask has; macSetupGrace is how long after it a
// command it ended may take to say so.
var (
	macSetupTimeout = 12 * time.Second
	macSetupGrace   = 3 * time.Second
)

// CheckMacSetup reads this Mac's preflight part by part (MacParts). What is
// not read within macSetupTimeout is left unread, never a verdict. run ends
// with its context (ExecRunner with a WaitDelay).
func CheckMacSetup(ctx context.Context, run Runner, f Facts) MacParts {
	ctx, cancel := context.WithTimeout(ctx, macSetupTimeout)
	defer cancel()
	done := make(chan MacParts, 1)
	go func() { done <- checkMacSetup(ctx, run, f) }()
	select {
	case p := <-done:
		return p
	case <-ctx.Done():
	}
	// Give what ended with ctx the moment it needs to say so; a file read
	// (a directory on a server that is gone) ends with nothing.
	select {
	case p := <-done:
		return p
	case <-time.After(macSetupGrace):
	}
	why := "this Mac could not be read within " + macSetupTimeout.String()
	return MacParts{Report: Report{OS: "darwin", Arch: "unknown", TartDiskUnread: why}, HVUnread: why, ArchUnread: why, MacOSUnread: why,
		Tart: Layer{Name: "tart", Unread: why, Problems: []string{why}}, Softnet: Layer{Name: "tart", Unread: why, Problems: []string{why}}}
}

func checkMacSetup(ctx context.Context, run Runner, f Facts) MacParts {
	r := Report{OS: "darwin"}
	p := macParts(ctx, run, f, &r)
	if p.ArchUnread+p.MacOSUnread != "" && r.TartFreeGB < float64(p.NeedGB) {
		p.ImagesUnread = "which image this Mac needs is not known"
	}
	if p.image != "" && p.Tart.CLI != "" && r.TartDiskUnread == "" && r.TartFreeGB < float64(p.NeedGB) {
		var err error
		if _, p.NeedGB, err = tartPullNeed(ctx, run, p.Tart.CLI, []string{p.image}); err != nil {
			p.ImagesUnread = err.Error()
		}
	}
	return p
}

// macParts reads the preflight's parts into r and the parts.
func macParts(ctx context.Context, run Runner, f Facts, r *Report) MacParts {
	var p MacParts
	p.Mac = Layer{Name: "tart"}
	res, err := runBounded(ctx, run, "sysctl", "-n", "kern.hv_support")
	if err != nil || res.Exit != 0 {
		p.HVUnread = commandFailure(res, err)
	}
	p.HV = p.HVUnread == "" && strings.TrimSpace(res.Stdout) == "1"
	if !p.HV {
		p.Mac.Problems = append(p.Mac.Problems, "Virtualization.framework is not available on this Mac")
	}
	p.ArchUnread, p.MacOSUnread = detectMacOS(ctx, run, r, &p.Mac)
	p.Tart = Layer{Name: "tart"}
	p.Hollow = detectTartCLI(ctx, run, f, &p.Tart)
	p.Softnet = Layer{Name: "tart"}
	detectSoftnet(ctx, run, f, *r, &p.Softnet)
	if m, ok := f.(managedSoftnetFacts); ok {
		if _, st, ok := m.ManagedSoftnet(); ok {
			p.SoftnetStore = &st
		}
	}
	p.Rest = Layer{Name: "tart"}
	p.NeedGB = tartDefaultFreeGB()
	if img, err := HostDefaultImage(*r, "tart"); err != nil {
		p.Rest.Warnings = append(p.Rest.Warnings, err.Error())
	} else {
		p.NeedGB, p.image = img.MinFreeGB, img.Image
		if img.Name == SmallLinux {
			p.Rest.Warnings = append(p.Rest.Warnings, "the default macOS guest is not supported on this host; the host default is the small Linux image (no GitHub Actions runner)")
		}
	}
	detectTartDisk(f, r, &p.Rest)
	detectSleep(ctx, run, &p.Rest)
	p.Report = *r
	return p
}

// layer is the parts merged into the one tart layer.
func (p MacParts) layer() Layer {
	l := p.Tart
	l.Isolation = "vm"
	l.Problems, l.Warnings = nil, nil
	tart, sn := p.Tart, p.Softnet
	if len(p.Mac.Problems) > 0 { // a Tart refused for this Mac is not lent: a hardening warning on top is noise
		tart.Warnings = nil
	}
	if len(p.Mac.Problems)+len(p.Tart.Problems) > 0 { // root fixing Softnet's permissions would not make the layer usable
		sn.RootSteps = nil
	}
	for _, part := range []Layer{p.Mac, tart, sn, p.Rest} {
		l.Problems = append(l.Problems, part.Problems...)
		l.Warnings = append(l.Warnings, part.Warnings...)
		l.RootSteps = append(l.RootSteps, part.RootSteps...)
	}
	l.SoftnetPath, l.SoftnetVersion, l.SoftnetBuild = p.Softnet.SoftnetPath, p.Softnet.SoftnetVersion, p.Softnet.SoftnetBuild
	l.SoftnetAlone = len(p.Softnet.Problems) > 0 && p.Softnet.Unread == "" && len(p.Mac.Problems)+len(p.Tart.Problems)+len(p.Rest.Problems) == 0
	l.Available = len(l.Problems) == 0
	return l
}
