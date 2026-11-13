package client

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"syscall"
	"testing"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/events"
)

func TestDefaultAs(t *testing.T) {
	cfg := &config.Config{Machine: "mini"}
	env := map[string]string{}
	get := func(k string) string { return env[k] }
	if DefaultAs(cfg, get) != "" {
		t.Fatal("empty env")
	}
	env["CLAUDE_CODE_SESSION_ID"] = "4BB7545A-1111"
	if got := DefaultAs(cfg, get); got != "mini/4bb7545a" {
		t.Fatal(got)
	}
	// Dogfood: a guest token inside the guest's own Claude Code session was
	// refused on every call ("Zm-As must be empty or human:victor").
	env["ZM_TOKEN"] = "zm_x"
	if got := DefaultAs(cfg, get); got != "" {
		t.Fatalf("with a token the runtime session must not be used: %q", got)
	}
	env["ZM_AS"] = "mini/zzzzzzzz"
	if got := DefaultAs(cfg, get); got != "mini/zzzzzzzz" {
		t.Fatal(got)
	}
}

func TestDefaultAsManagedSession(t *testing.T) {
	cfg := &config.Config{Machine: "mini"}
	for _, tt := range []struct {
		name string
		env  map[string]string
		want string
	}{
		{"human", nil, ""},
		{"managed marker only", map[string]string{"ZM_SESSION": "mini/aabbccdd"}, "mini/aabbccdd"},
		{"managed before runtime", map[string]string{"ZM_SESSION": "mini/aabbccdd", "CODEX_THREAD_ID": "11223344-thread"}, "mini/aabbccdd"},
		{"explicit human", map[string]string{"ZM_AS": "human:ana", "ZM_SESSION": "mini/aabbccdd"}, "human:ana"},
		{"token", map[string]string{"ZM_TOKEN": "zm_test", "ZM_SESSION": "mini/aabbccdd"}, ""},
		{"codex", map[string]string{"CODEX_THREAD_ID": "11223344-thread"}, "mini/11223344"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := DefaultAs(cfg, func(k string) string { return tt.env[k] }); got != tt.want {
				t.Fatalf("DefaultAs = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestHeadersAndProtocolCheck(t *testing.T) {
	var seen http.Header
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Clone()
		w.Header().Set(api.HeaderProtocol, r.URL.Query().Get("p"))
		api.WriteJSON(w, 200, api.Health{OK: true})
	})
	c := WithHandler(h)
	c.Token, c.Project, c.ProjectFrom, c.As, c.Cwd = "zm_x", "orchid", "env", "mini/abcd1234", "/secret/path"
	if err := c.Do(context.Background(), "GET", "/v0/health?p="+api.Protocol, nil, nil); err != nil {
		t.Fatal(err)
	}
	if seen.Get("Authorization") != "Bearer zm_x" || api.GetHeader(seen, api.HeaderProject) != "orchid" ||
		api.GetHeader(seen, api.HeaderProjectFrom) != "env" || api.GetHeader(seen, api.HeaderAs) != "mini/abcd1234" || api.GetHeader(seen, api.HeaderProtocol) != api.WireProtocol {
		t.Fatalf("headers %v", seen)
	}
	if api.GetHeader(seen, api.HeaderCwd) != "" {
		t.Fatal("cwd sent over a non-socket transport")
	}
	err := c.Do(context.Background(), "GET", "/v0/health?p=zm/v9", nil, nil)
	if !api.IsCode(err, api.CodeUnavailable) {
		t.Fatalf("protocol mismatch not refused: %v", err)
	}
}

// Events resumes by itself when the daemon cuts a lagging stream off.
func TestEventsResumesAfterOverflow(t *testing.T) {
	var sinces []string
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		since := r.URL.Query().Get("since")
		sinces = append(sinces, since)
		w.Header().Set(api.HeaderProtocol, api.Protocol)
		w.Header().Set("Content-Type", "text/event-stream")
		ev := func(id string) { fmt.Fprintf(w, "id: %s\nevent: t\ndata: {\"id\":%q,\"type\":\"t\"}\n\n", id, id) }
		switch len(sinces) {
		case 1: // live only; two events, then cut off
			ev("01A")
			ev("01B")
			fmt.Fprint(w, ": overflow 01B\n\n")
		case 2: // nothing delivered before the cut: resume from the daemon's cursor
			fmt.Fprint(w, ": overflow 01B\n\n")
		default:
			ev("01C")
		}
	})
	c := WithHandler(h)
	var got []string
	err := c.Events(context.Background(), api.EventQuery{}, func(e api.Event) error {
		got = append(got, e.ID)
		if e.ID == "01C" {
			return events.ErrStop // the caller is done; the live stream would go on
		}
		return nil
	})
	if err != nil || strings.Join(got, ",") != "01A,01B,01C" || strings.Join(sinces, ",") != ",01B,01B" {
		t.Fatalf("got %v sinces %q err %v", got, sinces, err)
	}
	// A daemon that keeps cutting off without progress is an error, not a loop.
	sinces = nil
	stuck := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sinces = append(sinces, r.URL.Query().Get("since"))
		w.Header().Set(api.HeaderProtocol, api.Protocol)
		fmt.Fprint(w, ": overflow 01B\n\n")
	})
	err = WithHandler(stuck).Events(context.Background(), api.EventQuery{}, func(api.Event) error { return nil })
	if !errors.Is(err, events.ErrLagged) || len(sinces) != MaxResumes+1 {
		t.Fatalf("stuck stream: %v after %d tries", err, len(sinces))
	}
}

func TestHostURL(t *testing.T) {
	for in, want := range map[string]string{
		"mini":                      "http://zm-mini:7433",
		"zm-mini":                   "http://zm-mini:7433",
		"zm-mini.tail1234.ts.net":   "http://zm-mini.tail1234.ts.net:7433",
		"100.64.0.1:9000":           "http://100.64.0.1:9000",
		"https://zm-mini.ts.net:1/": "https://zm-mini.ts.net:1/",
	} {
		if got := HostURL(in); got != want {
			t.Errorf("HostURL(%q) = %q, want %q", in, got, want)
		}
	}
}

// A request whose reused connection the daemon had closed is retried by the
// transport; when that last dial finds the socket missing or refused, the
// daemon is not running, though an earlier attempt got a connection. Any
// other failure after a connection is a cut-off.
func TestSocketFailureLastDialDecides(t *testing.T) {
	dial := func(errno syscall.Errno) error {
		return &url.Error{Op: "Get", URL: "http://zm/v0/health", Err: &net.OpError{Op: "dial", Net: "unix", Err: os.NewSyscallError("connect", errno)}}
	}
	for _, tc := range []struct {
		name    string
		err     error
		reached bool
		none    bool
	}{
		{"refused, never reached", dial(syscall.ECONNREFUSED), false, true},
		{"missing after a reused connection failed", dial(syscall.ENOENT), true, true},
		{"refused after a reused connection failed", dial(syscall.ECONNREFUSED), true, true},
		{"reset mid-request", &url.Error{Op: "Get", URL: "http://zm/v0/health", Err: &net.OpError{Op: "read", Net: "unix", Err: os.NewSyscallError("read", syscall.ECONNRESET)}}, true, false},
		{"EOF mid-request", &url.Error{Op: "Get", URL: "http://zm/v0/health", Err: io.EOF}, true, false},
	} {
		err := socketFailure("/tmp/zm.sock", http.MethodGet, tc.err, tc.reached)
		if NotRunning(err) != tc.none {
			t.Errorf("%s: NotRunning=%v (%v)", tc.name, NotRunning(err), err)
		}
	}
}
