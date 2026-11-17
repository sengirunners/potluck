package e2bgateway

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// wantSDK is what testdata/sdk_check.{py,mjs} print against a working gateway.
const wantSDK = `uname exit 0 out True
read hello from zm
info True running
listed True
connect True
cap True
exit 3 oops
killed True
`

// TestRealSDK runs the unmodified E2B SDKs against the gateway over a real
// listener. It is skipped unless one of these is set (install the SDKs
// yourself: `pip install e2b`, `npm install e2b`):
//
//	ZM_E2B_SDK_PYTHON  a python with e2b installed
//	ZM_E2B_SDK_NODE    a directory whose node_modules holds e2b (needs node on PATH)
//	ZM_E2B_SDK_PLAIN=1 Python over plain HTTP with the deprecated E2B_API_URL and
//	                   E2B_SANDBOX_URL (macOS: the Python SDK trusts only the keychain)
//	ZM_E2B_RECORD      a file to append each request the SDK sent, as JSON lines
func TestRealSDK(t *testing.T) {
	py, nodeDir := os.Getenv("ZM_E2B_SDK_PYTHON"), os.Getenv("ZM_E2B_SDK_NODE")
	if py == "" && nodeDir == "" {
		t.Skip("set ZM_E2B_SDK_PYTHON or ZM_E2B_SDK_NODE to run the real E2B SDKs")
	}
	h := newHarness(t)
	l4, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l4.Addr().(*net.TCPAddr).Port
	ls := []net.Listener{l4}
	if l6, err := net.Listen("tcp", net.JoinHostPort("::1", itoa(port))); err == nil {
		ls = append(ls, l6) // *.localhost resolves to ::1 first on macOS
	}
	h.g.cfg.Advertise = "e2b.localhost:" + itoa(port)
	tc, err := h.g.tlsConfig("", "")
	if err != nil {
		t.Fatal(err)
	}
	var handler http.Handler = h.g
	if rec := os.Getenv("ZM_E2B_RECORD"); rec != "" {
		handler = recorder(t, rec, h.g)
	}
	srv := &http.Server{Handler: handler, TLSConfig: tc}
	var p http.Protocols
	p.SetHTTP1(true)
	p.SetHTTP2(true)
	srv.Protocols = &p
	for _, l := range ls {
		go srv.Serve(newSniffListener(l, tc, true))
	}
	t.Cleanup(func() { srv.Close() })
	state := h.g.cfg.StateDir
	env := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + h.home, "E2B_API_KEY=" + testKey,
		"E2B_DOMAIN=" + h.g.cfg.Advertise}
	run := func(name string, cmd *exec.Cmd) {
		t.Helper()
		var out bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &out
		done := make(chan error, 1)
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		go func() { done <- cmd.Wait() }()
		select {
		case err = <-done:
		case <-time.After(3 * time.Minute):
			_ = cmd.Process.Kill()
			err = <-done
		}
		t.Logf("%s:\n%s", name, out.String())
		if err != nil || !strings.EqualFold(out.String(), wantSDK) { // Python prints True, Node true
			t.Fatalf("%s: %v; want output\n%s", name, err, wantSDK)
		}
	}
	if py != "" {
		cmd := exec.Command(py, "-I", filepath.Join(mustAbs(t, "testdata"), "sdk_check.py"))
		cmd.Dir = t.TempDir()
		cmd.Env = append(env, "SSL_CERT_FILE="+filepath.Join(state, "ca-bundle.pem"))
		if os.Getenv("ZM_E2B_SDK_PLAIN") == "1" {
			base := "http://127.0.0.1:" + itoa(port)
			cmd.Env = append(env, "E2B_API_URL="+base, "E2B_SANDBOX_URL="+base)
		}
		run("python", cmd)
	}
	if nodeDir != "" {
		script := filepath.Join(nodeDir, "sdk_check.mjs") // beside node_modules, so `import 'e2b'` resolves
		b, err := os.ReadFile(filepath.Join("testdata", "sdk_check.mjs"))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(script, b, 0o644); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("node", script)
		cmd.Dir = nodeDir
		cmd.Env = append(env, "NODE_EXTRA_CA_CERTS="+filepath.Join(state, "ca.pem"))
		run("node", cmd)
	}
}

func itoa(n int) string { return strconv.Itoa(n) }

func mustAbs(t *testing.T, p string) string {
	a, err := filepath.Abs(p)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// recorder appends each request (method, path, the headers the SDK sets,
// body) to file as a JSON line: how testdata/recorded.json was made.
func recorder(t *testing.T, file string, next http.Handler) http.Handler {
	var mu sync.Mutex
	f, err := os.OpenFile(file, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(body))
		hdr := map[string]string{}
		for _, k := range []string{"Content-Type", "Connect-Protocol-Version", "Connect-Timeout-Ms", "Content-Encoding", "Idempotency-Key", "E2b-Sandbox-Port", "Keepalive-Ping-Interval", "Authorization", "User-Agent"} {
			if v := r.Header.Get(k); v != "" {
				hdr[k] = v
			}
		}
		for _, k := range []string{"X-Api-Key", "E2b-Sandbox-Id", "X-Access-Token"} {
			if r.Header.Get(k) != "" {
				hdr[k] = "<" + k + ">"
			}
		}
		mu.Lock()
		_ = json.NewEncoder(f).Encode(map[string]any{"method": r.Method, "url": r.URL.RequestURI(), "host": r.Host, "proto": r.Proto, "header": hdr, "body": string(body)})
		mu.Unlock()
		next.ServeHTTP(w, r)
	})
}
