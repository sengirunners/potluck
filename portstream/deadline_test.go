package portstream_test

import (
	"context"
	"errors"
	"github.com/sengirunners/potluck/portstream"
	"github.com/sengirunners/potluck/protocol"
	"net"
	"os"
	"testing"
	"time"
)

func TestPendingPortDeadlines(t *testing.T) {
	for _, direction := range []string{"read", "write"} {
		for _, change := range []string{"shorten", "extend", "clear"} {
			t.Run(direction+"/"+change, func(t *testing.T) {
				entered := make(chan struct{})
				release := make(chan struct{})
				c, err := portstream.Dial(context.Background(), 3000, func(ctx context.Context, r protocol.PortRequest) (protocol.PortResult, error) {
					switch r.Op {
					case "open":
						return protocol.PortResult{Stream: "test"}, nil
					case "close":
						return protocol.PortResult{}, nil
					default:
						close(entered)
						select {
						case <-ctx.Done():
							return protocol.PortResult{}, ctx.Err()
						case <-release:
							return protocol.PortResult{Data: []byte{42}}, nil
						}
					}
				})
				if err != nil {
					t.Fatal(err)
				}
				defer c.Close()
				set := c.SetReadDeadline
				if direction == "write" {
					set = c.SetWriteDeadline
				}
				if change != "shorten" {
					set(time.Now().Add(100 * time.Millisecond))
				}
				done := make(chan error, 1)
				go func() {
					var err error
					if direction == "read" {
						_, err = c.Read(make([]byte, 1))
					} else {
						_, err = c.Write([]byte{1})
					}
					done <- err
				}()
				<-entered
				if change == "shorten" {
					set(time.Now())
					select {
					case err := <-done:
						if !errors.Is(err, os.ErrDeadlineExceeded) {
							t.Fatalf("pending %s returned %v", direction, err)
						}
						if ne, ok := err.(net.Error); !ok || !ne.Timeout() {
							t.Fatalf("not timeout: %v", err)
						}
					case <-time.After(300 * time.Millisecond):
						t.Fatalf("pending %s ignored shortened deadline", direction)
					}
					return
				}
				if change == "clear" {
					set(time.Time{})
				} else {
					set(time.Now().Add(time.Second))
				}
				select {
				case err := <-done:
					t.Fatalf("pending %s retained obsolete deadline: %v", direction, err)
				case <-time.After(200 * time.Millisecond):
				}
				close(release)
				if err := <-done; err != nil {
					t.Fatalf("pending %s after %s: %v", direction, change, err)
				}
			})
		}
	}
}

func TestPendingPortDeadlineDirectionsIndependent(t *testing.T) {
	entered := make(chan string, 2)
	c, err := portstream.Dial(context.Background(), 3000, func(ctx context.Context, r protocol.PortRequest) (protocol.PortResult, error) {
		if r.Op == "open" {
			return protocol.PortResult{Stream: "test"}, nil
		}
		if r.Op == "close" {
			return protocol.PortResult{}, nil
		}
		entered <- r.Op
		<-ctx.Done()
		return protocol.PortResult{}, ctx.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	readDone, writeDone := make(chan error, 1), make(chan error, 1)
	go func() { _, err := c.Read(make([]byte, 1)); readDone <- err }()
	go func() { _, err := c.Write([]byte{1}); writeDone <- err }()
	<-entered
	<-entered
	c.SetReadDeadline(time.Now())
	select {
	case err := <-readDone:
		if !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("read deadline did not unblock read")
	}
	select {
	case err := <-writeDone:
		t.Fatalf("read deadline canceled pending write: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	c.SetWriteDeadline(time.Now())
	select {
	case err := <-writeDone:
		if !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("write deadline did not unblock write")
	}
}
