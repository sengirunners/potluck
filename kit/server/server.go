// Package server is the daemon shell: it opens the store and the project
// registry, lets the binary's Wiring construct the modules into core.Deps,
// mounts their routes on one guarded router and serves the API on the
// home's socket (config.Names: the program's first socket name). It imports no module (internal/wiring has the lists).
// Owned by the integrator.
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"os"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/events"
	"github.com/sengirunners/potluck/kit/hometerm"
	"github.com/sengirunners/potluck/kit/project"
	"github.com/sengirunners/potluck/kit/store"
)

// Daemon is a wired, not yet listening daemon.
type Daemon struct {
	Deps *core.Deps
	// Handler is the full API with middleware. Requests reaching it are
	// remote (token or WhoIs, else 401) unless their context carries
	// core.WithLocal, which only the Unix listener sets. In-process tests
	// that want the socket's owner use SocketHandler.
	Handler http.Handler
	Router  *core.Router
	modules []core.Module
	stop    chan struct{}
	once    sync.Once
	closing chan struct{} // closed when shutdown starts: live event streams end
	swap    swapState
	stopBy  *stopNote // who asked the daemon to stop (the stopping event's data)
	// started is closed once every module has started (Serve); nil = no
	// Serve yet, so nothing waits (gate).
	started atomic.Pointer[chan struct{}]
	// program is config.Program when New ran: the program whose daemon
	// this is (its home's owner, its machine record).
	program string
}

// startGate bounds how long a socket call waits for the modules to start.
const startGate = 30 * time.Second

// gate holds a call on the socket (all but GET /v0/health) until every
// module has started. The socket listens first, so a client finds the
// daemon at once, and modules start while it does: a call in those moments
// must not find one half started (a restarted replica's GET
// /v0/team/remotes listed a joined project with no copy, or none at all,
// and its first `bd` call could miss the project). Starting never waits on
// the network, so the wait is the modules' few milliseconds; in-process
// callers (d.Handler) never wait.
func (dm *Daemon) gate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ch := dm.started.Load(); ch != nil && r.URL.Path != "/v0/health" {
			select {
			case <-*ch: // started: the usual case, no timer
			default:
				t := time.NewTimer(startGate)
				select {
				case <-*ch:
				case <-r.Context().Done():
					t.Stop()
					return
				case <-t.C:
				}
				t.Stop()
			}
		}
		next.ServeHTTP(w, r)
	})
}

// KeepSessions makes managed sessions outlive this daemon (`zm daemon`):
// their holders keep running when it stops or dies, and the next daemon
// adopts them (SPEC 8). Call before Serve.
func (dm *Daemon) KeepSessions() {
	if h := dm.Deps.SessionHooks(); h != nil && h.Keep != nil {
		h.Keep()
	}
}

// Wiring constructs a daemon's modules over d: it fills d's service fields,
// registers transports and schema, and returns the modules in start order
// (closed in reverse). internal/wiring has the lists; nil wires none.
type Wiring func(d *core.Deps) ([]core.Module, error)

// New opens the store in cfg.Home and wires the modules w returns. The
// store opens with the shell's tables alone (store.OpenForWiring: kit/project
// and kit/events read them below); every other migration, registered or
// added by w's modules, runs in one name order pass once w has built.
func New(ctx context.Context, cfg *config.Config, log *slog.Logger, w Wiring) (*Daemon, error) {
	if log == nil {
		log = slog.Default()
	}
	if err := cfg.EnsureHome(); err != nil {
		return nil, err
	}
	if err := config.CheckNotRestoring(cfg.Home); err != nil {
		return nil, err
	}
	// One authority per team project: a home that moved away, is frozen for
	// a move, or is a standby is not served (kit/hometerm).
	if err := hometerm.CheckServes(cfg.Home); err != nil {
		return nil, err
	}
	program := config.Program
	if err := config.ClaimHome(cfg.Home, program); err != nil {
		return nil, err
	}
	st, err := store.OpenForWiring(ctx, cfg.Home)
	if err != nil {
		return nil, err
	}
	stableMachine(ctx, cfg, st, log)
	if w := cfg.File.SandboxTaskWarning(); w != "" {
		log.Warn(w, "config", cfg.Path(config.FileConfig))
	}
	reg, err := project.Open(ctx, st, cfg)
	if err != nil {
		st.Close()
		return nil, err
	}
	unowned := reg.Unowned()
	for _, slug := range slices.Sorted(maps.Keys(unowned)) {
		log.Warn("project.toml has settings nothing in this build reads: kept and written back, not checked (a misspelt name looks the same)",
			"project", slug, "names", strings.Join(unowned[slug], ", "))
	}
	bus, err := events.New(ctx, st, cfg.Machine)
	if err != nil {
		st.Close()
		return nil, err
	}
	d := &core.Deps{Config: cfg, Store: st, Projects: reg, Events: bus, Log: log, Started: time.Now().UTC()}
	dm := &Daemon{Deps: d, stop: make(chan struct{}), closing: make(chan struct{}), program: program}
	d.Closing = dm.closing
	d.Zm = dm.zmState
	if err := dm.wire(ctx, w); err != nil {
		st.Close()
		return nil, err
	}
	dm.loadLastSwap()
	bus.SetVisibility(func(ctx context.Context, sc authz.Scope, addr string) bool { return d.Visible(ctx, sc, addr) })
	return dm, nil
}

// wire runs w, migrates the store (store.Migrate: the base and what w's
// modules added), then mounts the core routes and every module's on the
// router.
func (dm *Daemon) wire(ctx context.Context, w Wiring) error {
	d := dm.Deps
	if w != nil {
		mods, err := w(d)
		if err != nil {
			return err
		}
		dm.modules = mods
	}
	if err := d.Store.Migrate(ctx); err != nil {
		return err
	}
	if err := d.MigrationsErr(); err != nil {
		return err
	}
	// A redaction scrubs the event log too (the events package cannot
	// import core).
	d.AddScrubber(func(ctx context.Context, sc core.Scrub) error {
		return d.Events.Scrub(ctx, sc.Project, sc.Subjects, sc.Texts, core.MinUnscoped, core.ScrubText, core.ScrubJSON)
	})

	r := core.NewRouter()
	dm.coreRoutes(r.For("core"))
	for _, mod := range dm.modules {
		mod.Routes(r)
	}
	r.Fallback(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		api.WriteError(w, api.Errorf(api.CodeNotFound, "no route %s %s", r.Method, r.URL.Path).WithHint("see API.md"))
	}))
	if err := r.CheckAdmits(); err != nil {
		return err
	}
	dm.Router = r
	dm.Handler = dm.middleware(r)
	d.Handler = dm.Handler
	return nil
}

// middleware recovers panics, checks the protocol, authenticates, keeps a
// limited token to the routes that admit it, and logs.
func (dm *Daemon) middleware(next http.Handler) http.Handler {
	d := dm.Deps
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		defer func() {
			if p := recover(); p != nil {
				if p == http.ErrAbortHandler {
					panic(p)
				}
				d.Log.Error("panic", "path", r.URL.Path, "panic", fmt.Sprint(p), "stack", string(debug.Stack()))
				api.WriteError(w, api.Errorf(api.CodeInternal, "internal error"))
			}
		}()
		api.SetHeader(w.Header(), api.HeaderProtocol, api.WireProtocol)
		if p := api.GetHeader(r.Header, api.HeaderProtocol); p != "" && !api.SameProtocol(p, api.WireProtocol) {
			api.WriteError(w, api.Errorf(api.CodeBadRequest, "this daemon speaks %s; the client speaks %s", api.WireProtocol, p).
				WithHint("use the same "+config.Program+" version for the daemon and the CLI"))
			return
		}
		r = r.WithContext(d.Authenticate(r))
		if l := core.SubjectOf(r.Context()).Limit(); l != "" {
			// The one choke point for a limited token: before the gates,
			// the team proxy and the router (limitGate).
			var ok bool
			if r, ok = dm.limitGate(w, r, l); !ok {
				return
			}
		}
		if ts := core.TeamSubjectOf(r.Context()); ts != nil {
			// A team replica's call (SPEC 17.3): every answer carries the
			// project header it caches and its identity's scope_version.
			if h, ok := d.Projects.TeamHeader(ts); ok {
				api.SetHeader(w.Header(), api.HeaderTeam, api.EncodeTeamHeader(h))
				api.SetHeader(w.Header(), api.HeaderScopeVersion, strconv.FormatInt(h.ScopeVersion, 10))
			}
		}
		proxied := false
		serve := func(w http.ResponseWriter, r *http.Request) {
			proxied = d.Team != nil && d.Team.Proxy(w, r, next)
			if !proxied {
				next.ServeHTTP(w, r)
			} else if dm.Router != nil {
				dm.Router.MarkUsed(r)
			}
		}
		d.Gate(w, r, serve)
		as := ""
		if sub := core.SubjectOf(r.Context()); sub != nil {
			as = sub.Address
		}
		d.Log.Debug("request", "method", r.Method, "path", r.URL.Path, "as", as, "proxied", proxied, "dur", time.Since(start))
	})
}

// Serve listens on the Unix socket and serves until ctx ends or a stop
// request arrives, then shuts down.
func (dm *Daemon) Serve(ctx context.Context) error {
	d := dm.Deps
	started := make(chan struct{})
	dm.started.Store(&started)
	defer func() {
		select {
		case <-started:
		default:
			close(started) // a failed start: nothing waits on it
		}
	}()
	sock := d.Config.ListenSocketPath()
	ln, err := ListenUnix(sock)
	if err != nil {
		dm.Close()
		return err
	}
	ours, _ := os.Stat(sock)
	// The socket goes on every way out, a panic too. Our listener closes
	// first: still open, it would answer removeIfOurs's dial and stay.
	defer func() {
		_ = ln.Close()
		removeIfOurs(sock, ours)
	}()
	srv := &http.Server{Handler: dm.gate(dm.Handler), ReadHeaderTimeout: 10 * time.Second, ConnContext: markUnix}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	// The machine record as soon as the socket answers (calls wait for the
	// modules, gate): a client looking for the machine's daemon finds it
	// rather than starting another.
	if rec, ok := dm.writeRecord(ctx, sock); ok {
		defer dm.removeRecord(rec)
	}

	mctx, cancel := context.WithCancel(ctx)
	defer cancel()
	for _, mod := range dm.modules {
		if st, ok := mod.(core.Starter); ok {
			if err := st.Start(mctx); err != nil {
				d.Log.Error("module start", "module", fmt.Sprintf("%T", mod), "err", err)
			}
		}
	}
	close(started)
	go func() { // planner statistics stay fresh on a daemon that runs for weeks
		t := time.NewTicker(time.Hour)
		defer t.Stop()
		for {
			select {
			case <-mctx.Done():
				return
			case <-t.C:
				d.Store.Optimize(mctx)
			}
		}
	}()
	_, _ = d.Events.Publish(ctx, events.Input{Type: api.EventDaemonStarted, Subject: d.Config.Machine, Data: dm.healthBody(ctx, true)})
	d.Log.Info(config.Program+" daemon listening", "socket", sock, "machine", d.Config.Machine)
	go dm.selfCheckLoop(mctx)

	select {
	case <-ctx.Done():
	case <-dm.stop:
	case err = <-errc:
	}
	cancel()
	_, _ = d.Events.Publish(context.Background(), events.Input{Type: api.EventDaemonStopping, Subject: d.Config.Machine, Data: dm.stopBy})
	sctx, scancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer scancel()
	close(dm.closing) // live SSE streams end now instead of holding Shutdown for 5 s
	_ = srv.Shutdown(sctx)
	dm.Close()
	if errors.Is(err, http.ErrServerClosed) {
		err = nil
	}
	return err
}

// removeIfOurs removes the socket at path only if it is still the one this
// daemon created. The real guard is the home lock (LockHome): zm daemon
// holds it until Serve has returned, so no next daemon for this home can
// listen here before this runs. These checks are the second line, for a
// daemon run without the lock (tests, embedders): removing a live daemon's
// socket leaves it alive but unreachable (dogfood).
//
// The inode alone does not say so on Linux, which hands the next socket the
// inode the last one freed, and neither does the modification time on a VM,
// whose coarse clock tick can give both sockets the same one. Our listener
// is closed by now, so a socket that answers is the next daemon's: it stays
// whatever the stat says.
func removeIfOurs(path string, ours os.FileInfo) {
	fi, err := os.Stat(path)
	if err != nil || ours == nil || !os.SameFile(fi, ours) || !fi.ModTime().Equal(ours.ModTime()) {
		return
	}
	if c, err := net.DialTimeout("unix", path, time.Second); err == nil {
		c.Close()
		return
	}
	_ = os.Remove(path)
}

// markUnix tags connections accepted on a Unix socket as local. It is the
// only production caller of core.WithLocal; any other listener (tailnet,
// a future TCP door) is remote by default.
func markUnix(ctx context.Context, c net.Conn) context.Context {
	if uc, ok := c.(*net.UnixConn); ok {
		ctx = core.WithLocal(ctx)
		_, pid, err := peerCred(uc)
		if err != nil || pid <= 0 {
			pid = -1
		}
		ctx = core.WithPeerPID(ctx, pid)
		return ctx
	}
	return ctx
}

// SocketHandler is Handler as the Unix socket serves it (every request is
// local). For in-process clients in tests; never hand it to a listener.
func (dm *Daemon) SocketHandler() http.Handler {
	return dm.gate(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		dm.Handler.ServeHTTP(w, r.WithContext(core.WithLocal(r.Context())))
	}))
}

// Modules lists the wired modules (tests reach a module's test hooks, e.g.
// a fake GitHub publisher, by type assertion).
func (dm *Daemon) Modules() []core.Module { return append([]core.Module(nil), dm.modules...) }

// Close closes modules (reverse order) and the store.
func (dm *Daemon) Close() {
	for i := len(dm.modules) - 1; i >= 0; i-- {
		if c, ok := dm.modules[i].(core.Closer); ok {
			if err := c.Close(); err != nil {
				dm.Deps.Log.Warn("module close", "err", err)
			}
		}
	}
	_ = dm.Deps.Store.Close()
}

// ListenUnix listens on path with mode 0600. A stale socket (nobody
// answering) is removed; a live one means another daemon runs: error.
// Closing the listener leaves the file; Serve removes it (removeIfOurs).
func ListenUnix(path string) (net.Listener, error) {
	if len(path) > 103 { // sun_path is 104 bytes on macOS, 108 on Linux
		return nil, fmt.Errorf("socket path %s is too long (%d > 103 bytes); use a shorter home (%s)", path, len(path), config.HomeEnv())
	}
	if _, err := os.Stat(path); err == nil {
		if c, err := net.DialTimeout("unix", path, time.Second); err == nil {
			c.Close()
			return nil, fmt.Errorf("a %s daemon is already running on %s", config.Program, path)
		}
		_ = os.Remove(path)
	}
	old := umask(0o177)
	ln, err := net.Listen("unix", path)
	umask(old)
	if err != nil {
		return nil, err
	}
	_ = os.Chmod(path, 0o600)
	// Close must not unlink the path: by then it may be the next daemon's
	// socket (Go unlinks whatever is there). removeIfOurs, which checks, does.
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	return ln, nil
}

func (dm *Daemon) healthBody(ctx context.Context, full bool) api.Health {
	// The machine name is public: the tailnet hostname (zm-<machine>)
	// already says it, and peer discovery reads it from here. Process
	// details are for the socket's owner only.
	h := api.Health{OK: true, Protocol: api.WireProtocol, Version: api.Version, Commit: api.Commit, Machine: dm.Deps.Config.Machine}
	if full {
		h.PID, h.Started = os.Getpid(), dm.Deps.Started
		if hr, ok := dm.Deps.Team.(core.HedgeReporter); ok {
			if st := hr.HedgeStats(); st.Reads > 0 {
				h.Hedges = &st
			}
		}
		dm.swap.mu.Lock()
		h.SelfCheck, h.LastSwap = dm.swap.self, dm.swap.last
		dm.swap.mu.Unlock()
		h.Parts = dm.Deps.HealthParts(ctx)
	}
	return h
}

// stableMachine keeps the machine name stable (SPEC 2): sessions are named
// <machine>/<session8>, so a host rename (a network's DHCP) must not rename
// them. With no name persisted yet, the name this home's daemon last ran as
// is kept (an upgraded install), then the name is written to config.json.
func stableMachine(ctx context.Context, cfg *config.Config, st *store.Store, log *slog.Logger) {
	if config.Sanitize(cfg.File.Machine) == "" {
		var prev string
		_ = st.Registry().QueryRowContext(ctx, `SELECT subject FROM events WHERE type = ? ORDER BY id DESC LIMIT 1`,
			api.EventDaemonStarted).Scan(&prev)
		was := cfg.Machine
		if cfg.AdoptMachine(prev) {
			log.Info("keeping the machine name this home's daemon ran as", "machine", cfg.Machine, "host", was)
		}
	}
	note, warn, err := cfg.PersistMachine()
	switch {
	case err != nil:
		log.Warn("could not persist the machine name", "machine", cfg.Machine, "err", err)
	case warn:
		log.Warn(note)
	case note != "":
		log.Info(note)
	}
}
