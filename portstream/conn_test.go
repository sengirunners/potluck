package portstream_test

import (
	"context"
	"errors"
	"github.com/sengirunners/potluck/portstream"
	"github.com/sengirunners/potluck/protocol"
	"io"
	"os"
	"testing"
	"time"
)

func TestConnBoundsEOFAndCancellation(t *testing.T) {
	writes := 0
	closes := 0
	call := func(ctx context.Context, r protocol.PortRequest) (protocol.PortResult, error) {
		switch r.Op {
		case "open":
			return protocol.PortResult{Stream: "test"}, nil
		case "write":
			if len(r.Data) > protocol.PortPage {
				t.Fatal("unbounded write")
			}
			writes++
		case "read":
			return protocol.PortResult{Data: []byte{0, 255}, EOF: true}, nil
		case "close":
			closes++
		}
		return protocol.PortResult{}, nil
	}
	c, err := portstream.Dial(context.Background(), 3000, call)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := c.Write(make([]byte, protocol.PortPage+1)); err != nil || n != protocol.PortPage+1 || writes != 2 {
		t.Fatalf("write %d %v pages %d", n, err, writes)
	}
	got, err := io.ReadAll(c)
	if err != nil || len(got) != 2 || got[1] != 255 {
		t.Fatalf("EOF lost data: %v %v", got, err)
	}
	c.SetWriteDeadline(time.Now().Add(-time.Second))
	if _, err := c.Write([]byte{1}); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("deadline: %v", err)
	}
	c.Close()
	c.Close()
	if closes != 1 {
		t.Fatalf("close calls: %d", closes)
	}
}
func TestBlockedReadCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	c, err := portstream.Dial(ctx, 3000, func(ctx context.Context, r protocol.PortRequest) (protocol.PortResult, error) {
		if r.Op == "open" {
			return protocol.PortResult{Stream: "test"}, nil
		}
		if r.Op == "close" {
			return protocol.PortResult{}, nil
		}
		<-ctx.Done()
		return protocol.PortResult{}, ctx.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	cancel()
	done := make(chan error, 1)
	go func() { _, err := c.Read(make([]byte, 1)); done <- err }()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("read stayed open after cancellation")
	}
}
