package runtarget

import (
	"context"
	"database/sql"
	"slices"
	"time"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/events"
	"github.com/sengirunners/potluck/runtarget/rt"
)

// The deadline sweep (SPEC 20 "Lifetime", "Caps and spend"): a target ends
// its own resources at expires_at, and zm asks status once past it for the
// final state and spend, so every handle ends in zm's records (and the end
// hook fires) though nobody asks. It runs at start, for handles that passed
// their deadline while the daemon was down, and then every sweepEvery.
const (
	sweepEvery = 30 * time.Second
	sweepGrace = 5 * time.Second
	// lostAfter: past the deadline by this long and the driver still cannot
	// say how it ended, zm records it lost.
	lostAfter = 15 * time.Minute
)

var _ interface{ Start(context.Context) error } = (*Service)(nil)

// Start runs the sweep in the background.
func (s *Service) Start(ctx context.Context) error {
	s.warnUnpinned()
	// Moved to project ids now, while the joins are those of the upgrade
	// (teamlink started first), not at the first host call, which may come
	// after a new join under an old slug (hostkeys.go).
	_ = s.hostReady()
	go func() {
		s.Sweep(ctx)
		t := time.NewTicker(sweepEvery)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.Sweep(ctx)
			}
		}
	}()
	return nil
}

// Sweep settles every live handle past its deadline, and releases drifted
// boxes (booted off their pin, release failed at acquire), in every project.
// A binding whose driver fails once is skipped for the rest of the pass.
func (s *Service) Sweep(ctx context.Context) {
	now := s.now()
	for _, p := range s.d.Store.Projects(s.d.System().Scope, authz.Write) {
		failed := map[string]bool{}
		bindings := map[string]computeapi.RunTarget{}
		for _, b := range rt.ProjectTargets(s.d.Projects, p.ID) {
			bindings[b.Name] = b
		}
		s.settleCancelled(ctx, p.ID, p.DB, bindings) // acquires.go
		rows, err := liveRows(ctx, p.DB)
		if err != nil {
			continue
		}
		for _, rw := range rows {
			if ctx.Err() != nil {
				return
			}
			if rw.Handle == "" || failed[rw.Target] {
				continue
			}
			b, bound := bindings[rw.Target]
			if !bound {
				b, bound = remoteStub(p.ID, rw.Target) // a host's box: its verbs still go there
			}
			if bound && offPin(b, rw) {
				var rel struct {
					Released bool `json:"released"`
				}
				if err := s.call(ctx, b, verbRelease, rw.Handle, nil, &rel); err != nil {
					failed[rw.Target] = true
					continue
				}
				_ = s.record(ctx, p.ID, p.DB, rw.Target, rw.RequestID, upd{state: stateReleased})
				continue
			}
			exp, err := time.Parse(time.RFC3339Nano, rw.ExpiresAt)
			if err != nil { // no deadline from the driver: a day after the ask
				c, cerr := time.Parse(time.RFC3339Nano, rw.Created)
				if cerr != nil {
					continue
				}
				exp = c.Add(24 * time.Hour)
			}
			if now.Before(exp.Add(sweepGrace)) {
				if bound && s.renewHeld(ctx, p.DB, b, rw, exp, now) != nil {
					failed[rw.Target] = true // a slow or failing driver waits for the next pass
				}
				continue
			}
			if bound {
				if _, err := s.refresh(ctx, p.ID, b, p.DB, rw); err == nil {
					continue
				}
				failed[rw.Target] = true
			}
			if !bound || now.After(exp.Add(lostAfter)) {
				_ = s.record(ctx, p.ID, p.DB, rw.Target, rw.RequestID, upd{state: stateLost})
			}
		}
	}
}

// renewEvery is the most often zm renews one handle (SPEC 20 "Leases").
const renewEvery = time.Minute

// renewHeld renews a handle whose holder is live (core.Deps.HolderLive: the
// session module answers for a session, idle or not, since one waiting on a
// long run goes idle) once a third of its TTL is left, at most once a
// minute. A handle held by a person or a socket caller, by a holder that
// ended, or with no check registered (no session module) is not renewed:
// the target ends it at expires_at. Nor is one acquired with self_renew: its
// client renews it, so a client that dies lets it end at its TTL though the
// holder's session lives on.
func (s *Service) renewHeld(ctx context.Context, db *sql.DB, b computeapi.RunTarget, rw row, exp, now time.Time) error {
	if rw.SelfRenew || rw.TTLS <= 0 || rw.State == stateStopped || exp.Sub(now) > time.Duration(rw.TTLS)*time.Second/3 {
		return nil
	}
	a, err := api.ParseAddress(rw.Actor)
	if err != nil {
		return nil
	}
	if live, _ := s.d.HolderLive(ctx, a); !live {
		return nil
	}
	if last, err := time.Parse(time.RFC3339Nano, lastRenew(ctx, db, rw.Target, rw.RequestID)); err == nil && now.Sub(last) < renewEvery {
		return nil
	}
	d, err := s.describeOf(ctx, b)
	if err != nil {
		return err
	}
	if !slices.Contains(d.Verbs, verbRenew) {
		return nil
	}
	ttl := rw.TTLS
	if c := lifetimeCap(b, d); c > 0 {
		created, err := time.Parse(time.RFC3339Nano, capFrom(rw))
		if err != nil {
			return nil
		}
		// Up to the cap, not one TTL short of it.
		left := int(created.Add(time.Duration(c)*time.Second).Sub(now) / time.Second)
		if left <= 0 {
			return nil
		}
		ttl = min(ttl, left)
	}
	var ans struct {
		ExpiresAt string `json:"expires_at"`
	}
	at := now.UTC().Format(tsLayout)
	_ = setRenewed(ctx, db, rw.Target, rw.RequestID, at) // throttle even a refused renew
	if err := s.call(ctx, b, verbRenew, rw.Handle, map[string]any{"ttl_s": ttl}, &ans); err != nil {
		return err
	}
	_, err = update(ctx, db, rw.Target, rw.RequestID, upd{expiresAt: ans.ExpiresAt}, at)
	return err
}

// leaseExpired publishes SPEC 20's one signal for a handle that reached its
// deadline or was reclaimed: computeapi.EventRunTargetExpired, and nothing
// else (no mail: a run reaching its TTL is normal).
func (s *Service) leaseExpired(ctx context.Context, pid string, rw row) {
	if s.d.Events == nil {
		return
	}
	l := computeapi.RunTargetLease{Key: rw.Handle, Project: pid, Area: rw.Area, Holder: rw.Actor, Note: rw.State}
	if t, err := time.Parse(time.RFC3339Nano, rw.Created); err == nil {
		l.Claimed, l.Renewed = t, t
	}
	if t, err := time.Parse(time.RFC3339Nano, rw.ExpiresAt); err == nil {
		l.Expires = &t
	}
	if _, err := s.d.Events.Publish(ctx, events.Input{Type: computeapi.EventRunTargetExpired, Subject: rw.Handle, Project: pid,
		Area: rw.Area, Data: l, Mentions: []string{rw.Actor}}); err != nil && s.d.Log != nil {
		s.d.Log.Warn("runtarget: publish run_target.expired", "handle", rw.Handle, "err", err)
	}
}
