package tunnel

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/sealbox"
)

// MaxAnswer bounds one answer the client reads whole to check its
// signature (a snapshot of a large project is tens of MB).
var MaxAnswer int64 = 1 << 30

// transport is the member's side: it signs every request with this
// daemon's key for the pinned home and hands back only answers that home
// signed, read whole and with every unsigned header dropped.
type transport struct {
	key   ed25519.PrivateKey
	keyB  string // public key, base64url
	home  string // the pinned home key fingerprint
	base  *http.Transport
	extra http.Header
	now   func() time.Time // this member's clock (tests make it slow)
	svc   *Service         // certifies this daemon's seal key (nil in unit tests of signing alone)
	// homeSeal is the home's seal key from its last verified answer (nil:
	// it certified none, so it runs a zm from before sealing). Shared by
	// NoReuse's copies.
	homeSeal *atomic.Pointer[[]byte]
}

func newTransport(key ed25519.PrivateKey, home string, dialTimeout time.Duration, extra http.Header) *transport {
	if dialTimeout <= 0 {
		dialTimeout = 5 * time.Second
	}
	base := &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		DialContext:         (&net.Dialer{Timeout: dialTimeout, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:   true,
		TLSHandshakeTimeout: 10 * time.Second,
		MaxIdleConns:        32, MaxIdleConnsPerHost: 32, IdleConnTimeout: 90 * time.Second,
	}
	return &transport{key: key, keyB: base64.RawURLEncoding.EncodeToString(key.Public().(ed25519.PublicKey)),
		home: home, base: base, extra: extra.Clone(), now: time.Now, homeSeal: new(atomic.Pointer[[]byte])}
}

// NoReuse is the same transport on connections that are never reused (a
// hedge must not go out on another idle connection that is just as stuck).
func (t *transport) NoReuse() http.RoundTripper {
	c := *t
	c.base = t.base.Clone()
	c.base.DisableKeepAlives = true
	return &c
}

// CloseIdleConnections closes the base transport's idle connections.
func (t *transport) CloseIdleConnections() { t.base.CloseIdleConnections() }

func (t *transport) RoundTrip(req *http.Request) (*http.Response, error) {
	var body []byte
	if req.Body != nil && req.Body != http.NoBody {
		b, err := io.ReadAll(req.Body)
		req.Body.Close()
		if err != nil {
			return nil, err
		}
		body = b
	}
	resp, err := t.roundTrip(req, body, "")
	if err != nil {
		return nil, err
	}
	// Only the door's signed, pre-dispatch refusal proves that no write
	// ran. Never retry an ambiguous transport failure or an unverified answer.
	// Without a token only the joining calls are offered a challenge.
	if resp.StatusCode != http.StatusServiceUnavailable || req.Method == http.MethodGet || req.Method == http.MethodHead ||
		req.Header.Get("Authorization") == "" && !tokenless(req.Method, req.URL.Path) {
		return resp, nil
	}
	recovery := recoveryValue(resp.Header)
	if recovery == "" || req.Context().Err() != nil {
		return resp, nil
	}
	resp.Body.Close()
	// The second attempt keeps all application meaning, including the write
	// id, but signs a fresh nonce with the current process's challenge. A
	// second refusal is returned unchanged, even if the home restarted again.
	return t.roundTrip(req, body, recovery)
}

// roundTrip sends one independently signed attempt and verifies its whole
// answer. body was captured once, including for non-seekable request bodies.
func (t *transport) roundTrip(req *http.Request, body []byte, recovery string) (*http.Response, error) {
	// The connection that carries the answer, closed if the answer is not
	// the home's (GotConn runs before base.RoundTrip returns).
	var used net.Conn
	out := req.Clone(httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{
		GotConn: func(i httptrace.GotConnInfo) { used = i.Conn },
	}))
	for k, v := range t.extra {
		out.Header[k] = append([]string(nil), v...)
	}
	api.DelHeader(out.Header, HeaderRecovery)
	if recovery != "" {
		api.SetHeader(out.Header, HeaderRecovery, recovery)
	}
	sealIt := api.GetHeader(out.Header, api.HeaderSeal) != ""
	dropSealHeaders(out.Header)
	if !sealIt && secretInClear(out.URL.Path, body) {
		// Never in the clear over the tunnel, even if a caller forgot to ask.
		return nil, errClear(out.URL.Path, body, "this request (not sealed)")
	}
	if cert := t.ownCert(); cert != "" {
		api.SetHeader(out.Header, api.HeaderSealKey, cert) // signed below: the home takes it as this key's
	}
	var nb [16]byte
	if _, err := rand.Read(nb[:]); err != nil {
		return nil, err
	}
	nonce := base64.RawURLEncoding.EncodeToString(nb[:])
	api.DelHeader(out.Header, HeaderSig)
	api.SetHeader(out.Header, HeaderKey, t.keyB)
	api.SetHeader(out.Header, HeaderDate, strconv.FormatInt(t.now().Unix(), 10))
	api.SetHeader(out.Header, HeaderNonce, nonce)
	if sealIt {
		// The body holds what Cloudflare must not read: it goes sealed to
		// the home's certified key, bound to this request, or not at all.
		sealed, err := t.sealBody(req.Context(), req, out, body)
		if err != nil {
			return nil, err
		}
		body = sealed
		api.SetHeader(out.Header, api.HeaderSealed, "1")
	}
	out.Body, out.ContentLength = http.NoBody, 0
	if len(body) > 0 {
		out.Body, out.ContentLength = io.NopCloser(bytes.NewReader(body)), int64(len(body))
		out.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
	}
	sig := ed25519.Sign(t.key, requestString(t.home, out.Method, out.URL.RequestURI(), out.Header, body))
	api.SetHeader(out.Header, HeaderSig, base64.RawURLEncoding.EncodeToString(sig))
	resp, err := t.base.RoundTrip(out)
	if err != nil {
		return nil, err
	}
	resp, err = t.verify(req.Context(), resp, nonce)
	if errors.Is(err, ErrHomeKey) {
		t.distrust(used)
	}
	return resp, err
}

// distrust drops the connection that carried an answer the pinned home did
// not sign, and every idle one to the host: after a hijack each pooled
// connection may lead to the impostor, and must not cost a read once the
// hostname is back. A connection read to the end is idle again by now.
func (t *transport) distrust(c net.Conn) {
	if c != nil {
		c.Close()
	}
	t.base.CloseIdleConnections()
}

// verify checks an answer before anything reads it as data: the key it
// names must be the pinned one, and its signature must cover this
// request's nonce, the status, the signed headers and the whole body.
func (t *transport) verify(ctx context.Context, resp *http.Response, nonce string) (*http.Response, error) {
	hk, hs := get(resp.Header, HeaderHomeKey), get(resp.Header, HeaderHomeSig)
	if hk == "" || hs == "" {
		resp.Body.Close()
		return nil, fmt.Errorf("%w (HTTP %d): the tunnel or the home is down", ErrUnsigned, resp.StatusCode)
	}
	pub, err := base64.RawURLEncoding.DecodeString(hk)
	if err != nil || len(pub) != ed25519.PublicKeySize || Fingerprint(pub) != t.home {
		resp.Body.Close()
		return nil, ErrHomeKey
	}
	sig, err := base64.RawURLEncoding.DecodeString(hs)
	if err != nil {
		resp.Body.Close()
		return nil, ErrHomeKey
	}
	b, err := readAll(ctx, resp.Body)
	resp.Body.Close()
	if err != nil {
		return nil, err
	}
	if !ed25519.Verify(pub, responseString(t.home, nonce, resp.StatusCode, resp.Header, b), sig) {
		return nil, ErrHomeKey
	}
	// The home's seal key as of this answer: a home that stops certifying
	// one (downgraded) gets nothing sealed to an older one.
	if sp := sealbox.VerifyCert(pub, api.GetHeader(resp.Header, api.HeaderSealKey)); sp != nil {
		t.homeSeal.Store(&sp)
	} else {
		t.homeSeal.Store(nil)
	}
	for k := range resp.Header {
		if !signedHeader(k, false) {
			delete(resp.Header, k)
		}
	}
	resp.Body, resp.ContentLength = io.NopCloser(bytes.NewReader(b)), int64(len(b))
	return resp, nil
}

// readAll reads a whole answer, telling the caller's progress func (a
// hedge's silence timer) each time bytes arrive.
func readAll(ctx context.Context, r io.Reader) ([]byte, error) {
	progress := core.ReadProgress(ctx)
	var buf bytes.Buffer
	chunk := make([]byte, 32<<10)
	for {
		n, err := r.Read(chunk)
		if n > 0 {
			if int64(buf.Len()+n) > MaxAnswer {
				return nil, fmt.Errorf("answer over %d bytes", MaxAnswer)
			}
			buf.Write(chunk[:n])
			if progress != nil {
				progress()
			}
		}
		if err == io.EOF {
			return buf.Bytes(), nil
		}
		if err != nil {
			return nil, err
		}
	}
}

// ownCert is this daemon's Zm-Seal-Key, or "".
func (t *transport) ownCert() string {
	if t.svc == nil {
		return ""
	}
	return t.svc.SealCert()
}

// sealBody seals body to the home's seal key for the request out (signed
// with its date and nonce).
func (t *transport) sealBody(ctx context.Context, req, out *http.Request, body []byte) ([]byte, error) {
	if t.svc == nil || t.svc.sealOff.Load() {
		return nil, sealbox.ErrNoKey("this machine")
	}
	pub, err := t.homeSealKey(ctx, req)
	if err != nil {
		return nil, err
	}
	own, home := KeyID(t.key.Public().(ed25519.PublicKey)), "zmkey:"+t.home
	purpose := requestPurpose(out.Method, out.URL.RequestURI(), get(out.Header, HeaderDate), get(out.Header, HeaderNonce))
	return sealbox.SealWhole(body, func(pt []byte) (string, error) {
		return sealbox.Seal(own, home, pub, purpose, MaxSkew, pt, t.now())
	})
}

// homeSealKey is the home's certified seal key: from the last answer, else
// from a signed probe (any answer of the door carries it, a 401 too).
func (t *transport) homeSealKey(ctx context.Context, req *http.Request) ([]byte, error) {
	if p := t.homeSeal.Load(); p != nil {
		return *p, nil
	}
	u := *req.URL
	u.Path, u.RawPath, u.RawQuery, u.Fragment = "/v0/health", "", "", ""
	probe, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := t.roundTrip(probe, nil, "")
	if err != nil {
		return nil, err
	}
	resp.Body.Close()
	if p := t.homeSeal.Load(); p != nil {
		return *p, nil
	}
	return nil, sealbox.ErrNoKey("the home")
}
