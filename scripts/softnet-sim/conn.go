//go:build darwin

package main

import (
	"errors"
	"io"
	"math/rand/v2"
	"net"
	"net/netip"
	"sync"
	"time"
)

// conn is one TCP connection of the fake VM: in order, one segment in flight,
// enough for a TLS request to a real server.
type conn struct {
	v            *fakeVM
	to           netip.AddrPort
	port         uint16
	mu           sync.Mutex
	cond         *sync.Cond
	snd, una     uint32
	rcv          uint32
	in           []byte
	open, closed bool
	unwatch      func()
}

func (v *fakeVM) dial(to netip.AddrPort, wait time.Duration) (net.Conn, error) {
	c := &conn{v: v, to: to, port: uint16(50000 + rand.IntN(10000)), snd: rand.Uint32()}
	c.cond = sync.NewCond(&c.mu)
	c.unwatch = v.watch(c.take)
	syn := c.snd
	c.mu.Lock()
	defer c.mu.Unlock()
	until := time.Now().Add(wait)
	for !c.open && time.Now().Before(until) && !c.closed {
		c.seg(flagSYN, syn, nil)
		c.pause(time.Second, func() bool { return c.open || c.closed })
	}
	if !c.open {
		c.unwatch()
		return nil, errors.New("no SYN-ACK from " + to.String())
	}
	return c, nil
}

func (c *conn) pause(d time.Duration, done func() bool) {
	t := time.AfterFunc(d, func() { c.mu.Lock(); c.cond.Broadcast(); c.mu.Unlock() })
	defer t.Stop()
	until := time.Now().Add(d)
	for !done() && time.Now().Before(until) {
		c.cond.Wait()
	}
}

func (c *conn) seg(flags byte, seq uint32, data []byte) {
	p := c.v.packet(c.to.Addr(), protoTCP, tcp(c.v.ip, c.to.Addr(), c.port, c.to.Port(), seq, c.rcv, flags, data))
	c.v.send(eth(c.v.gwMAC, c.v.mac, etherIPv4, p.bytes()))
}

func (c *conn) take(s seen, _ []byte) {
	if !s.ip || s.proto != protoTCP || s.from != c.to.Addr() || s.sport != c.to.Port() || s.dport != c.port {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	defer c.cond.Broadcast()
	switch {
	case s.flags&flagRST != 0:
		c.closed = true
		return
	case !c.open:
		if s.flags&(flagSYN|flagACK) == flagSYN|flagACK {
			c.snd++
			c.una, c.rcv, c.open = c.snd, s.seq+1, true
			c.seg(flagACK, c.snd, nil)
		}
		return
	}
	if int32(s.ack-c.una) > 0 {
		c.una = s.ack
	}
	if len(s.data) > 0 || s.flags&flagFIN != 0 {
		if s.seq == c.rcv {
			c.in = append(c.in, s.data...)
			c.rcv += uint32(len(s.data))
			if s.flags&flagFIN != 0 {
				c.rcv++
				c.closed = true
			}
		}
		c.seg(flagACK, c.snd, nil)
	}
}

func (c *conn) Read(b []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	until := time.Now().Add(15 * time.Second)
	for len(c.in) == 0 && !c.closed && time.Now().Before(until) {
		c.pause(time.Second, func() bool { return len(c.in) > 0 || c.closed })
	}
	if len(c.in) == 0 {
		if c.closed {
			return 0, io.EOF
		}
		return 0, errors.New("read timed out")
	}
	n := copy(b, c.in)
	c.in = c.in[n:]
	return n, nil
}

func (c *conn) Write(b []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	sent := 0
	for sent < len(b) {
		n := min(1000, len(b)-sent)
		end := c.snd + uint32(n)
		for try := 0; int32(end-c.una) > 0; try++ {
			if c.closed || try == 10 {
				return sent, errors.New("write: not acknowledged")
			}
			c.seg(flagACK|flagPSH, c.snd, b[sent:sent+n])
			c.pause(time.Second, func() bool { return int32(end-c.una) <= 0 || c.closed })
		}
		c.snd = end
		sent += n
	}
	return sent, nil
}

func (c *conn) Close() error {
	c.mu.Lock()
	c.seg(flagRST|flagACK, c.snd, nil)
	c.closed = true
	c.mu.Unlock()
	c.unwatch()
	return nil
}

func (c *conn) LocalAddr() net.Addr              { return &net.TCPAddr{} }
func (c *conn) RemoteAddr() net.Addr             { return net.TCPAddrFromAddrPort(c.to) }
func (c *conn) SetDeadline(time.Time) error      { return nil }
func (c *conn) SetReadDeadline(time.Time) error  { return nil }
func (c *conn) SetWriteDeadline(time.Time) error { return nil }
