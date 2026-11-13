package runners

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/ghapp"
	"github.com/sengirunners/potluck/protocol"
)

// A runner's guard (jit_guard) is decided at its mint, from how GitHub has
// the repository then (ready) and allow_fork_prs then. GitHub gives a runner
// to any queued job of its repository with its labels, so while fork serving
// holds for a repository (private, allow_fork_prs) every runner minted for
// it on a box that can check it gets an open guard: a fork's job that takes
// an own branch's runner runs there, as the broker would serve it anyway,
// but only while that job's own event payload says the repository is
// private, which the box checks before its first step in the run's event
// payload (fixed when the run was created). Once fork serving stops
// holding, an open runner would let a fork's job through where the box
// cannot see the change: allow_fork_prs switched off, or a run created
// while the repository was still private. For those the broker retires every idle open runner as soon as it learns of the change
// (retireOpen), and decides every job that starts on one again from
// GitHub's answer then (decideNow, started), which refuses what the retire
// missed, after GitHub started it.

// guard is jit_guard for a runner of d's repository: the job's repository
// and the trusted events (with a pull_request* run's head the repository
// itself) as the box can check them; open widens it to every run of the
// repository whose own event payload says, at its job's start, that the
// repository is private (open_if_private). The payload is the run's,
// fixed when GitHub created the run, so a fork's run created after the
// repository was made public or internal is stopped before its first step,
// whatever the broker has learned by then; a job of a run created before
// (a later job of it, a re-run) still says private, and only the broker's
// retire and its decision at the job's start (decideNow) refuse it. A
// runner is open only on a box whose driver checks it
// (placed.guardsPrivate).
func guard(d delivery, open bool) *computeapi.JITGuard {
	return &computeapi.JITGuard{Repo: d.Repo.FullName, Events: trustedEvents, OpenIfPrivate: open}
}

// noPrivateGuard is why a box does not take a job whose run passed only
// under fork serving: its guard cannot be open (placed.guardsPrivate).
const noPrivateGuard = "its driver cannot check at a job's start that the repository is still private (no " +
	protocol.CapJITGuardPrivate + "), which a run from a fork or of an untrusted event needs"

// guardStops is why a taker the broker would serve only under fork serving
// does not run on a runner with the trusted events' guard: the box's hook
// stops it before its first step, whatever the broker says.
const guardStops = "its runner's guard serves only the repository's own branches and trusted events: stopped before its first step"

// Why an open runner is retired: the fact the broker learned.
const (
	retirePublic = "the repository is public or internal now, and its runner served forks' runs"
	retireOff    = "forks' runs are no longer served, and its runner served them"
)

// startTries are the waits between reads of GitHub at a job's start
// (decideNow): a few within a couple of seconds, so one failed answer does
// not refuse a job GitHub has started.
var startTries = []time.Duration{0, 300 * time.Millisecond, time.Second}

// takerWhy answers why job d, which took another job's runner, may not run
// there ("" if it may), and whether the broker itself refuses it (so its own
// record says so too: refuseTaker) rather than the runner's guard alone. On
// a runner with the trusted events' guard (not open) a taker that passes
// only under fork serving is stopped by the hook. A served taker whose run
// passed at trustAll is a swap without asking GitHub again.
func (s *Service) takerWhy(ctx context.Context, d delivery, open bool) (string, bool) {
	if j, err := s.job(ctx, d.Job.ID); err == nil && j.state == jobServing {
		switch {
		case j.trusted == trustAll:
			return "", false // holds whatever changed: a swap
		case !open:
			return guardStops, false
		}
	}
	why, level, ours := s.decideNow(ctx, d)
	switch {
	case why != "":
		return why, ours
	case !open && level != trustAll:
		return guardStops, false
	}
	return "", false
}

// decideNow answers why job d, started on a runner of ours, may not run
// there ("" if it may), the level its run passed at (trust), and whether its
// labels are the pool's: its labels, its installation's link, its
// repository's connection and its run, decided as GitHub has the repository
// now (its visibility and id, not the delivery's word) and with
// allow_fork_prs as it is now. GitHub is asked up to len(startTries) times;
// a check that still fails refuses.
func (s *Service) decideNow(ctx context.Context, d delivery) (string, int, bool) {
	_, ours, why := s.labels(d.Job.Labels)
	switch {
	case !ours:
		return "its labels are not the pool's", trustNone, false
	case why != "":
		return why, trustNone, true
	}
	var level int
	var err error
	for _, wait := range startTries {
		select {
		case <-ctx.Done():
			return "its check failed", trustNone, true
		case <-time.After(wait):
		}
		why, level, err = s.decideOnce(ctx, d)
		if err == nil || ghapp.NotFound(err) {
			break
		}
	}
	if err != nil {
		if cl, cerr := s.client(ctx); cerr == nil {
			if _, low := cl.RateLow(d.Installation.ID); low {
				return "its check failed: GitHub's API budget for this installation is nearly spent", trustNone, true
			}
		}
		return "its check failed", trustNone, true
	}
	return why, level, true
}

// decideOnce is one try of decideNow past the labels.
func (s *Service) decideOnce(ctx context.Context, d delivery) (string, int, error) {
	cl, err := s.client(ctx)
	if err != nil {
		return "", trustNone, err
	}
	gr, err := s.repoNow(ctx, cl, d.Installation.ID, d.Repo.FullName)
	if err != nil {
		return "", trustNone, err
	}
	d = d.withRepo(gr)
	if _, why, err := s.admit(ctx, d); err != nil || why != "" {
		return why, trustNone, err
	}
	return s.trusted(ctx, cl, d)
}

// repoNow is GitHub's answer for repository name (cl.Repo). Every read the
// broker acts on goes through it, so a repository GitHub says is public has
// its idle open runners retired (retireOpen) as soon as the broker learns it.
func (s *Service) repoNow(ctx context.Context, cl *ghapp.Client, inst int64, name string) (ghapp.Repo, error) {
	gr, err := cl.Repo(ctx, inst, name)
	if err == nil && gr.Public() {
		s.retireOpen(ctx, name, retirePublic)
	}
	return gr, err
}

// refuseTaker records why on the record of job d, of the pool's, refused on
// a runner it took: it is not served on one of its own either (a box it has
// is released).
func (s *Service) refuseTaker(ctx context.Context, d delivery, why string) error {
	why = "refused when it started on runner " + d.Job.RunnerName + " (" + why + ")"
	if j, err := s.job(ctx, d.Job.ID); err == nil && j.state == jobServing {
		if err := s.move(ctx, d.Job.ID, jobServing, jobServing, clip(why)); err != nil {
			return err
		}
		return s.finishAs(ctx, d.Job.ID, jobIgnored)
	}
	return s.upsert(ctx, d, jobIgnored, why, toServe...)
}

// retireOpen queues the retire (eventRetire) of the idle open runners of
// repo ("": of every repository) once the broker learned why (retirePublic,
// retireOff), if there are any. It is applied in the ends lane, in order
// with in_progress: a runner whose job's start was applied first is busy and
// stays. Nothing is queued for runners GitHub listed busy since the last
// sweep (listedBusy), nor while a
// retire of repo that failed waits for its next try (that try reads again
// from the start). A failure to queue is logged: the start-time decision
// still stands, and the sweep queues the switch-off retire again.
func (s *Service) retireOpen(ctx context.Context, repo, why string) {
	if err := s.queueRetire(ctx, repo, why); err != nil {
		s.logf("runners: queueing the retire of idle runners that served forks (%s): %v", why, err)
	}
}

func (s *Service) queueRetire(ctx context.Context, repo, why string) error {
	q, args := `SELECT runner FROM runners_jobs WHERE state = ? AND open_guard = 1 AND started = 0`, []any{jobServing}
	if repo != "" {
		q, args = q+` AND repo = ? COLLATE NOCASE`, append(args, repo)
	}
	rows, err := s.db().QueryContext(ctx, q, args...)
	if err != nil {
		return err
	}
	idle := false
	for rows.Next() {
		var runner string
		if err := rows.Scan(&runner); err != nil {
			rows.Close()
			return err
		}
		idle = idle || !s.listedBusy(runner)
	}
	rows.Close()
	if !idle {
		return nil
	}
	var failing int
	if err := s.db().QueryRowContext(ctx, `SELECT count(*) FROM runners_inbox WHERE event = ? AND done = 0 AND attempts > 0
		AND json_extract(body, '$.repo') = ? COLLATE NOCASE`, eventRetire, repo).Scan(&failing); err != nil {
		return err
	}
	if failing > 0 {
		return nil
	}
	now := s.now()
	b, _ := json.Marshal(map[string]any{"repo": repo, "why": why})
	_, err = s.db().ExecContext(ctx, `INSERT OR IGNORE INTO runners_inbox (delivery, event, body, next_at, received, lane) VALUES (?, ?, ?, 0, ?, ?)`,
		"retire-"+itoa(now.UnixNano()), eventRetire, b, now.UnixNano(), laneEnds)
	s.kick(laneEnds)
	return err
}

// listedBusy says whether GitHub listed runner busy to a retire.
func (s *Service) listedBusy(runner string) bool {
	s.flight.Lock()
	defer s.flight.Unlock()
	return s.busy[runner]
}

func (s *Service) setListedBusy(runner string) {
	s.flight.Lock()
	defer s.flight.Unlock()
	s.busy[runner] = true
}

// forgetBusy empties listedBusy (the sweep). GitHub can list a runner busy
// whose job was then cancelled before it started, so a busy answer is
// trusted until the next sweep only, and the retire asks GitHub again then.
// A runner really busy usually has its start recorded by then (started),
// which keeps it out of every retire; one whose start the broker has not
// seen is listed again each sweep while GitHub says busy.
func (s *Service) forgetBusy() {
	s.flight.Lock()
	defer s.flight.Unlock()
	clear(s.busy)
}

// retire is eventRetire: every serving job of the repository (or all) whose
// runner has the open guard and that no job has started on yet has its
// runner deleted and its box released, and is served again (unserve, as a
// box taken back: not counted against maxServes; ready decides it again, so
// a fork's run is then ignored and an own branch's gets the trusted events'
// guard). A runner GitHub says is busy is left to the start-time decision,
// and remembered until the next sweep (listedBusy). One job's error does not
// stop the others (retireJob). A retire of every repository is skipped once
// allow_fork_prs is on again. An error (GitHub did not answer) is retried,
// from the start. The sweep queues the switch-off retire again while
// allow_fork_prs is off, so a retire dropped after its tries is not the
// last.
func (s *Service) retire(ctx context.Context, body []byte) error {
	var q struct{ Repo, Why string }
	if json.Unmarshal(body, &q) != nil {
		return nil
	}
	if q.Repo == "" && s.conf().AllowForkPRs {
		return nil
	}
	sel, args := `SELECT job FROM runners_jobs WHERE state = ? AND open_guard = 1 AND started = 0`, []any{jobServing}
	if q.Repo != "" {
		sel, args = sel+` AND repo = ? COLLATE NOCASE`, append(args, q.Repo)
	}
	ids, err := s.jobIDs(ctx, sel+` ORDER BY job`, args...)
	if err != nil {
		return err
	}
	cl, err := s.client(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, id := range ids {
		if err := s.retireJob(ctx, cl, id, q.Why); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// retireJob is retire for one job. Its error does not stop the retire of
// the others: a job GitHub keeps failing for is retried with the next try.
// A runner list GitHub refuses the App (runnersOf) lists no busy runner.
func (s *Service) retireJob(ctx context.Context, cl *ghapp.Client, id int64, why string) error {
	j, err := s.job(ctx, id)
	if err != nil {
		return err
	}
	// A row whose box is not recorded yet is launch's (serve lane): launch
	// reads allow_fork_prs again once the row is serving, the start-time
	// decision holds the rest, and the sweep queues the switch-off retire again.
	if j.state != jobServing || !j.openGuard || j.started || j.handle == "" || s.listedBusy(j.runner) {
		return nil
	}
	l, err := s.link(ctx, j.installation)
	if errors.Is(err, sql.ErrNoRows) {
		return nil // unlinked: its boxes went with the link
	} else if err != nil {
		return err
	}
	rs, err := runnersOf(ctx, cl, j)
	if err != nil {
		return err
	}
	for _, r := range rs {
		if r.Name == j.runner && r.Busy {
			s.setListedBusy(j.runner)
			s.logf("runners: runner %s of job %d is busy: left to the decision at its job's start (%s)", j.runner, id, why)
			return nil
		}
	}
	sub, svc, err := s.poolSubject(ctx, l)
	if err != nil {
		return err
	}
	s.logf("runners: retiring idle runner %s of job %d (%s)", j.runner, id, why)
	if err := s.deleteRunner(ctx, l, j); err != nil {
		return err
	}
	ok, err := s.unserve(ctx, j, jobWaiting, clip("its runner "+j.runner+" was retired ("+why+"): box released, serving it again"))
	if ok {
		s.forget(ctx, l, sub, svc, j)
	}
	return err
}

// jobIDs is the job column of query's rows.
func (s *Service) jobIDs(ctx context.Context, query string, args ...any) ([]int64, error) {
	rows, err := s.db().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
