package server

// A home's term (kit/hometerm) at its daemon: the term route every daemon
// answers, the parked daemon of a home that does not serve, the owner's
// freeze and unfreeze, and the fence a serving home puts on itself when the
// successor it named serves its data at a higher epoch.
//
// Parking is decided at daemon start, the one producer: New gives a home
// whose term does not serve a parked daemon (newParked), which claims
// nothing, opens no store and wires no module, so no route, loop or module
// of it can write to the home. A serving daemon that parks (freeze, fence)
// first answers every call but its health and term parked (parkGate), then
// changes its term and stops; the daemon started after it is parked.

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/hometerm"
)

// Routes of the term. A parked daemon answers termPath to anyone, and
// healthPath, stopPath and unfreezePath to this machine's owner on its
// socket; every other call gets api.CodeParked.
const (
	healthPath   = "/v0/health"
	termPath     = api.TermPath
	stopPath     = "/v0/daemon/stop"
	freezePath   = "/v0/authority/freeze"
	unfreezePath = "/v0/authority/unfreeze"
)

// fenceEvery is how often a serving home that named a successor asks it for
// its term, beginning at start: the heartbeat period (SPEC: 15 s). Atomic:
// tests shorten it while another test's daemon may still be stopping.
var fenceEvery = func() *atomic.Int64 {
	var d atomic.Int64
	d.Store(int64(15 * time.Second))
	return &d
}()

// askTimeout bounds one call to the named successor.
const askTimeout = 10 * time.Second

// termRoutes are the term's routes on a serving daemon.
func (dm *Daemon) termRoutes(r *core.Router) {
	r.HandleFunc("GET "+termPath, core.Public, "none: this home's state and epoch, the key the caller reached it by, the home it took over from, and the home it named to serve after it", dm.term)
	r.HandleFunc("POST "+freezePath, core.Local, "local; refused during another caller's swap and while managed sessions would end; the daemon stops after, and parks from its next start", dm.freeze)
}

// newParked is the daemon of a home whose term t does not serve (New), with
// the modules listeners returns over its Deps (ParkedListeners), if any.
func newParked(cfg *config.Config, log *slog.Logger, program string, t hometerm.Term, listeners Wiring) (*Daemon, error) {
	d := &core.Deps{Config: cfg, Log: log, Started: time.Now().UTC(), Parked: true}
	dm := &Daemon{Deps: d, stop: make(chan struct{}), closing: make(chan struct{}), program: program, parked: &t}
	d.Closing = dm.closing
	dm.Handler = dm.parkedHandler()
	d.Handler = dm.Handler
	if listeners != nil {
		mods, err := listeners(d)
		if err != nil {
			return nil, err
		}
		dm.modules = mods
	}
	return dm, nil
}

// parkedHandler is a parked daemon's whole API. Anyone may read its term;
// this machine's owner, on its socket, its health, and may stop it or
// unfreeze it, and call the routes its wiring provided
// (core.ParkedRoutesService: the HQ routes a parked machine answers).
// Everything else, the health off the socket included (a 2xx health naming
// no seal key would read as a home that cannot seal), and those routes off
// the socket, is answered parked.
func (dm *Daemon) parkedHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		api.SetHeader(w.Header(), api.HeaderProtocol, api.WireProtocol)
		local := core.IsLocal(r.Context())
		switch {
		case r.Method == http.MethodGet && r.URL.Path == termPath:
			dm.term(w, r)
		case local && r.Method == http.MethodGet && r.URL.Path == healthPath:
			api.WriteJSON(w, http.StatusOK, dm.healthBody(r.Context(), local))
		case local && r.Method == http.MethodPost && r.URL.Path == stopPath:
			dm.daemonStop(w, r)
		case local && r.Method == http.MethodPost && r.URL.Path == unfreezePath:
			dm.unfreeze(w, r)
		case local && dm.parkedRoutes(r) != nil:
			dm.parkedRoutes(r).ServeHTTP(w, r)
		default:
			api.WriteError(w, dm.parkedErr(local))
		}
	})
}

// parkedRoutes are the routes the parked daemon's wiring provided
// (core.ParkedRoutesService) when they serve r; nil otherwise.
func (dm *Daemon) parkedRoutes(r *http.Request) core.ParkedRoutes {
	p, ok := dm.Deps.Service(core.ParkedRoutesService).(core.ParkedRoutes)
	if !ok || !p.Serves(r) {
		return nil
	}
	return p
}

// parkGate answers every call but health and the term (all, when termToo
// is false: a preview host's calls are never the daemon's own) parked once a
// serving daemon has begun to park, before anything else of the call runs.
// Until the daemon's first ask of its named successor has an answer
// (firstFence), it holds those calls: a home that comes back while its
// successor serves takes no write first. In-process callers are held too:
// the ask runs after every module has started (the tailnet must be up), so
// a module's Start that waited on a call through d.Handler would never end.
func (dm *Daemon) parkGate(next http.Handler, termToo bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if termToo && r.Method == http.MethodGet && (r.URL.Path == healthPath || r.URL.Path == termPath) {
			next.ServeHTTP(w, r)
			return
		}
		if p := dm.firstFence.Load(); p != nil {
			select {
			case <-*p:
			case <-r.Context().Done():
				return
			}
		}
		if dm.parking.Load() {
			api.SetHeader(w.Header(), api.HeaderProtocol, api.WireProtocol)
			api.WriteError(w, dm.parkedErr(core.IsLocal(r.Context())))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// parkedErr is the one answer of a parked daemon to a call it does not
// serve. Only this machine's owner (local) is told where HQ is now; another
// caller is told the state alone.
func (dm *Daemon) parkedErr(local bool) *api.Error {
	sb := api.ServedBy{Machine: dm.Deps.Config.Machine}
	t, err := hometerm.Load(dm.Deps.Config.Home)
	switch {
	case err != nil:
		return api.Errorf(api.CodeParked, "this daemon does not serve its home: its term cannot be read").WithData(sb)
	case t.Serves(): // unfrozen, or a park that was refused: the daemon is stopping
		sb.State = string(t.State)
		return api.Errorf(api.CodeParked, "this daemon is stopping; its home serves from its next start").WithData(sb)
	}
	sb.State = string(t.State)
	if !local {
		return api.Errorf(api.CodeParked, "this home does not serve now: it is %s", t.State).WithData(sb)
	}
	if t.State == hometerm.Fenced {
		sb.SuccessorURL = t.Successor.URL
	}
	e := api.Errorf(api.CodeParked, "%s", t.Why()).WithData(sb)
	if t.State == hometerm.Frozen {
		e = e.WithHint(config.Command("daemon unfreeze") + " serves it again if the move is called off")
	}
	return e
}

// term is GET /v0/authority/term.
func (dm *Daemon) term(w http.ResponseWriter, r *http.Request) {
	t, err := hometerm.Load(dm.Deps.Config.Home)
	if err != nil {
		dm.Deps.Log.Warn("this home's term cannot be read", "err", err)
		api.WriteError(w, api.Errorf(api.CodeInternal, "this home's term cannot be read"))
		return
	}
	api.WriteJSON(w, http.StatusOK, dm.termAnswer(r.Context(), t))
}

// termAnswer is t as the term route answers it to the caller of ctx.
func (dm *Daemon) termAnswer(ctx context.Context, t hometerm.Term) api.AuthorityTerm {
	a := api.AuthorityTerm{State: string(t.State), Epoch: t.Epoch}
	if rm := core.RemoteOf(ctx); rm != nil { // a network caller; the socket carries no Remote
		if fp, err := dm.Deps.HomeFingerprint(ctx, cmp.Or(rm.Via, core.ViaTailnet)); err == nil {
			a.Fingerprint = fp
		}
	}
	if p := t.Predecessor; p != nil {
		a.Predecessor = &api.HomeKey{Via: p.Via, Fingerprint: p.Fingerprint}
	}
	if s := t.Successor; s != nil { // its key only: the address is for this machine's owner (parkedErr)
		a.Successor = &api.HomeKey{Via: s.Via, Fingerprint: s.Fingerprint}
	}
	return a
}

// termErr is a refused term change as an API error.
func termErr(err error) error {
	if errors.Is(err, hometerm.ErrTransition) {
		return api.Errorf(api.CodeConflict, "%v", err)
	}
	return err
}

// caller is the address of r's caller, for the log and the stopping event.
func caller(r *http.Request) string {
	if sub := core.SubjectOf(r.Context()); sub != nil {
		return sub.Address
	}
	return "local" // a parked daemon's socket: this machine's owner
}

// freeze is POST /v0/authority/freeze: this serving home stops for a move
// at its epoch (hometerm Freeze). The daemon stops after its answer.
func (dm *Daemon) freeze(w http.ResponseWriter, r *http.Request) {
	if err := dm.stopGuard(r.Context(), api.DaemonStopRequest{}); err != nil {
		api.WriteError(w, err)
		return
	}
	t, err := dm.park(hometerm.Term.Freeze)
	if err != nil {
		api.WriteError(w, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, dm.termAnswer(r.Context(), t))
	by := caller(r)
	dm.requestStop(stopNote{By: by, What: "freeze"}, "by", by, "what", "freeze", "epoch", t.Epoch)
}

// unfreeze is POST /v0/authority/unfreeze, on a parked daemon: a frozen
// home serves again at the same epoch (hometerm Unfreeze), from the daemon
// started after this one, which stops after its answer.
func (dm *Daemon) unfreeze(w http.ResponseWriter, r *http.Request) {
	// A parked daemon holds no writer lock: it takes it for this change, and
	// is refused while another process changes the home.
	unlock, err := config.LockWriter(dm.Deps.Config.Home)
	if err != nil {
		api.WriteError(w, api.Errorf(api.CodeConflict, "%v", err))
		return
	}
	t, err := dm.Deps.ApplyTerm(hometerm.Term.Unfreeze)
	unlock()
	if err != nil {
		api.WriteError(w, termErr(err))
		return
	}
	api.WriteJSON(w, http.StatusOK, dm.termAnswer(r.Context(), t))
	by := caller(r)
	dm.requestStop(stopNote{By: by, What: "unfreeze"}, "by", by, "what", "unfreeze", "epoch", t.Epoch)
}

// park changes this serving daemon's term through move to one that does
// not serve. From just before the change every call but health and the
// term is answered parked (parkGate); a refused change serves on. The
// caller stops the daemon after.
func (dm *Daemon) park(move func(hometerm.Term) (hometerm.Term, error)) (hometerm.Term, error) {
	if dm.parked != nil || !dm.parking.CompareAndSwap(false, true) {
		return hometerm.Term{}, api.Errorf(api.CodeConflict, "this daemon is parked or parking already")
	}
	t, err := dm.Deps.ApplyTerm(func(cur hometerm.Term) (hometerm.Term, error) {
		next, err := move(cur)
		if err == nil && next.Serves() {
			err = fmt.Errorf("%w: parking left the home serving", hometerm.ErrTransition)
		}
		return next, err
	})
	if err != nil {
		dm.parking.Store(false)
		return hometerm.Term{}, termErr(err)
	}
	return t, nil
}

// fenceLoop asks the successor this serving home named for its term at
// start and every fenceEvery, until the home is fenced (checkFence) or ctx
// ends. Calls wait at parkGate until the first ask has its answer
// (asked); it is bounded by twice askTimeout (this home's key, then the
// successor's term). Serve waits for it to end before it returns.
func (dm *Daemon) fenceLoop(ctx context.Context, asked func()) {
	var said string
	fenced := dm.checkFence(ctx, &said)
	asked()
	if fenced {
		return
	}
	t := time.NewTicker(time.Duration(fenceEvery.Load()))
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if dm.checkFence(ctx, &said) {
			return
		}
	}
}

// checkFence asks the successor this home's term names, if any, for its
// term, over a client pinned to the successor's key. When that answer
// fences this home (fences), the term is fenced at the successor's epoch and
// the daemon stops; true then. A successor that does not answer, or whose
// answer does not fence this home, changes nothing; why is logged once per
// change of the reason (said).
func (dm *Daemon) checkFence(ctx context.Context, said *string) bool {
	d := dm.Deps
	t, err := hometerm.Load(d.Config.Home)
	if err != nil {
		dm.sayFence(said, slog.LevelWarn, "this home's term cannot be read; its successor is not asked", "", err)
		return false
	}
	if !t.Serves() || t.Successor == nil {
		*said = ""
		return false
	}
	succ := *t.Successor
	at, err := dm.askTerm(ctx, succ)
	if err != nil {
		if ctx.Err() == nil {
			dm.sayFence(said, slog.LevelWarn, "this home cannot ask the successor it named for its term; it serves on", succ.URL, err)
		}
		return false
	}
	own := func(via string) (string, error) {
		cctx, cancel := context.WithTimeout(ctx, askTimeout)
		defer cancel()
		return d.HomeFingerprint(cctx, via)
	}
	if err := fences(t, at, own); err != nil {
		dm.sayFence(said, slog.LevelInfo, "the successor this home named does not fence it; it serves on", succ.URL, err)
		return false
	}
	_, err = dm.park(func(cur hometerm.Term) (hometerm.Term, error) {
		if !cur.Serves() || cur.Epoch != t.Epoch || cur.Successor == nil || *cur.Successor != succ {
			return hometerm.Term{}, fmt.Errorf("%w: the term changed while its successor was asked", hometerm.ErrTransition)
		}
		return cur.Fence(succ, at.Epoch)
	})
	if err != nil {
		d.Log.Warn("the named successor serves at a higher epoch, but this home's term was not fenced", "url", succ.URL, "err", err)
		return false
	}
	d.Log.Warn("this home is fenced: the successor it named serves its data at a higher epoch; the daemon stops and parks from its next start",
		"url", succ.URL, "fingerprint", succ.Fingerprint, "epoch", at.Epoch, "was", t.Epoch)
	dm.requestStop(stopNote{By: d.System().Address, What: "fenced"}, "what", "fenced", "epoch", at.Epoch)
	return true
}

// sayFence logs why a check of the named successor fenced nothing, unless
// the last check said the same (said holds what it said).
func (dm *Daemon) sayFence(said *string, level slog.Level, msg, url string, err error) {
	why := msg + ": " + err.Error()
	if why == *said {
		return
	}
	*said = why
	dm.Deps.Log.Log(context.Background(), level, msg, "url", url, "err", err)
}

// fences is nil when at, the term that the successor named by a serving
// home's term t answered over a client pinned to its key, fences that
// home: the successor serves, t may be fenced at its epoch (hometerm's
// rules: above t's, and in range), and it names as its predecessor this
// home's own key on that transport (own). A successor that serves someone
// else's data fences nothing.
func fences(t hometerm.Term, at api.AuthorityTerm, own func(via string) (string, error)) error {
	if at.State != api.HQServing {
		return fmt.Errorf("the successor is %s, not serving", at.State)
	}
	if _, err := t.Fence(*t.Successor, at.Epoch); err != nil {
		return fmt.Errorf("the successor serves at epoch %d: %v", at.Epoch, err)
	}
	if at.Predecessor == nil {
		return errors.New("the successor names no home it took over from")
	}
	fp, err := own(at.Predecessor.Via)
	if err != nil {
		return fmt.Errorf("this home's key on %s: %v", at.Predecessor.Via, err)
	}
	if fp != at.Predecessor.Fingerprint {
		return fmt.Errorf("the successor took over from key %s on %s, not this home's %s", at.Predecessor.Fingerprint, at.Predecessor.Via, fp)
	}
	return nil
}

// askTerm is succ's answer to GET /v0/authority/term over a client pinned to
// its key: an error when it cannot be reached, is not the pinned key, or
// answers anything but its term.
func (dm *Daemon) askTerm(ctx context.Context, succ hometerm.Peer) (api.AuthorityTerm, error) {
	tr := dm.Deps.Transport(succ.Via)
	if tr == nil {
		return api.AuthorityTerm{}, fmt.Errorf("this daemon has no %q transport", succ.Via)
	}
	hc := tr.Client(core.JoinTarget{Via: succ.Via, URL: succ.URL, Fingerprint: succ.Fingerprint}, core.ClientOptions{DialTimeout: askTimeout})
	defer hc.CloseIdleConnections()
	cctx, cancel := context.WithTimeout(ctx, askTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, strings.TrimRight(succ.URL, "/")+termPath, nil)
	if err != nil {
		return api.AuthorityTerm{}, err
	}
	api.SetHeader(req.Header, api.HeaderProtocol, api.WireProtocol)
	resp, err := hc.Do(req)
	if err != nil {
		return api.AuthorityTerm{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return api.AuthorityTerm{}, fmt.Errorf("the successor answered %s", resp.Status)
	}
	var at api.AuthorityTerm
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&at); err != nil {
		return api.AuthorityTerm{}, fmt.Errorf("the successor's term does not parse: %v", err)
	}
	return at, nil
}
