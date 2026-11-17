package tunnel

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/project"
)

// BoxCredentialKind names the format of a box credential file.
const BoxCredentialKind = "potluck-box/1"

// MaxBoxCredential bounds a box credential file read whole.
const MaxBoxCredential = 4 << 10

// BoxCredential is what the potluck-token kind delivers into a box, as one
// file: the box's token, the per-box key that token is bound to, and the
// authority it reaches (its tunnel URL and key fingerprint, pinned). With it
// a program in the box calls its authority over the tunnel with no daemon
// and nothing else on disk. The token alone is refused over the tunnel; the
// key alone authenticates nothing.
type BoxCredential struct {
	Token string
	Key   ed25519.PrivateKey
	URL   string // the authority's tunnel URL
	Home  string // the authority's key fingerprint
}

// boxFile is a box credential's JSON, one object.
type boxFile struct {
	Kind  string `json:"kind"`
	Token string `json:"token"`
	Key   string `json:"key"` // hex of the ed25519 seed
	URL   string `json:"url"`
	Home  string `json:"home"`
}

// NewBoxKey makes a per-box key. Its seed is never written by the
// authority: it leaves only inside the credential it delivers.
func NewBoxKey() (ed25519.PrivateKey, error) {
	seed := make([]byte, ed25519.SeedSize)
	if _, err := rand.Read(seed); err != nil {
		return nil, err
	}
	return ed25519.NewKeyFromSeed(seed), nil
}

// BoxKeyID is the key id a box's token is bound to (MintTokenRequest.BoxKey).
func BoxKeyID(k ed25519.PrivateKey) string { return KeyID(k.Public().(ed25519.PublicKey)) }

// Encode is the credential file's content.
func (c BoxCredential) Encode() (string, error) {
	if err := c.check(); err != nil {
		return "", err
	}
	b, err := json.Marshal(boxFile{Kind: BoxCredentialKind, Token: c.Token, Key: hex.EncodeToString(c.Key.Seed()), URL: c.URL, Home: c.Home})
	return string(b), err
}

func (c BoxCredential) check() error {
	switch {
	case !authz.LooksLikeToken(c.Token):
		return errors.New("box credential: no token")
	case len(c.Key) != ed25519.PrivateKeySize:
		return errors.New("box credential: no key")
	case len(c.Home) != 26:
		return errors.New("box credential: no authority key fingerprint")
	}
	u, err := project.TunnelURL(c.URL)
	if err != nil || u != c.URL {
		return fmt.Errorf("box credential: authority url %q is not a tunnel url", c.URL)
	}
	return nil
}

// ParseBoxCredential reads a credential file's content. Errors never quote
// it.
func ParseBoxCredential(s string) (BoxCredential, error) {
	var f boxFile
	dec := json.NewDecoder(bytes.NewReader([]byte(s)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil || dec.More() {
		return BoxCredential{}, errors.New("box credential: not a " + BoxCredentialKind + " file")
	}
	if f.Kind != BoxCredentialKind {
		return BoxCredential{}, errors.New("box credential: not a " + BoxCredentialKind + " file")
	}
	seed, err := hex.DecodeString(f.Key)
	if err != nil || len(seed) != ed25519.SeedSize {
		return BoxCredential{}, errors.New("box credential: the key is not an ed25519 seed")
	}
	c := BoxCredential{Token: f.Token, Key: ed25519.NewKeyFromSeed(seed), URL: f.URL, Home: f.Home}
	if err := c.check(); err != nil {
		return BoxCredential{}, err
	}
	return c, nil
}

// ReadBoxCredential reads the credential file at path (the box's secrets
// directory holds it).
func ReadBoxCredential(path string) (BoxCredential, error) {
	f, err := os.Open(path)
	if err != nil {
		return BoxCredential{}, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, MaxBoxCredential+1))
	if err != nil {
		return BoxCredential{}, fmt.Errorf("box credential %s: %w", path, err)
	}
	if len(b) > MaxBoxCredential {
		return BoxCredential{}, fmt.Errorf("box credential %s: over %d bytes", path, MaxBoxCredential)
	}
	c, err := ParseBoxCredential(string(b))
	if err != nil {
		return BoxCredential{}, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

// BoxClient is a client for the authority c names, with no daemon: every
// request is signed with the box's key for the pinned authority and carries
// the box's token, and only answers that authority signed are read. It
// seals nothing (a box has no seal key), so a call that carries a secret is
// refused before it is sent.
func BoxClient(c BoxCredential) (*client.Client, error) {
	if err := c.check(); err != nil {
		return nil, err
	}
	cl := client.NewRemote(c.URL, &http.Client{Transport: newTransport(c.Key, c.Home, 0, nil)})
	cl.Token = c.Token
	return cl, nil
}

// Home is what a box needs to reach this daemon: its tunnel URL and its
// key's fingerprint. A daemon with no tunnel URL has none.
func (s *Service) Home() (url, fingerprint string, err error) {
	url = s.URL()
	if url == "" {
		return "", "", api.Errorf(api.CodeUnavailable, "this daemon has no tunnel url (tunnel.url): a box could not reach it")
	}
	fp, err := s.Fingerprint()
	if err != nil {
		return "", "", err
	}
	return url, fp, nil
}
