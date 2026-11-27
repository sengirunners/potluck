package tartpin_test

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/sengirunners/potluck/tartpin"
	"github.com/sengirunners/potluck/tartpin/tartpintest"
)

// installed is a place with Potluck's Tart in it.
func installed(t *testing.T) tartpin.Place {
	t.Helper()
	p := place(t)
	tarball, _, b := tartpintest.Release(t, "1.0.0", tartpintest.Whole())
	if _, err := p.Install(tarball, b); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestRemoveTakesOnlyItsOwnBundle(t *testing.T) {
	p := installed(t)
	mine := filepath.Join(p.AppsDir, "Other.app")
	if err := os.Mkdir(mine, 0o755); err != nil {
		t.Fatal(err)
	}
	if rec, err := p.Remove(); err != nil || rec.Version != "1.0.0" {
		t.Fatalf("%+v %v", rec, err)
	}
	if ents, _ := os.ReadDir(p.AppsDir); len(ents) != 1 || ents[0].Name() != "Other.app" {
		t.Fatalf("Applications holds %v", ents)
	}
	if _, err := os.Stat(p.RecordFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the record is still there: %v", err)
	}
	if _, err := p.Remove(); err != nil {
		t.Fatalf("a second run: %v", err)
	}
}

func TestRemoveLeavesWhatPotluckDidNotMake(t *testing.T) {
	for name, change := range map[string]func(p tartpin.Place) error{
		"changed since": func(p tartpin.Place) error {
			return os.WriteFile(filepath.Join(p.App(), "Contents", "mine"), nil, 0o600)
		},
		"a copy of it": func(p tartpin.Place) error {
			if err := os.CopyFS(p.App()+"2", os.DirFS(p.App())); err != nil {
				return err
			}
			if err := os.RemoveAll(p.App()); err != nil {
				return err
			}
			return os.Rename(p.App()+"2", p.App())
		},
		"a link": func(p tartpin.Place) error {
			if err := os.Rename(p.App(), p.App()+"2"); err != nil {
				return err
			}
			return os.Symlink(p.App()+"2", p.App())
		},
		"no record": func(p tartpin.Place) error { return os.Remove(p.RecordFile) },
	} {
		p := installed(t)
		if err := change(p); err != nil {
			t.Fatal(err)
		}
		if _, err := p.Remove(); !errors.Is(err, tartpin.ErrNotOurs) {
			t.Errorf("%s: %v", name, err)
		}
		if _, err := os.Stat(p.Program()); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// A copy the lender puts there between the check and the move is put back.
func TestRemovePutsBackWhatAppeared(t *testing.T) {
	p := installed(t)
	defer tartpin.SetBeforeMove(func() {
		if err := os.RemoveAll(p.App()); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(p.App(), "lender"), 0o755); err != nil {
			t.Fatal(err)
		}
	})()
	if _, err := p.Remove(); !errors.Is(err, tartpin.ErrNotOurs) {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(p.App(), "lender")); err != nil {
		t.Fatal(err)
	}
	if ents, _ := os.ReadDir(p.AppsDir); len(ents) != 1 {
		t.Fatalf("Applications holds %v", ents)
	}
}

func TestFinishLeavesAnInstalledOne(t *testing.T) {
	p := installed(t)
	if err := p.Finish(); !errors.Is(err, tartpin.ErrNotOurs) {
		t.Fatal(err)
	}
	if _, ours := p.Installed(); !ours {
		t.Fatal("it is gone")
	}
}

// A removal killed between its move and its delete is finished by the next.
func TestRemoveFinishesAnInterruptedOne(t *testing.T) {
	for name, interrupt := range map[string]func(p tartpin.Place, aside string) error{
		"moved aside": func(tartpin.Place, string) error { return nil },
		"half deleted": func(p tartpin.Place, aside string) error {
			if err := os.Remove(filepath.Join(aside, "tart.app", tartpin.ProgramInApp)); err != nil {
				return err
			}
			return os.Rename(aside, filepath.Join(p.AppsDir, ".tart.app.potluck-1"))
		},
	} {
		p := installed(t)
		aside := filepath.Join(p.AppsDir, ".tart.app.swap-1")
		if err := os.Mkdir(aside, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(p.App(), filepath.Join(aside, "tart.app")); err != nil {
			t.Fatal(err)
		}
		if err := interrupt(p, aside); err != nil {
			t.Fatal(err)
		}
		if !p.Unfinished() {
			t.Errorf("%s: not seen", name)
		}
		if err := p.Finish(); err != nil || p.Unfinished() {
			t.Errorf("%s: %v", name, err)
		}
		if ents, _ := os.ReadDir(p.AppsDir); len(ents) != 0 {
			t.Errorf("%s: Applications holds %v", name, ents)
		}
	}
}

// A work folder that may hold the lender's own bundle stays, and the caller
// is told, with the record, whether or not the bundle went.
func TestRemoveSaysWhatStays(t *testing.T) {
	p := installed(t)
	theirs := filepath.Join(p.AppsDir, ".tart.app.swap-9")
	if err := os.MkdirAll(filepath.Join(theirs, "tart.app", "mine"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := p.Remove()
	var left *tartpin.LeftError
	if !errors.As(err, &left) || len(left.Folders) != 1 || left.Folders[0] != theirs {
		t.Fatalf("%v", err)
	}
	if _, err := os.Stat(p.App()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the bundle: %v", err)
	}
	if err := p.Finish(); !errors.As(err, &left) || !p.Unfinished() {
		t.Fatalf("again: %v", err)
	}
	if err := os.RemoveAll(theirs); err != nil {
		t.Fatal(err)
	}
	if err := p.Finish(); err != nil || p.Unfinished() {
		t.Fatalf("once it is gone: %v", err)
	}
}

// Runs that wait on the lock a removal deletes go on with the one there now:
// one at a time, and none loses.
func TestRemoveAndInstallAtOnce(t *testing.T) {
	p := place(t)
	tarball, _, b := tartpintest.Release(t, "1.0.0", tartpintest.Whole())
	errs := make(chan error)
	for range 4 {
		go func() { _, err := p.Install(tarball, b); errs <- err }()
		go func() { _, err := p.Remove(); errs <- err }()
	}
	for range 8 {
		if err := <-errs; err != nil {
			t.Error(err)
		}
	}
	if _, err := p.Remove(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Dir(p.RecordFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the record's folder stays: %v", err)
	}
}

// A lock an install holds stays, and the install's folder with it.
func TestDropSpareLockLeavesAnInstallGoingOn(t *testing.T) {
	p := installed(t)
	if err := os.Remove(p.RecordFile); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(p.RecordFile + ".lock")
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	if err := p.DropSpareLock(); !errors.Is(err, tartpin.ErrBusy) || !p.HasSpareLock() {
		t.Fatal(err)
	}
	f.Close()
	if err := p.DropSpareLock(); err != nil || p.HasSpareLock() {
		t.Fatal(err)
	}
	if _, err := os.Stat(p.Program()); err != nil {
		t.Fatalf("a tart.app with no record went: %v", err)
	}
}
