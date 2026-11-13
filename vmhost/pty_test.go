package vmhost

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

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
	sp, err := call(t, m, "spawn", h, map[string]any{"request_id": "t1", "argv": []string{"sh", "-c", script},
		"pty": map[string]any{"rows": 24, "cols": 80, "term": "vt100"}})
	if err != nil {
		t.Fatal(err)
	}
	pid := sp["pid"].(string)
	var seen strings.Builder
	next := 0.0
	until := func(want string) {
		t.Helper()
		for deadline := time.Now().Add(15 * time.Second); !strings.Contains(seen.String(), want); {
			o, err := call(t, m, "output", h, map[string]any{"pid": pid, "stdout_from": next, "wait_s": 1})
			if err != nil {
				t.Fatal(err)
			}
			seen.WriteString(o["stdout"].(string))
			next = o["stdout_next"].(float64)
			if !time.Now().Before(deadline) || o["running"] == false && !strings.Contains(seen.String(), want) {
				t.Fatalf("no %q in %q (%v)", want, seen.String(), o)
			}
		}
	}
	until("24 80")
	until("term=vt100 home=")
	if strings.Contains(seen.String(), "\n") && !strings.Contains(seen.String(), "\r\n") {
		t.Errorf("a terminal's output has no CR LF: %q", seen.String())
	}
	if in, err := call(t, m, "input", h, map[string]any{"pid": pid, "rows": 30, "cols": 100, "data": "hello\n"}); err != nil || in["written"] != 6.0 || in["running"] != true {
		t.Fatalf("input: %v %v", in, err)
	}
	until("got:hello")
	until("30 100")
	if _, err := call(t, m, "input", h, map[string]any{"pid": pid, "data": "\x03"}); err != nil {
		t.Fatal(err)
	}
	o, err := call(t, m, "output", h, map[string]any{"pid": pid, "stdout_from": next, "wait_s": 10})
	for err == nil && o["running"] == true {
		o, err = call(t, m, "output", h, map[string]any{"pid": pid, "stdout_from": o["stdout_next"], "wait_s": 10})
	}
	if err != nil || o["exit"] != 130.0 {
		t.Fatalf("after Ctrl-C: %v %v, want exit 130", o, err)
	}
	if in, err := call(t, m, "input", h, map[string]any{"pid": pid, "data": "x"}); err != nil || in["running"] != false {
		t.Errorf("input after the end: %v %v", in, err)
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
		{tartCLI{"tart"}, "tart exec -i -t vm sh -c x"},
		{ociCLI{bin: "docker"}, "docker exec -i -t vm sh -c x"},
		{incusCLI{bin: "incus", remote: "r", project: "zm"}, "incus exec r:vm --project zm --force-interactive -- sh -c x"},
	} {
		cmd := c.l.ExecTTY("vm", "sh", "-c", "x")
		if got := strings.Join(append([]string{filepath.Base(cmd.Path)}, cmd.Args[1:]...), " "); got != c.want || cmd.Stdin != nil || cmd.Stdout != nil {
			t.Errorf("ExecTTY: %q, want %q", got, c.want)
		}
	}
}
