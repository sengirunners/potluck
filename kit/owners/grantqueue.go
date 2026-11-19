package owners

// Grants GitHub's rate limit refused (SPEC 22 "Join by GitHub"). Resolving a
// login to its account takes a GitHub call, and an authority without a lookup
// token (or with a busy one) can be refused for the rate limit. A refused
// grant is not lost and not left to the granter to repeat: it is kept in
// github_grant_queue, a worker (Start) makes it once GitHub's reset has
// passed, and the granter is mailed how it ended: made, or why not.
//
// The request is what was asked, not a standing authority: when it runs, the
// granter must still administer the project (the machine's owner always
// does), so a grant queued by an admin who lost the role meanwhile is
// dropped, and the granter is told.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/store"
)

var grantQueueMigrations = []store.Migration{
	{Kind: store.KindRegistry, Name: "owners/0006_grant_queue", Script: `
CREATE TABLE github_grant_queue (
	project       TEXT NOT NULL,
	area          TEXT NOT NULL DEFAULT '',
	login         TEXT NOT NULL, -- lower case
	role          TEXT NOT NULL,
	identity_id   TEXT NOT NULL, -- the granter
	identity_name TEXT NOT NULL,
	address       TEXT NOT NULL,
	machine_owner INTEGER NOT NULL, -- the machine's owner on its socket: needs no grant of its own
	token_id      TEXT NOT NULL DEFAULT '', -- the granter's token, which must still be live when the grant is made
	created       INTEGER NOT NULL,
	retry_at      INTEGER NOT NULL,
	PRIMARY KEY (project, area, login)
);`},
}

// queuedGrant is a row of github_grant_queue.
type queuedGrant struct {
	project, area, login, role string
	granter                    api.Identity
	address, tokenID           string
	machineOwner               bool
	created                    time.Time
}

// sameQueued is the SQL that matches only this snapshot of the row: a grant asked
// again meanwhile (a new role, a new time) is another row to the pass that
// holds the old one.
const sameQueued = ` project = ? AND area = ? AND login = ? AND role = ? AND identity_id = ? AND created = ?`

func (q queuedGrant) same() []any {
	return []any{q.project, q.area, q.login, q.role, q.granter.ID, store.Millis(q.created)}
}

// retryAfter is when to ask again after limited: its reset and QueueSlack,
// and never sooner than QueueEvery from now when the reset named is already
// past or unknown (GitHub's clock and ours differ), nor later than
// QueueGiveUp from now (a reset that far off is given up on when it comes).
func (s *Service) retryAfter(limited *RateLimited) time.Time {
	now := s.now()
	at := limited.ResetAt.Add(s.QueueSlack)
	if limited.ResetAt.IsZero() || !at.After(now) {
		at = now.Add(s.QueueEvery)
	}
	if far := now.Add(s.QueueGiveUp); at.After(far) {
		at = far
	}
	return at.UTC().Truncate(time.Millisecond)
}

// queueGrant keeps the grant of rel to login until GitHub answers. The same
// grant asked again replaces the queued one (the latest role wins).
func (s *Service) queueGrant(ctx context.Context, sub *authz.Subject, p api.ProjectRef, area string, rel authz.Relation, login string, limited *RateLimited) (api.GitHubGrant, error) {
	now, retry := s.now(), s.retryAfter(limited)
	login = strings.ToLower(login)
	if _, err := s.d.Store.Registry().ExecContext(ctx, `INSERT INTO github_grant_queue
		(project, area, login, role, identity_id, identity_name, address, machine_owner, token_id, created, retry_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (project, area, login) DO UPDATE SET role = excluded.role, identity_id = excluded.identity_id,
		identity_name = excluded.identity_name, address = excluded.address, machine_owner = excluded.machine_owner, token_id = excluded.token_id,
		created = excluded.created, retry_at = excluded.retry_at`,
		p.ID, area, login, string(rel), sub.Identity.ID, sub.Identity.Name, sub.Address, sub.Via == authz.ViaLocal && sub.TokenID == "", sub.TokenID,
		store.Millis(now), store.Millis(retry)); err != nil {
		return api.GitHubGrant{}, err
	}
	select {
	case s.queueWake <- struct{}{}:
	default:
	}
	s.d.Log.Info("owners: a grant waits for GitHub's rate limit", "project", p.Slug, "login", login, "retry", retry)
	return queuedView(p.Slug, area, login, string(rel), now, retry), nil
}

func queuedView(project, area, login, role string, created, retry time.Time) api.GitHubGrant {
	return api.GitHubGrant{Login: login, Role: role, Project: project, Area: area, Created: created, Queued: true, RetryAt: &retry}
}

// dequeueGrant cancels the queued grant to login (a revoke), saying whether
// there was one.
func (s *Service) dequeueGrant(ctx context.Context, project, area, login string) bool {
	res, err := s.d.Store.Registry().ExecContext(ctx, `DELETE FROM github_grant_queue WHERE project = ? AND area = ? AND login = ?`, project, area, strings.ToLower(login))
	if err != nil {
		return false
	}
	n, _ := res.RowsAffected()
	return n > 0
}

// queuedGrants are the project's grants still waiting, for the list.
func (s *Service) queuedGrants(ctx context.Context, p api.Project) ([]api.GitHubGrant, error) {
	rows, err := s.d.Store.Registry().QueryContext(ctx, `SELECT area, login, role, created, retry_at FROM github_grant_queue
		WHERE project = ? ORDER BY area, login`, p.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []api.GitHubGrant
	for rows.Next() {
		var area, login, role string
		var created, retry int64
		if err := rows.Scan(&area, &login, &role, &created, &retry); err != nil {
			return nil, err
		}
		out = append(out, queuedView(p.Slug, area, login, role, store.FromMillis(created), store.FromMillis(retry)))
	}
	return out, rows.Err()
}

// runGrantQueue works the queue until ctx ends: when the next grant falls
// due, at the latest after QueueEvery, and at once when a grant is queued.
func (s *Service) runGrantQueue(ctx context.Context) {
	for {
		wait := s.QueueEvery
		var next sql.NullInt64
		if err := s.d.Store.Registry().QueryRowContext(ctx, `SELECT min(retry_at) FROM github_grant_queue`).Scan(&next); err == nil && next.Valid {
			wait = min(wait, store.FromMillis(next.Int64).Sub(s.now()))
		}
		t := time.NewTimer(max(wait, 100*time.Millisecond))
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-s.ctx.Done():
			t.Stop()
			return
		case <-s.queueWake:
			t.Stop()
		case <-t.C:
		}
		if err := s.WorkGrantQueue(ctx); err != nil && ctx.Err() == nil {
			s.d.Log.Warn("owners: working the grant queue", "err", err)
		}
	}
}

// WorkGrantQueue makes every queued grant whose retry time has come. When
// GitHub refuses one for its rate limit, the rest that are due wait for
// the same reset without being tried.
func (s *Service) WorkGrantQueue(ctx context.Context) error {
	s.queueRun.Lock()
	defer s.queueRun.Unlock()
	rows, err := s.d.Store.Registry().QueryContext(ctx, `SELECT project, area, login, role, identity_id, identity_name, address, machine_owner, token_id, created
		FROM github_grant_queue WHERE retry_at <= ? ORDER BY retry_at`, store.Millis(s.now()))
	if err != nil {
		return err
	}
	var due []queuedGrant
	for rows.Next() {
		var q queuedGrant
		var owner int
		var created int64
		if err := rows.Scan(&q.project, &q.area, &q.login, &q.role, &q.granter.ID, &q.granter.Name, &q.address, &owner, &q.tokenID, &created); err != nil {
			rows.Close()
			return err
		}
		q.machineOwner, q.created = owner != 0, store.FromMillis(created)
		due = append(due, q)
	}
	rows.Close()
	var limited *RateLimited
	for _, q := range due {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if limited != nil {
			s.retryQueued(ctx, q, s.retryAfter(limited), s.d.Projects.Slug(q.project)+areaSuffix(q.area), limited)
			continue
		}
		limited = s.finishQueued(ctx, q)
	}
	return nil
}

// finishQueued makes one queued grant, or reschedules it (GitHub still
// refuses, or failed some other way and QueueGiveUp has not passed), or
// ends it and tells the granter why. It answers GitHub's rate limit when
// that was the reason.
func (s *Service) finishQueued(ctx context.Context, q queuedGrant) *RateLimited {
	slug := s.d.Projects.Slug(q.project)
	where := slug + areaSuffix(q.area)
	if slug == "" { // the project is gone: nothing to grant in, nobody to tell in it
		s.forgetQueued(ctx, q)
		return nil
	}
	end := func(format string, a ...any) {
		if s.forgetQueued(ctx, q) {
			s.tellGranter(ctx, q, fmt.Sprintf(format, a...))
		}
	}
	admin, err := s.stillAdmin(ctx, q)
	if err != nil {
		s.d.Log.Warn("owners: a queued grant's granter", "project", slug, "login", q.login, "err", err)
		s.retryQueued(ctx, q, s.now().Add(s.QueueBackoff), where, err)
		return nil
	}
	if !admin {
		end("The GitHub grant of %s in %s to @%s that waited for GitHub's rate limit was not made: you no longer administer %s.", q.role, where, q.login, where)
		return nil
	}
	if q.area != "" && !s.d.Projects.HasArea(q.project, q.area) {
		end("The GitHub grant of %s in %s to @%s that waited for GitHub's rate limit was not made: the area is gone.", q.role, where, q.login)
		return nil
	}
	acc, err := s.grantable(ctx, q.login)
	var limited *RateLimited
	switch {
	case err == nil:
		made, err := s.makeQueued(ctx, q, slug, acc)
		switch {
		case err != nil:
			s.d.Log.Warn("owners: making a queued grant", "project", slug, "login", q.login, "err", err)
			s.retryQueued(ctx, q, s.now().Add(s.QueueBackoff), where, err)
		case made:
			s.tellGranter(ctx, q, fmt.Sprintf("The GitHub grant of %s in %s to @%s that waited for GitHub's rate limit is made.", q.role, where, acc.Login))
		}
	case errors.As(err, &limited):
		s.retryQueued(ctx, q, s.retryAfter(limited), where, err)
		return limited
	case api.IsCode(err, api.CodeUnavailable):
		s.retryQueued(ctx, q, s.now().Add(s.QueueBackoff), where, err)
	default:
		end("The GitHub grant of %s in %s to @%s that waited for GitHub's rate limit was not made: %v", q.role, where, q.login, err)
	}
	return nil
}

// makeQueued claims q's row and makes its grant in one transaction: a
// grant revoked or asked again while GitHub was being asked is no longer
// the row this pass holds, and is not made (made false).
func (s *Service) makeQueued(ctx context.Context, q queuedGrant, slug string, acc Account) (made bool, err error) {
	rel, err := authz.ParseRelation(q.role)
	if err != nil {
		return false, err
	}
	err = store.Tx(ctx, s.d.Store.Registry(), func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM github_grant_queue WHERE`+sameQueued, q.same()...)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil
		}
		made = true
		_, err = s.putGrant(ctx, tx, api.ProjectRef{ID: q.project, Slug: slug}, q.area, rel, acc, q.address)
		return err
	})
	return made && err == nil, err
}

// retryQueued puts q off until at, or ends it when it has waited QueueGiveUp.
func (s *Service) retryQueued(ctx context.Context, q queuedGrant, at time.Time, where string, cause error) {
	if s.now().Sub(q.created) >= s.QueueGiveUp {
		if s.forgetQueued(ctx, q) {
			s.tellGranter(ctx, q, fmt.Sprintf("The GitHub grant of %s in %s to @%s was not made: GitHub did not answer for %s (%v). Grant it again later.",
				q.role, where, q.login, s.QueueGiveUp.Round(time.Hour), cause))
		}
		return
	}
	if _, err := s.d.Store.Registry().ExecContext(ctx, `UPDATE github_grant_queue SET retry_at = ? WHERE`+sameQueued, append([]any{store.Millis(at)}, q.same()...)...); err != nil {
		s.d.Log.Warn("owners: rescheduling a queued grant", "err", err)
	}
}

// forgetQueued drops q's row, saying whether this pass held it (a revoke
// or a newer ask may have taken it already; then nobody is told).
func (s *Service) forgetQueued(ctx context.Context, q queuedGrant) bool {
	res, err := s.d.Store.Registry().ExecContext(ctx, `DELETE FROM github_grant_queue WHERE`+sameQueued, q.same()...)
	if err != nil {
		s.d.Log.Warn("owners: dropping a queued grant", "err", err)
		return false
	}
	n, _ := res.RowsAffected()
	return n > 0
}

// tellGranter mails how q ended to where the granter asked from (a person,
// an agent's session or a machine), and to the project's owners.
func (s *Service) tellGranter(ctx context.Context, q queuedGrant, body string) {
	ref := api.ProjectRef{ID: q.project, Slug: s.d.Projects.Slug(q.project)}
	for _, to := range []string{q.address, "owners"} {
		if err := s.d.Notify(ctx, core.Notice{Project: ref, To: to, Body: body}); err != nil && !errors.Is(err, core.ErrNoNotifier) {
			s.d.Log.Warn("owners: mail", "to", to, "project", ref.Slug, "err", err)
		}
	}
}

// stillAdmin says whether the granter still may make the grant: the
// machine's owner always; anyone else by the grants their identity holds
// now, and by a token that is still live when it asked with one.
func (s *Service) stillAdmin(ctx context.Context, q queuedGrant) (bool, error) {
	if q.machineOwner {
		return true, nil
	}
	if q.tokenID != "" && !s.d.Projects.TokenLive(ctx, q.tokenID) { // a revoked or expired token asks nothing more
		return false, nil
	}
	tuples, err := s.d.Projects.Tuples(ctx, q.granter.ID)
	if err != nil {
		return false, err
	}
	return authz.FromTuples(tuples, nil).Can(authz.Admin, authz.Object{Project: q.project, Area: q.area}), nil
}
