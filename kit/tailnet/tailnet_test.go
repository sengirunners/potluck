package tailnet

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"tailscale.com/tailcfg"
	"tailscale.com/types/key"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/events"
	"github.com/sengirunners/potluck/kit/project"
	"github.com/sengirunners/potluck/kit/store"
)

func init() {
	pollEvery = 5 * time.Millisecond
	upWait = 2 * time.Second
	sweepEvery = time.Hour
	peersFresh = 0
	probeTimeout = time.Second
}

// fakeNode is a tailnet node with no network: Listen is loopback TCP, WhoIs
// answers from whoFn, and HTTPClient dials tailnet names through dial.
type fakeNode struct {
	mu      sync.Mutex
	cfg     NodeConfig
	state   string
	authURL string
	peers   []NodePeer
	whoFn   func(remoteAddr string) (Who, error)
	dial    map[string]string // tailnet host:port -> loopback addr
	addr    string            // the API listener
	ln      net.Listener
	lnErr   error // Listen fails with this when set
	started bool
	starts  int
	closed  bool
}

func newFakeNode() *fakeNode {
	return &fakeNode{state: "NeedsLogin", dial: map[string]string{},
		whoFn: func(string) (Who, error) { return Who{}, errors.New("no whois configured") }}
}

func (n *fakeNode) Start() error {
	n.mu.Lock()
	n.started, n.closed = true, false
	n.starts++
	n.mu.Unlock()
	return nil
}
func (n *fakeNode) Status(context.Context) (NodeStatus, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return NodeStatus{State: n.state, AuthURL: n.authURL, Suffix: "example.ts.net",
		SelfDNS: n.cfg.Hostname + ".example.ts.net", SelfIP: "100.64.0.1", Peers: n.peers}, nil
}
func (n *fakeNode) WhoIs(_ context.Context, addr string) (Who, error) {
	n.mu.Lock()
	f := n.whoFn
	n.mu.Unlock()
	return f(addr)
}
func (n *fakeNode) Listen(network, _ string) (net.Listener, error) {
	n.mu.Lock()
	lnErr := n.lnErr
	n.mu.Unlock()
	if lnErr != nil {
		return nil, lnErr
	}
	ln, err := net.Listen(network, "127.0.0.1:0")
	if err == nil {
		n.set(func(n *fakeNode) { n.addr, n.ln = ln.Addr().String(), ln })
	}
	return ln, err
}
func (n *fakeNode) HTTPClient() *http.Client {
	return &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		n.mu.Lock()
		to, ok := n.dial[addr]
		n.mu.Unlock()
		if !ok {
			return nil, errors.New("no route to " + addr)
		}
		var d net.Dialer
		return d.DialContext(ctx, network, to)
	}}}
}
func (n *fakeNode) Close() error            { n.mu.Lock(); n.closed = true; n.mu.Unlock(); return nil }
func (n *fakeNode) set(f func(n *fakeNode)) { n.mu.Lock(); f(n); n.mu.Unlock() }
func (n *fakeNode) get() fakeNode {
	n.mu.Lock()
	defer n.mu.Unlock()
	return fakeNode{cfg: n.cfg, addr: n.addr, ln: n.ln, started: n.started, starts: n.starts, closed: n.closed}
}

type env struct {
	s     *Service
	d     *core.Deps
	node  *fakeNode
	local *client.Client // the machine owner on the socket
	home  string
}

// tlsFake exposes the HTTPS listener separately from the ordinary API door.
// TLS itself is supplied by tsnet; the fake exercises lifecycle and routing.
type tlsFake struct {
	*fakeNode
	addr   chan string
	cert   tls.Certificate
	tlsErr error
}

func (n *tlsFake) ListenTLS(network, addr string) (net.Listener, error) {
	if n.tlsErr != nil {
		return nil, n.tlsErr
	}
	if network != "tcp" || addr != ":443" {
		return nil, errors.New("unexpected TLS address")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err == nil {
		n.addr <- ln.Addr().String()
	}
	if err != nil {
		return nil, err
	}
	return tls.NewListener(ln, &tls.Config{Certificates: []tls.Certificate{n.cert}, MinVersion: tls.VersionTLS12}), nil
}

func TestHTTPKeepsServingWhenHTTPSDisabled(t *testing.T) {
	e := newEnv(t, "")
	n := &tlsFake{fakeNode: e.node, addr: make(chan string, 1), tlsErr: errors.New("enable HTTPS certificates")}
	e.s.NewNode = func(c NodeConfig) Node { e.node.set(func(n *fakeNode) { n.cfg = c }); return n }
	e.node.set(func(n *fakeNode) { n.state = StateRunning })
	if _, err := e.s.Up(context.Background(), api.TailnetUpRequest{}); err != nil {
		t.Fatal(err)
	}
	st := e.waitRunning(t)
	if st.HTTPSURL != "" {
		t.Fatalf("disabled HTTPS advertised: %+v", st)
	}
	resp, err := http.Get("http://" + e.node.get().addr + "/v0/test/who")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("HTTP authentication: %d", resp.StatusCode)
	}
}

func TestHTTPSDoorLifecycle(t *testing.T) {
	e := newEnv(t, "")
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "potluck-mini.example.ts.net"}, DNSNames: []string{"potluck-mini.example.ts.net"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(cert)
	n := &tlsFake{fakeNode: e.node, addr: make(chan string, 1), cert: tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}}
	e.s.NewNode = func(c NodeConfig) Node { e.node.set(func(n *fakeNode) { n.cfg = c }); return n }
	e.node.set(func(n *fakeNode) { n.state = StateRunning })
	if _, err := e.s.Up(context.Background(), api.TailnetUpRequest{}); err != nil {
		t.Fatal(err)
	}
	e.waitRunning(t)
	addr := <-n.addr
	deadline := time.Now().Add(time.Second)
	st := e.s.Status(context.Background())
	for st.HTTPSURL == "" && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
		st = e.s.Status(context.Background())
	}
	if st.HTTPSURL != "https://potluck-mini.example.ts.net" {
		t.Fatalf("HTTPS URL = %q", st.HTTPSURL)
	}
	hc := &http.Client{Timeout: time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, ServerName: "potluck-mini.example.ts.net", MinVersion: tls.VersionTLS12}}}
	resp, err := hc.Get("https://" + addr + "/v0/test/who")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("network request without credentials: %d", resp.StatusCode)
	}
	e.s.Down(context.Background())
	if _, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
		t.Fatal("HTTPS listener remained open after tailnet down")
	}
}

// newEnv wires the tailnet service on machine "mini" over real core pieces
// (store, registry, events, Authenticate, the guarded router) plus one test
// route that reports the authenticated caller. configJSON, if set, is
// written to config.json first.
func newEnv(t *testing.T, configJSON string) *env {
	t.Helper()
	home, err := os.MkdirTemp("", "zm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	if configJSON != "" {
		if err := os.WriteFile(filepath.Join(home, config.FileConfig), []byte(configJSON), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv(config.EnvMachine, "mini")
	t.Setenv(config.EnvHuman, "ana")
	t.Setenv("TS_AUTHKEY", "")
	cfg, err := config.LoadFrom(home)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	st, err := store.Open(ctx, home)
	if err != nil {
		t.Fatal(err)
	}
	reg, err := project.Open(ctx, st, cfg)
	if err != nil {
		t.Fatal(err)
	}
	bus, err := events.New(ctx, st, cfg.Machine)
	if err != nil {
		t.Fatal(err)
	}
	d := &core.Deps{Config: cfg, Store: st, Projects: reg, Events: bus,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Started: time.Now()}
	e := &env{d: d, node: newFakeNode(), home: home}
	s := New(d)
	s.NewNode = func(c NodeConfig) Node { e.node.set(func(n *fakeNode) { n.cfg = c }); return e.node }
	e.s = s

	r := core.NewRouter()
	s.Routes(r)
	r.HandleFunc("GET /v0/test/who", core.Authed, "test", func(w http.ResponseWriter, r *http.Request) {
		sub := core.SubjectOf(r.Context())
		api.WriteJSON(w, 200, api.Whoami{As: sub.Address, Identity: sub.Identity, Via: sub.Via})
	})
	d.Handler = http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set(api.HeaderProtocol, api.Protocol)
		r.ServeHTTP(w, req.WithContext(d.Authenticate(req)))
	})
	// The Unix listener's mark, as server.Daemon.SocketHandler applies it
	// (test only: production code never calls core.WithLocal outside the server).
	e.local = client.WithHandler(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		d.Handler.ServeHTTP(w, req.WithContext(core.WithLocal(req.Context())))
	}))
	t.Cleanup(func() {
		s.Close()
		st.Close()
	})
	return e
}

func (e *env) remote(t *testing.T) *client.Client {
	t.Helper()
	addr := e.node.get().addr
	if addr == "" {
		t.Fatal("tailnet listener not up")
	}
	// The listener is on loopback here; a tailnet client names the node.
	return client.NewRemote("http://"+addr, &http.Client{Timeout: 5 * time.Second, Transport: hostAs{"zm-mini:7433"}})
}

// hostAs sends every request with this Host, as a client naming the node by
// its tailnet name would (core.OwnTailnetHost).
type hostAs struct{ host string }

func (h hostAs) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Host = h.host
	return http.DefaultTransport.RoundTrip(r)
}

func (e *env) waitRunning(t *testing.T) api.TailnetStatus {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		st := e.s.Status(context.Background())
		if st.Running {
			return st
		}
		if time.Now().After(deadline) {
			t.Fatalf("node never served: %+v", st)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (e *env) tailnetEvents(t *testing.T) []string {
	t.Helper()
	evs, err := e.d.Events.Since(context.Background(), authz.System("mini"), "", events.Filter{Types: []string{"dev.zm.tailnet."}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, ev := range evs {
		out = append(out, strings.TrimPrefix(ev.Type, "dev.zm.tailnet."))
	}
	return out
}

func savedTailnet(t *testing.T, home string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(home, config.FileConfig))
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Tailnet map[string]any `json:"tailnet"`
	}
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatal(err)
	}
	return f.Tailnet
}

func code(err error) string {
	var ae *api.Error
	if errors.As(err, &ae) {
		return ae.Code
	}
	if err == nil {
		return "ok"
	}
	return "transport: " + err.Error()
}

func TestSingleMachineStartsNoNode(t *testing.T) {
	e := newEnv(t, "")
	e.s.NewNode = func(NodeConfig) Node { t.Error("single-machine mode started a tailnet node"); return newFakeNode() }
	ctx := context.Background()
	if err := e.s.Start(ctx); err != nil {
		t.Fatal(err)
	}
	st, err := e.local.Tailnet(ctx)
	if err != nil || st.Enabled || st.Running || st.URL != "" {
		t.Fatalf("status %+v %v", st, err)
	}
	if ps, err := e.local.Peers(ctx); err != nil || len(ps) != 0 {
		t.Fatalf("peers %v %v", ps, err)
	}
	if _, err := os.Stat(filepath.Join(e.home, StateDir)); !os.IsNotExist(err) {
		t.Fatalf("tsnet state dir created in single-machine mode: %v", err)
	}
	if err := e.s.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestStartFromConfig(t *testing.T) {
	e := newEnv(t, `{"tailnet": {"enabled": true, "hostname": "zm-mini2", "port": 7444}}`)
	if err := e.s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	e.node.set(func(n *fakeNode) { n.state = StateRunning })
	st := e.waitRunning(t)
	if st.URL != "http://zm-mini2.example.ts.net:7444" || st.Hostname != "zm-mini2" {
		t.Fatalf("status %+v", st)
	}
	if got := e.node.get().cfg.Dir; got != filepath.Join(e.home, StateDir) {
		t.Fatalf("state dir %q", got)
	}
	if fi, err := os.Stat(filepath.Join(e.home, StateDir)); err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("state dir mode %v %v", fi, err)
	}
	e.s.Close()
	if !e.node.get().closed {
		t.Fatal("Close left the node open")
	}
}

// TestTailnetDoor is the boundary test: requests on the tailnet listener are
// authenticated by core from the WhoIs login (mapped identity) or a token,
// never as the owner, and local routes stay closed to them.
func TestTailnetDoor(t *testing.T) {
	e := newEnv(t, `{"agents": {"claude": ["claude", "--model", "opus"]}}`)
	ctx := context.Background()
	owner := e.d.Projects.Local("")
	if _, err := e.d.Projects.Create(ctx, owner, api.NewProjectRequest{Slug: "side"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.d.Projects.AddIdentity(ctx, api.NewIdentityRequest{Name: "carl", TailscaleLogin: "carl@example.com"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.d.Projects.Grant(ctx, owner, api.GrantRequest{Identity: "carl", Role: "member", Project: "side"}); err != nil {
		t.Fatal(err)
	}
	tok, err := e.d.Projects.Mint(ctx, owner, api.MintTokenRequest{As: "victor", Project: "side", Role: "guest"})
	if err != nil {
		t.Fatal(err)
	}

	// Up: waits for a login, saves enabled (never the auth key), keeps other keys.
	e.node.set(func(n *fakeNode) { n.authURL = "https://login.example/a1" })
	st, err := e.local.TailnetUp(ctx, api.TailnetUpRequest{AuthKey: "tskey-secret"})
	if err != nil || st.AuthURL != "https://login.example/a1" || st.Running || !st.Enabled || st.Hostname != "potluck-mini" {
		t.Fatalf("up %+v %v", st, err)
	}
	if got := e.node.get().cfg; got.AuthKey != "tskey-secret" || got.Hostname != "potluck-mini" {
		t.Fatalf("node config %+v", got)
	}
	b, _ := os.ReadFile(filepath.Join(e.home, config.FileConfig))
	if strings.Contains(string(b), "tskey") || !strings.Contains(string(b), `"opus"`) {
		t.Fatalf("config.json:\n%s", b)
	}
	if tn := savedTailnet(t, e.home); tn["enabled"] != true {
		t.Fatalf("saved tailnet %v", tn)
	}
	e.node.set(func(n *fakeNode) { n.state = StateRunning })
	st = e.waitRunning(t)
	if st.URL != "http://potluck-mini.example.ts.net:7433" || st.Tailnet != "example.ts.net" || st.AuthURL != "" {
		t.Fatalf("running %+v", st)
	}
	if _, err := e.local.TailnetUp(ctx, api.TailnetUpRequest{Hostname: "zm-other"}); code(err) != api.CodeConflict {
		t.Fatalf("up with another hostname: %v", err)
	}

	rc := e.remote(t)
	login := func(w Who, err error) {
		e.node.set(func(n *fakeNode) { n.whoFn = func(string) (Who, error) { return w, err } })
	}
	who := func(c *client.Client) (api.Whoami, error) {
		var w api.Whoami
		return w, c.Do(ctx, "GET", "/v0/test/who", nil, &w)
	}

	// A mapped login is that identity, never the owner.
	var seenAddr string
	e.node.set(func(n *fakeNode) {
		n.whoFn = func(a string) (Who, error) {
			seenAddr = a
			return Who{Login: "carl@example.com", Node: "host-b-laptop"}, nil
		}
	})
	w, err := who(rc)
	if err != nil || w.Identity.Name != "carl" || w.Via != authz.ViaTailnet || w.As != "human:carl" {
		t.Fatalf("mapped login: %+v %v", w, err)
	}
	if host, _, _ := net.SplitHostPort(seenAddr); host != "127.0.0.1" {
		t.Fatalf("WhoIs got remote addr %q", seenAddr)
	}
	// Local routes (tailnet control, peers) are the owner's on the socket only.
	for _, p := range []string{"/v0/tailnet", "/v0/peers"} {
		if err := rc.Do(ctx, "GET", p, nil, nil); code(err) != api.CodeForbidden {
			t.Fatalf("mapped login GET %s: %v", p, err)
		}
	}
	if err := rc.Do(ctx, "POST", "/v0/tailnet/down", nil, nil); code(err) != api.CodeForbidden {
		t.Fatalf("mapped login down: %v", err)
	}
	// Zm-As cannot pick the owner's address.
	asOwner := *rc
	asOwner.As = "human:ana"
	if _, err := who(&asOwner); code(err) != api.CodeForbidden {
		t.Fatalf("Zm-As owner over the tailnet: %v", err)
	}

	// Unknown logins, tagged nodes (no login) and failed WhoIs: 401.
	for name, c := range map[string]struct {
		w   Who
		err error
	}{
		"unmapped": {Who{Login: "mallory@example.com"}, nil},
		"tagged":   {Who{Node: "ci-runner"}, nil},
		"whois":    {Who{}, errors.New("no such peer")},
	} {
		login(c.w, c.err)
		if _, err := who(rc); code(err) != api.CodeUnauthenticated {
			t.Fatalf("%s: %v", name, err)
		}
		// A token works whatever WhoIs said, and is the token's identity.
		tc := *rc
		tc.Token = tok.Secret
		if w, err := who(&tc); err != nil || w.Identity.Name != "victor" || w.Via != authz.ViaToken {
			t.Fatalf("%s + token: %+v %v", name, w, err)
		}
	}

	// Down: saved, listener closed, events recorded.
	st, err = e.local.TailnetDown(ctx)
	if err != nil || st.Enabled || st.Running {
		t.Fatalf("down %+v %v", st, err)
	}
	if tn := savedTailnet(t, e.home); tn["enabled"] == true {
		t.Fatalf("saved after down %v", tn)
	}
	if !e.node.get().closed {
		t.Fatal("down left the node open")
	}
	var ae *api.Error
	if _, err := who(rc); err == nil || errors.As(err, &ae) {
		t.Fatalf("listener still answers after down: %v", err)
	}
	if got := strings.Join(e.tailnetEvents(t), ","); got != "up,down" {
		t.Fatalf("events %q", got)
	}
}

func TestUpFailureNotSaved(t *testing.T) {
	e := newEnv(t, "")
	ctx := context.Background()
	e.s.NewNode = func(NodeConfig) Node { return failNode{newFakeNode()} }
	if _, err := e.local.TailnetUp(ctx, api.TailnetUpRequest{}); code(err) != api.CodeUnavailable {
		t.Fatalf("up: %v", err)
	}
	// Not enabled; only the node's name, recorded before the node starts
	// (rename step 2), is in config.json.
	if c, err := config.LoadFrom(e.home); err != nil || c.File.Tailnet.Enabled || c.File.Tailnet.Hostname != "potluck-mini" {
		t.Fatalf("config after a failed up: %+v %v", c.File.Tailnet, err)
	}
	if st, _ := e.local.Tailnet(ctx); st.Error == "" || st.Running {
		t.Fatalf("status after failure %+v", st)
	}
	if _, err := e.local.TailnetUp(ctx, api.TailnetUpRequest{Hostname: "Bad Name"}); code(err) != api.CodeBadRequest {
		t.Fatalf("bad hostname: %v", err)
	}
}

// A hostname without the zm- prefix would make this daemon invisible to
// every other daemon's zm peers: refused, from the request and from config.
func TestUpRefusesNonZmHostname(t *testing.T) {
	e := newEnv(t, "")
	ctx := context.Background()
	e.s.NewNode = func(NodeConfig) Node { t.Error("started a node named outside zm-<machine>"); return newFakeNode() }
	for _, h := range []string{"mini", "zm-"} {
		_, err := e.local.TailnetUp(ctx, api.TailnetUpRequest{Hostname: h})
		var ae *api.Error
		if !errors.As(err, &ae) || ae.Code != api.CodeBadRequest || !strings.Contains(ae.Hint, "potluck-mini") {
			t.Fatalf("up --hostname %s: %v", h, err)
		}
	}
	e.d.Config.File.Tailnet.Hostname = "studio"
	if _, err := e.local.TailnetUp(ctx, api.TailnetUpRequest{}); code(err) != api.CodeBadRequest {
		t.Fatalf("up with config hostname studio: %v", err)
	}
	if _, err := os.Stat(filepath.Join(e.home, config.FileConfig)); !os.IsNotExist(err) {
		t.Fatalf("config saved after a refused up: %v", err)
	}
}

// up while waiting for a login, now with an auth key (the usual recovery:
// first up printed a URL, the user minted a key): the node restarts with the
// key. Once logged in, a key is refused with a reason, never dropped.
func TestUpWithAuthKeyWhileWaitingForLogin(t *testing.T) {
	e := newEnv(t, "")
	ctx := context.Background()
	e.node.set(func(n *fakeNode) { n.authURL = "https://login.example/a1" })
	if st, err := e.local.TailnetUp(ctx, api.TailnetUpRequest{}); err != nil || st.AuthURL == "" {
		t.Fatalf("first up %+v %v", st, err)
	}
	if _, err := e.local.TailnetUp(ctx, api.TailnetUpRequest{AuthKey: "tskey-new"}); err != nil {
		t.Fatalf("up with key: %v", err)
	}
	if got := e.node.get(); got.cfg.AuthKey != "tskey-new" || got.starts != 2 {
		t.Fatalf("node not restarted with the key: key %q starts %d", got.cfg.AuthKey, got.starts)
	}
	// Same hostname, no key, still waiting: a no-op, no restart.
	if _, err := e.local.TailnetUp(ctx, api.TailnetUpRequest{}); err != nil || e.node.get().starts != 2 {
		t.Fatalf("plain up restarted the node: %v starts %d", err, e.node.get().starts)
	}
	e.node.set(func(n *fakeNode) { n.state = StateRunning })
	e.waitRunning(t)
	_, err := e.local.TailnetUp(ctx, api.TailnetUpRequest{AuthKey: "tskey-other"})
	var ae *api.Error
	if !errors.As(err, &ae) || ae.Code != api.CodeConflict || !strings.Contains(ae.Hint, "down") {
		t.Fatalf("up with key while logged in: %v", err)
	}
	if got := e.node.get(); got.cfg.AuthKey != "tskey-new" || got.starts != 2 {
		t.Fatalf("logged-in node touched: %+v", got.cfg)
	}
}

// The listener fails: up is not a 200, nothing is saved, and the next up
// starts the node over instead of reporting the dead runner as fine. Same
// when a serving listener dies later.
func TestUpRestartsAfterListenerFailure(t *testing.T) {
	e := newEnv(t, "")
	ctx := context.Background()
	e.node.set(func(n *fakeNode) { n.state, n.lnErr = StateRunning, errors.New("port in use") })
	_, err := e.local.TailnetUp(ctx, api.TailnetUpRequest{})
	var ae *api.Error
	if !errors.As(err, &ae) || ae.Code != api.CodeUnavailable || !strings.Contains(ae.Message, "port in use") {
		t.Fatalf("up with a failing listener: %v", err)
	}
	// Not enabled; only the new node's name is recorded (rename step 2:
	// it keeps that name once its state exists).
	if c, err := config.LoadFrom(e.home); err != nil || c.File.Tailnet.Enabled || c.File.Tailnet.Hostname != "potluck-mini" {
		t.Fatalf("config after a failed up: %+v %v", c.File.Tailnet, err)
	}
	if st, _ := e.local.Tailnet(ctx); st.Running || !strings.Contains(st.Error, "port in use") {
		t.Fatalf("status %+v", st)
	}
	// Retrying with the same failure still fails (and restarts).
	if _, err := e.local.TailnetUp(ctx, api.TailnetUpRequest{}); code(err) != api.CodeUnavailable {
		t.Fatalf("second up: %v", err)
	}
	if got := e.node.get().starts; got != 2 {
		t.Fatalf("second up did not restart the node: starts %d", got)
	}
	// The cause is fixed: the next up serves.
	e.node.set(func(n *fakeNode) { n.lnErr = nil })
	st, err := e.local.TailnetUp(ctx, api.TailnetUpRequest{})
	if err != nil || !st.Running || st.Error != "" {
		t.Fatalf("third up %+v %v", st, err)
	}
	if tn := savedTailnet(t, e.home); tn["enabled"] != true {
		t.Fatalf("saved tailnet %v", tn)
	}
	// The serving listener dies: status says so and up restarts.
	e.node.get().ln.Close()
	deadline := time.Now().Add(3 * time.Second)
	for st, _ = e.local.Tailnet(ctx); st.Running || st.Error == ""; st, _ = e.local.Tailnet(ctx) {
		if time.Now().After(deadline) {
			t.Fatalf("listener death not reported: %+v", st)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if st, err := e.local.TailnetUp(ctx, api.TailnetUpRequest{}); err != nil || !st.Running || e.node.get().starts != 4 {
		t.Fatalf("up after the listener died %+v %v starts %d", st, err, e.node.get().starts)
	}
}

type failNode struct{ *fakeNode }

func (failNode) Start() error { return errors.New("control server unreachable") }

// healthServer is another daemon's tailnet listener answering /v0/health.
func healthServer(t *testing.T, protocol, version string) *httptest.Server {
	t.Helper()
	return healthServerAs(t, protocol, version, "")
}

func healthServerAs(t *testing.T, protocol, version, machine string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(api.HeaderProtocol, protocol)
		if r.URL.Path != "/v0/health" {
			api.WriteError(w, api.Errorf(api.CodeNotFound, "no route"))
			return
		}
		api.WriteJSON(w, 200, api.Health{OK: true, Protocol: protocol, Version: version, Machine: machine})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestPeersDiscovery(t *testing.T) {
	e := newEnv(t, "")
	ctx := context.Background()
	studio := healthServer(t, api.Protocol, "v9")
	future := healthServer(t, "zm/v1", "v99")
	e.node.set(func(n *fakeNode) {
		n.state = StateRunning
		n.peers = []NodePeer{
			{Host: "zm-studio", DNSName: "zm-studio.example.ts.net", Online: true, Login: "carl@example.com"},
			{Host: "zm-future", DNSName: "zm-future.example.ts.net", Online: true},
			{Host: "Example-Mini", DNSName: "zm-mini.example.ts.net", Online: true}, // ourselves
			{Host: "laptop", DNSName: "laptop.example.ts.net", Online: true},        // not a zm node
			{Host: "zm-gone", DNSName: "zm-gone.example.ts.net", Online: false},     // never answered
			{Host: "zm-quiet", DNSName: "zm-quiet.example.ts.net", Online: true},    // no daemon
		}
		n.dial["zm-studio.example.ts.net:7433"] = studio.Listener.Addr().String()
		n.dial["zm-future.example.ts.net:7433"] = future.Listener.Addr().String()
	})
	if _, err := e.local.TailnetUp(ctx, api.TailnetUpRequest{}); err != nil {
		t.Fatal(err)
	}
	e.waitRunning(t)
	// Serving begins before startup discovery finishes. Wait for that sweep
	// before capturing LastSeen, so its later publication cannot race ours.
	deadline := time.Now().Add(3 * time.Second)
	for {
		e.s.peerMu.Lock()
		swept := !e.s.lastSweep.IsZero()
		e.s.peerMu.Unlock()
		if swept {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("initial peer sweep did not finish")
		}
		time.Sleep(5 * time.Millisecond)
	}
	ps, err := e.local.Peers(ctx)
	if err != nil || len(ps) != 1 {
		t.Fatalf("peers %+v %v", ps, err)
	}
	p := ps[0]
	if p.Machine != "studio" || !p.Online || p.Version != "v9" || p.Owner != "carl@example.com" ||
		p.URL != "http://zm-studio.example.ts.net:7433" || p.LastSeen.IsZero() {
		t.Fatalf("studio %+v", p)
	}
	// It stops answering: listed offline with its last sighting.
	studio.Close()
	ps, _ = e.local.Peers(ctx)
	if len(ps) != 1 || ps[0].Online || !ps[0].LastSeen.Equal(p.LastSeen) || ps[0].Version != "v9" {
		t.Fatalf("after studio stopped %+v", ps)
	}
	// It leaves the tailnet: still listed, offline.
	e.node.set(func(n *fakeNode) { n.peers = nil })
	if ps, _ = e.local.Peers(ctx); len(ps) != 1 || ps[0].Online {
		t.Fatalf("after studio left %+v", ps)
	}
	// Down: every peer offline, the list survives.
	if _, err := e.local.TailnetDown(ctx); err != nil {
		t.Fatal(err)
	}
	if ps, _ = e.local.Peers(ctx); len(ps) != 1 || ps[0].Online {
		t.Fatalf("after down %+v", ps)
	}
}

// The machine name comes from the peer's health when it says one, else from
// its own hostname (never the de-duplicated DNS name). Two nodes claiming one
// machine collapse to the answering one.
func TestPeersMachineNames(t *testing.T) {
	e := newEnv(t, "")
	ctx := context.Background()
	named := healthServerAs(t, api.Protocol, "v9", "studio")
	dup := healthServer(t, api.Protocol, "v9")
	e.node.set(func(n *fakeNode) {
		n.state = StateRunning
		n.peers = []NodePeer{
			// Tailscale de-duplicated the DNS name; HostName is the node's own.
			{Host: "zm-studio", DNSName: "zm-studio-1.example.ts.net", Online: true},
			// Health names the machine: it wins over the hostname.
			{Host: "zm-desk", DNSName: "zm-desk.example.ts.net", Online: true},
			// A stale node with the same name that does not answer.
			{Host: "zm-studio", DNSName: "zm-studio.example.ts.net", Online: false},
		}
		n.dial["zm-studio-1.example.ts.net:7433"] = dup.Listener.Addr().String()
		n.dial["zm-desk.example.ts.net:7433"] = named.Listener.Addr().String()
	})
	if _, err := e.local.TailnetUp(ctx, api.TailnetUpRequest{}); err != nil {
		t.Fatal(err)
	}
	e.waitRunning(t)
	ps, err := e.local.Peers(ctx)
	if err != nil || len(ps) != 1 {
		t.Fatalf("peers %+v %v", ps, err)
	}
	// zm-desk says it is studio; zm-studio-1 (also studio) answers too:
	// one entry, online, the lower URL.
	if p := ps[0]; p.Machine != "studio" || !p.Online || p.URL != "http://zm-desk.example.ts.net:7433" {
		t.Fatalf("studio %+v", p)
	}
	// Without zm-desk: the de-duplicated node is still "studio", not "studio-1".
	e.node.set(func(n *fakeNode) { n.peers = n.peers[:1] })
	ps, _ = e.local.Peers(ctx)
	if len(ps) != 1 || ps[0].Machine != "studio" || !ps[0].Online || ps[0].URL != "http://zm-studio-1.example.ts.net:7433" {
		t.Fatalf("de-duplicated node %+v", ps)
	}
}

func TestWhoFrom(t *testing.T) {
	user := &tailcfg.UserProfile{LoginName: "carl@example.com"}
	phone := &tailcfg.Node{ComputedName: "iphone171", Hostinfo: (&tailcfg.Hostinfo{Hostname: "localhost", OS: "iOS"}).View()}
	if w := whoFrom(phone, user); w.OS != "iOS" || w.Host != "localhost" {
		t.Fatalf("phone: %+v", w)
	}
	if w := whoFrom(&tailcfg.Node{ComputedName: "host-b-laptop"}, user); w.Login != "carl@example.com" || w.Node != "host-b-laptop" {
		t.Fatalf("user node %+v", w)
	}
	tagged := &tailcfg.Node{ComputedName: "ci", Tags: []string{"tag:ci"}}
	if w := whoFrom(tagged, &tailcfg.UserProfile{LoginName: "tagged-devices"}); w.Login != "" || w.Node != "ci" {
		t.Fatalf("tagged node %+v", w)
	}
	if w := whoFrom(nil, nil); w.Login != "" || w.Node != "" {
		t.Fatalf("empty %+v", w)
	}
	// The device itself (approvals bind the decide right to it).
	nk := key.NewNode().Public()
	w := whoFrom(&tailcfg.Node{ComputedName: "phone", StableID: "nPHONE", Key: nk}, user)
	if w.NodeID != "nPHONE" || w.NodeKey != nk.String() || w.Tagged {
		t.Fatalf("device identity %+v", w)
	}
	if w := whoFrom(tagged, nil); !w.Tagged {
		t.Fatalf("tagged not marked: %+v", w)
	}
	// The node's own hostname survives a rename of its tailnet name.
	renamed := &tailcfg.Node{ComputedName: "build-box", Hostinfo: (&tailcfg.Hostinfo{Hostname: "zm-studio"}).View()}
	if w := whoFrom(renamed, user); w.Host != "zm-studio" || w.Node != "build-box" {
		t.Fatalf("renamed node %+v", w)
	}
}

// fromHost: a caller whose address is one of this machine's is this
// machine; unreadable host addresses fail closed.
func TestFromHost(t *testing.T) {
	s := &Service{HostAddrs: func() ([]netip.Addr, error) { return []netip.Addr{netip.MustParseAddr("100.64.0.9")}, nil }}
	for addr, want := range map[string]bool{"100.64.0.9:4000": true, "100.64.0.10:4000": false, "127.0.0.1:1": true,
		"[::1]:1": true, "garbage": true} {
		if got := s.fromHost(addr); got != want {
			t.Errorf("fromHost(%q) = %v, want %v", addr, got, want)
		}
	}
	s.HostAddrs = func() ([]netip.Addr, error) { return nil, errors.New("no interfaces") }
	if !s.fromHost("100.64.0.10:4000") {
		t.Fatal("unreadable host addresses did not fail closed")
	}
}

func TestMachineFromHost(t *testing.T) {
	for host, want := range map[string]string{"zm-studio": "studio", "ZM-Studio": "studio", "laptop": "", "zm-": "", "zm-a b": ""} {
		m, ok := machineFromHost(host)
		if (want == "") == ok || (ok && m != want) {
			t.Errorf("%q -> %q %v, want %q", host, m, ok, want)
		}
	}
}

// A parked daemon (kit/server term.go) runs the tailnet with no store,
// registry or event log: the node comes up and goes down, and its up and
// down are published nowhere instead of failing.
func TestParkedDaemonsTailnetHasNoEventLog(t *testing.T) {
	home, err := os.MkdirTemp("", "zm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	if err := os.WriteFile(filepath.Join(home, config.FileConfig), []byte(`{"tailnet": {"enabled": true, "hostname": "zm-mini2"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(config.EnvMachine, "mini")
	t.Setenv(config.EnvHuman, "ana")
	t.Setenv("TS_AUTHKEY", "")
	cfg, err := config.LoadFrom(home)
	if err != nil {
		t.Fatal(err)
	}
	d := &core.Deps{Config: cfg, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Started: time.Now(),
		Handler: http.NotFoundHandler()}
	node := newFakeNode()
	e := &env{d: d, node: node, home: home, s: New(d)}
	e.s.NewNode = func(c NodeConfig) Node { node.set(func(n *fakeNode) { n.cfg = c }); return node }
	if err := e.s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	node.set(func(n *fakeNode) { n.state = StateRunning })
	e.waitRunning(t)
	// Its health tells the owner this run joins the tailnet (kit/server).
	if self, err := e.s.Self(context.Background()); err != nil || !self.Enabled {
		t.Fatalf("Self of a node that came up: %+v %v", self, err)
	}
	e.s.Close()
	if !node.get().closed {
		t.Fatal("Close left the node open")
	}
}
