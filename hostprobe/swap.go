package hostprobe

import (
	"context"
	"strings"
)

// A box's memory, the secrets in its memory filesystem included, can be
// paged out to the host's swap. Exec refuses secrets in a guest that swaps
// to disk (vmhost/exec.go), but the host's own swap is the host owner's: on
// a trusted host it should be encrypted. swapNote says, in plain words,
// when it is not or cannot be told; "" when there is no swap, it is
// encrypted, or it is zram (memory).
func swapNote(ctx context.Context, run Runner, osName string) string {
	switch osName {
	case "darwin":
		res, err := run.Run(ctx, "sysctl", "-n", "vm.swapusage")
		if err == nil && res.Exit == 0 && strings.Contains(res.Stdout, "(encrypted)") {
			return ""
		}
		return "swap: cannot tell whether this Mac's swap is encrypted (sysctl vm.swapusage); a box's memory, its secrets included, may be written to disk"
	case "linux":
	default:
		return ""
	}
	res, err := run.Run(ctx, "swapon", "--show=NAME,TYPE", "--noheadings", "--raw")
	if err != nil || res.Exit != 0 {
		return "swap: cannot list this machine's swap (swapon --show); if it swaps to an unencrypted disk, a box's memory, its secrets included, can be written there"
	}
	var plain, unknown []string
	for _, line := range strings.Split(strings.TrimSpace(res.Stdout), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 || strings.HasPrefix(f[0], "/dev/zram") {
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
		}
	}
	switch {
	case len(plain) > 0:
		return "swap on " + strings.Join(plain, ", ") + " is not encrypted: a box's memory, its secrets included, can be written to that disk; encrypt swap (dm-crypt) or use zram"
	case len(unknown) > 0:
		return "swap: cannot tell whether swap on " + strings.Join(unknown, ", ") + " is encrypted; a box's memory, its secrets included, may be written to disk"
	}
	return ""
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
