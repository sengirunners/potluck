package vmhost

// The lender's dials (zm-eakh.12.97.13): the host's owner turns down, or
// back up, how much of the machine Potluck's boxes may use, from the menu
// bar, with no setup run again. One host-wide file beside the host override
// (hostmode.go), host-lend.json, holds {cores, memory_mb, keep_free_gb,
// since}; `potluck host lend` writes it (SetLend) and every binding's driver
// reads it when it starts a box:
//
//	cores        the most cores the boxes of one state directory hold at once
//	memory_mb    the most memory they hold at once
//	keep_free_gb no box starts, and no kept disk, volume or shelf grows, while
//	             the disk under the layer's disk directory has less free
//
// Zero, or no file, leaves that dial as the sizes were shared. A dial lower
// than what runs now starts nothing new until the boxes that run are under
// it; they finish as they would (the host switch's Stop now ends them). The
// file is read with the override's rules (readOwned): one that cannot be
// trusted refuses new work, so a lender's limit is never lost to a typo.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// HostLend is the lender's dials as written; a zero dial is as shared.
type HostLend struct {
	Cores      int       `json:"cores,omitempty"`
	MemoryMB   int       `json:"memory_mb,omitempty"`
	KeepFreeGB int       `json:"keep_free_gb,omitempty"`
	Since      time.Time `json:"since"`
}

// maxLend bounds each dial: no machine has more, and a typo is refused.
const (
	maxLendCores    = 4096
	maxLendMemoryMB = 64 << 20 // 64 TiB
	maxLendDiskGB   = 1 << 20  // 1 PiB
)

func (l HostLend) check() error {
	switch {
	case l.Cores < 0 || l.Cores > maxLendCores:
		return fmt.Errorf("cores %d is not 0..%d", l.Cores, maxLendCores)
	case l.MemoryMB < 0 || l.MemoryMB > maxLendMemoryMB:
		return fmt.Errorf("memory_mb %d is not 0..%d", l.MemoryMB, maxLendMemoryMB)
	case l.KeepFreeGB < 0 || l.KeepFreeGB > maxLendDiskGB:
		return fmt.Errorf("keep_free_gb %d is not 0..%d", l.KeepFreeGB, maxLendDiskGB)
	case l.Since.IsZero() && (l != HostLend{}):
		return errors.New("no since")
	}
	return nil
}

// hostLendPath is the dials' file: beside the override, so whatever moves
// one (the tests' TestMain) moves both.
func hostLendPath() string { return filepath.Join(filepath.Dir(hostOverridePath), "host-lend.json") }

// readLend reads the dials, which owner (or root) must own with their
// directory: no file is all as shared.
func readLend(path string, owner int) (HostLend, error) {
	b, ok, err := readOwned(path, owner)
	if err != nil || !ok {
		return HostLend{}, err
	}
	var l HostLend
	if err := decodeOne(path, b, &l); err != nil {
		return HostLend{}, err
	}
	if err := l.check(); err != nil {
		return HostLend{}, fmt.Errorf("%s: %v", path, err)
	}
	return l, nil
}

// lend is the dials as this binding reads them, with why they cannot be
// read in the words a refusal gives.
func (c config) lend() (HostLend, error) {
	l, err := readLend(hostLendPath(), c.HostOwner)
	if err != nil {
		return HostLend{}, fmt.Errorf("the lender's limits cannot be read, so no new work starts: %v", err)
	}
	return l, nil
}

// LendNow is the dials as a binding with owner (host_owner_uid) reads them
// now, for a program that links vmhost.
func LendNow(owner int) (HostLend, error) { return readLend(hostLendPath(), owner) }

// SetLend writes l as this host's dials, as the file's owner, and answers
// them as written and read back. All dials zero removes the file.
func SetLend(l HostLend) (HostLend, error) {
	path, me := hostLendPath(), os.Getuid()
	if l == (HostLend{Since: l.Since}) {
		if err := ownedDir(path, me); err != nil {
			return HostLend{}, err
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return HostLend{}, err
		}
		return HostLend{}, nil
	}
	if l.Since.IsZero() {
		l.Since = time.Now().UTC().Truncate(time.Second)
	}
	if err := l.check(); err != nil {
		return HostLend{}, err
	}
	if err := ownedDir(path, me); err != nil {
		return HostLend{}, err
	}
	b, err := json.Marshal(l)
	if err != nil {
		return HostLend{}, err
	}
	if err := writeOwned(path, b); err != nil {
		return HostLend{}, err
	}
	got, err := readLend(path, me)
	if err != nil {
		return HostLend{}, fmt.Errorf("wrote %s, but it does not read back: %v", path, err)
	}
	return got, nil
}

// Summary says in a line what the dials now lend.
func (l HostLend) Summary() string {
	cores, mem, disk := "all the cores shared", "all the memory shared", "no disk kept free for you beyond Potluck's own floor for kept disks"
	if l.Cores > 0 {
		cores = "at most " + amount("cores", l.Cores)
	}
	if l.MemoryMB > 0 {
		mem = "at most " + amount("memory", l.MemoryMB)
	}
	if l.KeepFreeGB > 0 {
		disk = fmt.Sprintf("%d GB of disk kept free for you", l.KeepFreeGB)
	}
	return "lending " + cores + " and " + mem + "; " + disk
}
