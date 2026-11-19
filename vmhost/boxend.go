package vmhost

import (
	"fmt"
	"syscall"
)

// endReader is a layer that can read how a VM it made ended (OCI: the
// container's state and its cgroup's memory events, oci_end.go). boxEnd is
// the one place that turns it into words; the supervisor records them as the
// box's why, and exec answers them, so every caller says the same thing.
type endReader interface {
	EndOf(vm string) (vmEnd, error)
}

// vmEnd is what a layer read of a VM's state.
type vmEnd struct {
	Running bool
	Exit    int  // the VM's own exit (a container's first process), when not running
	OOM     bool // the runtime or the kernel recorded a kill for memory
	LimitMB int  // its memory limit; 0: none, or unknown
}

// boxEnd says why vm ended, when its layer can read it and it ended for a
// cause worth naming: a kill for memory, or a signal. A VM that still runs,
// one that stopped with a plain exit (a container never started, a runtime
// that refused it), or a layer that cannot say answers named false, and the
// caller keeps its own words.
func boxEnd(l vmLayer, vm string) (why string, named bool) {
	er, ok := l.(endReader)
	if !ok {
		return "", false
	}
	e, err := er.EndOf(vm)
	if err != nil || e.Running {
		return "", false
	}
	why = e.words()
	return why, why != ""
}

// sigkillExit is a process's exit when SIGKILL ended it.
const sigkillExit = 128 + int(syscall.SIGKILL)

// words is the cause of an ended VM, "" for a plain exit.
func (e vmEnd) words() string {
	limit := ""
	if e.LimitMB > 0 {
		limit = fmt.Sprintf(" (limit %d MB)", e.LimitMB)
	}
	switch {
	case e.OOM:
		return "the box was killed: out of memory" + limit
	case e.Exit == sigkillExit && e.LimitMB > 0:
		return fmt.Sprintf("the box was killed (SIGKILL, exit %d): most likely out of memory%s, though its runtime did not record it as that", e.Exit, limit)
	case e.Exit > 128 && e.Exit < 128+65:
		return fmt.Sprintf("the box was killed by signal %d (exit %d)", e.Exit-128, e.Exit)
	}
	return ""
}
