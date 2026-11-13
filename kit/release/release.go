// Package release is the signed release format (SPEC 2 "Release channel"):
// a manifest naming each release tarball's SHA-256, signed with an Ed25519
// key that only CI holds. Machines pin the public keys (config
// update_keys) and install only what verifies. Standard library only.
//
// Two channels use it, each with its own Format (schema, tarball names,
// main program) and its own key: zm's (the package-level functions, which
// keep zm's names) and Potluck's (Potluck, format.go).
package release

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Schema names the manifest format; a machine refuses any other.
const Schema = "zm-release/1"

// File names published with every release.
const (
	ManifestFile  = "manifest.json"
	SignatureFile = "manifest.json.sig"
)

// KeyPrefix starts a public key as config writes it: ed25519:<base64>.
const KeyPrefix = "ed25519:"

// ChannelKey is the release channel's public key (made 2026-11-05; its seed
// is only in the zm-release environment). zm daemon update enable pins it in
// config update_keys, as a fleet's update setup script does; Verify
// trusts only config keys. A rotation adds the new key here and in the farm.
const ChannelKey = "ed25519:Q7WSYs/BkhwIeBsx3hR9TEoRjjXU8Wmk+UY3P5PhFwc="

// Manifest is what CI signs.
type Manifest struct {
	Schema  string            `json:"schema"`
	Version string            `json:"version"`
	Commit  string            `json:"commit"`
	Created time.Time         `json:"created"`
	Files   map[string]string `json:"files"` // tarball name -> hex SHA-256
}

var (
	commitRE  = regexp.MustCompile(`^[0-9a-f]{40}$`)
	versionRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	sumRE     = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// Tarball is the release tarball name for a platform (scripts/release).
func Tarball(version, goos, goarch string) string { return ZM.Tarball(version, goos, goarch) }

// Check reports a manifest that is not a well formed zm manifest.
func (m Manifest) Check() error { return ZM.Check(m) }

// Check reports a manifest that is not well formed in f.
func (f Format) Check(m Manifest) error {
	switch {
	case m.Schema != f.Schema:
		return fmt.Errorf("manifest schema %q, want %q", m.Schema, f.Schema)
	case !versionRE.MatchString(m.Version):
		return fmt.Errorf("manifest version %q is not a version", m.Version)
	case !commitRE.MatchString(m.Commit):
		return fmt.Errorf("manifest commit %q is not a full commit", m.Commit)
	case m.Created.IsZero():
		return errors.New("manifest has no created time")
	case len(m.Files) == 0:
		return errors.New("manifest names no files")
	}
	for name, sum := range m.Files {
		if !strings.HasPrefix(name, f.Name+"_"+m.Version+"_") || !strings.HasSuffix(name, ".tar.gz") || strings.ContainsAny(name, `/\`) {
			return fmt.Errorf("manifest file %q is not a tarball of %s", name, m.Version)
		}
		if !sumRE.MatchString(sum) {
			return fmt.Errorf("manifest sum for %s is not SHA-256", name)
		}
	}
	return nil
}

// FromSums builds a zm manifest from a SHA256SUMS file (`<hex>  <name>` lines).
func FromSums(version, commit string, created time.Time, sums []byte) (Manifest, error) {
	return ZM.FromSums(version, commit, created, sums)
}

// FromSums builds a manifest in f from a SHA256SUMS file.
func (f Format) FromSums(version, commit string, created time.Time, sums []byte) (Manifest, error) {
	m := Manifest{Schema: f.Schema, Version: version, Commit: commit, Created: created.UTC().Truncate(time.Second), Files: map[string]string{}}
	for _, line := range strings.Split(strings.TrimSpace(string(sums)), "\n") {
		sum, name, ok := strings.Cut(strings.TrimSpace(line), "  ")
		if !ok {
			return Manifest{}, fmt.Errorf("SHA256SUMS line %q", line)
		}
		m.Files[strings.TrimPrefix(name, "*")] = sum
	}
	return m, f.Check(m)
}

// Sign returns the zm manifest's bytes and their signature (base64).
func Sign(m Manifest, key ed25519.PrivateKey) (body []byte, sig string, err error) {
	return ZM.Sign(m, key)
}

// Sign returns the manifest's bytes and their signature (base64).
func (f Format) Sign(m Manifest, key ed25519.PrivateKey) (body []byte, sig string, err error) {
	if err = f.Check(m); err != nil {
		return nil, "", err
	}
	if body, err = json.MarshalIndent(m, "", "  "); err != nil {
		return nil, "", err
	}
	body = append(body, '\n')
	return body, base64.StdEncoding.EncodeToString(ed25519.Sign(key, body)), nil
}

// ParseKey reads a public key as config writes it.
func ParseKey(s string) (ed25519.PublicKey, error) {
	b, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(strings.TrimSpace(s), KeyPrefix))
	if !strings.HasPrefix(strings.TrimSpace(s), KeyPrefix) || err != nil || len(b) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("update key %q is not %s<base64 of 32 bytes>", s, KeyPrefix)
	}
	return ed25519.PublicKey(b), nil
}

// FormatKey writes a public key as config reads it.
func FormatKey(pub ed25519.PublicKey) string {
	return KeyPrefix + base64.StdEncoding.EncodeToString(pub)
}

// Verify checks body's signature against any of keys and returns the zm
// manifest.
func Verify(body []byte, sig string, keys []string) (Manifest, error) {
	return ZM.Verify(body, sig, keys)
}

// Verify checks body's signature against any of keys and returns the
// manifest, which must be f's. The exact signed bytes are parsed, never a
// re-encoding.
func (f Format) Verify(body []byte, sig string, keys []string) (Manifest, error) {
	if len(keys) == 0 {
		return Manifest{}, errors.New("no update keys configured (config update_keys)")
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(sig))
	if err != nil || len(raw) != ed25519.SignatureSize {
		return Manifest{}, errors.New("manifest signature is not an Ed25519 signature")
	}
	ok := false
	for _, k := range keys {
		pub, err := ParseKey(k)
		if err != nil {
			return Manifest{}, err
		}
		if ed25519.Verify(pub, body, raw) {
			ok = true
			break
		}
	}
	if !ok {
		return Manifest{}, errors.New("manifest signature does not verify with any configured update key")
	}
	var m Manifest
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return Manifest{}, fmt.Errorf("manifest: %w", err)
	}
	return m, f.Check(m)
}

// CheckFile reports data that is not the manifest's file name.
func (m Manifest) CheckFile(name string, data []byte) error {
	want, ok := m.Files[name]
	if !ok {
		return fmt.Errorf("the release has no %s", name)
	}
	got := sha256.Sum256(data)
	if hex.EncodeToString(got[:]) != want {
		return fmt.Errorf("%s does not match the signed manifest", name)
	}
	return nil
}
