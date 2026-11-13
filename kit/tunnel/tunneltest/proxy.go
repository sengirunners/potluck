// Package tunneltest stands in for Cloudflare in tests (SPEC 17.11): a
// loopback TCP proxy in front of a home's tunnel listener that can stall the
// connections open now (a lost packet in TCP backoff), go offline, or send
// its traffic to another home (a hijacked hostname). It forwards bytes only;
// it never sees zm's keys. Never linked into zm.
package tunneltest

import (
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// Proxy is a loopback TCP proxy.
type Proxy struct {
	ln      net.Listener
	target  atomic.Pointer[string]
	offline atomic.Bool
	held    atomic.Int64 // UnixNano until which new connections start stalled
	tap     atomic.Pointer[io.Writer]

	mu     sync.Mutex
	live   map[*pconn]struct{}
	gen    uint64 // bumped by every drop: a connection dialed across one is closed, not kept
	closed bool
}

type pconn struct {
	a, b  net.Conn
	stuck atomic.Int64 // UnixNano until which nothing is delivered
	slow  atomic.Int64 // UnixNano until which nothing is delivered back to the client
}

// New listens on a loopback port and forwards each connection to target
// (host:port).
func New(target string) (*Proxy, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	p := &Proxy{ln: ln, live: map[*pconn]struct{}{}}
	p.target.Store(&target)
	go p.serve()
	return p, nil
}

// URL is the proxy's http URL (what a tunnel invite names in tests).
func (p *Proxy) URL() string { return "http://" + p.ln.Addr().String() }

// SetTarget sends new connections to target, and closes the open ones.
func (p *Proxy) SetTarget(target string) {
	p.target.Store(&target)
	p.drop()
}

// Move sends new connections to target and leaves the open ones where they
// are (an edge whose streams stay pinned to the origin they reached).
func (p *Proxy) Move(target string) { p.target.Store(&target) }

// Tap copies every byte the proxy carries, both ways, to w (Cloudflare
// reads the traffic: a leak test looks for what must not be there). nil
// stops it. w must be safe for concurrent writes.
func (p *Proxy) Tap(w io.Writer) {
	if w == nil {
		p.tap.Store(nil)
		return
	}
	p.tap.Store(&w)
}

// Offline refuses (closes) every connection while on; going offline drops
// the open ones.
func (p *Proxy) Offline(on bool) {
	p.offline.Store(on)
	if on {
		p.drop()
	}
}

// Stall makes the connections open now deliver nothing either way for d.
// New connections are not affected.
func (p *Proxy) Stall(d time.Duration) {
	until := time.Now().Add(d).UnixNano()
	p.mu.Lock()
	defer p.mu.Unlock()
	for c := range p.live {
		c.stuck.Store(until)
	}
}

// StallReplies holds what the target sends back on the connections open now
// for d, while requests still reach it: a write the home runs whose answer
// the caller does not get in time. New connections are not affected.
func (p *Proxy) StallReplies(d time.Duration) {
	until := time.Now().Add(d).UnixNano()
	p.mu.Lock()
	defer p.mu.Unlock()
	for c := range p.live {
		c.slow.Store(until)
	}
}

// StallAll is Stall for every connection, including any opened before d
// ends: a request then cannot dodge the stall by dialing a fresh one.
func (p *Proxy) StallAll(d time.Duration) {
	until := time.Now().Add(d).UnixNano()
	p.held.Store(until)
	p.mu.Lock()
	defer p.mu.Unlock()
	for c := range p.live {
		c.stuck.Store(until)
	}
}

// Close stops the proxy and its connections.
func (p *Proxy) Close() error {
	p.mu.Lock()
	p.closed = true
	p.mu.Unlock()
	err := p.ln.Close()
	p.drop()
	return err
}

func (p *Proxy) drop() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.gen++
	for c := range p.live {
		c.a.Close()
		c.b.Close()
		delete(p.live, c)
	}
}

func (p *Proxy) serve() {
	for {
		a, err := p.ln.Accept()
		if err != nil {
			return
		}
		// Read the drop generation before offline and the target: a
		// SetTarget, Offline or Close that runs while this one dials then
		// shows at registration below.
		p.mu.Lock()
		gen := p.gen
		p.mu.Unlock()
		if p.offline.Load() {
			a.Close()
			continue
		}
		b, err := net.DialTimeout("tcp", *p.target.Load(), 2*time.Second)
		if err != nil {
			a.Close()
			continue
		}
		c := &pconn{a: a, b: b}
		c.stuck.Store(p.held.Load())
		p.mu.Lock()
		if p.gen != gen || p.offline.Load() || p.closed { // dropped, offline or closed while this one dialed
			p.mu.Unlock()
			a.Close()
			b.Close()
			continue
		}
		p.live[c] = struct{}{}
		p.mu.Unlock()
		done := func() {
			a.Close()
			b.Close()
			p.mu.Lock()
			delete(p.live, c)
			p.mu.Unlock()
		}
		var once sync.Once
		go func() { c.pipe(b, a, &p.tap, false); once.Do(done) }()
		go func() { c.pipe(a, b, &p.tap, true); once.Do(done) }()
	}
}

// pipe copies src to dst, holding what it read while the connection is
// stalled (and, for the reply direction, while replies are stalled).
func (c *pconn) pipe(dst io.Writer, src io.Reader, tap *atomic.Pointer[io.Writer], reply bool) {
	buf := make([]byte, 32<<10)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			for now := time.Now().UnixNano(); now < c.stuck.Load() || (reply && now < c.slow.Load()); now = time.Now().UnixNano() {
				time.Sleep(10 * time.Millisecond)
			}
			if w := tap.Load(); w != nil {
				_, _ = (*w).Write(buf[:n])
			}
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}
