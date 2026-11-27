package conformance

import (
	"context"
	"net"
	"strings"
	"sync"
	"testing"

	"github.com/sengirunners/potluck/conformance/refdriver"
)

// lanForbid is one address on this machine's LAN interface, whose listener
// takes every connection, for the fence checks: a forbidden address must
// open from here. With no LAN address it is one that does not open, and the
// check that uses it skips (CI always has one: refdriver.LANAddrCI).
var lanForbid = sync.OnceValue(func() string {
	ip, ok := refdriver.LANAddr()
	if !ok {
		return "192.0.2.1:9"
	}
	l, err := net.Listen("tcp", net.JoinHostPort(ip, "0"))
	if err != nil {
		return "192.0.2.1:9"
	}
	go func() {
		for c, err := l.Accept(); err == nil; c, err = l.Accept() {
			c.Close()
		}
	}()
	return l.Addr().String()
})

// fenceAliasNet: the host's fence knows two of this machine's addresses, a
// stand-in for its gateway first and its LAN address (where lanForbid
// answers) second, as a Mac's rules list the gateway and each address the
// Mac holds; the box must reach neither.
func fenceAliasNet(o *Options) {
	a := lanForbid()
	h, _, _ := net.SplitHostPort(a)
	o.Config["fence_addrs"] = "192.0.2.1," + h
	o.NetForbid = []string{a}
}

const multicastCheck = "a box's multicast reaches no program on this machine"

// A box behind the host's whole fence passes both checks: it reaches no
// address of this machine the fence names (the first or a later one), and
// its multicast reaches no program here. TestBrokenDrivers fails each with
// the fence missing one class.
func TestFenceGoodDriver(t *testing.T) {
	if _, ok := refdriver.LANAddrCI(t, "the fence's address check"); !ok {
		t.Skip("no LAN address")
	}
	path, opt := fixture(t, "box", "")
	opt.Only = []string{"net"}
	fenceAliasNet(&opt)
	opt.NetMulticast = true
	res := Run(context.Background(), path, opt)
	record(res, passedSet)
	out := Format(res)
	if strings.Contains(out, "SKIP  "+multicastCheck+": not proven:") {
		refdriver.NotHere(t, "no multicast datagram from here reaches this machine:\n"+out)
	}
	for _, want := range []string{"PASS  a box cannot reach " + lanForbid(), "PASS  " + multicastCheck} {
		if !strings.Contains(out, want) {
			t.Fatalf("want %q in:\n%s", want, out)
		}
	}
}
