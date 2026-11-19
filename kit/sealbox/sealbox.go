// Package sealbox is the one sealing primitive between daemons (SPEC 17.11
// "Sealed secrets"): a run-target call's secrets or jit_config, and a relayed
// op holding them, go from one daemon to the next only sealed to the
// receiving daemon's key, on every leg and whatever carries it (the tunnel,
// the tailnet). It holds no key and does no I/O but reading or making the
// key file; the tunnel module keeps the keys and the transports call this.
//
// The construction: HPKE (RFC 9180, Go's crypto/hpke) in base mode with
// DHKEM(X25519, HKDF-SHA256), HKDF-SHA256 and ChaCha20-Poly1305, one suite
// and no negotiation. Each daemon's X25519 key is apart from its ed25519
// identity key, which certifies it (Certify: the identity signs its key id,
// the suite and the public key). The HPKE info (Info) binds the sender's and
// the recipient's key ids, an expiry and a purpose (the request, or the op,
// box and verb), so a sealed blob opens nowhere else. What is ours is the
// certificate (one ed25519 signature) and the info string; the HPKE is the
// standard library's.
//
// Base mode does not authenticate the sender: anyone may seal to a public
// key. The sender's key id in the info is a label, and who sent a blob is
// what the channel carrying it proves (the tunnel's signed request or
// answer; the tailnet's pinned node).
package sealbox

import (
	"bytes"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/hpke"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/sengirunners/potluck/kit/api"
)

// Suite names the one suite in a certificate (a later suite gets a new
// name; a certificate naming another is refused, never downgraded).
const Suite = "x25519-hkdfsha256-chacha20poly1305"

// Field is the one field of a sealed request body.
const Field = "zm_sealed"

// prefix starts every sealed blob: zs1.<expiry unix>.<base64url(enc||ct)>.
const prefix = "zs1."

// MaxSealed bounds a sealed blob as text (a run-target call's secrets are
// at most 64 KiB; a relayed op's arguments ride a heartbeat of at most 8 MiB).
const MaxSealed = 12 << 20

var (
	kem  = hpke.DHKEM(ecdh.X25519())
	kdf  = hpke.HKDFSHA256()
	aead = hpke.ChaCha20Poly1305()
)

// ErrNoKey is a peer that certified no seal key (it runs a Potluck or zm
// from before sealing on this leg) or a sender that cannot seal: secrets and
// jit_config do not go to or from it. who names the side that is too old.
func ErrNoKey(who string) *api.Error {
	return api.Errorf(api.CodeForbidden, "secrets and jit_config go between machines only sealed to the receiving machine's key, and %s runs a Potluck (or zm) too old to seal them: update it", who).
		WithHint("update the daemon there (potluck upgrade, or zm daemon update); calls without secrets still work")
}

// ErrOpen is every failure to open a sealed blob: one message, so a
// tampered, expired, misdirected or replayed blob cannot be told apart.
var ErrOpen = api.Errorf(api.CodeBadRequest, "the sealed secrets could not be opened here (sealed for another key, request or op, expired, or changed on the way)").
	WithHint("try again: the sender seals to this machine's current key")

var fpEnc = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)

// Fingerprint is an identity key's pin: 26 base32 characters (130 bits) of
// sha256 over the public key.
func Fingerprint(pub ed25519.PublicKey) string {
	h := sha256.Sum256(append([]byte("zm-key/1\n"), pub...))
	return fpEnc.EncodeToString(h[:17])[:26]
}

// KeyID is an identity key's id, the name a sealed blob's info uses for
// either end: "zmkey:" and its fingerprint.
func KeyID(pub ed25519.PublicKey) string { return keyIDPrefix + Fingerprint(pub) }

const keyIDPrefix = "zmkey:"

// IsKeyID reports whether s has the shape KeyID makes.
func IsKeyID(s string) bool {
	fp, ok := strings.CutPrefix(s, keyIDPrefix)
	return ok && len(fp) == 26 && strings.Trim(fp, "abcdefghijklmnopqrstuvwxyz234567") == ""
}

func certString(keyID string, pub []byte) []byte {
	return []byte("zm-seal-key/1\n" + keyID + "\n" + Suite + "\n" + base64.RawURLEncoding.EncodeToString(pub))
}

// Certify is the Zm-Seal-Key value for the seal public key pub under the
// identity id: "1.<base64url pub>.<base64url ed25519 signature>".
func Certify(id ed25519.PrivateKey, pub []byte) string {
	sig := ed25519.Sign(id, certString(KeyID(id.Public().(ed25519.PublicKey)), pub))
	return "1." + base64.RawURLEncoding.EncodeToString(pub) + "." + base64.RawURLEncoding.EncodeToString(sig)
}

// VerifyCert is the seal public key cert carries when the identity idPub
// signed it for its own key id; nil otherwise.
func VerifyCert(idPub ed25519.PublicKey, cert string) []byte {
	parts := strings.Split(cert, ".")
	if len(parts) != 3 || parts[0] != "1" || len(idPub) != ed25519.PublicKeySize {
		return nil
	}
	pub, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || len(pub) != 32 {
		return nil
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || !ed25519.Verify(idPub, certString(KeyID(idPub), pub), sig) {
		return nil
	}
	if _, err := kem.NewPublicKey(pub); err != nil {
		return nil
	}
	return pub
}

// Peer is a key a header pair names: the identity's public key (base64url)
// and the seal key it certified. ok is false unless both are well formed
// and the certificate is the identity's.
func Peer(idB64, cert string) (keyID string, seal []byte, ok bool) {
	id, err := base64.RawURLEncoding.DecodeString(idB64)
	if err != nil || len(id) != ed25519.PublicKeySize || cert == "" {
		return "", nil, false
	}
	if seal = VerifyCert(id, cert); seal == nil {
		return "", nil, false
	}
	return KeyID(id), seal, true
}

// SigMessage is what a tailnet sender signs with its identity key: the
// request's purpose and the sha256 of the sealed body it sends (so the
// signature binds this ciphertext to this request and sender).
func SigMessage(purpose string, sealedBody []byte) []byte {
	h := sha256.Sum256(sealedBody)
	return []byte("zm-seal-sig/1\n" + purpose + "\n" + hex.EncodeToString(h[:]))
}

// VerifySig checks sig (base64url) over msg by the identity idB64 names.
func VerifySig(idB64, sig string, msg []byte) bool {
	id, err := base64.RawURLEncoding.DecodeString(idB64)
	if err != nil || len(id) != ed25519.PublicKeySize {
		return false
	}
	b, err := base64.RawURLEncoding.DecodeString(sig)
	return err == nil && ed25519.Verify(id, msg, b)
}

// Info is the HPKE info: who seals, for whom, until when, for what. Every
// part is length-prefixed, so no two contexts read alike.
func Info(from, to string, exp int64, purpose string) []byte {
	var b strings.Builder
	b.WriteString("zm-seal/1\n")
	for _, p := range []string{from, to, strconv.FormatInt(exp, 10), purpose} {
		b.WriteString(strconv.Itoa(len(p)))
		b.WriteByte(':')
		b.WriteString(p)
	}
	return []byte(b.String())
}

// Seal seals plaintext to pub (the seal key to's certificate carried), from
// the key id from, for purpose, until now+ttl.
func Seal(from, to string, pub []byte, purpose string, ttl time.Duration, plaintext []byte, now time.Time) (string, error) {
	pk, err := kem.NewPublicKey(pub)
	if err != nil {
		return "", ErrNoKey("the receiving machine")
	}
	exp := now.Add(ttl).Unix()
	ct, err := hpke.Seal(pk, kdf, aead, Info(from, to, exp, purpose), plaintext)
	if err != nil {
		return "", err
	}
	return prefix + strconv.FormatInt(exp, 10) + "." + base64.RawURLEncoding.EncodeToString(ct), nil
}

// Open opens sealed with the key k, as to, from the key id from, for
// purpose. A blob past its expiry by more than skew (the two clocks may
// differ that much) does not open. Every failure is ErrOpen.
func Open(k hpke.PrivateKey, from, to, purpose, sealed string, now time.Time, skew time.Duration) ([]byte, error) {
	rest, ok := strings.CutPrefix(sealed, prefix)
	if !ok || len(sealed) > MaxSealed {
		return nil, ErrOpen
	}
	es, b64, ok := strings.Cut(rest, ".")
	exp, err := strconv.ParseInt(es, 10, 64)
	if !ok || err != nil || es != strconv.FormatInt(exp, 10) || now.After(time.Unix(exp, 0).Add(skew)) {
		return nil, ErrOpen
	}
	ct, err := base64.RawURLEncoding.DecodeString(b64)
	if err != nil {
		return nil, ErrOpen
	}
	pt, err := hpke.Open(k, kdf, aead, Info(from, to, exp, purpose), ct)
	if err != nil {
		return nil, ErrOpen
	}
	return pt, nil
}

// LoadOrCreateKey reads the X25519 key at path (hex), or makes one there
// (0600, its directory 0700) when there is none. A key file group or others
// may read is refused, naming the chmod that fixes it.
func LoadOrCreateKey(path string) (hpke.PrivateKey, error) {
	if fi, err := os.Stat(path); err == nil && fi.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("seal key %s is readable by others (mode %o): chmod 600 %s", path, fi.Mode().Perm(), path)
	}
	if b, err := os.ReadFile(path); err == nil {
		raw, err := hex.DecodeString(strings.TrimSpace(string(b)))
		if err != nil || len(raw) != 32 {
			return nil, fmt.Errorf("seal key %s: not an X25519 key", path)
		}
		return kem.NewPrivateKey(raw)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		return LoadOrCreateKey(path) // another start made it first: its key wins
	}
	if err != nil {
		return nil, err
	}
	if _, err := f.WriteString(hex.EncodeToString(raw) + "\n"); err != nil {
		f.Close()
		os.Remove(path)
		return nil, err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	return kem.NewPrivateKey(raw)
}

// RequestPurpose binds a request's sealed body to that one request: its
// method, URI (as the receiver reads it), date and nonce.
func RequestPurpose(method, uri, date, nonce string) string {
	return "request\n" + method + "\n" + uri + "\n" + date + "\n" + nonce
}

// SealWhole replaces a request body with {"zm_sealed": <the body sealed>}:
// the whole body, so no field rides beside it in the clear, whatever its
// spelling (encoding/json folds case: "Secrets" is secrets to a route).
func SealWhole(body []byte, sealIt func([]byte) (string, error)) ([]byte, error) {
	sealed, err := sealIt(body)
	if err != nil {
		return nil, err
	}
	return json.Marshal(map[string]string{Field: sealed})
}

// OpenWhole is SealWhole undone: the body exactly as the sender sent it. A
// body with anything beside the sealed field is refused.
func OpenWhole(body []byte, openIt func(string) ([]byte, error)) ([]byte, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil || len(obj) != 1 {
		return nil, ErrOpen
	}
	var sealed string
	if json.Unmarshal(obj[Field], &sealed) != nil || sealed == "" {
		return nil, ErrOpen
	}
	return openIt(sealed)
}

// DropHeaders removes sealing's headers from a request: transport
// metadata, never route input.
func DropHeaders(h http.Header) {
	for _, n := range []string{api.HeaderSeal, api.HeaderSealed, api.HeaderSealKey, api.HeaderSealID, api.HeaderSealDate, api.HeaderSealNonce, api.HeaderSealSig, api.HeaderSealing} {
		api.DelHeader(h, n)
	}
}

// RunTargetPath reports whether path is a run-target route's, the only
// routes whose bodies carry secrets or a jit_config.
func RunTargetPath(path string) bool {
	return path == "/v0/run_targets" || strings.HasPrefix(path, "/v0/run_targets/")
}

// CarriesSecret reports whether a run-target body, or a relayed op's
// arguments, hold secrets, a jit_config or secret names to unset under any
// spelling encoding/json would read as them (a body that is not a JSON
// object counts as holding them): no secret value or name leaves in the
// clear. It is the one test every leg uses: what it says yes to goes sealed
// or not at all.
func CarriesSecret(body []byte) bool {
	if len(bytes.TrimSpace(body)) == 0 {
		return false
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(body, &obj) != nil {
		return true
	}
	for k, v := range obj {
		if !strings.EqualFold(k, "secrets") && !strings.EqualFold(k, "jit_config") && !strings.EqualFold(k, "unset") {
			continue
		}
		if t := string(bytes.TrimSpace(v)); t != "" && t != "null" && t != "{}" && t != `""` && t != "[]" {
			return true
		}
	}
	return false
}
