// Package hostswap reads whether this machine's swap could put a box's
// memory, the secrets in its memory filesystem included, on disk, and says
// what the authority does with the answer. A host reports the state in its
// advert (computeapi.RunTargetDescribe.Swap); potluck doctor prints
// it; the authority gives box secrets only to a host whose state is safe.
package hostswap

import (
	"context"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// States a host reports. Anything else, and no state at all (a host on a
// build from before this field), is not safe.
const (
	Off       = "off"        // no swap
	Zram      = "zram"       // swap in memory only
	Encrypted = "encrypted"  // swap on a dm-crypt device (a Mac's swap always is)
	PlainDisk = "plain-disk" // swap on a disk that is not encrypted
	Unknown   = "unknown"    // the machine would not say
)

// Fix is the three ways to make a host's swap safe, in plain words.
const Fix = "turn swap off (swapoff -a, and remove it from /etc/fstab), use zram, or encrypt swap (dm-crypt)"

// Safe says whether a box's memory stays off disk on a host reporting state.
func Safe(state string) bool {
	return state == Off || state == Zram || state == Encrypted
}

// Result is one command's answer; Runner runs one command. A command that
// ran and exited non-zero is a Result with Exit set and a nil error; the
// error is for "could not start".
type Result struct {
	Stdout string
	Exit   int
}

type Runner interface {
	Run(ctx context.Context, name string, args ...string) (Result, error)
}

// Reading is what Read found: the state, and, when it is not safe, a note
// in plain words for the owner ("" when it is safe).
type Reading struct {
	State string
	Note  string
}

// Read reads the machine's swap. osName is runtime.GOOS's word for it.
func Read(ctx context.Context, run Runner, osName string) Reading {
	switch osName {
	case "darwin":
		res, err := run.Run(ctx, "sysctl", "-n", "vm.swapusage")
		if err == nil && res.Exit == 0 && strings.Contains(res.Stdout, "(encrypted)") {
			return Reading{State: Encrypted}
		}
		return Reading{State: Unknown, Note: "swap: cannot tell whether this Mac's swap is encrypted (sysctl vm.swapusage); a box's memory, its secrets included, may be written to disk"}
	case "linux":
		return readLinux(ctx, run)
	}
	return Reading{State: Unknown, Note: "swap: not checked on " + osName + "; a box's memory, its secrets included, may be written to disk"}
}

func readLinux(ctx context.Context, run Runner) Reading {
	res, err := run.Run(ctx, "swapon", "--show=NAME,TYPE", "--noheadings", "--raw")
	if err != nil || res.Exit != 0 {
		return Reading{State: Unknown, Note: "swap: cannot list this machine's swap (swapon --show); if it swaps to an unencrypted disk, a box's memory, its secrets included, can be written there; " + Fix}
	}
	var plain, unknown []string
	var zram, crypt bool
	for _, line := range strings.Split(strings.TrimSpace(res.Stdout), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		if strings.HasPrefix(f[0], "/dev/zram") {
			zram = true
			continue
		}
		dev := f[0]
		if f[1] == "file" {
			src, err := run.Run(ctx, "findmnt", "-no", "SOURCE", "-T", f[0])
			dev, _, _ = strings.Cut(strings.TrimSpace(src.Stdout), "[") // btrfs: /dev/sda2[/@swap]
			if err != nil || src.Exit != 0 || dev == "" {
				unknown = append(unknown, f[0])
				continue
			}
		}
		// The device and every device under it: one is a dm-crypt mapping when the swap is encrypted.
		types, err := run.Run(ctx, "lsblk", "-nso", "TYPE", dev)
		switch {
		case err != nil || types.Exit != 0:
			unknown = append(unknown, f[0])
		case !hasLine(types.Stdout, "crypt"):
			plain = append(plain, f[0])
		default:
			crypt = true
		}
	}
	switch {
	case len(plain) > 0:
		return Reading{State: PlainDisk, Note: "swap on " + strings.Join(plain, ", ") + " is not encrypted: a box's memory, its secrets included, can be written to that disk; " + Fix}
	case len(unknown) > 0:
		return Reading{State: Unknown, Note: "swap: cannot tell whether swap on " + strings.Join(unknown, ", ") + " is encrypted; a box's memory, its secrets included, may be written to disk; " + Fix}
	case crypt:
		return Reading{State: Encrypted}
	case zram:
		return Reading{State: Zram}
	}
	return Reading{State: Off}
}

// hasLine says whether one of out's lines, trimmed, is s.
func hasLine(out, s string) bool {
	for _, l := range strings.Split(out, "\n") {
		if strings.TrimSpace(l) == s {
			return true
		}
	}
	return false
}

// Local reads this machine. Tests replace it.
var Local = func(ctx context.Context) Reading { return Read(ctx, ExecRunner{}, runtime.GOOS) }

// ExecRunner runs the real commands, looking in sbin too. A daemon's advert
// and potluck doctor read this machine through it, so they cannot disagree.
type ExecRunner struct{}

// sbin is where swapon, and sometimes lsblk and findmnt, live: not on the
// PATH of a daemon started by a login that is not root's.
var sbin = []string{"/usr/sbin", "/sbin"}

func (ExecRunner) Run(ctx context.Context, name string, args ...string) (Result, error) {
	path, err := exec.LookPath(name)
	for _, dir := range sbin {
		if err == nil {
			break
		}
		path, err = exec.LookPath(filepath.Join(dir, name))
	}
	if err != nil {
		return Result{}, err
	}
	cmd := exec.CommandContext(ctx, path, args...)
	var out strings.Builder
	cmd.Stdout = &out
	err = cmd.Run()
	res := Result{Stdout: out.String()}
	if ee, ok := err.(*exec.ExitError); ok {
		res.Exit = ee.ExitCode()
		return res, nil
	}
	return res, err
}
