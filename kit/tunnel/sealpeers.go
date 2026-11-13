package tunnel

// What this daemon remembers of the homes that named a seal key to it
// (zm-eakh.12.48.2), in $HOME/tunnel/seal_peers.json (0600): each home's key
// id and the sealing mode it last reported beside it. A home that reported
// required is never sent secrets in the clear again (nor are clear ops from
// it taken) until this machine's owner forgets it; one that reported
// preferred may fall back when it answers /v0/health naming no key (an
// auto-updater's rollback), warned every time.
//
// A record that cannot be read fails closed: a corrupt or empty file is
// moved aside as seal_peers.json.corrupt (an Error in the log) and, while
// that stays, every question about a home is an error naming the file and
// the forget step; forget starts a new, empty record.
//
// TODO(zm-eakh.12.48.2 review finding 6): bind each member machine to the
// seal identity it named at join (trust on first use), refusing another one
// later with an operator step to re-bind. Today the home takes a seal id
// only with a signature by that identity's key (proof of possession,
// kit/tailnet), and nothing records which machine it belongs to.

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/sengirunners/potluck/kit/api"
)

// SealPeersFile is the record under $HOME.
const SealPeersFile = "tunnel/seal_peers.json"

type sealHome struct {
	Key  string `json:"key"`
	Mode string `json:"mode,omitempty"`
}

type sealPeers struct {
	Homes map[string]sealHome `json:"homes"` // home fingerprint -> what it named
}

type sealPeersStore struct {
	mu sync.Mutex
}

func (s *Service) peersPath() string { return filepath.Join(s.d.Config.Home, SealPeersFile) }

// errPeers is the refusal while the record cannot be read.
func (s *Service) errPeers(why error) error {
	return api.Errorf(api.CodeUnavailable, "this machine's record of which pool machines seal (%s) cannot be read: %v; no secret goes in the clear meanwhile", s.peersPath(), why).
		WithHint("its owner fixes the file, or starts a new record with potluck sealing forget -P <pool> (every pool machine is then forgotten)")
}

// readPeers is the record. A file group or others may read is refused; a
// corrupt or empty one is moved aside and refused, as is any record while
// the moved-aside copy stays.
func (s *Service) readPeers() (sealPeers, error) {
	p := sealPeers{Homes: map[string]sealHome{}}
	path := s.peersPath()
	fi, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		if _, err := os.Stat(path + ".corrupt"); err == nil {
			return p, s.errPeers(errors.New("it was corrupt and was moved aside as " + path + ".corrupt"))
		}
		return p, nil
	}
	if err != nil {
		return p, s.errPeers(err)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return p, s.errPeers(fmt.Errorf("it is readable by others (mode %o): chmod 600 %s", fi.Mode().Perm(), path))
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return p, s.errPeers(err)
	}
	if len(b) == 0 || json.Unmarshal(b, &p) != nil {
		_ = os.Rename(path, path+".corrupt")
		s.logger().Error("sealing: the record of which pool machines seal is corrupt; moved aside, no secret goes in the clear until its owner runs potluck sealing forget", "file", path+".corrupt")
		return sealPeers{}, s.errPeers(errors.New("it was corrupt and was moved aside as " + path + ".corrupt"))
	}
	if p.Homes == nil {
		p.Homes = map[string]sealHome{}
	}
	return p, nil
}

// writePeers writes p whole: a temp file, synced, renamed over the record.
func (s *Service) writePeers(p sealPeers) error {
	b, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	path := s.peersPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	// The rename itself survives a crash only once the directory is synced.
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

// HomeSealed is the mode the home pinned as fp last reported with its key
// ("" when it never named one), or the refusal while the record cannot be
// read.
func (s *Service) HomeSealed(fp string) (string, error) {
	s.peers.mu.Lock()
	defer s.peers.mu.Unlock()
	p, err := s.readPeers()
	if err != nil {
		return "", err
	}
	h, ok := p.Homes[fp]
	if !ok {
		return "", nil
	}
	if h.Mode == "" {
		return "required", nil // a home that named a key but no mode: the stricter
	}
	return h.Mode, nil
}

// MarkHomeSealed records that the home pinned as fp named key id keyID,
// reporting mode. Refused while the record cannot be read (only forget
// starts a new one).
func (s *Service) MarkHomeSealed(fp, keyID, mode string) error {
	s.peers.mu.Lock()
	defer s.peers.mu.Unlock()
	p, err := s.readPeers()
	if err != nil {
		return err
	}
	if mode != "preferred" {
		mode = "required"
	}
	if h := p.Homes[fp]; h.Key == keyID && h.Mode == mode {
		return nil
	}
	p.Homes[fp] = sealHome{Key: keyID, Mode: mode}
	return s.writePeers(p)
}

// ForgetHomeSealed forgets the home pinned as fp. On a record that cannot
// be read it starts a new, empty one (every home forgotten) and removes the
// moved-aside copy.
func (s *Service) ForgetHomeSealed(fp string) (bool, error) {
	s.peers.mu.Lock()
	defer s.peers.mu.Unlock()
	p, err := s.readPeers()
	if err != nil {
		if werr := s.writePeers(sealPeers{Homes: map[string]sealHome{}}); werr != nil {
			return false, werr
		}
		_ = os.Remove(s.peersPath() + ".corrupt")
		s.logger().Warn("sealing: started a new record of which pool machines seal; every pool machine is forgotten", "file", s.peersPath(), "was", err)
		return true, nil
	}
	if _, ok := p.Homes[fp]; !ok {
		return false, nil
	}
	delete(p.Homes, fp)
	return true, s.writePeers(p)
}

// SignSeal signs msg with this daemon's identity key.
func (s *Service) SignSeal(msg []byte) (string, error) {
	if s.sealOff.Load() {
		return "", errors.New("sealing is off")
	}
	k, err := s.key()
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(ed25519.Sign(k, msg)), nil
}
