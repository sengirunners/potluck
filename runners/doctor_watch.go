package runners

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/sengirunners/potluck/jobrule"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/core"
)

// The self-check: every five minutes the broker runs checks 3, 4, 12, 13,
// 15 and 16 on the connected repositories (runners_repos) and tells the
// pool's owners once when a check starts failing. runners_doctor holds each
// check's last result and whether its failure was told, by what it is about:
// a repository (3, 16), an installation (4, 15) or the pool (12, 13). Check
// 16 fails per repository but its cause is the installation's webhook or
// link: while one repository's failure of it was told, another repository
// of that installation starting to fail it is recorded as told.
// Check 15 is per installation, but one cause (the rounds could not begin
// at all) fails it alike for each: a failure whose message another
// installation's told failure has is recorded as told.

// doctorEvery is how often the self-check runs; doctorKeep is how long a
// result nobody checked again (a repository disconnected) is kept.
const (
	doctorEvery = 5 * time.Minute
	doctorKeep  = 7 * 24 * time.Hour
)

// doctorState is where the next self-check starts in the connected list: it
// moves past the repositories each run checked, so a list longer than one
// run's gh budget is covered in turn.
type doctorState struct {
	mu     sync.Mutex
	offset int
}

// selfChecks are the checks the self-check tells about.
var selfChecks = []int{3, 4, 12, 13, 15, 16}

func (s *Service) doctorWatch(ctx context.Context) {
	defer s.wg.Done()
	ticker := time.NewTicker(doctorEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.DoctorSelfCheck(ctx)
		}
	}
}

// DoctorSelfCheck runs the self-check once. Embedders may call it after
// connection changes; calls are serialized.
func (s *Service) DoctorSelfCheck(ctx context.Context) {
	s.selfCheck.mu.Lock()
	defer s.selfCheck.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 4*time.Minute)
	defer cancel()
	ctx = doctorContext(ctx)
	p, err := s.pool()
	if err != nil {
		return
	}
	repos, err := s.doctorConnected(ctx)
	if err != nil {
		s.logf("runners: doctor: connected repositories: %v", err)
		return
	}
	if _, err := s.db().ExecContext(ctx, `DELETE FROM runners_doctor WHERE checked < ?`, s.now().Add(-doctorKeep).Unix()); err != nil {
		s.logf("runners: doctor state: %v", err)
	}
	if len(repos) == 0 {
		return
	}
	start := s.selfCheck.offset % len(repos)
	order := append(slices.Clone(repos[start:]), repos[:start]...)
	checked := 0
	for i, repo := range order {
		if doctorBudgetSpent(ctx) || ctx.Err() != nil {
			s.logf("runners: doctor %v; skipped repositories: %s", errDoctorBudget, strings.Join(order[i:], ", "))
			break
		}
		report := s.doctor(ctx, repo, false)
		s.doctorRecord(ctx, p, report)
		if doctorRefused(ctx) && i > 0 {
			break // its reads ran out: the next run starts with it
		}
		checked++
	}
	s.selfCheck.offset = start + max(1, checked)
}

// doctorConnected is the connected repositories of linked installations.
func (s *Service) doctorConnected(ctx context.Context) ([]string, error) {
	return s.doctorRepos(ctx, `SELECT r.repo FROM runners_repos r JOIN runners_links l ON l.installation = r.installation ORDER BY r.repo`)
}

// doctorInventory is what the doctor checks when no repository is named:
// the connected repositories, then the others in the job log (whose jobs
// came here and were refused, check 5 says why).
func (s *Service) doctorInventory(ctx context.Context) ([]string, error) {
	connected, err := s.doctorConnected(ctx)
	if err != nil {
		return nil, err
	}
	logged, err := s.doctorRepos(ctx, `SELECT DISTINCT repo FROM runners_jobs ORDER BY repo`)
	if err != nil {
		return nil, err
	}
	out := connected
	for _, r := range logged {
		if !slices.ContainsFunc(out, func(o string) bool { return strings.EqualFold(o, r) }) {
			out = append(out, r)
		}
	}
	return out, nil
}

func (s *Service) doctorRepos(ctx context.Context, query string) ([]string, error) {
	rows, err := s.db().QueryContext(ctx, query)
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
		if jobrule.ValidRepo(repo) {
			out = append(out, repo)
		}
	}
	return out, rows.Err()
}

// doctorRecord stores report's self-checks and tells the owners of each
// that starts failing. Missing evidence (a warn) neither tells nor resets.
func (s *Service) doctorRecord(ctx context.Context, p api.ProjectRef, report DoctorReport) {
	for _, c := range report.Checks {
		if !slices.Contains(selfChecks, c.Number) {
			continue
		}
		key, about := report.Repo, report.Repo
		switch {
		case c.Number == 12 || c.Number == 13:
			key, about = "pool", "the pool's machines"
		case (c.Number == 4 || c.Number == 15) && report.Installation != 0:
			key, about = "installation:"+itoa(report.Installation), "GitHub installation "+itoa(report.Installation)
		}
		notified := false
		err := s.db().QueryRowContext(ctx, `SELECT notified FROM runners_doctor WHERE repo = ? AND check_number = ?`, key, c.Number).Scan(&notified)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			s.logf("runners: doctor state: %v", err)
			continue
		}
		switch c.Status {
		case "fail":
			if !notified && c.Number == 16 && report.Installation != 0 {
				// A told failure checked within the hour: a repository
				// disconnected since does not silence the others for a week.
				var told int
				err = s.db().QueryRowContext(ctx, `SELECT count(*) FROM runners_doctor WHERE check_number = 16 AND installation = ? AND status = 'fail' AND notified = 1 AND checked >= ?`,
					report.Installation, report.CheckedAt.Add(-time.Hour).Unix()).Scan(&told)
				if err != nil {
					s.logf("runners: doctor state: %v", err)
					continue
				}
				notified = told > 0
			}
			if !notified && c.Number == 15 {
				// A failure that reads the same as another installation's
				// (the broker's rounds could not begin at all; one outage's
				// identical errors) has one cause: told for one within the
				// hour, told for all. The owners told are the same pool's.
				var told int
				err = s.db().QueryRowContext(ctx, `SELECT count(*) FROM runners_doctor WHERE check_number = 15 AND repo != ? AND message = ? AND status = 'fail' AND notified = 1 AND checked >= ?`,
					key, c.Message, report.CheckedAt.Add(-time.Hour).Unix()).Scan(&told)
				if err != nil {
					s.logf("runners: doctor state: %v", err)
					continue
				}
				notified = told > 0
			}
			if !notified {
				err = s.d.Notify(ctx, core.Notice{Project: p, To: "owners", Body: fmt.Sprintf("%s: check %d: %s\nNext: %s", about, c.Number, c.Message, c.Fix)})
				if err != nil && !errors.Is(err, core.ErrNoNotifier) {
					s.logf("runners: doctor notice: %v", err)
					continue
				}
				notified = true
			}
		case "pass":
			notified = false
		}
		_, err = s.db().ExecContext(ctx, `INSERT INTO runners_doctor(repo,check_number,status,notified,message,fix,checked,installation) VALUES(?,?,?,?,?,?,?,?)
			ON CONFLICT(repo,check_number) DO UPDATE SET status=excluded.status,notified=excluded.notified,message=excluded.message,fix=excluded.fix,checked=excluded.checked,installation=excluded.installation`,
			key, c.Number, c.Status, notified, c.Message, c.Fix, report.CheckedAt.Unix(), report.Installation)
		if err != nil {
			s.logf("runners: doctor state: %v", err)
		}
	}
}
