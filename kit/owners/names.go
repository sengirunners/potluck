package owners

// Owner names (SPEC 22; owners-2026-11-01 "The rule: a name is never
// rebound"). Two maps, both append-only, carry every answer:
//
//   - owner_names (owner id, name) -> project: a binding is never moved or
//     removed (triggers refuse it); renames and transfers add one.
//   - login_paths (login, name) -> owner id: every @login/name this
//     authority ever bound or answered is pinned to the owner it reached.
//     Resolving a pinned path follows the pin, whoever holds the login on
//     GitHub now: when Judy renames judy -> jd and GitHub later gives
//     "judy" to a newcomer, @judy/acme still reaches Judy's
//     project (moved: @jd/acme), and the newcomer cannot bind it.
//
// owner_logins and owner_accounts are only a cache of what GitHub said
// (login -> id, id -> login and whether GitHub still answers for it); a
// login's row is the one-hop redirect while GitHub answers 404 for it.
//
// A binding is made by a GitHub login the authority runs (purpose bind,
// login.go): the approving account must be the binder's linked one, and
// the owner that account or an org it is an active member of (asked live
// with the login's token, never cached).

import (
	"context"
	"database/sql"
	"errors"
	"github.com/sengirunners/potluck/kit/config"
	"net/http"
	"strings"
	"time"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/store"
)

var nameMigrations = []store.Migration{
	{Kind: store.KindRegistry, Name: "owners/0004_names", Script: `
CREATE TABLE owner_accounts (
	account TEXT PRIMARY KEY,
	login   TEXT NOT NULL,
	kind    TEXT NOT NULL,
	checked INTEGER NOT NULL,
	gone    INTEGER
);
CREATE TABLE owner_logins (
	login   TEXT PRIMARY KEY,
	account TEXT NOT NULL, -- '' : GitHub answered 404 and no account ever held it here (a negative cache)
	checked INTEGER NOT NULL
);
CREATE TABLE owner_names (
	owner   TEXT NOT NULL,
	name    TEXT NOT NULL,
	project TEXT NOT NULL,
	bound   INTEGER NOT NULL,
	current INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (owner, name)
);
CREATE INDEX owner_names_project ON owner_names (project);
CREATE TRIGGER owner_names_never_rebound BEFORE UPDATE OF owner, name, project ON owner_names
BEGIN SELECT RAISE(ABORT, 'owner_names: a name is never rebound'); END;
CREATE TRIGGER owner_names_never_removed BEFORE DELETE ON owner_names
BEGIN SELECT RAISE(ABORT, 'owner_names: a name is never removed'); END;
CREATE TRIGGER owner_names_never_replaced BEFORE INSERT ON owner_names
WHEN EXISTS (SELECT 1 FROM owner_names WHERE owner = NEW.owner AND name = NEW.name)
BEGIN SELECT RAISE(ABORT, 'owner_names: a name is never rebound'); END;
CREATE TABLE login_paths (
	login TEXT NOT NULL,
	name  TEXT NOT NULL,
	owner TEXT NOT NULL,
	PRIMARY KEY (login, name)
);
CREATE TRIGGER login_paths_never_rebound BEFORE UPDATE ON login_paths
BEGIN SELECT RAISE(ABORT, 'login_paths: a path is never rebound'); END;
CREATE TRIGGER login_paths_never_removed BEFORE DELETE ON login_paths
BEGIN SELECT RAISE(ABORT, 'login_paths: a path is never removed'); END;
CREATE TRIGGER login_paths_never_replaced BEFORE INSERT ON login_paths
WHEN EXISTS (SELECT 1 FROM login_paths WHERE login = NEW.login AND name = NEW.name)
BEGIN SELECT RAISE(ABORT, 'login_paths: a path is never rebound'); END;`},
}

// callerKey keys a caller's lookup budget: its identity (or its address).
func callerKey(sub *authz.Subject) string {
	if sub == nil {
		return ""
	}
	if sub.Identity.ID != "" {
		return sub.Identity.ID
	}
	return sub.Address
}

// pinSQL pins a path unless it is pinned already (an insert over an
// existing row, REPLACE included, is refused by a trigger).
const pinSQL = `INSERT INTO login_paths (login, name, owner) SELECT ?, ?, ?
	WHERE NOT EXISTS (SELECT 1 FROM login_paths WHERE login = ? AND name = ?)`

func ref(login, name string) string { return "@" + login + "/" + name }

// seen records what GitHub just said about acc: its current login, and
// that the login is acc's now (which drops another account's redirect).
func (s *Service) seen(ctx context.Context, acc Account) {
	now := store.Millis(s.now())
	db := s.d.Store.Registry()
	if _, err := db.ExecContext(ctx, `INSERT INTO owner_accounts (account, login, kind, checked) VALUES (?, ?, ?, ?)
		ON CONFLICT (account) DO UPDATE SET login = excluded.login, kind = excluded.kind, checked = excluded.checked, gone = NULL`,
		acc.ID, acc.Login, acc.Kind, now); err != nil {
		s.d.Log.Warn("owners: caching an account", "err", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO owner_logins (login, account, checked) VALUES (?, ?, ?)
		ON CONFLICT (login) DO UPDATE SET account = excluded.account, checked = excluded.checked`,
		strings.ToLower(acc.Login), acc.ID, now); err != nil {
		s.d.Log.Warn("owners: caching a login", "err", err)
	}
}

func (s *Service) fresh(checked int64) bool {
	return s.now().Sub(store.FromMillis(checked)) < s.TTL
}

// owner is the account login reaches now: GitHub's answer (cached for
// TTL; a login GitHub does not know is cached as missing for NegTTL), else
// the one-hop redirect of a login GitHub no longer answers. With GitHub
// away, a cached answer stands and anything else answers like a missing
// login (never an outage that tells a known login from an unseen one).
func (s *Service) owner(ctx context.Context, login string) (string, error) {
	login = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(login), "@"))
	missing := api.NotFound("owner", "@"+login)
	acct, checked, cached := s.cachedOwner(ctx, login)
	switch {
	case cached && acct == "" && s.now().Sub(store.FromMillis(checked)) < s.NegTTL:
		return "", missing
	case cached && acct != "" && s.fresh(checked):
		return acct, nil
	}
	a, err := s.acc.ByLogin(ctx, login)
	switch {
	case err == nil:
		s.seen(ctx, a)
		return a.ID, nil
	case errors.Is(err, ErrNotFound) && acct != "":
		// Renamed away and not reassigned: one hop to the same account.
		_, _ = s.d.Store.Registry().ExecContext(ctx, `UPDATE owner_logins SET checked = ? WHERE login = ?`, store.Millis(s.now()), login)
		return acct, nil
	case errors.Is(err, ErrNotFound):
		_, _ = s.d.Store.Registry().ExecContext(ctx, `INSERT INTO owner_logins (login, account, checked) VALUES (?, '', ?)
			ON CONFLICT (login) DO UPDATE SET checked = excluded.checked WHERE owner_logins.account = ''`, login, store.Millis(s.now()))
		return "", missing
	case acct != "":
		return acct, nil
	}
	s.d.Log.Warn("owners: GitHub did not answer a login lookup", "err", err)
	return "", missing
}

// cachedOwner is what owner_logins holds for login (account "" is a
// cached 404).
func (s *Service) cachedOwner(ctx context.Context, login string) (acct string, checked int64, ok bool) {
	err := s.d.Store.Registry().QueryRowContext(ctx, `SELECT account, checked FROM owner_logins WHERE login = ?`, login).Scan(&acct, &checked)
	return acct, checked, err == nil
}

// lookupAllowed spends one of key's GitHub lookups this minute (resolving
// names nobody resolved lately costs GitHub calls, which are the
// authority's quota for binds and refreshes too).
func (s *Service) lookupAllowed(key string) bool {
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	recent := s.lookups[key][:0]
	for _, t := range s.lookups[key] {
		if now.Sub(t) < time.Minute {
			recent = append(recent, t)
		}
	}
	if len(recent) >= s.MaxLookups {
		s.lookups[key] = recent
		return false
	}
	s.lookups[key] = append(recent, now)
	return true
}

// cachedLogin is account's login as last seen, never asking GitHub.
func (s *Service) cachedLogin(ctx context.Context, account string) string {
	var login string
	if s.d.Store.Registry().QueryRowContext(ctx, `SELECT login FROM owner_accounts WHERE account = ?`, account).Scan(&login) != nil || login == "" {
		return account
	}
	return login
}

// markGone records that GitHub no longer answers for account by id: its
// names are frozen until it answers again.
func (s *Service) markGone(ctx context.Context, account string) {
	now := store.Millis(s.now())
	if _, err := s.d.Store.Registry().ExecContext(ctx, `INSERT INTO owner_accounts (account, login, kind, checked, gone) VALUES (?, '', '', ?, ?)
		ON CONFLICT (account) DO UPDATE SET checked = excluded.checked, gone = coalesce(owner_accounts.gone, excluded.gone)`,
		account, now, now); err != nil {
		s.d.Log.Warn("owners: recording a gone account", "err", err)
	}
}

// alive asks GitHub now whether it answers for account by id (a frozen
// owner is one it does not: suspended or deleted).
func (s *Service) alive(ctx context.Context, account string) (bool, error) {
	a, err := s.acc.ByID(ctx, account)
	switch {
	case err == nil:
		s.seen(ctx, a)
		return true, nil
	case errors.Is(err, ErrNotFound):
		s.markGone(ctx, account)
		return false, nil
	}
	return false, providerDown(err)
}

// state is account's current login and whether it is frozen: cached for
// TTL, else asked by id; with GitHub away, the cached answer stands.
func (s *Service) state(ctx context.Context, account string) (login string, frozen bool) {
	var checked int64
	var gone sql.NullInt64
	err := s.d.Store.Registry().QueryRowContext(ctx, `SELECT login, checked, gone FROM owner_accounts WHERE account = ?`, account).Scan(&login, &checked, &gone)
	if err != nil || !s.fresh(checked) {
		if ok, err := s.alive(ctx, account); err == nil {
			_ = s.d.Store.Registry().QueryRowContext(ctx, `SELECT login FROM owner_accounts WHERE account = ?`, account).Scan(&login)
			gone.Valid = !ok
		}
	}
	if login == "" {
		login = account
	}
	return login, gone.Valid
}

// frozenErr refuses a write under a frozen owner.
func frozenErr(login string) error {
	return api.Errorf(api.CodeForbidden, "@%s's GitHub account is suspended or deleted: its names are frozen", login).
		WithHint("they still resolve, read-only; nothing is bound, renamed or transferred under them until GitHub answers for the account again")
}

// bindTarget checks a bind before its login starts: sub administers the
// project, the name is a project name, an owner is named.
func (s *Service) bindTarget(sub *authz.Subject, req api.GitHubLoginRequest) (api.Project, string, string, error) {
	p, err := s.d.Projects.Get(sub.Scope, req.Project)
	if err != nil {
		return p, "", "", err
	}
	if err := sub.Check(authz.Admin, authz.Object{Project: p.ID}, "project", req.Project); err != nil {
		return p, "", "", err
	}
	name := strings.ToLower(strings.TrimSpace(req.Name))
	if !api.ValidProjectName(name) {
		return p, "", "", api.Errorf(api.CodeBadRequest, "%q cannot name a project", req.Name).
			WithHint("2 to 63 of a-z 0-9 . _ - starting with a letter or digit; no reserved or blocked word, no zm- prefix")
	}
	owner := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(req.Owner), "@"))
	if owner == "" {
		return p, "", "", api.Errorf(api.CodeBadRequest, "name the owner: @<GitHub user or org>")
	}
	return p, owner, name, nil
}

// bindFacts is what a bind's login learns while GitHub's token is in hand:
// the owner as GitHub answers its login now, and (for an org) whether the
// approver is an active member of it. Errors are kept for the collect.
func (s *Service) bindFacts(ctx context.Context, l *login, holder Account, token string) {
	owner, err := s.acc.ByLogin(ctx, l.bindOwner)
	switch {
	case errors.Is(err, ErrNotFound):
		l.bindErr = s.missingOwner(ctx, l.bindOwner)
		return
	case err != nil:
		l.bindErr = providerDown(err)
		return
	}
	s.seen(ctx, owner)
	l.bindOwnerAcc = owner
	if owner.Kind == KindOrg {
		role, err := s.acc.OrgRole(ctx, token, owner.Login)
		if err != nil {
			l.bindErr = api.Errorf(api.CodeForbidden, "could not ask GitHub whether @%s is an admin of @%s", holder.Login, owner.Login)
			return
		}
		l.bindMember = role == "admin"
	}
}

// missingOwner is a login GitHub does not answer at a bind: a frozen owner
// when its last account is gone too, else no such account.
func (s *Service) missingOwner(ctx context.Context, login string) error {
	var last string
	if s.d.Store.Registry().QueryRowContext(ctx, `SELECT account FROM owner_logins WHERE login = ? AND account <> ''`, login).Scan(&last) == nil {
		if ok, err := s.alive(ctx, last); err == nil && !ok {
			return frozenErr(login)
		}
	}
	return api.Errorf(api.CodeNotFound, "no GitHub account @%s", login)
}

// bind binds @owner/name to the project, when the bind's login is
// collected: the approver is the binder's linked GitHub account, and the
// owner that account or an org it was an active member of. A name already
// bound under the owner (to another project), or pinned to another account
// under that login, is a conflict; the project's own name again makes it
// current (a rename back). Nothing is bound under a frozen owner, and a
// project whose current name is under one is not moved away.
func (s *Service) bind(ctx context.Context, l *login, holder Account) (api.NameBinding, error) {
	if l.bindErr != nil {
		return api.NameBinding{}, l.bindErr
	}
	var linked string
	_ = s.d.Store.Registry().QueryRowContext(ctx, `SELECT account FROM owner_links WHERE identity = ?`, l.identity.ID).Scan(&linked)
	if linked == "" || linked != holder.ID {
		return api.NameBinding{}, api.Errorf(api.CodeForbidden, "GitHub @%s is not %s's linked account", holder.Login, l.identity.Name).
			WithHint("link yours first: " + config.Command("login github"))
	}
	owner := l.bindOwnerAcc
	switch owner.Kind {
	case KindUser:
		if owner.ID != holder.ID {
			return api.NameBinding{}, api.Errorf(api.CodeForbidden, "only @%s binds names under @%s", owner.Login, owner.Login)
		}
	case KindOrg:
		if !l.bindMember {
			return api.NameBinding{}, api.Errorf(api.CodeForbidden, "@%s is not an admin of the GitHub org @%s", holder.Login, owner.Login).
				WithHint("a name under an org is bound by one of its admins: a name is never moved, so it is the org's to spend")
		}
	default:
		return api.NameBinding{}, api.Errorf(api.CodeForbidden, "@%s is a GitHub %s; projects belong to people and orgs", owner.Login, owner.Kind)
	}
	pid, name := l.bindProject, l.bindName
	var curOwner string
	if s.d.Store.Registry().QueryRowContext(ctx, `SELECT owner FROM owner_names WHERE project = ? AND current = 1`, pid).Scan(&curOwner) == nil &&
		curOwner != owner.ID {
		ok, err := s.alive(ctx, curOwner)
		if err != nil {
			return api.NameBinding{}, err
		}
		if !ok {
			cl, _ := s.state(ctx, curOwner)
			return api.NameBinding{}, frozenErr(cl)
		}
	}
	login := strings.ToLower(owner.Login)
	now := s.now()
	out := api.NameBinding{Path: ref(owner.Login, name), Account: owner.ID, Name: name, Project: pid, Current: true, Bound: now}
	err := store.Tx(ctx, s.d.Store.Registry(), func(tx *sql.Tx) error {
		// The freeze again, inside the transaction: the project's current
		// owner as it is now, by what GitHub last said of it.
		var cur string
		var gone sql.NullInt64
		if tx.QueryRowContext(ctx, `SELECT n.owner, a.gone FROM owner_names n LEFT JOIN owner_accounts a ON a.account = n.owner
			WHERE n.project = ? AND n.current = 1`, pid).Scan(&cur, &gone) == nil && cur != owner.ID && gone.Valid {
			return frozenErr(s.cachedLogin(ctx, cur))
		}
		var pinned, bound string
		var at int64
		switch err := tx.QueryRowContext(ctx, `SELECT owner FROM login_paths WHERE login = ? AND name = ?`, login, name).Scan(&pinned); {
		case err != nil && !errors.Is(err, sql.ErrNoRows):
			return err
		case err == nil && pinned != owner.ID:
			return api.Errorf(api.CodeConflict, "%s already reaches another GitHub account's project on this authority", ref(login, name)).
				WithHint("a path once answered keeps reaching it; pick another name")
		}
		switch err := tx.QueryRowContext(ctx, `SELECT project, bound FROM owner_names WHERE owner = ? AND name = ?`, owner.ID, name).Scan(&bound, &at); {
		case errors.Is(err, sql.ErrNoRows):
			if _, err := tx.ExecContext(ctx, `INSERT INTO owner_names (owner, name, project, bound) VALUES (?, ?, ?, ?)`,
				owner.ID, name, pid, store.Millis(now)); err != nil {
				return err
			}
		case err != nil:
			return err
		case bound != pid:
			return api.Errorf(api.CodeConflict, "%s is taken under @%s", ref(owner.Login, name), owner.Login).
				WithHint("a name is never moved to another project; pick another name")
		default:
			out.Bound = store.FromMillis(at)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE owner_names SET current = (owner = ? AND name = ?) WHERE project = ?`, owner.ID, name, pid); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, pinSQL, login, name, owner.ID, login, name)
		return err
	})
	if err != nil && (store.IsUnique(err) || strings.Contains(err.Error(), "never rebound")) {
		// Another bind of the same name won the race.
		return api.NameBinding{}, api.Errorf(api.CodeConflict, "%s was just bound by another request", ref(owner.Login, name))
	}
	if err != nil {
		return api.NameBinding{}, err
	}
	return out, nil
}

// Names lists the project's bindings, current first.
func (s *Service) Names(ctx context.Context, sub *authz.Subject, projectRef string) (api.NameBindingList, error) {
	p, err := s.d.Projects.Get(sub.Scope, projectRef)
	if err != nil {
		return api.NameBindingList{}, err
	}
	rows, err := s.d.Store.Registry().QueryContext(ctx, `SELECT owner, name, current, bound FROM owner_names WHERE project = ?
		ORDER BY current DESC, bound DESC`, p.ID)
	if err != nil {
		return api.NameBindingList{}, err
	}
	defer rows.Close()
	out := api.NameBindingList{Names: []api.NameBinding{}}
	for rows.Next() {
		b := api.NameBinding{Project: p.ID}
		var at int64
		if err := rows.Scan(&b.Account, &b.Name, &b.Current, &at); err != nil {
			return out, err
		}
		b.Bound = store.FromMillis(at)
		out.Names = append(out.Names, b)
	}
	if err := rows.Err(); err != nil {
		return out, err
	}
	rows.Close()
	for i, b := range out.Names {
		out.Names[i].Path = ref(s.cachedLogin(ctx, b.Account), b.Name)
	}
	return out, nil
}

// Resolve answers @login/name: the project it reaches, with its current
// path. A path this authority bound or answered before is pinned to the
// owner it reached then and follows the pin, whoever holds the login on
// GitHub now; a new path goes to the login's current holder and is pinned.
// A project sub cannot read answers like a missing name.
func (s *Service) Resolve(ctx context.Context, sub *authz.Subject, login, name string) (api.NameResolution, error) {
	// The refusal echoes the name as asked, so a hidden and a missing name
	// answer the same bytes but for what the caller typed.
	missing := api.NotFound("name", ref(strings.TrimPrefix(login, "@"), name))
	login = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(login), "@"))
	name = strings.ToLower(strings.TrimSpace(name))
	if login == "" || !api.ValidProjectName(name) {
		return api.NameResolution{}, missing
	}
	db := s.d.Store.Registry()
	var target string
	pinned := false
	switch err := db.QueryRowContext(ctx, `SELECT owner FROM login_paths WHERE login = ? AND name = ?`, login, name).Scan(&target); {
	case err == nil:
		pinned = true
	case !errors.Is(err, sql.ErrNoRows):
		return api.NameResolution{}, err
	default:
		if acct, checked, ok := s.cachedOwner(ctx, login); !ok || (acct != "" && !s.fresh(checked)) ||
			(acct == "" && s.now().Sub(store.FromMillis(checked)) >= s.NegTTL) {
			if !s.lookupAllowed(callerKey(sub)) {
				return api.NameResolution{}, api.Errorf(api.CodeRateLimited, "too many name lookups").WithRetryAfter(60)
			}
		}
		acct, err := s.owner(ctx, login)
		if api.IsCode(err, api.CodeNotFound) {
			return api.NameResolution{}, missing
		}
		if err != nil {
			return api.NameResolution{}, err
		}
		target = acct
	}
	var pid string
	if db.QueryRowContext(ctx, `SELECT project FROM owner_names WHERE owner = ? AND name = ?`, target, name).Scan(&pid) != nil {
		return api.NameResolution{}, missing
	}
	p, err := s.d.Projects.Get(sub.Scope, pid)
	if err != nil {
		return api.NameResolution{}, missing
	}
	if !pinned {
		if _, err := db.ExecContext(ctx, pinSQL, login, name, target, login, name); err != nil {
			return api.NameResolution{}, err
		}
	}
	out := api.NameResolution{Project: p.ID, Slug: p.Slug}
	var curOwner, curName string
	if err := db.QueryRowContext(ctx, `SELECT owner, name FROM owner_names WHERE project = ? AND current = 1`, p.ID).Scan(&curOwner, &curName); err != nil {
		curOwner, curName = target, name
	}
	curLogin, curFrozen := s.state(ctx, curOwner)
	_, frozen := s.state(ctx, target)
	out.Path = s.reachingPath(ctx, curOwner, curLogin, curName, login, name)
	out.Moved = !strings.EqualFold(out.Path, ref(login, name))
	out.Frozen = frozen || curFrozen
	return out, nil
}

// reachingPath is the project's current path, @<its owner's login now>/name,
// unless that path is pinned to another account (a login the owner took
// over from someone else): then a path this authority pinned to the owner
// for the name, else the path asked. The answer always reaches the project.
func (s *Service) reachingPath(ctx context.Context, owner, login, name, askedLogin, askedName string) string {
	db := s.d.Store.Registry()
	var pinned string
	err := db.QueryRowContext(ctx, `SELECT owner FROM login_paths WHERE login = ? AND name = ?`, strings.ToLower(login), name).Scan(&pinned)
	if err != nil || pinned == owner {
		return ref(login, name)
	}
	var alt string
	if db.QueryRowContext(ctx, `SELECT login FROM login_paths WHERE owner = ? AND name = ? ORDER BY rowid DESC LIMIT 1`, owner, name).Scan(&alt) == nil {
		return ref(alt, name)
	}
	return ref(askedLogin, askedName)
}

func (s *Service) handleNames(r *http.Request) (any, error) {
	return s.Names(r.Context(), core.SubjectOf(r.Context()), r.PathValue("project"))
}

func (s *Service) handleResolve(r *http.Request) (any, error) {
	return s.Resolve(r.Context(), core.SubjectOf(r.Context()), r.PathValue("owner"), r.PathValue("name"))
}
