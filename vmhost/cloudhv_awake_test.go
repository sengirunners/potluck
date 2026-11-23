package vmhost

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/sengirunners/potluck/protocol"
)

// napSlots gives the fixture's binding nap slots, with awake slots.
func (c *chTest) napSlots(awake int) { c.l.c.NapSlots, c.l.c.Awake = 2, awake }

// live writes a live linux box's record for vm in the fixture's state
// directory, as an acquire under the fixture's binding does. The func it
// answers ends the box's record.
func (c chTest) live(vm string) func() {
	c.t.Helper()
	return c.record(vm, stReady)
}

// record writes vm's record in state, and answers a func that ends it.
func (c chTest) record(vm, state string) func() {
	c.t.Helper()
	st := store{c.l.state}
	write := func(state string) {
		c.t.Helper()
		r := record{ID: vm, Kind: "box", OS: "linux", VM: vm, State: state, Config: map[string]string{"vm": "cloudhv", "ch_dir": c.l.c.Dir}}
		if err := st.locked(func() error { return st.save(r) }); err != nil {
			c.t.Fatal(err)
		}
	}
	write(state)
	return func() { write(stLost) }
}

// gaveUp says whether vm naps without a slot.
func (c chTest) gaveUp(vm string) func() bool {
	return func() bool {
		_, gaveUp := c.l.Slot(vm, nil)
		return gaveUp && c.state(vm).State == "napping"
	}
}

// hypervisor says whether vm's monitor has a child process.
func (c chTest) hypervisor(vm string) bool {
	pid := c.state(vm).PID
	if pid <= 1 {
		return false
	}
	b, _ := exec.Command("pgrep", "-P", strconv.Itoa(pid)).Output()
	return len(bytes.TrimSpace(b)) > 0
}

// events are vm's events of one kind.
func (c chTest) events(vm, kind string) []map[string]any {
	b, _ := os.ReadFile(filepath.Join(c.l.dir(vm), "events.jsonl"))
	var out []map[string]any
	for _, line := range strings.Split(string(b), "\n") {
		var e map[string]any
		if json.Unmarshal([]byte(line), &e) == nil && e["event"] == kind {
			out = append(out, e)
		}
	}
	return out
}

// ask runs a command for vm as the driver's exec does, and answers its exit
// code and what it printed.
func (c chTest) ask(vm string) (code int, stdout, stderr string) {
	code, stdout, stderr, _ = c.asked(vm, "sh", "-c", "echo ran")
	return code, stdout, stderr
}

// asked is ask for any command, and answers too whether the command said
// that its VM's wake was refused.
func (c chTest) asked(vm string, argv ...string) (code int, stdout, stderr string, refused bool) {
	var out, errb bytes.Buffer
	cmd := c.l.Exec(vm, nil, &out, &errb, argv...)
	said := c.l.WakeRefusal(vm, cmd)
	_ = cmd.Run()
	_, refused = said(errb.String())
	return cmd.ProcessState.ExitCode(), out.String(), errb.String(), refused
}

// marks are the refusal marks left in vm's directory.
func (c chTest) marks(vm string) []string {
	ps, _ := filepath.Glob(filepath.Join(c.l.dir(vm), cloudhvRefusedFile+"*"))
	return ps
}

// refused runs a command for vm that must be refused for want of a slot:
// nothing ran, and the VM naps on with its snapshot and no hypervisor. It
// answers what the refusal said.
func (c chTest) refused(vm, when string) string {
	c.t.Helper()
	code, out, said, refused := c.asked(vm, "sh", "-c", "echo ran")
	if code != 255 || out != "" || !strings.Contains(said, protocol.NoFreeSlot("linux")) || !refused {
		c.t.Fatalf("a command for a napping VM %s: exit %d, printed %q, said %q, refused %v: want exit 255, nothing run and a refused wake", when, code, out, said, refused)
	}
	if left := c.marks(vm); len(left) != 0 {
		c.t.Fatalf("a refusal that was read left %v in the VM's directory", left)
	}
	if !c.gaveUp(vm)() || !c.has(vm, "snap/sealed") || c.hypervisor(vm) {
		c.t.Fatalf("after the refused wake %s: %+v, snapshot there %v, hypervisor %v: the VM must nap on, whole", when, c.state(vm), c.has(vm, "snap/sealed"), c.hypervisor(vm))
	}
	return said
}

// awake is the driver's count of the guests that hold a slot.
func (c chTest) awake() int {
	c.t.Helper()
	st, n := store{c.l.state}, 0
	if err := st.locked(func() error {
		rs, err := st.readAll()
		_, n, _ = st.slotUse(rs, c.l, "linux")
		return err
	}); err != nil {
		c.t.Fatal(err)
	}
	return n
}

// With nap slots a VM that naps gives its slot up, and takes it back before
// it wakes: while as many guests as the binding's slots are awake, it naps
// on, with its command refused and nothing of it lost. Every guest that
// holds a slot in the state directory counts, a box of another layer too.
func TestCloudHVANappingVMGivesUpItsSlot(t *testing.T) {
	c := cloudhvFixture(t, 1)
	c.napSlots(1)
	c.live("vm1")
	c.boot("vm1", false)
	if n := c.awake(); n != 1 {
		t.Fatalf("%d guests hold a slot with one VM awake, want 1", n)
	}
	mem := c.run("vm1", "sh", "-c", "echo $FAKE_MEM")
	ran := time.Now().UnixNano()
	c.until("the idle VM naps", c.gaveUp("vm1"))
	// No stop wakes it now, so its guest wrote its files out twice: when
	// it was squeezed, and again just before the pause.
	b, _ := os.ReadFile(filepath.Join(c.l.dir("vm1"), "squeezed"))
	syncs := 0
	for _, at := range strings.Fields(string(b)) {
		if n, _ := strconv.ParseInt(at, 10, 64); n > ran {
			syncs++
		}
	}
	if syncs != 2 {
		t.Fatalf("the guest was asked to write its files out %d times between its last command and its nap, want 2", syncs)
	}
	if n := c.awake(); n != 0 {
		t.Fatalf("%d guests hold a slot with the one VM napping, want 0", n)
	}

	c.live("vm2")
	c.boot("vm2", false) // in the slot vm1 gave up
	release := c.hold("vm2")
	c.refused("vm1", "with the slot's VM awake")
	release()
	c.until("the second VM naps", c.gaveUp("vm2"))

	gone := c.live("other") // a box that is no VM of this layer: it never naps
	c.refused("vm1", "with a box of another layer in the slot")
	gone()
	if got := c.run("vm1", "sh", "-c", "echo $FAKE_MEM"); got != mem {
		t.Errorf("once a slot was free the command saw %q, want the same guest's %q", got, mem)
	}
	if st := c.state("vm1"); st.State != "awake" || st.Waking || c.gaveUp("vm1")() || c.awake() != 1 {
		t.Errorf("after the wake: %+v, %d guests hold a slot: want it awake, holding the one", st, c.awake())
	}
}

// A refusal is told from a command's own words by what the host left, not
// by what the VM does afterwards: it is still read as a refusal when the VM
// has woken since, and a command that prints a refusal's words is not one.
func TestCloudHVARefusalIsNotMistaken(t *testing.T) {
	// A mark named in the driver's own environment is nobody's to make.
	stray := filepath.Join(t.TempDir(), cloudhvRefusedFile+"stray")
	if err := os.WriteFile(stray, []byte("kept\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(cloudhvRefusedEnv, stray)
	c := cloudhvFixture(t, 1)
	c.napSlots(1)
	c.live("vm1")
	c.boot("vm1", false)
	c.until("vm1 naps", c.gaveUp("vm1"))
	c.live("vm2")
	c.boot("vm2", false)
	release := c.hold("vm2")
	// A command that was not asked to say so (a spawn's, a port's) leaves
	// nothing behind, however often it is refused.
	var errb bytes.Buffer
	cmd := c.l.Exec("vm1", nil, io.Discard, &errb, "true")
	if _ = cmd.Run(); cmd.ProcessState.ExitCode() != 255 || !strings.Contains(errb.String(), cloudhvNapsOn) {
		t.Fatalf("a command for the napping VM with no slot free: exit %d, said %q", cmd.ProcessState.ExitCode(), errb.String())
	}
	if left := c.marks("vm1"); len(left) != 0 {
		t.Fatalf("a refused command that was not asked to mark its refusal left %v", left)
	}
	if b, _ := os.ReadFile(stray); string(b) != "kept\n" {
		t.Fatalf("a refused command wrote the file its driver's environment named: it holds %q", b)
	}
	words := errb.String()

	// One that was asked says so although the VM wakes before it is read.
	errb.Reset()
	cmd = c.l.Exec("vm1", nil, io.Discard, &errb, "true")
	refused := c.l.WakeRefusal("vm1", cmd)
	if _ = cmd.Run(); cmd.ProcessState.ExitCode() != 255 {
		t.Fatalf("a second command for the napping VM: exit %d, said %q", cmd.ProcessState.ExitCode(), errb.String())
	}
	release()
	c.until("vm2 naps", c.gaveUp("vm2"))
	defer c.hold("vm1")() // another command wakes vm1 before the refusal is read
	if why, ok := refused(errb.String()); !ok || !strings.Contains(why, "awake now") {
		t.Errorf("a refused wake read once the VM had woken: refused %v, because %q: want it read, with its reason", ok, why)
	}

	// A command that ran, printed a refusal's words and exited 255 is not one.
	code, _, said, was := c.asked("vm1", "sh", "-c", `printf '%s' "$1" >&2; exit 255`, "sh", words)
	if code != 255 || !strings.Contains(said, cloudhvNapsOn) {
		t.Fatalf("the command that prints a refusal's words: exit %d, %q", code, said)
	}
	if was {
		t.Error("a command's own words were read as a refused wake")
	}
}

// A VM that took its slot back holds it while it is restored: the count has
// it awake, and no other VM wakes in its slot.
func TestCloudHVAWakingVMHoldsItsSlot(t *testing.T) {
	c := cloudhvFixture(t, 1)
	c.napSlots(1)
	for _, vm := range []string{"vm1", "vm2"} {
		c.live(vm)
		c.boot(vm, false)
		c.until(vm+" naps", c.gaveUp(vm))
	}
	slow := filepath.Join(c.l.dir("vm1"), "slow-restore")
	if err := os.WriteFile(slow, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- c.l.Exec("vm1", nil, io.Discard, io.Discard, "true").Run() }()
	c.until("vm1 is being restored", func() bool { return c.state("vm1").Waking })
	if st := c.state("vm1"); st.State != "napping" || c.gaveUp("vm1")() || c.awake() != 1 {
		t.Fatalf("a VM being restored: %+v, gave up its slot %v, %d guests hold one: want it counted awake", st, c.gaveUp("vm1")(), c.awake())
	}
	c.refused("vm2", "while another is restored into the only slot")
	if err := os.Remove(slow); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("the command that woke vm1: %v", err)
	}
	if st := c.state("vm1"); st.State != "awake" || st.Waking {
		t.Errorf("after the restore: %+v", st)
	}
}

// Four napping VMs and one slot, with commands for all four at once, six
// times over: no two of them ever have a hypervisor at the same moment.
func TestCloudHVNoTwoVMsWakeInOneSlot(t *testing.T) {
	t.Setenv(envCloudHVAwakeWait, "1s")
	c := cloudhvFixture(t, 1)
	c.napSlots(1)
	vms := []string{"vm1", "vm2", "vm3", "vm4"}
	for _, vm := range vms {
		c.live(vm)
		c.boot(vm, false)
		c.until(vm+" naps", c.gaveUp(vm))
	}
	stop := make(chan struct{})
	var most, ran int32
	var watch sync.WaitGroup
	watch.Add(1)
	go func() {
		defer watch.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			n := int32(0)
			for _, vm := range vms {
				if c.hypervisor(vm) {
					n++
				}
			}
			if n > atomic.LoadInt32(&most) {
				atomic.StoreInt32(&most, n)
			}
			time.Sleep(2 * time.Millisecond)
		}
	}()
	for round := 0; round < 6; round++ {
		var wg sync.WaitGroup
		for _, vm := range vms {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if c.l.Exec(vm, nil, io.Discard, io.Discard, "sleep", "0.2").Run() == nil {
					atomic.AddInt32(&ran, 1)
				}
			}()
		}
		wg.Wait()
	}
	close(stop)
	watch.Wait()
	if most != 1 || ran < 6 {
		t.Errorf("%d commands ran, and at most %d hypervisors at one moment: want one in each round at least, and never two", ran, most)
	}
}

// Commands that wait behind a refused wake are refused with it, not each
// after a wait of its own.
func TestCloudHVCommandsBehindARefusedWakeAreRefusedWithIt(t *testing.T) {
	t.Setenv(envCloudHVAwakeWait, "5s")
	c := cloudhvFixture(t, 1)
	c.napSlots(1)
	c.live("vm1")
	c.boot("vm1", false)
	c.until("vm1 naps", c.gaveUp("vm1"))
	c.live("vm2")
	c.boot("vm2", false)
	defer c.hold("vm2")()

	done := make(chan string, 3)
	ask := func() {
		if code, _, said := c.ask("vm1"); code != 255 || !strings.Contains(said, cloudhvNapsOn) {
			done <- "exit " + strconv.Itoa(code) + ", said " + said
			return
		}
		done <- ""
	}
	go ask()
	c.until("the first command's wake waits for a slot", func() bool { return len(c.events("vm1", "slot_wait")) == 1 })
	go ask()
	go ask()
	for i := 0; i < 3; i++ {
		if bad := <-done; bad != "" {
			t.Errorf("a command for the napping VM with no slot free: %s: want a refused wake", bad)
		}
	}
	if ev := c.events("vm1", "wake_refused"); len(ev) != 1 || ev[0]["commands"] != 3.0 {
		t.Errorf("three commands, two of them come while the first waited: refusals %v, want one, of all three", ev)
	}
	if !c.gaveUp("vm1")() {
		t.Errorf("after the refusals: %+v: the VM must nap on", c.state("vm1"))
	}
}

// A stop is not kept waiting by a wake that waits for a slot: neither a
// kept VM's power-off nor the end of the VM's supervisor.
func TestCloudHVAStopDoesNotWaitForASlot(t *testing.T) {
	const wait = 30 * time.Second
	t.Setenv(envCloudHVAwakeWait, wait.String())
	c := cloudhvFixture(t, 1)
	c.napSlots(1)
	for _, vm := range []string{"vm1", "vm3"} {
		c.live(vm)
	}
	cmd, _ := c.boot("vm1", true)
	_, held := c.boot("vm3", false)
	c.until("vm1 and vm3 nap", func() bool { return c.gaveUp("vm1")() && c.gaveUp("vm3")() })
	c.live("vm2")
	c.boot("vm2", false)
	defer c.hold("vm2")()
	// Among so many records a wait counts once in five seconds: a stop is
	// not kept waiting for the next count either.
	st := store{c.l.state}
	if err := st.locked(func() error {
		for i := 0; i < 20*cloudhvAwakePollRecords; i++ {
			if err := st.save(record{ID: fmt.Sprintf("old%d", i), Kind: "box", OS: "linux", State: stLost}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	for _, x := range []struct {
		vm   string
		stop func()
		gone func() bool
	}{
		{"vm1", func() { c.l.PowerOff("vm1", cmd.Process.Pid) }, func() bool { return !cloudhvMonAlive(c.l.dir("vm1")) && c.has("vm1", "disk.qcow2") }},
		{"vm3", func() { held.Close() }, func() bool { return !cloudhvMonAlive(c.l.dir("vm3")) && !c.has("vm3", "vm.json") }},
	} {
		asked := make(chan int, 1)
		go func() { code, _, _ := c.ask(x.vm); asked <- code }()
		c.until(x.vm+"'s wake waits for a slot", func() bool { return len(c.events(x.vm, "slot_wait")) == 1 })
		t0 := time.Now()
		x.stop()
		c.until(x.vm+" is stopped", x.gone)
		if took := time.Since(t0); took > 4*time.Second {
			t.Errorf("%s's stop took %s with a wake waiting %s for a slot", x.vm, took, wait)
		}
		if code := <-asked; code == 0 {
			t.Errorf("the command that waited for %s's slot ran in a VM that was stopped", x.vm)
		}
	}
}

// A count that cannot be made refuses the wake, as a full host does: when
// another holds the count's lock for the whole wait, when a record cannot
// be read or the records cannot be listed, when the VM's own state cannot
// be written, and when the state directory is gone.
func TestCloudHVAWakeThatCannotCountIsRefused(t *testing.T) {
	c := cloudhvFixture(t, 1)
	c.napSlots(1)
	c.live("vm1")
	c.boot("vm1", false)
	mem := c.run("vm1", "sh", "-c", "echo $FAKE_MEM")
	c.until("vm1 naps", c.gaveUp("vm1"))
	same := func(when string) {
		t.Helper()
		if got := c.run("vm1", "sh", "-c", "echo $FAKE_MEM"); got != mem {
			t.Fatalf("%s the command saw %q, want the same guest's %q", when, got, mem)
		}
		c.until("vm1 naps again", c.gaveUp("vm1"))
	}
	uncounted := func(when string) {
		t.Helper()
		if said := c.refused("vm1", when); !strings.Contains(said, "could not be counted") || strings.Contains(said, c.l.state) || strings.Contains(said, c.l.c.Dir) {
			t.Errorf("%s the refusal said %q, want it to say the count could not be made, with no path of the host's", when, said)
		}
	}
	st := store{c.l.state}
	if err := st.locked(func() error { uncounted("while the count's lock is held"); return nil }); err != nil {
		t.Fatal(err)
	}
	same("once the lock was free")

	bad := st.file("torn")
	if err := os.WriteFile(bad, []byte("{torn"), 0o600); err != nil {
		t.Fatal(err)
	}
	uncounted("with a record that cannot be read")
	if err := os.Remove(bad); err != nil {
		t.Fatal(err)
	}
	same("once the record was gone")
	if err := os.Mkdir(bad, 0o700); err != nil {
		t.Fatal(err)
	}
	uncounted("with a directory in a record's place")
	if err := os.Remove(bad); err != nil {
		t.Fatal(err)
	}
	same("once the directory was gone")

	runs := filepath.Join(c.l.state, "runs")
	if err := os.Chmod(runs, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(runs, 0o700) })
	if os.Geteuid() != 0 { // root lists it all the same
		uncounted("with records that cannot be listed")
	}
	if err := os.Chmod(runs, 0o700); err != nil {
		t.Fatal(err)
	}
	same("once the records could be listed")

	tmp := filepath.Join(c.l.dir("vm1"), "state.json.tmp") // in the way of the VM's state file
	if err := os.Mkdir(tmp, 0o700); err != nil {
		t.Fatal(err)
	}
	uncounted("when its state cannot be written")
	if err := os.Remove(tmp); err != nil {
		t.Fatal(err)
	}
	same("once its state could be written")

	c.l.state = filepath.Join(c.l.state, "gone")
	c.boot("vm3", false)
	c.until("vm3 naps", func() bool { return c.state("vm3").State == "napping" })
	if code, _, said := c.ask("vm3"); code != 255 || !strings.Contains(said, "could not be counted") || strings.Contains(said, c.l.state) {
		t.Errorf("a wake whose state directory is gone: exit %d, said %q: want it refused", code, said)
	}
	if c.state("vm3").State != "napping" || !c.has("vm3", "snap/sealed") {
		t.Errorf("after that refusal: %+v: the VM must nap on, whole", c.state("vm3"))
	}
}

// The count finds a VM by the ch_dir its record was made under, and the
// state directory from wherever the VM's monitor runs: two bindings with
// nap slots and a ch_dir each do not take each other's napping VMs for
// awake ones, and a relative state directory is found.
func TestCloudHVTheCountFindsOtherBindingsVMs(t *testing.T) {
	c1 := cloudhvFixture(t, 1)
	c1.napSlots(1)
	t.Chdir(filepath.Dir(c1.l.state))
	c1.l.state = filepath.Base(c1.l.state)
	c2 := c1
	c2.l.c.Dir, c2.l.c.Net = filepath.Join(filepath.Dir(c1.l.c.Dir), "vms2"), "10.254.253.0/28"
	if err := os.Mkdir(c2.l.c.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	c1.live("vm1")
	c1.boot("vm1", false)
	c1.until("vm1 naps", c1.gaveUp("vm1"))
	c2.live("vmB")
	c2.boot("vmB", false)
	c2.until("vmB naps", c2.gaveUp("vmB"))
	if n := c1.awake(); n != 0 {
		t.Errorf("%d guests hold a slot with both bindings' VMs napping, want 0", n)
	}
	defer c2.hold("vmB")() // wakes: nothing is awake
	c1.refused("vm1", "with the other binding's VM awake")
}

// A VM wakes by the slots its binding has when it boots: a kept VM made
// without nap slots gives its slot up once it has booted under them.
func TestCloudHVAKeptVMTakesItsBindingsSlotsAtBoot(t *testing.T) {
	c := cloudhvFixture(t, 1)
	c.live("vm1")
	cmd, _ := c.boot("vm1", true)
	c.until("vm1 naps", func() bool { return c.state("vm1").State == "napping" })
	if c.gaveUp("vm1")() {
		t.Fatal("a VM booted without nap slots gave its slot up")
	}
	c.l.PowerOff("vm1", cmd.Process.Pid)
	c.napSlots(1)
	c.start("vm1", c.l.BootKept)
	c.until("vm1 naps without its slot", c.gaveUp("vm1"))
	c.live("vm2")
	c.boot("vm2", false)
	defer c.hold("vm2")()
	c.refused("vm1", "booted under nap slots, with the slot's VM awake")
}

// Without nap slots a napping VM keeps its slot, and wakes without a count,
// however many guests hold slots.
func TestCloudHVNoNapSlotsANappingVMKeepsItsSlot(t *testing.T) {
	c := cloudhvFixture(t, 1)
	for _, vm := range []string{"vm1", "vm2", "other"} {
		c.live(vm)
	}
	c.boot("vm1", false)
	c.boot("vm2", false)
	defer c.hold("vm2")()
	mem := c.run("vm1", "sh", "-c", "echo $FAKE_MEM")
	c.until("vm1 naps", func() bool { return c.state("vm1").State == "napping" })
	if c.gaveUp("vm1")() || c.awake() != 3 {
		t.Errorf("no nap slots: the napping VM gave up its slot %v, %d guests hold one, want 3", c.gaveUp("vm1")(), c.awake())
	}
	if got := c.run("vm1", "sh", "-c", "echo $FAKE_MEM"); got != mem {
		t.Errorf("the napping VM's command saw %q, want the same guest's %q", got, mem)
	}
	if ev := c.events("vm1", "slot_wait"); len(ev) != 0 {
		t.Errorf("a VM without nap slots waited for a slot: %v", ev)
	}
}

// napBinding is a cloud-hypervisor box binding over the tests' hypervisor,
// with one slot and one nap slot, for the driver's own verbs. It answers
// the binding and the directory its VMs are in.
func napBinding(t *testing.T) (map[string]string, string) {
	t.Helper()
	c := cloudhvFixture(t, 1)
	state := stateDir(t)
	pin := strings.Repeat("ab", 32)
	if err := os.WriteFile(filepath.Join(c.l.c.Images, "img.raw.sha256"), []byte(pin+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return map[string]string{"vm": "cloudhv", "target": "t", "kind": "box", "size": "small", "cpu": "1", "memory": "512",
		"images": "img@sha256:" + pin, "state_dir": state, "boot_timeout_s": "20", "persist_min_free_gb": "0",
		"ch": c.l.c.Bin, "ch_firmware": c.l.c.Firmware, "ch_qemu_img": c.l.c.QemuImg, "ch_images": c.l.c.Images,
		"ch_net": "10.254.254.0/28", "ch_nap_idle_s": "1", "ch_nap_slots": "1", "linux_slots": "1"}, filepath.Join(state, "cloudhv")
}

// napBox acquires a box on a napBinding and waits until it is ready. It
// answers the box's handle and a reader of its VM's state.
func napBox(t *testing.T, m map[string]string, dir, key string, persist bool) (string, func() cloudhvState) {
	t.Helper()
	a, err := call(t, m, "acquire", "", map[string]any{"request_id": key, "ttl_s": 300, "persist": persist})
	if err != nil {
		t.Fatalf("acquire %s: %v", key, err)
	}
	h := a["handle"].(string)
	waitState(t, m, h, "ready")
	vm := ""
	for _, r := range (store{m["state_dir"]}).all() {
		if r.Handle == h {
			vm = r.VM
		}
	}
	return h, func() cloudhvState {
		var s cloudhvState
		if b, err := os.ReadFile(filepath.Join(dir, vm, "state.json")); err == nil {
			_ = json.Unmarshal(b, &s)
		}
		return s
	}
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for end := time.Now().Add(20 * time.Second); !ok(); time.Sleep(20 * time.Millisecond) {
		if time.Now().After(end) {
			t.Fatalf("never happened: %s", what)
		}
	}
}

// The driver's verbs on a binding with nap slots: a napping box frees its
// slot for an acquire; a command for it while the slot is taken is refused
// as unavailable, and a spawn's process ends saying so; and its stop does
// not wait for a slot.
func TestCloudHVNapSlotsThroughTheDriver(t *testing.T) {
	const wait = 4 * time.Second
	t.Setenv(envCloudHVAwakeWait, wait.String())
	m, dir := napBinding(t)
	free := func() float64 {
		t.Helper()
		d, err := call(t, m, "describe", "", nil)
		if err != nil {
			t.Fatal(err)
		}
		return d["slots"].(map[string]any)["free"].(float64)
	}
	ha, sa := napBox(t, m, dir, "a", true)
	if n := free(); n != 0 {
		t.Fatalf("%v free with the one slot's box awake, want 0", n)
	}
	eventually(t, "a naps", func() bool { return sa().State == "napping" })
	if n := free(); n != 1 {
		t.Fatalf("%v free with the one box napping, want 1", n)
	}
	torn := store{m["state_dir"]}.file("torn") // a record the count cannot read: no box is admitted by a count that may miss one
	if err := os.WriteFile(torn, []byte("{torn"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := call(t, m, "acquire", "", map[string]any{"request_id": "t", "ttl_s": 300}); err == nil || !strings.Contains(err.Error(), "torn") {
		t.Fatalf("an acquire with a record that cannot be read: %v, want it refused", err)
	}
	if _, err := call(t, m, "exec", ha, map[string]any{"argv": []string{"true"}}); code(err) != "unavailable" ||
		!strings.Contains(err.Error(), "could not be counted") || !strings.Contains(err.Error(), "torn") || !strings.Contains(err.Error(), "nothing ran") {
		t.Fatalf("a command for the napping box with a record that cannot be read: %v, want unavailable, saying why", err)
	}
	if err := os.Remove(torn); err != nil {
		t.Fatal(err)
	}
	hb, sb := napBox(t, m, dir, "b", false)
	held := make(chan struct{})
	go func() {
		defer close(held)
		_, _ = call(t, m, "exec", hb, map[string]any{"argv": []string{"sleep", "60"}})
	}()
	eventually(t, "b is awake under its command", func() bool { s := sb(); return s.State == "awake" && s.Active >= 1 })
	if _, err := call(t, m, "acquire", "", map[string]any{"request_id": "c", "ttl_s": 300}); code(err) != "unavailable" ||
		!strings.Contains(err.Error(), "1 awake of 1 at once, 2 boxes of 2 in all") {
		t.Errorf("a third box with one awake and one napping: %v, want unavailable", err)
	}

	res, err := call(t, m, "exec", ha, map[string]any{"argv": []string{"true"}})
	if code(err) != "unavailable" || !strings.Contains(err.Error(), protocol.NoFreeSlot("linux")) || !strings.Contains(err.Error(), "nothing ran") {
		t.Errorf("a command for the napping box with the slot taken: %v %v, want unavailable", res, err)
	}
	// The commands the driver runs in the guest itself, for the box's
	// secrets and before a spawn that carries secrets, are refused the same:
	// nothing ran in the guest, so nothing failed there.
	if res, err := call(t, m, "secrets", ha, map[string]any{"secrets": map[string]string{"K": "v"}}); code(err) != "unavailable" ||
		!strings.Contains(err.Error(), protocol.NoFreeSlot("linux")) || !strings.Contains(err.Error(), "nothing ran") {
		t.Errorf("the box's secrets set with the slot taken: %v %v, want unavailable", res, err)
	}
	if res, err := call(t, m, "spawn", ha, map[string]any{"request_id": "s0", "argv": []string{"true"}, "secrets": map[string]string{"K": "v"}}); code(err) != "unavailable" ||
		!strings.Contains(err.Error(), protocol.NoFreeSlot("linux")) || !strings.Contains(err.Error(), "nothing ran") {
		t.Errorf("a spawn with secrets for the napping box with the slot taken: %v %v, want unavailable", res, err)
	}
	// A spawn with secrets that already made its process answers that
	// process when it is asked again: it is not told that nothing ran.
	asked := false
	for _, r := range (store{m["state_dir"]}).all() {
		if r.Handle != ha {
			continue
		}
		asked = true
		made, err := (store{m["state_dir"]}).procs(r.ID).New("s9")
		if err != nil {
			t.Fatal(err)
		}
		if again, err := call(t, m, "spawn", ha, map[string]any{"request_id": "s9", "argv": []string{"true"}, "secrets": map[string]string{"K": "v"}}); err != nil || again["pid"] != made.ID() {
			t.Errorf("a spawn with secrets asked again for the napping box: %v %v, want the process it made, %s", again, err, made.ID())
		}
		if err := os.RemoveAll(string(made)); err != nil {
			t.Fatal(err)
		}
	}
	if !asked {
		t.Fatal("the napping box has no record")
	}
	sp, err := call(t, m, "spawn", ha, map[string]any{"request_id": "s1", "argv": []string{"true"}})
	if err != nil {
		t.Fatalf("a spawn for the napping box: %v", err)
	}
	var o map[string]any
	eventually(t, "the spawned process ends", func() bool {
		o, err = call(t, m, "output", ha, map[string]any{"pid": sp["pid"], "wait_s": 1})
		return err != nil || o["running"] == false
	})
	if err != nil || o["exit"] != 255.0 || !strings.Contains(o["stderr"].(string), protocol.NoFreeSlot("linux")) {
		t.Fatalf("the spawned process: %v %v, want exit 255 and the refusal's words", o, err)
	}
	words := o["stderr"].(string)
	if again, err := call(t, m, "spawn", ha, map[string]any{"request_id": "s1", "argv": []string{"true"}}); err != nil || again["pid"] != sp["pid"] {
		t.Errorf("the same spawn asked again: %v %v, want the process it already has, %v", again, err, sp["pid"])
	}
	if _, err := call(t, m, "port", ha, map[string]any{"op": "open", "port": 8080}); code(err) != "unavailable" {
		t.Errorf("a port call for the napping box with the slot taken: %v, want unavailable", err)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, "*", cloudhvRefusedFile+"*")); len(left) != 0 {
		t.Errorf("the refused commands left %v in the box's VM directory", left)
	}
	if s := sa(); s.State != "napping" || s.Waking {
		t.Fatalf("after the refusals a is %+v, want it napping", s)
	}

	// What a refused spawn said, said again by a command that runs, is that
	// command's own: it ran, and is answered so.
	if _, err := call(t, m, "release", hb, nil); err != nil {
		t.Fatal(err)
	}
	<-held
	proof := filepath.Join(t.TempDir(), "ran")
	res, err = call(t, m, "exec", ha, map[string]any{"argv": []string{"sh", "-c", `echo ran > "$1"; printf '%s' "$2" >&2; exit 255`, "sh", proof, words}})
	if b, _ := os.ReadFile(proof); err != nil || res["exit"] != 255.0 || string(b) != "ran\n" {
		t.Fatalf("a command that ran, said a refusal's words and exited 255: %v %v, wrote %q: want its own exit", res, err, b)
	}
	eventually(t, "a naps again", func() bool { return sa().State == "napping" })
	hc, sc := napBox(t, m, dir, "c", false)
	held = make(chan struct{})
	go func() {
		defer close(held)
		_, _ = call(t, m, "exec", hc, map[string]any{"argv": []string{"sleep", "60"}})
	}()
	eventually(t, "c is awake under its command", func() bool { s := sc(); return s.State == "awake" && s.Active >= 1 })
	hb = hc

	t0 := time.Now()
	if _, err := call(t, m, "stop", ha, nil); err != nil {
		t.Fatalf("stop of the napping box: %v", err)
	}
	if took := time.Since(t0); took > wait/2 {
		t.Errorf("the napping box's stop took %s with no slot free: want it not to wait %s for one", took, wait)
	}
	if st, err := call(t, m, "status", ha, nil); err != nil || st["state"] != "stopped" {
		t.Errorf("after the stop: %v %v", st, err)
	}
	if _, err := call(t, m, "release", hb, nil); err != nil {
		t.Error(err)
	}
	<-held
}

// A stopped box holds its slot until its hypervisor is gone: no napping box
// wakes in it while the stop is still shutting the hypervisor down.
func TestCloudHVAStoppingBoxHoldsItsSlot(t *testing.T) {
	t.Setenv(envCloudHVAwakeWait, "2s")
	m, dir := napBinding(t)
	hn, sn := napBox(t, m, dir, "n", false)
	eventually(t, "n naps", func() bool { return sn().State == "napping" })
	hk, sk := napBox(t, m, dir, "k", true)
	held := make(chan struct{})
	go func() {
		defer close(held)
		_, _ = call(t, m, "exec", hk, map[string]any{"argv": []string{"sleep", "60"}})
	}()
	eventually(t, "k is awake under its command", func() bool { s := sk(); return s.State == "awake" && s.Active >= 1 })
	monK := sk().PID
	var vmK string
	for _, r := range (store{m["state_dir"]}).all() {
		if r.Handle == hk {
			vmK = r.VM
		}
	}
	slow := filepath.Join(dir, vmK, "slow-shutdown")
	if err := os.WriteFile(slow, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	stopped := make(chan error, 1)
	go func() { _, err := call(t, m, "stop", hk, nil); stopped <- err }()
	eventually(t, "k's record is stopped", func() bool {
		st, err := call(t, m, "status", hk, nil)
		return err == nil && st["state"] == "stopped"
	})
	_, err := call(t, m, "exec", hn, map[string]any{"argv": []string{"true"}})
	kids, _ := exec.Command("pgrep", "-P", strconv.Itoa(monK)).Output()
	if len(bytes.TrimSpace(kids)) == 0 {
		t.Fatal("k's hypervisor was gone before the stop was let finish: the test showed nothing")
	}
	if code(err) != "unavailable" {
		t.Errorf("a command for the napping box while the stopped one's hypervisor still ran: %v, want unavailable", err)
	}
	if err := os.Remove(slow); err != nil {
		t.Fatal(err)
	}
	if err := <-stopped; err != nil {
		t.Fatalf("stop: %v", err)
	}
	<-held
	if res, err := call(t, m, "exec", hn, map[string]any{"argv": []string{"true"}}); err != nil || res["exit"] != 0.0 {
		t.Errorf("a command for the napping box once the stopped one was gone: %v %v", res, err)
	}
}

// A box whose supervisor died holds its slot until the driver sweeps. A
// command, which does not sweep, is refused for want of that slot; the
// refusal sweeps, and the next try wakes the box.
func TestCloudHVARefusedCommandSweeps(t *testing.T) {
	t.Setenv(envCloudHVAwakeWait, "1s")
	m, dir := napBinding(t)
	ha, sa := napBox(t, m, dir, "a", false)
	eventually(t, "a naps", func() bool { return sa().State == "napping" })
	hb, sb := napBox(t, m, dir, "b", false)
	st := store{m["state_dir"]}
	r, err := st.load(idOf(hb))
	if err != nil {
		t.Fatal(err)
	}
	_ = syscall.Kill(r.Supervisor, syscall.SIGKILL) // a zombie of this test binary still answers kill 0: name a dead pid
	if _, err := st.update(r.ID, func(o *record) error { o.Supervisor = deadPID(t); return nil }); err != nil {
		t.Fatal(err)
	}
	eventually(t, "b's VM ends with its supervisor", func() bool { return sb().PID == 0 })
	if _, err := call(t, m, "exec", ha, map[string]any{"argv": []string{"true"}}); code(err) != "unavailable" {
		t.Fatalf("a command for the napping box while the dead box's record is live: %v, want unavailable", err)
	}
	if res, err := call(t, m, "exec", ha, map[string]any{"argv": []string{"true"}}); err != nil || res["exit"] != 0.0 {
		t.Errorf("the next try: %v %v, want the box woken", res, err)
	}
}

// A box that naps before its start has finished holds its slot: no other box
// is given it, and the next step of its start wakes it on a full host.
func TestCloudHVAStartingBoxHoldsItsSlotThroughANap(t *testing.T) {
	c := cloudhvFixture(t, 1)
	c.napSlots(1)
	c.record("vm1", stStarting)
	c.boot("vm1", false)
	c.run("vm1", "true") // a step of its start
	c.until("the starting VM naps", c.gaveUp("vm1"))
	if n := c.awake(); n != 1 {
		t.Fatalf("%d guests hold a slot with the one box napping in its start, want 1", n)
	}
	c.live("vm2")
	c.boot("vm2", false)
	c.run("vm2", "true")
	c.until("the second VM naps", c.gaveUp("vm2"))
	c.refused("vm2", "with the slot held by a box still starting")
	if code, out, said := c.ask("vm1"); code != 0 || out != "ran\n" {
		t.Fatalf("the next step of the napping box's start: exit %d, printed %q, said %q: want it run", code, out, said)
	}
}

// A VM whose monitor died after it took its slot holds the slot for as long
// as its record is live, and no longer.
func TestCloudHVADeadWakingVMHoldsItsSlotWithItsRecord(t *testing.T) {
	c := cloudhvFixture(t, 1)
	c.napSlots(1)
	gone := c.live("vm1")
	if err := c.l.Clone("img", "vm1", 2, 512); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(cloudhvState{State: "napping", Waking: true, PID: deadPID(t)})
	if err := os.WriteFile(filepath.Join(c.l.dir("vm1"), "state.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	if n := c.awake(); n != 1 {
		t.Fatalf("%d guests hold a slot with a VM that died while waking and a live record, want 1", n)
	}
	gone()
	if n := c.awake(); n != 0 {
		t.Fatalf("%d guests hold a slot once that VM's record ended, want 0", n)
	}
}

// A file an unclean shutdown left in a record's place is set aside by the
// sweep when it is older than the host's last start, so the count is made
// again; a newer one is left, and refuses (TestCloudHVNapSlotsThroughTheDriver).
// What was set aside is kept as long as an ended record is, from when it
// was set aside, and then removed.
func TestCloudHVATornRecordFromBeforeTheHostStartedIsSetAside(t *testing.T) {
	if hostBootTime().IsZero() {
		t.Skip("this host does not say when it started")
	}
	m, dir := napBinding(t)
	torn := store{m["state_dir"]}.file("torn")
	if err := os.MkdirAll(filepath.Dir(torn), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(torn, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	old := hostBootTime().Add(-time.Hour)
	if long := time.Now().Add(-keepFor - time.Hour); long.Before(old) {
		old = long // written longer ago than a file set aside is kept
	}
	if err := os.Chtimes(torn, old, old); err != nil {
		t.Fatal(err)
	}
	h, _ := napBox(t, m, dir, "a", false)
	sweep := func() {
		t.Helper()
		if _, err := call(t, m, "release", h, nil); err != nil {
			t.Fatal(err)
		}
		h, _ = napBox(t, m, dir, "a"+h, false)
	}
	if _, err := os.Stat(torn + ".torn"); err != nil {
		t.Fatalf("the torn record, written more than %s ago, was not set aside and kept: %v", keepFor, err)
	}
	sweep()
	if _, err := os.Stat(torn + ".torn"); err != nil {
		t.Fatalf("a file just set aside did not outlast the next sweep: %v", err)
	}
	for _, c := range []struct {
		age  time.Duration
		kept bool
	}{{keepFor - time.Hour, true}, {keepFor + time.Hour, false}} {
		at := time.Now().Add(-c.age)
		if err := os.Chtimes(torn+".torn", at, at); err != nil {
			t.Fatal(err)
		}
		sweep()
		if _, err := os.Stat(torn + ".torn"); (err == nil) != c.kept {
			t.Errorf("a file set aside %s ago: kept %v after a sweep, want %v", c.age, err == nil, c.kept)
		}
	}
}

// A command that gave up while its VM's wake waited for a slot takes none
// later: the VM naps on when a slot comes free, and wakes for the next
// command that asks.
func TestCloudHVAWakeWhoseCommandLeftIsDropped(t *testing.T) {
	t.Setenv(envCloudHVAwakeWait, "30s")
	c := cloudhvFixture(t, 1)
	c.napSlots(1)
	c.live("vm1")
	c.boot("vm1", false)
	c.until("vm1 naps", c.gaveUp("vm1"))
	c.live("vm2")
	c.boot("vm2", false)
	release := c.hold("vm2")
	cmd := c.l.Exec("vm1", nil, io.Discard, io.Discard, "true")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	c.until("vm1's wake waits for a slot", func() bool { return len(c.events("vm1", "slot_wait")) == 1 })
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	c.until("the wake of the command that left is dropped", func() bool { return len(c.events("vm1", "wake_dropped")) == 1 })
	release()
	c.until("vm2 naps", c.gaveUp("vm2"))
	if !c.gaveUp("vm1")() || c.hypervisor("vm1") || len(c.events("vm1", "wake")) != 0 {
		t.Fatalf("with a slot free and no command for it: %+v, hypervisor %v: the VM must nap on", c.state("vm1"), c.hypervisor("vm1"))
	}
	if code, out, said := c.ask("vm1"); code != 0 || out != "ran\n" {
		t.Fatalf("the next command for it: exit %d, printed %q, said %q: want it run", code, out, said)
	}
}

// A wait for a slot counts less often the more records its state directory
// holds: each count reads them all under the lock every verb takes.
func TestCloudHVASlotWaitCountsLessOftenAmongManyRecords(t *testing.T) {
	t.Setenv(envCloudHVAwakeWait, "2s")
	c := cloudhvFixture(t, 1)
	c.napSlots(1)
	c.live("vm1")
	c.boot("vm1", false)
	c.until("vm1 naps", c.gaveUp("vm1"))
	c.live("vm2")
	c.boot("vm2", false)
	defer c.hold("vm2")()
	st := store{c.l.state}
	if err := st.locked(func() error {
		for i := 0; i < 4*cloudhvAwakePollRecords; i++ { // ended long ago: five polls to a count
			if err := st.save(record{ID: fmt.Sprintf("old%d", i), Kind: "box", OS: "linux", State: stLost}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	c.refused("vm1", "among many records")
	ev := c.events("vm1", "wake_refused")
	if len(ev) != 1 || ev[0]["counts"].(float64) < 1 || ev[0]["counts"].(float64) > 3 {
		t.Fatalf("a wait of 2 s among %d records: %v, want it to have counted at most 3 times", 4*cloudhvAwakePollRecords, ev)
	}
}

// A wake's event tells its wait for a slot from the restore.
func TestCloudHVAWakeSaysHowLongItWaitedForItsSlot(t *testing.T) {
	t.Setenv(envCloudHVAwakeWait, "30s")
	c := cloudhvFixture(t, 1)
	c.napSlots(1)
	c.live("vm1")
	c.boot("vm1", false)
	c.until("vm1 naps", c.gaveUp("vm1"))
	c.live("vm2")
	c.boot("vm2", false)
	release := c.hold("vm2")
	ran := make(chan int, 1)
	go func() { code, _, _ := c.ask("vm1"); ran <- code }()
	c.until("vm1's wake waits for a slot", func() bool { return len(c.events("vm1", "slot_wait")) == 1 })
	time.Sleep(500 * time.Millisecond)
	release()
	if code := <-ran; code != 0 {
		t.Fatalf("the command that waited for a slot: exit %d", code)
	}
	ev := c.events("vm1", "wake")
	if len(ev) != 1 || ev[0]["slot_wait_ms"].(float64) < 500 || ev[0]["restore_ms"].(float64) >= ev[0]["slot_wait_ms"].(float64) {
		t.Fatalf("the wake's event: %v, want a slot wait of 500 ms or more, and a restore that leaves it out", ev)
	}
}

// A guest that cannot write its files out just before the pause is not
// napped: its VM stays awake, whole, and runs commands.
func TestCloudHVANapWithoutItsLastSyncIsOff(t *testing.T) {
	for name, slots := range map[string]bool{"a VM that keeps its slot": false, "a VM that gives its slot up": true} {
		t.Run(name, func(t *testing.T) {
			c := cloudhvFixture(t, 1)
			if slots {
				c.napSlots(1)
				c.live("vm1")
			}
			if err := c.l.Clone("img", "vm1", 2, 512); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(c.l.dir("vm1"), "sync-fails"), nil, 0o600); err != nil {
				t.Fatal(err)
			}
			c.boot("vm1", false)
			squeezes := func() int {
				b, _ := os.ReadFile(filepath.Join(c.l.dir("vm1"), "squeezed"))
				return strings.Count(string(b), "\n")
			}
			// Each try squeezes the guest, which works, and then fails at the sync.
			c.until("the monitor has tried every nap it makes", func() bool { return squeezes() >= cloudhvNapRetries })
			time.Sleep(1500 * time.Millisecond) // a further try would come by now
			if st := c.state("vm1"); squeezes() != cloudhvNapRetries || st.State != "awake" || st.Naps != 0 || c.has("vm1", "snap/sealed") {
				t.Fatalf("after %d squeezes, each followed by a sync that failed: %+v, snapshot %v: want the VM awake after %d tries", squeezes(), st, c.has("vm1", "snap/sealed"), cloudhvNapRetries)
			}
			if got := c.run("vm1", "sh", "-c", "echo ok"); got != "ok" {
				t.Errorf("a command after the failed naps: %q", got)
			}
		})
	}
}

// A command that waited behind one that left is refused 10 seconds after it
// asked itself: the wait is not begun again for it.
func TestCloudHVACommandBehindOneThatLeftKeepsItsOwnWait(t *testing.T) {
	const wait = 4 * time.Second
	t.Setenv(envCloudHVAwakeWait, wait.String())
	c := cloudhvFixture(t, 1)
	c.napSlots(1)
	c.live("vm1")
	c.boot("vm1", false)
	c.until("vm1 naps", c.gaveUp("vm1"))
	c.live("vm2")
	c.boot("vm2", false)
	defer c.hold("vm2")()
	first := c.l.Exec("vm1", nil, io.Discard, io.Discard, "true")
	if err := first.Start(); err != nil {
		t.Fatal(err)
	}
	c.until("vm1's wake waits for a slot", func() bool { return len(c.events("vm1", "slot_wait")) == 1 })
	type answer struct {
		code    int
		refused bool
		took    time.Duration
	}
	second := make(chan answer, 1)
	go func() {
		t0 := time.Now()
		code, _, _, refused := c.asked("vm1", "true")
		second <- answer{code, refused, time.Since(t0)}
	}()
	time.Sleep(3 * time.Second)
	_ = first.Process.Kill()
	_ = first.Wait()
	if a := <-second; a.code != 255 || !a.refused || a.took < wait-300*time.Millisecond || a.took > wait+1500*time.Millisecond {
		t.Fatalf("the command behind one that left after 3 s: exit %d, refused %v, after %s: want it refused %s after it asked", a.code, a.refused, a.took.Round(10*time.Millisecond), wait)
	}
	if n := len(c.events("vm1", "wake_dropped")); n != 1 {
		t.Fatalf("%d wakes dropped for the command that left, want 1", n)
	}
}

// A command that stood behind the monitor's own nap for longer than its wait
// is taken up with the wait already over. It is still given its count: a
// lock that is busy for a moment just then does not refuse it while the
// host's slot is free.
func TestCloudHVACommandTakenUpLateIsStillCounted(t *testing.T) {
	t.Setenv(envCloudHVAwakeWait, "2s")
	c := cloudhvFixture(t, 1)
	c.napSlots(1)
	c.live("vm1")
	c.made("vm1", "slow-shutdown")
	c.boot("vm1", false)
	c.until("the nap's shutdown has begun", func() bool { return c.has("vm1", "flushed") })
	type answer struct {
		code    int
		said    string
		refused bool
	}
	got := make(chan answer, 1)
	go func() {
		code, _, said, refused := c.asked("vm1", "true")
		got <- answer{code, said, refused}
	}()
	time.Sleep(3 * time.Second) // the command has stood behind the nap for longer than its 2 s
	st := store{c.l.state}
	if err := st.locked(func() error {
		_ = os.Remove(filepath.Join(c.l.dir("vm1"), "slow-shutdown")) // the nap ends now,
		time.Sleep(400 * time.Millisecond)                            // while a verb of the driver holds the lock
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if a := <-got; a.code != 0 || a.refused {
		t.Fatalf("a command taken up after its wait, the lock busy for 0.4 s and the slot free: exit %d, refused %v, said %q; refusals %v", a.code, a.refused, a.said, c.events("vm1", "wake_refused"))
	}
}

// A box told to stop while its wake waits takes no slot, though one came
// free in the same moment: its command is refused and the box is not woken.
func TestCloudHVAStopTakesNoSlotThatCameFree(t *testing.T) {
	t.Setenv(envCloudHVAwakeWait, "30s")
	c := cloudhvFixture(t, 1)
	c.napSlots(1)
	c.live("vm1")
	cmd, _ := c.boot("vm1", true)
	c.until("vm1 naps", c.gaveUp("vm1"))
	gone := c.live("other") // a box of another layer in the slot
	st := store{c.l.state}
	if err := st.locked(func() error { // counts five seconds apart
		for i := 0; i < 20*cloudhvAwakePollRecords; i++ {
			if err := st.save(record{ID: fmt.Sprintf("old%d", i), Kind: "box", OS: "linux", State: stLost}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	type answer struct {
		code      int
		out, said string
	}
	asked := make(chan answer, 1)
	go func() { code, out, said := c.ask("vm1"); asked <- answer{code, out, said} }()
	c.until("vm1's wake waits for a slot", func() bool { return len(c.events("vm1", "slot_wait")) == 1 })
	gone() // the slot comes free
	c.l.PowerOff("vm1", cmd.Process.Pid)
	if a := <-asked; a.code == 0 || len(c.events("vm1", "wake")) != 0 {
		t.Fatalf("a box told to stop was woken for the command that waited for its slot: exit %d, printed %q, said %q", a.code, a.out, a.said)
	}
}
