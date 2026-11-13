package runtarget

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/protocol"
)

// An acquire before it has a handle (docs/protocol.md, Acquire progress and
// cancel): progress says how far it got and cancel stops it, both by the
// caller's request_id. The daemon answers from what it holds itself (an
// acquire queued behind the binding's lock, a row still acquiring) and, with
// capability acquire-progress, from the driver. Without the capability it can
// only say starting, and cannot cancel.

// requestIDOK is the one rule for a caller's request_id.
func requestIDOK(id string) error {
	if id == "" || len(id) > 200 || strings.ContainsAny(id, "\r\n\t ") {
		return api.Errorf(api.CodeBadRequest, "request_id is required (one word, at most 200 bytes)")
	}
	return nil
}

// inFlight is what this daemon knows of the acquires it is serving now.
type inFlight struct {
	mu sync.Mutex
	m  map[string]*flight
}

type flight struct {
	n          int  // acquire calls holding it (a retry beside the first)
	asked      bool // the driver has been sent the acquire
	cancelled  bool // stopped here, before its driver call
	stopped    bool // its driver answered a cancel cancelled: true
	cancelling bool // a cancel was sent its driver (its answer may not be in)
	jit        bool // an acquire holding it carried a jit_config
}

func flightKey(project, target, key string) string { return project + "\x00" + target + "\x00" + key }

// enter holds k for one acquire call, which carried a jit_config if jit;
// leave lets it go.
func (f *inFlight) enter(k string, jit bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.m == nil {
		f.m = map[string]*flight{}
	}
	if f.m[k] == nil {
		f.m[k] = &flight{}
	}
	f.m[k].n++
	f.m[k].jit = f.m[k].jit || jit
}

func (f *inFlight) leave(k string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if e := f.m[k]; e != nil {
		if e.n--; e.n <= 0 {
			delete(f.m, k)
		}
	}
}

// ask marks k's driver asked; false when k was cancelled first, and the
// acquire then never reaches the driver.
func (f *inFlight) ask(k string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	e := f.m[k]
	if e == nil {
		return true
	}
	if e.cancelled {
		return false
	}
	e.asked = true
	return true
}

// cancel records that k's driver answered a cancel cancelled: true. An
// acquire holding k that has not asked its driver yet stops here (one that
// has is the driver's to stop); either way a handle it still gets is ended
// (stopped).
func (f *inFlight) cancel(k string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if e := f.m[k]; e != nil {
		e.stopped = true
		if !e.asked {
			e.cancelled = true
		}
	}
}

// stopped reports whether a cancel of k stood while an acquire held it.
func (f *inFlight) stopped(k string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	e := f.m[k]
	return e != nil && e.stopped
}

// sendingCancel records that a cancel of k is about to be sent its driver.
func (f *inFlight) sendingCancel(k string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if e := f.m[k]; e != nil {
		e.cancelling = true
	}
}

// cancelSent reports whether a cancel of k was sent its driver while an
// acquire held it, stood or not: a driver may have crashed after making the
// box and keeping the cancel, before the cancel's answer came back.
func (f *inFlight) cancelSent(k string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	e := f.m[k]
	return e != nil && (e.stopped || e.cancelling)
}

// peek answers whether an acquire holds k, whether its driver was asked,
// whether it was cancelled before that, and whether the driver's message
// may be shown: only for an acquire held here that carried no jit_config.
func (f *inFlight) peek(k string) (held, asked, cancelled, quiet bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	e := f.m[k]
	if e == nil {
		return false, false, false, true
	}
	return true, e.asked, e.cancelled, e.jit
}

// A cancel of a request_id nothing here knows (no acquire holds it, no row
// has it) is kept by the driver for 24 hours (docs/protocol.md), so each
// costs driver state: cancelQuota bounds how many one subject sends in that
// time. The subject is the actor on the authority. A lending host cannot
// tell a known key from a new one, so it counts every cancel it relays, per
// actor (the tag its key carries) under a cap for the whole project: one
// runaway actor cannot use up the project's budget for the others. A member
// with several actors (agent sessions) has a cap for each; the project's cap
// is the backstop.
type cancelQuota struct {
	mu sync.Mutex
	m  map[string][]time.Time // each subject's cancels counted, oldest first
}

const (
	cancelsUnknownMax      = 64   // per actor, of keys this daemon does not know
	cancelsActorRelayedMax = 256  // per actor of a project, of every cancel a lending host relays
	cancelsRelayedMax      = 1024 // per project, of every cancel a lending host relays
	cancelKept             = 24 * time.Hour
)

// quotaCap is one subject's limit in a take.
type quotaCap struct {
	who string
	max int
}

// take counts one cancel of who's at now: false, and nothing counted, when
// who already sent max within cancelKept.
func (q *cancelQuota) take(who string, max int, now time.Time) (ok bool, wait time.Duration) {
	who, wait = q.takeAll(now, quotaCap{who, max})
	return who == "", wait
}

// takeAll counts one cancel against every cap, or none: the first subject
// already at its max is returned (and nothing counted), with how long until
// its oldest counted cancel leaves the count; "" when all took it.
func (q *cancelQuota) takeAll(now time.Time, caps ...quotaCap) (string, time.Duration) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.m == nil {
		q.m = map[string][]time.Time{}
	}
	fresh := func(ts []time.Time) []time.Time {
		i := 0
		for i < len(ts) && now.Sub(ts[i]) >= cancelKept {
			i++
		}
		return ts[i:]
	}
	if len(q.m) > 4096 { // subjects that went quiet are forgotten
		for k, ts := range q.m {
			if len(fresh(ts)) == 0 {
				delete(q.m, k)
			}
		}
	}
	for _, c := range caps {
		ts := fresh(q.m[c.who])
		if len(ts) >= c.max {
			q.m[c.who] = ts
			if c.max <= 0 || len(ts) == 0 { // a cap of nothing is never open
				return c.who, cancelKept
			}
			return c.who, cancelKept - now.Sub(ts[len(ts)-c.max])
		}
	}
	for _, c := range caps {
		q.m[c.who] = append(fresh(q.m[c.who]), now)
	}
	return "", 0
}

// errCancelQuota refuses a cancel past limit in 24 hours, telling the caller
// to retry after wait (when the count next has room): in retry_after, and in
// the message, which is all a lending host's refusal keeps on its way to the
// authority's caller.
func errCancelQuota(limit int, of string, wait time.Duration) error {
	return api.Errorf(api.CodeRateLimited, "%d cancels of %s in 24 hours is the most this daemon sends a driver; cancel only a request_id you sent; retry in %d minutes", limit, of, int((wait+time.Minute-1)/time.Minute)).WithRetryAfter(max(1, int((wait+time.Second-1)/time.Second)))
}

func errCancelled() error {
	return api.Errorf(api.CodeConflict, "the acquire with this request_id was cancelled")
}

// acquireRef is a progress or cancel request resolved: the binding, the key
// the driver knows (the request_id, keyed by actor), and the daemon's row.
type acquireRef struct {
	project string
	b       computeapi.RunTarget
	actor   string
	rid     string // the caller's request_id
	key     string // what the driver was sent
	db      *sql.DB
	rw      row
	seen    bool
}

// progressCapable reports whether b's driver serves progress and cancel
// (asked only when the daemon cannot answer on its own: describe is a driver
// call). A caller polling progress would run describe on every poll, so an
// answer is kept for capsFor.
func (s *Service) progressCapable(ctx context.Context, project string, b computeapi.RunTarget) (bool, error) {
	k := project + "\x00" + b.Name + "\x00" + b.Driver + "\x00" + b.Host
	if v, ok := s.progressCaps.Load(k); ok && s.now().Sub(v.(capsSeen).at) < capsFor {
		return v.(capsSeen).ok, nil
	}
	d, err := s.describeOf(ctx, b)
	if err != nil {
		return false, err
	}
	ok := slices.Contains(d.Capabilities, protocol.CapAcquireProgress) &&
		slices.Contains(d.Verbs, protocol.VerbProgress) && slices.Contains(d.Verbs, protocol.VerbCancel)
	s.progressCaps.Store(k, capsSeen{ok: ok, at: s.now()})
	return ok, nil
}

// capsSeen is progressCapable's answer for a binding, and when it was found.
type capsSeen struct {
	ok bool
	at time.Time
}

const capsFor = 10 * time.Second

func (s *Service) acquireOf(r *http.Request, a authz.Action) (acquireRef, error) {
	rid := r.PathValue("request_id")
	if err := requestIDOK(rid); err != nil {
		return acquireRef{}, err
	}
	if builderRequest(rid) { // a dependency layer's builder is the daemon's alone (deps.go)
		return acquireRef{}, api.NotFound("acquire", rid)
	}
	t, b, db, err := s.target(r, a)
	if err != nil {
		return acquireRef{}, err
	}
	sub := core.SubjectOf(r.Context())
	actor := sub.Address
	if q := r.URL.Query().Get("actor"); q != "" && q != actor {
		if !owner(r.Context()) { // as acquire: only the machine's owner acts for another
			return acquireRef{}, api.Errorf(api.CodeForbidden, "actor must be the caller")
		}
		actor = q
	}
	ref := acquireRef{project: t.Project.ID, b: b, actor: actor, rid: rid, key: actorKey(actor, rid), db: db}
	if ref.rw, ref.seen, err = byRequest(r.Context(), db, b.Name, ref.key); err != nil {
		return ref, err
	}
	if ref.seen && (ref.rw.Actor != actor || !sub.Can(a, authz.Object{Project: t.Project.ID, Area: ref.rw.Area})) {
		return ref, api.NotFound("acquire", rid) // as acquire answers another's key
	}
	return ref, nil
}

func (s *Service) acquireProgress(r *http.Request) (any, error) {
	ref, err := s.acquireOf(r, authz.Read)
	if err != nil {
		return nil, err
	}
	out := computeapi.RunTargetAcquireProgress{RequestID: ref.rid}
	if ref.seen && ref.rw.Handle != "" {
		if endedByCancel(ref.rw) {
			out.State = protocol.AcquireCancelled
			return out, nil
		}
		out.State, out.Handle = protocol.AcquireReady, ref.rw.Handle
		return out, nil
	}
	held, asked, cancelled, quiet := s.inflight.peek(flightKey(ref.project, ref.b.Name, ref.key))
	switch {
	case cancelled:
		out.State = protocol.AcquireCancelled
		return out, nil
	case held && !asked:
		out.State, out.Phase = protocol.AcquireStarting, protocol.AcquirePhaseQueued
		return out, nil
	}
	pending := held || ref.seen // ref.seen without a handle: still acquiring
	capable, err := s.progressCapable(r.Context(), ref.project, ref.b)
	if err != nil {
		return nil, err
	}
	var p protocol.AcquireProgress
	if capable {
		err = s.call(r.Context(), ref.b, protocol.VerbProgress, "", map[string]string{"request_id": ref.key}, &p)
		if oldHost(ref.b, err) {
			capable = false
		}
		err = withheld(protocol.VerbProgress, err)
	}
	if !capable {
		if !pending {
			return nil, api.NotFound("acquire", ref.rid)
		}
		out.State = protocol.AcquireStarting
		return out, nil
	}
	switch {
	case api.IsCode(err, api.CodeNotFound) && pending: // sent, not yet recorded by the driver
		out.State = protocol.AcquireStarting
		return out, nil
	case err != nil:
		return nil, err
	case !protocol.AcquireState(p.State):
		return nil, api.Errorf(api.CodeInternal, "potluck-driver-%s answered a progress state that is none of the protocol's", ref.b.Driver) // its words could quote what was sent
	}
	out.State = p.State
	if p.State == protocol.AcquireStarting {
		// Only a starting acquire's message, and only when this daemon holds
		// the acquire and knows it carried no jit_config: a driver may quote
		// what it was sent, and a jit_config is a secret. A failed acquire's
		// message never: the caller had acquire's scrubbed refusal already.
		if !quiet {
			out.Message = oneLine(p.Message)
		}
		if protocol.AcquirePhase(p.Phase) {
			out.Phase = p.Phase
		}
	}
	return out, nil
}

func (s *Service) cancelAcquire(r *http.Request) (any, error) {
	ref, err := s.acquireOf(r, authz.Operate)
	if err != nil {
		return nil, err
	}
	out := computeapi.RunTargetAcquireCancel{RequestID: ref.rid}
	fk := flightKey(ref.project, ref.b.Name, ref.key)
	if ref.seen && ref.rw.Handle != "" {
		if endedByCancel(ref.rw) {
			out.Cancelled, out.State = true, protocol.AcquireCancelled
			return out, nil // a second cancel: it still stands
		}
		out.State, out.Handle = protocol.AcquireReady, ref.rw.Handle
		return out, nil // made: release the handle instead
	}
	capable, err := s.progressCapable(r.Context(), ref.project, ref.b)
	if err != nil {
		return nil, err
	}
	if !capable {
		return nil, api.Errorf(api.CodeNotImplemented, "potluck-driver-%s cannot cancel an acquire (no %s capability)", ref.b.Driver, protocol.CapAcquireProgress)
	}
	// A repeat of a cancel that stood is answered here, without the driver.
	if s.inflight.stopped(fk) || (ref.seen && ref.rw.cancelled) {
		out.Cancelled, out.State = true, protocol.AcquireCancelled
		return out, nil
	}
	// A key no driver was sent (no acquire here asked one, no row from an
	// earlier call) is counted: the driver keeps its cancel. An acquire still
	// queued here counts too, as it costs nothing to start one.
	if _, asked, _, _ := s.inflight.peek(fk); !asked && !ref.seen {
		if ok, wait := s.cancels.take("actor:"+ref.actor, cancelsUnknownMax, s.now()); !ok {
			return nil, errCancelQuota(cancelsUnknownMax, "acquires no driver was sent", wait)
		}
	}
	// The driver keeps the cancel, for an acquire in flight or one it has
	// not seen yet (a queued acquire whose key an earlier call sent stops
	// here too once it stands, before its driver call).
	s.inflight.sendingCancel(fk)
	var c protocol.CancelAnswer
	err = s.call(r.Context(), ref.b, protocol.VerbCancel, "", map[string]string{"request_id": ref.key}, &c)
	switch {
	case oldHost(ref.b, err):
		return nil, api.Errorf(api.CodeNotImplemented, "%s's daemon cannot relay cancel", ref.b.Host)
	case err != nil:
		return nil, withheld(protocol.VerbCancel, err)
	case !protocol.AcquireState(c.State):
		return nil, api.Errorf(api.CodeInternal, "potluck-driver-%s answered a cancel state that is none of the protocol's", ref.b.Driver)
	}
	if c.Cancelled {
		s.inflight.cancel(fk)
		// After the mark: an acquire with the driver inserted its row
		// before it asked, so the row is there to mark, and one that
		// leaves it acquiring (its list failed) is followed up by the sweep.
		if err := markCancelled(r.Context(), ref.db, ref.b.Name, ref.key); err != nil {
			return nil, err
		}
		// An acquire that got its handle before the mark above recorded
		// it first (acquire records, then asks stopped): end it here, so a
		// cancel that answers cancelled never leaves a box live.
		rw, seen, err := byRequest(r.Context(), ref.db, ref.b.Name, ref.key)
		if err != nil {
			return nil, err
		}
		if held, asked, _, _ := s.inflight.peek(fk); seen && rw.Handle == "" && (!held || !asked) {
			// No acquire here is with the driver for this key (the daemon
			// restarted, or a repeat is still queued, stopped here now):
			// what an earlier call made under the key, if anything, is found
			// by list.
			if h, found, lerr := s.reconcile(r.Context(), ref.b, ref.key); lerr == nil && found {
				_ = s.record(r.Context(), ref.project, ref.db, ref.b.Name, ref.key, upd{handle: h.Handle, state: h.State, expiresAt: h.ExpiresAt})
				rw, _, _ = byRequest(r.Context(), ref.db, ref.b.Name, ref.key)
			}
		}
		if seen && rw.Handle != "" {
			if !s.endCancelled(r.Context(), ref.project, ref.db, ref.b, rw) {
				out.State, out.Handle = protocol.AcquireReady, rw.Handle
				return out, nil // it could not be ended: held, so its holder releases it
			}
			out.Cancelled, out.State = true, protocol.AcquireCancelled
			return out, nil
		}
	}
	if !c.Cancelled {
		// Too late: made. The acquire may have recorded the handle while
		// this cancel was with the driver and answered its caller an error
		// (cutOff): this answer hands it over, to be released.
		if rw, seen, err := byRequest(r.Context(), ref.db, ref.b.Name, ref.key); err == nil && seen && rw.Handle != "" {
			out.Handle = rw.Handle
		}
	}
	out.Cancelled, out.State = c.Cancelled, c.State
	return out, nil
}

// endCancelled ends the box an acquire made though its cancel stood: true
// once it is ended (or was already). False when the driver would not end
// it: it stays recorded and held, to be released by its holder or the sweep.
// Either way the row records it (setCancelled): ended by its cancel, or a
// plain held handle.
func (s *Service) endCancelled(ctx context.Context, project string, db *sql.DB, b computeapi.RunTarget, rw row) bool {
	ctx = context.WithoutCancel(ctx)
	ended := terminal(rw.State)
	if !ended {
		ended = s.releaseRow(ctx, project, db, b, rw.Handle, rw.RequestID, nil) == nil
	}
	if !ended {
		// The acquire and the cancel may both end it; the other's release
		// may be the one that took.
		now, ok, _ := byRequest(ctx, db, b.Name, rw.RequestID)
		ended = ok && terminal(now.State)
	}
	_ = setCancelled(ctx, db, b.Name, rw.RequestID, ended)
	return ended
}

// endedByCancel: rw's handle was ended because a cancel of its acquire
// stood, so its caller never held it, and progress and cancel say
// cancelled. A handle its cancel could not end is held, and ready.
func endedByCancel(rw row) bool { return rw.cancelled && terminal(rw.State) }

// releaseRow ends a handle the acquire itself will not hand over, and
// records it released. args are release's (nil, or deps.go's seal).
func (s *Service) releaseRow(ctx context.Context, project string, db *sql.DB, b computeapi.RunTarget, handle, requestID string, args any) error {
	var rel struct {
		Released bool `json:"released"`
	}
	if err := s.call(ctx, b, verbRelease, handle, args, &rel); err != nil {
		return err
	}
	return s.record(ctx, project, db, b.Name, requestID, upd{state: stateReleased})
}

// cancelStood is an acquire's last step once its row holds the handle
// (recorded before this asks, so a cancel that stands later reads the row
// and ends it itself, in cancelAcquire): if a cancel of fk stood, the box
// is ended and the acquire answers conflict. One that cannot be ended is
// left held and answered, rather than live with no holder.
func (s *Service) cancelStood(ctx context.Context, project string, db *sql.DB, b computeapi.RunTarget, fk string, rw row) error {
	if s.inflight.stopped(fk) && s.endCancelled(ctx, project, db, b, rw) {
		return errCancelled()
	}
	return nil
}

// settleCancelled follows up the acquires of project whose cancel stood
// while they had no handle and whose driver call ended without zm learning
// what it made (cut off, or crashed, and then the list failed): one no
// acquire here holds is found by list and ended, or, when the driver lists
// nothing for it, forgotten. A driver that fails waits for the next pass.
func (s *Service) settleCancelled(ctx context.Context, project string, db *sql.DB, bindings map[string]computeapi.RunTarget) {
	// Older than an acquire, its retry and the list it reconciles with can
	// take, so none is still with a driver started before a restart.
	limit := acquireLimit
	if s.acquireWithin > 0 {
		limit = s.acquireWithin
	}
	before := s.now().Add(-3*limit - verbLimit).UTC().Format(tsLayout)
	failed := map[string]bool{}
	calls := 0
	eachCancelledAcquiring(ctx, db, before, cancelledPage, func(rw row) bool {
		if s.settleOne(ctx, project, db, bindings, failed, rw) {
			calls++
		}
		return calls < cancelledCallsMax
	})
}

// A pass settles at most cancelledCallsMax rows that need a driver call (a
// crash can leave many); the rest wait for the next pass, so the sweep
// still gets on to deadlines. cancelledPage is how many rows are read at a time.
const (
	cancelledPage     = 100
	cancelledCallsMax = 100
)

// eachCancelledAcquiring runs f on every row cancelledAcquiring lists, a page
// at a time in key order, until f says stop or ctx ends: rows f leaves
// alone (an unbound target, a failing driver) never hide later ones.
func eachCancelledAcquiring(ctx context.Context, db *sql.DB, before string, page int, f func(row) bool) {
	var target, requestID string
	for {
		rows, err := cancelledAcquiring(ctx, db, before, target, requestID, page)
		if err != nil {
			return
		}
		for _, rw := range rows {
			if ctx.Err() != nil || !f(rw) {
				return
			}
		}
		if len(rows) < page {
			return
		}
		target, requestID = rows[len(rows)-1].Target, rows[len(rows)-1].RequestID
	}
}

// settleOne follows up one cancelled acquiring row (settleCancelled); true
// when it needed a driver call.
func (s *Service) settleOne(ctx context.Context, project string, db *sql.DB, bindings map[string]computeapi.RunTarget, failed map[string]bool, rw row) bool {
	b, bound := bindings[rw.Target]
	if !bound {
		b, bound = remoteStub(project, rw.Target)
	}
	if held, _, _, _ := s.inflight.peek(flightKey(project, rw.Target, rw.RequestID)); held || !bound || failed[rw.Target] {
		return false
	}
	h, found, err := s.reconcile(ctx, b, rw.RequestID)
	switch {
	case err != nil:
		failed[rw.Target] = true
		return true
	case !found:
		_ = deleteAcquiring(ctx, db, rw.Target, rw.RequestID) // nothing was made, and the driver keeps the cancel
		return true
	}
	if err := s.record(ctx, project, db, rw.Target, rw.RequestID, upd{handle: h.Handle, state: h.State, expiresAt: h.ExpiresAt}); err != nil {
		return true
	}
	if now, ok, err := byRequest(ctx, db, rw.Target, rw.RequestID); err == nil && ok && now.Handle != "" {
		s.endCancelled(ctx, project, db, b, now) // one it cannot end stays held: the sweep ends it at its deadline
	}
	return true
}

// cutOff answers an acquire whose driver call answered err though it may
// have made the resource: its first call was cut off at its time limit and
// its retry (SPEC 20: same key, never afresh) answered err, timed out again
// or refused; or a cancel stood, and the driver failed in any way (it may
// have crashed after making the box). The answer says nothing of what was
// made: zm reconciles from list before the key is forgotten, keeps what it
// finds, and ends it if a cancel stood. A list that fails leaves the row
// acquiring: a retry learns the rest, and with a cancel that stood the
// sweep does (settleCancelled).
func (s *Service) cutOff(ctx context.Context, project string, db *sql.DB, b computeapi.RunTarget, requestID, fk string, seen bool, err error) (any, error) {
	retry := err
	if errors.Is(err, errTimeout) {
		retry = api.Errorf(api.CodeUnavailable, "potluck-driver-%s did not answer acquire; retry with the same request_id", b.Driver)
	}
	if s.inflight.stopped(fk) {
		retry = errCancelled()
		_ = markCancelled(ctx, db, b.Name, requestID) // the sweep's to follow up, should the list below fail
	}
	h, found, lerr := s.reconcile(ctx, b, requestID)
	switch {
	case lerr != nil:
		return nil, retry
	case !found:
		if !seen {
			_ = deleteAcquiring(ctx, db, b.Name, requestID) // nothing was made
		}
		return nil, retry
	}
	if err := s.record(ctx, project, db, b.Name, requestID, upd{handle: h.Handle, state: h.State, expiresAt: h.ExpiresAt}); err != nil {
		return nil, err
	}
	rw, ok, rerr := byRequest(ctx, db, b.Name, requestID)
	if rerr != nil || !ok {
		return nil, retry
	}
	if err := s.cancelStood(ctx, project, db, b, fk, rw); err != nil {
		return nil, err
	}
	if s.inflight.stopped(fk) {
		return rw.RunTargetHandle, nil // cancelled, but it could not be ended: held, and answered
	}
	return nil, retry
}

// notRelayed ends HostCall's refusal of a verb it does not relay; daemons
// from before progress and cancel answer them with it.
const notRelayed = "is not relayed"

// oldHost: a lending host whose daemon predates progress and cancel refuses
// to relay them, though its driver may list the capability. Only that
// refusal counts: any other bad_request is the driver's own answer.
func oldHost(b computeapi.RunTarget, err error) bool {
	var e *api.Error
	return b.Host != "" && errors.As(err, &e) && e.Code == api.CodeBadRequest && strings.HasSuffix(e.Message, notRelayed)
}

// withheld keeps a driver's progress or cancel refusal's code and drops its
// words: it may quote what the acquire was sent (a jit_config), which
// neither verb carries, so it cannot be scrubbed of it. The daemon's own
// errors (a pin refused, a lending host's answer, already withheld there)
// keep theirs.
func withheld(verb string, err error) error {
	var w *driverWords
	if !errors.As(err, &w) {
		return err
	}
	return api.Errorf(w.err.Code, "the driver refused %s (%s; its message withheld)", verb, w.err.Code)
}

// progressRelayed is a lending host's progress or cancel answer as it
// leaves for the authority: the fields the authority reads, no message, and
// a refusal's code alone. A driver may quote what its acquire was sent, this
// host does not know what that was, and the answer crosses the tunnel.
func progressRelayed(verb string, out json.RawMessage, err error) (json.RawMessage, error) {
	if err != nil {
		return nil, withheld(verb, err)
	}
	// A word the authority would refuse goes as nothing, never as written.
	var v any
	if verb == protocol.VerbCancel {
		var c protocol.CancelAnswer
		_ = json.Unmarshal(out, &c)
		if !protocol.AcquireState(c.State) {
			c.State = ""
		}
		v = c
	} else {
		var p, q protocol.AcquireProgress
		_ = json.Unmarshal(out, &p)
		if protocol.AcquireState(p.State) {
			q.State = p.State
		}
		if protocol.AcquirePhase(p.Phase) {
			q.Phase = p.Phase
		}
		v = q
	}
	b, merr := json.Marshal(v)
	if merr != nil {
		return nil, merr
	}
	return b, nil
}
