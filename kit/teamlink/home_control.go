package teamlink

// The HQ routes (kit/api/authority.go): an owner reads where this home's
// team projects are served from, names a standby, moves HQ, calls a move
// off or resumes it, takes over on the standby and retires a fenced old HQ.
//
// Potluck holds no move of its own. The program above registers a
// HomeEngine (Hooks.Home on a serving daemon, ProvideParkedRoutes on a
// parked one; zm's internal/homes). Every press runs in this order: who
// pressed (pressedBy, the one permission check of every press), that an
// engine is here, the body decoded (one JSON value) and Checked, the checks
// this daemon can make of what the screen showed (HQ and epoch, this
// machine and its state), then one engine method, whose answer is cleaned
// before it leaves. Nothing acts on a request before its Check. With no
// engine every press answers api.ReasonNoEngine. The engine starts a
// detached command and returns once its first log line is written;
// progress is read back through Status. Nothing here writes a term: the
// engine's command does, through the daemon's one door.
//
// Where each is answered:
//
//   - A serving daemon answers every route on its router. On a member's
//     socket a call goes to the home of its joined team projects (homeFor):
//     the one home they share, or the home of the project the call names.
//     When that home does not answer, GET /v0/authority is filled from what
//     this machine recorded of it (silentStatus). Promote and retire are
//     refused on a serving daemon: they belong to a parked one.
//   - A parked daemon answers the status, abort, resume, promote and retire
//     (ProvideParkedRoutes) on its socket only, to this machine's owner as
//     this home's registry.db names them (parkedOwner): it checks no token,
//     so it can judge no network caller. A press on a parked machine from
//     another machine is answered parked; the owner runs the command on
//     that machine.
//
// What leaves is checked, not trusted: an engine's error is answered with
// fixed words chosen by its code and reason (engineErr), never its own
// message, hint or data; its free text (Reason.Text, PreflightCheck.Text
// and Fix, MoveStep.Detail, HomeMove.Failure) only when fixedPhrase says
// so; and its codes, ids, names and the public address only when they are
// what their field holds (cleaner). Anything else is replaced, and the
// field's name and length logged, never its text.

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strings"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/hometerm"
)

// HomeEngine is the move engine behind the HQ routes. The routes have
// checked the caller (by is who pressed: identity, machine, time) and the
// request (its Check) before a method runs; on a serving daemon also a
// press's Shown against this home's term, and on any daemon a takeover's
// or retire's Machine against this machine and its state. The engine
// serializes all presses (one at a time, whichever route or daemon they
// came through) and is idempotent per move id: a press repeated for a move
// that already took it answers that move as it is, and starts nothing
// twice. It records by in its intent log and, once HQ serves, as one event
// on each project, and judges everything else: the state of the home and
// of any move, a move id or plan id, Shown on a parked machine. A refusal
// is an *api.Error (api.RefuseHQ for a reason the screen switches on); the
// routes answer it by its code and reason alone (engineErr). A Start*
// returns once its detached command has written its first log line.
type HomeEngine interface {
	// Status is this home's HQ, standby and any move as the engine knows
	// them. The routes set ServedBy and CanManage, and on a serving daemon
	// Machine, State and Epoch, from this daemon's own term.
	Status(ctx context.Context) (api.AuthorityStatus, error)
	Candidates(ctx context.Context) (api.CandidateList, error)
	// Preflight answers a move's checks and numbers and starts nothing.
	Preflight(ctx context.Context, req api.MoveRequest) (api.HomePreflight, error)
	// SetStandby names req.Machine HQ's standby. The standby home it sets
	// up there holds, in its remotes directory (DirRemotes, <name>.token
	// files), the node-bound tokens that machine's own home joined with: on
	// that parked standby they alone say who its owner is (parkedOwner).
	SetStandby(ctx context.Context, by api.MoveActor, req api.StandbyRequest) (api.AuthorityStatus, error)
	ReleaseStandby(ctx context.Context, by api.MoveActor, req api.StandbyRequest) (api.AuthorityStatus, error)
	// StartMove runs the preflight of req.PlanID again and starts the move
	// only when every check passes (MoveAnswer.Move nil otherwise).
	StartMove(ctx context.Context, by api.MoveActor, req api.MoveRequest) (api.MoveAnswer, error)
	AbortMove(ctx context.Context, by api.MoveActor, req api.MoveControl) (api.HomeMove, error)
	ResumeMove(ctx context.Context, by api.MoveActor, req api.MoveControl) (api.HomeMove, error)
	// StartPromote is a takeover on this standby (or on a fenced home
	// taking its data back); by is a person's own press, never a session's.
	StartPromote(ctx context.Context, by api.MoveActor, req api.PromoteRequest) (api.HomeMove, error)
	// Retire drops a fenced old HQ's copy; by is a person's own press.
	Retire(ctx context.Context, by api.MoveActor, req api.RetireRequest) (api.HomeMove, error)
}

// hq answers the HQ routes over one daemon's deps and engine: a serving
// daemon's (Service.hq) or a parked one's (ProvideParkedRoutes).
type hq struct {
	d      *core.Deps
	engine func() HomeEngine // nil result: no engine
}

func (s *Service) hq() hq { return hq{d: s.d, engine: func() HomeEngine { return s.Hooks.Home }} }

// noEngine is every press's answer on a daemon with no HomeEngine.
func noEngine() *api.Error { return api.ErrNoEngine("a program with a move engine") }

// pressGuard is the router's words for every press.
const pressGuard = "an owner's press: this machine's owner on its socket, or a member machine's node-bound token whose identity owns every team project here by its own grants; never a minted token, a box's token or a tailnet login"

// personGuard is added for a press that changes where the home lives or
// who may take it over: a person's own, never a session's.
const personGuard = ", and a person's own (no session)"

// homeRoutes are the HQ routes on a serving daemon.
func (s *Service) homeRoutes(r *core.Router) {
	h := s.hq()
	r.HandleFunc("GET "+api.AuthorityPath, core.Authed, "this machine's owner on its socket, or a member machine's node-bound token", jsonH(func(r *http.Request) (any, error) {
		if !memberCaller(r) {
			return nil, api.Errorf(api.CodeForbidden, "HQ's status is for this machine's owner and the machines of its team projects")
		}
		return h.status(r)
	}))
	r.HandleFunc("GET "+api.CandidatesPath, core.Authed, pressGuard, jsonH(h.candidates))
	r.HandleFunc("POST "+api.StandbyPath, core.Authed, pressGuard+personGuard+"; Shown must be this HQ at its epoch", jsonH(func(r *http.Request) (any, error) {
		return press(h, r, true, func(q api.StandbyRequest) error { return h.servingShown(q.Shown) }, HomeEngine.SetStandby, cleanStatus)
	}))
	r.HandleFunc("POST "+api.StandbyReleasePath, core.Authed, pressGuard+personGuard+"; Shown must be this HQ at its epoch", jsonH(func(r *http.Request) (any, error) {
		return press(h, r, true, func(q api.StandbyRequest) error { return h.servingShown(q.Shown) }, HomeEngine.ReleaseStandby, cleanStatus)
	}))
	r.HandleFunc("POST "+api.MovePath, core.Authed, pressGuard+personGuard+"; check_only answers the preflight and starts nothing; otherwise Shown must be this HQ at its epoch", jsonH(h.move))
	h.parkedPresses(func(pattern, guard string, fn func(*http.Request) (any, error)) {
		r.HandleFunc(pattern, core.Authed, guard, jsonH(fn))
	})
}

// parkedPresses are the presses a parked machine answers too (abort and
// resume on the old HQ, promote on the standby, retire on the fenced old
// HQ), each given to add with its pattern and guard words. Abort and
// resume back out of a stopped move, so a session may press them; promote
// and retire are a person's.
func (h hq) parkedPresses(add func(pattern, guard string, fn func(*http.Request) (any, error))) {
	add("POST "+api.MoveAbortPath, pressGuard, func(r *http.Request) (any, error) {
		return press(h, r, false, nil, HomeEngine.AbortMove, cleanMove)
	})
	add("POST "+api.MoveResumePath, pressGuard, func(r *http.Request) (any, error) {
		return press(h, r, false, nil, HomeEngine.ResumeMove, cleanMove)
	})
	add("POST "+api.PromotePath, pressGuard+personGuard+"; machine must be this one, a standby or a fenced home", func(r *http.Request) (any, error) {
		return press(h, r, true, func(q api.PromoteRequest) error {
			return h.parkedAs(q.Machine, "only a standby, or a replaced old HQ taking its data back, takes over", hometerm.Standby, hometerm.Fenced)
		}, HomeEngine.StartPromote, cleanMove)
	})
	add("POST "+api.RetirePath, pressGuard+personGuard+"; machine must be this one, a fenced home", func(r *http.Request) (any, error) {
		return press(h, r, true, func(q api.RetireRequest) error {
			return h.parkedAs(q.Machine, "only a replaced (fenced) old HQ is retired", hometerm.Fenced)
		}, HomeEngine.Retire, cleanMove)
	})
}

// homeRows are the HQ routes a member's proxy sends to the home of its
// joined team projects (toHome).
func (s *Service) homeRows() []RouteRule {
	own := &RouteHook{Own: s.toHome}
	const once = "an owner's press: a lost answer is read back from GET /v0/authority, never pressed twice"
	rows := []RouteRule{
		{Pattern: "GET " + api.AuthorityPath, Kind: KindOwn, Hedge: Hedged, Hook: own},
		{Pattern: "GET " + api.CandidatesPath, Kind: KindOwn, Hedge: Hedged, Hook: own},
	}
	for _, p := range []string{api.StandbyPath, api.StandbyReleasePath, api.MovePath, api.MoveAbortPath, api.MoveResumePath, api.PromotePath, api.RetirePath} {
		rows = append(rows, RouteRule{Pattern: "POST " + p, Kind: KindOwn, Hedge: Once, Why: once, Hook: own})
	}
	return rows
}

// ProvideParkedRoutes gives a parked daemon (its wiring, server.ParkedListeners)
// the HQ routes it answers, over e: the status, abort, resume, promote and
// retire, on this machine's socket only (kit/server answers nothing else of
// them), to this machine's owner as this home's registry.db names them.
func ProvideParkedRoutes(d *core.Deps, e HomeEngine) {
	if e == nil {
		panic("teamlink: ProvideParkedRoutes needs an engine")
	}
	h := hq{d: d, engine: func() HomeEngine { return e }}
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+api.AuthorityPath, jsonH(h.status))
	h.parkedPresses(func(pattern, _ string, fn func(*http.Request) (any, error)) { mux.HandleFunc(pattern, jsonH(fn)) })
	d.Provide(core.ParkedRoutesService, parkedRoutes{mux: mux})
}

// parkedRoutes is a parked daemon's HQ routes (core.ParkedRoutes).
type parkedRoutes struct{ mux *http.ServeMux }

func (p parkedRoutes) Serves(r *http.Request) bool {
	_, pat := p.mux.Handler(r)
	return pat != ""
}

func (p parkedRoutes) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !core.IsLocal(r.Context()) {
		// kit/server never hands it one; refused all the same.
		api.WriteError(w, api.Errorf(api.CodeParked, "this home does not serve now"))
		return
	}
	p.mux.ServeHTTP(w, r)
}

// ---- presses ----

// checker is a request whose Check names the first missing field.
type checker interface{ Check() error }

// press is every POST but the move: who pressed (pressedBy) and that an
// engine is here, then the body decoded (one JSON value) and Checked before
// anything acts on it (an empty body decodes to the zero request, which
// Check refuses), the checks of what the screen showed that this daemon
// can make (here; nil: none), the engine's one method, and its answer
// cleaned.
func press[Q checker, A any](h hq, r *http.Request, person bool, here func(Q) error,
	call func(HomeEngine, context.Context, api.MoveActor, Q) (A, error), clean func(*A, *slog.Logger)) (any, error) {
	by, err := pressedBy(r, h.d, person)
	if err != nil {
		return nil, err
	}
	e := h.engine()
	if e == nil {
		return nil, noEngine()
	}
	var req Q
	if err := api.DecodeOneJSON(r, &req); err != nil {
		return nil, err
	}
	if err := req.Check(); err != nil {
		return nil, err
	}
	if here != nil {
		if err := here(req); err != nil {
			return nil, err
		}
	}
	v, err := call(e, r.Context(), by, req)
	if err != nil {
		return nil, h.engineErr(err)
	}
	clean(&v, h.d.Log)
	return v, nil
}

// move is POST /v0/authority/move: the preflight alone with CheckOnly,
// else the move. The preflight's numbers are the owner's to read, so even
// a check-only call is a person's press.
func (h hq) move(r *http.Request) (any, error) {
	return press(h, r, true, func(q api.MoveRequest) error {
		if q.CheckOnly {
			return nil
		}
		return h.servingShown(q.Shown)
	}, func(e HomeEngine, ctx context.Context, by api.MoveActor, q api.MoveRequest) (api.MoveAnswer, error) {
		if q.CheckOnly {
			p, err := e.Preflight(ctx, q)
			return api.MoveAnswer{Preflight: p}, err
		}
		return e.StartMove(ctx, by, q)
	}, cleanMoveAnswer)
}

// candidates is GET /v0/authority/candidates (an owner's).
func (h hq) candidates(r *http.Request) (any, error) {
	if _, err := pressedBy(r, h.d, false); err != nil {
		return nil, err
	}
	e := h.engine()
	if e == nil {
		return nil, noEngine()
	}
	v, err := e.Candidates(r.Context())
	if err != nil {
		return nil, h.engineErr(err)
	}
	cleanCandidates(&v, h.d.Log)
	return v, nil
}

// servingShown refuses a press on HQ whose screen showed another HQ or
// epoch than this serving home's own (api.ReasonStale).
func (h hq) servingShown(sh api.Shown) error {
	t, err := h.term()
	if err != nil {
		return err
	}
	if !t.Serves() || sh.From != h.d.Config.Machine || sh.Epoch == nil || *sh.Epoch != t.Epoch {
		return api.RefuseHQ(api.CodeConflict, api.ReasonStale, "the screen is out of date: HQ is %s at epoch %d", h.d.Config.Machine, t.Epoch).
			WithHint("read HQ's status again, then press again")
	}
	return nil
}

// parkedAs refuses a takeover or a retire that names a machine other than
// this one, or reaches it while its home is in none of states (what says
// which may), so neither ever reaches the engine of a serving HQ
// (api.ReasonStale).
func (h hq) parkedAs(machine, what string, states ...hometerm.State) error {
	if machine != h.d.Config.Machine {
		return api.RefuseHQ(api.CodeConflict, api.ReasonStale, "the screen is out of date: this is %s, not %s", h.d.Config.Machine, machine).
			WithHint("press it on the machine it names")
	}
	t, err := h.term()
	if err != nil {
		return err
	}
	if !slices.Contains(states, t.State) {
		return api.RefuseHQ(api.CodeConflict, api.ReasonStale, "the screen is out of date: %s is %s now; %s", h.d.Config.Machine, t.State, what).
			WithHint("read HQ's status again")
	}
	return nil
}

// term is this daemon's term, or a fixed refusal when it cannot be read.
func (h hq) term() (hometerm.Term, error) {
	t, err := hometerm.Load(h.d.Config.Home)
	if err != nil {
		h.d.Log.Warn("HQ: this home's term cannot be read", "err", err)
		return t, api.Errorf(api.CodeInternal, "this home's term cannot be read")
	}
	return t, nil
}

// refusal is the fixed words of an engine's refusal with a reason.
type refusal struct{ message, hint string }

// refusals are what an engine's refusal is answered with, by its reason:
// one for every api.Reasons code.
var refusals = map[string]refusal{
	api.ReasonNotTrusted:    {"that machine is not trusted with this home's secrets", "an owner trusts its machine class first"},
	api.ReasonNeedsUpdate:   {"that machine's build cannot follow HQ", "update it, then press again"},
	api.ReasonOffline:       {"that machine is offline", "start its daemon, then press again"},
	api.ReasonNoDisk:        {"that machine has no room for a copy of this home", "free room on it, then press again"},
	api.ReasonPublicAddress: {"that machine connects through the public address, so HQ cannot reach it", ""},
	api.ReasonIsHQ:          {"that machine is HQ already", ""},
	api.ReasonIsStandby:     {"that machine is the standby already", ""},
	api.ReasonBehind:        {"the standby is more than two minutes behind HQ", "wait until it catches up, then press again"},
	api.ReasonNoEngine:      {"moving HQ needs a program with a move engine on this machine", ""},
	api.ReasonStale:         {"the screen is out of date", "read HQ's status again, then press again"},
	api.ReasonNotOwner:      {"only an owner of every team project this home holds moves HQ", ""},
	api.ReasonHQAlive:       {"HQ still answers the standby, so it is not down", "move it with a planned move instead"},
}

// engineCodes are the error codes an engine's refusal keeps; any other is
// answered internal.
var engineCodes = []string{api.CodeBadRequest, api.CodeForbidden, api.CodeNotFound, api.CodeConflict, api.CodeUnavailable, api.CodeInternal}

// engineErr is an engine's error as the routes answer it, rebuilt from
// fixed words: its code when it is one of engineCodes (else internal), its
// reason when it is one of api.Reasons, with that reason's message and hint
// (refusals), or a general sentence. Nothing else of it leaves: not its
// message, hint or data. Its code, reason and message length are logged.
func (h hq) engineErr(err error) error {
	code, reason, size := api.CodeInternal, "", len(err.Error())
	var ae *api.Error
	if errors.As(err, &ae) {
		if slices.Contains(engineCodes, ae.Code) {
			code = ae.Code
		}
		if r := api.HQRefusalReason(ae); slices.Contains(api.Reasons, r) {
			reason = r
		}
		size = len(ae.Message) + len(ae.Hint) + len(ae.Data)
	}
	h.d.Log.Warn("HQ: the move engine refused; answered with fixed words", "code", code, "reason", reason, "len", size)
	if f, ok := refusals[reason]; ok {
		out := api.RefuseHQ(code, reason, "%s", f.message)
		if f.hint != "" {
			out = out.WithHint(f.hint)
		}
		return out
	}
	if code == api.CodeInternal {
		return api.Errorf(code, "the move engine failed; its machine's log says why")
	}
	return api.Errorf(code, "the move engine refused; its machine's log says why")
}

// ---- the status ----

// status is GET /v0/authority as this daemon answers it: the engine's
// status, cleaned (without one, what this serving home's term says), with
// ServedBy, CanManage and, while this home serves, Machine, State and
// Epoch from this daemon's own term.
func (h hq) status(r *http.Request) (any, error) {
	t, err := h.term()
	if err != nil {
		return nil, err
	}
	var st api.AuthorityStatus
	switch e := h.engine(); {
	case e != nil:
		if st, err = e.Status(r.Context()); err != nil {
			return nil, h.engineErr(err)
		}
		cleanStatus(&st, h.d.Log)
	case h.d.Parked:
		return nil, noEngine()
	default:
		st = h.bareStatus(r.Context(), t)
	}
	st.ServedBy = h.servedBy(t, core.IsLocal(r.Context()))
	if t.Serves() {
		st.Machine, st.State, st.Epoch = h.d.Config.Machine, api.HQServing, t.Epoch
	}
	_, perr := pressedBy(r, h.d, false)
	st.CanManage = perr == nil
	return st, nil
}

// servedBy is this daemon as the one answering; the address of HQ now for
// the owner of a fenced home (on its socket), as a parked daemon's refusal
// says it.
func (h hq) servedBy(t hometerm.Term, local bool) api.ServedBy {
	sb := api.ServedBy{Machine: h.d.Config.Machine, State: string(t.State)}
	if local && t.State == hometerm.Fenced && t.Successor != nil {
		sb.SuccessorURL = t.Successor.URL
	}
	return sb
}

// bareStatus is a serving home's status with no engine: its standby is the
// successor its term names, if any, whose copy this daemon cannot know.
func (h hq) bareStatus(ctx context.Context, t hometerm.Term) api.AuthorityStatus {
	st := api.AuthorityStatus{Standby: api.StandbyStatus{State: api.StandbyNone}}
	if t.Successor != nil {
		st.Standby = api.StandbyStatus{Machine: h.designated(ctx, t.Successor.Fingerprint), State: api.StandbyUnknown}
	}
	return st
}

// designated is the machine name kept for the successor with fingerprint
// fp (Designate), "" when none was kept.
func (h hq) designated(ctx context.Context, fp string) string {
	var m string
	err := h.d.Store.Registry().QueryRowContext(ctx, `SELECT machine FROM team_designated WHERE fingerprint = ?`, fp).Scan(&m)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		h.d.Log.Warn("HQ: the successor's machine name cannot be read", "err", err)
	}
	return m
}

// ---- a member's call ----

// toHome sends an HQ call on a member's socket to the home of its joined
// team projects (homeFor); false leaves it to this machine's router. When
// that home does not answer at all, GET /v0/authority is answered from what
// this machine recorded of it (silentStatus).
func (s *Service) toHome(w http.ResponseWriter, r *http.Request, next http.Handler) bool {
	x, named, err := s.homeFor(r)
	if err != nil {
		api.WriteError(w, err)
		return true
	}
	if x == nil {
		return false
	}
	if r.Method != http.MethodGet || r.URL.Path != api.AuthorityPath {
		s.Forward(w, r, x, "", named)
		return true
	}
	resp, err := s.ForwardResp(r, x, "", named, nil)
	if err != nil {
		if awayErr(err) && !x.Home().Behind && Ended(x.Snapshot()) == nil {
			api.WriteJSON(w, http.StatusOK, s.silentStatus(x))
			return true
		}
		api.WriteError(w, err)
		return true
	}
	defer resp.Body.Close()
	s.relay(w, r, resp)
	return true
}

// homeFor is the joined team project whose home an HQ call on this
// machine's socket goes to (named: by ?project= or Zm-Project), or nil for
// this machine's own home: the one a project named here is in; else, when
// this machine holds no team project, the one home its joins share. Joins
// at more than one home must name one.
func (s *Service) homeFor(r *http.Request) (x *Remote, named bool, err error) {
	ref := r.URL.Query().Get("project")
	if ref == "" {
		ref = api.GetHeader(r.Header, api.HeaderProject)
	}
	if ref != "" {
		slug, _, _ := strings.Cut(ref, "/")
		if _, local := s.d.Projects.Lookup(slug); local {
			return nil, false, nil
		}
		if x := s.ByName(slug); x != nil {
			return x, true, nil
		}
		return nil, false, api.NotFound("project", slug)
	}
	for _, p := range s.d.Projects.List(authz.All()) {
		if s.d.Projects.IsTeam(p.ID) {
			return nil, false, nil // this machine is HQ of its own team projects
		}
	}
	homes := map[string][]string{} // home key: the slugs joined there
	first := map[string]*Remote{}
	for _, x := range s.List() {
		snap := x.Snapshot()
		if Ended(snap) != nil {
			continue
		}
		k := cmp.Or(snap.Via, api.ViaTailnet) + " " + snap.Fingerprint
		if _, ok := first[k]; !ok {
			first[k] = x
		}
		homes[k] = append(homes[k], snap.Project.Slug)
	}
	switch len(homes) {
	case 0:
		return nil, false, nil
	case 1:
		for _, x := range first {
			return x, false, nil
		}
	}
	var each []string
	for _, slugs := range homes {
		slices.Sort(slugs)
		each = append(each, strings.Join(slugs, ", "))
	}
	slices.Sort(each)
	return nil, false, api.Errorf(api.CodeBadRequest, "this machine joined team projects at %d homes (%s): name a project whose HQ you mean", len(homes), strings.Join(each, "; ")).
		WithHint("name one of those projects with the command's project option (--pool in potluck, --project in zm)")
}

// silentStatus is GET /v0/authority for x's home when it did not answer:
// HQ as this machine last recorded it (its machine, its epoch, the
// successor it named), said to be silent. Nobody here can press on it: a
// takeover is pressed on the standby's own socket.
func (s *Service) silentStatus(x *Remote) api.AuthorityStatus {
	h, snap := x.Home(), x.Snapshot()
	sb := api.ServedBy{Machine: s.d.Config.Machine, State: api.HQServing}
	if t, err := hometerm.Load(s.d.Config.Home); err == nil {
		sb.State = string(t.State)
	}
	st := api.AuthorityStatus{
		ServedBy: sb,
		Machine:  snap.Authority, State: api.HQServing, Epoch: h.Epoch,
		Silent:  &api.HQSilence{StandbyProbe: api.ProbeUnknown},
		Standby: api.StandbyStatus{State: api.StandbyNone},
	}
	if !h.Heard.IsZero() {
		since := h.Heard.UTC()
		st.Silent.Since = &since
	}
	if h.Successor != nil {
		st.Standby = api.StandbyStatus{Machine: h.Successor.Machine, State: api.StandbyUnknown}
	}
	return st
}
