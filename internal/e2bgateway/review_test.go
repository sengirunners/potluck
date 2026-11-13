package e2bgateway

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"github.com/sengirunners/potluck/computeclient"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

const testKey2 = "e2b_fedcba9876543210fedcba9876543210"

// twoKeys gives the harness testKey and testKey2, both token-less (one zm
// actor), on a fresh gateway over the same state.
func (h *harness) twoKeys(t *testing.T) {
	t.Helper()
	if err := os.WriteFile(h.g.cfg.KeysFile, []byte(testKey+"\n"+testKey2+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.restart(t)
}

func (h *harness) restart(t *testing.T) {
	t.Helper()
	g, err := newGateway(h.g.cfg, h.owner)
	if err != nil {
		t.Fatal(err)
	}
	h.g = g
}

func (h *harness) createAs(t *testing.T, key, idem string) (*httptest.ResponseRecorder, string, string) {
	t.Helper()
	hdr := map[string]string{"X-API-Key": key}
	if idem != "" {
		hdr["Idempotency-Key"] = idem
	}
	w := h.do(t, "POST", "/v2/sandboxes", "", hdr, []byte(`{"timeout":120}`))
	var sb map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &sb)
	id, _ := sb["sandboxID"].(string)
	tok, _ := sb["envdAccessToken"].(string)
	return w, id, tok
}

func ids(t *testing.T, w *httptest.ResponseRecorder) []string {
	t.Helper()
	var l []map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &l); err != nil {
		t.Fatalf("%s: %v", w.Body, err)
	}
	var out []string
	for _, s := range l {
		out = append(out, s["sandboxID"].(string))
	}
	return out
}

// TestKeysOwnTheirBoxesAfterRestart: two keys sharing one zm actor; after a
// gateway restart the first key to list or get sees only its own box, and
// the other key still owns (and reaches) its own.
func TestKeysOwnTheirBoxesAfterRestart(t *testing.T) {
	h := newHarness(t)
	h.twoKeys(t)
	_, idA, _ := h.createAs(t, testKey, "")
	_, idB, tokB := h.createAs(t, testKey2, "")
	h.restart(t)
	if got := ids(t, h.do(t, "GET", "/v2/sandboxes", "", map[string]string{"X-API-Key": testKey}, nil)); len(got) != 1 || got[0] != idA {
		t.Fatalf("A lists %v, want only %s", got, idA)
	}
	if w := h.do(t, "GET", "/sandboxes/"+idB, "", map[string]string{"X-API-Key": testKey}, nil); w.Code != 404 {
		t.Fatalf("A gets B's box: %d %s", w.Code, w.Body)
	}
	if w := h.do(t, "GET", "/sandboxes/"+idB, "", map[string]string{"X-API-Key": testKey2}, nil); w.Code != 200 || !strings.Contains(w.Body.String(), tokB) {
		t.Fatalf("B gets its box: %d %s", w.Code, w.Body)
	}
	if w := h.do(t, "GET", "/health", "", map[string]string{"E2b-Sandbox-Id": idB, "X-Access-Token": tokB}, nil); w.Code != 204 {
		t.Fatalf("B's token after a restart: %d %s", w.Code, w.Body)
	}
	if got := ids(t, h.do(t, "GET", "/v2/sandboxes", "", map[string]string{"X-API-Key": testKey2}, nil)); len(got) != 1 || got[0] != idB {
		t.Fatalf("B lists %v, want only %s", got, idB)
	}
}

// TestIdempotencyKeyPerAPIKey: the same Idempotency-Key from two keys makes
// two boxes, a repeat answers the first, and a repeat after the box ended is
// refused, never the dead id.
func TestIdempotencyKeyPerAPIKey(t *testing.T) {
	h := newHarness(t)
	h.twoKeys(t)
	_, a1, _ := h.createAs(t, testKey, "same")
	_, b1, _ := h.createAs(t, testKey2, "same")
	if a1 == "" || b1 == "" || a1 == b1 {
		t.Fatalf("A %s, B %s", a1, b1)
	}
	if _, a2, _ := h.createAs(t, testKey, "same"); a2 != a1 {
		t.Fatalf("A's repeat: %s, want %s", a2, a1)
	}
	if w := h.do(t, "GET", "/sandboxes/"+b1, "", map[string]string{"X-API-Key": testKey2}, nil); w.Code != 200 {
		t.Fatalf("B lost its box to A's repeat: %d %s", w.Code, w.Body)
	}
	if w := h.do(t, "DELETE", "/sandboxes/"+a1, "", map[string]string{"X-API-Key": testKey}, nil); w.Code != 204 {
		t.Fatalf("kill: %d", w.Code)
	}
	if w, id, _ := h.createAs(t, testKey, "same"); w.Code != 409 || id != "" {
		t.Fatalf("a repeat after the box ended: %d %s", w.Code, w.Body)
	}
}

type failReader struct{ err error }

func (f failReader) Read([]byte) (int, error) { return 0, f.err }

// TestUploadFailures: an upload of exactly one chunk lands whole; a read
// error at the chunk boundary or mid-chunk, or a body past its limit,
// fails (400, 413) and leaves neither the file nor a temp file.
func TestUploadFailures(t *testing.T) {
	h := newHarness(t)
	id, token := h.create(t, `{}`)
	dp := map[string]string{"E2b-Sandbox-Id": id, "X-Access-Token": token}
	exact := bytes.Repeat([]byte("z"), fileChunk)
	oct := map[string]string{"E2b-Sandbox-Id": id, "X-Access-Token": token, "Content-Type": "application/octet-stream"}
	if w := h.do(t, "POST", "/files?path=up/exact.bin", "", oct, exact); w.Code != 200 {
		t.Fatalf("exactly one chunk: %d %s", w.Code, w.Body)
	}
	if w := h.do(t, "GET", "/files?path=up/exact.bin", "", dp, nil); !bytes.Equal(w.Body.Bytes(), exact) {
		t.Fatalf("read back %d bytes", w.Body.Len())
	}
	s, _, err := h.g.lookup(context.Background(), h.g.keys[0], id)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("POST", "/files", nil)
	for _, tc := range []struct {
		name string
		at   int
		err  error
		want int
	}{
		{"at the boundary", fileChunk, errors.New("connection reset"), 400},
		{"mid-chunk", fileChunk + 100, io.ErrUnexpectedEOF, 400},
		{"past the limit at the boundary", fileChunk, &http.MaxBytesError{Limit: 1}, 413},
		{"in the first chunk", 10, errors.New("boom"), 400},
	} {
		data := io.MultiReader(bytes.NewReader(bytes.Repeat([]byte("q"), tc.at)), failReader{tc.err})
		if _, status, err := h.g.putFile(r, s, "up/bad.bin", data); err == nil || status != tc.want {
			t.Fatalf("%s: %d %v", tc.name, status, err)
		}
		if w := h.do(t, "GET", "/files?path=up/bad.bin", "", dp, nil); w.Code != 404 {
			t.Fatalf("%s: a failed upload left the file: %d", tc.name, w.Code)
		}
		if _, out, _, _ := h.run(t, id, token, "ls -A up", nil, nil); out != "exact.bin\n" {
			t.Fatalf("%s: left in up/: %q", tc.name, out)
		}
	}
}

// TestGzipUploadCap: fileMax bounds the decompressed upload.
func TestGzipUploadCap(t *testing.T) {
	h := newHarness(t)
	id, token := h.create(t, `{}`)
	defer func(n int) { fileMax = n }(fileMax)
	fileMax = 1000
	gz := func(n int) []byte {
		var b bytes.Buffer
		zw := gzip.NewWriter(&b)
		_, _ = zw.Write(bytes.Repeat([]byte("a"), n))
		_ = zw.Close()
		return b.Bytes()
	}
	hdr := map[string]string{"E2b-Sandbox-Id": id, "X-Access-Token": token, "Content-Type": "application/octet-stream", "Content-Encoding": "gzip"}
	if w := h.do(t, "POST", "/files?path=big.txt", "", hdr, gz(5000)); w.Code != 413 {
		t.Fatalf("past the cap decompressed: %d %s", w.Code, w.Body)
	}
	if w := h.do(t, "POST", "/files?path=ok.txt", "", hdr, gz(1000)); w.Code != 200 {
		t.Fatalf("at the cap: %d %s", w.Code, w.Body)
	}
	dp := map[string]string{"E2b-Sandbox-Id": id, "X-Access-Token": token}
	if w := h.do(t, "GET", "/files?path=big.txt", "", dp, nil); w.Code != 404 {
		t.Fatalf("a refused upload left the file: %d", w.Code)
	}
	if w := h.do(t, "GET", "/files?path=ok.txt", "", dp, nil); w.Body.Len() != 1000 {
		t.Fatalf("ok.txt: %d bytes", w.Body.Len())
	}
}

// TestEndedBoxesForgotten: a box ended behind the gateway's back is 404 and
// dropped from its memory, on get and on list.
func TestEndedBoxesForgotten(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	end := func(id string) {
		h.g.mu.Lock()
		s := h.g.sandboxes[id]
		h.g.mu.Unlock()
		if _, err := computeclient.ReleaseRunTarget(ctx, h.owner, s.target, s.hid); err != nil {
			t.Fatal(err)
		}
	}
	known := func(id string) bool { h.g.mu.Lock(); defer h.g.mu.Unlock(); return h.g.sandboxes[id] != nil }
	id1, _ := h.create(t, `{}`)
	id2, _ := h.create(t, `{}`)
	end(id1)
	if w := h.do(t, "GET", "/sandboxes/"+id1, "", map[string]string{"X-API-Key": testKey}, nil); w.Code != 404 || known(id1) {
		t.Fatalf("get of an ended box: %d, still known %v", w.Code, known(id1))
	}
	end(id2)
	if got := ids(t, h.do(t, "GET", "/v2/sandboxes", "", map[string]string{"X-API-Key": testKey}, nil)); len(got) != 0 || known(id2) {
		t.Fatalf("list after an end: %v, still known %v", got, known(id2))
	}
}

// TestDomainAndAddr: --domain must make a real name constraint; plain HTTP
// only on loopback.
func TestDomainAndAddr(t *testing.T) {
	for d, ok := range map[string]bool{"e2b.localhost": true, "e2b.example.com": true, "": false, "localhost": false,
		".localhost": false, "e2b..localhost": false, "e2b.localhost.": false, "E2B.localhost": false, "-a.localhost": false, "a_b.localhost": false} {
		if err := validDomain(d); (err == nil) != ok {
			t.Fatalf("%q: %v", d, err)
		}
	}
	for _, tc := range []struct {
		host    string
		tlsOnly bool
		ok      bool
	}{{"localhost", false, true}, {"127.0.0.1", false, true}, {"::1", false, true}, {"0.0.0.0", false, false}, {"", false, false},
		{"10.0.0.5", false, false}, {"e2b.example.com", false, false}, {"0.0.0.0", true, true}} {
		if err := checkAddr(tc.host, tc.tlsOnly); (err == nil) != tc.ok {
			t.Fatalf("%+v: %v", tc, err)
		}
	}
}

// TestTLSOnlyRefusesPlain: with plain off, a plain HTTP request gets no answer.
func TestTLSOnlyRefusesPlain(t *testing.T) {
	h := newHarness(t)
	tc, err := h.g.tlsConfig("", "")
	if err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: h.g, TLSConfig: tc}
	go srv.Serve(newSniffListener(l, tc, false))
	defer srv.Close()
	c := &http.Client{Timeout: 5 * time.Second}
	if resp, err := c.Get("http://" + l.Addr().String() + "/v2/sandboxes"); err == nil {
		resp.Body.Close()
		t.Fatalf("plain HTTP answered %d with --tls-only", resp.StatusCode)
	}
}
