package server_test

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/hometerm"
	. "github.com/sengirunners/potluck/kit/server"
	"github.com/sengirunners/potluck/kit/store"
	"github.com/sengirunners/potluck/kit/swap"
	"github.com/sengirunners/potluck/kit/tunnel"
	"github.com/sengirunners/potluck/wiring"
)

// termHome is a short home with term t written (none when t is nil), and
// its config.
func termHome(t *testing.T, term *hometerm.Term) *config.Config {
	t.Helper()
	home := shortHome(t)
	if term != nil {
		if err := hometerm.Create(home, *term); err != nil {
			t.Fatal(err)
		}
	}
	cfg, err := config.LoadFrom(home)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Human, cfg.Machine = "owner", "host"
	return cfg
}

// served is a daemon's Serve, running in the test.
type served struct {
	done chan struct{} // closed when Serve returned
	err  error         // what it returned
}

// serve runs dm until the test ends. A daemon that stopped before it
// answered (one that fenced itself at start) is not waited for.
func serve(t *testing.T, dm *Daemon, cfg *config.Config) (*client.Client, *served) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	s := &served{done: make(chan struct{})}
	go func() {
		s.err = dm.Serve(ctx)
		close(s.done)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-s.done:
		case <-time.After(10 * time.Second):
			t.Error("the daemon did not stop")
		}
	})
	c := client.NewUnix(cfg.SocketPath())
	if err := c.WaitReady(ctx, 5*time.Second); err != nil {
		select {
		case <-s.done:
		default:
			t.Fatal(err)
		}
	}
	return c, s
}

// waitServed waits for Serve to return.
func waitServed(t *testing.T, s *served) {
	t.Helper()
	select {
	case <-s.done:
		if s.err != nil {
			t.Fatalf("serve: %v", s.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the daemon did not stop")
	}
}

func loadTerm(t *testing.T, cfg *config.Config) hometerm.Term {
	t.Helper()
	got, err := hometerm.Load(cfg.Home)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// parkedAnswer fails unless err is the parked answer saying state.
func parkedAnswer(t *testing.T, what string, err error, state hometerm.State) {
	t.Helper()
	var e *api.Error
	if !errors.As(err, &e) || e.Code != api.CodeParked || e.HTTPStatus != http.StatusServiceUnavailable {
		t.Fatalf("%s: %v, want the parked answer", what, err)
	}
	var sb api.ServedBy
	if json.Unmarshal(e.Data, &sb) != nil || sb.State != string(state) || sb.Machine != "host" {
		t.Fatalf("%s: parked answer's data %s, want state %s", what, e.Data, state)
	}
}

// A home whose term does not serve gets a parked daemon: it claims nothing
// and opens no store, answers its health and its term, lets this machine's
// owner stop it, and answers every other call with the one parked code. A
// term that cannot be read starts no daemon.
func TestParkedDaemon(t *testing.T) {
	succ := hometerm.Peer{Via: "tailnet", URL: "https://zm-next.example.ts.net", Fingerprint: "fp-next"}
	fenced, _ := hometerm.Initial().Fence(succ, 3)
	frozen, _ := hometerm.Term{Epoch: 2, State: hometerm.Serving}.Designate(succ)
	frozen, _ = frozen.Freeze()
	standby, _ := hometerm.NewStandby(succ, 1)
	for _, term := range []hometerm.Term{fenced, frozen, standby} {
		t.Run(string(term.State), func(t *testing.T) {
			cfg := termHome(t, &term)
			dm, err := New(context.Background(), cfg, DiscardLog(), wiring.Kit)
			if err != nil {
				t.Fatalf("a %s home has no daemon: %v", term.State, err)
			}
			if len(dm.Modules()) != 0 || dm.Deps.Store != nil {
				t.Fatalf("a %s home's daemon wired %d modules (store %v)", term.State, len(dm.Modules()), dm.Deps.Store)
			}
			c, done := serve(t, dm, cfg)
			ctx := context.Background()
			if h, err := c.Health(ctx); err != nil || h.Machine != "host" || h.PID == 0 {
				t.Fatalf("health: %+v %v", h, err)
			}
			// It holds no writer lock: a tool may change the parked home.
			unlock, err := config.LockWriter(cfg.Home)
			if err != nil {
				t.Fatalf("a %s home's daemon holds the writer lock: %v", term.State, err)
			}
			unlock()
			at, err := c.AuthorityTerm(ctx)
			if err != nil || at.State != string(term.State) || at.Epoch != term.Epoch || at.Fingerprint != "" {
				t.Fatalf("term: %+v %v", at, err)
			}
			// The fenced home names the one it fenced for, the frozen one the
			// standby it named; a standby names none (succ is its predecessor).
			if want := map[bool]*api.HomeKey{true: {Via: succ.Via, Fingerprint: succ.Fingerprint}}[term.State != hometerm.Standby]; !sameKey(at.Successor, want) {
				t.Fatalf("the %s term names successor %+v, want %+v", term.State, at.Successor, want)
			}
			for _, call := range []struct{ method, path string }{
				{"GET", "/v0/whoami"}, {"GET", "/v0/projects"}, {"POST", "/v0/projects"}, {"GET", "/v0/events"},
				{"POST", "/v0/team/presence"}, {"POST", "/v0/authority/freeze"}, {"POST", "/v0/health"},
				{"DELETE", "/v0/authority/term"}, {"GET", "/v0/no/such/route"}, {"GET", "/v0/daemon/manifest"},
			} {
				parkedAnswer(t, call.method+" "+call.path, c.Do(ctx, call.method, call.path, struct{}{}, nil), term.State)
			}
			err = c.Do(ctx, "GET", "/v0/projects", nil, nil)
			var ae *api.Error
			var sb api.ServedBy
			if !errors.As(err, &ae) || json.Unmarshal(ae.Data, &sb) != nil {
				t.Fatalf("the parked answer: %v", err)
			}
			if want := map[bool]string{true: succ.URL}[term.State == hometerm.Fenced]; sb.SuccessorURL != want {
				t.Fatalf("the owner is told HQ is at %q, want %q", sb.SuccessorURL, want)
			}
			if term.State == hometerm.Frozen && !strings.Contains(err.Error(), "daemon unfreeze") {
				t.Fatalf("the owner of a frozen home is not told how to serve it again: %v", err)
			}

			// Off the socket (a handler handed to another listener), a
			// caller is not the owner: it may not stop or unfreeze, and is
			// not told where HQ is.
			for _, p := range []string{"/v0/daemon/stop", "/v0/authority/unfreeze"} {
				w := httptest.NewRecorder()
				dm.Handler.ServeHTTP(w, httptest.NewRequest("POST", p, strings.NewReader("{}")))
				if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), `"parked"`) || strings.Contains(w.Body.String(), succ.URL) {
					t.Fatalf("POST %s off the socket: %d %s", p, w.Code, w.Body.String())
				}
			}

			if _, err := c.StopDaemon(ctx, api.DaemonStopRequest{What: "stop"}); err != nil {
				t.Fatal(err)
			}
			waitServed(t, done)
			if _, err := os.Stat(filepath.Join(cfg.Home, store.FileRegistry)); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("a %s home's store was opened (%v)", term.State, err)
			}
			if owner := config.HomeOwner(cfg.Home); owner != "" {
				t.Fatalf("a %s home was claimed by %q", term.State, owner)
			}
			if got := loadTerm(t, cfg); got.State != term.State || got.Epoch != term.Epoch {
				t.Fatalf("the term changed: %+v", got)
			}
		})
	}

	t.Run("unreadable term", func(t *testing.T) {
		cfg := termHome(t, nil)
		if err := os.WriteFile(filepath.Join(cfg.Home, hometerm.File), []byte("{not json"), 0o600); err != nil {
			t.Fatal(err)
		}
		if dm, err := New(context.Background(), cfg, DiscardLog(), wiring.Kit); err == nil || !strings.Contains(err.Error(), "until its term can be read") {
			if dm != nil {
				dm.Close()
			}
			t.Fatalf("a home whose term cannot be read: %v", err)
		}
		if _, err := os.Stat(filepath.Join(cfg.Home, store.FileRegistry)); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("its store was opened (%v)", err)
		}
	})
}

// freeLoopback is a loopback address nothing listens on now.
func freeLoopback(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

// A parked daemon given its listeners (ParkedListeners) answers its term to
// other machines, here over the real tunnel door with no token, naming the
// key it was reached by. Every other call over the network is answered
// parked, its health too, and tells the caller nothing of where HQ is.
func TestParkedDaemonAnswersItsTermOverTheNetwork(t *testing.T) {
	ctx := context.Background()
	succ := hometerm.Peer{Via: core.ViaTunnel, URL: "https://next.example", Fingerprint: "fp-next"}
	fenced, _ := hometerm.Term{Epoch: 3, State: hometerm.Serving}.Fence(succ, 4)
	cfg := termHome(t, &fenced)
	listen := freeLoopback(t)
	cfg.File.Tunnel = config.Tunnel{Listen: listen, URL: "http://" + listen}
	dm, err := New(ctx, cfg, DiscardLog(), wiring.Kit, ParkedListeners(wiring.Parked))
	if err != nil {
		t.Fatal(err)
	}
	serve(t, dm, cfg)
	fp, err := dm.Deps.HomeFingerprint(ctx, core.ViaTunnel)
	if err != nil {
		t.Fatal(err)
	}

	caller := termHome(t, nil)
	cdm, err := New(ctx, caller, DiscardLog(), wiring.Kit)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cdm.Close)
	hc := cdm.Deps.Transport(core.ViaTunnel).Client(core.JoinTarget{Via: core.ViaTunnel, URL: "http://" + listen, Fingerprint: fp}, core.ClientOptions{DialTimeout: 5 * time.Second})
	call := func(method, path, token string) (int, string) {
		t.Helper()
		req, _ := http.NewRequestWithContext(ctx, method, "http://"+listen+path, nil)
		api.SetHeader(req.Header, api.HeaderProtocol, api.WireProtocol)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := hc.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		defer resp.Body.Close()
		var b strings.Builder
		_, _ = io.Copy(&b, resp.Body)
		return resp.StatusCode, b.String()
	}
	code, body := call("GET", api.TermPath, "")
	var at api.AuthorityTerm
	if code != http.StatusOK || json.Unmarshal([]byte(body), &at) != nil || at.State != api.HQFenced || at.Epoch != 4 || at.Fingerprint != fp {
		t.Fatalf("the parked home's term over the tunnel: %d %s", code, body)
	}
	// It names the key it fenced for, never that home's address.
	if !sameKey(at.Successor, &api.HomeKey{Via: core.ViaTunnel, Fingerprint: "fp-next"}) || strings.Contains(body, succ.URL) {
		t.Fatalf("the fenced home's successor over the tunnel: %s", body)
	}
	for _, c := range []struct{ method, path string }{{"GET", "/v0/health"}, {"GET", "/v0/whoami"}, {"POST", "/v0/team/presence"}, {"POST", "/v0/authority/unfreeze"}, {"POST", "/v0/daemon/stop"}} {
		code, body := call(c.method, c.path, "zm_tok")
		if code != http.StatusServiceUnavailable || !strings.Contains(body, `"parked"`) || strings.Contains(body, succ.URL) {
			t.Fatalf("%s %s over the tunnel: %d %s", c.method, c.path, code, body)
		}
	}
	if got := loadTerm(t, cfg); got.State != hometerm.Fenced || got.Epoch != 4 {
		t.Fatalf("the term changed: %+v", got)
	}
	// Tokens it cannot check put nothing in its replay journal.
	if b, err := os.ReadFile(filepath.Join(cfg.Home, tunnel.NonceFile)); len(b) != 0 {
		t.Fatalf("the parked home journaled the token callers' nonces: %q (%v)", b, err)
	}
}

// joinedNet is a tailnet module whose run joins the tailnet or not.
type joinedNet struct {
	fakeNet
	enabled bool
}

func (n joinedNet) Self(context.Context) (core.NetSelf, error) {
	return core.NetSelf{Enabled: n.enabled}, nil
}

// A daemon's health tells its owner on the socket whether this run joins the
// tailnet, as its tailnet module says (freeze and unfreeze start the next
// with the same listeners), a parked daemon's too; a network caller is not
// told.
func TestHealthSaysTheDaemonJoinsTheTailnet(t *testing.T) {
	ctx := context.Background()
	frozen, _ := hometerm.Term{Epoch: 2, State: hometerm.Serving}.Freeze()
	for _, term := range []*hometerm.Term{nil, &frozen} {
		for _, tailnet := range []bool{true, false} {
			cfg := termHome(t, term)
			withNet := func(d *core.Deps) ([]core.Module, error) {
				d.Net = joinedNet{enabled: tailnet}
				return nil, nil
			}
			dm, err := New(ctx, cfg, DiscardLog(), withNet, ParkedListeners(withNet))
			if err != nil {
				t.Fatal(err)
			}
			c, _ := serve(t, dm, cfg)
			if h, err := c.Health(ctx); err != nil || h.Tailnet != tailnet {
				t.Fatalf("health on the socket (term %v, tailnet %v): %+v %v", term, tailnet, h, err)
			}
			if dm.Deps.Store != nil { // a serving daemon answers health off the socket
				w := httptest.NewRecorder()
				dm.Handler.ServeHTTP(w, httptest.NewRequest("GET", "/v0/health", nil))
				if w.Code != http.StatusOK || strings.Contains(w.Body.String(), "tailnet") {
					t.Fatalf("health off the socket: %d %s", w.Code, w.Body.String())
				}
			}
		}
	}
}

// A serving home (no term file, or one that names a successor) is served,
// and answers its term.
func TestServingDaemonAnswersItsTerm(t *testing.T) {
	succ := hometerm.Peer{Via: "tailnet", URL: "https://zm-next.example.ts.net", Fingerprint: "fp-next"}
	named, _ := hometerm.Term{Epoch: 4, State: hometerm.Serving, Predecessor: &hometerm.Peer{Via: "tunnel", URL: "https://old.example", Fingerprint: "fp-old"}}.Designate(succ)
	for _, term := range []*hometerm.Term{nil, &named} {
		cfg := termHome(t, term)
		dm, err := New(context.Background(), cfg, DiscardLog(), wiring.Kit)
		if err != nil {
			t.Fatal(err)
		}
		c, _ := serve(t, dm, cfg)
		ctx := context.Background()
		if _, err := c.Whoami(ctx); err != nil {
			t.Fatalf("a serving home's daemon: %v", err)
		}
		at, err := c.AuthorityTerm(ctx)
		want := api.AuthorityTerm{State: api.HQServing}
		if term != nil {
			want = api.AuthorityTerm{State: api.HQServing, Epoch: 4, Predecessor: &api.HomeKey{Via: "tunnel", Fingerprint: "fp-old"},
				Successor: &api.HomeKey{Via: "tailnet", Fingerprint: "fp-next"}}
		}
		if err != nil || at.State != want.State || at.Epoch != want.Epoch || !sameKey(at.Predecessor, want.Predecessor) || !sameKey(at.Successor, want.Successor) {
			t.Fatalf("term %+v %v, want %+v", at, err, want)
		}
	}
}

// Freeze stops a serving home at its epoch: from its answer on, every call
// but health and the term is parked, and the daemon stops; the next is
// parked. Unfreeze, on that parked daemon, serves the home again at the same
// epoch from the daemon after it.
func TestFreezeThenUnfreezeKeepsTheEpoch(t *testing.T) {
	cfg := termHome(t, &hometerm.Term{Epoch: 3, State: hometerm.Serving})
	ctx := context.Background()
	dm, err := New(ctx, cfg, DiscardLog(), wiring.Kit)
	if err != nil {
		t.Fatal(err)
	}
	c, done := serve(t, dm, cfg)
	at, err := c.FreezeHome(ctx)
	if err != nil || at.State != api.HQFrozen || at.Epoch != 3 {
		t.Fatalf("freeze: %+v %v", at, err)
	}
	w := httptest.NewRecorder()
	dm.SocketHandler().ServeHTTP(w, httptest.NewRequest("GET", "/v0/projects", nil))
	if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), `"parked"`) {
		t.Fatalf("a call after the freeze answered: %d %s", w.Code, w.Body.String())
	}
	waitServed(t, done)
	if got := loadTerm(t, cfg); got.State != hometerm.Frozen || got.Epoch != 3 {
		t.Fatalf("after freeze the term is %+v", got)
	}

	dm, err = New(ctx, cfg, DiscardLog(), wiring.Kit)
	if err != nil {
		t.Fatal(err)
	}
	c, done = serve(t, dm, cfg)
	parkedAnswer(t, "a frozen home's daemon", c.Do(ctx, "GET", "/v0/whoami", nil, nil), hometerm.Frozen)
	// While another process changes the home, unfreeze is refused.
	unlock, err := config.LockWriter(cfg.Home)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.UnfreezeHome(ctx); !api.IsCode(err, api.CodeConflict) {
		t.Fatalf("unfreeze while another process holds the writer lock: %v", err)
	}
	unlock()
	if got := loadTerm(t, cfg); got.State != hometerm.Frozen {
		t.Fatalf("a refused unfreeze changed the term: %+v", got)
	}
	if at, err := c.UnfreezeHome(ctx); err != nil || at.State != api.HQServing || at.Epoch != 3 {
		t.Fatalf("unfreeze: %+v %v", at, err)
	}
	waitServed(t, done)
	if got := loadTerm(t, cfg); got.State != hometerm.Serving || got.Epoch != 3 {
		t.Fatalf("after unfreeze the term is %+v", got)
	}

	dm, err = New(ctx, cfg, DiscardLog(), wiring.Kit)
	if err != nil {
		t.Fatal(err)
	}
	c, _ = serve(t, dm, cfg)
	if _, err := c.Whoami(ctx); err != nil {
		t.Fatalf("the unfrozen home is not served: %v", err)
	}
	if _, err := c.UnfreezeHome(ctx); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("unfreeze on a serving daemon: %v", err)
	}
}

// Only this machine's owner on its socket freezes the home: a project
// owner's token over the network is refused. A freeze the term refuses
// leaves the daemon serving every call.
func TestFreezeRefusals(t *testing.T) {
	cfg := termHome(t, &hometerm.Term{Epoch: 3, State: hometerm.Serving})
	ctx := context.Background()
	dm, err := New(ctx, cfg, DiscardLog(), wiring.Kit)
	if err != nil {
		t.Fatal(err)
	}
	c, _ := serve(t, dm, cfg)
	if _, err := c.CreateProject(ctx, api.NewProjectRequest{Slug: "proj"}); err != nil {
		t.Fatal(err)
	}
	owner, err := c.MintToken(ctx, api.MintTokenRequest{As: "helper", Project: "proj", Role: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "/v0/authority/freeze", strings.NewReader("{}"))
	req.Header.Set(api.HeaderAuthorization, "Bearer "+owner.Secret)
	req = req.WithContext(core.WithRemote(req.Context(), core.Remote{Login: "x@example.com", NodeID: "nDEV", NodeKey: "nodekey:dev"}))
	w := httptest.NewRecorder()
	dm.Handler.ServeHTTP(w, req)
	if w.Code < 400 || w.Code == http.StatusServiceUnavailable {
		t.Fatalf("a network freeze with a project owner's token: %d %s", w.Code, w.Body.String())
	}
	if got := loadTerm(t, cfg); got.State != hometerm.Serving {
		t.Fatalf("a refused freeze changed the term: %+v", got)
	}

	// During another caller's swap (an upgrade), as a stop is.
	l, err := swap.Acquire(cfg.Home, api.SwapHolder{By: "host/aaaa1111", What: "upgrade"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.FreezeHome(ctx)
	l.Release()
	if !api.IsCode(err, api.CodeConflict) || !strings.Contains(err.Error(), "upgrade") {
		t.Fatalf("a freeze during a swap: %v", err)
	}
	if got := loadTerm(t, cfg); got.State != hometerm.Serving {
		t.Fatalf("a freeze during a swap changed the term: %+v", got)
	}

	// The term moved on under the daemon (frozen by hand): the freeze is
	// refused, and the daemon still serves.
	if _, err := hometerm.Apply(cfg.Home, hometerm.Term.Freeze); err != nil {
		t.Fatal(err)
	}
	if _, err := c.FreezeHome(ctx); !api.IsCode(err, api.CodeConflict) {
		t.Fatalf("a freeze the term refuses: %v", err)
	}
	if _, err := c.Whoami(ctx); err != nil {
		t.Fatalf("after a refused freeze the daemon does not serve: %v", err)
	}

	// A park whose move leaves the home serving is refused, and parks nothing.
	if _, err := hometerm.Apply(cfg.Home, hometerm.Term.Unfreeze); err != nil {
		t.Fatal(err)
	}
	if _, err := dm.Park(func(cur hometerm.Term) (hometerm.Term, error) { return cur, nil }); !api.IsCode(err, api.CodeConflict) {
		t.Fatalf("a park that leaves the home serving: %v", err)
	}
	if _, err := c.Whoami(ctx); err != nil {
		t.Fatalf("after a refused park the daemon does not serve: %v", err)
	}
}

// Once a daemon parks (here: a freeze), a module's public host (a preview)
// is answered parked too, before its handler runs; before, it is served.
func TestParkingCoversPublicHosts(t *testing.T) {
	cfg := termHome(t, &hometerm.Term{Epoch: 3, State: hometerm.Serving})
	ctx := context.Background()
	var reached atomic.Int32
	dm, err := New(ctx, cfg, DiscardLog(), func(d *core.Deps) ([]core.Module, error) {
		core.PublicIngress(d).RegisterHostSource(func(h string) bool { return h == "box-1.preview.example" },
			http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached.Add(1); w.WriteHeader(http.StatusOK) }))
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	ingress := core.PublicIngress(dm.Deps).Wrap(dm.Handler)
	preview := func() *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		ingress.ServeHTTP(w, httptest.NewRequest("GET", "http://box-1.preview.example/v0/health", nil))
		return w
	}
	c, done := serve(t, dm, cfg)
	if w := preview(); w.Code != http.StatusOK || reached.Load() != 1 {
		t.Fatalf("a preview while serving: %d (reached %d)", w.Code, reached.Load())
	}
	if _, err := c.FreezeHome(ctx); err != nil {
		t.Fatal(err)
	}
	if w := preview(); w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), `"parked"`) || reached.Load() != 1 {
		t.Fatalf("a preview once parking: %d %s (reached %d)", w.Code, w.Body.String(), reached.Load())
	}
	waitServed(t, done)
}

// fakeTunnel stands in for the tunnel transport: its client reaches the
// test's successor, and it names this home's key as fp.
type fakeTunnel struct {
	core.TeamTransport
	hc   *http.Client
	fp   string
	asks atomic.Int32
}

func (f *fakeTunnel) Via() string { return core.ViaTunnel }
func (f *fakeTunnel) Client(core.JoinTarget, core.ClientOptions) *http.Client {
	f.asks.Add(1)
	return f.hc
}
func (f *fakeTunnel) HomeFingerprint(context.Context) (string, error) { return f.fp, nil }

// A serving home that named a successor asks it for its term at start and
// every heartbeat. When the successor serves at a higher epoch and took over
// from this home's key, this home writes fenced at that epoch and its daemon
// stops. A successor that does not answer, serves at no higher epoch, does
// not serve, or took over from another home changes nothing.
func TestSelfFence(t *testing.T) {
	defer SetFenceEvery(20 * time.Millisecond)()
	us := &api.HomeKey{Via: core.ViaTunnel, Fingerprint: "fp-home"}
	for _, tc := range []struct {
		name   string
		answer *api.AuthorityTerm // nil: the successor does not answer
		fenced bool
		status int // the successor's status (0: 200)
	}{
		{"serves higher, took over from this home", &api.AuthorityTerm{State: api.HQServing, Epoch: 2, Predecessor: us}, true, 0},
		{"unreachable", nil, false, 0},
		{"serves at the same epoch", &api.AuthorityTerm{State: api.HQServing, Epoch: 1, Predecessor: us}, false, 0},
		{"standby at a higher epoch", &api.AuthorityTerm{State: api.HQStandby, Epoch: 2, Predecessor: us}, false, 0},
		{"fenced at a higher epoch", &api.AuthorityTerm{State: api.HQFenced, Epoch: 5, Predecessor: us}, false, 0},
		{"serves higher, took over from another home", &api.AuthorityTerm{State: api.HQServing, Epoch: 2, Predecessor: &api.HomeKey{Via: core.ViaTunnel, Fingerprint: "fp-other"}}, false, 0},
		{"serves higher, names no predecessor", &api.AuthorityTerm{State: api.HQServing, Epoch: 2}, false, 0},
		{"serves at an epoch out of range", &api.AuthorityTerm{State: api.HQServing, Epoch: hometerm.MaxEpoch + 1, Predecessor: us}, false, 0},
		// At the very top the fence still holds: this home is retired, never left serving beside it.
		{"serves at the top epoch, took over from this home", &api.AuthorityTerm{State: api.HQServing, Epoch: hometerm.MaxEpoch, Predecessor: us}, true, 0},
		{"a fencing term in an error answer", &api.AuthorityTerm{State: api.HQServing, Epoch: 2, Predecessor: us}, false, http.StatusInternalServerError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var answered atomic.Int32
			succSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/v0/authority/term" {
					http.NotFound(w, r)
					return
				}
				answered.Add(1)
				api.WriteJSON(w, cmp.Or(tc.status, http.StatusOK), tc.answer)
			}))
			defer succSrv.Close()
			ft := &fakeTunnel{hc: succSrv.Client(), fp: "fp-home"}
			if tc.answer == nil {
				ft.hc = &http.Client{Transport: roundTrip(func(*http.Request) (*http.Response, error) { return nil, errors.New("unreachable") })}
			}
			succ := hometerm.Peer{Via: core.ViaTunnel, URL: succSrv.URL, Fingerprint: "fp-next"}
			term, _ := hometerm.Term{Epoch: 1, State: hometerm.Serving}.Designate(succ)
			cfg := termHome(t, &term)
			ctx := context.Background()
			dm, err := New(ctx, cfg, DiscardLog(), func(d *core.Deps) ([]core.Module, error) {
				d.AddTransport(ft)
				return nil, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			c, done := serve(t, dm, cfg)
			if tc.fenced {
				waitServed(t, done)
				got := loadTerm(t, cfg)
				if got.State != hometerm.Fenced || got.Epoch != tc.answer.Epoch || got.Successor == nil || *got.Successor != succ {
					t.Fatalf("after its successor took over, the term is %+v", got)
				}
				return
			}
			deadline := time.Now().Add(10 * time.Second)
			for ft.asks.Load() < 3 {
				if time.Now().After(deadline) {
					t.Fatalf("the successor was asked %d times", ft.asks.Load())
				}
				time.Sleep(5 * time.Millisecond)
			}
			if tc.answer != nil && answered.Load() == 0 {
				t.Fatal("the successor was never reached")
			}
			if got := loadTerm(t, cfg); got.State != hometerm.Serving || got.Epoch != 1 || got.Successor == nil || *got.Successor != succ {
				t.Fatalf("the term changed: %+v", got)
			}
			if _, err := c.Whoami(ctx); err != nil {
				t.Fatalf("the home is not served: %v", err)
			}
		})
	}
}

// A term that changes while its successor is asked (the owner named another)
// is not fenced by the answer to the old one.
func TestSelfFenceNotOnATermThatChanged(t *testing.T) {
	us := &api.HomeKey{Via: core.ViaTunnel, Fingerprint: "fp-home"}
	other := hometerm.Peer{Via: core.ViaTunnel, URL: "http://127.0.0.1:1", Fingerprint: "fp-other"}
	var home string
	succSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := hometerm.Apply(home, func(cur hometerm.Term) (hometerm.Term, error) { return cur.Designate(other) }); err != nil {
			t.Error(err)
		}
		api.WriteJSON(w, http.StatusOK, api.AuthorityTerm{State: api.HQServing, Epoch: 2, Predecessor: us})
	}))
	defer succSrv.Close()
	ft := &fakeTunnel{hc: succSrv.Client(), fp: "fp-home"}
	succ := hometerm.Peer{Via: core.ViaTunnel, URL: succSrv.URL, Fingerprint: "fp-next"}
	term, _ := hometerm.Term{Epoch: 1, State: hometerm.Serving}.Designate(succ)
	cfg := termHome(t, &term)
	home = cfg.Home
	ctx := context.Background()
	dm, err := New(ctx, cfg, DiscardLog(), func(d *core.Deps) ([]core.Module, error) {
		d.AddTransport(ft)
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	c, _ := serve(t, dm, cfg)
	if _, err := c.Whoami(ctx); err != nil { // answered once the first ask has its answer
		t.Fatalf("the home is not served: %v", err)
	}
	if got := loadTerm(t, cfg); got.State != hometerm.Serving || got.Epoch != 1 || got.Successor == nil || *got.Successor != other {
		t.Fatalf("the term is %+v, want serving at 1 naming the other successor", got)
	}
}

// A home that named a successor takes no call but its health and its term
// until its first ask of the successor has an answer: one that fences it
// answers the calls it held parked.
func TestFirstFenceHoldsCalls(t *testing.T) {
	release := make(chan struct{})
	us := &api.HomeKey{Via: core.ViaTunnel, Fingerprint: "fp-home"}
	succSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
		api.WriteJSON(w, http.StatusOK, api.AuthorityTerm{State: api.HQServing, Epoch: 2, Predecessor: us})
	}))
	defer succSrv.Close()
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	ft := &fakeTunnel{hc: succSrv.Client(), fp: "fp-home"}
	succ := hometerm.Peer{Via: core.ViaTunnel, URL: succSrv.URL, Fingerprint: "fp-next"}
	term, _ := hometerm.Term{Epoch: 1, State: hometerm.Serving}.Designate(succ)
	cfg := termHome(t, &term)
	ctx := context.Background()
	dm, err := New(ctx, cfg, DiscardLog(), func(d *core.Deps) ([]core.Module, error) {
		d.AddTransport(ft)
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	c, done := serve(t, dm, cfg)
	if at, err := c.AuthorityTerm(ctx); err != nil || at.State != api.HQServing {
		t.Fatalf("the term while the successor is asked: %+v %v", at, err)
	}
	held := make(chan error, 1)
	go func() { _, err := c.Whoami(ctx); held <- err }()
	select {
	case err := <-held:
		t.Fatalf("a call was answered before the successor was: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	close(release)
	select {
	case err := <-held:
		parkedAnswer(t, "the held call", err, hometerm.Fenced)
	case <-time.After(10 * time.Second):
		t.Fatal("the held call was never answered")
	}
	waitServed(t, done)
}

// The fence through the real tunnel door: a home whose named successor is
// reached over the tunnel reads its term with no token of the successor's,
// and fences itself once the successor serves at a higher epoch, having
// taken over from this home's tunnel key.
func TestSelfFenceThroughTheTunnelDoor(t *testing.T) {
	ctx := context.Background()
	cfgA := termHome(t, &hometerm.Term{Epoch: 1, State: hometerm.Serving})
	dmA, err := New(ctx, cfgA, DiscardLog(), wiring.Kit)
	if err != nil {
		t.Fatal(err)
	}
	fpA, err := dmA.Deps.HomeFingerprint(ctx, core.ViaTunnel)
	if err != nil {
		t.Fatal(err)
	}

	cfgB := termHome(t, &hometerm.Term{Epoch: 2, State: hometerm.Serving,
		Predecessor: &hometerm.Peer{Via: core.ViaTunnel, URL: "https://old.example", Fingerprint: fpA}})
	listen := freeLoopback(t)
	cfgB.File.Tunnel = config.Tunnel{Listen: listen, URL: "http://" + listen}
	dmB, err := New(ctx, cfgB, DiscardLog(), wiring.Kit)
	if err != nil {
		t.Fatal(err)
	}
	serve(t, dmB, cfgB)
	fpB, err := dmB.Deps.HomeFingerprint(ctx, core.ViaTunnel)
	if err != nil {
		t.Fatal(err)
	}

	succ := hometerm.Peer{Via: core.ViaTunnel, URL: "http://" + listen, Fingerprint: fpB}
	if _, err := hometerm.Apply(cfgA.Home, func(cur hometerm.Term) (hometerm.Term, error) { return cur.Designate(succ) }); err != nil {
		t.Fatal(err)
	}
	_, done := serve(t, dmA, cfgA)
	waitServed(t, done)
	if got := loadTerm(t, cfgA); got.State != hometerm.Fenced || got.Epoch != 2 || got.Successor == nil || *got.Successor != succ {
		t.Fatalf("after its successor took over, the term is %+v", got)
	}

	// Parked from its next start, it names the successor it fenced for by
	// the key that successor holds, so that home, and only it, can tell
	// the fence is for it.
	dmA, err = New(ctx, cfgA, DiscardLog(), wiring.Kit)
	if err != nil {
		t.Fatal(err)
	}
	c, _ := serve(t, dmA, cfgA)
	if at, err := c.AuthorityTerm(ctx); err != nil || at.State != api.HQFenced || !sameKey(at.Successor, &api.HomeKey{Via: core.ViaTunnel, Fingerprint: fpB}) {
		t.Fatalf("the fenced home's term: %+v %v, want successor %s on the tunnel", at, err, fpB)
	}
}

// sameKey is whether a and b name the same key, or both none.
func sameKey(a, b *api.HomeKey) bool {
	return a == nil && b == nil || a != nil && b != nil && *a == *b
}

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
