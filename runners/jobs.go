package runners

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/ghapp"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/placement"
	"github.com/sengirunners/potluck/runtarget/rt"
)

var (
	sigRe      = regexp.MustCompile(`^sha256=[0-9a-f]{64}$`)
	deliveryRe = regexp.MustCompile(`^[A-Za-z0-9-]{1,100}$`)
	sizeRe     = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,31}$`)
	// ourRunner is what launch names a runner: potluck-<job>[-<n>], or
	// zm-<job>[-<n>] for one named before the rename's step 2 (still cleaned
	// up, adopted and told apart from others' runners).
	ourRunner = regexp.MustCompile(`^(potluck|zm)-([0-9]+)(?:-([0-9]+))?$`)
	// runnerPrefix starts the name of a runner launch makes.
	runnerPrefix = "potluck-"
	// trustedEvents start runs only someone who can push (or GitHub itself)
	// can cause; a pull_request run is from the repository itself once the
	// fork check passed.
	trustedEvents = []string{"push", "pull_request", "workflow_dispatch", "schedule", "merge_group", "release", "repository_dispatch"}
	// GitHub's own runner labels besides self-hosted: a job may name these.
	githubLabels = []string{"linux", "macos", "windows", "x64", "arm64", "arm"}
)

const (
	maxTries     = 6                // failures in a row before a delivery is dropped (a serve waiting for a box is not one)
	maxServes    = 3                // serves of one job: the first and two after its runner was taken or its box failed (a box taken back before the job started is not counted)
	maxMints     = 1000             // runners minted for one job, refused boxes included (a backstop: RefuseCool and queueMax bound it first)
	queueMax     = 24 * time.Hour   // GitHub cancels a job queued this long: a waiting serve stops then
	laneServe    = 0                // queued deliveries and re-serves: a serve waits for a box to boot
	laneEnds     = 1                // in_progress, completed, installation: these end boxes, never behind a serve
	eventRequeue = "zm_requeue"     // the broker's own: serve a job whose runner was taken or box lost (never from GitHub)
	eventRerun   = "zm_rerun"       // the broker's own: re-run the jobs of a run that lost their box while they ran (lost.go)
	eventRetire  = "potluck_retire" // the broker's own: retire idle runners whose open guard no longer holds (guard.go)
)

// hook is POST /v0/runners/hook: the App's deliveries, verified, stored,
// answered 202; the worker applies them.
func (s *Service) hook(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	bad := api.Errorf(api.CodeUnauthenticated, "signature does not match")
	sig := strings.TrimSpace(r.Header.Get("X-Hub-Signature-256"))
	if !sigRe.MatchString(sig) {
		api.WriteError(w, bad)
		return
	}
	secret, err := s.secret(ctx, s.conf().GitHub.WebhookSecret)
	if err != nil || secret == "" {
		api.WriteError(w, bad)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxPayload+1))
	if err != nil || len(body) > maxPayload {
		api.WriteError(w, api.Errorf(api.CodeBadRequest, "payload too large or unreadable"))
		return
	}
	if !ghapp.VerifyHook([]byte(secret), body, sig) {
		api.WriteError(w, bad)
		return
	}
	delivery, event := r.Header.Get("X-GitHub-Delivery"), r.Header.Get("X-GitHub-Event")
	if !deliveryRe.MatchString(delivery) {
		api.WriteError(w, api.Errorf(api.CodeBadRequest, "no X-GitHub-Delivery"))
		return
	}
	if event == "workflow_job" || event == "installation" {
		if err := s.store(ctx, delivery, event, body); err != nil {
			api.WriteError(w, err)
			return
		}
	}
	api.WriteJSON(w, http.StatusAccepted, map[string]bool{"accepted": true})
}

// store puts one of GitHub's deliveries in the inbox for the workers, once
// per delivery id: a webhook's, or one the checking of GitHub made from what
// GitHub listed (check.go). One the broker does not hear (accounts.go) is
// dropped and logged instead. A queued job goes to the serve lane, the rest
// to the ends lane.
func (s *Service) store(ctx context.Context, delivery, event string, body []byte) error {
	if why, err := s.unheard(ctx, event, body); err != nil || why != "" {
		if why != "" {
			s.dropped(event, why)
		}
		return err
	}
	lane := laneEnds
	var a struct {
		Action string `json:"action"`
	}
	if event == "workflow_job" && (json.Unmarshal(body, &a) != nil || a.Action == "queued") {
		lane = laneServe
	}
	if _, err := s.db().ExecContext(ctx, `INSERT OR IGNORE INTO runners_inbox (delivery, event, body, next_at, received, lane) VALUES (?, ?, ?, ?, ?, ?)`,
		delivery, event, body, 0, s.now().UnixNano(), lane); err != nil {
		return err
	}
	s.kick(lane)
	return nil
}

// work applies one lane's deliveries, oldest first, retrying a failed one
// with backoff. A serve waits for a box to boot, so queued jobs have a lane
// of their own with a few workers (Serves): in_progress and completed, which
// release boxes (a refused job's takeover among them), never wait behind
// one, and are applied by one worker in arrival order.
func (s *Service) work(ctx context.Context, lane int) {
	defer s.wg.Done()
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		s.drain(ctx, lane)
		select {
		case <-ctx.Done():
			return
		case <-s.wake[lane]:
		case <-t.C:
		}
	}
}

// item is one due delivery a worker took.
type item struct {
	delivery, event string
	body            []byte
	tries, fails    int
	received, job   int64
}

func (s *Service) drain(ctx context.Context, lane int) {
	for ctx.Err() == nil {
		it, ok := s.next(ctx, lane)
		if !ok {
			return
		}
		s.settle(ctx, lane, it, s.apply(ctx, it.event, it.body))
		s.flight.Lock()
		delete(s.taking[lane], it.delivery)
		if lane == laneServe {
			delete(s.jobsOn, it.job)
		}
		s.flight.Unlock()
		if lane == laneServe {
			s.kick(lane) // a delivery skipped for its job may be next
		}
	}
}

// next takes the oldest due delivery of lane no worker holds, one never
// tried before any retry: serves waiting for a size no box has are made due
// on every freed box (nudge), and taken oldest first they would keep the few
// workers from ever reaching a new job a free box fits. In the serve lane,
// none of a job another worker is serving (one job, one serve at a time).
func (s *Service) next(ctx context.Context, lane int) (item, bool) {
	s.flight.Lock()
	defer s.flight.Unlock()
	rows, err := s.db().QueryContext(ctx, `SELECT delivery, event, body, attempts, fails, received FROM runners_inbox
		WHERE done = 0 AND lane = ? AND next_at <= ? ORDER BY attempts > 0, received LIMIT 64`, lane, s.now().UnixNano())
	if err != nil {
		return item{}, false
	}
	defer rows.Close()
	for rows.Next() {
		var it item
		if rows.Scan(&it.delivery, &it.event, &it.body, &it.tries, &it.fails, &it.received) != nil || s.taking[lane][it.delivery] {
			continue
		}
		it.job = jobOf(it.event, it.body)
		if lane == laneServe && s.jobsOn[it.job] {
			continue
		}
		s.taking[lane][it.delivery] = true
		if lane == laneServe {
			s.jobsOn[it.job] = true
		}
		return it, true
	}
	return item{}, false
}

// jobOf is the job a delivery is about (0: none).
func jobOf(event string, body []byte) int64 {
	var x struct {
		Job int64 `json:"job"`
		WJ  struct {
			ID int64 `json:"id"`
		} `json:"workflow_job"`
	}
	_ = json.Unmarshal(body, &x)
	if event == eventRequeue || event == eventRerun {
		return x.Job
	}
	return x.WJ.ID
}

// waitErr is a serve that cannot go on yet while GitHub still has the job
// queued (no box fits or took it, the API budget is low): retried, at most
// WaitMax apart, never dropped for it until queueMax.
type waitErr struct {
	why   string
	until time.Time // not before this (the budget's reset)
}

func (e *waitErr) Error() string { return e.why }

// settle records a delivery's outcome: done, or its next try.
func (s *Service) settle(ctx context.Context, lane int, it item, err error) {
	if err == nil {
		_, _ = s.db().ExecContext(ctx, `UPDATE runners_inbox SET done = 1, body = x'' WHERE delivery = ?`, it.delivery)
		return
	}
	now := s.now()
	it.tries++
	done := 0
	var gap time.Duration
	var w *waitErr
	if errors.As(err, &w) {
		it.fails = 0
		next := now.Add(min(s.Retry<<min(it.tries-1, 10), s.WaitMax))
		if w.until.After(next) {
			next = w.until
		}
		gap = next.Sub(now)
		if now.Sub(time.Unix(0, it.received)) > queueMax {
			done = 2
			s.giveUp(ctx, it.job, "GitHub kept it queued for a day: given up ("+w.why+")")
			s.rerunDropped(ctx, it, "its run did not end within a day")
		}
	} else if it.fails++; it.fails < maxTries {
		// An error backs off by the errors in a row, not by earlier waits:
		// at most Retry<<(maxTries-2) (8 min at the default 30 s) before
		// the next try, whatever the delivery waited for before.
		gap = s.Retry << (it.fails - 1)
	} else {
		done = 2
		s.logf("runners: delivery %s dropped after %d failed tries: %v", it.delivery, it.fails, err)
		if lane == laneServe {
			s.giveUp(ctx, it.job, fmt.Sprintf("given up after %d failed tries: %s", it.fails, oneLine(err.Error())))
			s.rerunDropped(ctx, it, fmt.Sprintf("GitHub did not take the re-run in %d tries: %s", it.fails, oneLine(err.Error())))
		}
	}
	_, _ = s.db().ExecContext(ctx, `UPDATE runners_inbox SET attempts = ?, fails = ?, next_at = ?, done = ? WHERE delivery = ?`,
		it.tries, it.fails, now.Add(gap).UnixNano(), done, it.delivery)
}

// giveUp fails a job still to be served, saying why.
func (s *Service) giveUp(ctx context.Context, job int64, why string) {
	_, _ = s.db().ExecContext(ctx, `UPDATE runners_jobs SET state = ?, why = ?, updated = ? WHERE job = ? AND state IN (?, ?, ?)`,
		jobFailed, clip(why), s.now().Unix(), job, jobWaiting, jobFailed, jobTaken)
}

// nudge makes every waiting serve due now: a box was freed.
func (s *Service) nudge(ctx context.Context) {
	// Only serves that wait for a box (fails 0): one backing off after an
	// error keeps its backoff.
	_, _ = s.db().ExecContext(ctx, `UPDATE runners_inbox SET next_at = 0 WHERE done = 0 AND lane = ? AND fails = 0 AND attempts > 0 AND next_at > ?`,
		laneServe, s.now().UnixNano())
	s.kick(laneServe)
}

func oneLine(s string) string { return clip(strings.Join(strings.Fields(s), " ")) }

func (s *Service) logf(format string, a ...any) {
	if s.d.Log != nil {
		s.d.Log.Warn(fmt.Sprintf(format, a...))
	}
}

type delivery struct {
	Action string `json:"action"`
	Job    struct {
		ID         int64    `json:"id"`
		RunID      int64    `json:"run_id"`
		Labels     []string `json:"labels"`
		RunnerName string   `json:"runner_name"`
		Conclusion string   `json:"conclusion"`
		RunAttempt int      `json:"run_attempt"`
	} `json:"workflow_job"`
	Repo struct {
		ID         int64  `json:"id"`
		FullName   string `json:"full_name"`
		Private    bool   `json:"private"`
		Visibility string `json:"visibility"`
	} `json:"repository"`
	Installation struct {
		ID int64 `json:"id"`
	} `json:"installation"`
}

// public reports whether d's repository counts as public (ghapp.Public), as
// d says it is: a delivery's word until ready or decideNow set GitHub's
// answer now (withRepo).
func (d delivery) public() bool { return ghapp.Public(d.Repo.Private, d.Repo.Visibility) }

// withRepo is d with its repository as GitHub answers it now.
func (d delivery) withRepo(gr ghapp.Repo) delivery {
	d.Repo.Private, d.Repo.Visibility, d.Repo.ID = gr.Private, gr.Visibility, gr.ID
	return d
}

func (s *Service) apply(ctx context.Context, event string, body []byte) error {
	switch event {
	case eventRequeue:
		return s.reserve(ctx, body)
	case eventRerun:
		return s.rerun(ctx, body)
	case eventRetire:
		return s.retire(ctx, body)
	}
	var d delivery
	if err := json.Unmarshal(body, &d); err != nil {
		return nil // nothing to retry
	}
	switch {
	case event == "installation" && d.Action == "deleted":
		if l, err := s.link(ctx, d.Installation.ID); err == nil {
			return s.dropLink(ctx, l)
		}
	case event == "workflow_job" && d.Action == "queued":
		return s.serve(ctx, d)
	case event == "workflow_job" && d.Action == "in_progress":
		return s.started(ctx, d)
	case event == "workflow_job" && d.Action == "completed":
		return s.ended(ctx, d)
	}
	return nil
}

// upsert writes a job's state and why: a new row, or an existing one still
// in one of the states from (any, when none is given).
func (s *Service) upsert(ctx context.Context, d delivery, state, why string, from ...string) error {
	lb, _ := json.Marshal(d.Job.Labels)
	now := s.now().Unix()
	q := `INSERT INTO runners_jobs (job, installation, repo, run, labels, state, why, created, updated)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT(job) DO UPDATE SET state = excluded.state, why = excluded.why, updated = excluded.updated`
	args := []any{d.Job.ID, d.Installation.ID, d.Repo.FullName, d.Job.RunID, string(lb), state, clip(why), now, now}
	if len(from) > 0 {
		q += ` WHERE runners_jobs.state IN (?` + strings.Repeat(", ?", len(from)-1) + `)`
		for _, f := range from {
			args = append(args, f)
		}
	}
	_, err := s.db().ExecContext(ctx, q, args...)
	return err
}

func (s *Service) record(ctx context.Context, d delivery, state, why string) error {
	return s.upsert(ctx, d, state, why)
}

// toServe: the states a serve may take a job from.
var toServe = []string{jobWaiting, jobFailed, jobTaken}

// wait records why a job waits and answers the waitErr that retries it.
func (s *Service) wait(ctx context.Context, d delivery, why string, until time.Time) error {
	if err := s.upsert(ctx, d, jobWaiting, why, toServe...); err != nil {
		return err
	}
	return &waitErr{why: why, until: until}
}

// serve takes a queued job: the checks of SPEC 20, then placement, mint and
// acquire, retried while GitHub still has it waiting.
func (s *Service) serve(ctx context.Context, d delivery) error {
	j, err := s.job(ctx, d.Job.ID)
	fresh := errors.Is(err, sql.ErrNoRows)
	switch {
	case err == nil && j.state != jobFailed && j.state != jobWaiting:
		return nil // a redelivery: already served, done or ignored
	case err != nil && !errors.Is(err, sql.ErrNoRows):
		return err
	}
	w, ours, why := s.labels(d.Job.Labels)
	if !ours {
		if !fresh { // it was the pool's when it came: the label changed since
			return s.move(ctx, d.Job.ID, j.state, jobIgnored, s.notOurs())
		}
		return nil // another runner's job: not recorded
	}
	if why != "" {
		return s.upsert(ctx, d, jobIgnored, why, jobWaiting, jobFailed)
	}
	l, why, err := s.admit(ctx, d)
	if err != nil {
		return err
	}
	if why != "" {
		return s.upsert(ctx, d, jobIgnored, why, jobWaiting, jobFailed)
	}
	if fresh || j.state == jobFailed {
		if err := s.upsert(ctx, d, jobWaiting, "being checked with GitHub", jobFailed); err != nil {
			return err
		}
	}
	return s.try(ctx, l, d, w, false)
}

// try serves a job a serve may take: GitHub still has it waiting (asked at
// most every CheckEvery once its run passed the checks), the run's checks
// (once), placement, then the boxes in turn. again: a re-serve, which stops
// once GitHub has the job running anywhere.
func (s *Service) try(ctx context.Context, l link, d delivery, w want, again bool) error {
	cl, err := s.client(ctx)
	if err != nil {
		return err
	}
	if until, low := cl.RateLow(d.Installation.ID); low {
		return s.wait(ctx, d, "GitHub's API budget for this installation is nearly spent: waiting for its reset at "+
			until.UTC().Format("15:04 UTC"), until)
	}
	j, err := s.job(ctx, d.Job.ID)
	if err != nil {
		return err
	}
	var observed *ghapp.Job
	var checkedRun *ghapp.Run
	if j.trusted == trustNone || s.now().Unix()-j.checked >= int64(s.CheckEvery/time.Second) {
		// GitHub may deliver a queued job late (a retried delivery, a replay):
		// one it already finished is never served.
		gj, err := cl.Job(ctx, d.Installation.ID, d.Repo.FullName, d.Job.ID)
		if err != nil {
			return err
		}
		observed = &gj
		switch {
		case gj.Status == "completed":
			return s.upsert(ctx, d, jobDone, "completed before it was served", toServe...)
		case gj.Status == "in_progress" && again:
			return s.upsert(ctx, d, jobDone, "GitHub has it running elsewhere: not served again", toServe...)
		case gj.Status == "in_progress" && gj.RunnerName != "" && !ourRunner.MatchString(gj.RunnerName):
			return s.upsert(ctx, d, jobDone, "GitHub gave it to another runner, "+placement.Quote(gj.RunnerName), toServe...)
		}
		if _, err := s.db().ExecContext(ctx, `UPDATE runners_jobs SET checked = ? WHERE job = ?`, s.now().Unix(), d.Job.ID); err != nil {
			return err
		}
	}
	if j.trusted == trustNone { // on the delivery's word; ready decides again on GitHub's before each mint
		run, err := cl.Run(ctx, d.Installation.ID, d.Repo.FullName, d.Job.RunID)
		checkedRun = &run
		if err != nil {
			return err // fails closed: retried, never served unchecked
		}
		why, level := s.trust(run, d)
		if why != "" {
			return s.upsert(ctx, d, jobIgnored, why, toServe...)
		}
		if _, err := s.db().ExecContext(ctx, `UPDATE runners_jobs SET trusted = ? WHERE job = ?`, level, d.Job.ID); err != nil {
			return err
		}
	}
	sub, svc, err := s.poolSubject(ctx, l)
	if err != nil {
		return err
	}
	if err := s.ensureLimit(ctx, cl, d, checkedRun, observed); err != nil {
		return err
	}
	j, err = s.job(ctx, d.Job.ID)
	if err != nil {
		return err
	}
	// A run that passed only under fork serving goes only to a box whose
	// guard can be open (launch checks again on GitHub's answer then).
	targets, why, err := s.place(ctx, sub, svc, l, w, j.limit+startupAllowance, j.trusted == trustForkPRs && s.forkPRs(d))
	if err != nil {
		return err
	}
	if len(targets) == 0 {
		return s.wait(ctx, d, why, time.Time{})
	}
	var refused []string
	for _, target := range targets {
		stop, refusal, err := s.launch(ctx, cl, sub, svc, l, d, target)
		if err != nil || stop {
			return err
		}
		refused = append(refused, target.name+": "+refusal)
	}
	return s.wait(ctx, d, "no box took it: "+strings.Join(refused, "; "), time.Time{})
}

// launch mints the job's next runner on its repository and acquires a box
// on target that starts it, guarded (jit_guard: the box stops a job its
// rules would refuse before that job's first step). It first asks GitHub
// how the repository is now (ready): what is checked, and the guard, go by
// that answer, not by the queued delivery's. stop: served, or nothing
// left to do (it ended meanwhile); else refusal says why target did not take
// it (the runner is deleted again). Runner k of a job is potluck-<job>-<k> with
// request id gh-<job>-<k> (the first: potluck-<job>, gh-<job>), so a retry never
// reuses a request id with another jit_config.
func (s *Service) launch(ctx context.Context, cl *ghapp.Client, sub *authz.Subject, svc rt.Service, l link, d delivery,
	at placed) (stop bool, refusal string, err error) {
	target := at.name
	d, why, err := s.ready(ctx, cl, d)
	if err != nil {
		return false, "", err
	}
	if why != "" {
		return true, "", s.upsert(ctx, d, jobIgnored, why, toServe...)
	}
	// A run that passed only under fork serving needs an open guard, which
	// only a box that can check it gives; on another box's strict guard it
	// would be stopped and fail on GitHub. It waits for one that can.
	if s.forkPRs(d) && !at.guardsPrivate {
		j, err := s.job(ctx, d.Job.ID)
		if err != nil {
			return false, "", err
		}
		if j.trusted != trustAll {
			return false, noPrivateGuard, nil
		}
	}
	// A new runner no job has taken yet: started and runs are of the last one
	// (a stale runs would have the sweep end this box for that runner's job).
	// Serving before the mint, so the sweep can find the runner; never over a
	// job that ended meanwhile (completed is applied in the other lane).
	run, err := s.secretsRun(ctx, cl, l, d, at.reguards) // job grants: the box guarded to the run, nothing minted yet
	if err != nil {
		return false, "", err
	}
	var n int
	// As GitHub has the repository now (ready), and only where the box's guard
	// can check again at the job's start that it is still private (guard).
	open := s.forkPRs(d) && at.guardsPrivate
	err = s.db().QueryRowContext(ctx, `UPDATE runners_jobs SET state = ?, why = '', target = ?, handle = '', started = 0, runs = 0, mints = mints + 1,
		open_guard = ?, updated = ? WHERE job = ? AND state IN (?, ?, ?) AND mints < ? RETURNING mints`, jobServing, target, open, s.now().Unix(), d.Job.ID,
		jobWaiting, jobFailed, jobTaken, maxMints).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		s.giveUpIf(ctx, d.Job.ID)
		return true, "", nil // it ended (or was served) while it was checked
	}
	if err != nil {
		return false, "", err
	}
	// allow_fork_prs read again now the row says open: a switch-off saved
	// before this read is seen here, one after it counts this row (retireOpen).
	if open && !s.forkPRs(d) {
		open = false
		if _, err := s.db().ExecContext(ctx, `UPDATE runners_jobs SET open_guard = 0 WHERE job = ?`, d.Job.ID); err != nil {
			return false, "", err
		}
	}
	name, key := runnerPrefix+itoa(d.Job.ID), "gh-"+itoa(d.Job.ID)
	if n > 1 {
		name, key = name+"-"+strconv.Itoa(n), key+"-"+strconv.Itoa(n)
	}
	if _, err := s.db().ExecContext(ctx, `UPDATE runners_jobs SET runner = ? WHERE job = ?`, name, d.Job.ID); err != nil {
		return false, "", err
	}
	scope := scopeOf(d.Repo.FullName)
	id, jit, err := cl.JITConfig(ctx, d.Installation.ID, scope, name, d.Job.Labels, 0)
	if err != nil {
		_ = s.move(ctx, d.Job.ID, jobServing, jobWaiting, "GitHub refused the runner: "+err.Error())
		return false, "", err
	}
	budget, err := s.job(ctx, d.Job.ID)
	if err != nil {
		return false, "", err
	}
	ttl, leased := budget.limit+startupAllowance, s.now().Unix() // the job's limit from here on, whatever job_ttl_s becomes
	h, err := svc.Acquire(ctx, sub, l.project, target, computeapi.AcquireRunTargetRequest{RequestID: key, TTLS: ttl, JITConfig: jit,
		JITGuard: withRun(guard(d, open), run)})
	jit = ""
	if err != nil {
		_ = cl.DeleteRunner(ctx, d.Installation.ID, scope, id)
		_ = s.move(ctx, d.Job.ID, jobServing, jobWaiting, "no box: "+oneLine(err.Error()))
		s.refusedBy(target, err.Error())
		return false, oneLine(err.Error()), nil // the next box, else retried: a pool that was full may have room later
	}
	// The handle is what releases the box: keep trying to record it. (A crash
	// between the acquire and this leaves a box that lives out job_ttl_s.)
	for try := 0; ; try++ {
		res, err := s.db().ExecContext(ctx, `UPDATE runners_jobs SET handle = ?, why = '', ttl = ?, leased = ?, updated = ? WHERE job = ? AND state = ? AND runner = ?`,
			h.Handle, ttl, leased, s.now().Unix(), d.Job.ID, jobServing, name)
		if err == nil {
			if rows, _ := res.RowsAffected(); rows == 0 {
				// It ended, or a refused job took the runner, while the box
				// started (the other lane): the box and runner are no one's now.
				if _, err := svc.Release(ctx, sub, l.project, target, handleID(h.Handle)); err != nil && !api.IsCode(err, api.CodeNotFound) {
					return true, "", err
				}
				_ = cl.DeleteRunner(ctx, d.Installation.ID, scope, id) // else the sweep's, once idle
			}
			return true, "", nil
		}
		if try == 4 || ctx.Err() != nil {
			return true, "", err
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// giveUpIf fails a job still to be served that has had maxMints runners.
func (s *Service) giveUpIf(ctx context.Context, id int64) {
	if j, err := s.job(ctx, id); err == nil && j.mints >= maxMints && slices.Contains(toServe, j.state) {
		s.giveUp(ctx, id, fmt.Sprintf("%d runners minted and no box kept one: given up", maxMints))
	}
}

// move sets a job's state and why if it is still in state from.
func (s *Service) move(ctx context.Context, job int64, from, to, why string) error {
	_, err := s.db().ExecContext(ctx, `UPDATE runners_jobs SET state = ?, why = ?, updated = ? WHERE job = ? AND state = ?`,
		to, why, s.now().Unix(), job, from)
	return err
}

func handleID(h string) string { return h[strings.LastIndex(h, "/")+1:] }

// admit answers why a pool job is not served from what the delivery says
// ("" if it may be): its installation's link, its repository's connection
// (repos.go). It also answers the link.
func (s *Service) admit(ctx context.Context, d delivery) (link, string, error) {
	l, err := s.link(ctx, d.Installation.ID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return l, "the installation is not linked to a pool area", nil
	case err != nil:
		return l, "", err
	}
	why, err := s.connected(ctx, d, nil)
	return l, why, err
}

// trusted answers why a job's run is not served ("" if it may be), and the
// level it passed at (trust): it is of another repository, from a fork, or
// started by an event outsiders cause. allow_fork_prs serves those of a
// private repository; for a public one nothing does. Its reasons never name
// the switch: they say what is served.
func (s *Service) trusted(ctx context.Context, cl *ghapp.Client, d delivery) (string, int, error) {
	run, err := cl.Run(ctx, d.Installation.ID, d.Repo.FullName, d.Job.RunID)
	if err != nil {
		return "", trustNone, err
	}
	why, level := s.trust(run, d)
	return why, level, nil
}

// trustReason is trusted's answer for run.
func (s *Service) trustReason(run ghapp.Run, d delivery) string {
	why, _ := s.trust(run, d)
	return why
}

// trust is the one rule for which runs are served: why run is not ("" if it
// is), and the level it passed at. A run of the repository's own branches
// started by a trusted event passes at trustAll, whatever forkPRs says;
// another passes only where forkPRs serves it, at trustForkPRs.
func (s *Service) trust(run ghapp.Run, d delivery) (string, int) {
	off := ": this pool serves only its repositories' own branches"
	if d.public() {
		off = ": never served for a public repository"
	}
	why := ""
	switch {
	case !strings.EqualFold(run.Repo, d.Repo.FullName):
		return "the run belongs to another repository", trustNone
	case run.FromFork():
		why = "a run from a fork" + off
	case !slices.Contains(trustedEvents, run.Event):
		// Others (pull_request_target, workflow_run, issue_comment, ...) run
		// the repository's workflow on input from people who cannot push to
		// it, and can check their code out: as untrusted as a fork's run.
		why = "a " + run.Event + " run" + off
	}
	switch {
	case why == "":
		return "", trustAll
	case s.forkPRs(d):
		return "", trustForkPRs
	}
	return why, trustNone
}

// forkPRs reports whether d's runs from forks and of every event are served:
// allow_fork_prs, for a repository that does not count as public only.
func (s *Service) forkPRs(d delivery) bool { return !d.public() && s.conf().AllowForkPRs }

// jobTTL is job_ttl_s as the next job gets it: never under jobTTLMin, so a
// hand-edited value cannot end boxes between sweeps.
func (s *Service) jobTTL() int {
	if t := s.conf().JobTTLS; t > 0 {
		return max(t, s.jobTTLMin())
	}
	return 3600
}

// jobTTLMin is the shortest job_ttl_s: a lease is renewed once a sweep, so
// it outlives three (one sweep may be missed, a second be late).
func (s *Service) jobTTLMin() int { return int(3 * s.SweepEvery / time.Second) }

// renew carries the original lease budget, bounded by the actual job's
// absolute runtime deadline. Where it reaches the host's lifetime cap it
// renews up to that cap, never past it. caps: each project's bindings' caps, listed once a sweep, when a
// renewal was refused.
func (s *Service) renew(ctx context.Context, j job, caps map[string]map[string]int) {
	l, err := s.link(ctx, j.installation)
	if err != nil {
		return
	}
	sub, svc, err := s.poolSubject(ctx, l)
	if err != nil {
		return
	}
	ttl := s.leaseTTL(j)
	active, err := s.activeJob(ctx, j)
	if err != nil {
		return
	}
	if active.deadline > 0 {
		if s.runtimeExpired(ctx, j) {
			return
		}
		ttl = min(ttl, int(active.deadline-s.now().Unix()))
		if ttl <= 0 {
			return
		}
	}
	s.renewing(j.id)
	_, err = svc.Renew(ctx, sub, l.project, j.target, handleID(j.handle), ttl)
	if !api.IsCode(err, api.CodeBadRequest) || j.leased == 0 {
		return
	}
	byName, ok := caps[l.project]
	if !ok {
		list, err := svc.List(ctx, sub, l.project, true)
		if err != nil {
			return
		}
		byName = map[string]int{}
		for _, v := range list.Targets {
			c := v.TTLMaxS
			if v.Describe != nil {
				c = placement.LifeCap(c, v.Describe.TTLMaxS)
			}
			byName[v.Name] = c
		}
		caps[l.project] = byName
	}
	// leased is from just before the acquire, and a second is taken off for
	// the whole seconds both are kept in: never past the host's own count.
	if left := int(j.leased + int64(byName[j.target]) - s.now().Unix() - 1); byName[j.target] > 0 && left > 0 && left < ttl {
		s.renewing(j.id)
		_, _ = svc.Renew(ctx, sub, l.project, j.target, handleID(j.handle), left)
	}
}

// scopeOf is where a job's runner registers: on the job's repository, for an
// org's installation too. An org runner sits in a runner group every repo of
// the org may use (public ones too, where the org allows it), so a job the
// broker refused, from another repo, with the same labels, could take it.
func scopeOf(repo string) ghapp.Scope { return ghapp.Scope{Repo: repo} }

// started checks who took a job (in_progress names the runner). A broker
// runner is for the job it was minted for, but GitHub gives it to any job of
// its repository with its labels, so a job the broker refuses there (a fork's
// run, a pull_request_target) can take it. The box's job-started hook stops
// such a job before its first step (jit_guard: the same rules, as the box
// can check them, guard); here its box is released, the runner deleted, and
// the job the runner was minted for requeued to be served again. A job the
// broker would serve taking it (of any run) is a swap, left alone. One
// without the pool's labels passes the hook (labels are not in its
// environment) and runs until its box goes.
//
// A runner minted while fork serving held for its repository has an open
// guard (guard), unless its box cannot check one. Every job that starts on one, its own too,
// whose run did not pass at trustAll (which holds whatever changed) is
// decided again from how GitHub has the repository now and allow_fork_prs
// as it is now (decideNow), since either may have changed since the mint and
// the retire (retireOpen) may have missed it; its own job refused is not
// served again. A check GitHub does not answer refuses. On a runner with the
// trusted events' guard, a taker whose run passes only under fork serving is
// stopped by the hook whatever the broker says: its job fails on GitHub, and
// the runner's own job is served again. A refused job of the pool's says why
// on its own record too.
func (s *Service) started(ctx context.Context, d delivery) error {
	name := d.Job.RunnerName
	if !ourRunner.MatchString(name) {
		return nil
	}
	var owner int64
	var taken string
	var open bool
	var level int
	err := s.db().QueryRowContext(ctx, `SELECT job, why, open_guard, trusted FROM runners_jobs WHERE runner = ? AND state IN (?, ?) AND installation = ? AND repo = ? COLLATE NOCASE`,
		name, jobServing, jobTaken, d.Installation.ID, d.Repo.FullName).Scan(&owner, &taken, &open, &level)
	if errors.Is(err, sql.ErrNoRows) {
		return nil // not a runner of ours, or already ended
	}
	if err != nil {
		return err
	}
	if owner == d.Job.ID { // a job on its own runner: its job grants' secrets now (jobsecrets.go)
		if !open || level == trustAll { // trustAll holds whatever changed: nothing to decide again
			return s.startedOn(ctx, d, owner)
		}
		why, _, _ := s.decideNow(ctx, d)
		if why == "" {
			return s.startedOn(ctx, d, owner)
		}
		s.logf("runners: job %d started on its runner %s, now refused (%s); releasing its box", owner, name, why)
		if _, err := s.db().ExecContext(ctx, `UPDATE runners_jobs SET why = ? WHERE job = ?`,
			clip("refused when it started on its runner "+name+" ("+why+"): box released"), owner); err != nil {
			return err
		}
		return s.finishAs(ctx, owner, jobIgnored)
	}
	if taken == "" { // a serving job's why is empty; else decided on an earlier try
		why, refused := s.takerWhy(ctx, d, open)
		if why == "" {
			return s.startedOn(ctx, d, owner) // one the broker would serve: a swap
		}
		s.logf("runners: job %d took runner %s of job %d (%s); releasing its box", d.Job.ID, name, owner, why)
		if _, err := s.db().ExecContext(ctx, `UPDATE runners_jobs SET why = ? WHERE job = ?`,
			clip("its runner "+name+" was taken by job "+itoa(d.Job.ID)+" ("+why+"): box released"), owner); err != nil {
			return err
		}
		if refused {
			if err := s.refuseTaker(ctx, d, why); err != nil {
				return err
			}
		}
	}
	if err := s.finishAs(ctx, owner, jobTaken); err != nil {
		return err
	}
	return s.requeue(ctx, owner, name)
}

// markStarted records that job by took the runner of job id: its box is in
// use, so a box that ends now is not a reason to serve id again, but one to
// re-run by (boxLost).
func (s *Service) markStarted(ctx context.Context, id, by int64) error {
	return s.startRuntime(ctx, id, by, nil)
}

// requeue queues the job to be served again, in the serve lane, once per taken runner
// (the delivery id names it). Only the broker makes this event: hook stores
// GitHub's workflow_job and installation alone.
func (s *Service) requeue(ctx context.Context, job int64, from string) error {
	err := s.queueAgain(ctx, s.db(), job, from)
	s.kick(laneServe)
	return err
}

// queueAgain is requeue's insert, in db (a transaction, or the store). An
// item for from that is still to be applied is kept; one already settled is
// due again: a serve that ended is never left without its requeue. The
// caller kicks the serve lane.
func (s *Service) queueAgain(ctx context.Context, db interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}, job int64, from string) error {
	if from == "" {
		return errors.New("requeue of job " + itoa(job) + " without its runner")
	}
	b, _ := json.Marshal(map[string]any{"job": job, "from": from})
	_, err := db.ExecContext(ctx, `INSERT INTO runners_inbox (delivery, event, body, next_at, received, lane) VALUES (?, ?, ?, 0, ?, ?)
		ON CONFLICT(delivery) DO UPDATE SET body = excluded.body, next_at = 0, received = excluded.received, attempts = 0, fails = 0, done = 0
		WHERE runners_inbox.done != 0`,
		"requeue-"+from, eventRequeue, b, s.now().UnixNano(), laneServe)
	return err
}

// unserve ends the serve of job j, not started, on its runner and box, and
// queues the job to be served again (requeue), in one transaction: a serve
// is never ended without its requeue. Only from serving with j's handle and
// no job started on it, so of two paths ending the same serve (boxGone,
// retire) only the first queues it: ok says whether this one did. to is
// jobTaken, counted against maxServes (the box failed on its own), or
// jobWaiting, not counted (its host took the box back or stopped serving,
// or the broker retired its runner). checked = 0: the serve asks GitHub
// first, so a job that runs elsewhere meanwhile (on another job's runner)
// is not served again. The caller has deleted the runner, and releases the
// box once ok (forget). A row whose box is not recorded yet (launch, in the
// serve lane) is not one: its runner name may still be the last serve's.
func (s *Service) unserve(ctx context.Context, j job, to, why string) (ok bool, err error) {
	tx, err := s.db().BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx, `UPDATE runners_jobs SET state = ?, why = ?, handle = '', checked = 0, updated = ? WHERE job = ? AND state = ? AND started = 0 AND handle = ? AND handle != ''`,
		to, why, s.now().Unix(), j.id, jobServing, j.handle)
	if err != nil {
		return false, err
	}
	if rows, _ := res.RowsAffected(); rows == 0 {
		return false, nil
	}
	if err := s.queueAgain(ctx, tx, j.id, j.runner); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	s.kick(laneServe)
	return true, nil
}

// reserve serves a job whose runner was taken, or whose box failed or was
// taken back before it started (requeue), once more, while GitHub still has
// it waiting for a runner: a new runner and box. A taken runner or a failed
// box counts against maxServes; a box taken back does not (boxGone).
func (s *Service) reserve(ctx context.Context, body []byte) error {
	var q struct {
		Job  int64  `json:"job"`
		From string `json:"from"`
	}
	if json.Unmarshal(body, &q) != nil {
		return nil
	}
	j, err := s.job(ctx, q.Job)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil
	case err != nil:
		return err
	case j.state == jobServing && j.runner == q.From:
		return errors.New("the taken runner's box is still being released") // started retries it
	case j.state == jobTaken && j.reserves >= maxServes-1:
		return s.move(ctx, q.Job, jobTaken, jobFailed, fmt.Sprintf("its runner was taken or its box failed %d times: not served again", j.reserves+1))
	case j.state == jobTaken:
		// Re-serving clears only the old box assignment. Each actual job's
		// selected budget and first execution clock remain on its own row.
		res, err := s.db().ExecContext(ctx, `UPDATE runners_jobs SET state = ?, reserves = reserves + 1, checked = 0, updated = ?, started = 0, runs = 0 WHERE job = ? AND state = ?`,
			jobWaiting, s.now().Unix(), q.Job, jobTaken)
		if err != nil {
			return err
		}
		if rows, _ := res.RowsAffected(); rows == 0 {
			return nil
		}
	case j.state != jobWaiting:
		return nil // served again already, or ended
	}
	// The repository's visibility and id are not stored: launch asks GitHub
	// for them (ready) before the runner, as for every serve.
	d := delivery{}
	d.Job.ID, d.Job.RunID, d.Job.Labels, d.Installation.ID, d.Repo.FullName = q.Job, j.run, j.labels, j.installation, j.repo
	l, err := s.link(ctx, d.Installation.ID)
	if errors.Is(err, sql.ErrNoRows) {
		return s.move(ctx, q.Job, jobWaiting, jobFailed, "the installation is not linked to a pool area")
	} else if err != nil {
		return err
	}
	w, ours, _ := s.labels(d.Job.Labels)
	if !ours {
		return s.move(ctx, q.Job, jobWaiting, jobIgnored, s.notOurs())
	}
	return s.try(ctx, l, d, w, true)
}

// notOurs is why a job the pool recorded is not served any more: the pool's
// label changed, and the job's labels do not name the new one.
func (s *Service) notOurs() string {
	return "its labels no longer name the pool: the pool's label is now " + placement.Quote(s.conf().Label) + " (" + s.cmd("runners set label <name>") + "): not served"
}

// poolSubject is zm-runners as the link's token: capped to the pool project
// and the link's area, so the run-target routes' checks, budget and ledger
// apply as for any member of that area.
func (s *Service) poolSubject(ctx context.Context, l link) (*authz.Subject, rt.Service, error) {
	svc := rt.From(s.d)
	if svc == nil {
		return nil, nil, api.Errorf(api.CodeUnavailable, "run targets are off on this machine")
	}
	s.flight.Lock()
	wrap := s.targets
	s.flight.Unlock()
	if wrap != nil {
		svc = wrap(svc)
	}
	sub, err := s.d.SubscriptionSubject(ctx, l.tokenID)
	if err != nil {
		return nil, nil, api.Errorf(api.CodeUnavailable, "the link's token was revoked: link installation %d again", l.Installation)
	}
	return sub, svc, nil
}

type job struct {
	id, installation, run    int64
	runs                     int64 // the job that took its runner (0: none yet)
	repo, state, target, why string
	handle, runner           string
	labels                   []string
	updated, checked         int64
	mints, reserves          int
	started                  bool
	openGuard                bool  // its runner's jit_guard is open (guard)
	trusted                  int   // trustNone, trustForkPRs or trustAll (repos.go)
	ttl                      int   // the lease its box was acquired with
	leased                   int64 // when, just before the acquire
	limit                    int   // this job's selected runtime, independent of its box's actual occupant
	limitSource              string
	startedAt, deadline      int64 // this actual job's execution clock; runs identifies the job on its box
	stopCause                string
}

func (s *Service) job(ctx context.Context, id int64) (job, error) {
	j := job{id: id}
	var labels string
	err := s.db().QueryRowContext(ctx, `SELECT installation, run, repo, state, target, why, handle, runner, labels, updated, checked, mints,
		reserves, started, trusted, runs, ttl, leased, limit_s, limit_source, started_at, deadline, stop_cause, open_guard FROM runners_jobs WHERE job = ?`, id).
		Scan(&j.installation, &j.run, &j.repo, &j.state, &j.target, &j.why, &j.handle, &j.runner, &labels, &j.updated, &j.checked, &j.mints,
			&j.reserves, &j.started, &j.trusted, &j.runs, &j.ttl, &j.leased, &j.limit, &j.limitSource, &j.startedAt, &j.deadline, &j.stopCause, &j.openGuard)
	_ = json.Unmarshal([]byte(labels), &j.labels)
	return j, err
}

// ended is completed. A pool job not served yet is marked done, and one
// whose serve failed or whose runner was taken ends, so a serve still in the
// other lane serves nothing; a served job is finished.
func (s *Service) ended(ctx context.Context, d delivery) error {
	if _, ours, _ := s.labels(d.Job.Labels); ours {
		lb, _ := json.Marshal(d.Job.Labels)
		now := s.now().Unix()
		if _, err := s.db().ExecContext(ctx, `INSERT INTO runners_jobs (job, installation, repo, run, labels, state, why, created, updated)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT(job) DO UPDATE SET state = excluded.state, why = excluded.why, updated = excluded.updated
			WHERE runners_jobs.state IN (?, ?, ?)`, d.Job.ID, d.Installation.ID, d.Repo.FullName, d.Job.RunID, string(lb), jobDone,
			"completed on GitHub before a box took it", now, now, jobWaiting, jobFailed, jobTaken); err != nil {
			return err
		}
	}
	secretsErr := s.endJobSecrets(ctx, d.Job.ID) // revoked now, not only when its box ends
	// Stopped on a box guarded to another run, whichever of GitHub's
	// in_progress and completed came first: recorded before lostEnded reads.
	s.endedOnGuardedBox(ctx, d.Installation.ID, d.Repo.FullName, d.Job.ID, d.Job.RunID, d.Job.RunAttempt, d.Job.RunnerName)
	if err := s.githubEnded(ctx, d.Job.ID, d.Job.Conclusion); err != nil {
		return err
	}
	if err := s.lostEnded(ctx, d.Job.ID, d.Job.RunID, d.Job.RunAttempt, d.Job.Conclusion); err != nil {
		return err
	}
	s.nudge(ctx)      // a box may be free: waiting serves try now
	return secretsErr // the completion not recorded: applied again
}

// jobEnded releases what a job GitHub completed held. The box it ran on is
// the one whose runner it took (runs): its own, or another job's, since
// GitHub gives a runner to any job of the repository with its labels. That
// other job's row loses its box and is served again while GitHub still has it
// queued (reserve asks; one that runs elsewhere or ended is done). The job's
// own box goes too unless another job runs on it: then it stays, renewed,
// until that job ends.
func (s *Service) jobEnded(ctx context.Context, id int64) error {
	rows, err := s.db().QueryContext(ctx, `SELECT job, runner FROM runners_jobs WHERE state = ? AND runs = ? AND job != ?`, jobServing, id, id)
	if err != nil {
		return err
	}
	type held struct {
		job    int64
		runner string
	}
	var others []held
	for rows.Next() {
		var h held
		if err := rows.Scan(&h.job, &h.runner); err != nil {
			rows.Close()
			return err
		}
		others = append(others, h)
	}
	rows.Close()
	for _, h := range others {
		if err := s.finishAs(ctx, h.job, jobTaken); err != nil {
			return err
		}
		if err := s.move(ctx, h.job, jobTaken, jobTaken, "job "+itoa(id)+" ran on its runner "+h.runner+" and ended: box released"); err != nil {
			return err
		}
		if err := s.requeue(ctx, h.job, h.runner); err != nil {
			return err
		}
	}
	j, err := s.job(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil || j.state == jobServing && j.runs != 0 && j.runs != id {
		return err // another job runs on its box
	}
	return s.finish(ctx, id)
}

// finish ends a served job: the box released, the runner gone.
func (s *Service) finish(ctx context.Context, id int64) error { return s.finishAs(ctx, id, jobDone) }

// finishAs is finish ending in state end.
//
// The job ends only with the handle that was released: launch, in the other
// lane, may record its box's handle between the read and the end (the job
// ended, or its runner was taken, while the box started), and that box would
// be no one's until its lease ran out. Then the newer handle is released too.
func (s *Service) finishAs(ctx context.Context, id int64, end string) error {
	for {
		j, err := s.job(ctx, id)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil || j.state != jobServing {
			return err
		}
		l, err := s.link(ctx, j.installation)
		if err == nil && j.handle != "" {
			if sub, svc, perr := s.poolSubject(ctx, l); perr == nil {
				if _, rerr := svc.Release(ctx, sub, l.project, j.target, handleID(j.handle)); rerr != nil && !api.IsCode(rerr, api.CodeNotFound) {
					if !s.leaseLapsed(j) {
						return rerr
					}
					s.logf("runners: job %d: its box on %s was not renewed for its whole lease and the host did not answer the release (%s): row ended",
						id, j.target, oneLine(rerr.Error()))
				}
			}
		}
		if err == nil {
			if derr := s.deleteRunner(ctx, l, j); derr != nil {
				return derr
			}
		}
		res, err := s.db().ExecContext(ctx, `UPDATE runners_jobs SET state = ?, updated = ? WHERE job = ? AND state = ? AND handle = ?`,
			end, s.now().Unix(), id, jobServing, j.handle)
		if err != nil {
			return err
		}
		if rows, _ := res.RowsAffected(); rows > 0 {
			return nil
		}
		// The handle changed (or the job ended) meanwhile: read it again.
	}
}

func (s *Service) deleteRunner(ctx context.Context, l link, j job) error {
	cl, err := s.client(ctx)
	if err != nil || j.runner == "" {
		return err
	}
	scope := scopeOf(j.repo)
	rs, err := runnersOf(ctx, cl, j)
	if err != nil {
		return err
	}
	for _, r := range rs {
		if r.Name == j.runner {
			if err := cl.DeleteRunner(ctx, l.Installation, scope, r.ID); err != nil {
				return err
			}
		}
	}
	return nil
}

// runnersOf lists job j's runner (by its name) in j's repository. A list
// GitHub refuses the App (ghapp.NoRepoAccess: the repository gone, out of
// the installation, or no permission for its runners) lists none: the
// broker can neither see nor delete a runner there, and its callers release
// the box, which is what stops the runner.
func runnersOf(ctx context.Context, cl *ghapp.Client, j job) ([]ghapp.Runner, error) {
	rs, err := cl.Runners(ctx, j.installation, scopeOf(j.repo), j.runner)
	if ghapp.NoRepoAccess(err) {
		return nil, nil
	}
	return rs, err
}

// sweeper runs the sweep every SweepEvery.
func (s *Service) sweeper(ctx context.Context) {
	defer s.wg.Done()
	t := time.NewTicker(s.SweepEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.Sweep(ctx)
		}
	}
}

// Sweep releases boxes whose job (the one that runs on the box) ended without
// a delivery (renewing those still running), deletes our runners (potluck-*, zm-*) no live job holds, reads
// connected repositories' settings due again (checkRepos), queues the retire of idle runners that served
// forks while allow_fork_prs is off (retireOpen), and forgets old deliveries and states (jobsKept).
func (s *Service) Sweep(ctx context.Context) {
	if s.conf().GitHub.AppID == 0 {
		return
	}
	cl, err := s.client(ctx)
	if err != nil {
		return
	}
	rows, err := s.db().QueryContext(ctx, `SELECT job FROM runners_jobs WHERE state = ?`, jobServing)
	if err != nil {
		return
	}
	var serving []int64
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			serving = append(serving, id)
		}
	}
	rows.Close()
	held, caps := map[string]bool{}, map[string]map[string]int{}
	for _, id := range serving {
		j, err := s.job(ctx, id)
		if err != nil {
			continue
		}
		held[j.runner] = true
		on := cmp.Or(j.runs, id) // the job on its box: the one that took its runner
		gj, err := cl.Job(ctx, j.installation, j.repo, on)
		switch {
		case err != nil:
			// GitHub not answering is not a running job ending: its box is
			// kept. (No such job is GitHub's word that it ended; a job not
			// started has no deadline to bound the renewals: not renewed.)
			if j.handle != "" && j.started && !ghapp.NotFound(err) {
				s.renew(ctx, j, caps)
			}
		case gj.Status == "completed":
			if on != id { // its completion never came: sweepLost re-runs it if it was stopped there
				s.endedOnGuardedBox(ctx, j.installation, j.repo, on, gj.RunID, gj.RunAttempt, j.runner)
			}
			_ = s.githubEnded(ctx, on, gj.Conclusion)
		case j.handle != "":
			if !j.started && gj.Status == "in_progress" && gj.RunnerName == j.runner {
				if s.startRuntime(ctx, j.id, on, &gj) != nil {
					continue
				}
				j, _ = s.job(ctx, j.id)
			}
			s.renew(ctx, j, caps)
		}
	}
	s.sweepLost(ctx, cl)
	s.sweepOrphans(ctx, cl, held)
	s.forgetBusy()
	s.checkRepos(ctx, cl)
	if !s.conf().AllowForkPRs { // a retire dropped after its tries is queued again
		s.retireOpen(ctx, "", retireOff)
	}
	now := s.now()
	_, _ = s.db().ExecContext(ctx, `DELETE FROM runners_inbox WHERE done != 0 AND received < ?`, now.Add(-7*24*time.Hour).UnixNano())
	_, _ = s.db().ExecContext(ctx, `DELETE FROM runners_pending WHERE expires <= ?`, now.Unix())
	_, _ = s.db().ExecContext(ctx, `DELETE FROM runners_jobs WHERE state != ? AND updated < ?`, jobServing, now.Add(-jobsKept).Unix())
	_, _ = s.db().ExecContext(ctx, `DELETE FROM runners_lost WHERE updated < ?`, now.Add(-jobsKept).Unix())
	_, _ = s.db().ExecContext(ctx, `DELETE FROM runners_bounce_reruns WHERE updated < ?`, now.Add(-jobsKept).Unix())
	s.forgetJobSecrets(ctx, now.Add(-jobsKept))
}

// jobsKept is how long the records of a job that ended are kept (its
// runners_jobs row, its lost record, its job grant rows).
const jobsKept = 30 * 24 * time.Hour

// sweepOrphans deletes our runners (potluck-<job>, zm-<job>) that no serving job holds and that are
// not running a job, once they are older than OrphanAge by our record. A
// runner is ours only when its name matches launch's AND our record of that
// job (runners_jobs, same installation and repository) shows launch minted
// it: an idle runner of someone else's named like ours is never deleted,
// nor is any runner when the record cannot be read (this round). A record
// is forgotten 30 days after its job ended (Sweep); a runner of ours older
// than that stays for its owner or GitHub to remove, the safe direction.
func (s *Service) sweepOrphans(ctx context.Context, cl *ghapp.Client, held map[string]bool) {
	rows, err := s.db().QueryContext(ctx, `SELECT DISTINCT j.installation, j.repo FROM runners_jobs j JOIN runners_links l ON l.installation = j.installation
		WHERE j.updated > ?`, s.now().Add(-7*24*time.Hour).Unix())
	if err != nil {
		return
	}
	type ir struct {
		inst int64
		repo string
	}
	var seen []ir
	for rows.Next() {
		var x ir
		if rows.Scan(&x.inst, &x.repo) == nil {
			seen = append(seen, x)
		}
	}
	rows.Close()
	done := map[string]bool{}
	cutoff := s.now().Add(-s.OrphanAge).Unix()
	for _, x := range seen {
		if _, err := s.link(ctx, x.inst); err != nil {
			continue
		}
		scope := scopeOf(x.repo)
		key := itoa(x.inst) + "|" + scope.Repo
		if done[key] {
			continue
		}
		rs, err := cl.Runners(ctx, x.inst, scope, "") // every runner: mintedRunner picks ours under either name
		if err != nil {
			continue // an org's next repo may still mint (this one gone from the installation)
		}
		done[key] = true
		for _, r := range rs {
			if held[r.Name] || r.Busy || !ourRunner.MatchString(r.Name) {
				continue // a runner someone else named potluck-something or zm-something is not ours
			}
			updated, ours, err := s.mintedRunner(ctx, x.inst, x.repo, r.Name)
			switch {
			case err != nil:
				s.logf("runners: orphan sweep: reading the record of runner %s: %v; skipped this round", r.Name, err)
				continue
			case !ours:
				continue // no record of launch minting it here: someone else's
			case updated > cutoff:
				continue // just ended or just minted: give its delivery time
			}
			_ = cl.DeleteRunner(ctx, x.inst, scope, r.ID)
		}
	}
}

// mintedRunner reports whether launch minted runner name for a job of
// installation inst's repo: the name is <prefix>-<job>[-<k>], our record of
// that job is for the same installation and repository, and k (1 without a
// suffix) is at most the record's mints. updated is the record's.
func (s *Service) mintedRunner(ctx context.Context, inst int64, repo, name string) (updated int64, ok bool, err error) {
	m := ourRunner.FindStringSubmatch(name)
	if m == nil {
		return 0, false, nil
	}
	job, err := strconv.ParseInt(m[2], 10, 64)
	if err != nil {
		return 0, false, nil
	}
	k := int64(1)
	if m[3] != "" {
		if k, err = strconv.ParseInt(m[3], 10, 64); err != nil {
			return 0, false, nil
		}
	}
	var mints int64
	err = s.db().QueryRowContext(ctx, `SELECT updated, mints FROM runners_jobs WHERE job = ? AND installation = ? AND repo = ? COLLATE NOCASE`,
		job, inst, repo).Scan(&updated, &mints)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return updated, k >= 1 && k <= mints, nil
}

// watcher runs Watch every WatchEvery.
func (s *Service) watcher(ctx context.Context) {
	defer s.wg.Done()
	t := time.NewTicker(s.WatchEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.Watch(ctx)
		}
	}
}

// Watch checks the box of each served job: a box its host took back or lost
// (reclaimed by its machine's owner, expired, lost, or no such handle there)
// or whose host stopped advertising it is no one's now. Before a job started
// on it, the job is served again (requeue) while GitHub still has it waiting;
// once one runs on it, that job is re-run when GitHub has ended it (boxLost,
// lost.go). A box that ended by itself (ended) was not taken: never a loss.
// A box that is there costs no GitHub call. A host that went away keeps its
// box until its lease (the job's limit plus the startup allowance, no longer
// renewed) ends it; the runner it
// holds is deleted.
func (s *Service) Watch(ctx context.Context) {
	rows, err := s.db().QueryContext(ctx, `SELECT job FROM runners_jobs WHERE state = ? AND handle != ''`, jobServing)
	if err != nil {
		return
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	s.flight.Lock()
	for id := range s.unlisted {
		if !slices.Contains(ids, id) {
			delete(s.unlisted, id)
		}
	}
	for id := range s.goneAsk {
		if !slices.Contains(ids, id) {
			delete(s.goneAsk, id)
		}
	}
	for id := range s.renewed {
		if !slices.Contains(ids, id) {
			delete(s.renewed, id)
		}
	}
	s.flight.Unlock()
	listed := map[string]map[string]bool{} // project -> the bindings it lists now
	for _, id := range ids {
		if ctx.Err() != nil {
			return
		}
		j, err := s.job(ctx, id)
		if err != nil {
			continue
		}
		if s.runtimeExpired(ctx, j) {
			continue
		}
		l, err := s.link(ctx, j.installation)
		if err != nil {
			continue
		}
		sub, svc, err := s.poolSubject(ctx, l)
		if err != nil {
			continue
		}
		// A runner that has not started within the startup allowance on a
		// box its host still has: the box failed, counted (boxGone). Acted
		// on only when the host lists the box and says nothing else about
		// it (a box taken back or a host that stopped serving is never
		// counted as one), or when the listing itself fails.
		unstarted := !j.started && j.leased > 0 && s.now().Unix() >= j.leased+startupAllowance
		names, ok := listed[l.project]
		if !ok {
			list, err := svc.List(ctx, sub, l.project, false)
			if err != nil {
				if unstarted {
					s.boxGone(ctx, l, sub, svc, j, "runner did not start within the 300s startup allowance", false, true)
				}
				continue
			}
			names = map[string]bool{}
			for _, t := range list.Targets {
				names[t.Name] = true
			}
			listed[l.project] = names
		}
		if !names[j.target] {
			// A host's advert lapses after 60 s without a beat (a sleeping
			// laptop, a restart): gone only once it stays away HostGrace.
			s.flight.Lock()
			since, seen := s.unlisted[id]
			if !seen {
				s.unlisted[id], since = s.now(), s.now()
			}
			s.flight.Unlock()
			// Not listed is not the host saying the box ended (its daemon
			// restarts, the network to it drops): a job that runs there keeps
			// its box, renewed, until the host answers that it ended or GitHub
			// ends the job. One still queued is served elsewhere meanwhile.
			if !j.started && s.now().Sub(since) >= s.HostGrace {
				s.boxGone(ctx, l, sub, svc, j, "its host stopped serving "+j.target+" before the job started", false, false)
			}
			continue
		}
		s.flight.Lock()
		delete(s.unlisted, id)
		s.flight.Unlock()
		h, err := svc.Status(ctx, sub, l.project, j.target, handleID(j.handle))
		switch {
		case err == nil && (h.State == "reclaimed" || h.State == "expired" || h.State == "lost"):
			// Only reclaimed is the host taking it back; expired and lost are
			// the box failing (its VM stopped, its supervisor died).
			s.boxEnded(ctx, l, sub, svc, j, "its box on "+j.target+" ended ("+h.State+")", h.State != "reclaimed")
		case err == nil && h.State == "ended":
			// The box's own process finished: nobody took it. No conforming
			// box says so (docs/protocol.md: a runner's exit leaves its box
			// ready); from a driver that does, a job that ran on it ended
			// with it and its completed (or the sweep) closes the row, never
			// a loss. One that had not started is served again.
			if !j.started {
				s.boxGone(ctx, l, sub, svc, j, "its box on "+j.target+" ended by itself before the job started", false, true)
			}
		case rt.IsNoHandle(err):
			// Only a not_found about the handle itself: one for any other
			// reason (a host out of the project) says nothing about the box.
			s.boxEnded(ctx, l, sub, svc, j, "its box on "+j.target+" is gone", true)
		case unstarted:
			s.boxGone(ctx, l, sub, svc, j, "runner did not start within the 300s startup allowance", false, true)
		}
	}
}

// boxEnded is the host itself saying j's box ended or is gone: before a job
// started on it, boxGone; after, boxLost.
// failed: as boxGone's.
func (s *Service) boxEnded(ctx context.Context, l link, sub *authz.Subject, svc rt.Service, j job, why string, failed bool) {
	if j.started {
		s.boxLost(ctx, l, sub, svc, j, why, nil)
		return
	}
	s.boxGone(ctx, l, sub, svc, j, why+" before the job started", true, failed)
}

// boxGone serves j again: its box is no one's (released is never this: the
// broker or the box's holder chose it). Only while GitHub still has the job
// queued and its runner could be deleted (GitHub refuses to delete a busy
// one): a job that did start there, its in_progress not applied yet, lost
// its box while it ran (boxLost). The box is released in the background (a
// vanished host's relay waits); its runner is gone, so it serves nothing.
//
// hostSaid: the host answered that the box ended (else it is only unlisted).
// A job GitHub has running on the box lost it only on the host's word; on an
// unlisted host it is left to run.
//
// failed: the box failed on its own (its runner never started, it ended by
// itself, expired, was lost or is gone), which points at the host: the job is
// then taken, and counts against maxServes like a taken runner, and its
// binding cools like one that refused a box (refusedBy), so the job is served
// again on the next box that fits rather than on the binding that just failed
// it. A box its
// host took back (reclaimed) or stopped serving (a drain, a close, its owner
// back) leaves the job waiting, served
// again each time: GitHub cancelling the queued job (queueMax) and maxMints
// bound it. Each round costs a mint and a few GitHub calls (RateLow stops
// them all near the installation's limit), at most once a Watch for a host
// that takes every box back, once a HostGrace for one that keeps leaving.
func (s *Service) boxGone(ctx context.Context, l link, sub *authz.Subject, svc rt.Service, j job, why string, hostSaid, failed bool) {
	cl, err := s.client(ctx)
	if err != nil {
		return
	}
	// At most one ask per job per CheckEvery when it leads nowhere (GitHub
	// does not answer, the runner is busy), never on a low budget.
	s.flight.Lock()
	next, held := s.goneAsk[j.id]
	s.flight.Unlock()
	if _, low := cl.RateLow(j.installation); low || held && s.now().Before(next) {
		return
	}
	s.flight.Lock()
	s.goneAsk[j.id] = s.now().Add(s.CheckEvery)
	s.flight.Unlock()
	gj, err := cl.Job(ctx, j.installation, j.repo, j.id)
	again := jobWaiting
	if failed {
		again = jobTaken
	}
	state, note := again, why+": serving it again"
	switch {
	case err != nil:
		return
	case gj.Status == "completed": // finished (cancelled while queued, say): its box's release failed
		state, note = jobDone, why+"; GitHub has it completed"
	case gj.Status == "in_progress" && gj.RunnerName == j.runner:
		if s.startRuntime(ctx, j.id, j.id, &gj) == nil && hostSaid {
			j.started, j.runs = true, j.id
			s.boxLost(ctx, l, sub, svc, j, strings.TrimSuffix(why, " before the job started"), &gj)
		}
		return
	}
	if err := s.deleteRunner(ctx, l, j); err != nil {
		return // busy, or GitHub did not answer: asked again after CheckEvery
	}
	if state == jobDone {
		res, err := s.db().ExecContext(ctx, `UPDATE runners_jobs SET state = ?, why = ?, handle = '', updated = ? WHERE job = ? AND state = ? AND started = 0 AND handle = ?`,
			state, note, s.now().Unix(), j.id, jobServing, j.handle)
		if err != nil {
			return
		}
		if rows, _ := res.RowsAffected(); rows == 0 {
			return
		}
	} else {
		if state == jobTaken {
			s.refusedBy(j.target, why) // before unserve, whose requeue serves it at once
		}
		if ok, err := s.unserve(ctx, j, state, note); err != nil || !ok {
			return
		}
	}
	s.logf("runners: job %d: %s", j.id, note)
	s.forget(ctx, l, sub, svc, j)
}

// JobView is one job as GET /v0/runners/jobs shows it.
type JobView struct {
	ID          int64    `json:"job_id"`
	Repo        string   `json:"repo"`
	Run         int64    `json:"run_id"`
	Labels      []string `json:"labels"`
	State       string   `json:"state"`
	Why         string   `json:"why,omitempty"`
	Target      string   `json:"target,omitempty"` // the box binding it was placed on: <binding>@<machine> for a host's
	Runner      string   `json:"runner,omitempty"`
	Created     string   `json:"created"`
	Updated     string   `json:"updated"`
	LimitS      int      `json:"limit_s"`
	LimitSource string   `json:"limit_source"`
	StartedAt   int64    `json:"started_at,omitempty"`
	Deadline    int64    `json:"deadline,omitempty"`
}

// jobs is GET /v0/runners/jobs: the latest jobs (?limit, default 50, at
// most 500) of links in the caller's readable areas, newest first.
func (s *Service) jobs(r *http.Request) (any, error) {
	ctx := r.Context()
	p, err := s.readablePool(r)
	if err != nil {
		return nil, err
	}
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err != nil || n < 1 || n > 500 {
			return nil, api.Errorf(api.CodeBadRequest, "limit is 1 to 500")
		} else {
			limit = n
		}
	}
	areaSQL, args := core.SubjectOf(ctx).Scope.AreaSQL(p.ID, "l.area", authz.Read)
	rows, err := s.db().QueryContext(ctx, `SELECT j.job, j.repo, j.run, j.labels, j.state, j.why, j.target, j.runner, j.created, j.updated, j.limit_s, j.limit_source, j.started_at, j.deadline, j.stop_cause
		FROM runners_jobs j JOIN runners_links l ON l.installation = j.installation WHERE l.project = ? AND `+areaSQL+`
		ORDER BY j.updated DESC, j.job DESC LIMIT ?`, append(append([]any{p.ID}, args...), limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []JobView{}
	for rows.Next() {
		var v JobView
		var labels string
		var created, updated int64
		var stopCause string
		if err := rows.Scan(&v.ID, &v.Repo, &v.Run, &labels, &v.State, &v.Why, &v.Target, &v.Runner, &created, &updated, &v.LimitS, &v.LimitSource, &v.StartedAt, &v.Deadline, &stopCause); err != nil {
			return nil, err
		}
		if why := timeoutReason(stopCause, v.LimitS, v.LimitSource); why != "" {
			v.Why = why
		}
		_ = json.Unmarshal([]byte(labels), &v.Labels)
		v.Created, v.Updated = time.Unix(created, 0).UTC().Format(time.RFC3339), time.Unix(updated, 0).UTC().Format(time.RFC3339)
		out = append(out, v)
	}
	return map[string]any{"jobs": out}, rows.Err()
}
