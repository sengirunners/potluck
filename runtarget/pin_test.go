package runtarget

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestPinDriverWritesAndRefreshesDigest(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, "drivers")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "potluck-driver-fixture")
	expectedPath, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, content := range []string{"#!/bin/sh\nexit 0\n", "#!/bin/sh\nexit 1\n"} {
		if err := os.WriteFile(file, []byte(content), 0700); err != nil {
			t.Fatal(err)
		}
		pinned, err := PinDriver(home, []string{dir}, "fixture")
		want := DriverPin{Path: filepath.Join(expectedPath, "potluck-driver-fixture"), SHA256: fmt.Sprintf("%x", sha256.Sum256([]byte(content)))}
		if err != nil || pinned.Path != want.Path || pinned.SHA256 != want.SHA256 || pinned.Pinned == "" {
			t.Fatalf("PinDriver = %+v, %v, want %+v", pinned, err, want)
		}
		pins, err := readPins(home)
		if err != nil || len(pins) != 1 || pins["fixture"] != pinned {
			t.Fatalf("stored pins = %+v, %v, want %+v", pins, err, want)
		}
	}
	before, err := readPins(home)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(file, 0770); err != nil {
		t.Fatal(err)
	}
	if _, err := PinDriver(home, []string{dir}, "fixture"); err == nil {
		t.Fatal("pinned a group-writable driver")
	}
	if _, err := PinDriver(home, []string{dir}, "missing"); err == nil {
		t.Fatal("pinned a missing driver")
	}
	after, err := readPins(home)
	if err != nil || len(after) != len(before) || after["fixture"] != before["fixture"] {
		t.Fatalf("refusals changed pins: %v, %v", after, err)
	}
}
