package vmhost

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/sengirunners/potluck/softnet"
)

// A boot that pins the managed Softnet checks the build again: one
// withdrawn after preflight pinned it never starts.
func TestBootRefusesAWithdrawnManagedSoftnet(t *testing.T) {
	dir := t.TempDir()
	content := "#!/bin/sh\nexit 0\n"
	sum := sha256.Sum256([]byte(content))
	b := softnet.Build{ID: "1.0-test", Version: "softnet 1.0-test", SHA256: hex.EncodeToString(sum[:]), Bytes: int64(len(content)),
		Arch: "arm64", MinMacOS: 15, MaxMacOS: 15, LicenseSHA256: softnet.Builds[0].LicenseSHA256}
	oldBuilds, oldStore := softnet.Builds, managedSoftnet
	t.Cleanup(func() { softnet.Builds, managedSoftnet = oldBuilds, oldStore })
	softnet.Builds = []softnet.Build{b}
	store := softnet.Store{Dir: filepath.Join(dir, "store", "Softnet"), Owner: os.Geteuid(), Group: os.Getegid(), Top: dir}
	managedSoftnet = func() softnet.Store { return store }
	src := filepath.Join(dir, "softnet-download")
	if err := os.WriteFile(src, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Install(src, b, func(string, softnet.Build) error { return nil }); err != nil {
		t.Fatal(err)
	}
	tart := filepath.Join(dir, "tart")
	resolved := filepath.Join(dir, "resolved")
	script := "#!/bin/sh\nif [ \"$1\" = run ]; then command -v softnet > " + strconv.Quote(resolved) + "; fi\n"
	if err := os.WriteFile(tart, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd, held, err := (tartCLI{bin: tart, softnetPath: store.Current()}).Boot("fake-vm", true, io.Discard)
	if err != nil {
		t.Fatalf("sound managed build: %v", err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	held.Close()
	// Tart runs the build just checked, not whatever current names later.
	if got, _ := os.ReadFile(resolved); strings.TrimSpace(string(got)) != store.Build(b.ID) {
		t.Fatalf("tart resolved softnet %q, want %s", got, store.Build(b.ID))
	}
	softnet.Builds[0].Withdrawn = "it lets a box reach the LAN"
	if _, _, err := (tartCLI{bin: tart, softnetPath: store.Current()}).Boot("fake-vm", true, io.Discard); err == nil || !strings.Contains(err.Error(), "withdrawn: it lets a box reach the LAN") {
		t.Fatalf("withdrawn managed build: %v", err)
	}
}
