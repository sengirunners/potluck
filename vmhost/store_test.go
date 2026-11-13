package vmhost

import (
	"os"
	"path/filepath"
	"testing"
)

// A supervisor still running after the owner removed the state directory finds it gone; its
// next locked call fails and leaves nothing behind.
func TestLockedDoesNotRecreateRemovedStateDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	st := store{dir}
	if err := st.create(); err != nil {
		t.Fatal(err)
	}
	ran := false
	if err := st.locked(func() error { ran = true; return nil }); err != nil || !ran {
		t.Fatalf("locked on a present directory: ran=%v err=%v", ran, err)
	}
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	ran = false
	if err := st.locked(func() error { ran = true; return nil }); err == nil || ran {
		t.Fatalf("locked on a removed directory: ran=%v err=%v, want an error without running f", ran, err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("state directory was recreated (stat err %v)", err)
	}
}

// The driver's entry point makes a missing state directory: a first run.
func TestDispatchCreatesStateDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "fresh", "state")
	m := cfg(t)
	m["state_dir"] = dir
	if _, err := call(t, m, "list", "", nil); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(filepath.Join(dir, "runs")); err != nil || !fi.IsDir() {
		t.Fatalf("runs under a first-run state directory: %v", err)
	}
}
