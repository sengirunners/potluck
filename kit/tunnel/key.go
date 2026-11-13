package tunnel

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/secrets"
)

// KeyFile is this daemon's key under $ZM_HOME: the hex of an ed25519 seed,
// 0600 in a 0700 directory, made on first use. One key per daemon, as one
// tailnet node per daemon: it signs as a member and as a home.
const KeyFile = "tunnel/key"

// loadOrCreateKey reads the key at path, or makes it (created). Two
// daemons racing on one home (never meant to happen) both end up with the
// file's key.
func loadOrCreateKey(path string) (k ed25519.PrivateKey, created bool, err error) {
	if k, err := readKey(path); err == nil || !errors.Is(err, os.ErrNotExist) {
		return k, false, err
	}
	k, err = makeKey(path)
	return k, err == nil, err
}

// loadKey is loadOrCreateKey with an escrow (config tunnel.key): the file
// when there is one; else the escrow's key, written to the file; else, only
// when no escrow is named, a new key. A named escrow that cannot be read is
// an error, never a new key: a new key would make every member refuse this
// home (zm-c5y6c.20). A key from the escrow is not fresh: earlier runs used it.
func loadKey(ctx context.Context, path, escrow string, rd *secrets.Reader) (k ed25519.PrivateKey, created bool, err error) {
	if escrow == "" {
		return loadOrCreateKey(path)
	}
	if k, err := readKey(path); err == nil || !errors.Is(err, os.ErrNotExist) {
		return k, false, err
	}
	seed, err := escrowSeed(ctx, escrow, rd)
	if err != nil {
		return nil, false, err
	}
	k, err = writeKey(path, seed)
	return k, false, err
}

// escrowSeed reads the seed the escrow ref holds.
func escrowSeed(ctx context.Context, escrow string, rd *secrets.Reader) ([]byte, error) {
	ref, err := secrets.Parse(escrow)
	if err != nil {
		return nil, err
	}
	v, err := rd.Get(ctx, ref)
	if err != nil {
		return nil, secrets.Prefix(err, "the home key escrow "+ref.String()+": ")
	}
	seed, err := hex.DecodeString(strings.TrimSpace(v.Reveal()))
	if err != nil || len(seed) != ed25519.SeedSize {
		return nil, api.Errorf(api.CodeUnavailable, "the home key escrow %s does not hold an ed25519 seed", ref.String())
	}
	return seed, nil
}

func makeKey(path string) (ed25519.PrivateKey, error) {
	seed := make([]byte, ed25519.SeedSize)
	if _, err := rand.Read(seed); err != nil {
		return nil, err
	}
	return writeKey(path, seed)
}

// writeKey writes seed to path (0600, never over a file there: that file's
// key wins).
func writeKey(path string, seed []byte) (ed25519.PrivateKey, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		return readKey(path)
	}
	if err != nil {
		return nil, err
	}
	if _, err := f.WriteString(hex.EncodeToString(seed) + "\n"); err != nil {
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
	return ed25519.NewKeyFromSeed(seed), nil
}

func readKey(path string) (ed25519.PrivateKey, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	seed, err := hex.DecodeString(strings.TrimSpace(string(b)))
	if err != nil || len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("tunnel key %s: not an ed25519 seed", path)
	}
	return ed25519.NewKeyFromSeed(seed), nil
}
