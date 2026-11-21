package server_test

import (
	"context"
	"errors"
	. "github.com/sengirunners/potluck/kit/server"
	"github.com/sengirunners/potluck/wiring"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/events"
)

// shortHome is a temp ZM_HOME short enough for a Unix socket path.
func shortHome(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "zm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

func TestDaemonOverSocket(t *testing.T) {
	home := shortHome(t)
	cfg, err := config.LoadFrom(home)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Human, cfg.Machine = "ana", "mini"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dm, err := New(ctx, cfg, nil, wiring.Kit)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- dm.Serve(ctx) }()
	c := client.NewUnix(cfg.SocketPath())
	if err := c.WaitReady(ctx, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(cfg.SocketPath()); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("socket mode: %v %v", fi.Mode(), err)
	}
	h, err := c.Health(ctx)
	if err != nil || h.Protocol != api.WireProtocol || h.Machine != "mini" || h.PID == 0 {
		t.Fatalf("health %+v %v", h, err)
	}
	w, err := c.Whoami(ctx)
	if err != nil || w.As != "human:ana" || w.Via != "local" || w.Identity.Name != "ana" {
		t.Fatalf("whoami %+v %v", w, err)
	}
	c.As = "mini/abcd1234"
	if w, _ = c.Whoami(ctx); w.Session != "mini/abcd1234" {
		t.Fatalf("session caller %+v", w)
	}
	c.As = "other/abcd1234"
	if _, err = c.Whoami(ctx); !api.IsCode(err, api.CodeForbidden) {
		t.Fatalf("foreign session: %v", err)
	}
	c.As = ""

	// No project anywhere: writes are refused with how to pick one.
	p, err := c.CreateProject(ctx, api.NewProjectRequest{Slug: "zm", RepoRules: api.RepoRules{Paths: []string{home}}})
	if err != nil || p.Role != "owner" {
		t.Fatalf("create %+v %v", p, err)
	}
	c.Cwd = home
	res, err := c.Resolve(ctx)
	if err != nil || res.Project == nil || res.Rule != "cwd-path" {
		t.Fatalf("resolve %+v %v", res, err)
	}
	c.Cwd, c.Project, c.ProjectFrom = "", "nope", "env"
	if res, _ = c.Resolve(ctx); res.Project != nil || res.Steps[0].Rule != "env" {
		t.Fatalf("explicit miss %+v", res)
	}
	c.Project = ""

	// The events log has the daemon start and the project; resume works.
	var got []api.Event
	if err := c.Events(ctx, api.EventQuery{Since: "0", NoLive: true}, func(e api.Event) error { got = append(got, e); return nil }); err != nil {
		t.Fatal(err)
	}
	if len(got) < 2 || got[0].Type != api.EventDaemonStarted || got[0].Project != api.ProjectNone {
		t.Fatalf("events %+v", got)
	}

	// Protocol mismatch is refused both ways.
	req, _ := http.NewRequest("GET", "http://zm/v0/health", nil)
	req.Header.Set(api.HeaderProtocol, "zm/v9")
	resp, err := clientHTTP(cfg.SocketPath()).Do(req)
	if err != nil || resp.StatusCode != 400 {
		t.Fatalf("protocol mismatch: %v %v", resp, err)
	}
	resp.Body.Close()

	// Implemented module routes answer; unknown routes 404.
	if st, err := c.Tailnet(ctx); err != nil || st.Running {
		t.Fatalf("tailnet: %+v %v", st, err)
	}
	if err = c.Do(ctx, "GET", "/v0/nope", nil, nil); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("404: %v", err)
	}

	// A second daemon on the same home refuses to start: the first holds
	// the home's writer lock.
	if dm2, err := New(ctx, cfg, nil, wiring.Kit); err == nil || !strings.Contains(err.Error(), "is changing") {
		if dm2 != nil {
			dm2.Close()
		}
		t.Fatalf("second daemon: %v", err)
	}

	if r, err := c.StopDaemon(ctx, api.DaemonStopRequest{}); err != nil || !r.Stopping {
		t.Fatalf("stop %+v %v", r, err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("daemon did not stop")
	}
	if _, err := os.Stat(cfg.SocketPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("socket left behind")
	}
}

func clientHTTP(sock string) *http.Client { return &http.Client{Transport: unixTransport(sock)} }

func TestReadSSEStop(t *testing.T) {
	r := strings.NewReader("data: {\"id\":\"01A\",\"type\":\"x\"}\n\ndata: {\"id\":\"01B\"}\n\n")
	n := 0
	err := events.ReadSSE(io.NopCloser(r), func(api.Event) error { n++; return events.ErrStop })
	if err != nil || n != 1 {
		t.Fatal(n, err)
	}
}

// A live event reader that never hangs up does not hold the daemon's
// shutdown: its stream ends as soon as the daemon stops.
func TestStopEndsLiveStreams(t *testing.T) {
	home := shortHome(t)
	cfg, err := config.LoadFrom(home)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Human, cfg.Machine = "ana", "mini"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dm, err := New(ctx, cfg, nil, wiring.Kit)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- dm.Serve(ctx) }()
	c := client.NewUnix(cfg.SocketPath())
	if err := c.WaitReady(ctx, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	streaming := make(chan struct{})
	go func() {
		_ = c.Events(ctx, api.EventQuery{Since: "0"}, func(api.Event) error {
			select {
			case <-streaming:
			default:
				close(streaming)
			}
			return nil
		})
	}()
	<-streaming
	start := time.Now()
	if _, err := c.StopDaemon(ctx, api.DaemonStopRequest{}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
		if d := time.Since(start); d > 2*time.Second {
			t.Errorf("shutdown took %s with a live reader", d)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("daemon did not stop")
	}
}

// panicStarter panics in Start: a module bug between listen and shutdown.
type panicStarter struct{}

func (panicStarter) Routes(*core.Router)         {}
func (panicStarter) Start(context.Context) error { panic("a module's bug") }

// A panic in Serve still removes the socket: a file left behind made the
// next start and every client meet a socket nobody answers.
func TestPanicInServeRemovesTheSocket(t *testing.T) {
	home := shortHome(t)
	cfg, err := config.LoadFrom(home)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Human, cfg.Machine = "ana", "mini"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dm, err := New(ctx, cfg, nil, wiring.Kit)
	if err != nil {
		t.Fatal(err)
	}
	defer dm.Close()
	dm.AddModule(panicStarter{})
	recovered := func() (r any) {
		defer func() { r = recover() }()
		_ = dm.Serve(ctx)
		return nil
	}()
	if recovered == nil {
		t.Fatal("Serve did not panic")
	}
	if _, err := os.Stat(cfg.SocketPath()); !os.IsNotExist(err) {
		t.Fatalf("socket after a panic in Serve: %v", err)
	}
}
