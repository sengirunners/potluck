//go:build !darwin && !linux

package runtarget

import "syscall"

// ctimeOf is unknown here: every driver call hashes the file (no cache).
func ctimeOf(*syscall.Stat_t) (int64, bool) { return 0, false }
