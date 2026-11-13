package refdriver

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// Reproduce a supervisor ending the resource between status loading a running
// record and sampling spend. No scheduler delay or repeated probabilistic run:
// the clock callback gives the terminal writer its exact opportunity to win.
func TestStatusSpendDoesNotExceedConcurrentTerminalSpend(t *testing.T) {
	state := t.TempDir()
	start := time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC)
	r := record{Handle: "run:test/r3", Kind: "run", State: "running", Start: start, Deadline: start.Add(time.Minute)}
	if err := save(state, r); err != nil {
		t.Fatal(err)
	}
	terminal := func(at time.Time) error {
		current, err := load(recordFile(state, r.Handle))
		if err != nil {
			return err
		}
		code := 0
		current.State, current.End, current.Exit = "ended", at, &code
		return save(state, current)
	}
	writer, err := os.OpenFile(filepath.Join(state, "lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	ended := false
	d := drv{state: state, cfg: map[string]string{"target": "test", "kind": "run"}}
	d.now = func() time.Time {
		err := syscall.Flock(int(writer.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		switch {
		case err == nil:
			// The broken status path dropped its lock before spending. The
			// supervisor writes an endpoint just before status samples now.
			if err := terminal(start.Add(time.Second)); err != nil {
				t.Fatal(err)
			}
			ended = true
			if err := syscall.Flock(int(writer.Fd()), syscall.LOCK_UN); err != nil {
				t.Fatal(err)
			}
		case errors.Is(err, syscall.EWOULDBLOCK):
			// The observation owns the lock; the supervisor must end later.
		default:
			t.Fatal(err)
		}
		return start.Add(2 * time.Second)
	}
	read := func() (string, float64) {
		t.Helper()
		result, err := d.status(request{ID: r.Handle})
		if err != nil {
			t.Fatal(err)
		}
		out := result.(map[string]any)
		return out["state"].(string), out["spend"].(map[string]any)["amount"].(float64)
	}
	firstState, first := read()
	if !ended {
		if err := locked(state, func() error { return terminal(start.Add(2 * time.Second)) }); err != nil {
			t.Fatal(err)
		}
	}
	lastState, last := read()
	if firstState != "running" || lastState != "ended" {
		t.Fatalf("states %q -> %q, want running -> ended", firstState, lastState)
	}
	if last < first {
		t.Fatalf("spend fell across terminal transition: %v -> %v", first, last)
	}
	_, again := read()
	if again != last {
		t.Fatalf("terminal spend changed: %v -> %v", last, again)
	}
}

// fakeTB records what a LAN helper did to its test.
type fakeTB struct{ fatal, skip bool }

func (*fakeTB) Helper()        {}
func (f *fakeTB) Fatal(...any) { f.fatal = true }
func (f *fakeTB) Skip(...any)  { f.skip = true }

func TestLANAddrCIFailsUnderCI(t *testing.T) {
	old := lanAddr
	defer func() { lanAddr = old }()
	lanAddr = func() (string, bool) { return "", false }
	for _, c := range []struct {
		ci          string
		fatal, skip bool
	}{{"true", true, false}, {"1", true, false}, {"", false, true}, {"false", false, true}} {
		t.Setenv("CI", c.ci)
		var f fakeTB
		if _, ok := LANAddrCI(&f, "x"); ok || f.fatal != c.fatal {
			t.Errorf("CI=%q: LANAddrCI ok=%v fatal=%v, want fatal=%v", c.ci, ok, f.fatal, c.fatal)
		}
		f = fakeTB{}
		RequireLANAddr(&f, "x")
		// A real Fatal ends the test before Skip; the fake does not.
		if f.fatal != c.fatal || (!c.fatal && f.skip != c.skip) {
			t.Errorf("CI=%q: RequireLANAddr fatal=%v skip=%v, want fatal=%v skip=%v", c.ci, f.fatal, f.skip, c.fatal, c.skip)
		}
	}
	lanAddr = func() (string, bool) { return "10.0.0.1", true }
	t.Setenv("CI", "true")
	var f fakeTB
	if ip := RequireLANAddr(&f, "x"); ip != "10.0.0.1" || f.fatal || f.skip {
		t.Errorf("with a LAN address: %q fatal=%v skip=%v", ip, f.fatal, f.skip)
	}
}

func TestNoNCFailsUnderCI(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // no nc
	for _, c := range []struct {
		ci          string
		fatal, skip bool
	}{{"true", true, false}, {"1", true, false}, {"", false, true}, {"false", false, true}} {
		t.Setenv("CI", c.ci)
		var f fakeTB
		FirewallCI(&f, t.TempDir(), "127.0.0.1:1")
		if f.fatal != c.fatal || f.skip != c.skip {
			t.Errorf("CI=%q: FirewallCI fatal=%v skip=%v, want fatal=%v skip=%v", c.ci, f.fatal, f.skip, c.fatal, c.skip)
		}
		f = fakeTB{}
		NoNC(&f, "the guest has no nc")
		if f.fatal != c.fatal || f.skip != c.skip {
			t.Errorf("CI=%q: NoNC fatal=%v skip=%v, want fatal=%v skip=%v", c.ci, f.fatal, f.skip, c.fatal, c.skip)
		}
	}
}

func TestAcquiredListsEveryReceivedAcquire(t *testing.T) {
	state := t.TempDir()
	if got := Acquired(state); len(got) != 0 {
		t.Fatalf("Acquired before any acquire: %v", got)
	}
	d := &drv{state: state}
	for _, k := range []string{"k1", "k2", "k1"} {
		if err := d.logAcquire(k); err != nil {
			t.Fatal(err)
		}
	}
	if got := Acquired(state); len(got) != 3 || got[0] != "k1" || got[1] != "k2" || got[2] != "k1" {
		t.Errorf("Acquired = %v, want k1 k2 k1", got)
	}
}
