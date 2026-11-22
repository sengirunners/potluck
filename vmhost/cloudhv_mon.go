package vmhost

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/sengirunners/potluck/vmhost/cloudhvguest"
)

// The monitor (the driver re-executed with --cloudhv-vm) is the host process
// that is a cloud-hypervisor VM for as long as it exists. The hypervisor
// process itself comes and goes under it:
//
//	awake    a cloud-hypervisor process runs the guest
//	napping  no hypervisor process, no guest memory on the host: the guest's
//	         memory and device state are a snapshot in the VM's directory
//
// A nap is taken when no command has run in the guest for nap_idle_s. First
// the guest is asked to hand back what it is not using (file cache), so the
// snapshot holds little more than the programs' own memory; then the VM is
// paused, saved, and its hypervisor shut down (not killed: shutdown). The next command (cloudhvExec asks
// the monitor to hold the VM awake before it connects) restores it: the same
// programs, the same open connections, a clock set forward to now.
//
// One nap is restored once and deleted, so no two guests ever start from the
// same memory. Restore is always the hypervisor's copying mode: the on-demand
// mode brings the guest back holding its full size of host memory.
//
// The monitor's exit is the VM's end: the guest shut down, the hypervisor
// died, a restore failed, the supervisor's pipe closed, or it was told to
// stop. In mode "delete" it then removes the VM; in mode "keep" (a kept disk,
// persist.go) it leaves the disk and the VM's record.

// cloudhvState is state.json in the VM's directory: for List, for Destroy,
// and for whoever measures how long VMs are really awake.
type cloudhvState struct {
	State         string    `json:"state"` // booting, awake, napping
	Since         time.Time `json:"since"`
	PID           int       `json:"pid"`
	HypervisorPID int       `json:"hypervisor_pid,omitempty"`
	Naps          int       `json:"naps"`
	AwakeMs       int64     `json:"awake_ms"`
	NappedMs      int64     `json:"napped_ms"`
	Active        int       `json:"active"` // commands in flight
}

const (
	cloudhvWakeFor = 20 * time.Second // a restore that takes longer has failed
	// A shutdown: the hypervisor is asked, then given this long to write out
	// what it holds of the disk and exit.
	cloudhvShutdownAsk = 20 * time.Second
	cloudhvShutdownFor = 30 * time.Second
	// cloudhvStopFor is the longest a kept VM's monitor takes to stop it (end):
	// reach the guest, sync it, shut the hypervisor down. Whoever asked for
	// the stop waits this long before it kills anything, because a killed
	// hypervisor loses disk writes.
	cloudhvStopFor    = time.Second + syncFor + cloudhvShutdownAsk + cloudhvShutdownFor + 5*time.Second
	cloudhvNapRetries = 3 // failed naps in a row before the monitor stops trying
)

// cloudhvSettle is the wait between the squeeze and the snapshot, while the
// guest's freed pages reach the host. Tests shorten it.
var cloudhvSettle = 3 * time.Second

// chHold is a command's request to hold the VM awake. The monitor answers on
// reply; a refusal is then written to the client, and sent is closed once it
// has been, so a monitor that ends because of the refusal does not exit before
// the client is told why.
type chHold struct {
	reply chan error
	sent  chan struct{}
}

// cloudhvRefusalWait bounds the monitor's wait for a refusal to reach its client.
const cloudhvRefusalWait = 2 * time.Second

type chmon struct {
	dir, mode string
	v         cloudhvVM
	log       io.Writer

	ch      *exec.Cmd
	gen     int           // counts hypervisor processes: an exit event names the one it is about
	exited  chan int      // a hypervisor process ended
	holds   chan chHold   // a command wants the VM awake
	release chan struct{} // a command ended

	st        cloudhvState
	idleSince time.Time
	squeezed  time.Time // when the guest was last squeezed for a nap; zero: not yet
	napFails  int
	answered  bool // the guest's agent has answered at least once
}

// cloudhvMonitor is the --cloudhv-vm process.
func cloudhvMonitor(dir, mode string) int {
	// The hypervisor is told to die with the thread that started it, so the
	// one goroutine that starts it keeps its thread for good.
	runtime.LockOSThread()
	if err := os.Chdir(dir); err != nil { // sockets are named relative to it: their full paths can pass the 108-byte limit
		fmt.Fprintln(os.Stderr, "potluck-driver-tart:", err)
		return 1
	}
	v, err := cloudhvLoad(".")
	if err != nil {
		fmt.Fprintln(os.Stderr, "potluck-driver-tart:", err)
		return 1
	}
	lock, err := os.OpenFile("mon.lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		fmt.Fprintln(os.Stderr, "potluck-driver-tart:", err)
		return 1
	}
	defer lock.Close()
	// Tried for a moment: a driver asking whether a monitor runs
	// (cloudhvMonAlive) holds the lock shared for an instant.
	for end := time.Now().Add(2 * time.Second); syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(end) {
			fmt.Fprintln(os.Stderr, "potluck-driver-tart: VM", v.Name, "already has a monitor")
			return 1
		}
	}
	m := &chmon{dir: dir, mode: mode, v: v, log: os.Stderr, exited: make(chan int, 4), holds: make(chan chHold),
		release: make(chan struct{}, 64)}
	cloudhvClearRuntime(".")
	m.st = cloudhvState{State: "booting", Since: time.Now().UTC(), PID: os.Getpid()}

	stop := make(chan string, 2)
	go func() { // the supervisor's pipe: its closing ends the VM
		_, _ = io.Copy(io.Discard, os.Stdin)
		stop <- "its supervisor is gone"
	}()
	sig := make(chan os.Signal, 2)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	go func() { <-sig; stop <- "it was told to stop" }()

	ctl, err := net.Listen("unix", "ctl.sock")
	if err != nil {
		fmt.Fprintln(os.Stderr, "potluck-driver-tart:", err)
		return 1
	}
	_ = os.Chmod("ctl.sock", 0o600)
	go m.serve(ctl)

	if err := m.start(nil); err != nil {
		m.logf("could not start: %v", err)
		m.end(false)
		return 1
	}
	m.idleSince = time.Now()
	m.save()
	timer := time.NewTimer(time.Hour)
	defer timer.Stop()
	m.arm(timer)
	for {
		select {
		case why := <-stop:
			m.logf("ending: %s", why)
			m.end(m.mode == "keep")
			return 0
		case g := <-m.exited:
			if g != m.gen || m.st.State == "napping" {
				continue // a hypervisor the monitor ended itself
			}
			m.logf("the VM stopped")
			m.ch = nil // already gone: nothing to end
			m.end(false)
			return 0
		case h := <-m.holds:
			var err error
			if m.st.State == "napping" {
				if err = m.wake(); err != nil {
					m.logf("wake failed, the VM is lost: %v", err)
					h.reply <- err
					select {
					case <-h.sent:
					case <-time.After(cloudhvRefusalWait):
					}
					m.end(false)
					return 1
				}
			}
			m.st.Active++
			m.squeezed = time.Time{}
			if m.st.State == "booting" {
				m.setState("awake")
			}
			m.save()
			h.reply <- nil
		case <-m.release:
			if m.st.Active > 0 {
				m.st.Active--
			}
			if m.st.Active == 0 {
				m.idleSince = time.Now()
				m.arm(timer)
			}
			m.save()
		case <-timer.C:
			if lost := m.idle(); lost {
				m.logf("the VM is lost")
				m.end(false)
				return 1
			}
			m.arm(timer)
		}
	}
}

func (m *chmon) logf(f string, a ...any) {
	fmt.Fprintf(m.log, "potluck-driver-tart: cloudhv %s: %s\n", m.v.Name, fmt.Sprintf(f, a...))
}

// arm sets the timer for the next thing an idle VM is due: its squeeze, then
// its snapshot.
func (m *chmon) arm(t *time.Timer) {
	if !t.Stop() {
		select {
		case <-t.C:
		default:
		}
	}
	if m.v.NapIdleS <= 0 || m.st.Active > 0 || m.st.State == "napping" || m.napFails >= cloudhvNapRetries {
		return
	}
	due := m.idleSince.Add(time.Duration(m.v.NapIdleS) * time.Second)
	if !m.squeezed.IsZero() {
		due = m.squeezed.Add(cloudhvSettle)
	}
	t.Reset(max(time.Until(due), 10*time.Millisecond))
}

// idle is the timer's turn: squeeze an idle guest, then, once what it gave
// back has reached the host and nothing has run since, nap it. lost: the nap
// failed in a way that leaves no VM to go on with.
func (m *chmon) idle() (lost bool) {
	if m.v.NapIdleS <= 0 || m.st.Active > 0 || m.st.State != "awake" || m.napFails >= cloudhvNapRetries {
		return false
	}
	if time.Since(m.idleSince) < time.Duration(m.v.NapIdleS)*time.Second {
		return false
	}
	if m.squeezed.IsZero() {
		if err := m.squeeze(); err != nil {
			if !m.answered { // still booting: not a failed nap
				m.idleSince = time.Now()
				return false
			}
			m.failNap("squeeze", err)
		}
		return false
	}
	if time.Since(m.squeezed) < cloudhvSettle {
		return false
	}
	if lost, err := m.nap(); err != nil {
		m.failNap("nap", err)
		return lost
	}
	return false
}

func (m *chmon) failNap(what string, err error) {
	m.napFails++
	m.squeezed = time.Time{}
	m.idleSince = time.Now() // try again after another idle period
	m.logf("%s failed (%d of %d tries): %v", what, m.napFails, cloudhvNapRetries, err)
}

func (m *chmon) setState(s string) {
	now := time.Now().UTC()
	d := now.Sub(m.st.Since).Milliseconds()
	if m.st.State == "napping" {
		m.st.NappedMs += d
	} else {
		m.st.AwakeMs += d
	}
	m.st.State, m.st.Since = s, now
}

func (m *chmon) save() {
	st := m.st
	if m.ch != nil && m.ch.Process != nil && st.State != "napping" {
		st.HypervisorPID = m.ch.Process.Pid
	}
	b, _ := json.Marshal(st)
	if os.WriteFile("state.json.tmp", b, 0o600) == nil {
		_ = os.Rename("state.json.tmp", "state.json")
	}
}

// event appends one line to events.jsonl, the VM's record of naps and wakes.
func (m *chmon) event(kind string, fields map[string]any) {
	fields["event"], fields["at"] = kind, time.Now().UTC().Format(time.RFC3339Nano)
	b, _ := json.Marshal(fields)
	if f, err := os.OpenFile("events.jsonl", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); err == nil {
		_, _ = f.Write(append(b, '\n'))
		f.Close()
	}
}

// start runs a hypervisor process: a fresh boot, or (restore set) a restore
// of the snapshot directory.
func (m *chmon) start(restore *string) error {
	for _, s := range []string{"api.sock", "vsock.sock"} {
		_ = os.Remove(s)
	}
	abs := func(n string) string { return filepath.Join(m.dir, n) }
	a := []string{"--api-socket", "path=api.sock"}
	if restore != nil {
		a = append(a, "--restore", "source_url=file://"+abs(*restore))
	} else {
		console := []string{"--serial", "off", "--console", "off"}
		if m.v.Console {
			console = []string{"--serial", "file=" + abs("console.log"), "--console", "off"}
		}
		a = append(a,
			"--kernel", m.v.Firmware,
			"--disk", "path="+abs("disk.qcow2")+",image_type=qcow2,backing_files=on",
			"--cpus", "boot="+strconv.Itoa(m.v.CPU),
			// Memory the guest frees goes back to the host as it is freed
			// (free page reporting), so an awake VM holds what it uses.
			"--memory", "size="+strconv.Itoa(m.v.MemoryMB)+"M,shared=on",
			"--balloon", "size=0,free_page_reporting=on",
			"--net", "tap="+m.v.Tap+",mac="+m.v.MAC,
			// A relative socket name: the hypervisor runs in the VM's
			// directory, and a full path can pass a unix socket's limit.
			"--vsock", "cid="+strconv.FormatUint(uint64(m.v.CID()), 10)+",socket=vsock.sock")
		a = append(a, console...)
	}
	cmd := exec.Command(m.v.Bin, a...)
	logf, err := os.OpenFile("hypervisor.log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer logf.Close()
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.SysProcAttr = cloudhvChildAttr()
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("%s: %v", m.v.Bin, err)
	}
	m.gen++
	m.ch = cmd
	g := m.gen
	go func() { _ = cmd.Wait(); m.exited <- g }()
	return nil
}

// hypervisorSaid is the end of the hypervisor's log, for the run's log when
// the hypervisor exits: the log itself goes with the VM's directory.
func hypervisorSaid() string {
	f, err := os.Open("hypervisor.log")
	if err != nil {
		return "(no log)"
	}
	defer f.Close()
	const keep = 600
	if st, err := f.Stat(); err == nil && st.Size() > keep {
		_, _ = f.Seek(-keep, io.SeekEnd)
	}
	b, _ := io.ReadAll(io.LimitReader(f, keep))
	return strings.TrimSpace(string(b))
}

// kill ends the current hypervisor process and waits for it.
func (m *chmon) kill() {
	if m.ch == nil || m.ch.Process == nil {
		return
	}
	g := m.gen
	_ = m.ch.Process.Kill()
	for deadline := time.After(10 * time.Second); ; {
		select {
		case e := <-m.exited:
			if e == g {
				m.ch = nil
				return
			}
		case <-deadline:
			m.ch = nil
			return
		}
	}
}

// api calls the hypervisor's HTTP API on its unix socket.
func cloudhvAPI(method, path string, body any, timeout time.Duration) error {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	c := http.Client{Timeout: timeout, Transport: &http.Transport{DisableKeepAlives: true,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", "api.sock")
		}}}
	req, err := http.NewRequest(method, "http://localhost/api/v1/"+path, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := c.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(res.Body, 2000))
		return fmt.Errorf("%s: %s: %s", path, res.Status, strings.TrimSpace(string(b)))
	}
	return nil
}

// cloudhvGuest opens a connection to potluck-guest through the hypervisor's
// vsock socket in the current directory (its handshake: "CONNECT <port>").
func cloudhvGuest(timeout time.Duration) (net.Conn, error) {
	c, err := net.DialTimeout("unix", "vsock.sock", timeout)
	if err != nil {
		return nil, err
	}
	_ = c.SetDeadline(time.Now().Add(timeout))
	if _, err := fmt.Fprintf(c, "CONNECT %d\n", cloudhvguest.Port); err != nil {
		c.Close()
		return nil, err
	}
	// Byte by byte: nothing after the line may be read ahead.
	var line []byte
	for b := make([]byte, 1); len(line) < 64; {
		if _, err := c.Read(b); err != nil {
			c.Close()
			return nil, fmt.Errorf("the guest agent did not answer: %w", err)
		}
		if b[0] == '\n' {
			break
		}
		line = append(line, b[0])
	}
	if !strings.HasPrefix(string(line), "OK") {
		c.Close()
		return nil, fmt.Errorf("the guest agent did not answer: %q", line)
	}
	_ = c.SetDeadline(time.Time{})
	return c, nil
}

// guestOp runs one of the agent's own ops (not a command).
func guestOp(req cloudhvguest.Request, timeout time.Duration) error {
	c, err := cloudhvGuest(min(timeout, 2*time.Second))
	if err != nil {
		return err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(timeout))
	errb := capped{max: 2000} // what a guest says is never held without a bound
	code, err := cloudhvguest.Do(c, req, nil, io.Discard, &errb)
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("%s: %s", req.Op, strings.TrimSpace(errb.String()))
	}
	return nil
}

func (m *chmon) squeeze() error {
	if err := guestOp(cloudhvguest.Request{Op: cloudhvguest.OpSqueeze}, 30*time.Second); err != nil {
		return err
	}
	m.squeezed, m.answered = time.Now(), true
	return nil
}

// shutdown asks the hypervisor to exit and waits for it. A hypervisor holds
// disk writes its guest has not asked it to flush, and writes them out when
// it exits; killed, it loses them. clean is false when it had to be killed or
// did not exit well.
func (m *chmon) shutdown() (clean bool) {
	if m.ch == nil || m.ch.Process == nil {
		return false
	}
	g, cmd := m.gen, m.ch
	if err := cloudhvAPI("PUT", "vmm.shutdown", nil, cloudhvShutdownAsk); err != nil {
		m.logf("the hypervisor refused to shut down: %v", err)
		m.kill()
		return false
	}
	for deadline := time.After(cloudhvShutdownFor); ; {
		select {
		case e := <-m.exited:
			if e == g {
				m.ch = nil
				return cmd.ProcessState != nil && cmd.ProcessState.Success()
			}
		case <-deadline:
			m.kill()
			return false
		}
	}
}

// nap saves the paused VM and ends its process. An error with lost false
// leaves the VM awake, as it was. lost: the VM can neither go on nor be
// restored, and the monitor ends it.
func (m *chmon) nap() (lost bool, err error) {
	t0 := time.Now()
	if err := cloudhvAPI("PUT", "vm.pause", nil, 10*time.Second); err != nil {
		// The call may have timed out here and still paused the VM: resume,
		// which a VM that is still running refuses, and that is fine.
		_ = cloudhvAPI("PUT", "vm.resume", nil, 10*time.Second)
		return false, err
	}
	back := func(why error) (bool, error) { // resume: the nap is off
		_ = os.RemoveAll("snap.tmp")
		_ = os.RemoveAll("snap")
		if rerr := cloudhvAPI("PUT", "vm.resume", nil, 10*time.Second); rerr != nil {
			return true, fmt.Errorf("%v, and the paused VM could not be resumed: %v", why, rerr)
		}
		return false, why
	}
	_ = os.RemoveAll("snap.tmp")
	if err := os.Mkdir("snap.tmp", 0o700); err != nil {
		return back(err)
	}
	tp := time.Now()
	if err := cloudhvAPI("PUT", "vm.snapshot", map[string]string{"destination_url": "file://" + filepath.Join(m.dir, "snap.tmp")}, 5*time.Minute); err != nil {
		return back(err)
	}
	if err := os.Rename("snap.tmp", "snap"); err != nil {
		return back(err)
	}
	ts := time.Now()
	m.setState("napping") // from here the hypervisor's exit is the monitor's own doing
	// Shut down, never killed: the saved guest believes every write it made
	// is on its disk, and a killed hypervisor loses the ones the guest had
	// not flushed (measured: a file written just before a nap read back
	// different after the wake).
	if !m.shutdown() {
		_ = os.RemoveAll("snap")
		return true, errors.New("the hypervisor did not shut down cleanly after the snapshot, so the disk may lack writes the saved guest holds: the nap cannot be restored")
	}
	m.st.Naps++
	m.napFails = 0
	m.squeezed = time.Time{}
	m.save()
	m.event("nap", map[string]any{"pause_ms": tp.Sub(t0).Milliseconds(), "snapshot_ms": ts.Sub(tp).Milliseconds(),
		"total_ms": time.Since(t0).Milliseconds(), "snapshot_bytes": diskUsed("snap"), "idle_s": int(t0.Sub(m.idleSince).Seconds())})
	return false, nil
}

// wake restores the VM from its nap.
func (m *chmon) wake() error {
	t0 := time.Now()
	napped := t0.Sub(m.st.Since)
	if err := m.v.imageSame(); err != nil {
		return err
	}
	snap := "snap"
	if err := m.start(&snap); err != nil {
		return err
	}
	g := m.gen
	deadline := time.Now().Add(cloudhvWakeFor)
	var err error
	for {
		// One call waits out the restore: a hypervisor answers resume when
		// the restore is done. A call given up early would still be carried
		// out, and every later one refused as "already running". The loop
		// is for the moments before the API socket is there.
		if err = cloudhvAPI("PUT", "vm.resume", nil, max(time.Until(deadline), time.Second)); err == nil {
			break
		}
		select {
		case e := <-m.exited:
			if e == g {
				m.ch = nil
				// The answer goes to whoever woke the box; what the
				// hypervisor said stays in the run's log, on this machine.
				m.logf("the hypervisor exited during the restore; its log ends: %s", hypervisorSaid())
				return fmt.Errorf("the hypervisor exited during the restore: %v", err)
			}
		default:
		}
		if time.Now().After(deadline) {
			m.kill()
			return fmt.Errorf("the restore did not finish in %s: %v", cloudhvWakeFor, err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	tr := time.Now()
	// The guest's clock stood still. A guest that cannot be told is still
	// awake, with a clock that is behind: said, not fatal.
	var werr error
	for end := time.Now().Add(3 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if werr = guestOp(cloudhvguest.Request{Op: cloudhvguest.OpWoke, UnixNano: time.Now().UnixNano()}, 2*time.Second); werr == nil || time.Now().After(end) {
			break
		}
	}
	if werr != nil {
		m.logf("woke, but the guest's clock was not set: %v", werr)
	}
	_ = os.RemoveAll("snap")
	m.setState("awake")
	m.save()
	m.event("wake", map[string]any{"restore_ms": tr.Sub(t0).Milliseconds(), "total_ms": time.Since(t0).Milliseconds(), "napped_s": int(napped.Seconds())})
	return nil
}

// diskUsed is the bytes a directory's files really take (a snapshot's memory
// file is sparse).
func diskUsed(dir string) int64 {
	var n int64
	_ = filepath.Walk(dir, func(_ string, fi os.FileInfo, err error) error {
		if err == nil && fi.Mode().IsRegular() {
			if st, ok := fi.Sys().(*syscall.Stat_t); ok {
				n += st.Blocks * 512
			}
		}
		return nil
	})
	return n
}

// end stops the VM for good. keep leaves its disk and record (a kept box,
// asked to stop): the guest's files are synced first when it is awake, and
// the hypervisor shut down so that what it holds of the disk is written.
func (m *chmon) end(keep bool) {
	if keep && m.st.State != "napping" && m.ch != nil {
		c, err := cloudhvGuest(time.Second)
		if err == nil {
			_ = c.SetDeadline(time.Now().Add(syncFor))
			_, _ = cloudhvguest.Do(c, cloudhvguest.Request{Op: cloudhvguest.OpExec, Argv: []string{"sync"}}, nil, io.Discard, io.Discard)
			c.Close()
		}
		m.shutdown()
	}
	m.kill()
	m.setState("ended")
	cloudhvNetDown(m.v)
	if keep || m.mode == "keep" {
		cloudhvClearRuntime(".")
		return
	}
	_ = os.RemoveAll(m.dir)
}

// serve answers the monitor's control socket. A client writes the line
// "hold": the VM is woken if it naps, the client is answered "ok", and the VM
// stays awake until that connection closes.
func (m *chmon) serve(l net.Listener) {
	for {
		c, err := l.Accept()
		if err != nil {
			return
		}
		go func() {
			defer c.Close()
			if line, _ := bufio.NewReader(c).ReadString('\n'); strings.TrimSpace(line) != "hold" {
				return
			}
			h := chHold{make(chan error, 1), make(chan struct{})}
			m.holds <- h
			if err := <-h.reply; err != nil {
				fmt.Fprintf(c, "err %v\n", err)
				close(h.sent)
				return
			}
			fmt.Fprintln(c, "ok")
			_, _ = io.Copy(io.Discard, c) // until the command's client goes
			m.release <- struct{}{}
		}()
	}
}

// cloudhvExec is the --cloudhv-exec process: one command in the guest. Its
// exit code is the command's; 255 when the VM could not be reached.
func cloudhvExec(dir string, argv []string) int {
	fail := func(f string, a ...any) int {
		fmt.Fprintf(os.Stderr, "potluck-driver-tart: "+f+"\n", a...)
		return 255
	}
	if err := os.Chdir(dir); err != nil {
		return fail("the VM is gone: %v", err)
	}
	v, err := cloudhvLoad(".")
	if err != nil {
		return fail("the VM is gone: %v", err)
	}
	ctl, err := net.DialTimeout("unix", "ctl.sock", 2*time.Second)
	if err != nil {
		return fail("the VM is not running")
	}
	defer ctl.Close()
	_ = ctl.SetDeadline(time.Now().Add(cloudhvWakeFor + 10*time.Second))
	if _, err := io.WriteString(ctl, "hold\n"); err != nil {
		return fail("the VM is not running: %v", err)
	}
	line, err := bufio.NewReader(ctl).ReadString('\n')
	if err != nil || strings.TrimSpace(line) != "ok" {
		return fail("the VM could not be woken: %s%v", strings.TrimSpace(line), errOrNothing(err))
	}
	_ = ctl.SetDeadline(time.Time{})
	g, err := cloudhvGuest(2 * time.Second)
	if err != nil {
		return fail("%v", err)
	}
	defer g.Close()
	// Root and Docker are the VM's record's, which only the host writes.
	code, err := cloudhvguest.Do(g, cloudhvguest.Request{Op: cloudhvguest.OpExec, Argv: argv, Root: v.Root, Docker: v.Docker}, os.Stdin, os.Stdout, os.Stderr)
	if err != nil {
		return fail("%v", err)
	}
	return code
}

func errOrNothing(err error) string {
	if err == nil {
		return ""
	}
	return " " + err.Error()
}
