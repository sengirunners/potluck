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
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/ids"
	"github.com/sengirunners/potluck/kit/tunnel/tunneltest"
)

func newService(t *testing.T) *Service {
	t.Helper()
	home, err := os.MkdirTemp("", "zm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	cfg, err := config.LoadFrom(home)
	if err != nil {
		t.Fatal(err)
	}
	return New(&core.Deps{Config: cfg})
}

// A daemon restart rebuilds the dependency/service registry and reads only
// durable home state. Reusing a live Deps would incorrectly register two
// producers under one service name and retain other process-local modules.
func restartService(t *testing.T, old *Service) *Service {
	t.Helper()
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadFrom(old.d.Config.Home)
	if err != nil {
		t.Fatal(err)
	}
	next := New(&core.Deps{Config: cfg})
	t.Cleanup(func() { next.Close() })
	return next
}

// echo answers what the door attached: the caller's key id and path.
func echo(seen *atomic.Pointer[core.Remote]) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rm := core.RemoteOf(r.Context())
		seen.Store(rm)
		b, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Zm-Team", "header-value")
		w.Header().Set("X-Unsigned", "dropped")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"path":"` + r.URL.Path + `","body":"` + string(b) + `"}`))
	})
}

// home is a door in front of echo on a loopback HTTP server.
func home(t *testing.T) (*Service, *httptest.Server, *atomic.Pointer[core.Remote]) {
	t.Helper()
	s := newService(t)
	var seen atomic.Pointer[core.Remote]
	srv := httptest.NewServer(s.Handler(echo(&seen)))
	t.Cleanup(srv.Close)
	return s, srv, &seen
}

func member(t *testing.T, homeFP string) (*Service, *http.Client) {
	t.Helper()
	m := newService(t)
	return m, &http.Client{Transport: m.Transport(homeFP, time.Second, nil)}
}

func do(t *testing.T, c *http.Client, method, u, token, body string) (*http.Response, string, error) {
	t.Helper()
	req, _ := http.NewRequest(method, u, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b), nil
}

func TestSignedRoundTrip(t *testing.T) {
	h, srv, seen := home(t)
	fp, _ := h.Fingerprint()
	m, c := member(t, fp)
	resp, body, err := do(t, c, "POST", srv.URL+"/v0/work/a%3Ab?x=%7e1", "zm_tok", "hello")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 201 || !strings.Contains(body, `"body":"hello"`) {
		t.Fatalf("answer %d %s", resp.StatusCode, body)
	}
	if resp.Header.Get("X-Unsigned") != "" || resp.Header.Get("Zm-Team") != "header-value" {
		t.Fatalf("headers: unsigned kept or signed dropped: %v", resp.Header)
	}
	kid, _ := m.KeyID()
	rm := seen.Load()
	if rm == nil || rm.Via != core.ViaTunnel || rm.NodeID != kid || rm.Self || rm.Login != "" || rm.Addr != "127.0.0.1" {
		t.Fatalf("remote: %+v (want key %s)", rm, kid)
	}
	// The home's own key is Self (redemption refuses it).
	own := &http.Client{Transport: h.Transport(fp, time.Second, nil)}
	if _, _, err := do(t, own, "GET", srv.URL+"/v0/x", "zm_tok", ""); err != nil || !seen.Load().Self {
		t.Fatalf("own key: %v %+v", err, seen.Load())
	}
}

var testLoginID = ids.New()

func TestDoorRefusals(t *testing.T) {
	h, srv, seen := home(t)
	fp, _ := h.Fingerprint()
	_, c := member(t, fp)
	cases := []struct {
		method, path, token string
		want                int
	}{
		{"GET", "/v0/projects", "", 401},                     // signed, no token
		{"POST", "/v0/invites/redeem", "", 201},              // the one route without a token
		{"GET", "/v0/invites/redeem", "", 401},               // only its POST
		{"GET", "/v0/join/github", "", 201},                  // the GitHub join's calls (SPEC 22): signed, no token
		{"POST", "/v0/join/github", "", 201},                 //
		{"POST", "/v0/login/github/" + testLoginID, "", 201}, // its collect: a ULID only
		{"POST", "/v0/login/github/start", "", 401},          // a link or bind login is a member's
		{"GET", "/v0/login/github/" + testLoginID, "", 401},  // only the POST
		{"POST", "/v0/login/github/bind", "", 401},           // no other word is tokenless
		{"POST", "/v0/login/github/a/b", "", 401},            // exactly one id
		{"POST", "/v0/login/github/", "", 401},               //
		{"DELETE", "/v0/join/github", "", 401},               //
		{"GET", "/v0/grants/github?project=x", "", 401},      // nothing else of GitHub's
		{"GET", "/v0/authority/term", "", 201},               // the home's term: signed, no token
		{"POST", "/v0/authority/term", "", 401},              // only its GET
		{"GET", "/v0/authority/standby", "", 401},            // nothing else of the authority's
		{"GET", "/v0/authority/term/x", "", 401},             // exactly the term
		{"GET", "/v0/events", "zm_tok", 400},                 // no live stream
		{"GET", "/v0/events?live=0", "zm_tok", 201},
	}
	for _, tc := range cases {
		seen.Store(nil)
		resp, body, err := do(t, c, tc.method, srv.URL+tc.path, tc.token, "")
		if err != nil {
			t.Fatalf("%s %s: %v", tc.method, tc.path, err)
		}
		if resp.StatusCode != tc.want {
			t.Errorf("%s %s: %d %s, want %d", tc.method, tc.path, resp.StatusCode, body, tc.want)
		}
		if tc.want != 201 && seen.Load() != nil {
			t.Errorf("%s %s reached the handler", tc.method, tc.path)
		}
	}
	// Unsigned: 401 before anything (plain HTTP client), the GitHub join's
	// calls included.
	for _, c := range [][2]string{{"GET", "/v0/health"}, {"GET", "/v0/authority/term"}, {"GET", "/v0/join/github"}, {"POST", "/v0/join/github"}, {"POST", "/v0/login/github/" + testLoginID}} {
		seen.Store(nil)
		req, _ := http.NewRequest(c[0], srv.URL+c[1], strings.NewReader("{}"))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 401 || seen.Load() != nil {
			t.Fatalf("unsigned %s %s: %d (reached the handler: %v)", c[0], c[1], resp.StatusCode, seen.Load() != nil)
		}
	}
}

// capture is a proxy in front of srv that keeps the last request it
// forwarded (headers and body) and can rewrite answers.
type capture struct {
	last    atomic.Pointer[http.Request]
	body    atomic.Pointer[[]byte]
	rewrite func(*http.Response) error
}

func (c *capture) server(t *testing.T, target string) *httptest.Server {
	u, _ := url.Parse(target)
	rp := httputil.NewSingleHostReverseProxy(u)
	rp.ModifyResponse = func(r *http.Response) error {
		if c.rewrite != nil {
			return c.rewrite(r)
		}
		return nil
	}
	p := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(b))
		c.last.Store(r.Clone(context.Background()))
		c.body.Store(&b)
		rp.ServeHTTP(w, r)
	}))
	t.Cleanup(p.Close)
	return p
}

func TestReplayAndSkew(t *testing.T) {
	h, srv, _ := home(t)
	fp, _ := h.Fingerprint()
	_, c := member(t, fp)
	var cp capture
	p := cp.server(t, srv.URL)
	if resp, _, err := do(t, c, "POST", p.URL+"/v0/work", "zm_tok", "w1"); err != nil || resp.StatusCode != 201 {
		t.Fatalf("first: %v", err)
	}
	// Whoever saw the request sends it again: refused, and never 401.
	old := cp.last.Load()
	again, _ := http.NewRequest(old.Method, srv.URL+old.URL.RequestURI(), bytes.NewReader(*cp.body.Load()))
	again.Header = old.Header.Clone()
	resp, err := http.DefaultClient.Do(again)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 400 || !strings.Contains(string(b), "replay") {
		t.Fatalf("replay: %d %s", resp.StatusCode, b)
	}
	// The same request with another body (or path) does not verify.
	again, _ = http.NewRequest(old.Method, srv.URL+old.URL.RequestURI(), strings.NewReader("w2"))
	again.Header = old.Header.Clone()
	again.Header.Set("Potluck-Nonce", "another")
	if resp, _ := http.DefaultClient.Do(again); resp.StatusCode != 401 {
		t.Fatalf("altered body: %d", resp.StatusCode)
	}
	// A clock 10 minutes off: 400 with a hint, not 401.
	k := ed25519.NewKeyFromSeed(make([]byte, 32))
	tr := newTransport(k, fp, time.Second, nil)
	req, _ := http.NewRequest("GET", srv.URL+"/v0/x", nil)
	req.Header.Set("Authorization", "Bearer zm_tok")
	stale := time.Now().Add(-10 * time.Minute).Unix()
	req.Header.Set(HeaderDate, strconv.FormatInt(stale, 10))
	req.Header.Set(HeaderKey, tr.keyB)
	req.Header.Set(HeaderNonce, "nonce-of-22-character")
	sig := ed25519.Sign(k, requestString(fp, "GET", "/v0/x", req.Header, nil))
	req.Header.Set(HeaderSig, b64(sig))
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 400 || !strings.Contains(string(b), "clocks") {
		t.Fatalf("skew: %d %s", resp.StatusCode, b)
	}
}

func TestPinnedHome(t *testing.T) {
	h, srv, _ := home(t)
	fp, _ := h.Fingerprint()
	// Another home answers at the hostname (hijacked): its key is not the
	// pinned one.
	other := newService(t)
	ofp, _ := other.Fingerprint()
	_, c := member(t, ofp)
	if _, _, err := do(t, c, "GET", srv.URL+"/v0/x", "zm_tok", ""); !errors.Is(err, ErrHomeKey) {
		t.Fatalf("other home: %v", err)
	}
	// Something in the middle changes an answer: refused.
	_, c = member(t, fp)
	var cp capture
	cp.rewrite = func(r *http.Response) error {
		b, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(bytes.Replace(b, []byte("x"), []byte("y"), 1)))
		return nil
	}
	p := cp.server(t, srv.URL)
	if _, _, err := do(t, c, "GET", p.URL+"/v0/x", "zm_tok", ""); !errors.Is(err, ErrHomeKey) {
		t.Fatalf("altered answer: %v", err)
	}
	// ... or a signed header.
	cp.rewrite = func(r *http.Response) error { r.Header.Set("Zm-Team", "forged"); return nil }
	if _, _, err := do(t, c, "GET", p.URL+"/v0/x", "zm_tok", ""); !errors.Is(err, ErrHomeKey) {
		t.Fatalf("altered header: %v", err)
	}
	// An answer from another request (its nonce) does not verify either:
	// the signature covers the nonce, which the proxy cannot change.
	// An unsigned answer (the tunnel's own error page): unreachable.
	cp.rewrite = func(r *http.Response) error {
		r.Header.Del(HeaderHomeSig)
		r.Header.Del("Potluck-Home-Sig")
		r.StatusCode = 502
		return nil
	}
	if _, _, err := do(t, c, "GET", p.URL+"/v0/x", "zm_tok", ""); !errors.Is(err, ErrUnsigned) {
		t.Fatalf("unsigned answer: %v", err)
	}
}

// The client reports progress while it reads an answer whole.
func TestReadProgress(t *testing.T) {
	s := newService(t)
	big := bytes.Repeat([]byte("a"), 1<<20)
	srv := httptest.NewServer(s.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(big) })))
	t.Cleanup(srv.Close)
	fp, _ := s.Fingerprint()
	_, c := member(t, fp)
	var n atomic.Int32
	req, _ := http.NewRequestWithContext(core.WithReadProgress(context.Background(), func() { n.Add(1) }), "GET", srv.URL+"/v0/x", nil)
	req.Header.Set("Authorization", "Bearer zm_tok")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if len(b) != len(big) || n.Load() < 2 {
		t.Fatalf("read %d bytes, %d progress calls", len(b), n.Load())
	}
}

func TestNormURI(t *testing.T) {
	for in, want := range map[string]string{
		"/v0/work/a%3ab":       "/v0/work/a%3Ab",
		"/v0/x%7e%2f%41?q=%2D": "/v0/x~%2FA?q=-",
		"/plain?x=1":           "/plain?x=1",
	} {
		if got := normURI(in); got != want {
			t.Errorf("normURI(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestKeyFile(t *testing.T) {
	s := newService(t)
	a, _ := s.Fingerprint()
	fi, err := os.Stat(s.d.Config.Home + "/" + KeyFile)
	if err != nil || fi.Mode().Perm() != 0o600 || len(a) != 26 {
		t.Fatalf("key file: %v %v %q", fi, err, a)
	}
	again := restartService(t, s)
	if b, _ := again.Fingerprint(); a != b {
		t.Fatalf("key not kept: %s %s", a, b)
	}
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	if id := KeyID(pub); !strings.HasPrefix(id, "zmkey:") {
		t.Fatal(id)
	}
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// A request captured before a restart is refused after it: the replay
// journal outlives the daemon.
func TestReplayAfterRestart(t *testing.T) {
	h := newService(t)
	var seen atomic.Pointer[core.Remote]
	srv := httptest.NewServer(h.Handler(echo(&seen)))
	t.Cleanup(srv.Close)
	fp, _ := h.Fingerprint()
	_, c := member(t, fp)
	var cp capture
	p := cp.server(t, srv.URL)
	if resp, _, err := do(t, c, "POST", p.URL+"/v0/work/x/comment", "zm_tok", "once"); err != nil || resp.StatusCode != 201 {
		t.Fatalf("first: %v", err)
	}
	// The home restarts: same home and key, a new process.
	h2 := restartService(t, h)
	srv2 := httptest.NewServer(h2.Handler(echo(&seen)))
	t.Cleanup(srv2.Close)
	old := cp.last.Load()
	again, _ := http.NewRequest(old.Method, srv2.URL+old.URL.RequestURI(), bytes.NewReader(*cp.body.Load()))
	again.Header = old.Header.Clone()
	seen.Store(nil)
	resp, err := http.DefaultClient.Do(again)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 400 || !strings.Contains(string(b), "already received") || seen.Load() != nil {
		t.Fatalf("replay after a restart: %d %s", resp.StatusCode, b)
	}
}

// Neither the home's clock stepping back nor a slow member clock locks a
// member out after a restart, within the skew allowed.
func TestClockCorrectionsAfterRestart(t *testing.T) {
	h := newService(t)
	var seen atomic.Pointer[core.Remote]
	fp, _ := h.Fingerprint()
	_, c := member(t, fp)
	srv := httptest.NewServer(h.Handler(echo(&seen)))
	t.Cleanup(srv.Close)
	if resp, _, err := do(t, c, "GET", srv.URL+"/v0/x", "zm_tok", ""); err != nil || resp.StatusCode != 201 {
		t.Fatalf("before: %v", err)
	}
	// Restart, then the home's clock steps back two minutes.
	h2 := restartService(t, h)
	srv2 := httptest.NewServer(h2.Handler(echo(&seen)))
	t.Cleanup(srv2.Close)
	oldNow := nowFunc
	nowFunc = func() time.Time { return time.Now().Add(-2 * time.Minute) }
	t.Cleanup(func() { nowFunc = oldNow })
	if resp, body, err := do(t, c, "GET", srv2.URL+"/v0/x", "zm_tok", ""); err != nil || resp.StatusCode != 201 {
		t.Fatalf("home clock stepped back: %v %d %s", err, resp.StatusCode, body)
	}
	nowFunc = oldNow
	// A member whose clock is three minutes slow, right after the restart.
	slow, _ := member(t, fp)
	tr := slow.Transport(fp, time.Second, nil).(*transport)
	tr.now = func() time.Time { return time.Now().Add(-3 * time.Minute) }
	sc := &http.Client{Transport: tr}
	if resp, body, err := do(t, sc, "GET", srv2.URL+"/v0/x", "zm_tok", ""); err != nil || resp.StatusCode != 201 {
		t.Fatalf("slow member: %v %d %s", err, resp.StatusCode, body)
	}
	// Past the skew allowed, a clear 400 that does not blame one side.
	tr.now = func() time.Time { return time.Now().Add(-10 * time.Minute) }
	resp, body, err := do(t, sc, "GET", srv2.URL+"/v0/x", "zm_tok", "")
	if err != nil || resp.StatusCode != 400 || !strings.Contains(body, "one of the two clocks") {
		t.Fatalf("10 minutes slow: %v %d %s", err, resp.StatusCode, body)
	}
}

func TestNonceLimits(t *testing.T) {
	h, srv, _ := home(t)
	fp, _ := h.Fingerprint()
	old := MaxNoncesPerKey
	MaxNoncesPerKey = 3
	t.Cleanup(func() { MaxNoncesPerKey = old })
	_, a := member(t, fp)
	_, b := member(t, fp)
	for i := 0; i < 3; i++ {
		if resp, _, err := do(t, a, "GET", srv.URL+"/v0/x", "zm_tok", ""); err != nil || resp.StatusCode != 201 {
			t.Fatalf("a %d: %v", i, err)
		}
	}
	if resp, _, _ := do(t, a, "GET", srv.URL+"/v0/x", "zm_tok", ""); resp.StatusCode != 503 {
		t.Fatalf("a over its budget: %d", resp.StatusCode)
	}
	// Another member's budget is its own.
	if resp, _, _ := do(t, b, "GET", srv.URL+"/v0/x", "zm_tok", ""); resp.StatusCode != 201 {
		t.Fatalf("b: %d", resp.StatusCode)
	}
	// A nonce over 64 characters does not verify.
	k := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{7}, 32))
	tr := newTransport(k, fp, time.Second, nil)
	req, _ := http.NewRequest("GET", srv.URL+"/v0/x", nil)
	req.Header.Set("Authorization", "Bearer zm_tok")
	req.Header.Set(HeaderDate, strconv.FormatInt(time.Now().Unix(), 10))
	req.Header.Set(HeaderKey, tr.keyB)
	req.Header.Set(HeaderNonce, strings.Repeat("n", 65))
	req.Header.Set(HeaderSig, b64(ed25519.Sign(k, requestString(fp, "GET", "/v0/x", req.Header, nil))))
	if resp, _ := http.DefaultClient.Do(req); resp.StatusCode != 401 {
		t.Fatalf("long nonce: %d", resp.StatusCode)
	}
}

// Repeated headers canonicalise without ambiguity: two values are not the
// one value "a,b".
func TestCanonHeadersUnambiguous(t *testing.T) {
	two := http.Header{"Zm-As": {"a", "b"}}
	one := http.Header{"Zm-As": {"a,b"}}
	shifted := http.Header{"Zm-A": {"sa"}}
	if canonHeaders(two, "", false) == canonHeaders(one, "", false) || canonHeaders(http.Header{"Zm-As": {"a"}}, "", false) == canonHeaders(shifted, "", false) {
		t.Fatal("ambiguous canonical headers")
	}
}

func TestClientAddrTrust(t *testing.T) {
	s := newService(t)
	d := &door{s: s}
	req := httptest.NewRequest("POST", "/v0/invites/redeem", nil)
	req.Header.Set("CF-Connecting-IP", "198.51.100.7")
	req.RemoteAddr = "127.0.0.1:5555"
	if got := d.clientAddr(req); got != "198.51.100.7" {
		t.Fatalf("loopback peer, no secret configured: %s", got)
	}
	req.RemoteAddr = "192.0.2.1:5555" // not cloudflared on this machine
	if got := d.clientAddr(req); got != "192.0.2.1" {
		t.Fatalf("non-loopback peer: %s", got)
	}
	secret := "0123456789abcdef0123"
	if err := s.save(config.Tunnel{EdgeSecret: secret}); err != nil {
		t.Fatal(err)
	}
	req.RemoteAddr = "127.0.0.1:5555"
	if got := d.clientAddr(req); got != "127.0.0.1" {
		t.Fatalf("secret configured, not sent: %s", got)
	}
	req.Header.Set(EdgeSecretHeader, secret)
	if got := d.clientAddr(req); got != "198.51.100.7" {
		t.Fatalf("secret sent: %s", got)
	}
	// cloudflared's ingress carries it as the Host (zm tunnel setup).
	req.Header.Del(EdgeSecretHeader)
	req.Host = EdgeHost(secret)
	if got := d.clientAddr(req); got != "198.51.100.7" || !d.fromEdge(req) {
		t.Fatalf("secret as the Host: %s", got)
	}
	req.Host = EdgeHost("ffffffffffffffffffff")
	if got := d.clientAddr(req); got != "127.0.0.1" {
		t.Fatalf("another secret as the Host: %s", got)
	}
}

// The home's clock jumps ahead past an entry's old expiry (it is pruned
// there under a 6-minute keep), then steps back inside the skew: the
// captured request must still be refused.
func TestClockStepBackNeverResurrectsANonce(t *testing.T) {
	h, srv, _ := home(t)
	fp, _ := h.Fingerprint()
	m, c := member(t, fp)
	var cp capture
	p := cp.server(t, srv.URL)
	if resp, _, err := do(t, c, "POST", p.URL+"/v0/work/x/comment", "zm_tok", "once"); err != nil || resp.StatusCode != 201 {
		t.Fatalf("first: %v", err)
	}
	old := cp.last.Load()
	body := *cp.body.Load()
	t0 := time.Now()
	oldNow := nowFunc
	t.Cleanup(func() { nowFunc = oldNow })
	// Seven minutes on: another member call (its clock agrees) prunes.
	nowFunc = func() time.Time { return time.Now().Add(7 * time.Minute) }
	tr := m.Transport(fp, time.Second, nil).(*transport)
	tr.now = nowFunc
	if resp, b, err := do(t, &http.Client{Transport: tr}, "GET", srv.URL+"/v0/x", "zm_tok", ""); err != nil || resp.StatusCode != 201 {
		t.Fatalf("after the jump: %v %s", err, b)
	}
	// The clock steps back to two minutes after the capture.
	nowFunc = func() time.Time { return time.Now().Add(t0.Add(2 * time.Minute).Sub(time.Now())) }
	again, _ := http.NewRequest(old.Method, srv.URL+old.URL.RequestURI(), bytes.NewReader(body))
	again.Header = old.Header.Clone()
	resp, err := http.DefaultClient.Do(again)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 400 || !strings.Contains(string(b), "already received") {
		t.Fatalf("replay after a clock step back: %d %s", resp.StatusCode, b)
	}
}

// A journal that cannot be read whole at start (the key is not new):
// writes are refused for a few minutes, reads go on (the journal is
// rewritten clean, so they can be recorded).
func TestUnreadableJournalRefusesWrites(t *testing.T) {
	h := newService(t)
	fp, _ := h.Fingerprint() // the key exists before the "restart"
	path := h.d.Config.Home + "/" + NonceFile
	// A line longer than the reader takes: the read stops with an error.
	if err := os.WriteFile(path, bytes.Repeat([]byte("x"), 2*maxLine), 0o600); err != nil {
		t.Fatal(err)
	}
	h2 := restartService(t, h)
	var seen atomic.Pointer[core.Remote]
	srv := httptest.NewServer(h2.Handler(echo(&seen)))
	t.Cleanup(srv.Close)
	_, c := member(t, fp)
	if resp, b, err := do(t, legacyClient(c), "POST", srv.URL+"/v0/work/x/comment", "zm_tok", "w"); err != nil || resp.StatusCode != 503 {
		t.Fatalf("write with an unreadable journal: %v %d %s", err, resp.StatusCode, b)
	}
	if resp, b, err := do(t, c, "GET", srv.URL+"/v0/x", "zm_tok", ""); err != nil || resp.StatusCode == 503 {
		t.Fatalf("read with an unreadable journal: %v %d %s", err, resp.StatusCode, b)
	}
}

// A journal that cannot be written: the request is refused (503), never run
// memory-only, and the next one opens it again.
func TestUnwritableJournalRefusesThenRecovers(t *testing.T) {
	h, srv, seen := home(t)
	fp, _ := h.Fingerprint()
	_, c := member(t, fp)
	if resp, _, err := do(t, c, "GET", srv.URL+"/v0/x", "zm_tok", ""); err != nil || resp.StatusCode != 201 {
		t.Fatalf("first: %v", err)
	}
	path := h.d.Config.Home + "/" + NonceFile
	h.journal.mu.Lock()
	h.journal.jf.Close()
	h.journal.jf = nil
	h.journal.mu.Unlock()
	if err := os.Chmod(path, 0o400); err != nil {
		t.Fatal(err)
	}
	seen.Store(nil)
	if resp, _, err := do(t, c, "GET", srv.URL+"/v0/x", "zm_tok", ""); err != nil || resp.StatusCode != 503 || seen.Load() != nil {
		t.Fatalf("unwritable journal: %v %d", err, resp.StatusCode)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if resp, _, err := do(t, c, "GET", srv.URL+"/v0/x", "zm_tok", ""); err != nil || resp.StatusCode != 201 {
		t.Fatalf("after the journal came back: %v %d", err, resp.StatusCode)
	}
}

// Compaction really runs while appends go on: expired entries are
// dropped, every append made during it survives in the file, and a restart
// reloads them all.
func TestCompactionKeepsEveryAppend(t *testing.T) {
	oldSlack := compactSlack
	compactSlack = 16
	t.Cleanup(func() { compactSlack = oldSlack })
	h := newService(t)
	if _, err := h.Fingerprint(); err != nil { // a fresh key: no earlier run
		t.Fatal(err)
	}
	j := h.journal
	t0 := time.Now()
	// 300 entries that will have expired by t1.
	for i := 0; i < 300; i++ {
		if r := j.add("zmkey:old", fmt.Sprintf("old-nonce-%06d", i), t0.Unix(), t0); r != nonceNew {
			t.Fatalf("old %d: %d", i, r)
		}
	}
	t1 := t0.Add(time.Duration(keepFor()+60) * time.Second)
	// While the first compaction is between its write and its swap, 50 more
	// appends land: they must be carried into the new file.
	var once sync.Once
	beforeSwap = func() {
		once.Do(func() {
			for i := 0; i < 50; i++ {
				if r := j.add("zmkey:mid", fmt.Sprintf("mid-nonce-%06d", i), t1.Unix(), t1); r != nonceNew {
					t.Errorf("mid append %d: %d", i, r)
				}
			}
		})
	}
	t.Cleanup(func() { beforeSwap = nil })
	// Eight writers append at t1 at once: the first prunes the old ones,
	// which makes the journal mostly dead, so compactions start while the
	// others keep appending.
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				if r := j.add("zmkey:new", fmt.Sprintf("w%d-nonce-%06d", w, i), t1.Unix(), t1); r != nonceNew {
					t.Errorf("writer %d append %d: %d", w, i, r)
				}
			}
		}()
	}
	wg.Wait()
	j.wg.Wait()
	j.mu.Lock()
	compactions := j.compactions
	j.mu.Unlock()
	if compactions < 1 {
		t.Fatal("no compaction ran")
	}
	// Every append is in the file: a restart reloads them all, and none of
	// the expired ones.
	h2 := restartService(t, h)
	h2.journal.load(t1)
	got := h2.journal.keys["zmkey:new"]
	if len(h2.journal.keys["zmkey:mid"]) != 50 {
		t.Fatalf("appends made during a compaction: %d of 50 survived", len(h2.journal.keys["zmkey:mid"]))
	}
	if len(got) != 8*200 || len(h2.journal.keys["zmkey:old"]) != 0 {
		t.Fatalf("after %d compactions and a restart: %d new, %d old", compactions, len(got), len(h2.journal.keys["zmkey:old"]))
	}
	for w := 0; w < 8; w++ {
		for i := 0; i < 200; i++ {
			if _, ok := got[fmt.Sprintf("w%d-nonce-%06d", w, i)]; !ok {
				t.Fatalf("append w%d/%d lost", w, i)
			}
		}
	}
	if h2.journal.degraded() {
		t.Fatal("a clean journal degraded the restart")
	}
}

// A failed compaction backs off, and the journal it left keeps working.
func TestCompactionBacksOff(t *testing.T) {
	oldSlack := compactSlack
	compactSlack = 4
	t.Cleanup(func() { compactSlack = oldSlack })
	h := newService(t)
	h.Fingerprint()
	j := h.journal
	now := time.Now()
	for i := 0; i < 20; i++ {
		j.add("zmkey:a", fmt.Sprintf("a-nonce-%010d", i), now.Unix()-int64(keepFor())-120, now) // already expired
	}
	// The temp file's place is a directory: the rewrite fails.
	if err := os.MkdirAll(h.d.Config.Home+"/"+NonceFile+".tmp", 0o700); err != nil {
		t.Fatal(err)
	}
	later := now.Add(2 * time.Minute)
	for i := 0; i < 20; i++ {
		if r := j.add("zmkey:b", fmt.Sprintf("b-nonce-%010d", i), later.Unix(), later); r != nonceNew {
			t.Fatalf("append %d after a failed compaction: %d", i, r)
		}
		j.wg.Wait()
	}
	j.mu.Lock()
	retry, retryLines, done := j.retryAt, j.retryLines, j.compactions
	j.mu.Unlock()
	_ = retryLines
	if retry.IsZero() || done != 0 {
		t.Fatalf("no back-off after a failed compaction: retry %v, compactions %d", retry, done)
	}
	// The obstacle goes and the back-off passes: the next compaction runs
	// and clears the raised threshold.
	if err := os.RemoveAll(h.d.Config.Home + "/" + NonceFile + ".tmp"); err != nil {
		t.Fatal(err)
	}
	oldBackoff := compactBackoff
	compactBackoff = 0
	t.Cleanup(func() { compactBackoff = oldBackoff })
	j.mu.Lock()
	j.retryAt = time.Now().Add(-time.Second)
	j.mu.Unlock()
	// Twelve minutes on, the first twenty have expired: the journal is
	// mostly dead lines again.
	later2 := later.Add(12 * time.Minute)
	for i := 20; i < 40; i++ {
		if r := j.add("zmkey:b", fmt.Sprintf("b-nonce-%010d", i), later2.Unix(), later2); r != nonceNew {
			t.Fatalf("append %d: %d", i, r)
		}
		j.wg.Wait()
	}
	j.mu.Lock()
	retry, retryLines, done = j.retryAt, j.retryLines, j.compactions
	j.mu.Unlock()
	if done == 0 || !retry.IsZero() || retryLines != 0 {
		t.Fatalf("after a successful compaction: compactions %d, retry %v, retry lines %d", done, retry, retryLines)
	}
	h2 := restartService(t, h)
	h2.journal.load(later2)
	if len(h2.journal.keys["zmkey:b"]) != 20 {
		t.Fatalf("live appends after the compactions: %d", len(h2.journal.keys["zmkey:b"]))
	}
}

// The edge is "seen" only by a request after the latest up: a re-run's
// check cannot pass on an old sighting.
func TestEdgeSeenResetsOnUpAndDown(t *testing.T) {
	s := newService(t)
	t.Cleanup(func() { s.Close() })
	secret := "00112233445566778899aabbccddeeff"
	up := func() {
		t.Helper()
		body := `{"listen":"127.0.0.1:0","url":"https://zm.example.com","edge_secret":"` + secret + `"}`
		rec := httptest.NewRecorder()
		s.handleUp(rec, httptest.NewRequest("POST", "/v0/tunnel/up", strings.NewReader(body)))
		if rec.Code != 200 {
			t.Fatalf("up: %d %s", rec.Code, rec.Body)
		}
	}
	up()
	if s.Status().EdgeSeen != nil {
		t.Fatal("seen before any request")
	}
	srv := httptest.NewServer(s.Handler(http.NotFoundHandler()))
	t.Cleanup(srv.Close)
	req, _ := http.NewRequest("GET", srv.URL+"/v0/health", nil)
	req.Host = EdgeHost(secret)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.Header.Get("Cache-Control") != "no-store" || s.Status().EdgeSeen == nil {
		t.Fatalf("after an edge request: %v %+v", resp.Header, s.Status())
	}
	up()
	if s.Status().EdgeSeen != nil {
		t.Fatal("an old sighting survived up")
	}
	rec := httptest.NewRecorder()
	s.handleDown(rec, httptest.NewRequest("POST", "/v0/tunnel/down", nil))
	if st := s.Status(); st.EdgeSeen != nil || st.Running || st.URL != "" {
		t.Fatalf("after down: %+v", st)
	}
}

// A signed tokenless GitHub join call, sent again by whoever saw it, is
// refused as a replay; a new one passes; the cache is bounded.
func TestTokenlessReplayRefused(t *testing.T) {
	h, srv, seen := home(t)
	fp, _ := h.Fingerprint()
	_, c := member(t, fp)
	var cp capture
	p := cp.server(t, srv.URL)
	path := "/v0/login/github/" + testLoginID
	if resp, _, err := do(t, c, "POST", p.URL+path, "", `{"secret":"s"}`); err != nil || resp.StatusCode != 201 {
		t.Fatalf("first collect: %v", err)
	}
	seen.Store(nil)
	old := cp.last.Load()
	again, _ := http.NewRequest(old.Method, srv.URL+old.URL.RequestURI(), bytes.NewReader(*cp.body.Load()))
	again.Header = old.Header.Clone()
	resp, err := http.DefaultClient.Do(again)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 400 || !strings.Contains(string(b), "replay") || seen.Load() != nil {
		t.Fatalf("replayed collect: %d %s (reached the handler: %v)", resp.StatusCode, b, seen.Load() != nil)
	}
	// A fresh signed one passes.
	if resp, _, err := do(t, c, "POST", p.URL+path, "", `{"secret":"s"}`); err != nil || resp.StatusCode != 201 {
		t.Fatalf("second collect: %v", err)
	}
}

func TestTokenlessNonceCaps(t *testing.T) {
	var c tokenlessNonces
	now := time.Now()
	date := now.Unix()
	// One address fills its own cap and nothing more.
	for i := 0; i < MaxTokenlessNoncesPerAddr; i++ {
		if got := c.add("198.51.100.1", fmt.Sprint("k", i), "n", date, now); got != nonceNew {
			t.Fatalf("nonce %d: %d", i, got)
		}
	}
	if got := c.add("198.51.100.1", "k0", "n", date, now); got != nonceSeen {
		t.Fatalf("replay: %d", got)
	}
	if got := c.add("198.51.100.1", "over", "n", date, now); got != nonceFull {
		t.Fatalf("per-address cap: %d", got)
	}
	// ... and another address is not blocked by it.
	if got := c.add("198.51.100.2", "legit", "n", date, now); got != nonceNew {
		t.Fatalf("another address while one is full: %d", got)
	}
	// The global cap is the last resort.
	for a := 0; len(c.seen) < MaxTokenlessNonces; a++ {
		c.add(fmt.Sprint("addr", a), fmt.Sprint("key", a), "n", date, now)
	}
	if got := c.add("another", "k", "n", date, now); got != nonceFull {
		t.Fatalf("global cap: %d", got)
	}
	// A date far ahead is clamped: past now+2*MaxSkew everything is
	// forgotten, empty addresses included.
	c2 := tokenlessNonces{}
	c2.add("a", "k", "n", now.Add(24*time.Hour).Unix(), now)
	later := now.Add(2*MaxSkew + time.Second)
	if got := c2.add("b", "k", "n", later.Unix(), later); got != nonceNew {
		t.Fatalf("a far-future date outlived the window: %d", got)
	}
	c2.pruneLocked(later)
	if len(c2.seen) != 1 || len(c2.perAdr) != 1 {
		t.Fatalf("after the window: %d entries in %d addresses", len(c2.seen), len(c2.perAdr))
	}
	// A replay from another address is a replay.
	var c3 tokenlessNonces
	if got := c3.add("198.51.100.1", "k", "n", now.Unix(), now); got != nonceNew {
		t.Fatalf("first: %d", got)
	}
	if got := c3.add("198.51.100.2", "k", "n", now.Unix(), now); got != nonceSeen {
		t.Fatalf("the same nonce from a second address: %d, want a replay", got)
	}
}

func TestAddrBucket(t *testing.T) {
	for in, want := range map[string]string{
		"203.0.113.9":          "203.0.113.9",
		"::ffff:203.0.113.9":   "203.0.113.9",
		"2001:db8:1:2:3:4:5:6": "2001:db8:1:2::/64",
		"2001:db8:1:2:ffff::1": "2001:db8:1:2::/64",
		"fe80::1%eth0":         "fe80::/64",
		"not an address":       "not an address",
	} {
		if got := core.AddrBucket(in); got != want {
			t.Errorf("AddrBucket(%q) = %q, want %q", in, got, want)
		}
	}
}

// A replayed tokenless GET is harmless and takes no cache entry.
func TestTokenlessGETsAreNotRecorded(t *testing.T) {
	h, srv, _ := home(t)
	fp, _ := h.Fingerprint()
	_, c := member(t, fp)
	for i := 0; i < MaxTokenlessNoncesPerAddr+5; i++ {
		if resp, _, err := do(t, c, "GET", srv.URL+"/v0/join/github", "", ""); err != nil || resp.StatusCode != 201 {
			t.Fatalf("discover %d: %v", i, err)
		}
	}
	if n := len(h.tokenless.seen); n != 0 {
		t.Fatalf("%d nonces recorded for GETs", n)
	}
}

// A captured signed POST replayed from another client address (another
// CF-Connecting-IP) is refused as a replay.
func TestTokenlessReplayFromAnotherAddress(t *testing.T) {
	h, srv, seen := home(t)
	fp, _ := h.Fingerprint()
	_, c := member(t, fp)
	var cp capture
	p := cp.server(t, srv.URL)
	if resp, _, err := do(t, c, "POST", p.URL+"/v0/join/github", "", `{"project":"x"}`); err != nil || resp.StatusCode != 201 {
		t.Fatalf("first: %v", err)
	}
	seen.Store(nil)
	old := cp.last.Load()
	again, _ := http.NewRequest(old.Method, srv.URL+old.URL.RequestURI(), bytes.NewReader(*cp.body.Load()))
	again.Header = old.Header.Clone()
	again.Header.Set("CF-Connecting-IP", "198.51.100.77") // honoured only from loopback: the test's peer is
	resp, err := http.DefaultClient.Do(again)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 400 || !strings.Contains(string(b), "replay") || seen.Load() != nil {
		t.Fatalf("replay from another address: %d %s", resp.StatusCode, b)
	}
}

// After a hijack goes away, the very next read reaches the real home: a bad
// answer makes every pooled connection to that host suspect, so none of the
// impostor's is reused (zm-rumn). The edge keeps its open streams on the
// impostor (Move), as Cloudflare would; only new connections reach the home.
func TestHijackDropsPooledConnections(t *testing.T) {
	h, srv, _ := home(t)
	fp, _ := h.Fingerprint()
	realH := h.Handler(echo(new(atomic.Pointer[core.Remote])))
	other := newService(t)
	impostors := map[string]http.Handler{
		// A relay keeps the home's key but alters the body: the signature
		// fails only after the whole answer was read.
		"altered": http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rec := httptest.NewRecorder()
			realH.ServeHTTP(rec, r)
			for k, v := range rec.Header() {
				w.Header()[k] = v
			}
			w.WriteHeader(rec.Code)
			_, _ = w.Write(bytes.Replace(rec.Body.Bytes(), []byte("x"), []byte("y"), 1))
		}),
		// Another home answers with no body: the wrong key is seen at once.
		"other home": other.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		})),
	}
	const n = 6
	for name, imp := range impostors {
		t.Run(name, func(t *testing.T) {
			// The impostor holds each answer until n requests are in, so the
			// member opens n connections to it.
			arrived := make(chan struct{}, n)
			release := make(chan struct{})
			isrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				arrived <- struct{}{}
				select {
				case <-release:
				case <-time.After(5 * time.Second):
				}
				imp.ServeHTTP(w, r)
			}))
			t.Cleanup(isrv.Close)
			p, err := tunneltest.New(strings.TrimPrefix(isrv.URL, "http://"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { p.Close() })
			_, c := member(t, fp)
			t.Cleanup(c.CloseIdleConnections)
			errs := make(chan error, n)
			for i := 0; i < n; i++ {
				go func() {
					_, _, err := do(t, c, "GET", p.URL()+"/v0/x", "zm_tok", "")
					errs <- err
				}()
			}
			for i := 0; i < n; i++ {
				select {
				case <-arrived:
				case <-time.After(5 * time.Second):
					t.Fatalf("only %d of %d requests reached the impostor", i, n)
				}
			}
			close(release)
			for i := 0; i < n; i++ {
				if err := <-errs; !errors.Is(err, ErrHomeKey) {
					t.Fatalf("read through the impostor: %v", err)
				}
			}
			p.Move(strings.TrimPrefix(srv.URL, "http://"))
			resp, body, err := do(t, c, "GET", p.URL()+"/v0/x", "zm_tok", "")
			if err != nil || resp.StatusCode != http.StatusCreated {
				t.Fatalf("first read on the real home: %v %s", err, body)
			}
		})
	}
}
