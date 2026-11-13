package e2bgateway

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
)

// do sends one request to the gateway in process.
func (h *harness) do(t *testing.T, method, target, host string, hdr map[string]string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, target, bytes.NewReader(body))
	if host != "" {
		r.Host = host
	}
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.g.ServeHTTP(w, r)
	return w
}

// frame is one Connect envelope of a server stream.
type frame struct {
	flags byte
	msg   map[string]any
}

func frames(t *testing.T, b []byte) []frame {
	t.Helper()
	var out []frame
	for len(b) > 0 {
		if len(b) < 5 {
			t.Fatalf("short envelope: %q", b)
		}
		n := binary.BigEndian.Uint32(b[1:5])
		var m map[string]any
		if err := json.Unmarshal(b[5:5+n], &m); err != nil {
			t.Fatalf("frame %q: %v", b[5:5+n], err)
		}
		out = append(out, frame{b[0], m})
		b = b[5+n:]
	}
	return out
}

// streamResult reads a Start stream: the end event's exit code, stdout and
// stderr, or the end-of-stream error's code.
func streamResult(t *testing.T, b []byte) (exit int, stdout, stderr, errCode, errMsg string) {
	t.Helper()
	fs := frames(t, b)
	if len(fs) < 2 || fs[0].msg["event"].(map[string]any)["start"] == nil || fs[len(fs)-1].flags != 2 {
		t.Fatalf("stream: %+v", fs)
	}
	exit = -1
	for _, f := range fs[1 : len(fs)-1] {
		ev := f.msg["event"].(map[string]any)
		if d, ok := ev["data"].(map[string]any); ok {
			for k, v := range d {
				raw, _ := base64.StdEncoding.DecodeString(v.(string))
				if k == "stdout" {
					stdout += string(raw)
				} else {
					stderr += string(raw)
				}
			}
		}
		if e, ok := ev["end"].(map[string]any); ok {
			exit = int(e["exitCode"].(float64))
		}
	}
	if e, ok := fs[len(fs)-1].msg["error"].(map[string]any); ok {
		errCode, errMsg = e["code"].(string), e["message"].(string)
	}
	return
}

// TestRecordedRequests replays, route by route, the requests the pinned
// SDKs sent (testdata/recorded.jsonl, recorded by TestRealSDK with
// ZM_E2B_RECORD: e2b 2.63.0 for Python over plain HTTP, for TypeScript over
// TLS and HTTP/2) against a gateway over a refdriver box, and checks each
// answer as the SDK reads it.
func TestRecordedRequests(t *testing.T) {
	h := newHarness(t)
	f, err := os.Open(filepath.Join("testdata", "recorded.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var id, token string
	starts, killed := 0, false
	seen := map[string]bool{}
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		var rec struct {
			Method, URL, Host, Body string
			Header                  map[string]string
		}
		if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
			t.Fatal(err)
		}
		hdr := map[string]string{}
		for k, v := range rec.Header {
			switch v {
			case "<X-Api-Key>":
				v = testKey
			case "<E2b-Sandbox-Id>":
				v = id
			case "<X-Access-Token>":
				v = token
			}
			hdr[k] = v
		}
		url := strings.ReplaceAll(rec.URL, "<id>", id)
		host := strings.ReplaceAll(strings.ReplaceAll(rec.Host, "<id>", id), "<port>", "49982")
		w := h.do(t, rec.Method, url, host, hdr, []byte(rec.Body))
		route := rec.Method + " " + strings.ReplaceAll(strings.SplitN(rec.URL, "?", 2)[0], "<id>", "{id}")
		seen[route] = true
		body := w.Body.Bytes()
		want := map[string]int{"POST /v2/sandboxes": 201, "DELETE /sandboxes/{id}": 204, "POST /sandboxes/{id}/timeout": 204}[route]
		if want == 0 {
			want = 200
		}
		if w.Code != want {
			t.Fatalf("%s: %d %s", route, w.Code, body)
		}
		switch route {
		case "POST /v2/sandboxes":
			var sb map[string]any
			if err := json.Unmarshal(body, &sb); err != nil {
				t.Fatal(err)
			}
			id, _ = sb["sandboxID"].(string)
			token, _ = sb["envdAccessToken"].(string)
			if !strings.HasPrefix(id, "pl") || len(id) != 24 || !strings.HasPrefix(token, "potluck_e2b_") || sb["clientID"] != "potluck" || sb["envdVersion"] != envdVersion || sb["domain"] != "e2b.localhost:49982" || sb["templateID"] != "base" {
				t.Fatalf("create: %s", body)
			}
			starts, killed = 0, false
		case "POST /process.Process/Start":
			exit, out, errOut, code, msg := streamResult(t, body)
			switch starts {
			case 0:
				if exit != 0 || !strings.Contains(out, " ") || code != "" {
					t.Fatalf("uname -a: %d %q %q %s", exit, out, errOut, code)
				}
			case 1:
				if code != "out_of_range" || !strings.Contains(msg, "1048576 byte cap") || exit != -1 {
					t.Fatalf("over the output cap: %d %s %s", exit, code, msg)
				}
			case 2:
				if exit != 3 || errOut != "oops\n" || code != "" {
					t.Fatalf("exit 3: %d %q %s", exit, errOut, code)
				}
			}
			starts++
		case "POST /files":
			var infos []writeInfo
			if err := json.Unmarshal(body, &infos); err != nil || len(infos) != 1 || infos[0].Name != "hello.txt" || infos[0].Type != "file" || !strings.HasSuffix(infos[0].Path, "/hello.txt") {
				t.Fatalf("write: %s", body)
			}
		case "GET /files":
			if string(body) != "hello from zm\n" {
				t.Fatalf("read: %q", body)
			}
		case "GET /sandboxes/{id}":
			var d map[string]any
			if err := json.Unmarshal(body, &d); err != nil || d["sandboxID"] != id || d["state"] != "running" || d["endAt"] == "" {
				t.Fatalf("get: %s", body)
			}
		case "GET /v2/sandboxes":
			var l []map[string]any
			if err := json.Unmarshal(body, &l); err != nil {
				t.Fatal(err)
			}
			found := false
			for _, s := range l {
				found = found || s["sandboxID"] == id
			}
			if found == killed {
				t.Fatalf("list (killed %v): %s", killed, body)
			}
		case "POST /v2/sandboxes/{id}/connect":
			var sb map[string]any
			if err := json.Unmarshal(body, &sb); err != nil || sb["sandboxID"] != id || sb["envdAccessToken"] != token {
				t.Fatalf("connect: %s", body)
			}
		case "DELETE /sandboxes/{id}":
			killed = true
		case "POST /sandboxes/{id}/timeout":
		default:
			t.Fatalf("a recorded route the test does not check: %s", route)
		}
	}
	for _, r := range []string{"POST /v2/sandboxes", "GET /sandboxes/{id}", "GET /v2/sandboxes", "DELETE /sandboxes/{id}",
		"POST /sandboxes/{id}/timeout", "POST /v2/sandboxes/{id}/connect", "POST /process.Process/Start", "POST /files", "GET /files"} {
		if !seen[r] {
			t.Fatalf("no recorded request for %s", r)
		}
	}
}

// create makes a sandbox through the gateway and returns its id and token.
func (h *harness) create(t *testing.T, body string) (string, string) {
	t.Helper()
	w := h.do(t, "POST", "/sandboxes", "", map[string]string{"X-API-Key": testKey, "Content-Type": "application/json"}, []byte(body))
	if w.Code != 201 {
		t.Fatalf("create: %d %s", w.Code, w.Body)
	}
	var sb map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &sb)
	return sb["sandboxID"].(string), sb["envdAccessToken"].(string)
}

func envelope(v any) []byte {
	b, _ := json.Marshal(v)
	var hdr [5]byte
	binary.BigEndian.PutUint32(hdr[1:], uint32(len(b)))
	return append(hdr[:], b...)
}

func (h *harness) run(t *testing.T, id, token, cmd string, extra map[string]string, envs map[string]string) (int, string, string, string) {
	t.Helper()
	hdr := map[string]string{"E2b-Sandbox-Id": id, "X-Access-Token": token, "Content-Type": "application/connect+json"}
	for k, v := range extra {
		hdr[k] = v
	}
	proc := map[string]any{"cmd": "/bin/sh", "args": []string{"-c", cmd}}
	if envs != nil {
		proc["envs"] = envs
	}
	w := h.do(t, "POST", "/process.Process/Start", "", hdr, envelope(map[string]any{"process": proc}))
	if w.Code != 200 {
		t.Fatalf("start: %d %s", w.Code, w.Body)
	}
	exit, out, errOut, code, _ := streamResult(t, w.Body.Bytes())
	return exit, out, errOut, code
}

// TestAuthAndRefusals: no key, a wrong key, a wrong or another sandbox's
// access token, and a host naming another sandbox are refused; what zm
// cannot honour (network rules, pause, volumes, PTY, stdin streaming) is
// refused, never quietly dropped; an unserved route says so.
func TestAuthAndRefusals(t *testing.T) {
	h := newHarness(t)
	for _, k := range []string{"", "e2b_wrong_wrong_wrong_wrong", testKey + "x"} {
		if w := h.do(t, "GET", "/v2/sandboxes", "", map[string]string{"X-API-Key": k}, nil); w.Code != 401 {
			t.Fatalf("key %q: %d", k, w.Code)
		}
	}
	id, token := h.create(t, `{"templateID":"base","timeout":60}`)
	id2, token2 := h.create(t, `{}`)
	for _, tc := range []struct {
		id, token, host string
		want            int
	}{
		{id, "", "", 401},
		{id, "zmt_00", "", 401},
		{id, token2, "", 401},
		{id2, token, "", 401},
		{id, token, envdPort + "-" + id2 + ".e2b.localhost:49982", 400},
		{id, token, "", 204},
		{id, token, envdPort + "-" + id + ".e2b.localhost:49982", 204},
	} {
		w := h.do(t, "GET", "/health", tc.host, map[string]string{"E2b-Sandbox-Id": tc.id, "X-Access-Token": tc.token}, nil)
		if w.Code != tc.want {
			t.Fatalf("%+v: %d %s", tc, w.Code, w.Body)
		}
	}
	for _, body := range []string{`{"allow_internet_access":false}`, `{"network":{"denyOut":["0.0.0.0/0"]}}`, `{"autoPause":true}`,
		`{"volumeMounts":[{"name":"v","path":"/data"}]}`, `{"envVars":{"BAD-NAME":"x"}}`, `{"timeout":-1}`} {
		if w := h.do(t, "POST", "/v2/sandboxes", "", map[string]string{"X-API-Key": testKey}, []byte(body)); w.Code != 400 {
			t.Fatalf("%s: %d %s", body, w.Code, w.Body)
		}
	}
	for _, p := range []string{"/sandboxes/" + id + "/pause", "/templates"} {
		w := h.do(t, "POST", p, "", map[string]string{"X-API-Key": testKey}, nil)
		var e e2bError
		if w.Code != 501 || json.Unmarshal(w.Body.Bytes(), &e) != nil || e.Code != 501 || e.Message == "" {
			t.Fatalf("%s: %d %s", p, w.Code, w.Body)
		}
	}
	hdr := map[string]string{"E2b-Sandbox-Id": id, "X-Access-Token": token, "Content-Type": "application/connect+json"}
	for _, req := range []map[string]any{
		{"process": map[string]any{"cmd": "/bin/sh"}, "pty": map[string]any{"size": map[string]any{"cols": 80, "rows": 24}}},
		{"process": map[string]any{"cmd": "/bin/sh"}, "stdin": true},
	} {
		w := h.do(t, "POST", "/process.Process/Start", "", hdr, envelope(req))
		if w.Code != 400 || !strings.Contains(w.Body.String(), "invalid_argument") {
			t.Fatalf("%v: %d %s", req, w.Code, w.Body)
		}
	}
	w := h.do(t, "POST", "/filesystem.Filesystem/ListDir", "", map[string]string{"E2b-Sandbox-Id": id, "X-Access-Token": token, "Content-Type": "application/json"}, []byte(`{"path":"/"}`))
	if w.Code != 501 || !strings.Contains(w.Body.String(), `"code":"unimplemented"`) {
		t.Fatalf("an unserved RPC: %d %s", w.Code, w.Body)
	}
	// A killed sandbox is gone on both planes.
	if w := h.do(t, "DELETE", "/sandboxes/"+id, "", map[string]string{"X-API-Key": testKey}, nil); w.Code != 204 {
		t.Fatalf("kill: %d %s", w.Code, w.Body)
	}
	if w := h.do(t, "GET", "/sandboxes/"+id, "", map[string]string{"X-API-Key": testKey}, nil); w.Code != 404 {
		t.Fatalf("get after kill: %d %s", w.Code, w.Body)
	}
	if w := h.do(t, "GET", "/health", "", map[string]string{"E2b-Sandbox-Id": id, "X-Access-Token": token}, nil); w.Code != 502 {
		t.Fatalf("envd after kill: %d %s", w.Code, w.Body)
	}
}

// TestCommandsAndFiles: sandbox envs reach commands (the command's own
// win), the client's deadline kills a command with deadline_exceeded, and
// a file larger than one exec's chunk round-trips byte for byte; a missing
// file is 404; a gateway restarted on the same state finds the sandbox and
// accepts its token, but not its metadata.
func TestCommandsAndFiles(t *testing.T) {
	h := newHarness(t)
	id, token := h.create(t, `{"envVars":{"FOO":"from-create","BAR":"b"},"metadata":{"team":"a"}}`)
	if exit, out, _, _ := h.run(t, id, token, `printf '%s %s' "$FOO" "$BAR"`, nil, map[string]string{"FOO": "from-cmd"}); exit != 0 || out != "from-cmd b" {
		t.Fatalf("envs: %d %q", exit, out)
	}
	// A value keeps its spaces, quotes and trailing newlines
	// (TestCommandOfKeepsEnvsOutOfArgv checks it is in no argv).
	const val = "s3cr3t 'quoted' value\n\n"
	if exit, out, errOut, _ := h.run(t, id, token, `printf '[%s]' "$TOKEN"`, nil, map[string]string{"TOKEN": val}); exit != 0 || out != "["+val+"]" {
		t.Fatalf("an env with spaces and newlines: %d %q %q", exit, out, errOut)
	}
	// An env named PATH that hides cat does not stop the others landing.
	if exit, out, errOut, _ := h.run(t, id, token, `printf '%s %s' "$PATH" "$ZETA"`, nil, map[string]string{"PATH": "/nonexistent", "ZETA": "z"}); exit != 0 || out != "/nonexistent z" {
		t.Fatalf("an env PATH without cat: %d %q %q", exit, out, errOut)
	}
	lw := h.do(t, "POST", "/process.Process/Start", "", map[string]string{"E2b-Sandbox-Id": id, "X-Access-Token": token, "Content-Type": "application/connect+json"},
		envelope(map[string]any{"process": map[string]any{"cmd": "/bin/true", "envs": map[string]string{"lower": "x"}}}))
	if !strings.Contains(lw.Body.String(), "not [A-Z][A-Z0-9_]{0,63}") {
		t.Fatalf("a lower-case env name: %d %s", lw.Code, lw.Body)
	}
	began := time.Now()
	if _, _, _, code := h.run(t, id, token, "sleep 20", map[string]string{"Connect-Timeout-Ms": "1000"}, nil); code != "deadline_exceeded" {
		t.Fatalf("deadline: %q", code)
	}
	if time.Since(began) > 15*time.Second {
		t.Fatal("the deadline did not cut the command")
	}
	data := make([]byte, fileChunk*2+12345)
	_, _ = rand.Read(data)
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	part, _ := mw.CreateFormFile("file", "sub/dir/blob.bin")
	_, _ = part.Write(data)
	_ = mw.Close()
	dp := map[string]string{"E2b-Sandbox-Id": id, "X-Access-Token": token}
	w := h.do(t, "POST", "/files", "", map[string]string{"E2b-Sandbox-Id": id, "X-Access-Token": token, "Content-Type": mw.FormDataContentType()}, buf.Bytes())
	if w.Code != 200 || !strings.Contains(w.Body.String(), "/sub/dir/blob.bin") {
		t.Fatalf("write: %d %s", w.Code, w.Body)
	}
	if w := h.do(t, "GET", "/files?path=sub/dir/blob.bin", "", dp, nil); w.Code != 200 || !bytes.Equal(w.Body.Bytes(), data) {
		t.Fatalf("read back: %d, %d bytes of %d", w.Code, w.Body.Len(), len(data))
	}
	oct := map[string]string{"E2b-Sandbox-Id": id, "X-Access-Token": token, "Content-Type": "application/octet-stream"}
	if w := h.do(t, "POST", "/files?path=empty.txt", "", oct, nil); w.Code != 200 {
		t.Fatalf("an empty octet-stream write: %d %s", w.Code, w.Body)
	}
	if w := h.do(t, "GET", "/files?path=empty.txt", "", dp, nil); w.Code != 200 || w.Body.Len() != 0 {
		t.Fatalf("read empty: %d %q", w.Code, w.Body)
	}
	if w := h.do(t, "GET", "/files?path=nope.txt", "", dp, nil); w.Code != 404 {
		t.Fatalf("missing: %d %s", w.Code, w.Body)
	}
	if w := h.do(t, "GET", "/v2/sandboxes?metadata=team%3Db", "", map[string]string{"X-API-Key": testKey}, nil); w.Body.String() != "[]\n" {
		t.Fatalf("metadata filter: %s", w.Body)
	}
	g2, err := newGateway(h.g.cfg, h.owner)
	if err != nil {
		t.Fatal(err)
	}
	h.g = g2
	if w := h.do(t, "GET", "/health", "", dp, nil); w.Code != 204 {
		t.Fatalf("after a restart: %d %s", w.Code, w.Body)
	}
	if w := h.do(t, "GET", "/sandboxes/"+id, "", map[string]string{"X-API-Key": testKey}, nil); w.Code != 200 || !strings.Contains(w.Body.String(), `"metadata":{}`) {
		t.Fatalf("get after a restart: %d %s", w.Code, w.Body)
	}
	if w := h.do(t, "POST", "/sandboxes/"+id+"/timeout", "", map[string]string{"X-API-Key": testKey}, []byte(`{"timeout":7200}`)); w.Code != 400 {
		t.Fatalf("a timeout past ttl_max_s: %d %s", w.Code, w.Body)
	}
}

// TestKeysFile: the keys file must be 0600, keys 20+ bytes and distinct;
// keygen appends one that loads.
func TestKeysFile(t *testing.T) {
	dir, err := os.MkdirTemp("", "zm")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	p := filepath.Join(dir, "keys")
	base := &client.Client{As: "mini/aaaaaaaa"}
	for _, tc := range []struct {
		text string
		mode os.FileMode
		ok   bool
	}{
		{testKey + "\n", 0o644, false},
		{"short\n", 0o600, false},
		{testKey + "\n" + testKey + "\n", 0o600, false},
		{testKey + " tok extra\n", 0o600, false},
		{"# none\n", 0o600, false},
		{testKey + "\n# c\n" + testKey + "2 zmtoken\n", 0o600, true},
	} {
		_ = os.Remove(p)
		if err := os.WriteFile(p, []byte(tc.text), tc.mode); err != nil {
			t.Fatal(err)
		}
		_ = os.Chmod(p, tc.mode)
		keys, err := loadKeys(p, base)
		if (err == nil) != tc.ok {
			t.Fatalf("%q %o: %v", tc.text, tc.mode, err)
		}
		if tc.ok && (len(keys) != 2 || keys[0].c != base || keys[1].c == nil || keys[1].c.Token != "zmtoken" || keys[1].c.As != "" || match(keys, testKey) != keys[0] || match(keys, "x") != nil) {
			t.Fatalf("loaded %+v", keys)
		}
	}
}

// TestTLS: one port answers plain HTTP and TLS; the CA signs a leaf per name
// under the domain only, is name-constrained to it, and HTTP/2 is negotiated.
func TestTLS(t *testing.T) {
	h := newHarness(t)
	tc, err := h.g.tlsConfig("", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tc.GetCertificate(&tls.ClientHelloInfo{ServerName: "api.e2b.app"}); err == nil {
		t.Fatal("signed a name outside the domain")
	}
	pemBytes, err := os.ReadFile(filepath.Join(h.g.cfg.StateDir, "ca.pem"))
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(pemBytes)
	if fi, err := os.Stat(filepath.Join(h.g.cfg.StateDir, "ca-key.pem")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("ca-key.pem: %v %v", fi.Mode(), err)
	}
	if b, _ := os.ReadFile(filepath.Join(h.g.cfg.StateDir, "ca-bundle.pem")); !bytes.HasSuffix(b, pemBytes) {
		t.Fatal("ca-bundle.pem does not end with the CA")
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: h.g, TLSConfig: tc}
	var p http.Protocols
	p.SetHTTP1(true)
	p.SetHTTP2(true)
	srv.Protocols = &p
	go srv.Serve(newSniffListener(l, tc, true))
	defer srv.Close()
	addr := l.Addr().String()
	dial := func(network, _ string) (net.Conn, error) { return net.Dial(network, addr) }
	tr := &http.Transport{Dial: dial, TLSClientConfig: &tls.Config{RootCAs: pool}, ForceAttemptHTTP2: true}
	defer tr.CloseIdleConnections()
	for _, u := range []string{"https://api.e2b.localhost/v2/sandboxes", "https://" + envdPort + "-zmabc.e2b.localhost/health", "http://api.e2b.localhost/v2/sandboxes"} {
		req, _ := http.NewRequest("GET", u, nil)
		req.Header.Set("X-API-Key", testKey)
		resp, err := tr.RoundTrip(req)
		if err != nil {
			t.Fatalf("%s: %v", u, err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if strings.HasPrefix(u, "https") && resp.Proto != "HTTP/2.0" {
			t.Fatalf("%s: %s, want HTTP/2", u, resp.Proto)
		}
		if strings.Contains(u, "/v2/sandboxes") && resp.StatusCode != 200 {
			t.Fatalf("%s: %d", u, resp.StatusCode)
		}
	}
	bad := &tls.Config{RootCAs: pool, ServerName: "api.e2b.app"}
	if c, err := tls.Dial("tcp", addr, bad); err == nil {
		c.Close()
		t.Fatal("a handshake for a name outside the domain succeeded")
	}
}

// TestExecTimeout: the client's Connect deadline becomes exec's timeout;
// none, or one past 600 s, runs to the cap and says so.
func TestExecTimeout(t *testing.T) {
	for _, tc := range []struct {
		hdr  string
		want int
		over bool
	}{{"", 600, true}, {"60000", 60, false}, {"1", 1, false}, {"600000", 600, false}, {"600001", 600, true}, {"junk", 600, true}} {
		if got, over := execTimeout(tc.hdr); got != tc.want || over != tc.over {
			t.Fatalf("%q: %d %v", tc.hdr, got, over)
		}
	}
}

// TestCommandOfKeepsEnvsOutOfArgv: with envs, the argv is the fixed
// wrapper and the command, the values only in secrets; without, the
// command alone and no secrets.
func TestCommandOfKeepsEnvsOutOfArgv(t *testing.T) {
	var req startRequest
	req.Process.Cmd, req.Process.Args = "/bin/sh", []string{"-c", "echo hi"}
	req.Process.Envs = map[string]string{"FOO": "from-cmd", "API_KEY": "k-123"}
	c, err := commandOf(req, map[string]string{"FOO": "from-create", "BAR": "bar-value-9"})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"FOO": "from-cmd", "API_KEY": "k-123", "BAR": "bar-value-9"}
	if len(c.secrets) != len(want) {
		t.Fatalf("secrets %v, want %v", c.secrets, want)
	}
	for k, v := range want {
		if c.secrets[k] != v {
			t.Errorf("secret %s = %q, want %q", k, c.secrets[k], v)
		}
	}
	for _, a := range c.argv {
		for _, v := range want {
			if strings.Contains(a, v) {
				t.Errorf("argv %q carries a value", c.argv)
			}
		}
	}
	if got := strings.Join(c.argv[len(c.argv)-3:], " "); got != "/bin/sh -c echo hi" {
		t.Errorf("argv ends %q, want the command", got)
	}
	req.Process.Envs = nil
	if c, err := commandOf(req, nil); err != nil || c.secrets != nil || strings.Join(c.argv, " ") != "/bin/sh -c echo hi" {
		t.Errorf("no envs: %q %v %v, want the command alone", c.argv, c.secrets, err)
	}
	if _, err := commandOf(req, map[string]string{"NUL": "a\x00b"}); err == nil || strings.Contains(err.Error(), "a\x00b") {
		t.Errorf("an env with a NUL byte: %v, want refused without its value", err)
	}
}

func TestExecErrCode(t *testing.T) {
	for err, want := range map[error]string{
		api.Errorf(api.CodeBadRequest, "x"):  "invalid_argument",
		api.Errorf(api.CodeForbidden, "x"):   "permission_denied",
		api.Errorf(api.CodeUnavailable, "x"): "unavailable",
		io.EOF:                               "unavailable",
	} {
		if got := execErrCode(err); got != want {
			t.Errorf("execErrCode(%v) = %q, want %q", err, got, want)
		}
	}
}
