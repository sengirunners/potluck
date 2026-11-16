package hostswap

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

type fakeRunner struct{ out map[string]Result }

func (f fakeRunner) Run(_ context.Context, name string, args ...string) (Result, error) {
	line := strings.Join(append([]string{name}, args...), " ")
	if res, ok := f.out[line]; ok {
		return res, nil
	}
	return Result{}, fmt.Errorf("fake: no such command %s", name)
}

func ok(s string) Result { return Result{Stdout: s} }

func TestRead(t *testing.T) {
	const list = "swapon --show=NAME,TYPE --noheadings --raw"
	for _, c := range []struct {
		name, os string
		out      map[string]Result
		state    string
		note     string // a substring; "" wants no note
	}{
		{"mac, encrypted", "darwin", map[string]Result{"sysctl -n vm.swapusage": ok("total = 0.00M  used = 0.00M  free = 0.00M  (encrypted)\n")}, Encrypted, ""},
		{"mac, unreadable", "darwin", nil, Unknown, "cannot tell"},
		{"another OS", "plan9", nil, Unknown, "not checked"},
		{"linux, no swap", "linux", map[string]Result{list: ok("")}, Off, ""},
		{"linux, zram only", "linux", map[string]Result{list: ok("/dev/zram0 partition\n")}, Zram, ""},
		{"linux, plain partition", "linux", map[string]Result{list: ok("/dev/sda2 partition\n"), "lsblk -nso TYPE /dev/sda2": ok("part\ndisk\n")}, PlainDisk, "swap on /dev/sda2 is not encrypted"},
		{"linux, plain next to zram", "linux", map[string]Result{list: ok("/dev/zram0 partition\n/dev/sda2 partition\n"), "lsblk -nso TYPE /dev/sda2": ok("part\ndisk\n")}, PlainDisk, "swap on /dev/sda2"},
		{"linux, dm-crypt partition", "linux", map[string]Result{list: ok("/dev/dm-1 partition\n"), "lsblk -nso TYPE /dev/dm-1": ok("crypt\npart\ndisk\n")}, Encrypted, ""},
		{"linux, file on a plain disk", "linux", map[string]Result{list: ok("/swap.img file\n"), "findmnt -no SOURCE -T /swap.img": ok("/dev/vda1\n"),
			"lsblk -nso TYPE /dev/vda1": ok("part\ndisk\n")}, PlainDisk, "swap on /swap.img is not encrypted"},
		{"linux, file on an encrypted root", "linux", map[string]Result{list: ok("/swapfile file\n"), "findmnt -no SOURCE -T /swapfile": ok("/dev/mapper/root\n"),
			"lsblk -nso TYPE /dev/mapper/root": ok("lvm\ncrypt\npart\ndisk\n")}, Encrypted, ""},
		{"linux, a btrfs swap file on dm-crypt", "linux", map[string]Result{list: ok("/swap/swapfile file\n"), "findmnt -no SOURCE -T /swap/swapfile": ok("/dev/mapper/root[/@swap]\n"),
			"lsblk -nso TYPE /dev/mapper/root": ok("crypt\npart\ndisk\n")}, Encrypted, ""},
		{"linux, no swapon", "linux", nil, Unknown, "cannot list"},
		{"linux, lsblk fails", "linux", map[string]Result{list: ok("/dev/sdb1 partition\n")}, Unknown, "cannot tell whether swap on /dev/sdb1"},
	} {
		got := Read(context.Background(), fakeRunner{c.out}, c.os)
		if got.State != c.state {
			t.Errorf("%s: state %q, want %q", c.name, got.State, c.state)
		}
		if c.note == "" && got.Note != "" || c.note != "" && !strings.Contains(got.Note, c.note) {
			t.Errorf("%s: note %q, want %q", c.name, got.Note, c.note)
		}
		if Safe(got.State) != (got.Note == "") {
			t.Errorf("%s: Safe(%q) and the note disagree (%q)", c.name, got.State, got.Note)
		}
	}
}

// A refusal's fix is in every note of a linux host that has one.
func TestNotesNameTheFix(t *testing.T) {
	got := Read(context.Background(), fakeRunner{map[string]Result{"swapon --show=NAME,TYPE --noheadings --raw": ok("/dev/sda2 partition\n"), "lsblk -nso TYPE /dev/sda2": ok("part\n")}}, "linux")
	for _, w := range []string{"swapoff", "zram", "dm-crypt"} {
		if !strings.Contains(got.Note, w) {
			t.Errorf("note %q lacks %q", got.Note, w)
		}
	}
}

func TestSafe(t *testing.T) {
	for state, want := range map[string]bool{Off: true, Zram: true, Encrypted: true, PlainDisk: false, Unknown: false, "": false, "fast": false} {
		if Safe(state) != want {
			t.Errorf("Safe(%q) = %t", state, !want)
		}
	}
}
