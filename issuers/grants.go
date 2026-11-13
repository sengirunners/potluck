package issuers

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/grants"
	"github.com/sengirunners/potluck/kit/core"
)

// Register hooks g to the grants package on d: grants checks github-token
// grants and requests through it (grants.ProvideIssuer), and a removed
// grant ends every token issued under it (grants.OnGrantRemoved). Call it
// where the daemon constructs g, before it serves.
func Register(d *core.Deps, g *GitHubToken) {
	grants.ProvideIssuer(d, g)
	grants.OnGrantRemoved(d, func(ctx context.Context, _, id string) {
		if err := g.EndGrant(ctx, id); err != nil && d.Log != nil {
			d.Log.Warn("secrets issuer: ending a removed grant's tokens", "grant", id, "err", err)
		}
	})
}

// GrantCheck is Options.Check over grants.Check: the request as kind
// github-token, scope box, params Request's own JSON, the box's host class
// from classOf (the source the issuer asks itself), and as the subject the
// request's Caller as authenticated (its cap included) for an identity, or
// the job. A refusal is the grants check's (recorded in the ledger there).
func GrantCheck(d *core.Deps, classOf ClassOf) Check {
	return func(ctx context.Context, req Request) (grants.Decision, error) {
		class, err := classOf(ctx, req.Box)
		if err != nil {
			return grants.Decision{}, err
		}
		params, err := json.Marshal(ParamsOf(req))
		if err != nil {
			return grants.Decision{}, err
		}
		gr := grants.Request{Project: req.Box.Project, Area: req.Box.Area, Kind: computeapi.SecretKindGitHubToken,
			Scope: computeapi.SecretScopeBox, Verb: "issue", Box: req.Box.ID, Host: req.Box.Host, HostClass: class,
			Name: req.Name, Params: params, Reason: req.Reason, Job: req.Subject.Job}
		if req.Subject.Job == nil {
			c := req.Caller
			if c == nil || c.Identity.ID == "" || c.Identity.ID != req.Subject.IdentityID {
				return grants.Decision{}, errors.New("no authenticated caller for this identity's credential")
			}
			gr.Subject = c
		}
		return grants.Check(ctx, d, gr)
	}
}
