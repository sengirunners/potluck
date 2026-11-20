package owners

// GitHub grants (SPEC 22 "Join by GitHub"; owners-2026-11-01 Q11): a role
// in a project or area for a GitHub user, by numeric id, or for a GitHub
// org, whose active members qualify (checked live at join, never cached).
// No identity exists until the person joins.

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/store"
)

var grantMigrations = []store.Migration{
	{Kind: store.KindRegistry, Name: "owners/0002_github_grants", Script: `
CREATE TABLE github_grants (
	account    TEXT NOT NULL,
	login      TEXT NOT NULL,
	kind       TEXT NOT NULL,
	project    TEXT NOT NULL,
	area       TEXT NOT NULL DEFAULT '',
	role       TEXT NOT NULL,
	created    INTEGER NOT NULL,
	created_by TEXT NOT NULL DEFAULT '',
	PRIMARY KEY (account, project, area)
);
CREATE INDEX github_grants_project ON github_grants (project);`},
}

func (s *Service) grantRoutes(r *core.Router) {
	r.HandleFunc("POST /v0/grants/github", core.Authed, "admin on the project/area; the login resolved to its GitHub id now (or as the owner cache last heard it); when GitHub's rate limit refuses, queued and made after the reset", jsonH(s.handleGrant))
	r.HandleFunc("DELETE /v0/grants/github", core.Authed, "admin on the project/area", jsonH(s.handleRevoke))
	r.HandleFunc("GET /v0/grants/github", core.Authed, "admin on the project named by ?project=", jsonH(s.handleGrants))
}

// adminOf is the project (by ref) and area sub administers. A project or
// area sub cannot see answers like a missing one.
func (s *Service) adminOf(sub *authz.Subject, ref, area string) (api.Project, string, error) {
	p, err := s.d.Projects.Get(sub.Scope, ref)
	if err != nil {
		return p, "", err
	}
	area = strings.ToLower(strings.TrimSpace(area))
	if area != "" && (!s.d.Projects.HasArea(p.ID, area) || !sub.Can(authz.Read, authz.Object{Project: p.ID, Area: area})) {
		return p, "", api.NotFound("area", area)
	}
	return p, area, sub.Check(authz.Admin, authz.Object{Project: p.ID, Area: area}, "project", ref)
}

func loginArg(s string) string { return strings.TrimPrefix(strings.TrimSpace(s), "@") }

func (s *Service) handleGrant(r *http.Request) (any, error) {
	var req api.GitHubGrantRequest
	if err := api.DecodeJSON(r, &req); err != nil {
		return nil, err
	}
	return s.Grant(r.Context(), core.SubjectOf(r.Context()), req)
}

// Grant gives a GitHub user or org a role in a project or area, keyed to
// the account's numeric id as GitHub answers the login (or as the owner
// cache last heard it, within TTL). When GitHub's rate limit refuses, the
// grant is queued and finishes after the reset (grantqueue.go).
func (s *Service) Grant(ctx context.Context, sub *authz.Subject, req api.GitHubGrantRequest) (api.GitHubGrant, error) {
	rel, err := authz.ParseRelation(req.Role)
	if err != nil {
		return api.GitHubGrant{}, err
	}
	p, area, err := s.adminOf(sub, req.Project, req.Area)
	if err != nil {
		return api.GitHubGrant{}, err
	}
	acc, err := s.grantable(ctx, loginArg(req.Owner))
	var limited *RateLimited
	if errors.As(err, &limited) {
		return s.queueGrant(ctx, sub, api.ProjectRef{ID: p.ID, Slug: p.Slug}, area, rel, loginArg(req.Owner), limited)
	}
	if err != nil {
		return api.GitHubGrant{}, err
	}
	// The newer ask wins over one still queued: drop that first, so a worker
	// holding it cannot make it over this one (makeQueued claims the row).
	s.dequeueGrant(ctx, p.ID, area, loginArg(req.Owner))
	return s.putGrant(ctx, s.d.Store.Registry(), api.ProjectRef{ID: p.ID, Slug: p.Slug}, area, rel, acc, sub.Address)
}

// grantable is the account a grant to login goes to: the owner cache's if
// it holds a fresh answer for the login, else GitHub's. An org must also
// show its members to the authority's own token. A *RateLimited answer is
// returned as it is (the caller queues); every other failure is the
// answer the granter gets.
func (s *Service) grantable(ctx context.Context, login string) (Account, error) {
	acc, ok := s.cachedAccount(ctx, login)
	if !ok {
		var err error
		switch acc, err = s.acc.ByLogin(ctx, login); {
		case errors.Is(err, ErrNotFound):
			return Account{}, api.Errorf(api.CodeNotFound, "no GitHub account @%s", login)
		case isRateLimited(err):
			return Account{}, err
		case err != nil:
			return Account{}, providerDown(err)
		}
		s.seen(ctx, acc)
	}
	switch {
	case acc.Kind != KindUser && acc.Kind != KindOrg:
		return Account{}, api.Errorf(api.CodeForbidden, "@%s is a GitHub %s; grant a person or an org", acc.Login, acc.Kind)
	case acc.Kind == KindOrg:
		// Members are re-checked hourly with the authority's own token, so
		// it must see the org's members (it is a member's).
		switch _, err := s.acc.OrgMember(ctx, acc.Login, acc.Login); {
		case errors.Is(err, ErrNoView):
			return Account{}, api.Errorf(api.CodeForbidden, "this authority cannot see @%s's members, so it could not re-check them", acc.Login).
				WithHint(`an org grant needs github.lookup_token: a secret reference (file:, keychain: or env:; never the token itself in config.json) to a token of a member of @` + acc.Login + ` with scope read:org only; or, with no github.lookup_token, runners.github's App installed on @` + acc.Login + ` with Members read`)
		case errors.Is(err, ErrLookupRefused):
			return Account{}, api.Errorf(api.CodeUnavailable, "GitHub refused this authority's lookup token, so it could not check @%s's members", acc.Login).
				WithHint("try again: a token minted from runners.github's App is minted anew; a github.lookup_token GitHub refuses must be replaced")
		case isRateLimited(err):
			return Account{}, err
		case err != nil:
			return Account{}, providerDown(err)
		}
	}
	return acc, nil
}

func isRateLimited(err error) bool {
	var rl *RateLimited
	return errors.As(err, &rl)
}

// cachedAccount is the account login reached when GitHub was last asked,
// if that was within TTL and the account has not gone since and still
// holds login (a renamed-away login's redirect is not an answer for it).
// Never asks GitHub; a login cached as missing is not used (a new account
// would be refused for NegTTL).
func (s *Service) cachedAccount(ctx context.Context, login string) (Account, bool) {
	login = strings.ToLower(login)
	var acc Account
	var checked, accChecked int64
	var gone sql.NullInt64
	err := s.d.Store.Registry().QueryRowContext(ctx, `SELECT a.account, a.login, a.kind, l.checked, a.checked, a.gone
		FROM owner_logins l JOIN owner_accounts a ON a.account = l.account WHERE l.login = ? AND l.account <> ''`, login).
		Scan(&acc.ID, &acc.Login, &acc.Kind, &checked, &accChecked, &gone)
	if err != nil || gone.Valid || acc.Login == "" || acc.Kind == "" || !strings.EqualFold(acc.Login, login) || !s.fresh(checked) || !s.fresh(accChecked) {
		return Account{}, false
	}
	return acc, true
}

// execer is a database or a transaction.
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// putGrant records the grant of rel on acc in the project and area.
func (s *Service) putGrant(ctx context.Context, db execer, p api.ProjectRef, area string, rel authz.Relation, acc Account, by string) (api.GitHubGrant, error) {
	now := time.Now().UTC().Truncate(time.Millisecond)
	if _, err := db.ExecContext(ctx, `INSERT INTO github_grants (account, login, kind, project, area, role, created, created_by)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT (account, project, area) DO UPDATE SET role = excluded.role, login = excluded.login`,
		acc.ID, acc.Login, acc.Kind, p.ID, area, string(rel), store.Millis(now), by); err != nil {
		return api.GitHubGrant{}, err
	}
	return api.GitHubGrant{Account: acc.ID, Login: acc.Login, Kind: acc.Kind, Role: string(rel), Project: p.Slug, Area: area, Created: now}, nil
}

func (s *Service) handleRevoke(r *http.Request) (any, error) {
	var req api.GitHubGrantRequest
	if err := api.DecodeJSON(r, &req); err != nil {
		return nil, err
	}
	ctx := r.Context()
	sub := core.SubjectOf(ctx)
	p, area, err := s.adminOf(sub, req.Project, req.Area)
	if err != nil {
		return nil, err
	}
	// By the login it was granted under, its current login, or its id.
	login := loginArg(req.Owner)
	cancelled := s.dequeueGrant(ctx, p.ID, area, login) // a grant still waiting for GitHub's rate limit, and any made one below
	var account string
	err = s.d.Store.Registry().QueryRowContext(ctx, `SELECT account FROM github_grants WHERE project = ? AND area = ? AND
		(lower(login) = lower(?) OR account = ?)`, p.ID, area, login, req.Owner).Scan(&account)
	if err != nil {
		acc, ok := s.cachedAccount(ctx, login)
		if !ok {
			var e error
			acc, e = s.acc.ByLogin(ctx, login)
			ok = e == nil
		}
		if ok {
			err = s.d.Store.Registry().QueryRowContext(ctx, `SELECT account FROM github_grants WHERE project = ? AND area = ? AND account = ?`,
				p.ID, area, acc.ID).Scan(&account)
		}
	}
	if err != nil && cancelled {
		return api.GitHubRevoke{Revoked: true}, nil
	}
	if err != nil {
		return nil, api.Errorf(api.CodeNotFound, "no GitHub grant for @%s in %s", login, p.Slug+areaSuffix(area))
	}
	if _, err := s.d.Store.Registry().ExecContext(ctx, `DELETE FROM github_grants WHERE account = ? AND project = ? AND area = ?`, account, p.ID, area); err != nil {
		return nil, err
	}
	n, err := s.revokeAdmissions(ctx, account, p.ID, area, "the GitHub grant to @"+login+" was revoked by "+sub.Address)
	if err != nil {
		return nil, err
	}
	return api.GitHubRevoke{Revoked: true, Admissions: n}, nil
}

func areaSuffix(a string) string {
	if a == "" {
		return ""
	}
	return "/" + a
}

func (s *Service) handleGrants(r *http.Request) (any, error) {
	sub := core.SubjectOf(r.Context())
	p, _, err := s.adminOf(sub, r.URL.Query().Get("project"), "")
	if err != nil {
		return nil, err
	}
	rows, err := s.d.Store.Registry().QueryContext(r.Context(), `SELECT account, login, kind, area, role, created FROM github_grants
		WHERE project = ? ORDER BY area, lower(login)`, p.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := api.GitHubGrantList{Grants: []api.GitHubGrant{}}
	if out.Queued, err = s.queuedGrants(r.Context(), p); err != nil {
		return nil, err
	}
	for rows.Next() {
		g := api.GitHubGrant{Project: p.Slug}
		var at int64
		if err := rows.Scan(&g.Account, &g.Login, &g.Kind, &g.Area, &g.Role, &at); err != nil {
			return nil, err
		}
		g.Created = store.FromMillis(at)
		out.Grants = append(out.Grants, g)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	for i, g := range out.Grants {
		if g.Kind != KindUser {
			continue
		}
		if out.Grants[i].Ceilings, err = s.d.Projects.AccountCeilings(r.Context(), sub, g.Account, p.ID); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// memberships asks GitHub, live with the login's token, which of the
// orgs granted in the project ref names the person is an active member
// of, keyed by the org's account id: each org is looked up by id for its
// current login first, so a renamed org's members still match and a new
// org squatting its old login never does. Any error counts as "not a
// member" (the join's one refusal, never an outage that tells apart).
func (s *Service) memberships(ctx context.Context, ref, token string) map[string]bool {
	out := map[string]bool{}
	p, ok := s.d.Projects.Lookup(strings.TrimSpace(ref))
	if !ok {
		return out
	}
	rows, err := s.d.Store.Registry().QueryContext(ctx, `SELECT DISTINCT account FROM github_grants WHERE project = ? AND kind = ?`, p.ID, KindOrg)
	if err != nil {
		return out
	}
	var orgs []string
	for rows.Next() {
		var o string
		if rows.Scan(&o) == nil {
			orgs = append(orgs, o)
		}
	}
	rows.Close()
	for _, id := range orgs {
		org, err := s.acc.ByID(ctx, id)
		if err != nil || org.Kind != KindOrg {
			continue
		}
		s.renamed(ctx, org)
		if ok, err := s.acc.Member(ctx, token, org.Login); err == nil && ok {
			out[id] = true
		}
	}
	return out
}

// renamed records an account's current login on its GitHub grants.
func (s *Service) renamed(ctx context.Context, acc Account) {
	_, _ = s.d.Store.Registry().ExecContext(ctx, `UPDATE github_grants SET login = ? WHERE account = ? AND login <> ?`, acc.Login, acc.ID, acc.Login)
}

// grantKey names the GitHub grant an admission came from.
type grantKey struct{ account, kind, area string }

// grantFor is the strongest role acc holds in project pid through GitHub
// grants: its own (by id), or an org's it is an active member of (orgs,
// by org id, asked live when GitHub answered the login). A whole-project
// grant wins a tie. It answers the grant it came from.
//
// With scoped, only grants that cover area count: a whole-project grant
// covers any area, an area grant only its own (area "" then asks for the
// whole project, which only a whole-project grant covers).
func (s *Service) grantFor(ctx context.Context, pid string, acc Account, orgs map[string]bool, scoped bool, area string) (role string, from grantKey, err error) {
	rows, err := s.d.Store.Registry().QueryContext(ctx, `SELECT account, kind, area, role FROM github_grants WHERE project = ?`, pid)
	if err != nil {
		return "", grantKey{}, err
	}
	defer rows.Close()
	frozen := s.orgJoinsFrozen(ctx, pid)
	best := authz.None
	for rows.Next() {
		var g grantKey
		var rl string
		if err := rows.Scan(&g.account, &g.kind, &g.area, &rl); err != nil {
			return "", grantKey{}, err
		}
		if !(g.kind == KindUser && g.account == acc.ID) && !(g.kind == KindOrg && orgs[g.account] && !frozen) {
			continue
		}
		if scoped && g.area != "" && g.area != area {
			continue
		}
		r := authz.Relation(rl)
		if authz.Max(best, r) != best || (r == best && g.area == "" && from.area != "") {
			best, from = r, g
		}
	}
	return string(best), from, rows.Err()
}
