package boxport_test

import (
	"bytes"
	"context"
	"github.com/sengirunners/potluck/boxport"
	"github.com/sengirunners/potluck/portstream"
	"github.com/sengirunners/potluck/protocol"
	"io"
	"net"
	"os"
	"os/exec"
	"testing"
	"time"
)

func TestPortBinaryEOFAndClose(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	payload := bytes.Repeat([]byte{0, 255, 1, 128}, 40000)
	done := make(chan error, 1)
	go func() {
		c, err := listener.Accept()
		if err != nil {
			done <- err
			return
		}
		defer c.Close()
		b := make([]byte, len(payload))
		_, err = io.ReadFull(c, b)
		if err == nil && !bytes.Equal(b, payload) {
			err = io.ErrUnexpectedEOF
		}
		if err == nil {
			_, err = c.Write(b)
		}
		done <- err
	}()
	call := func(ctx context.Context, req protocol.PortRequest) (protocol.PortResult, error) {
		return boxport.Exchange(ctx, req, time.Now().Add(20*time.Second), func(in io.Reader, out, errout io.Writer, argv ...string) *exec.Cmd {
			cmd := exec.Command(argv[0], argv[1:]...)
			cmd.Stdin, cmd.Stdout, cmd.Stderr = in, out, errout
			cmd.Env = append(os.Environ(), "TMPDIR="+dir)
			return cmd
		})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := portstream.Dial(ctx, listener.Addr().(*net.TCPAddr).Port, call)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if n, err := c.Write(payload); err != nil || n != len(payload) {
		t.Fatalf("write %d: %v", n, err)
	}
	got, err := io.ReadAll(c)
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("read %d bytes: %v", len(got), err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
}
func TestPortRejectsBeforeCommand(t *testing.T) {
	for _, req := range []protocol.PortRequest{{Op: "open", Port: 0}, {Op: "open", Port: 65536}, {Op: "read", Stream: "../../host"}, {Op: "write", Stream: "012345678901234567890123456789012345678901234567", Data: make([]byte, protocol.PortPage+1)}, {Op: "connect", Port: 80}} {
		_, err := boxport.Exchange(context.Background(), req, time.Now().Add(time.Minute), func(io.Reader, io.Writer, io.Writer, ...string) *exec.Cmd {
			t.Fatal("invalid request reached guest")
			return nil
		})
		if err == nil {
			t.Fatalf("accepted %+v", req)
		}
	}
	_, err := boxport.Exchange(context.Background(), protocol.PortRequest{Op: "open", Port: 80}, time.Now().Add(-time.Second), func(io.Reader, io.Writer, io.Writer, ...string) *exec.Cmd {
		t.Fatal("expired request reached guest")
		return nil
	})
	if err == nil {
		t.Fatal("expired lease accepted")
	}
}

func TestPortOpposingLargeTransfers(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	payload := bytes.Repeat([]byte{0, 255, 128, 42}, 512<<10)
	peerDone := make(chan error, 1)
	go func() {
		c, err := listener.Accept()
		if err != nil {
			peerDone <- err
			return
		}
		defer c.Close()
		c.SetDeadline(time.Now().Add(12 * time.Second))
		tcp := c.(*net.TCPConn)
		tcp.SetReadBuffer(32768)
		tcp.SetWriteBuffer(32768)
		// The peer deliberately fills its outgoing direction before draining the
		// rest of the client's upload. Read and Write must progress independently.
		if _, err = c.Write(payload); err == nil {
			b := make([]byte, len(payload))
			_, err = io.ReadFull(c, b)
			if err == nil && !bytes.Equal(b, payload) {
				err = io.ErrUnexpectedEOF
			}
		}
		peerDone <- err
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	call := func(ctx context.Context, req protocol.PortRequest) (protocol.PortResult, error) {
		return boxport.Exchange(ctx, req, time.Now().Add(15*time.Second), func(in io.Reader, out, errout io.Writer, argv ...string) *exec.Cmd {
			cmd := exec.Command(argv[0], argv[1:]...)
			cmd.Stdin, cmd.Stdout, cmd.Stderr = in, out, errout
			cmd.Env = append(os.Environ(), "TMPDIR="+dir)
			return cmd
		})
	}
	c, err := portstream.Dial(ctx, listener.Addr().(*net.TCPAddr).Port, call)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	written := make(chan error, 1)
	go func() { _, err := c.Write(payload); written <- err }()
	time.Sleep(50 * time.Millisecond)
	got := make([]byte, len(payload))
	if _, err := io.ReadFull(c, got); err != nil {
		t.Fatalf("opposing response stalled: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatal("opposing response changed bytes")
	}
	if err := <-written; err != nil {
		t.Fatalf("opposing upload stalled: %v", err)
	}
	if err := <-peerDone; err != nil {
		t.Fatal(err)
	}
}
