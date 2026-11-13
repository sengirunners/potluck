package release

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// Potluck's channel is its own: its manifests, tarball names and main
// program, and a manifest of one channel never verifies as the other's.
func TestPotluckFormatIsItsOwn(t *testing.T) {
	seed, pub, err := NewKey()
	if err != nil {
		t.Fatal(err)
	}
	key, err := ParseSeed(seed)
	if err != nil {
		t.Fatal(err)
	}
	if FormatKey(key.Public().(ed25519.PublicKey)) != pub {
		t.Fatal("NewKey's public key is not its seed's")
	}
	if _, err := ParseSeed("not a seed"); err == nil {
		t.Fatal("ParseSeed took a bad seed")
	}

	dir := t.TempDir()
	name := Potluck.Tarball("main-abc", "linux", "amd64")
	if name != "potluck_main-abc_linux_amd64.tar.gz" {
		t.Fatalf("tarball %q", name)
	}
	tb := tarball(t, "potluck_main-abc_linux_amd64", map[string]string{"potluck": "p", "potluck-driver-local": "d", "README.md": "r"})
	sum := sha256.Sum256(tb)
	if err := os.WriteFile(filepath.Join(dir, "SHA256SUMS"), []byte(hex.EncodeToString(sum[:])+"  "+name+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := Potluck.SignDir(dir, "main-abc", commit, time.Now(), key)
	if err != nil {
		t.Fatal(err)
	}
	if m.Schema != "potluck-release/1" {
		t.Fatalf("schema %q", m.Schema)
	}
	body, _ := os.ReadFile(filepath.Join(dir, ManifestFile))
	sig, _ := os.ReadFile(filepath.Join(dir, SignatureFile))
	if _, err := Potluck.Verify(body, string(sig), []string{pub}); err != nil {
		t.Fatalf("verify: %v", err)
	}
	if _, err := Verify(body, string(sig), []string{pub}); err == nil || !strings.Contains(err.Error(), "schema") {
		t.Fatalf("zm's channel took a potluck manifest: %v", err)
	}
	_, other, _ := NewKey()
	if _, err := Potluck.Verify(body, string(sig), []string{other}); err == nil {
		t.Fatal("a manifest verified with a key that did not sign it")
	}

	out := t.TempDir()
	got, err := Potluck.Unpack(m, tb, "linux", "amd64", out)
	slices.Sort(got) // the tarball helper writes its map in any order
	if err != nil || strings.Join(got, ",") != "potluck,potluck-driver-local" {
		t.Fatalf("unpack: %v %v", got, err)
	}
	bad := append([]byte(nil), tb...)
	bad[len(bad)-1] ^= 1
	if _, err := Potluck.Unpack(m, bad, "linux", "amd64", t.TempDir()); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("a tampered tarball unpacked: %v", err)
	}
	if _, err := Potluck.Unpack(m, tb, "darwin", "arm64", t.TempDir()); err == nil {
		t.Fatal("unpacked a platform the release does not name")
	}
	noMain := tarball(t, "potluck_v1_linux_amd64", map[string]string{"zm": "z"})
	if _, err := Potluck.Extract(noMain, "v1", "linux", "amd64", t.TempDir()); err == nil {
		t.Fatal("a potluck release with no potluck extracted")
	}
}
