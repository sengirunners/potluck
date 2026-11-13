package e2bgateway

// API keys and the access tokens the gateway hands out.

import (
	"bufio"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/sengirunners/potluck/kit/client"
)

// apiKey is one line of the keys file: an E2B API key and the zm client it
// acts as (the gateway's own, or a zm token's).
type apiKey struct {
	key  string
	name string // a short, non-secret tag for logs and access tokens
	c    *client.Client
}

// readSecretFile reads a file that must be a regular 0600 file, trimmed.
func readSecretFile(path string) (string, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !fi.Mode().IsRegular() || fi.Mode().Perm() != 0o600 {
		return "", fmt.Errorf("%s must be a regular file with mode 0600", path)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

// loadKeys reads the keys file: one `<api key> [<zm token>]` per line, '#'
// comments. A key without a token acts as base (the gateway's own zm
// identity); with one, as that token. The file must be 0600; a key shorter
// than 20 bytes, or repeated, is refused. No key is ever logged.
func loadKeys(path string, base *client.Client) ([]*apiKey, error) {
	text, err := readSecretFile(path)
	if err != nil {
		return nil, fmt.Errorf("keys file: %w (potluck-e2b keygen makes one)", err)
	}
	var keys []*apiKey
	seen := map[string]bool{}
	sc := bufio.NewScanner(strings.NewReader(text))
	for n := 1; sc.Scan(); n++ {
		f := strings.Fields(sc.Text())
		if len(f) == 0 || strings.HasPrefix(f[0], "#") {
			continue
		}
		if len(f) > 2 || len(f[0]) < 20 {
			return nil, fmt.Errorf("%s line %d: want `<api key of 20+ bytes> [<token>]`", path, n)
		}
		if seen[f[0]] {
			return nil, fmt.Errorf("%s line %d: a repeated key", path, n)
		}
		seen[f[0]] = true
		c := base
		if len(f) == 2 {
			cp := *base
			cp.Token, cp.As = f[1], ""
			c = &cp
		}
		sum := sha256.Sum256([]byte(f[0]))
		name := hex.EncodeToString(sum[:4])
		if seen[name] { // the tag in request ids must name one key
			return nil, fmt.Errorf("%s line %d: key tag collides with another key's; make a new key", path, n)
		}
		seen[name] = true
		keys = append(keys, &apiKey{key: f[0], name: name, c: c})
	}
	if len(keys) == 0 {
		return nil, errors.New("keys file: no keys (potluck-e2b keygen adds one)")
	}
	return keys, nil
}

// match finds the key equal to k, comparing every key in constant time.
func match(keys []*apiKey, k string) *apiKey {
	var got *apiKey
	for _, a := range keys {
		if subtle.ConstantTimeCompare([]byte(a.key), []byte(k)) == 1 {
			got = a
		}
	}
	return got
}

// loadSecret reads the state directory's token secret, making it on first use.
func loadSecret(path string) ([]byte, error) {
	if s, err := readSecretFile(path); err == nil {
		b, err := hex.DecodeString(s)
		if err != nil || len(b) < 32 {
			return nil, fmt.Errorf("%s: not a 32-byte hex secret", path)
		}
		return b, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	_, err = f.WriteString(hex.EncodeToString(b) + "\n")
	return b, err
}

// accessTokenPrefix starts an envd access token (zmt_ before the rename's
// step 2: a token is derived, never stored, so a live sandbox's old token
// stops working when the gateway is upgraded).
const accessTokenPrefix = "potluck_e2b_"

// accessToken is the envd access token for a sandbox made through key k:
// the data plane carries no API key, so this both authenticates a request
// and says which key's zm identity runs it.
func accessToken(secret []byte, k *apiKey, sandboxID string) string {
	m := hmac.New(sha256.New, secret)
	m.Write([]byte(k.name + "\x00" + k.key + "\x00" + sandboxID))
	return accessTokenPrefix + hex.EncodeToString(m.Sum(nil))
}

// keyForToken finds the key whose access token for sandboxID is tok.
func keyForToken(secret []byte, keys []*apiKey, sandboxID, tok string) *apiKey {
	var got *apiKey
	for _, k := range keys {
		if hmac.Equal([]byte(accessToken(secret, k, sandboxID)), []byte(tok)) {
			got = k
		}
	}
	return got
}
