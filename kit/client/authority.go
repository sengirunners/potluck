package client

// Owned by the HQ routes (kit/api/authority.go). One method per route.
//
// None of these routes is ever queued: a member forwards a call to HQ (or to
// the parked machine it is for) or fails it, so a 200 is the only success
// (a 200 is not always "it started": see api.MoveAnswer).
// Do turns a replica's 202 queued answer into an *api.Error with
// api.CodeQueued, so a press a member queued by mistake reads as an error,
// never as done. A refusal is an *api.Error; api.HQRefusalReason reads its
// reason (api.ReasonStale on a 409 when the screen is out of date,
// api.ReasonNotOwner on a 403, api.ReasonNoEngine on a 503). A request
// missing a field its Check requires is refused here and never sent.

import (
	"context"
	"net/http"

	"github.com/sengirunners/potluck/kit/api"
)

// AuthorityTerm is GET /v0/authority/term: any daemon's state and epoch.
func (c *Client) AuthorityTerm(ctx context.Context) (api.AuthorityTerm, error) {
	var out api.AuthorityTerm
	return out, c.Do(ctx, http.MethodGet, api.TermPath, nil, &out)
}

// FreezeHome is POST /v0/authority/freeze (this machine's owner, on a
// serving daemon): the home stops for a move at its epoch, and its daemon
// stops after the answer (its next start is parked).
func (c *Client) FreezeHome(ctx context.Context) (api.AuthorityTerm, error) {
	var out api.AuthorityTerm
	return out, c.Do(ctx, http.MethodPost, "/v0/authority/freeze", struct{}{}, &out)
}

// UnfreezeHome is POST /v0/authority/unfreeze (this machine's owner, on a
// parked daemon of a frozen home): the home serves again at the same
// epoch from its daemon's next start; this daemon stops after the answer.
func (c *Client) UnfreezeHome(ctx context.Context) (api.AuthorityTerm, error) {
	var out api.AuthorityTerm
	return out, c.Do(ctx, http.MethodPost, "/v0/authority/unfreeze", struct{}{}, &out)
}

// Authority is GET /v0/authority: HQ, its standby and any move.
func (c *Client) Authority(ctx context.Context) (api.AuthorityStatus, error) {
	var out api.AuthorityStatus
	return out, c.Do(ctx, http.MethodGet, api.AuthorityPath, nil, &out)
}

// AuthorityCandidates is GET /v0/authority/candidates.
func (c *Client) AuthorityCandidates(ctx context.Context) (api.CandidateList, error) {
	var out api.CandidateList
	return out, c.Do(ctx, http.MethodGet, api.CandidatesPath, nil, &out)
}

// SetStandby is POST /v0/authority/standby.
func (c *Client) SetStandby(ctx context.Context, req api.StandbyRequest) (api.AuthorityStatus, error) {
	var out api.AuthorityStatus
	if err := req.Check(); err != nil {
		return out, err
	}
	return out, c.Do(ctx, http.MethodPost, api.StandbyPath, req, &out)
}

// ReleaseStandby is POST /v0/authority/standby/release.
func (c *Client) ReleaseStandby(ctx context.Context, req api.StandbyRequest) (api.AuthorityStatus, error) {
	var out api.AuthorityStatus
	if err := req.Check(); err != nil {
		return out, err
	}
	return out, c.Do(ctx, http.MethodPost, api.StandbyReleasePath, req, &out)
}

// MoveHQ is POST /v0/authority/move: the preflight, and with req.CheckOnly
// false the move it starts. A nil error with out.Move nil means nothing
// started (check only, or a check failed: out.Preflight says which).
func (c *Client) MoveHQ(ctx context.Context, req api.MoveRequest) (api.MoveAnswer, error) {
	var out api.MoveAnswer
	if err := req.Check(); err != nil {
		return out, err
	}
	return out, c.Do(ctx, http.MethodPost, api.MovePath, req, &out)
}

// AbortMove is POST /v0/authority/move/abort (at the parked old HQ).
func (c *Client) AbortMove(ctx context.Context, req api.MoveControl) (api.HomeMove, error) {
	var out api.HomeMove
	if err := req.Check(); err != nil {
		return out, err
	}
	return out, c.Do(ctx, http.MethodPost, api.MoveAbortPath, req, &out)
}

// ResumeMove is POST /v0/authority/move/resume (at the parked old HQ).
func (c *Client) ResumeMove(ctx context.Context, req api.MoveControl) (api.HomeMove, error) {
	var out api.HomeMove
	if err := req.Check(); err != nil {
		return out, err
	}
	return out, c.Do(ctx, http.MethodPost, api.MoveResumePath, req, &out)
}

// PromoteStandby is POST /v0/authority/promote (at the parked standby).
func (c *Client) PromoteStandby(ctx context.Context, req api.PromoteRequest) (api.HomeMove, error) {
	var out api.HomeMove
	if err := req.Check(); err != nil {
		return out, err
	}
	return out, c.Do(ctx, http.MethodPost, api.PromotePath, req, &out)
}

// RetireHQ is POST /v0/authority/retire (at the fenced, parked old HQ).
func (c *Client) RetireHQ(ctx context.Context, req api.RetireRequest) (api.HomeMove, error) {
	var out api.HomeMove
	if err := req.Check(); err != nil {
		return out, err
	}
	return out, c.Do(ctx, http.MethodPost, api.RetirePath, req, &out)
}
