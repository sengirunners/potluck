package vmhost

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A run still starting when the driver stops waiting for it is no refusal:
// its supervisor may yet make it, so the driver answers internal (the daemon
// then looks it up) and never unavailable (which the daemon takes as made
// nothing and lets another binding take the request id).
func TestWaitStartedTimeoutIsNotARefusal(t *testing.T) {
	state := t.TempDir()
	if err := os.MkdirAll(filepath.Join(state, "runs"), 0o700); err != nil {
		t.Fatal(err)
	}
	st := store{state}
	if err := st.save(record{ID: "r-slow", State: stStarting, Supervisor: os.Getpid(), Start: time.Now().UTC(), Deadline: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	d := &drv{cfg: config{StateDir: state, BootTimeout: -15}, st: st} // the wait is over at once
	for name, wait := range map[string]func() error{
		"waitStarted":   func() error { _, err := d.waitStarted("r-slow"); return err },
		"waitRestarted": func() error { _, err := d.waitRestarted("r-slow", 0); return err },
	} {
		var e refusal
		if err := wait(); !errors.As(err, &e) || e.code != "internal" {
			t.Errorf("%s past its deadline: %v, want internal", name, err)
		}
	}
}
