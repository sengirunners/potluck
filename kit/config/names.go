package config

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sync"
)

// Names are the names a program's home and socket go by, each list in the
// order kit reads it. Kit holds none of its own: a program sets its names
// with Use before its first Load (cmd/potluck: cli.UseHome; zm: its
// wiring). A program renamed keeps its old names after the new ones, so
// one order serves the daemon, its CLI and any app that finds the daemon:
//
//	HomeEnvs {"NEW_HOME", "OLD_HOME"}, HomeDirs {".new", ".old"}, Sockets {"new.sock", "old.sock"}
//
// The home is the first variable set; else the first directory in the
// user's home that exists; else the first directory (a new home). Two
// variables set to different directories: the first wins, with a warning.
// The socket a client dials is the first one in the home that exists, else
// the first; the daemon listens on the first. Nothing on disk is moved.
type Names struct {
	HomeEnvs []string
	HomeDirs []string
	Sockets  []string
}

// NoSocketName is the socket's file name when the program names none (a
// test that wires nothing and loads a home by path).
const NoSocketName = "daemon.sock"

var (
	namesMu sync.RWMutex
	names   Names
)

// Use sets this program's names. Main calls it first, before any Load; a
// test that changes them restores the old ones (Wired) when it ends.
func Use(n Names) {
	n = Names{HomeEnvs: slices.Clone(n.HomeEnvs), HomeDirs: slices.Clone(n.HomeDirs), Sockets: slices.Clone(n.Sockets)}
	namesMu.Lock()
	names = n
	namesMu.Unlock()
}

// Wired returns the names Use set (empty lists before any Use).
func Wired() Names {
	namesMu.RLock()
	defer namesMu.RUnlock()
	return Names{HomeEnvs: slices.Clone(names.HomeEnvs), HomeDirs: slices.Clone(names.HomeDirs), Sockets: slices.Clone(names.Sockets)}
}

// HomeEnv is the variable a person sets to choose the home: the program's
// first ("" when it names none).
func HomeEnv() string {
	if n := Wired(); len(n.HomeEnvs) > 0 {
		return n.HomeEnvs[0]
	}
	return ""
}

// ErrNoNames is HomeDir's error when the program named no home (it never
// called Use): kit has no home of its own to fall back to.
var ErrNoNames = errors.New("config: this program names no home (main calls config.Use before Load)")

// Resolved is where ResolveHome found the home: Dir, an absolute path, and
// From, the variable that named it or "" for a directory in the user's
// home. Warning is set when two of the variables name different homes.
type Resolved struct {
	Dir, From, Warning string
}

// ResolveHome applies n's home order. getenv reads a variable, userHome is
// the user's home ("" when there is none) and isDir reports whether a path
// is a directory. It reads nothing else, so a table test drives it.
func ResolveHome(n Names, getenv func(string) string, userHome string, isDir func(string) bool) (Resolved, error) {
	var r Resolved
	for _, env := range n.HomeEnvs {
		v := getenv(env)
		if v == "" {
			continue
		}
		abs, err := filepath.Abs(v)
		if err != nil {
			return Resolved{}, fmt.Errorf("%s=%s: %w", env, v, err)
		}
		if r.From == "" {
			r.Dir, r.From = abs, env
			continue
		}
		if abs != r.Dir && r.Warning == "" {
			r.Warning = fmt.Sprintf("%s and %s name different homes; using %s=%s", r.From, env, r.From, r.Dir)
		}
	}
	if r.From != "" {
		return r, nil
	}
	if len(n.HomeDirs) == 0 {
		if len(n.HomeEnvs) == 0 {
			return Resolved{}, ErrNoNames
		}
		return Resolved{}, fmt.Errorf("no home: set %s", n.HomeEnvs[0])
	}
	if userHome == "" {
		if len(n.HomeEnvs) == 0 {
			return Resolved{}, errors.New("no home directory")
		}
		return Resolved{}, fmt.Errorf("no home directory; set %s", n.HomeEnvs[0])
	}
	for _, d := range n.HomeDirs {
		if p := filepath.Join(userHome, d); isDir(p) {
			return Resolved{Dir: p}, nil
		}
	}
	return Resolved{Dir: filepath.Join(userHome, n.HomeDirs[0])}, nil
}

// SocketIn is the socket a client dials in home under n: the first of
// n.Sockets that exists there, else the first (NoSocketName when n names
// none). exists reports whether a path exists.
func SocketIn(n Names, home string, exists func(string) bool) string {
	if len(n.Sockets) == 0 {
		return filepath.Join(home, NoSocketName)
	}
	for _, s := range n.Sockets {
		if p := filepath.Join(home, s); exists(p) {
			return p
		}
	}
	return filepath.Join(home, n.Sockets[0])
}

// ListenSocketIn is the socket the daemon listens on in home: n's first.
func ListenSocketIn(n Names, home string) string {
	if len(n.Sockets) == 0 {
		return filepath.Join(home, NoSocketName)
	}
	return filepath.Join(home, n.Sockets[0])
}

// WarnTo is where HomeDir writes ResolveHome's warning, once per process.
var WarnTo io.Writer = os.Stderr

var warnOnce sync.Once

func isDir(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

func exists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}
