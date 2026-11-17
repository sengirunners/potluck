// Package tailnettest is a tailnet on loopback for tests: several daemons
// on one machine, each with its own fake node behind the same tailnet.Node
// interface the tsnet path uses. Listeners are 127.0.0.1 TCP; WhoIs answers
// from the identities the test gave each node (stable node ID, node key,
// login, tags), keyed by the connection's addresses, so the daemons see the
// same node identities and refusals they would on a real tailnet. Nothing
// here joins a real tailnet or uses an auth key. Never linked into zm.
package tailnettest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sengirunners/potluck/kit/tailnet"
)

// Suffix is the fake tailnet's MagicDNS suffix.
const Suffix = "loop.ts.net"

// Identity is what WhoIs reports for a node.
type Identity struct {
	NodeID  string // stable node ID, e.g. nAUTH
	NodeKey string // node public key
	Login   string // "" for tagged nodes
	Tagged  bool
	// Host groups nodes on one "machine": a caller from a node with the same
	// Host is this machine (core.Remote.Self). "" = its own machine.
	Host string
	OS   string // self-reported OS
	// Relays makes the node serve subnet routes (or be an exit node).
	Relays bool
}

// Net is one fake tailnet.
type Net struct {
	mu       sync.Mutex
	offline  map[string]bool             // names whose dials hang until the caller gives up (Offline)
	cut      map[[2]string]bool          // (from, to) node names partitioned (Cut)
	slow     map[[2]string]time.Duration // (from, to) node names: added to every read (Slow)
	live     map[*conn][2]string         // open dialled connections: (from, to)
	nodes    map[string]*Node            // by the name the tailnet gave (hostname, or hostname-1 when taken)
	byListen map[string]*Node            // listener address -> node
	byConn   map[[2]string]*Node         // a dialled connection's (local, listener) addresses -> the dialling node
	newest   map[[2]string]*conn         // the connection byConn's entry is for (see track)
	fullOnly map[string]bool             // names that resolve only by their full MagicDNS name (Shared)
	tap      atomic.Pointer[io.Writer]   // every byte of every dialled connection, both ways (Tap)
}

// Tap writes every byte of every connection dialled from now on, both ways,
// to w (a relay that could read the traffic; nil stops it). w must be safe
// for concurrent writes.
func (n *Net) Tap(w io.Writer) {
	if w == nil {
		n.tap.Store(nil)
		return
	}
	n.tap.Store(&w)
}

// Shared makes node name resolve only by its full MagicDNS name
// (name.loop.ts.net), as a node shared in from another tailnet does.
func (n *Net) Shared(name string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.fullOnly == nil {
		n.fullOnly = map[string]bool{}
	}
	n.fullOnly[name] = true
}

// New returns an empty tailnet.
func New() *Net {
	return &Net{nodes: map[string]*Node{}, byListen: map[string]*Node{}, byConn: map[[2]string]*Node{}, newest: map[[2]string]*conn{}}
}

// NodeFunc returns a tailnet.Service.NewNode that makes a node with id.
func (n *Net) NodeFunc(id Identity) func(tailnet.NodeConfig) tailnet.Node {
	return func(c tailnet.NodeConfig) tailnet.Node { return &Node{net: n, id: id, cfg: c} }
}

// SetIdentity changes a node's identity (by node ID): a re-keyed or newly
// tagged node.
func (n *Net) SetIdentity(nodeID string, id Identity) {
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, x := range n.nodes {
		if x.id.NodeID == nodeID {
			x.id = id
		}
	}
}

// Offline makes dials to the node named name hang until the dialler's
// context ends, as a dial to a peer whose laptop is closed does on a real
// tailnet (instead of failing at once); online puts it back.
func (n *Net) Offline(name string, offline bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.offline == nil {
		n.offline = map[string]bool{}
	}
	n.offline[name] = offline
}

// Cut partitions node from from node to (names as the tailnet gave them,
// e.g. "zm-host-b", "zm-mini"): connections already open between them
// drop, and new dials from one to the other hang until the dialler gives up,
// as on a real tailnet when a laptop loses its network. cut=false heals it.
func (n *Net) Cut(from, to string, cut bool) {
	n.mu.Lock()
	if n.cut == nil {
		n.cut = map[[2]string]bool{}
	}
	n.cut[[2]string{from, to}] = cut
	n.cut[[2]string{to, from}] = cut
	var drop []*conn
	if cut {
		for c, ft := range n.live {
			if (ft[0] == from && ft[1] == to) || (ft[0] == to && ft[1] == from) {
				drop = append(drop, c)
			}
		}
	}
	n.mu.Unlock()
	for _, c := range drop {
		_ = c.Close()
	}
}

// Slow adds d to every read by from on connections to to, new or open, as a
// slow link does. d = 0 restores it.
func (n *Net) Slow(from, to string, d time.Duration) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.slow == nil {
		n.slow = map[[2]string]time.Duration{}
	}
	n.slow[[2]string{from, to}] = d
}

// Node is one fake node.
type Node struct {
	net  *Net
	id   Identity
	cfg  tailnet.NodeConfig
	name string // the name the tailnet gave it
	ln   net.Listener
	up   bool
}

var _ tailnet.Node = (*Node)(nil)
var _ tailnet.HostChecker = (*Node)(nil)

// Start joins the tailnet at once (no login). A hostname another node holds
// gets "-1", as Tailscale renames a duplicate.
func (x *Node) Start() error {
	n := x.net
	n.mu.Lock()
	defer n.mu.Unlock()
	name := x.cfg.Hostname
	for i := 1; ; i++ {
		if o, taken := n.nodes[name]; !taken || o == x {
			break
		}
		name = fmt.Sprintf("%s-%d", x.cfg.Hostname, i)
	}
	x.name, x.up = name, true
	n.nodes[name] = x
	return nil
}

// Status reports Running with the other nodes as peers.
func (x *Node) Status(context.Context) (tailnet.NodeStatus, error) {
	n := x.net
	n.mu.Lock()
	defer n.mu.Unlock()
	if !x.up {
		return tailnet.NodeStatus{}, errors.New("tailnet node not started")
	}
	st := tailnet.NodeStatus{State: tailnet.StateRunning, Suffix: Suffix, SelfDNS: x.name + "." + Suffix, SelfIP: "127.0.0.1", SelfID: x.id.NodeID}
	for name, o := range n.nodes {
		if o == x {
			continue
		}
		st.Peers = append(st.Peers, tailnet.NodePeer{Host: o.cfg.Hostname, DNSName: name + "." + Suffix, IP: "127.0.0.1", Online: true, Login: o.id.Login,
			ID: o.id.NodeID, Key: o.id.NodeKey, OS: o.id.OS, Tagged: o.id.Tagged, Relays: o.id.Relays, Shared: n.fullOnly[name]})
	}
	// In node ID order: deterministic, so a test can put a node first.
	slices.SortFunc(st.Peers, func(a, b tailnet.NodePeer) int { return strings.Compare(a.ID, b.ID) })
	return st, nil
}

// who is the node behind addr as x sees it: the dialler of a connection to
// x's listener from addr, or the node listening on addr.
func (n *Net) who(x *Node, addr string) (*Node, bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if x.ln != nil {
		if o, ok := n.byConn[[2]string{addr, x.ln.Addr().String()}]; ok {
			return o, true
		}
	}
	o, ok := n.byListen[addr]
	return o, ok
}

// WhoIs identifies the node behind a connection's address (either end).
func (x *Node) WhoIs(_ context.Context, remoteAddr string) (tailnet.Who, error) {
	o, ok := x.net.who(x, remoteAddr)
	if !ok {
		return tailnet.Who{}, fmt.Errorf("whois %s: not a tailnet address", remoteAddr)
	}
	x.net.mu.Lock()
	defer x.net.mu.Unlock()
	return tailnet.Who{Login: o.id.Login, Node: o.name + "." + Suffix, Host: o.cfg.Hostname, NodeID: o.id.NodeID,
		NodeKey: o.id.NodeKey, Tagged: o.id.Tagged, OS: o.id.OS}, nil
}

// FromThisHost: the caller's node is on this node's machine (same Host, or
// this node itself). An address the tailnet does not know counts as this
// machine (fail closed, like an unreadable interface list).
func (x *Node) FromThisHost(remoteAddr string) bool {
	o, ok := x.net.who(x, remoteAddr)
	if !ok {
		return true
	}
	return o == x || (o.id.Host != "" && o.id.Host == x.id.Host)
}

// Listen listens on loopback for this node.
func (x *Node) Listen(network, _ string) (net.Listener, error) {
	ln, err := net.Listen(network, "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	x.net.mu.Lock()
	x.ln = ln
	x.net.byListen[ln.Addr().String()] = x
	x.net.mu.Unlock()
	return ln, nil
}

// Dial connects to another node's listener by its tailnet name (zm-mini,
// zm-mini.loop.ts.net, with any port) and records the connection as this
// node's for the other side's WhoIs.
func (x *Node) Dial(ctx context.Context, network, addr string) (net.Conn, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	full := strings.HasSuffix(strings.TrimSuffix(host, "."), "."+Suffix)
	host = strings.TrimSuffix(strings.TrimSuffix(host, "."), "."+Suffix)
	n := x.net
	n.mu.Lock()
	o, ok := n.nodes[host]
	if !full && n.fullOnly[host] {
		ok = false // a shared node: its short name does not resolve
	}
	var to string
	if ok && o.ln != nil {
		to = o.ln.Addr().String()
	}
	up, off := x.up, n.offline[host] || n.cut[[2]string{x.name, host}]
	n.mu.Unlock()
	if !up {
		return nil, errors.New("tailnet node not started")
	}
	if off {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	switch {
	case !ok:
		// A name this node cannot resolve: what a real dial reports.
		return nil, &net.DNSError{Err: "no such host (no route to it on the tailnet)", Name: host, IsNotFound: true}
	case to == "":
		return nil, fmt.Errorf("dial %s: connection refused (no route to it on the tailnet)", addr)
	}
	var d net.Dialer
	c, err := d.DialContext(ctx, network, to)
	if err != nil {
		return nil, err
	}
	n.mu.Lock()
	off = n.offline[host] || n.cut[[2]string{x.name, host}]
	var cc *conn
	if !off {
		cc = n.trackLocked(x, host, c)
	}
	n.mu.Unlock()
	if off {
		// Cut while this dial ran: Cut's scan of the open connections missed
		// it, so it must not get through.
		_ = c.Close()
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return cc, nil
}

// track records c as x's, by both its addresses: the kernel hands one local
// port to connections with different destinations (seen live: another
// node's peer probe took a replica's WhoIs at the authority). Only the newest
// connection on a pair clears its entry: an old one's close must not leave
// the new one unknown to WhoIs, which a node-bound token answers with 401 for
// as long as it lives.
func (n *Net) track(x *Node, host string, c net.Conn) *conn {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.trackLocked(x, host, c)
}

// trackLocked is track with n.mu held.
func (n *Net) trackLocked(x *Node, host string, c net.Conn) *conn {
	local := [2]string{c.LocalAddr().String(), c.RemoteAddr().String()}
	link := [2]string{x.name, host}
	cc := &conn{Conn: c, closed: make(chan struct{}), tap: n.tap.Load(), delay: func() time.Duration {
		n.mu.Lock()
		defer n.mu.Unlock()
		return n.slow[link]
	}}
	n.byConn[local], n.newest[local] = x, cc
	if n.live == nil {
		n.live = map[*conn][2]string{}
	}
	n.live[cc] = [2]string{x.name, host}
	cc.done = func() {
		n.mu.Lock()
		if n.newest[local] == cc {
			delete(n.byConn, local)
			delete(n.newest, local)
		}
		delete(n.live, cc)
		n.mu.Unlock()
	}
	return cc
}

type conn struct {
	net.Conn
	once   sync.Once
	done   func()
	closed chan struct{}
	delay  func() time.Duration
	tap    *io.Writer // Net.Tap when it was dialled
	// stuck: UnixNano until which reads deliver nothing (Stall).
	stuck atomic.Int64
}

// Stall makes the connections open now between from and to (either way
// round) deliver nothing for d, as a TCP connection does while a lost
// packet waits out its retransmission backoff. New connections are not
// affected.
func (n *Net) Stall(from, to string, d time.Duration) {
	until := time.Now().Add(d).UnixNano()
	n.mu.Lock()
	defer n.mu.Unlock()
	for c, ft := range n.live {
		if (ft[0] == from && ft[1] == to) || (ft[0] == to && ft[1] == from) {
			c.stuck.Store(until)
		}
	}
}

func (c *conn) Close() error {
	c.once.Do(func() {
		close(c.closed)
		if c.done != nil {
			c.done()
		}
	})
	return c.Conn.Close()
}

// Read is the connection's, after the link's delay (Net.Slow), held until
// a Stall is over (a read already waiting when the stall began is held too).
func (c *conn) Read(p []byte) (int, error) {
	if d := c.delay(); d > 0 {
		select {
		case <-time.After(d):
		case <-c.closed:
		}
	}
	n, err := c.Conn.Read(p)
	if c.tap != nil && n > 0 {
		_, _ = (*c.tap).Write(p[:n])
	}
	for d := time.Until(time.Unix(0, c.stuck.Load())); d > 0; d = time.Until(time.Unix(0, c.stuck.Load())) {
		select {
		case <-time.After(d):
		case <-c.closed:
			return n, err
		}
	}
	return n, err
}

// Write is the connection's, copied to the tap (Net.Tap).
func (c *conn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	if c.tap != nil && n > 0 {
		_, _ = (*c.tap).Write(p[:n])
	}
	return n, err
}

// HTTPClient dials through the fake tailnet.
func (x *Node) HTTPClient() *http.Client {
	return &http.Client{Transport: &http.Transport{DialContext: x.Dial}}
}

// Close leaves the tailnet.
func (x *Node) Close() error {
	n := x.net
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.nodes[x.name] == x {
		delete(n.nodes, x.name)
	}
	if x.ln != nil {
		delete(n.byListen, x.ln.Addr().String())
	}
	x.up = false
	return nil
}

// Node returns the node the tailnet named name (e.g. "zm-mini"), or nil.
func (n *Net) Node(name string) *Node {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.nodes[name]
}
