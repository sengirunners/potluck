// Package daemonrecord is the machine's record of its Potluck daemons: a
// daemon built on Potluck's kit (potluck's own, or a program that adds
// modules on top of it) writes one record while it listens, and a client
// that has no home of its own finds the machine's daemon by reading them
// (Live). kit/server writes and removes the records; nothing else does.
//
// The records live in one per-user directory that is no program's home
// (Dir): one file per program, <program>.json, written whole (0600, in a
// 0700 directory) and replaced atomically.
package daemonrecord

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/sengirunners/potluck/kit/atomicfile"
	"github.com/sengirunners/potluck/kit/client"
)

// EnvDir names the records' directory instead of the default (tests, and a
// person who keeps per-user state elsewhere).
const EnvDir = "POTLUCK_DAEMONS_DIR"

// DefaultDir is the records' directory under the user's home.
const DefaultDir = ".local/state/potluck/daemons"

// Record is one running daemon, as the daemon wrote it when it started
// listening.
type Record struct {
	Program  string    `json:"program"`  // config.Program of the binary that runs it
	Exe      string    `json:"exe"`      // that binary, symlinks resolved
	Home     string    `json:"home"`     // its home directory
	Socket   string    `json:"socket"`   // the Unix socket it listens on
	PID      int       `json:"pid"`      // its process
	Protocol string    `json:"protocol"` // api.WireProtocol
	Version  string    `json:"version"`  // api.Version of its build
	Modules  []string  `json:"modules"`  // the modules its routes come from (core.Route.Module), sorted
	Started  time.Time `json:"started"`
}

// Serves reports whether every one of modules has routes on this daemon.
func (r Record) Serves(modules ...string) bool {
	for _, m := range modules {
		if !slices.Contains(r.Modules, m) {
			return false
		}
	}
	return true
}

// programName is what a record's file may be named after.
var programName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// Dir is the records' directory: $POTLUCK_DAEMONS_DIR, else DefaultDir in
// the user's home. In a test binary only the variable names one: with it
// unset Dir is "" and records are off (no write, none read), so no test
// touches a real home's records whatever HOME is.
func Dir() string {
	if d := os.Getenv(EnvDir); d != "" {
		if abs, err := filepath.Abs(d); err == nil {
			return abs
		}
		return d
	}
	if testing.Testing() {
		return ""
	}
	h, err := os.UserHomeDir()
	if err != nil || h == "" {
		return ""
	}
	return filepath.Join(h, DefaultDir)
}

// path is r's file in dir.
func path(dir, program string) (string, error) {
	if !programName.MatchString(program) {
		return "", fmt.Errorf("daemonrecord: program name %q cannot name a record", program)
	}
	return filepath.Join(dir, program+".json"), nil
}

// Write writes r as its program's record in dir (made 0700 when missing,
// and refused when it is not this user's alone).
// A record of the same program that is still live for another home stays:
// the machine's daemon of a program is the first one that runs; written
// reports whether r was written. With dir "" (records off) it writes none.
func Write(ctx context.Context, dir string, r Record) (written bool, err error) {
	if dir == "" {
		return false, nil
	}
	p, err := path(dir, r.Program)
	if err != nil {
		return false, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return false, err
	}
	if fi, err := os.Lstat(dir); err == nil && fi.IsDir() && fi.Mode().Perm()&0o077 != 0 {
		_ = os.Chmod(dir, 0o700) // this user's directory made under a looser umask; another's stays refused below
	}
	if err := private(dir, true); err != nil {
		return false, err
	}
	if old, err := read(p); err == nil && old.Home != r.Home && alive(ctx, old) {
		return false, nil
	}
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return false, err
	}
	if err := atomicfile.WritePrivate(p, append(b, '\n')); err != nil {
		return false, err
	}
	return true, nil
}

// Remove removes r's program's record from dir if it is still r's (the same
// process and home): a daemon that stops never removes the next one's.
func Remove(dir string, r Record) error {
	if dir == "" {
		return nil
	}
	p, err := path(dir, r.Program)
	if err != nil {
		return err
	}
	old, err := read(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil || old.PID != r.PID || old.Home != r.Home {
		return nil
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// Read is every record in dir, live or not, sorted by program. A file that
// is not a record, or not this user's alone (private), is skipped; a
// directory that is not this user's alone is an error: a record names a
// socket to dial and a program to run, so only this user may write one.
func Read(dir string) ([]Record, error) {
	if dir == "" {
		return nil, nil
	}
	if err := private(dir, true); errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []Record
	for _, e := range ents {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".json") || strings.HasPrefix(name, ".") {
			continue
		}
		r, err := read(filepath.Join(dir, name))
		if err != nil || r.Program+".json" != name {
			continue
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Program < out[j].Program })
	return out, nil
}

// Live is the records in Dir that keep accepts (nil: all) whose daemon
// runs: its process is this user's and alive, and its socket is there and, when it
// answers, answers as that process (a daemon too slow to answer, or on another
// protocol major, still counts: it is up). The records are probed at once,
// each for at most a few seconds.
func Live(ctx context.Context, keep func(Record) bool) ([]Record, error) {
	all, err := Read(Dir())
	if err != nil {
		return nil, err
	}
	var asked []Record
	for _, r := range all {
		if keep == nil || keep(r) {
			asked = append(asked, r)
		}
	}
	up := make([]bool, len(asked))
	var wg sync.WaitGroup
	for i, r := range asked {
		wg.Go(func() { up[i] = alive(ctx, r) })
	}
	wg.Wait()
	var out []Record
	for i, r := range asked {
		if up[i] {
			out = append(out, r)
		}
	}
	return out, nil
}

// probeWait bounds one record's socket probe.
const probeWait = 2 * time.Second

// alive reports whether r's process is alive and this user's (a signal
// may be sent to it), and its socket is there and does not answer as another process.
func alive(ctx context.Context, r Record) bool {
	if r.PID <= 0 || r.Socket == "" {
		return false
	}
	if err := syscall.Kill(r.PID, 0); err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, probeWait)
	defer cancel()
	h, err := client.NewUnix(r.Socket).Health(ctx)
	if err != nil {
		// A live process whose socket is there but did not answer health
		// (slow, or speaking another protocol major) is a daemon that is up:
		// treating it as absent would let a second daemon start beside it.
		// Only "not running" (socket missing or refused) means gone.
		return !client.NotRunning(err)
	}
	return h.PID == r.PID
}

// private checks that p is this user's alone: owned by this user, no
// group or other permission bits, not a symlink, and a directory (dir) or
// a regular file.
func private(p string, dir bool) error {
	fi, err := os.Lstat(p)
	if err != nil {
		return err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	switch {
	case dir && !fi.IsDir(), !dir && !fi.Mode().IsRegular():
		return fmt.Errorf("daemonrecord: %s is not a %s", p, map[bool]string{true: "directory", false: "regular file"}[dir])
	case !ok || int(st.Uid) != os.Getuid():
		return fmt.Errorf("daemonrecord: %s is not owned by this user", p)
	case fi.Mode().Perm()&0o077 != 0:
		return fmt.Errorf("daemonrecord: %s is open to other users (mode %v); want %s", p, fi.Mode().Perm(), map[bool]string{true: "0700", false: "0600"}[dir])
	}
	return nil
}

func read(p string) (Record, error) {
	if err := private(p, false); err != nil {
		return Record{}, err
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return Record{}, err
	}
	var r Record
	if err := json.Unmarshal(b, &r); err != nil {
		return Record{}, err
	}
	if r.Program == "" || r.Home == "" || r.Socket == "" {
		return Record{}, errors.New("daemonrecord: incomplete record " + p)
	}
	return r, nil
}
