package tunnel

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base32"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/sealbox"
)

// Headers of the signed exchange (SPEC 17.11), under their old names. This
// build sends each under its new name, Potluck-* (api.SetHeader), and reads
// the new name first, then the old (a step 1 peer's); every zm-* and
// potluck-* header is signed.
const (
	HeaderKey      = "Zm-Key"      // the caller's public key, base64url
	HeaderDate     = "Zm-Date"     // Unix seconds
	HeaderNonce    = "Zm-Nonce"    // 16 random bytes, base64url, never reused
	HeaderSig      = "Zm-Sig"      // ed25519 over requestString
	HeaderHomeKey  = "Zm-Home-Key" // the home's public key, base64url
	HeaderHomeSig  = "Zm-Home-Sig" // ed25519 over responseString
	HeaderRecovery = "Zm-Recovery" // signed, process-local recovery challenge
)

// recoveryValue accepts one canonical 256-bit challenge. A duplicate or a
// malformed header must never turn an ordinary refusal into a retry.
func recoveryValue(h http.Header) string {
	v, ok := one(h, HeaderRecovery)
	if !ok || len(v) != 43 {
		return ""
	}
	b, err := base64.RawURLEncoding.DecodeString(v)
	if err != nil || len(b) != 32 || base64.RawURLEncoding.EncodeToString(b) != v {
		return ""
	}
	return v
}

// one is the value of a header sent exactly once under its old and new
// names together (a duplicate, or both names, is no value).
func one(h http.Header, name string) (string, bool) {
	vs := append(h.Values(api.HeaderNew(name)), h.Values(name)...)
	if len(vs) != 1 {
		return "", false
	}
	return vs[0], true
}

// get is a header of the signed exchange under its new name, else its old.
func get(h http.Header, name string) string { return api.GetHeader(h, name) }

var fpEnc = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)

// Fingerprint is a key's pin in a tunnel invite: 26 base32 characters (130
// bits) of sha256 over the public key.
func Fingerprint(pub ed25519.PublicKey) string { return sealbox.Fingerprint(pub) }

// KeyID is the "node" a key-bound token records: never a Tailscale stable
// ID, which has no colon.
func KeyID(pub ed25519.PublicKey) string { return sealbox.KeyID(pub) }

// requestString is what a member signs: the home it is meant for, the
// request line, the date and nonce, the headers that carry meaning and the
// body's hash.
func requestString(homeFP, method, uri string, h http.Header, body []byte) []byte {
	return []byte("zm-req/1\n" + homeFP + "\n" + method + "\n" + normURI(uri) + "\n" + get(h, HeaderDate) + "\n" +
		get(h, HeaderNonce) + "\n" + canonHeaders(h, HeaderSig, true) + "\n" + sum(body))
}

// responseString is what the home signs: the request's nonce (an answer
// cannot be moved to another request), the status, the headers that carry
// meaning and the body's hash.
func responseString(homeFP, nonce string, status int, h http.Header, body []byte) []byte {
	return []byte("zm-resp/1\n" + homeFP + "\n" + nonce + "\n" + strconv.Itoa(status) + "\n" +
		canonHeaders(h, HeaderHomeSig, false) + "\n" + sum(body))
}

func sum(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// canonHeaders hashes Content-Type, every Zm-* and Potluck-* header but skip
// (under either name) and, for a request, Authorization: names in lower case, sorted, each value in order
// as <len>:<name><len>:<value>.
// Other headers (the tunnel's own, Content-Length, Date) are not signed,
// and the client drops them from an answer.
func canonHeaders(h http.Header, skip string, auth bool) string {
	var names []string
	for k := range h {
		if signedHeader(k, auth) && !strings.EqualFold(k, skip) && !strings.EqualFold(k, api.HeaderNew(skip)) {
			names = append(names, strings.ToLower(k))
		}
	}
	sort.Strings(names)
	// Every name and value is length-prefixed, one entry per value: two
	// values never read as one "a,b", and a value cannot fake a name.
	var b strings.Builder
	for _, n := range names {
		for _, v := range h.Values(n) {
			b.WriteString(strconv.Itoa(len(n)))
			b.WriteByte(':')
			b.WriteString(n)
			b.WriteString(strconv.Itoa(len(v)))
			b.WriteByte(':')
			b.WriteString(v)
		}
	}
	return sum([]byte(b.String()))
}

func signedHeader(k string, auth bool) bool {
	l := strings.ToLower(k)
	return l == "content-type" || api.IsOwnHeader(l) || (auth && l == "authorization")
}

// normURI is RFC 3986's percent-encoding normalisation (section 6.2.2.2:
// upper-case hex, unreserved characters decoded), which an edge may apply
// on the way; anything else, %2F included, stays as sent.
func normURI(s string) string {
	if !strings.Contains(s, "%") {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '%' && i+2 < len(s) && isHex(s[i+1]) && isHex(s[i+2]) {
			v := unhex(s[i+1])<<4 | unhex(s[i+2])
			if unreserved(v) {
				b.WriteByte(v)
			} else {
				b.WriteByte('%')
				b.WriteString(strings.ToUpper(s[i+1 : i+3]))
			}
			i += 2
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}

func isHex(c byte) bool {
	return '0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F'
}

func unhex(c byte) byte {
	switch {
	case '0' <= c && c <= '9':
		return c - '0'
	case 'a' <= c && c <= 'f':
		return c - 'a' + 10
	}
	return c - 'A' + 10
}

func unreserved(c byte) bool {
	return 'A' <= c && c <= 'Z' || 'a' <= c && c <= 'z' || '0' <= c && c <= '9' || c == '-' || c == '.' || c == '_' || c == '~'
}

// VerifyAnswer checks an answer the way a member's transport does: the
// home key it names must have fingerprint wantFP, and its signature must
// cover nonce (sent with the request), the status, the signed headers and
// body. A stranger's request (no key of its own) may send a nonce too, and
// the home signs its answer the same way (zm tunnel setup's check).
func VerifyAnswer(wantFP, nonce string, status int, h http.Header, body []byte) error {
	pub, err := base64.RawURLEncoding.DecodeString(get(h, HeaderHomeKey))
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return ErrUnsigned
	}
	if Fingerprint(pub) != wantFP {
		return ErrHomeKey
	}
	sig, err := base64.RawURLEncoding.DecodeString(get(h, HeaderHomeSig))
	if err != nil || !ed25519.Verify(pub, responseString(wantFP, nonce, status, h, body), sig) {
		return ErrHomeKey
	}
	return nil
}
