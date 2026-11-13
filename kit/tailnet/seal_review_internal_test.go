package tailnet

// Tests from the security review of the tailnet seal (zm-eakh.12.48.2):
// a stranger cannot fill the home's nonce set, a sealed request opens only
// from the node that sent it, a seal id is one machine's, a key-less answer
// never makes a member send in the clear to a home that seals, and every
// answer of the listener names the key, a 503 while starting included.

import (
	"bytes"
	"context"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/sealbox"
	"github.com/sengirunners/potluck/kit/tunnel"
)

// captureRT records the request the seal transport would put on the wire
// and answers as the home would (naming the home's key).
type captureRT struct {
	id, cert string
	last     *http.Request
	body     []byte
}

func (c *captureRT) RoundTrip(r *http.Request) (*http.Response, error) {
	c.last = r
	c.body = nil
	if r.Body != nil {
		c.body, _ = io.ReadAll(r.Body)
	}
	h := http.Header{}
	api.SetHeader(h, api.HeaderSealID, c.id)
	api.SetHeader(h, api.HeaderSealKey, c.cert)
	return &http.Response{StatusCode: 200, Header: h, Body: io.NopCloser(strings.NewReader("{}")), Request: r}, nil
}

// nodeST is daemon d's seal transport from node, answered by c.
func nodeST(t *testing.T, d *core.Deps, home *tunnel.Service, node string) (*sealTransport, *captureRT) {
	t.Helper()
	id, cert, err := home.SealIdentity()
	if err != nil {
		t.Fatal(err)
	}
	c := &captureRT{id: id, cert: cert}
	st := newSealTransport(d, c)
	st.self = func(*http.Request) string { return node }
	return st, c
}

// sealedReq is one sealed exec as it arrives at the home's listener.
func sealedReq(t *testing.T, st *sealTransport, c *captureRT, token string) *http.Request {
	t.Helper()
	req, _ := http.NewRequest("POST", "http://home/v0/run_targets/sandbox/handles/b1/exec", strings.NewReader(`{"argv":["true"],"secrets":{"A":"v"}}`))
	api.SetHeader(req.Header, api.HeaderProject, "p1")
	api.SetHeader(req.Header, api.HeaderSeal, "body")
	req.Header.Set("Authorization", "Bearer "+token)
	if _, err := st.RoundTrip(req); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(c.last.Method, c.last.URL.RequestURI(), bytes.NewReader(c.body))
	for k, v := range c.last.Header {
		r.Header[k] = append([]string(nil), v...)
	}
	return r
}

func door(s *Service, r *http.Request, node string) (int, string) {
	w := httptest.NewRecorder()
	rm := core.Remote{NodeID: node, Node: node}
	own, fault := s.nameKey(w, r)
	if _, ok := s.sealDoor(w, r, &rm, own, fault); ok {
		return 200, ""
	}
	return w.Code, w.Body.String()
}

// A node with no valid token gets nothing opened and puts nothing in the
// nonce set; a member that floods fills only its own share, and another
// member still gets through.
func TestTailnetNonceFloodByStranger(t *testing.T) {
	defer func(a, b int) { sealNoncesMax, sealNoncesPerNode = a, b }(sealNoncesMax, sealNoncesPerNode)
	sealNoncesMax, sealNoncesPerNode = 64, 8
	hd, htun := sealDeps(t)
	home := &Service{d: hd}
	sd, _ := sealDeps(t)
	sst, sc := nodeST(t, sd, htun, "nSTRANGER")
	for i := range 200 {
		if code, msg := door(home, sealedReq(t, sst, sc, "not-a-real-token"), "nSTRANGER"); code != 401 {
			t.Fatalf("stranger request %d: %d %s", i, code, msg)
		}
	}
	if n := len(home.sealNonces.seen); n != 0 {
		t.Fatalf("a stranger put %d nonces in the set", n)
	}
	// A member floods: past its share it is refused, 503, and only it.
	fd, _ := sealDeps(t)
	fst, fc := nodeST(t, fd, htun, "nM2")
	full := false
	for range sealNoncesPerNode + 2 {
		code, msg := door(home, sealedReq(t, fst, fc, "other-member-token"), "nM2")
		if code == 503 && strings.Contains(msg, "too many") {
			full = true
		} else if code != 200 {
			t.Fatalf("flooding member: %d %s", code, msg)
		}
	}
	if !full {
		t.Fatal("a member was never capped")
	}
	md, _ := sealDeps(t)
	mst, mc := nodeST(t, md, htun, "nMEMBER")
	if code, msg := door(home, sealedReq(t, mst, mc, "real-member-token"), "nMEMBER"); code != 200 {
		t.Fatalf("a member under another's flood: %d %s", code, msg)
	}
}

// Expired nonces leave the set from its front without a scan, and the
// per-node count follows.
func TestTailnetNonceSetEviction(t *testing.T) {
	var n nonceSet
	now := sealNow()
	for i := range 10 {
		if n.add("zmkey:a", "nA", strconv.Itoa(i), now.Add(SealSkew), now) != nonceNew {
			t.Fatal("not taken")
		}
	}
	if n.add("zmkey:a", "nA", "3", now.Add(SealSkew), now) != nonceSeen {
		t.Fatal("a replay was taken")
	}
	later := now.Add(SealSkew + 1)
	if n.add("zmkey:a", "nA", "3", later.Add(SealSkew), later) != nonceNew || len(n.seen) != 1 || n.byNode["nA"] != 1 {
		t.Fatalf("expired entries kept: %d seen, %d for nA", len(n.seen), n.byNode["nA"])
	}
}

// The nonce set is keyed by seal id, and the purpose binds the sending
// node: the same sealed request opens neither again nor from another node.
func TestTailnetReplayFromAnotherNode(t *testing.T) {
	hd, htun := sealDeps(t)
	home := &Service{d: hd}
	md, _ := sealDeps(t)
	st, c := nodeST(t, md, htun, "nA")
	r := sealedReq(t, st, c, "tok")
	body, hdr := c.body, r.Header.Clone()
	again := func() *http.Request {
		x := httptest.NewRequest(r.Method, r.RequestURI, bytes.NewReader(body))
		x.Header = hdr.Clone()
		return x
	}
	if code, msg := door(home, again(), "nA"); code != 200 {
		t.Fatalf("first: %d %s", code, msg)
	}
	if code, msg := door(home, again(), "nA"); code != 400 || !strings.Contains(msg, "replay") {
		t.Fatalf("replay from the same node: %d %s", code, msg)
	}
	if code, msg := door(home, again(), "nB"); code != 400 || !strings.Contains(msg, "could not be opened") {
		t.Fatalf("replay from another node: %d %s", code, msg)
	}
}

// A seal id is the machine's that first named it: a stranger naming a
// member's id is refused before anything opens (no token), and another
// member naming it is refused too.
func TestTailnetSpoofedSealID(t *testing.T) {
	hd, htun := sealDeps(t)
	home := &Service{d: hd}
	vd, vtun := sealDeps(t)
	vst, vc := nodeST(t, vd, htun, "nMEMBER")
	if code, msg := door(home, sealedReq(t, vst, vc, "tok"), "nMEMBER"); code != 200 {
		t.Fatalf("the member's own call: %d %s", code, msg)
	}
	vid, vcert, _ := vtun.SealIdentity()
	vkid, _, _ := sealbox.Peer(vid, vcert)
	hkid, hpub, _ := keyOf(htun)
	spoof := func(token, node string) (int, string) {
		now := sealNow()
		d, n := strconv.FormatInt(now.Unix(), 10), "AAAAAAAAAAAAAAAAAAAAAA"+node
		r := httptest.NewRequest("POST", "/v0/run_targets/sandbox/handles/b1/exec", nil)
		api.SetHeader(r.Header, api.HeaderProject, "p1")
		api.SetHeader(r.Header, api.HeaderSealID, vid)
		api.SetHeader(r.Header, api.HeaderSealKey, vcert)
		api.SetHeader(r.Header, api.HeaderSealDate, d)
		api.SetHeader(r.Header, api.HeaderSealNonce, n)
		api.SetHeader(r.Header, api.HeaderSealed, "1")
		r.Header.Set("Authorization", "Bearer "+token)
		purpose := tailnetPurpose("POST", "/v0/run_targets/sandbox/handles/b1/exec", d, n, node, r.Header)
		b, err := sealbox.SealWhole([]byte(`{"secrets":{"EVIL":"attacker-chosen"}}`), func(pt []byte) (string, error) {
			return sealbox.Seal(vkid, hkid, hpub, purpose, SealSkew, pt, now)
		})
		if err != nil {
			t.Fatal(err)
		}
		r.Body = io.NopCloser(bytes.NewReader(b))
		return door(home, r, node)
	}
	if code, msg := spoof("not-a-real-token", "nSTRANGER"); code != 401 {
		t.Fatalf("stranger naming the member's id: %d %s", code, msg)
	}
	// Another member names the victim's id: without the victim's identity
	// key it cannot sign, so nothing opens; and the victim is not locked out.
	if code, msg := spoof("other-member-token", "nM2"); code != 400 || !strings.Contains(msg, "could not be opened") || strings.Contains(msg, "member") {
		t.Fatalf("another member naming the member's id: %d %s", code, msg)
	}
	if code, msg := door(home, sealedReq(t, vst, vc, "tok"), "nMEMBER"); code != 200 {
		t.Fatalf("the member after another named its id: %d %s", code, msg)
	}
	// A signature by another key, or none, does not open either.
	r := sealedReq(t, vst, vc, "tok")
	api.DelHeader(r.Header, api.HeaderSealSig)
	if code, _ := door(home, r, "nMEMBER"); code != 400 {
		t.Fatalf("no signature: %d", code)
	}
}

// A request that names a key but whose caller is not a member's token (a
// tailnet login the route may take) passes on WITHOUT the key, so the
// route's sealing mode check sees a caller that sends in the clear.
func TestTailnetUnauthenticatedKeyDropped(t *testing.T) {
	hd, _ := sealDeps(t)
	home := &Service{d: hd}
	md, mtun := sealDeps(t)
	_ = md
	id, cert, _ := mtun.SealIdentity()
	r := httptest.NewRequest("POST", "/v0/run_targets/sandbox/handles/b1/exec", strings.NewReader(`{"secrets":{"A":"v"}}`))
	api.SetHeader(r.Header, api.HeaderSealID, id)
	api.SetHeader(r.Header, api.HeaderSealKey, cert)
	w := httptest.NewRecorder()
	rm := core.Remote{NodeID: "nLOGIN", Login: "lou@example.com"}
	own, fault := home.nameKey(w, r)
	if _, ok := home.sealDoor(w, r, &rm, own, fault); !ok || rm.SealKey != nil || rm.SealID != "" {
		t.Fatalf("passed on with the key: ok=%v %+v", ok, rm)
	}
}

// fakeReporter is a home's reported sealing mode.
type fakeReporter string

func (f fakeReporter) ReportedSealing(context.Context) string { return string(f) }

// keylessFront is a home behind a front that can answer as one starting
// (503, no key) or as a build from before sealing (2xx, no key).
func keylessFront(t *testing.T, h *sealHome, mode *string) *httptest.Server {
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		h.wire.add(b)
		switch *mode {
		case "starting":
			api.WriteError(w, api.Errorf(api.CodeUnavailable, "the daemon is starting"))
		case "older":
			w.WriteHeader(200)
		default:
			r.Body = io.NopCloser(bytes.NewReader(b))
			h.srv.Config.Handler.ServeHTTP(w, r)
		}
	}))
	t.Cleanup(front.Close)
	return front
}

// A home that seals but answers once without its key (503 while starting)
// never gets the body in the clear: the member reads "older home" only from
// a 2xx /v0/health. A home that reported required and names no key later is
// refused until this machine's owner forgets it, across a restart.
func TestTailnetKeylessAnswerNeverDowngrades(t *testing.T) {
	hd, _ := sealDeps(t)
	hd.Provide(core.SealingService, core.SealingReporter(fakeReporter("required")))
	md, mtun := sealDeps(t)
	h := newSealHome(t, hd)
	mode := "starting"
	front := keylessFront(t, h, &mode)
	const fp = "homefingerprint"
	st := memberST(md, http.DefaultTransport.(*http.Transport).Clone())
	st.fp = fp
	c := &http.Client{Transport: st}
	url := front.URL + "/v0/run_targets/sandbox/handles/b1/exec"
	body := `{"argv":["true"],"secrets":{"A":"` + canary + `"}}`
	if _, _, err := post(t, c, url, body, true, "tok"); err == nil || !strings.Contains(err.Error(), "named no seal key") || h.wire.has(canary) {
		t.Fatalf("while starting: %v (clear on the wire: %v)", err, h.wire.has(canary))
	}
	mode = "up"
	if code, out, err := post(t, c, url, body, true, "tok"); err != nil || code != 200 || h.wire.has(canary) {
		t.Fatalf("after start: %d %s %v (clear: %v)", code, out, err, h.wire.has(canary))
	}
	if m, err := mtun.HomeSealed(fp); m != "required" || err != nil {
		t.Fatalf("recorded: %q %v", m, err)
	}
	mode = "starting"
	if _, _, err := post(t, c, url, body, true, "tok"); err != nil && h.wire.has(canary) {
		t.Fatal("a 503 after the key was known sent the body in the clear")
	}
	// A restart of the member (a fresh transport), the home now names none.
	mode = "older"
	st2 := memberST(md, http.DefaultTransport.(*http.Transport).Clone())
	st2.fp = fp
	c2 := &http.Client{Transport: st2}
	if _, _, err := post(t, c2, url, body, true, "tok"); err == nil || !strings.Contains(err.Error(), "requires sealing") || !strings.Contains(err.Error(), "potluck sealing forget") || h.wire.has(canary) {
		t.Fatalf("a required home that sealed before, keyless now: %v (clear: %v)", err, h.wire.has(canary))
	}
	if forgot, err := mtun.ForgetHomeSealed(fp); !forgot || err != nil {
		t.Fatalf("forget: %v %v", forgot, err)
	}
	if code, _, err := post(t, c2, url, body, true, "tok"); err != nil || code != 200 || !h.wire.has(canary) {
		t.Fatalf("after the owner forgot it: %d %v (clear, as before sealing: %v)", code, err, h.wire.has(canary))
	}
}

// A home that reported preferred and is rolled back (it answers /v0/health
// 2xx naming no key): a running member notices without a restart and sends
// in the clear, warning every call; a non-2xx never causes that.
func TestTailnetPreferredRollback(t *testing.T) {
	hd, _ := sealDeps(t)
	hd.Provide(core.SealingService, core.SealingReporter(fakeReporter("preferred")))
	md, mtun := sealDeps(t)
	logs := &wireTap{}
	md.Log = slog.New(slog.NewTextHandler(logs, nil))
	h := newSealHome(t, hd)
	mode := "up"
	front := keylessFront(t, h, &mode)
	st := memberST(md, http.DefaultTransport.(*http.Transport).Clone())
	st.fp = "prefhome"
	c := &http.Client{Transport: st}
	url := front.URL + "/v0/run_targets/sandbox/handles/b1/exec"
	body := `{"argv":["true"],"secrets":{"A":"` + canary + `"}}`
	if code, _, err := post(t, c, url, body, true, "tok"); err != nil || code != 200 || h.wire.has(canary) {
		t.Fatalf("sealed: %d %v", code, err)
	}
	if m, _ := mtun.HomeSealed("prefhome"); m != "preferred" {
		t.Fatalf("recorded mode %q", m)
	}
	mode = "starting"
	if _, _, err := post(t, c, url, body, true, "tok"); h.wire.has(canary) {
		t.Fatalf("a 503 sent in the clear (%v)", err)
	}
	mode = "older"
	// The first call still goes sealed (the key was kept); its key-less
	// answer drops the key, and the next calls probe and go clear.
	_, _, _ = post(t, c, url, body, true, "tok")
	for range 2 {
		if code, _, err := post(t, c, url, body, true, "tok"); err != nil || code != 200 {
			t.Fatalf("rolled back: %d %v", code, err)
		}
	}
	if !h.wire.has(canary) {
		t.Fatal("a preferred home rolled back got nothing in the clear")
	}
	if n := strings.Count(logs.String(), "stopped sealing"); n < 2 {
		t.Fatalf("want a Warn every call, got %d: %s", n, logs.String())
	}
}

// Every answer of the listener names the key, the 503 while the daemon is
// starting included.
func TestTailnetStartingAnswerNamesKey(t *testing.T) {
	hd, _ := sealDeps(t)
	s := &Service{d: hd}
	rec := httptest.NewRecorder()
	s.RemoteHandler(newFakeNode()).ServeHTTP(rec, httptest.NewRequest("GET", "/v0/health", nil))
	if rec.Code != 503 || api.GetHeader(rec.Header(), api.HeaderSealID) == "" || api.GetHeader(rec.Header(), api.HeaderSealKey) == "" {
		t.Fatalf("starting: %d %v", rec.Code, rec.Header())
	}
}

// A daemon that cannot read its own seal key sends no secrets: a fault, not
// an older build.
func TestTailnetOwnKeyFaultRefuses(t *testing.T) {
	hd, _ := sealDeps(t)
	md, _ := sealDeps(t)
	key := filepath.Join(md.Config.Home, tunnel.SealKeyFile)
	if err := os.MkdirAll(filepath.Dir(key), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(key, []byte(hex.EncodeToString(bytes.Repeat([]byte{7}, 32))+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	h := newSealHome(t, hd)
	c := &http.Client{Transport: memberST(md, http.DefaultTransport.(*http.Transport).Clone())}
	_, _, err := post(t, c, h.srv.URL+"/v0/run_targets/sandbox/handles/b1/exec", `{"secrets":{"A":"`+canary+`"}}`, true, "tok")
	if err == nil || !strings.Contains(err.Error(), "chmod 600") || h.wire.has(canary) {
		t.Fatalf("own key fault: %v (clear: %v)", err, h.wire.has(canary))
	}
}
