package tailnet

// Sealing over the tailnet (SPEC 17.11 "Sealed secrets", zm-eakh.12.48.2).
// A run-target call's secrets or jit_config go between daemons sealed to the
// receiving daemon's key on every leg, the tailnet included, with the same
// primitive (kit/sealbox) and the same whole-body envelope as the tunnel.
//
// How each end learns the other's key: every request a member's (or a
// host's) daemon sends its home over the tailnet, and every answer the home
// gives over its tailnet listener, names the sender's identity key
// (Zm-Seal-Id) and the seal key it certified (Zm-Seal-Key). Neither is
// signed per message here, as the tunnel's are: what vouches for them is
// the tailnet itself, a connection to the pinned node (the member's side
// checks the peer of every new connection) or from the node WhoIs names (the
// home's side). So over the tailnet a seal key is as authentic as the
// tailnet's own node keys, which Tailscale's control plane distributes.
//
// Old peers: a home that answers /v0/health (2xx) naming no key runs a
// Potluck (or zm) from before this; a member sends it secrets as before, in
// the clear over the tailnet, warning once. A home that named a key to this
// machine is recorded with the mode it reported (core.Sealer.HomeSealed):
// required, it never gets them in the clear again until this machine's
// owner forgets it; preferred, it gets them in the clear after such a 2xx
// (an auto-updater's rollback), warned every call. A daemon that
// cannot read its own key refuses: that is a fault to fix, not an old build.
// A caller that names a key and sends secrets in the clear is refused: it can
// seal, so the clear copy is a bug or a stripped envelope, never used.

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/sealbox"
)

// Tunables (tests shorten them).
var (
	// SealSkew is how long a body sealed over the tailnet opens (from its
	// date) and how far the two clocks may differ.
	SealSkew = 5 * time.Minute
	// sealMaxBody bounds a run-target body the home reads to check it.
	sealMaxBody int64 = 16 << 20
	// sealNow is the clock (tests step it).
	sealNow = time.Now
)

// sealAuth authenticates a request's token at the home before anything
// sealed is read, opened or recorded (tests replace it): the machine the
// token is for, or the refusal.
var sealAuth = func(s *Service, r *http.Request, rm core.Remote) (string, error) {
	unauth := api.Errorf(api.CodeUnauthenticated, "authentication required").WithHint("a sealed call needs a member's token")
	if s.d == nil || s.d.Projects == nil || r.Header.Get(api.HeaderAuthorization) == "" {
		return "", unauth
	}
	ctx := s.d.Authenticate(r.WithContext(core.WithRemote(r.Context(), rm)))
	if err := core.AuthErr(ctx); err != nil {
		return "", err
	}
	sub := core.SubjectOf(ctx)
	if sub == nil {
		return "", unauth
	}
	if sub.Machine != "" {
		return sub.Machine, nil
	}
	return "node:" + rm.NodeID, nil
}

// sealer is this daemon's seal key (the tunnel module's), or nil.
func sealer(d *core.Deps) core.Sealer {
	if d == nil {
		return nil
	}
	sl, _ := d.Transport(core.ViaTunnel).(core.Sealer)
	return sl
}

// ---- the member's side ----

// sealPeer is the home's key as an answer named it, and the sealing mode
// it reported beside it.
type sealPeer struct {
	id   string
	key  []byte
	mode string
}

// sealTransport names this daemon's key on every request to the home,
// seals a body the caller asked to (Zm-Seal) to the home's key, refuses a
// run-target body holding secrets that it was not asked to seal, and keeps
// the home's key from each answer that names one (an answer naming none
// never clears it: only a 2xx /v0/health naming none says "older home").
type sealTransport struct {
	next http.RoundTripper
	d    *core.Deps
	// fp is the home's pin (the join's fingerprint): what HomeSealed keys.
	fp string
	// self is this daemon's own stable node id (bound into every purpose).
	self func(*http.Request) string
	peer *atomic.Pointer[sealPeer] // shared by NoReuse's copies
	// warned: this transport sent secrets in the clear once (the Warn is
	// once per home per start). Shared by NoReuse's copies.
	warned *atomic.Bool
}

func newSealTransport(d *core.Deps, next http.RoundTripper) *sealTransport {
	return &sealTransport{next: next, d: d, self: func(*http.Request) string { return "" },
		peer: new(atomic.Pointer[sealPeer]), warned: new(atomic.Bool)}
}

// errOlderHome is a home that answered /v0/health (2xx) naming no seal key.
var errOlderHome = errors.New("the home names no seal key")

// NoReuse is the same transport on connections that are never reused (a
// hedge).
func (t *sealTransport) NoReuse() http.RoundTripper {
	c := *t
	switch b := t.next.(type) {
	case *http.Transport:
		x := b.Clone()
		x.DisableKeepAlives = true
		c.next = x
	case headerTransport:
		x := b.Transport.Clone()
		x.DisableKeepAlives = true
		c.next = headerTransport{x, b.h}
	}
	return &c
}

// CloseIdleConnections closes the base transport's idle connections.
func (t *sealTransport) CloseIdleConnections() {
	if c, ok := t.next.(interface{ CloseIdleConnections() }); ok {
		c.CloseIdleConnections()
	}
}

// ownKey is this daemon's identity and cert: "", "" (sealing off: a test
// standing in for a build from before sealing), or an error (a key that
// cannot be read: a fault, logged loudly, never a reason to send in the
// clear).
func ownKey(d *core.Deps) (string, string, error) {
	sl := sealer(d)
	if sl == nil {
		return "", "", nil
	}
	id, cert, err := sl.SealIdentity()
	if err != nil && d.Log != nil {
		// At most once a minute: every answer of the listener asks.
		if now := time.Now().UnixNano(); now-ownKeyLogged.Load() >= int64(time.Minute) {
			ownKeyLogged.Store(now)
			d.Log.Error("sealing: this machine cannot read its seal key; no secret leaves it until fixed", "err", err)
		}
	}
	return id, cert, err
}

// ownKeyLogged is when the last "cannot read its seal key" was logged.
var ownKeyLogged atomic.Int64

func (t *sealTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	sealIt := api.GetHeader(req.Header, api.HeaderSeal) != ""
	inspect := sealIt || sealbox.RunTargetPath(req.URL.Path)
	out := req.Clone(req.Context())
	sealbox.DropHeaders(out.Header)
	id, cert, ownErr := ownKey(t.d)
	if inspect {
		var body []byte
		if req.Body != nil && req.Body != http.NoBody {
			b, err := io.ReadAll(req.Body)
			req.Body.Close()
			if err != nil {
				return nil, err
			}
			body = b
		}
		if !sealIt && sealbox.CarriesSecret(body) {
			// Never in the clear, even if a caller forgot to ask.
			return nil, sealbox.ErrNoKey("this request (not sealed)")
		}
		if sealIt {
			switch {
			case ownErr != nil:
				return nil, api.Errorf(api.CodeUnavailable, "this machine cannot read its seal key, so it sends no secrets: %v", ownErr).
					WithHint("fix the key file named (zm status shows the home), then try again")
			case id == "":
				// Sealing off: this daemon stands in for a build from before
				// sealing, which sent the body as it is.
			default:
				sealed, err := t.seal(req, out, body)
				switch {
				case errors.Is(err, errOlderHome):
					// The pinned home answered /v0/health (2xx) naming no key.
					// What it last reported decides (docs/daemon.md, "Sealing
					// modes"): required, never in the clear until this machine's
					// owner forgets it; preferred (an auto-updater's rollback),
					// in the clear, warned every call; never sealed, as before.
					mode := ""
					if sl := sealer(t.d); sl != nil && t.fp != "" {
						m, herr := sl.HomeSealed(t.fp)
						if herr != nil {
							return nil, herr
						}
						mode = m
					}
					switch mode {
					case "":
						if t.warned.CompareAndSwap(false, true) && t.d != nil && t.d.Log != nil {
							t.d.Log.Warn("sealing: secrets sent in the clear over the tailnet to a pool's machine that cannot seal: update it", "home", req.URL.Host)
						}
					case "preferred":
						if t.d != nil && t.d.Log != nil {
							t.d.Log.Warn("sealing: the pool's machine stopped sealing (it named a key before, in a pool that prefers sealing): secrets sent in the clear over the tailnet; update it", "home", req.URL.Host)
						}
					default:
						return nil, api.Errorf(api.CodeForbidden, "the pool's machine named a seal key to this machine before, in a pool that requires sealing, and names none now: refused (a downgrade, or it was put back to an older build)").
							WithHint("if its owner put it back to an older build on purpose: potluck sealing forget -P <pool>, on this machine (it forgets that pool machine for every pool it serves)")
					}
					for _, n := range []string{api.HeaderSealDate, api.HeaderSealNonce, api.HeaderSealed, api.HeaderSealSig} {
						api.DelHeader(out.Header, n)
					}
				case err != nil:
					return nil, err
				default:
					body = sealed
				}
			}
		}
		out.Body, out.ContentLength, out.GetBody = http.NoBody, 0, nil
		if len(body) > 0 {
			out.Body, out.ContentLength = io.NopCloser(bytes.NewReader(body)), int64(len(body))
			out.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
		}
	}
	if id != "" {
		api.SetHeader(out.Header, api.HeaderSealID, id)
		api.SetHeader(out.Header, api.HeaderSealKey, cert)
	}
	resp, err := t.next.RoundTrip(out)
	if err != nil {
		return nil, err
	}
	// The home's key and mode as of this answer, kept (and recorded across
	// restarts) when it names a key. An answer naming none changes nothing,
	// except that one to a request sent sealed drops the kept key, so the
	// next call probes (a rollback is noticed without a restart).
	if hid, key, ok := sealbox.Peer(api.GetHeader(resp.Header, api.HeaderSealID), api.GetHeader(resp.Header, api.HeaderSealKey)); ok {
		mode := api.GetHeader(resp.Header, api.HeaderSealing)
		old := t.peer.Swap(&sealPeer{id: hid, key: key, mode: mode})
		if (old == nil || old.id != hid || old.mode != mode) && t.fp != "" && (id != "" || ownErr != nil) {
			if sl := sealer(t.d); sl != nil {
				if err := sl.MarkHomeSealed(t.fp, hid, mode); err != nil && t.d.Log != nil {
					t.d.Log.Warn("sealing: record the home's seal key", "err", err)
				}
			}
		}
	} else if api.GetHeader(out.Header, api.HeaderSealed) != "" {
		t.peer.Store(nil)
	}
	return resp, nil
}

// seal is body sealed to the home's key for the one request out, which it
// dates and gives a nonce; errOlderHome when the home names no key.
func (t *sealTransport) seal(req, out *http.Request, body []byte) ([]byte, error) {
	sl := sealer(t.d)
	p, err := t.homeKey(req)
	if err != nil {
		return nil, err
	}
	var nb [16]byte
	if _, err := rand.Read(nb[:]); err != nil {
		return nil, err
	}
	now := sealNow()
	date, nonce := strconv.FormatInt(now.Unix(), 10), base64.RawURLEncoding.EncodeToString(nb[:])
	api.SetHeader(out.Header, api.HeaderSealDate, date)
	api.SetHeader(out.Header, api.HeaderSealNonce, nonce)
	api.SetHeader(out.Header, api.HeaderSealed, "1")
	purpose := tailnetPurpose(out.Method, out.URL.RequestURI(), date, nonce, t.self(req), out.Header)
	sealed, err := sealbox.SealWhole(body, func(pt []byte) (string, error) {
		return sl.SealTo(p.id, p.key, purpose, SealSkew, pt)
	})
	if err != nil {
		return nil, err
	}
	// Proof of possession: the identity this request names signs its
	// purpose and the sealed body, so a seal id is taken only from whoever
	// holds its key.
	sig, err := sl.SignSeal(sealbox.SigMessage(purpose, sealed))
	if err != nil {
		return nil, api.Errorf(api.CodeUnavailable, "this machine cannot sign with its identity key, so it sends no secrets: %v", err)
	}
	api.SetHeader(out.Header, api.HeaderSealSig, sig)
	return sealed, nil
}

// tailnetPurpose binds a body sealed over the tailnet to its one request:
// method, URI, date, nonce, the sending node (WhoIs names it at the home),
// and the project the request names (headers the tunnel signs, and nothing
// signs here).
func tailnetPurpose(method, uri, date, nonce, node string, h http.Header) string {
	return sealbox.RequestPurpose(method, uri, date, nonce) + "\nnode " + node + "\nproject " + api.GetHeader(h, api.HeaderProject)
}

// homeKey is the home's key: the one an answer named (while the record
// still holds it: an operator's forget drops it), else from a probe of
// /v0/health. A 2xx probe naming no key is errOlderHome; any other probe
// answer without a key (a 503 while starting) is an error, never a reason to
// send in the clear.
func (t *sealTransport) homeKey(req *http.Request) (*sealPeer, error) {
	if p := t.peer.Load(); p != nil {
		if t.fp == "" {
			return p, nil
		}
		if sl := sealer(t.d); sl != nil {
			if mode, err := sl.HomeSealed(t.fp); err != nil || mode != "" {
				return p, nil // still recorded (or unreadable: sealing is safe)
			}
		}
		t.peer.CompareAndSwap(p, nil) // forgotten by the operator
	}
	u := *req.URL
	u.Path, u.RawPath, u.RawQuery, u.Fragment = "/v0/health", "", "", ""
	probe, err := http.NewRequestWithContext(req.Context(), http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := t.RoundTrip(probe)
	if err != nil {
		return nil, err
	}
	resp.Body.Close()
	if p := t.peer.Load(); p != nil {
		return p, nil
	}
	if resp.StatusCode/100 == 2 {
		return nil, errOlderHome
	}
	return nil, api.Errorf(api.CodeUnavailable, "the pool's machine answered %d and named no seal key: no secret is sent", resp.StatusCode).
		WithHint("try again in a moment (it may be starting)")
}

// ---- the home's side ----

// nameKey names this daemon's key on an answer (every answer of the
// tailnet listener, a 503 while starting included), and the sealing mode it
// reports (required when any pool it serves requires sealing). It answers
// the key state: ok, off (a test standing in for an older build), or a fault.
func (s *Service) nameKey(w http.ResponseWriter, r *http.Request) (ok bool, fault error) {
	id, cert, err := ownKey(s.d)
	if err != nil {
		return false, err
	}
	if id == "" {
		return false, nil
	}
	api.SetHeader(w.Header(), api.HeaderSealID, id)
	api.SetHeader(w.Header(), api.HeaderSealKey, cert)
	if rep, ok := s.d.Service(core.SealingService).(core.SealingReporter); ok {
		api.SetHeader(w.Header(), api.HeaderSealing, rep.ReportedSealing(r.Context()))
	}
	return true, nil
}

// sealDoor is the tailnet listener's sealing step, between WhoIs and the
// routes: it takes the caller's key, authenticates the caller's token before
// reading, opening or recording anything sealed, opens a sealed body, and
// refuses secrets in the clear from a caller that seals. It writes the
// refusal and answers false when the request must stop. own and fault are
// nameKey's.
func (s *Service) sealDoor(w http.ResponseWriter, r *http.Request, rm *core.Remote, own bool, fault error) (*http.Request, bool) {
	if rm.NodeID != "" {
		// Only a caller WhoIs named: the node vouches for the key it names.
		if id, key, ok := sealbox.Peer(api.GetHeader(r.Header, api.HeaderSealID), api.GetHeader(r.Header, api.HeaderSealKey)); ok {
			rm.SealID, rm.SealKey = id, key
		}
	}
	sealed := api.GetHeader(r.Header, api.HeaderSealed) != ""
	date, nonce := api.GetHeader(r.Header, api.HeaderSealDate), api.GetHeader(r.Header, api.HeaderSealNonce)
	idB64, sig := api.GetHeader(r.Header, api.HeaderSealID), api.GetHeader(r.Header, api.HeaderSealSig)
	sealbox.DropHeaders(r.Header)
	if !sealed && (rm.SealKey == nil || !sealbox.RunTargetPath(r.URL.Path)) {
		// Nothing sealed, and nothing to check: a caller from before sealing
		// on the tailnet sends what it always sent; the route decides by the
		// pool's sealing mode (runtarget/sealing.go).
		return r, true
	}
	switch {
	case fault != nil:
		// Not why: the caller is not authenticated yet (its owner's log says).
		api.WriteError(w, api.Errorf(api.CodeUnavailable, "this machine cannot take secrets now").
			WithHint("its owner sees why in its log; calls without secrets still work"))
		return r, false
	case !own && sealed:
		api.WriteError(w, sealbox.ErrOpen)
		return r, false
	case !own:
		return r, true // sealing off: a build from before sealing checked nothing
	}
	if _, err := sealAuth(s, r, *rm); err != nil {
		if sealed {
			api.WriteError(w, err)
			return r, false
		}
		// Not a member's token (perhaps a tailnet login the route takes):
		// nothing is read here, and the key it named is not taken, so the
		// route's sealing mode check sees a caller that sends in the clear.
		rm.SealID, rm.SealKey = "", nil
		return r, true
	}
	body, rerr := io.ReadAll(io.LimitReader(r.Body, sealMaxBody+1))
	if rerr != nil {
		api.WriteError(w, api.Errorf(api.CodeBadRequest, "reading the body: %v", rerr))
		return r, false
	}
	if int64(len(body)) > sealMaxBody {
		api.WriteError(w, api.Errorf(api.CodeTooLarge, "body over %d bytes", sealMaxBody))
		return r, false
	}
	if sealed {
		opened, e := s.openSealed(r, rm, idB64, sig, body, date, nonce)
		if e != nil {
			api.WriteError(w, e)
			return r, false
		}
		body = opened
	} else if sealbox.CarriesSecret(body) {
		// A caller that seals sent secrets in the clear (a bug, or an
		// envelope stripped on the way): never used.
		api.WriteError(w, api.Errorf(api.CodeForbidden, "secrets and jit_config must arrive sealed from a machine that can seal them, and this call carried them in the clear: refused, not used").
			WithHint("update the calling machine's daemon; if it is current, report this"))
		return r, false
	}
	r.Body, r.ContentLength = io.NopCloser(bytes.NewReader(body)), int64(len(body))
	return r, true
}

// openSealed opens a body sealed for this daemon by rm (its token already
// authenticated): dated within SealSkew, bound to rm's node, signed by the
// identity it names (idB64; sig over its purpose and the sealed body), its
// nonce never seen with that seal id.
func (s *Service) openSealed(r *http.Request, rm *core.Remote, idB64, sig string, body []byte, date, nonce string) ([]byte, error) {
	sl := sealer(s.d)
	if sl == nil || rm.SealID == "" || rm.NodeID == "" || len(nonce) < 16 || len(nonce) > 64 {
		return nil, sealbox.ErrOpen
	}
	sec, err := strconv.ParseInt(date, 10, 64)
	now := sealNow()
	if err != nil || date != strconv.FormatInt(sec, 10) {
		return nil, sealbox.ErrOpen
	}
	if skew := now.Sub(time.Unix(sec, 0)); skew > SealSkew || skew < -SealSkew {
		return nil, api.Errorf(api.CodeBadRequest, "this request's clock and the home's differ by %s (at most %s)", skew.Round(time.Second), SealSkew).
			WithHint("one of the two clocks is wrong: check this machine's (automatic time), then the home's")
	}
	uri := r.RequestURI
	if uri == "" {
		uri = r.URL.RequestURI()
	}
	purpose := tailnetPurpose(r.Method, uri, date, nonce, rm.NodeID, r.Header)
	if !sealbox.VerifySig(idB64, sig, sealbox.SigMessage(purpose, body)) {
		return nil, sealbox.ErrOpen // not signed by the identity it names
	}
	opened, err := sealbox.OpenWhole(body, func(sealed string) ([]byte, error) { return sl.OpenFrom(rm.SealID, purpose, sealed) })
	if err != nil {
		return nil, sealbox.ErrOpen
	}
	// Recorded once it opened, for an authenticated member's token and a
	// signature by the seal id's own key only (base-mode HPKE lets anyone
	// seal to this key; the token, the signature and the per-node cap bound
	// the set).
	switch s.sealNonces.add(rm.SealID, rm.NodeID, nonce, time.Unix(sec, 0).Add(2*SealSkew), now) {
	case nonceSeen:
		return nil, api.Errorf(api.CodeBadRequest, "this request was already received (a replay)")
	case nonceFull:
		return nil, api.Errorf(api.CodeUnavailable, "too many sealed requests from this machine; try again in a minute")
	}
	return opened, nil
}

// nonceSet is the nonces of sealed bodies opened over the tailnet, by seal
// id, until they could no longer open, at most sealNoncesPerNode per calling
// node. In memory: after a restart a body sealed before it could be taken
// again within its window (docs/daemon.md).
type nonceSet struct {
	mu     sync.Mutex
	seen   map[string]time.Time // seal id + "\n" + nonce -> until
	byNode map[string]int
	queue  []nonceEntry // in the order added (their untils near sorted)
	head   int
}

type nonceEntry struct {
	key, node string
	until     time.Time
}

// Bounds of the set: in all, and per calling node.
var (
	sealNoncesMax     = 1 << 16
	sealNoncesPerNode = 1 << 12
)

const (
	nonceNew = iota
	nonceSeen
	nonceFull
)

// add records nonce under sealID for node until until. Expired entries go
// from the front of the queue first (amortised constant time, no scan).
func (n *nonceSet) add(sealID, node, nonce string, until, now time.Time) int {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.seen == nil {
		n.seen, n.byNode = map[string]time.Time{}, map[string]int{}
	}
	for n.head < len(n.queue) && !now.Before(n.queue[n.head].until) {
		e := n.queue[n.head]
		if n.seen[e.key].Equal(e.until) {
			delete(n.seen, e.key)
		}
		if n.byNode[e.node]--; n.byNode[e.node] <= 0 {
			delete(n.byNode, e.node)
		}
		n.queue[n.head] = nonceEntry{}
		n.head++
	}
	if n.head > 1024 && n.head*2 > len(n.queue) {
		n.queue = append([]nonceEntry(nil), n.queue[n.head:]...)
		n.head = 0
	}
	k := sealID + "\n" + nonce
	if t, ok := n.seen[k]; ok && now.Before(t) {
		return nonceSeen
	}
	if n.byNode[node] >= sealNoncesPerNode || len(n.seen) >= sealNoncesMax {
		// Full of live nonces: refuse rather than forget one early.
		return nonceFull
	}
	n.seen[k] = until
	n.byNode[node]++
	n.queue = append(n.queue, nonceEntry{key: k, node: node, until: until})
	return nonceNew
}
