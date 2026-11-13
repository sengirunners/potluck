package conformance

import (
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/sengirunners/potluck/protocol"
)

// Acquire progress and cancel (docs/protocol.md): with capability
// acquire-progress, progress answers an acquire's state by its request_id
// while it has no handle yet, and cancel stops it. A driver whose acquire
// answers before progress sees it in flight skips the in-flight checks: it
// is not wrong, only too fast to watch.

const (
	pgCaps     = "a driver listing progress and cancel lists capability acquire-progress, and the other way round"
	pgUnknown  = "progress of a request_id never sent answers not_found"
	pgFirst    = "cancel before any acquire: cancelled true, then an acquire with that request_id answers conflict and makes nothing"
	pgFailed   = "cancel after a refused acquire: cancelled true, then an acquire with that request_id answers conflict and makes nothing"
	pgInFlight = "progress of an acquire in flight answers starting, with a phase"
	pgReady    = "progress after an acquire answered says ready"
	pgLate     = "a refused repeat of an acquire that answered leaves it ready; then cancel: cancelled false, the resource still live"
	pgCancel   = "cancel of an acquire in flight: cancelled true, the acquire answers conflict, progress cancelled, nothing live"
	pgAfter    = "after a cancel, an acquire with the same request_id answers conflict and makes nothing; a second cancel answers cancelled true"
)

type flight struct {
	a   acquired
	e   *protocol.Error
	err error
}

// acquireAsync sends h's acquire on its own goroutine (exchange is safe for
// that); the caller tracks what it made with track, on its own goroutine.
func (r *run) acquireAsync(h held) <-chan flight {
	ch := make(chan flight, 1)
	go func() {
		var f flight
		f.e, f.err = r.call(verbAcquire, "", h.sent, &f.a)
		ch <- f
	}()
	return ch
}

func (r *run) progressOf(key string) (protocol.AcquireProgress, error) {
	p, e, err := r.progressCall(key)
	if err == nil {
		err = must(e, nil)
	}
	return p, err
}

// progressCall is one progress call: a refusal as e, a bad answer as err.
func (r *run) progressCall(key string) (protocol.AcquireProgress, *protocol.Error, error) {
	var p protocol.AcquireProgress
	e, err := r.call(protocol.VerbProgress, "", args("request_id", key), &p)
	if err != nil || e != nil {
		return p, e, err
	}
	return p, nil, progressShape(p, key)
}

func progressShape(p protocol.AcquireProgress, key string) error {
	switch {
	case p.RequestID != key:
		return fmt.Errorf("progress answered request_id %q, want %q", p.RequestID, key)
	case !protocol.AcquireState(p.State):
		return fmt.Errorf("progress answered state %q, want starting, ready, failed or cancelled", p.State)
	case p.State == protocol.AcquireStarting && !protocol.AcquirePhase(p.Phase):
		return fmt.Errorf("progress answered starting with phase %q, want a lowercase word", p.Phase)
	}
	return nil
}

// inFlightKnown is how soon after an acquire is sent progress must know it
// (docs/protocol.md); generous, for a loaded machine.
const inFlightKnown = 3 * time.Second

// watch polls key's progress until its acquire lands (or, with stopAt, until
// progress first says starting). It answers how many starting answers it saw.
// A not_found asked inFlightKnown after the acquire was sent, while it is
// still out, is an error: a driver cannot pass by never saying starting.
func (r *run) watch(key string, ch <-chan flight, stopAtStarting bool) (int, *flight, error) {
	starting := 0
	var bad error
	sent := time.Now()
	for {
		select {
		case f := <-ch:
			return starting, &f, bad
		default:
		}
		asked := time.Since(sent)
		began := time.Now()
		p, e, err := r.progressCall(key)
		if took := time.Since(began); took >= inFlightKnown && bad == nil {
			// The acquire was out when the call began: progress waited on it.
			bad = fmt.Errorf("progress took %s while an acquire was in flight; it must not wait for one", took.Round(time.Second))
		}
		if e != nil && e.Code == protocol.CodeNotFound && asked >= inFlightKnown && bad == nil {
			select {
			case f := <-ch:
				return starting, &f, bad // it landed meanwhile: nothing proven
			default:
				bad = fmt.Errorf("progress answered not_found %s into an acquire still in flight", asked.Round(time.Second))
			}
		}
		switch {
		case err != nil:
			if bad == nil {
				bad = err
			}
		case e != nil: // not_found: the driver has not seen the acquire yet
			if e.Code != protocol.CodeNotFound && bad == nil {
				bad = must(e, nil)
			}
		case p.State == protocol.AcquireStarting:
			starting++
			if stopAtStarting {
				return starting, nil, bad
			}
		}
		select {
		case f := <-ch:
			return starting, &f, bad
		case <-time.After(20 * time.Millisecond):
		case <-r.ctx.Done():
			f := <-ch
			return starting, &f, r.ctx.Err()
		}
	}
}

func (r *run) acquireProgress() {
	caps := []string{}
	if r.desc.Capabilities != nil {
		caps = *r.desc.Capabilities
	}
	hasCap := slices.Contains(caps, protocol.CapAcquireProgress)
	if !r.has(protocol.VerbProgress) && !r.has(protocol.VerbCancel) && !hasCap {
		r.skip("acquire progress and cancel", "describe lists no acquire-progress capability")
		return
	}
	if !r.add(pgCaps, func() error {
		if !r.has(protocol.VerbProgress) || !r.has(protocol.VerbCancel) || !hasCap {
			return fmt.Errorf("verbs %v, capabilities %v: want progress, cancel and acquire-progress together", r.desc.Verbs, caps)
		}
		return nil
	}()) {
		return
	}
	r.add(pgUnknown, r.expect(protocol.VerbProgress, "", args("request_id", r.key()), protocol.CodeNotFound))
	first := r.newHeld(r.longTTL())
	r.add(pgFirst, func() error {
		var c protocol.CancelAnswer
		if err := must(r.call(protocol.VerbCancel, "", args("request_id", first.key), &c)); err != nil {
			return fmt.Errorf("cancel: %v", err)
		}
		if !c.Cancelled {
			return fmt.Errorf("cancel answered cancelled false (state %s)", c.State)
		}
		a, e, err := r.acquire(first.sent, r.cfg)
		first.acquired = a
		if err := wantCode(e, err, protocol.CodeConflict); err != nil {
			return fmt.Errorf("acquire after the cancel: %v", err)
		}
		return r.nothingLive(first.key)
	}())
	r.endHeld(first)

	// A refused acquire holds nothing, so a cancel after it still stands:
	// a retry with the key queued behind it must never make the resource.
	failed := r.newHeld(r.longTTL())
	r.add(pgFailed, func() error {
		var m map[string]any
		if err := json.Unmarshal(failed.sent, &m); err != nil {
			return err
		}
		m["ttl_s"] = r.ttlMax() + 1
		over, _ := json.Marshal(m)
		a, e, err := r.acquire(over, r.cfg)
		r.endHeld(held{acquired: a}) // a driver that took it: ended here, the refuse group fails it
		if err := wantCode(e, err, protocol.CodeBadRequest); err != nil {
			return fmt.Errorf("acquire with ttl_s past ttl_max_s: %v", err)
		}
		var c protocol.CancelAnswer
		if err := must(r.call(protocol.VerbCancel, "", args("request_id", failed.key), &c)); err != nil {
			return fmt.Errorf("cancel: %v", err)
		}
		if !c.Cancelled {
			return fmt.Errorf("cancel answered cancelled false (state %s) for an acquire that was refused", c.State)
		}
		a, e, err = r.acquire(failed.sent, r.cfg)
		failed.acquired = a
		if err := wantCode(e, err, protocol.CodeConflict); err != nil {
			return fmt.Errorf("acquire after the cancel: %v", err)
		}
		return r.nothingLive(failed.key)
	}())
	r.endHeld(failed)

	// An acquire watched to the end.
	h := r.newHeld(r.longTTL())
	starting, f, werr := r.watch(h.key, r.acquireAsync(h), false)
	h.acquired, f.e, f.err = r.track(h.sent, f.a, f.e, f.err)
	defer r.endHeld(h)
	if !r.add("acquire, watched with progress", must(f.e, f.err)) {
		return
	}
	if starting == 0 && werr == nil {
		r.skip(pgInFlight, "the acquire answered before progress saw it starting")
	} else {
		r.add(pgInFlight, werr)
	}
	r.add(pgReady, func() error {
		p, err := r.progressOf(h.key)
		if err == nil && p.State != protocol.AcquireReady {
			err = fmt.Errorf("progress says %s (%s), want ready", p.State, p.Phase)
		}
		return err
	}())
	r.add(pgLate, func() error {
		var m map[string]any
		if err := json.Unmarshal(h.sent, &m); err != nil {
			return err
		}
		m["ttl_s"] = r.ttlMax() + 1
		over, _ := json.Marshal(m)
		a, e, err := r.acquire(over, r.cfg)
		if a.Handle != "" && a.Handle != h.Handle {
			r.endHeld(held{acquired: a})
			return fmt.Errorf("a repeat with ttl_s past ttl_max_s made %s", a.Handle)
		}
		if err != nil {
			return fmt.Errorf("a repeat with ttl_s past ttl_max_s: %v", err)
		}
		_ = e // refused, or answered the key's handle: either leaves the resource
		if p, err := r.progressOf(h.key); err != nil || p.State != protocol.AcquireReady {
			return fmt.Errorf("progress after a refused repeat: %s %v, want ready", p.State, err)
		}
		var c protocol.CancelAnswer
		if err := must(r.call(protocol.VerbCancel, "", args("request_id", h.key), &c)); err != nil {
			return err
		}
		if c.Cancelled {
			return fmt.Errorf("cancel answered cancelled true for an acquire that had answered %s", h.Handle)
		}
		st, err := r.status(h.Handle)
		if err == nil && !slices.Contains(liveStates, st.State) {
			err = fmt.Errorf("status %s after the cancel, want it live", st.State)
		}
		return err
	}())

	// An acquire cancelled in flight.
	c := r.newHeld(r.longTTL())
	ch := r.acquireAsync(c)
	_, landed, werr := r.watch(c.key, ch, true)
	if landed != nil {
		c.acquired, _, _ = r.track(c.sent, landed.a, landed.e, landed.err)
		r.endHeld(c)
		if werr != nil {
			r.add(pgCancel, werr)
			return
		}
		r.skip(pgCancel, "the acquire answered before progress saw it starting")
		r.skip(pgAfter, "no acquire was cancelled in flight")
		return
	}
	var ans protocol.CancelAnswer
	asked := time.Now()
	cerr := must(r.call(protocol.VerbCancel, "", args("request_id", c.key), &ans))
	took := time.Since(asked)
	ff := <-ch
	lag := time.Since(asked)
	c.acquired, ff.e, ff.err = r.track(c.sent, ff.a, ff.e, ff.err)
	defer r.endHeld(c)
	// A cancel that says ready lost an honest race only if the acquire
	// answers at once after the cancel was sent: one still out
	// inFlightKnown later was in flight, and cancel refused to stop it or
	// waited for it.
	if werr == nil && cerr == nil && !ans.Cancelled && ans.State == protocol.AcquireReady && ff.e == nil && ff.err == nil && lag < inFlightKnown {
		r.skip(pgCancel, "the acquire finished as the cancel was sent")
		r.skip(pgAfter, "no acquire was cancelled in flight")
		return
	}
	if !r.add(pgCancel, func() error {
		switch {
		case werr != nil:
			return werr
		case cerr != nil:
			return fmt.Errorf("cancel: %v", cerr)
		case took >= inFlightKnown:
			return fmt.Errorf("cancel took %s with an acquire in flight; it must not wait for one", took.Round(time.Second))
		case !ans.Cancelled:
			return fmt.Errorf("cancel answered cancelled false (state %s) for an acquire in flight", ans.State)
		}
		if err := wantCode(ff.e, ff.err, protocol.CodeConflict); err != nil {
			return fmt.Errorf("the cancelled acquire %v (handle %q)", err, ff.a.Handle)
		}
		p, err := r.progressOf(c.key)
		if err == nil && p.State != protocol.AcquireCancelled {
			err = fmt.Errorf("progress says %s after the cancel, want cancelled", p.State)
		}
		if err == nil {
			err = r.nothingLive(c.key)
		}
		return err
	}()) {
		return
	}
	r.add(pgAfter, func() error {
		_, e, err := r.acquire(c.sent, r.cfg)
		if err := wantCode(e, err, protocol.CodeConflict); err != nil {
			return fmt.Errorf("acquire again: %v", err)
		}
		if err := r.nothingLive(c.key); err != nil {
			return err
		}
		var again protocol.CancelAnswer
		if err := must(r.call(protocol.VerbCancel, "", args("request_id", c.key), &again)); err != nil {
			return fmt.Errorf("a second cancel: %v", err)
		}
		if !again.Cancelled {
			return fmt.Errorf("a second cancel answered cancelled false (state %s)", again.State)
		}
		return nil
	}())
}

// nothingLive: list shows no live handle for key.
func (r *run) nothingLive(key string) error {
	es, err := r.list()
	if err != nil {
		return err
	}
	for _, e := range es {
		if e.RequestID == key && slices.Contains(liveStates, e.State) {
			r.live[e.Handle] = true // ended by the suite
			return fmt.Errorf("list shows %s %s for the cancelled request_id", e.Handle, e.State)
		}
	}
	return nil
}
