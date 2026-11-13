package protocol

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// A driver is a program named for its driver name: potluck-driver-<name>, or
// zm-run-<name> (its old name). A daemon looks for the new name first, so a
// machine may carry either or both, and only in its fixed directories, with
// each driver pinned by sha256 (FindDriver here; the pins in runtarget,
// DRIVERS.md); the conformance suite still finds a name on PATH (LookDriver).
const (
	DriverPrefix    = "potluck-driver-"
	DriverPrefixOld = "zm-run-"
)

// DriverProgram is the driver name of a program name ("zm-run-tart" and
// "potluck-driver-tart" are "tart"), or "" if it names no driver.
func DriverProgram(program string) string {
	for _, p := range []string{DriverPrefix, DriverPrefixOld} {
		if n, ok := strings.CutPrefix(program, p); ok {
			return n
		}
	}
	return ""
}

// LookDriver finds driver name's program on PATH: potluck-driver-<name>,
// else zm-run-<name>. A name that is already a program name is looked up
// as it is.
func LookDriver(name string) (string, error) {
	if DriverProgram(name) != "" {
		return exec.LookPath(name)
	}
	p, err := exec.LookPath(DriverPrefix + name)
	if err == nil {
		return p, nil
	}
	return exec.LookPath(DriverPrefixOld + name)
}

// DriverStateDir is a driver's default state directory under home:
// ~/.potluck-driver-<name>, unless only the old ~/.zm-run-<name> exists,
// which a machine that has it keeps using (never moved: a driver may name
// its VMs after the directory's path, and records live in it).
func DriverStateDir(home, name string) string {
	dir := filepath.Join(home, "."+DriverPrefix+name)
	if _, err := os.Stat(dir); err == nil {
		return dir
	}
	if old := filepath.Join(home, "."+DriverPrefixOld+name); isDir(old) {
		return old
	}
	return dir
}

func isDir(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

// DriversDir is the directory under a daemon's home it also runs drivers from.
const DriversDir = "drivers"

// ErrDriverName and ErrNoDriver are FindDriver's refusals.
var (
	ErrDriverName = errors.New("not a driver name: lowercase letters, digits, . _ - and no leading dot or dash")
	ErrNoDriver   = errors.New("drivers run only from these directories, never from PATH")
)

// driverName is a driver's name: no path separator, no leading dot or dash.
var driverName = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)

// DriverDirs are the only directories a daemon with this home runs drivers
// from, in order: the directory of the running program (symlinks resolved;
// zm's old zm-run-<driver> names are found there too), then <home>/drivers.
// A command run by the same program (potluck host, potluck driver pin) gets
// the daemon's directories this way.
func DriverDirs(home string) []string {
	var dirs []string
	if exe, err := os.Executable(); err == nil {
		if r, err := filepath.EvalSymlinks(exe); err == nil {
			dirs = append(dirs, filepath.Dir(r))
		}
	}
	return append(dirs, filepath.Join(home, DriversDir))
}

// FindDriver is potluck-driver-<driver>, else zm-run-<driver>, in the first
// of dirs that has either (a file, or a link to one). PATH is never searched.
func FindDriver(dirs []string, driver string) (string, error) {
	if !driverName.MatchString(driver) {
		return "", fmt.Errorf("%q: %w", driver, ErrDriverName)
	}
	for _, dir := range dirs {
		for _, prefix := range []string{DriverPrefix, DriverPrefixOld} {
			p := filepath.Join(dir, prefix+driver)
			if st, err := os.Stat(p); err == nil && st.Mode().IsRegular() {
				return p, nil
			}
		}
	}
	return "", fmt.Errorf("no %s%s (or %s%s) in %s: %w", DriverPrefix, driver, DriverPrefixOld, driver, strings.Join(dirs, " or "), ErrNoDriver)
}
