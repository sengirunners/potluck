package runners

import (
	"cmp"
	"encoding/json"
	"slices"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/placement"
)

// whenFree is v as it would be once the reasons that pass by themselves are
// gone: the box is no longer busy or away, and a spent budget has cleared
// with its period. v is not changed.
func whenFree(v computeapi.RunTargetView) computeapi.RunTargetView {
	if v.Describe != nil {
		d := *v.Describe
		d.Available = true
		v.Describe = &d
	}
	v.BudgetSpent = nil
	return v
}

// Eligibility is RunsOn with the distinctions a status screen needs: a box
// that is lent and fits but cannot take a job right now (its owner is using
// the machine, every slot is full) is not a box that was never lent, and a
// box that did not describe itself is not known to be either. The first and
// third send a person to wait or look; only the second, to lend another
// machine.
type Eligibility struct {
	// Labels place a job on the box once it is free; nil when it cannot
	// serve this pool as configured, or is not known to.
	Labels []string
	// Now: a job placed this moment would be accepted (RunsOn's answer).
	Now bool
	// Unknown: the box did not describe itself, so whether it serves is
	// unknown; Why says what happened.
	Unknown bool
	// Why is the reason when Now is false: for a box that only waits, its
	// driver's own busy-or-away text; for any other, the refusal.
	Why string
}

// Eligible reports whether v serves the pool, now or when it is next free.
// v is the project-scoped view from GET /v0/run_targets?describe=1, as for
// RunsOn.
func Eligible(v computeapi.RunTargetView, setup Setup) Eligibility {
	labels, ok, why := RunsOn(v, setup)
	if ok {
		return Eligibility{Labels: labels, Now: true}
	}
	// A box that did not describe itself says so in its own words; RunsOn's
	// reason may be about another binding a size is mapped to.
	if placement.TierOf(v) != placement.Public && v.Kind == computeapi.RunTargetKindBox && (v.Error != "" || v.Describe == nil) {
		return Eligibility{Unknown: true, Why: placement.Refusal(v, placement.Want{}, nil)}
	}
	if free, ok2, _ := RunsOn(whenFree(v), setup); ok2 {
		// Only busy-or-away (or a spent budget) was in the way: say that, not
		// the broker's sentence about every box.
		if r := placement.Refusal(v, placement.Want{}, nil); r != "" {
			why = r
		}
		return Eligibility{Labels: free, Why: why}
	}
	return Eligibility{Why: why}
}

// PoolStatus is what the pool's lent machines add up to.
type PoolStatus struct {
	// NotReady is set when the broker's label or job limit is missing: no
	// machine can be judged until it is set.
	NotReady string `json:"not_ready,omitempty"`
	// RunsOn is the runs-on line for the best eligible machine, even when it
	// is away now: a Mac first, then one that is free, then placement's own
	// order. "" when none is eligible.
	RunsOn string `json:"runs_on,omitempty"`
	// Waiting lists "<name>: <why>" for machines that are eligible but cannot
	// take a job now; Unknown, those that did not describe themselves;
	// Refused, those that cannot serve this pool. Each is sorted by name.
	Waiting []string `json:"waiting,omitempty"`
	Unknown []string `json:"unknown,omitempty"`
	Refused []string `json:"refused,omitempty"`
}

// Status folds the pool's run targets (project-scoped, described) into one
// PoolStatus. With no target listed, everything is empty. A lent machine
// that is asleep or offline is not listed either.
func Status(targets []computeapi.RunTargetView, setup Setup) PoolStatus {
	var st PoolStatus
	if setup.Label == "" || setup.JobTTLS <= 0 {
		st.NotReady = "pool label and effective job_ttl_s must be set"
		return st
	}
	type candidate struct {
		name   string
		labels []string
		now    bool
	}
	var cands []candidate
	var fits []placement.Fit
	for _, t := range targets {
		e := Eligible(t, setup)
		switch {
		case e.Unknown:
			st.Unknown = append(st.Unknown, t.Name+": "+e.Why)
		case e.Labels == nil:
			st.Refused = append(st.Refused, t.Name+": "+e.Why)
		default:
			cands = append(cands, candidate{t.Name, e.Labels, e.Now})
			fits = append(fits, placement.FitOf(whenFree(t)))
			if !e.Now {
				st.Waiting = append(st.Waiting, t.Name+": "+e.Why)
			}
		}
	}
	slices.Sort(st.Waiting)
	slices.Sort(st.Unknown)
	slices.Sort(st.Refused)
	if len(cands) == 0 {
		return st
	}
	order := placement.Rank(fits)
	rank := func(name string) int { return slices.Index(order, name) }
	// Labels are the pool request followed by its OS label.
	best := slices.MinFunc(cands, func(a, b candidate) int {
		isMac := func(c candidate) int {
			if c.labels[1] == "macos" {
				return 0
			}
			return 1
		}
		free := func(c candidate) int {
			if c.now {
				return 0
			}
			return 1
		}
		return cmp.Or(cmp.Compare(isMac(a), isMac(b)), cmp.Compare(free(a), free(b)), cmp.Compare(rank(a.name), rank(b.name)))
	})
	encoded, _ := json.Marshal([]string{"self-hosted", best.labels[1], best.labels[0]})
	st.RunsOn = "runs-on: " + string(encoded)
	return st
}
