package menubar

import (
	"cmp"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/sengirunners/potluck/vmhost"
)

// The menu's job window (docs/menubar.md "Jobs this Mac ran"): every job
// this machine's sizes ran, the counts (running now, started in the last 12
// hours, per day) and an electricity estimate that says how it is worked
// out. `potluck host jobs --json` answers a JobsView; the app draws it.

// WattsPerCore is the estimate's draw of one busy core: about what one
// Apple silicon performance core draws flat out (3 to 6 W).
const WattsPerCore = 5.0

// jobDays is how many days, today among them, the per-day counts cover.
const jobDays = 14

// recent is the span of the "last 12 hours" count.
const recent = 12 * time.Hour

// JobFacts are what the job window is made from.
type JobFacts struct {
	// Jobs are every job of every state directory this machine's sizes
	// use, in any order.
	Jobs []vmhost.Job
	// Pools names the pool each relayed job's handle came from. A handle
	// not in it was started on this machine if its job ended at or after
	// PoolsFrom (the host's ledger forgets older ones), or is still up;
	// before it, or with PoolsFrom zero (no ledger read), it is not known.
	Pools     map[string]string
	PoolsFrom time.Time
	// Unread says which state directories could not be read, and why.
	Unread []string
	Loc    *time.Location
	Now    time.Time
}

// JobsView is the job window.
type JobsView struct {
	Title string `json:"title"`
	// Summary is the counts in one line.
	Summary string `json:"summary"`
	Running int    `json:"running"`
	Last12h int    `json:"last_12h"`
	Total   int    `json:"total"`
	// Days are the last jobDays local days, today first.
	Days   []JobDay  `json:"days"`
	Energy Energy    `json:"energy"`
	Jobs   []JobLine `json:"jobs"`
	// Note says what could not be read ("" when all could).
	Note string `json:"note,omitempty"`
}

// JobDay is one local day's jobs (by when they started).
type JobDay struct {
	Date  string  `json:"date"` // 2006-01-02
	Label string  `json:"label"`
	Jobs  int     `json:"jobs"`
	Wh    float64 `json:"wh"`
	// Energy is Wh as the window shows it ("" for a day with no jobs).
	Energy string `json:"energy"`
}

// Energy is the electricity estimate.
type Energy struct {
	WattsPerCore float64 `json:"watts_per_core"`
	RunningWh    float64 `json:"running_wh"`
	Last12hWh    float64 `json:"last_12h_wh"`
	TotalWh      float64 `json:"total_wh"`
	// Unknown counts the jobs whose cores are not known, left out.
	Unknown int `json:"unknown,omitempty"`
	// Line is the estimate in one line; How says how it is worked out.
	Line string `json:"line"`
	How  string `json:"how"`
}

// JobLine is one job as the window lists it, newest first.
type JobLine struct {
	Handle  string `json:"handle"`
	Start   string `json:"start"` // RFC 3339
	When    string `json:"when"`
	Took    string `json:"took"`
	Size    string `json:"size"`
	Pool    string `json:"pool"`
	Ended   string `json:"ended"`
	Energy  string `json:"energy"`
	Running bool   `json:"running"`
}

// jobWh is the estimate for one job (ok false: its cores are not known).
func jobWh(j vmhost.Job) (float64, bool) {
	if j.CPU <= 0 {
		return 0, false
	}
	return float64(j.CPU) * j.UpS / 3600 * WattsPerCore, true
}

// BuildJobs turns the facts into the job window.
func BuildJobs(f JobFacts) JobsView {
	loc := cmp.Or(f.Loc, time.UTC)
	nowT := f.Now
	if nowT.IsZero() {
		nowT = now()
	}
	v := JobsView{Title: "Jobs this Mac ran", Jobs: []JobLine{}, Energy: Energy{WattsPerCore: WattsPerCore}}
	today := midnight(nowT.In(loc))
	days := map[string]*JobDay{}
	for i := range jobDays {
		d := today.AddDate(0, 0, -i)
		v.Days = append(v.Days, JobDay{Date: d.Format("2006-01-02"), Label: dayLabel(d, today)})
	}
	for i := range v.Days {
		days[v.Days[i].Date] = &v.Days[i]
	}
	jobs := append([]vmhost.Job(nil), f.Jobs...)
	sortNewest(jobs)
	e := &v.Energy
	for _, j := range jobs {
		v.Total++
		wh, known := jobWh(j)
		if !known {
			e.Unknown++
		}
		e.TotalWh += wh
		if j.Running() {
			v.Running++
			e.RunningWh += wh
		}
		if nowT.Sub(j.Start) <= recent {
			v.Last12h++
			e.Last12hWh += wh
		}
		if d := days[j.Start.In(loc).Format("2006-01-02")]; d != nil {
			d.Jobs++
			d.Wh += wh
		}
		line := JobLine{Handle: j.Handle, Start: j.Start.UTC().Format(time.RFC3339), When: clockAt(j.Start, nowT, loc),
			Took: took(j.UpS), Size: sizeWords(j), Pool: poolOf(f, j), Ended: ended(j),
			Running: j.Running(), Energy: "not known"}
		switch {
		case known && wh == 0:
			line.Energy = "none"
		case known:
			line.Energy = "about " + wattHours(wh)
		}
		v.Jobs = append(v.Jobs, line)
	}
	for i := range v.Days {
		if v.Days[i].Jobs > 0 {
			v.Days[i].Energy = "about " + wattHours(v.Days[i].Wh)
		}
	}
	v.Summary = fmt.Sprintf("%d running now, %d started in the last 12 hours, %d in all", v.Running, v.Last12h, v.Total)
	e.Line = fmt.Sprintf("Electricity, estimated: about %s so far by the %d running, %s in the last 12 hours, %s in all",
		wattHours(e.RunningWh), v.Running, wattHours(e.Last12hWh), wattHours(e.TotalWh))
	e.How = fmt.Sprintf("An estimate, not a measurement: each job's cores × the hours it ran × %g W a core, "+
		"about what one busy Apple silicon core draws. It assumes the job kept its cores busy the whole time, so "+
		"the real use is likely lower, and it leaves out what your Mac draws anyway. A job counts on the day, and in "+
		"the 12 hours, it started in.", WattsPerCore)
	if e.Unknown > 0 {
		e.How += fmt.Sprintf(" %s whose cores are not known (the image's own) %s left out.", plural(e.Unknown, "job"), isAre(e.Unknown))
	}
	if len(f.Unread) > 0 {
		v.Note = "not everything could be read: " + strings.Join(f.Unread, "; ")
	}
	return v
}

// poolOf is the pool j came from, in words.
func poolOf(f JobFacts, j vmhost.Job) string {
	if p := f.Pools[j.Handle]; p != "" {
		return p
	}
	if !f.PoolsFrom.IsZero() && (j.End.IsZero() || !j.End.Before(f.PoolsFrom)) {
		return "this Mac"
	}
	return "not known"
}

func isAre(n int) string {
	if n == 1 {
		return "is"
	}
	return "are"
}

func midnight(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, t.Location())
}

func dayLabel(d, today time.Time) string {
	switch {
	case d.Equal(today):
		return "Today"
	case d.Equal(today.AddDate(0, 0, -1)):
		return "Yesterday"
	}
	return d.Format("Mon Jan 2")
}

// clockAt is t as a local "Jan 2 15:04" (with the year when it is not
// this one), or "Today 15:04".
func clockAt(t, now time.Time, loc *time.Location) string {
	t, now = t.In(loc), now.In(loc)
	if midnight(t).Equal(midnight(now)) {
		return "Today " + t.Format("15:04")
	}
	if t.Year() != now.Year() {
		return t.Format("Jan 2 2006 15:04")
	}
	return t.Format("Jan 2 15:04")
}

// took is a time up in words: "40 s", "12 min", "1 h 5 min", "2 d 3 h".
func took(s float64) string {
	d := time.Duration(math.Round(s)) * time.Second
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%d s", int(d/time.Second))
	case d < time.Hour:
		return fmt.Sprintf("%d min", int(d/time.Minute))
	case d < 24*time.Hour:
		return fmt.Sprintf("%d h %d min", int(d/time.Hour), int(d%time.Hour/time.Minute))
	}
	return fmt.Sprintf("%d d %d h", int(d/(24*time.Hour)), int(d%(24*time.Hour)/time.Hour))
}

// sizeWords is "small: 2 cores, 4 GB memory", with what is known.
func sizeWords(j vmhost.Job) string {
	var parts []string
	if j.CPU > 0 {
		parts = append(parts, plural(j.CPU, "core"))
	}
	if j.MemoryMB > 0 {
		parts = append(parts, memWords(j.MemoryMB)+" memory")
	}
	name := cmp.Or(j.Size, j.Binding)
	if len(parts) == 0 {
		return name
	}
	return name + ": " + strings.Join(parts, ", ")
}

func memWords(mb int) string {
	if mb%1024 == 0 {
		return fmt.Sprintf("%d GB", mb/1024)
	}
	if mb > 1024 {
		return fmt.Sprintf("%.1f GB", float64(mb)/1024)
	}
	return fmt.Sprintf("%d MB", mb)
}

// ended is how a job ended, in a lender's words.
func ended(j vmhost.Job) string {
	why := func(s string) string {
		if j.Why == "" {
			return s
		}
		return s + ": " + j.Why
	}
	switch j.State {
	case "running", "ready", "starting":
		if j.Running() {
			return "running"
		}
	case "stopped":
		return "stopped, its disk kept for a while"
	case "released":
		return "finished"
	case "ended":
		if j.Exit != nil && *j.Exit != 0 {
			return fmt.Sprintf("finished with exit code %d", *j.Exit)
		}
		return "finished"
	case "expired":
		return "reached its time limit"
	case "reclaimed":
		return why("ended early to give the Mac back")
	case "lost":
		return why("lost")
	case "failed":
		return why("did not start")
	}
	return why(j.State)
}

// wattHours is Wh as "40 Wh", or "1.3 kWh" from 1000 Wh.
func wattHours(wh float64) string {
	switch {
	case wh >= 1000:
		return fmt.Sprintf("%.1f kWh", wh/1000)
	case wh >= 10:
		return fmt.Sprintf("%.0f Wh", wh)
	}
	return fmt.Sprintf("%.1f Wh", wh)
}

// sortNewest orders jobs newest start first.
func sortNewest(js []vmhost.Job) {
	slices.SortStableFunc(js, func(a, b vmhost.Job) int { return b.Start.Compare(a.Start) })
}
