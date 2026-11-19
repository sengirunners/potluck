package vmhost

import (
	"context"
	"os/exec"
	"regexp"
	"time"
)

// TartBootArguments is the production headless boot policy; preflight checks
// that the selected Tart understands each option before pulling an image,
// and refuses a Tart that lacks one.
func TartBootArguments(isolated bool) []string {
	a := []string{"--no-graphics", "--no-audio", "--no-clipboard"}
	if isolated {
		a = append(a, softnetArgs...)
	}
	return a
}

// TartHardening is a boot option that takes away something Tart gives a
// guest which exposes nothing of the host: a box gets it whenever the host's
// Tart knows it, and boots without it on an older Tart, which preflight
// warns about (never refuses).
type TartHardening struct {
	Flag  string
	Since string // the first Tart release that knows Flag
	// Without is what a box keeps when Tart does not know Flag, in plain words.
	Without string
}

// TartHardeningArguments are the hardening options, in boot order.
//
// --no-usb-accessories: Tart's "USB accessories" are a virtual USB keyboard
// and a virtual USB pointing device that Tart creates itself
// (Sources/tart/Platform/Darwin.swift and Linux.swift, keyboards and
// pointingDevices; Sources/tart/VM.swift, configureInputDevices, at Tart
// 2.38.0 through 2.41.1). Their input comes only from Tart's own window or
// VNC, which a box never has (--no-graphics, no --vnc). Tart attaches no other
// USB device (its disks are virtio) and Apple's Virtualization framework has no
// way to hand a host USB device to a guest, so none of the Mac's own USB
// devices reach a box with or without the flag; it is passed to drop the two
// virtual USB devices too. A macOS guest keeps Tart's Mac keyboard and
// trackpad, which are not USB.
func TartHardeningArguments() []TartHardening {
	return []TartHardening{{
		Flag:    "--no-usb-accessories",
		Since:   "2.38.0",
		Without: "Tart's own virtual USB keyboard and pointer, which no one can type into on a box with no window (none of this Mac's USB devices reach a box either way)",
	}}
}

// TartUpdateHint says how to get a newer Tart.
const TartUpdateHint = "download the latest tart.tar.gz from https://github.com/openai/tart/releases/latest and replace the tart on your PATH (brew upgrade tart, if Homebrew installed it)"

// TartHelpHas reports whether `tart run --help` output lists flag.
func TartHelpHas(help, flag string) bool {
	return regexp.MustCompile(`(?:^|[\s,])` + regexp.QuoteMeta(flag) + `(?:[=\s]|$)`).MatchString(help)
}

// tartHelpTimeout bounds the boot's read of `tart run --help`.
const tartHelpTimeout = 10 * time.Second

// hardening is the hardening options this Tart knows, read from its
// `run --help` at boot so a Tart updated since preflight is seen. It writes
// nothing to the box's log: a boot failure's words come from that log's last
// lines (diagnoseBoot), and the preflight warning already names what is
// missing.
func (t tartCLI) hardening() []string {
	ctx, cancel := context.WithTimeout(context.Background(), tartHelpTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, t.bin, "run", "--help").Output()
	if err != nil {
		return nil
	}
	var a []string
	for _, h := range TartHardeningArguments() {
		if TartHelpHas(string(out), h.Flag) {
			a = append(a, h.Flag)
		}
	}
	return a
}
