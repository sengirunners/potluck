package cloudhvguest

import (
	"bytes"
	"encoding/json"
	"errors"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// serve answers one request on a pipe, as the agent does on a vsock
// connection, and returns the host's end.
func serve(t *testing.T, h Hooks) net.Conn {
	t.Helper()
	host, guest := net.Pipe()
	go Serve(guest, h)
	t.Cleanup(func() { host.Close() })
	return host
}

func TestExecStreamsAndExitCode(t *testing.T) {
	var out, errb bytes.Buffer
	code, err := Do(serve(t, Hooks{}), Request{Op: OpExec, Argv: []string{"sh", "-c", "echo out; echo err >&2; exit 3"}}, nil, &out, &errb)
	if err != nil || code != 3 || out.String() != "out\n" || errb.String() != "err\n" {
		t.Fatalf("code %d, err %v, stdout %q, stderr %q", code, err, out.String(), errb.String())
	}
}

func TestExecStdin(t *testing.T) {
	var out bytes.Buffer
	in := strings.Repeat("line\n", 20000) // more than one frame
	code, err := Do(serve(t, Hooks{}), Request{Op: OpExec, Argv: []string{"cat"}}, strings.NewReader(in), &out, nil)
	if err != nil || code != 0 || out.String() != in {
		t.Fatalf("code %d, err %v, %d bytes back of %d", code, err, out.Len(), len(in))
	}
}

func TestExecNoStdinReadsNothing(t *testing.T) {
	var out bytes.Buffer
	code, err := Do(serve(t, Hooks{}), Request{Op: OpExec, Argv: []string{"sh", "-c", "cat; echo done"}}, nil, &out, nil)
	if err != nil || code != 0 || out.String() != "done\n" {
		t.Fatalf("code %d, err %v, stdout %q", code, err, out.String())
	}
}

func TestExecCannotStartIs127(t *testing.T) {
	var errb bytes.Buffer
	code, err := Do(serve(t, Hooks{}), Request{Op: OpExec, Argv: []string{"/no/such/program"}}, nil, nil, &errb)
	if err != nil || code != 127 || errb.Len() == 0 {
		t.Fatalf("code %d, err %v, stderr %q", code, err, errb.String())
	}
	if code, _ := Do(serve(t, Hooks{}), Request{Op: OpExec}, nil, nil, nil); code != 127 {
		t.Fatalf("an empty argv answered %d, want 127", code)
	}
}

func TestExecKilledBySignal(t *testing.T) {
	code, err := Do(serve(t, Hooks{}), Request{Op: OpExec, Argv: []string{"sh", "-c", "kill -TERM $$"}}, nil, nil, nil)
	if err != nil || code != 128+int(syscall.SIGTERM) {
		t.Fatalf("code %d, err %v", code, err)
	}
}

// A command that leaves a process behind holding its output does not hold
// the answer.
func TestExecAnswerDoesNotWaitForWhatItLeftBehind(t *testing.T) {
	var out bytes.Buffer
	t0 := time.Now()
	code, err := Do(serve(t, Hooks{}), Request{Op: OpExec, Argv: []string{"sh", "-c", "sleep 20 & echo started"}}, nil, &out, nil)
	if err != nil || code != 0 || out.String() != "started\n" {
		t.Fatalf("code %d, err %v, stdout %q", code, err, out.String())
	}
	if d := time.Since(t0); d > 5*time.Second {
		t.Fatalf("the answer took %s", d)
	}
}

// Output still on its way when the command ends is not cut off by the grace
// that is there for processes left behind.
func TestExecOutputIsWholeHoweverSlowTheReader(t *testing.T) {
	grace := outputGrace
	outputGrace = 50 * time.Millisecond
	defer func() { outputGrace = grace }()
	host := serve(t, Hooks{})
	b, _ := json.Marshal(Request{Op: OpExec, Argv: []string{"sh", "-c", "head -c 300000 /dev/zero; echo end"}})
	if err := WriteFrame(host, FrameHello, b); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond) // the command is long done, its output not yet read
	var n int
	var tail []byte
	for {
		typ, p, err := ReadFrame(host)
		if err != nil {
			t.Fatal(err)
		}
		if typ == FrameExit {
			break
		}
		n, tail = n+len(p), p
	}
	if n != 300004 || !bytes.HasSuffix(tail, []byte("end\n")) {
		t.Fatalf("%d bytes of output, want 300004 ending in end", n)
	}
}

// A command that does not read its input still ends when the host hangs up.
func TestHangupKillsACommandThatIgnoresItsInput(t *testing.T) {
	host, pids := started(t, Request{Op: OpExec, Argv: []string{"sh", "-c", "echo $$; sleep 60"}, Stdin: true})
	chunk := make([]byte, 32<<10)
	for i := 0; i < 16; i++ { // more than a pipe holds
		if err := WriteFrame(host, FrameStdin, chunk); err != nil {
			t.Fatal(err)
		}
	}
	host.Close()
	endsSoon(t, "the connection", pids...)
}

// The host hanging up ends the command and everything in its group.
func TestHangupKillsTheCommand(t *testing.T) {
	host, pids := started(t, Request{Op: OpExec, Argv: []string{"sh", "-c", "sleep 60 & echo $$ $!; wait"}})
	host.Close()
	endsSoon(t, "the connection", pids...)
}

// started sends req as an agent with no hooks would get it and answers the
// host's end and the pids the command printed first, on one line.
func started(t *testing.T, req Request) (net.Conn, []int) {
	t.Helper()
	host := serve(t, Hooks{})
	b, _ := json.Marshal(req)
	if err := WriteFrame(host, FrameHello, b); err != nil {
		t.Fatal(err)
	}
	typ, p, err := ReadFrame(host)
	if err != nil || typ != FrameStdout {
		t.Fatalf("frame %q, %v", typ, err)
	}
	var pids []int
	for _, f := range strings.Fields(string(p)) {
		pid, err := strconv.Atoi(f)
		if err != nil {
			t.Fatal(err)
		}
		pids = append(pids, pid)
	}
	if len(pids) == 0 {
		t.Fatalf("the command printed no pid: %q", p)
	}
	return host, pids
}

// endsSoon waits for every one of pids to be gone.
func endsSoon(t *testing.T, what string, pids ...int) {
	t.Helper()
	for _, pid := range pids {
		for end := time.Now().Add(5 * time.Second); syscall.Kill(pid, 0) == nil; time.Sleep(20 * time.Millisecond) {
			if time.Now().After(end) {
				_ = syscall.Kill(pid, syscall.SIGKILL)
				t.Fatalf("process %d outlived %s", pid, what)
			}
		}
	}
}

func TestCommandHookSetsTheCommandUp(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(dir+"/marker", nil, 0o600); err != nil {
		t.Fatal(err)
	}
	h := Hooks{Command: func(c *exec.Cmd) { c.Dir, c.Env = dir, []string{"WHO=box", "PATH=" + os.Getenv("PATH")} }}
	var out bytes.Buffer
	code, err := Do(serve(t, h), Request{Op: OpExec, Argv: []string{"sh", "-c", "echo $WHO; ls"}}, nil, &out, nil)
	if err != nil || code != 0 || out.String() != "box\nmarker\n" {
		t.Fatalf("code %d, err %v, stdout %q", code, err, out.String())
	}
}

// An exec says what its binding gives the box: root for its user, Docker
// started. The guest does each before the command, and a guest that cannot
// fails the command and says why.
func TestExecGivesRootAndDockerFirst(t *testing.T) {
	var did []string
	var dockerErr error
	h := Hooks{Command: func(*exec.Cmd) { did = append(did, "command") },
		Root:   func() error { did = append(did, "root"); return nil },
		Docker: func() error { did = append(did, "docker"); return dockerErr }}
	for _, req := range []Request{{Op: OpExec, Argv: []string{"true"}}, {Op: OpExec, Argv: []string{"true"}, Root: true, Docker: true}} {
		if code, err := Do(serve(t, h), req, nil, nil, nil); err != nil || code != 0 {
			t.Fatalf("%+v: code %d, err %v", req, code, err)
		}
	}
	if got := strings.Join(did, " "); got != "command root docker command" {
		t.Errorf("the guest did %q", got)
	}
	dockerErr, did = errors.New("no docker.service"), nil
	var errb bytes.Buffer
	code, err := Do(serve(t, h), Request{Op: OpExec, Argv: []string{"true"}, Docker: true}, nil, nil, &errb)
	if err != nil || code != 127 || !strings.Contains(errb.String(), "docker: no docker.service") || strings.Join(did, " ") != "docker" {
		t.Errorf("Docker could not start: code %d, err %v, stderr %q, the guest did %v", code, err, errb.String(), did)
	}
	for _, req := range []Request{{Op: OpExec, Argv: []string{"true"}, Root: true}, {Op: OpExec, Argv: []string{"true"}, Docker: true}} {
		errb.Reset()
		code, err = Do(serve(t, Hooks{}), req, nil, nil, &errb)
		if err != nil || code != 127 || !strings.Contains(errb.String(), "not supported") {
			t.Errorf("%+v in a guest that cannot: code %d, err %v, stderr %q", req, code, err, errb.String())
		}
	}
	// An agent built before root and docker ran the command and says nothing
	// of either: the host does not take that for a box with root.
	for _, req := range []Request{{Op: OpExec, Argv: []string{"true"}, Root: true}, {Op: OpExec, Argv: []string{"true"}, Root: true, Docker: true}} {
		a, b := net.Pipe()
		go func() {
			defer b.Close()
			if _, _, err := ReadFrame(b); err == nil {
				_ = WriteFrame(b, FrameExit, []byte(`{"code":0}`))
			}
		}()
		errb.Reset()
		code, err = Do(a, req, nil, nil, &errb)
		a.Close()
		if err != nil || code != 127 || !strings.Contains(errb.String(), "rebuild the image") {
			t.Errorf("%+v answered by an agent that knows no root: code %d, err %v, stderr %q", req, code, err, errb.String())
		}
	}
}

func TestOps(t *testing.T) {
	var squeezed int
	var woke int64
	h := Hooks{Squeeze: func() error { squeezed++; return nil }, Woke: func(n int64) error { woke = n; return nil }}
	for _, c := range []struct {
		req  Request
		h    Hooks
		code int
	}{
		{Request{Op: OpPing}, Hooks{}, 0},
		{Request{Op: OpSqueeze}, h, 0},
		{Request{Op: OpWoke, UnixNano: 42}, h, 0},
		{Request{Op: OpSqueeze}, Hooks{}, 1}, // a guest that cannot: said, not passed over
		{Request{Op: OpWoke}, Hooks{}, 1},
		{Request{Op: "format-disk"}, h, 127},
	} {
		code, err := Do(serve(t, c.h), c.req, nil, nil, nil)
		if err != nil || code != c.code {
			t.Errorf("%s: code %d, err %v, want %d", c.req.Op, code, err, c.code)
		}
	}
	if squeezed != 1 || woke != 42 {
		t.Errorf("squeezed %d times, woke with %d", squeezed, woke)
	}
}

func TestFrameLimit(t *testing.T) {
	if err := WriteFrame(&bytes.Buffer{}, FrameStdout, make([]byte, maxFrame+1)); err == nil {
		t.Error("a frame over the limit was written")
	}
	if _, _, err := ReadFrame(bytes.NewReader([]byte{FrameStdout, 0xff, 0xff, 0xff, 0xff})); err == nil {
		t.Error("a frame over the limit was read")
	}
}

func TestGuestAddr(t *testing.T) {
	ip, gw, ok := GuestAddr(10<<24 | 213<<16 | 6) // 10.213.0.6
	if !ok || ip.String() != "10.213.0.6" || gw.String() != "10.213.0.5" {
		t.Fatalf("%v %v %v", ip, gw, ok)
	}
	for _, cid := range []uint32{3, 10<<24 | 213<<16 | 5, 10<<24 | 213<<16 | 4, 192<<24 | 168<<16 | 2} {
		if _, _, ok := GuestAddr(cid); ok {
			t.Errorf("context id %d was read as a guest address", cid)
		}
	}
}
