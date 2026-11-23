package vmhost

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/sengirunners/potluck/protocol"
)

// A napping VM has no hypervisor process and holds no memory, so a binding
// with ch_nap_slots=N holds N more VMs than its slots, as long as no more
// guests than its slots are awake at once:
//
//   - the driver's count (room and slotUse, driver.go) leaves a VM that gave
//     its slot up for a nap out of the slots in use, and bounds all of the
//     binding's live VMs by slots + N. A new VM is admitted by that count,
//     as on any layer;
//   - a napping VM takes its slot back before it is restored (takeSlot): its
//     monitor makes the same count, under the same lock, and marks the VM
//     waking when fewer guests than the slots are awake. When that stays
//     untrue the VM naps on, its command is refused with
//     protocol.NoFreeSlot's words, and nothing of it is lost.
//
// The count is of every guest that holds a slot in the state directory,
// whatever binding or layer made it: a guest that does not nap, or naps with
// its slot, is awake for as long as its record is live, and a VM of this
// layer for as long as it has a hypervisor process (Slot) or its box is
// still starting.
//
// The binding's cores and memory limits (room) still count every live VM,
// napping or not: a nap ends whenever a command comes.

// cloudhvAwakeWait is how long a wake waits for a slot before it is refused,
// and cloudhvAwakePoll how often it counts meanwhile. Tests shorten the wait.
var cloudhvAwakeWait = 10 * time.Second

const cloudhvAwakePoll = 250 * time.Millisecond

// cloudhvAwakeLockTry is how long the lock is tried, at the least, for a
// command whose wait was over before its turn came.
const cloudhvAwakeLockTry = time.Second

// A count reads every record of the state directory under its lock, which
// every verb of the driver takes: a wait counts less often the more records
// there are, one poll longer for each cloudhvAwakePollRecords of them.
const cloudhvAwakePollRecords = 500

// cloudhvNapsOn starts what a refused wake says to the command that asked.
var cloudhvNapsOn = protocol.NoFreeSlot("linux") + ": the box naps on"

// cloudhvNoSlot is a wake's error when the VM got no slot: it still naps.
// counts is how often the slots were counted for it. gone: the wait ended
// because the command that asked had left, not because the host was full.
type cloudhvNoSlot struct {
	why    string
	counts int
	gone   bool
}

func (e cloudhvNoSlot) Error() string { return cloudhvNapsOn + ": " + e.why }

// takeSlot takes the napping VM's slot back, for the restore that follows:
// it waits until fewer guests than the binding's slots are awake, and marks
// the VM waking in its state file under the lock the count is made under, so
// no two takers count the same free slot. A VM that kept its slot (no
// ch_nap_slots) has nothing to take. A count that cannot be made, or a mark
// that cannot be written, refuses as a full host does. The lock is never
// waited for: a driver that holds it may be waiting for this monitor to
// stop. The wait is h's command's: it ends cloudhvAwakeWait after the
// command asked, however long it stood behind others, and at once when the
// command has left or the VM is told to stop: no slot is taken after either.
// A command taken up with its wait already over is still given a count: the
// lock is tried for cloudhvAwakeLockTry before it is refused for a busy lock.
func (m *chmon) takeSlot(h chHold) error {
	if m.v.Awake <= 0 {
		return nil
	}
	st, l := store{m.v.StateDir}, cloudhvCLI{c: cloudhvConfig{Dir: filepath.Dir(m.dir)}}
	said, counts, gone := false, 0, h.gone
	for began, end := time.Now(), h.at.Add(cloudhvAwakeWait); ; {
		select {
		case <-gone: // a slot taken now would be held by a box with no command to run
			return cloudhvNoSlot{why: "the command that asked is gone", counts: counts, gone: true}
		default:
		}
		took, records := false, 0
		held, err := st.tryLocked(func() error {
			rs, err := st.readAll()
			if err != nil {
				return err
			}
			counts, records = counts+1, len(rs)
			others := rs[:0:0] // the VM's own record is no other guest in its way
			for _, r := range rs {
				if l.vmDir(r.VM, r.Config) != filepath.Clean(m.dir) {
					others = append(others, r)
				}
			}
			if _, awake, _ := st.slotUse(others, l, "linux"); awake >= m.v.Awake {
				return nil
			}
			if m.stopping.Load() { // a slot taken now would wake a box that is being stopped
				return nil
			}
			m.st.Waking = true
			if err := m.save(); err != nil {
				m.st.Waking = false
				m.logf("its slot could not be marked taken: %v", err)
				return fmt.Errorf("its slot could not be marked taken: %w", noPath(err))
			}
			took = true
			return nil
		})
		pause, why := cloudhvAwakePoll, fmt.Sprintf("all %d boxes this host runs awake at once are awake now", m.v.Awake)
		switch {
		case took:
			return nil
		case err != nil:
			why = fmt.Sprintf("the host's awake boxes could not be counted: %v", err) // to the box's caller: with no path of this host (noPath)
		case !held:
			pause, why = 5*time.Millisecond, "the host's awake boxes could not be counted in time"
		default:
			pause *= time.Duration(1 + records/cloudhvAwakePollRecords)
		}
		if m.stopping.Load() {
			return cloudhvNoSlot{why: "the box was told to stop", counts: counts}
		}
		if held && !said {
			said = true
			m.event("slot_wait", map[string]any{"why": why})
		}
		left := time.Until(end)
		if !held && err == nil && counts == 0 { // refused for a busy lock only after it was tried for a while
			left = max(left, time.Until(began.Add(min(cloudhvAwakeWait, cloudhvAwakeLockTry))))
		}
		if left <= 0 {
			return cloudhvNoSlot{why: why, counts: counts}
		}
		// Until the next count, looking up for a stop or a command that left.
		for until := time.Now().Add(min(pause, left)); !m.stopping.Load(); {
			d := time.Until(until)
			if d <= 0 {
				break
			}
			select {
			case <-gone:
				until = time.Time{}
			case <-time.After(min(d, cloudhvAwakePoll)):
			}
		}
	}
}

// refuseWaiting answers a refused wake to the command that asked and to
// every command already waiting behind it: none of them is given a wait of
// its own. The VM naps on, and any of them may ask again.
func (m *chmon) refuseWaiting(h chHold, full cloudhvNoSlot) {
	for n := 1; ; n++ {
		h.reply <- full
		select {
		case h = <-m.holds:
		default:
			m.event("wake_refused", map[string]any{"commands": n, "counts": full.counts, "why": full.Error()})
			return
		}
	}
}

// NapSlots, Slot and WakeRefusal are napper's (layer.go).
func (l cloudhvCLI) NapSlots() int { return l.c.NapSlots }

// Slot reads the VM's own files, in the ch_dir its record was made under.
// up: its monitor runs, and it does not nap, or is being woken. gaveUp: it
// was booted under ch_nap_slots and naps, with no wake under way.
func (l cloudhvCLI) Slot(vm string, config map[string]string) (up, gaveUp bool) {
	dir := l.vmDir(vm, config)
	v, err := cloudhvLoad(dir)
	if err != nil {
		return false, false
	}
	var st cloudhvState
	b, err := os.ReadFile(filepath.Join(dir, "state.json"))
	napping := err == nil && json.Unmarshal(b, &st) == nil && st.State == "napping" && !st.Waking
	return cloudhvMonAlive(dir) && !napping, napping && v.Awake > 0
}

// vmDir is the directory of the VM a record names: under the ch_dir its
// binding gave, else under this layer's.
func (l cloudhvCLI) vmDir(vm string, config map[string]string) string {
	if d := config["ch_dir"]; d != "" {
		return filepath.Join(d, vm)
	}
	return filepath.Clean(l.dir(vm))
}

// A refused wake's words could as well be a command's own, printed in the
// guest before it exited 255. So the driver's exec asks its command to say
// a refusal out of band (WakeRefusal): it names, in the command's
// environment, a file in the VM's directory that does not exist, and the
// command's process on the host (cloudhvExec) makes that file when the wake
// was refused. Nothing in the guest can make it, and a command not asked (a
// spawn's, a port's) leaves none.
const (
	cloudhvRefusedFile = "refused."
	cloudhvRefusedEnv  = "POTLUCK_CLOUDHV_REFUSED"
)

func (l cloudhvCLI) WakeRefusal(vm string, cmd *exec.Cmd) (refused func(stderr string) (why string, ok bool)) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return func(string) (string, bool) { return "", false }
	}
	mark := filepath.Join(l.dir(vm), cloudhvRefusedFile+hex.EncodeToString(b[:]))
	cmd.Env = append(cmd.Environ(), cloudhvRefusedEnv+"="+mark)
	return func(stderr string) (string, bool) { return napsOnWhy(stderr), os.Remove(mark) == nil }
}

// napsOnWhy is the reason a refused wake gave, in what its command said.
func napsOnWhy(stderr string) string {
	_, why, _ := strings.Cut(stderr, cloudhvNapsOn+": ")
	why, _, _ = strings.Cut(why, "\n")
	if why = strings.TrimSpace(why); why == "" {
		why = "its host gave it no slot"
	}
	return why
}

// cloudhvMarkRefused leaves the mark the command was asked for, if it was.
// A mark that cannot be made leaves the command an exit of 255 with the
// refusal's words, which is still true of it.
func cloudhvMarkRefused() {
	if mark := os.Getenv(cloudhvRefusedEnv); strings.HasPrefix(filepath.Base(mark), cloudhvRefusedFile) {
		_ = os.WriteFile(mark, nil, 0o600)
	}
}
