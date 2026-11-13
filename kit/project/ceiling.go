package project

// Grant ceilings: an admin lowering or removing a grant holds over a join
// that is not the admin's own act. The GitHub mapping still saying member
// does not undo an owner lowering a member to guest (zm-eakh.12.5.10.14),
// removing them (zm-eakh.12.23.22), or removing and re-adding them lower.
// A ceiling lives apart from the grant row, so deleting the row
// (Registry.Revoke, or RevokeAdmitted when a GitHub grant goes) leaves it.
// Setting one cancels the identity's unredeemed invites it would undo
// (zm-eakh.12.23.24); an invite made later is the owner's later word, and
// its raise moves the ceiling like Grant's.
//
// The stable fact behind a GitHub join is the GitHub account, not the
// identity: unlinking an identity (owners' Unlink) records here which
// account it was (ceiling_accounts), so the account joining again under
// another identity is still held by that identity's ceilings
// (zm-eakh.12.23.27). Every link is recorded there too (owners' linkTx),
// so an owner's removal or lowering of one identity reaches the grants of
// every other identity the same account is or was (lowerSameAccount): a
// person who unlinked and joined again before the owner acted keeps
// nothing the owner took away (zm-eakh.12.23.31).
//
// An admin sees every ceiling on objects it administers (Ceilings, in the
// grants list; AccountCeilings, on each GitHub user grant). Listing lifts
// nothing (zm-eakh.12.23.28, zm-eakh.12.23.23).

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/store"
)

func init() {
	// relation is what the admin left ('' after a removal); lowered_from
	// is the highest role it was lowered from, so a later hand-set role
	// below that still holds. Grants lowered under 0004 come over with
	// lowered_from owner (their old role is unknown: fail closed), and
	// every unredeemed invite of a lowered identity in that project goes.
	store.RegisterRegistry("project/0005_grant_ceilings", `
CREATE TABLE grant_ceilings (
	subject      TEXT NOT NULL,
	object       TEXT NOT NULL,
	project      TEXT NOT NULL,
	area         TEXT NOT NULL DEFAULT '',
	relation     TEXT NOT NULL,
	lowered_from TEXT NOT NULL,
	set_by       TEXT NOT NULL,
	set_at       INTEGER NOT NULL,
	PRIMARY KEY (subject, object)
);
CREATE INDEX grant_ceilings_project ON grant_ceilings (project);
INSERT INTO grant_ceilings (subject, object, project, area, relation, lowered_from, set_by, set_at)
	SELECT subject, object, project, area, relation, 'owner', lowered_by, CAST(strftime('%s', 'now') AS INTEGER) * 1000
	FROM grants WHERE lowered_by != '';
DELETE FROM invites WHERE redeemed IS NULL AND node = '' AND EXISTS (SELECT 1 FROM grants g JOIN identities i ON i.id = g.subject
	WHERE g.lowered_by != '' AND i.name = invites.identity AND g.project = invites.project);
ALTER TABLE grants DROP COLUMN lowered_by;`)
	// One row per identity and each GitHub account it is or was linked to
	// (a link, or the unlink that ended it): a fact, never removed.
	// project/0007 is taken: kit/owners registers it to fill this table
	// from owner_links (it must sort after 0006).
	store.RegisterRegistry("project/0006_ceiling_accounts", `
CREATE TABLE ceiling_accounts (
	subject TEXT NOT NULL,
	account TEXT NOT NULL,
	PRIMARY KEY (subject, account)
);
CREATE INDEX ceiling_accounts_account ON ceiling_accounts (account);`)
}

// RememberAccount records, inside the transaction that makes or removes an
// identity's link, that identity subject is or was account: a later join by
// account under any identity is held by subject's ceilings (ownerCap), and
// an owner's lowering of subject lowers the account's other identities
// (lowerSameAccount). An empty subject or account is refused, never stored.
func RememberAccount(ctx context.Context, tx *sql.Tx, subject, account string) error {
	if subject == "" || account == "" {
		return errors.New("project: remembering an account needs an identity and an account")
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO ceiling_accounts (subject, account) VALUES (?, ?) ON CONFLICT DO NOTHING`, subject, account)
	return err
}

// setCeiling records an owner's act setting subject's relation on
// project/area from prev to rel (rel None: removed): when rel is below
// what the subject held before any lowering still in place, a ceiling at
// rel; otherwise (a raise back, or a first grant) no ceiling. An act that
// lowers the grant itself (rel below prev) also removes the subject's
// unredeemed invites in project that would raise past it (cancelInvites):
// those were made before it. A raise that keeps a ceiling leaves invites
// alone, since any still there were made after the lowering. Grant, Revoke and a redeemed invite call it; a GitHub
// join never does.
func setCeiling(ctx context.Context, tx *sql.Tx, subject, project, area string, prev, rel authz.Relation, by string, now time.Time) error {
	obj := authz.AreaObject(project, area)
	from := prev
	var was string
	switch err := tx.QueryRowContext(ctx, `SELECT lowered_from FROM grant_ceilings WHERE subject = ? AND object = ?`, subject, obj).Scan(&was); {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return err
	default:
		w, err := authz.ParseRelation(was)
		if err != nil {
			return err
		}
		from = authz.Max(from, w)
	}
	if authz.Max(from, rel) == rel {
		_, err := tx.ExecContext(ctx, `DELETE FROM grant_ceilings WHERE subject = ? AND object = ?`, subject, obj)
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO grant_ceilings (subject, object, project, area, relation, lowered_from, set_by, set_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT (subject, object) DO UPDATE SET relation = excluded.relation,
		lowered_from = excluded.lowered_from, set_by = excluded.set_by, set_at = excluded.set_at`,
		subject, obj, project, area, string(rel), string(from), by, store.Millis(now)); err != nil {
		return err
	}
	if authz.Max(prev, rel) == rel {
		return nil
	}
	return cancelInvites(ctx, tx, subject, project, area, rel)
}

// lowerSameAccount applies an owner's act that lowered subject's grant on
// project/area to rel (None: removed) to every other identity that shares a
// GitHub account with subject in ceiling_accounts (one hop: an identity
// that was two accounts does not join those accounts' other identities,
// which may be other people). Each of their grants above rel on what the
// act took is lowered or removed with a ceiling (setCeiling), so the live
// node tokens it backs count for no more (tokenStanding): the grant on the
// same object and, for a whole-project act, their area grants in the
// project too (the owner cannot see these identities are subject's, so
// they keep no corner of a project subject was removed from). An area's
// act never touches their whole-project grant: the actor may hold admin on
// the area alone, and subject's own whole-project grant would stay too.
// Their unredeemed invites past rel go, as subject's do. It answers the
// names of the identities it lowered. The identities in except are never
// touched: the machine owner's own, and the actor's (an owner tidying up
// their own second identity keeps their own grant).
func lowerSameAccount(ctx context.Context, tx *sql.Tx, subject string, except []string, project, area string, rel authz.Relation, by string, now time.Time) ([]string, error) {
	skip, err := json.Marshal(except)
	if err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT DISTINCT i.id, i.name FROM ceiling_accounts mine
		JOIN ceiling_accounts other ON other.account = mine.account AND other.subject != mine.subject
		JOIN identities i ON i.id = other.subject
		WHERE mine.subject = ? AND i.id NOT IN (SELECT value FROM json_each(?)) ORDER BY i.name`, subject, string(skip))
	if err != nil {
		return nil, err
	}
	var same []capSubject
	for rows.Next() {
		var s capSubject
		if err := rows.Scan(&s.id, &s.name); err != nil {
			rows.Close()
			return nil, err
		}
		same = append(same, s)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var lowered []string
	for _, s := range same {
		type held struct {
			area string
			rel  authz.Relation
		}
		var take []held
		rows, err := tx.QueryContext(ctx, `SELECT area, relation FROM grants WHERE subject = ? AND project = ? AND (? = '' OR area = ?)`,
			s.id, project, area, area)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var h held
			var r string
			if err := rows.Scan(&h.area, &r); err != nil {
				rows.Close()
				return nil, err
			}
			if h.rel, err = authz.ParseRelation(r); err != nil {
				rows.Close()
				return nil, err
			}
			if authz.Max(h.rel, rel) != rel {
				take = append(take, h)
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
		for _, h := range take {
			obj := authz.AreaObject(project, h.area)
			if rel == authz.None {
				_, err = tx.ExecContext(ctx, `DELETE FROM grants WHERE subject = ? AND object = ?`, s.id, obj)
			} else {
				_, err = tx.ExecContext(ctx, `UPDATE grants SET relation = ? WHERE subject = ? AND object = ?`, string(rel), s.id, obj)
			}
			if err != nil {
				return nil, err
			}
			if err := setCeiling(ctx, tx, s.id, project, h.area, h.rel, rel, by, now); err != nil {
				return nil, err
			}
		}
		// Invites made before this act must not give back what it took,
		// whether or not a grant came down.
		if err := cancelInvites(ctx, tx, s.id, project, area, rel); err != nil {
			return nil, err
		}
		if len(take) > 0 {
			if err := bumpScope(ctx, tx, s.id); err != nil {
				return nil, err
			}
			lowered = append(lowered, s.name)
		}
	}
	return lowered, nil
}

// cancelInvites removes subject's unredeemed invites in project that a
// ceiling of rel on area would not allow: above rel on that object, on the
// whole project (which covers the area), or, for a whole-project ceiling,
// on any area.
func cancelInvites(ctx context.Context, tx *sql.Tx, subject, project, area string, rel authz.Relation) error {
	rows, err := tx.QueryContext(ctx, `SELECT id, area, role FROM invites WHERE project = ? AND redeemed IS NULL AND node = ''
		AND identity = (SELECT name FROM identities WHERE id = ?)`, project, subject)
	if err != nil {
		return err
	}
	var gone []string
	for rows.Next() {
		var id, ia, role string
		if err := rows.Scan(&id, &ia, &role); err != nil {
			rows.Close()
			return err
		}
		ir, err := authz.ParseRelation(role)
		if err != nil {
			rows.Close()
			return err
		}
		if (area == "" || ia == "" || ia == area) && authz.Max(rel, ir) != rel {
			gone = append(gone, id)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, id := range gone {
		if _, err := tx.ExecContext(ctx, `DELETE FROM invites WHERE id = ?`, id); err != nil {
			return err
		}
	}
	return nil
}

// ownerCap is the most a join that is not an owner's own act (Admit: the
// GitHub join) gives identity in project/area: the least of identityCap
// for identity and for every other identity account (the GitHub account
// joining; empty for none) is or was linked to (ceiling_accounts). The
// account is the person: an owner's removal or lowering of any identity it
// was caps its joins under any other, until an owner raises that identity
// again. by names the identity whose ceiling set the answer (identity when
// none capped it). None means the join is refused (errOwnerRemoved).
func ownerCap(ctx context.Context, tx *sql.Tx, identity, account, project, area string, role authz.Relation) (capped authz.Relation, by string, err error) {
	subjects, err := capSubjects(ctx, tx, identity, account)
	if err != nil {
		return authz.None, "", err
	}
	capped, by = role, identity
	for _, s := range subjects {
		c, err := identityCap(ctx, tx, s.id, project, area, role)
		if err != nil {
			return authz.None, "", err
		}
		if authz.Min(capped, c) != capped {
			capped, by = c, s.name
		}
	}
	return capped, by, nil
}

type capSubject struct{ id, name string }

// capSubjects is identity (when it exists) and every identity account is
// or was linked to (ceiling_accounts).
func capSubjects(ctx context.Context, tx *sql.Tx, identity, account string) ([]capSubject, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id, name FROM identities WHERE name = ?
		UNION SELECT i.id, i.name FROM ceiling_accounts a JOIN identities i ON i.id = a.subject WHERE a.account = ? AND a.account != ''`,
		identity, account)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []capSubject
	for rows.Next() {
		var s capSubject
		if err := rows.Scan(&s.id, &s.name); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// identityCap is the most a join gives role on project/area under the
// ceilings of identity subject (an id). A ceiling on the area caps a join
// to it; a whole-project ceiling caps an area's join too, but never below
// the area's own grant (a grant never demotes); an area ceiling caps a
// whole-project join, which would cover it, but never below the
// whole-project grant itself.
func identityCap(ctx context.Context, tx *sql.Tx, subject, project, area string, role authz.Relation) (authz.Relation, error) {
	ceiling := func(area string) (rel authz.Relation, ok bool, err error) {
		var r string
		switch err := tx.QueryRowContext(ctx, `SELECT relation FROM grant_ceilings WHERE subject = ? AND object = ?`,
			subject, authz.AreaObject(project, area)).Scan(&r); {
		case errors.Is(err, sql.ErrNoRows):
			return authz.None, false, nil
		case err != nil:
			return authz.None, false, err
		}
		rel, err = ceilingRelation(r)
		return rel, err == nil, err
	}
	own, lowered, err := ceiling(area)
	if err != nil {
		return authz.None, err
	}
	if lowered {
		return authz.Min(role, own), nil
	}
	// A cap from another object never takes the join below the grant on
	// its own object: that grant has no ceiling, and a grant never demotes.
	var held string
	switch err := tx.QueryRowContext(ctx, `SELECT relation FROM grants WHERE subject = ? AND object = ?`,
		subject, authz.AreaObject(project, area)).Scan(&held); {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return authz.None, err
	}
	floor := func(capped authz.Relation) authz.Relation {
		return authz.Max(capped, authz.Min(role, authz.Relation(held)))
	}
	if area == "" {
		rows, err := tx.QueryContext(ctx, `SELECT relation FROM grant_ceilings WHERE subject = ? AND project = ? AND area != ''`, subject, project)
		if err != nil {
			return authz.None, err
		}
		defer rows.Close()
		capped := role
		for rows.Next() {
			var r string
			if err := rows.Scan(&r); err != nil {
				return authz.None, err
			}
			rel, err := ceilingRelation(r)
			if err != nil {
				return authz.None, err
			}
			capped = authz.Min(capped, rel)
		}
		if err := rows.Err(); err != nil {
			return authz.None, err
		}
		return floor(capped), nil
	}
	whole, wholeLowered, err := ceiling("")
	if err != nil {
		return authz.None, err
	}
	if !wholeLowered {
		return role, nil
	}
	return floor(authz.Min(role, whole)), nil
}

// ceilingRelation reads a stored ceiling: empty is a removal, anything else a
// role (a bad value is an error, never a silent cap).
func ceilingRelation(r string) (authz.Relation, error) {
	if r == "" {
		return authz.None, nil
	}
	return authz.ParseRelation(r)
}

// errOwnerRemoved refuses a join an owner's removal of identity holds back
// (ownerCap answered None, by identity).
func errOwnerRemoved(identity, slug string) error {
	return api.Errorf(api.CodeForbidden, "an owner removed or lowered %s's grants in %s", identity, slug).
		WithHint("an owner sees this in the grants list's ceilings and can add you back with a new invite (a grant to identity " + identity + " also lifts this, and counts again for its machines' live tokens)")
}

// Ceilings lists the owners' ceilings on objects where sub has admin (one
// project, or all), as Grants lists grants.
func (r *Registry) Ceilings(ctx context.Context, sub *authz.Subject, project string) ([]api.Ceiling, error) {
	pid, err := r.listedProject(sub, project)
	if err != nil {
		return nil, err
	}
	return r.ceilings(ctx, sub, pid, "")
}

// AccountCeilings lists the ceilings in project pid on the identities
// account is or was linked to, on objects where sub has admin: what holds
// back the account's GitHub joins there.
func (r *Registry) AccountCeilings(ctx context.Context, sub *authz.Subject, account, pid string) ([]api.Ceiling, error) {
	if account == "" || pid == "" {
		return nil, errors.New("project: account ceilings need an account and a project")
	}
	return r.ceilings(ctx, sub, pid, account)
}

// ceilings reads grant_ceilings on objects where sub has admin: in project
// pid ("" for all), on the identities account is or was ("" for any).
func (r *Registry) ceilings(ctx context.Context, sub *authz.Subject, pid, account string) ([]api.Ceiling, error) {
	rows, err := r.st.Registry().QueryContext(ctx, `SELECT i.name, c.project, c.area, c.relation, c.lowered_from, c.set_by, c.set_at,
		COALESCE((SELECT group_concat(a.account, ' ') FROM ceiling_accounts a WHERE a.subject = c.subject), '')
		FROM grant_ceilings c JOIN identities i ON i.id = c.subject
		WHERE (? = '' OR c.project = ?) AND (? = '' OR c.subject IN (SELECT subject FROM ceiling_accounts WHERE account = ?))
		ORDER BY c.project, c.area, i.name`, pid, pid, account, account)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []api.Ceiling{}
	for rows.Next() {
		var c api.Ceiling
		var p, accounts string
		var at int64
		if err := rows.Scan(&c.Identity, &p, &c.Area, &c.Role, &c.LoweredFrom, &c.SetBy, &at, &accounts); err != nil {
			return nil, err
		}
		if !sub.Can(authz.Admin, authz.Object{Project: p, Area: c.Area}) {
			continue
		}
		c.Project, c.SetAt = r.Slug(p), store.FromMillis(at)
		// The accounts are the identity's link history across projects:
		// only a whole-project admin sees them.
		if accounts != "" && sub.Can(authz.Admin, authz.Object{Project: p}) {
			c.Accounts = strings.Fields(accounts)
			slices.Sort(c.Accounts)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
