//go:build darwin

package main

import (
	"fmt"
	"net/netip"
	"os/exec"
	"strings"
	"sync"
)

// world is everything beyond this Mac that a probe may try to reach: a fake
// interface pair whose far end answers, as any address routed to it, every
// TCP SYN (with a SYN-ACK), UDP datagram (with an echo) and ping. So a probe
// that gets out arrives and is answered, on every port, without leaving this
// Mac (the real hosts of -allow and -forbid and the HTTPS request excepted).
type world struct {
	tap  *tap
	mac  mac
	near string // this Mac's end
	far  string
}

// Addresses of the pair: this Mac's, and the next hop every route names. The
// subnet is the fake LAN.
var (
	lanHost = netip.MustParseAddr("10.77.0.1")
	lanHop  = netip.MustParseAddr("10.77.0.2")
)

// worldRoutes are the networks routed to the fake world. The public one
// is routed there only while this runs.
var worldRoutes = []string{
	"10.200.0.0/16", "172.22.0.0/16", "192.168.222.0/24", // LANs behind the router
	"100.64.0.0/16",      // a tailnet's peers
	"100.100.100.100/32", // Tailscale's resolver
	"198.18.0.0/24", "192.0.2.0/24", "203.0.113.0/24",
	"169.254.169.254/32",
	"9.9.9.0/24", // "public" addresses
}

func run(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).CombinedOutput()
	s := strings.TrimSpace(string(out))
	if err != nil {
		return s, fmt.Errorf("%s %s: %v: %s", name, strings.Join(args, " "), err, s)
	}
	return s, nil
}

func newWorld(undo *undoer) (*world, error) {
	w := &world{near: "feth770", far: "feth771", mac: mac{0x66, 0x65, 0x74, 0x68, 0x07, 0x71}}
	for _, n := range []string{w.near, w.far} {
		undo.add(func() { run("/sbin/ifconfig", n, "destroy") })
		if _, err := run("/sbin/ifconfig", n, "create"); err != nil {
			return nil, err
		}
	}
	for _, a := range [][]string{
		{w.near, "peer", w.far},
		{w.near, "inet", lanHost.String(), "netmask", "255.255.255.0", "up"},
		{w.far, "up"},
	} {
		if _, err := run("/sbin/ifconfig", a...); err != nil {
			return nil, err
		}
	}
	for _, r := range worldRoutes {
		undo.add(func() { run("/sbin/route", "-n", "delete", "-net", r, lanHop.String()) })
		if _, err := run("/sbin/route", "-n", "add", "-net", r, lanHop.String()); err != nil {
			return nil, err
		}
	}
	t, err := openTap(w.far)
	if err != nil {
		return nil, err
	}
	w.tap = t
	go t.each(w.answer)
	undo.add(func() { t.f.Close() })
	return w, nil
}

// undoer is what this program changed on the Mac and must put back, also
// when it is interrupted.
type undoer struct {
	mu   sync.Mutex
	fns  []func()
	done bool
}

func (u *undoer) add(f func()) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.done { // the program is ending: nothing more is changed
		u.mu.Unlock()
		select {}
	}
	u.fns = append(u.fns, f)
}

// run puts everything back, the latest first. A change is added before it is
// made, so one under way is put back too.
func (u *undoer) run() {
	u.mu.Lock()
	defer u.mu.Unlock()
	for i := len(u.fns) - 1; i >= 0; i-- {
		u.fns[i]()
	}
	u.fns, u.done = nil, true
}

func (w *world) answer(f []byte) {
	s, ok := parse(f)
	if !ok || s.src == w.mac {
		return
	}
	switch {
	case s.typ == etherARP && s.arpOp == 1 && s.to != lanHost:
		var sha mac
		copy(sha[:], s.data)
		w.tap.write(eth(s.src, w.mac, etherARP, arp(2, w.mac, s.to, sha, s.from)))
	case !s.ip:
	case s.proto == protoTCP && s.flags&(flagSYN|flagACK) == flagSYN:
		w.ip(s, tcp(s.to, s.from, s.dport, s.sport, 1000, s.seq+1, flagSYN|flagACK, nil))
	case s.proto == protoUDP && s.dport != 0 && s.data != nil:
		w.ip(s, udp(s.to, s.from, s.dport, s.sport, s.data))
	case s.proto == protoICMP && s.sport == 8:
		w.ip(s, icmpEcho(0, s.dport, seqOf(f), s.data))
	}
}

func seqOf(f []byte) uint16 { // an ICMP echo's sequence number, no IP options
	if len(f) < 42 {
		return 0
	}
	return be.Uint16(f[40:])
}

func (w *world) ip(s seen, seg []byte) {
	w.tap.write(eth(s.src, w.mac, etherIPv4, ipv4{src: s.to, dst: s.from, proto: s.proto, payload: seg}.bytes()))
}

// send puts one IPv4 packet from the world onto the fake LAN, toward this Mac
// (its router to the VM's network).
func (w *world) send(p ipv4, to mac) error {
	return w.tap.write(eth(to, w.mac, etherIPv4, p.bytes()))
}
