package cloudhvguest

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// A hook made by once runs until it has succeeded and never after, one call
// at a time: a first try that failed is tried again by the next command.
func TestOnceRunsUntilItSucceeds(t *testing.T) {
	calls, fail := 0, true
	f := once(func() error {
		calls++
		if fail {
			return errors.New("not yet")
		}
		return nil
	})
	if err := f(); err == nil || calls != 1 {
		t.Fatalf("the first try: %v after %d calls, want its failure", err, calls)
	}
	fail = false
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := f(); err != nil {
				t.Errorf("after it could succeed: %v", err)
			}
		}()
	}
	wg.Wait()
	if calls != 2 {
		t.Errorf("the hook ran %d times, want once failing and once succeeding", calls)
	}
}

// The agent writes the one sudo rule, whole and for the box's user only, and
// takes away one an earlier boot left.
func TestSudoRule(t *testing.T) {
	dir := t.TempDir()
	rule := filepath.Join(dir, "potluck-box")
	if err := dropSudo(rule); err != nil {
		t.Errorf("no rule to remove: %v", err)
	}
	t.Setenv("PATH", dir) // no sudo here
	if err := giveSudo(rule, "box"); err == nil {
		t.Error("a rule was written in an image without sudo")
	}
	if err := os.WriteFile(filepath.Join(dir, "sudo"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"", "root\nbox ALL=(ALL) ALL", "box,other", "%wheel", "ALL", "#1000", "box other", "Box"} {
		if err := giveSudo(rule, name); err == nil {
			t.Errorf("a rule was written for the account %q", name)
		}
	}
	if _, err := os.Stat(rule); err == nil {
		t.Fatal("a refused rule is on disk")
	}
	if err := giveSudo(rule, "box"); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(rule)
	fi, _ := os.Stat(rule)
	if err != nil || string(b) != "box ALL=(ALL:ALL) NOPASSWD: ALL\n" || fi.Mode().Perm() != 0o440 {
		t.Errorf("the rule is %q, mode %v, %v", b, fi.Mode().Perm(), err)
	}
	if left, _ := filepath.Glob(rule + ".*"); len(left) > 0 {
		t.Errorf("left beside the rule: %v", left)
	}
	if err := giveSudo(rule, "box"); err != nil { // an agent started again in a box that keeps its disk
		t.Errorf("a second write: %v", err)
	}
	if err := dropSudo(rule); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(rule); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the rule is still there after the agent started: %v", err)
	}
}
