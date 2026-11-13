package runners

// Connected repositories (potluck runners allow|disallow|repos): the broker
// serves a job only of a repository its pool's owner connected, and a public
// repository's only with the owner's yes for it. The record of that yes is
// runners_repos, listed at GET /v0/runners/repos, keyed by name and holding
// GitHub's repository id: a repository that takes a connected one's name
// (the old one renamed or deleted) is not connected. Its rows also hold what
// GitHub last said about the repository: its visibility and, for a public
// one, the approval rule for outside contributors' runs, read at the yes and
// again every RepoCheckEvery (a failed read backs off). What is served is
// decided on GitHub's answer just before each runner is minted (ready), not
// on these rows.

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/sengirunners/potluck/ghapp"
	"github.com/sengirunners/potluck/jobrule"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/store"
)

var repoMigrations = []store.Migration{
	// A repository's jobs served before this table existed came from every
	// repository of a linked installation, public ones only under the old
	// global switch: each repository with a job the broker took (not
	// ignored) is connected, without a yes for public use. A public one is
	// refused until its owner gives it. Such a row has no repository id
	// (repo_id 0) until GitHub's API names it (ready, checkRepo).
	{Kind: store.KindRegistry, Name: "runners/0008_repos", Script: `CREATE TABLE runners_repos (
	repo         TEXT PRIMARY KEY COLLATE NOCASE,
	repo_id      INTEGER NOT NULL DEFAULT 0,
	installation INTEGER NOT NULL,
	public       INTEGER NOT NULL DEFAULT 0,
	by           TEXT NOT NULL,
	created      INTEGER NOT NULL,
	visibility   TEXT NOT NULL DEFAULT '',
	approval     TEXT NOT NULL DEFAULT '',
	approval_err TEXT NOT NULL DEFAULT '',
	checked      INTEGER NOT NULL DEFAULT 0,
	tried        INTEGER NOT NULL DEFAULT 0,
	fails        INTEGER NOT NULL DEFAULT 0
);
INSERT OR IGNORE INTO runners_repos (repo, installation, by, created)
	SELECT j.repo, j.installation, 'upgrade', max(j.created) FROM runners_jobs j JOIN runners_links l ON l.installation = j.installation
	WHERE j.state != 'ignored' GROUP BY j.repo COLLATE NOCASE;`},
}

// Repo is one connected repository (GET /v0/runners/repos).
type Repo struct {
	Repo         string `json:"repo"` // owner/name as GitHub names it
	Installation int64  `json:"installation_id"`
	// Public is the pool owner's yes to serve it while it is public: its own
	// branches only (a fork's run is never served for a public repository).
	Public bool   `json:"public"`
	By     string `json:"by"` // who connected it ("upgrade": it had jobs here before connecting existed)
	Since  string `json:"since"`
	// Visibility is GitHub's at Checked: private, internal or public ("" not
	// read yet).
	Visibility string `json:"visibility,omitempty"`
	// Approval is GitHub's approval rule for outside contributors' runs at
	// Checked (ghapp.Approve*), read for a public repository only.
	Approval string `json:"approval,omitempty"`
	// Warning says, in plain words, why the repository's settings are weaker
	// than this broker wants, or that they could not be read.
	Warning string `json:"warning,omitempty"`
	Checked string `json:"checked,omitempty"`
}

// AllowRequest is POST /v0/runners/repos.
type AllowRequest struct {
	Repo   string `json:"repo"`             // owner/name
	Public bool   `json:"public,omitempty"` // the yes a public repository needs
}

// allowHint is the command that connects repo.
func (s *Service) allowHint(repo string, public bool) string {
	if public {
		return s.cmd("runners allow " + repo + " --public")
	}
	return s.cmd("runners allow " + repo)
}

// connected answers why d's repository is not served ("" if it may be): not
// connected, connected through another installation, another repository
// under the connected one's name, or public without the owner's yes. now is
// GitHub's API answer for it (nil: d is a delivery, GitHub's word as of when
// it was sent, which ready asks again before a runner is minted). A row the
// upgrade made has no repository id: now fills it, a delivery passes it.
func (s *Service) connected(ctx context.Context, d delivery, now *ghapp.Repo) (string, error) {
	if now != nil {
		d = d.withRepo(*now)
	}
	name, id, private := d.Repo.FullName, d.Repo.ID, !d.public()
	if id == 0 {
		return "GitHub did not name the repository's id: not served", nil
	}
	var inst, rid int64
	var public bool
	err := s.db().QueryRowContext(ctx, `SELECT installation, public, repo_id FROM runners_repos WHERE repo = ?`, name).Scan(&inst, &public, &rid)
	if errors.Is(err, sql.ErrNoRows) {
		var was string
		err = s.db().QueryRowContext(ctx, `SELECT repo FROM runners_repos WHERE repo_id = ?`, id).Scan(&was)
		if err == nil {
			return name + " was connected as " + was + ", a name it no longer has (" + s.allowHint(name, !private) + ")", nil
		} else if !errors.Is(err, sql.ErrNoRows) {
			return "", err
		}
		return name + " is not connected to this pool (" + s.allowHint(name, !private) + ")", nil
	}
	switch {
	case err != nil:
		return "", err
	case inst != d.Installation.ID:
		return name + " was connected through another installation of the App (" + s.allowHint(name, !private) + " again)", nil
	case rid != 0 && rid != id:
		return name + " is another repository than the one connected under that name (that one was renamed or deleted since): " +
			s.allowHint(name, !private) + " connects this one", nil
	case rid == 0 && now != nil:
		if _, err := s.db().ExecContext(ctx, `UPDATE runners_repos SET repo_id = ? WHERE repo = ? AND repo_id = 0`, id, name); err != nil {
			return "", err
		}
	}
	if !private && !public {
		kind, _ := ghapp.Audience(d.Repo.Private, d.Repo.Visibility)
		return ghapp.Article(kind) + " " + kind + " repository its owner has not switched on: its own branches are served after " + s.allowHint(name, true), nil
	}
	return "", nil
}

// Trust levels (runners_jobs.trusted): what a job's run passed (trust).
// trustAll is every check of trusted, which holds whatever the repository or
// allow_fork_prs become; trustForkPRs is the repository and nothing else,
// passed only where forkPRs served runs from forks and of every event (or
// before levels were recorded), so it holds only while forkPRs still does.
const (
	trustNone    = 0
	trustForkPRs = 1
	trustAll     = 2
)

// ready answers d as GitHub has its repository now and why its next runner
// is not minted ("" if it may be): it is checked again as at the first
// serve (connected), and its run again unless it passed at trustAll (a
// level decided under fork serving holds only while that does: the
// repository may have gone public, or allow_fork_prs been switched off,
// while it waited). launch asks it before every mint, first serves and
// re-serves alike, and makes the runner's guard (guard) from the answer; an
// error (GitHub did not answer) is retried, never minted past.
func (s *Service) ready(ctx context.Context, cl *ghapp.Client, d delivery) (delivery, string, error) {
	if until, low := cl.RateLow(d.Installation.ID); low {
		return d, "", s.wait(ctx, d, "GitHub's API budget for this installation is nearly spent: waiting for its reset at "+
			until.UTC().Format("15:04 UTC"), until)
	}
	gr, err := s.repoNow(ctx, cl, d.Installation.ID, d.Repo.FullName)
	if err != nil {
		return d, "", err
	}
	d = d.withRepo(gr)
	if why, err := s.connected(ctx, d, &gr); err != nil || why != "" {
		return d, why, err
	}
	j, err := s.job(ctx, d.Job.ID)
	if err != nil {
		return d, "", err
	}
	if j.trusted == trustAll {
		return d, "", nil
	}
	why, level, err := s.trusted(ctx, cl, d)
	if err != nil || why != "" {
		return d, why, err
	}
	_, err = s.db().ExecContext(ctx, `UPDATE runners_jobs SET trusted = ? WHERE job = ?`, level, d.Job.ID)
	return d, "", err
}

// poolOwner checks that the caller owns the pool project, for what (a
// sentence's object: "changing the runner broker's settings").
func (s *Service) poolOwner(r *http.Request, what string) (api.ProjectRef, *authz.Subject, error) {
	p, err := s.pool()
	if err != nil {
		return p, nil, err
	}
	sub := core.SubjectOf(r.Context())
	if err := sub.Check(authz.Read, authz.Object{Project: p.ID}, "project", p.Slug); err != nil {
		return p, nil, err
	}
	if sub.Scope.Role(p.ID) != authz.Owner {
		return p, nil, api.Errorf(api.CodeForbidden, "%s takes an owner of %s", what, p.Slug)
	}
	return p, sub, nil
}

// allow is POST /v0/runners/repos (local): the pool's owner connects a
// repository of a linked installation. A public one needs Public, the yes
// for its own branches. It answers the record, with GitHub's approval rule
// for outside contributors read now.
func (s *Service) allow(r *http.Request) (any, error) {
	ctx := r.Context()
	var req AllowRequest
	if err := api.DecodeJSON(r, &req); err != nil {
		return nil, err
	}
	req.Repo = strings.TrimSpace(req.Repo)
	if !jobrule.ValidRepo(req.Repo) {
		return nil, api.Errorf(api.CodeBadRequest, "repo %q: owner/name", req.Repo)
	}
	p, sub, err := s.poolOwner(r, "connecting a repository to the runner broker")
	if err != nil {
		return nil, err
	}
	cl, err := s.client(ctx)
	if err != nil {
		return nil, err
	}
	inst, err := cl.RepoInstallation(ctx, req.Repo)
	if ghapp.NotFound(err) {
		return nil, api.Errorf(api.CodeNotFound, "the App %s is not installed on %s, or not given that repository", s.conf().GitHub.Slug, req.Repo).
			WithHint(s.cmd("runners link") + ": install it there, with " + req.Repo + " among its repositories")
	} else if err != nil {
		return nil, err
	}
	l, err := s.link(ctx, inst)
	if errors.Is(err, sql.ErrNoRows) || err == nil && l.project != p.ID {
		return nil, api.Errorf(api.CodeConflict, "the App's installation %d on %s is not linked to this pool", inst, req.Repo).
			WithHint(s.cmd("runners link " + itoa(inst)))
	} else if err != nil {
		return nil, err
	}
	gr, err := s.repoNow(ctx, cl, inst, req.Repo)
	if err != nil {
		return nil, err
	}
	switch {
	case gr.Public() && !req.Public:
		kind, who := ghapp.Audience(gr.Private, gr.Visibility)
		return nil, api.Errorf(api.CodeBadRequest, "%s is %s: %s can propose code to it", gr.FullName, kind, who).
			WithHint(s.allowHint(gr.FullName, true) + " serves its own branches (pushes, manual runs, schedules, pull requests from its own branches); a fork's pull request is never served")
	case !gr.Public() && req.Public:
		// The yes is given while the repository is public, never ahead of it.
		// Once given it stays on the record: a repository made private and
		// public again is served again without a new yes, until the owner
		// allow for it while it is private (which records no yes) or disallow.
		return nil, api.Errorf(api.CodeBadRequest, "%s is private: --public is a yes given once it is public, not before", gr.FullName).
			WithHint(s.allowHint(gr.FullName, false) + " connects it now; if it is made public, its jobs are refused until " + s.allowHint(gr.FullName, true))
	}
	now := s.now().Unix()
	// One record per repository: a row of its id under an older name goes.
	if _, err := s.db().ExecContext(ctx, `DELETE FROM runners_repos WHERE repo_id = ? AND repo != ?`, gr.ID, gr.FullName); err != nil {
		return nil, err
	}
	if _, err := s.db().ExecContext(ctx, `INSERT INTO runners_repos (repo, repo_id, installation, public, by, created) VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(repo) DO UPDATE SET repo = excluded.repo, repo_id = excluded.repo_id, installation = excluded.installation, public = excluded.public,
		by = excluded.by, created = excluded.created, visibility = '', approval = '', approval_err = '', checked = 0, tried = 0, fails = 0`,
		gr.FullName, gr.ID, inst, req.Public, sub.Address, now); err != nil {
		return nil, err
	}
	s.checkRepo(ctx, cl, gr.FullName, inst, &gr)
	return s.repo(ctx, gr.FullName)
}

// disallow is DELETE /v0/runners/repos/{owner}/{name} (local): the pool's
// owner disconnects a repository. Its jobs still waiting are refused at
// their next try; one that runs keeps its box until it ends.
func (s *Service) disallow(r *http.Request) (any, error) {
	repo := r.PathValue("owner") + "/" + r.PathValue("name")
	if !jobrule.ValidRepo(repo) {
		return nil, api.Errorf(api.CodeBadRequest, "repo %q: owner/name", repo)
	}
	if _, _, err := s.poolOwner(r, "disconnecting a repository from the runner broker"); err != nil {
		return nil, err
	}
	res, err := s.db().ExecContext(r.Context(), `DELETE FROM runners_repos WHERE repo = ?`, repo)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, api.NotFound("connected repository", repo)
	}
	return map[string]string{"repo": repo}, nil
}

// repos is GET /v0/runners/repos: the connected repositories of links in the
// caller's readable areas of the pool project.
func (s *Service) repos(r *http.Request) (any, error) {
	ctx := r.Context()
	p, err := s.pool()
	if err != nil {
		return nil, err
	}
	areaSQL, args := core.SubjectOf(ctx).Scope.AreaSQL(p.ID, "l.area", authz.Read)
	rows, err := s.db().QueryContext(ctx, `SELECT `+repoCols+` FROM runners_repos r JOIN runners_links l ON l.installation = r.installation
		WHERE l.project = ? AND `+areaSQL+` ORDER BY r.repo`, append([]any{p.ID}, args...)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Repo{}
	for rows.Next() {
		v, err := scanRepo(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return map[string]any{"repos": out}, rows.Err()
}

const repoCols = `r.repo, r.installation, r.public, r.by, r.created, r.visibility, r.approval, r.approval_err, r.checked`

func scanRepo(row interface{ Scan(...any) error }) (Repo, error) {
	var v Repo
	var created, checked int64
	var approvalErr string
	if err := row.Scan(&v.Repo, &v.Installation, &v.Public, &v.By, &created, &v.Visibility, &v.Approval, &approvalErr, &checked); err != nil {
		return v, err
	}
	v.Since = time.Unix(created, 0).UTC().Format(time.RFC3339)
	if checked > 0 {
		v.Checked = time.Unix(checked, 0).UTC().Format(time.RFC3339)
	}
	v.Warning = approvalWarning(v.Visibility, v.Approval, approvalErr)
	return v, nil
}

// repo is one record as GET /v0/runners/repos shows it.
func (s *Service) repo(ctx context.Context, name string) (Repo, error) {
	return scanRepo(s.db().QueryRowContext(ctx, `SELECT `+repoCols+` FROM runners_repos r WHERE r.repo = ?`, name))
}

// visibilityOf is gr's visibility as GitHub named it, else ("": a server
// before visibility) what its private flag says.
func visibilityOf(gr ghapp.Repo) string {
	if gr.Visibility != "" {
		return gr.Visibility
	}
	return map[bool]string{true: "private", false: "public"}[gr.Private]
}

// approvalWarning is the plain-words warning for a repository whose
// approval rule for outside contributors' runs is weaker than "all outside
// contributors" (or could not be read), "" for one that is not public or
// whose rule is that.
func approvalWarning(visibility, approval, readErr string) string {
	if visibility != "public" {
		return ""
	}
	const why = "Their runs are never served here, but each can take a runner meant for your own job and cost it its start."
	const fix = " To make every outside contributor wait for your approval: the repository's Settings > Actions > General > Approval for running fork pull request workflows from contributors > Require approval for all external contributors."
	switch approval {
	case ghapp.ApproveAll:
		return ""
	case ghapp.ApproveFirstTime:
		return "Outside contributors who have contributed before can start workflow runs on this public repository without your approval. " + why + fix
	case ghapp.ApproveNewToGitHub:
		return "Outside contributors can start workflow runs on this public repository without your approval, unless their GitHub account is new. " + why + fix
	case "":
		return "GitHub did not say whether outside contributors' workflow runs on this public repository wait for your approval (" + readErr + "). " + why + fix
	}
	return "GitHub answered an approval rule for outside contributors this version does not know (" + approval + "): take it as weaker than requiring approval for all of them. " + why + fix
}

// checkRepos reads, for each connected repository due (RepoCheckEvery after
// its last read; after a failed one, RepoCheckEvery doubled per failure in a
// row, up to 16 times), its visibility and (public) its approval rule for
// outside contributors' runs. An installation whose API budget is nearly
// spent waits for the next sweep. A sweep runs it.
func (s *Service) checkRepos(ctx context.Context, cl *ghapp.Client) {
	rows, err := s.db().QueryContext(ctx, `SELECT repo, installation, checked, tried, fails FROM runners_repos`)
	if err != nil {
		return
	}
	type due struct {
		repo                 string
		inst, checked, tried int64
		fails                int
	}
	now := s.now()
	var all []due
	for rows.Next() {
		var d due
		if rows.Scan(&d.repo, &d.inst, &d.checked, &d.tried, &d.fails) != nil {
			continue
		}
		last, gap := d.checked, s.RepoCheckEvery
		if d.fails > 0 {
			last, gap = d.tried, s.RepoCheckEvery<<min(d.fails-1, 4)
		}
		if !time.Unix(last, 0).Add(gap).After(now) {
			all = append(all, d)
		}
	}
	rows.Close()
	for _, d := range all {
		if _, low := cl.RateLow(d.inst); low {
			continue
		}
		if _, err := s.link(ctx, d.inst); err != nil {
			continue
		}
		s.checkRepo(ctx, cl, d.repo, d.inst, nil)
	}
}

// checkRepo reads repo's visibility (unless gr is GitHub's answer already)
// and, when it is public, its approval rule, and records both. A rule weaker
// than "all outside contributors", or one that could not be read, is logged
// in plain words; GET /v0/runners/repos shows it. A read that fails, or that
// answers another repository than the one connected, is recorded as a
// failure (backed off) and logged.
func (s *Service) checkRepo(ctx context.Context, cl *ghapp.Client, repo string, inst int64, gr *ghapp.Repo) {
	now := s.now().Unix()
	failed := func(why string) {
		var fails int
		if s.db().QueryRowContext(ctx, `UPDATE runners_repos SET tried = ?, fails = fails + 1 WHERE repo = ? RETURNING fails`, now, repo).Scan(&fails) == nil {
			next := s.RepoCheckEvery << min(fails-1, 4)
			s.logf("runners: %s: %s (read again in %s)", repo, why, next)
		}
	}
	if gr == nil {
		r, err := s.repoNow(ctx, cl, inst, repo)
		switch {
		case ghapp.NotFound(err):
			failed("GitHub does not find it (deleted, or the App no longer has it); its jobs are not served")
			return
		case err != nil:
			failed("GitHub did not answer its visibility: " + oneLine(err.Error()))
			return
		}
		gr = &r
	}
	var rid int64
	if err := s.db().QueryRowContext(ctx, `SELECT repo_id FROM runners_repos WHERE repo = ?`, repo).Scan(&rid); err != nil {
		return
	}
	if rid != 0 && rid != gr.ID {
		failed("the repository GitHub has under this name is another one than the one connected (renamed or deleted since); its jobs are not served")
		return
	}
	vis := visibilityOf(*gr)
	var approval, readErr string
	if vis == "public" {
		var err error
		if approval, err = cl.ForkPRApproval(ctx, inst, repo); err != nil {
			readErr = oneLine(err.Error())
		}
	}
	if _, err := s.db().ExecContext(ctx, `UPDATE runners_repos SET repo_id = ?, visibility = ?, approval = ?, approval_err = ?, checked = ?, tried = ?, fails = 0
		WHERE repo = ?`, gr.ID, vis, approval, readErr, now, now, repo); err != nil {
		return
	}
	if w := approvalWarning(vis, approval, readErr); w != "" {
		s.logf("runners: %s: %s", repo, w)
	}
}

// dropRepos disconnects the repositories connected through installation.
func (s *Service) dropRepos(ctx context.Context, installation int64) error {
	_, err := s.db().ExecContext(ctx, `DELETE FROM runners_repos WHERE installation = ?`, installation)
	return err
}

// connectedOf is the repositories connected through installation, by name:
// the only ones the checker asks GitHub about.
func (s *Service) connectedOf(ctx context.Context, installation int64) ([]string, error) {
	rows, err := s.db().QueryContext(ctx, `SELECT repo FROM runners_repos WHERE installation = ? ORDER BY repo`, installation)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var repo string
		if err := rows.Scan(&repo); err != nil {
			return nil, err
		}
		out = append(out, repo)
	}
	return out, rows.Err()
}
