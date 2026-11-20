package vmhost

import (
	"os"
	"testing"
	"time"
)

// spendOf is the amount a status of the handle answers, read as the driver
// answers it after its sweep (no sweep here: the race is between the two).
func spendOf(t *testing.T, d *drv, handle string) float64 {
	t.Helper()
	s, err := d.status(handle)
	if err != nil {
		t.Error(err)
		return 0
	}
	return s.(map[string]any)["spend"].(map[string]any)["amount"].(float64)
}

func testDrv(t *testing.T, m map[string]string) *drv {
	t.Helper()
	c, err := parseConfig(m)
	if err != nil {
		t.Fatal(err)
	}
	return &drv{cfg: c, st: store{c.StateDir}, vm: newLayer(c)}
}

// zm-eakh.12.51: a run's supervisor ends it while a status is answering. The
// end is stamped under the lock and saved a moment later; a status that read
// the record in between must not answer more than the end then counts.
func TestSpendNeverFallsWhenARunEndsUnderARead(t *testing.T) {
	m := cfg(t)
	now := time.Now().UTC()
	r := put(t, m, record{ID: "r000000000001", Kind: "run", State: stRunning,
		Start: now.Add(-time.Minute), RunStart: now.Add(-time.Minute), Deadline: now.Add(time.Hour)})
	d := testDrv(t, m)
	held, read := make(chan struct{}), make(chan float64, 1)
	go func() {
		<-held
		time.Sleep(20 * time.Millisecond) // the read comes well after the end's time
		read <- spendOf(t, d, r.Handle)
	}()
	first, got := 0.0, false
	err := d.st.locked(func() error { // as supervise.go's end: the time taken, then the save
		o, err := d.st.load(r.ID)
		if err != nil {
			return err
		}
		exit := 0
		o.State, o.Exit, o.End = stEnded, &exit, time.Now().UTC()
		close(held)
		select {
		case first = <-read: // a read that does not wait for the lock answers now
			got = true
		case <-time.After(500 * time.Millisecond): // one that waits answers after the save
		}
		return d.st.save(o)
	})
	if err != nil {
		t.Fatal(err)
	}
	if !got {
		first = <-read
	}
	if second := spendOf(t, d, r.Handle); second < first {
		t.Errorf("spend fell from %v to %v when the run ended under a read", first, second)
	}
}

// A persistent box answered while its supervisor was alive, which then died:
// the sweep counts its time up to the last beat, and a stop never counts less
// than a status already answered.
func TestSpendNeverFallsWhenAKeptBoxSupervisorDies(t *testing.T) {
	m := cfg(t)
	now := time.Now().UTC()
	r := put(t, m, record{ID: "r000000000002", State: stReady, Persist: true, KeepS: 3600, Supervisor: os.Getpid(),
		Start: now.Add(-2 * time.Hour), Seg: now.Add(-time.Hour), RanMs: 1000, RunStart: now.Add(-time.Hour), Deadline: now.Add(time.Hour)})
	st := store{m["state_dir"]}
	beat := now.Add(-30 * time.Second)
	if err := os.WriteFile(st.beatFile(r.ID), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(st.beatFile(r.ID), beat, beat); err != nil {
		t.Fatal(err)
	}
	first := spendOf(t, testDrv(t, m), r.Handle)
	if _, err := st.update(r.ID, func(o *record) error { o.Supervisor = deadPID(t); return nil }); err != nil {
		t.Fatal(err)
	}
	s, err := call(t, m, "status", r.Handle, nil) // its sweep stops the box
	if err != nil || s["state"] != stStopped {
		t.Fatalf("status after the supervisor died: %v %v", s, err)
	}
	if second := s["spend"].(map[string]any)["amount"].(float64); second < first {
		t.Errorf("spend fell from %v to %v when the supervisor died", first, second)
	}
}

// zm-82u4: a run whose supervisor died two days ago, swept only now, counts
// its time to the last its files show of it (lastSeen), not to the sweep;
// and a status read while that supervisor still ran never answered more.
func TestSweepCountsADeadSupervisorsJobToItsLastSeen(t *testing.T) {
	m := cfg(t)
	now := time.Now().UTC()
	start, seen := now.Add(-72*time.Hour), now.Add(-48*time.Hour)
	r := put(t, m, record{ID: "r000000000003", Kind: "run", State: stRunning, Supervisor: os.Getpid(),
		Start: start, RunStart: start, Deadline: now.Add(time.Hour)})
	st := store{m["state_dir"]}
	age := func() { // the record and the beat were last written at seen
		t.Helper()
		if err := os.WriteFile(st.beatFile(r.ID), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		for _, p := range []string{st.file(r.ID), st.beatFile(r.ID)} {
			if err := os.Chtimes(p, seen, seen); err != nil {
				t.Fatal(err)
			}
		}
	}
	age()
	first := spendOf(t, testDrv(t, m), r.Handle)
	if _, err := st.update(r.ID, func(o *record) error { o.Supervisor = deadPID(t); return nil }); err != nil {
		t.Fatal(err)
	}
	age()
	s, err := call(t, m, "status", r.Handle, nil) // its sweep ends the run
	if err != nil || s["state"] != stLost {
		t.Fatalf("status after the supervisor died: %v %v", s, err)
	}
	second := s["spend"].(map[string]any)["amount"].(float64)
	want := seen.Sub(start).Minutes()
	t.Logf("spend before the death %.1f min, after the sweep %.1f min (to the sweep it would be %.1f)", first, second, now.Sub(start).Minutes())
	if second < want-1 || second > want+1 {
		t.Errorf("spend after the sweep %.1f min, want %.1f (start to last seen)", second, want)
	}
	if second < first {
		t.Errorf("spend fell from %v to %v when the supervisor died", first, second)
	}
	js, err := Jobs(m["state_dir"], time.Now())
	if err != nil || len(js) != 1 || !js[0].End.Equal(seen) {
		t.Errorf("the lender's list: %+v %v, want one job ended at %v", js, err, seen)
	}
}

// zm-82u4.1: whether a job whose supervisor died ran out its time is judged
// by when it ended (lastSeen), not by when the sweep came. One that died two
// hours ago, an hour before its deadline, is lost (unbilled) though the sweep
// comes after that deadline; and a file stamped ahead of now (the clock
// stepped back) cannot end a job in the future or past its deadline.
func TestSweepJudgesExpiryByWhenADeadSupervisorsJobEnded(t *testing.T) {
	m := cfg(t)
	now := time.Now().UTC()
	st := store{m["state_dir"]}
	for _, c := range []struct {
		id             string
		seen, deadline time.Time
	}{
		{"r000000000004", now.Add(-2 * time.Hour), now.Add(-time.Hour)}, // died before its deadline, swept after it
		{"r000000000005", now.Add(2 * time.Hour), now.Add(time.Hour)},   // its files stamped ahead of now
	} {
		start := now.Add(-3 * time.Hour)
		r := put(t, m, record{ID: c.id, Kind: "run", State: stRunning, Supervisor: deadPID(t),
			Start: start, RunStart: start, Deadline: c.deadline})
		if err := os.WriteFile(st.beatFile(r.ID), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		for _, p := range []string{st.file(r.ID), st.beatFile(r.ID)} {
			if err := os.Chtimes(p, c.seen, c.seen); err != nil {
				t.Fatal(err)
			}
		}
		s, err := call(t, m, "status", r.Handle, nil) // its sweep ends the run
		after := time.Now()
		if err != nil || s["state"] != stLost || s["exit"] != nil {
			t.Errorf("%s: status after the sweep %v %v, want lost with no exit", c.id, s, err)
		}
		got, err := st.load(r.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.State != stLost || got.Exit != nil {
			t.Errorf("%s: record %s exit %v, want lost with no exit", c.id, got.State, got.Exit)
		}
		if c.seen.After(after) { // clamped to the sweep's now
			if got.End.Before(now) || got.End.After(after) {
				t.Errorf("%s: End %v, want the sweep's now (%v..%v)", c.id, got.End, now, after)
			}
		} else if !got.End.Equal(c.seen) {
			t.Errorf("%s: End %v, want its last seen %v", c.id, got.End, c.seen)
		}
	}
}
