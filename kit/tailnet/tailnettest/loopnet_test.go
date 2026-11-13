package tailnettest

import (
	"context"
	"net"
	"testing"
)

type addrConn struct {
	net.Conn
	addr, to net.Addr
}

func (c addrConn) LocalAddr() net.Addr  { return c.addr }
func (c addrConn) RemoteAddr() net.Addr { return c.to }
func (c addrConn) Close() error         { return nil }

// The load test's replicas lost the authority 90 s after it restarted: an
// idle connection to the old listener closed and took WhoIs away from a newer
// connection on the same local port (the kernel may reuse it for another
// destination), and that connection then got 401s for good.
func TestClosingAnOldConnectionKeepsTheNewOnesWhoIs(t *testing.T) {
	n := New()
	auth := &Node{net: n, name: "zm-mini", id: Identity{NodeID: "nMINI"}, ln: fakeListener{loopAddr(41000)}}
	x := &Node{net: n, name: "zm-carl", id: Identity{NodeID: "nCARL"}}
	addr := loopAddr(40000)
	before := n.track(x, "zm-mini", addrConn{addr: addr, to: auth.ln.Addr()})
	after := n.track(x, "zm-mini", addrConn{addr: addr, to: auth.ln.Addr()})
	if err := before.Close(); err != nil {
		t.Fatal(err)
	}
	who, err := auth.WhoIs(context.Background(), addr.String())
	if err != nil || who.NodeID != "nCARL" {
		t.Fatalf("whois after the old connection closed: %+v %v", who, err)
	}
	if err := after.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := auth.WhoIs(context.Background(), addr.String()); err == nil {
		t.Fatal("whois knows a closed connection")
	}
}

func loopAddr(port int) *net.TCPAddr { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port} }

// orchid-k456q: after the load test's authority restarted, two replicas got
// 401 for good. Linux hands one local port to connections with different
// destinations, so a replica's connection to the authority and another node's
// connection elsewhere (a peer probe) can share 127.0.0.1:P: keyed by the
// local address alone, the newer one took the older one's WhoIs, and its
// close left the older one unknown (a node-bound token's 401) while it lived.
func TestOneLocalPortToTwoListenersKeepsBothWhoIs(t *testing.T) {
	n := New()
	auth := &Node{net: n, name: "zm-mini", id: Identity{NodeID: "nMINI"}}
	peer := &Node{net: n, name: "zm-p3", id: Identity{NodeID: "nP3"}}
	carl := &Node{net: n, name: "zm-carl", id: Identity{NodeID: "nCARL"}}
	sam := &Node{net: n, name: "zm-sam", id: Identity{NodeID: "nSAM"}}
	auth.ln, peer.ln = fakeListener{loopAddr(41000)}, fakeListener{loopAddr(42000)}
	shared := loopAddr(40000)
	n.track(carl, "zm-mini", addrConn{addr: shared, to: auth.ln.Addr()})
	probe := n.track(sam, "zm-p3", addrConn{addr: shared, to: peer.ln.Addr()})
	ctx := context.Background()
	if who, err := auth.WhoIs(ctx, shared.String()); err != nil || who.NodeID != "nCARL" {
		t.Fatalf("the authority's whois with a probe on the same local port: %+v %v", who, err)
	}
	if who, err := peer.WhoIs(ctx, shared.String()); err != nil || who.NodeID != "nSAM" {
		t.Fatalf("the peer's whois: %+v %v", who, err)
	}
	if err := probe.Close(); err != nil {
		t.Fatal(err)
	}
	if who, err := auth.WhoIs(ctx, shared.String()); err != nil || who.NodeID != "nCARL" {
		t.Fatalf("the authority's whois after the probe closed: %+v %v", who, err)
	}
}

type fakeListener struct{ addr net.Addr }

func (l fakeListener) Accept() (net.Conn, error) { return nil, net.ErrClosed }
func (l fakeListener) Close() error              { return nil }
func (l fakeListener) Addr() net.Addr            { return l.addr }
