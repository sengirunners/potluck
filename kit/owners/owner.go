package owners

// GET /v0/owner (SPEC 16 "Owners decide"): whether the caller's identity owns
// or administers a project, judged as POST /v0/asks/{id}/choose judges it
// (core.Deps.ProjectAdmin), and the GitHub login it is linked to. A client
// shows decision buttons only when the owner path will take the answer, and
// offers Sign in with GitHub when the identity is not linked.

import (
	"context"
	"net/http"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/core"
)

func (s *Service) handleOwner(r *http.Request) (any, error) {
	return s.Owner(r.Context(), core.SubjectOf(r.Context()), r.URL.Query().Get("project"))
}

// Owner reports sub's owner right on the project ref names; a project sub
// cannot see is the 404 of a missing one.
func (s *Service) Owner(ctx context.Context, sub *authz.Subject, ref string) (api.OwnerStatus, error) {
	if sub == nil {
		return api.OwnerStatus{}, api.Errorf(api.CodeUnauthenticated, "authentication required")
	}
	if ref == "" {
		return api.OwnerStatus{}, api.Errorf(api.CodeBadRequest, "?project= is required")
	}
	p, err := s.d.Projects.Get(sub.Scope, ref)
	if err != nil {
		return api.OwnerStatus{}, err
	}
	owner, err := s.d.ProjectAdmin(ctx, sub, p.ID)
	if err != nil {
		return api.OwnerStatus{}, err
	}
	out := api.OwnerStatus{Project: p.Slug, Identity: sub.Identity.Name, Owner: owner}
	if sub.Identity.ID != "" {
		out.GitHub = s.GitHubLogin(ctx, sub.Identity.ID)
	}
	return out, nil
}

// GitHubLogin is the login identity is linked to, or "" (core.Deps.GitHubLogin).
func (s *Service) GitHubLogin(ctx context.Context, identity string) string {
	var login string
	_ = s.d.Store.Registry().QueryRowContext(ctx, `SELECT login FROM owner_links WHERE identity = ?`, identity).Scan(&login)
	return login
}

// LinkedAccount is the account ("github:<numeric id>") and login identity is
// linked to, or "", "" (core.Deps.LinkedAccount). The link was proved by the
// device flow at this authority (owner_links); one account may own several
// identities, each linked to one account.
func (s *Service) LinkedAccount(ctx context.Context, identity string) (account, login string) {
	_ = s.d.Store.Registry().QueryRowContext(ctx, `SELECT account, login FROM owner_links WHERE identity = ?`, identity).Scan(&account, &login)
	return account, login
}
