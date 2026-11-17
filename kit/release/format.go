package release

import (
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Format is one release channel's names: the manifest schema a machine
// accepts, and Name, which starts every tarball
// (<Name>_<version>_<os>_<arch>.tar.gz) and is the program each must carry.
// A manifest of one format never verifies as another's, whatever key
// signed it.
type Format struct {
	Schema string
	Name   string
}

var (
	// ZM is zm's channel (zm's own release scripts build it, not this module).
	ZM = Format{Schema: Schema, Name: "zm"}
	// Potluck is Potluck's channel (scripts/release here), signed with its
	// own keys (internal/upgrade/release_keys.txt).
	Potluck = Format{Schema: "potluck-release/1", Name: "potluck"}
)

// Tarball is the release tarball name for a platform.
func (f Format) Tarball(version, goos, goarch string) string {
	return fmt.Sprintf("%s_%s_%s_%s.tar.gz", f.Name, version, goos, goarch)
}

// Unpack checks tarball against verified manifest m and writes its
// programs for goos/goarch into dir (Extract). It returns the names
// written.
func (f Format) Unpack(m Manifest, tarball []byte, goos, goarch, dir string) ([]string, error) {
	if err := m.CheckFile(f.Tarball(m.Version, goos, goarch), tarball); err != nil {
		return nil, err
	}
	return f.Extract(tarball, m.Version, goos, goarch, dir)
}

// SignDir reads the SHA256SUMS in dir and writes the signed manifest
// (ManifestFile, SignatureFile) beside it.
func (f Format) SignDir(dir, version, commit string, created time.Time, key ed25519.PrivateKey) (Manifest, error) {
	sums, err := os.ReadFile(filepath.Join(dir, "SHA256SUMS"))
	if err != nil {
		return Manifest{}, err
	}
	m, err := f.FromSums(version, commit, created, sums)
	if err != nil {
		return Manifest{}, err
	}
	body, sig, err := f.Sign(m, key)
	if err != nil {
		return Manifest{}, err
	}
	if err := os.WriteFile(filepath.Join(dir, ManifestFile), body, 0o644); err != nil {
		return Manifest{}, err
	}
	return m, os.WriteFile(filepath.Join(dir, SignatureFile), []byte(sig+"\n"), 0o644)
}

// NewKey makes a signing key pair: seed is the private key as a CI secret
// holds it (base64 of the 32-byte Ed25519 seed), public as config
// update_keys and the compiled-in key write it.
func NewKey() (seed, public string, err error) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		return "", "", err
	}
	return base64.StdEncoding.EncodeToString(priv.Seed()), FormatKey(pub), nil
}

// ParseSeed reads a private key as NewKey writes it.
func ParseSeed(s string) (ed25519.PrivateKey, error) {
	seed, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil || len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("a signing key is base64 of a %d-byte Ed25519 seed", ed25519.SeedSize)
	}
	return ed25519.NewKeyFromSeed(seed), nil
}
