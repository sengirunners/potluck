package main

import (
	"encoding/binary"
	"net/netip"
)

type mac [6]byte

var (
	broadcastMAC = mac{0xff, 0xff, 0xff, 0xff, 0xff, 0xff}
	be           = binary.BigEndian
)

const (
	etherIPv4 = 0x0800
	etherARP  = 0x0806
	etherIPv6 = 0x86dd

	protoICMP = 1
	protoTCP  = 6
	protoUDP  = 17

	flagFIN = 1
	flagSYN = 2
	flagRST = 4
	flagPSH = 8
	flagACK = 16
)

func eth(dst, src mac, typ uint16, payload []byte) []byte {
	b := make([]byte, 0, 14+len(payload))
	b = append(b, dst[:]...)
	b = append(b, src[:]...)
	b = be.AppendUint16(b, typ)
	return append(b, payload...)
}

func sum(b []byte, acc uint32) uint32 {
	for ; len(b) > 1; b = b[2:] {
		acc += uint32(be.Uint16(b))
	}
	if len(b) == 1 {
		acc += uint32(b[0]) << 8
	}
	return acc
}

func fold(acc uint32) uint16 {
	for acc>>16 != 0 {
		acc = acc&0xffff + acc>>16
	}
	return ^uint16(acc)
}

// ipv4 is one IPv4 packet. frag is the flags and fragment offset field;
// options must be a multiple of 4 bytes.
type ipv4 struct {
	src, dst netip.Addr
	proto    byte
	id       uint16
	frag     uint16
	options  []byte
	payload  []byte
}

func (p ipv4) bytes() []byte {
	h := make([]byte, 20+len(p.options))
	h[0] = 0x40 | byte(len(h)/4)
	be.PutUint16(h[2:], uint16(len(h)+len(p.payload)))
	be.PutUint16(h[4:], p.id)
	be.PutUint16(h[6:], p.frag)
	h[8], h[9] = 64, p.proto
	s, d := p.src.As4(), p.dst.As4()
	copy(h[12:], s[:])
	copy(h[16:], d[:])
	copy(h[20:], p.options)
	be.PutUint16(h[10:], fold(sum(h, 0)))
	return append(h, p.payload...)
}

func l4sum(src, dst netip.Addr, proto byte, seg []byte) uint16 {
	s, d := src.AsSlice(), dst.AsSlice()
	acc := sum(d, sum(s, 0)) + uint32(proto) + uint32(len(seg))
	return fold(sum(seg, acc))
}

func udp(src, dst netip.Addr, sport, dport uint16, data []byte) []byte {
	b := make([]byte, 8, 8+len(data))
	be.PutUint16(b[0:], sport)
	be.PutUint16(b[2:], dport)
	be.PutUint16(b[4:], uint16(8+len(data)))
	b = append(b, data...)
	be.PutUint16(b[6:], l4sum(src, dst, protoUDP, b))
	return b
}

func tcp(src, dst netip.Addr, sport, dport uint16, seq, ack uint32, flags byte, data []byte) []byte {
	b := make([]byte, 20, 24+len(data))
	be.PutUint16(b[0:], sport)
	be.PutUint16(b[2:], dport)
	be.PutUint32(b[4:], seq)
	be.PutUint32(b[8:], ack)
	b[12], b[13] = 5<<4, flags
	be.PutUint16(b[14:], 65535)
	if flags&flagSYN != 0 { // MSS 1200
		b = append(b, 2, 4, 0x04, 0xb0)
		b[12] = 6 << 4
	}
	b = append(b, data...)
	be.PutUint16(b[16:], l4sum(src, dst, protoTCP, b))
	return b
}

func icmpEcho(typ byte, id, seq uint16, data []byte) []byte {
	b := make([]byte, 8, 8+len(data))
	b[0] = typ
	be.PutUint16(b[4:], id)
	be.PutUint16(b[6:], seq)
	b = append(b, data...)
	be.PutUint16(b[2:], fold(sum(b, 0)))
	return b
}

func arp(op uint16, sha mac, spa netip.Addr, tha mac, tpa netip.Addr) []byte {
	b := []byte{0, 1, 8, 0, 6, 4}
	b = be.AppendUint16(b, op)
	s, t := spa.As4(), tpa.As4()
	b = append(b, sha[:]...)
	b = append(b, s[:]...)
	b = append(b, tha[:]...)
	return append(b, t[:]...)
}

// seen is a parsed frame: enough to match replies and describe it.
type seen struct {
	dst, src     mac
	typ          uint16
	ip           bool
	from, to     netip.Addr
	proto        byte
	sport, dport uint16 // TCP and UDP; ICMP: type, id
	flags        byte
	seq, ack     uint32
	data         []byte
	arpOp        uint16
}

func parse(f []byte) (seen, bool) {
	var s seen
	if len(f) < 14 {
		return s, false
	}
	copy(s.dst[:], f[0:6])
	copy(s.src[:], f[6:12])
	s.typ = be.Uint16(f[12:])
	p := f[14:]
	switch s.typ {
	case etherARP:
		if len(p) < 28 {
			return s, true
		}
		s.arpOp = be.Uint16(p[6:])
		s.from, _ = netip.AddrFromSlice(p[14:18])
		s.to, _ = netip.AddrFromSlice(p[24:28])
		s.data = p[8:14] // sender hardware address
	case etherIPv4:
		if len(p) < 20 {
			return s, true
		}
		hl, tl := int(p[0]&15)*4, int(be.Uint16(p[2:]))
		if hl < 20 || len(p) < hl || tl < hl {
			return s, true
		}
		if tl < len(p) {
			p = p[:tl]
		}
		s.ip, s.proto = true, p[9]
		s.from, _ = netip.AddrFromSlice(p[12:16])
		s.to, _ = netip.AddrFromSlice(p[16:20])
		if be.Uint16(p[6:])&0x1fff != 0 {
			return s, true // not a first fragment: no ports
		}
		q := p[hl:]
		switch {
		case s.proto == protoUDP && len(q) >= 8:
			s.sport, s.dport, s.data = be.Uint16(q), be.Uint16(q[2:]), q[8:]
		case s.proto == protoTCP && len(q) >= 20:
			s.sport, s.dport, s.seq, s.ack, s.flags = be.Uint16(q), be.Uint16(q[2:]), be.Uint32(q[4:]), be.Uint32(q[8:]), q[13]
			if o := int(q[12]>>4) * 4; o >= 20 && o <= len(q) {
				s.data = q[o:]
			}
		case s.proto == protoICMP && len(q) >= 8:
			s.sport, s.dport, s.data = uint16(q[0]), be.Uint16(q[4:]), q[8:]
		}
	}
	return s, true
}
