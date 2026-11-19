package cli

import (
	"fmt"
	"time"
)

// jobTiming measures sequential client phases. Acquisition details overlap
// acquire and must not be added to the phases' sum.
type jobTiming struct {
	started, since time.Time
	current        string
	elapsed        map[string]time.Duration
	setup          *float64
	baseFill       time.Duration
	deps           bool
}

type jobAcquireDetails struct {
	BaseFill float64 `json:"base_fill,omitempty"`
	// DepsAcquire includes placement and box startup as well as deps work;
	// the daemon does not expose a separate build/wait duration.
	DepsAcquire float64 `json:"deps_acquire,omitempty"`
}

func newJobTiming(start time.Time) *jobTiming {
	if start.IsZero() {
		start = time.Now()
	}
	return &jobTiming{started: start, since: start, current: "prepare", elapsed: map[string]time.Duration{
		"prepare": 0, "acquire": 0, "push": 0, "start": 0, "run": 0, "collect": 0, "cleanup": 0,
	}}
}
func (t *jobTiming) enter(phase string) {
	if t == nil {
		return
	}
	now := time.Now()
	t.elapsed[t.current] += now.Sub(t.since)
	t.current, t.since = phase, now
}
func (t *jobTiming) spawned() {
	seconds := time.Since(t.started).Seconds()
	t.setup = &seconds
	t.enter("run")
}
func (t *jobTiming) fill(d time.Duration) { t.baseFill += d }
func (t *jobTiming) result(r *jobResult, err error) {
	t.enter(t.current)
	r.Seconds = time.Since(t.started).Seconds()
	r.SetupS = t.setup
	r.Phases = make(map[string]float64, len(t.elapsed))
	for phase, d := range t.elapsed {
		r.Phases[phase] = d.Seconds()
	}
	if t.baseFill > 0 || t.deps {
		detail := &jobAcquireDetails{BaseFill: t.baseFill.Seconds()}
		if t.deps {
			detail.DepsAcquire = max(0, (t.elapsed["acquire"] - t.baseFill).Seconds())
		}
		r.AcquireDetails = detail
	}
	r.ErrorCode = failureCode(err)
	if err != nil {
		r.Error = err.Error()
	}
}
func (o boxJobOptions) finish(r *jobResult, err error) {
	o.timing.result(r, err)
	if o.errw == nil {
		return
	}
	setup := "not started"
	if r.SetupS != nil {
		setup = fmt.Sprintf("%.3fs", *r.SetupS)
	}
	fmt.Fprintf(o.errw, "%s: setup %s; acquire %.3fs, push %.3fs, start %.3fs, run %.3fs, collect %.3fs; total %.3fs\n", o.p.name(), setup, r.Phases["acquire"], r.Phases["push"], r.Phases["start"], r.Phases["run"], r.Phases["collect"], r.Seconds)
}
