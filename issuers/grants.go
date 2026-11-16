package issuers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/grants"
	"github.com/sengirunners/potluck/kit/core"
)

// Register hooks g to the grants package on d: grants checks grants and
// requests of g's kind through it (grants.ProvideIssuer), and a removed
// grant ends every credential issued under it (grants.OnGrantRemoved). Call
// it where the daemon constructs g, before it serves.
func Register(d *core.Deps, g *Minted) {
	grants.ProvideIssuer(d, g)
	grants.OnGrantRemoved(d, func(ctx context.Context, _, id string) {
		if err := g.EndGrant(ctx, id); err != nil && d.Log != nil {
			d.Log.Warn("secrets issuer: ending a removed grant's credentials", "grant", id, "err", err)
		}
	})
}

// ParamsFor is req's params as the grant check of kind reads them (each
// kind's Within): github-token's repository, permissions and reason,
// potluck-token's role.
func ParamsFor(kind string, req Request) (any, error) {
	switch kind {
	case KindGitHubToken:
		return ParamsOf(req), nil
	case KindPotluckToken:
		return PotluckParams{Role: req.Role}, nil
	}
	return nil, fmt.Errorf("issuers: no params for kind %q", kind)
}

// GrantCheck is Options.Check over grants.Check for an issuer of kind: the
// request as that kind, scope box, params ParamsFor, the box's host class
// from classOf (the source the issuer asks itself), and as the subject the
// request's Caller as authenticated (its cap included) for an identity, or
// the job. A refusal is the grants check's (recorded in the ledger there).
func GrantCheck(d *core.Deps, classOf ClassOf, kind string) Check {
	return func(ctx context.Context, req Request) (grants.Decision, error) {
		class, err := classOf(ctx, req.Box)
		if err != nil {
			return grants.Decision{}, err
		}
		p, err := ParamsFor(kind, req)
		if err != nil {
			return grants.Decision{}, err
		}
		params, err := json.Marshal(p)
		if err != nil {
			return grants.Decision{}, err
		}
		gr := grants.Request{Project: req.Box.Project, Area: req.Box.Area, Kind: kind,
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
