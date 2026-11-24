package menubar

import (
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/potluck/vmhost"
)

// The job window counts running now, started in the last 12 hours and per
// local day; the estimate is cores × hours × WattsPerCore, says so, and
// names the jobs it could not count.
func TestBuildJobs(t *testing.T) {
	loc := time.FixedZone("here", -5*3600)
	nowT := time.Date(2026, 11, 19, 15, 0, 0, 0, loc)
	exit := 2
	jobs := []vmhost.Job{
		{Handle: "box:t/old", Binding: "t", Kind: "box", Size: "small", CPU: 1, MemoryMB: 512,
			Start: nowT.Add(-26 * time.Hour), End: nowT.Add(-25 * time.Hour), UpS: 3600, State: "released"},
		{Handle: "box:t/run", Binding: "t", Kind: "box", Size: "large", CPU: 4, MemoryMB: 8192,
			Start: nowT.Add(-30 * time.Minute), UpS: 1800, State: "ready"},
		{Handle: "box:t/fail", Binding: "t", Kind: "box", Size: "large", CPU: 4, MemoryMB: 8192,
			Start: nowT.Add(-2 * time.Hour), End: nowT.Add(-2 * time.Hour), State: "failed", Why: "no image"},
		{Handle: "run:r/x", Binding: "r", Kind: "run", Start: nowT.Add(-3 * time.Hour), End: nowT.Add(-3*time.Hour + 40*time.Second),
			UpS: 40, State: "ended", Exit: &exit},
		{Handle: "box:t/kept", Binding: "t", Kind: "box", Size: "small", CPU: 1, MemoryMB: 1536,
			Start: nowT.Add(-13 * time.Hour), UpS: 7200, State: "stopped"},
	}
	v := BuildJobs(JobFacts{Jobs: jobs, Pools: map[string]string{"box:t/run": "acme"},
		PoolsFrom: nowT.Add(-25*time.Hour - time.Minute), Loc: loc, Now: nowT})
	if v.Running != 1 || v.Last12h != 3 || v.Total != 5 {
		t.Fatalf("counts: running %d, 12 h %d, total %d", v.Running, v.Last12h, v.Total)
	}
	if v.Summary != "1 running now, 3 started in the last 12 hours, 5 in all" {
		t.Errorf("summary %q", v.Summary)
	}
	// Energy: the running one 4 cores × 0.5 h × 5 W = 10 Wh; the old one 5 Wh;
	// the kept one 1 × 2 h × 5 = 10 Wh; the run's cores are not known.
	e := v.Energy
	if e.RunningWh != 10 || e.Last12hWh != 10 || e.TotalWh != 25 || e.Unknown != 1 {
		t.Errorf("energy %+v", e)
	}
	if !strings.Contains(e.Line, "about 10 Wh so far by the 1 running") || !strings.Contains(e.Line, "25 Wh in all") {
		t.Errorf("energy line %q", e.Line)
	}
	if !strings.Contains(e.How, "5 W a core") || !strings.Contains(e.How, "estimate") || !strings.Contains(e.How, "1 job whose cores are not known") {
		t.Errorf("how %q", e.How)
	}
	if len(v.Days) != jobDays || v.Days[0].Label != "Today" || v.Days[0].Jobs != 4 || v.Days[1].Label != "Yesterday" || v.Days[1].Jobs != 1 {
		t.Fatalf("days %+v", v.Days[:2])
	}
	if v.Days[0].Energy != "about 20 Wh" || v.Days[1].Energy != "about 5.0 Wh" || v.Days[2].Energy != "" || v.Days[2].Label != "Tue Nov 17" {
		t.Errorf("days %+v", v.Days[1:3])
	}
	if len(v.Jobs) != 5 || v.Jobs[0].Handle != "box:t/run" || v.Jobs[4].Handle != "box:t/old" {
		t.Fatalf("order %+v", v.Jobs)
	}
	want := map[string]JobLine{
		"box:t/run":  {When: "Today 14:30", Took: "30 min", Size: "large: 4 cores, 8 GB memory", Pool: "acme", Ended: "running", Energy: "about 10 Wh", Running: true},
		"box:t/fail": {Took: "0 s", Ended: "did not start: no image", Pool: "this Mac", Energy: "none"},
		"run:r/x":    {Took: "40 s", Size: "r", Ended: "finished with exit code 2", Energy: "not known"},
		"box:t/kept": {When: "Today 02:00", Took: "2 h 0 min", Size: "small: 1 core, 1.5 GB memory", Ended: "stopped, its disk kept for a while"},
		"box:t/old":  {When: "Nov 18 13:00", Took: "1 h 0 min", Pool: "this Mac", Ended: "finished", Energy: "about 5.0 Wh"},
	}
	for _, j := range v.Jobs {
		w := want[j.Handle]
		for _, c := range [][3]string{{"when", w.When, j.When}, {"took", w.Took, j.Took}, {"size", w.Size, j.Size},
			{"pool", w.Pool, j.Pool}, {"ended", w.Ended, j.Ended}, {"energy", w.Energy, j.Energy}} {
			if c[1] != "" && c[1] != c[2] {
				t.Errorf("%s %s: %q, want %q", j.Handle, c[0], c[2], c[1])
			}
		}
		if j.Running != w.Running {
			t.Errorf("%s running %v", j.Handle, j.Running)
		}
	}
}

// No jobs is a window of zeros, and the menu always offers it.
func TestBuildJobsNone(t *testing.T) {
	old := vmhost.Job{Handle: "box:t/x", Start: time.Now().Add(-40 * 24 * time.Hour), End: time.Now().Add(-40 * 24 * time.Hour)}
	if p := BuildJobs(JobFacts{Jobs: []vmhost.Job{old}, PoolsFrom: time.Now().Add(-29 * 24 * time.Hour)}).Jobs[0].Pool; p != "not known" {
		t.Errorf("a job older than the ledger: pool %q", p)
	}
	if p := BuildJobs(JobFacts{Jobs: []vmhost.Job{{Handle: "box:t/y", Start: time.Now()}}}).Jobs[0].Pool; p != "not known" {
		t.Errorf("no ledger read: pool %q", p)
	}
	v := BuildJobs(JobFacts{Unread: []string{"x: denied"}})
	if v.Total != 0 || len(v.Jobs) != 0 || v.Jobs == nil || v.Note != "not everything could be read: x: denied" {
		t.Errorf("%+v", v)
	}
	m := Build(Facts{Cores: 4}).Menu
	if len(m.Windows) != 3 || m.Windows[1].ID != "jobs" || strings.Join(m.Windows[1].Args, " ") != "--json host jobs" {
		t.Errorf("windows %+v", m.Windows)
	}
}
