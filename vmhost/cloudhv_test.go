package vmhost

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/sengirunners/potluck/vmhost/cloudhvguest"
)

// The cloud-hypervisor layer's tests start no VM and touch no network. The
// hypervisor is this test binary (fakeCloudHV): it serves the API calls the
// monitor makes and stands in for the guest on the vsock socket, running the
// "guest's" commands here. What stands in for the guest's memory is one
// value, made at boot, written by a snapshot and read back by a restore, and
// every command sees it as $FAKE_MEM: a command that sees the same value
// after a nap ran in the restored guest, not a fresh one. What stands in for
// the disk writes a hypervisor holds is the file "flushed", written only when
// the fake is asked to shut down: a fake that was killed leaves none.

const (
	envFakeCloudHV   = "VMHOST_TEST_FAKE_CLOUDHV"
	envCloudHVNetLog = "VMHOST_TEST_CLOUDHV_NETLOG"
)

// cloudhvTestStubs replaces the root helper's two calls for this process
// (the test, or a monitor it started): up succeeds, down is written down.
func cloudhvTestStubs() {
	cloudhvSettle = 20 * time.Millisecond
	cloudhvNetUp = func(cloudhvVM, *os.File) error { return nil }
	cloudhvNetDown = func(v cloudhvVM) {
		if p := os.Getenv(envCloudHVNetLog); p != "" {
			if f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); err == nil {
				fmt.Fprintln(f, v.Tap)
				f.Close()
			}
		}
	}
}

// fakeCloudHV is the tests' hypervisor process. It runs in the VM's directory.
func fakeCloudHV(args []string) int {
	var api, vsock, restore string
	for i := 0; i+1 < len(args); i++ {
		switch args[i] {
		case "--api-socket":
			api = strings.TrimPrefix(args[i+1], "path=")
		case "--vsock":
			for _, kv := range strings.Split(args[i+1], ",") {
				if v, ok := strings.CutPrefix(kv, "socket="); ok {
					vsock = v
				}
			}
		case "--restore":
			restore = strings.TrimPrefix(args[i+1], "source_url=file://")
		}
	}
	type memory struct{ Vsock, Mem string }
	var mu sync.Mutex
	mem, paused := memory{vsock, fmt.Sprintf("mem-%d-%d", os.Getpid(), time.Now().UnixNano())}, false
	if restore != "" { // a restored VM starts paused, with what the snapshot held
		b, err := os.ReadFile(filepath.Join(restore, "fake.json"))
		if err != nil || json.Unmarshal(b, &mem) != nil {
			fmt.Fprintln(os.Stderr, "fake hypervisor: no snapshot to restore:", err)
			return 1
		}
		paused = true
	}
	al, err := net.Listen("unix", api)
	if err != nil {
		fmt.Fprintln(os.Stderr, "fake hypervisor:", err)
		return 1
	}
	vl, err := net.Listen("unix", mem.Vsock)
	if err != nil {
		fmt.Fprintln(os.Stderr, "fake hypervisor:", err)
		return 1
	}
	time.AfterFunc(2*time.Minute, func() { os.Exit(3) }) // never outlives a test run
	mux := http.NewServeMux()
	set := func(p bool) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			mu.Lock()
			paused = p
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		}
	}
	// Files in the VM's directory are the test's switches and the fake's
	// record: "paused" is there while the VM is paused, "snapshots" has a
	// line per snapshot asked for, "fail-snapshot" makes each one fail and
	// "dirty-shutdown" makes the hypervisor exit badly when shut down.
	mux.HandleFunc("PUT /api/v1/vm.pause", func(w http.ResponseWriter, r *http.Request) {
		_ = os.WriteFile("paused", nil, 0o600)
		set(true)(w, r)
	})
	mux.HandleFunc("PUT /api/v1/vm.resume", func(w http.ResponseWriter, r *http.Request) {
		_ = os.Remove("paused")
		set(false)(w, r)
	})
	mux.HandleFunc("PUT /api/v1/vm.snapshot", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Dest string `json:"destination_url"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		p := paused
		mu.Unlock()
		dir := strings.TrimPrefix(body.Dest, "file://")
		if f, err := os.OpenFile("snapshots", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); err == nil {
			fmt.Fprintln(f, "asked")
			f.Close()
		}
		if _, err := os.Stat("fail-snapshot"); err == nil {
			http.Error(w, "the test asked for a failed snapshot", http.StatusInternalServerError)
			return
		}
		if !p || dir == body.Dest {
			http.Error(w, "a snapshot is of a paused VM, to a file:// directory", http.StatusInternalServerError)
			return
		}
		b, _ := json.Marshal(mem)
		if err := os.WriteFile(filepath.Join(dir, "fake.json"), b, 0o600); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("PUT /api/v1/vmm.shutdown", func(w http.ResponseWriter, _ *http.Request) {
		_ = os.WriteFile("flushed", nil, 0o600)
		w.WriteHeader(http.StatusNoContent)
		code := 0
		if _, err := os.Stat("dirty-shutdown"); err == nil {
			code = 1
		}
		time.AfterFunc(10*time.Millisecond, func() { os.Exit(code) })
	})
	go func() { _ = http.Serve(al, mux) }()
	mark := func(name string) error {
		f, err := os.OpenFile(name, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return err
		}
		fmt.Fprintln(f, time.Now().UnixNano())
		return f.Close()
	}
	hooks := cloudhvguest.Hooks{
		Squeeze: func() error { return mark("squeezed") },
		Woke:    func(int64) error { return mark("woke") },
		Command: func(c *exec.Cmd) { c.Env = append(os.Environ(), "FAKE_MEM="+mem.Mem) },
	}
	for {
		c, err := vl.Accept()
		if err != nil {
			return 0
		}
		go func() {
			var line []byte
			for b := make([]byte, 1); len(line) < 64; line = append(line, b[0]) {
				if _, err := c.Read(b); err != nil {
					c.Close()
					return
				}
				if b[0] == '\n' {
					break
				}
			}
			for { // a paused guest answers nothing
				mu.Lock()
				p := paused
				mu.Unlock()
				if !p {
					break
				}
				time.Sleep(2 * time.Millisecond)
			}
			if string(line) != fmt.Sprintf("CONNECT %d", cloudhvguest.Port) {
				c.Close()
				return
			}
			fmt.Fprintf(c, "OK %d\n", cloudhvguest.Port)
			cloudhvguest.Serve(c, hooks)
		}()
	}
}

type chTest struct {
	t      *testing.T
	l      cloudhvCLI
	netlog string
}

// cloudhvFixture is a layer whose hypervisor is the fake, with one image,
// img. nap is ch_nap_idle_s.
func cloudhvFixture(t *testing.T, nap int) chTest {
	t.Helper()
	dir := t.TempDir()
	for _, d := range []string{"images", "vms"} {
		if err := os.Mkdir(filepath.Join(dir, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	qemu := filepath.Join(dir, "qemu-img") // makes the last argument, as create does
	for p, body := range map[string]string{filepath.Join(dir, "images", "img.raw"): "disk", filepath.Join(dir, "fw"): "firmware",
		qemu: "#!/bin/sh\nfor a; do last=$a; done\n: > \"$last\"\n"} {
		if err := os.WriteFile(p, []byte(body), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	netlog := filepath.Join(dir, "netlog")
	t.Setenv(envFakeCloudHV, "1")
	t.Setenv(envCloudHVNetLog, netlog)
	return chTest{t, cloudhvCLI{c: cloudhvConfig{Bin: self, Firmware: filepath.Join(dir, "fw"), QemuImg: qemu,
		Images: filepath.Join(dir, "images"), Dir: filepath.Join(dir, "vms"), Net: "10.254.254.0/28", NapIdleS: nap}, state: dir}, netlog}
}

// boot clones and boots vm and waits for its guest to answer. The VM is
// destroyed when the test ends.
// made clones a VM and leaves switch files for the fake hypervisor in its
// directory, before anything of it runs.
func (c chTest) made(vm string, switches ...string) {
	c.t.Helper()
	if err := c.l.Clone("img", vm, 2, 512); err != nil {
		c.t.Fatal(err)
	}
	for _, s := range switches {
		if err := os.WriteFile(filepath.Join(c.l.dir(vm), s), nil, 0o600); err != nil {
			c.t.Fatal(err)
		}
	}
}

func (c chTest) boot(vm string, kept bool) (*exec.Cmd, io.WriteCloser) {
	c.t.Helper()
	clone, boot := c.l.Clone, c.l.Boot
	if kept {
		clone, boot = c.l.CloneKept, c.l.BootKept
	}
	if _, err := os.Stat(filepath.Join(c.l.dir(vm), "vm.json")); err != nil {
		if err := clone("img", vm, 2, 512); err != nil {
			c.t.Fatal(err)
		}
	}
	return c.start(vm, boot)
}

func (c chTest) start(vm string, boot func(string, bool, io.Writer) (*exec.Cmd, io.WriteCloser, error)) (*exec.Cmd, io.WriteCloser) {
	c.t.Helper()
	var log bytes.Buffer
	cmd, held, err := boot(vm, true, &log)
	if err != nil {
		c.t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	c.t.Cleanup(func() {
		c.l.Destroy(vm, cmd.Process.Pid)
		held.Close()
		<-done
	})
	for end := time.Now().Add(15 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if c.l.Exec(vm, nil, io.Discard, io.Discard, "true").Run() == nil {
			return cmd, held
		}
		if time.Now().After(end) {
			c.t.Fatalf("the guest never answered; monitor said: %s", log.String())
		}
	}
}

// run is one command in the guest: its output, trimmed.
func (c chTest) run(vm string, argv ...string) string {
	c.t.Helper()
	var out, errb bytes.Buffer
	if err := c.l.Exec(vm, nil, &out, &errb, argv...).Run(); err != nil {
		c.t.Fatalf("%v: %v: %s", argv, err, errb.String())
	}
	return strings.TrimSpace(out.String())
}

// hold keeps vm awake (waking it first) until the func it answers is called:
// a command that waits for its input to close.
func (c chTest) hold(vm string) func() {
	c.t.Helper()
	cmd := c.l.Exec(vm, nil, io.Discard, io.Discard, "cat")
	in, err := cmd.StdinPipe()
	if err != nil {
		c.t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		c.t.Fatal(err)
	}
	c.until("the VM is awake under a command", func() bool { st := c.state(vm); return st.State == "awake" && st.Active >= 1 })
	return func() { in.Close(); _ = cmd.Wait() }
}

func (c chTest) state(vm string) cloudhvState {
	var st cloudhvState
	if b, err := os.ReadFile(filepath.Join(c.l.dir(vm), "state.json")); err == nil {
		_ = json.Unmarshal(b, &st)
	}
	return st
}

// until waits up to 15 s for ok.
func (c chTest) until(what string, ok func() bool) {
	c.t.Helper()
	for end := time.Now().Add(15 * time.Second); !ok(); time.Sleep(20 * time.Millisecond) {
		if time.Now().After(end) {
			c.t.Fatalf("never happened: %s", what)
		}
	}
}

func (c chTest) has(vm, name string) bool {
	_, err := os.Stat(filepath.Join(c.l.dir(vm), name))
	return err == nil
}

func chGone(pid int) bool { return pid <= 1 || syscall.Kill(pid, 0) != nil }

// An idle VM naps: its memory is on disk and no hypervisor process is left.
// The next command wakes it, and runs in the same guest.
func TestCloudHVNapsWhenIdleAndWakesForACommand(t *testing.T) {
	c := cloudhvFixture(t, 1)
	c.boot("vm1", false)
	release := c.hold("vm1")
	mem := c.run("vm1", "sh", "-c", "echo $FAKE_MEM")
	hv := c.state("vm1").HypervisorPID
	if mem == "" || hv <= 1 {
		t.Fatalf("mem %q, hypervisor pid %d", mem, hv)
	}
	release()
	c.until("the idle VM naps", func() bool { return c.state("vm1").State == "napping" })
	if st := c.state("vm1"); st.Naps != 1 || st.HypervisorPID != 0 || !chGone(hv) {
		t.Errorf("napping: %+v, hypervisor %d gone %v: a napping VM has no hypervisor process", st, hv, chGone(hv))
	}
	if !c.has("vm1", "snap/fake.json") || !c.has("vm1", "squeezed") {
		t.Error("napping with no snapshot, or with a guest that was never asked to give memory back")
	}
	if !c.has("vm1", "flushed") {
		t.Error("the hypervisor was killed, not shut down: it loses the disk writes its guest had not flushed")
	}
	if c.has("vm1", "woke") {
		t.Error("the guest was told it woke before any nap")
	}
	release = c.hold("vm1") // wakes it, and keeps it awake while it is looked at
	if got := c.run("vm1", "sh", "-c", "echo $FAKE_MEM"); got != mem {
		t.Errorf("after the nap the command saw %q, want the same guest's %q", got, mem)
	}
	st := c.state("vm1")
	if st.State != "awake" || st.HypervisorPID <= 1 || st.HypervisorPID == hv || st.NappedMs <= 0 {
		t.Errorf("after the wake: %+v", st)
	}
	if !c.has("vm1", "woke") || c.has("vm1", "snap") {
		t.Error("a woken guest is told the time, and a nap is restored once: its snapshot is deleted")
	}
	ev, _ := os.ReadFile(filepath.Join(c.l.dir("vm1"), "events.jsonl"))
	if n, w := strings.Index(string(ev), `"event":"nap"`), strings.Index(string(ev), `"event":"wake"`); n < 0 || w < n {
		t.Errorf("events: %s", ev)
	}
	release()
	c.until("it naps again", func() bool { return c.state("vm1").Naps == 2 && c.state("vm1").State == "napping" })
}

// A VM with a command running is not idle, however long the command runs.
func TestCloudHVDoesNotNapUnderACommand(t *testing.T) {
	c := cloudhvFixture(t, 1)
	c.boot("vm1", false)
	release := c.hold("vm1")
	time.Sleep(1800 * time.Millisecond) // past the idle time and the settle
	if st := c.state("vm1"); st.State != "awake" || st.Naps != 0 || c.has("vm1", "squeezed") {
		t.Fatalf("napped under a running command: %+v", st)
	}
	release()
	c.until("it naps once the command ended", func() bool { return c.state("vm1").State == "napping" })
}

// ch_nap_idle_s 0: never.
func TestCloudHVNapOffByDefault(t *testing.T) {
	c := cloudhvFixture(t, 0)
	c.boot("vm1", false)
	time.Sleep(1500 * time.Millisecond)
	if st := c.state("vm1"); st.State == "napping" || st.Naps != 0 || c.has("vm1", "squeezed") {
		t.Fatalf("%+v", st)
	}
}

// A nap whose snapshot is gone cannot be woken: the command fails, the VM
// ends, and nothing of it is left.
func TestCloudHVLostSnapshotEndsTheVM(t *testing.T) {
	c := cloudhvFixture(t, 1)
	c.boot("vm1", false)
	c.until("the idle VM naps", func() bool { return c.state("vm1").State == "napping" })
	if err := os.Remove(filepath.Join(c.l.dir("vm1"), "snap", "fake.json")); err != nil {
		t.Fatal(err)
	}
	var errb bytes.Buffer
	cmd := c.l.Exec("vm1", nil, io.Discard, &errb, "true")
	if err := cmd.Run(); err == nil || cmd.ProcessState.ExitCode() != 255 {
		t.Fatalf("a command in a VM that cannot wake: %v (%s), want exit 255", err, errb.String())
	}
	c.until("the lost VM is removed", func() bool { return !c.has("vm1", "") })
}

func TestCloudHVDestroyLeavesNothing(t *testing.T) {
	c := cloudhvFixture(t, 0)
	cmd, _ := c.boot("vm1", false)
	v, _ := cloudhvLoad(c.l.dir("vm1"))
	hv := c.state("vm1").HypervisorPID
	if vs, err := c.l.List(); err != nil || len(vs) != 1 || vs[0].Name != "vm1" || !vs[0].Running {
		t.Fatalf("list: %+v, %v", vs, err)
	}
	c.l.Destroy("vm1", cmd.Process.Pid)
	c.until("its processes end", func() bool { return chGone(hv) && !cloudhvMonAlive(c.l.dir("vm1")) })
	if c.has("vm1", "") {
		t.Error("the VM's directory is still there")
	}
	if vs, _ := c.l.List(); len(vs) != 0 {
		t.Errorf("list after destroy: %+v", vs)
	}
	if b, _ := os.ReadFile(c.netlog); !strings.Contains(string(b), v.Tap) {
		t.Errorf("the VM's tap %s was not taken down (%q)", v.Tap, b)
	}
}

// The supervisor's pipe closing, or the hypervisor dying, is the VM's end.
func TestCloudHVEndsWithItsSupervisorOrHypervisor(t *testing.T) {
	c := cloudhvFixture(t, 0)
	_, held := c.boot("vm1", false)
	hv := c.state("vm1").HypervisorPID
	held.Close()
	c.until("vm1 ends when its pipe closes", func() bool { return !c.has("vm1", "") && chGone(hv) })

	c.boot("vm2", false)
	if err := syscall.Kill(c.state("vm2").HypervisorPID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	c.until("vm2 ends when its hypervisor dies", func() bool { return !c.has("vm2", "") })
}

// A kept VM's stop leaves its disk and nothing running; its next boot is a
// new guest on that disk, even when it was napping at the stop.
func TestCloudHVKeptDiskSurvivesAStop(t *testing.T) {
	c := cloudhvFixture(t, 1)
	cmd, _ := c.boot("vm1", true)
	mem := c.run("vm1", "sh", "-c", "echo $FAKE_MEM")
	c.until("it naps", func() bool { return c.state("vm1").State == "napping" })
	c.l.PowerOff("vm1", cmd.Process.Pid)
	_ = os.Remove(filepath.Join(c.l.dir("vm1"), "flushed")) // the nap's own shutdown left it
	if cloudhvMonAlive(c.l.dir("vm1")) || !c.has("vm1", "disk.qcow2") || !c.has("vm1", "vm.json") || c.has("vm1", "snap") || c.has("vm1", "state.json") {
		t.Fatal("a stopped kept VM keeps its disk and record, and nothing of the boot that ended")
	}
	if vs, _ := c.l.List(); len(vs) != 1 || vs[0].Running {
		t.Fatalf("list of a stopped VM: %+v", vs)
	}
	cmd, _ = c.start("vm1", c.l.BootKept)
	release := c.hold("vm1")
	if got := c.run("vm1", "sh", "-c", "echo $FAKE_MEM"); got == mem || got == "" {
		t.Errorf("after stop and start the guest is %q, the stopped one was %q: a stopped VM starts from its disk", got, mem)
	}
	// Stopped while awake: the hypervisor is shut down, so the disk has all of it.
	done := make(chan struct{})
	go func() { c.l.PowerOff("vm1", cmd.Process.Pid); close(done) }()
	c.until("the stop ends the command and the VM", func() bool { return !cloudhvMonAlive(c.l.dir("vm1")) })
	release()
	<-done
	if !c.has("vm1", "flushed") || !c.has("vm1", "disk.qcow2") {
		t.Error("an awake kept VM's stop shuts its hypervisor down and keeps the disk")
	}
	if b, _ := os.ReadFile(c.netlog); strings.Count(string(b), "pch") < 2 {
		t.Errorf("each stop takes the VM's tap down (%q)", b)
	}
}

// Without its fence a VM does not start.
func TestCloudHVBootNeedsTheFence(t *testing.T) {
	c := cloudhvFixture(t, 0)
	if err := c.l.Clone("img", "vm1", 2, 512); err != nil {
		t.Fatal(err)
	}
	up := cloudhvNetUp
	cloudhvNetUp = func(cloudhvVM, *os.File) error { return fmt.Errorf("sudo refused") }
	defer func() { cloudhvNetUp = up }()
	cmd, _, err := c.l.Boot("vm1", true, io.Discard)
	if err == nil || cmd != nil || !strings.Contains(err.Error(), "not started") {
		t.Fatalf("boot with no fence: cmd %v, err %v", cmd, err)
	}
	if cloudhvMonAlive(c.l.dir("vm1")) {
		t.Fatal("a monitor runs")
	}
}

// A snapshot that fails leaves the VM running, as it was, and after three
// failures in a row the monitor stops trying.
func TestCloudHVFailedSnapshotLeavesTheVMAwake(t *testing.T) {
	c := cloudhvFixture(t, 1)
	c.made("vm1", "fail-snapshot")
	c.boot("vm1", false)
	asked := func() int {
		b, _ := os.ReadFile(filepath.Join(c.l.dir("vm1"), "snapshots"))
		return strings.Count(string(b), "asked")
	}
	c.until("the monitor has tried three naps", func() bool { return asked() >= cloudhvNapRetries })
	time.Sleep(2500 * time.Millisecond) // a fourth try would come in about a second
	if n := asked(); n != cloudhvNapRetries {
		t.Errorf("%d snapshots asked for, want %d and no more", n, cloudhvNapRetries)
	}
	if st := c.state("vm1"); st.State != "awake" || st.Naps != 0 {
		t.Errorf("state %+v, want awake with no naps", st)
	}
	if c.has("vm1", "paused") {
		t.Error("the VM was left paused after a failed nap")
	}
	if got := c.run("vm1", "sh", "-c", "echo $FAKE_MEM"); got == "" {
		t.Error("no command runs after the failed naps")
	}
}

// A hypervisor that does not shut down cleanly after the snapshot may have
// lost disk writes the saved guest holds: the VM is ended, not restored.
func TestCloudHVUncleanShutdownAtANapLosesTheVM(t *testing.T) {
	c := cloudhvFixture(t, 1)
	c.made("vm1", "dirty-shutdown")
	c.boot("vm1", false)
	c.until("the VM is ended and removed", func() bool { return !c.has("vm1", "") })
}

// A VM's disk is only what it changed over its image. An image replaced or
// rewritten since is refused, at a boot and at a wake.
func TestCloudHVImageReplacedUnderAVM(t *testing.T) {
	c := cloudhvFixture(t, 1)
	raw := filepath.Join(c.l.c.Images, "img.raw")
	replace := func() {
		t.Helper()
		if err := os.WriteFile(raw+".new", []byte("another disk"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(raw+".new", raw); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.l.CloneKept("img", "kept", 2, 512); err != nil {
		t.Fatal(err)
	}
	c.boot("napper", false)
	c.until("the idle VM naps", func() bool { return c.state("napper").State == "napping" })
	replace()
	if cmd, _, err := c.l.BootKept("kept", true, io.Discard); err == nil || cmd != nil || !strings.Contains(err.Error(), "made over") {
		t.Errorf("boot over a replaced image: cmd %v, err %v", cmd, err)
	}
	var errb bytes.Buffer
	cmd := c.l.Exec("napper", nil, io.Discard, &errb, "true")
	if err := cmd.Run(); err == nil || cmd.ProcessState.ExitCode() != 255 || !strings.Contains(errb.String(), "made over") {
		t.Errorf("a wake over a replaced image: %v (%s), want exit 255 naming the image", err, errb.String())
	}
	c.until("the VM that could not wake is removed", func() bool { return !c.has("napper", "") })
}

// The helper is given the network lock and holds it until it ends, though
// the process that started it has let go.
func TestCloudHVHelperHoldsTheNetworkLock(t *testing.T) {
	dir := t.TempDir()
	helper := filepath.Join(dir, "helper")
	gate := filepath.Join(dir, "end")
	if err := os.WriteFile(helper, []byte("#!/bin/sh\nwhile [ ! -e '"+gate+"' ]; do sleep 0.05; done\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	held, locked := cloudhvNetLock(dir, time.Second)
	if !locked {
		t.Fatal("no lock")
	}
	cmd := cloudhvHelperCmd(helper, true, held, "up")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	held.Close()
	if f, free := cloudhvNetLock(dir, 100*time.Millisecond); free {
		t.Error("the lock is free while the helper runs")
	} else if f != nil {
		f.Close()
	}
	if err := os.WriteFile(gate, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	f, free := cloudhvNetLock(dir, time.Second)
	if !free {
		t.Error("the lock is still held after the helper ended")
	}
	if f != nil {
		f.Close()
	}
	if c := cloudhvHelperCmd(helper, false, nil, "down", "pchd50002"); filepath.Base(c.Path) != "sudo" || c.Args[1] != "-n" || c.Args[2] != helper {
		t.Errorf("not root: %v, want it run through sudo -n", c.Args)
	}
}

// A VM naps as its binding says at each boot. One still running under an
// older setting takes no secrets though the binding now serves them.
func TestCloudHVAVMNapsAsItsBindingSaysNow(t *testing.T) {
	c := cloudhvFixture(t, 0)
	if err := c.l.CloneKept("img", "vm1", 2, 512); err != nil {
		t.Fatal(err)
	}
	c.l.c.NapIdleS = 60
	cmd, _ := c.start("vm1", c.l.BootKept)
	if v, err := cloudhvLoad(c.l.dir("vm1")); err != nil || v.NapIdleS != 60 {
		t.Fatalf("after a boot under ch_nap_idle_s=60: %+v, %v", v, err)
	}
	now := config{VM: "cloudhv", Kind: "box", CloudHV: cloudhvConfig{Dir: c.l.c.Dir}} // the binding's key set back to 0
	r := record{Handle: "box:a/r1", OS: "linux", VM: "vm1"}
	if !boxSecretsServed(now, "linux") {
		t.Fatal("the binding serves no secrets with naps off")
	}
	if err := secretsListed(now, r, 1); code(err) != "forbidden" {
		t.Errorf("a secret for a VM that still naps: %v, want forbidden", err)
	}
	c.l.PowerOff("vm1", cmd.Process.Pid)
	c.l.c.NapIdleS = 0
	c.start("vm1", c.l.BootKept)
	if err := secretsListed(now, r, 1); err != nil {
		t.Errorf("a secret once the VM was started again with naps off: %v", err)
	}
}

// What decides that a binding which naps its boxes takes no secrets: the
// functions describe, exec, spawn and the secrets verb all ask. A nap saves a
// box's memory to the host's disk.
func TestCloudHVNappingBindingTakesNoSecrets(t *testing.T) {
	for nap, want := range map[int]bool{0: true, 60: false} {
		c := config{VM: "cloudhv", Kind: "box", CloudHV: cloudhvConfig{NapIdleS: nap}}
		if got := boxSecretsServed(c, "linux"); got != want {
			t.Errorf("ch_nap_idle_s=%d: secrets served %v, want %v", nap, got, want)
		}
		err := secretsListed(c, record{Handle: "box:a/r1", OS: "linux"}, 1)
		if (err == nil) != want || (err != nil && code(err) != "bad_request") {
			t.Errorf("ch_nap_idle_s=%d: an exec with a secret: %v", nap, err)
		}
		if err := secretsListed(c, record{Handle: "box:a/r1", OS: "linux"}, 0); err != nil {
			t.Errorf("ch_nap_idle_s=%d: an exec with no secret: %v", nap, err)
		}
	}
	if boxSecretsServed(config{VM: "tart", Kind: "box", CloudHV: cloudhvConfig{NapIdleS: 60}}, "linux") != true {
		t.Error("another layer was refused secrets for a cloudhv key")
	}
	// A runner's registration is a credential in the box's memory too.
	m := map[string]string{"vm": "cloudhv", "ch_firmware": "/fw/CLOUDHV.fd", "ch_images": "/images", "ch_nap_idle_s": "60", "runner_dir": "/opt/runner"}
	for k, v := range cfg(t) {
		m[k] = v
	}
	m["kind"] = "box"
	if _, err := parseConfig(m); err == nil || !strings.Contains(err.Error(), "runner_dir") {
		t.Errorf("ch_nap_idle_s with runner_dir: %v, want it refused", err)
	}
}

// A cancelled boot's helper runs as root and outlives the boot. A destroy
// waits for it, so the tap it was still making is removed and not left.
func TestCloudHVDestroyWaitsForABootsHelper(t *testing.T) {
	c := cloudhvFixture(t, 0)
	if err := c.l.Clone("img", "vm1", 2, 512); err != nil {
		t.Fatal(err)
	}
	dir := c.l.dir("vm1")
	helper, locked := cloudhvNetLock(dir, time.Second) // the helper, still at work
	if !locked {
		t.Fatal("no lock")
	}
	wait := cloudhvNetWait
	defer func() { cloudhvNetWait = wait }()
	cloudhvNetWait = 100 * time.Millisecond
	if _, _, err := c.l.Boot("vm1", true, io.Discard); err == nil {
		t.Fatal("a boot started while an earlier boot's helper ran")
	}
	cloudhvNetWait = 10 * time.Second
	done := make(chan struct{})
	go func() { c.l.Destroy("vm1", 0); close(done) }()
	select {
	case <-done:
		t.Fatal("destroy did not wait for the helper")
	case <-time.After(300 * time.Millisecond):
	}
	if b, _ := os.ReadFile(c.netlog); len(b) != 0 {
		t.Fatalf("the tap was removed while the helper ran: %q", b)
	}
	helper.Close() // the helper ends
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("destroy did not go on once the helper ended")
	}
	if b, _ := os.ReadFile(c.netlog); !strings.Contains(string(b), "pch") {
		t.Fatalf("the tap was not removed: %q", b)
	}
	if _, err := os.Stat(dir); err == nil {
		t.Fatal("the VM's directory is left")
	}
}

// Each VM gets its own /30, tap and context id, and the guest reads the same
// addresses out of that id.
func TestCloudHVCloneGivesEachVMItsOwnLink(t *testing.T) {
	c := cloudhvFixture(t, 0)
	c.l.c.Net = "10.254.254.0/29" // two links
	seen := map[string]bool{}
	for _, vm := range []string{"a", "b"} {
		if err := c.l.Clone("img", vm, 0, 0); err != nil {
			t.Fatal(err)
		}
		v, err := cloudhvLoad(c.l.dir(vm))
		if err != nil {
			t.Fatal(err)
		}
		ip, gw, ok := cloudhvguest.GuestAddr(v.CID())
		if !ok || ip.String() != v.GuestIP || gw.String() != v.HostIP || !cloudhvTapName.MatchString(v.Tap) {
			t.Errorf("%s: %+v; the guest would take %v via %v", vm, v, ip, gw)
		}
		if v.CPU != cloudhvCPU || v.MemoryMB != cloudhvMemory {
			t.Errorf("%s: size %d cpu, %d MiB", vm, v.CPU, v.MemoryMB)
		}
		for _, k := range []string{v.GuestIP, v.HostIP, v.Tap, v.MAC} {
			if seen[k] {
				t.Errorf("%s shares %s with another VM", vm, k)
			}
			seen[k] = true
		}
	}
	if err := c.l.Clone("img", "c", 0, 0); err == nil || !strings.Contains(err.Error(), "no free link") {
		t.Fatalf("a third VM in a range of two links: %v", err)
	}
	c.l.Destroy("a", 0)
	if err := c.l.Clone("img", "c", 0, 0); err != nil {
		t.Fatalf("after one was destroyed: %v", err)
	}
}

// A link a VM gave up is not the next one handed out.
func TestCloudHVCloneDoesNotReuseALinkAtOnce(t *testing.T) {
	c := cloudhvFixture(t, 0)
	link := func(vm string) string {
		t.Helper()
		if err := c.l.Clone("img", vm, 0, 0); err != nil {
			t.Fatal(err)
		}
		v, _ := cloudhvLoad(c.l.dir(vm))
		return v.GuestIP
	}
	a := link("a")
	c.l.Destroy("a", 0)
	if b := link("b"); b == a {
		t.Fatalf("the next VM got %s, the link a VM just left", b)
	}
}

// A box's image is pinned to the digest written beside it.
func TestCloudHVImagePin(t *testing.T) {
	c := cloudhvFixture(t, 0)
	raw := filepath.Join(c.l.c.Images, "img.raw")
	a, b := strings.Repeat("a", 64), strings.Repeat("b", 64)
	if _, err := c.l.imagePath("img@sha256:" + a); err == nil {
		t.Error("a pinned image with no digest file was taken")
	}
	if err := os.WriteFile(raw+".sha256", []byte(a+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if p, err := c.l.imagePath("img@sha256:" + a); err != nil || p != raw {
		t.Errorf("%q, %v", p, err)
	}
	if _, err := c.l.imagePath("img@sha256:" + b); err == nil {
		t.Error("an image pinned to another digest was taken")
	}
	late := time.Now().Add(time.Hour)
	if err := os.Chtimes(raw, late, late); err != nil {
		t.Fatal(err)
	}
	if _, err := c.l.imagePath("img@sha256:" + a); err == nil {
		t.Error("an image changed after its digest was written was taken")
	}
	for _, bad := range []string{"../img", "no-such", "a/b", ""} {
		if _, err := c.l.imagePath(bad); err == nil {
			t.Errorf("image %q was taken", bad)
		}
	}
}

func TestCloudHVConfig(t *testing.T) {
	first := cfg(t)
	base := func() map[string]string {
		m := map[string]string{"vm": "cloudhv", "ch_firmware": "/fw/CLOUDHV.fd", "ch_images": "/images"}
		for k, v := range first {
			m[k] = v
		}
		return m
	}
	c, err := parseConfig(base())
	if err != nil {
		t.Fatal(err)
	}
	if h := c.CloudHV; h.Bin != "cloud-hypervisor" || h.QemuImg != "qemu-img" || h.Net != cloudhvNetDefault || h.NapIdleS != 0 ||
		h.Dir != filepath.Join(c.StateDir, "cloudhv") || h.Console {
		t.Errorf("defaults: %+v", h)
	}
	if c.isolation() != "ephemeral" {
		t.Errorf("isolation %q: the layer's only network is the fenced one", c.isolation())
	}
	raw := map[string]string{}
	c.CloudHV.raw(raw)
	for k, v := range raw { // the supervisor reads the same layer back
		m := base()
		m[k] = v
		if c2, err := parseConfig(m); err != nil || c2.CloudHV != c.CloudHV {
			t.Errorf("%s=%s read back: %+v, %v", k, v, c2.CloudHV, err)
		}
	}
	for k, v := range map[string]string{"ch_firmware": "", "ch_images": "relative/dir", "ch_dir": "relative", "ch_net": "192.168.0.0/24",
		"ch_nap_idle_s": "soon", "ch_console": "yes", "network": "nat"} {
		m := base()
		m[k] = v
		if _, err := parseConfig(m); err == nil {
			t.Errorf("%s=%q was accepted", k, v)
		}
	}
	for _, n := range []string{"10.0.0.0/31", "10.1.2.0/33", "11.0.0.0/16", "fd00::/64", "ten"} {
		if _, _, err := cloudhvRange(n); err == nil {
			t.Errorf("ch_net %s was accepted", n)
		}
	}
	if b, n, err := cloudhvRange("10.213.0.0/16"); err != nil || n != 1<<14 || cloudhvIP(b) != "10.213.0.0" {
		t.Errorf("%d links from %s, %v", n, cloudhvIP(b), err)
	}
}

// The fence drops everything docs/network.md lists (the same ranges as
// hostprobe's box network), and the root helper takes
// nothing but the layer's own taps and addresses.
func TestCloudHVFenceAndHelperArguments(t *testing.T) {
	fence := cloudhvFence()
	for _, want := range []string{"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "168.63.129.16", "169.254.0.0/16", "172.16.0.0/12",
		"192.0.0.0/24", "192.168.0.0/16", "198.18.0.0/15", "224.0.0.0/4", "240.0.0.0/4",
		"meta nfproto ipv6 drop", "fib saddr . iif oif missing drop", "fib daddr type { local, broadcast } drop",
		"tcp dport { 25, 465, 587, 2525 } drop", "hook prerouting priority raw", `oifname "pch*" drop`} {
		if !strings.Contains(fence, want) {
			t.Errorf("the fence lacks %q", want)
		}
	}
	for _, good := range [][]string{{"up", "pchd50002", "1000", "10.213.0.1"}, {"down", "pchfefe02"}} {
		if _, _, _, _, err := cloudhvNetArgs(good); err != nil {
			t.Errorf("%v: %v", good, err)
		}
	}
	// Each bad case names the tap that goes with its address, so it is its
	// own fault that is refused.
	for _, bad := range [][]string{nil, {"up"}, {"down", "eth0"}, {"down", "pch0a0b02", "extra"}, {"up", "docker0", "1000", "10.213.0.1"},
		{"up", "pchd50002", "root", "10.213.0.1"}, {"up", "pcha80102", "1000", "192.168.1.1"}, {"up", "pchd50003", "1000", "10.213.0.2"},
		{"up", "pch0a0b0g", "1000", "10.213.0.1"}, {"flush", "pch0a0b02"},
		{"up", "pchd50006", "1000", "10.213.0.1"}} { // the last: another link's tap with this link's address
		if _, _, _, _, err := cloudhvNetArgs(bad); err == nil {
			t.Errorf("the helper took %v", bad)
		}
	}
	if os.Geteuid() != 0 {
		if code := cloudhvNetHelper([]string{"down", "pchfefefe"}); code == 0 {
			t.Error("the helper ran without root")
		}
	}
	// A link is refused when the host already routes its address elsewhere.
	for routes, clash := range map[string]bool{
		"default via 192.168.1.1 dev enp8s0 proto dhcp src 192.168.1.210 metric 100\n":                             false,
		"default via 192.168.1.1 dev enp8s0\n10.213.0.0/30 dev pch0a0b02 proto kernel scope link src 10.213.0.1\n": false,
		"": false,
		"default via 192.168.1.1 dev enp8s0\n10.213.0.0/24 dev incusbr0 proto kernel scope link src 10.213.0.1\n": true,
		"10.0.0.0/8 via 100.64.0.1 dev tailscale0\n":                                                              true,
		"10.213.0.0/30 dev pch0a0b06 proto kernel scope link src 10.213.0.1\n":                                    true,
	} {
		if got := cloudhvRouteClash(routes, "pch0a0b02"); (got != "") != clash {
			t.Errorf("routes %q: clash %q, want one: %v", routes, got, clash)
		}
	}
	if _, err := os.Stat(toolCmd("no-such-tool").Path); err == nil || !filepath.IsAbs(toolCmd("no-such-tool").Path) {
		t.Error("the helper looks a missing tool up somewhere it should not")
	}
	if env := toolCmd("ip").Env; len(env) == 0 {
		t.Error("the helper's tools inherit the caller's environment")
	}
}
