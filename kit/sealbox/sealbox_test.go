package sealbox

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// One test for every leg: any spelling encoding/json reads as secrets or
// jit_config, non-empty, or a body that is not an object.
func TestCarriesSecret(t *testing.T) {
	for body, want := range map[string]bool{
		``:                               false,
		`{"argv":["true"]}`:              false,
		`{"secrets":{}}`:                 false,
		`{"secrets":null}`:               false,
		`{"jit_config":""}`:              false,
		`null`:                           false,
		`{"secrets":{"A":"x"}}`:          true,
		`{"Secrets":{"A":"x"}}`:          true,
		`{"ſecrets":{"A":"x"}}`:          true,
		`{"SECRETS":{"A":"x"}}`:          true,
		`{"jit_config":"x"}`:             true,
		`{"JIT_CONFIG":"x"}`:             true,
		`{"secrets":{},"SECRETS":"x"}`:   true,
		`["secrets"]`:                    true,
		`not json`:                       true,
		`{"argv":["x"],"secrets":"str"}`: true,
		`{"unset":["API_KEY"]}`:          true,
		`{"UNSET":["API_KEY"]}`:          true,
		`{"unset":[]}`:                   false,
		`{"unset":null}`:                 false,
	} {
		if got := CarriesSecret([]byte(body)); got != want {
			t.Errorf("CarriesSecret(%s) = %v, want %v", body, got, want)
		}
	}
}

// A header pair names a key only when the certificate is that identity's.
func TestPeer(t *testing.T) {
	pub, id, _ := ed25519.GenerateKey(rand.Reader)
	k, err := LoadOrCreateKey(filepath.Join(t.TempDir(), "seal"))
	if err != nil {
		t.Fatal(err)
	}
	cert := Certify(id, k.PublicKey().Bytes())
	b64 := base64.RawURLEncoding.EncodeToString(pub)
	kid, seal, ok := Peer(b64, cert)
	if !ok || kid != KeyID(pub) || !bytes.Equal(seal, k.PublicKey().Bytes()) {
		t.Fatalf("own pair: %q %v", kid, ok)
	}
	other, _, _ := ed25519.GenerateKey(rand.Reader)
	for _, c := range [][2]string{
		{base64.RawURLEncoding.EncodeToString(other), cert}, // another identity
		{b64, ""}, {"", cert}, {"!!", cert}, {b64, cert + "x"},
	} {
		if _, _, ok := Peer(c[0], c[1]); ok {
			t.Errorf("pair %q taken", c)
		}
	}
	// Sealed to it, it opens with its key only, as to, before expiry.
	now := time.Now()
	sealed, err := Seal("zmkey:a", kid, seal, "p", time.Minute, []byte("pt"), now)
	if err != nil {
		t.Fatal(err)
	}
	if pt, err := Open(k, "zmkey:a", kid, "p", sealed, now, time.Minute); err != nil || string(pt) != "pt" {
		t.Fatalf("open: %q %v", pt, err)
	}
	if _, err := Open(k, "zmkey:a", kid, "p", sealed, now.Add(3*time.Minute), time.Minute); err != ErrOpen {
		t.Fatalf("expired: %v", err)
	}
}

// A key file group or others may read is refused, naming the fix.
func TestKeyFileMode(t *testing.T) {
	p := filepath.Join(t.TempDir(), "seal")
	if _, err := LoadOrCreateKey(p); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreateKey(p); err == nil || !strings.Contains(err.Error(), "chmod 600 "+p) {
		t.Fatalf("group-readable key: %v", err)
	}
	if err := os.Chmod(p, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreateKey(p); err != nil {
		t.Fatalf("after chmod: %v", err)
	}
}
