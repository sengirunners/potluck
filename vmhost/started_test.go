package vmhost

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/sengirunners/potluck/boxproc"
)

// A test whose driver starts processes (a run's supervisor, a spawn's
// holder, the sweep's detached helpers) gets its state directory from
// stateDir, or has endAtCleanup watch it: when the test ends, every one of
// those processes, and everything each started, has exited before any of
// the test's directories goes. A release answers once the box is gone, but
// its supervisor then runs its own cleanup for a moment (the fake CLI
// appends to its calls file, removes a container directory), and a TempDir
// removed under that fails "directory not empty".

const (
	endWait    = 30 * time.Second // a supervisor's own cleanup: OCI Destroy retries for up to 15 s
	orphanWait = time.Second      // then what it left: a fake's guest process, which a real VM's end ends
)

// stateDir answers a new state directory for a driver (os.MkdirTemp: short,
// its paths show in logs), removed once what it started has ended.
func stateDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "zmrt")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	endAtCleanup(t, dir)
	return dir
}

// endAtCleanup has t's cleanup end what was started from state. It makes
// t's TempDir first: Go registers the removal of all of a test's TempDirs
// when the first is made, and cleanups run last registered first, so this
// one runs before any TempDir goes, those the test makes later too.
func endAtCleanup(t *testing.T, state string) {
	t.Helper()
	_ = t.TempDir()
	t.Cleanup(func() { endStarted(t, state) })
}

// endStarted releases what is still live in state (as release does, but
// without the driver's sweep, which may start a warm box) and waits for
// every process started from it to exit: each is a session leader (Setsid)
// whose argv names state, and what it starts stays in its session (a boot's
// own process group too). A leader that outlives endWait fails the test,
// naming its pid; what a gone leader left is ended after orphanWait.
func endStarted(t *testing.T, state string) {
	t.Helper()
	st := store{state}
	self := os.Getpid()
	known := map[int]bool{} // from state's files: a pid there may since be another process's
	for _, r := range st.all() {
		if r.Supervisor > 0 && r.Supervisor != self {
			known[r.Supervisor] = true
		}
	}
	metas, _ := filepath.Glob(filepath.Join(state, "procs", "*", "*"))
	for _, d := range metas {
		if m, err := boxproc.Dir(d).Meta(); err == nil && m.Holder > 0 && m.Holder != self {
			known[m.Holder] = true
		}
	}
	for _, r := range st.all() {
		if r.live() {
			_, _ = st.update(r.ID, func(o *record) error {
				if o.live() {
					o.State, o.End = stReleased, time.Now().UTC()
				}
				return nil
			})
		}
	}
	orphaned := map[int]time.Time{}
	deadline, killed := time.Now().Add(endWait), false
	for {
		ps, err := listProcs()
		if err != nil {
			t.Errorf("listing processes to end what %s started: %v", state, err)
			return
		}
		// Rebuilt each pass from known (the leaders seen so far, pruned when a
		// pid is running as something else): a leader's pid recycled by an
		// unrelated process must not make that process's session count.
		leaders := map[int]bool{}
		up := map[int]bool{} // leaders still running
		for _, p := range ps {
			switch {
			case p.zombie:
				// Started in process (Setsid: in this test binary only a driver's
				// detached processes are): its session counts, and it is reaped.
				if sid, err := unix.Getsid(p.pid); p.ppid == self && (known[p.pid] || err == nil && sid == p.pid) {
					leaders[p.pid], known[p.pid] = true, true
					_, _ = syscall.Wait4(p.pid, nil, syscall.WNOHANG, nil)
				}
			case startedFrom(p.args, state):
				leaders[p.pid], up[p.pid], known[p.pid] = true, true, true
			default:
				delete(known, p.pid) // running, and not started from state: a reused pid
			}
		}
		for pid := range known {
			leaders[pid] = true // gone: its session may still hold what it started
		}
		var live, left []proc
		for _, p := range ps {
			if p.zombie || p.pid == self {
				continue
			}
			sid, err := unix.Getsid(p.pid)
			switch {
			case err != nil || !leaders[sid]:
			case up[sid]:
				live = append(live, p)
			default:
				left = append(left, p)
			}
		}
		if len(live) == 0 && len(left) == 0 {
			return
		}
		now := time.Now()
		for _, p := range left {
			if at, ok := orphaned[p.pid]; !ok {
				orphaned[p.pid] = now
			} else if now.Sub(at) > orphanWait {
				t.Logf("ending pid %d (%s): its supervisor or holder is gone", p.pid, p.args)
				_ = syscall.Kill(p.pid, syscall.SIGKILL)
			}
		}
		if len(live) > 0 && now.After(deadline) {
			if killed {
				t.Errorf("still running after SIGKILL: %s", describe(live))
				return
			}
			t.Errorf("started from %s and still running %s after release: %s", state, endWait, describe(live))
			for _, p := range live {
				_ = syscall.Kill(p.pid, syscall.SIGKILL)
			}
			deadline, killed = now.Add(5*time.Second), true
		}
		time.Sleep(50 * time.Millisecond)
	}
}

type proc struct {
	pid, ppid int
	zombie    bool
	args      string
}

func listProcs() ([]proc, error) {
	out, err := exec.Command("ps", "-A", "-ww", "-o", "pid=,ppid=,stat=,args=").Output()
	if err != nil {
		return nil, err
	}
	var ps []proc
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) < 3 {
			continue
		}
		pid, err1 := strconv.Atoi(f[0])
		ppid, err2 := strconv.Atoi(f[1])
		if err1 != nil || err2 != nil {
			continue
		}
		ps = append(ps, proc{pid: pid, ppid: ppid, zombie: strings.HasPrefix(f[2], "Z"), args: strings.Join(f[3:], " ")})
	}
	return ps, nil
}

// startedFrom says whether args are a driver's detached process for state:
// <driver> --supervise|--hold|--measure <state> <id>, or --shelf-fill <state>.
func startedFrom(args, state string) bool {
	f := strings.Fields(args)
	for i := 0; i+1 < len(f); i++ {
		switch f[i] {
		case flagSupervise, flagHold, flagMeasure, flagShelfFill:
			if f[i+1] == state {
				return true
			}
		}
	}
	return false
}

func describe(ps []proc) string {
	var b strings.Builder
	for _, p := range ps {
		fmt.Fprintf(&b, "\n  pid %d: %s", p.pid, p.args)
	}
	return b.String()
}
