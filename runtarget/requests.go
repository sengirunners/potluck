package runtarget

import (
	"context"
	"database/sql"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/kit/api"
)

// boxRequestFree refuses a box acquire on target whose request id (actor
// keyed) already names a box acquire on another of the project's bindings.
// A sandbox create walks the ranked bindings with one request id
// (computeclient.AcquireBox) and a retry starts the walk again: it is sent to
// the binding that holds the request (conflict, data used_on), never answered
// with a second box here. Only an ended box of a binding since removed frees
// the id: it answers nothing. hidden is the caller's view of another's row
// (it answers as missing). The caller holds the request id's lock
// (Service.requestMu), so no other binding records it meanwhile.
func (s *Service) boxRequestFree(ctx context.Context, project string, db *sql.DB, target, requestID string, hidden func(row) bool) error {
	rows, err := usedElsewhere(ctx, db, target, computeapi.RunTargetKindBox, requestID)
	if err != nil {
		return err
	}
	for _, o := range rows {
		if o.Ended != "" {
			if _, _, err := s.bindingNamed(ctx, project, o.Target); err != nil {
				continue
			}
		}
		if hidden(o) {
			return api.NotFound("handle", requestID)
		}
		return api.Errorf(api.CodeConflict, "request_id %s was used for a box on run target %s", requestID, o.Target).
			WithHint("ask " + o.Target + " with it: it answers what it made").WithData(computeapi.AcquireUsedOn{Target: o.Target})
	}
	return nil
}
