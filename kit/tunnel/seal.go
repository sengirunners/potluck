package tunnel

// Sealed secrets (SPEC 17.11 "Sealed secrets", zm-eakh.12.23.17,
// zm-eakh.12.48.2): this daemon's seal key and its certificate, and the
// tunnel's use of kit/sealbox, the one sealing primitive every leg uses. Each
// daemon's X25519 key ($ZM_HOME/tunnel/seal, 0600, made on first use) is
// apart from its ed25519 identity key, which certifies it. Over the tunnel a
// peer learns that certificate only on a request or answer the same identity
// signed, and checks it against that identity, so the key a secret is sealed
// to is always the one the key-bound token (or the pinned home) has.

import (
	"bytes"
	"crypto/ed25519"
	"crypto/hpke"
	"encoding/base64"
	"fmt"
	"net/http"
	"path/filepath"
	"time"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/sealbox"
)

// SealKeyFile is this daemon's sealing key under $ZM_HOME: the hex of an
// X25519 private key, 0600, made on first use. It is not escrowed: a home
// restored without it makes a new one and certifies it with the (escrowed)
// identity key; members take the new certificate from the next answer.
const SealKeyFile = "tunnel/seal"

// sealKey is this daemon's sealing key, read or made on first use.
func (s *Service) sealKey() (hpke.PrivateKey, error) {
	s.keyMu.Lock()
	defer s.keyMu.Unlock()
	if s.sk == nil {
		k, err := sealbox.LoadOrCreateKey(filepath.Join(s.d.Config.Home, SealKeyFile))
		if err != nil {
			return nil, err
		}
		s.sk = k
	}
	return s.sk, nil
}

// SealCert is this daemon's Zm-Seal-Key: its seal key, certified by its
// identity key. "" while sealing is off (a test standing in for an older zm)
// or either key cannot be read.
func (s *Service) SealCert() string {
	if s.sealOff.Load() {
		return ""
	}
	id, err := s.key()
	if err != nil {
		return ""
	}
	k, err := s.sealKey()
	if err != nil {
		return ""
	}
	s.keyMu.Lock()
	defer s.keyMu.Unlock()
	if s.cert == "" {
		s.cert = sealbox.Certify(id, k.PublicKey().Bytes())
	}
	return s.cert
}

// SealIdentity is this daemon's identity public key (base64url) and its
// Zm-Seal-Key, for a leg whose requests and answers are not signed with the
// identity (the tailnet, where the pinned node vouches for both). "", "",
// nil while sealing is off (a test standing in for a build from before
// sealing); an error when a key cannot be read, which is this machine's fault
// to fix, never a reason to send in the clear.
func (s *Service) SealIdentity() (string, string, error) {
	if s.sealOff.Load() {
		return "", "", nil
	}
	id, err := s.key()
	if err != nil {
		return "", "", fmt.Errorf("this machine's identity key cannot be read: %w", err)
	}
	if _, err := s.sealKey(); err != nil {
		return "", "", fmt.Errorf("this machine's seal key cannot be read: %w", err)
	}
	cert := s.SealCert()
	if cert == "" {
		return "", "", nil // turned off meanwhile
	}
	return base64.RawURLEncoding.EncodeToString(id.Public().(ed25519.PublicKey)), cert, nil
}

// SealOff makes this daemon behave like one from before sealing (tests: an
// older peer): it certifies no seal key on its requests and answers, seals
// nothing and opens nothing. It returns the undo.
func (s *Service) SealOff() func() {
	s.sealOff.Store(true)
	return func() { s.sealOff.Store(false) }
}

// sealTo seals for the daemon with key id peer, whose certified seal key is
// pub.
func (s *Service) sealTo(peer string, pub []byte, purpose string, ttl time.Duration, plaintext []byte) (string, error) {
	if s.sealOff.Load() || len(pub) == 0 {
		return "", sealbox.ErrNoKey("the receiving machine")
	}
	own, err := s.KeyID()
	if err != nil {
		return "", err
	}
	return sealbox.Seal(own, peer, pub, purpose, ttl, plaintext, nowFunc())
}

// openFrom opens what the daemon with key id from sealed for this one.
func (s *Service) openFrom(from, purpose, sealed string) ([]byte, error) {
	if s.sealOff.Load() {
		return nil, sealbox.ErrOpen
	}
	k, err := s.sealKey()
	if err != nil {
		return nil, err
	}
	own, err := s.KeyID()
	if err != nil {
		return nil, err
	}
	return sealbox.Open(k, from, own, purpose, sealed, nowFunc(), MaxSkew)
}

// requestPurpose binds a request's sealed body to that one signed
// request: its method, URI, date and nonce (the door refuses a nonce seen
// before, so the blob cannot ride another request either).
func requestPurpose(method, uri, date, nonce string) string {
	return sealbox.RequestPurpose(method, normURI(uri), date, nonce)
}

// secretInClear reports whether a run-target body holds secrets or a
// jit_config (sealbox.CarriesSecret): the tunnel never carries such a body
// unsealed, whoever forgot to ask for sealing.
func secretInClear(path string, body []byte) bool {
	return sealbox.RunTargetPath(path) && sealbox.CarriesSecret(body) || grantInClear(path, body)
}

// grantInClear is a secret grant's body, which names a secret.
func grantInClear(path string, body []byte) bool {
	return path == SecretGrantsPath && len(bytes.TrimSpace(body)) > 0
}

// errClear is the refusal of a body that crossed, or would cross, in the
// clear: who runs a build too old to seal; a grant's says the grant route
// needs sealing.
func errClear(path string, body []byte, who string) error {
	if grantInClear(path, body) {
		return api.Errorf(api.CodeForbidden, "a secret grant names a secret, so it goes between machines only sealed to the receiving machine's key, and %s runs a Potluck (or zm) too old to seal it: update it", who).
			WithHint("update the daemon there (potluck upgrade, or zm daemon update); listing and removing grants still work")
	}
	return sealbox.ErrNoKey(who)
}

// SecretGrantsPath is the secret grants route: a body sent there names a
// secret, so it crosses a tunnel only sealed.
const SecretGrantsPath = "/v0/secret_grants"

// dropSealHeaders removes sealing's headers: never route input.
func dropSealHeaders(h http.Header) { sealbox.DropHeaders(h) }
