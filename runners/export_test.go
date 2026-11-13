package runners

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"time"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/runtarget/rt"
)

// HideTargets makes the named bindings' host go away as the broker sees it
// (a host daemon restarting, the network between host and authority down):
// they are not listed and their boxes do not answer, while the boxes
// themselves live on. No names: every host is back.
func (s *Service) HideTargets(names ...string) {
	s.flight.Lock()
	defer s.flight.Unlock()
	s.targets = nil
	if len(names) > 0 {
		s.targets = func(svc rt.Service) rt.Service { return hidden{svc, names} }
	}
}

type hidden struct {
	rt.Service
	names []string
}

func (h hidden) gone(target string) bool {
	for _, n := range h.names {
		if n == target {
			return true
		}
	}
	return false
}

func (h hidden) List(ctx context.Context, sub *authz.Subject, project string, describe bool) (computeapi.RunTargetList, error) {
	l, err := h.Service.List(ctx, sub, project, describe)
	var kept []computeapi.RunTargetView
	for _, t := range l.Targets {
		if !h.gone(t.Name) {
			kept = append(kept, t)
		}
	}
	l.Targets = kept
	return l, err
}

var errAway = api.Errorf(api.CodeUnavailable, "the host does not answer")

func (h hidden) Status(ctx context.Context, sub *authz.Subject, project, target, id string) (computeapi.RunTargetHandle, error) {
	if h.gone(target) {
		return computeapi.RunTargetHandle{}, errAway
	}
	return h.Service.Status(ctx, sub, project, target, id)
}

func (h hidden) Release(ctx context.Context, sub *authz.Subject, project, target, id string) (computeapi.RunTargetRelease, error) {
	if h.gone(target) {
		return computeapi.RunTargetRelease{}, errAway
	}
	return h.Service.Release(ctx, sub, project, target, id)
}

func (h hidden) Renew(ctx context.Context, sub *authz.Subject, project, target, id string, ttlS int) (computeapi.RunTargetHandle, error) {
	if h.gone(target) {
		return computeapi.RunTargetHandle{}, errAway
	}
	return h.Service.Renew(ctx, sub, project, target, id, ttlS)
}

// AnswerStatus puts f between the broker and what the run-target service
// answers for a box's status (target, and the real answer): a box that ended
// by itself, a host that answers for no handle. nil: as it is.
func (s *Service) AnswerStatus(f func(target string, h computeapi.RunTargetHandle, err error) (computeapi.RunTargetHandle, error)) {
	s.flight.Lock()
	defer s.flight.Unlock()
	s.targets = nil
	if f != nil {
		s.targets = func(svc rt.Service) rt.Service { return answered{svc, f} }
	}
}

type answered struct {
	rt.Service
	f func(string, computeapi.RunTargetHandle, error) (computeapi.RunTargetHandle, error)
}

func (a answered) Status(ctx context.Context, sub *authz.Subject, project, target, id string) (computeapi.RunTargetHandle, error) {
	h, err := a.Service.Status(ctx, sub, project, target, id)
	return a.f(target, h, err)
}

// Undone counts deliveries not yet applied (or still to be retried).
func (s *Service) Undone(ctx context.Context) (n int) {
	_ = s.db().QueryRowContext(ctx, `SELECT count(*) FROM runners_inbox WHERE done = 0`).Scan(&n)
	return n
}

// Fits is placement's ranking (place.go) for size and OS.
func Fits(views []computeapi.RunTargetView, size, os string, image ...string) ([]string, string) {
	w := want{size: size, os: os}
	if len(image) > 0 {
		w.image = image[0]
	}
	return fits(views, w, nil, nil, 3600)
}

// FitLabels passes labels through the broker's real parser and placement,
// replacing only the run-target listing with the supplied snapshot.
func (s *Service) FitLabels(views []computeapi.RunTargetView, labels []string) ([]string, string) {
	w, ours, why := s.labels(labels)
	if !ours || why != "" {
		return nil, "labels not served: " + why
	}
	got, why, err := s.place(context.Background(), nil, viewTargets{views: views}, link{}, w, s.jobTTL()+startupAllowance, false)
	if err != nil {
		panic(err)
	}
	var names []string
	for _, p := range got {
		names = append(names, p.name)
	}
	return names, why
}

type viewTargets struct {
	rt.Service
	views []computeapi.RunTargetView
}

func (v viewTargets) List(context.Context, *authz.Subject, string, bool) (computeapi.RunTargetList, error) {
	return computeapi.RunTargetList{Targets: v.views}, nil
}

// Pending counts runners_pending's states (any kind, live or not).
func (s *Service) Pending(ctx context.Context) (n int) {
	_ = s.db().QueryRowContext(ctx, `SELECT count(*) FROM runners_pending`).Scan(&n)
	return n
}

// RecordJob writes a runners_jobs row as a step 1 or earlier launch left it:
// job of inst's repo, ended a day ago (inside Sweep's 30-day retention), its
// runner named runner after mints mints.
func (s *Service) RecordJob(ctx context.Context, job, inst int64, repo, runner string, mints int) error {
	day := s.now().Add(-24 * time.Hour).Unix()
	_, err := s.db().ExecContext(ctx, `INSERT INTO runners_jobs (job, installation, repo, run, labels, state, runner, created, updated, mints)
		VALUES (?, ?, ?, 1, '[]', 'completed', ?, ?, ?, ?)`, job, inst, repo, runner, day, day, mints)
	return err
}

// BreakJobsLookup makes every per-runner record lookup fail (and Mend undoes
// it): the column mintedRunner reads is renamed.
func (s *Service) BreakJobsLookup(ctx context.Context, broken bool) error {
	from, to := "mints", "mints_gone"
	if !broken {
		from, to = to, from
	}
	_, err := s.db().ExecContext(ctx, `ALTER TABLE runners_jobs RENAME COLUMN `+from+` TO `+to)
	return err
}

// AfterRelease observes a host release after it happened but before its caller
// can commit its terminal row. It composes with AnswerStatus.
func (s *Service) AfterRelease(f func()) {
	s.flight.Lock()
	defer s.flight.Unlock()
	prior := s.targets
	s.targets = func(v rt.Service) rt.Service {
		if prior != nil {
			v = prior(v)
		}
		return releaseObserved{v, f}
	}
}

type releaseObserved struct {
	rt.Service
	after func()
}

func (v releaseObserved) Release(ctx context.Context, sub *authz.Subject, project, target, id string) (computeapi.RunTargetRelease, error) {
	h, err := v.Service.Release(ctx, sub, project, target, id)
	v.after()
	return h, err
}

// RefuseRelease models a temporary host failure before it releases a box.
func (s *Service) RefuseRelease(refuse func() bool) {
	s.flight.Lock()
	defer s.flight.Unlock()
	prior := s.targets
	s.targets = func(v rt.Service) rt.Service {
		if prior != nil {
			v = prior(v)
		}
		return releaseRefused{v, refuse}
	}
}

type releaseRefused struct {
	rt.Service
	refuse func() bool
}

func (v releaseRefused) Release(ctx context.Context, sub *authz.Subject, project, target, id string) (computeapi.RunTargetRelease, error) {
	if v.refuse() {
		return computeapi.RunTargetRelease{}, api.Errorf(api.CodeUnavailable, "temporary host failure")
	}
	return v.Service.Release(ctx, sub, project, target, id)
}

// ForgetRepoID leaves repo's connection as the upgrade (runners/0008) makes
// one: no repository id until GitHub's API names it.
func (s *Service) ForgetRepoID(ctx context.Context, repo string) error {
	_, err := s.db().ExecContext(ctx, `UPDATE runners_repos SET repo_id = 0 WHERE repo = ?`, repo)
	return err
}

// AnswerTargets makes the pool's listing views alone (a host whose describe
// failed, one whose advert lapsed and is not listed). nil: as it is.
func (s *Service) AnswerTargets(views []computeapi.RunTargetView) {
	s.flight.Lock()
	defer s.flight.Unlock()
	s.targets = nil
	if views != nil {
		s.targets = func(svc rt.Service) rt.Service { return viewTargets{svc, views} }
	}
}

// RecordServed writes a completed runners_jobs row of repo that target
// served for labels a day ago.
func (s *Service) RecordServed(ctx context.Context, job, inst int64, repo, target string, labels ...string) error {
	lb, _ := json.Marshal(labels)
	day := s.now().Add(-24 * time.Hour).Unix()
	_, err := s.db().ExecContext(ctx, `INSERT INTO runners_jobs (job, installation, repo, run, labels, state, target, created, updated)
		VALUES (?, ?, ?, 1, ?, 'completed', ?, ?, ?)`, job, inst, repo, string(lb), target, day, day)
	return err
}

// LinkAlso links installation to (account's) with the same pool token as
// installation from, and connects repo under it: a second installation of
// one pool.
func (s *Service) LinkAlso(ctx context.Context, from, to int64, account, repo string) error {
	if _, err := s.db().ExecContext(ctx, `INSERT INTO runners_links (installation, account, account_type, project, area, linked_by, token_id, created)
		SELECT ?, ?, account_type, project, area, linked_by, token_id, created FROM runners_links WHERE installation = ?`, to, account, from); err != nil {
		return err
	}
	_, err := s.db().ExecContext(ctx, `INSERT INTO runners_repos (repo, repo_id, installation, by, created) VALUES (?, 1, ?, 'test', 0)`, repo, to)
	return err
}

// CountLists counts the run-target listings the broker asks for from now
// on (it composes with AnswerTargets set before it).
func (s *Service) CountLists() *atomic.Int64 {
	s.flight.Lock()
	defer s.flight.Unlock()
	n := new(atomic.Int64)
	prior := s.targets
	s.targets = func(v rt.Service) rt.Service {
		if prior != nil {
			v = prior(v)
		}
		return listCounted{v, n}
	}
	return n
}

type listCounted struct {
	rt.Service
	n *atomic.Int64
}

func (v listCounted) List(ctx context.Context, sub *authz.Subject, project string, describe bool) (computeapi.RunTargetList, error) {
	v.n.Add(1)
	return v.Service.List(ctx, sub, project, describe)
}

// Every is how long the checker waits between rounds.
func (s *Service) Every() time.Duration { return s.every() }

// Awake sets the checker's heartbeat as if it had beaten since since with
// no sleep (the fixtures' tick is an hour: no beat comes on its own).
func (s *Service) Awake(since time.Time) {
	a := &s.checkState().awake
	a.mu.Lock()
	a.since, a.beat = since, since
	a.mu.Unlock()
}

// JobSecretsState is the state of job's newest job-grant row ("" none):
// planned, issuing, issued, failed or ended.
func (s *Service) JobSecretsState(ctx context.Context, job int64) string {
	var st string
	_ = s.db().QueryRowContext(ctx, `SELECT state FROM runners_job_secrets WHERE job = ? ORDER BY id DESC LIMIT 1`, job).Scan(&st)
	return st
}

// LoseIssuing forgets which job grant rows this process is issuing, as a
// crash and restart would: the issueSecrets still running stands for the
// process that is gone.
func (s *Service) LoseIssuing() {
	s.issuingMu.Lock()
	defer s.issuingMu.Unlock()
	s.issuing = nil
}

// JobSecretsRows is how many job-grant rows job has.
func (s *Service) JobSecretsRows(ctx context.Context, job int64) (n int) {
	_ = s.db().QueryRowContext(ctx, `SELECT COUNT(*) FROM runners_job_secrets WHERE job = ?`, job).Scan(&n)
	return n
}

// AgeJobSecrets moves job's job-grant rows' last change back by d.
func (s *Service) AgeJobSecrets(ctx context.Context, job int64, d time.Duration) error {
	_, err := s.db().ExecContext(ctx, `UPDATE runners_job_secrets SET updated = updated - ? WHERE job = ?`, int64(d/time.Second), job)
	return err
}

// ServeAgainKeepingBox queues job to be served again as when its box was
// taken back, but leaves that box running: as when the release did not
// reach its host.
func (s *Service) ServeAgainKeepingBox(ctx context.Context, job int64) error {
	j, err := s.job(ctx, job)
	if err != nil {
		return err
	}
	if _, err := s.db().ExecContext(ctx, `UPDATE runners_jobs SET state = ?, handle = '', updated = ? WHERE job = ?`, jobTaken, s.now().Unix(), job); err != nil {
		return err
	}
	return s.requeue(ctx, job, j.runner)
}
