package tailnet

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/sealbox"
	"github.com/sengirunners/potluck/kit/tunnel"
)

// sealDeps is a daemon's deps with its seal key (the tunnel module's) in
// a temp home.
func sealDeps(t *testing.T) (*core.Deps, *tunnel.Service) {
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
	d := &core.Deps{Config: cfg}
	tun := tunnel.New(d)
	d.AddTransport(tun.TeamTransport())
	return d, tun
}

// wireTap records every request and answer whole, as a tailnet relay that
// could read the traffic would.
type wireTap struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (w *wireTap) add(b []byte)                { w.mu.Lock(); w.buf.Write(b); w.mu.Unlock() }
func (w *wireTap) Write(b []byte) (int, error) { w.add(b); return len(b), nil }
func (w *wireTap) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}
func (w *wireTap) has(s string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return bytes.Contains(w.buf.Bytes(), []byte(s))
}
func (w *wireTap) len() int { w.mu.Lock(); defer w.mu.Unlock(); return w.buf.Len() }

// sealHome is a home's tailnet listener in front of a route that records
// the body it got: WhoIs names every caller node (nMEMBER).
type sealHome struct {
	s    *Service
	srv  *httptest.Server
	wire *wireTap
	mu   sync.Mutex
	got  []byte
	rm   core.Remote
	hits int
	node string // the caller's node as WhoIs names it
}

func newSealHome(t *testing.T, d *core.Deps) *sealHome {
	h := &sealHome{s: &Service{d: d}, wire: &wireTap{}, node: "nMEMBER"}
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var hdr bytes.Buffer
		_ = r.Header.Write(&hdr)
		h.wire.add([]byte(r.Method + " " + r.RequestURI + "\n" + hdr.String() + "\n"))
		h.wire.add(b)
		r.Body = io.NopCloser(bytes.NewReader(b))
		rm := core.Remote{NodeID: h.node, Node: "member"}
		own, fault := h.s.nameKey(w, r)
		r, ok := h.s.sealDoor(w, r, &rm, own, fault)
		if !ok {
			return
		}
		if api.GetHeader(r.Header, api.HeaderSealKey) != "" || api.GetHeader(r.Header, api.HeaderSealed) != "" {
			t.Errorf("sealing headers reached the route: %v", r.Header)
		}
		body, _ := io.ReadAll(r.Body)
		h.mu.Lock()
		h.got, h.rm = body, rm
		h.hits++
		h.mu.Unlock()
		w.WriteHeader(200)
	}))
	t.Cleanup(h.srv.Close)
	return h
}

func (h *sealHome) last() ([]byte, core.Remote, int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.got, h.rm, h.hits
}

const canary = "c4n4ry-hunter2-7f3a"

// testTokens are the tokens the tests' homes take, and the machine each is
// for (sealAuth stands in for the token registry).
var testTokens = map[string]string{"zm_tok": "member", "tok": "member", "real-member-token": "member", "other-member-token": "m2"}

func init() {
	sealAuth = func(_ *Service, r *http.Request, _ core.Remote) (string, error) {
		if m, ok := testTokens[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")]; ok {
			return m, nil
		}
		return "", api.Errorf(api.CodeUnauthenticated, "invalid or expired token")
	}
}

// memberST is a member daemon's seal transport, its node nMEMBER.
func memberST(d *core.Deps, next http.RoundTripper) *sealTransport {
	st := newSealTransport(d, next)
	st.self = func(*http.Request) string { return "nMEMBER" }
	return st
}

func post(t *testing.T, c *http.Client, url, body string, seal bool, token string) (int, string, error) {
	t.Helper()
	req, _ := http.NewRequest("POST", url, strings.NewReader(body))
	api.SetHeader(req.Header, api.HeaderProject, "p1")
	if seal {
		api.SetHeader(req.Header, api.HeaderSeal, "body")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), nil
}

// Over the tailnet a run-target body with secrets goes sealed to the
// home's key, whole: the wire carries no secret, name or argv, the route
// gets the body as sent, and the home knows the member's key.
func TestTailnetSealsSecretBodies(t *testing.T) {
	hd, _ := sealDeps(t)
	md, mtun := sealDeps(t)
	h := newSealHome(t, hd)
	c := &http.Client{Transport: memberST(md, http.DefaultTransport.(*http.Transport).Clone())}
	url := h.srv.URL + "/v0/run_targets/sandbox/handles/b1/exec?project=p"
	body := `{"argv":["sh","-c","run-it"],"secrets":{"CANARY_NAME":"` + canary + `"},"jit_config":"` + canary + `"}`
	code, out, err := post(t, c, url, body, true, "zm_tok")
	if err != nil || code != 200 {
		t.Fatalf("sealed exec: %d %s %v", code, out, err)
	}
	got, rm, _ := h.last()
	if string(got) != body {
		t.Fatalf("route got %s, want %s", got, body)
	}
	if h.wire.has(canary) || h.wire.has("CANARY_NAME") || h.wire.has("run-it") || !h.wire.has(`{"zm_sealed":"zs1.`) {
		t.Fatal("the wire carried the body in the clear, or nothing sealed")
	}
	mid, mcert, _ := mtun.SealIdentity()
	wantID, wantKey, _ := sealbox.Peer(mid, mcert)
	if rm.SealID != wantID || !bytes.Equal(rm.SealKey, wantKey) {
		t.Fatalf("home's view of the member's key: %+v", rm)
	}

	// Any spelling encoding/json reads as secrets goes sealed too.
	for _, b := range []string{
		`{"argv":["true"],"Secrets":{"A":"` + canary + `"}}`,
		`{"argv":["true"],"ſecrets":{"A":"` + canary + `"}}`,
		`{"argv":["true"],"JIT_CONFIG":"` + canary + `"}`,
	} {
		if code, out, err := post(t, c, url, b, true, "zm_tok"); err != nil || code != 200 {
			t.Fatalf("sealed %s: %d %s %v", b, code, out, err)
		}
		if got, _, _ := h.last(); string(got) != b || h.wire.has(canary) {
			t.Fatalf("%s: route got %s, or the wire carried it", b, got)
		}
	}

	// A caller that forgot to ask for sealing: refused before it leaves.
	before := h.wire.len()
	if _, _, err := post(t, c, url, `{"Secrets":{"A":"`+canary+`"}}`, false, "zm_tok"); err == nil || !strings.Contains(err.Error(), "too old") || h.wire.len() != before {
		t.Fatalf("unsealed secrets: %v (wire grew: %v)", err, h.wire.len() != before)
	}
	// Without a token a sealed body is never opened.
	if code, _, _ := post(t, c, url, body, true, ""); code == 200 {
		t.Fatal("tokenless sealed request opened")
	}
}

// What the home refuses: a tampered envelope, one sealed for another key,
// one replayed, secrets in the clear from a caller that seals. A caller
// from before sealing (it names no key) is taken as before.
func TestTailnetSealRefusals(t *testing.T) {
	hd, _ := sealDeps(t)
	md, _ := sealDeps(t)
	h := newSealHome(t, hd)
	st := memberST(md, http.DefaultTransport.(*http.Transport).Clone())
	c := &http.Client{Transport: st}
	url := h.srv.URL + "/v0/run_targets/sandbox/handles/b1/exec"
	body := `{"argv":["true"],"secrets":{"A":"` + canary + `"}}`
	if code, out, err := post(t, c, url, body, true, "zm_tok"); err != nil || code != 200 {
		t.Fatalf("sealed: %d %s %v", code, out, err)
	}

	// Capture what went: the sealed request, to send again by hand.
	var sent *http.Request
	var sentBody []byte
	capture := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Body != nil && api.GetHeader(r.Header, api.HeaderSealed) != "" {
			b, _ := io.ReadAll(r.Body)
			sent, sentBody = r.Clone(r.Context()), b
			r.Body = io.NopCloser(bytes.NewReader(b))
		}
		return http.DefaultTransport.RoundTrip(r)
	})
	st2 := memberST(md, capture)
	if code, out, err := post(t, &http.Client{Transport: st2}, url, body, true, "zm_tok"); err != nil || code != 200 {
		t.Fatalf("captured: %d %s %v", code, out, err)
	}
	resend := func(b []byte, edit func(http.Header)) (int, string) {
		t.Helper()
		req, _ := http.NewRequest("POST", url, bytes.NewReader(b))
		req.Header = sent.Header.Clone()
		if edit != nil {
			edit(req.Header)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		out, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(out)
	}
	_, _, hits := h.last()
	// Replayed whole: refused.
	if code, out := resend(sentBody, nil); code != 400 || !strings.Contains(out, "replay") {
		t.Fatalf("replay: %d %s", code, out)
	}
	// Tampered: each part of the blob, and the binding headers.
	var env map[string]string
	_ = json.Unmarshal(sentBody, &env)
	blob := env[sealbox.Field]
	for _, i := range []int{len("zs1.") + 2, len(blob) - 20, len(blob) - 1} {
		bad := []byte(blob)
		bad[i] ^= 1
		b, _ := json.Marshal(map[string]string{sealbox.Field: string(bad)})
		if code, out := resend(b, nil); code != 400 || !strings.Contains(out, "could not be opened") {
			t.Fatalf("tampered byte %d: %d %s", i, code, out)
		}
	}
	if code, out := resend(sentBody, func(h http.Header) { api.SetHeader(h, api.HeaderSealNonce, "another-nonce-22-chars") }); code != 400 || !strings.Contains(out, "could not be opened") {
		t.Fatalf("another nonce: %d %s", code, out)
	}
	if code, out := resend(sentBody, func(h http.Header) { api.SetHeader(h, api.HeaderProject, "other-project") }); code != 400 || !strings.Contains(out, "could not be opened") {
		t.Fatalf("another project: %d %s", code, out)
	}
	if code, _ := resend(sentBody, func(h http.Header) { h.Del("Authorization") }); code == 200 {
		t.Fatal("tokenless resend opened")
	}
	// Another path (another box or verb): bound to its URI, it opens nowhere else.
	req, _ := http.NewRequest("POST", h.srv.URL+"/v0/run_targets/sandbox/handles/b2/exec", bytes.NewReader(sentBody))
	req.Header = sent.Header.Clone()
	api.SetHeader(req.Header, api.HeaderSealNonce, "yet-another-nonce-22ch")
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != 400 {
		t.Fatalf("another box: %v %v", resp, err)
	}
	if _, _, n := h.last(); n != hits {
		t.Fatal("a refused request reached the route")
	}

	// Sealed for another home's key (a wrong recipient): never opens here.
	_, otun := sealDeps(t)
	id, key, _ := keyOf(otun)
	sl := sealer(md)
	nonce := "wrong-recipient-nonce-1"
	date := sent.Header.Get(api.HeaderNew(api.HeaderSealDate))
	purpose := tailnetPurpose("POST", "/v0/run_targets/sandbox/handles/b1/exec", date, nonce, "nMEMBER", sent.Header)
	misb, err := sealbox.SealWhole([]byte(body), func(pt []byte) (string, error) { return sl.SealTo(id, key, purpose, time.Minute, pt) })
	if err != nil {
		t.Fatal(err)
	}
	if code, out := resend(misb, func(h http.Header) { api.SetHeader(h, api.HeaderSealNonce, nonce) }); code != 400 || !strings.Contains(out, "could not be opened") {
		t.Fatalf("wrong recipient: %d %s", code, out)
	}

	// Secrets in the clear from a caller that names a key: refused, unused.
	clear := []byte(`{"argv":["true"],"SECRETS":{"A":"x"}}`)
	if code, out := resend(clear, func(h http.Header) { api.DelHeader(h, api.HeaderSealed) }); code != 403 || !strings.Contains(out, "in the clear") {
		t.Fatalf("clear from a sealing caller: %d %s", code, out)
	}
	if _, _, n := h.last(); n != hits {
		t.Fatal("clear secrets reached the route")
	}
	// From a caller that names none (a daemon from before): taken, as before.
	if code, out := resend(clear, func(h http.Header) {
		for _, n := range []string{api.HeaderSealed, api.HeaderSealID, api.HeaderSealKey, api.HeaderSealDate, api.HeaderSealNonce} {
			api.DelHeader(h, n)
		}
	}); code != 200 {
		t.Fatalf("clear from an older caller: %d %s", code, out)
	}
	if got, rm, _ := h.last(); string(got) != string(clear) || rm.SealKey != nil {
		t.Fatalf("older caller: route got %s, key %v", got, rm.SealKey)
	}
}

// A home from before sealing on the tailnet names no key, and has no
// sealing mode: the member sends as before, in the clear over the tailnet
// (no worse than before), warns once, and never marks the body sealed. A
// home that names a key is never sent secrets in the clear.
func TestTailnetOlderHome(t *testing.T) {
	md, _ := sealDeps(t)
	logs := &wireTap{}
	md.Log = slog.New(slog.NewTextHandler(logs, nil))
	wire := &wireTap{}
	var sealedHdr atomic.Bool
	old := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		wire.add(b)
		if api.GetHeader(r.Header, api.HeaderSealed) != "" {
			sealedHdr.Store(true)
		}
		w.WriteHeader(200)
	}))
	t.Cleanup(old.Close)
	c := &http.Client{Transport: memberST(md, http.DefaultTransport.(*http.Transport).Clone())}
	for range 3 {
		code, _, err := post(t, c, old.URL+"/v0/run_targets/sandbox/handles/b1/exec", `{"secrets":{"A":"`+canary+`"}}`, true, "zm_tok")
		if err != nil || code != 200 || !wire.has(canary) || sealedHdr.Load() {
			t.Fatalf("older home: %d %v (clear on the wire: %v, marked sealed: %v)", code, err, wire.has(canary), sealedHdr.Load())
		}
	}
	if n := strings.Count(logs.String(), "level=WARN"); n != 1 || !strings.Contains(logs.String(), "update it") || logs.has(canary) {
		t.Fatalf("want one Warn naming no secret, got %d: %s", n, logs.String())
	}
	// Forgetting to ask for sealing is still refused, before it leaves.
	before := wire.len()
	if _, _, err := post(t, c, old.URL+"/v0/run_targets/sandbox/handles/b1/exec", `{"secrets":{"A":"x"}}`, false, "zm_tok"); err == nil || wire.len() != before {
		t.Fatalf("unsealed without asking: %v", err)
	}
	if code, _, err := post(t, c, old.URL+"/v0/run_targets/sandbox/handles/b1/exec", `{"argv":["true"]}`, false, "zm_tok"); err != nil || code != 200 {
		t.Fatalf("secret-free call: %d %v", code, err)
	}
	// This member names its key on every call (a home that seals takes it).
	var hdr http.Header
	probe := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hdr = r.Header.Clone() }))
	t.Cleanup(probe.Close)
	_, _, _ = post(t, c, probe.URL+"/v0/health", "", false, "")
	if id := api.GetHeader(hdr, api.HeaderSealID); id == "" || api.GetHeader(hdr, api.HeaderSealKey) == "" {
		t.Fatalf("no key named: %v", hdr)
	} else if _, err := base64.RawURLEncoding.DecodeString(id); err != nil {
		t.Fatalf("seal id %q", id)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// keyOf is a daemon's seal key id and key, as its headers name them.
func keyOf(tun *tunnel.Service) (string, []byte, bool) {
	idB64, cert, err := tun.SealIdentity()
	if err != nil {
		return "", nil, false
	}
	return sealbox.Peer(idB64, cert)
}
