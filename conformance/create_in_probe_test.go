package conformance

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func probeListener(t *testing.T) net.Listener {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	return l
}

func probeResult(t *testing.T, result <-chan error) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("connection observation did not finish")
		return nil
	}
}

func probeSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(5 * time.Second):
		t.Fatal("connection observation did not reach synchronization point")
	}
}

// Hold Accept until the broken driver's completed connect AND refusal. An
// immediate post-acquire snapshot is deterministically empty at that point.
// Closing rather than draining through the barrier would lose the queued dial.
func TestCreateInProbeDelayedAccept(t *testing.T) {
	l := &gatedProbeListener{Listener: probeListener(t), gate: make(chan struct{}), closed: make(chan struct{})}
	t.Cleanup(func() { l.Close() })
	returned := make(chan struct{})
	result := make(chan error, 1)
	go func() {
		result <- checkCreateInConnection(context.Background(), l, func() error {
			c, err := net.DialTimeout("tcp", l.Addr().String(), time.Second)
			if err != nil {
				return err
			}
			c.Close()
			close(returned)
			return nil // the driver answered forbidden
		})
	}()
	probeSignal(t, returned)
	select {
	case err := <-result:
		t.Fatalf("observation returned before queued connections were accepted: %v", err)
	default:
	}
	close(l.gate)
	if err := probeResult(t, result); err == nil || !strings.Contains(err.Error(), "connected to the host") {
		t.Fatalf("completed forbidden dial went undetected: %v", err)
	}
	if l.accepting.Load() != 0 {
		t.Fatal("returned with Accept still running")
	}
}

func TestCreateInProbeCleanRefusal(t *testing.T) {
	if err := checkCreateInConnection(context.Background(), probeListener(t), func() error { return nil }); err != nil {
		t.Fatalf("own barrier counted as a driver connection: %v", err)
	}
}

func TestCreateInProbeAcquireFailure(t *testing.T) {
	l := &gatedProbeListener{Listener: probeListener(t), gate: make(chan struct{}), closed: make(chan struct{})}
	want := errors.New("driver did not refuse")
	if err := checkCreateInConnection(context.Background(), l, func() error { return want }); !errors.Is(err, want) {
		t.Fatalf("lost acquire failure: %v", err)
	}
	if l.accepting.Load() != 0 {
		t.Fatal("failure left Accept running")
	}
}

func TestCreateInProbeCanceledBeforeAcquire(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := checkCreateInConnection(ctx, probeListener(t), func() error {
		t.Error("called driver after cancellation")
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation passed: %v", err)
	}
}

func TestCreateInProbeCancelClosesActiveACK(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started, finished := make(chan struct{}), make(chan struct{})
	l := &mappedProbeListener{Listener: probeListener(t), wrap: func(c net.Conn) net.Conn {
		return &blockedProbeACK{Conn: c, started: started, finished: finished, closed: make(chan struct{})}
	}}
	result := make(chan error, 1)
	go func() { result <- checkCreateInConnection(ctx, l, func() error { return nil }) }()
	probeSignal(t, started)
	cancel()
	if err := probeResult(t, result); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled ACK passed: %v", err)
	}
	select {
	case <-finished:
	default:
		t.Fatal("returned before active ACK writer exited")
	}
}

func TestCreateInProbeObservationFailures(t *testing.T) {
	t.Run("dial", func(t *testing.T) {
		l := probeListener(t)
		err := checkCreateInConnection(context.Background(), l, func() error { return l.Close() })
		if err == nil || !strings.Contains(err.Error(), "barrier dial") {
			t.Fatalf("failed dial passed: %v", err)
		}
	})
	t.Run("accept", func(t *testing.T) {
		want := errors.New("injected accept failure")
		l := &failedProbeListener{Listener: probeListener(t), err: want}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := checkCreateInConnection(ctx, l, func() error { return nil }); !errors.Is(err, want) {
			t.Fatalf("lost accept failure: %v", err)
		}
	})
	t.Run("read", func(t *testing.T) {
		l := &mappedProbeListener{Listener: probeListener(t), wrap: func(c net.Conn) net.Conn {
			return &failedProbeACK{Conn: c}
		}}
		if err := checkCreateInConnection(context.Background(), l, func() error { return nil }); err == nil || !strings.Contains(err.Error(), "barrier ACK") {
			t.Fatalf("missing ACK passed: %v", err)
		}
	})
}

type gatedProbeListener struct {
	net.Listener
	gate, closed chan struct{}
	once         sync.Once
	accepting    atomic.Int32
}

func (l *gatedProbeListener) Accept() (net.Conn, error) {
	l.accepting.Add(1)
	defer l.accepting.Add(-1)
	select {
	case <-l.gate:
		return l.Listener.Accept()
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

func (l *gatedProbeListener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return l.Listener.Close()
}

type mappedProbeListener struct {
	net.Listener
	wrap func(net.Conn) net.Conn
}

func (l *mappedProbeListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return l.wrap(c), nil
}

type failedProbeListener struct {
	net.Listener
	err error
}

func (l *failedProbeListener) Accept() (net.Conn, error) { return nil, l.err }

type failedProbeACK struct{ net.Conn }

func (c *failedProbeACK) Write([]byte) (int, error) { return 0, errors.New("injected ACK failure") }

type blockedProbeACK struct {
	net.Conn
	started, finished, closed chan struct{}
	once                      sync.Once
}

func (c *blockedProbeACK) Write([]byte) (int, error) {
	close(c.started)
	defer close(c.finished)
	<-c.closed
	return 0, net.ErrClosed
}

func (c *blockedProbeACK) Close() error {
	c.once.Do(func() { close(c.closed) })
	return c.Conn.Close()
}
