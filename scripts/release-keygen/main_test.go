package main

import (
	"bytes"
	"crypto/ed25519"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sengirunners/potluck/kit/release"
)

// The seed goes only to a new 0600 file; what is printed is the path and the
// public key, which matches the seed.
func TestKeygenNeverPrintsTheSeed(t *testing.T) {
	var out bytes.Buffer
	if err := run(&out, ""); err == nil {
		t.Fatal("ran with no -seed-file")
	}
	path := filepath.Join(t.TempDir(), "seed")
	if err := run(&out, path); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	seed := strings.TrimSpace(string(b))
	if strings.Contains(out.String(), seed) {
		t.Fatal("the seed was printed")
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Errorf("seed file mode %v, want 0600", fi.Mode().Perm())
	}
	key, err := release.ParseSeed(seed)
	if err != nil {
		t.Fatal(err)
	}
	if pub := release.FormatKey(key.Public().(ed25519.PublicKey)); !strings.Contains(out.String(), pub) {
		t.Errorf("output %q does not name the public key %s", out.String(), pub)
	}
	if err := run(&out, path); err == nil {
		t.Fatal("overwrote an existing seed file")
	}
}
