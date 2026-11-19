package vmhost

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The lender's job list: a box that runs is listed running with its size; a
// released one says so with its time up; once the driver forgets the record
// (keepFor past its end) the archive still lists it, once.
func TestJobsOutliveTheirRecords(t *testing.T) {
	m := fakePersistBox(t)
	m["persist_max"] = "0"
	if js, err := Jobs(m["state_dir"], time.Now()); err != nil || len(js) != 0 {
		t.Fatalf("before any job: %v %v", js, err)
	}
	a, err := call(t, m, "acquire", "", map[string]any{"request_id": "k1", "ttl_s": 120})
	if err != nil {
		t.Fatal(err)
	}
	h := a["handle"].(string)
	js, err := Jobs(m["state_dir"], time.Now())
	if err != nil || len(js) != 1 {
		t.Fatalf("one running: %v %v", js, err)
	}
	if j := js[0]; j.Handle != h || !j.Running() || j.Size != "small" || j.CPU != 1 || j.MemoryMB != 512 || j.Binding != "t" || j.Kind != "box" {
		t.Fatalf("running job: %+v", j)
	}
	time.Sleep(20 * time.Millisecond)
	if _, err := call(t, m, "release", h, nil); err != nil {
		t.Fatal(err)
	}
	js, _ = Jobs(m["state_dir"], time.Now())
	if len(js) != 1 || js[0].Running() || js[0].State != "released" || js[0].End.IsZero() || js[0].UpS <= 0 {
		t.Fatalf("released job: %+v", js)
	}
	up := js[0].UpS

	// Forgotten: its end moved past keepFor, the next call's sweep removes it.
	st := store{m["state_dir"]}
	for _, r := range st.all() {
		if _, err := st.update(r.ID, func(r *record) error {
			r.End, r.Start = r.End.Add(-keepFor-time.Hour), r.Start.Add(-keepFor-time.Hour)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := call(t, m, "list", "", nil); err != nil {
		t.Fatal(err)
	}
	if ps, _ := filepath.Glob(filepath.Join(m["state_dir"], "runs", "*.json")); len(ps) != 0 {
		t.Fatalf("the sweep kept %v", ps)
	}
	js, err = Jobs(m["state_dir"], time.Now())
	if err != nil || len(js) != 1 || js[0].Handle != h || js[0].State != "released" || js[0].UpS != up {
		t.Fatalf("from the archive: %+v %v", js, err)
	}
	b, _ := os.ReadFile(filepath.Join(m["state_dir"], jobsFile))
	for _, secret := range []string{"request_id", "k1", "script"} {
		if strings.Contains(string(b), secret) {
			t.Errorf("the archive holds %q: %s", secret, b)
		}
	}
}

// A state directory that does not exist has run nothing; an archive past
// jobsMax keeps its newer half, so the newest jobs stay listed.
func TestJobsArchiveBound(t *testing.T) {
	dir := t.TempDir()
	if js, err := Jobs(filepath.Join(dir, "none"), time.Now()); err != nil || js != nil {
		t.Fatalf("no state directory: %v %v", js, err)
	}
	defer func(n int64) { jobsMax = n }(jobsMax)
	jobsMax = 4 << 10
	st := store{dir}
	t0 := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	for i := range 100 {
		r := record{Handle: "box:t/b" + string(rune('a'+i%26)) + string(rune('a'+i/26)), Target: "t", Kind: "box", State: stReleased,
			Start: t0.Add(time.Duration(i) * time.Minute), End: t0.Add(time.Duration(i)*time.Minute + 30*time.Second)}
		st.archive(r)
	}
	st.archive(record{Handle: "box:t/warm", Warm: true, Start: t0, State: stReleased}) // nobody's
	if fi, err := os.Stat(filepath.Join(dir, jobsFile)); err != nil || fi.Size() >= jobsMax {
		t.Fatalf("archive size: %v %v", fi, err)
	}
	js, err := Jobs(dir, time.Now())
	if err != nil || len(js) < 10 || len(js) >= 100 {
		t.Fatalf("bounded archive: %d jobs, %v", len(js), err)
	}
	if !js[0].Start.Equal(t0.Add(99*time.Minute)) || js[0].UpS != 30 {
		t.Errorf("the newest job: %+v", js[0])
	}
	for _, j := range js {
		if j.Handle == "box:t/warm" {
			t.Errorf("a warm box no acquire took is listed")
		}
	}
}

// A start failure's own words (they can quote what the job printed) never
// reach the list or the archive: only its report's path. A job whose
// supervisor died is not running, though no sweep ran. A line too long to
// be a job's does not hide the others.
func TestJobsKeepNoFailureWordsAndNoDeadJobs(t *testing.T) {
	dir := t.TempDir()
	st := store{dir}
	if err := st.create(); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "runs"), 0o700); err != nil {
		t.Fatal(err)
	}
	t0 := time.Now().Add(-time.Hour).UTC()
	failed := record{ID: "rf", Handle: "box:t/f", Target: "t", Kind: "box", State: stFailed, Start: t0, End: t0.Add(time.Second),
		Why: "checkout: fatal: token ghp_SECRETsecret123 printed this\nmore output; report: " + filepath.Join(dir, "failures", "f.txt")}
	dead := record{ID: "rd", Handle: "box:t/d", Target: "t", Kind: "box", State: stReady, Start: t0, Supervisor: deadPID(t)}
	for _, r := range []record{failed, dead} {
		if err := st.save(r); err != nil {
			t.Fatal(err)
		}
	}
	long := `{"handle":"box:t/long","why":"` + strings.Repeat("x", 2<<20) + `"}` + "\n"
	ok := `{"handle":"box:t/ok","kind":"box","start":"` + t0.Add(-time.Hour).Format(time.RFC3339) + `","state":"released"}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, jobsFile), []byte(long+ok), 0o600); err != nil {
		t.Fatal(err)
	}
	js, err := Jobs(dir, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]Job{}
	for _, j := range js {
		by[j.Handle] = j
	}
	if j := by["box:t/f"]; j.State != stFailed || strings.Contains(j.Why, "SECRET") || strings.Contains(j.Why, "printed") || !strings.Contains(j.Why, "f.txt") {
		t.Errorf("failed job: %+v", j)
	}
	if j := by["box:t/d"]; j.Running() || j.State != stLost || j.End.IsZero() {
		t.Errorf("a job whose supervisor died: %+v", j)
	}
	if _, ok := by["box:t/ok"]; !ok {
		t.Errorf("a long line hid the next: %v", js)
	}
	st.archive(failed)
	b, _ := os.ReadFile(filepath.Join(dir, jobsFile))
	if strings.Contains(string(b), "SECRET") {
		t.Errorf("the archive holds a failure's words")
	}
}
