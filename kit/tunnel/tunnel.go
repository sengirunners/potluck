// Package tunnel is the tunnel transport (SPEC 17.11): a team's home serves
// its API on a loopback HTTP listener that a Cloudflare Tunnel (cloudflared)
// forwards to, and a member reaches it at an https URL without Tailscale.
//
// Cloudflare is transport only. zm's own auth is unchanged (invites, per
// member key-bound tokens, scope checks in core); this package adds what
// the tailnet gave for free: an identity per daemon. Each daemon has one
// ed25519 key. A member signs every request with it (the key id is the
// "node" its token is bound to), and the home signs every answer, which the
// member checks against the key fingerprint its invite pinned. Why our own
// signing and not a library: Cloudflare ends TLS at its edge, so nothing
// transport-level reaches the home; the scheme is ~200 lines of stdlib
// ed25519 and sha256.
// Second driver expected: any reverse proxy or tunnel that forwards HTTP
// (ngrok, a Caddy box), unchanged.
package tunnel

import (
	"context"
	"crypto/ed25519"
	"crypto/hpke"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sengirunners/potluck/kit/api"

	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/project"
	"github.com/sengirunners/potluck/kit/secrets"
)

// Service is the tunnel module.
type Service struct {
	d *core.Deps

	keyMu    sync.Mutex
	k        ed25519.PrivateKey
	keyFresh bool            // this process made the key: no earlier run used it
	sk       hpke.PrivateKey // the seal key (seal.go), under keyMu
	cert     string          // its certificate, under keyMu
	sealOff  atomic.Bool     // tests: behave as a zm from before sealing
	peers    sealPeersStore  // seal_peers.json (sealpeers.go)

	journal *nonceJournal // shared by every door of this daemon (one file)
	// tokenless remembers the nonces of the GitHub join's tokenless calls
	// (memory only, the skew window).
	tokenless tokenlessNonces
	// Never persisted: a request signed for an earlier process cannot use
	// its recovery challenge after the replay journal is lost.
	recoveryOnce sync.Once
	recovery     string
	recoveryErr  error

	// Secrets reads the home key escrow (nil: secrets.Default; tests swap it).
	Secrets *secrets.Reader

	cfgMu    sync.Mutex   // save
	edgeSeen atomic.Int64 // UnixNano of the last request that came through the edge (its secret)
	tcfg     atomic.Pointer[config.Tunnel]

	mu     sync.Mutex
	srv    *http.Server
	listen string
	err    string
	door   *door
}

var (
	_ core.Module  = (*Service)(nil)
	_ core.Starter = (*Service)(nil)
	_ core.Closer  = (*Service)(nil)
)

// New returns the tunnel module. Nothing listens until Start, and only
// when config names a listener.
func New(d *core.Deps) *Service {
	s := &Service{d: d}
	d.Provide(ServiceName, s)
	t := d.Config.File.Tunnel
	s.tcfg.Store(&t)
	log := d.Log
	if log == nil {
		log = slog.Default()
	}
	s.journal = &nonceJournal{path: filepath.Join(d.Config.Home, NonceFile), log: log, fresh: func() bool {
		s.keyMu.Lock()
		defer s.keyMu.Unlock()
		return s.keyFresh
	}}
	s.door = &door{s: s, nonces: s.journal}
	return s
}

// Routes registers the status and control routes (zm tunnel).
func (s *Service) Routes(r *core.Router) {
	r = r.For("tunnel")
	r.HandleFunc("POST /v0/tunnel/preview-domain", core.Local, "machine owner configures an existing wildcard HTTPS preview domain", s.handlePreviewDomain)
	r.HandleFunc("GET /v0/tunnel", core.Local, "local: the tunnel listener's status and this daemon's key fingerprint", func(w http.ResponseWriter, r *http.Request) {
		api.WriteJSON(w, http.StatusOK, s.Status())
	})
	r.HandleFunc("POST /v0/tunnel/up", core.Local, "local: start (or move) the loopback listener and save tunnel.* in config (zm tunnel setup)", s.handleUp)
	r.HandleFunc("POST /v0/tunnel/down", core.Local, "local: stop the listener and clear tunnel.* in config (zm tunnel remove)", s.handleDown)
	r.HandleFunc("POST /v0/tunnel/key", core.Local, "local: name (or clear) the secret reference that holds this daemon's key outside the home, after checking it holds this key (zm tunnel key)", s.handleKey)
}

// handleKey is POST /v0/tunnel/key {"ref"}: the escrow is saved only when
// it holds this daemon's key, so a restore that reads it starts with the key
// members pinned. A daemon with no key (its escrow could not be read, say a
// restored home whose disk path moved) adopts the key the new ref holds:
// the recovery step, never a new key. "" clears it, refused while the
// daemon has no key: clearing then would make a new key every member
// refuses. The key itself never crosses this route.
func (s *Service) handleKey(w http.ResponseWriter, r *http.Request) {
	var req api.TunnelKeyRequest
	if err := api.DecodeJSON(r, &req); err != nil {
		api.WriteError(w, err)
		return
	}
	if req.Ref == "" {
		if s.cfg().Key != "" {
			if _, err := s.key(); err != nil {
				api.WriteError(w, api.Errorf(api.CodeConflict, "this daemon has no key: clearing its escrow would make a new one, and every member would refuse this home").
					WithHint("point it at the key instead: zm tunnel key escrow --to <where the key is>"))
				return
			}
		}
	} else {
		ref, err := s.escrowRef(req.Ref)
		if err != nil {
			api.WriteError(w, err)
			return
		}
		seed, err := escrowSeed(r.Context(), ref.String(), s.secrets())
		if err != nil {
			api.WriteError(w, err)
			return
		}
		if err := s.adoptOrMatch(seed); err != nil {
			api.WriteError(w, err)
			return
		}
		req.Ref = ref.String()
	}
	if err := s.update(func(c *config.Tunnel) { c.Key = req.Ref }); err != nil {
		api.WriteError(w, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, s.Status())
}

// escrowRef parses an escrow reference; a file inside this home is refused:
// a restore loses it with the home.
func (s *Service) escrowRef(text string) (secrets.Ref, error) {
	ref, err := secrets.Parse(text)
	if err != nil {
		return ref, err
	}
	if ref.Scheme == "file" {
		if !filepath.IsAbs(ref.Name) {
			return ref, api.Errorf(api.CodeBadRequest, "file: escrow needs an absolute path")
		}
		if InHome(s.d.Config.Home, ref.Name) {
			return ref, api.Errorf(api.CodeBadRequest, "the escrow is inside this home, which a restore replaces").
				WithHint("a file on another disk, the keychain, or env:")
		}
	}
	return ref, nil
}

// InHome says whether path is home or under it: by file identity, walking
// up from path to its nearest existing parent (so case-insensitive and
// firmlinked spellings of the home count), with a string check after
// resolving symlinks for what cannot be stat'ed.
func InHome(home, path string) bool {
	if hi, err := os.Stat(home); err == nil {
		for p := filepath.Clean(path); ; p = filepath.Dir(p) {
			if fi, err := os.Stat(p); err == nil && os.SameFile(hi, fi) {
				return true
			}
			if p == filepath.Dir(p) {
				break
			}
		}
	}
	real := func(p string) string {
		p = filepath.Clean(p)
		if r, err := filepath.EvalSymlinks(p); err == nil {
			return r
		}
		if r, err := filepath.EvalSymlinks(filepath.Dir(p)); err == nil {
			return filepath.Join(r, filepath.Base(p))
		}
		return p
	}
	h, p := real(home), real(path)
	rel, err := filepath.Rel(h, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// adoptOrMatch checks seed is this daemon's key; a daemon with no key yet
// whose escrow cannot be read takes it (written to the key file, not fresh).
func (s *Service) adoptOrMatch(seed []byte) error {
	k, err := s.key()
	if err != nil {
		s.keyMu.Lock()
		defer s.keyMu.Unlock()
		if s.k == nil {
			path := filepath.Join(s.d.Config.Home, KeyFile)
			nk, werr := writeKey(path, seed)
			if werr != nil {
				if _, serr := os.Stat(path); serr == nil {
					return api.Errorf(api.CodeConflict, "this home's key file cannot be read (%v), and is never written over", err).
						WithHint("move " + path + " aside, then run this again")
				}
				return werr
			}
			s.k, s.keyFresh = nk, false
		}
		k = s.k
	}
	if !ed25519.NewKeyFromSeed(seed).Equal(k) {
		return api.Errorf(api.CodeConflict, "the escrow holds another key than this daemon's").
			WithHint("zm tunnel key escrow writes this daemon's key there")
	}
	return nil
}

func (s *Service) handleUp(w http.ResponseWriter, r *http.Request) {
	var req api.TunnelUpRequest
	if err := api.DecodeJSON(r, &req); err != nil {
		api.WriteError(w, err)
		return
	}
	if req.PreviewDomain != "" && (!core.ValidPublicSuffix(req.PreviewDomain) || len(req.PreviewDomain) > 204) {
		api.WriteError(w, api.Errorf(api.CodeBadRequest, "preview_domain must be a lowercase DNS domain of at most 204 characters"))
		return
	}
	listen, err := loopbackAddr(req.Listen)
	if err != nil {
		api.WriteError(w, err)
		return
	}
	u, err := project.TunnelURL(req.URL)
	if err != nil {
		api.WriteError(w, err)
		return
	}
	if req.EdgeSecret != "" && !edgeSecretOK(req.EdgeSecret) {
		api.WriteError(w, api.Errorf(api.CodeBadRequest, "edge_secret: 16 to 64 lowercase hex characters"))
		return
	}
	if err := s.up(listen); err != nil {
		api.WriteError(w, api.Errorf(api.CodeUnavailable, "tunnel listener on %s: %v", listen, err))
		return
	}
	// The edge is seen again only by a request after this (a re-run's
	// check must be real).
	s.edgeSeen.Store(0)
	s.journal.load(nowFunc())
	if err := s.update(func(c *config.Tunnel) {
		*c = config.Tunnel{PreviewDomain: req.PreviewDomain, Listen: listen, URL: u, EdgeSecret: req.EdgeSecret, Key: c.Key}
	}); err != nil {
		s.stop() // never a listener config does not record
		api.WriteError(w, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, s.Status())
}

func (s *Service) handleDown(w http.ResponseWriter, r *http.Request) {
	s.stop()
	s.edgeSeen.Store(0)
	// The key escrow stays: the key is this daemon's identity, not the
	// listener's (it signs as a member too).
	if err := s.update(func(c *config.Tunnel) { *c = config.Tunnel{Key: c.Key} }); err != nil {
		api.WriteError(w, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, s.Status())
}

// save records t as config.json's tunnel block (re-read first: other
// modules write it too).
func (s *Service) save(t config.Tunnel) error {
	return s.update(func(c *config.Tunnel) { *c = t })
}

// update applies fn to the tunnel block as last saved and saves it, as one
// step under cfgMu: two at once cannot bring back an older listener or key
// escrow.
func (s *Service) update(fn func(*config.Tunnel)) error {
	s.cfgMu.Lock()
	defer s.cfgMu.Unlock()
	t := s.cfg()
	fn(&t)
	fresh, err := config.LoadFrom(s.d.Config.Home)
	if err != nil {
		return err
	}
	if fresh.File.Tunnel != t {
		fresh.File.Tunnel = t
		if err := fresh.Save(); err != nil {
			return err
		}
	}
	s.d.Config.File.Tunnel = t
	s.tcfg.Store(&t)
	return nil
}

func (s *Service) secrets() *secrets.Reader {
	if s.Secrets != nil {
		return s.Secrets
	}
	return secrets.Default()
}

// logger is the daemon's log (slog's default when a test gave none).
func (s *Service) logger() *slog.Logger {
	if s.d.Log != nil {
		return s.d.Log
	}
	return slog.Default()
}

// cfg is the tunnel block as last loaded or saved (read without a lock by
// every request).
func (s *Service) cfg() config.Tunnel {
	if p := s.tcfg.Load(); p != nil {
		return *p
	}
	return config.Tunnel{}
}

func (s *Service) stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.srv != nil {
		_ = s.srv.Close()
		s.srv, s.listen = nil, ""
	}
}

func edgeSecretOK(v string) bool {
	if len(v) < 16 || len(v) > 64 {
		return false
	}
	for _, c := range v {
		if !('0' <= c && c <= '9' || 'a' <= c && c <= 'f') {
			return false
		}
	}
	return true
}

// Start opens the listener when config names one (off by default).
func (s *Service) Start(ctx context.Context) error {
	if l := s.cfg().Listen; l != "" {
		if err := s.up(l); err != nil {
			s.logger().Warn("tunnel: listener", "listen", l, "err", err)
		}
		// The replay journal now, so a window of refused writes (a journal
		// that could not be read) counts from the start.
		s.journal.load(nowFunc())
	}
	return nil
}

// Close stops the listener.
func (s *Service) Close() error {
	s.journal.close()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.srv != nil {
		_ = s.srv.Close()
		s.srv, s.listen = nil, ""
	}
	return nil
}

// Status is GET /v0/tunnel.
func (s *Service) Status() api.TunnelStatus {
	fp, kerr := s.Fingerprint()
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.cfg()
	st := api.TunnelStatus{PreviewDomain: c.PreviewDomain, Listen: c.Listen, URL: s.URL(), Fingerprint: fp, Error: s.err, EdgeSecret: c.EdgeSecret != "", KeyEscrow: c.Key}
	if kerr != nil && st.Error == "" {
		st.Error = "no home key: " + kerr.Error() + " (zm tunnel key escrow --to <where the key is> adopts it; never clear it)"
	}
	if s.srv != nil {
		st.Running, st.Listen = true, s.listen
	}
	if n := s.edgeSeen.Load(); n > 0 {
		t := time.Unix(0, n).UTC()
		st.EdgeSeen = &t
	}
	return st
}

// NonceFile is the replay journal under $ZM_HOME (nonceJournal).
const NonceFile = "tunnel/nonces"

// Handler is the tunnel door in front of next (the listener serves it in
// front of the daemon's API; tests call it in process).
func (s *Service) Handler(next http.Handler) http.Handler {
	return core.PublicIngress(s.d).Wrap(&door{s: s, next: next, nonces: s.journal})
}

func (s *Service) up(listen string) error {
	addr, err := loopbackAddr(listen)
	if err != nil {
		return err
	}
	s.mu.Lock()
	running := s.srv != nil && s.listen == addr
	s.mu.Unlock()
	if running {
		return nil
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		s.mu.Lock()
		s.err = err.Error()
		s.mu.Unlock()
		return err
	}
	if _, err := s.key(); err != nil {
		ln.Close()
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.srv != nil {
		_ = s.srv.Close()
	}
	// No ConnContext: nothing here is local. The door attaches the signing
	// key as the Remote; tokens decide the rest.
	s.door.next = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.d.Handler == nil {
			api.WriteError(w, api.Errorf(api.CodeUnavailable, "the daemon is starting"))
			return
		}
		s.d.Handler.ServeHTTP(w, r)
	})
	srv := &http.Server{Handler: core.PublicIngress(s.d).Wrap(s.door), ReadHeaderTimeout: 10 * time.Second}
	s.srv, s.listen, s.err = srv, ln.Addr().String(), ""
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.logger().Warn("tunnel: listener stopped", "err", err)
			s.mu.Lock()
			if s.srv == srv {
				s.srv, s.err = nil, err.Error()
			}
			s.mu.Unlock()
		}
	}()
	s.logger().Info("tunnel: listening for cloudflared", "listen", s.listen, "url", s.URL())
	return nil
}

// loopbackAddr checks that listen is host:port on a loopback address.
func loopbackAddr(listen string) (string, error) {
	host, port, err := net.SplitHostPort(strings.TrimSpace(listen))
	if err != nil || port == "" {
		return "", api.Errorf(api.CodeBadRequest, "tunnel listen %q: want host:port, e.g. 127.0.0.1:7435", listen)
	}
	if strings.EqualFold(host, "localhost") {
		host = "127.0.0.1"
	}
	ip, err := netip.ParseAddr(host)
	if err != nil || !ip.IsLoopback() {
		return "", api.Errorf(api.CodeBadRequest, "tunnel listen %q: only a loopback address; cloudflared forwards to it", listen)
	}
	return net.JoinHostPort(ip.String(), port), nil
}

func (s *Service) key() (ed25519.PrivateKey, error) {
	s.keyMu.Lock()
	defer s.keyMu.Unlock()
	if s.k == nil {
		k, created, err := loadKey(context.Background(), filepath.Join(s.d.Config.Home, KeyFile), s.cfg().Key, s.secrets())
		if err != nil {
			return nil, err
		}
		s.k, s.keyFresh = k, created
	}
	return s.k, nil
}

func (s *Service) recoveryChallenge() (string, error) {
	s.recoveryOnce.Do(func() {
		var b [32]byte
		if _, s.recoveryErr = rand.Read(b[:]); s.recoveryErr == nil {
			s.recovery = base64.RawURLEncoding.EncodeToString(b[:])
		}
	})
	return s.recovery, s.recoveryErr
}

// Fingerprint is this daemon's key's pin (26 base32 characters).
func (s *Service) Fingerprint() (string, error) {
	k, err := s.key()
	if err != nil {
		return "", err
	}
	return Fingerprint(k.Public().(ed25519.PublicKey)), nil
}

// KeyID is the node a key-bound token records ("zmkey:<fp>").
func (s *Service) KeyID() (string, error) {
	k, err := s.key()
	if err != nil {
		return "", err
	}
	return KeyID(k.Public().(ed25519.PublicKey)), nil
}

// URL is tunnel.url, checked, or "".
func (s *Service) URL() string {
	u, err := project.TunnelURL(s.cfg().URL)
	if err != nil {
		return ""
	}
	return u
}

// Transport signs every request with this daemon's key for the home
// pinned by fp and returns only answers that home's key signed. A key that cannot be read
// fails every call with that error.
func (s *Service) Transport(fp string, dialTimeout time.Duration, extra http.Header) http.RoundTripper {
	k, err := s.key()
	if err != nil {
		return errTransport{err}
	}
	t := newTransport(k, fp, dialTimeout, extra)
	t.svc = s
	return t
}

type errTransport struct{ err error }

func (e errTransport) RoundTrip(*http.Request) (*http.Response, error) { return nil, e.err }
