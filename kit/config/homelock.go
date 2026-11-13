package config

import (
	"fmt"
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
		return nil, fmt.Errorf("a zm daemon already runs for %s (pid %s); if `zm status` cannot reach it, its socket is gone: kill %s, then start again", home, pid, pid)
	}
	_ = f.Truncate(0)
	_, _ = f.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0)
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}
