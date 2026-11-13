package client

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/events"
)

// Dogfood: a request cut off by `zm daemon stop` printed "daemon is not
// running" (it was, until then).
func TestCutOffRequestIsNotNotRunning(t *testing.T) {
	dir, err := os.MkdirTemp("", "zm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "s.sock")

	// Never started: "not running".
	err = NewUnix(sock).Do(context.Background(), http.MethodGet, "/v0/health", nil, nil)
	if !api.IsCode(err, api.CodeUnavailable) || !strings.Contains(err.Error(), "not running") {
		t.Fatalf("no daemon: %v", err)
	}

	for _, method := range []string{http.MethodGet, http.MethodPost} {
		ln, err := net.Listen("unix", sock)
		if err != nil {
			t.Fatal(err)
		}
		arrived := make(chan struct{})
		srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			close(arrived)
			<-r.Context().Done() // hangs until the "daemon" goes away
		})}
		go func() { _ = srv.Serve(ln) }()
		errc := make(chan error, 1)
		go func() { errc <- NewUnix(sock).Do(context.Background(), method, "/v0/work", nil, nil) }()
		select {
		case <-arrived:
		case <-time.After(5 * time.Second):
			t.Fatal("request never arrived")
		}
		_ = srv.Close() // the daemon stops: listener and connections close, the socket is gone
		_ = os.Remove(sock)
		var got error
		select {
		case got = <-errc:
		case <-time.After(5 * time.Second):
			t.Fatal("client did not return")
		}
		e, ok := got.(*api.Error)
		if !ok || e.Code != api.CodeUnavailable || strings.Contains(e.Message, "not running") || !strings.Contains(e.Message, "stopped or restarted during this request") {
			t.Fatalf("%s cut off: %v", method, got)
		}
		if writes := strings.Contains(e.Hint, "may or may not"); writes != (method == http.MethodPost) {
			t.Fatalf("%s hint: %q", method, e.Hint)
		}
	}

	// The caller giving up is its own error, never "the daemon stopped".
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() })}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := NewUnix(sock).Do(ctx, http.MethodGet, "/v0/work", nil, nil); err == nil || strings.Contains(err.Error(), "stopped or restarted") {
		t.Fatalf("cancelled: %v", err)
	}
}

// Review: `zm events` cut off by `zm daemon stop` exited 0 with no
// message. A live stream the daemon ends (it ends them cleanly on shutdown)
// or drops is the same cut-off; a replay ending, the caller stopping and the
// caller giving up are not.
func TestCutOffEventStream(t *testing.T) {
	ends := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(api.HeaderProtocol, api.Protocol)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "id: 01A\nevent: t\ndata: {\"id\":\"01A\",\"type\":\"t\"}\n\n")
	})
	none := func(api.Event) error { return nil }
	err := WithHandler(ends).Events(context.Background(), api.EventQuery{}, none)
	if e, ok := err.(*api.Error); !ok || e.Code != api.CodeUnavailable || !strings.Contains(e.Message, "stopped or restarted during this request") {
		t.Fatalf("live stream ended by the daemon: %v", err)
	}
	if err := WithHandler(ends).Events(context.Background(), api.EventQuery{NoLive: true}, none); err != nil {
		t.Fatalf("replay end: %v", err)
	}
	if err := WithHandler(ends).Events(context.Background(), api.EventQuery{}, func(api.Event) error { return events.ErrStop }); err != nil {
		t.Fatalf("caller stopped: %v", err)
	}
	mine := errors.New("mine")
	if err := WithHandler(ends).Events(context.Background(), api.EventQuery{}, func(api.Event) error { return mine }); err != mine {
		t.Fatalf("caller's own error: %v", err)
	}

	// Over the socket: the connection drops mid-stream (daemon killed).
	dir, err := os.MkdirTemp("", "zm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "s.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	arrived := make(chan struct{})
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(api.HeaderProtocol, api.Protocol)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		close(arrived)
		<-r.Context().Done()
	})}
	go func() { _ = srv.Serve(ln) }()
	errc := make(chan error, 1)
	go func() { errc <- NewUnix(sock).Events(context.Background(), api.EventQuery{}, none) }()
	select {
	case <-arrived:
	case <-time.After(5 * time.Second):
		t.Fatal("stream never opened")
	}
	_ = srv.Close()
	select {
	case err = <-errc:
	case <-time.After(5 * time.Second):
		t.Fatal("client did not return")
	}
	if e, ok := err.(*api.Error); !ok || e.Code != api.CodeUnavailable || !strings.Contains(e.Message, "stopped or restarted") || !strings.Contains(e.Message, sock) {
		t.Fatalf("dropped stream: %v", err)
	}
	// The caller giving up (ctrl-C) is a quiet end.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	slow := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(api.HeaderProtocol, api.Protocol)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})
	if err := WithHandler(slow).Events(ctx, api.EventQuery{}, none); err != nil {
		t.Fatalf("cancelled: %v", err)
	}
}

// TestIsNoDaemonErrno: a missing or refused socket reads by its errno, and
// by its text when the errno is not wrapped; a timeout is neither.
func TestIsNoDaemonErrno(t *testing.T) {
	for _, err := range []error{
		&net.OpError{Op: "dial", Net: "unix", Err: &os.SyscallError{Syscall: "connect", Err: syscall.ENOENT}},
		fmt.Errorf("dial: %w", syscall.ECONNREFUSED),
		errors.New("dial unix /x/zm.sock: connect: connection refused"),
	} {
		if !isNoDaemon(err) {
			t.Errorf("not running: %v", err)
		}
	}
	if isNoDaemon(context.DeadlineExceeded) {
		t.Error("a timeout read as not running")
	}
}
