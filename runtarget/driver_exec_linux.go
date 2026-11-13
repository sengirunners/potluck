package runtarget

import (
	"fmt"
	"os"
	"os/exec"
)

// execVerified points cmd at the file v hashed, through its open descriptor,
// so a file renamed over v.path after the hash is never run.
//
// A binary runs as /proc/self/fd/N, N being the daemon's descriptor: the
// kernel opens it before it closes close-on-exec descriptors, so N may stay
// close-on-exec and no other child of the daemon inherits it.
//
// A #! script cannot: its interpreter opens the script by the path the
// kernel hands it (/proc/self/fd/N) after the exec, when a close-on-exec N
// is already gone. Clearing close-on-exec on N in the daemon would leak it
// into every child forked meanwhile, so the descriptor is passed as fd 3 of
// this child only (ExtraFiles: open, not close-on-exec, there) and the
// script runs as /proc/self/fd/3. The script's $0 is that path, and it and
// its children keep that read-only descriptor of the script.
//
// Without /proc the path is run, checked once more (driver_exec_other.go's
// window).
func execVerified(cmd *exec.Cmd, v *verified) error {
	cmd.Args[0] = v.path
	if _, err := os.Stat("/proc/self/fd"); err != nil {
		cmd.Path = v.path
		return v.recheck()
	}
	head := make([]byte, 2)
	if n, _ := v.f.ReadAt(head, 0); n == 2 && string(head) == "#!" {
		cmd.ExtraFiles = []*os.File{v.f}
		cmd.Path = "/proc/self/fd/3"
		return nil
	}
	cmd.Path = fmt.Sprintf("/proc/self/fd/%d", v.f.Fd())
	return nil
}
