package runners

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/placement"
	"github.com/sengirunners/potluck/protocol"
	"github.com/sengirunners/potluck/runtarget/rt"
)

// doctorPool is the pool's machines as one doctor report reads them: one
// listing, judged by placement's own rule (bindingFits), never a copy of it.
type doctorPool struct {
	views []computeapi.RunTargetView
	sizes map[string]string
	load  map[string]int
	cool  map[string]string
	// served is each binding that took a job before, with what that job
	// asked: evidence that a host missing from the listing now (asleep, its
	// advert lapsed) can serve such a job.
	served map[string][]want
}

// doctorPool reads the pool of l once per doctor run (doctorContext): every
// repository of one command or self-check is judged on one listing.
func (s *Service) doctorPool(ctx context.Context, sub *authz.Subject, svc rt.Service, l link) (*doctorPool, error) {
	run, cached := ctx.Value(doctorRunKey{}).(*doctorRun)
	if cached && run.pools[l.project] != nil {
		return run.pools[l.project], nil
	}
	p, err := s.readDoctorPool(ctx, sub, svc, l)
	if err == nil && cached {
		if run.pools == nil {
			run.pools = map[string]*doctorPool{}
		}
		run.pools[l.project] = p
	}
	return p, err
}

func (s *Service) readDoctorPool(ctx context.Context, sub *authz.Subject, svc rt.Service, l link) (*doctorPool, error) {
	list, err := svc.List(ctx, sub, l.project, true)
	if err != nil {
		return nil, err
	}
	p := &doctorPool{views: list.Targets, sizes: s.conf().Pool.Sizes, load: s.load(ctx), cool: s.cooling(), served: map[string][]want{}}
	rows, err := s.db().QueryContext(ctx, `SELECT DISTINCT target, labels FROM runners_jobs WHERE target != ''`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var target, raw string
		if err := rows.Scan(&target, &raw); err != nil {
			return nil, err
		}
		var labels []string
		if json.Unmarshal([]byte(raw), &labels) != nil {
			continue
		}
		if w, ours, why := s.labels(labels); ours && why == "" {
			p.served[target] = append(p.served[target], w)
		}
	}
	return p, rows.Err()
}

func (p *doctorPool) listed(name string) bool {
	return slices.ContainsFunc(p.views, func(v computeapi.RunTargetView) bool { return v.Name == name })
}

// fit is placement's answer for w and a job lease of ttl seconds: the
// bindings that would take it now, or why none would and whether that is
// permanent. Busy and cooling hosts, a host whose describe failed, and a
// binding missing from the listing that served such a job before (its host
// asleep) are not permanent: the job waits for them. want{} asks for any
// job (check 12 for the pool): any binding that served a job counts.
func (p *doctorPool) fit(w want, ttl int) (names []string, why string, permanent bool) {
	names, why = bindingFits(p.views, p.sizes, w, p.load, p.cool, ttl)
	if len(names) > 0 {
		return names, "", false
	}
	if b := p.sizes[w.size]; w.size != "" && b != "" && !p.listed(b) {
		return nil, why, false
	}
	free := make([]computeapi.RunTargetView, len(p.views))
	for i, v := range p.views {
		free[i] = whenFree(v)
	}
	if n, _ := bindingFits(free, p.sizes, w, nil, nil, ttl); len(n) > 0 {
		return nil, why, false
	}
	for _, v := range p.views {
		if v.Kind == computeapi.RunTargetKindBox && placement.TierOf(v) != placement.Public && (v.Error != "" || v.Describe == nil) {
			return nil, "unavailable: " + v.Name + " did not describe itself, so whether it serves this is unknown: " + why, false
		}
	}
	var away []string
	for target, wants := range p.served {
		if p.listed(target) {
			continue
		}
		if slices.ContainsFunc(wants, func(s want) bool {
			return w == (want{}) || s.os == w.os && s.image == w.image && (w.size == "" || s.size == w.size)
		}) {
			away = append(away, target)
		}
	}
	if len(away) > 0 {
		sort.Strings(away)
		return nil, "unavailable: " + strings.Join(away, ", ") + " served such a job before and is not listed now (its host asleep or offline?): " + why, false
	}
	return nil, why, true
}

// runners is check 13: what each box binding's newest GitHub runner
// refresh found (describe's runners, protocol.RunnerStatus).
func (p *doctorPool) runners(out *DoctorReport, cmd func(string) string) {
	var failed, ok []string
	for _, v := range p.views {
		d := v.Describe
		if v.Kind != computeapi.RunTargetKindBox || placement.TierOf(v) == placement.Public || d == nil || !slices.Contains(d.Capabilities, protocol.CapGitHubJIT) {
			continue
		}
		for _, r := range d.Runners {
			switch {
			case !r.Valid():
			case r.Refresh == protocol.RunnerFailed:
				failed = append(failed, fmt.Sprintf("%s (image %s): %s", v.Name, placement.Quote(r.Image), r.Why))
			default:
				ok = append(ok, fmt.Sprintf("%s %s", v.Name, placement.Quote(r.Version)))
			}
		}
	}
	switch {
	case len(failed) > 0:
		out.set(13, "fail", "A box got no working GitHub runner: its refresh failed on "+strings.Join(failed, "; ")+".",
			"read that host's daemon log for the refresh; its next box tries again")
	case len(ok) > 0:
		out.set(13, "pass", "GitHub runners are current: "+strings.Join(ok, ", ")+".", "")
	default:
		out.set(13, "warn", "unavailable: no box of the pool has reported its GitHub runner yet (a box reports once it starts one)", "run a job, then "+cmd("ci doctor")+" again")
	}
}
