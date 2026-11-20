package runtarget

import (
	"net/http"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
)

func (s *Service) release(r *http.Request) (any, error) {
	pid, b, db, rw, err := s.loadHandle(r, authz.Operate)
	if err != nil {
		return nil, err
	}
	ctx := r.Context()
	if err := holder(ctx, rw, true); err != nil {
		return nil, err
	}
	var req computeapi.ReleaseRunTargetRequest
	if err := api.DecodeJSON(r, &req); err != nil {
		return nil, err
	}
	args, err := baseRelease(ctx, rw, req)
	if err != nil {
		return nil, err
	}
	var ans struct {
		Released bool                       `json:"released"`
		Exit     *int                       `json:"exit"`
		Spend    *computeapi.RunTargetSpend `json:"spend"`
	}
	if err := s.call(ctx, b, verbRelease, rw.Handle, args, &ans); err != nil {
		return nil, err
	}
	d, _ := s.describeOf(ctx, b)
	// One status after the end for the final spend figure (SPEC 20), then one
	// write that ends the row, so the end hook sees the final figure once.
	u := upd{spend: s.unitSpend(d, ans.Spend), exit: ans.Exit}
	var st statusAnswer
	if err := s.call(ctx, b, verbStatus, rw.Handle, nil, &st); err == nil && driverStates[st.State] {
		if sp := s.unitSpend(d, st.Spend); sp != nil && (u.spend == nil || sp.Amount > u.spend.Amount) {
			u.spend = sp
		}
		if !ans.Released {
			u.state, u.publish = st.State, st.publishOf()
		}
	}
	if ans.Released {
		u.state, u.exit = stateReleased, nil
	}
	if err := s.record(ctx, pid, db, b.Name, rw.RequestID, u); err != nil {
		return nil, err
	}
	final, _, _ := byHandle(ctx, db, rw.Handle)
	out := computeapi.RunTargetRelease{Handle: rw.Handle, Released: ans.Released, Spend: final.Spend}
	if !ans.Released {
		out.Exit = ans.Exit
	}
	return out, nil
}
