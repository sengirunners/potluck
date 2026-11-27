//go:build darwin

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"os/signal"
	"slices"
	"sort"
	"strings"
	"syscall"
	"time"

	potlucksoftnet "github.com/sengirunners/potluck/softnet"
	"github.com/sengirunners/potluck/vmhost"
)

// ruleSets are Potluck's rules as a box on this Mac boots with them now (its
// aliases added), the control, and the broken sets.
func ruleSets() ([]ruleSet, error) {
	block, err := vmhost.SoftnetBlockRules()
	if err != nil {
		return nil, err
	}
	allow := vmhost.SoftnetAllowRules()
	with := func(rule string) string { return allow + "," + rule }
	var noOwn, noMulticast []string // without this Mac's own addresses; without the multicast block
	for _, r := range strings.Split(block, ",") {
		if !strings.HasSuffix(r, "/32") {
			noOwn = append(noOwn, r)
		}
		if r != "224.0.0.0/4" {
			noMulticast = append(noMulticast, r)
		}
	}
	return []ruleSet{
		{Name: "potluck", Block: block, Allow: allow},
		{Name: openRules, Allow: "0.0.0.0/0"},
		{Name: "broken: no rules", must: []string{"host", "host-public", "mail", "in-host", "multicast"}},
		{Name: "broken: no block of this Mac's own addresses", Block: strings.Join(noOwn, ","), Allow: allow, must: []string{"host-public"}},
		{Name: "broken: no multicast block", Block: strings.Join(noMulticast, ","), Allow: allow, must: []string{"multicast"}},
		// Potluck's rules before this Mac's own addresses and multicast were
		// blocked: the gateway and mail alone.
		{Name: "broken: the gateway and mail only", Block: strings.Join(slices.DeleteFunc(slices.Clone(noOwn), func(r string) bool { return r == "224.0.0.0/4" }), ","), Allow: allow, must: []string{"host-public", "multicast"}},
		{Name: "broken: no mail block", Block: "@host", Allow: allow, must: []string{"mail"}},
		{Name: "broken: no host block", Block: strings.TrimPrefix(block, "@host,"), Allow: allow, must: []string{"host", "in-host"}},
		{Name: "broken: no DNS", Block: block, must: []string{"dns"}},
		{Name: "broken: one host port", Block: block, Allow: with("out proto tcp @host port 22"), must: []string{"host"}},
		{Name: "broken: the LAN", Block: block, Allow: with("10.0.0.0/8"), must: []string{"lan"}},
		{Name: "broken: the tailnet", Block: block, Allow: with("out 100.64.0.0/10"), must: []string{"tailnet"}},
		{Name: "broken: bridge isolation off", Block: block, Allow: with("0.0.0.0/0"), must: []string{"lan", "peer"}},
		{Name: "broken: one mail port", Block: strings.TrimSuffix(block, ",out proto tcp port 587"), Allow: allow, must: []string{"mail"}},
		{Name: "broken: public blocked", Block: block + ",0.0.0.0/0", Allow: allow, must: []string{"public"}},
		{Name: "broken: a Softnet that filters nothing", Block: block, Allow: allow, mutant: true,
			must: []string{"before-lease", "mail", "host", "host-public", "multicast", "lan", "tailnet", "special", "spoof", "odd", "peer"}},
	}, nil
}

type checker struct {
	softnet, mutant string
	as              invoker
	pl              place
	w               *world
	wants           map[string]probe // group + name
	undo            *undoer
}

var (
	vmMAC   = mac{0x5e, 0x77, 0x6a, 0x01, 0x02, 0x03}
	peerMAC = mac{0x5e, 0x77, 0x6a, 0x01, 0x02, 0x04}
)

func (c *checker) start(rs ruleSet, m mac) (*fakeVM, error) {
	bin := c.softnet
	if rs.mutant {
		bin = c.mutant
	}
	return startVM(bin, c.as, m, rs.Block, rs.Allow, c.undo)
}

func (c *checker) runSet(rs ruleSet) (r runReport) {
	r.ruleSet = rs
	v, err := c.start(rs, vmMAC)
	if err != nil {
		r.Started = err.Error()
		return r
	}
	ended := false
	defer func() {
		if !ended {
			v.stop()
		}
	}()
	// Where the gateway will be, before the VM is told: the bridge its
	// interface is on.
	gwGuess := v.bridgeAddr()
	if !gwGuess.IsValid() {
		r.Started = "the interface's bridge has no address"
		return r
	}
	es := early(v.mac, gwGuess)
	r.Results = v.sendAll(es)
	for _, p := range es {
		c.wants[p.group+" "+p.name] = p
	}
	if err := v.lease(); err != nil {
		r.Started = err.Error()
		return r
	}
	if gwGuess != v.gw {
		r.Harness = append(r.Harness, fmt.Sprintf("the frames before the lease went to %v, not the gateway %v", gwGuess, v.gw))
	}
	r.VM, r.Gateway = v.ip.String(), v.gw.String()
	for _, d := range v.dns {
		r.DNS = append(r.DNS, d.String())
	}
	pl := c.pl
	pl.hostAddrs = append([]netip.Addr{v.gw}, pl.hostAddrs...)
	ps := v.probes(pl)
	for _, p := range ps {
		c.wants[p.group+" "+p.name] = p
	}
	r.Results = append(r.Results, v.sendAll(ps)...)
	r.Results = append(r.Results, c.multicast(v)...)
	canary := func(after string) {
		if why := v.canary(); why != "" {
			r.Harness = append(r.Harness, "after "+after+": "+why)
		}
	}
	canary("the probes")
	if !rs.mutant {
		r.HTTPS = v.https("api.github.com")
	}
	in, apart, missed := c.inbound(v, rs)
	r.Results, r.Apart = append(r.Results, in...), apart
	r.Harness = append(r.Harness, missed...)
	canary("the inbound checks")
	v6 := func(f []byte) bool {
		return len(f) > 14 && be.Uint16(f[12:]) == etherIPv6 && !bytes.Equal(f[6:12], v.mac[:])
	}
	// What this Mac and its vmnet send by themselves, router solicitations and
	// neighbour discovery among them.
	onWire, handed := v.wire.count(v6), v.rec.count(v6)
	r.InboundIPv6 = fmt.Sprintf("%d IPv6 frames on the VM's interface, %d handed to the VM", onWire, handed)
	if onWire == 0 {
		r.Harness = append(r.Harness, "no IPv6 frame came to the VM's interface")
	}
	const ipv6In = "any IPv6 frame"
	c.wants["in-host "+ipv6In] = probe{group: "in-host", name: ipv6In, want: stopped, control: controlNone}
	r.Results = append(r.Results, result{Group: "in-host", Name: ipv6In, Out: handed > 0})
	select {
	case err := <-v.exit:
		r.Ended = fmt.Sprintf("softnet ended by itself before it was stopped: %v: %s", err, strings.TrimSpace(v.stderr.String()))
		v.exit <- err
	default:
	}
	r.Harness = append(r.Harness, v.lost()...)
	// Once it serves the VM it needs root no more.
	if ps, err := run("/bin/ps", "-o", "uid=,ruid=,svuid=", "-p", fmt.Sprint(v.cmd.Process.Pid)); err == nil {
		r.Process = strings.Fields(ps)
	}
	err = v.stop()
	ended = true
	if r.Ended == "" {
		r.Ended = "on SIGINT, exit status 0"
		if err != nil {
			r.Ended = "on SIGINT: " + err.Error()
		}
	}
	return r
}

// https is one real request through Softnet: the name looked up at the
// gateway, then TLS with the server's certificate checked.
func (v *fakeVM) https(host string) string {
	var ip netip.Addr
	ok := v.await(eth(v.gwMAC, v.mac, etherIPv4, v.packet(v.gw, protoUDP, udp(v.ip, v.gw, 41999, 53, dnsQuery(0x7777, host))).bytes()), 5*time.Second, func(s seen, _ []byte) bool {
		if s.proto != protoUDP || s.sport != 53 || s.dport != 41999 {
			return false
		}
		a, ok := dnsA(s.data)
		if ok {
			ip = a
		}
		return ok
	})
	if !ok {
		return "failed: no DNS answer for " + host
	}
	cl := &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{
		TLSClientConfig: &tls.Config{ServerName: host},
		DialContext: func(context.Context, string, string) (net.Conn, error) {
			return v.dial(netip.AddrPortFrom(ip, 443), 10*time.Second)
		},
	}}
	resp, err := cl.Get("https://" + host + "/zen")
	if err != nil {
		return "failed: " + err.Error()
	}
	resp.Body.Close()
	return fmt.Sprintf("ok: GET https://%s/zen answered %s from the address the gateway's DNS gave", host, resp.Status)
}

// multicast is what programs on this Mac that joined a multicast group on the
// VM's bridge get from the VM, and what the Mac's mDNS responder answers.
// Out is "a program on this Mac got it".
func (c *checker) multicast(v *fakeVM) []result {
	var out []result
	add := func(name string, w want, ctl control, got bool) {
		c.wants["multicast "+name] = probe{group: "multicast", name: name, want: w, control: ctl}
		out = append(out, result{Group: "multicast", Name: name, Out: got})
	}
	ifi := bridgeOf(v.gw)
	for _, g := range []netip.AddrPort{netip.MustParseAddrPort("224.0.0.251:5353"), netip.MustParseAddrPort("239.255.255.250:3702"), netip.MustParseAddrPort("239.77.77.77:7777")} {
		k, err := net.ListenMulticastUDP("udp4", ifi, net.UDPAddrFromAddrPort(g))
		if err != nil {
			c.wants["multicast listener "+g.String()] = probe{group: "multicast", want: answered}
			out = append(out, result{Group: "multicast", Name: "listener " + g.String()})
			continue
		}
		tok := token("multicast " + g.String())
		got := make(chan bool, 1)
		go func() {
			b := make([]byte, 2048)
			k.SetReadDeadline(time.Now().Add(3 * time.Second))
			for {
				n, _, err := k.ReadFrom(b)
				if err != nil {
					got <- false
					return
				}
				if bytes.Contains(b[:n], tok) {
					got <- true
					return
				}
			}
		}()
		a := g.Addr().As4()
		for i := 0; i < 3; i++ {
			v.send(eth(mac{1, 0, 0x5e, a[1] & 0x7f, a[2], a[3]}, v.mac, etherIPv4, v.packet(g.Addr(), protoUDP, udp(v.ip, g.Addr(), 35000, g.Port(), tok)).bytes()))
			time.Sleep(200 * time.Millisecond)
		}
		add("a program on this Mac got a datagram to "+g.String(), stopped, controlOut, <-got)
		k.Close()
	}
	group := netip.MustParseAddr("224.0.0.251")
	q := dnsQuery(0, "_services._dns-sd._udp.local")
	be.PutUint16(q[len(q)-4:], 12)     // PTR
	be.PutUint16(q[len(q)-2:], 0x8001) // the answer to this address, not the group
	answer := v.await(eth(mac{1, 0, 0x5e, 0, 0, 0xfb}, v.mac, etherIPv4, v.packet(group, protoUDP, udp(v.ip, group, 5353, 5353, q)).bytes()), 4*time.Second, func(s seen, _ []byte) bool {
		return s.proto == protoUDP && s.sport == 5353
	})
	add("this Mac's mDNS answer reached the VM", stopped, controlOut, answer)
	return out
}

// bridgeOf is the interface that holds gw, the VM's bridge.
func bridgeOf(gw netip.Addr) *net.Interface {
	ifs, _ := net.Interfaces()
	for _, i := range ifs {
		as, _ := i.Addrs()
		for _, a := range as {
			if n, ok := a.(*net.IPNet); ok && n.IP.Equal(gw.AsSlice()) {
				return &i
			}
		}
	}
	return nil
}

// inbound is what others can put into the VM: this Mac, the LAN and a second
// VM at the same time. A result's Out is "the VM got it".
// apart is as in runReport; missed is what it could not check, and why.
func (c *checker) inbound(v *fakeVM, rs ruleSet) (out []result, apart bool, missed []string) {
	v.answers.Store(true)
	got := func(group, name string, w want, ctl control, ok func(seen, []byte) bool) {
		c.wants[group+" "+name] = probe{group: group, name: name, want: w, control: ctl}
		out = append(out, result{Group: group, Name: name, Out: v.rec.any(func(f []byte) bool {
			s, p := parse(f)
			return p && ok(s, f)
		})})
	}
	// This Mac: what it sends must come to the VM's interface to mean
	// anything.
	host := func(name string, ok func(seen, []byte) bool) {
		got("in-host", name, stopped, controlOut, ok)
		if !v.wire.any(func(f []byte) bool { s, p := parse(f); return p && ok(s, f) }) {
			missed = append(missed, "in-host "+name+": no such frame came to the VM's interface")
		}
	}
	for _, p := range []uint16{22, 8080} {
		if k, err := net.DialTimeout("tcp4", fmt.Sprintf("%s:%d", v.ip, p), time.Second); err == nil {
			k.Close()
		}
		host(fmt.Sprintf("tcp %d", p), func(s seen, _ []byte) bool {
			return s.proto == protoTCP && s.from == v.gw && s.dport == p && s.flags&flagSYN != 0
		})
	}
	for _, lp := range []string{":0", ":5300"} {
		if k, err := net.ListenPacket("udp4", lp); err == nil {
			for i := 0; i < 3; i++ {
				k.WriteTo(token("from host "+lp), &net.UDPAddr{IP: v.ip.AsSlice(), Port: 9999})
				time.Sleep(100 * time.Millisecond)
			}
			k.Close()
		}
		time.Sleep(300 * time.Millisecond)
		host("udp from source port "+lp, func(_ seen, f []byte) bool { return bytes.Contains(f, token("from host "+lp)) })
	}
	exec.Command("/sbin/ping", "-c", "2", "-t", "2", v.ip.String()).Run()
	host("icmp echo", func(s seen, _ []byte) bool { return s.proto == protoICMP && s.sport == 8 && s.from == v.gw })

	// The LAN, through this Mac's routing. No promise; compared only.
	if ifi, err := net.InterfaceByName(c.w.near); err == nil && len(ifi.HardwareAddr) == 6 {
		near, lan := mac(ifi.HardwareAddr), netip.MustParseAddr("10.77.0.9")
		for i := 0; i < 3; i++ {
			c.w.send(ipv4{src: lan, dst: v.ip, proto: protoTCP, id: 0x3001, payload: tcp(lan, v.ip, 33001, 8080, 5, 0, flagSYN, nil)}, near)
			c.w.send(ipv4{src: lan, dst: v.ip, proto: protoUDP, id: 0x3002, payload: udp(lan, v.ip, 33002, 9999, token("from lan"))}, near)
			time.Sleep(300 * time.Millisecond)
		}
		got("in-lan", "tcp 8080", parity, controlNone, func(s seen, _ []byte) bool { return s.proto == protoTCP && s.from == lan && s.sport == 33001 })
		got("in-lan", "udp 9999", parity, controlNone, func(_ seen, f []byte) bool { return bytes.Contains(f, token("from lan")) })
	}

	// Another VM, under the same rules.
	p, err := c.start(rs, peerMAC)
	if err != nil {
		return out, false, append(missed, "the second VM did not start: "+err.Error())
	}
	defer func() {
		if err := p.stop(); err != nil {
			missed = append(missed, "the second VM's softnet: "+err.Error())
		}
	}()
	if err := p.lease(); err != nil {
		return out, false, append(missed, "the second VM: "+err.Error())
	}
	apart = p.gw != v.gw
	send := func(dst mac, typ uint16, b []byte) {
		for i := 0; i < 2; i++ {
			p.send(eth(dst, p.mac, typ, b))
			time.Sleep(50 * time.Millisecond)
		}
	}
	pub := netip.MustParseAddr("9.9.9.77")
	send(broadcastMAC, etherARP, arp(1, p.mac, p.ip, mac{}, v.ip))
	send(v.mac, etherIPv4, p.packet(v.ip, protoTCP, tcp(p.ip, v.ip, 34001, 22, 1, 0, flagSYN, nil)).bytes())
	send(v.mac, etherIPv4, p.packet(v.ip, protoUDP, udp(p.ip, v.ip, 34002, 9999, token("peer direct"))).bytes())
	send(v.mac, etherIPv4, p.packet(v.ip, protoICMP, icmpEcho(8, 34003, 1, token("peer ping"))).bytes())
	send(v.mac, etherIPv4, p.packet(pub, protoUDP, udp(p.ip, pub, 34004, 443, token("peer public address"))).bytes())
	send(broadcastMAC, etherIPv4, p.packet(pub, protoUDP, udp(p.ip, pub, 34005, 443, token("peer broadcast frame"))).bytes())
	send(p.gwMAC, etherIPv4, p.packet(v.ip, protoUDP, udp(p.ip, v.ip, 34006, 9999, token("peer by gateway"))).bytes())
	send(v.mac, etherIPv6, ipv6(p.mac, "ff02::1", 58, append([]byte{128, 0, 0, 0, 0, 1, 0, 1}, token("peer ipv6")...)))
	time.Sleep(time.Second)
	got("peer", "arp request", stopped, controlOut, func(s seen, _ []byte) bool { return s.typ == etherARP && s.src == p.mac })
	got("peer", "tcp 22", stopped, controlOut, func(s seen, _ []byte) bool { return s.proto == protoTCP && s.sport == 34001 })
	for _, t := range []string{"peer direct", "peer ping", "peer public address", "peer broadcast frame"} {
		got("peer", t, stopped, controlOut, func(_ seen, f []byte) bool { return bytes.Contains(f, token(t)) })
	}
	got("peer", "peer by gateway", stopped, controlNone, func(_ seen, f []byte) bool { return bytes.Contains(f, token("peer by gateway")) })
	got("peer", "peer ipv6", stopped, controlNone, func(_ seen, f []byte) bool { return bytes.Contains(f, token("peer ipv6")) })
	// A frame of the peer's on this VM's interface, whatever the VM was given.
	c.wants["peer any frame on this VM's interface"] = probe{group: "peer", want: stopped, control: controlOut}
	out = append(out, result{Group: "peer", Name: "any frame on this VM's interface", Out: v.wire.any(func(f []byte) bool {
		return len(f) >= 14 && bytes.Equal(f[6:12], p.mac[:])
	})})
	if why := p.canary(); why != "" {
		missed = append(missed, "the second VM: "+why)
	}
	for _, l := range p.lost() {
		missed = append(missed, "the second VM: "+l)
	}
	return out, apart, missed
}

// bridgeAddr is this Mac's address on the bridge the VM's interface is a
// member of, invalid when it has none in 5 s.
func (v *fakeVM) bridgeAddr() netip.Addr {
	for i := 0; i < 50; i++ {
		for n := 100; n < 110; n++ {
			name := fmt.Sprintf("bridge%d", n)
			out, err := run("/sbin/ifconfig", name)
			if err != nil || !strings.Contains(out, "member: "+v.ifname+" ") {
				continue
			}
			ifi, err := net.InterfaceByName(name)
			if err != nil {
				continue
			}
			as, _ := ifi.Addrs()
			for _, a := range as {
				if n, ok := a.(*net.IPNet); ok && n.IP.To4() != nil {
					ad, _ := netip.AddrFromSlice(n.IP.To4())
					return ad
				}
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return netip.Addr{}
}

func sysctl(n string) string { s, _ := run("/usr/sbin/sysctl", "-n", n); return s }

func main() {
	softnet := flag.String("softnet", "", "the softnet executable to check (root owned and setuid, as Tart runs it)")
	mutant := flag.String("mutant", "", "a softnet that filters nothing (root owned and setuid): the control for what every rule set stops")
	uid := flag.Uint("uid", 501, "the user softnet is started as")
	gid := flag.Uint("gid", 20, "that user's group")
	forbid := flag.String("forbid", "", "comma list of real address:port on the LAN that must stay out of reach")
	allow := flag.String("allow", "1.1.1.1:443", "comma list of real address:port on the internet that must answer")
	only := flag.String("only", "", "run only the rule sets whose name contains this (and potluck and open); such a run does not pass")
	label := flag.String("label", "", "a name for this run in the report")
	jsonOut := flag.String("json", "", "write the report here")
	mdOut := flag.String("md", "", "write a readable report here")
	flag.Parse()
	if *softnet == "" || os.Geteuid() != 0 {
		fmt.Fprintln(os.Stderr, "usage: sudo softnet-sim -softnet <executable> (in a throwaway macOS VM)")
		os.Exit(2)
	}
	if sysctl("kern.hv_vmm_present") != "1" {
		fmt.Fprintln(os.Stderr, "softnet-sim: this is not a virtual machine: it changes routes and addresses, so it runs only in a throwaway macOS VM")
		os.Exit(2)
	}
	if err := check(*softnet, *mutant, *label, *forbid, *allow, *only, *jsonOut, *mdOut, invoker{uint32(*uid), uint32(*gid)}); err != nil {
		fmt.Fprintln(os.Stderr, "softnet-sim:", err)
		os.Exit(1)
	}
}

func addrPorts(list string) (out []netip.AddrPort, err error) {
	for _, s := range strings.Split(list, ",") {
		if s == "" {
			continue
		}
		a, err := netip.ParseAddrPort(s)
		if err != nil {
			return nil, err
		}
		if !a.Addr().Is4() {
			return nil, fmt.Errorf("%s: an IPv4 address and port", s)
		}
		out = append(out, a)
	}
	return out, nil
}

// hostAliases are addresses of this Mac's own besides its gateway and network
// ones: what a tailnet, a second LAN and a VPN give it, and the public
// address of a rented Mac or one put on the internet directly.
var hostAliases = []string{"100.101.5.5", "10.88.0.1", "172.31.200.1", "45.33.40.1"}

func check(softnet, mutant, label, forbid, allow, only, jsonOut, mdOut string, as invoker) error {
	var rep report
	rep.Label, rep.MacOS, rep.Build = label, sysctl("kern.osproductversion"), sysctl("kern.osversion")
	rep.VMMPres, rep.HVSupport = sysctl("kern.hv_vmm_present"), sysctl("kern.hv_support")
	b, err := os.ReadFile(softnet)
	if err != nil {
		return err
	}
	rep.SHA256 = fmt.Sprintf("%x", sha256.Sum256(b))
	rep.Softnet, _ = run(softnet, "--version")
	c := &checker{softnet: softnet, mutant: mutant, as: as, wants: map[string]probe{}, undo: &undoer{}}
	defer c.undo.run()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	signal.Ignore(syscall.SIGPIPE) // a report piped to head
	go func() {
		<-sig
		c.undo.run()
		os.Exit(130)
	}()
	if c.pl.extra, err = addrPorts(forbid); err != nil {
		return err
	}
	if c.pl.real, err = addrPorts(allow); err != nil {
		return err
	}
	// This Mac's network addresses, then the aliases.
	ifs, _ := net.Interfaces()
	for _, i := range ifs {
		if !strings.HasPrefix(i.Name, "en") {
			continue
		}
		as, _ := i.Addrs()
		for _, a := range as {
			if n, ok := a.(*net.IPNet); ok && n.IP.To4() != nil && !n.IP.IsLinkLocalUnicast() {
				ad, _ := netip.AddrFromSlice(n.IP.To4())
				c.pl.own = append(c.pl.own, ad)
			}
		}
	}
	c.pl.hostAddrs = slices.Clone(c.pl.own)
	for _, a := range hostAliases {
		c.undo.add(func() { run("/sbin/ifconfig", "lo0", "-alias", a) })
		if _, err := run("/sbin/ifconfig", "lo0", "alias", a, "netmask", "255.255.255.255"); err != nil {
			return err
		}
		c.pl.hostAddrs = append(c.pl.hostAddrs, netip.MustParseAddr(a))
	}
	for _, a := range c.pl.hostAddrs {
		rep.HostAddrs = append(rep.HostAddrs, a.String())
	}
	if c.w, err = newWorld(c.undo); err != nil {
		return err
	}
	end, notes := listeners()
	defer end()
	rep.Notes = notes

	if mutant != "" {
		b, err := os.ReadFile(mutant)
		if err != nil {
			return err
		}
		rep.MutantSHA256 = fmt.Sprintf("%x", sha256.Sum256(b))
	}
	if !slices.ContainsFunc(potlucksoftnet.Builds, func(b potlucksoftnet.Build) bool { return b.SHA256 == rep.SHA256 }) {
		rep.Notes = append(rep.Notes, "this Softnet is none Potluck pins")
	}
	if forbid == "" {
		rep.Unproven = append(rep.Unproven, "no -forbid: no real host on the LAN was tried")
	}
	if only != "" {
		rep.Unproven = append(rep.Unproven, fmt.Sprintf("only the rule sets named like %q ran", only))
	}
	sets, err := ruleSets()
	if err != nil {
		return err
	}
	for _, rs := range sets {
		if rs.mutant && mutant == "" || len(rs.must) > 0 && !strings.Contains(rs.Name, only) {
			continue
		}
		fmt.Printf("%s ...\n", rs.Name)
		rep.Runs = append(rep.Runs, c.runSet(rs))
		time.Sleep(2 * time.Second)
	}
	rep.verdicts(c.wants)
	if jsonOut != "" {
		j, _ := json.MarshalIndent(rep, "", " ")
		if err := os.WriteFile(jsonOut, append(j, '\n'), 0o644); err != nil {
			return err
		}
	}
	md := rep.text()
	if mdOut != "" {
		if err := os.WriteFile(mdOut, []byte(md), 0o644); err != nil {
			return err
		}
	}
	fmt.Print(md)
	if !rep.Pass {
		return errors.New("this run does not show the rules hold: see its failures and what it did not show")
	}
	return nil
}

func (rep report) text() string {
	var b strings.Builder
	fmt.Fprintf(&b, "# softnet-sim %s\n\nmacOS %s (%s), in a VM: %s, kern.hv_support %s\n%s\nsha256 %s\nthe Softnet that filters nothing: sha256 %s\nthis Mac's addresses: the gateway, %s\n", rep.Label, rep.MacOS, rep.Build, rep.VMMPres, rep.HVSupport, rep.Softnet, rep.SHA256, rep.MutantSHA256, strings.Join(rep.HostAddrs, ", "))
	for _, n := range rep.Notes {
		fmt.Fprintf(&b, "note: %s\n", n)
	}
	for _, n := range rep.Unproven {
		fmt.Fprintf(&b, "not shown: %s\n", n)
	}
	b.WriteString("\n| rules | verdict | probes | left or got in | answered | failing groups | https |\n| --- | --- | --- | --- | --- | --- | --- |\n")
	for _, r := range rep.Runs {
		outN, ans := 0, 0
		for _, x := range r.Results {
			if x.Out {
				outN++
			}
			if x.Answered {
				ans++
			}
		}
		var gs []string
		for g, n := range groupsOf(r.Failures) {
			gs = append(gs, fmt.Sprintf("%s %d", g, n))
		}
		sort.Strings(gs)
		fmt.Fprintf(&b, "| %s | %s | %d | %d | %d | %s | %s |\n", r.Name, r.Verdict, len(r.Results), outN, ans, strings.Join(gs, ", "), r.HTTPS)
	}
	for _, r := range rep.Runs {
		if len(r.must) > 0 {
			continue
		}
		fmt.Fprintf(&b, "\n## %s\n\nblock %s\nallow %s\nVM %s, gateway %s, DNS %s\n%s\n%s\nended %s\n\n", r.Name, r.Block, r.Allow, r.VM, r.Gateway, strings.Join(r.DNS, " "), "effective, real and saved uid: "+strings.Join(r.Process, " "), r.InboundIPv6, r.Ended)
		if r.Apart {
			b.WriteString("macOS gave each VM a network of its own, so the peer checks prove nothing here\n\n")
		}
		for _, f := range r.Failures {
			fmt.Fprintf(&b, "- %s\n", f)
		}
		type n struct{ all, out, ans int }
		per := map[string]*n{}
		var order []string
		for _, x := range r.Results {
			if per[x.Group] == nil {
				per[x.Group] = &n{}
				order = append(order, x.Group)
			}
			per[x.Group].all++
			if x.Out {
				per[x.Group].out++
			}
			if x.Answered {
				per[x.Group].ans++
			}
		}
		b.WriteString("\n| group | probes | left or got in | answered |\n| --- | --- | --- | --- |\n")
		for _, g := range order {
			fmt.Fprintf(&b, "| %s | %d | %d | %d |\n", g, per[g].all, per[g].out, per[g].ans)
		}
	}
	return b.String()
}
