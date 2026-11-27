package runners

// A job's runtime limit and its box's renewable lease are different clocks.
// The host must fit the runtime plus five minutes to boot and register. Once
// GitHub starts the job, its reported start fixes the deadline; duplicate
// deliveries never extend it. Watch enforces deadlines at its normal cadence.

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/sengirunners/potluck/ghapp"
	"github.com/sengirunners/potluck/kit/api"
	"gopkg.in/yaml.v3"
)

const startupAllowance = 300

type runtimeLimit struct {
	seconds int
	source  string
	// fallback is why job_ttl_s applies instead of the workflow's
	// timeout-minutes; empty when the workflow's limit applies.
	fallback string
}

// workflowSource is where workflowLimit reads a run, its workflow file and a
// job: GitHub through the App for the broker (*ghapp.Client), a workflow
// file read with gh for the doctor's check 11.
type workflowSource interface {
	Run(ctx context.Context, installation int64, repo string, run int64) (ghapp.Run, error)
	WorkflowFile(ctx context.Context, installation int64, repo, path, sha string) ([]byte, error)
	Job(ctx context.Context, installation int64, repo string, id int64) (ghapp.Job, error)
}

func (s *Service) workflowLimit(ctx context.Context, cl workflowSource, installation int64, repo string, run, id int64, knownRun *ghapp.Run, knownJob *ghapp.Job) runtimeLimit {
	fallback := func(why string) runtimeLimit {
		return runtimeLimit{seconds: s.jobTTL(), source: "configured fallback: " + why, fallback: why}
	}
	var source ghapp.Run
	var err error
	if knownRun != nil {
		source = *knownRun
	} else {
		source, err = cl.Run(ctx, installation, repo, run)
	}
	if err != nil {
		return fallback("workflow source unavailable")
	}
	// For these events the workflow is at GITHUB_SHA. PRs have distinct code
	// and workflow revisions; REST does not expose the caller workflow SHA.
	switch source.Event {
	case "push", "workflow_dispatch":
	default:
		return fallback("workflow revision is unresolved for " + cmp.Or(source.Event, "unknown event"))
	}
	if source.HasReferencedWorkflows {
		return fallback("reusable workflow identity is unresolved")
	}
	body, err := cl.WorkflowFile(ctx, installation, repo, source.Path, source.HeadSHA)
	if err != nil {
		if errors.Is(err, ghapp.ErrWorkflowContentsRead) {
			return fallback(ghapp.ErrWorkflowContentsRead.Error())
		}
		return fallback("immutable workflow file unavailable")
	}
	var jc ghapp.Job
	if knownJob != nil {
		jc = *knownJob
	} else {
		jc, err = cl.Job(ctx, installation, repo, id)
	}
	if err != nil || jc.Name == "" {
		return fallback("GitHub job display name unavailable")
	}
	var doc struct {
		Jobs map[string]struct {
			Name     yaml.Node `yaml:"name"`
			Timeout  yaml.Node `yaml:"timeout-minutes"`
			Uses     yaml.Node `yaml:"uses"`
			Strategy yaml.Node `yaml:"strategy"`
		} `yaml:"jobs"`
	}
	if yaml.Unmarshal(body, &doc) != nil || len(doc.Jobs) == 0 {
		return fallback("workflow jobs cannot be read")
	}
	selected := ""
	for key, j := range doc.Jobs {
		if j.Uses.Kind != 0 {
			return fallback("reusable workflow identity is unresolved")
		}
		if j.Strategy.Kind != 0 {
			return fallback("matrix job identity is unresolved")
		}
		name := key
		if j.Name.Kind != 0 {
			if j.Name.Kind != yaml.ScalarNode || j.Name.Tag != "!!str" || strings.Contains(j.Name.Value, "${{") {
				return fallback("dynamic job name is unresolved")
			}
			name = j.Name.Value
		}
		if name == jc.Name {
			if selected != "" {
				return fallback("job display name is ambiguous")
			}
			selected = key
		}
	}
	if selected == "" {
		return fallback("job display name does not identify a workflow job")
	}
	timeout := doc.Jobs[selected].Timeout
	if timeout.Kind == 0 {
		return fallback("workflow omits timeout-minutes")
	}
	if strings.Contains(timeout.Value, "${{") {
		return fallback("timeout-minutes expression is unresolved")
	}
	n, err := strconv.Atoi(timeout.Value)
	if timeout.Kind != yaml.ScalarNode || timeout.Tag != "!!int" || err != nil || n < 1 || n > jobTTLMax/60 {
		return fallback("timeout-minutes is not a literal integer within the broker five-day ceiling")
	}
	return runtimeLimit{seconds: n * 60, source: fmt.Sprintf("workflow %s@%s job %s", source.Path, source.HeadSHA, selected)}
}

func (s *Service) ensureLimit(ctx context.Context, cl *ghapp.Client, d delivery, knownRun *ghapp.Run, knownJob *ghapp.Job) error {
	j, err := s.job(ctx, d.Job.ID)
	if err != nil || j.limit > 0 {
		return err
	}
	v := s.workflowLimit(ctx, cl, d.Installation.ID, d.Repo.FullName, d.Job.RunID, d.Job.ID, knownRun, knownJob)
	_, err = s.db().ExecContext(ctx, `UPDATE runners_jobs SET limit_s = ?, limit_source = ? WHERE job = ? AND limit_s = 0`, v.seconds, v.source, d.Job.ID)
	return err
}

// startRuntime follows the job that actually takes the runner, including a
// trusted job that takes another job's minted runner. Missing started_at is
// observed now, once, rather than guessed from the box acquisition time.
func (s *Service) startRuntime(ctx context.Context, id, by int64, known *ghapp.Job) error {
	holder, err := s.job(ctx, id)
	if err != nil {
		return err
	}
	if holder.started && holder.runs == by {
		if by != id {
			return s.followTaker(ctx, id, by) // again, where an earlier try failed after the start was recorded
		}
		return nil
	}
	cl, err := s.client(ctx)
	if err != nil {
		return err
	}
	var jc ghapp.Job
	if known != nil {
		jc = *known
	} else {
		jc, err = cl.Job(ctx, holder.installation, holder.repo, by)
	}
	if err != nil {
		return err
	}
	actual, err := s.job(ctx, by)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	v := runtimeLimit{seconds: actual.limit, source: actual.limitSource}
	if v.seconds == 0 {
		v = s.workflowLimit(ctx, cl, holder.installation, holder.repo, cmp.Or(jc.RunID, holder.run), by, nil, &jc)
	}
	start := s.now().Unix()
	if t, ok := jc.Started(); ok && t.Unix() <= start {
		start = t.Unix()
	} else {
		v.source += "; start observed by broker (GitHub started_at unavailable)"
	}
	// The actual job owns its budget and clock, even when its own box is
	// currently running another job. Assignment facts on that box stay intact.
	_, err = s.db().ExecContext(ctx, `INSERT INTO runners_jobs (job,installation,repo,run,labels,state,why,created,updated,limit_s,limit_source,started_at,deadline)
 VALUES (?,?,?,?, '[]', ?, ?,?,?,?,?,?,?) ON CONFLICT(job) DO UPDATE SET
 state = CASE WHEN runners_jobs.state IN (?,?,?) THEN excluded.state ELSE runners_jobs.state END,
 limit_s = CASE WHEN runners_jobs.limit_s = 0 THEN excluded.limit_s ELSE runners_jobs.limit_s END,
 limit_source = CASE WHEN runners_jobs.started_at = 0 THEN excluded.limit_source ELSE runners_jobs.limit_source END,
 started_at = CASE WHEN runners_jobs.started_at = 0 THEN excluded.started_at ELSE runners_jobs.started_at END,
 deadline = CASE WHEN runners_jobs.started_at = 0 THEN excluded.deadline ELSE runners_jobs.deadline END`, by, holder.installation, holder.repo, cmp.Or(jc.RunID, holder.run), jobRunning, "running on runner "+holder.runner+" minted for job "+itoa(id), s.now().Unix(), s.now().Unix(), v.seconds, v.source, start, start+int64(v.seconds), jobWaiting, jobFailed, jobTaken)
	if err != nil {
		return err
	}
	_, err = s.db().ExecContext(ctx, `UPDATE runners_jobs SET started = 1, runs = ? WHERE job = ? AND state = ? AND started = 0`, by, id, jobServing)
	if err != nil || by == id {
		return err
	}
	return s.followTaker(ctx, id, by)
}

// followTaker gives the box of job id, whose runner job by took (GitHub
// gives a runner any queued job of its repository with its labels), to by:
// the box's lease budget becomes by's limit plus the startup allowance, and
// the box is renewed to it now, longer or shorter than id's. Where by has no
// box of its own, by's row carries the box from here on and id, still queued
// on GitHub, is served again with a new box (requeue; reserve asks GitHub
// first). Where by has its own box, id keeps carrying this one: by's idle
// runner is there for id's job. So too where id's box holds job grants of
// id's run (it is guarded to that run and its secrets are issued through
// id's row, issueSecrets; a row ended before any job claimed it still
// guards the box: claimUnclaimed) or has an open guard (fork serving: the retire of
// open runners, retireOpen, and the rules for who may take one go by id's
// row). A box re-guarded to by's run (takeOver) is by's like any other; by's
// rows of job grants for no box it holds go then, so nothing is ever issued
// through them into the box by now carries.
func (s *Service) followTaker(ctx context.Context, id, by int64) error {
	holder, err := s.job(ctx, id)
	if err != nil || holder.state != jobServing || holder.runs != by || holder.handle == "" {
		return err
	}
	actual, err := s.job(ctx, by)
	if err != nil || actual.limit == 0 {
		return err
	}
	ttl := actual.limit + startupAllowance
	tx, err := s.db().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	now := s.now().Unix()
	carrier := id
	var moved int64
	if !holder.openGuard {
		res, err := tx.ExecContext(ctx, `UPDATE runners_jobs SET state = ?, why = '', target = ?, handle = ?, runner = ?, ttl = ?, leased = ?, started = 1, runs = ?,
			updated = ? WHERE job = ? AND state = ? AND NOT EXISTS (SELECT 1 FROM runners_job_secrets WHERE job = ? AND run = ? AND (state IN (?, ?, ?) OR `+claimUnclaimed+`))`, jobServing, holder.target,
			holder.handle, holder.runner, ttl, holder.leased, by, now, by, jobRunning, id, holder.run, secretsPlanned, secretsIssuing, secretsIssued)
		if err != nil {
			return err
		}
		moved, _ = res.RowsAffected()
	}
	if moved > 0 {
		carrier = by
		res, err := tx.ExecContext(ctx, `UPDATE runners_jobs SET state = ?, why = ?, handle = '', started = 0, runs = 0, checked = 0, updated = ?
			WHERE job = ? AND state = ? AND handle = ? AND runs = ?`, jobWaiting,
			clip("its runner "+holder.runner+" was taken by job "+itoa(by)+", which keeps its box: served again"), now, id, jobServing, holder.handle, by)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil // the box changed hands meanwhile: nothing moved
		}
		if err := dropServes(ctx, tx, by); err != nil {
			return err
		}
		if err := s.queueAgain(ctx, tx, id, holder.runner); err != nil {
			return err
		}
	} else if _, err := tx.ExecContext(ctx, `UPDATE runners_jobs SET ttl = ? WHERE job = ? AND state = ? AND handle = ? AND runs = ?`,
		ttl, id, jobServing, holder.handle, by); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if carrier == by {
		s.kick(laneServe)
	}
	j, err := s.job(ctx, carrier)
	if err != nil {
		return err
	}
	s.renew(ctx, j, map[string]map[string]int{})
	return nil
}

// activeJob follows the holder's assignment. Runtime facts belong only to
// the actual job's row; the holder's own job may run on a different box.
func (s *Service) activeJob(ctx context.Context, holder job) (job, error) {
	if !holder.started || holder.runs == 0 {
		return job{}, nil
	}
	if holder.runs == holder.id {
		return holder, nil
	}
	actual, err := s.job(ctx, holder.runs)
	if err != nil {
		s.logf("runners: holder job %d: actual job %d is missing: %v", holder.id, holder.runs, err)
	}
	return actual, err
}

func (s *Service) runtimeExpired(ctx context.Context, j job) bool {
	actual, err := s.activeJob(ctx, j)
	if err == nil && actual.stopCause == causeGitHubTimeout {
		_ = s.jobEnded(ctx, actual.id, "")
		return true
	}
	if err != nil || actual.deadline == 0 || s.now().Unix() < actual.deadline {
		return false
	}
	if actual.stopCause == "" {
		if cl, e := s.client(ctx); e == nil {
			if gj, e := cl.Job(ctx, j.installation, j.repo, actual.id); e == nil && gj.Status == "completed" {
				_ = s.githubEnded(ctx, actual.id, gj.Conclusion, gj.RunnerName)
				return true
			}
		}
	}
	victim := actual.id
	carrierWhy := "job " + itoa(victim) + " on its runner reached its time limit; its box was released"
	_ = s.stopRunning(ctx, j, victim, causeBrokerTimeout, timeoutReason(causeBrokerTimeout, actual.limit, actual.limitSource), carrierWhy)
	return true
}

// Stop causes (runners_jobs.stop_cause): why the broker ended a running job
// itself, so a box that goes with it is no loss to re-run (lost.go).
const (
	causeBrokerTimeout = "broker_timeout" // its time limit (runtimeExpired)
	causeGitHubTimeout = "github_timeout" // GitHub's timed_out (githubEnded)
	causeProgram       = "program_stop"   // the embedding program ended it (EndJob)
)

// releaseCarrier releases carrier j's box (one already gone counts as released).
func (s *Service) releaseCarrier(ctx context.Context, j job) error {
	l, err := s.link(ctx, j.installation)
	if err != nil {
		return err
	}
	sub, svc, err := s.poolSubject(ctx, l)
	if err != nil {
		return err
	}
	if _, err = svc.Release(ctx, sub, l.project, j.target, handleID(j.handle)); err != nil && !api.IsCode(err, api.CodeNotFound) {
		return err
	}
	return nil
}

// stopRunning ends job victim, running on carrier j's box, for cause: the
// cause flagged on victim first (so the box's end is not taken for a loss),
// then the box released, then the rows ended. why is victim's; carrierWhy is
// j's when j is another job, whose own job, still queued on GitHub, is
// served again. A victim already flagged for another cause, or no longer on
// j's box, is not stopped (a conflict); a host that does not answer the
// release leaves the rows for the next try (the time limit's) or for the
// program to ask again (its end).
func (s *Service) stopRunning(ctx context.Context, j job, victim int64, cause, why, carrierWhy string) error {
	flag, err := s.db().ExecContext(ctx, `UPDATE runners_jobs SET stop_cause = ? WHERE job = ? AND stop_cause IN ('', ?) AND EXISTS (SELECT 1 FROM runners_jobs carrier WHERE carrier.job = ? AND carrier.state = ? AND carrier.runs = ? AND carrier.handle = ?)`, cause, victim, cause, j.id, jobServing, victim, j.handle)
	if err != nil {
		return err
	}
	if n, _ := flag.RowsAffected(); n == 0 {
		return api.Errorf(api.CodeConflict, "job %d is no longer running on the box of job %d, or is being ended for another reason", victim, j.id)
	}
	if err := s.releaseCarrier(ctx, j); err != nil {
		// The time limit keeps its flag: runtimeExpired tries again on the next
		// watch. Nothing retries a program's end but the program, so its flag
		// goes, and the job is as it was (its box's loss is a loss again).
		// A host that released the box but whose answer was lost makes the
		// next watch take it for a loss, and re-run the job: the program's
		// end was never confirmed, so that is its fallback.
		if cause == causeProgram {
			if _, cerr := s.db().ExecContext(context.WithoutCancel(ctx), `UPDATE runners_jobs SET stop_cause = '' WHERE job = ? AND stop_cause = ?`, victim, cause); cerr != nil {
				return errors.Join(err, cerr)
			}
		}
		return err
	}
	state := jobDone
	if victim != j.id {
		state = jobTaken
	} else {
		carrierWhy = why
	}
	res, err := s.db().ExecContext(ctx, `UPDATE runners_jobs SET state = ?, why = ?, handle = '', updated = ? WHERE job = ? AND state = ? AND runs = ? AND handle = ?`, state, clip(carrierWhy), s.now().Unix(), j.id, jobServing, victim, j.handle)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil // ended meanwhile (completed, in the other lane)
	}
	s.logf("runners: job %d: %s", victim, why)
	if victim != j.id {
		// A trusted job took this runner: the original queued job still needs a
		// runner. Its original budget and actual clock were never overwritten.
		_, _ = s.db().ExecContext(ctx, `UPDATE runners_jobs SET why = ?, state = CASE WHEN state = ? AND runs != 0 AND runs != job THEN state ELSE ? END, updated = ? WHERE job = ?`, clip(why), jobServing, jobDone, s.now().Unix(), victim)
		_ = s.jobEnded(ctx, victim, "")
		_ = s.requeue(ctx, j.id, j.runner)
	}
	return nil
}

// githubEnded preserves GitHub's authoritative timeout conclusion. A generic
// failure or cancellation alone does not establish that a time limit fired.
// ranOn is the runner GitHub says the job ran on, "" when it has not said.
func (s *Service) githubEnded(ctx context.Context, id int64, conclusion, ranOn string) error {
	if conclusion == "timed_out" {
		_, err := s.db().ExecContext(ctx, `UPDATE runners_jobs SET stop_cause = ? WHERE job = ? AND stop_cause = ''`, causeGitHubTimeout, id)
		if err != nil {
			return err
		}
	}
	// GitHub's completed_at, when a delivery brought it (markEnded), else when
	// the broker saw the end.
	if _, err := s.db().ExecContext(ctx, `UPDATE runners_jobs SET ended_at = ? WHERE job = ? AND ended_at = 0`, s.now().Unix(), id); err != nil {
		return err
	}
	_, err := s.db().ExecContext(ctx, `UPDATE runners_jobs SET state = ?, updated = ? WHERE job = ? AND state = ?`, jobDone, s.now().Unix(), id, jobRunning)
	if err != nil {
		return err
	}
	return s.jobEnded(ctx, id, ranOn)
}

// timeoutReason derives the user-visible cause from the actual job's first
// proven timeout decision. A pending release is a stop request, not proof
// that an unreachable host has already stopped the process.
func timeoutReason(cause string, limit int, source string) string {
	switch cause {
	case causeBrokerTimeout:
		return fmt.Sprintf("job time limit reached (%d s; %s); stop requested; not re-run", limit, source)
	case causeGitHubTimeout:
		return "GitHub stopped the job at its workflow time limit (timed_out); not re-run"
	default:
		return ""
	}
}
