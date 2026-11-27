package cli

// Removing the home so that a removal cut off at any moment leaves either the
// whole home or none of it under its name, and something the next uninstall
// knows for Potluck's and finishes.

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/sengirunners/potluck/hostsetup"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/release"
	"github.com/sengirunners/potluck/kit/swap"
)

// homeDropping is what a home's directory is named while it is deleted.
func homeDropping(home string) string { return home + ".removing" }

// homeLeftover is the directory a removal of home cut off part way left ("":
// none). One that says it is another program's, or nothing at all and holds
// files, is not Potluck's to take.
func homeLeftover(home string) string {
	d := homeDropping(home)
	if fi, err := os.Lstat(d); err != nil || !fi.IsDir() {
		return ""
	}
	if ents, err := os.ReadDir(d); err != nil || (len(ents) > 0 && config.HomeOwner(d) != config.Program) || homeLockHeld(d) {
		return ""
	}
	return d
}

// ownLeftover makes this run's home potluck's own when a removal of that was
// cut off and the run would use another program's daemon: the rest is still
// potluck's to remove.
func (a *app) ownLeftover() error {
	if !a.own || os.Getenv(EnvHome) != "" {
		return nil
	}
	if h, err := a.home(); err == nil && h.Other == nil {
		return nil
	}
	own, err := config.HomeDir()
	if err != nil || isDir(own) || homeLeftover(own) == "" {
		return err
	}
	cfg, err := config.LoadClientFrom(own)
	a.homeV, a.homeErr = daemonHome{Cfg: cfg}, err
	return err
}

// removeHome deletes home. Renamed first, so that a person who sets Potluck
// up again after a removal cut off starts from a new home, never from part
// of the old one; where it cannot be (a mount, a link), it goes in place.
func removeHome(home string) error {
	if d := homeLeftover(home); d != "" {
		if err := dropHome(d); err != nil {
			return err
		}
	}
	if fi, err := os.Lstat(home); err != nil || !fi.IsDir() {
		return os.RemoveAll(home)
	}
	// One that never had a daemon is marked too, for homeLeftover.
	if err := config.ClaimHome(home, config.Program); err == nil && !lexists(homeDropping(home)) && os.Rename(home, homeDropping(home)) == nil {
		return dropHome(homeDropping(home))
	}
	return dropHome(home)
}

// dropHome deletes the directory d, the file that says it is Potluck's last.
func dropHome(d string) error {
	ents, err := os.ReadDir(d)
	if errors.Is(err, fs.ErrNotExist) {
		return nil // another uninstall's
	}
	if err != nil {
		return err
	}
	for _, e := range ents {
		if e.Name() == config.FileProgram {
			continue
		}
		if err := os.RemoveAll(filepath.Join(d, e.Name())); err != nil {
			return err
		}
	}
	return os.RemoveAll(d)
}

// checkHomeRemovable refuses to remove a directory that does not look like a
// Potluck home, or that is the user's own home or a parent of it: a
// POTLUCK_HOME pointed at the wrong place must never take a person's files.
func checkHomeRemovable(home string) error {
	h, err := filepath.Abs(home)
	if err != nil {
		return err
	}
	if uh, err := os.UserHomeDir(); err == nil {
		if rel, err := filepath.Rel(h, uh); err == nil && (rel == "." || !strings.HasPrefix(rel, "..")) {
			return fmt.Errorf("%s is your own home folder or holds it: not removing it (POTLUCK_HOME names the wrong place?)", h)
		}
	}
	// config.json alone proves nothing: a project folder has one. The
	// registry is the daemon's own file, and `program` names its owner.
	if lexists(filepath.Join(h, config.FileRegistry)) || config.HomeOwner(h) == config.Program || hostRunOnly(h) {
		return nil
	}
	return fmt.Errorf("%s does not look like a Potluck home (no %s, and no %s naming %s, in it): not removing it", h, config.FileRegistry, config.FileProgram, config.Program)
}

// hostRunFiles are what a host run, the menu bar item, an upgrade, a daemon's
// first start and uninstall itself write before a daemon claims the home.
var hostRunFiles = []string{hostLockName, hostPIDName, hostBgPIDName, hostLogName, hostsetup.FileName, menuSeenName, stoppedName,
	fileAutostartLock, config.FileLog, config.FileHomeLock, config.FileWriterLock, swap.FileLock, swap.FilePending, release.StateFile, UpdateLog}

// hostRunOnly reports a directory with nothing in it but those (and the
// copies a write cut off leaves), or with nothing: a first setup cut off
// early.
func hostRunOnly(dir string) bool {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range ents {
		ours := slices.ContainsFunc(hostRunFiles, func(n string) bool {
			cut, ok := strings.CutPrefix(e.Name(), "."+n+".") // a temporary copy
			return e.Name() == n || e.Name() == n+".tmp" || (ok && (cut == "new" || strings.Trim(cut, "0123456789") == ""))
		})
		if e.Name() == config.FileProgram { // a claim cut short
			fi, err := e.Info()
			ours = err == nil && fi.Size() == 0
		}
		if !e.Type().IsRegular() || !ours {
			return false
		}
	}
	return true
}
