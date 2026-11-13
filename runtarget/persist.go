package runtarget

// Kept disks (SPEC 20 "Boxes", Kept disks; zm-eakh.12.5.23;
// trusted hosts only). A box acquired with
// persist keeps its disk when it stops: stop ends what runs in it and keeps
// the disk until expires_at (the driver's keep, 30 days by default), start
// boots that same disk on the same host with a new lease, and release
// deletes it. A stopped box holds no lease time: its row stays live (it
// counts against handles_max) and the sweep settles it when the disk's
// deadline passes. Only a trusted host keeps a disk: a public host could
// read it.

import (
	"context"
	"database/sql"
	"net/http"
	"slices"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
)

const (
	verbStop     = "stop"
	verbStart    = "start"
	stateStopped = computeapi.StateStopped
)

// checkPersist refuses an acquire's persist where no disk may be kept.
func (s *Service) checkPersist(ctx context.Context, b computeapi.RunTarget, d *computeapi.RunTargetDescribe, req computeapi.AcquireRunTargetRequest) error {
	if !req.Persist {
		return nil
	}
	switch {
	case b.Kind != computeapi.RunTargetKindBox:
		return api.Errorf(api.CodeBadRequest, "persist is for a box target")
	case req.JITConfig != "":
		return api.Errorf(api.CodeBadRequest, "a GitHub runner box is never persistent: it serves one job and is wiped")
	case !slices.Contains(d.Capabilities, computeapi.CapPersist):
		return api.Errorf(api.CodeBadRequest, "run target %s keeps no disks (potluck-driver-%s lists no persist capability)", b.Name, b.Driver)
	}
	return s.hostTrusted(ctx, b, "kept disk (persist)")
}

// capFrom is when a handle's lifetime cap counts from: its acquire, or the
// last start of a persistent box.
func capFrom(rw row) string {
	if rw.Started != "" {
		return rw.Started
	}
	return rw.Created
}

// persistentBox is the path's handle, a box acquired with persist.
func (s *Service) persistentBox(r *http.Request, ownerToo bool) (string, computeapi.RunTarget, *sql.DB, row, error) {
	pid, b, db, rw, err := s.loadHandle(r, authz.Operate)
	if err != nil {
		return "", b, nil, rw, err
	}
	switch {
	case b.Kind != computeapi.RunTargetKindBox:
		return "", b, nil, rw, api.Errorf(api.CodeBadRequest, "%s is a %s target: only a box stops and starts", b.Name, b.Kind)
	case holder(r.Context(), rw, ownerToo) != nil:
		return "", b, nil, rw, holder(r.Context(), rw, ownerToo)
	case !rw.Persist:
		return "", b, nil, rw, api.Errorf(api.CodeBadRequest, "sandbox %s was created without persist: it has no disk to keep", sandboxID(rw.Handle)).
			WithHint("zm sandbox rm ends it; zm sandbox create --persist makes a box that stops and starts")
	}
	return pid, b, db, rw, nil
}

// stop stops a persistent box, keeping its disk (the holder's, or the
// machine owner's). A stopped box answers as it is.
func (s *Service) stop(r *http.Request) (any, error) {
	pid, b, db, rw, err := s.persistentBox(r, true)
	if err != nil {
		return nil, err
	}
	switch rw.State {
	case stateStopped:
		return rw.RunTargetHandle, nil
	case stateReady:
	default:
		return nil, api.Errorf(api.CodeConflict, "sandbox %s is %s", sandboxID(rw.Handle), rw.State)
	}
	ctx := r.Context()
	var ans struct {
		Stopped   bool                       `json:"stopped"`
		ExpiresAt string                     `json:"expires_at"`
		Spend     *computeapi.RunTargetSpend `json:"spend"`
	}
	if err := s.call(context.WithoutCancel(ctx), b, verbStop, rw.Handle, nil, &ans); err != nil {
		return nil, err
	}
	if !ans.Stopped {
		return nil, api.Errorf(api.CodeInternal, "potluck-driver-%s did not stop %s", b.Driver, rw.Handle)
	}
	return s.recordAs(ctx, pid, b, db, rw, upd{state: stateStopped, expiresAt: ans.ExpiresAt}, ans.Spend, "")
}

// start boots a stopped box's kept disk again on the host that keeps it,
// with a new lease (the holder's alone).
func (s *Service) start(r *http.Request) (any, error) {
	pid, b, db, rw, err := s.persistentBox(r, false)
	if err != nil {
		return nil, err
	}
	var req computeapi.StartRunTargetRequest
	if err := api.DecodeJSON(r, &req); err != nil {
		return nil, err
	}
	ctx := r.Context()
	// The row may lag the driver (a lease's end stops a box before the sweep
	// asks): what the driver says now decides.
	if cur, err := s.refresh(ctx, pid, b, db, rw); err == nil {
		rw.RunTargetHandle = cur
	}
	switch {
	case req.TTLS < 0:
		return nil, api.Errorf(api.CodeBadRequest, "ttl_s must not be negative")
	case rw.State == stateReady:
		return rw.RunTargetHandle, nil
	case rw.State != stateStopped:
		return nil, api.Errorf(api.CodeConflict, "sandbox %s is %s: its disk is gone", sandboxID(rw.Handle), rw.State)
	}
	// A host demoted to public since the stop keeps the disk, but gets no box
	// on it: release deletes it.
	if err := s.hostTrusted(ctx, b, "kept disk (persist)"); err != nil {
		return nil, err
	}
	d, err := s.describeOf(ctx, b)
	if err != nil {
		return nil, err
	}
	if c := lifetimeCap(b, d); c > 0 && req.TTLS > c {
		return nil, api.Errorf(api.CodeBadRequest, "ttl_s %d is past the lifetime cap %d", req.TTLS, c)
	}
	// A start is a new lease: the binding's budget and the area's minute limit apply, as to an acquire.
	if err := s.budgetAllowed(ctx, pid, b, db); err != nil {
		return nil, err
	}
	if err := s.boxAllowed(ctx, pid, rw.Area, db); err != nil { // ends.go
		return nil, err
	}
	var args any
	if req.TTLS > 0 {
		args = map[string]any{"ttl_s": req.TTLS}
	}
	var ans acquireAnswer
	// Booting outlives a caller that goes away, as an acquire does.
	if err := s.callWithin(context.WithoutCancel(ctx), b, verbStart, rw.Handle, args, &ans, acquireLimit); err != nil {
		return nil, err
	}
	now := s.now().UTC().Format(tsLayout)
	return s.recordAs(ctx, pid, b, db, rw, upd{state: stateReady, expiresAt: ans.ExpiresAt, ttl: ans.TTLS}, ans.Spend, now)
}

// recordAs writes a stop's or a start's answer (started: the start's time,
// with the spend figure as of it: what the box ran before this start) and
// answers the row.
func (s *Service) recordAs(ctx context.Context, pid string, b computeapi.RunTarget, db *sql.DB, rw row, u upd, sp *computeapi.RunTargetSpend, started string) (any, error) {
	ctx = context.WithoutCancel(ctx)
	if sp != nil {
		d, _ := s.describeOf(ctx, b)
		u.spend = s.unitSpend(d, sp)
	}
	if err := s.record(ctx, pid, db, b.Name, rw.RequestID, u); err != nil {
		return nil, err
	}
	if started != "" {
		if _, err := db.ExecContext(ctx, `UPDATE run_handles SET started=?, started_spend=spend_amount WHERE target=? AND request_id=?`, started, b.Name, rw.RequestID); err != nil {
			return nil, err
		}
	}
	out, _, err := byHandle(ctx, db, rw.Handle)
	return out.RunTargetHandle, err
}
