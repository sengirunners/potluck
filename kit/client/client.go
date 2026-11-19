// Package client talks to a zm daemon: over $ZM_HOME/zm.sock by
// default, or over HTTP to $ZM_HOST (a daemon's tailnet listener) with
// $ZM_TOKEN. The core transport and the core endpoints are in client.go and
// project.go (integrator); each module owns its own file of typed methods
// (mail.go, work.go, session.go, tailnet.go).
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/events"
)

// Client is safe for concurrent use once configured.
type Client struct {
	base string // "http://zm" for the socket, else ZM_HOST
	hc   *http.Client
	// Socket is the Unix socket path, when using one.
	Socket string

	// As is sent as Zm-As (the caller's reply address) when non-empty.
	As string
	// Token is sent as "Authorization: Bearer <Token>" when non-empty.
	Token string
	// Project is sent as Zm-Project; ProjectFrom says where it came from
	// ("flag" or "env"), for `zm project why`.
	Project     string
	ProjectFrom string
	// Cwd is sent as Zm-Cwd on the Unix socket only (never over the network).
	Cwd string
	// NoQueue sends Zm-No-Queue: 1: a team replica never queues the write
	// (a claim, lease or release); unanswered, it fails with an outcome.
	NoQueue bool
	// Autostart, when set, starts this machine's daemon (SPEC 17.1). It is
	// called when a socket request finds no daemon (it never connected, so
	// nothing was sent) and this process has never reached one; the request
	// is then sent once more. nil (the default) never starts anything.
	Autostart func(ctx context.Context) error
}

// everConnected is set once any request in this process got a connection
// to a daemon socket. A daemon that goes away after that is not restarted
// behind the caller's back (zm daemon stop while the cockpit runs).
var everConnected atomic.Bool

// startAttempt makes one start attempt per process, success or failure: a
// long-lived caller (event reconnects) that keeps finding
// no daemon after a failed start gets that failure again at once instead of
// another 10 s start on every request. Concurrent callers wait for the one
// attempt and share its outcome. An attempt cut short by its caller's
// context does not count.
var startAttempt struct {
	sync.Mutex
	tried bool
	err   error
}

func (c *Client) autostartOnce(ctx context.Context) error {
	a := &startAttempt
	a.Lock()
	defer a.Unlock()
	if a.tried {
		return a.err
	}
	err := c.Autostart(ctx)
	if ctx.Err() == nil {
		a.tried, a.err = true, err
	}
	return err
}

// New returns a client for cfg: $ZM_HOST (with $ZM_TOKEN) if set, else
// cfg's socket. As = DefaultAs; Project = $ZM_PROJECT; Cwd = the working dir.
func New(cfg *config.Config) *Client {
	var c *Client
	if h := os.Getenv(config.EnvHost); h != "" {
		c = NewRemote(HostURL(h), nil)
	} else {
		c = NewUnix(cfg.SocketPath())
		c.Cwd, _ = os.Getwd()
	}
	c.As = DefaultAs(cfg, os.Getenv)
	c.Token = os.Getenv(config.EnvToken)
	if p := os.Getenv(config.EnvProject); p != "" {
		c.Project, c.ProjectFrom = p, "env"
	}
	return c
}

// Agent runtimes' own session ids, as set in the processes they start
// (verified by the integrations builder; an unset variable is harmless).
var RuntimeSessionEnv = []string{"CLAUDE_CODE_SESSION_ID", "CODEX_THREAD_ID"}

// DefaultAs is who a CLI call answers as: $ZM_AS; else $ZM_SESSION; else
// <machine>/<Session8 of the runtime's id> (the address its hooks register);
// else "" (the human). With $ZM_TOKEN set neither session fallback is used:
// the caller is the token's identity (a guest running zm inside their own Claude
// Code session would otherwise be refused on every call).
func DefaultAs(cfg *config.Config, getenv func(string) string) string {
	if as := getenv(config.EnvAs); as != "" {
		return as
	}
	if getenv(config.EnvToken) != "" {
		return ""
	}
	if sess := getenv("ZM_SESSION"); sess != "" {
		return sess
	}
	for _, k := range RuntimeSessionEnv {
		if id := strings.TrimSpace(getenv(k)); id != "" {
			return api.SessionAddress(cfg.Machine, api.Session8(id))
		}
	}
	return ""
}

// TailnetPort is the port every zm daemon's tailnet listener uses.
const TailnetPort = "7433"

// HostURL turns $ZM_HOST into a base URL: a URL is used as is; a bare
// machine name ("mini") means its tailnet node by its older name,
// http://zm-mini:7433 (a node made from the rename's step 2 on is
// potluck-mini: name it so); a host name or host:port gets http://.
func HostURL(h string) string {
	h = strings.TrimSpace(h)
	switch {
	case strings.Contains(h, "://"):
		return h
	case !strings.ContainsAny(h, ".:"):
		if !strings.HasPrefix(h, api.NodePrefix) && !strings.HasPrefix(h, api.NodePrefixNew) {
			h = api.NodePrefix + h
		}
		return "http://" + h + ":" + TailnetPort
	case !strings.Contains(h, ":"):
		return "http://" + h + ":" + TailnetPort
	}
	return "http://" + h
}

// DialTimeout bounds connecting to the Unix socket.
var DialTimeout = time.Second

// NewUnix returns a client for a daemon's Unix socket.
func NewUnix(socket string) *Client {
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			// A stopped daemon fails at once (no socket, or refused); the
			// timeout is for a wedged one, so hooks and the cockpit fail fast.
			d := net.Dialer{Timeout: DialTimeout}
			return d.DialContext(ctx, "unix", socket)
		},
		// One process keeps one or a few connections to its own daemon: the
		// cockpit and MCP server reuse them for every call.
		MaxIdleConns: 8, MaxIdleConnsPerHost: 8, IdleConnTimeout: 90 * time.Second,
		DisableCompression: true, // a local socket: gzip only costs CPU
	}
	return &Client{base: "http://zm", hc: &http.Client{Transport: tr}, Socket: socket}
}

// NewRemote returns a client for a daemon's base URL (its tailnet listener).
// hc may carry a custom transport; nil = http.DefaultClient.
func NewRemote(baseURL string, hc *http.Client) *Client {
	if hc == nil {
		hc = http.DefaultClient
	}
	return &Client{base: strings.TrimRight(baseURL, "/"), hc: hc}
}

// WithHandler returns a client that calls h in-process (tests).
func WithHandler(h http.Handler) *Client {
	return &Client{base: "http://zm", hc: &http.Client{Transport: handlerTransport{h}}}
}

type handlerTransport struct{ h http.Handler }

func (t handlerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Body == nil { // like a real server: handlers always see a body
		r.Body = http.NoBody
	}
	rec := newRecorder()
	t.h.ServeHTTP(rec, r)
	return rec.result(r), nil
}

// Do sends in as JSON (nil = no body) and decodes a 2xx response into out
// (nil = discard). A non-2xx response is returned as *api.Error.
// Long-running calls rely on ctx; the client sets no global timeout.
func (c *Client) Do(ctx context.Context, method, path string, in, out any) error {
	resp, err := c.send(ctx, method, path, in, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusAccepted {
		// A team replica queued the write (its authority is away): not done.
		var q api.QueuedResponse
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if json.Unmarshal(b, &q) == nil && q.Queued {
			return &api.Error{Code: api.CodeQueued, Message: q.Note, Hint: "write " + q.Write + "; " + config.Command("outbox") + " shows how it ends",
				Data: json.RawMessage(b), HTTPStatus: resp.StatusCode}
		}
		if out == nil {
			return nil
		}
		return json.Unmarshal(b, out)
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// Stream sends a request and returns the open 2xx response for the caller to
// read (SSE, scrollback). The caller closes the body.
func (c *Client) Stream(ctx context.Context, method, path string, in any) (*http.Response, error) {
	return c.send(ctx, method, path, in, nil)
}

// StreamHeader is Stream with extra request headers: for a caller outside
// this package that must read the answer's headers, such as an Upgrade
// offering more than one protocol (zm's AttachEmbed). The caller closes the
// body.
func (c *Client) StreamHeader(ctx context.Context, method, path string, in any, hdr http.Header) (*http.Response, error) {
	return c.send(ctx, method, path, in, hdr)
}

// Upgrade performs an HTTP Upgrade (e.g. api.AttachProtocol) and returns the
// raw bidirectional connection.
func (c *Client) Upgrade(ctx context.Context, path, protocol string) (io.ReadWriteCloser, error) {
	resp, err := c.send(ctx, http.MethodGet, path, nil, http.Header{"Connection": {"Upgrade"}, "Upgrade": {protocol}})
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		resp.Body.Close()
		return nil, fmt.Errorf("upgrade %s: got %s", path, resp.Status)
	}
	rwc, ok := resp.Body.(io.ReadWriteCloser)
	if !ok {
		resp.Body.Close()
		return nil, fmt.Errorf("upgrade %s: body is not writable", path)
	}
	return rwc, nil
}

func (c *Client) send(ctx context.Context, method, path string, in any, hdr http.Header) (*http.Response, error) {
	var body []byte
	if raw, ok := in.(RawBody); ok {
		body = raw.Data
		hdr = hdr.Clone()
		if hdr == nil {
			hdr = http.Header{}
		}
		hdr.Set("Content-Type", raw.Type)
	} else if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return nil, err
		}
		body = b
	}
	resp, err := c.sendOnce(ctx, method, path, body, in != nil, hdr)
	if err == nil || c.Autostart == nil || !errors.Is(err, errNoDaemon) || everConnected.Load() {
		return resp, err
	}
	// Nothing reached a daemon, so nothing was applied: start one, send again.
	if serr := c.autostartOnce(ctx); serr != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &api.Error{Code: api.CodeUnavailable, Message: config.Program + " daemon not running at " + c.Socket + " and it did not start: " + serr.Error(),
			Hint: "start it by hand to see why: " + config.Command("daemon")}
	}
	return c.sendOnce(ctx, method, path, body, in != nil, hdr)
}

// RawBody is a request body sent as it is (not JSON), with its type.
type RawBody struct {
	Data []byte
	Type string
}

// errNoDaemon marks the "not running" answer: the request never connected.
var errNoDaemon = errors.New("no daemon")

// noDaemonError is the "not running" answer; errors.Is(err, errNoDaemon).
type noDaemonError struct{ e *api.Error }

func (e noDaemonError) Error() string   { return e.e.Error() }
func (e noDaemonError) Unwrap() []error { return []error{e.e, errNoDaemon} }

// NotRunning reports whether err says no daemon answered on the socket at all
// (as opposed to one that stopped during the request).
func NotRunning(err error) bool { return errors.Is(err, errNoDaemon) }

func (c *Client) sendOnce(ctx context.Context, method, path string, b []byte, hasBody bool, hdr http.Header) (*http.Response, error) {
	var body io.Reader
	if hasBody {
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return nil, err
	}
	for k, v := range hdr {
		req.Header[k] = v
	}
	if hasBody && req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}
	api.SetHeader(req.Header, api.HeaderProtocol, api.WireProtocol)
	if c.As != "" {
		api.SetHeader(req.Header, api.HeaderAs, c.As)
	}
	if c.Token != "" {
		req.Header.Set(api.HeaderAuthorization, "Bearer "+c.Token)
	}
	if c.Project != "" {
		api.SetHeader(req.Header, api.HeaderProject, c.Project)
		if c.ProjectFrom != "" {
			api.SetHeader(req.Header, api.HeaderProjectFrom, c.ProjectFrom)
		}
	}
	if c.NoQueue {
		api.SetHeader(req.Header, api.HeaderNoQueue, "1")
	}
	if c.Cwd != "" && c.Socket != "" {
		api.SetHeader(req.Header, api.HeaderCwd, c.Cwd)
	}
	// reached: this request got a connection to the daemon, so a failure
	// after that is the daemon going away mid-request, not "not running".
	var reached atomic.Bool
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{
		GotConn: func(httptrace.GotConnInfo) {
			reached.Store(true)
			if c.Socket != "" {
				everConnected.Store(true)
			}
		},
	}))
	resp, err := c.hc.Do(req)
	if err != nil {
		if c.Socket != "" && ctx.Err() == nil {
			return nil, socketFailure(c.Socket, method, err, reached.Load())
		}
		return nil, err
	}
	if p := api.GetHeader(resp.Header, api.HeaderProtocol); !api.SameProtocol(p, api.WireProtocol) {
		resp.Body.Close()
		if p == "" {
			p = "no " + config.Program + " protocol"
		}
		return nil, &api.Error{Code: api.CodeUnavailable, Message: fmt.Sprintf("the daemon speaks %s; this %s speaks %s", p, config.Program, api.WireProtocol),
			Hint: "use the same " + config.Program + " version for the daemon and the CLI (" + config.Command("daemon stop") + "; " + config.Command("daemon", "--detach") + ")"}
	}
	if resp.StatusCode >= 300 && resp.StatusCode != http.StatusSwitchingProtocols {
		defer resp.Body.Close()
		var er api.ErrorResponse
		b, _ := io.ReadAll(io.LimitReader(resp.Body, api.MaxBody))
		if json.Unmarshal(b, &er) != nil || er.Error == nil {
			er.Error = &api.Error{Code: api.CodeInternal, Message: strings.TrimSpace(resp.Status + " " + string(b))}
		}
		er.Error.HTTPStatus = resp.StatusCode
		return nil, er.Error
	}
	return resp, nil
}

// socketFailure classifies a failed request over the socket. The last dial
// finding the socket missing or refused is "not running" even when an
// earlier attempt got a connection: the transport retries a request whose
// reused idle connection the daemon had closed, so a daemon that just
// stopped looks reached and then not there. Any other failure after a
// connection is a cut-off.
func socketFailure(socket, method string, err error, reached bool) error {
	var oe *net.OpError
	dial := errors.As(err, &oe) && oe.Op == "dial"
	if (dial || !reached) && isNoDaemon(err) {
		return noDaemonError{&api.Error{Code: api.CodeUnavailable, Message: config.Program + " daemon not running at " + socket,
			Hint: "start it with: " + config.Command("daemon", "--detach") + "; most " + config.Program + " commands start it on their own"}}
	}
	if reached {
		return cutOff(socket, method)
	}
	return err
}

// cutOff is the answer for a request the daemon stopped answering after it
// was connected: it stopped or restarted meanwhile (zm daemon stop, an
// upgrade). A write may or may not have been applied.
func cutOff(socket, method string) *api.Error {
	e := &api.Error{Code: api.CodeUnavailable, Message: "the " + config.Program + " daemon at " + socket + " stopped or restarted during this request",
		Hint: "run it again once the daemon is back (" + config.Command("status") + "; " + config.Command("daemon", "--detach") + " starts it)"}
	if method != http.MethodGet && method != http.MethodHead {
		e.Hint = "it may or may not have taken effect: check, then retry once the daemon is back (" + config.Command("status") + "; " + config.Command("daemon", "--detach") + " starts it)"
	}
	return e
}

// dropped reports whether a stream read failed because the connection went
// away (as opposed to bad data).
func dropped(err error) bool {
	return errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, net.ErrClosed) ||
		errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE) || strings.Contains(err.Error(), "connection reset")
}

// isNoDaemon: the dial found the socket missing or refused. The text match
// is the fallback for an error that does not wrap its errno.
func isNoDaemon(err error) bool {
	if errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ECONNREFUSED) {
		return true
	}
	s := err.Error()
	return strings.Contains(s, "no such file") || strings.Contains(s, "connection refused")
}

// Seg escapes one path segment: "/v0/work/" + Seg("gh:o/r#7") + "/claim".
func Seg(s string) string { return url.PathEscape(s) }

// Query appends non-empty values as a query string.
func Query(path string, kv ...string) string {
	q := url.Values{}
	for i := 0; i+1 < len(kv); i += 2 {
		if kv[i+1] != "" {
			q.Set(kv[i], kv[i+1])
		}
	}
	if len(q) == 0 {
		return path
	}
	return path + "?" + q.Encode()
}

// Itoa formats ints for Query; 0 becomes "".
func Itoa(n int64) string {
	if n == 0 {
		return ""
	}
	return strconv.FormatInt(n, 10)
}

// Bool formats bools for Query; false becomes "".
func Bool(b bool) string {
	if b {
		return "1"
	}
	return ""
}

// Health is GET /v0/health.
func (c *Client) Health(ctx context.Context) (api.Health, error) {
	var h api.Health
	return h, c.Do(ctx, http.MethodGet, "/v0/health", nil, &h)
}

// Whoami is GET /v0/whoami.
func (c *Client) Whoami(ctx context.Context) (api.Whoami, error) {
	var w api.Whoami
	return w, c.Do(ctx, http.MethodGet, "/v0/whoami", nil, &w)
}

// StopDaemon is POST /v0/daemon/stop.
func (c *Client) StopDaemon(ctx context.Context, req api.DaemonStopRequest) (api.DaemonStopResponse, error) {
	var r api.DaemonStopResponse
	return r, c.Do(ctx, http.MethodPost, "/v0/daemon/stop", req, &r)
}

// Events streams GET /v0/events, calling fn per event until ctx ends, the
// stream ends (q.NoLive) or fn returns an error (events.ErrStop = stop quietly).
// When the daemon cuts the stream off because this reader fell behind
// (": overflow <id>"), Events reconnects with since = the last event fn saw
// (or the cursor the daemon named) so fn sees every event exactly once, in
// order. It gives up after MaxResumes consecutive overflows with no progress.
func (c *Client) Events(ctx context.Context, q api.EventQuery, fn func(api.Event) error) error {
	live := ""
	if q.NoLive {
		live = "0"
	}
	since, stalls := q.Since, 0
	for {
		resp, err := c.Stream(ctx, http.MethodGet,
			Query("/v0/events", "types", q.Types, "subject", q.Subject, "project", q.Project, "since", since, "live", live), nil)
		if err != nil {
			return err
		}
		seen, stopped, fnErr := "", false, false
		err = events.ReadSSE(resp.Body, func(e api.Event) error {
			seen = e.ID
			err := fn(e)
			stopped, fnErr = err == events.ErrStop, err != nil && err != events.ErrStop
			return err
		})
		resp.Body.Close()
		if ctx.Err() != nil || stopped {
			return nil
		}
		var lag *events.LagError
		if !errors.As(err, &lag) {
			// A live stream never ends on its own: the daemon stopped or
			// restarted (zm daemon stop ends live streams cleanly). Say
			// so instead of exiting 0 in silence. A replay (NoLive) ends.
			if !fnErr && ((err == nil && !q.NoLive) || (err != nil && dropped(err))) {
				where := c.Socket
				if where == "" {
					where = c.base
				}
				return cutOff(where, http.MethodGet)
			}
			return err
		}
		switch {
		case seen != "":
			since, stalls = seen, 0
		case lag.Last != "":
			since = lag.Last
			stalls++
		default:
			stalls++
		}
		if stalls > MaxResumes {
			return err
		}
	}
}

// MaxResumes bounds back-to-back event-stream overflows that delivered nothing.
var MaxResumes = 3

// WaitReady polls Health until the daemon answers or timeout passes.
func (c *Client) WaitReady(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		_, err := c.Health(ctx)
		if err == nil || time.Now().After(deadline) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// Manifest is GET /v0/daemon/manifest (SPEC 2, "Swaps").
func (c *Client) Manifest(ctx context.Context) (api.DaemonManifest, error) {
	var m api.DaemonManifest
	return m, c.Do(ctx, http.MethodGet, "/v0/daemon/manifest", nil, &m)
}

// RecordSwap is POST /v0/daemon/swaps.
func (c *Client) RecordSwap(ctx context.Context, rec api.SwapRecord) (api.SwapRecord, error) {
	var r api.SwapRecord
	return r, c.Do(ctx, http.MethodPost, "/v0/daemon/swaps", rec, &r)
}
