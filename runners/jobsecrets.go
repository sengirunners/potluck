package runners

// Job grants (docs/daemon.md "Job grants"; the secrets door's runner side).
// A job of a repository that a job grant of a minted kind names gets that
// credential in its box, and no other job ever runs a step in that box:
//
//   - At serve (launch), before the runner is minted, the broker asks GitHub
//     about the job's run (jobOf) and keeps the grants that name it. If any
//     do, the box's guard is narrowed to that run, its workflow and its ref,
//     and holds the job until the door's ready marker (protocol.JITRun);
//     nothing is minted yet. A driver that cannot hold a job refuses the
//     acquire, so such a job is never served on one.
//   - When GitHub says (workflow_job in_progress) that a job of that run
//     started on the box's runner (its own job, or another of the run that
//     took it), the broker asks GitHub again, issues each grant's
//     credential into the box (issuers), then writes the ready marker. A
//     job of another run gets nothing: the hook stops it.
//   - Any failure (GitHub not answering, a grant gone, an issue refused, the
//     marker not written) revokes what the box got and writes no marker:
//     the hook stops the job before its first step.
//   - Revocation: every end of the box (released, expired, lost, ...)
//     reaches the issuer through the run targets' end hook, and the
//     completion of the job that held the box ends its credentials at once
//     (endJobSecrets).
//
// Every fact about the job is GitHub's answer (the signed webhook, the run,
// the repository, the compare of its commit with the default branch), never
// the box's or the workflow file's word.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/ghapp"
	"github.com/sengirunners/potluck/grants"
	"github.com/sengirunners/potluck/issuers"
	"github.com/sengirunners/potluck/jobrule"
	"github.com/sengirunners/potluck/kit/secrets"
	"github.com/sengirunners/potluck/kit/store"
	"github.com/sengirunners/potluck/protocol"
	"github.com/sengirunners/potluck/runtarget/rt"
)

var jobSecretMigrations = []store.Migration{
	// One row per job served under job grants: the run its box is guarded
	// to, and what the door did for it.
	{Kind: store.KindRegistry, Name: "runners/0011_job_secrets", Script: `CREATE TABLE runners_job_secrets (
	job INTEGER PRIMARY KEY,
	project TEXT NOT NULL,
	area TEXT NOT NULL,
	run INTEGER NOT NULL,
	workflow TEXT NOT NULL,
	ref TEXT NOT NULL,
	handle TEXT NOT NULL DEFAULT '',
	holder INTEGER NOT NULL DEFAULT 0,
	state TEXT NOT NULL,
	why TEXT NOT NULL DEFAULT '',
	updated INTEGER NOT NULL
);`},
	// One row per serve, not per job: a job served again (a new box) gets a
	// new row while the row of its earlier box stays, so the job of its run
	// that took that box still finds it at its completion (endJobSecrets).
	{Kind: store.KindRegistry, Name: "runners/0012_job_secrets_serves", Script: `CREATE TABLE runners_job_secrets_serves (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	job INTEGER NOT NULL,
	project TEXT NOT NULL,
	area TEXT NOT NULL,
	run INTEGER NOT NULL,
	workflow TEXT NOT NULL,
	ref TEXT NOT NULL,
	handle TEXT NOT NULL DEFAULT '',
	holder INTEGER NOT NULL DEFAULT 0,
	state TEXT NOT NULL,
	why TEXT NOT NULL DEFAULT '',
	updated INTEGER NOT NULL
);
INSERT INTO runners_job_secrets_serves (job, project, area, run, workflow, ref, handle, holder, state, why, updated)
	SELECT job, project, area, run, workflow, ref, handle, holder, state, why, updated FROM runners_job_secrets;
DROP TABLE runners_job_secrets;
ALTER TABLE runners_job_secrets_serves RENAME TO runners_job_secrets;
CREATE INDEX runners_job_secrets_job ON runners_job_secrets (job);
CREATE INDEX runners_job_secrets_holder ON runners_job_secrets (holder, state);
CREATE INDEX runners_job_secrets_handle ON runners_job_secrets (project, handle);
CREATE INDEX runners_job_secrets_updated ON runners_job_secrets (updated);`},
	// The jobs GitHub said completed: a stale in_progress of one, applied
	// after its completion while GitHub's API still shows it running,
	// claims a box's row only to give it up (issueSecrets).
	{Kind: store.KindRegistry, Name: "runners/0013_job_secrets_completed", Script: `CREATE TABLE runners_job_secrets_completed (
	job INTEGER PRIMARY KEY,
	updated INTEGER NOT NULL
);
CREATE INDEX runners_job_secrets_completed_updated ON runners_job_secrets_completed (updated);`},
}

// Job secrets' states.
const (
	secretsPlanned = "planned" // the box is guarded to the run; nothing minted
	secretsIssuing = "issuing" // one issueSecrets holds it and is minting
	secretsIssued  = "issued"  // every credential and the marker are in the box
	secretsFailed  = "failed"  // nothing is left in the box; the hook stops the job
	secretsEnded   = "ended"   // the job completed: revoked
)

// A row is held while it is issuing or issued: a job (its own, or one of its
// run that took its runner) has its box's credentials, and that job's
// completion must still find the row to revoke them.
const secretsHeld = "('" + secretsIssuing + "', '" + secretsIssued + "')"

// jobOf is d's job as GitHub answers it now: its run (repository and head
// repository with their ids, workflow file, event, head branch and commit,
// the workflows it called) and the repository (its id and default branch).
// Ref is set only when the head branch is the default branch, the head
// commit is on it (GitHub's compare), no tag carries the branch's name, a
// ruleset the App cannot bypass requires a pull request on it
// (ghapp.Reviewed), and every uses: the workflow pulls in is pinned
// (unpinnedUses):
// a tag named like the branch, or a commit force-pushed away, gets none. A
// re-run (run_attempt above 1) gets one only while its commit is still the
// branch's tip: anyone who can re-run could otherwise run an old commit's
// workflow file, one since fixed, with a fresh credential. A
// run that called a reusable workflow from anywhere but its own repository
// at its own commit gets none either: a grant naming a workflow file
// trusts only that file's commit. jobrule.Check then refuses whatever is
// missing or wrong. The ref is asked about only when one of candidates
// would name the run on the default branch, so a repository's other jobs
// never wait on those calls.
func (s *Service) jobOf(ctx context.Context, cl *ghapp.Client, d delivery, candidates []computeapi.SecretGrant) (jobrule.Job, error) {
	run, err := cl.Run(ctx, d.Installation.ID, d.Repo.FullName, d.Job.RunID)
	if err != nil {
		return jobrule.Job{}, err
	}
	repo, err := cl.Repo(ctx, d.Installation.ID, d.Repo.FullName)
	if err != nil {
		return jobrule.Job{}, err
	}
	j := jobrule.Job{Repo: run.Repo, HeadRepo: run.HeadRepo, HeadRepoID: run.HeadRepoID, Workflow: run.Path,
		DefaultBranch: repo.DefaultBranch, Event: run.Event, RunID: run.ID}
	if run.RepoID == repo.ID && run.ID == d.Job.RunID && strings.EqualFold(run.Repo, repo.FullName) {
		j.RepoID = run.RepoID
	}
	if !run.CallsOwnOnly {
		j.Workflow = "" // jobrule refuses it
	}
	if repo.DefaultBranch == "" || run.HeadBranch != repo.DefaultBranch {
		return j, nil
	}
	maybe := j
	maybe.Ref = "refs/heads/" + repo.DefaultBranch
	if len(matching(issuers.From(s.d), candidates, maybe)) == 0 {
		return j, nil
	}
	on, tip, err := cl.OnBranch(ctx, d.Installation.ID, d.Repo.FullName, repo.DefaultBranch, run.HeadSHA)
	if err != nil || !on {
		return j, err
	}
	if run.Attempt < 1 {
		return j, nil
	}
	if run.Attempt > 1 && !tip {
		// A re-run of an older commit: its workflow file may be one since
		// fixed. The broker's own re-runs of lost jobs are re-runs too.
		s.logf("runners: job %d of %s gets no job grant: run %d is attempt %d and its commit is no longer the tip of %s",
			d.Job.ID, d.Repo.FullName, run.ID, run.Attempt, repo.DefaultBranch)
		return j, nil
	}
	tag, err := cl.TagExists(ctx, d.Installation.ID, d.Repo.FullName, repo.DefaultBranch)
	if err != nil || tag {
		return j, err
	}
	// The default branch counts as reviewed only when a ruleset this App
	// cannot bypass requires a pull request on it, and the run's workflow
	// trusts only code that cannot move (pinned.go).
	reviewed, why, err := cl.Reviewed(ctx, d.Installation.ID, d.Repo.FullName, repo.DefaultBranch)
	if err != nil {
		return j, err
	}
	if !reviewed {
		s.logf("runners: job %d of %s gets no job grant: %s", d.Job.ID, d.Repo.FullName, why)
		return j, nil
	}
	why, err = unpinnedUses(ctx, cl, d.Installation.ID, d.Repo.FullName, run.Path, run.HeadSHA)
	if err != nil {
		return j, err
	}
	if why != "" {
		s.logf("runners: job %d of %s gets no job grant: %s", d.Job.ID, d.Repo.FullName, why)
		return j, nil
	}
	j.Ref = "refs/heads/" + repo.DefaultBranch
	return j, nil
}

// matching is the candidates that name job, of a kind an issuer of iss
// mints here (a grant of a kind no issuer runs for is passed over, so it
// never fails the job's other grants).
func matching(iss issuers.Minter, candidates []computeapi.SecretGrant, job jobrule.Job) []computeapi.SecretGrant {
	var out []computeapi.SecretGrant
	if iss == nil {
		return nil
	}
	for _, g := range candidates {
		if iss.Mints(g.Kind) && grants.JobMatches(g, job) {
			out = append(out, g)
		}
	}
	return out
}

// secretsRun is the run d's box is guarded to when job grants name its job
// (nil: none do, and the job is served as any other), and records it as a
// new row. It mints nothing. GitHub not answering is an error: the job
// waits, never served unchecked. Rows of the job's earlier serves that are
// not held go; a held one stays for its holder's completion (secretsHeld).
func (s *Service) secretsRun(ctx context.Context, cl *ghapp.Client, l link, d delivery) (*protocol.JITRun, error) {
	// Served unguarded: no planned row may stay from an earlier serve, or a
	// later in_progress would mint into a box that does not hold the job.
	unguarded := func() (*protocol.JITRun, error) {
		_, err := s.db().ExecContext(ctx, `DELETE FROM runners_job_secrets WHERE job = ? AND state NOT IN `+secretsHeld, d.Job.ID)
		return nil, err
	}
	if issuers.From(s.d) == nil {
		return unguarded()
	}
	candidates, err := grants.JobGrants(ctx, s.d, l.project, l.Area, d.Repo.FullName)
	if err != nil {
		return nil, err
	}
	if len(candidates) == 0 {
		return unguarded()
	}
	job, err := s.jobOf(ctx, cl, d, candidates)
	if err != nil {
		return nil, err
	}
	if len(matching(issuers.From(s.d), candidates, job)) == 0 {
		return unguarded()
	}
	run := &protocol.JITRun{ID: job.RunID, WorkflowRef: job.Repo + "/" + job.Workflow + "@" + job.Ref, Ref: job.Ref, HoldS: holdS}
	if err := run.Check(d.Repo.FullName); err != nil {
		return nil, err
	}
	tx, err := s.db().BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM runners_job_secrets WHERE job = ? AND state NOT IN `+secretsHeld, d.Job.ID); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO runners_job_secrets (job, project, area, run, workflow, ref, state, updated) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		d.Job.ID, l.project, l.Area, job.RunID, job.Workflow, job.Ref, secretsPlanned, s.now().Unix()); err != nil {
		return nil, err
	}
	return run, tx.Commit()
}

// withRun is guard g narrowed to run (nil: g as it is).
func withRun(g *computeapi.JITGuard, run *protocol.JITRun) *computeapi.JITGuard {
	if run != nil {
		g.Run = run
	}
	return g
}

// holdS is how long a guarded box's hook holds its job for the ready
// marker; issueLimit, how long the broker gives issuing, counted from the
// start of the hold (GitHub's started_at, or the box's acquire if later:
// issueLeft), so a slow issue or a late webhook is given up and revoked
// before the hook stops the job.
const (
	holdS      = 300
	issueLimit = 240 * time.Second
)

// startedOn records that job d started on the runner of job owner (its
// own, or a swap), then issues owner's box's job grants for d. Issuing runs
// beside the delivery lane, so a slow GitHub or issuer never holds up the
// completions (and revocations) queued behind it.
func (s *Service) startedOn(ctx context.Context, d delivery, owner int64) error {
	if err := s.markStarted(ctx, owner, d.Job.ID); err != nil {
		return err
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.issueSecrets(ctx, d, owner)
	}()
	return nil
}

// issueSecrets issues the credentials of job owner's box, which GitHub says
// job d holds now, then the ready marker. d is owner itself, or a job of
// the same run that took its runner (a swap: the box is guarded to the run,
// so any of its jobs may hold it; a job of another run is stopped by the
// hook and gets nothing). GitHub is asked for d itself first (startedHere):
// a late in_progress for a job that has since completed, or that runs
// elsewhere, gets nothing and leaves the row for the job that does start
// there. It fails closed: anything that does not pass revokes what the box
// got and writes no marker.
func (s *Service) issueSecrets(ctx context.Context, d delivery, owner int64) {
	ctx, cancel := context.WithTimeout(ctx, issueLimit)
	defer cancel()
	var id, run int64
	var project, area, workflow, ref, state string
	// The newest row is the job's current serve; older ones are held boxes.
	err := s.db().QueryRowContext(ctx, `SELECT id, project, area, run, workflow, ref, state FROM runners_job_secrets WHERE job = ? ORDER BY id DESC LIMIT 1`, owner).
		Scan(&id, &project, &area, &run, &workflow, &ref, &state)
	if errors.Is(err, sql.ErrNoRows) {
		return // no job grant names it: its box holds nothing
	}
	if err != nil {
		s.logf("runners: job %d: reading its job grants: %v; its box gets nothing", owner, err)
		return
	}
	if state != secretsPlanned {
		return // issuing or issued (a redelivery), failed or ended: never again
	}
	if d.Job.RunID == run {
		gj, why := s.startedHere(ctx, d, owner, run)
		var left time.Duration
		if why == "" {
			left, why = s.issueLeft(ctx, gj, owner)
		}
		if why != "" {
			s.logf("runners: job %d on the runner of job %d gets no secrets (%s)", d.Job.ID, owner, why)
			return
		}
		var cancelHold context.CancelFunc
		ctx, cancelHold = context.WithTimeout(ctx, left)
		defer cancelHold()
	}
	// Claim the row: one issueSecrets per box, however often GitHub
	// redelivers. A job GitHub said completed claims nothing: its
	// completion, applied before (endJobSecrets records it first), found no
	// row held by it, and none would revoke what it got; the row stays for
	// a job that does start there.
	res, err := s.db().ExecContext(ctx, `UPDATE runners_job_secrets SET state = ?, holder = ?, updated = ? WHERE id = ? AND state = ?
		AND NOT EXISTS (SELECT 1 FROM runners_job_secrets_completed WHERE job = ?)`,
		secretsIssuing, d.Job.ID, s.now().Unix(), id, secretsPlanned, d.Job.ID)
	if err != nil {
		s.logf("runners: job %d: claiming its job grants: %v; its box gets nothing", owner, err)
		return
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return
	}
	// Held by this process until this returns, so a completion never takes
	// it for a restart's orphan (its box is recorded on the row only after
	// this, and a row with none is never one).
	s.ownIssuing(id, true)
	defer s.ownIssuing(id, false)
	if d.Job.RunID != run {
		s.secretsFailed(ctx, id, owner, nil, "", "job "+itoa(d.Job.ID)+" of another run took its runner")
		return
	}
	j, err := s.boxOf(ctx, owner, d.Job.RunnerName)
	if err != nil || j.state != jobServing || j.handle == "" || j.runner != d.Job.RunnerName {
		s.secretsFailed(ctx, id, owner, nil, "", "its box is not the one GitHub says the job started on")
		return
	}
	box := issuers.Box{Project: project, Area: area, ID: j.handle}
	fail := func(why string) { s.secretsFailed(ctx, id, owner, &box, j.handle, why) }
	// The box is on the row before anything is minted into it: a completion
	// after a restart finds it there (endJobSecrets).
	res, err = s.db().ExecContext(ctx, `UPDATE runners_job_secrets SET handle = ? WHERE id = ? AND state = ?`, j.handle, id, secretsIssuing)
	if n, err := rowsOf(res, err); err != nil {
		fail("recording its box: " + err.Error())
		return
	} else if n != 1 {
		return // the job completed (endJobSecrets): nothing minted
	}
	iss, door := issuers.From(s.d), rt.DoorFrom(s.d)
	if iss == nil || door == nil {
		fail("this machine mints no secrets now")
		return
	}
	cl, err := s.client(ctx)
	if err != nil {
		fail("no GitHub client: " + err.Error())
		return
	}
	candidates, err := grants.JobGrants(ctx, s.d, project, area, d.Repo.FullName)
	if err != nil {
		fail("reading the grants: " + err.Error())
		return
	}
	job, err := s.jobOf(ctx, cl, d, candidates)
	if err != nil {
		fail("GitHub could not be asked about its run: " + err.Error())
		return
	}
	if job.RunID != run || job.Workflow != workflow || job.Ref != ref {
		fail("its run is not the one its box was guarded to")
		return
	}
	gs := matching(iss, candidates, job)
	if len(gs) == 0 {
		fail("no grant names its run now")
		return
	}
	for _, g := range gs {
		req, err := issuers.JobRequest(g, job, box)
		if err == nil {
			_, err = iss.Issue(ctx, req)
		}
		if err != nil {
			fail("issuing " + g.Name + ": " + err.Error())
			return
		}
	}
	if err := door.BoxSecret(ctx, project, j.handle, protocol.SecretNameReady, secrets.NewValue(strconv.FormatInt(run, 10))); err != nil {
		if errors.Is(err, rt.ErrMaybeDelivered) {
			fail("the ready marker may have reached the box (its credentials are revoked): " + err.Error())
			return
		}
		fail("the ready marker: " + err.Error())
		return
	}
	res, err = s.db().ExecContext(context.WithoutCancel(ctx), `UPDATE runners_job_secrets SET state = ?, handle = ?, holder = ?, why = '', updated = ? WHERE id = ? AND state = ?`,
		secretsIssued, j.handle, d.Job.ID, s.now().Unix(), id, secretsIssuing)
	if n, _ := rowsOf(res, err); n != 1 {
		// The job completed while minting (endJobSecrets), or the row could
		// not be recorded issued: what was just issued must not outlive it.
		if err != nil {
			s.logf("runners: job %d: recording its box's secrets issued: %v; revoking them", d.Job.ID, err)
		} else {
			s.logf("runners: job %d completed while its box's secrets were issued; revoking them", d.Job.ID)
		}
		if err := iss.End(context.WithoutCancel(ctx), box, issuers.EndJobCompleted); err != nil {
			s.logf("runners: job %d: revoking its box's credentials: %v", owner, err)
		}
	}
}

// startedLimit is how long startedHere asks GitHub again while it does not
// show the job on a runner yet (its API can lag the webhook).
const startedLimit = 30 * time.Second

// startedHere is GitHub's answer for job d and "" when it says d is in
// progress on runner d.Job.RunnerName of job owner, in run; else why not.
// It asks again while GitHub shows the job queued or on no runner, for up
// to startedLimit.
func (s *Service) startedHere(ctx context.Context, d delivery, owner, run int64) (ghapp.Job, string) {
	j, err := s.job(ctx, owner)
	if err != nil {
		return ghapp.Job{}, "reading job " + itoa(owner) + ": " + err.Error()
	}
	if j.runner != d.Job.RunnerName {
		return ghapp.Job{}, "its box's runner is " + strconv.Quote(j.runner) + " now"
	}
	cl, err := s.client(ctx)
	if err != nil {
		return ghapp.Job{}, "no GitHub client: " + err.Error()
	}
	ctx, cancel := context.WithTimeout(ctx, startedLimit)
	defer cancel()
	for {
		gj, err := cl.Job(ctx, j.installation, j.repo, d.Job.ID)
		switch {
		case err != nil && ctx.Err() != nil:
			return gj, "GitHub did not show it started within " + startedLimit.String()
		case err != nil:
			return gj, "GitHub could not be asked about it: " + err.Error()
		case runsOn(gj, j.runner, run):
			return gj, ""
		case gj.Status != "queued" && gj.RunnerName != "":
			return gj, "GitHub says it is " + gj.Status + " on runner " + strconv.Quote(gj.RunnerName)
		}
		select {
		case <-ctx.Done():
			return gj, "GitHub still shows it " + gj.Status + " on runner " + strconv.Quote(gj.RunnerName)
		case <-time.After(time.Second):
		}
	}
}

// issueLeft is how long issuing for job gj, on the box of job owner, may
// still take ("" why), or why it may not start. Its box's hook began holding
// the job when it started (GitHub's started_at), not when the broker heard
// of it, so a webhook or a lane running late leaves less of issueLimit. The
// hook cannot have held it before its box was acquired, so a start GitHub
// dates earlier (a time in the queue) counts from the acquire. A start
// GitHub does not give leaves nothing.
func (s *Service) issueLeft(ctx context.Context, gj ghapp.Job, owner int64) (time.Duration, string) {
	t, ok := gj.Started()
	if !ok {
		return 0, "GitHub gives no time it started"
	}
	j, err := s.job(ctx, owner)
	if err != nil {
		return 0, "reading job " + itoa(owner) + ": " + err.Error()
	}
	if j.leased > t.Unix() {
		t = time.Unix(j.leased, 0)
	}
	left := min(issueLimit-s.now().Sub(t), issueLimit) // a start ahead of this clock gets issueLimit
	if left <= 0 {
		return 0, "it started at " + t.UTC().Format(time.RFC3339) + ", too long ago to issue before its box's hook stops it"
	}
	return left, ""
}

// ownIssuing records (on) or forgets that this process's issueSecrets holds
// row id. A row issuing that none holds was left by a process that is gone.
func (s *Service) ownIssuing(id int64, on bool) {
	s.issuingMu.Lock()
	defer s.issuingMu.Unlock()
	if s.issuing == nil {
		s.issuing = map[int64]bool{}
	}
	if on {
		s.issuing[id] = true
	} else {
		delete(s.issuing, id)
	}
}

// runsOn is whether GitHub's gj is in progress on runner, in run: the job
// that holds that runner's box.
func runsOn(gj ghapp.Job, runner string, run int64) bool {
	return gj.Status == "in_progress" && gj.RunnerName == runner && gj.RunID == run
}

// handleWait is how long issueSecrets waits for the serve to record its
// box's handle when GitHub says the job started before the acquire
// returned (inside issueLimit, leaving time to issue).
const handleWait = 90 * time.Second

// boxOf is job owner's record once its box's handle is recorded: an
// in_progress can arrive between the runner starting in the box and the
// serve recording the handle (launch), so a serving job on runner whose
// handle is not recorded yet is read again until it is, or handleWait
// passes.
func (s *Service) boxOf(ctx context.Context, owner int64, runner string) (job, error) {
	ctx, cancel := context.WithTimeout(ctx, handleWait)
	defer cancel()
	t := time.NewTicker(100 * time.Millisecond)
	defer t.Stop()
	for {
		j, err := s.job(ctx, owner)
		if err != nil || j.state != jobServing || j.runner != runner || j.handle != "" {
			return j, err
		}
		select {
		case <-ctx.Done():
			return j, nil
		case <-t.C:
		}
	}
}

// secretsFailed records why row id (of job) gets nothing and revokes what
// its box got.
func (s *Service) secretsFailed(ctx context.Context, id, job int64, box *issuers.Box, handle, why string) {
	s.logf("runners: job %d gets no secrets (%s); its box's hook stops it", job, why)
	if box != nil {
		if iss := issuers.From(s.d); iss != nil {
			if err := iss.End(context.WithoutCancel(ctx), *box, issuers.EndGone); err != nil {
				s.logf("runners: job %d: revoking its box's credentials: %v", job, err)
			}
		}
	}
	if _, err := s.db().ExecContext(context.WithoutCancel(ctx), `UPDATE runners_job_secrets SET state = ?, handle = ?, why = ?, updated = ? WHERE id = ? AND state IN (?, ?, ?)`,
		secretsFailed, handle, clip(why), s.now().Unix(), id, secretsPlanned, secretsIssuing, secretsIssued); err != nil {
		s.logf("runners: job %d: recording why it gets no secrets: %v", job, err)
	}
}

// endJobSecrets revokes the credentials of every box job held at once:
// GitHub says the job completed. That box is its own, or the one of another
// job of its run whose runner it took. Its error is only that the
// completion could not be recorded: the delivery is applied again, and what
// could be revoked already was.
func (s *Service) endJobSecrets(ctx context.Context, job int64) error {
	// Recorded first: an issueSecrets claiming a row for it from now on
	// claims nothing, and one that claimed before is found below.
	_, recErr := s.db().ExecContext(ctx, `INSERT INTO runners_job_secrets_completed (job, updated) VALUES (?, ?) ON CONFLICT(job) DO UPDATE SET updated = excluded.updated`,
		job, s.now().Unix())
	if recErr != nil {
		recErr = fmt.Errorf("recording that job %d completed: %w", job, recErr)
	}
	// A box it holds being issued by a process that is gone (a restart while
	// issuing): no issueSecrets will see the row ended, so it is revoked here.
	orphans := s.orphanedIssuing(ctx, job)
	// Its own box not started on yet, or a box it holds being issued:
	// nothing is minted from now on, and the issueSecrets in flight revokes
	// what it got once it sees the row ended.
	if _, err := s.db().ExecContext(ctx, `UPDATE runners_job_secrets SET state = ?, updated = ? WHERE (job = ? AND state = ?) OR (holder = ? AND state = ?)`,
		secretsEnded, s.now().Unix(), job, secretsPlanned, job, secretsIssuing); err != nil {
		s.logf("runners: job %d: recording that its job completed: %v", job, err)
	}
	for _, b := range orphans {
		s.logf("runners: job %d completed; revoking box %s, whose secrets a process now gone was issuing", job, b.ID)
		if iss := issuers.From(s.d); iss != nil {
			if err := iss.End(context.WithoutCancel(ctx), b, issuers.EndJobCompleted); err != nil {
				s.logf("runners: job %d: revoking its box's credentials: %v", job, err)
			}
		}
	}
	rows, err := s.db().QueryContext(ctx, `SELECT id, project, area, handle FROM runners_job_secrets WHERE holder = ? AND state = ?`, job, secretsIssued)
	if err != nil {
		s.logf("runners: job %d: reading the boxes it held: %v; their ends revoke them", job, err)
		return recErr
	}
	type held struct {
		id                    int64
		project, area, handle string
	}
	var hs []held
	for rows.Next() {
		var h held
		if err := rows.Scan(&h.id, &h.project, &h.area, &h.handle); err != nil {
			s.logf("runners: job %d: reading a box it held: %v; its end revokes it", job, err)
			continue
		}
		hs = append(hs, h)
	}
	if err := rows.Err(); err != nil {
		s.logf("runners: job %d: reading the boxes it held: %v; their ends revoke them", job, err)
	}
	rows.Close()
	for _, h := range hs {
		// Recorded ended first: a renewal or re-issue asking jobHolds from
		// now on is refused, and one already past it is undone by the End.
		if _, err := s.db().ExecContext(ctx, `UPDATE runners_job_secrets SET state = ?, updated = ? WHERE id = ?`, secretsEnded, s.now().Unix(), h.id); err != nil {
			s.logf("runners: job %d: recording that its job completed: %v", job, err)
		}
		if iss := issuers.From(s.d); iss != nil {
			if err := iss.End(context.WithoutCancel(ctx), issuers.Box{Project: h.project, Area: h.area, ID: h.handle}, issuers.EndJobCompleted); err != nil {
				s.logf("runners: job %d: revoking its box's credentials: %v", job, err)
			}
		}
	}
	return recErr
}

// orphanedIssuing is the boxes of the rows job holds issuing that no
// issueSecrets of this process holds. A row's box is recorded before
// anything is minted into it, so one with none got nothing.
func (s *Service) orphanedIssuing(ctx context.Context, job int64) []issuers.Box {
	rows, err := s.db().QueryContext(ctx, `SELECT id, project, area, handle FROM runners_job_secrets WHERE holder = ? AND state = ? AND handle != ''`, job, secretsIssuing)
	if err != nil {
		s.logf("runners: job %d: reading the boxes being issued for it: %v; their ends revoke them", job, err)
		return nil
	}
	defer rows.Close()
	var out []issuers.Box
	for rows.Next() {
		var id int64
		var b issuers.Box
		if err := rows.Scan(&id, &b.Project, &b.Area, &b.ID); err != nil {
			s.logf("runners: job %d: reading a box being issued for it: %v; its end revokes it", job, err)
			continue
		}
		s.issuingMu.Lock()
		mine := s.issuing[id]
		s.issuingMu.Unlock()
		if !mine {
			out = append(out, b)
		}
	}
	if err := rows.Err(); err != nil {
		s.logf("runners: job %d: reading the boxes being issued for it: %v; their ends revoke them", job, err)
	}
	return out
}

// forgetJobSecrets deletes the rows last changed before cutoff (Sweep, with
// runners_jobs' retention), whatever their state: no box lives that long,
// and a row left issued (its completion never seen) holds nothing by then.
// A renewal asking about a forgotten row is refused (jobHolds).
func (s *Service) forgetJobSecrets(ctx context.Context, cutoff time.Time) {
	if _, err := s.db().ExecContext(ctx, `DELETE FROM runners_job_secrets WHERE updated < ?`, cutoff.Unix()); err != nil {
		s.logf("runners: forgetting old job grant rows: %v", err)
	}
	if _, err := s.db().ExecContext(ctx, `DELETE FROM runners_job_secrets_completed WHERE updated < ?`, cutoff.Unix()); err != nil {
		s.logf("runners: forgetting old job completions: %v", err)
	}
}

// jobHolds is issuers.JobHolds: whether the job req names still holds its
// box, asked before its credential is renewed or issued again after a
// restart. It holds it only while the box's row is issued (a completion
// recorded it ended, durably, so a restart never undoes it) to that run,
// and GitHub says the job holding it is in progress on the box's runner.
// GitHub saying it finished records the row ended, as its completion would.
func (s *Service) jobHolds(ctx context.Context, req issuers.Request) (bool, error) {
	if req.Subject.Job == nil || req.Box.Project == "" || req.Box.ID == "" {
		return false, nil
	}
	var id, owner, holder, run int64
	err := s.db().QueryRowContext(ctx, `SELECT id, job, holder, run FROM runners_job_secrets WHERE project = ? AND handle = ? AND state = ? ORDER BY id DESC LIMIT 1`,
		req.Box.Project, req.Box.ID, secretsIssued).Scan(&id, &owner, &holder, &run)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if run != req.Subject.Job.RunID || holder == 0 {
		return false, nil
	}
	j, err := s.job(ctx, owner)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if j.handle != req.Box.ID || j.runner == "" {
		return false, nil
	}
	cl, err := s.client(ctx)
	if err != nil {
		return false, err
	}
	gj, err := cl.Job(ctx, j.installation, j.repo, holder)
	if err != nil {
		return false, err
	}
	if runsOn(gj, j.runner, run) {
		return true, nil
	}
	if gj.Status == "completed" {
		if _, err := s.db().ExecContext(ctx, `UPDATE runners_job_secrets SET state = ?, updated = ? WHERE id = ? AND state = ?`,
			secretsEnded, s.now().Unix(), id, secretsIssued); err != nil {
			s.logf("runners: job %d: recording that its job completed: %v", owner, err)
		}
	}
	return false, nil
}

// rowsOf is how many rows an Exec changed (0 on any error).
func rowsOf(res sql.Result, err error) (int64, error) {
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
