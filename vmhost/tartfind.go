package vmhost

import (
	"os"
	"os/exec"
	"path/filepath"

	"github.com/sengirunners/potluck/tartpin"
)

// Where a Mac's Tart is looked for, the one place every part of Potluck asks
// (the preflight in hostprobe, the driver's boots and the VM layer's
// commands). A lender may have installed Tart three ways: Homebrew or a link
// the install guide shows (a program in a bin directory), or by dragging
// tart.app into Applications, which puts the program inside the bundle and
// nothing on PATH. A daemon started through launchd does not have the PATH of
// the lender's Terminal, so the usual bin directories are tried too.

// tartApp is the bundle Tart's release unpacks to, and tartInApp the
// program inside it (tartpin installs it in ~/Applications).
const tartApp = tartpin.AppName

var tartInApp = tartpin.ProgramInApp

// TartSearch is what FindTart reads, so a test can give it facts and a
// temporary directory instead of this Mac.
type TartSearch struct {
	LookPath   func(name string) (string, bool) // PATH
	Executable func(path string) bool           // a regular file this user can run
	Exists     func(path string) bool           // anything at the path (a tart.app folder)
	Home       string                           // the user's home; "" leaves ~/Applications and ~/.local/bin out
}

// SystemTartSearch is this machine.
func SystemTartSearch() TartSearch {
	home, _ := os.UserHomeDir()
	return TartSearch{
		LookPath: func(name string) (string, bool) {
			p, err := exec.LookPath(name)
			return p, err == nil
		},
		Executable: IsExecutableFile,
		Exists:     func(path string) bool { _, err := os.Stat(path); return err == nil },
		Home:       home,
	}
}

// TartFound is what FindTart found.
type TartFound struct {
	Path   string   // the Tart program, "" when there is none
	Hollow []string // a tart.app that exists but holds no program (a half-copied or damaged download)
}

// SystemBinDirs are the machine's bin directories a launchd PATH lacks.
var SystemBinDirs = []string{"/opt/homebrew/bin", "/usr/local/bin"}

// FindSoftnet is the softnet set up outside Potluck: on PATH, else in
// SystemBinDirs. It does not look at Potluck's managed install.
func (s TartSearch) FindSoftnet() (string, bool) {
	if p, ok := s.LookPath("softnet"); ok {
		return p, true
	}
	for _, d := range SystemBinDirs {
		if p := filepath.Join(d, "softnet"); s.Executable(p) {
			return p, true
		}
	}
	return "", false
}

// tartBinDirs are the directories searched after PATH, which a daemon's
// launchd PATH lacks: the install guide's link, Homebrew (Apple silicon and
// Intel).
func (s TartSearch) tartBinDirs() []string {
	if s.Home == "" {
		return SystemBinDirs
	}
	return append([]string{filepath.Join(s.Home, ".local", "bin")}, SystemBinDirs...)
}

// tartAppDirs are the places a dragged tart.app is looked for: Applications,
// then the user's own Applications.
func (s TartSearch) tartAppDirs() []string {
	dirs := []string{"/Applications"}
	if s.Home != "" {
		dirs = append(dirs, filepath.Join(s.Home, "Applications"))
	}
	return dirs
}

// FindTart looks for Tart: PATH first (what the lender chose to put there
// wins), then the usual bin directories, then tart.app in /Applications and
// ~/Applications. It does not run what it finds: a copy that cannot run is
// the caller's to report.
func FindTart(s TartSearch) TartFound {
	var f TartFound
	if p, ok := s.LookPath("tart"); ok {
		f.Path = p
		return f
	}
	for _, d := range s.tartBinDirs() {
		p := filepath.Join(d, "tart")
		if s.Executable(p) {
			f.Path = p
			return f
		}
	}
	for _, d := range s.tartAppDirs() {
		app := filepath.Join(d, tartApp)
		p := filepath.Join(app, tartInApp)
		switch {
		case s.Executable(p):
			f.Path = p
			return f
		case s.Exists(app):
			f.Hollow = append(f.Hollow, app)
		}
	}
	return f
}

// ResolveTart is the Tart program to run when a binding names none: the one
// FindTart finds on this Mac, else the bare name "tart", which fails where it
// is run with the usual "not found".
func ResolveTart() string {
	if p := FindTart(SystemTartSearch()).Path; p != "" {
		return p
	}
	return "tart"
}

// IsExecutableFile reports whether path is a regular file with an execute
// bit set (as exec.LookPath judges a PATH entry), following links.
func IsExecutableFile(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.Mode().IsRegular() && st.Mode().Perm()&0o111 != 0
}
