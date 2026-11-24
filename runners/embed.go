package runners

// What a program that embeds the broker (its daemon is kit/server with
// wiring.Compute and modules of its own; programlink.go) reaches in-process:
// read a job, hold a job's box through run targets' OnBoxRequest, end a
// running job. Go methods with no route: only code compiled into the daemon
// calls them, so no network caller and no pool area's admin reaches them.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/store"
	"github.com/sengirunners/potluck/runtarget/rt"
)

// embedMigrations: ended_at is GitHub's completed_at for the job (0: not
// ended, or ended before the column); stop_cause gains program_stop, which
// SQLite can add to a CHECK only by building the table again.
var embedMigrations = []store.Migration{
	{Kind: store.KindRegistry, Name: "runners/0016_program_jobs", Script: `CREATE TABLE runners_jobs_new (
	job          INTEGER PRIMARY KEY,
	installation INTEGER NOT NULL,
	repo         TEXT NOT NULL,
	run          INTEGER NOT NULL,
	labels       TEXT NOT NULL,
	state        TEXT NOT NULL,
	why          TEXT NOT NULL DEFAULT '',
	target       TEXT NOT NULL DEFAULT '',
	handle       TEXT NOT NULL DEFAULT '',
	runner       TEXT NOT NULL DEFAULT '',
	created      INTEGER NOT NULL,
	updated      INTEGER NOT NULL,
	mints        INTEGER NOT NULL DEFAULT 0,
	reserves     INTEGER NOT NULL DEFAULT 0,
	started      INTEGER NOT NULL DEFAULT 0,
	trusted      INTEGER NOT NULL DEFAULT 0,
	checked      INTEGER NOT NULL DEFAULT 0,
	runs         INTEGER NOT NULL DEFAULT 0,
	ttl          INTEGER NOT NULL DEFAULT 0,
	leased       INTEGER NOT NULL DEFAULT 0,
	limit_s      INTEGER NOT NULL DEFAULT 0,
	limit_source TEXT NOT NULL DEFAULT '',
	started_at   INTEGER NOT NULL DEFAULT 0,
	deadline     INTEGER NOT NULL DEFAULT 0,
	stop_cause   TEXT NOT NULL DEFAULT '' CHECK (stop_cause IN ('','broker_timeout','github_timeout','program_stop')),
	open_guard   INTEGER NOT NULL DEFAULT 1,
	ended_at     INTEGER NOT NULL DEFAULT 0
);
INSERT INTO runners_jobs_new (job, installation, repo, run, labels, state, why, target, handle, runner, created, updated, mints, reserves,
	started, trusted, checked, runs, ttl, leased, limit_s, limit_source, started_at, deadline, stop_cause, open_guard)
SELECT job, installation, repo, run, labels, state, why, target, handle, runner, created, updated, mints, reserves,
	started, trusted, checked, runs, ttl, leased, limit_s, limit_source, started_at, deadline, stop_cause, open_guard FROM runners_jobs;
DROP TABLE runners_jobs;
ALTER TABLE runners_jobs_new RENAME TO runners_jobs;
CREATE INDEX runners_jobs_state ON runners_jobs (state);
CREATE INDEX runners_jobs_updated ON runners_jobs (updated);`},
}

// Job is one GitHub job the broker recorded, as an embedding program reads
// it (Job, Jobs).
type Job struct {
	ID           int64
	Run          int64
	Repository   string // owner/name
	Installation int64
	Area         string // the area its installation is linked to ("" once unlinked)
	Labels       []string
	// State is the broker's: waiting, serving, running, done, failed,
	// ignored, taken or lost (runners.go). Why says what it waits for or why
	// it ended.
	State, Why string
	// Target and Handle are the box binding and run-target handle of the box
	// the job runs on once it started (another job's, when it took that
	// job's runner), else of the box made for it while it waits to start;
	// "" when it has none.
	Target, Handle string
	// Started and Ended are GitHub's started_at and completed_at, or when
	// the broker saw the start or end where GitHub gave no time (a
	// completed delivery that never came, found by asking GitHub); zero
	// until then.
	Started, Ended time.Time
}

const jobCols = `j.job, j.run, j.repo, j.installation, coalesce(l.area, ''), j.labels, j.state, j.why, j.target, j.handle, j.runs, j.started_at, j.ended_at,
	j.stop_cause, j.limit_s, j.limit_source FROM runners_jobs j LEFT JOIN runners_links l ON l.installation = j.installation`

// Job reads job id. api.CodeNotFound: the broker has no record of it (not
// the pool's, or not delivered yet).
func (s *Service) Job(ctx context.Context, id int64) (Job, error) {
	rows, err := s.db().QueryContext(ctx, `SELECT `+jobCols+` WHERE j.job = ?`, id)
	if err != nil {
		return Job{}, err
	}
	out, err := s.scanJobs(ctx, rows)
	if err != nil {
		return Job{}, err
	}
	if len(out) == 0 {
		return Job{}, api.Errorf(api.CodeNotFound, "the broker has no job %d", id)
	}
	return out[0], nil
}

// Jobs reads installation's jobs, the most recently changed first, at most
// limit (1..500; another value is 50).
func (s *Service) Jobs(ctx context.Context, installation int64, limit int) ([]Job, error) {
	if limit < 1 || limit > 500 {
		limit = 50
	}
	rows, err := s.db().QueryContext(ctx, `SELECT `+jobCols+` WHERE j.installation = ? ORDER BY j.updated DESC, j.job DESC LIMIT ?`, installation, limit)
	if err != nil {
		return nil, err
	}
	return s.scanJobs(ctx, rows)
}

func (s *Service) scanJobs(ctx context.Context, rows *sql.Rows) ([]Job, error) {
	type read struct {
		Job
		runs     int64
		stopWhy  string
		started  int64
		ended    int64
		limitSrc string
		limit    int
	}
	var all []read
	for rows.Next() {
		var r read
		var labels string
		if err := rows.Scan(&r.ID, &r.Run, &r.Repository, &r.Installation, &r.Area, &labels, &r.State, &r.Why, &r.Target, &r.Handle, &r.runs,
			&r.started, &r.ended, &r.stopWhy, &r.limit, &r.limitSrc); err != nil {
			rows.Close()
			return nil, err
		}
		_ = json.Unmarshal([]byte(labels), &r.Labels)
		all = append(all, r)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]Job, 0, len(all))
	for _, r := range all {
		if why := timeoutReason(r.stopWhy, r.limit, r.limitSrc); why != "" {
			r.Why = why
		}
		if r.started > 0 {
			r.Started = time.Unix(r.started, 0).UTC()
		}
		if r.ended > 0 {
			r.Ended = time.Unix(r.ended, 0).UTC()
		}
		// Its own row's box is the job's only while no other job runs on it.
		if r.State != jobServing || r.runs != 0 && r.runs != r.ID {
			r.Target, r.Handle = "", ""
		}
		err := s.db().QueryRowContext(ctx, `SELECT target, handle FROM runners_jobs WHERE state = ? AND runs = ? AND handle != '' ORDER BY job = ? DESC LIMIT 1`,
			jobServing, r.ID, r.ID).Scan(&r.Target, &r.Handle)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		out = append(out, r.Job)
	}
	return out, nil
}

// markEnded records GitHub's completed_at on job d, once; githubEnded
// records when the broker saw the end where this did not.
func (s *Service) markEnded(ctx context.Context, d delivery) error {
	t, err := time.Parse(time.RFC3339, d.Job.CompletedAt)
	if err != nil {
		return nil
	}
	_, err = s.db().ExecContext(ctx, `UPDATE runners_jobs SET ended_at = ? WHERE job = ? AND ended_at = 0`, t.Unix(), d.Job.ID)
	return err
}

// runnerJob is d as run targets' OnBoxRequest subscribers see it.
func runnerJob(d delivery) rt.RunnerJob {
	return rt.RunnerJob{ID: d.Job.ID, Repository: d.Repo.FullName, Installation: d.Installation.ID, Labels: append([]string(nil), d.Job.Labels...)}
}

// held asks run targets' box subscribers whether a box on at may be had for
// job d in l's area, before a runner is minted for it: refusal is a
// subscriber's no (the job waits with it on its row, and is asked again on
// the waiting serve's backoff), never counted against the binding
// (refusedBy), since it is a rule of the area, not of the box.
func (s *Service) held(ctx context.Context, svc rt.Service, l link, d delivery, at placed) (refusal string, err error) {
	j := runnerJob(d)
	err = svc.BoxAllowed(rt.WithRunnerJob(ctx, j), l.project, l.Area, rt.BoxRequest{Target: at.name, Size: at.size, Job: &j})
	if refusedByArea(err) {
		return oneLine(err.Error()), nil
	}
	return "", err
}

// refusedByArea reports whether err is a box subscriber's refusal (rt.BoxRefusal).
func refusedByArea(err error) bool {
	var r *rt.BoxRefusal
	return errors.As(err, &r)
}

// EndJob ends job id, running on a pool box, now: the job's box is released,
// so GitHub ends the job as failed, and the broker does not serve or re-run
// it again (stop_cause program_stop). why says why, in plain words, on the
// job's row. A job whose runner was minted for another job leaves that job
// queued, served again on a new box. A release the host does not answer
// leaves the job running and is the error: the program asks again.
// api.CodeNotFound: no such job;
// api.CodeConflict: it is not running on a box of the pool (not started yet,
// or ended), or is being ended already (another EndJob, or its time limit).
func (s *Service) EndJob(ctx context.Context, id int64, why string) error {
	why = oneLine(why)
	if why == "" {
		return api.Errorf(api.CodeBadRequest, "ending a job says why")
	}
	// One end of a job at a time: a failed end clears the flag another end
	// set for the same job's release still in flight. Other jobs' ends do not
	// wait on it (a host that does not answer holds only its own).
	s.flight.Lock()
	if s.ending[id] {
		s.flight.Unlock()
		return api.Errorf(api.CodeConflict, "job %d is being ended already", id)
	}
	if s.ending == nil {
		s.ending = map[int64]bool{}
	}
	s.ending[id] = true
	s.flight.Unlock()
	defer func() {
		s.flight.Lock()
		delete(s.ending, id)
		s.flight.Unlock()
	}()
	var carrier int64
	err := s.db().QueryRowContext(ctx, `SELECT job FROM runners_jobs WHERE state = ? AND started = 1 AND runs = ? AND handle != '' ORDER BY job = ? DESC LIMIT 1`,
		jobServing, id, id).Scan(&carrier)
	if errors.Is(err, sql.ErrNoRows) {
		j, jerr := s.job(ctx, id)
		if errors.Is(jerr, sql.ErrNoRows) {
			return api.Errorf(api.CodeNotFound, "the broker has no job %d", id)
		}
		if jerr != nil {
			return jerr
		}
		return api.Errorf(api.CodeConflict, "job %d is not running on a pool box (it is %s)", id, j.state)
	}
	if err != nil {
		return err
	}
	c, err := s.job(ctx, carrier)
	if err != nil {
		return err
	}
	carrierWhy := "job " + itoa(id) + ", which ran on its runner, was ended (" + why + "); its box was released"
	return s.stopRunning(ctx, c, id, causeProgram, clip("ended: "+why+"; not re-run"), carrierWhy)
}
