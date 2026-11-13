package runtarget

import (
	"context"
	"database/sql"
	"time"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/runtarget/rt"
)

// EndedSince implements rt.Service: project's handles matching only ended
// at or after since, oldest end first.
func (s *Service) EndedSince(ctx context.Context, projectID string, only rt.EndedOnly, since time.Time, limit int) ([]computeapi.RunTargetHandle, error) {
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}
	db, err := s.d.Store.Project(s.d.System().Scope, authz.Read, projectID)
	if err != nil {
		return nil, err
	}
	rows, err := endedRows(ctx, db, only, since.UTC().Format(tsLayout), limit)
	if err != nil {
		return nil, err
	}
	out := make([]computeapi.RunTargetHandle, 0, len(rows))
	for _, rw := range rows {
		out = append(out, rw.RunTargetHandle)
	}
	return out, nil
}

// OnEnd subscribes f to every handle's end (rt.Service).
func (s *Service) OnEnd(f rt.EndFunc) {
	s.endMu.Lock()
	defer s.endMu.Unlock()
	s.ends = append(s.ends, f)
}

// OnBoxAcquire subscribes f to every new box acquire (rt.Service).
func (s *Service) OnBoxAcquire(f rt.BoxAcquireFunc) {
	s.endMu.Lock()
	defer s.endMu.Unlock()
	s.boxChecks = append(s.boxChecks, f)
}

// boxAllowed asks every OnBoxAcquire subscriber about a new box in area (an
// acquire, or the start of a stopped box); a subscriber that asks for the
// area's live boxes gets every binding's. The first refusal answers; live
// boxes that cannot be read refuse too. Only acquires on one binding are
// serialised (starts take no lock), and a box still being acquired counts
// nothing yet, so two at one moment may both pass: a limit is a fair-share
// ceiling, not an exact one.
func (s *Service) boxAllowed(ctx context.Context, project, area string, db *sql.DB) error {
	s.endMu.Lock()
	fs := append([]rt.BoxAcquireFunc(nil), s.boxChecks...)
	s.endMu.Unlock()
	live := func() ([]computeapi.RunTargetHandle, error) {
		rows, err := liveBoxes(ctx, db, area)
		if err != nil {
			return nil, api.Errorf(api.CodeUnavailable, "the live sandboxes of area %s could not be read, so no new sandbox there: %v", area, err)
		}
		out := make([]computeapi.RunTargetHandle, 0, len(rows))
		for _, rw := range rows {
			out = append(out, rw.RunTargetHandle)
		}
		return out, nil
	}
	for _, f := range fs {
		if err := f(ctx, project, area, live); err != nil {
			return err
		}
	}
	return nil
}

// ended calls every OnEnd subscriber, each on its own goroutine (a panic is
// recovered and logged), once per handle.
func (s *Service) ended(ctx context.Context, project string, h computeapi.RunTargetHandle) {
	s.forgetBox(ctx, project, h.Handle)
	s.endMu.Lock()
	fs := append([]rt.EndFunc(nil), s.ends...)
	s.endMu.Unlock()
	for _, f := range fs {
		go func() {
			defer func() {
				if r := recover(); r != nil && s.d.Log != nil {
					s.d.Log.Error("run target end hook panicked", "handle", h.Handle, "panic", r)
				}
			}()
			f(ctx, project, h)
		}()
	}
}
