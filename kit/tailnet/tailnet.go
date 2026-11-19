// Package tailnet is the optional network door: an embedded Tailscale node
// (tsnet) that serves this daemon's API to the tailnet, and the list of
// other zm nodes on it. Owned by the tailnet builder. See SPEC.md
// section 13 "Remote access (tailnet)".
//
// Single-machine mode never starts Tailscale: the node starts only when
// config.json says tailnet.enabled or the owner runs `zm tailnet up`.
//
// The tailnet listener is the ONLY hard security boundary in v0. Every
// request it accepts is passed to d.Handler with
//
//	core.WithRemote(ctx, core.Remote{Login: <WhoIs login>, Node: <WhoIs node>})
//
// and core authenticates it (bearer token, else the WhoIs login mapped to a
// registry identity, else 401); nothing in this package decides who may do
// what. The listener never marks a request local. v0 forwards nothing
// between daemons (no peer mail).
package tailnet

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/events"
)

const (
	// DefaultPort is the tailnet listener's port.
	DefaultPort = 7433
	// HostPrefix starts a new node's tailnet hostname: potluck-<machine>. A
	// node made before the rename's step 2 is HostPrefixOld + machine
	// (zm-<machine>) and keeps that name (nodeName). Peer discovery looks for
	// both.
	HostPrefix    = api.NodePrefixNew
	HostPrefixOld = api.NodePrefix
	// MaxNodeName is a DNS label's length: a node's MagicDNS name is one.
	MaxNodeName = 63
	// StateDir is the tsnet state directory inside $ZM_HOME.
	StateDir = "tsnet"
)

// Tunables (tests shorten them).
var (
	pollEvery    = 500 * time.Millisecond // login wait loop
	upWait       = 10 * time.Second       // how long tailnet/up waits for Running or a login URL
	sweepEvery   = 30 * time.Second       // peer refresh while serving
	peersFresh   = 5 * time.Second        // GET /v0/peers reuses a sweep this recent
	probeTimeout = 3 * time.Second
)

// Service implements the /v0/tailnet and /v0/peers routes.
type Service struct {
	d *core.Deps
	// NewNode creates the tailnet node. Default: tsnet. Tests use a fake.
	NewNode func(NodeConfig) Node
	// HostAddrs lists this machine's own IP addresses (a caller from one of
	// them is this machine: core.Remote.Self). Default: the interfaces.
	HostAddrs func() ([]netip.Addr, error)

	ctx    context.Context // ends at Close
	cancel context.CancelFunc
	wg     sync.WaitGroup

	// sealNonces are the nonces of bodies sealed to this daemon over the
	// tailnet that it opened (seal.go): each opens once.
	sealNonces nonceSet

	ctl     sync.Mutex // serializes up, down and close
	mu      sync.Mutex
	enabled bool
	run     *runner // nil while the node is down
	lastErr string

	peerMu    sync.Mutex
	peers     map[string]api.Peer // by machine
	lastSweep time.Time
}

// runner is one running tailnet node.
type runner struct {
	node    Node
	cfg     NodeConfig
	cancel  context.CancelFunc
	done    chan struct{}
	tlsDone chan struct{}
	serving atomic.Bool
	https   atomic.Bool
	// failed is set when serve gave up (listen or serve error). The node
	// stays open for Status; the next up replaces the runner.
	failed atomic.Bool
}

var (
	_ core.Module  = (*Service)(nil)
	_ core.Starter = (*Service)(nil)
	_ core.Closer  = (*Service)(nil)
)

// New returns the tailnet service. It starts nothing.
func New(d *core.Deps) *Service {
	ctx, cancel := context.WithCancel(context.Background())
	return &Service{d: d, NewNode: defaultNode, ctx: ctx, cancel: cancel, peers: map[string]api.Peer{}}
}

// Routes registers /v0/tailnet/* and /v0/peers. All are local: only the
// machine's owner on its socket controls or inspects the network door.
func (s *Service) Routes(r *core.Router) {
	r = r.For("tailnet")
	r.HandleFunc("GET /v0/tailnet", core.Local, "local: node status", s.handleStatus)
	r.HandleFunc("POST /v0/tailnet/up", core.Local, "local: join the tailnet (saves config)", s.handleUp)
	r.HandleFunc("POST /v0/tailnet/down", core.Local, "local: leave", s.handleDown)
	r.HandleFunc("GET /v0/peers", core.Local, "local: "+config.Program+" nodes on the tailnet", s.handlePeers)
}

// Start brings the node up when config says tailnet.enabled. It never
// blocks: tsnet starts in the background and the login URL goes to the log.
func (s *Service) Start(ctx context.Context) error {
	s.mu.Lock()
	on := s.d.Config.File.Tailnet.Enabled
	s.mu.Unlock()
	if !on {
		return nil
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		if _, err := s.Up(s.ctx, api.TailnetUpRequest{}); err != nil {
			s.d.Log.Warn("tailnet: start", "err", err)
		}
	}()
	return nil
}

// Close shuts the node down. It does not change config.
func (s *Service) Close() error {
	s.cancel()
	s.wg.Wait()
	s.stopNode()
	return nil
}

func (s *Service) port() int {
	s.mu.Lock()
	p := s.d.Config.File.Tailnet.Port
	s.mu.Unlock()
	if p > 0 {
		return p
	}
	return DefaultPort
}

// Up starts the node if it is not running and returns its status without
// waiting for a login. It does not save config (the route does).
func (s *Service) Up(ctx context.Context, req api.TailnetUpRequest) (api.TailnetStatus, error) {
	if err := s.up(req); err != nil {
		return s.Status(ctx), err
	}
	return s.Status(ctx), nil
}

func (s *Service) up(req api.TailnetUpRequest) error {
	host := strings.ToLower(strings.TrimSpace(req.Hostname))
	if host != "" && !api.ValidName(host) {
		return api.Errorf(api.CodeBadRequest, "bad tailnet hostname %q", req.Hostname).WithHint("lowercase letters, digits and dashes")
	}
	// ctl serializes up and down, so a restart below cannot race another up.
	s.ctl.Lock()
	defer s.ctl.Unlock()
	s.mu.Lock()
	if s.ctx.Err() != nil {
		s.mu.Unlock()
		return api.Errorf(api.CodeUnavailable, "the daemon is shutting down")
	}
	cfg := s.d.Config
	name, fresh := host, false
	// derived: the name came from nodeName with none on record; it is
	// recorded before the node starts, so the rule cannot pick another name
	// for this node later (tsnet state present, no recorded name).
	derived := name == "" && strings.TrimSpace(cfg.File.Tailnet.Hostname) == ""
	if name == "" {
		name, fresh = nodeName(cfg)
	}
	if fresh && len(name) > MaxNodeName { // a new name only: an existing node keeps the name it has
		s.mu.Unlock()
		return api.Errorf(api.CodeBadRequest, "tailnet node name %q is %d characters; a DNS name's label holds at most %d", name, len(name), MaxNodeName).
			WithHint(fmt.Sprintf("give this machine a name of at most %d characters (config.json \"machine\"), or pass --hostname %s<a shorter name>", MaxNodeName-len(HostPrefix), HostPrefix))
	}
	nc := NodeConfig{
		Dir:        cfg.Path(StateDir),
		Hostname:   name,
		AuthKey:    strings.TrimSpace(req.AuthKey),
		ControlURL: first(req.ControlURL, cfg.File.Tailnet.ControlURL),
		Logf: func(format string, args ...any) {
			s.d.Log.Info("tailnet: " + strings.TrimSpace(fmt.Sprintf(format, args...)))
		},
	}
	if _, ok := machineFromHost(nc.Hostname); !ok {
		s.mu.Unlock()
		return api.Errorf(api.CodeBadRequest, "tailnet hostname %q does not start with %q", nc.Hostname, HostPrefix).
			WithHint(config.Command("peers") + " finds other daemons by their potluck-<machine> (or older zm-<machine>) node name; use e.g. --hostname " + HostPrefix + cfg.Machine)
	}
	if r := s.run; r != nil {
		switch {
		case nc.Hostname != r.cfg.Hostname || nc.ControlURL != r.cfg.ControlURL:
			s.mu.Unlock()
			return api.Errorf(api.CodeConflict, "already on the tailnet as %s", r.cfg.Hostname).
				WithHint("run " + config.Command("tailnet down") + " first to change the hostname or control server")
		case r.failed.Load():
			// The listener died (serve set lastErr): start over, never a no-op.
		case nc.AuthKey != "":
			if st, err := r.node.Status(s.ctx); err == nil && st.State == StateRunning {
				s.mu.Unlock()
				return api.Errorf(api.CodeConflict, "already logged in to the tailnet as %s; the auth key was not used", r.cfg.Hostname).
					WithHint("run " + config.Command("tailnet down") + " first to use a new auth key")
			}
			// Still waiting for a login: restart the node with the new key.
		default:
			s.enabled = true
			s.mu.Unlock()
			return nil
		}
		s.run = nil
		s.mu.Unlock()
		s.stopRunner(r)
		s.mu.Lock()
	}
	defer s.mu.Unlock()
	if derived {
		// Never a node whose name is not on disk: a restart would take a
		// fresh node (its state now there) for an older one and rename it.
		if err := s.recordName(nc.Hostname); err != nil {
			s.lastErr = err.Error()
			return api.Errorf(api.CodeInternal, "could not save this node's name %s in config.json (tailnet.hostname): %v", nc.Hostname, err).
				WithHint("fix config.json (or the home's permissions), then run " + config.Command("tailnet up") + " again; the node was not started")
		}
	}
	if err := os.MkdirAll(nc.Dir, 0o700); err != nil {
		s.lastErr = err.Error()
		return api.Errorf(api.CodeInternal, "tailnet state dir: %v", err)
	}
	node := s.NewNode(nc)
	if err := node.Start(); err != nil {
		_ = node.Close()
		s.lastErr = err.Error()
		return api.Errorf(api.CodeUnavailable, "tailnet: %v", err)
	}
	ctx, cancel := context.WithCancel(s.ctx)
	r := &runner{node: node, cfg: nc, cancel: cancel, done: make(chan struct{}), tlsDone: make(chan struct{})}
	s.run, s.enabled, s.lastErr = r, true, ""
	go s.serve(ctx, r)
	return nil
}

// serve waits for the node to log in, then serves the API on the tailnet
// until ctx ends.
func (s *Service) serve(ctx context.Context, r *runner) {
	defer close(r.done)
	defer r.cancel()
	tlsStarted := false
	defer func() {
		if !tlsStarted {
			close(r.tlsDone)
		}
	}()
	logged := ""
	var st NodeStatus
	for {
		var err error
		st, err = r.node.Status(ctx)
		if err == nil && st.State == StateRunning {
			break
		}
		if err == nil && st.AuthURL != "" && st.AuthURL != logged {
			logged = st.AuthURL
			s.d.Log.Info("tailnet: log in to join this machine to your tailnet", "url", st.AuthURL)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(pollEvery):
		}
	}
	ln, err := r.node.Listen("tcp", fmt.Sprintf(":%d", s.port()))
	if err != nil {
		r.failed.Store(true)
		s.setErr(fmt.Errorf("listen on the tailnet: %w", err))
		return
	}
	// No ConnContext: nothing on this listener is ever local (SPEC section 1).
	srv := &http.Server{Handler: s.RemoteHandler(r.node), ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	r.serving.Store(true)
	tlsStarted = true
	go s.serveHTTPS(ctx, r, st.SelfDNS)
	s.d.Log.Info("tailnet: serving the "+config.Program+" API", "hostname", r.cfg.Hostname, "port", s.port())
	s.publish(context.WithoutCancel(ctx), api.EventTailnetUp)
	s.refreshPeers(ctx)

	t := time.NewTicker(sweepEvery)
	defer t.Stop()
loop:
	for {
		select {
		case <-ctx.Done():
			break loop
		case err = <-errc:
			r.failed.Store(true)
			s.setErr(fmt.Errorf("tailnet listener: %w", err))
			break loop
		case <-t.C:
			s.refreshPeers(ctx)
		}
	}
	r.serving.Store(false)
	_ = srv.Close() // also ends SSE streams
	s.publish(context.WithoutCancel(ctx), api.EventTailnetDown)
}

// serveHTTPS is independent of the existing HTTP listener: provisioning a
// certificate must not hold up peer traffic or daemon shutdown.
func (s *Service) serveHTTPS(ctx context.Context, r *runner, dns string) {
	defer close(r.tlsDone)
	node, ok := r.node.(TLSNode)
	if !ok {
		return
	}
	var ln net.Listener
	var err error
	for {
		ln, err = node.ListenTLS("tcp", ":443")
		if err == nil {
			break
		}
		if ctx.Err() != nil {
			return
		}
		s.d.Log.Warn("tailnet: HTTPS listener unavailable; enable HTTPS certificates and MagicDNS in the Tailscale DNS admin page", "err", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(30 * time.Second):
		}
	}
	if ctx.Err() != nil {
		_ = ln.Close()
		return
	}
	srv := &http.Server{Handler: s.RemoteHandler(r.node), ReadHeaderTimeout: 10 * time.Second}
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = srv.Close()
		case <-done:
		}
	}()
	r.https.Store(true)
	s.d.Log.Info("tailnet: serving HTTPS", "url", "https://"+dns+"/v0/approvals")
	err = srv.Serve(ln)
	r.https.Store(false)
	close(done)
	if err != nil && err != http.ErrServerClosed {
		s.d.Log.Warn("tailnet: HTTPS listener stopped", "err", err)
	}
}

// RemoteHandler is what the tailnet listener serves: every request is
// identified with node.WhoIs and handed to d.Handler with core.WithRemote.
// A failed WhoIs still marks the request remote, with no login, so core
// accepts only a bearer token (fail closed). It never calls core.WithLocal.
func (s *Service) RemoteHandler(node Node) http.Handler {
	var selfMu sync.Mutex
	selfID, selfLogin := "", ""
	self := func(ctx context.Context) (string, string) {
		selfMu.Lock()
		defer selfMu.Unlock()
		if selfID == "" || selfLogin == "" {
			if st, err := node.Status(ctx); err == nil && st.SelfID != "" {
				selfID, selfLogin = st.SelfID, st.SelfLogin
			}
		}
		return selfID, selfLogin
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Every answer names this daemon's seal key, a 503 while starting
		// too: a member reads "an older home" only from a 2xx health answer.
		own, fault := s.nameKey(w, r)
		next := s.d.Handler
		if next == nil {
			api.WriteError(w, api.Errorf(api.CodeUnavailable, "the daemon is starting"))
			return
		}
		var rm core.Remote
		if who, err := node.WhoIs(r.Context(), r.RemoteAddr); err != nil {
			s.d.Log.Warn("tailnet: whois", "remote", r.RemoteAddr, "err", err)
		} else {
			rm.Login, rm.Node = who.Login, who.Node
			rm.NodeID, rm.NodeKey, rm.Tagged, rm.OS = who.NodeID, who.NodeKey, who.Tagged, who.OS
			// Another zm daemon, by its own hostname (which zm
			// tailnet up requires to be zm-<machine>) or its tailnet name.
			_, byHost := machineFromHost(who.Host)
			_, byName := machineFromHost(who.Node)
			rm.Zm = byHost || byName
			// This daemon's own node, or another node on this machine (the
			// Mac's own Tailscale app, where its agents run): never a
			// separate device. Approvals refuse to pair it.
			id, login := self(r.Context())
			rm.SameLogin = who.Login != "" && strings.EqualFold(who.Login, login)
			if hc, ok := node.(HostChecker); ok {
				rm.Self = (id != "" && who.NodeID == id) || hc.FromThisHost(r.RemoteAddr)
			} else {
				rm.Self = (id != "" && who.NodeID == id) || s.fromHost(r.RemoteAddr)
			}
		}
		r, ok := s.sealDoor(w, r, &rm, own, fault)
		if !ok {
			return
		}
		next.ServeHTTP(w, r.WithContext(core.WithRemote(r.Context(), rm)))
	})
}

// fromHost reports whether remoteAddr's IP is one of this machine's own
// addresses. When the host's addresses cannot be read it says yes (fail
// closed: the caller is treated as this machine).
func (s *Service) fromHost(remoteAddr string) bool {
	host := remoteAddr
	if h, _, err := net.SplitHostPort(remoteAddr); err == nil {
		host = h
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return true
	}
	addrs := s.HostAddrs
	if addrs == nil {
		addrs = interfaceAddrs
	}
	own, err := addrs()
	if err != nil {
		return true
	}
	ip = ip.Unmap()
	for _, a := range own {
		if a.Unmap() == ip {
			return true
		}
	}
	return ip.IsLoopback()
}

// interfaceAddrs lists this machine's IP addresses.
func interfaceAddrs() ([]netip.Addr, error) {
	as, err := net.InterfaceAddrs()
	if err != nil {
		return nil, err
	}
	var out []netip.Addr
	for _, a := range as {
		if p, err := netip.ParsePrefix(a.String()); err == nil {
			out = append(out, p.Addr())
		}
	}
	return out, nil
}

func (s *Service) setErr(err error) {
	s.d.Log.Warn("tailnet", "err", err)
	s.mu.Lock()
	s.lastErr = err.Error()
	s.mu.Unlock()
}

func (s *Service) publish(ctx context.Context, typ string) {
	st := s.Status(ctx)
	if _, err := s.d.Events.Publish(ctx, events.Input{Type: typ, Subject: s.d.Config.Machine, Data: st}); err != nil {
		s.d.Log.Warn("tailnet: publish", "type", typ, "err", err)
	}
}

// Down stops the node. It does not change config (the route does).
func (s *Service) Down(ctx context.Context) api.TailnetStatus {
	s.mu.Lock()
	s.enabled, s.lastErr = false, ""
	s.mu.Unlock()
	s.stopNode()
	return s.Status(ctx)
}

// stopNode stops the serving loop and closes the node.
func (s *Service) stopNode() {
	s.ctl.Lock()
	defer s.ctl.Unlock()
	s.mu.Lock()
	r := s.run
	s.run = nil
	s.mu.Unlock()
	if r != nil {
		s.stopRunner(r)
	}
}

// stopRunner stops one runner's serving loop, closes its node and marks
// every known peer offline. s.run must no longer point at r.
func (s *Service) stopRunner(r *runner) {
	r.cancel()
	<-r.done
	if err := r.node.Close(); err != nil {
		s.d.Log.Warn("tailnet: close", "err", err)
	}
	<-r.tlsDone
	s.peerMu.Lock()
	for m, p := range s.peers {
		p.Online = false
		s.peers[m] = p
	}
	s.peerMu.Unlock()
}

// Status reports the node's state.
func (s *Service) Status(ctx context.Context) api.TailnetStatus {
	s.mu.Lock()
	r := s.run
	st := api.TailnetStatus{Enabled: s.enabled, Error: s.lastErr}
	s.mu.Unlock()
	if r == nil {
		return st
	}
	st.Hostname = r.cfg.Hostname
	ns, err := r.node.Status(ctx)
	if err != nil {
		st.Error = err.Error()
		return st
	}
	st.Running = ns.State == StateRunning && r.serving.Load()
	st.Tailnet = ns.Suffix
	if ns.State != StateRunning {
		st.AuthURL = ns.AuthURL
	}
	if host := first(ns.SelfDNS, ns.SelfIP); host != "" {
		st.URL = baseURL(host, s.port())
	}
	if r.https.Load() && ns.SelfDNS != "" {
		st.HTTPSURL = "https://" + ns.SelfDNS
	}
	return st
}

// waitStatus polls Status until the node serves, needs a login, fails, or
// wait passes.
func (s *Service) waitStatus(ctx context.Context, wait time.Duration) api.TailnetStatus {
	deadline := time.Now().Add(wait)
	for {
		st := s.Status(ctx)
		if st.Running || st.AuthURL != "" || st.Error != "" || !st.Enabled || time.Now().After(deadline) {
			return st
		}
		select {
		case <-ctx.Done():
			return st
		case <-time.After(pollEvery / 2):
		}
	}
}

// nodeName is the name this daemon's node asks for when up names none: the
// one config.json records (tailnet.hostname); else, when the home already
// holds a node's state, zm-<machine>: a node made before the rename's step 2,
// which keeps its name (peers, replicas and codes know it by it); else, for a
// new node, potluck-<machine> (fresh). up records either before it starts
// the node, so once a node has state its name is always on record.
func nodeName(cfg *config.Config) (name string, fresh bool) {
	if h := strings.ToLower(strings.TrimSpace(cfg.File.Tailnet.Hostname)); h != "" {
		return h, false
	}
	if ents, err := os.ReadDir(cfg.Path(StateDir)); err == nil && len(ents) > 0 {
		return HostPrefixOld + cfg.Machine, false
	}
	return HostPrefix + cfg.Machine, true
}

// recordName saves the node's name as tailnet.hostname, unless the file
// names one already. s.mu held.
func (s *Service) recordName(name string) error {
	fresh, err := config.LoadFrom(s.d.Config.Home)
	if err != nil {
		return err
	}
	if fresh.File.Tailnet.Hostname != "" {
		return nil
	}
	fresh.File.Tailnet.Hostname = name
	if err := fresh.Save(); err != nil {
		return err
	}
	s.d.Config.File.Tailnet.Hostname = name
	return nil
}

// saveEnabled records tailnet.enabled (and the hostname / control server
// when given) in config.json, re-reading the file so other keys stay as the
// human left them. The auth key is never saved.
func (s *Service) saveEnabled(on bool, req api.TailnetUpRequest) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	fresh, err := config.LoadFrom(s.d.Config.Home)
	if err != nil {
		return err
	}
	tn := fresh.File.Tailnet
	tn.Enabled = on
	if h := strings.ToLower(strings.TrimSpace(req.Hostname)); h != "" {
		tn.Hostname = h
	}
	if c := strings.TrimSpace(req.ControlURL); c != "" {
		tn.ControlURL = c
	}
	fresh.File.Tailnet = tn
	if err := fresh.Save(); err != nil {
		return err
	}
	s.d.Config.File.Tailnet = tn
	return nil
}

func baseURL(host string, port int) string {
	if strings.Contains(host, ":") { // IPv6
		host = "[" + host + "]"
	}
	return fmt.Sprintf("http://%s:%d", host, port)
}

func first(xs ...string) string {
	for _, x := range xs {
		if x = strings.TrimSpace(x); x != "" {
			return x
		}
	}
	return ""
}
