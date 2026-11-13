package boxproc

import (
	"encoding/json"
	"io"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// A process on a terminal: the size asked, input reaching it, a resize, a
// Ctrl-C its line discipline turns into SIGINT, and input after the end.
func TestPTYInputResizeInterrupt(t *testing.T) {
	ps := Procs(t.TempDir())
	d, err := ps.New("r1")
	if err != nil {
		t.Fatal(err)
	}
	m, _ := d.Meta()
	m.PTY = true
	if err := d.SetMeta(m); err != nil {
		t.Fatal(err)
	}
	done := make(chan int, 1)
	go func() {
		done <- Hold(d, func(stdout, _ io.Writer) (func() int, error) {
			cmd := exec.Command("sh", "-c", `test -t 0 && test -t 1 || exit 9; stty size; read x; echo "got:$x"; stty size; read y; echo never`)
			wait, err := StartPTY(d, cmd, PTY{Rows: 24, Cols: 80}, nil, stdout, nil)
			if err != nil {
				return nil, err
			}
			m, _ := d.Meta()
			m.Holder = 0
			_ = d.SetMeta(m)
			return func() int { _ = wait(); return ExitCode(cmd.ProcessState) }, nil
		})
	}()
	var seen strings.Builder
	var next int64
	until := func(want string) {
		t.Helper()
		for deadline := time.Now().Add(10 * time.Second); !strings.Contains(seen.String(), want); {
			if !time.Now().Before(deadline) {
				t.Fatalf("no %q in %q", want, seen.String())
			}
			o, err := Output(d, OutputArgs{PID: d.ID(), StdoutFrom: next, MaxBytes: DefaultBytes, WaitS: 1}, func() error { return nil })
			if err != nil {
				t.Fatal(err)
			}
			seen.WriteString(o["stdout"].(string))
			next = o["stdout_next"].(int64)
		}
	}
	until("24 80")
	in := func(kv string) map[string]any {
		t.Helper()
		a, data, err := ParseInput(json.RawMessage(kv))
		if err != nil {
			t.Fatal(err)
		}
		out, err := Input(d, a, data)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	if out := in(`{"pid":"x","rows":30,"cols":100,"data":"hello\n"}`); out["written"] != 6 || out["running"] != true {
		t.Fatalf("input: %v", out)
	}
	until("got:hello")
	until("30 100")
	in(`{"pid":"x","data":"\u0003"}`)
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Ctrl-C did not end the process")
	}
	if x := d.Exit(); x == nil || *x != 130 {
		t.Fatalf("exit %v, want 130 (SIGINT); output %q", x, seen.String())
	}
	if out := in(`{"pid":"x","data":"late"}`); out["running"] != false || out["written"] != 0 {
		t.Errorf("input after the end: %v", out)
	}
}

func TestParsePTYAndInputBounds(t *testing.T) {
	for _, bad := range []string{
		`{"pty":{"rows":0,"cols":80}}`, `{"pty":{"rows":24,"cols":1001}}`, `{"pty":{"rows":24,"cols":80,"term":"x y"}}`,
		`{"pty":{"rows":24,"cols":80},"secrets":{"A":"b"}}`,
	} {
		if _, err := ParsePTY(json.RawMessage(bad)); err == nil {
			t.Errorf("ParsePTY %s: no refusal", bad)
		}
	}
	p, err := ParsePTY(json.RawMessage(`{"pty":{"rows":24,"cols":80}}`))
	if err != nil || p.Term != DefaultTerm {
		t.Errorf("ParsePTY: %+v %v", p, err)
	}
	if p, err := ParsePTY(json.RawMessage(`{"argv":["sh"]}`)); p != nil || err != nil {
		t.Errorf("no pty: %+v %v", p, err)
	}
	for _, bad := range []string{`{}`, `{"pid":"p","rows":3}`, `{"pid":"p","rows":3,"cols":1001}`, `{"pid":"p","data":"!","encoding":"base64"}`,
		`{"pid":"p","data":"` + strings.Repeat("a", MaxInput+1) + `"}`} {
		if _, _, err := ParseInput(json.RawMessage(bad)); err == nil {
			t.Errorf("ParseInput %.40s: no refusal", bad)
		}
	}
	ps := Procs(t.TempDir())
	d, _ := ps.New("plain")
	if _, err := Input(d, InputArgs{PID: d.ID()}, []byte("x")); err == nil || !strings.Contains(err.Error(), "no terminal") {
		t.Errorf("input to a process with no terminal: %v", err)
	}
}

// A waiting output call looks often only on a terminal (zm-eakh.12.5.21
// review): every other waiter keeps the 50 ms tick.
func TestPollTick(t *testing.T) {
	ps := Procs(t.TempDir())
	plain, _ := ps.New("plain")
	tty, _ := ps.New("tty")
	m, _ := tty.Meta()
	m.PTY = true
	_ = tty.SetMeta(m)
	if a, b := pollTick(plain), pollTick(tty); a != 50*time.Millisecond || b != 10*time.Millisecond {
		t.Errorf("pollTick: plain %s, terminal %s; want 50ms and 10ms", a, b)
	}
}
