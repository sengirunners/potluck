package refdriver

import (
	"errors"
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
	r := record{Handle: "box:test/cleanup", Kind: "box", State: "ready", Deadline: time.Now().Add(time.Minute)}
	if err := save(state, r); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := KillAll(state); err != nil {
			t.Error(err)
		}
	})
	d := drv{state: state}
	if err := d.superviseRecord(r); err != nil {
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
