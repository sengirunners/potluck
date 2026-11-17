package tailnet

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"tailscale.com/client/local"
	"tailscale.com/tailcfg"
	"tailscale.com/tsnet"
)

// Node is the embedded tailnet node. The real one is tsnet; tests use a fake,
// so nothing in this package's tests touches a real tailnet.
type Node interface {
	// Start brings the node up without waiting for a login.
	Start() error
	// Status reports the backend state, the login URL and the peers it sees.
	Status(ctx context.Context) (NodeStatus, error)
	// WhoIs identifies the tailnet caller behind a connection's remote address.
	WhoIs(ctx context.Context, remoteAddr string) (Who, error)
	// Listen listens on the node's tailnet address, e.g. ("tcp", ":7433").
	Listen(network, addr string) (net.Listener, error)
	// HTTPClient dials through the tailnet (peer health probes).
	HTTPClient() *http.Client
	Close() error
}

// TLSNode provides a tailnet-only listener with a MagicDNS certificate.
type TLSNode interface {
	ListenTLS(network, addr string) (net.Listener, error)
}

// Dialer is an optional Node method: connect to a tailnet address (e.g.
// "zm-mini:7433") through this node; a team replica reaches its authority
// this way (SPEC 17.4). tsnet and tailnettest nodes have it; for others the
// dial of HTTPClient's transport is used.
type Dialer interface {
	Dial(ctx context.Context, network, addr string) (net.Conn, error)
}

func dial(n Node, ctx context.Context, network, addr string) (net.Conn, error) {
	if d, ok := n.(Dialer); ok {
		return d.Dial(ctx, network, addr)
	}
	if t, ok := n.HTTPClient().Transport.(*http.Transport); ok && t.DialContext != nil {
		return t.DialContext(ctx, network, addr)
	}
	return nil, errors.New("this tailnet node cannot dial")
}

// HostChecker is an optional Node method: whether the caller at remoteAddr
// runs on this machine (core.Remote.Self). A node without it is asked
// nothing and the host's interface addresses decide; the loopback test
// network (tailnettest) answers from the identities it fakes.
type HostChecker interface {
	FromThisHost(remoteAddr string) bool
}

// NodeConfig is what the service hands to NewNode.
type NodeConfig struct {
	Dir        string // state directory, $ZM_HOME/tsnet
	Hostname   string // zm-<machine> by default
	AuthKey    string // else TS_AUTHKEY (tsnet reads it), else a login URL
	ControlURL string // "" = Tailscale; e.g. a Headscale URL
	// Logf receives user-facing messages (the login URL).
	Logf func(format string, args ...any)
}

// StateRunning is the backend state of a logged-in, connected node.
const StateRunning = "Running"

// NodeStatus is the part of Tailscale's status zm uses.
type NodeStatus struct {
	State   string // NeedsLogin, Starting, Running, ...
	AuthURL string // set while waiting for a browser login
	Suffix  string // MagicDNS suffix, e.g. tail1234.ts.net
	SelfDNS string // this node's FQDN without the trailing dot
	SelfIP  string
	SelfID  string // this node's stable node ID ("" until logged in)
	// SelfLogin is the login this node signed in with: the person who put this
	// daemon on the tailnet.
	SelfLogin string
	Peers     []NodePeer
}

// NodePeer is another node on the tailnet.
type NodePeer struct {
	Host    string // the node's own hostname (zm nodes: zm-<machine>)
	DNSName string // FQDN without the trailing dot
	IP      string // a Tailscale IP, IPv4 preferred
	Online  bool
	Login   string // owner's login name ("" for tagged nodes)
	ID      string // its stable node ID (two peers may report one Host)
}

// Who is the tailnet identity behind a connection.
type Who struct {
	// Login is the node owner's login name. It is "" for tagged nodes:
	// Tailscale reports every tagged node as the same pseudo-user, which must
	// never map to one zm identity.
	Login string
	Node  string // the node's MagicDNS name
	// Host is the node's own hostname (Hostinfo), which a rename in the
	// admin console does not change: zm daemons set zm-<machine>.
	Host string
	// NodeID and NodeKey identify the device itself (stable node ID, node
	// public key). Approvals bind the decide right to them (SPEC section 16).
	NodeID  string
	NodeKey string
	Tagged  bool // a tagged node (server, CI), never a person's device
	// OS is the node's self-reported OS (Hostinfo): "iOS", "android", "macOS",
	// "linux", "windows". Self-reported, so it only narrows, never grants alone.
	OS string
}

// EnvTestTSNet lets a test binary start a real tsnet node on purpose
// ("1"); without it every test daemon's default node refuses to start. Its
// new name, EnvTestTSNetNew, is read too.
const (
	EnvTestTSNet    = "ZM_TEST_TSNET"
	EnvTestTSNetNew = "POTLUCK_TEST_TSNET"
)

// defaultNode is the real node, except in a test binary: a test daemon that
// forgot its fake (tailnettest) must never reach a real tailnet's control
// server (a route walk of POST /v0/tailnet/up did once).
func defaultNode(c NodeConfig) Node {
	if testing.Testing() && os.Getenv(EnvTestTSNet) != "1" && os.Getenv(EnvTestTSNetNew) != "1" {
		return refusedNode{}
	}
	return newTSNode(c)
}

var errTestNode = errors.New("a test binary may not start a real tailnet node: give the daemon a tailnettest node (or set " + EnvTestTSNet + "=1 on purpose)")

// refusedNode is the default node in a test binary: every call fails.
type refusedNode struct{}

func (refusedNode) Start() error                               { return errTestNode }
func (refusedNode) Status(context.Context) (NodeStatus, error) { return NodeStatus{}, errTestNode }
func (refusedNode) WhoIs(context.Context, string) (Who, error) { return Who{}, errTestNode }
func (refusedNode) Listen(string, string) (net.Listener, error) {
	return nil, errTestNode
}
func (refusedNode) HTTPClient() *http.Client { return &http.Client{Transport: refusedTransport{}} }
func (refusedNode) Close() error             { return nil }

type refusedTransport struct{}

func (refusedTransport) RoundTrip(*http.Request) (*http.Response, error) { return nil, errTestNode }

// tsNode is the real Node on tailscale.com/tsnet.
type tsNode struct {
	s  *tsnet.Server
	lc *local.Client
}

func newTSNode(c NodeConfig) Node {
	return &tsNode{s: &tsnet.Server{
		Dir: c.Dir, Hostname: c.Hostname, AuthKey: c.AuthKey, ControlURL: c.ControlURL,
		UserLogf: quietLogin(c.Logf, time.Now), // tsnet repeats the login URL every 5 s
		Logf:     func(string, ...any) {},      // backend logs are very verbose
	}}
}

func (n *tsNode) Start() error {
	if err := n.s.Start(); err != nil {
		return err
	}
	lc, err := n.s.LocalClient()
	if err != nil {
		return err
	}
	n.lc = lc
	return nil
}

func (n *tsNode) Status(ctx context.Context) (NodeStatus, error) {
	if n.lc == nil {
		return NodeStatus{}, errors.New("tailnet node not started")
	}
	st, err := n.lc.Status(ctx)
	if err != nil {
		return NodeStatus{}, err
	}
	out := NodeStatus{State: st.BackendState, AuthURL: st.AuthURL}
	if st.CurrentTailnet != nil {
		out.Suffix = st.CurrentTailnet.MagicDNSSuffix
	}
	if st.Self != nil {
		out.SelfID = string(st.Self.ID)
		out.SelfDNS = strings.TrimSuffix(st.Self.DNSName, ".")
		if u, ok := st.User[st.Self.UserID]; ok {
			out.SelfLogin = u.LoginName
		}
		for _, ip := range st.Self.TailscaleIPs {
			if ip.Is4() || out.SelfIP == "" {
				out.SelfIP = ip.String()
			}
		}
	}
	for _, p := range st.Peer {
		np := NodePeer{Host: p.HostName, DNSName: strings.TrimSuffix(p.DNSName, "."), Online: p.Online, ID: string(p.ID)}
		for _, ip := range p.TailscaleIPs {
			if ip.Is4() || np.IP == "" {
				np.IP = ip.String()
			}
		}
		if p.Tags == nil || p.Tags.Len() == 0 {
			if u, ok := st.User[p.UserID]; ok {
				np.Login = u.LoginName
			}
		}
		out.Peers = append(out.Peers, np)
	}
	return out, nil
}

func (n *tsNode) WhoIs(ctx context.Context, remoteAddr string) (Who, error) {
	if n.lc == nil {
		return Who{}, errors.New("tailnet node not started")
	}
	r, err := n.lc.WhoIs(ctx, remoteAddr)
	if err != nil {
		return Who{}, err
	}
	return whoFrom(r.Node, r.UserProfile), nil
}

// whoFrom turns a WhoIs answer into a Who. Tagged nodes get no login.
func whoFrom(node *tailcfg.Node, user *tailcfg.UserProfile) Who {
	var w Who
	if node != nil {
		w.Node = node.ComputedName
		if node.Hostinfo.Valid() {
			w.Host = node.Hostinfo.Hostname()
			w.OS = node.Hostinfo.OS()
		}
		w.NodeID = string(node.StableID)
		if !node.Key.IsZero() {
			w.NodeKey = node.Key.String()
		}
		w.Tagged = node.IsTagged()
	}
	if user != nil && (node == nil || !node.IsTagged()) {
		w.Login = user.LoginName
	}
	return w
}

func (n *tsNode) Listen(network, addr string) (net.Listener, error) { return n.s.Listen(network, addr) }
func (n *tsNode) ListenTLS(network, addr string) (net.Listener, error) {
	return n.s.ListenTLS(network, addr)
}
func (n *tsNode) HTTPClient() *http.Client { return n.s.HTTPClient() }
func (n *tsNode) Dial(ctx context.Context, network, addr string) (net.Conn, error) {
	return n.s.Dial(ctx, network, addr)
}
func (n *tsNode) Close() error { return n.s.Close() }
