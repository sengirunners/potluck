package vmhost

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/sengirunners/potluck/protocol"
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
	envFakeCloudHV = "VMHOST_TEST_FAKE_CLOUDHV"
	// envCloudHVAwakeWait: how long a monitor a test starts waits for a
	// slot at a wake, where the test's 200 ms is too short to tell by.
	envCloudHVAwakeWait = "VMHOST_TEST_CLOUDHV_AWAKE_WAIT"
	envCloudHVNetLog    = "VMHOST_TEST_CLOUDHV_NETLOG"
	// envCloudHVMem: the directory that stands in for the host's memory
	// (cloudhvMemRoot). Unset or missing, the host has none.
	envCloudHVMem = "VMHOST_TEST_CLOUDHV_MEM"
	// envCloudHVNapRest shortens a monitor's rest after its failed naps.
	envCloudHVNapRest = "VMHOST_TEST_CLOUDHV_NAP_REST"
	// envCloudHVNoCore: "fails", a monitor cannot set its core limit;
	// "set", it is taken as set (the test's own hard limit is 0, so no
	// process it starts can have 1).
	envCloudHVNoCore = "VMHOST_TEST_CLOUDHV_NO_CORE"
)

// cloudhvTestStubs replaces the root helper's two calls for this process
// (the test, or a monitor it started): up succeeds, down is written down.
func cloudhvTestStubs() {
	cloudhvSettle = 20 * time.Millisecond
	cloudhvAwakeWait = 200 * time.Millisecond
	if d, err := time.ParseDuration(os.Getenv(envCloudHVAwakeWait)); err == nil {
		cloudhvAwakeWait = d
	}
	if d, err := time.ParseDuration(os.Getenv(envCloudHVNapRest)); err == nil {
		cloudhvNapRest = d
	}
	switch os.Getenv(envCloudHVNoCore) {
	case "fails":
		cloudhvNoCore = func() error { return errors.New("the test's host allows no core limit of 1 byte") }
	case "set":
		cloudhvNoCore = func() error { return nil }
	}
	cloudhvNetUp = func(cloudhvVM, *os.File) error { return nil }
	cloudhvTapAddr = fakeTapAddr
	cloudhvMemRoot = func() (string, error) {
		d := os.Getenv(envCloudHVMem)
		if fi, err := os.Stat(d); err != nil || !fi.IsDir() {
			return "", errors.New("the test's host has no memory filesystem")
		}
		return d, nil
	}
	cloudhvNetDown = func(v cloudhvVM) {
		if p := os.Getenv(envCloudHVNetLog); p != "" {
			if f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); err == nil {
				fmt.Fprintln(f, v.Tap)
				f.Close()
			}
		}
	}
}

// fakeTapAddr is the hardware address of the tests' tap, which is no device:
// its own (cloudhvTapMAC), or what the file "tap-address" in the VM's
// directory says, for a test in which something changed it. A monitor and
// its hypervisor both run in the VM's directory.
func fakeTapAddr(tap string) (string, error) {
	if b, err := os.ReadFile("tap-address"); err == nil {
		return strings.TrimSpace(string(b)), nil
	}
	return cloudhvTapMAC(tap), nil
}

// fakeRan is the file the tests' hypervisor leaves in a VM's directory when
// it is run: what it was last told, an argument to a line.
const fakeRan = "hypervisor-ran"

// fakeCloudHV is the tests' hypervisor process. It runs in the VM's directory.
// As the real one does, it takes for the tap's address the one it is told
// (host_mac), or told none the one the tap has just then, which on a real
// host is not always the one the tap has later: the fake takes one no tap
// has. It exits when the tap does not have the address it was told, and at
// a restore when the tap does not have the one it took, since it may not
// set it. It exits when it is told to write a log or a console file itself.
func fakeCloudHV(args []string) int {
	_ = os.WriteFile(fakeRan, []byte(strings.Join(args, "\n")+"\n"), 0o600)
	var api, vsock, restore, tap, hostMAC string
	for i := 0; i+1 < len(args); i++ {
		switch args[i] {
		case "--net":
			for _, kv := range strings.Split(args[i+1], ",") {
				if v, ok := strings.CutPrefix(kv, "tap="); ok {
					tap = v
				}
				if v, ok := strings.CutPrefix(kv, "host_mac="); ok {
					hostMAC = v
				}
			}
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
		case "--serial", "--console", "--log-file":
			// A file the hypervisor writes itself has no limit.
			if args[i] == "--log-file" || strings.Contains(args[i+1], "file=") {
				fmt.Fprintln(os.Stderr, "the tests' hypervisor is told to write a file itself:", args[i], args[i+1])
				return 1
			}
		}
	}
	type memory struct{ Vsock, Mem, Tap, HostMAC string }
	var mu sync.Mutex
	told := hostMAC != ""
	if !told {
		hostMAC = "06:00:00:00:00:01"
	}
	mem, paused := memory{vsock, fmt.Sprintf("mem-%d-%d", os.Getpid(), time.Now().UnixNano()), tap, hostMAC}, false
	if restore != "" { // a restored VM starts paused, with what the snapshot held
		b, err := os.ReadFile(filepath.Join(restore, "fake.json"))
		if err != nil || json.Unmarshal(b, &mem) != nil {
			fmt.Fprintln(os.Stderr, "fake hypervisor: no snapshot to restore:", err)
			return 1
		}
		paused = true
	}
	if has, _ := fakeTapAddr(mem.Tap); has != mem.HostMAC && (told || restore != "") {
		fmt.Fprintln(os.Stderr, "fake hypervisor: TapSetMac(IoctlError(35108, Os { code: 1, kind: PermissionDenied }))")
		return 1
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
	// record: "paused" is there while the VM is paused, "pauses" has a line
	// per pause and "snapshots" one per snapshot asked for, "fail-snapshot" makes each one fail
	// with the memory already written, "odd-snapshot" leaves a link beside
	// the memory, "dirty-shutdown" makes the hypervisor exit badly when shut down, and
	// a restore does not finish while "slow-restore" is there, nor a
	// shutdown while "slow-shutdown" is.
	mux.HandleFunc("PUT /api/v1/vm.pause", func(w http.ResponseWriter, r *http.Request) {
		_ = os.WriteFile("paused", nil, 0o600)
		if f, err := os.OpenFile("pauses", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); err == nil {
			fmt.Fprintln(f, "paused")
			f.Close()
		}
		set(true)(w, r)
	})
	mux.HandleFunc("PUT /api/v1/vm.resume", func(w http.ResponseWriter, r *http.Request) {
		for end := time.Now().Add(10 * time.Second); restore != "" && time.Now().Before(end); time.Sleep(5 * time.Millisecond) {
			if _, err := os.Stat("slow-restore"); err != nil {
				break
			}
		}
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
		if !p || dir == body.Dest {
			http.Error(w, "a snapshot is of a paused VM, to a file:// directory", http.StatusInternalServerError)
			return
		}
		b, _ := json.Marshal(mem)
		if err := os.WriteFile(filepath.Join(dir, "fake.json"), b, 0o600); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if _, err := os.Stat("fail-snapshot"); err == nil {
			http.Error(w, "the test asked for a failed snapshot", http.StatusInternalServerError)
			return
		}
		if _, err := os.Stat("odd-snapshot"); err == nil {
			_ = os.Symlink("fake.json", filepath.Join(dir, "link"))
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
		go func() {
			for end := time.Now().Add(10 * time.Second); time.Now().Before(end); time.Sleep(5 * time.Millisecond) {
				if _, err := os.Stat("slow-shutdown"); err != nil {
					break
				}
			}
			time.Sleep(10 * time.Millisecond)
			os.Exit(code)
		}()
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
		Squeeze: func() error {
			// A test's switch: a squeeze that follows another closely fails,
			// which is the one just before a pause and no other.
			if b, err := os.ReadFile("squeezed"); err == nil && isFile("sync-fails") {
				f := strings.Fields(string(b))
				if last, _ := strconv.ParseInt(f[len(f)-1], 10, 64); time.Since(time.Unix(0, last)) < 800*time.Millisecond {
					return fmt.Errorf("the fake guest's files are not written out")
				}
			}
			return mark("squeezed")
		},
		Woke:    func(int64) error { return mark("woke") },
		Command: func(c *exec.Cmd) { c.Env = append(os.Environ(), "FAKE_MEM="+mem.Mem) },
		Root:    func() error { return mark("root") },
		Docker:  func() error { return mark("docker") },
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
	if err := os.Mkdir(filepath.Join(dir, "mem"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv(envCloudHVMem, filepath.Join(dir, "mem"))
	if !coreLimitCanBeOne() {
		t.Setenv(envCloudHVNoCore, "set")
	}
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

// coreLimitCanBeOne says whether a process this test starts can set its core
// limit to 1 byte: not when the test's own hard limit is 0.
func coreLimitCanBeOne() bool {
	var l syscall.Rlimit
	return syscall.Getrlimit(syscall.RLIMIT_CORE, &l) != nil || l.Max >= 1
}

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
	ran := time.Now().UnixNano()
	release()
	c.until("the idle VM naps", func() bool { return c.state("vm1").State == "napping" })
	// Its guest wrote its files out twice: when it was squeezed, and again
	// just before the pause, since a stop may not be able to wake it.
	b, _ := os.ReadFile(filepath.Join(c.l.dir("vm1"), "squeezed"))
	syncs := 0
	for _, at := range strings.Fields(string(b)) {
		if n, _ := strconv.ParseInt(at, 10, 64); n > ran {
			syncs++
		}
	}
	if syncs != 2 {
		t.Errorf("the guest was asked to write its files out %d times between its last command and its nap, want 2", syncs)
	}
	if st := c.state("vm1"); st.Naps != 1 || st.HypervisorPID != 0 || !chGone(hv) {
		t.Errorf("napping: %+v, hypervisor %d gone %v: a napping VM has no hypervisor process", st, hv, chGone(hv))
	}
	if !c.has("vm1", "snap/sealed") || !c.has("vm1", "squeezed") {
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
	if err := os.Remove(filepath.Join(c.l.dir("vm1"), "snap", cloudhvSealFile)); err != nil {
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

	// Also when another process still holds the hypervisor's output open.
	if !hypervisorStreams() {
		return
	}
	c.boot("vm3", false)
	holder, err := strconv.Atoi(c.run("vm3", "sh", "-c", "setsid sleep 300 > /proc/$PPID/fd/2 2>&1 < /dev/null & echo $!"))
	if err != nil || syscall.Kill(holder, 0) != nil {
		t.Fatalf("no process holds the hypervisor's output: %v", err)
	}
	t.Cleanup(func() { _ = syscall.Kill(holder, syscall.SIGKILL) })
	if err := syscall.Kill(c.state("vm3").HypervisorPID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	c.until("vm3 ends when its hypervisor dies with its output still held", func() bool { return !c.has("vm3", "") })
	if syscall.Kill(holder, 0) != nil {
		t.Error("the process that held the hypervisor's output had gone: nothing was shown")
	}
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

// The address of the host's end of a link is made from the tap's name: it is
// one an owner may set, it is no group address, it is not the guest's, and
// two taps do not share one.
func TestCloudHVTapAddress(t *testing.T) {
	a, b := cloudhvTapMAC(cloudhvTapOf(0x0afefe02)), cloudhvTapMAC(cloudhvTapOf(0x0afefe06))
	if a != "02:70:63:fe:fe:02" || b != "02:70:63:fe:fe:06" {
		t.Fatalf("addresses %q and %q", a, b)
	}
	for _, m := range []string{a, b} {
		hw, err := net.ParseMAC(m)
		if err != nil || hw[0]&3 != 2 {
			t.Errorf("%s: %v: want a locally administered address of one device", m, err)
		}
		if strings.HasPrefix(m, "52:54:00") {
			t.Errorf("%s could be a guest's", m)
		}
	}
	for _, tap := range []string{"", "pch", "eth0", "pchzzzzzz", "pch0000001"} {
		if m := cloudhvTapMAC(tap); m != "" {
			t.Errorf("%q, no tap of the layer, has the address %q", tap, m)
		}
	}
}

// A VM is not started on a tap that does not have its own address, and the
// monitor says so: the hypervisor is never run.
func TestCloudHVAVMWhoseTapHasAnotherAddressIsNotStarted(t *testing.T) {
	c := cloudhvFixture(t, 0)
	if err := c.l.CloneKept("img", "vm1", 2, 512); err != nil { // kept: its directory is still there to look in
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(c.l.dir("vm1"), "tap-address"), []byte("ee:00:00:00:00:01\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var log bytes.Buffer
	cmd, held, err := c.l.BootKept("vm1", true, &log)
	if err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	held.Close()
	defer c.l.Destroy("vm1", 0)
	if said := log.String(); !strings.Contains(said, "has the hardware address ee:00:00:00:00:01, not its own 02:70:63:") {
		t.Fatalf("the monitor said %q", said)
	}
	if !c.has("vm1", "vm.json") || c.has("vm1", fakeRan) {
		t.Fatalf("the VM's directory is there: %v; the hypervisor was run: %v", c.has("vm1", "vm.json"), c.has("vm1", fakeRan))
	}
}

// A napping VM whose tap's address was changed is not woken, and its command
// is told why.
func TestCloudHVANappingVMWhoseTapAddressChangedIsNotWoken(t *testing.T) {
	c := cloudhvFixture(t, 1)
	c.boot("vm1", false)
	c.until("vm1 naps", func() bool { return c.state("vm1").State == "napping" })
	if err := os.WriteFile(filepath.Join(c.l.dir("vm1"), "tap-address"), []byte("ee:00:00:00:00:01\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, out, said := c.ask("vm1"); code == 0 || out != "" || !strings.Contains(said, "has the hardware address ee:00:00:00:00:01") {
		t.Fatalf("a command for it: exit %d, printed %q, said %q", code, out, said)
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

// A snapshot that fails, or one that cannot be sealed, leaves the VM
// running, as it was, with nothing of its memory saved: not on disk, and not
// what the hypervisor had written into the host's memory by then. After
// three failures in a row the monitor stops trying.
func TestCloudHVFailedSnapshotLeavesTheVMAwake(t *testing.T) {
	for _, how := range []string{"fail-snapshot", "odd-snapshot"} {
		t.Run(how, func(t *testing.T) { cloudhvFailedNap(t, how) })
	}
}

func cloudhvFailedNap(t *testing.T, how string) {
	c := cloudhvFixture(t, 1)
	c.made("vm1", how)
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
	got := c.run("vm1", "sh", "-c", "echo $FAKE_MEM")
	if got == "" {
		t.Fatal("no command runs after the failed naps")
	}
	if where := holds(t, c.l.state, got); len(where) != 0 || c.has("vm1", "snap") || c.has("vm1", "snap.tmp") {
		t.Errorf("after the failed naps the guest's memory is in the clear in %v; snap %v, snap.tmp %v", where, c.has("vm1", "snap"), c.has("vm1", "snap.tmp"))
	}
	if memDir, err := cloudhvMemDir(c.l.dir("vm1")); err != nil {
		t.Error(err)
	} else if _, err := os.Stat(memDir); err == nil {
		t.Error("what the failed snapshot wrote is still in the host's memory")
	}
}

// After three failed naps the monitor leaves the VM be for a while, not for
// good: once what failed is over and the rest is too, the VM naps.
func TestCloudHVNapsAreTriedAgainAfterARest(t *testing.T) {
	t.Setenv(envCloudHVNapRest, "3s")
	c := cloudhvFixture(t, 1)
	c.made("vm1", "fail-snapshot")
	c.boot("vm1", false)
	mem := c.run("vm1", "sh", "-c", "echo $FAKE_MEM")
	asked := func() int {
		b, _ := os.ReadFile(filepath.Join(c.l.dir("vm1"), "snapshots"))
		return strings.Count(string(b), "asked")
	}
	c.until("the monitor has tried three naps", func() bool { return asked() >= cloudhvNapRetries })
	if err := os.Remove(filepath.Join(c.l.dir("vm1"), "fail-snapshot")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Second) // without the rest a fourth try comes in about a second, and naps
	if st := c.state("vm1"); asked() != cloudhvNapRetries || st.State != "awake" {
		t.Fatalf("within the rest: %d snapshots asked for, %+v", asked(), st)
	}
	c.until("the VM naps once the rest is over", func() bool { return c.state("vm1").State == "napping" })
	if got := c.run("vm1", "sh", "-c", "echo $FAKE_MEM"); got != mem {
		t.Fatalf("after the wake the guest's memory is %q, was %q", got, mem)
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

// A VM naps as its binding says at each boot, and its monitor says in the
// VM's state that it seals what a nap saves. A VM that naps and whose state
// does not say so, as one whose monitor is of a driver that saved naps in
// the clear, takes no secrets until it is started again. The VM's record
// cannot say it for the monitor.
func TestCloudHVAVMNapsAsItsBindingSaysNow(t *testing.T) {
	c := cloudhvFixture(t, 0)
	if err := c.l.CloneKept("img", "vm1", 2, 512); err != nil {
		t.Fatal(err)
	}
	// A VM that does not nap saves no memory, whatever its monitor says:
	// one made without naps, with the state file a driver that never
	// sealed a nap wrote.
	if err := c.l.CloneKept("img", "vm2", 2, 512); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(c.l.dir("vm2"), "state.json"), []byte(`{"state":"awake"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	noNaps := config{VM: "cloudhv", Kind: "box", CloudHV: cloudhvConfig{Dir: c.l.c.Dir}}
	r2 := record{Handle: "box:a/r2", OS: "linux", VM: "vm2"}
	if v, err := cloudhvLoad(c.l.dir("vm2")); err != nil || v.NapIdleS != 0 {
		t.Fatalf("a VM made without naps: %+v, %v", v, err)
	}
	if err := secretsListed(noNaps, r2, 1); err != nil {
		t.Errorf("a secret for a VM that does not nap: %v", err)
	}
	if err := napsAnyway(noNaps, r2); err != nil {
		t.Errorf("the secrets verb's check for a VM that does not nap: %v", err)
	}
	c.l.c.NapIdleS = 60
	cmd, _ := c.start("vm1", c.l.BootKept)
	if v, err := cloudhvLoad(c.l.dir("vm1")); err != nil || v.NapIdleS != 60 {
		t.Fatalf("after a boot under ch_nap_idle_s=60: %+v, %v", v, err)
	}
	c.until("the monitor is at rest", func() bool { st := c.state("vm1"); return st.State == "awake" && st.Active == 0 })
	if !c.state("vm1").NapSealed {
		t.Fatalf("the monitor does not say it seals naps: %+v", c.state("vm1"))
	}
	now := config{VM: "cloudhv", Kind: "box", CloudHV: cloudhvConfig{Dir: c.l.c.Dir, NapIdleS: 60}}
	r := record{Handle: "box:a/r1", OS: "linux", VM: "vm1", State: stReady}
	if err := secretsListed(now, r, 1); err != nil {
		t.Errorf("a secret for a VM whose naps are sealed: %v", err)
	}
	if why := (&drv{cfg: now}).warmOffBinding(r); why != "" {
		t.Errorf("a warm box whose naps are sealed is not handed out: %s", why)
	}
	refused := func(what string) {
		t.Helper()
		// Warm, such a box is never handed out: it could not be started
		// again, and would take no secrets for as long as it lived.
		if why := (&drv{cfg: now}).warmOffBinding(r); why == "" {
			t.Errorf("a warm box that is %s would be handed out", what)
		}
		if err := secretsListed(now, r, 1); code(err) != "forbidden" {
			t.Errorf("an exec's secret for %s: %v, want forbidden", what, err)
		}
		if err := napsAnyway(now, r); code(err) != "forbidden" {
			t.Errorf("the secrets verb's check for %s: %v, want forbidden", what, err)
		}
	}
	state, rec := filepath.Join(c.l.dir("vm1"), "state.json"), filepath.Join(c.l.dir("vm1"), "vm.json")
	st := c.state("vm1")
	st.NapSealed = false // as the monitor of a driver that saved naps in the clear writes it
	b, _ := json.Marshal(st)
	if err := os.WriteFile(state, b, 0o600); err != nil {
		t.Fatal(err)
	}
	refused("a VM whose monitor does not say it seals naps")
	// A record that says so, as a driver that seals left it before such a
	// monitor was started, does not stand in for the monitor.
	var raw map[string]any
	b, _ = os.ReadFile(rec)
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	raw["nap_sealed"] = true
	b, _ = json.Marshal(raw)
	if err := os.WriteFile(rec, b, 0o600); err != nil {
		t.Fatal(err)
	}
	refused("a VM whose record alone says its naps are sealed")
	if err := os.Remove(state); err != nil {
		t.Fatal(err)
	}
	refused("a VM whose state cannot be read")
	// A warm box still starting has no monitor to say so yet: it is not
	// ended for it.
	starting := r
	starting.State = stStarting
	if why := (&drv{cfg: now}).warmOffBinding(starting); why != "" {
		t.Errorf("a warm box still starting, its state not yet written, is ended: %s", why)
	}
	// A box with no VM of this layer has no nap to ask about.
	if err := secretsListed(noNaps, record{Handle: "box:a/r3", OS: "linux", VM: "none"}, 1); err != nil {
		t.Errorf("a secret for a VM with no record: %v", err)
	}
	c.l.PowerOff("vm1", cmd.Process.Pid)
	c.start("vm1", c.l.BootKept)
	if err := secretsListed(now, r, 1); err != nil {
		t.Errorf("a secret once the VM was started again: %v", err)
	}
}

// A binding that naps its boxes serves secrets as one that does not: a nap's
// saved memory reaches the disk sealed. And it may have a runner_dir.
func TestCloudHVANappingBindingServesSecrets(t *testing.T) {
	for _, nap := range []int{0, 60} {
		c := config{VM: "cloudhv", Kind: "box", CloudHV: cloudhvConfig{NapIdleS: nap}}
		if !boxSecretsServed("linux") {
			t.Errorf("ch_nap_idle_s=%d: no secrets served", nap)
		}
		for _, n := range []int{0, 1} {
			if err := secretsListed(c, record{Handle: "box:a/r1", OS: "linux"}, n); err != nil {
				t.Errorf("ch_nap_idle_s=%d: an exec with %d secrets: %v", nap, n, err)
			}
		}
	}
	m := map[string]string{"vm": "cloudhv", "ch_firmware": "/fw/CLOUDHV.fd", "ch_images": "/images", "ch_nap_idle_s": "60", "runner_dir": "/opt/runner"}
	for k, v := range cfg(t) {
		m[k] = v
	}
	m["kind"], m["size"], m["cpu"], m["memory"] = "box", "small", "1", "512"
	m["images"] = "fake/linux@sha256:" + strings.Repeat("ab", 32)
	if _, err := parseConfig(m); err != nil {
		t.Errorf("ch_nap_idle_s with runner_dir: %v", err)
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
		h.Dir != filepath.Join(c.StateDir, "cloudhv") || h.Console || h.Limits != (cloudhvLimits{}) {
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
		"ch_nap_idle_s": "soon", "ch_console": "yes", "network": "nat", "ch_nap_slots": "3",
		"ch_disk_mb_s": "fast", "ch_disk_ops_s": "-1", "ch_net_mbit_s": "1.5"} {
		m := base()
		m[k] = v
		if _, err := parseConfig(m); err == nil {
			t.Errorf("%s=%q was accepted", k, v)
		}
	}
	for k, v := range map[string]string{"ch_disk_mb_s": "100001", "ch_disk_ops_s": "10000001", "ch_net_mbit_s": "100001"} {
		m := base()
		m[k] = v
		if _, err := parseConfig(m); err == nil {
			t.Errorf("%s=%s, a rate past the most it can be, was accepted", k, v)
		}
	}
	// The rates, up to the most each can be, and the supervisor reads the
	// same back.
	m := base()
	m["ch_disk_mb_s"], m["ch_disk_ops_s"], m["ch_net_mbit_s"] = "100000", "10000000", "100000"
	c, err = parseConfig(m)
	if want := (cloudhvLimits{DiskMBs: 100000, DiskOps: 10000000, NetMbit: 100000}); err != nil || c.CloudHV.Limits != want {
		t.Fatalf("the largest rates: %+v, %v, want %+v", c.CloudHV.Limits, err, want)
	}
	m["ch_disk_mb_s"], m["ch_disk_ops_s"], m["ch_net_mbit_s"] = "20", "2000", "10"
	c, err = parseConfig(m)
	if want := (cloudhvLimits{DiskMBs: 20, DiskOps: 2000, NetMbit: 10}); err != nil || c.CloudHV.Limits != want {
		t.Fatalf("the three rates: %+v, %v, want %+v", c.CloudHV.Limits, err, want)
	}
	m = base()
	c.CloudHV.raw(m)
	if c2, err := parseConfig(m); err != nil || c2.CloudHV != c.CloudHV {
		t.Errorf("the rates read back: %+v, %v, want %+v", c2.CloudHV, err, c.CloudHV)
	}
	// Nap slots: the binding's slots are then how many VMs are awake at once,
	// and the supervisor reads the same back.
	m = base()
	m["ch_nap_idle_s"], m["ch_nap_slots"], m["linux_slots"] = "60", "5", "7"
	c, err = parseConfig(m)
	if err != nil || c.CloudHV.NapSlots != 5 || c.CloudHV.Awake != 7 {
		t.Fatalf("ch_nap_slots=5 with linux_slots=7: %+v, %v", c.CloudHV, err)
	}
	m = base()
	c.CloudHV.raw(m)
	if c2, err := parseConfig(m); err != nil || c2.CloudHV != c.CloudHV {
		t.Errorf("nap slots read back: %+v, %v, want %+v", c2.CloudHV, err, c.CloudHV)
	}
	// A warm box that naps would hold no slot to be handed out in.
	m = base()
	m["ch_nap_idle_s"], m["ch_nap_slots"], m["warm"] = "60", "5", "1"
	if _, err := parseConfig(m); err == nil || !strings.Contains(err.Error(), "ch_nap_slots with warm") {
		t.Errorf("ch_nap_slots with warm: %v, want it refused", err)
	}
	delete(m, "ch_nap_slots")
	if _, err := parseConfig(m); err != nil && strings.Contains(err.Error(), "ch_nap_slots") {
		t.Errorf("warm without ch_nap_slots: %v", err)
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

// root=on and docker=on are a cloudhv box binding's. Describe then lists
// root, root-vm and docker; the guest of a VM made under the binding is told
// to give its user root and to start Docker, and the guest of a VM made
// without them is told neither, whatever the binding says later.
func TestCloudHVRootAndDocker(t *testing.T) {
	first := cfg(t)
	base := func(kv ...string) map[string]string {
		m := map[string]string{"vm": "cloudhv", "ch_firmware": "/fw/CLOUDHV.fd", "ch_images": "/images"}
		for k, v := range first {
			m[k] = v
		}
		delete(m, "image")
		m["kind"], m["size"], m["cpu"], m["memory"] = "box", "c2m4", "2", "4096"
		m["images"] = "box@sha256:" + strings.Repeat("a", 64)
		for i := 0; i < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return m
	}
	for _, c := range []struct {
		kv           []string
		root, docker bool
		caps         []string
		bad          string
	}{
		{nil, false, false, nil, ""},
		{[]string{"root", "on"}, true, false, []string{protocol.CapRoot, protocol.CapRootVM}, ""},
		{[]string{"docker", "on"}, true, true, []string{protocol.CapRoot, protocol.CapRootVM, protocol.CapDocker}, ""},
		{[]string{"root", "off", "docker", "on"}, false, false, nil, "docker=on needs root"},
		{[]string{"root", "on", "kind", "run", "image", "img"}, false, false, nil, "box binding"},
		{[]string{"root", "yes"}, false, false, nil, "root is on or off"},
	} {
		got, err := parseConfig(base(c.kv...))
		if c.bad != "" {
			if err == nil || !strings.Contains(err.Error(), c.bad) {
				t.Errorf("%v: %v, want it refused with %q", c.kv, err, c.bad)
			}
			continue
		}
		if err != nil || got.Root != c.root || got.Docker != c.docker {
			t.Errorf("%v: root %v, docker %v, %v", c.kv, got.Root, got.Docker, err)
			continue
		}
		// Acquire refuses a Linux image under a Tart binding's root=on; this
		// layer's images are Linux.
		if got.tartRoot() {
			t.Errorf("%v: taken as a Tart binding's root=on, which refuses every Linux image", c.kv)
		}
		d, err := call(t, base(c.kv...), "describe", "", nil)
		if err != nil {
			t.Errorf("%v: describe: %v", c.kv, err)
			continue
		}
		listed := anyStrings(d["capabilities"])
		for _, cp := range []string{protocol.CapRoot, protocol.CapRootVM, protocol.CapDocker} {
			if slices.Contains(listed, cp) != slices.Contains(c.caps, cp) {
				t.Errorf("%v: describe lists %v, want %s listed: %v", c.kv, listed, cp, slices.Contains(c.caps, cp))
			}
		}
		raw := map[string]string{}
		got.rootArgs(raw)
		if why := keptRootDiffers(raw, got); why != "" {
			t.Errorf("%v: a box made under this binding differs from it: %s", c.kv, why)
		}
		if other, _ := parseConfig(base("docker", "on")); !c.docker && keptRootDiffers(raw, other) == "" {
			t.Errorf("%v: a kept box made without docker would start under docker=on", c.kv)
		}
	}

	c := cloudhvFixture(t, 0)
	c.boot("plain", false)
	if c.run("plain", "echo", "ran"); c.has("plain", "root") || c.has("plain", "docker") {
		t.Errorf("a VM made without root: its guest was told root %v, docker %v", c.has("plain", "root"), c.has("plain", "docker"))
	}
	c.l.root, c.l.docker = true, true
	c.boot("rooted", false)
	if c.run("rooted", "echo", "ran"); !c.has("rooted", "root") || !c.has("rooted", "docker") {
		t.Errorf("a VM made with root and docker: its guest was told root %v, docker %v", c.has("rooted", "root"), c.has("rooted", "docker"))
	}
	// The first VM is what it was made as, though the layer now says root.
	if c.run("plain", "echo", "ran"); c.has("plain", "root") || c.has("plain", "docker") {
		t.Errorf("a VM made without root, after the binding changed: its guest was told root %v, docker %v", c.has("plain", "root"), c.has("plain", "docker"))
	}
}

// The fence drops everything docs/network.md lists (the same ranges as
// hostprobe's box network), and the root helper takes
// nothing but the layer's own taps and addresses.
func TestCloudHVFenceAndHelperArguments(t *testing.T) {
	fence := cloudhvFence()
	for _, want := range []string{"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "168.63.129.16", "169.254.0.0/16", "172.16.0.0/12",
		"192.0.0.0/24", "192.168.0.0/16", "198.18.0.0/15", "224.0.0.0/4", "240.0.0.0/4",
		"meta nfproto ipv6 drop", "fib saddr . iif oif missing drop", "ip saddr & 0.0.0.3 != 0.0.0.2 drop", "fib daddr type { local, broadcast } drop",
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
