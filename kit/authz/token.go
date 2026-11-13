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
// 52 characters after it.
func LooksLikeToken(s string) bool {
	for _, p := range []string{TokenPrefix, TokenPrefixOld} {
		if strings.HasPrefix(s, p) && len(s) == len(p)+52 {
			return true
		}
	}
	return false
}

// BearerToken extracts the secret from an Authorization header value,
// "Bearer zm_...".
func BearerToken(header string) string {
	if v, ok := strings.CutPrefix(strings.TrimSpace(header), "Bearer "); ok {
		return strings.TrimSpace(v)
	}
	return ""
}
