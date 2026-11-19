package tunnel

import (
	"bytes"
	"crypto/ed25519"
	"crypto/subtle"
	"encoding/base64"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/ids"
	"github.com/sengirunners/potluck/kit/sealbox"
)

// Tunables (tests shorten them).
var (
	// MaxSkew: how far a request's Zm-Date may be from the home's clock.
	MaxSkew = 5 * time.Minute
	// MaxBody bounds a request body on the tunnel (the replica's proxy
	// refuses over 1 MiB; an outbox batch is larger).
	MaxBody int64 = 16 << 20
	// MaxNoncesPerKey bounds one member key's share of the replay cache
	// (about 200 requests a second over its 11 minutes); past it that key's
	// requests are refused (503) until entries age out, never let through
	// unchecked.
	MaxNoncesPerKey = 1 << 17
	// EdgeSecretHeader carries tunnel.edge_secret when it is configured (a
	// Cloudflare Transform Rule adds it at the edge): only then, and only
	// from a loopback peer, is CF-Connecting-IP believed.
	EdgeSecretHeader = "X-Zm-Edge-Secret"
	// EdgeSecretHeaderNew is EdgeSecretHeader's new name, taken as well.
	EdgeSecretHeaderNew = "X-Potluck-Edge-Secret" // X-: never a signed header (sign.go)
	// MaxNonceLen bounds a nonce (the client sends 22 characters).
	MaxNonceLen = 64
	// nowFunc is the home's clock (tests step it).
	nowFunc = time.Now
)

// door is the tunnel listener's handler (SPEC 17.11), in front of the
// daemon's full API: every request signed by a daemon key, fresh and never
// replayed; without a token only redemption; never a live
// event stream; every answer signed by this daemon's key.
type door struct {
	s      *Service
	next   http.Handler
	nonces *nonceJournal
}

func (d *door) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	key, err := d.s.key()
	if err != nil {
		w.Header().Set("Cache-Control", "no-store")
		api.WriteError(w, api.Errorf(api.CodeUnavailable, "tunnel key: %v", err))
		return
	}
	if d.fromEdge(r) {
		d.s.edgeSeen.Store(time.Now().UnixNano())
	}
	bw := &bufWriter{h: http.Header{}}
	issuedRecovery := false
	nonce := get(r.Header, HeaderNonce)
	defer func() {
		if p := recover(); p != nil {
			panic(p) // http.ErrAbortHandler: no answer at all, never half of one
		}
		// Only this door's pre-dispatch refusal may promise a retry is safe.
		// A handler's own 503 can follow side effects and must carry none.
		if !issuedRecovery {
			api.DelHeader(bw.h, HeaderRecovery)
		}
		if api.GetHeader(bw.h, api.HeaderProtocol) == "" {
			// The door's own refusals are this daemon's answers too: a
			// client reads them as errors, never as another protocol.
			api.SetHeader(bw.h, api.HeaderProtocol, api.WireProtocol)
		}
		bw.finish(w, key, nonce, d.s.SealCert())
	}()
	body, err := io.ReadAll(io.LimitReader(r.Body, MaxBody+1))
	if err != nil {
		api.WriteError(bw, api.Errorf(api.CodeBadRequest, "reading the body: %v", err))
		return
	}
	if int64(len(body)) > MaxBody {
		api.WriteError(bw, api.Errorf(api.CodeTooLarge, "body over %d bytes", MaxBody))
		return
	}
	fp := Fingerprint(key.Public().(ed25519.PublicKey))
	caller, ok := verifyRequest(r, fp, body)
	if !ok {
		api.WriteError(bw, api.Errorf(api.CodeUnauthenticated, "every request on this listener is signed by a %s daemon's key", config.Program).
			WithHint("join with "+config.Command("join", "<https code>")+"; this is not a web page"))
		return
	}
	// Freshness is 400, not 401: three 401s purge a replica's token, and a
	// skewed clock must not do that. Only the distance between the two
	// clocks counts, never when this daemon started: a clock correction on
	// either side does not lock members out beyond the skew allowed.
	sec, _ := strconv.ParseInt(get(r.Header, HeaderDate), 10, 64)
	now := nowFunc()
	if skew := now.Sub(time.Unix(sec, 0)); skew > MaxSkew || skew < -MaxSkew {
		api.WriteError(bw, api.Errorf(api.CodeBadRequest, "this request's clock and the home's differ by %s (at most %s)", skew.Round(time.Second), MaxSkew).
			WithHint("one of the two clocks is wrong: check this machine's (automatic time), then the home's"))
		return
	}
	if r.Header.Get(api.HeaderAuthorization) == "" && !tokenless(r.Method, r.URL.Path) {
		api.WriteError(bw, api.Errorf(api.CodeUnauthenticated, "authentication required").
			WithHint("only a member's key-bound token works here ("+config.Command("join")+")"))
		return
	}
	if r.Header.Get(api.HeaderAuthorization) == "" && r.Method == http.MethodPost && githubJoinCall(r.Method, r.URL.Path) {
		// Tokenless callers (anyone can make a key) get a bounded cache of
		// their own, for the skew window only: a replayed start or collect
		// is refused (a replayed GET is harmless). Each client address has a
		// cap before the global one, so one address cannot fill it.
		switch d.s.tokenless.add(core.AddrBucket(d.clientAddr(r)), caller, nonce, sec, now) {
		case nonceSeen:
			api.WriteError(bw, api.Errorf(api.CodeBadRequest, "this request was already received (a replay)"))
			return
		case nonceFull:
			api.WriteError(bw, api.Errorf(api.CodeUnavailable, "too many joins in progress; try again in a minute"))
			return
		}
	}
	own := KeyID(key.Public().(ed25519.PublicKey))
	rm := core.Remote{Via: core.ViaTunnel, NodeID: caller, Addr: d.clientAddr(r), Self: caller == own}
	if !d.s.sealOff.Load() {
		// The caller's seal key, only as its own identity certified it (in a
		// header this request's signature covers): what the authority seals
		// a host's op to (SPEC 17.11 "Sealed secrets").
		pub, _ := base64.RawURLEncoding.DecodeString(get(r.Header, HeaderKey))
		if rm.SealKey = sealbox.VerifyCert(pub, api.GetHeader(r.Header, api.HeaderSealKey)); rm.SealKey != nil {
			rm.SealID = caller
		}
	}
	ctx := core.WithRemote(r.Context(), rm)
	r = r.WithContext(ctx)
	if r.Header.Get(api.HeaderAuthorization) != "" && d.s.d.Projects != nil &&
		api.IsCode(core.AuthErr(d.s.d.Authenticate(r)), api.CodeUnauthenticated) {
		// A token that is not this key's (a minted one, another key's, a
		// revoked one) reaches nothing, not even the public routes. Other
		// refusals (409 rescope) are the route's to answer.
		api.WriteError(bw, api.Errorf(api.CodeUnauthenticated, "invalid or expired token"))
		return
	}
	// Replays are refused for token callers only, so only a member's key
	// can put nonces in the cache (anyone can make a key). A replayed
	// redemption gets the same key-bound token again, useless to whoever
	// replays it.
	if r.Header.Get(api.HeaderAuthorization) != "" {
		switch d.nonces.add(caller, nonce, sec, now) {
		case nonceNoJournal:
			api.WriteError(bw, api.Errorf(api.CodeUnavailable, "the home cannot record this request against replays now").
				WithHint("try again shortly; the home's owner should check its disk"))
			return
		case nonceSeen:
			api.WriteError(bw, api.Errorf(api.CodeBadRequest, "this request was already received (a replay)"))
			return
		case nonceFull:
			api.WriteError(bw, api.Errorf(api.CodeUnavailable, "too many requests; try again in a minute"))
			return
		}
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead && d.nonces.degraded() {
		// A current challenge proves this request was signed after this
		// process started. Missing old nonces cannot hide its replay: the
		// normal authenticated nonce append above still has to succeed.
		// The joining calls (redemption, the GitHub join's POSTs) are
		// offered it too: they never had journal entries to lose, so the
		// hold guards nothing of theirs, and without it a new member could
		// not join for the whole window after every start with an empty
		// journal (a hosted home starts that way each time). The challenge
		// still keeps a request signed before this start out.
		allowed := false
		if r.Header.Get(api.HeaderAuthorization) != "" || tokenless(r.Method, r.URL.Path) {
			if challenge, err := d.s.recoveryChallenge(); err == nil {
				allowed = recoveryValue(r.Header) == challenge
				if !allowed {
					api.SetHeader(bw.h, HeaderRecovery, challenge)
					issuedRecovery = true
				}
			}
		}
		if !allowed {
			api.WriteError(bw, api.Errorf(api.CodeUnavailable, "the home is not taking writes over the tunnel for %s after its start without a fresh recovery challenge", time.Duration(keepFor())*time.Second).
				WithHint("reads work; a current "+config.Program+" retries safely, or try the write again later"))
			return
		}
	}
	// Recovery is transport metadata, not authority or route input.
	api.DelHeader(r.Header, HeaderRecovery)
	if api.GetHeader(r.Header, api.HeaderSealed) != "" {
		// A body sealed to this home's key for this one request: opened here,
		// so the route sees the request as the tailnet would carry it. Only a
		// member's (a token caller's, its nonce now recorded) is opened.
		if r.Header.Get(api.HeaderAuthorization) == "" || d.s.sealOff.Load() {
			api.WriteError(bw, sealbox.ErrOpen)
			return
		}
		uri := r.RequestURI
		if uri == "" {
			uri = r.URL.RequestURI()
		}
		purpose := requestPurpose(r.Method, uri, get(r.Header, HeaderDate), nonce)
		opened, err := sealbox.OpenWhole(body, func(sealed string) ([]byte, error) { return d.s.openFrom(caller, purpose, sealed) })
		if err != nil {
			api.WriteError(bw, sealbox.ErrOpen)
			return
		}
		body = opened
	}
	if api.GetHeader(r.Header, api.HeaderSealed) == "" && secretInClear(r.URL.Path, body) {
		// Secrets in the clear have crossed already (a broken or foreign
		// client); they are never used, so nothing comes to rely on it.
		api.WriteError(bw, errClear(r.URL.Path, body, "the calling machine"))
		return
	}
	dropSealHeaders(r.Header)
	switch p := r.URL.Path; {
	case p == "/v0/events" && r.URL.Query().Get("live") != "0":
		api.WriteError(bw, api.Errorf(api.CodeBadRequest, "live event streams are not served over the tunnel").WithHint("ask with live=0"))
		return
	}
	r.Body, r.ContentLength = io.NopCloser(bytes.NewReader(body)), int64(len(body))
	d.next.ServeHTTP(bw, r)
}

// tokenless says whether a signed request may pass with no token: the
// calls of joining, never anything of a member's. Redemption; and the
// GitHub join (SPEC 22 over https): discover, start, and collect (the poll
// route answers only the key that started that login, with its secret).
// Every one is still signed, fresh, and under the routes' own limits.
func tokenless(method, path string) bool {
	return method == http.MethodPost && path == "/v0/invites/redeem" || githubJoinCall(method, path)
}

// githubJoinCall is one of the GitHub join's three calls. The collect path
// takes only the shape of the id the authority mints (a ULID), so another
// route added under /v0/login/github/ is never tokenless by accident.
func githubJoinCall(method, path string) bool {
	switch {
	case method == http.MethodGet && path == "/v0/join/github",
		method == http.MethodPost && path == "/v0/join/github":
		return true
	}
	id, ok := strings.CutPrefix(path, "/v0/login/github/")
	return ok && method == http.MethodPost && ids.Valid(id)
}

// verifyRequest checks the caller's signature; it returns the caller's key id.
func verifyRequest(r *http.Request, homeFP string, body []byte) (string, bool) {
	pub, err := base64.RawURLEncoding.DecodeString(get(r.Header, HeaderKey))
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return "", false
	}
	sig, err := base64.RawURLEncoding.DecodeString(get(r.Header, HeaderSig))
	nonce, okN := one(r.Header, HeaderNonce)
	date, okD := one(r.Header, HeaderDate)
	_, okK := one(r.Header, HeaderKey)
	if n := len(nonce); err != nil || n < 16 || n > MaxNonceLen || !b64url(nonce) || date == "" || !okN || !okD || !okK {
		return "", false
	}
	uri := r.RequestURI
	if uri == "" {
		uri = r.URL.RequestURI()
	}
	if !ed25519.Verify(pub, requestString(homeFP, r.Method, uri, r.Header, body), sig) {
		return "", false
	}
	return KeyID(pub), true
}

// clientAddr is the caller's address for redemption's rate limits:
// CF-Connecting-IP only from a loopback peer (cloudflared on this machine)
// and, when tunnel.edge_secret is set, only with that secret in
// EdgeSecretHeader; else the peer's own address, so a caller that could
// name any address gets one bucket. A process on this machine is its
// owner anyway (SPEC 1).
func (d *door) clientAddr(r *http.Request) string {
	peer, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		peer = r.RemoteAddr
	}
	ip, err := netip.ParseAddr(peer)
	if err != nil || !ip.IsLoopback() {
		return peer
	}
	if d.s.cfg().EdgeSecret != "" && !d.fromEdge(r) {
		return peer
	}
	if cf, err := netip.ParseAddr(strings.TrimSpace(r.Header.Get("CF-Connecting-IP"))); err == nil {
		return cf.String()
	}
	return peer
}

// fromEdge reports whether r carries tunnel.edge_secret: in
// EdgeSecretHeader (a Transform Rule at the edge), or as the Host
// EdgeHost(secret), which cloudflared's ingress sets for its own requests
// (originRequest.httpHostHeader; zm tunnel setup writes it), so no zone
// rule is needed.
func (d *door) fromEdge(r *http.Request) bool {
	want := d.s.cfg().EdgeSecret
	if want == "" {
		return false
	}
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	return subtle.ConstantTimeCompare([]byte(r.Header.Get(EdgeSecretHeader)), []byte(want)) == 1 ||
		subtle.ConstantTimeCompare([]byte(r.Header.Get(EdgeSecretHeaderNew)), []byte(want)) == 1 ||
		subtle.ConstantTimeCompare([]byte(strings.ToLower(host)), []byte(EdgeHost(want))) == 1 ||
		subtle.ConstantTimeCompare([]byte(strings.ToLower(host)), []byte(EdgeHostNew(want))) == 1
}

// EdgeHost is the Host cloudflared sends when it carries the edge secret
// (what tunnel setup writes); EdgeHostNew is its new name, taken as well.
func EdgeHost(secret string) string { return "zm-edge-" + secret }

// EdgeHostNew is EdgeHost's new name: potluck-edge-<secret>.
func EdgeHostNew(secret string) string { return "potluck-edge-" + secret }

// bufWriter holds an answer until it is whole, so it can be signed.
type bufWriter struct {
	h      http.Header
	status int
	buf    bytes.Buffer
	over   bool
}

func (b *bufWriter) Header() http.Header { return b.h }
func (b *bufWriter) WriteHeader(code int) {
	if b.status == 0 {
		b.status = code
	}
}
func (b *bufWriter) Write(p []byte) (int, error) {
	if b.status == 0 {
		b.status = http.StatusOK
	}
	// Never cached at the edge, whatever the zone's cache rules (unsigned:
	// the member drops it).
	b.h.Set("Cache-Control", "no-store")
	if int64(b.buf.Len()+len(p)) > MaxAnswer {
		b.over = true
		return 0, io.ErrShortWrite
	}
	return b.buf.Write(p)
}

// Flush is a no-op: nothing leaves before the answer is whole.
func (b *bufWriter) Flush() {}

func (b *bufWriter) finish(w http.ResponseWriter, key ed25519.PrivateKey, nonce, sealCert string) {
	if b.over {
		b.h, b.status, b.buf = http.Header{}, 0, bytes.Buffer{}
		b.over = false
		api.WriteError(b, api.Errorf(api.CodeInternal, "answer too large for the tunnel"))
	}
	if b.status == 0 {
		b.status = http.StatusOK
	}
	body := b.buf.Bytes()
	if b.h.Get("Content-Type") == "" && len(body) > 0 {
		// net/http would sniff one after signing; name it now so it is signed.
		b.h.Set("Content-Type", http.DetectContentType(body))
	}
	// This home's certified seal key on every answer (signed with it): a
	// member seals secrets to it. Never a route's to set.
	api.DelHeader(b.h, api.HeaderSealKey)
	if sealCert != "" {
		api.SetHeader(b.h, api.HeaderSealKey, sealCert)
	}
	pub := key.Public().(ed25519.PublicKey)
	api.SetHeader(b.h, HeaderHomeKey, base64.RawURLEncoding.EncodeToString(pub))
	api.DelHeader(b.h, HeaderHomeSig)
	sig := ed25519.Sign(key, responseString(Fingerprint(pub), nonce, b.status, b.h, body))
	api.SetHeader(b.h, HeaderHomeSig, base64.RawURLEncoding.EncodeToString(sig))
	dst := w.Header()
	for k, v := range b.h {
		dst[k] = v
	}
	w.WriteHeader(b.status)
	_, _ = w.Write(body)
}

// b64url reports whether s uses only base64url's alphabet (a nonce goes
// into the journal as one word).
func b64url(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !('A' <= c && c <= 'Z' || 'a' <= c && c <= 'z' || '0' <= c && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}
