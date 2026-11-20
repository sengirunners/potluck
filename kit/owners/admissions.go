package owners

// Admissions: what each GitHub join gave (SPEC 22). A GitHub grant is the
// source of the grants and node-bound tokens of the identities it admitted,
// so revoking the grant revokes them, exactly (by source, never a grant
// made some other way). An org grant's members are re-checked with the
// authority's own lookup token every RecheckEvery; a member who left the
// org loses what the org's grant gave at the next check.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/store"
)

var admissionMigrations = []store.Migration{
	{Kind: store.KindRegistry, Name: "owners/0003_admissions", Script: `
CREATE TABLE github_admissions (
	id            TEXT PRIMARY KEY,
	grant_account TEXT NOT NULL,
	grant_kind    TEXT NOT NULL,
	grant_area    TEXT NOT NULL DEFAULT '',
	project       TEXT NOT NULL,
	area          TEXT NOT NULL DEFAULT '',
	account       TEXT NOT NULL,
	identity      TEXT NOT NULL,
	node          TEXT NOT NULL,
	token         TEXT NOT NULL,
	source        TEXT NOT NULL,
	created       INTEGER NOT NULL,
	checked       INTEGER NOT NULL,
	revoked       INTEGER
);
CREATE INDEX github_admissions_grant ON github_admissions (grant_account, project, grant_area);
CREATE TABLE github_org_checks (
	project       TEXT PRIMARY KEY,
	failing_since INTEGER NOT NULL,
	reason        TEXT NOT NULL,
	mailed        INTEGER NOT NULL DEFAULT 0
);`},
}

// source is what an admission's grants row records as created_by.
func source(user string, g grantKey) string {
	return fmt.Sprintf("%s grant:%s:%s", user, g.account, g.area) // "github:<id> grant:github:<id>:<area>"
}

type admission struct {
	id, identity, project, area, token, src, account, grantAccount string
}

// revokeAdmissions revokes every live admission one GitHub grant gave
// (its grants rows by source, its node-bound tokens) and tells the
// identities and owners why. It answers how many went.
func (s *Service) revokeAdmissions(ctx context.Context, grantAccount, project, grantArea, why string) (int, error) {
	rows, err := s.d.Store.Registry().QueryContext(ctx, `SELECT id, identity, project, area, token, source, account, grant_account
		FROM github_admissions WHERE grant_account = ? AND project = ? AND grant_area = ? AND revoked IS NULL`, grantAccount, project, grantArea)
	if err != nil {
		return 0, err
	}
	var all []admission
	for rows.Next() {
		var a admission
		if err := rows.Scan(&a.id, &a.identity, &a.project, &a.area, &a.token, &a.src, &a.account, &a.grantAccount); err != nil {
			rows.Close()
			return 0, err
		}
		all = append(all, a)
	}
	rows.Close()
	for _, a := range all {
		if err := s.revokeAdmission(ctx, a, why); err != nil {
			return 0, err
		}
	}
	return len(all), nil
}

func (s *Service) revokeAdmission(ctx context.Context, a admission, why string) error {
	if _, err := s.d.Projects.RevokeToken(ctx, a.token); err != nil && !api.IsCode(err, api.CodeNotFound) {
		return err
	}
	if _, err := s.d.Projects.RevokeAdmitted(ctx, a.identity, a.project, a.area, a.src); err != nil {
		return err
	}
	if _, err := s.d.Store.Registry().ExecContext(ctx, `UPDATE github_admissions SET revoked = ? WHERE id = ?`, store.Millis(time.Now()), a.id); err != nil {
		return err
	}
	if who, err := s.d.Projects.Identity(ctx, a.identity); err == nil {
		s.tell(ctx, who, []string{a.project}, fmt.Sprintf("Identity %s lost what a GitHub grant gave it in %s (its node-bound token from that join "+
			"is revoked): %s.", who.Name, s.d.Projects.Slug(a.project), why))
	}
	return nil
}

// Recheck checks every live admission an org grant gave: GitHub, asked
// with the authority's lookup token, must still count its user a member of
// the org (looked up by id, under its current login). Only what GitHub
// positively reports as ended (not a member any more; the org or the
// account gone) removes an admission. A check that cannot run (the token
// expired, revoked, unable to see the members, rate limited) removes
// nothing: the project's owners are mailed once, and after OrgFailGrace of
// failures new joins through org grants stop (nobody is revoked) until a
// check runs again.
func (s *Service) Recheck(ctx context.Context) error {
	rows, err := s.d.Store.Registry().QueryContext(ctx, `SELECT id, identity, project, area, token, source, account, grant_account
		FROM github_admissions WHERE grant_kind = ? AND revoked IS NULL`, KindOrg)
	if err != nil {
		return err
	}
	var all []admission
	for rows.Next() {
		var a admission
		if err := rows.Scan(&a.id, &a.identity, &a.project, &a.area, &a.token, &a.src, &a.account, &a.grantAccount); err != nil {
			rows.Close()
			return err
		}
		all = append(all, a)
	}
	rows.Close()
	failed, ran := map[string]string{}, map[string]bool{}
	for _, a := range all {
		ended, why, err := s.checkOrg(ctx, a)
		switch {
		case err != nil:
			if _, seen := failed[a.project]; !seen {
				failed[a.project] = err.Error()
			}
			s.d.Log.Warn("owners: an org membership check could not run", "admission", a.id, "err", err)
		case ended:
			ran[a.project] = true
			if err := s.revokeAdmission(ctx, a, why); err != nil {
				s.d.Log.Warn("owners: revoking an ended org membership", "admission", a.id, "err", err)
			}
		default:
			ran[a.project] = true
			_, _ = s.d.Store.Registry().ExecContext(ctx, `UPDATE github_admissions SET checked = ? WHERE id = ?`, store.Millis(time.Now()), a.id)
		}
	}
	for p := range ran {
		if _, bad := failed[p]; !bad {
			s.orgChecksRan(ctx, p)
		}
	}
	for p, reason := range failed {
		s.orgChecksFailing(ctx, p, reason)
	}
	return nil
}

// checkOrg asks GitHub whether a's org membership ended (and why); an
// error is a check that could not run.
func (s *Service) checkOrg(ctx context.Context, a admission) (bool, string, error) {
	org, err := s.acc.ByID(ctx, a.grantAccount)
	if errors.Is(err, ErrNotFound) {
		return true, "the GitHub org is gone", nil
	} else if err != nil {
		return false, "", err
	}
	user, err := s.acc.ByID(ctx, a.account)
	if errors.Is(err, ErrNotFound) {
		return true, "the GitHub account is gone", nil
	} else if err != nil {
		return false, "", err
	}
	ok, err := s.acc.OrgMember(ctx, org.Login, user.Login)
	if err != nil {
		return false, "", err
	}
	return !ok, "@" + user.Login + " is no longer a member of the GitHub org @" + org.Login, nil
}

// orgChecksRan records that project's org checks ran again: a later
// failure is told (or logged) afresh.
func (s *Service) orgChecksRan(ctx context.Context, project string) {
	_, _ = s.d.Store.Registry().ExecContext(ctx, `DELETE FROM github_org_checks WHERE project = ?`, project)
	s.orgUntold.Delete(project)
}

// orgChecksFailing records that project's org checks could not run, and
// tells its owners (core.Deps.Notify) the first time.
func (s *Service) orgChecksFailing(ctx context.Context, project, reason string) {
	now := store.Millis(time.Now())
	db := s.d.Store.Registry()
	if _, err := db.ExecContext(ctx, `INSERT INTO github_org_checks (project, failing_since, reason) VALUES (?, ?, ?)
		ON CONFLICT (project) DO UPDATE SET reason = excluded.reason`, project, now, reason); err != nil {
		s.d.Log.Warn("owners: recording failing org checks", "err", err)
		return
	}
	var mailed int
	if db.QueryRowContext(ctx, `SELECT mailed FROM github_org_checks WHERE project = ?`, project).Scan(&mailed) != nil || mailed != 0 {
		return
	}
	if _, logged := s.orgUntold.Load(project); logged {
		return // no notifier: logged once while the checks keep failing
	}
	ref := api.ProjectRef{ID: project, Slug: s.d.Projects.Slug(project)}
	body := fmt.Sprintf("GitHub org membership checks failing for %s: %s. Nobody is removed while they fail; after %s, new joins through "+
		"org grants stop until a check runs again. Fix github.lookup_token (a secret reference to an org member's token, scope read:org), or with none set, install runners.github's App on the org with Members read.",
		ref.Slug, reason, s.OrgFailGrace)
	switch err := s.d.Notify(ctx, core.Notice{Project: ref, To: "owners", Body: body}); {
	case err == nil:
		_, _ = db.ExecContext(ctx, `UPDATE github_org_checks SET mailed = 1 WHERE project = ?`, project)
	case errors.Is(err, core.ErrNoNotifier):
		s.orgUntold.Store(project, true)
	}
}

// orgJoinsFrozen says whether project's org checks have failed for longer
// than OrgFailGrace: new joins through org grants wait for a check.
func (s *Service) orgJoinsFrozen(ctx context.Context, project string) bool {
	var since int64
	if s.d.Store.Registry().QueryRowContext(ctx, `SELECT failing_since FROM github_org_checks WHERE project = ?`, project).Scan(&since) != nil {
		return false
	}
	return time.Since(store.FromMillis(since)) >= s.OrgFailGrace
}

// Start re-checks org admissions every RecheckEvery, and works the queue of
// rate-limited grants (grantqueue.go), until ctx ends.
func (s *Service) Start(ctx context.Context) error {
	go s.runGrantQueue(ctx)
	go func() {
		t := time.NewTicker(s.RecheckEvery)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				_ = s.Recheck(ctx)
			}
		}
	}()
	return nil
}
