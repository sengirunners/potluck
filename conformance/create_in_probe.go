package conformance

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"
)

const createInProbeTimeout = 3 * time.Second

// checkCreateInConnection owns l. After acquire returns forbidden, its own TCP
// connection is an ordered barrier behind completed driver connections in the
// loopback listener's queue. Its ACK proves those accepts have been observed;
// merely closing the listener could discard them. This does not claim to prove
// the absence of detached connections initiated after the driver's response.
func checkCreateInConnection(ctx context.Context, l net.Listener, acquire func() error) (err error) {
	p := &createInProbe{listener: l, done: make(chan struct{})}
	go p.serve()
	stopCancel := context.AfterFunc(ctx, p.stop)
	defer func() {
		stopCancel()
		p.stop()
		<-p.done
		if p.err != nil {
			err = errors.Join(err, fmt.Errorf("connection observation: %w", p.err))
		}
		if ctx.Err() != nil {
			err = errors.Join(err, ctx.Err())
		}
		if err == nil && p.count != 1 { // exclude exactly our acknowledged barrier
			err = fmt.Errorf("it connected to the host before refusing (%d connections)", p.count-1)
		}
	}()
	if err = ctx.Err(); err != nil {
		return err
	}
	if err = acquire(); err != nil {
		return err
	}
	barrierCtx, cancel := context.WithTimeout(ctx, createInProbeTimeout)
	defer cancel()
	c, err := (&net.Dialer{}).DialContext(barrierCtx, "tcp", l.Addr().String())
	if err != nil {
		return fmt.Errorf("connection observation barrier dial: %w", err)
	}
	defer c.Close()
	stopRead := context.AfterFunc(barrierCtx, func() { c.Close() })
	defer stopRead()
	deadline, _ := barrierCtx.Deadline()
	if err = c.SetReadDeadline(deadline); err != nil {
		return fmt.Errorf("connection observation barrier deadline: %w", err)
	}
	var ack [12]byte
	if _, err = io.ReadFull(c, ack[:]); err != nil {
		return fmt.Errorf("connection observation barrier ACK: %w", err)
	}
	if string(ack[:4]) != "zmcp" || binary.BigEndian.Uint64(ack[4:]) == 0 {
		return fmt.Errorf("connection observation barrier: invalid ACK")
	}
	return nil
}

type createInProbe struct {
	listener net.Listener
	done     chan struct{}
	mu       sync.Mutex
	stopped  bool
	active   net.Conn
	// Written only by serve; read after joining done.
	count uint64
	err   error
}

func (p *createInProbe) stop() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.stopped {
		p.stopped = true
		p.listener.Close()
		if p.active != nil {
			p.active.Close()
		}
	}
}

func (p *createInProbe) serve() {
	defer close(p.done)
	for {
		c, err := p.listener.Accept()
		if err != nil {
			p.mu.Lock()
			stopped := p.stopped
			p.mu.Unlock()
			if !stopped || !errors.Is(err, net.ErrClosed) {
				p.err = err
			}
			return
		}
		p.mu.Lock()
		if p.stopped {
			c.Close()
			p.mu.Unlock()
			return
		}
		p.active = c
		p.mu.Unlock()
		p.count++ // even a peer that resets before reading remains evidence
		var ack [12]byte
		copy(ack[:4], "zmcp")
		binary.BigEndian.PutUint64(ack[4:], p.count)
		if err := c.SetWriteDeadline(time.Now().Add(createInProbeTimeout)); err != nil {
			p.err = fmt.Errorf("ACK deadline: %w", err)
			c.Close()
			return
		}
		// A forbidden driver may close without reading; its write error must not
		// erase the accept. Our own barrier requires a complete ACK separately.
		_, _ = c.Write(ack[:])
		c.Close()
		p.mu.Lock()
		p.active = nil
		p.mu.Unlock()
	}
}
