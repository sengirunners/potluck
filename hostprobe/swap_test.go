package hostprobe

import (
	"context"
	"strings"
	"testing"
)

func TestSwapNote(t *testing.T) {
	const list = "swapon --show=NAME,TYPE --noheadings --raw"
	for _, c := range []struct {
		name, os string
		out      map[string]Result
		want     string // a substring; "" wants no note
	}{
		{"mac, encrypted", "darwin", map[string]Result{"sysctl -n vm.swapusage": ok("total = 0.00M  used = 0.00M  free = 0.00M  (encrypted)\n")}, ""},
		{"mac, unreadable", "darwin", nil, "cannot tell"},
		{"linux, no swap", "linux", map[string]Result{list: ok("")}, ""},
		{"linux, zram only", "linux", map[string]Result{list: ok("/dev/zram0 partition\n")}, ""},
		{"linux, plain partition", "linux", map[string]Result{list: ok("/dev/sda2 partition\n"), "lsblk -nso TYPE /dev/sda2": ok("part\ndisk\n")}, "swap on /dev/sda2 is not encrypted"},
		{"linux, dm-crypt partition", "linux", map[string]Result{list: ok("/dev/dm-1 partition\n"), "lsblk -nso TYPE /dev/dm-1": ok("crypt\npart\ndisk\n")}, ""},
		{"linux, file on a plain disk", "linux", map[string]Result{list: ok("/swap.img file\n"), "findmnt -no SOURCE -T /swap.img": ok("/dev/vda1\n"),
			"lsblk -nso TYPE /dev/vda1": ok("part\ndisk\n")}, "swap on /swap.img is not encrypted"},
		{"linux, file on an encrypted root", "linux", map[string]Result{list: ok("/swapfile file\n"), "findmnt -no SOURCE -T /swapfile": ok("/dev/mapper/root\n"),
			"lsblk -nso TYPE /dev/mapper/root": ok("lvm\ncrypt\npart\ndisk\n")}, ""},
		{"linux, a btrfs swap file on dm-crypt", "linux", map[string]Result{list: ok("/swap/swapfile file\n"), "findmnt -no SOURCE -T /swap/swapfile": ok("/dev/mapper/root[/@swap]\n"),
			"lsblk -nso TYPE /dev/mapper/root": ok("crypt\npart\ndisk\n")}, ""},
		{"linux, no swapon", "linux", nil, "cannot list"},
		{"linux, lsblk fails", "linux", map[string]Result{list: ok("/dev/sdb1 partition\n")}, "cannot tell whether swap on /dev/sdb1"},
	} {
		got := swapNote(context.Background(), &fakeRunner{out: c.out}, c.os)
		if c.want == "" && got != "" || c.want != "" && !strings.Contains(got, c.want) {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}
