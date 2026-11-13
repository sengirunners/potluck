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
