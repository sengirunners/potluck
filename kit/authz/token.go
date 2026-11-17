package authz

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"strings"
)

// TokenPrefix starts every bearer token this build mints: "potluck_".
// A token with the old prefix, TokenPrefixOld ("zm_", every token minted
// before the rename's step 2), is read as well (LooksLikeToken); the stored
// hash covers the whole secret, prefix included, so each is its own token.
const (
	TokenPrefix    = "potluck_"
	TokenPrefixOld = "zm_"
)

// TokenPrefixNew is TokenPrefix (its step 1 name).
const TokenPrefixNew = TokenPrefix

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// NewTokenSecret returns a fresh opaque secret: "potluck_" + 52 base32 chars (256 bits).
func NewTokenSecret() string {
	var b [32]byte
	_, _ = rand.Read(b[:])
	return TokenPrefix + strings.ToLower(b32.EncodeToString(b[:]))
}

// HashToken is what the registry stores: hex sha256 of the whole secret.
func HashToken(secret string) string {
	h := sha256.Sum256([]byte(strings.TrimSpace(secret)))
	return hex.EncodeToString(h[:])
}

// LooksLikeToken is a cheap shape check before a lookup: either prefix and
// 52 characters after it, or a limited token's shape (TokenLimit).
func LooksLikeToken(s string) bool {
	for _, p := range []string{TokenPrefix, TokenPrefixOld} {
		if strings.HasPrefix(s, p) && len(s) == len(p)+52 {
			return true
		}
	}
	_, ok := TokenLimit(s)
	return ok
}

// A limited token (a route limit, Cap.Limit) has its own shape and hash, so
// code that predates limits can never take one for a full token: its secret
// is TokenPrefix + limit + "_" + 52 base32 characters, which the older
// 52-character check refuses, and its stored hash is HashLimited, which no
// older lookup computes.

// MaxLimitLen bounds a limit name.
const MaxLimitLen = 16

// ValidLimit reports whether name can be a route limit: 1 to MaxLimitLen
// lowercase ASCII letters (so the secret's shape stays unambiguous).
func ValidLimit(name string) bool {
	if len(name) == 0 || len(name) > MaxLimitLen {
		return false
	}
	for i := 0; i < len(name); i++ {
		if name[i] < 'a' || name[i] > 'z' {
			return false
		}
	}
	return true
}

// NewLimitedSecret returns a fresh secret for a token limited to limit:
// "potluck_<limit>_" + 52 base32 chars (256 bits). limit must be ValidLimit.
func NewLimitedSecret(limit string) string {
	var b [32]byte
	_, _ = rand.Read(b[:])
	return TokenPrefix + limit + "_" + strings.ToLower(b32.EncodeToString(b[:]))
}

// TokenLimit is the limit a limited token's secret names, and whether s has
// a limited token's shape at all. A plain token is never limited.
func TokenLimit(s string) (string, bool) {
	rest, ok := strings.CutPrefix(s, TokenPrefix)
	if !ok || len(rest) < 52+2 {
		return "", false
	}
	limit, sep, body := rest[:len(rest)-53], rest[len(rest)-53], rest[len(rest)-52:]
	if sep != '_' || !ValidLimit(limit) {
		return "", false
	}
	for i := 0; i < len(body); i++ {
		if c := body[i]; !(c >= 'a' && c <= 'z' || c >= '2' && c <= '7') {
			return "", false
		}
	}
	return limit, true
}

// limitedDomain separates a limited token's hash from HashToken's.
const limitedDomain = "potluck-limited/1\n"

// HashLimited is what the registry stores for a token limited to limit:
// hex sha256 of a domain tag, the limit and the whole secret. It never
// equals HashToken of the same secret, so a lookup by HashToken (every
// build before limits) finds no limited row.
func HashLimited(limit, secret string) string {
	h := sha256.Sum256([]byte(limitedDomain + limit + "\n" + strings.TrimSpace(secret)))
	return hex.EncodeToString(h[:])
}

// BearerToken extracts the secret from an Authorization header value,
// "Bearer zm_...".
func BearerToken(header string) string {
	if v, ok := strings.CutPrefix(strings.TrimSpace(header), "Bearer "); ok {
		return strings.TrimSpace(v)
	}
	return ""
}
