package tartpin

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// LeftError is a removal that left work folders of interrupted installs, which
// could not be deleted or may hold a bundle Potluck did not make, and so the
// record. What else it removes is gone.
type LeftError struct{ Folders []string }

func (e *LeftError) Error() string {
	return "a hidden folder that could not be deleted or may hold something of yours stays, and Potluck's record of its Tart with it: " + strings.Join(e.Folders, ", ") + ": delete it if you do not need it"
}

// Unfinished reports that nothing is at App() but the record is still there:
// a removal was interrupted, or the bundle was deleted by hand. Remove
// finishes it, and Finish does only that.
func (p Place) Unfinished() bool { return lexists(p.RecordFile) && !lexists(p.App()) }

// Remove deletes Potluck's bundle at App() and the installs' record, and
// nothing else: a tart.app Potluck did not make, or one changed since, is
// left as it is (ErrNotOurs). The bundle is moved aside in one rename,
// checked there (a copy the lender puts in its place meanwhile is put back),
// and only then deleted. While a hidden work folder of an interrupted install that may
// hold a bundle stays, so does the record, and that is a *LeftError, whether or
// not the bundle went. With the record go its lock file and folder. Nothing
// at App() is no error: the record goes.
func (p Place) Remove() (Record, error) { return p.remove(false) }

// Finish is Remove for a removal that was interrupted: a bundle at App(),
// Potluck's too, is left as it is (ErrNotOurs).
func (p Place) Finish() error {
	_, err := p.remove(true)
	return err
}

func (p Place) remove(finish bool) (Record, error) {
	unlock, err := p.lock()
	if err != nil {
		return Record{}, err
	}
	defer unlock()
	if finish && lexists(p.App()) {
		return Record{}, fmt.Errorf("%s: %w", p.App(), ErrNotOurs)
	}
	p.sweep(-time.Hour) // the lock is held: every work folder is an interrupted run's
	rec, ours := p.Installed()
	if !ours && lexists(p.App()) {
		return Record{}, fmt.Errorf("%s: %w", p.App(), ErrNotOurs)
	}
	if ours {
		if err := p.removeBundle(rec); err != nil {
			return rec, err
		}
	}
	var folders []string
	for _, prefix := range []string{swapPrefix, stagePrefix} {
		left, err := filepath.Glob(filepath.Join(p.AppsDir, prefix+"*"))
		if err != nil {
			return rec, err
		}
		folders = append(folders, left...)
	}
	if len(folders) > 0 {
		return rec, &LeftError{Folders: folders}
	}
	if err := os.Remove(p.RecordFile); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return rec, err
	}
	return rec, p.dropLock()
}

// dropLock deletes the lock file, and the folder it is in when that is empty.
// Lock held: a run that waits for it takes a new one (lock).
func (p Place) dropLock() error {
	if err := os.Remove(p.RecordFile + ".lock"); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	_ = syscall.Rmdir(filepath.Dir(p.RecordFile)) // not empty, or a link: it stays
	return nil
}

// HasSpareLock reports that the lock file, or only its empty folder, is there
// and no record is: an install or a removal was interrupted before or after
// it had one.
func (p Place) HasSpareLock() bool {
	if lexists(p.RecordFile) {
		return false
	}
	dir := filepath.Dir(p.RecordFile)
	if fi, err := os.Lstat(dir); err != nil || !fi.IsDir() {
		return lexists(p.RecordFile + ".lock")
	}
	ents, err := os.ReadDir(dir)
	return err == nil && len(ents) == 0 || lexists(p.RecordFile+".lock")
}

// ErrBusy is returned by DropSpareLock while an install or a removal runs.
var ErrBusy = errors.New("a Tart install or removal is going on")

// DropSpareLock deletes a lock file with no record, and the folders an install
// interrupted before its record unpacked into. A tart.app is never touched.
func (p Place) DropSpareLock() error {
	if !p.HasSpareLock() {
		return nil
	}
	unlock, err := p.flock(syscall.LOCK_EX | syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return ErrBusy
	}
	if err != nil {
		return err
	}
	defer unlock()
	if lexists(p.RecordFile) {
		return nil // an install's since
	}
	p.sweep(-time.Hour) // the lock is held: every work folder is an interrupted run's
	return p.dropLock()
}

// removeBundle deletes the bundle rec at App(). Lock held.
func (p Place) removeBundle(rec Record) error {
	dir, err := os.MkdirTemp(p.AppsDir, swapPrefix)
	if err != nil {
		return err
	}
	aside := filepath.Join(dir, AppName)
	beforeMove()
	if err := renameNew(p.App(), aside); err != nil {
		os.Remove(dir)
		return fmt.Errorf("cannot move %s aside: %w. Nothing was removed", p.App(), err)
	}
	if out, ok := p.made(aside); !ok || out != rec {
		if err := renameNew(aside, p.App()); err != nil {
			kept := filepath.Join(p.AppsDir, "tart put aside "+strings.TrimPrefix(filepath.Base(dir), swapPrefix))
			if os.Rename(dir, kept) == nil {
				aside = filepath.Join(kept, AppName)
			}
			return fmt.Errorf("%s changed during the removal: %w, and it could not be put back (%v): it is at %s", p.App(), ErrNotOurs, err, aside)
		}
		os.Remove(dir)
		return fmt.Errorf("%s changed during the removal: %w. Nothing was removed", p.App(), ErrNotOurs)
	}
	// Under this name a half-deleted bundle is swept by a later run.
	doomed := filepath.Join(p.AppsDir, stagePrefix+strings.TrimPrefix(filepath.Base(dir), swapPrefix))
	if err := os.Rename(dir, doomed); err != nil {
		return err // whole, under the name a later run knows it by
	}
	return os.RemoveAll(doomed)
}
