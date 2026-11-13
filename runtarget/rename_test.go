package runtarget_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sengirunners/potluck/runtarget"
)

// The daemon runs potluck-driver-<name>, else zm-run-<name> (what zm's
// release and an old updater install), from its fixed directories only, in
// order; never from PATH, however early PATH has one.
func TestFindDriverNames(t *testing.T) {
	a, b, onPath := t.TempDir(), t.TempDir(), t.TempDir()
	t.Setenv("PATH", onPath)
	write := func(dir, name string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	write(onPath, "potluck-driver-x")
	dirs := []string{a, b}
	if _, err := runtarget.FindDriver(dirs, "x"); err == nil || !strings.Contains(err.Error(), "never from PATH") {
		t.Fatalf("PATH shadowing: %v", err)
	}
	inB := write(b, "potluck-driver-x")
	if p, err := runtarget.FindDriver(dirs, "x"); err != nil || p != inB {
		t.Fatalf("second dir: %q %v", p, err)
	}
	old := write(a, "zm-run-x")
	if p, err := runtarget.FindDriver(dirs, "x"); err != nil || p != old {
		t.Fatalf("old name in the first dir: %q %v", p, err)
	}
	nw := write(a, "potluck-driver-x")
	if p, err := runtarget.FindDriver(dirs, "x"); err != nil || p != nw {
		t.Fatalf("both names: %q %v", p, err)
	}
	for _, bad := range []string{"../x", "a/b", ".x", "-x", "X", ""} {
		if _, err := runtarget.FindDriver(dirs, bad); err == nil {
			t.Errorf("name %q was looked up", bad)
		}
	}
}
