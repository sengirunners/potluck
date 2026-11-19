package refdriver

import (
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestKillAllJoinsSupervisorExit(t *testing.T) {
	state := t.TempDir()
	if err := save(state, record{Handle: "box:test/cleanup", Kind: "box", Supervisor: 1 << 30}); err != nil {
		t.Fatal(err)
	}
	oldObserve, oldSignal := observeSupervisor, signalSupervisor
	t.Cleanup(func() { observeSupervisor, signalSupervisor = oldObserve, oldSignal })
	entered, complete := make(chan struct{}), make(chan struct{})
	signaled := false
	observeSupervisor = func(pid int) (supervisorExit, error) {
		if pid != 1<<30 {
			t.Errorf("observed pid %d", pid)
		}
		return supervisorExit{close: func() {}, wait: func() error {
			if !signaled {
				t.Error("wait before signal")
			}
			close(entered)
			<-complete
			return nil
		}}, nil
	}
	signalSupervisor = func(pid int) error { signaled = true; return nil }
	done := make(chan error, 1)
	go func() { done <- KillAll(state) }()
	select {
	case <-entered:
	case <-done:
		t.Fatal("cleanup returned before observing supervisor exit")
	case <-time.After(time.Second):
		t.Fatal("cleanup did not join supervisor")
	}
	select {
	case <-done:
		t.Fatal("cleanup returned before supervisor exit")
	default:
	}
	close(complete)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("cleanup did not finish after exit")
	}
}

func TestKillAllActualSupervisorExit(t *testing.T) {
	state := t.TempDir()
	// Start set: with a zero Start the supervisor's early check
	// (time.Since(r.Start) > early) ends the box on its first tick and saves
	// it while superviseRecord saves the supervisor's pid (zm-k5bv).
	r := record{Handle: "box:test/cleanup", Kind: "box", State: "ready", Start: time.Now().UTC(), Deadline: time.Now().Add(time.Minute)}
	if err := save(state, r); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := KillAll(state); err != nil {
			t.Error(err)
		}
	})
	d := drv{state: state}
	// With the lock held, as every acquire calls it, so the supervisor's own
	// save cannot overwrite the pid the driver records.
	if err := locked(state, func() error { return d.superviseRecord(r) }); err != nil {
		t.Fatal(err)
	}
	r, err := load(recordFile(state, r.Handle))
	if err != nil {
		t.Fatal(err)
	}
	exit, err := observeSupervisorExit(r.Supervisor)
	if err != nil {
		t.Fatal(err)
	}
	defer exit.close()
	if err := KillAll(state); err != nil {
		t.Fatal(err)
	}
	if err := exit.wait(); err != nil {
		t.Fatalf("fixture supervisor still alive after cleanup: %v", err)
	}
}

func TestKillAllReportsExitFailure(t *testing.T) {
	state := t.TempDir()
	if err := save(state, record{Handle: "box:test/cleanup", Kind: "box", Supervisor: 1 << 30}); err != nil {
		t.Fatal(err)
	}
	oldObserve, oldSignal := observeSupervisor, signalSupervisor
	t.Cleanup(func() { observeSupervisor, signalSupervisor = oldObserve, oldSignal })
	want := errors.New("missing exit notification")
	observeSupervisor = func(int) (supervisorExit, error) {
		return supervisorExit{close: func() {}, wait: func() error { return want }}, nil
	}
	signalSupervisor = func(int) error { return nil }
	if err := KillAll(state); !errors.Is(err, want) {
		t.Fatalf("got %v, want exit failure", err)
	}
}

// Saves of one record from two writers never leave a half-written record
// for a reader: the supervisor and the driver both save it (zm-k5bv).
func TestSaveConcurrentWritersNeverPublishHalfARecord(t *testing.T) {
	state := t.TempDir()
	r := record{Handle: "box:test/race", Kind: "box", State: "ready", Deadline: time.Now().Add(time.Minute), Dir: strings.Repeat("d", 64<<10)}
	if err := save(state, r); err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if err := save(state, r); err != nil {
					t.Errorf("save: %v", err)
					return
				}
			}
		}()
	}
	for i := 0; i < 2000; i++ {
		if _, err := load(recordFile(state, r.Handle)); err != nil {
			close(stop)
			wg.Wait()
			t.Fatalf("read %d: %v", i, err)
		}
	}
	close(stop)
	wg.Wait()
	if t.Failed() {
		return
	}
	if left, _ := filepath.Glob(filepath.Join(state, "*.tmp")); len(left) != 0 {
		t.Fatalf("temp files left: %v", left)
	}
}
