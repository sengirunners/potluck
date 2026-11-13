// Package portstream adapts bounded lease-authorized port operations to net.Conn.
package portstream

import (
	"context"
	"errors"
	"github.com/sengirunners/potluck/protocol"
	"io"
	"net"
	"os"
	"sync"
	"time"
)

type Call func(context.Context, protocol.PortRequest) (protocol.PortResult, error)
type Conn struct {
	ctx                         context.Context
	cancel                      context.CancelFunc
	call                        Call
	id                          string
	readMu, writeMu             sync.Mutex
	pending                     []byte
	eof                         bool
	readDeadline, writeDeadline deadline
	once                        sync.Once
}

func Dial(ctx context.Context, port int, call Call) (net.Conn, error) {
	ctx, cancel := context.WithCancel(ctx)
	out, err := call(ctx, protocol.PortRequest{Op: "open", Port: port})
	if err != nil {
		cancel()
		return nil, err
	}
	if out.Stream == "" {
		cancel()
		return nil, io.ErrUnexpectedEOF
	}
	return &Conn{ctx: ctx, cancel: cancel, call: call, id: out.Stream}, nil
}
func (c *Conn) op(op string, data []byte) (protocol.PortResult, error) {
	d := &c.readDeadline
	if op == "write" {
		d = &c.writeDeadline
	}
	expired := d.wait()
	ctx, cancel := context.WithCancelCause(c.ctx)
	defer cancel(nil)
	select {
	case <-expired:
		return protocol.PortResult{}, os.ErrDeadlineExceeded
	default:
	}
	finished := make(chan struct{})
	defer close(finished)
	go func() {
		select {
		case <-expired:
			cancel(os.ErrDeadlineExceeded)
		case <-ctx.Done():
		case <-finished:
		}
	}()
	if err := ctx.Err(); err != nil {
		if errors.Is(context.Cause(ctx), os.ErrDeadlineExceeded) {
			return protocol.PortResult{}, os.ErrDeadlineExceeded
		}
		return protocol.PortResult{}, err
	}
	out, err := c.call(ctx, protocol.PortRequest{Op: op, Stream: c.id, Data: data})
	if errors.Is(context.Cause(ctx), os.ErrDeadlineExceeded) {
		return out, os.ErrDeadlineExceeded
	}
	return out, err
}

func (c *Conn) Read(b []byte) (int, error) {
	if len(b) == 0 {
		return 0, nil
	}
	c.readMu.Lock()
	defer c.readMu.Unlock()
	for len(c.pending) == 0 && !c.eof {
		out, err := c.op("read", nil)
		if err != nil {
			return 0, err
		}
		if len(out.Data) > protocol.PortPage {
			return 0, io.ErrShortBuffer
		}
		c.pending = out.Data
		c.eof = out.EOF
		if len(c.pending) == 0 && !c.eof {
			select {
			case <-c.ctx.Done():
				return 0, c.ctx.Err()
			case <-time.After(10 * time.Millisecond):
			}
		}
	}
	n := copy(b, c.pending)
	c.pending = c.pending[n:]
	if n == 0 && c.eof {
		return 0, io.EOF
	}
	return n, nil
}
func (c *Conn) Write(b []byte) (int, error) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	n := 0
	for len(b) > 0 {
		size := min(len(b), protocol.PortPage)
		if _, err := c.op("write", b[:size]); err != nil {
			return n, err
		}
		n += size
		b = b[size:]
	}
	return n, nil
}
func (c *Conn) Close() error {
	c.once.Do(func() {
		c.cancel()
		c.readDeadline.set(time.Time{})
		c.writeDeadline.set(time.Time{})
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		_, _ = c.call(ctx, protocol.PortRequest{Op: "close", Stream: c.id})
	})
	return nil
}
func (c *Conn) LocalAddr() net.Addr  { return address("potluck") }
func (c *Conn) RemoteAddr() net.Addr { return address("sandbox-loopback") }
func (c *Conn) SetDeadline(t time.Time) error {
	if c.ctx.Err() != nil {
		return net.ErrClosed
	}
	c.readDeadline.set(t)
	c.writeDeadline.set(t)
	return nil
}
func (c *Conn) SetReadDeadline(t time.Time) error {
	if c.ctx.Err() != nil {
		return net.ErrClosed
	}
	c.readDeadline.set(t)
	return nil
}
func (c *Conn) SetWriteDeadline(t time.Time) error {
	if c.ctx.Err() != nil {
		return net.ErrClosed
	}
	c.writeDeadline.set(t)
	return nil
}

type address string

func (a address) Network() string { return "potluck-port" }
func (a address) String() string  { return string(a) }
