package runners

// A box taken back while its job ran (zm-eakh.12.43). A host may take a box
// back at any time: borrowed compute going home is normal. Before a job
// started on the box the job is still queued at GitHub and is served again
// (boxGone). Once a job runs on it GitHub cannot move it: a job assigned to a
// runner that vanished stays in_progress until GitHub's lost-communication
// timeout (about 10 minutes) fails it, GitHub has no call that cancels one
// job (only a whole run, healthy jobs included), refuses to delete a busy
// runner, and re-runs a job only once its whole run has completed. So:
//
//  1. Watch hears from the host that it took the box back or lost it
//     (reclaimed, expired, lost) or that it has no such handle
//     (rt.IsNoHandle), and asks GitHub about the job that runs on it. Only
//     when GitHub still has that job in_progress on that runner is the loss
//     recorded (runners_lost): a job that reported a conclusion first ended
//     for its own reasons and is never re-run. A box that ended by itself
//     (ended: its own process finished) was not taken and is never a loss.
//     A host that is only unlisted, does not answer, or answers not_found
//     for anything but the handle (it is out of the project) has said
//     nothing: the job keeps its box, renewed, and if GitHub then fails it,
//     it is not re-run.
//  2. When GitHub ends the job (completed, or the sweep when that delivery
//     never comes), a conclusion other than failure (a person cancelled it,
//     say) closes the record: not re-run.
//  3. Else the broker waits for the run to complete and for GitHub to have
//     ended every lost job of it, and asks GitHub to re-run: the one job when
//     it is the run's only lost one, else the run's failed jobs (one call;
//     GitHub takes no second re-run while the first runs). A run that ended
//     any other way than failed (cancelled by a person or by a newer run) is
//     not re-run. The job comes back queued under a new id and is served like
//     any other.
//  4. A run is re-run this way while its attempt is below maxAttempts (3):
//     two re-runs at most, counted by GitHub's own run_attempt, so a host that
//     keeps taking boxes back ends the loop, and a person's re-runs count too.
//  5. Without Actions: write on the App the loss is recorded and logged, the
//     job stays failed as before, and nothing retries.
//
// A person sees the job's row (potluck runners jobs) say so in plain words,
// and the same line in the daemon's log. An early lease expiry is unexpected
// host loss. A persisted job runtime timeout is intentional and never enters
// this retry path.

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/sengirunners/potluck/ghapp"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/runtarget/rt"
)

const (
	maxAttempts = 3 // attempts of one run: a job that lost its box is re-run on attempts 1 and 2
	// maxBounceReruns: re-runs of one run for bounces alone (a job stopped
	// on a box guarded to another run, bounce), not counted in maxAttempts.
	maxBounceReruns = 5
	lostAskMax      = 10        // Watches in a row GitHub may leave boxLost's question unanswered before the job is given up on
	lostRunning     = "running" // runners_lost.state: GitHub still has the job running on the vanished runner
	lostEnded       = "ended"   // GitHub failed it: re-run once its run completed
	lostClosed      = "closed"  // re-run, or not: the job's row says which
)

// forget drops Watch's notes on j and releases its box in the background (a
// vanished host's relay waits).
func (s *Service) forget(ctx context.Context, l link, sub *authz.Subject, svc rt.Service, j job) {
	s.flight.Lock()
	delete(s.unlisted, j.id)
	delete(s.goneAsk, j.id)
	delete(s.lostAsk, j.id)
	s.flight.Unlock()
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		rctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
		defer cancel()
		_, _ = svc.Release(rctx, sub, l.project, j.target, handleID(j.handle))
	}()
}

// boxLost records that j's box ended (why, the host's own word) while a job
// ran on it: j's own, or the job that took j's runner (j.runs). gj is
// GitHub's word on that job when the caller just asked. The box is no one's
// either way, so j's row ends here; the loss is recorded only while GitHub
// still has the job running on j's runner. When another job ran on it, j's
// own job is served again if GitHub still has it queued (reserve asks).
// GitHub not answering is asked again on the next Watch, lostAskMax times;
// then, or when GitHub has no such job, the box is let go without a re-run.
func (s *Service) boxLost(ctx context.Context, l link, sub *authz.Subject, svc rt.Service, j job, why string, gj *ghapp.Job) {
	victim := cmp.Or(j.runs, j.id)
	unanswered := ""
	if gj == nil {
		cl, err := s.client(ctx)
		if err != nil {
			return
		}
		if _, low := cl.RateLow(j.installation); low {
			return // asked on a later Watch
		}
		g, err := cl.Job(ctx, j.installation, j.repo, victim)
		if err != nil {
			s.flight.Lock()
			s.lostAsk[j.id]++
			n := s.lostAsk[j.id]
			s.flight.Unlock()
			if !ghapp.NotFound(err) && n < lostAskMax {
				return
			}
			unanswered = fmt.Sprintf("; GitHub did not say whether job %d still ran (%d tries, last: %s): not re-run", victim, n, oneLine(err.Error()))
		}
		gj = &g
	}
	note := "the lending machine took its box back while the job ran (" + why + ")"
	if strings.HasSuffix(why, "(expired)") {
		note = "the box lease expired before the job's runtime deadline (" + why + "): unexpected host loss"
	}

	lost := gj.Status == "in_progress" && gj.RunnerName == j.runner
	state, rowWhy := jobDone, note
	switch {
	case lost && victim == j.id:
		state, rowWhy = jobLost, note+": re-run once GitHub ends it"
	case lost:
		rowWhy = note + ": job " + itoa(victim) + ", which ran on it, is re-run once GitHub ends it"
	case unanswered != "":
		rowWhy = note + unanswered
	case gj.Status == "completed":
		rowWhy = note + "; the job had ended by then"
	}
	if victim != j.id {
		state = jobTaken // its own job may still wait for a runner
	}
	now := s.now().Unix()
	tx, err := s.db().BeginTx(ctx, nil)
	if err != nil {
		return
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx, `UPDATE runners_jobs SET state = ?, why = ?, handle = '', updated = ? WHERE job = ? AND state = ? AND handle = ? AND NOT EXISTS (SELECT 1 FROM runners_jobs actual WHERE actual.job = ? AND actual.stop_cause != '')`,
		state, clip(rowWhy), now, j.id, jobServing, j.handle, victim)
	if err != nil {
		return
	}
	if rows, _ := res.RowsAffected(); rows == 0 {
		return // it ended meanwhile (completed, in the other lane)
	}
	if lost {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO runners_lost (job, installation, repo, run, attempt, state, why, updated)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, victim, j.installation, j.repo, gj.RunID, gj.RunAttempt, lostRunning, clip(note), now); err != nil {
			return
		}
	}
	if state == jobTaken {
		if err := s.queueAgain(ctx, tx, j.id, j.runner); err != nil {
			return
		}
	}
	if tx.Commit() != nil {
		return
	}
	s.kick(laneServe)
	s.logf("runners: job %d: %s", j.id, rowWhy)
	s.forget(ctx, l, sub, svc, j)
}

// lostJob is one runners_lost row.
type lostJob struct {
	job, installation, run int64
	attempt                int
	repo, why              string
	bounced                bool // stopped on a box guarded to another run (bounce)
}

// lostEnded is GitHub ending job: if the broker recorded it as having lost
// its box, it is re-run once its run completed (eventRerun, one per run and
// attempt), unless it ended any other way than failed. run, attempt and
// conclusion are GitHub's (a zero run or attempt: as recorded at the loss).
func (s *Service) lostEnded(ctx context.Context, job, run int64, attempt int, conclusion string) error {
	lo := lostJob{job: job}
	err := s.db().QueryRowContext(ctx, `SELECT installation, repo, run, attempt, why FROM runners_lost WHERE job = ? AND state = ?`, job, lostRunning).
		Scan(&lo.installation, &lo.repo, &lo.run, &lo.attempt, &lo.why)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	lo.run, lo.attempt = cmp.Or(run, lo.run), cmp.Or(attempt, lo.attempt, 1)
	if conclusion != "failure" {
		return s.closeLost(ctx, []lostJob{lo}, jobDone, "; it ended "+cmp.Or(conclusion, "without a conclusion")+" on GitHub: not re-run")
	}
	if _, err := s.db().ExecContext(ctx, `UPDATE runners_lost SET state = ?, run = ?, attempt = ?, updated = ? WHERE job = ? AND state = ?`,
		lostEnded, lo.run, lo.attempt, s.now().Unix(), job, lostRunning); err != nil {
		return err
	}
	if err := s.noteLost(ctx, job, jobLost, lo.why+": GitHub failed it; re-run once the rest of its run ends"); err != nil {
		return err
	}
	b, _ := json.Marshal(map[string]any{"job": job, "run": lo.run, "attempt": lo.attempt, "installation": lo.installation, "repo": lo.repo})
	// One re-run item per run and attempt. One already settled (it found no
	// lost job yet, or closed others) is due again for this one, else its row
	// would stay ended until the 30-day delete.
	_, err = s.db().ExecContext(ctx, `INSERT INTO runners_inbox (delivery, event, body, next_at, received, lane) VALUES (?, ?, ?, 0, ?, ?)
		ON CONFLICT(delivery) DO UPDATE SET body = excluded.body, next_at = 0, received = excluded.received, attempts = 0, fails = 0, done = 0
		WHERE runners_inbox.done != 0`,
		fmt.Sprintf("rerun-%d-%d", lo.run, lo.attempt), eventRerun, b, s.now().UnixNano(), laneServe)
	s.kick(laneServe)
	return err
}

// noteLost writes what became of a lost job on its row, if it has one that
// holds no box (a job that ran on another's runner has its own box until it
// ends).
func (s *Service) noteLost(ctx context.Context, job int64, state, why string) error {
	_, err := s.db().ExecContext(ctx, `UPDATE runners_jobs SET state = ?, why = ?, updated = ? WHERE job = ? AND state IN (?, ?)`,
		state, clip(why), s.now().Unix(), job, jobLost, jobDone)
	return err
}

// closeLost closes lost jobs: each row ends in state, its why followed by what.
func (s *Service) closeLost(ctx context.Context, jobs []lostJob, state, what string) error {
	for _, lo := range jobs {
		if err := s.noteLost(ctx, lo.job, state, lo.why+what); err != nil {
			return err
		}
		if _, err := s.db().ExecContext(ctx, `UPDATE runners_lost SET state = ?, updated = ? WHERE job = ?`, lostClosed, s.now().Unix(), lo.job); err != nil {
			return err
		}
		s.logf("runners: job %d of %s: %s%s", lo.job, lo.repo, lo.why, what)
	}
	return nil
}

// lostOf lists the lost jobs GitHub has failed of one attempt of a run (a
// row left from an earlier attempt is not this re-run's).
func (s *Service) lostOf(ctx context.Context, repo string, run int64, attempt int) ([]lostJob, error) {
	rows, err := s.db().QueryContext(ctx, `SELECT job, installation, attempt, why, bounced FROM runners_lost WHERE run = ? AND repo = ? COLLATE NOCASE AND state = ?
		AND attempt = ? ORDER BY job`, run, repo, lostEnded, attempt)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []lostJob
	for rows.Next() {
		lo := lostJob{run: run, repo: repo}
		if err := rows.Scan(&lo.job, &lo.installation, &lo.attempt, &lo.why, &lo.bounced); err != nil {
			return nil, err
		}
		out = append(out, lo)
	}
	return out, rows.Err()
}

// rerun asks GitHub to run a run's lost jobs again (eventRerun), once the run
// completed: GitHub refuses a re-run before. It waits like a serve does (at
// most WaitMax apart, sooner when a job ends), for a day at most.
func (s *Service) rerun(ctx context.Context, body []byte) error {
	var q struct {
		Run, Installation int64
		Attempt           int
		Repo              string
	}
	if json.Unmarshal(body, &q) != nil {
		return nil
	}
	// A bounce re-run an earlier try left asking (a process gone between its
	// record and GitHub's answer) is dropped first, whatever this try does:
	// its answer is not known, so the next attempt is not the broker's.
	if err := s.bounceRerunUnasked(ctx, q.Repo, q.Run, q.Attempt); err != nil {
		return err
	}
	jobs, err := s.lostOf(ctx, q.Repo, q.Run, q.Attempt)
	if err != nil || len(jobs) == 0 {
		return err
	}
	cl, err := s.client(ctx)
	if err != nil {
		return err
	}
	if until, low := cl.RateLow(q.Installation); low {
		return &waitErr{why: "GitHub's API budget for this installation is nearly spent", until: until}
	}
	run, err := cl.Run(ctx, q.Installation, q.Repo, q.Run)
	if err != nil {
		return err
	}
	var running, bounceReruns int
	if err := s.db().QueryRowContext(ctx, `SELECT count(*) FROM runners_lost WHERE run = ? AND repo = ? COLLATE NOCASE AND state = ?`,
		q.Run, q.Repo, lostRunning).Scan(&running); err != nil {
		return err
	}
	if err := s.db().QueryRowContext(ctx, `SELECT count(*) FROM runners_bounce_reruns WHERE run = ? AND repo = ? AND attempt < ? AND asked = 1`,
		q.Run, q.Repo, q.Attempt).Scan(&bounceReruns); err != nil {
		return err
	}
	// A batch of bounces alone counts against maxBounceReruns; any other
	// against maxAttempts, less the attempts the broker re-ran for bounces
	// alone (GitHub's run_attempt counts both, and a person's re-runs).
	bounces := !slices.ContainsFunc(jobs, func(lo lostJob) bool { return !lo.bounced })
	switch {
	case run.Attempt > q.Attempt:
		// A person re-ran it, or the broker did and stopped before it wrote
		// so (its record, left asking, was dropped above: a person's, as far
		// as grants go).
		return s.closeLost(ctx, jobs, jobDone, fmt.Sprintf("; its run is on attempt %d already: not asked again", run.Attempt))
	case bounces && bounceReruns >= maxBounceReruns:
		s.logf("runners: ALERT: run %d of %s: its jobs were stopped on boxes guarded to other runs %d times; not re-run again (same-label credentialed workflows keep taking each other's runners)",
			q.Run, q.Repo, bounceReruns+1)
		return s.closeLost(ctx, jobs, jobLost, fmt.Sprintf("; its run was re-run %d times for bounces already: not re-run again", bounceReruns))
	case !bounces && q.Attempt-bounceReruns >= maxAttempts:
		return s.closeLost(ctx, jobs, jobLost, fmt.Sprintf("; that was attempt %d of %d: not re-run again", q.Attempt-bounceReruns, maxAttempts))
	case run.Status != "completed":
		return &waitErr{why: "run " + itoa(q.Run) + " still has jobs running: its lost job is re-run once they end"}
	case running > 0:
		// GitHub's completed for another lost job of the run is still on its
		// way (or the sweep's to find): one call re-runs them together.
		return &waitErr{why: "run " + itoa(q.Run) + " has another lost job GitHub has not ended yet"}
	case run.Conclusion != "failure":
		return s.closeLost(ctx, jobs, jobDone, "; its run ended "+cmp.Or(run.Conclusion, "without a conclusion")+
			" (cancelled by a person or by a newer run, say): not re-run")
	}
	// A re-run is a serve the broker asks for: never of a repository it would
	// not serve now (disconnected, renamed, or made public without the yes).
	gr, err := s.repoNow(ctx, cl, q.Installation, q.Repo)
	if err != nil {
		return err
	}
	d := delivery{}
	d.Installation.ID, d.Repo.FullName = q.Installation, q.Repo
	if why, err := s.connected(ctx, d, &gr); err != nil {
		return err
	} else if why != "" {
		return s.closeLost(ctx, jobs, jobLost, "; not re-run: "+why)
	}
	// A bounce re-run is recorded asking before GitHub is asked, so the next
	// attempt's serve, which may come before this returns, waits for the
	// answer (bounceRerunsOnly) instead of being served without its grants.
	if bounces {
		if _, err := s.db().ExecContext(ctx, `INSERT INTO runners_bounce_reruns (repo, run, attempt, updated, asked) VALUES (?, ?, ?, ?, 0)
			ON CONFLICT(repo, run, attempt) DO UPDATE SET updated = excluded.updated WHERE asked = 0`, q.Repo, q.Run, q.Attempt, s.now().Unix()); err != nil {
			return fmt.Errorf("recording the bounce re-run of run %d: %w", q.Run, err)
		}
	}
	if len(jobs) == 1 {
		err = cl.RerunJob(ctx, q.Installation, q.Repo, jobs[0].job)
	} else {
		err = cl.RerunFailedJobs(ctx, q.Installation, q.Repo, q.Run)
	}
	if err != nil && bounces {
		// GitHub took no re-run, or its answer was lost: either way the next
		// attempt is not recorded as the broker's.
		if uerr := s.bounceRerunUnasked(ctx, q.Repo, q.Run, q.Attempt); uerr != nil {
			s.logf("runners: run %d of %s: %v", q.Run, q.Repo, uerr)
		}
	}
	switch {
	case errors.Is(err, ghapp.ErrNoActionsWrite):
		return s.closeLost(ctx, jobs, jobLost, "; not re-run: the GitHub App lacks Actions: write (add Repository permissions > Actions: "+
			"Read and write in the App's settings, then accept the request on each installation)")
	case err != nil:
		return err
	}
	if bounces {
		// Only a run now on the very next attempt is the broker's: another
		// attempt in between (a person's, racing the call) means the record
		// would vouch for an attempt the broker did not start.
		after, err := s.attemptAfterRerun(ctx, cl, q.Installation, q.Repo, q.Run, q.Attempt)
		if err != nil {
			return err
		}
		if after != q.Attempt+1 {
			if uerr := s.bounceRerunUnasked(ctx, q.Repo, q.Run, q.Attempt); uerr != nil {
				return uerr
			}
			return s.closeLost(ctx, jobs, jobDone, fmt.Sprintf("; re-queued, but its run is on attempt %d, not %d: not recorded as a bounce re-run", after, q.Attempt+1))
		}
		// GitHub took a re-run the broker asked for: one more attempt of the
		// run is the broker's. Which one does not matter here: an attempt
		// counts as the broker's only when every attempt before it has such
		// a record (bounceRerunsOnly), and each record is one attempt GitHub
		// started for the broker, so a person's attempt in between always
		// leaves one short. Not recorded asked (an error): the delivery is
		// applied again and drops the record first.
		if _, err := s.db().ExecContext(ctx, `UPDATE runners_bounce_reruns SET asked = 1, updated = ? WHERE repo = ? AND run = ? AND attempt = ?`,
			s.now().Unix(), q.Repo, q.Run, q.Attempt); err != nil {
			return fmt.Errorf("recording the bounce re-run of run %d asked: %w", q.Run, err)
		}
		return s.closeLost(ctx, jobs, jobDone, fmt.Sprintf("; re-queued: bounce re-run %d of %d", bounceReruns+1, maxBounceReruns))
	}
	return s.closeLost(ctx, jobs, jobDone, fmt.Sprintf("; re-queued: attempt %d of %d", q.Attempt-bounceReruns+1, maxAttempts))
}

// rerunReadTries and rerunReadWait bound how long attemptAfterRerun waits for
// GitHub's run to show the attempt its re-run made.
const (
	rerunReadTries = 3
	rerunReadWait  = 2 * time.Second
)

// attemptAfterRerun is run's attempt after a re-run of attempt was accepted.
// A read still showing attempt itself is GitHub lagging behind its own
// answer, so it is read again a few times (a number below it too); a number
// above it is returned at once, and after the last try whatever was read is.
func (s *Service) attemptAfterRerun(ctx context.Context, cl *ghapp.Client, installation int64, repo string, run int64, attempt int) (int, error) {
	for try := 1; ; try++ {
		r, err := cl.Run(ctx, installation, repo, run)
		if err != nil {
			return 0, err
		}
		if r.Attempt > attempt || try == rerunReadTries {
			return r.Attempt, nil
		}
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(rerunReadWait):
		}
	}
}

// rerunDropped closes a run's lost jobs when their re-run is given up on.
func (s *Service) rerunDropped(ctx context.Context, it item, what string) {
	var q struct {
		Run     int64
		Attempt int
		Repo    string
	}
	if it.event != eventRerun || json.Unmarshal(it.body, &q) != nil {
		return
	}
	if err := s.bounceRerunUnasked(ctx, q.Repo, q.Run, q.Attempt); err != nil {
		s.logf("runners: run %d of %s: %v", q.Run, q.Repo, err)
	}
	if jobs, err := s.lostOf(ctx, q.Repo, q.Run, q.Attempt); err == nil {
		_ = s.closeLost(ctx, jobs, jobLost, "; not re-run: "+strings.TrimSpace(what))
	}
}

// bounceRerunUnasked drops the bounce re-run of attempt of run left asking
// (rerun): GitHub did not take it, or its answer is not known, so the next
// attempt is not the broker's as far as grants go.
func (s *Service) bounceRerunUnasked(ctx context.Context, repo string, run int64, attempt int) error {
	if _, err := s.db().ExecContext(context.WithoutCancel(ctx), `DELETE FROM runners_bounce_reruns WHERE repo = ? AND run = ? AND attempt = ? AND asked = 0`,
		repo, run, attempt); err != nil {
		return fmt.Errorf("dropping the unanswered bounce re-run of attempt %d: %w", attempt, err)
	}
	return nil
}

// bounceRerunsOnly is whether attempt of run (of repo) exists only through
// the broker's own bounce re-runs: GitHub took a re-run of each attempt
// before it, 1 to attempt-1, that the broker asked for because its jobs were
// bounced (runners_bounce_reruns, asked). Such an attempt is the broker's
// recovery of attempt 1, not a person's re-run. Any attempt in between that
// a person (or the API, or a host-loss re-run) started breaks the chain. A
// re-run of one of them still being asked of GitHub makes the caller wait
// for its answer (a waitErr), for askingLimit at most.
func (s *Service) bounceRerunsOnly(ctx context.Context, repo string, run int64, attempt int) (bool, error) {
	if attempt < 2 {
		return false, nil
	}
	var n, asked int
	var asking int64 // when the newest record still asking was written
	if err := s.db().QueryRowContext(ctx, `SELECT count(*), coalesce(sum(asked), 0), coalesce(max(CASE WHEN asked = 0 THEN updated END), 0)
		FROM runners_bounce_reruns WHERE repo = ? AND run = ? AND attempt BETWEEN 1 AND ?`,
		repo, run, attempt-1).Scan(&n, &asked, &asking); err != nil {
		return false, err
	}
	switch {
	case n != attempt-1:
		return false, nil
	case asked == n:
		return true, nil
	case s.now().Sub(time.Unix(asking, 0)) > askingLimit:
		return false, nil // left asking by a process gone: not known to be the broker's
	}
	return false, &waitErr{why: "the broker's bounce re-run of run " + itoa(run) + " waits for GitHub's answer"}
}

// askingLimit is how long a bounce re-run may stay asking (recorded, GitHub's
// answer not yet) before it counts as unanswered: a call to GitHub takes
// 15 s at most (ghapp), and the re-run and its reads (rerunReadTries, with
// rerunReadWait between) take about 64 s in the worst case, so a record older
// than this was left by a process that stopped before it heard.
const askingLimit = 2 * time.Minute

// sweepLost ends lost jobs whose completed delivery never came.
func (s *Service) sweepLost(ctx context.Context, cl *ghapp.Client) {
	rows, err := s.db().QueryContext(ctx, `SELECT job, installation, repo FROM runners_lost WHERE state = ? ORDER BY job`, lostRunning)
	if err != nil {
		return
	}
	var jobs []lostJob
	for rows.Next() {
		var lo lostJob
		if rows.Scan(&lo.job, &lo.installation, &lo.repo) == nil {
			jobs = append(jobs, lo)
		}
	}
	rows.Close()
	for _, lo := range jobs {
		if _, low := cl.RateLow(lo.installation); low {
			continue // the next sweep
		}
		gj, err := cl.Job(ctx, lo.installation, lo.repo, lo.job)
		switch {
		case ghapp.NotFound(err):
			_ = s.lostEnded(ctx, lo.job, 0, 0, "deleted")
		case err == nil && gj.Status == "completed":
			_ = s.lostEnded(ctx, lo.job, gj.RunID, gj.RunAttempt, gj.Conclusion)
		}
	}
}

// leaseTTL is the lease j's box is acquired and renewed with.
func (s *Service) leaseTTL(j job) int { return cmp.Or(j.ttl, s.jobTTL()+startupAllowance) }

// renewing notes that j's box lease is about to be renewed. Whether the call
// then succeeds is not known for sure (a host may apply a renewal whose
// answer never arrives), so the ask counts.
func (s *Service) renewing(id int64) {
	s.flight.Lock()
	s.renewed[id] = s.now()
	s.flight.Unlock()
}

// leaseLapsed reports whether j's box has gone a whole lease without a
// renewal asked: the host ends a box whose lease ran out, so by then the box
// is gone whether or not the host ever answers a release, and a row whose
// job GitHub ended can end too, no longer counting against its binding in
// placement. The lease counts from the latest of: the acquire, this
// process's last renewal ask, and this process's start (an earlier process
// may have renewed it just before it stopped); the startup allowance is
// added as slack (a box that boots before its lease starts, a renewal the
// host applied late, the host's clock).
func (s *Service) leaseLapsed(j job) bool {
	s.flight.Lock()
	from := time.Unix(j.leased, 0)
	for _, t := range []time.Time{s.renewed[j.id], s.begun} {
		if t.After(from) {
			from = t
		}
	}
	s.flight.Unlock()
	return s.now().After(from.Add(time.Duration(s.leaseTTL(j)+startupAllowance) * time.Second))
}
