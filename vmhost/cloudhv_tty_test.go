package vmhost

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/potluck/protocol"
)

// A terminal in a cloud-hypervisor box, through the driver's verbs over the
// tests' hypervisor, whose guest is the real agent: describe lists it; a
// spawn with pty wakes a napping box and its command runs on a terminal of
// the size asked, which alone echoes and ends lines; a resize reaches it,
// with something typed and with nothing; the box stays awake while the
// terminal is open and naps after a Ctrl-C ended its command.
func TestCloudHVBoxTerminal(t *testing.T) {
	m, dir := napBinding(t)
	d, err := call(t, m, "describe", "", nil)
	if err != nil || !strings.Contains(strings.Join(anyStrings(d["verbs"]), ","), "input") || !strings.Contains(strings.Join(anyStrings(d["capabilities"]), ","), "pty") {
		t.Fatalf("describe: verbs %v, capabilities %v, %v", d["verbs"], d["capabilities"], err)
	}
	h, state := napBox(t, m, dir, "a", false)
	eventually(t, "the box naps", func() bool { return state().State == "napping" })

	script := `test -t 0 && test -t 1 || exit 9; stty size; echo "term=$TERM"; read x; echo "got:$x"; read y; stty size; trap 'stty size' WINCH; echo armed; while :; do sleep 0.05; done`
	b := openTerm(t, m, h, 20, 70, []string{"sh", "-c", script})
	b.until("20 70\r\n")
	b.until("term=vt100\r\n")
	if in := b.input("data", "hello\n"); in["written"] != 6.0 || in["running"] != true {
		t.Fatalf("input: %v", in)
	}
	b.until("got:hello\r\n")
	// The guest's terminal is the only one at work: the host's, which the
	// client stands on, neither echoes what is typed nor ends lines again.
	if n := strings.Count(b.seen.String(), "hello"); n != 2 || strings.Contains(b.seen.String(), "\r\r\n") {
		t.Errorf("what was typed shows %d times, want 2 (the guest terminal's echo, and the command's answer), and no line ended twice: %q", n, b.seen.String())
	}
	b.input("rows", 30, "cols", 100, "data", "\n")
	b.until("30 100\r\n")
	b.until("armed")
	b.input("rows", 41, "cols", 97)
	b.until("41 97\r\n")
	time.Sleep(1500 * time.Millisecond) // past the binding's idle time
	if s := state(); s.State != "awake" {
		t.Errorf("the box is %q with a terminal open, want it awake", s.State)
	}
	b.input("data", "\x03")
	if code := b.end(); code != 130 {
		t.Fatalf("after Ctrl-C: exit %v, want 130; the terminal showed %q", code, b.seen.String())
	}
	eventually(t, "the box naps once its terminal's command ended", func() bool { return state().State == "napping" })
}

// What spawn's stdin types first is the guest terminal's too, though the
// client is not there yet when it is typed: it is echoed once, and a Ctrl-D
// in it ends the guest command's input, not the client's.
func TestCloudHVBoxTerminalTypedFirst(t *testing.T) {
	m, dir := napBinding(t)
	h, _ := napBox(t, m, dir, "a", false)
	b := openTerm(t, m, h, 24, 80, []string{"sh", "-c", `read x; echo "got:$x"`}, "stdin", "hello\n")
	if code := b.end(); code != 0 || b.seen.String() != "hello\r\ngot:hello\r\n" {
		t.Errorf("a line typed first: exit %v, the terminal showed %q, want the guest's echo and the answer", code, b.seen.String())
	}
	b = openTerm(t, m, h, 24, 80, []string{"sh", "-c", `cat; echo cat-done`}, "stdin", "\x04")
	if code := b.end(); code != 0 || !strings.Contains(b.seen.String(), "cat-done") {
		t.Errorf("a Ctrl-D typed first: exit %v, the terminal showed %q, want the guest command's input ended", code, b.seen.String())
	}
}

// What is typed on a box's terminal arrives whole, however much it is: the
// client, on a host terminal that takes a few thousand bytes at a time,
// carries on all of it.
func TestCloudHVBoxTerminalTakesManyBytes(t *testing.T) {
	m, dir := napBinding(t)
	h, _ := napBox(t, m, dir, "a", false)
	b := openTerm(t, m, h, 24, 80, []string{"sh", "-c", `stty -echo; echo ready; head -n 2100 | cksum; echo summed`})
	b.until("ready")
	typed := strings.Repeat(strings.Repeat("0123456789abcdef", 6)+"\n", 2100)
	for rest := typed; rest != ""; {
		n := min(len(rest), 60000)
		if in := b.input("data", rest[:n]); in["written"] != float64(n) {
			t.Fatalf("input of %d bytes: %v", n, in)
		}
		rest = rest[n:]
	}
	b.until("summed")
	cmd := exec.Command("cksum")
	cmd.Stdin = strings.NewReader(typed)
	sum, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	if want := strings.TrimSpace(string(sum)); !strings.Contains(b.seen.String(), want) {
		t.Errorf("the terminal showed %q, want the sum %q of what was typed", b.seen.String(), want)
	}
}

// A terminal's process ends at its timeout and at a signal, with nothing of
// it left in the guest; and at its timeout though the guest does not answer
// the command that ends it there; and a signal the guest's command
// could not send is refused, not counted as sent.
func TestCloudHVBoxTerminalEnds(t *testing.T) {
	bin := t.TempDir()
	t.Setenv("PATH", bin+":"+os.Getenv("PATH")) // the tests' guest runs its commands with this PATH
	t.Setenv(envSignalWait, "300ms")
	m, dir := napBinding(t)
	h, _ := napBox(t, m, dir, "a", false)
	gone := func(what, pid string) {
		t.Helper()
		eventually(t, what+": nothing of it is left in the guest", func() bool {
			return exec.Command("kill", "-0", strings.TrimSpace(pid)).Run() != nil
		})
	}
	pidOf := func(b *boxTerm) string {
		b.until("\r\n")
		return strings.SplitN(b.seen.String(), "\r\n", 2)[0]
	}
	b := openTerm(t, m, h, 24, 80, []string{"sh", "-c", `echo $$; exec sleep 60`}, "timeout_s", 5)
	pid := pidOf(b)
	if code := b.end(); code != 124 {
		t.Errorf("a terminal past its timeout: exit %v, want 124", code)
	}
	gone("a timeout", pid)

	b = openTerm(t, m, h, 24, 80, []string{"sh", "-c", `echo $$; exec sleep 60`})
	pid = pidOf(b)
	if s, err := call(t, m, "signal", h, map[string]any{"pid": b.pid, "signal": "KILL"}); err != nil || s["sent"] != true {
		t.Fatalf("signal: %v %v", s, err)
	}
	if code := b.end(); code != 137 {
		t.Errorf("a terminal's process killed: exit %v, want 137", code)
	}
	gone("a signal", pid)

	// The guest's signal command now hangs: a guest can make it.
	if err := os.WriteFile(filepath.Join(bin, "perl"), []byte("#!/bin/sh\nexec sleep 60\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	b = openTerm(t, m, h, 24, 80, []string{"sh", "-c", `echo $$; exec sleep 60`}, "timeout_s", 5)
	pid = pidOf(b)
	t0 := time.Now()
	if code := b.end(); code != 124 || time.Since(t0) > 9*time.Second {
		t.Errorf("a terminal past its timeout in a guest that does not answer: exit %v after %s, want 124 soon after 5 s", code, time.Since(t0).Round(time.Millisecond))
	}
	gone("a timeout in a guest that does not answer", pid)

	// And one whose signal command fails: signal says so, and sends once it works.
	if err := os.WriteFile(filepath.Join(bin, "perl"), []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	b = openTerm(t, m, h, 24, 80, []string{"sh", "-c", `echo $$; exec sleep 60`})
	pid = pidOf(b)
	if s, err := call(t, m, "signal", h, map[string]any{"pid": b.pid, "signal": "KILL"}); code(err) != "unavailable" {
		t.Errorf("signal in a guest whose signal command fails: %v %v, want unavailable", s, err)
	}
	if err := os.Remove(filepath.Join(bin, "perl")); err != nil {
		t.Fatal(err)
	}
	if s, err := call(t, m, "signal", h, map[string]any{"pid": b.pid, "signal": "KILL"}); err != nil || s["sent"] != true || b.end() != 137 {
		t.Errorf("signal once the guest's signal command works: %v %v, exit %v", s, err, b.last["exit"])
	}
	gone("a signal after one that failed", pid)
}

// A terminal for a napping box that is not woken, its slot being taken: the
// process ends 255 and its output says why.
func TestCloudHVBoxTerminalForABoxNotWoken(t *testing.T) {
	m, dir := napBinding(t)
	ha, sa := napBox(t, m, dir, "a", false)
	eventually(t, "a naps", func() bool { return sa().State == "napping" })
	hb, sb := napBox(t, m, dir, "b", false)
	held := openTerm(t, m, hb, 24, 80, []string{"sh", "-c", "echo up; read x"})
	held.until("up")
	if s := sb(); s.State != "awake" {
		t.Fatalf("b is %q under its terminal, want awake", s.State)
	}
	b := openTerm(t, m, ha, 24, 80, []string{"sh", "-c", "echo ran"})
	if code := b.end(); code != 255 || !strings.Contains(b.seen.String(), protocol.NoFreeSlot("linux")) || strings.Contains(b.seen.String(), "ran") {
		t.Errorf("a terminal for a napping box with the slot taken: exit %v, the terminal showed %q, want 255 and the reason", code, b.seen.String())
	}
	held.input("data", "\n")
	if code := held.end(); code != 0 {
		t.Errorf("the terminal that held the slot ended %v", code)
	}
}

// The layer's terminal exec is the driver itself as the command's client,
// with no stdio set (the holder attaches its terminal) and no refusal mark
// of the driver's own; the layer asks the holder for a raw terminal.
// Started on no terminal, the command still runs on one in the guest, of
// the usual size.
func TestCloudHVExecTTY(t *testing.T) {
	c := cloudhvFixture(t, 0)
	cmd := c.l.ExecTTY("vm", "sh", "-c", "x")
	want := []string{flagCloudHVTTY, c.l.dir("vm"), "sh", "-c", "x"}
	if strings.Join(cmd.Args[1:], "\x00") != strings.Join(want, "\x00") || cmd.Path != c.l.c.Bin || cmd.Stdin != nil || cmd.Stdout != nil || cmd.Stderr != nil {
		t.Errorf("ExecTTY: %s %q, stdio %v %v %v", filepath.Base(cmd.Path), cmd.Args[1:], cmd.Stdin, cmd.Stdout, cmd.Stderr)
	}
	if got := cmd.Env[len(cmd.Env)-1]; got != cloudhvRefusedEnv+"=" {
		t.Errorf("ExecTTY's environment ends %q, want the refusal mark cleared", got)
	}
	if _, raw := vmLayer(c.l).(rawTTYLayer); !raw {
		t.Error("the layer does not ask the holder for a raw terminal")
	}
	c.boot("vm", false)
	var out bytes.Buffer
	cmd = c.l.ExecTTY("vm", "sh", "-c", "test -t 0 && stty size")
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil || strings.TrimSpace(out.String()) != "24 80" {
		t.Errorf("a terminal's command started on no terminal: %v, %q, want 24 80", err, out.String())
	}
}
