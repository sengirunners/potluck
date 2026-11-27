//go:build darwin

package main

import (
	"bytes"
	"fmt"
	"math/rand/v2"
	"net"
	"net/netip"
	"regexp"
	"slices"
	"sync"
	"time"
)

// place is the Mac a run is on: its addresses and the endpoints beyond it.
type place struct {
	hostAddrs []netip.Addr     // this Mac's own, the vmnet gateway first
	own       []netip.Addr     // of those, the ones its networks gave it
	extra     []netip.AddrPort // real hosts that must stay out of reach (-forbid)
	real      []netip.AddrPort // real internet hosts that must answer (-allow)
}

var dottedQuad = regexp.MustCompile(`\b\d+\.\d+\.\d+\.\d+\b`)

// named is a probe's name with the addresses that differ between runs and
// Macs called what they are, so a probe is compared with itself: macOS gives
// the VM a network per run.
func (v *fakeVM) named(name string, pl place) string {
	return dottedQuad.ReplaceAllStringFunc(name, func(s string) string {
		a, err := netip.ParseAddr(s)
		b := netip.PrefixFrom(v.ip, 24)
		switch i := slices.Index(pl.own, a); {
		case err != nil:
			return s
		case a == v.gw:
			return "gateway"
		case a == v.ip:
			return "vm"
		case i >= 0:
			return fmt.Sprintf("host%d", i)
		case b.Contains(a) && a.As4()[3] == 255:
			return "vmnet-broadcast"
		case b.Contains(a):
			return "vmnet-neighbour"
		}
		return s
	})
}

var (
	hostTCP = []uint16{22, 25, 80, 88, 443, 445, 465, 548, 587, 631, 3283, 5000, 5432, 5900, 7000, 8080, 11434, 18080, 49152, 65000}
	hostUDP = []uint16{69, 123, 137, 161, 500, 514, 1900, 4500, 5354, 41641, 51820, 65000}
	public  = netip.MustParseAddr("9.9.9.9")
	mailTo  = netip.MustParseAddr("9.9.9.25")
)

// lanTargets are addresses a box must not reach through this Mac.
var lanTargets = []struct {
	group string
	addr  string
}{
	{"lan", "10.77.0.9"}, {"lan", "10.200.1.1"}, {"lan", "172.22.1.1"}, {"lan", "192.168.222.1"}, {"lan", "169.254.169.254"},
	{"tailnet", "100.64.3.4"}, {"tailnet", "100.100.100.100"},
	{"special", "198.18.0.5"}, {"special", "192.0.2.5"}, {"special", "203.0.113.5"},
}

func token(name string) []byte { return []byte("softnet-sim:" + name) }

// probes are every frame the VM sends once it has its address.
func (v *fakeVM) probes(pl place) []probe {
	var ps []probe
	// Ports no earlier run used: this Mac keeps answering an old one's SYNs.
	port := uint16(16000 + rand.IntN(30000))
	next := func() uint16 { port++; return port }
	add := func(p probe) {
		p.name = v.named(p.name, pl)
		ps = append(ps, p)
	}
	ipf := func(dst netip.Addr, proto byte, seg []byte) []byte {
		return eth(v.gwMAC, v.mac, etherIPv4, v.packet(dst, proto, seg).bytes())
	}
	tcpTo := func(group string, w want, c control, dst netip.Addr, dport uint16) {
		sp := next()
		add(probe{group, fmt.Sprintf("tcp %s:%d", dst, dport), ipf(dst, protoTCP, tcp(v.ip, dst, sp, dport, uint32(sp)<<8, 0, flagSYN, nil)), w, c, func(s seen) bool {
			return s.proto == protoTCP && s.dport == sp && s.sport == dport && s.flags&(flagSYN|flagACK) == flagSYN|flagACK
		}})
	}
	udpTo := func(group string, w want, c control, dst netip.Addr, dport uint16, data []byte) {
		sp := next()
		add(probe{group, fmt.Sprintf("udp %s:%d", dst, dport), ipf(dst, protoUDP, udp(v.ip, dst, sp, dport, data)), w, c, func(s seen) bool {
			return s.proto == protoUDP && s.dport == sp && len(s.data) > 0
		}})
	}
	ping := func(group string, w want, c control, dst netip.Addr) {
		id := next()
		add(probe{group, "icmp echo " + dst.String(), ipf(dst, protoICMP, icmpEcho(8, id, 1, token("ping"))), w, c, func(s seen) bool {
			return s.proto == protoICMP && s.sport == 0 && s.dport == id
		}})
	}

	// What must work.
	udpTo("dns", answered, controlAnswer, v.gw, 53, dnsQuery(0x7001, "github.com"))
	tcpTo("dns", answered, controlAnswer, v.gw, 53)
	for _, p := range []uint16{22, 80, 443, 993, 2525, 8443} {
		tcpTo("public", answered, controlAnswer, public, p)
	}
	for _, p := range []uint16{53, 123, 443} {
		udpTo("public", answered, controlAnswer, public, p, token("public"))
	}
	ping("public", answered, controlAnswer, public)
	for _, a := range pl.real {
		tcpTo("internet", answered, controlAnswer, a.Addr(), a.Port())
	}

	// Mail, to anywhere, and shapes of it a filter could miss.
	for _, p := range []uint16{25, 465, 587} {
		tcpTo("mail", stopped, controlAnswer, mailTo, p)
		for _, fl := range []struct {
			name string
			f    byte
		}{{"ack", flagACK}, {"fin", flagFIN}, {"push-ack", flagACK | flagPSH}, {"syn-ack", flagSYN | flagACK}, {"none", 0}, {"all", 63}} {
			sp := next()
			add(probe{"mail", fmt.Sprintf("tcp %s flags %s:%d", fl.name, mailTo, p), ipf(mailTo, protoTCP, tcp(v.ip, mailTo, sp, p, 1, 1, fl.f, nil)), stopped, controlOut, nil})
		}
		seg := tcp(v.ip, mailTo, next(), p, 1, 0, flagSYN, nil)
		f := v.packet(mailTo, protoTCP, seg[:16])
		f.frag = 0x2000
		add(probe{"mail", fmt.Sprintf("tcp first fragment %s:%d", mailTo, p), eth(v.gwMAC, v.mac, etherIPv4, f.bytes()), stopped, controlOut, nil})
		f = v.packet(mailTo, protoTCP, seg[:8])
		f.frag = 0x2000
		add(probe{"mail", fmt.Sprintf("tcp 8-byte first fragment %s:%d", mailTo, p), eth(v.gwMAC, v.mac, etherIPv4, f.bytes()), stopped, controlNone, nil})
		o := v.packet(mailTo, protoTCP, seg)
		o.options = []byte{1, 1, 1, 1}
		add(probe{"mail", fmt.Sprintf("tcp with IP options %s:%d", mailTo, p), eth(v.gwMAC, v.mac, etherIPv4, o.bytes()), stopped, controlNone, nil})
	}

	// This Mac, at each of its addresses: a public one (Softnet lets a global
	// address out unless a rule blocks it) in a group of its own.
	for _, a := range pl.hostAddrs {
		group := "host"
		if !a.IsPrivate() && !netip.MustParsePrefix("100.64.0.0/10").Contains(a) {
			group = "host-public"
		}
		for _, p := range hostTCP {
			tcpTo(group, stopped, controlAnswer, a, p)
		}
		for _, p := range hostUDP {
			c := controlAnswer
			if p == 137 { // netbiosd has it, and does not answer this
				c = controlOut
			}
			udpTo(group, stopped, c, a, p, token("host"))
		}
		ping(group, stopped, controlAnswer, a)
	}
	for _, p := range []uint16{54, 5353} {
		udpTo("host", stopped, controlOut, v.gw, p, dnsQuery(0x7002, "github.com"))
	}
	sp := next()
	f := v.packet(v.gw, protoUDP, udp(v.ip, v.gw, sp, 53, dnsQuery(0x7003, "github.com"))[:24])
	f.frag = 0x2000
	add(probe{"host", "udp first fragment to the gateway's port 53", eth(v.gwMAC, v.mac, etherIPv4, f.bytes()), stopped, controlOut, nil})
	f = v.packet(v.gw, protoUDP, bytes.Repeat([]byte{0x53}, 24))
	f.frag = 8
	add(probe{"host", "udp later fragment to the gateway", eth(v.gwMAC, v.mac, etherIPv4, f.bytes()), stopped, controlOut, nil})
	for _, pr := range []byte{2, 4, 41, 47, 50, 132} {
		add(probe{"host", fmt.Sprintf("ip protocol %d to the gateway", pr), ipf(v.gw, pr, bytes.Repeat([]byte{pr}, 32)), stopped, controlOut, nil})
	}

	// The LAN, the tailnet, special addresses.
	for _, t := range lanTargets {
		a := netip.MustParseAddr(t.addr)
		for _, p := range []uint16{22, 53, 80, 443} {
			tcpTo(t.group, stopped, controlAnswer, a, p)
		}
		for _, p := range []uint16{53, 161, 41641} {
			udpTo(t.group, stopped, controlAnswer, a, p, token(t.group))
		}
		ping(t.group, stopped, controlAnswer, a)
	}
	for _, a := range pl.extra {
		tcpTo("real-lan", stopped, controlAnswer, a.Addr(), a.Port())
	}
	bcast := v.ip.As4()
	bcast[3] = 255
	for _, a := range []netip.Addr{netip.MustParseAddr("240.0.0.1"), netip.AddrFrom4(bcast), allIP} {
		sp := next()
		add(probe{"special", "udp " + a.String() + ":5353", eth(broadcastMAC, v.mac, etherIPv4, v.packet(a, protoUDP, udp(v.ip, a, sp, 5353, token("multicast"))).bytes()), stopped, controlOut, nil})
	}
	for _, a := range []string{"127.0.0.1", "0.0.0.0"} {
		tcpTo("special", stopped, controlOut, netip.MustParseAddr(a), 22)
	}

	// A VM that says it is another machine.
	other := mac{2, 0, 0x5e, 0x10, 0, 0x99}
	neighbour := v.ip.As4()
	neighbour[3] ^= 0x15
	if neighbour[3] == 255 { // the broadcast address is another probe's
		neighbour[3] = v.ip.As4()[3] ^ 0x0b
	}
	nb := netip.AddrFrom4(neighbour)
	dnsFrom := func(src netip.Addr, m mac) []byte {
		p := v.packet(v.gw, protoUDP, udp(src, v.gw, next(), 53, dnsQuery(0x7004, "github.com")))
		p.src = src
		return eth(v.gwMAC, m, etherIPv4, p.bytes())
	}
	pubFrom := func(src netip.Addr, m mac) []byte {
		sp := next()
		p := v.packet(public, protoTCP, tcp(src, public, sp, 443, 9, 0, flagSYN, nil))
		p.src = src
		return eth(v.gwMAC, m, etherIPv4, p.bytes())
	}
	spoof := func(name string, f []byte) { add(probe{"spoof", name, f, stopped, controlNone, nil}) }
	spoof("dns with another source MAC", dnsFrom(v.ip, other))
	spoof("public tcp with another source MAC", pubFrom(v.ip, other))
	spoof("public tcp with the gateway's source MAC", pubFrom(v.ip, v.gwMAC))
	for _, s := range []netip.Addr{nb, v.gw, anyIP, public, netip.MustParseAddr("127.0.0.1")} {
		spoof("dns with source address "+s.String(), dnsFrom(s, v.mac))
		spoof("public tcp with source address "+s.String(), pubFrom(s, v.mac))
	}
	spoof("arp reply claiming the gateway", eth(broadcastMAC, v.mac, etherARP, arp(2, v.mac, v.gw, broadcastMAC, nb)))
	spoof("arp request claiming the gateway", eth(broadcastMAC, v.mac, etherARP, arp(1, v.mac, v.gw, mac{}, v.gw)))
	spoof("arp reply claiming a neighbour", eth(broadcastMAC, v.mac, etherARP, arp(2, v.mac, nb, v.gwMAC, v.gw)))
	spoof("arp with another sender MAC inside", eth(broadcastMAC, v.mac, etherARP, arp(1, other, v.ip, mac{}, v.gw)))
	spoof("arp with another Ethernet source", eth(broadcastMAC, other, etherARP, arp(1, v.mac, v.ip, mac{}, v.gw)))
	spoof("arp operation 3", eth(broadcastMAC, v.mac, etherARP, arp(3, v.mac, v.ip, mac{}, v.gw)))
	spoof("dhcp request for another MAC", dhcpFrame(v.mac, anyIP, allIP, broadcastMAC, dhcpMessage(1, other, 0x9901, netip.Addr{})))
	spoof("dhcp request from another Ethernet source", dhcpFrame(other, anyIP, allIP, broadcastMAC, dhcpMessage(1, other, 0x9902, netip.Addr{})))
	spoof("dhcp renewal for another MAC", dhcpFrame(v.mac, v.ip, v.gw, v.gwMAC, dhcpMessage(3, other, 0x9903, v.ip)))
	spoof("dhcp server answer from the VM", eth(broadcastMAC, v.mac, etherIPv4, ipv4{src: v.ip, dst: allIP, proto: protoUDP, id: 0x99, payload: udp(v.ip, allIP, 67, 68, dhcpMessage(5, other, 0x9904, netip.Addr{}))}.bytes()))

	// Frames that are not IPv4 as Softnet knows it.
	odd := func(name string, f []byte) { add(probe{"odd", name, f, stopped, controlNone, nil}) }
	for _, a := range []struct{ name, to string }{{"ipv6 echo all nodes", "ff02::1"}, {"ipv6 echo all routers", "ff02::2"}, {"ipv6 echo public", "2606:4700:4700::1111"}, {"ipv6 echo unique-local", "fd00::1"}} {
		odd(a.name, eth(mac{0x33, 0x33, 0, 0, 0, 1}, v.mac, etherIPv6, ipv6(v.mac, a.to, 58, []byte{128, 0, 0, 0, 0, 1, 0, 1, 's', 'i', 'm', '6'})))
	}
	odd("ipv6 router solicitation", eth(mac{0x33, 0x33, 0, 0, 0, 2}, v.mac, etherIPv6, ipv6(v.mac, "ff02::2", 58, []byte{133, 0, 0, 0, 0, 0, 0, 0})))
	odd("ipv6 tcp 443 public", eth(v.gwMAC, v.mac, etherIPv6, ipv6(v.mac, "2606:4700:4700::1111", 6, tcpV6(v.mac, "2606:4700:4700::1111", next(), 443))))
	odd("ipv6 tcp 25 public", eth(v.gwMAC, v.mac, etherIPv6, ipv6(v.mac, "2606:4700:4700::1111", 6, tcpV6(v.mac, "2606:4700:4700::1111", next(), 25))))
	odd("ipv6 dhcp", eth(mac{0x33, 0x33, 0, 1, 0, 2}, v.mac, etherIPv6, ipv6(v.mac, "ff02::1:2", 17, bytes.Repeat([]byte{1}, 20))))
	inner := v.packet(public, protoTCP, tcp(v.ip, public, next(), 443, 5, 0, flagSYN, nil)).bytes()
	odd("vlan-tagged ipv4", eth(v.gwMAC, v.mac, 0x8100, append([]byte{0, 10, 8, 0}, inner...)))
	odd("qinq-tagged ipv4", eth(v.gwMAC, v.mac, 0x88a8, append([]byte{0, 10, 0x81, 0, 0, 10, 8, 0}, inner...)))
	odd("pppoe", eth(v.gwMAC, v.mac, 0x8864, append([]byte{0x11, 0, 0, 1, 0, 50, 0, 0x21}, inner...)))
	odd("lldp", eth(mac{1, 0x80, 0xc2, 0, 0, 0x0e}, v.mac, 0x88cc, []byte{2, 7, 4, 1, 2, 3, 4, 5, 6, 0, 0}))
	odd("spanning tree", eth(mac{1, 0x80, 0xc2, 0, 0, 0}, v.mac, 38, append([]byte{0x42, 0x42, 3}, make([]byte, 35)...)))
	odd("rarp", eth(broadcastMAC, v.mac, 0x8035, arp(3, v.mac, anyIP, v.mac, anyIP)))
	odd("ethertype 0x88b5", eth(broadcastMAC, v.mac, 0x88b5, token("experimental")))
	odd("ipv4 ethertype holding ipv6", eth(v.gwMAC, v.mac, etherIPv4, ipv6(v.mac, "ff02::1", 58, []byte{128, 0, 0, 0, 0, 9, 0, 9})))
	odd("ipv4 cut short", eth(v.gwMAC, v.mac, etherIPv4, inner[:15]))
	bad := append([]byte(nil), inner...)
	bad[0] = 0x43
	odd("ipv4 header length 12", eth(v.gwMAC, v.mac, etherIPv4, bad))
	bad = append([]byte(nil), inner...)
	be.PutUint16(bad[2:], 9000)
	odd("ipv4 total length past the frame", eth(v.gwMAC, v.mac, etherIPv4, bad))
	lsrr := v.packet(public, protoTCP, tcp(v.ip, public, next(), 443, 6, 0, flagSYN, nil))
	lsrr.options = []byte{131, 7, 4, 10, 77, 0, 9, 0}
	odd("ipv4 source route to the LAN", eth(v.gwMAC, v.mac, etherIPv4, lsrr.bytes()))

	// No promise is made of these.
	for _, pr := range []byte{2, 4, 41, 47, 50, 132, 253} {
		add(probe{"parity", fmt.Sprintf("ip protocol %d public", pr), ipf(public, pr, bytes.Repeat([]byte{pr}, 32)), parity, controlNone, nil})
	}
	for _, fr := range []struct {
		name string
		f    uint16
	}{{"first", 0x2000}, {"later", 64}} {
		p := v.packet(public, protoUDP, udp(v.ip, public, next(), 443, bytes.Repeat([]byte{1}, 40)))
		p.frag = fr.f
		add(probe{"parity", "udp " + fr.name + " fragment public", eth(v.gwMAC, v.mac, etherIPv4, p.bytes()), parity, controlNone, nil})
		p = v.packet(public, protoTCP, tcp(v.ip, public, next(), 443, 1, 0, flagSYN, bytes.Repeat([]byte{1}, 40)))
		p.frag = fr.f
		add(probe{"parity", "tcp " + fr.name + " fragment public", eth(v.gwMAC, v.mac, etherIPv4, p.bytes()), parity, controlNone, nil})
	}
	for _, fl := range []byte{flagACK, flagFIN, flagRST, flagSYN | flagACK, 0, 63} {
		add(probe{"parity", fmt.Sprintf("tcp flags %d public:443 with no connection", fl), ipf(public, protoTCP, tcp(v.ip, public, next(), 443, 1, 1, fl, nil)), parity, controlNone, nil})
	}
	for _, p := range []uint16{25, 465, 587} {
		udpTo("parity", parity, controlNone, mailTo, p, token("udp mail port"))
	}
	add(probe{"parity", "udp public, wrong checksum", func() []byte {
		u := udp(v.ip, public, next(), 443, token("bad sum"))
		u[6] ^= 0xff
		return ipf(public, protoUDP, u)
	}(), parity, controlNone, nil})
	add(probe{"parity", "udp source port 0 public", ipf(public, protoUDP, udp(v.ip, public, 0, 443, token("port 0"))), parity, controlNone, nil})
	return ps
}

// early are the frames a VM sends before it has an address.
func early(m mac, gwGuess netip.Addr) []probe {
	other := mac{2, 0, 0x5e, 0x10, 0, 0x98}
	ip := netip.MustParseAddr("192.168.64.50")
	mk := func(name string, w want, f []byte) probe { return probe{"before-lease", name, f, w, controlNone, nil} }
	return []probe{
		mk("public tcp", stopped, eth(broadcastMAC, m, etherIPv4, ipv4{src: ip, dst: public, proto: protoTCP, id: 0x101, payload: tcp(ip, public, 999, 443, 1, 0, flagSYN, nil)}.bytes())),
		mk("public tcp from 0.0.0.0", stopped, eth(broadcastMAC, m, etherIPv4, ipv4{src: anyIP, dst: public, proto: protoTCP, id: 0x102, payload: tcp(anyIP, public, 999, 443, 1, 0, flagSYN, nil)}.bytes())),
		mk("dns to the gateway", stopped, eth(broadcastMAC, m, etherIPv4, ipv4{src: ip, dst: gwGuess, proto: protoUDP, id: 0x103, payload: udp(ip, gwGuess, 999, 53, dnsQuery(1, "github.com"))}.bytes())),
		mk("arp claiming the gateway", stopped, eth(broadcastMAC, m, etherARP, arp(2, m, gwGuess, broadcastMAC, ip))),
		mk("arp for an address it has no lease for", stopped, eth(broadcastMAC, m, etherARP, arp(1, m, ip, mac{}, gwGuess))),
		mk("dhcp for another MAC", stopped, dhcpFrame(m, anyIP, allIP, broadcastMAC, dhcpMessage(1, other, 0x9801, netip.Addr{}))),
		mk("dhcp port 67 to the gateway from 0.0.0.0", stopped, dhcpFrame(m, anyIP, gwGuess, broadcastMAC, dhcpMessage(1, m, 0x9802, netip.Addr{}))),
		mk("ipv6 router solicitation", stopped, eth(mac{0x33, 0x33, 0, 0, 0, 2}, m, etherIPv6, ipv6(m, "ff02::2", 58, []byte{133, 0, 0, 0, 0, 0, 0, 1}))),
		mk("arp probe from 0.0.0.0", parity, eth(broadcastMAC, m, etherARP, arp(1, m, anyIP, mac{}, gwGuess))),
	}
}

// sendAll sends the probes and says what became of each.
func (v *fakeVM) sendAll(ps []probe) []result {
	var mu sync.Mutex // a watcher may still run as it is stopped
	replies := make([]bool, len(ps))
	stop := v.watch(func(s seen, _ []byte) {
		if !s.ip {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		for i, p := range ps {
			if p.reply != nil && p.reply(s) {
				replies[i] = true
			}
		}
	})
	for _, p := range ps {
		v.send(p.frame)
		time.Sleep(4 * time.Millisecond)
	}
	time.Sleep(3 * time.Second)
	stop()
	mu.Lock()
	defer mu.Unlock()
	out := make([]result, len(ps))
	for i, p := range ps {
		out[i] = result{Group: p.group, Name: p.name, Answered: replies[i], Out: v.wire.any(func(f []byte) bool {
			return bytes.HasPrefix(f, p.frame) || len(f) >= 14 && bytes.HasPrefix(p.frame, f) && len(p.frame)-len(f) < 4
		})}
	}
	return out
}

func dnsQuery(id uint16, name string) []byte {
	b := be.AppendUint16(nil, id)
	b = append(b, 1, 0, 0, 1, 0, 0, 0, 0, 0, 0)
	for _, l := range bytes.Split([]byte(name), []byte(".")) {
		b = append(append(b, byte(len(l))), l...)
	}
	return append(b, 0, 0, 1, 0, 1)
}

// dnsA is the first A record of a DNS answer.
func dnsA(b []byte) (netip.Addr, bool) {
	for i := 12; i+14 <= len(b); i++ {
		if bytes.Equal(b[i:i+4], []byte{0, 1, 0, 1}) && b[i+8] == 0 && b[i+9] == 4 && b[i-2]&0xc0 == 0xc0 {
			return netip.AddrFrom4([4]byte(b[i+10 : i+14])), true
		}
	}
	return netip.Addr{}, false
}

func linkLocal(m mac) netip.Addr {
	b := [16]byte{0xfe, 0x80, 8: m[0] ^ 2, 9: m[1], 10: m[2], 11: 0xff, 12: 0xfe, 13: m[3], 14: m[4], 15: m[5]}
	return netip.AddrFrom16(b)
}

func ipv6(m mac, to string, proto byte, payload []byte) []byte {
	s, d := linkLocal(m), netip.MustParseAddr(to)
	if proto == 58 {
		payload = append([]byte(nil), payload...)
		be.PutUint16(payload[2:], l4sum(s, d, proto, payload))
	}
	b := make([]byte, 40)
	b[0], b[6], b[7] = 0x60, proto, 255
	be.PutUint16(b[4:], uint16(len(payload)))
	copy(b[8:], s.AsSlice())
	copy(b[24:], d.AsSlice())
	return append(b, payload...)
}

func tcpV6(m mac, to string, sport, dport uint16) []byte {
	return tcp(linkLocal(m), netip.MustParseAddr(to), sport, dport, 1, 0, flagSYN, nil)
}

// listeners answer on this Mac: TCP by accepting, UDP by echoing.
func listeners() (func(), []string) {
	var closers []func() error
	var notes []string
	for _, p := range hostTCP {
		l, err := net.Listen("tcp4", fmt.Sprintf("0.0.0.0:%d", p))
		if err != nil {
			notes = append(notes, fmt.Sprintf("tcp %d is another program's here", p))
			continue
		}
		closers = append(closers, l.Close)
		go func() {
			for {
				c, err := l.Accept()
				if err != nil {
					return
				}
				c.Close()
			}
		}()
	}
	for _, p := range hostUDP {
		c, err := net.ListenPacket("udp4", fmt.Sprintf("0.0.0.0:%d", p))
		if err != nil {
			notes = append(notes, fmt.Sprintf("udp %d is another program's here", p))
			continue
		}
		closers = append(closers, c.Close)
		go func() {
			b := make([]byte, 2048)
			for {
				n, a, err := c.ReadFrom(b)
				if err != nil {
					return
				}
				c.WriteTo(b[:n], a)
			}
		}()
	}
	return func() {
		for _, c := range closers {
			c()
		}
	}, notes
}
