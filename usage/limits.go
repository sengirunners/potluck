package usage

import (
	"cmp"
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/project"
	"github.com/sengirunners/potluck/runtarget/rt"
)

// Per-area sandbox minute limits (zm-eakh.12.5.11): [[box_minutes]] in
// project.toml, a section this module owns. A plain fair-share quota on the
// team pool, not billing.
//
//	[[box_minutes]]
//	area = "team1"
//	minutes = 600
//	period = "month"
const limitsSection = "box_minutes"

func init() { project.RegisterSection(limitsSection, limitsSec{}) }

type limitsSec struct{}

func (limitsSec) Parse(where string, raw any) (any, error) {
	ts, ok := raw.([]map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s: %s must be [[%s]] tables", where, limitsSection, limitsSection)
	}
	var out []computeapi.AreaMinuteLimit
	for i, t := range ts {
		r := project.NewTOMLReader(fmt.Sprintf("%s [[%s]] #%d", where, limitsSection, i+1), t)
		l := computeapi.AreaMinuteLimit{Area: r.Str("area"), Minutes: int(r.Int("minutes")), Period: r.Str("period")}
		if err := r.Done(); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, nil
}

// Validate checks each limit's shape. It does not ask that the area exist:
// a limit on no area limits nothing, and PUT with 0 minutes clears it.
func (limitsSec) Validate(v any, _ project.File) error {
	ls, _ := v.([]computeapi.AreaMinuteLimit)
	seen := map[string]bool{}
	for _, l := range ls {
		if err := checkLimit(l); err != nil {
			return err
		}
		if seen[l.Area] {
			return api.Errorf(api.CodeConflict, "area %s has two sandbox minute limits", l.Area)
		}
		seen[l.Area] = true
	}
	return nil
}

func checkLimit(l computeapi.AreaMinuteLimit) error {
	if !api.ValidSlug(l.Area) {
		return api.Errorf(api.CodeBadRequest, "a sandbox minute limit names an area (got %q)", l.Area)
	}
	if l.Minutes <= 0 || !rt.ValidPeriod(l.Period) {
		return api.Errorf(api.CodeBadRequest, "area %s's sandbox minute limit needs a positive number of minutes and a period of day, week or month", l.Area)
	}
	return nil
}

func (limitsSec) Encode(v any) string {
	ls, _ := v.([]computeapi.AreaMinuteLimit)
	var b strings.Builder
	for _, l := range ls {
		fmt.Fprintf(&b, "\n[[%s]]\narea = %s\nminutes = %d\nperiod = %s\n", limitsSection,
			project.TOMLString(l.Area), l.Minutes, project.TOMLString(l.Period))
	}
	return b.String()
}

// limits are project id's per-area limits (a copy).
func limits(r *project.Registry, id string) []computeapi.AreaMinuteLimit {
	ls, _ := r.Section(id, limitsSection).([]computeapi.AreaMinuteLimit)
	return slices.Clone(ls)
}

// allowBox is run targets' OnBoxAcquire check: a new box in area is refused
// once the area's billed minutes this period, its live boxes counted as if
// they ended now, reach its limit. Minutes that cannot be read refuse too.
// An ended box counts in the period it ended in, as the ledger records it; a
// live one by its lease from acquire to now, an upper bound on what it will
// be billed (a stopped persistent box's stopped time included).
func (s *Service) allowBox(ctx context.Context, projectID, area string, liveOf func() ([]computeapi.RunTargetHandle, error)) error {
	if area == "" {
		return nil
	}
	ls := limits(s.d.Projects, projectID)
	i := slices.IndexFunc(ls, func(l computeapi.AreaMinuteLimit) bool { return l.Area == area })
	if i < 0 {
		return nil
	}
	l := ls[i]
	live, err := liveOf() // first: a box that ends after this is in the reads below
	if err != nil {
		return err
	}
	now := s.now()
	used, err := s.areaMinutes(ctx, projectID, area, rt.PeriodStart(l.Period, now), now, live)
	if err != nil {
		return api.Errorf(api.CodeUnavailable, "area %s's sandbox minutes could not be read, so no new sandbox there until they can: %v", area, err)
	}
	if used >= l.Minutes {
		return api.Errorf(api.CodeForbidden, "area %s has used %d of its %d sandbox minutes this %s, so no new sandbox there until the %s ends",
			area, used, l.Minutes, l.Period, l.Period).
			WithHint(fmt.Sprintf("a project owner or admin can raise the limit: potluck usage limit %s <minutes> --per %s", area, l.Period))
	}
	return nil
}

// maxEndPages is how many pages of 1000 ended boxes areaMinutes reads from
// run targets for one area and period; an area with more than that in a
// period reads as unreadable (so refuses) rather than as under its limit.
var maxEndPages = 50

// areaMinutes is area's billed minutes for boxes that ended at or after
// start, plus live boxes' minutes so far. It reads, in this order, run
// targets' own record of the area's boxes that ended since start (a row the
// ledger has not written, however long ago it ended, still counts: the end
// hook runs after the end, and a failed write waits for Reconcile) and then
// (in one read) the ledger's sum and the handles it holds, so a row written
// in between is counted once.
func (s *Service) areaMinutes(ctx context.Context, projectID, area string, start, now time.Time, live []computeapi.RunTargetHandle) (int, error) {
	svc := rt.From(s.d)
	if svc == nil {
		return 0, fmt.Errorf("run targets are not wired")
	}
	var recent []computeapi.RunTargetHandle
	complete, err := endedPages(ctx, svc, projectID, rt.EndedOnly{Area: area, Kind: computeapi.RunTargetKindBox}, start, maxEndPages, func(hs []computeapi.RunTargetHandle) error {
		recent = append(recent, hs...)
		return nil
	})
	if err != nil {
		return 0, err
	}
	if !complete {
		return 0, fmt.Errorf("too many sandboxes ended since %s to read", start.UTC().Format(time.RFC3339))
	}
	db, err := s.d.Store.Project(s.d.System().Scope, authz.Read, projectID)
	if err != nil {
		return 0, err
	}
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	used, err := billedMinutes(ctx, tx, `area = ? AND ended >= ?`, area, start.UTC().Format(wireTime))
	if err != nil {
		return 0, err
	}
	have, err := handlesSince(ctx, tx, area, start)
	if err != nil {
		return 0, err
	}
	for _, h := range recent {
		if h.Kind == computeapi.RunTargetKindBox && h.Area == area && !have[h.Handle] {
			have[h.Handle] = true
			if u := Compute(h); u.Billed && u.Ended >= start.UTC().Format(wireTime) {
				used += u.BillableMinutes
			}
		}
	}
	for _, h := range live {
		if h.Area == area && !have[h.Handle] {
			used += soFar(h, now)
		}
	}
	return used, nil
}

// soFar is a live box's minutes as if it were released at now, by the same
// lease rule as Compute (leaseSpan): from acquire (or from when it was asked
// for, when a timed-out acquire left no answer time) to now, never past
// expires_at. The driver's figure as of its last status call may be long
// ago, so it decides only what the box ran before it was last stopped: a
// stopped box counts its figure, a box started again its figure as of that
// start plus its time since the start. Without a time figure the whole lease
// counts; a box started again with no figure kept at its start counts its
// latest figure plus time since the start, which may count the current run
// twice but never too little.
func soFar(h computeapi.RunTargetHandle, now time.Time) int {
	upTo := func(from string) float64 {
		d, _ := leaseSpan(from, now, h.ExpiresAt)
		return max(0, d.Minutes())
	}
	lease := upTo(cmp.Or(h.Acquired, h.Created))
	minutes := lease
	if h.Spend != nil {
		if ran, ok := ranMinutes(h.Spend.Unit, h.Spend.Amount); ok {
			switch {
			case h.State == computeapi.StateStopped:
				minutes = min(lease, ran)
			case h.Started != "":
				if b := h.StartedSpend; b != nil { // the same unit as Spend (one column)
					ran, _ = ranMinutes(h.Spend.Unit, b.Amount)
				}
				minutes = min(lease, ran+upTo(h.Started))
			}
		}
	}
	return wholeMinutes(minutes)
}

// limitsList answers GET /v0/usage/limits.
func (s *Service) limitsList(w http.ResponseWriter, r *http.Request) {
	t, err := s.d.WriteTarget(r.Context(), "", authz.Read)
	if err != nil {
		api.WriteError(w, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, s.limitsView(core.SubjectOf(r.Context()), t.Project))
}

// limitsView is p's limits on the areas sub reads: an area it cannot read
// is not named to it, as everywhere else.
func (s *Service) limitsView(sub *authz.Subject, p api.ProjectRef) computeapi.AreaMinuteLimits {
	ls := slices.DeleteFunc(limits(s.d.Projects, p.ID), func(l computeapi.AreaMinuteLimit) bool {
		return !sub.Can(authz.Read, authz.Object{Project: p.ID, Area: l.Area})
	})
	if ls == nil {
		ls = []computeapi.AreaMinuteLimit{}
	}
	return computeapi.AreaMinuteLimits{Project: p.Slug, Limits: ls}
}

// limitSet answers PUT /v0/usage/limits/{area}. Only a whole-project owner
// or admin sets a limit: an area's own admin must not raise its own.
func (s *Service) limitSet(w http.ResponseWriter, r *http.Request) {
	out, err := s.setLimit(r)
	if err != nil {
		api.WriteError(w, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, out)
}

func (s *Service) setLimit(r *http.Request) (computeapi.AreaMinuteLimits, error) {
	ctx := r.Context()
	var req computeapi.SetAreaMinuteLimitRequest
	if err := api.DecodeJSON(r, &req); err != nil {
		return computeapi.AreaMinuteLimits{}, err
	}
	t, err := s.d.WriteTarget(ctx, "", authz.Read)
	if err != nil {
		return computeapi.AreaMinuteLimits{}, err
	}
	// The caller already reads the project (above), so saying why leaks nothing.
	if !core.SubjectOf(ctx).Can(authz.Admin, authz.Object{Project: t.Project.ID}) {
		return computeapi.AreaMinuteLimits{}, api.Errorf(api.CodeForbidden,
			"only an owner or admin of the whole project %s sets an area's sandbox minute limit", t.Project.Slug)
	}
	area := r.PathValue("area")
	if req.Minutes < 0 {
		return computeapi.AreaMinuteLimits{}, api.Errorf(api.CodeBadRequest, "minutes must not be negative (0 removes the limit)")
	}
	if req.Period != "" && !rt.ValidPeriod(req.Period) {
		return computeapi.AreaMinuteLimits{}, api.Errorf(api.CodeBadRequest, "period must be day, week or month, not %q", req.Period)
	}
	if req.Minutes > 0 && !s.d.Projects.HasArea(t.Project.ID, area) { // 0 may clear a limit left on no area
		return computeapi.AreaMinuteLimits{}, api.NotFound("area", area)
	}
	err = s.editLimits(ctx, t.Project.ID, func(ls []computeapi.AreaMinuteLimit) ([]computeapi.AreaMinuteLimit, error) {
		// Without a period the area keeps the one it has, else a month.
		period := cmp.Or(req.Period, "month")
		if req.Period == "" {
			if i := slices.IndexFunc(ls, func(o computeapi.AreaMinuteLimit) bool { return o.Area == area }); i >= 0 {
				period = ls[i].Period
			}
		}
		ls = slices.DeleteFunc(ls, func(o computeapi.AreaMinuteLimit) bool { return o.Area == area })
		if req.Minutes > 0 {
			l := computeapi.AreaMinuteLimit{Area: area, Minutes: req.Minutes, Period: period}
			if err := checkLimit(l); err != nil {
				return nil, err
			}
			ls = append(ls, l)
		}
		return ls, nil
	})
	if err != nil {
		return computeapi.AreaMinuteLimits{}, err
	}
	return s.limitsView(core.SubjectOf(ctx), t.Project), nil
}

// editLimits rewrites project id's limits as edit says, dropping any on an
// area the project no longer defines in the same write: such a limit would
// come back if an area of that name were made again.
func (s *Service) editLimits(ctx context.Context, id string, edit func([]computeapi.AreaMinuteLimit) ([]computeapi.AreaMinuteLimit, error)) error {
	// Read before SetSection, whose fn must not call the registry. An area
	// removed and made again between this read and the write keeps no limit;
	// the limit then reads as absent, which an owner sees and sets again.
	gone := map[string]bool{}
	for _, l := range limits(s.d.Projects, id) {
		gone[l.Area] = !s.d.Projects.HasArea(id, l.Area)
	}
	_, err := s.d.Projects.SetSection(ctx, id, limitsSection, func(old any) (any, error) {
		ls, _ := old.([]computeapi.AreaMinuteLimit)
		ls = slices.DeleteFunc(slices.Clone(ls), func(l computeapi.AreaMinuteLimit) bool { return gone[l.Area] })
		ls, err := edit(ls)
		if err != nil || len(ls) == 0 {
			return nil, err
		}
		return ls, nil
	})
	return err
}

// dropGoneLimits removes project id's limits on areas it no longer defines
// (Start's passes run it, so a limit outlives its area by at most a pass).
func (s *Service) dropGoneLimits(ctx context.Context, id string) error {
	if !slices.ContainsFunc(limits(s.d.Projects, id), func(l computeapi.AreaMinuteLimit) bool { return !s.d.Projects.HasArea(id, l.Area) }) {
		return nil
	}
	return s.editLimits(ctx, id, func(ls []computeapi.AreaMinuteLimit) ([]computeapi.AreaMinuteLimit, error) { return ls, nil })
}
