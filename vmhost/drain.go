package vmhost

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"
)

// A drain (hostmode.go) in the supervisor. A GitHub runner serves one job:
// while it still waits for one it is idle, so it ends at once, before it can
// take a job and be cut; once it has a job it finishes it, and its box then
// ends itself (a box otherwise stays ready after its runner exits). Any other
// run or box runs on. Whatever is still running at the drain's deadline is
// reclaimed, its why ending in drainCut. A runner is idle until it prints
// that it took a job, so a drain in the moment between GitHub assigning a job
// and that line ends the runner and cuts that job.

// runnerJobLine is what a GitHub runner prints when it takes a job.
var runnerJobLine = []byte("Running job: ")

// jobWatch passes a runner's output on and notes when it takes a job.
type jobWatch struct {
	w    io.Writer
	mu   sync.Mutex
	tail []byte
	seen bool
	done chan struct{} // closed when the runner exits
}

func newJobWatch(w io.Writer) *jobWatch { return &jobWatch{w: w, done: make(chan struct{})} }

func (j *jobWatch) Write(p []byte) (int, error) {
	j.mu.Lock()
	if !j.seen {
		buf := append(j.tail, p...)
		j.seen = bytes.Contains(buf, runnerJobLine)
		j.tail = append([]byte(nil), buf[max(0, len(buf)-len(runnerJobLine)+1):]...)
	}
	j.mu.Unlock()
	return j.w.Write(p)
}

// idle says whether the runner is still waiting for a job, or has exited.
func (j *jobWatch) idle() bool {
	select {
	case <-j.done:
		return true
	default:
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	return !j.seen
}

// refusesNew answers why new work may not start now (nil: it may): the
// supervisor asks before boot and before the run or box starts.
func (s *sup) refusesNew() error {
	if ok, why := s.cfg.admitsNow(); !ok {
		return errors.New(why)
	}
	return nil
}

// hostTick applies the host's state to live run or box r; true when it ended it.
func (s *sup) hostTick(r record) bool {
	h := s.cfg.host()
	why := h.why
	switch h.verdict {
	case hostAdmits, hostHolds:
		return false
	case hostDrains:
		if s.jobs == nil || !s.jobs.idle() {
			return false // running work finishes
		}
		if s.jobs.seenJob() {
			why += ": drained, its runner finished its job"
		} else {
			why += ": drained, its runner had no job"
		}
	}
	if r.Persist { // the owner's machine back at once; the box's disk kept
		s.powerOff()
		s.stopKept("reclaimed: " + why)
	} else {
		s.vm.Destroy(r.VM, s.runPID)
		s.end(stReclaimed, nil, why)
	}
	if h.mode == modeAuto && h.verdict == hostCloses && !h.rd.outside {
		fmt.Fprintln(s.log, reclaimTiming(h.rd.at, time.Now(), h.rd.idle, h.rd.byInput))
	} else {
		fmt.Fprintf(s.log, "potluck-driver-tart: reclaimed (host %s): %s\n", h.mode, why)
	}
	return true
}

func (j *jobWatch) seenJob() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.seen
}
