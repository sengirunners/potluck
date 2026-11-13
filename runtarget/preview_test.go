package runtarget_test

import (
	"bufio"
	"context"
	"fmt"
	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/computeclient"
	"github.com/sengirunners/potluck/conformance/refdriver"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/server"
	"github.com/sengirunners/potluck/protocol"
	"github.com/sengirunners/potluck/runtarget"
	"github.com/sengirunners/potluck/wiring"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSandboxPortPreview(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cfg, err := config.LoadFrom(home)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Human, cfg.Machine = "judy", "mini"
	cfg.File.Tunnel.URL = "https://api.example.test"
	cfg.File.Tunnel.PreviewDomain = "example.test"
	var ingress http.Handler
	dm, err := server.New(context.Background(), cfg, nil, func(d *core.Deps) ([]core.Module, error) {
		mods, err := wiring.Compute(d)
		ingress = core.PublicIngress(d).Wrap(http.NotFoundHandler())
		return mods, err
	})
	if err != nil {
		t.Fatal(err)
	}
	defer dm.Close()
	fix, _ := filepath.EvalSymlinks(t.TempDir())
	state := filepath.Join(fix, "state")
	driver, err := refdriver.Install(fix, state, "")
	if err != nil {
		t.Fatal(err)
	}
	defer refdriver.KillAll(state)
	for _, m := range dm.Modules() {
		if s, ok := m.(*runtarget.Service); ok {
			s.SetLookPath(func(string) (string, error) { return driver, nil })
		}
	}
	ctx := context.Background()
	owner := client.WithHandler(dm.SocketHandler())
	owner.Project, owner.ProjectFrom = "acme", "flag"
	if _, err := owner.CreateProject(ctx, api.NewProjectRequest{Slug: "acme"}); err != nil {
		t.Fatal(err)
	}
	mint := func(as string) *client.Client {
		tok, err := owner.MintToken(ctx, api.MintTokenRequest{As: as, Project: "acme", Role: "member"})
		if err != nil {
			t.Fatal(err)
		}
		c := client.WithHandler(dm.Handler)
		c.Token, c.Project, c.ProjectFrom = tok.Secret, "acme", "flag"
		return c
	}
	ana, bo := mint("ana"), mint("bo")
	if _, err := computeclient.BindRunTarget(ctx, owner, computeapi.BindRunTargetRequest{Target: computeapi.RunTarget{Name: "box", Driver: "ref", Kind: "box", Isolation: "ephemeral", Images: []string{refPin}}}); err != nil {
		t.Fatal(err)
	}
	h, err := computeclient.AcquireRunTarget(ctx, ana, "box", computeapi.AcquireRunTargetRequest{RequestID: "preview", TTLS: 120})
	if err != nil {
		t.Fatal(err)
	}
	id := idOf(h.Handle)
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/socket" {
			conn, rw, err := w.(http.Hijacker).Hijack()
			if err != nil {
				return
			}
			defer conn.Close()
			fmt.Fprint(rw, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n")
			rw.Flush()
			b := make([]byte, 4)
			for {
				if _, err := io.ReadFull(rw, b); err != nil {
					return
				}
				rw.Write(b)
				rw.Flush()
			}
		}
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, "/asset.js?x=1", 302)
			return
		}
		fmt.Fprintf(w, "%s %s", r.Host, r.URL.RequestURI())
	}))
	defer app.Close()
	u, _ := url.Parse(app.URL)
	_, portText, _ := net.SplitHostPort(u.Host)
	var port int
	fmt.Sscan(portText, &port)
	if _, err := computeclient.DialRunTargetPort(ctx, bo, "box", id, port); err == nil {
		t.Fatal("other holder opened port")
	}
	for _, p := range []int{0, 65536} {
		if _, err := computeclient.DialRunTargetPort(ctx, ana, "box", id, p); code(err) != api.CodeBadRequest {
			t.Fatalf("invalid port %d: %v", p, err)
		}
	}
	if _, err := computeclient.PublishRunTargetPreview(ctx, ana, "box", id, computeapi.PreviewRequest{Port: port}); code(err) != api.CodeBadRequest {
		t.Fatalf("implicit public exposure: %v", err)
	}
	c, err := computeclient.DialRunTargetPort(ctx, ana, "box", id, port)
	if err != nil {
		t.Fatalf("private port: %v", err)
	}
	fmt.Fprintf(c, "GET /private HTTP/1.1\r\nHost: sandbox\r\nConnection: close\r\n\r\n")
	privateResp, err := http.ReadResponse(bufio.NewReader(c), &http.Request{Method: "GET"})
	if err != nil {
		t.Fatalf("private response: %v", err)
	}
	io.Copy(io.Discard, privateResp.Body)
	privateResp.Body.Close()
	c.Close()
	// A peer can finish its response while continuing to consume upload.
	// Read EOF must leave the shared producer's write direction authorized.
	halfListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer halfListener.Close()
	upload := strings.Repeat("u", protocol.PortPage+1)
	halfDone := make(chan error, 1)
	go func() {
		peer, err := halfListener.Accept()
		if err != nil {
			halfDone <- err
			return
		}
		defer peer.Close()
		peer.SetDeadline(time.Now().Add(5 * time.Second))
		_, err = peer.Write([]byte("done"))
		if err == nil {
			err = peer.(*net.TCPConn).CloseWrite()
		}
		if err == nil {
			b := make([]byte, len(upload))
			_, err = io.ReadFull(peer, b)
			if err == nil && string(b) != upload {
				err = io.ErrUnexpectedEOF
			}
		}
		halfDone <- err
	}()
	half, err := computeclient.DialRunTargetPort(ctx, ana, "box", id, halfListener.Addr().(*net.TCPAddr).Port)
	if err != nil {
		t.Fatal(err)
	}
	defer half.Close()
	reply, err := io.ReadAll(half)
	if err != nil || string(reply) != "done" {
		t.Fatalf("half-close read: %q %v", reply, err)
	}
	if _, err := io.WriteString(half, upload); err != nil {
		t.Fatalf("read EOF revoked upload direction: %v", err)
	}
	if err := <-halfDone; err != nil {
		t.Fatal(err)
	}
	half.Close()
	var channel protocol.PortResult
	portPath := "/v0/run_targets/box/handles/" + id + "/port"
	if err := ana.Do(ctx, "POST", portPath, protocol.PortRequest{Op: "open", Port: port}, &channel); err != nil {
		t.Fatal(err)
	}
	defer ana.Do(ctx, "POST", portPath, protocol.PortRequest{Op: "close", Stream: channel.Stream}, nil)
	other, err := computeclient.AcquireRunTarget(ctx, ana, "box", computeapi.AcquireRunTargetRequest{RequestID: "other", TTLS: 120})
	if err != nil {
		t.Fatal(err)
	}
	if err := ana.Do(ctx, "POST", "/v0/run_targets/box/handles/"+idOf(other.Handle)+"/port", protocol.PortRequest{Op: "read", Stream: channel.Stream}, nil); code(err) != api.CodeNotFound {
		t.Fatalf("cross-box stream: %v", err)
	}
	if err := bo.Do(ctx, "POST", portPath, protocol.PortRequest{Op: "read", Stream: channel.Stream}, nil); err == nil {
		t.Fatal("other holder read stream")
	}
	preview, err := computeclient.PublishRunTargetPreview(ctx, ana, "box", id, computeapi.PreviewRequest{Port: port, Public: true})
	if err != nil {
		t.Fatal(err)
	}
	public := httptest.NewServer(ingress)
	defer public.Close()
	fetch := func(path string) *http.Response {
		t.Helper()
		req, _ := http.NewRequest("GET", public.URL+path, nil)
		pu, _ := url.Parse(preview.URL)
		req.Host = pu.Host
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	resp := fetch("/asset.js?x=1")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(body), "/asset.js?x=1") || !strings.Contains(string(body), preview.ID+".example.test") {
		t.Fatalf("origin/path: %d %s", resp.StatusCode, body)
	}
	list, err := computeclient.RunTargetPreviews(ctx, ana, "box", id)
	if err != nil || len(list.Previews) != 1 {
		t.Fatalf("list: %+v %v", list, err)
	}
	if err := computeclient.RevokeRunTargetPreview(ctx, bo, "box", id, preview.ID); err == nil {
		t.Fatal("other holder revoked preview")
	}
	// Raw upgrade proves the same generic byte transport preserves the 101
	// handshake and then carries binary frames without buffering the stream.
	tcp, err := net.DialTimeout("tcp", strings.TrimPrefix(public.URL, "http://"), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer tcp.Close()
	tcp.SetDeadline(time.Now().Add(10 * time.Second))
	pu, _ := url.Parse(preview.URL)
	fmt.Fprintf(tcp, "GET /socket HTTP/1.1\r\nHost: %s\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n", pu.Host)
	reader := bufio.NewReader(tcp)
	upgrade, err := http.ReadResponse(reader, &http.Request{Method: "GET"})
	if err != nil || upgrade.StatusCode != 101 {
		t.Fatalf("upgrade: %v %v", upgrade, err)
	}
	tcp.Write([]byte{0, 255, 1, 128})
	echo := make([]byte, 4)
	if _, err := io.ReadFull(reader, echo); err != nil || echo[1] != 255 {
		t.Fatalf("upgrade bytes %v %v", echo, err)
	}
	if err := computeclient.RevokeRunTargetPreview(ctx, ana, "box", id, preview.ID); err != nil {
		t.Fatal(err)
	}
	tcp.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := reader.ReadByte(); err == nil {
		t.Fatal("revoked upgraded stream still open")
	} else if e, ok := err.(net.Error); ok && e.Timeout() {
		t.Fatal("revocation did not close upgraded stream")
	}
	resp = fetch("/")
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("revoked URL: %d", resp.StatusCode)
	}
	if _, err := computeclient.ReleaseRunTarget(ctx, ana, "box", id); err != nil {
		t.Fatal(err)
	}
	if _, err := computeclient.DialRunTargetPort(ctx, ana, "box", id, port); err == nil {
		t.Fatal("released lease opened channel")
	}
	if err := ana.Do(ctx, "POST", portPath, protocol.PortRequest{Op: "read", Stream: channel.Stream}, nil); err == nil {
		t.Fatal("released lease read old channel")
	}
	kept, err := computeclient.AcquireRunTarget(ctx, ana, "box", computeapi.AcquireRunTargetRequest{RequestID: "restart-preview", TTLS: 120, Persist: true})
	if err != nil {
		t.Fatal(err)
	}
	keptID := idOf(kept.Handle)
	preview, err = computeclient.PublishRunTargetPreview(ctx, ana, "box", keptID, computeapi.PreviewRequest{Port: port, Public: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := computeclient.StopRunTarget(ctx, ana, "box", keptID); err != nil {
		t.Fatal(err)
	}
	if _, err := computeclient.StartRunTarget(ctx, ana, "box", keptID, 120); err != nil {
		t.Fatal(err)
	}
	resp = fetch("/")
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("old preview survived a new lease on the reused handle: %d", resp.StatusCode)
	}
	preview, err = computeclient.PublishRunTargetPreview(ctx, ana, "box", keptID, computeapi.PreviewRequest{Port: port, Public: true})
	if err != nil {
		t.Fatal(err)
	}
	resp = fetch("/")
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("new lease cannot publish: %d", resp.StatusCode)
	}
	if _, err := computeclient.ReleaseRunTarget(ctx, ana, "box", keptID); err != nil {
		t.Fatal(err)
	}
	short, err := computeclient.AcquireRunTarget(ctx, ana, "box", computeapi.AcquireRunTargetRequest{RequestID: "expires", TTLS: 1})
	if err != nil {
		t.Fatal(err)
	}
	shortID := idOf(short.Handle)
	preview, err = computeclient.PublishRunTargetPreview(ctx, ana, "box", shortID, computeapi.PreviewRequest{Port: port, Public: true})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(1200 * time.Millisecond)
	resp = fetch("/")
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("expired preview: %d", resp.StatusCode)
	}
	if _, err := computeclient.DialRunTargetPort(ctx, ana, "box", shortID, port); err == nil {
		t.Fatal("expired lease opened port")
	}

}
