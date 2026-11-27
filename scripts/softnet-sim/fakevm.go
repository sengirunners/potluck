//go:build darwin

package main

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// fakeVM is Softnet run as Tart runs it, with this program in the guest's
// place on the datagram socket.
type fakeVM struct {
	fd     *os.File
	cmd    *exec.Cmd
	stderr bytes.Buffer
	exit   chan error
	mac    mac
	ifname string // the vmenet interface Softnet's vmnet interface made

	ip, gw   netip.Addr
	gwMAC    mac
	dns      []netip.Addr
	answers  atomic.Bool // answers ARP, SYNs, datagrams and pings sent to it
	rec      record
	wireTap  *tap
	wire     record // the vmenet interface: what left Softnet, what came for the VM
	mu       sync.Mutex
	watchers map[int]func(seen, []byte)
	nextW    int
	ipID     uint16
	unsent   []string // why, for each frame softnet could not be handed
}

// invoker is the ordinary user Softnet is started as: its setuid bit, not
// this program's root, is what gives it root.
type invoker struct{ uid, gid uint32 }

func vmenets() map[string]bool {
	out := map[string]bool{}
	ifs, _ := net.Interfaces()
	for _, i := range ifs {
		if strings.HasPrefix(i.Name, "vmenet") {
			out[i.Name] = true
		}
	}
	return out
}

// startVM starts softnet with these rules and waits for its interface.
func startVM(softnet string, as invoker, m mac, block, allow string, undo *undoer) (*fakeVM, error) {
	v := &fakeVM{mac: m, exit: make(chan error, 1), watchers: map[int]func(seen, []byte){}, ipID: 0x4000}
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_DGRAM, 0)
	if err != nil {
		return nil, err
	}
	for _, fd := range fds { // Tart's buffer sizes
		syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_RCVBUF, 4<<20)
		syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_SNDBUF, 1<<20)
	}
	syscall.CloseOnExec(fds[0])
	syscall.CloseOnExec(fds[1])
	syscall.SetNonblock(fds[0], true)
	v.fd = os.NewFile(uintptr(fds[0]), "vm")
	theirs := os.NewFile(uintptr(fds[1]), "softnet")
	args := []string{"--vm-fd", "0", "--vm-mac-address", net.HardwareAddr(m[:]).String()}
	if block != "" {
		args = append(args, "--block", block)
	}
	if allow != "" {
		args = append(args, "--allow", allow)
	}
	before := vmenets()
	v.cmd = exec.Command(softnet, args...)
	v.cmd.Stdin, v.cmd.Stderr, v.cmd.Dir = theirs, &v.stderr, "/"
	v.cmd.Env = []string{"PATH=/usr/bin:/bin:/usr/sbin:/sbin"}
	v.cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: as.uid, Gid: as.gid}, Setpgid: true}
	if err := v.cmd.Start(); err != nil {
		return nil, err
	}
	undo.add(func() { v.cmd.Process.Signal(syscall.SIGINT) }) // once it has ended, this does nothing
	theirs.Close()
	go func() { v.exit <- v.cmd.Wait() }()
	until := time.Now().Add(20 * time.Second)
	for v.ifname == "" {
		for n := range vmenets() {
			if !before[n] {
				v.ifname = n
			}
		}
		select {
		case err := <-v.exit:
			v.fd.Close()
			return nil, fmt.Errorf("softnet ended before its interface was up: %v: %s", err, strings.TrimSpace(v.stderr.String()))
		case <-time.After(50 * time.Millisecond):
		}
		if time.Now().After(until) {
			v.stop()
			return nil, fmt.Errorf("softnet made no vmenet interface in 20 s: %s", strings.TrimSpace(v.stderr.String()))
		}
	}
	if v.wireTap, err = openTap(v.ifname); err != nil {
		v.stop()
		return nil, err
	}
	go v.wireTap.each(v.wire.add)
	go v.read()
	return v, nil
}

// stop ends Softnet the way Tart does (SIGINT) and says how it ended.
func (v *fakeVM) stop() error {
	v.cmd.Process.Signal(syscall.SIGINT)
	var err error
	select {
	case err = <-v.exit:
	case <-time.After(10 * time.Second):
		v.cmd.Process.Kill()
		err = errors.New("softnet did not end on SIGINT in 10 s")
	}
	if v.wireTap != nil {
		v.wireTap.f.Close()
	}
	v.fd.Close()
	return err
}

// send hands Softnet a frame, again while its socket is full. A frame it
// could not hand over is counted: nothing is known of that probe.
func (v *fakeVM) send(f []byte) {
	for i := 0; ; i++ {
		_, err := v.fd.Write(f)
		if err == nil {
			return
		}
		if !errors.Is(err, syscall.ENOBUFS) || i == 50 {
			v.mu.Lock()
			v.unsent = append(v.unsent, err.Error())
			v.mu.Unlock()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// canary sends what every rule set lets through, an ARP request for the
// gateway, and says what did not see it ("" when the interface's tap and the
// VM's socket both did): a silence before it means something then.
func (v *fakeVM) canary() string {
	req := eth(broadcastMAC, v.mac, etherARP, arp(1, v.mac, v.ip, mac{}, v.gw))
	out := func(f []byte) bool { return bytes.HasPrefix(f, req) }
	in := func(f []byte) bool {
		s, ok := parse(f)
		return ok && s.typ == etherARP && s.arpOp == 2 && s.from == v.gw
	}
	outs, ins := v.wire.count(out), v.rec.count(in)
	for i := 0; i < 10; i++ {
		v.send(req)
		time.Sleep(300 * time.Millisecond)
		if v.wire.count(out) > outs && v.rec.count(in) > ins {
			return ""
		}
	}
	return fmt.Sprintf("an ARP request for the gateway was not seen: on the interface %v, its answer at the VM %v", v.wire.count(out) > outs, v.rec.count(in) > ins)
}

// lost is every way this program may have missed a frame.
func (v *fakeVM) lost() (out []string) {
	if l := v.wireTap.lost(); l != "" {
		out = append(out, l)
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if len(v.unsent) > 0 {
		out = append(out, fmt.Sprintf("%d frames could not be handed to softnet: %s", len(v.unsent), v.unsent[0]))
	}
	return out
}

func (v *fakeVM) read() {
	buf := make([]byte, 1<<16)
	for {
		n, err := v.fd.Read(buf)
		if err != nil {
			return
		}
		f := append([]byte(nil), buf[:n]...)
		v.rec.add(f)
		s, ok := parse(f)
		if !ok {
			continue
		}
		if v.answers.Load() {
			v.respond(s, f)
		}
		v.mu.Lock()
		ws := make([]func(seen, []byte), 0, len(v.watchers))
		for _, w := range v.watchers {
			ws = append(ws, w)
		}
		v.mu.Unlock()
		for _, w := range ws {
			w(s, f)
		}
	}
}

func (v *fakeVM) respond(s seen, f []byte) {
	if !v.ip.IsValid() {
		return
	}
	switch {
	case s.typ == etherARP && s.arpOp == 1 && s.to == v.ip:
		var sha mac
		copy(sha[:], s.data)
		v.send(eth(s.src, v.mac, etherARP, arp(2, v.mac, v.ip, sha, s.from)))
	case !s.ip || s.to != v.ip:
	case s.proto == protoTCP && s.flags&(flagSYN|flagACK) == flagSYN:
		v.send(eth(s.src, v.mac, etherIPv4, v.packet(s.from, protoTCP, tcp(v.ip, s.from, s.dport, s.sport, 7000, s.seq+1, flagSYN|flagACK, nil)).bytes()))
	case s.proto == protoUDP && s.sport != 67 && s.sport != 53:
		v.send(eth(s.src, v.mac, etherIPv4, v.packet(s.from, protoUDP, udp(v.ip, s.from, s.dport, s.sport, s.data)).bytes()))
	case s.proto == protoICMP && s.sport == 8:
		v.send(eth(s.src, v.mac, etherIPv4, v.packet(s.from, protoICMP, icmpEcho(0, s.dport, seqOf(f), s.data)).bytes()))
	}
}

func (v *fakeVM) packet(dst netip.Addr, proto byte, payload []byte) ipv4 {
	v.mu.Lock()
	v.ipID++
	id := v.ipID
	v.mu.Unlock()
	return ipv4{src: v.ip, dst: dst, proto: proto, id: id, payload: payload}
}

// watch calls fn with every frame Softnet hands the VM, until the returned
// func is called.
func (v *fakeVM) watch(fn func(seen, []byte)) func() {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.nextW++
	k := v.nextW
	v.watchers[k] = fn
	return func() { v.mu.Lock(); delete(v.watchers, k); v.mu.Unlock() }
}

// await sends f (again every 500 ms) until a frame satisfies ok.
func (v *fakeVM) await(f []byte, wait time.Duration, ok func(seen, []byte) bool) bool {
	got := make(chan struct{}, 1)
	defer v.watch(func(s seen, f []byte) {
		if ok(s, f) {
			select {
			case got <- struct{}{}:
			default:
			}
		}
	})()
	until := time.After(wait)
	for {
		v.send(f)
		select {
		case <-got:
			return true
		case <-until:
			return false
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// dhcpMessage is a BOOTP request from hardware address ch.
func dhcpMessage(kind byte, ch mac, xid uint32, ciaddr netip.Addr, options ...[]byte) []byte {
	b := make([]byte, 236)
	b[0], b[1], b[2] = 1, 1, 6
	be.PutUint32(b[4:], xid)
	if ciaddr.IsValid() {
		c := ciaddr.As4()
		copy(b[12:], c[:])
	}
	copy(b[28:], ch[:])
	b = append(b, 0x63, 0x82, 0x53, 0x63, 53, 1, kind)
	for _, o := range options {
		b = append(b, o...)
	}
	b = append(b, 55, 3, 1, 3, 6, 255)
	for len(b) < 300 {
		b = append(b, 0)
	}
	return b
}

func dhcpFrame(src mac, from, to netip.Addr, dst mac, msg []byte) []byte {
	return eth(dst, src, etherIPv4, ipv4{src: from, dst: to, proto: protoUDP, id: 1, payload: udp(from, to, 68, 67, msg)}.bytes())
}

var (
	anyIP = netip.MustParseAddr("0.0.0.0")
	allIP = netip.MustParseAddr("255.255.255.255")
)

func dhcpOption(msg []byte, code byte) []byte {
	if len(msg) < 240 {
		return nil
	}
	for o := msg[240:]; len(o) >= 2 && o[0] != 255; {
		if o[0] == 0 {
			o = o[1:]
			continue
		}
		n := int(o[1])
		if len(o) < 2+n {
			return nil
		}
		if o[0] == code {
			return o[2 : 2+n]
		}
		o = o[2+n:]
	}
	return nil
}

// lease gets an address from this Mac's DHCP server through Softnet, then the
// gateway's hardware address.
func (v *fakeVM) lease() error {
	ask := func(msg []byte, kind byte) []byte {
		var got []byte
		v.await(dhcpFrame(v.mac, anyIP, allIP, broadcastMAC, msg), 15*time.Second, func(s seen, f []byte) bool {
			if s.ip && s.proto == protoUDP && s.sport == 67 && bytes.Equal(dhcpOption(s.data, 53), []byte{kind}) && bytes.Equal(s.data[28:34], v.mac[:]) {
				if got == nil {
					got = append([]byte(nil), s.data...)
				}
				return true
			}
			return false
		})
		return got
	}
	offer := ask(dhcpMessage(1, v.mac, 0x51a0, netip.Addr{}), 2)
	if offer == nil {
		return errors.New("no DHCP offer came through Softnet in 15 s")
	}
	want, _ := netip.AddrFromSlice(offer[16:20])
	sid := dhcpOption(offer, 54)
	w4 := want.As4()
	ack := ask(dhcpMessage(3, v.mac, 0x51a1, netip.Addr{}, append([]byte{50, 4}, w4[:]...), append([]byte{54, 4}, sid...)), 5)
	if ack == nil {
		return errors.New("no DHCP ACK came through Softnet in 15 s")
	}
	v.ip, _ = netip.AddrFromSlice(ack[16:20])
	v.gw, _ = netip.AddrFromSlice(dhcpOption(ack, 3))
	for o := dhcpOption(ack, 6); len(o) >= 4; o = o[4:] {
		a, _ := netip.AddrFromSlice(o[:4])
		v.dns = append(v.dns, a)
	}
	ok := v.await(eth(broadcastMAC, v.mac, etherARP, arp(1, v.mac, v.ip, mac{}, v.gw)), 10*time.Second, func(s seen, _ []byte) bool {
		if s.typ == etherARP && s.arpOp == 2 && s.from == v.gw {
			copy(v.gwMAC[:], s.data)
			return true
		}
		return false
	})
	if !ok {
		return errors.New("the gateway answered no ARP request in 10 s")
	}
	return nil
}
