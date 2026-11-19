package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// FileHomeLock is held by the running daemon for its whole life, and by a
// restore into the home for all of it (zm-c339).
const FileHomeLock = "daemon.lock"

// LockHome takes the one-daemon-per-home lock (an flock, so a crashed
// holder never leaves it held) and records the pid in it. A second daemon on
// the same home is refused even when the first one's socket is gone
// (dogfood: three daemons ran on one registry.db after a stop/start race
// unlinked the socket of the newest). release drops it.
func LockHome(home string) (release func(), err error) {
	path := filepath.Join(home, FileHomeLock)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		b, _ := os.ReadFile(path)
		f.Close()
		pid := strings.TrimSpace(string(b))
		if _, perr := strconv.Atoi(pid); perr != nil {
			pid = "unknown"
		}
		return nil, fmt.Errorf("a %s daemon already runs for %s (pid %s); if `%s` cannot reach it, its socket is gone: kill %s, then start again", Program, home, pid, Command("status"), pid)
	}
	_ = f.Truncate(0)
	_, _ = f.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0)
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}

// FileProgram names the program whose daemon owns the home: the first
// daemon to start on a home writes its program (Program) there, and a
// daemon of any other program refuses the home (ClaimHome).
const FileProgram = "program"

// ClaimHome is a daemon's check, before it opens anything in home, that
// home is program's: a home with no FileProgram yet becomes program's; one
// that names another program is refused, so a daemon never opens another
// program's registry.db.
func ClaimHome(home, program string) error {
	path := filepath.Join(home, FileProgram)
	b, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return os.WriteFile(path, []byte(program+"\n"), 0o600)
	case err != nil:
		return err
	}
	owner := strings.TrimSpace(string(b))
	switch owner {
	case program:
		return nil
	case "": // a write cut short
		return os.WriteFile(path, []byte(program+"\n"), 0o600)
	}
	return fmt.Errorf("%s is the home of %s's daemon, not %s's: a daemon never opens another program's home "+
		"(if this home is yours under a new program name, write %q into %s)", home, owner, program, program, path)
}

// HomeOwner is the program FileProgram names in home ("" when none has
// claimed it yet, or it cannot be read).
func HomeOwner(home string) string {
	b, err := os.ReadFile(filepath.Join(home, FileProgram))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}
