package tunnel

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/sealbox"
)

func sealPub(t *testing.T, s *Service) []byte {
	t.Helper()
	id, _ := s.key()
	pub := sealbox.VerifyCert(id.Public().(ed25519.PublicKey), s.SealCert())
	if pub == nil {
		t.Fatal("no certified seal key")
	}
	return pub
}

// Sealed for one daemon, one purpose, one sender, until its expiry: it
// opens there and nowhere else, and a changed byte opens nowhere.
func TestSealOpensOnlyWhereItWasSealedFor(t *testing.T) {
	a, h, other := newService(t), newService(t), newService(t)
	aid, _ := a.KeyID()
	hid, _ := h.KeyID()
	const purpose = `["run_target op","p1","op1","sandbox","exec","box1"]`
	plain := []byte(`{"secrets":{"API_TOKEN":"hunter2hunter2"}}`)
	sealed, err := a.sealTo(hid, sealPub(t, h), purpose, time.Minute, plain)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(sealed, "hunter2") || !strings.HasPrefix(sealed, "zs1.") {
		t.Fatalf("sealed: %q", sealed)
	}
	if got, err := h.openFrom(aid, purpose, sealed); err != nil || !bytes.Equal(got, plain) {
		t.Fatalf("round trip: %q %v", got, err)
	}
	refused := func(what string, got []byte, err error) {
		t.Helper()
		if err == nil || got != nil || !api.IsCode(err, api.CodeBadRequest) || strings.Contains(err.Error(), "hunter2") {
			t.Errorf("%s: want the one 400, got %q %v", what, got, err)
		}
	}
	// Another daemon's key (wrong key).
	got, err := other.openFrom(aid, purpose, sealed)
	refused("another daemon", got, err)
	// Replays: another op, box, verb or project; another sender.
	for _, p := range []string{
		`["run_target op","p1","op2","sandbox","exec","box1"]`,
		`["run_target op","p1","op1","sandbox","exec","box2"]`,
		`["run_target op","p1","op1","sandbox","spawn","box1"]`,
		`["run_target op","p2","op1","sandbox","exec","box1"]`,
		"request\nPOST\n/v0/run_targets/sandbox/box1/exec\n1\nnonce",
	} {
		got, err := h.openFrom(aid, p, sealed)
		refused("purpose "+p, got, err)
	}
	oid, _ := other.KeyID()
	got, err = h.openFrom(oid, purpose, sealed)
	refused("claimed by another sender", got, err)
	// Sealed for another host, then handed to h: h is not its recipient
	// (the info names the recipient's key id, the KEM its key).
	forOther, _ := a.sealTo(oid, sealPub(t, other), purpose, time.Minute, plain)
	got, err = h.openFrom(aid, purpose, forOther)
	refused("sealed for another host", got, err)
	// Sealed to h's key but naming another recipient id.
	misnamed, _ := a.sealTo(oid, sealPub(t, h), purpose, time.Minute, plain)
	got, err = h.openFrom(aid, purpose, misnamed)
	refused("another recipient id", got, err)
	// Tampered: each part of the blob.
	rest := strings.TrimPrefix(sealed, "zs1.")
	exp, b64, _ := strings.Cut(rest, ".")
	ct, _ := base64.RawURLEncoding.DecodeString(b64)
	for i := range []int{0, 1, 2} {
		bad := bytes.Clone(ct)
		bad[[]int{0, 40, len(bad) - 1}[i]] ^= 1 // the encapsulated key, the ciphertext, the tag
		got, err := h.openFrom(aid, purpose, "zs1."+exp+"."+base64.RawURLEncoding.EncodeToString(bad))
		refused("tampered byte", got, err)
	}
	n, _ := strconv.ParseInt(exp, 10, 64)
	got, err = h.openFrom(aid, purpose, "zs1."+strconv.FormatInt(n+3600, 10)+"."+b64)
	refused("a later expiry", got, err)
	got, err = h.openFrom(aid, purpose, "zs2."+rest)
	refused("another version", got, err)
	// Expired (past the skew both clocks may have).
	defer func(f func() time.Time) { nowFunc = f }(nowFunc)
	nowFunc = func() time.Time { return time.Now().Add(time.Minute + MaxSkew + time.Second) }
	got, err = h.openFrom(aid, purpose, sealed)
	refused("expired", got, err)
}

// A certificate counts only as its own identity signed it, for this suite.
func TestSealCertificate(t *testing.T) {
	a, b := newService(t), newService(t)
	aid, _ := a.key()
	bid, _ := b.key()
	ap, bp := aid.Public().(ed25519.PublicKey), bid.Public().(ed25519.PublicKey)
	cert := a.SealCert()
	if sealbox.VerifyCert(ap, cert) == nil {
		t.Fatal("own certificate refused")
	}
	if sealbox.VerifyCert(bp, cert) != nil {
		t.Fatal("a's certificate taken as b's")
	}
	parts := strings.Split(cert, ".")
	other := strings.Split(b.SealCert(), ".")
	for _, bad := range []string{
		"", "2." + parts[1] + "." + parts[2], parts[0] + "." + other[1] + "." + parts[2], cert + ".x",
		parts[0] + "." + parts[1] + "." + other[2],
	} {
		if sealbox.VerifyCert(ap, bad) != nil {
			t.Errorf("certificate %q taken", bad)
		}
	}
	// The seal key is the file's across restarts; another home's is not.
	pub := sealPub(t, a)
	again := New(&core.Deps{Config: a.d.Config})
	if !bytes.Equal(sealPub(t, again), pub) {
		t.Fatal("seal key not kept across a restart")
	}
	if fi, err := os.Stat(filepath.Join(a.d.Config.Home, SealKeyFile)); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("seal key file: %v %v", fi, err)
	}
	if _, err := a.sealTo("zmkey:x", nil, "p", time.Minute, []byte("x")); !api.IsCode(err, api.CodeForbidden) || !strings.Contains(err.Error(), "too old") {
		t.Fatalf("no key: %v", err)
	}
}

// tap is a Cloudflare that reads everything: every byte both ways.
type tap struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (c *tap) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.Write(p)
}

func (c *tap) has(s string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return bytes.Contains(c.buf.Bytes(), []byte(s))
}

// tapped is home's door behind a handler that records each request whole.
func tapped(t *testing.T, h *Service, next http.Handler, wire *tap) *httptest.Server {
	door := h.Handler(next)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var hdr bytes.Buffer
		_ = r.Header.Write(&hdr)
		wire.Write([]byte(r.Method + " " + r.RequestURI + "\n" + hdr.String() + "\n"))
		wire.Write(b)
		r.Body = io.NopCloser(bytes.NewReader(b))
		rec := httptest.NewRecorder()
		door.ServeHTTP(rec, r)
		_ = rec.Result().Header.Write(wire)
		wire.Write(rec.Body.Bytes())
		for k, v := range rec.Header() {
			w.Header()[k] = v
		}
		w.WriteHeader(rec.Code)
		_, _ = w.Write(rec.Body.Bytes())
	}))
	t.Cleanup(srv.Close)
	return srv
}

// A member asks for fields to be sealed: the wire carries only ciphertext,
// the route behind the door gets the fields back as sent, and the door
// records the member's certified seal key for the authority's own sealing.
func TestSealedRequestFields(t *testing.T) {
	h := newService(t)
	fp, _ := h.Fingerprint()
	var seen atomic.Pointer[core.Remote]
	var got atomic.Pointer[[]byte]
	wire := &tap{}
	srv := tapped(t, h, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.Store(core.RemoteOf(r.Context()))
		b, _ := io.ReadAll(r.Body)
		got.Store(&b)
		if r.Header.Get(api.HeaderSealed) != "" || r.Header.Get(api.HeaderSealKey) != "" {
			t.Errorf("sealing headers reached the route: %v", r.Header)
		}
		w.WriteHeader(200)
	}), wire)
	m, c := member(t, fp)
	const secret, jit = "hunter2hunter2", "QUJDQUJDQUJDQUJD"
	body := `{"argv":["true"],"secrets":{"API_TOKEN":"` + secret + `"},"jit_config":"` + jit + `"}`
	send := func(c *http.Client, token string) (*http.Response, string) {
		req, _ := http.NewRequest("POST", srv.URL+"/v0/run_targets/sandbox/b1/exec", strings.NewReader(body))
		req.Header.Set(api.HeaderSeal, "secrets,jit_config")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := c.Do(req)
		if err != nil {
			return nil, err.Error()
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp, string(b)
	}
	resp, out := send(c, "zm_tok")
	if resp == nil || resp.StatusCode != 200 {
		t.Fatalf("sealed request: %v %s", resp, out)
	}
	var want, have map[string]any
	_ = json.Unmarshal([]byte(body), &want)
	_ = json.Unmarshal(*got.Load(), &have)
	if !jsonEqual(want, have) {
		t.Fatalf("route got %s, want %s", *got.Load(), body)
	}
	if wire.has(secret) || wire.has(jit) || wire.has("API_TOKEN") || wire.has(`"argv"`) || !wire.has(`{"zm_sealed":"zs1.`) {
		t.Fatal("the wire carried the body (a secret, its name, the jit_config, the argv) in the clear, or nothing sealed")
	}
	// Any spelling encoding/json reads as secrets is sealed with the rest:
	// the whole body goes sealed, and the route gets it byte for byte.
	for _, b := range []string{
		`{"argv":["true"],"Secrets":{"API_TOKEN":"` + secret + `"}}`,
		`{"argv":["true"],"ſecrets":{"API_TOKEN":"` + secret + `"}}`,
		`{"argv":["true"],"secrets":{"A":"x"},"SECRETS":{"API_TOKEN":"` + secret + `"},"JIT_CONFIG":"` + jit + `"}`,
	} {
		body = b
		if resp, out := send(c, "zm_tok"); resp == nil || resp.StatusCode != 200 || string(*got.Load()) != b {
			t.Fatalf("sealed %s: %v %s, route got %s", b, resp, out, *got.Load())
		}
		if wire.has(secret) || wire.has(jit) {
			t.Fatalf("%s crossed in the clear", b)
		}
	}
	body = `{"argv":["true"],"secrets":{"API_TOKEN":"` + secret + `"},"jit_config":"` + jit + `"}`
	// A caller that forgot to ask for sealing: the transport refuses it.
	before := wire.len()
	req0, _ := http.NewRequest("POST", srv.URL+"/v0/run_targets/sandbox/b1/exec", strings.NewReader(`{"Secrets":{"API_TOKEN":"`+secret+`"}}`))
	req0.Header.Set("Authorization", "Bearer zm_tok")
	if _, err := c.Do(req0); err == nil || !strings.Contains(err.Error(), "too old") || wire.len() != before {
		t.Fatalf("unsealed secrets: %v (wire grew: %v)", err, wire.len() != before)
	}
	if rm := seen.Load(); rm == nil || !bytes.Equal(rm.SealKey, sealPub(t, m)) {
		t.Fatalf("door's seal key for the member: %+v", rm)
	}
	// Without a token nothing is opened (the probe and redemption are tokenless).
	if resp, out := send(c, ""); resp != nil && resp.StatusCode == 200 {
		t.Fatalf("tokenless sealed request opened: %s", out)
	}

	// The same sealed body in another request (a fresh nonce, signed by the
	// member's own key): bound to its first request, it opens nowhere else.
	k, _ := m.key()
	last := wire.lastSealedBody(t)
	req, _ := http.NewRequest("POST", srv.URL+"/v0/run_targets/sandbox/b1/exec", bytes.NewReader(last))
	req.Header.Set("Authorization", "Bearer zm_tok")
	req.Header.Set(api.HeaderSealed, "1")
	req.Header.Set(HeaderKey, base64.RawURLEncoding.EncodeToString(k.Public().(ed25519.PublicKey)))
	req.Header.Set(HeaderDate, strconv.FormatInt(time.Now().Unix(), 10))
	req.Header.Set(HeaderNonce, "another-nonce-22-chars")
	req.Header.Set(HeaderSig, base64.RawURLEncoding.EncodeToString(ed25519.Sign(k, requestString(fp, "POST", "/v0/run_targets/sandbox/b1/exec", req.Header, last))))
	r2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b2, _ := io.ReadAll(r2.Body)
	r2.Body.Close()
	if r2.StatusCode != 400 || !strings.Contains(string(b2), "could not be opened") {
		t.Fatalf("sealed body in another request: %d %s", r2.StatusCode, b2)
	}
	// Secrets in the clear reaching the door (a foreign client) are never used.
	clear := []byte(`{"argv":["true"],"Secrets":{"API_TOKEN":"x"}}`)
	req, _ = http.NewRequest("POST", srv.URL+"/v0/run_targets/sandbox/b1/exec", bytes.NewReader(clear))
	req.Header.Set("Authorization", "Bearer zm_tok")
	req.Header.Set(HeaderKey, base64.RawURLEncoding.EncodeToString(k.Public().(ed25519.PublicKey)))
	req.Header.Set(HeaderDate, strconv.FormatInt(time.Now().Unix(), 10))
	req.Header.Set(HeaderNonce, "yet-another-nonce-22ch")
	req.Header.Set(HeaderSig, base64.RawURLEncoding.EncodeToString(ed25519.Sign(k, requestString(fp, "POST", "/v0/run_targets/sandbox/b1/exec", req.Header, clear))))
	got.Store(nil)
	r3, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	r3.Body.Close()
	if r3.StatusCode != 403 || got.Load() != nil {
		t.Fatalf("clear secrets at the door: %d, route reached: %v", r3.StatusCode, got.Load() != nil)
	}

	// An older home certifies no seal key: the member refuses before
	// anything leaves, saying which side is too old.
	undo := h.SealOff()
	_, c2 := member(t, fp)
	if _, out := send(c2, "zm_tok"); !strings.Contains(out, "too old") || !strings.Contains(out, "the home") {
		t.Fatalf("older home: %s", out)
	}
	undo()
	// An older member (sealing off) refuses too, never sends in the clear.
	m3, c3 := member(t, fp)
	m3.SealOff()
	before = wire.len()
	if _, out := send(c3, "zm_tok"); !strings.Contains(out, "too old") {
		t.Fatalf("older member: %s", out)
	}
	if wire.has(secret) || wire.has(jit) || wire.len() != before {
		t.Fatal("an older member's refused request reached the wire")
	}
}

func (c *tap) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.Len()
}

// lastSealedBody is the last request body on the wire with a sealed field.
func (c *tap) lastSealedBody(t *testing.T) []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	b := c.buf.Bytes()
	i := bytes.LastIndex(b, []byte(`{"zm_sealed"`))
	if i < 0 {
		t.Fatal("no sealed body on the wire")
	}
	j := bytes.IndexByte(b[i:], '}')
	for j >= 0 && json.Valid(b[i:i+j+1]) == false {
		k := bytes.IndexByte(b[i+j+1:], '}')
		if k < 0 {
			j = -1
			break
		}
		j += k + 1
	}
	if j < 0 {
		t.Fatal("no whole sealed body on the wire")
	}
	return bytes.Clone(b[i : i+j+1])
}

func jsonEqual(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return bytes.Equal(x, y)
}

// A secret grant's body names a secret: it crosses a tunnel only sealed,
// like a run target's secrets; an empty body (a list, a removal) is none.
func TestSecretGrantBodyIsSecret(t *testing.T) {
	for _, c := range []struct {
		path, body string
		want       bool
	}{
		{SecretGrantsPath, `{"name":"API_KEY","kind":"value","to":{"role":"member"}}`, true},
		{SecretGrantsPath, "", false},
		{SecretGrantsPath + "/01ABC", "", false},
		{"/v0/run_targets/box/handles/1/exec", `{"argv":["true"],"secrets":{"A":"x"}}`, true},
		{"/v0/run_targets/box/handles/1/exec", `{"argv":["true"]}`, false},
		{"/v0/usage", `{"name":"x"}`, false},
	} {
		if got := secretInClear(c.path, []byte(c.body)); got != c.want {
			t.Errorf("%s %s: %v, want %v", c.path, c.body, got, c.want)
		}
	}
	// An older member's unsealed grant is told the grant route needs sealing.
	if err := errClear(SecretGrantsPath, []byte(`{"name":"A"}`), "the calling machine"); !strings.Contains(err.Error(), "secret grant") || strings.Contains(err.Error(), "jit_config") {
		t.Errorf("a grant's refusal: %v", err)
	}
	if err := errClear("/v0/run_targets/b/handles/1/exec", []byte(`{"secrets":{"A":"x"}}`), "the calling machine"); !strings.Contains(err.Error(), "jit_config") {
		t.Errorf("exec's refusal: %v", err)
	}
}
