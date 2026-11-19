package owners

// Unlink (SPEC 22; owners-2026-11-01 Q2): the way back from a link nobody
// meant, such as a phished device code. The machine owner, an admin of a
// project the identity holds a grant in, or the identity's own replica
// removes the link; with revoke, the identity's node-bound tokens go too. The
// GitHub user then links again with a fresh device flow.

import (
	"context"
	"database/sql"
	"fmt"
	"github.com/sengirunners/potluck/kit/config"
	"net/http"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/project"
	"github.com/sengirunners/potluck/kit/store"
)

func (s *Service) handleUnlink(r *http.Request) (any, error) {
	return s.Unlink(r.Context(), core.SubjectOf(r.Context()), r.PathValue("identity"), r.URL.Query().Get("revoke") == "1")
}

// mayUnlink says whether sub may unlink identity, mirroring linkable: a
// credential that stands for a whole person (the machine owner on its
// socket, or the identity's own replica's node-bound token), or an admin
// of a project the identity holds a grant in. A minted token or a tailnet
// login of the identity itself is capped or ambient and may not (a
// viewer-capped CI token must never drop its human's link). Anyone else
// gets the same answer as a missing identity.
func (s *Service) mayUnlink(ctx context.Context, sub *authz.Subject, who api.Identity) bool {
	switch {
	case sub == nil:
		return false
	case sub.Via == authz.ViaLocal && sub.TokenID == "":
		return true
	case sub.Via == authz.ViaToken && sub.Node != "" && sub.Identity.ID == who.ID:
		return true
	}
	tuples, err := s.d.Projects.Tuples(ctx, who.ID)
	if err != nil {
		return false
	}
	for _, t := range tuples {
		if p, _, err := authz.ParseObject(t.Object); err == nil && sub.Can(authz.Admin, authz.Object{Project: p}) {
			return true
		}
	}
	return false
}

// Unlink removes identity's GitHub link (and, with revoke, its node-bound
// tokens), and tells the identity and the owners.
func (s *Service) Unlink(ctx context.Context, sub *authz.Subject, ref string, revoke bool) (api.GitHubUnlink, error) {
	who, err := s.d.Projects.Identity(ctx, ref)
	if err != nil || !s.mayUnlink(ctx, sub, who) {
		return api.GitHubUnlink{}, api.NotFound("identity", ref)
	}
	var account, login string
	_ = s.d.Store.Registry().QueryRowContext(ctx, `SELECT account, login FROM owner_links WHERE identity = ?`, who.ID).Scan(&account, &login)
	if account == "" {
		return api.GitHubUnlink{}, api.Errorf(api.CodeNotFound, "identity %s is not linked to GitHub", who.Name)
	}
	// The link goes only with the record that this identity was account,
	// so the account joining again as a new identity is still held by its
	// ceilings (project.RememberAccount).
	if err := store.Tx(ctx, s.d.Store.Registry(), func(tx *sql.Tx) error {
		if err := project.RememberAccount(ctx, tx, who.ID, account); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `DELETE FROM owner_links WHERE identity = ? AND account = ?`, who.ID, account)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil || n != 1 {
			return api.Errorf(api.CodeConflict, "identity %s's GitHub link changed while unlinking", who.Name).WithHint("try again")
		}
		return nil
	}); err != nil {
		return api.GitHubUnlink{}, err
	}
	out := api.GitHubUnlink{Identity: who.Name, Account: account, Login: login, Revoked: []string{}}
	if revoke {
		toks, err := s.d.Projects.Tokens(ctx)
		if err != nil {
			return out, err
		}
		for _, t := range toks {
			if t.Identity == who.Name && t.Node != "" && t.Revoked == nil {
				if _, err := s.d.Projects.RevokeToken(ctx, t.ID); err != nil {
					return out, err
				}
				out.Revoked = append(out.Revoked, t.ID)
			}
		}
	}
	s.tell(ctx, who, nil, fmt.Sprintf("Identity %s was unlinked from GitHub @%s (%s) on %s by %s; %d node-bound tokens revoked. "+
		"The GitHub user links again with %s, or %s.", who.Name, login, account, s.d.Config.Machine, sub.Address, len(out.Revoked), config.Command("login github"), config.Command("join", "<project>")))
	return out, nil
}
