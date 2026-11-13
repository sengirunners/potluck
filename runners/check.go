package runners

// Checking GitHub for waiting jobs (zm-eakh.12.54.5). The broker asks GitHub
// for the queued and running workflow runs of its connected repositories
// (runners_repos, read afresh each round) and their jobs, so it serves jobs with no public address (no webhook can reach a
// laptop at home), and catches up on jobs queued while it was asleep or
// stopped. What it finds becomes the same workflow_job deliveries a webhook
// brings, stored in the inbox under the checker's own delivery ids: the same
// workers serve them, through the same checks (serve, admit, trusted), and a
// webhook's copy of one is harmless (serve, started and ended are
// idempotent per job).
//
// With no public_url the broker checks every PollEvery (default 15 s); with
// one, GitHub's webhooks serve and the check catches up every CatchUp
// (default 5 min). Both check on start and on wake from sleep: the schedule
// is by the wall clock, which moves while a Mac sleeps, so the first tick
// after a wake finds the check due; a clock stepped back counts as due too.
//
// Every list is read page by page, each page a conditional request
// (ghapp/lists.go): an idle repository costs only 304s, which GitHub does not
// count against the installation's hourly budget. What the checker does
// spend is capped per installation by PollBudget, a bucket refilled by the
// hour and kept in runners_checks so a restart does not refill it; it stops
// an installation's round when GitHub says its budget is low or refuses for
// a rate limit (RateLow, until GitHub's reset or Retry-After), as waiting
// serves do.
//
// Every call names its one repository, with a token for that repository
// only (ghapp's cache): the checker never holds a token for a whole
// installation, and never asks about a repository that is not connected.
//
// A list that fails is recorded and skipped: the round goes on with the rest,
// and that list (one repository's runs, one run's jobs) is asked again after twice as long each time it fails again,
// up to CatchUp, while the failure stays in the round's record.

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sengirunners/potluck/ghapp"
	"github.com/sengirunners/potluck/kit/store"
)

var checkMigrations = []store.Migration{
	// The checker's last round per installation, for doctor (GET
	// /v0/runners/setup "check"): when it last heard every list it asks for,
	// and its last errors or cut short round; and its own budget's bucket
	// (tokens as of filled, unix ms), which a restart reads back.
	{Kind: store.KindRegistry, Name: "runners/0007_checks", Script: `CREATE TABLE runners_checks (
	installation INTEGER PRIMARY KEY,
	repos        INTEGER NOT NULL DEFAULT 0,
	last_ok      INTEGER NOT NULL DEFAULT 0,
	error        TEXT NOT NULL DEFAULT '',
	error_at     INTEGER NOT NULL DEFAULT 0,
	cut          TEXT NOT NULL DEFAULT '',
	cut_at       INTEGER NOT NULL DEFAULT 0,
	tokens       REAL NOT NULL DEFAULT 0,
	filled       INTEGER NOT NULL DEFAULT 0
);`},
	// failing_since: when the installation's rounds began failing without a
	// complete one between (0: the last round had no failed list, complete
	// or only cut short). A gap with no failing round (asleep) starts it
	// again.
	{Kind: store.KindRegistry, Name: "runners/0010_checks_failing", Script: `ALTER TABLE runners_checks ADD COLUMN failing_since INTEGER NOT NULL DEFAULT 0;`},
}

const (
	defaultPollEvery    = 15 * time.Second
	defaultCatchUpEvery = 5 * time.Minute
	defaultPollTick     = time.Second
	defaultPollBudget   = 1500 // counted calls an hour per installation: under a third of GitHub's 5000
	// jobsEvery: a run still queued or running has its jobs listed at least
	// this often, though its run did not change (a job that queues behind
	// another one may not touch the run's updated_at).
	jobsEvery = 2 * time.Minute
	// pollPerMinute bounds the checker's requests a minute (304s included):
	// far under GitHub's secondary limit of 900 a minute. Many repositories
	// stretch the round.
	pollPerMinute = 300

	budgetSpent = "the checker's own budget for this installation is spent: it refills within the hour"
)

// runStatuses are the run statuses whose jobs may be waiting for a runner.
var runStatuses = []string{"queued", "in_progress", "waiting"}

// checks is the checker's memory: the lists as last answered (with their
// ETags), and each failing list's back-off. Lost at a restart, which costs
// one counted round; the budget is not (runners_checks).
type checks struct {
	mu    sync.Mutex // one round at a time
	insts map[int64]*instCheck
	last  time.Time    // when the last round started
	span  atomic.Int64 // how long the last round that ran to its end took (ns)
	repos atomic.Int64 // connected repositories of every installation, as last read
	awake awake        // the heartbeat: since when this daemon has been awake
	// failed is why the last round could not begin (no App client, the
	// links unreadable); nil once one did.
	failed atomic.Pointer[checkFailure]
}

// awake is the heartbeat's record (awakeLoop): its last beat, and since when
// it has beaten with no sleep or clock step between. It has its own lock: a
// round in progress holds checks.mu, and a stuck one must not stop the beat.
type awake struct {
	mu    sync.Mutex
	beat  time.Time
	since time.Time
}

// tick records a beat at now and returns the step from the last one, and
// whether the step was a sleep or a clock moved (longer than most, or back):
// awake then starts again at now.
func (a *awake) tick(now time.Time, most time.Duration) (step time.Duration, moved bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	step = now.Sub(a.beat)
	moved = !a.beat.IsZero() && (step > most || step < -time.Minute)
	if a.since.IsZero() || moved {
		a.since = now
	}
	a.beat = now
	return step, moved
}

// Since is when the daemon was last seen to start or wake (zero: no beat yet).
func (a *awake) Since() time.Time {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.since
}

type checkFailure struct {
	err   string
	at    time.Time
	since time.Time // the first of the rounds in a row that could not begin
}

type instCheck struct {
	repos  []string // its connected repositories, as the round last read them
	tokens float64
	filled time.Time
	// start is the repository the next round begins with (lower case; "" the
	// first): a cut round's next goes on from there, though repositories were
	// connected or disconnected meanwhile.
	start string
	repo  map[string]*repoCheck
	noted noted // what runners_checks last got
	// back is the installation failing as a whole (GitHub has no such
	// installation, or refuses its token: ghapp.WholeInstallation): one
	// back-off, not one per repository.
	back backoff
}

// noted is a round as runners_checks records it.
type noted struct {
	at           time.Time
	heard        bool
	repos        int
	errText, cut string
}

type repoCheck struct {
	runs map[string]ghapp.List[ghapp.Run] // run status -> its list as last answered
	seen map[int64]*runCheck              // runs active at the last round
	back backoff                          // its run lists failing
}

type runCheck struct {
	run     ghapp.Run                   // as its repository's lists last answered it
	updated time.Time                   // its updated_at when its jobs were last listed and stored
	jobs    ghapp.List[ghapp.ListedJob] // as last listed and stored
	at      time.Time                   // jobs last listed and stored
	back    backoff                     // its job list (or storing what it says) failing
}

// backoff is a list that keeps failing: after the nth failure in a row it is
// asked again after base·2ⁿ, at most most.
type backoff struct {
	fails int
	next  time.Time
	err   string
}

// due reports whether the list may be asked now: it has not failed, its wait
// is over, or the wait left is longer than any (the clock moved back).
func (b *backoff) due(now time.Time, most time.Duration) bool {
	return b.fails == 0 || !now.Before(b.next) || b.next.Sub(now) > most
}

// why is the failure as a round records it.
func (b *backoff) why(most time.Duration) string {
	if b.fails < 2 {
		return b.err
	}
	return fmt.Sprintf("%s (%d failures in a row: asked again less often, at least every %s)", b.err, b.fails, most)
}

func (s *Service) pollEvery() time.Duration    { return cmp.Or(s.PollEvery, defaultPollEvery) }
func (s *Service) catchUpEvery() time.Duration { return cmp.Or(s.CatchUp, defaultCatchUpEvery) }
func (s *Service) pollTick() time.Duration     { return cmp.Or(s.PollTick, defaultPollTick) }
func (s *Service) pollBudget() float64         { return float64(cmp.Or(s.PollBudget, defaultPollBudget)) }

// pollBucket is the most the checker saves up: five minutes' worth.
func (s *Service) pollBucket() float64 { return max(s.pollBudget()/12, 1) }

// checking reports whether GitHub's webhooks cannot reach this broker (no
// public_url): the checker is then how jobs arrive.
func (s *Service) checking() bool { return s.conf().PublicURL == "" }

func (s *Service) checkState() *checks {
	s.checksOnce.Do(func() { s.checks = &checks{insts: map[int64]*instCheck{}} })
	return s.checks
}

// Afford and Spend make an installation's bucket the Budget of its lists.
func (c *instCheck) Afford() bool { return c.tokens >= 1 }

func (c *instCheck) Spend(counted bool) {
	if counted {
		c.tokens--
	}
}

// runLists is the bucket as the repositories' run lists see it: they stop
// while reserve calls are left, which the jobs of the runs they found are
// listed with. Without it a round whose run lists cost more than the bucket
// never lists a job.
type runLists struct {
	*instCheck
	reserve float64
}

func (b runLists) Afford() bool { return b.tokens >= 1+b.reserve }

// jobsReserve is the share of the bucket kept for listing jobs: an eighth,
// whole calls (none for a bucket of fewer than eight).
func (s *Service) jobsReserve() float64 { return math.Floor(s.pollBucket() / 8) }

// active is the runs the repository's lists last answered, each once.
func (c *repoCheck) active() []ghapp.Run {
	var out []ghapp.Run
	for _, st := range runStatuses {
		for _, r := range c.runs[st].All() {
			if !slices.ContainsFunc(out, func(x ghapp.Run) bool { return x.ID == r.ID }) {
				out = append(out, r)
			}
		}
	}
	return out
}

// failWindow is how long after a round another has surely been recorded:
// two catch-up intervals (a failing list backs off up to one), or two
// rounds' every if longer, a minute (noteCheck writes a like round at most
// once a minute), and the last round's length: on a dead network, where
// every request waits for its timeout, a round of many repositories takes
// longer than all the rest, and its installations' records are that much
// further apart. Failing rounds closer than this are one streak.
func (s *Service) failWindow() time.Duration {
	return 2*max(s.every(), s.catchUpEvery()) + time.Minute + max(time.Duration(s.checkState().span.Load()), 0)
}

// every is how long a round waits for the next: by the mode, stretched so
// the checker stays under pollPerMinute. A repository costs its run lists and,
// when its cached token is due, one mint (they expire together).
func (s *Service) every() time.Duration {
	every := s.catchUpEvery()
	if s.checking() {
		every = s.pollEvery()
	}
	n := s.checkState().repos.Load()
	return max(every, time.Duration(n*int64(len(runStatuses)+1))*time.Minute/pollPerMinute)
}

// due reports whether something last done at last is due again at now, by
// the wall clock: never done, every gone by, or the clock moved back past it.
func due(last, now time.Time, every time.Duration) bool {
	return last.IsZero() || now.Before(last) || now.Sub(last) >= every
}

// checkLoop runs Check when it is due: at the first tick, then every every()
// by the wall clock.
func (s *Service) checkLoop(ctx context.Context) {
	defer s.wg.Done()
	t := time.NewTicker(s.pollTick())
	defer t.Stop()
	c := s.checkState()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if s.conf().GitHub.AppID == 0 {
			continue
		}
		c.mu.Lock()
		last := c.last
		c.mu.Unlock()
		if due(last, s.now(), s.every()) {
			s.Check(ctx)
		}
	}
}

// awakeLoop is the heartbeat: a beat every PollTick, apart from the rounds
// (checkLoop waits for each), so a stuck round does not look like a sleep.
// A step longer than a tick and a minute is a sleep or a clock moved: the
// wall clock schedule then finds the check due (checkLoop).
func (s *Service) awakeLoop(ctx context.Context) {
	defer s.wg.Done()
	t := time.NewTicker(s.pollTick())
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		step, moved := s.checkState().awake.tick(s.now(), s.pollTick()+time.Minute)
		switch {
		case moved && step < 0:
			s.logf("runners: the clock moved back %s", (-step).Round(time.Second))
		case moved:
			s.logf("runners: the clock moved %s in one tick (asleep?)", step.Round(time.Second))
		}
	}
}

// Check asks GitHub once for every linked installation's waiting jobs and
// stores what it finds for the workers (the loop calls it when due).
func (s *Service) Check(ctx context.Context) {
	c := s.checkState()
	c.mu.Lock()
	defer c.mu.Unlock()
	c.last = s.now()
	cl, err := s.client(ctx)
	if err != nil {
		s.checkFailed(c, "the GitHub App's client: "+oneLine(err.Error()))
		return
	}
	insts, err := s.linkedInstallations(ctx)
	if err != nil {
		s.checkFailed(c, "reading the linked installations: "+oneLine(err.Error()))
		return
	}
	c.failed.Store(nil)
	for id := range c.insts {
		if !slices.Contains(insts, id) {
			delete(c.insts, id)
		}
	}
	for _, id := range insts {
		if ctx.Err() != nil {
			return
		}
		ic := c.insts[id]
		if ic == nil {
			ic = s.newInstCheck(ctx, id)
			c.insts[id] = ic
		}
		s.checkInstallation(ctx, cl, id, ic)
	}
	n := 0
	for _, ic := range c.insts {
		n += len(ic.repos)
	}
	c.repos.Store(int64(n))
	if d := s.now().Sub(c.last); d > 0 { // a clock moved back measures nothing
		c.span.Store(int64(d))
	}
}

// checkFailed records why a round could not begin, logged when it changes.
func (s *Service) checkFailed(c *checks, why string) {
	if was := c.failed.Load(); was == nil || was.err != why {
		s.logf("runners: checking GitHub for waiting jobs: %s", why)
	}
	now := s.now()
	since := now
	// A clock moved back starts the streak again: a since later than now
	// would keep it from lasting until the clock caught up.
	if was := c.failed.Load(); was != nil && now.Sub(was.at) <= s.failWindow() && !was.since.After(now) {
		since = was.since
	}
	c.failed.Store(&checkFailure{why, now, since})
}

func (s *Service) linkedInstallations(ctx context.Context) ([]int64, error) {
	rows, err := s.db().QueryContext(ctx, `SELECT installation FROM runners_links ORDER BY installation`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// newInstCheck is the checker's memory of installation inst, its budget as
// runners_checks last kept it (a full bucket the first time).
func (s *Service) newInstCheck(ctx context.Context, inst int64) *instCheck {
	ic := &instCheck{tokens: s.pollBucket(), filled: s.now(), repo: map[string]*repoCheck{}}
	var tokens float64
	var filled int64
	if err := s.db().QueryRowContext(ctx, `SELECT tokens, filled FROM runners_checks WHERE installation = ?`, inst).Scan(&tokens, &filled); err == nil && filled > 0 {
		ic.tokens, ic.filled = min(tokens, s.pollBucket()), time.UnixMilli(filled).UTC()
	}
	return ic
}

// saveBudget keeps installation inst's bucket for the next start.
func (s *Service) saveBudget(ctx context.Context, inst int64, ic *instCheck) {
	if _, err := s.db().ExecContext(ctx, `INSERT INTO runners_checks (installation, tokens, filled) VALUES (?, ?, ?)
		ON CONFLICT(installation) DO UPDATE SET tokens = excluded.tokens, filled = excluded.filled`, inst, ic.tokens, ic.filled.UnixMilli()); err != nil {
		s.logf("runners: keeping the check's budget of installation %d: %v", inst, err)
	}
}

// round is what one installation's round found wrong: failed lists (errs),
// and why it read less than everything (cuts). short: some list was not
// asked at all, so the round did not hear everything even with no error.
type round struct {
	errs, cuts []string
	short      bool
}

func (r *round) cut(why string) {
	if !slices.Contains(r.cuts, why) {
		r.cuts = append(r.cuts, why)
	}
}

// stop ends the round short for why.
func (r *round) stop(why string) {
	r.short = true
	r.cut(why)
}

// fail records a failed list: its back-off, logged the first time in a row.
func (s *Service) fail(r *round, b *backoff, inst int64, why string) {
	b.fails++
	b.next = s.now().Add(min(s.every()<<min(b.fails, 16), s.catchUpEvery()))
	b.err = why
	if b.fails == 1 {
		s.logf("runners: checking installation %d for waiting jobs: %s", inst, why)
	}
	r.errs = append(r.errs, b.why(s.catchUpEvery()))
}

// rateLow reports whether GitHub's budget for inst is low (or it refused for
// a rate limit): its round stops until the reset.
func rateLow(cl *ghapp.Client, inst int64) (string, bool) {
	until, low := cl.RateLow(inst)
	if !low {
		return "", false
	}
	return "GitHub's API budget for this installation is nearly spent: waiting for its reset at " + until.UTC().Format("15:04:05 UTC"), true
}

// rateWhy is why the round stops for GitHub's rate limit (err: the list's
// own answer to it).
func rateWhy(cl *ghapp.Client, inst int64, err error) string {
	why, _ := rateLow(cl, inst)
	return cmp.Or(why, oneLine(err.Error()))
}

// failing is why each list in ic's memory that still fails fails, as a round
// records it: the installation as a whole, then each repository's run lists
// and the jobs of its runs, in a fixed order.
func (ic *instCheck) failing(most time.Duration) []string {
	var out []string
	if ic.back.fails > 0 {
		out = append(out, ic.back.why(most))
	}
	for _, repo := range ic.repos {
		rc := ic.repo[strings.ToLower(repo)]
		if rc == nil {
			continue
		}
		if rc.back.fails > 0 {
			out = append(out, rc.back.why(most))
		}
		for _, id := range slices.Sorted(maps.Keys(rc.seen)) {
			if b := &rc.seen[id].back; b.fails > 0 {
				out = append(out, b.why(most))
			}
		}
	}
	return out
}

// checkInstallation is one round for one installation, recorded in
// runners_checks.
func (s *Service) checkInstallation(ctx context.Context, cl *ghapp.Client, inst int64, ic *instCheck) {
	now := s.now()
	if d := now.Sub(ic.filled); d > 0 { // a clock moved back refills nothing and charges nothing
		ic.tokens = min(s.pollBucket(), ic.tokens+d.Hours()*s.pollBudget())
	}
	ic.filled = now
	before := ic.tokens
	var r round
	most := s.catchUpEvery()
	defer func() {
		if ic.tokens != before { // kept at a shutdown too: the budget outlives the daemon
			s.saveBudget(context.WithoutCancel(ctx), inst, ic)
		}
		if ctx.Err() != nil { // a shutdown is not a round's record
			return
		}
		// A list that still fails stays in the record though the round ended
		// before it was reached.
		for _, why := range ic.failing(most) {
			if !slices.Contains(r.errs, why) {
				r.errs = append(r.errs, why)
			}
		}
		s.noteCheck(ctx, inst, ic, r)
	}()
	// The connected repositories, read afresh: one connected a moment ago is
	// checked in this round, one disconnected is not, and its memory goes.
	repos, err := s.connectedOf(ctx, inst)
	if err != nil {
		if ctx.Err() == nil {
			r.short, r.errs = true, append(r.errs, "reading the connected repositories: "+oneLine(err.Error()))
		}
		return
	}
	ic.repos = repos
	for name := range ic.repo {
		if !slices.ContainsFunc(repos, func(r string) bool { return strings.EqualFold(r, name) }) {
			delete(ic.repo, name)
		}
	}
	if !ic.back.due(now, most) {
		r.short, r.errs = true, append(r.errs, ic.back.why(most))
		return
	}
	if why, low := rateLow(cl, inst); low {
		r.stop(why)
		return
	}
	n := len(repos)
	if n == 0 {
		ic.back = backoff{} // nothing is asked of the installation: no failure of it stays in the record
		return
	}
	// The run lists first, every repository's, from where the last round
	// stopped; then the jobs of the runs that changed. The lists leave a
	// share of the bucket for the jobs (runLists).
	begin := 0
	if i := slices.IndexFunc(repos, func(r string) bool { return strings.ToLower(r) >= ic.start }); i >= 0 {
		begin = i
	}
	type ran struct {
		name string
		repo *repoCheck
	}
	var listed []ran
	lists := runLists{ic, s.jobsReserve()}
	cut := false // the lists stopped for the budget: ic.start is where, and the jobs of what was listed are still listed
lists:
	for i := range n {
		if ctx.Err() != nil {
			return
		}
		at := (begin + i) % n
		name := repos[at]
		rc := ic.repo[strings.ToLower(name)]
		if rc == nil {
			rc = &repoCheck{runs: map[string]ghapp.List[ghapp.Run]{}, seen: map[int64]*runCheck{}}
			ic.repo[strings.ToLower(name)] = rc
		}
		if !rc.back.due(now, most) {
			r.errs = append(r.errs, rc.back.why(most))
			continue
		}
		failed := false
		for _, st := range runStatuses {
			l, err := cl.Runs(ctx, inst, name, st, rc.runs[st], lists)
			rc.runs[st] = l
			if ctx.Err() != nil {
				return
			}
			if errors.Is(err, ghapp.ErrBudget) {
				ic.start = strings.ToLower(name)
				r.stop(budgetSpent)
				cut = true
				break lists
			}
			if errors.Is(err, ghapp.ErrRateLow) {
				ic.start = strings.ToLower(name)
				r.stop(rateWhy(cl, inst, err))
				return
			}
			if ghapp.WholeInstallation(err) {
				r.short = true
				s.fail(&r, &ic.back, inst, "the App's installation: "+oneLine(err.Error()))
				return
			}
			if err == nil || ghapp.Answered(err) {
				ic.back = backoff{} // GitHub answered for the installation: whatever it failed with before is over
			}
			if err != nil {
				s.fail(&r, &rc.back, inst, "listing "+name+"'s "+st+" runs: "+oneLine(err.Error()))
				failed = true
			}
			if why, low := rateLow(cl, inst); low {
				ic.start = strings.ToLower(name)
				r.stop(why)
				return
			}
			if failed {
				break
			}
			if l.More {
				r.cut(fmt.Sprintf("only the newest %d of %s's %s runs are read", len(l.All()), name, st))
			}
		}
		if !failed {
			rc.back = backoff{}
			listed = append(listed, ran{name, rc})
		}
	}
	if !cut {
		ic.start = ""
	}
	for _, x := range listed {
		if s.checkRuns(ctx, cl, inst, ic, x.name, x.repo, &r) {
			if !cut {
				ic.start = strings.ToLower(x.name)
			}
			break
		}
	}
}

// checkRuns lists the jobs of repo's runs that are new or changed (or not
// listed for jobsEvery), and once more those of a run that stopped being
// active while the broker holds a job of it. It answers whether the round
// stops here (the budget, GitHub's rate limit).
func (s *Service) checkRuns(ctx context.Context, cl *ghapp.Client, inst int64, ic *instCheck, repo string, rc *repoCheck, r *round) bool {
	now := s.now()
	most := s.catchUpEvery()
	active := rc.active()
	for id, seen := range rc.seen {
		if slices.ContainsFunc(active, func(r ghapp.Run) bool { return r.ID == id }) {
			continue
		}
		held, err := s.holdsRun(ctx, inst, repo, id)
		if ctx.Err() != nil {
			return true
		}
		if err != nil { // kept: asked again next round
			r.errs = append(r.errs, "reading the broker's jobs of "+repo+"'s run "+itoa(id)+": "+oneLine(err.Error()))
			continue
		}
		if held {
			// Its last jobs ended since: their completed, as a webhook brings it.
			if !seen.back.due(now, most) {
				r.errs = append(r.errs, seen.back.why(most))
				continue
			}
			stop, ok := s.listJobs(ctx, cl, inst, ic, repo, seen.run, seen, r)
			if stop {
				return true
			}
			if !ok {
				continue // kept: asked again once its back-off is over
			}
		}
		delete(rc.seen, id)
	}
	for _, run := range active {
		if ctx.Err() != nil {
			return true
		}
		seen := rc.seen[run.ID]
		if seen == nil {
			seen = &runCheck{}
			rc.seen[run.ID] = seen
		}
		seen.run = run
		if !seen.back.due(now, most) {
			r.errs = append(r.errs, seen.back.why(most))
			continue
		}
		if seen.updated.Equal(run.Updated) && !due(seen.at, now, jobsEvery) {
			continue
		}
		stop, ok := s.listJobs(ctx, cl, inst, ic, repo, run, seen, r)
		if stop {
			return true
		}
		if ok {
			seen.updated = run.Updated
		}
	}
	return false
}

// listJobs lists run's jobs and stores what they say. Only once every store
// succeeded does seen keep the list (its ETags): a list whose storing failed
// is listed and stored again. ok: listed and stored; stop: the round stops
// here.
func (s *Service) listJobs(ctx context.Context, cl *ghapp.Client, inst int64, ic *instCheck, repo string, run ghapp.Run, seen *runCheck, r *round) (stop, ok bool) {
	l, err := cl.RunJobs(ctx, inst, repo, run.ID, seen.jobs, ic)
	switch {
	case ctx.Err() != nil:
		return true, false
	case errors.Is(err, ghapp.ErrBudget):
		r.stop(budgetSpent) // this run's jobs were not listed: the round is short
		return true, false
	case errors.Is(err, ghapp.ErrRateLow):
		r.stop(rateWhy(cl, inst, err))
		return true, false
	}
	where := repo + "'s run " + itoa(run.ID)
	switch {
	case err != nil:
		err = fmt.Errorf("listing the jobs of %s: %s", where, oneLine(err.Error()))
	case l.Changed():
		if e := s.storeJobs(ctx, inst, repo, run, l.All()); e != nil {
			if ctx.Err() != nil {
				return true, false
			}
			err = fmt.Errorf("storing what the jobs of %s say: %s", where, oneLine(e.Error()))
		}
	}
	if err == nil {
		seen.jobs, seen.at, seen.back = l, s.now(), backoff{}
		if l.More {
			r.cut(fmt.Sprintf("only the first %d jobs of %s are read", len(l.All()), where))
		}
	} else {
		s.fail(r, &seen.back, inst, err.Error())
	}
	if why, low := rateLow(cl, inst); low {
		r.stop(why)
		return true, err == nil
	}
	return false, err == nil
}

// storeJobs stores what a run's jobs say as workflow_job deliveries, each
// once: a queued job the broker has no record of, one that started on a
// runner of the broker's (who took it: started), and the completed jobs of a
// run the broker holds a job of (ended). It stops at the first failure.
func (s *Service) storeJobs(ctx context.Context, inst int64, repo string, run ghapp.Run, jobs []ghapp.ListedJob) error {
	held := -1 // whether the broker holds a job of the run: asked once, when needed
	for _, j := range jobs {
		var d delivery
		d.Job.ID, d.Job.RunID, d.Job.Labels, d.Job.RunnerName = j.ID, cmp.Or(j.RunID, run.ID), j.Labels, j.RunnerName
		d.Job.Conclusion, d.Job.RunAttempt = j.Conclusion, j.RunAttempt
		d.Repo.ID, d.Repo.FullName, d.Repo.Private, d.Repo.Visibility = run.RepoID, cmp.Or(run.Repo, repo), run.Private, run.Visibility
		d.Installation.ID = inst
		switch j.Status {
		case "queued":
			known, err := s.known(ctx, j.ID)
			if err != nil {
				return err
			}
			if known {
				continue
			}
			d.Action = "queued"
		case "in_progress":
			if !ourRunner.MatchString(j.RunnerName) {
				continue
			}
			d.Action = "in_progress"
		case "completed":
			if held < 0 {
				h, err := s.holdsRun(ctx, inst, repo, run.ID)
				if err != nil {
					return err
				}
				held = map[bool]int{false: 0, true: 1}[h]
			}
			if held == 0 {
				continue
			}
			d.Action = "completed"
		default:
			continue
		}
		body, err := json.Marshal(d)
		if err != nil {
			return err
		}
		if err := s.store(ctx, "check-"+itoa(j.ID)+"-"+d.Action, "workflow_job", body); err != nil {
			return fmt.Errorf("job %d: %w", j.ID, err)
		}
	}
	return nil
}

// known reports whether the broker has a record of job (a webhook brought
// it, or an earlier check did): serving it again is the record's business.
func (s *Service) known(ctx context.Context, job int64) (bool, error) {
	var one int
	err := s.db().QueryRowContext(ctx, `SELECT 1 FROM runners_jobs WHERE job = ?`, job).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// holdsRun reports whether the broker has a record of a job of run (served,
// waiting, or lost and to be re-run), so its jobs' ends matter.
func (s *Service) holdsRun(ctx context.Context, inst int64, repo string, run int64) (bool, error) {
	var one int
	err := s.db().QueryRowContext(ctx, `SELECT 1 FROM runners_jobs WHERE run = ? AND installation = ? AND repo = ? COLLATE NOCASE
		UNION ALL SELECT 1 FROM runners_lost WHERE run = ? AND installation = ? LIMIT 1`, run, inst, repo, run, inst).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// noteCheck records a round of installation inst. Heard (every list asked
// for answered, none skipped): last_ok is now and an earlier error is
// cleared; else error says which lists failed. cut says why the round read
// less than everything.
//
// A round like the last one recorded less than a minute ago is not written
// again: last_ok is then up to a minute old.
func (s *Service) noteCheck(ctx context.Context, inst int64, ic *instCheck, r round) {
	errs := r.errs
	if len(errs) > 3 {
		errs = append(slices.Clip(errs[:3]), fmt.Sprintf("and %d more", len(r.errs)-3))
	}
	errText, cut := strings.Join(errs, "; "), strings.Join(r.cuts, "; ")
	heard := !r.short && len(r.errs) == 0
	n := noted{at: s.now(), heard: heard, repos: len(ic.repos), errText: errText, cut: cut}
	if was := ic.noted; was.heard == n.heard && was.repos == n.repos && was.errText == n.errText && was.cut == n.cut &&
		n.at.Sub(was.at) < time.Minute && n.at.After(was.at) {
		return
	}
	ic.noted = n
	now := n.at.Unix()
	cutAt := int64(0)
	if cut != "" {
		cutAt = now
	}
	var err error
	if heard {
		_, err = s.db().ExecContext(ctx, `INSERT INTO runners_checks (installation, repos, last_ok, cut, cut_at) VALUES (?, ?, ?, ?, ?)
			ON CONFLICT(installation) DO UPDATE SET repos = excluded.repos, last_ok = excluded.last_ok, error = '', error_at = 0,
			failing_since = 0, cut = excluded.cut, cut_at = excluded.cut_at`, inst, n.repos, now, clip(cut), cutAt)
	} else {
		errAt := int64(0)
		if errText != "" {
			errAt = now
		}
		// A failing round continues the streak of the last one if that one
		// failed within failWindow of this round's start (this round's own
		// length does not split it: Check holds checks.mu, so last is this
		// round's start); else it starts one, as it does when the clock moved
		// back past the streak's start. A round only cut short is not failing.
		gap := s.failWindow() + max(n.at.Sub(s.checkState().last), 0)
		_, err = s.db().ExecContext(ctx, `INSERT INTO runners_checks (installation, repos, error, error_at, failing_since, cut, cut_at) VALUES (?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(installation) DO UPDATE SET repos = excluded.repos, error = excluded.error, error_at = excluded.error_at,
			failing_since = CASE WHEN excluded.error_at = 0 THEN 0
				WHEN runners_checks.failing_since = 0 OR runners_checks.failing_since > excluded.error_at
					OR excluded.error_at - runners_checks.error_at > ? THEN excluded.error_at
				ELSE runners_checks.failing_since END,
			cut = excluded.cut, cut_at = excluded.cut_at`, inst, n.repos, clip(errText), errAt, errAt, clip(cut), cutAt, int64(gap/time.Second))
	}
	if err != nil {
		s.logf("runners: recording the check of installation %d: %v", inst, err)
	}
}

// Checking is how the broker learns of GitHub's waiting jobs, as GET
// /v0/runners/setup shows it ("check").
type Checking struct {
	// Mode is "check" (no public_url: GitHub is asked every EveryS) or
	// "webhook" (GitHub's deliveries serve; a catch-up check every EveryS).
	// Both check on start and on wake from sleep.
	Mode   string `json:"mode"`
	EveryS int    `json:"every_s"`
	// Error is why the last round could not begin at all (no App client, the
	// links unreadable).
	Error        string `json:"error,omitempty"`
	ErrorAt      string `json:"error_at,omitempty"`
	FailingSince string `json:"failing_since,omitempty"` // the first of the rounds in a row that could not begin
	// AwakeSince is when the daemon last started or woke (the heartbeat,
	// which beats apart from the rounds): rounds that stop while it stays
	// awake are stuck, not asleep. "": no beat yet.
	AwakeSince    string                `json:"awake_since,omitempty"`
	Installations []CheckedInstallation `json:"installations"`
}

// CheckedInstallation is the checker's last round of one linked installation.
type CheckedInstallation struct {
	Installation int64  `json:"installation_id"`
	Repos        int    `json:"repos"`             // its connected repositories, the ones checked, as the last round read them
	LastOK       string `json:"last_ok,omitempty"` // RFC 3339: the last round in which every list answered ("": none yet)
	Error        string `json:"error,omitempty"`   // the lists that failed in a round since then (repository, which list)
	ErrorAt      string `json:"error_at,omitempty"`
	FailingSince string `json:"failing_since,omitempty"` // the first of the failing rounds in a row ("": the last round did not fail)
	Cut          string `json:"cut,omitempty"`           // why the last round read less than everything
	CutAt        string `json:"cut_at,omitempty"`
}

// Checked is the checker's state: its mode and, per linked installation,
// its last round.
func (s *Service) Checked(ctx context.Context) (Checking, error) {
	out := Checking{Mode: "webhook", EveryS: int(s.every() / time.Second), Installations: []CheckedInstallation{}}
	if s.checking() {
		out.Mode = "check"
	}
	at := func(t int64) string {
		if t == 0 {
			return ""
		}
		return time.Unix(t, 0).UTC().Format(time.RFC3339)
	}
	if f := s.checkState().failed.Load(); f != nil {
		out.Error, out.ErrorAt, out.FailingSince = f.err, at(f.at.Unix()), at(f.since.Unix())
	}
	if t := s.checkState().awake.Since(); !t.IsZero() {
		out.AwakeSince = at(t.Unix())
	}
	rows, err := s.db().QueryContext(ctx, `SELECT l.installation, coalesce(c.repos, 0), coalesce(c.last_ok, 0), coalesce(c.error, ''),
		coalesce(c.error_at, 0), coalesce(c.failing_since, 0), coalesce(c.cut, ''), coalesce(c.cut_at, 0)
		FROM runners_links l LEFT JOIN runners_checks c ON c.installation = l.installation ORDER BY l.installation`)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var v CheckedInstallation
		var ok, errAt, since, cutAt int64
		if err := rows.Scan(&v.Installation, &v.Repos, &ok, &v.Error, &errAt, &since, &v.Cut, &cutAt); err != nil {
			return out, err
		}
		v.LastOK, v.ErrorAt, v.FailingSince, v.CutAt = at(ok), at(errAt), at(since), at(cutAt)
		out.Installations = append(out.Installations, v)
	}
	return out, rows.Err()
}
