package vmhost

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// envSignalWait shortens, for a holder a test starts, the wait for the
// guest command that ends a terminal's process at its timeout.
const envSignalWait = "VMHOST_TEST_SIGNAL_WAIT"

// boxTerm is one process on a terminal in a box, through the driver's
// verbs: what its terminal showed so far, and its end.
type boxTerm struct {
	t      *testing.T
	m      map[string]string
	h, pid string
	seen   strings.Builder
	next   float64
	last   map[string]any
}

// openTerm spawns argv on a terminal of rows by cols; more is spawn's other
// args, in pairs.
func openTerm(t *testing.T, m map[string]string, h string, rows, cols int, argv []string, more ...any) *boxTerm {
	t.Helper()
	args := map[string]any{"request_id": fmt.Sprintf("t%d", time.Now().UnixNano()), "argv": argv, "pty": map[string]any{"rows": rows, "cols": cols, "term": "vt100"}}
	for i := 0; i < len(more); i += 2 {
		args[more[i].(string)] = more[i+1]
	}
	sp, err := call(t, m, "spawn", h, args)
	if err != nil {
		t.Fatal(err)
	}
	return &boxTerm{t: t, m: m, h: h, pid: sp["pid"].(string)}
}

func (b *boxTerm) read(wait int) {
	b.t.Helper()
	o, err := call(b.t, b.m, "output", b.h, map[string]any{"pid": b.pid, "stdout_from": b.next, "wait_s": wait})
	if err != nil {
		b.t.Fatal(err)
	}
	b.seen.WriteString(o["stdout"].(string))
	b.next, b.last = o["stdout_next"].(float64), o
}

// until reads the terminal until it showed want.
func (b *boxTerm) until(want string) {
	b.t.Helper()
	for deadline := time.Now().Add(30 * time.Second); !strings.Contains(b.seen.String(), want); {
		b.read(1)
		if !time.Now().Before(deadline) || b.last["running"] == false && !strings.Contains(b.seen.String(), want) {
			b.t.Fatalf("no %q in %q (%v)", want, b.seen.String(), b.last)
		}
	}
}

// end reads the terminal to its process's exit code.
func (b *boxTerm) end() float64 {
	b.t.Helper()
	for deadline := time.Now().Add(30 * time.Second); b.last == nil || b.last["running"] == true; {
		if b.read(5); !time.Now().Before(deadline) {
			b.t.Fatalf("the process never ended: %q (%v)", b.seen.String(), b.last)
		}
	}
	return b.last["exit"].(float64)
}

func (b *boxTerm) input(kv ...any) map[string]any {
	b.t.Helper()
	args := map[string]any{"pid": b.pid}
	for i := 0; i < len(kv); i += 2 {
		args[kv[i].(string)] = kv[i+1]
	}
	in, err := call(b.t, b.m, "input", b.h, args)
	if err != nil {
		b.t.Fatal(err)
	}
	return in
}

// A process on a terminal over the fake layer (zm-eakh.12.5.21): describe
// lists input and pty, the command sees a terminal of the size asked in
// its cwd with TERM set, input reaches it, a resize and a Ctrl-C reach it,
// and a terminal refuses secrets.
func TestBoxTerminal(t *testing.T) {
	m, h := fakeBox(t)
	d, _ := call(t, m, "describe", "", nil)
	if !strings.Contains(strings.Join(anyStrings(d["verbs"]), ","), "input") || !strings.Contains(strings.Join(anyStrings(d["capabilities"]), ","), "pty") {
		t.Fatalf("describe: verbs %v, capabilities %v", d["verbs"], d["capabilities"])
	}
	script := `test -t 0 && test -t 1 || exit 9; stty size; echo "term=$TERM home=$(pwd)"; read x; echo "got:$x"; stty size; read y; echo never`
	b := openTerm(t, m, h, 24, 80, []string{"sh", "-c", script})
	pid := b.pid
	b.until("24 80")
	b.until("term=vt100 home=")
	if strings.Contains(b.seen.String(), "\n") && !strings.Contains(b.seen.String(), "\r\n") {
		t.Errorf("a terminal's output has no CR LF: %q", b.seen.String())
	}
	if in := b.input("rows", 30, "cols", 100, "data", "hello\n"); in["written"] != 6.0 || in["running"] != true {
		t.Fatalf("input: %v", in)
	}
	b.until("got:hello")
	b.until("30 100")
	b.input("data", "\x03")
	if code := b.end(); code != 130 {
		t.Fatalf("after Ctrl-C: exit %v, want 130; the terminal showed %q", code, b.seen.String())
	}
	if in := b.input("data", "x"); in["running"] != false {
		t.Errorf("input after the end: %v", in)
	}
	for name, args := range map[string]map[string]any{
		"pty with secrets": {"request_id": "t2", "argv": []string{"sh"}, "pty": map[string]any{"rows": 24, "cols": 80}, "secrets": map[string]string{"A": "b"}},
		"pty rows 0":       {"request_id": "t3", "argv": []string{"sh"}, "pty": map[string]any{"rows": 0, "cols": 80}},
	} {
		if _, err := call(t, m, "spawn", h, args); code(err) != "bad_request" {
			t.Errorf("spawn %s: %v", name, err)
		}
	}
	plain, err := call(t, m, "spawn", h, map[string]any{"request_id": "t4", "argv": []string{"sleep", "5"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := call(t, m, "input", h, map[string]any{"pid": plain["pid"], "data": "x"}); code(err) != "bad_request" {
		t.Errorf("input to a process with no terminal: %v", err)
	}
	if _, err := call(t, m, "input", h, map[string]any{"pid": "p000000000000", "data": "x"}); code(err) != "not_found" {
		t.Errorf("input to an unknown pid: %v", err)
	}
	if _, err := call(t, m, "release", h, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := call(t, m, "input", h, map[string]any{"pid": pid, "data": "x"}); code(err) != "conflict" {
		t.Errorf("input after release: %v", err)
	}
}

// ExecTTY on the fake layer: the "guest" command runs on the host terminal
// the holder attaches, as a real layer's guest runs on one sized like it.
func (l dirLayer) ExecTTY(vm string, argv ...string) *exec.Cmd {
	cmd := l.Exec(vm, nil, nil, nil, argv...)
	cmd.SysProcAttr = nil
	return cmd
}

// Each real layer's terminal exec: its own tty flag, stdin attached, no
// stdio set (the holder attaches its terminal).
func TestLayersExecTTY(t *testing.T) {
	for _, c := range []struct {
		l    ttyLayer
		want string
	}{
		{tartCLI{bin: "tart"}, "tart exec -i -t vm sh -c x"},
		{ociCLI{bin: "docker"}, "docker exec -i -t vm sh -c x"},
		{incusCLI{bin: "incus", remote: "r", project: "zm"}, "incus exec r:vm --project zm --force-interactive -- sh -c x"},
	} {
		cmd := c.l.ExecTTY("vm", "sh", "-c", "x")
		if got := strings.Join(append([]string{filepath.Base(cmd.Path)}, cmd.Args[1:]...), " "); got != c.want || cmd.Stdin != nil || cmd.Stdout != nil {
			t.Errorf("ExecTTY: %q, want %q", got, c.want)
		}
	}
}
