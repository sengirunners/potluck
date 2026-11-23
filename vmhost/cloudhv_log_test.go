package vmhost

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// hypervisorStreams says whether the tests' guest can write to its
// hypervisor's streams, as a guest can make a real one say things: it
// reaches them through /proc.
func hypervisorStreams() bool {
	_, err := os.Stat("/proc/self/fd/2")
	return err == nil
}

// logParts are the parts of vm's log name, older first, and how many files
// of that name its directory has.
func (c chTest) logParts(vm, name string) (older, newer []byte, files int) {
	parts := cloudhvLogFiles(filepath.Join(c.l.dir(vm), name))
	newer, _ = os.ReadFile(parts[0])
	older, _ = os.ReadFile(parts[1])
	all, _ := filepath.Glob(parts[0] + "*")
	return older, newer, len(all)
}

// says has the tests' guest write script's output to stream fd of its
// hypervisor (1 or 2), and fails the test when that does not end.
func (c chTest) says(vm string, fd int, script string) {
	c.t.Helper()
	cmd := c.l.Exec(vm, nil, io.Discard, io.Discard, "sh", "-c", fmt.Sprintf("{ %s; } > /proc/$PPID/fd/%d", script, fd))
	late := time.AfterFunc(30*time.Second, func() { _ = cmd.Process.Kill() })
	defer late.Stop()
	if err := cmd.Run(); err != nil {
		c.t.Fatalf("the guest could not make its hypervisor say that on stream %d: %v", fd, err)
	}
}

// said waits until vm's log name holds mark in its newer part.
func (c chTest) said(vm, name, mark string) {
	c.t.Helper()
	c.until(name+" of "+vm+" holds "+mark, func() bool {
		_, newer, _ := c.logParts(vm, name)
		return bytes.Contains(newer, []byte(mark))
	})
}

// A guest that makes its hypervisor say a great deal, and one that floods
// its serial console, each fill two files of cloudhvLogMax and no more,
// which hold the end of what was said; and what is said after a nap and a
// wake is kept too. With no console asked for there is no console.log. A
// kept VM's stop leaves its hypervisor's log and its record of naps, and
// nothing of its console.
func TestCloudHVLogsAreBounded(t *testing.T) {
	if !hypervisorStreams() {
		t.Skip("the tests' guest reaches its hypervisor's streams through /proc")
	}
	c := cloudhvFixture(t, 1)
	c.l.c.Console = true
	c.boot("vm1", false)
	release := c.hold("vm1")
	flood := `echo MARK-START; head -c 1500000 /dev/zero | tr '\0' x | fold -w 100; echo MARK-END`
	for fd, name := range map[int]string{2: cloudhvHypervisorLog, 1: cloudhvConsoleLog} {
		c.says("vm1", fd, flood)
		c.said("vm1", name, "MARK-END")
		older, newer, files := c.logParts("vm1", name)
		if files != 2 || len(older) == 0 || len(older) > cloudhvLogMax || len(newer) > cloudhvLogMax {
			t.Errorf("%s: %d files, of %d and %d bytes, want two of at most %d", name, files, len(older), len(newer), cloudhvLogMax)
		}
		if bytes.Contains(older, []byte("MARK-START")) || bytes.Contains(newer, []byte("MARK-START")) {
			t.Errorf("%s still holds the start of 1.5 MB said", name)
		}
	}
	release()
	c.until("the VM naps", func() bool { return c.state("vm1").State == "napping" })
	c.says("vm1", 1, "echo CONSOLE-AFTER-A-WAKE")
	c.says("vm1", 2, "echo SAID-AFTER-A-WAKE")
	c.said("vm1", cloudhvConsoleLog, "CONSOLE-AFTER-A-WAKE")
	c.said("vm1", cloudhvHypervisorLog, "SAID-AFTER-A-WAKE")

	c.l.c.Console = false
	c.boot("vm2", false)
	c.says("vm2", 1, "echo ON-STDOUT")
	c.said("vm2", cloudhvHypervisorLog, "ON-STDOUT")
	if _, _, files := c.logParts("vm2", cloudhvConsoleLog); files != 0 {
		t.Errorf("a VM with no console asked for has %d console files", files)
	}

	c.l.c.Console = true
	cmd, _ := c.boot("vm3", true)
	c.says("vm3", 1, flood)
	c.says("vm3", 2, "echo KEPT-SAID")
	c.until("the kept VM's console is in two files", func() bool {
		_, _, files := c.logParts("vm3", cloudhvConsoleLog)
		return files == 2
	})
	c.until("the kept VM naps", func() bool { return c.state("vm3").State == "napping" })
	c.l.PowerOff("vm3", cmd.Process.Pid)
	if _, _, files := c.logParts("vm3", cloudhvConsoleLog); files != 0 || !c.has("vm3", "disk.qcow2") {
		t.Errorf("a stopped kept VM has %d console files, want none beside its disk", files)
	}
	if _, said, _ := c.logParts("vm3", cloudhvHypervisorLog); !bytes.Contains(said, []byte("KEPT-SAID")) || len(c.events("vm3", "nap")) == 0 {
		t.Errorf("a stopped kept VM's hypervisor log is %d bytes and its record has %d naps, want both kept", len(said), len(c.events("vm3", "nap")))
	}
}

// The record of naps and wakes is kept in parts too: a nearly full one is
// set aside at the next line, and one found larger than a part may be is
// started again.
func TestCloudHVEventsAreBounded(t *testing.T) {
	c := cloudhvFixture(t, 1)
	full := bytes.Repeat([]byte("{}\n"), (cloudhvLogMax-30)/3)
	for vm, found := range map[string][]byte{"vm1": full, "vm2": bytes.Repeat(full, 3)} {
		c.made(vm)
		if err := os.WriteFile(filepath.Join(c.l.dir(vm), cloudhvEventsLog), found, 0o600); err != nil {
			t.Fatal(err)
		}
		c.boot(vm, false)
	}
	for vm, want := range map[string]int{"vm1": len(full), "vm2": 0} {
		c.until(vm+" records its nap", func() bool { return len(c.events(vm, "nap")) > 0 })
		older, newer, _ := c.logParts(vm, cloudhvEventsLog)
		if len(older) != want || len(c.events(vm, "nap")) == 0 || len(newer) > 4096 {
			t.Errorf("%s's record of naps: an older part of %d bytes, want %d, and a newer of %d bytes with %d naps, want a short one with the nap",
				vm, len(older), want, len(newer), len(c.events(vm, "nap")))
		}
	}
}

// A log that cannot be written is dropped: the hypervisor that says it is
// neither stopped nor ended, and its guest goes on.
func TestCloudHVLogThatCannotBeWritten(t *testing.T) {
	if !hypervisorStreams() {
		t.Skip("the tests' guest reaches its hypervisor's streams through /proc")
	}
	c := cloudhvFixture(t, 0)
	c.made("vm1")
	if err := os.Mkdir(filepath.Join(c.l.dir("vm1"), cloudhvHypervisorLog), 0o700); err != nil { // no file can be opened under this name
		t.Fatal(err)
	}
	c.boot("vm1", false)
	c.says("vm1", 2, "head -c 1500000 /dev/zero | tr '\\0' x | fold -w 100")
	if got := c.run("vm1", "echo", "still-here"); got != "still-here" {
		t.Errorf("after its hypervisor said 1.5 MB to a log that cannot be written, the guest answered %q", got)
	}
}

// One write larger than a part of a log leaves the end of it, in one part.
func TestCloudHVLogTakesOneLargeWrite(t *testing.T) {
	l := cloudhvLog{Name: filepath.Join(t.TempDir(), "log")}
	big := append(bytes.Repeat([]byte("x"), cloudhvLogMax+5000), "MARK-END"...)
	for range 2 {
		if n, err := l.Write(big); n != len(big) || err != nil {
			t.Fatalf("write: %d, %v", n, err)
		}
	}
	for _, name := range cloudhvLogFiles(l.Name) {
		if b, _ := os.ReadFile(name); len(b) != cloudhvLogMax || !bytes.HasSuffix(b, []byte("MARK-END")) {
			t.Errorf("%s holds %d bytes, want the last %d of the write", filepath.Base(name), len(b), cloudhvLogMax)
		}
	}
	// The end of a log, for another log to quote, is read across its parts.
	_, _ = l.Write([]byte("-AND-MORE\n"))
	if got := cloudhvLogEnd(l.Name, 20); got != "xxMARK-END-AND-MORE" {
		t.Errorf("the last 20 bytes of a log whose newer part holds 10, trimmed: %q", got)
	}
	if got := cloudhvLogEnd(l.Name, 5); got != "MORE" {
		t.Errorf("the last 5 bytes of that log, trimmed: %q", got)
	}
	if got := cloudhvLogEnd(l.Name+".none", 20) + cloudhvLogEnd(l.Name, 0); got != "" {
		t.Errorf("the end of a log that is not there, and no bytes of one that is: %q", got)
	}
	if want := int64(2*cloudhvLogMax + 10); l.Written() != want {
		t.Errorf("the log says it has written %d bytes, want %d", l.Written(), want)
	}
	// What gives nothing leaves no file.
	none := cloudhvLog{Name: filepath.Join(t.TempDir(), "log")}
	if _, err := io.Copy(&none, &reads{r: strings.NewReader("")}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(none.Name); err == nil {
		t.Error("a log copied to from nothing has a file")
	}
}

// bootLogged boots vm, cloned first when it is not there, and answers the
// run's log, which the test reads while the VM's monitor writes it.
func (c chTest) bootLogged(vm string, kept bool) (*lockedBuffer, *exec.Cmd) {
	c.t.Helper()
	clone, boot := c.l.Clone, c.l.Boot
	if kept {
		clone, boot = c.l.CloneKept, c.l.BootKept
	}
	if !c.has(vm, "vm.json") {
		if err := clone("img", vm, 2, 512); err != nil {
			c.t.Fatal(err)
		}
	}
	var runlog lockedBuffer
	cmd, held, err := boot(vm, true, &runlog)
	if err != nil {
		c.t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	c.t.Cleanup(func() { c.l.Destroy(vm, cmd.Process.Pid); held.Close(); <-done })
	c.until("the guest answers", func() bool { return c.l.Exec(vm, nil, io.Discard, io.Discard, "true").Run() == nil })
	return &runlog, cmd
}

// hypervisorExits kills vm's hypervisor and answers what the run's log then
// has for its last words.
func (c chTest) hypervisorExits(vm string, runlog *lockedBuffer) string {
	c.t.Helper()
	if err := syscall.Kill(c.state(vm).HypervisorPID, syscall.SIGKILL); err != nil {
		c.t.Fatal(err)
	}
	c.until("the run's log says the hypervisor exited", func() bool { return strings.Contains(runlog.String(), "its log ends: ") })
	_, said, _ := strings.Cut(runlog.String(), "its log ends: ")
	return strings.TrimSpace(said)
}

// A hypervisor that exits by itself leaves the end of what it said in the
// run's log: its own log goes with the VM's directory. What a hypervisor of
// an earlier boot said, or the one before a nap, is not taken for its words.
func TestCloudHVHypervisorThatExitsLeavesWhatItSaid(t *testing.T) {
	if !hypervisorStreams() {
		t.Skip("the tests' guest reaches its hypervisor's streams through /proc")
	}
	const nothing = "(nothing from it is in its log)"
	c := cloudhvFixture(t, 0)
	_, cmd := c.bootLogged("vm1", true)
	c.says("vm1", 2, "echo WORDS-OF-AN-EARLIER-BOOT")
	c.said("vm1", cloudhvHypervisorLog, "WORDS-OF-AN-EARLIER-BOOT")
	c.l.PowerOff("vm1", cmd.Process.Pid)
	runlog, _ := c.bootLogged("vm1", true)
	if _, said, _ := c.logParts("vm1", cloudhvHypervisorLog); !bytes.Contains(said, []byte("WORDS-OF-AN-EARLIER-BOOT")) {
		t.Fatalf("the kept VM's hypervisor log holds %q, want what was said before its stop", said)
	}
	if said := c.hypervisorExits("vm1", runlog); said != nothing {
		t.Errorf("a hypervisor that said nothing exited, and the run's log has for its words: %q", said)
	}
	c.until("the VM's monitor has ended", func() bool { return !cloudhvMonAlive(c.l.dir("vm1")) })
	runlog, _ = c.bootLogged("vm1", true)
	c.says("vm1", 2, "echo LAST-WORDS")
	c.said("vm1", cloudhvHypervisorLog, "LAST-WORDS")
	if said := c.hypervisorExits("vm1", runlog); said != "LAST-WORDS" {
		t.Errorf("a hypervisor that said LAST-WORDS exited, and the run's log has for its words: %q", said)
	}

	c = cloudhvFixture(t, 1)
	runlog, _ = c.bootLogged("vm2", false)
	c.says("vm2", 2, "echo WORDS-BEFORE-THE-NAP")
	c.said("vm2", cloudhvHypervisorLog, "WORDS-BEFORE-THE-NAP")
	c.until("the VM naps", func() bool { return c.state("vm2").State == "napping" })
	release := c.hold("vm2")
	defer release()
	if said := c.hypervisorExits("vm2", runlog); said != nothing {
		t.Errorf("a hypervisor woken from a nap that said nothing exited, and the run's log has for its words: %q", said)
	}
}

// reads counts the reads made of r.
type reads struct {
	r io.Reader
	n int
}

func (r *reads) Read(p []byte) (int, error) {
	r.n++
	return r.r.Read(p)
}

// What comes a byte at a time, as a hypervisor's serial port sends it, is
// copied to a log whole and in far fewer reads than bytes.
func TestCloudHVLogReadsABatchAtATime(t *testing.T) {
	l := cloudhvLog{Name: filepath.Join(t.TempDir(), "log")}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	const sent = 20000
	cmd := exec.Command("sh", "-c", fmt.Sprintf("i=0; while [ $i -lt %d ]; do printf $((i %% 10)); i=$((i + 1)); done", sent))
	cmd.Stdout = w // the shell writes each digit by itself
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	w.Close()
	defer func() { _ = cmd.Wait() }()
	from := &reads{r: r}
	if n, err := io.Copy(&l, from); n != sent || err != nil {
		t.Fatalf("copy: %d, %v", n, err)
	}
	if b, _ := os.ReadFile(l.Name); string(b) != strings.Repeat("0123456789", sent/10) {
		t.Errorf("the log holds %d bytes, want the %d sent a byte at a time, in order", len(b), sent)
	}
	if from.n > sent/5 {
		t.Errorf("%d bytes sent one at a time took %d reads, want a batch at a time", sent, from.n)
	}
}
