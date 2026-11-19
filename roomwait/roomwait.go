// Package roomwait is the one wait of a command that asked for a box and was
// told only that there is no room now: it asks again, up to a bound, saying
// why and when it asks next. potluck sandbox create, potluck run, potluck
// agent-task and potluck host's check boxes wait through it (docs/daemon.md,
// placement).
//
// Only a temporary no room waits: an acquire refusal marked
// computeapi.RefusedNoRoom (every slot in use, the host's reclaim policy
// holding new work, its admission controller's no), or a *Full a caller
// returns for what it read itself. A target not listed now
// (computeapi.TargetNotListed) waits too, but only after an earlier ask in
// the same wait got a no room: the target answered then, so its host is out
// of touch for a moment. A target never seen in the wait fails at once.
// Every other answer, a hard refusal among them (bad_request, forbidden, an
// isolation refusal, a size no host offers), ends the wait at once.
package roomwait

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/kit/api"
)

// Default is how long a command waits for room unless told otherwise.
const Default = 30 * time.Minute

// The pace of asking again: a host's own retry time, held within minAsk
// and maxAsk; without one, firstAsk doubling up to backoffMax. A line is
// said again for an unchanged answer after sayAgain.
const (
	minAsk     = 5 * time.Second
	maxAsk     = 10 * time.Minute
	firstAsk   = 10 * time.Second
	backoffMax = 2 * time.Minute
	sayAgain   = 5 * time.Minute
)

// Full is a temporary no room: Where has none now (a binding, a machine),
// Why says what is full in the host's words, Retry is the host's own retry
// time (0: none given). Err is the answer it was read from, which the wait
// returns when it ends (nil: the Full itself).
type Full struct {
	Where, Why string
	Retry      time.Duration
	Err        error
}

func (f *Full) Error() string {
	if f.Err != nil {
		return f.Err.Error()
	}
	return computeapi.AcquireRefused{On: f.Where, Why: f.Why, RetryS: int(f.Retry / time.Second)}.NoRoomWords()
}

func (f *Full) Unwrap() error { return f.Err }

// Of says whether err is only a temporary no room, and what it says.
func Of(err error) (Full, bool) {
	var f *Full
	if errors.As(err, &f) {
		return *f, true
	}
	r, ok := computeapi.NoRoom(err)
	if !ok {
		return Full{}, false
	}
	return Full{Where: r.On, Why: r.Why, Retry: time.Duration(r.RetryS) * time.Second, Err: err}, true
}

// Wait is how long a command waits for room, and where it says so.
type Wait struct {
	// For is the bound; 0 (or less) waits not at all: a no room is
	// answered at once, as any other refusal.
	For time.Duration
	// Say takes one line per change (nil: none).
	Say func(line string)
	// Sleep and Now stand in for the clock (nil: a timer, time.Now).
	Sleep func(ctx context.Context, d time.Duration) error
	Now   func() time.Time
}

// Do asks, and while the answer is only a temporary no room (Of) and w.For
// is not over, says why and when it asks next, waits, and asks again: every
// ask is a real one, so a host's admission controller counts the demand
// that waits. It answers ask's last answer: nil once one gets room, any
// other answer at once, and the last no room when the bound or ctx ends
// the wait, after a line saying so. A no room made nothing, so nothing is
// held then.
func (w Wait) Do(ctx context.Context, ask func(context.Context) error) error {
	now, sleep := w.Now, w.Sleep
	if now == nil {
		now = time.Now
	}
	if sleep == nil {
		sleep = timer
	}
	start := now()
	backoff := firstAsk
	said, saidAt := "", time.Time{}
	// room is the last no room of this wait, as "<where> had no room: <why>"
	// ("": none yet). Every ask of one wait names the same target, so a
	// no room earlier means the target a later ask finds unlisted answered.
	room := ""
	for {
		err := ask(ctx)
		f, ok := Of(err)
		if w.For <= 0 {
			return err
		}
		// state is what the answer says: "<where> has no room: <why>", or
		// a target gone from the listing; ended is the wait's last line of it.
		var state, ended string
		switch name, gone := computeapi.NotListed(err); {
		case ok:
			where, why := cmp.Or(f.Where, "the pool"), cmp.Or(f.Why, "no reason given")
			room = where + " had no room: " + why
			state, ended = where+" has no room: "+why, where+" still has no room: "+why
		case gone && room != "":
			state = name + " " + notListedNow
			ended = state + "; before that, " + room
			gone := api.Errorf(api.CodeUnavailable, "%s %s; before that, %s", name, notListedNow, room).
				WithData(computeapi.TargetNotListed{Name: name})
			if e := (*api.Error)(nil); errors.As(err, &e) {
				gone = gone.WithHint(e.Hint) // where the targets are listed
			}
			err = gone
		default:
			return err
		}
		t := now()
		waited := t.Sub(start)
		switch {
		case ctx.Err() != nil:
			w.say("stopped waiting for room (nothing held): " + state)
			return err
		case waited >= w.For:
			w.say(fmt.Sprintf("gave up waiting for room after %s: %s", round(w.For), ended))
			return err
		}
		next := f.Retry
		if !ok || next <= 0 {
			next, backoff = backoff, min(2*backoff, backoffMax)
		}
		next = min(max(next, minAsk), maxAsk, w.For-waited) // the last ask comes at the bound
		if state != said || t.Sub(saidAt) >= sayAgain {
			w.say(fmt.Sprintf("%s; asking again at %s (waited %s of %s)", state, t.Add(next).Format("15:04:05"), round(waited), round(w.For)))
			said, saidAt = state, t
		}
		if sleep(ctx, next) != nil {
			w.say("stopped waiting for room (nothing held): " + state)
			return err
		}
	}
}

// notListedNow is what a target that answered earlier in a wait and is gone
// from the listing now is: not a target that does not exist.
const notListedNow = "is not listed now (it answered earlier in this wait; its host may be out of touch for a moment)"

func (w Wait) say(line string) {
	if w.Say != nil {
		w.Say(line)
	}
}

func timer(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// round is d as a person reads a wait: "45s", "4m", "1h30m".
func round(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Round(time.Second)/time.Second))
	}
	m := int(d.Round(time.Minute) / time.Minute)
	switch {
	case m < 60:
		return fmt.Sprintf("%dm", m)
	case m%60 == 0:
		return fmt.Sprintf("%dh", m/60)
	}
	return fmt.Sprintf("%dh%dm", m/60, m%60)
}
