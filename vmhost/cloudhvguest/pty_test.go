package cloudhvguest

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
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
)

// session is one command on a terminal, as the host sees it: what it types,
// the size it says the terminal has, and what the terminal showed.
type session struct {
	t       *testing.T
	in      *io.PipeWriter
	resized chan struct{}
	done    chan struct{}
	code    int
	err     error

	mu    sync.Mutex
	size  Size
	shown bytes.Buffer
}

func (s *session) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.shown.Write(p)
}

func (s *session) screen() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.shown.String()
}

// open runs req's command on a terminal of size, served by an agent with h.
func open(t *testing.T, h Hooks, size Size, req Request) *session {
	t.Helper()
	r, w := io.Pipe()
	s := &session{t: t, in: w, resized: make(chan struct{}, 1), done: make(chan struct{}), size: size}
	conn := serve(t, h)
	go func() {
		defer close(s.done)
		s.code, s.err = DoPTY(conn, req, func() Size {
			s.mu.Lock()
			defer s.mu.Unlock()
			return s.size
		}, s.resized, r, s)
	}()
	t.Cleanup(func() { w.Close(); conn.Close(); <-s.done })
	return s
}

func (s *session) typed(text string) {
	s.t.Helper()
	if _, err := io.WriteString(s.in, text); err != nil {
		s.t.Fatal(err)
	}
}

func (s *session) until(want string) {
	s.t.Helper()
	for end := time.Now().Add(10 * time.Second); !strings.Contains(s.screen(), want); time.Sleep(10 * time.Millisecond) {
		select {
		case <-s.done:
			if !strings.Contains(s.screen(), want) {
				s.t.Fatalf("the command ended (code %d, %v) and the terminal never showed %q: %q", s.code, s.err, want, s.screen())
			}
		default:
		}
		if time.Now().After(end) {
			s.t.Fatalf("the terminal never showed %q: %q", want, s.screen())
		}
	}
}

// end waits for the command's exit code.
func (s *session) end() int {
	s.t.Helper()
	select {
	case <-s.done:
	case <-time.After(10 * time.Second):
		s.t.Fatalf("the command never ended: %q", s.screen())
	}
	if s.err != nil {
		s.t.Fatal(s.err)
	}
	return s.code
}

// A command on a terminal: all three of its streams are one terminal of the
// size asked, which echoes what is typed and ends its lines as a terminal
// does; a resize with nobody telling of it reaches it with what is typed
// next; and a Ctrl-C is the command's interrupt.
func TestPTYCommandRunsOnATerminal(t *testing.T) {
	script := `test -t 0 && test -t 1 && test -t 2 || exit 9; stty size; read x; echo "got:$x"; read y; stty size; read z; echo never`
	s := open(t, Hooks{}, Size{Rows: 7, Cols: 133}, Request{Argv: []string{"sh", "-c", script}})
	s.until("7 133\r\n")
	s.typed("hello\n")
	s.until("got:hello\r\n")
	if n := strings.Count(s.screen(), "hello"); n != 2 {
		t.Errorf("what was typed shows %d times, want 2 (the terminal's echo, and the command's answer): %q", n, s.screen())
	}
	// A new size with nobody telling: it goes with what is typed next.
	s.mu.Lock()
	s.size = Size{Rows: 30, Cols: 100}
	s.mu.Unlock()
	s.typed("\n")
	s.until("30 100\r\n")
	s.typed("\x03")
	if code := s.end(); code != 128+int(syscall.SIGINT) {
		t.Errorf("exit %d after a Ctrl-C, want %d; the terminal showed %q", code, 128+int(syscall.SIGINT), s.screen())
	}
	if strings.Contains(s.screen(), "never") {
		t.Errorf("the command ran on after a Ctrl-C: %q", s.screen())
	}
}

// The host sends a new size before what was typed after it, so the guest's
// terminal has the size when the command reads what was typed.
func TestPTYNewSizeIsSentBeforeWhatIsTypedAfterIt(t *testing.T) {
	host, guest := net.Pipe()
	defer guest.Close()
	var mu sync.Mutex
	size := Size{24, 80}
	typed, in := io.Pipe()
	defer in.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = DoPTY(host, Request{Argv: []string{"sh"}}, func() Size {
			mu.Lock()
			defer mu.Unlock()
			return size
		}, nil, typed, io.Discard)
	}()
	next := func() (byte, []byte) {
		t.Helper()
		typ, p, err := ReadFrame(guest)
		if err != nil {
			t.Fatal(err)
		}
		return typ, p
	}
	var req Request
	if typ, p := next(); typ != FrameHello || json.Unmarshal(p, &req) != nil || req.Op != OpPTY || req.Rows != 24 || req.Cols != 80 {
		t.Fatalf("the first frame is %q %q", typ, p)
	}
	mu.Lock()
	size = Size{41, 97}
	mu.Unlock()
	go func() { _, _ = io.WriteString(in, "a") }()
	if typ, p := next(); typ != FrameSize || !bytes.Equal(p, Size{41, 97}.bytes()) {
		t.Fatalf("after a new size and a key, the host sent %q %q first, want the size", typ, p)
	}
	if typ, p := next(); typ != FrameStdin || string(p) != "a" {
		t.Errorf("then %q %q, want the key", typ, p)
	}
	exit(guest, Result{})
	<-done
}

// A resize the host tells of reaches the terminal with nothing typed.
func TestPTYResizeWithNothingTyped(t *testing.T) {
	script := `trap 'stty size' WINCH; echo ready; while :; do sleep 0.05; done`
	s := open(t, Hooks{}, Size{Rows: 24, Cols: 80}, Request{Argv: []string{"sh", "-c", script}})
	s.until("ready")
	s.mu.Lock()
	s.size = Size{Rows: 41, Cols: 97}
	s.mu.Unlock()
	s.resized <- struct{}{}
	s.until("41 97")
}

// A command on a terminal is set up as an exec's is: as the agent's hook
// says, after what its binding gives the box, and not at all in a guest that
// cannot give it.
func TestPTYCommandIsSetUpAsAnExecIs(t *testing.T) {
	dir := t.TempDir()
	var did []string
	h := Hooks{Command: func(c *exec.Cmd) {
		did = append(did, "command")
		c.Dir, c.Env = dir, []string{"WHO=box", "PATH=" + os.Getenv("PATH")}
	},
		Root:   func() error { did = append(did, "root"); return nil },
		Docker: func() error { did = append(did, "docker"); return nil }}
	s := open(t, h, Size{Rows: 24, Cols: 80}, Request{Argv: []string{"sh", "-c", `echo "$WHO in $(pwd)"`}, Root: true, Docker: true})
	if code := s.end(); code != 0 || !strings.Contains(s.screen(), "box in "+dir) || strings.Join(did, " ") != "root docker command" {
		t.Errorf("code %d, the terminal showed %q, the guest did %v", code, s.screen(), did)
	}
	for _, req := range []Request{{Argv: []string{"true"}, Root: true}, {Argv: []string{"true"}, Docker: true}, {}} {
		s := open(t, Hooks{}, Size{Rows: 24, Cols: 80}, req)
		if code := s.end(); code != 127 || !strings.Contains(s.screen(), "not supported") && len(req.Argv) > 0 {
			t.Errorf("%+v in a guest that cannot: code %d, the terminal showed %q", req, code, s.screen())
		}
	}
	s = open(t, Hooks{}, Size{Rows: 24, Cols: 80}, Request{Argv: []string{"/no/such/program"}})
	if code := s.end(); code != 127 || s.screen() == "" {
		t.Errorf("a command that cannot start: code %d, the terminal showed %q", code, s.screen())
	}
}

// The terminal is the command's user's own. A guest that cannot give it to
// that user runs no command on it.
func TestPTYBelongsToTheCommandsUser(t *testing.T) {
	if os.Getuid() == 0 {
		h := Hooks{Command: func(c *exec.Cmd) { c.SysProcAttr.Credential = &syscall.Credential{Uid: 65534, Gid: 65534} }}
		s := open(t, h, Size{Rows: 24, Cols: 80}, Request{Argv: []string{"sh", "-c", `test -O "$(tty)" && echo mine`}})
		if code := s.end(); code != 0 || !strings.Contains(s.screen(), "mine") {
			t.Errorf("code %d, the terminal showed %q", code, s.screen())
		}
		return
	}
	// Not root: this process can give a terminal to nobody else.
	h := Hooks{Command: func(c *exec.Cmd) {
		c.SysProcAttr.Credential = &syscall.Credential{Uid: uint32(os.Getuid()) + 1, Gid: uint32(os.Getgid()), NoSetGroups: true}
	}}
	s := open(t, h, Size{Rows: 24, Cols: 80}, Request{Argv: []string{"true"}})
	if code := s.end(); code != 127 || !strings.Contains(s.screen(), "could not be given to the command's user") {
		t.Errorf("a terminal that cannot be its user's: code %d, the terminal showed %q", code, s.screen())
	}
}

// A command that leaves a process behind on its terminal does not hold the
// answer, and the terminal is taken from what it left: a later write to it
// fails.
func TestPTYAnswerDoesNotWaitForWhatItLeftBehind(t *testing.T) {
	said := filepath.Join(t.TempDir(), "said")
	left := `trap "" HUP; (sleep 2; echo late 2>/dev/null; echo "the write answered $?" > "$SAID") & echo started`
	h := Hooks{Command: func(c *exec.Cmd) { c.Env = append(os.Environ(), "SAID="+said) }}
	t0 := time.Now()
	s := open(t, h, Size{Rows: 24, Cols: 80}, Request{Argv: []string{"sh", "-c", left}})
	if code := s.end(); code != 0 || !strings.Contains(s.screen(), "started") {
		t.Fatalf("code %d, the terminal showed %q", code, s.screen())
	}
	if d := time.Since(t0); d > 1900*time.Millisecond {
		t.Fatalf("the answer took %s: it waited for what the command left", d)
	}
	var b []byte
	for end := time.Now().Add(10 * time.Second); len(b) == 0 && time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		b, _ = os.ReadFile(said)
	}
	if got := strings.TrimSpace(string(b)); got == "" || got == "the write answered 0" {
		t.Errorf("what the command left wrote to its terminal after the answer: %q, want the write to fail", got)
	}
}

// The host hanging up ends a command on a terminal and what it started in
// its group, though neither would end for the terminal going away.
func TestPTYHangupKillsTheCommand(t *testing.T) {
	host, pids := started(t, Request{Op: OpPTY, Argv: []string{"sh", "-c", `trap "" HUP; sleep 60 & echo $$ $!; wait`}, Rows: 24, Cols: 80})
	host.Close()
	endsSoon(t, "the connection", pids...)
}

// A command that reads nothing of what is typed at it, a keystroke to a
// frame, still gets a resize and still ends when the host hangs up.
func TestPTYTypedAheadHidesNeitherAResizeNorAHangup(t *testing.T) {
	host, pids := started(t, Request{Op: OpPTY, Argv: []string{"sh", "-c", `trap "" HUP; echo $$; trap 'stty size' WINCH; while :; do sleep 0.05; done`}, Rows: 24, Cols: 80})
	shown := make(chan string, 1)
	go func() { // the host reads what the terminal shows
		var all strings.Builder
		for {
			_, p, err := ReadFrame(host)
			if err != nil {
				return
			}
			if all.Write(p); strings.Contains(all.String(), "41 97") {
				select {
				case shown <- all.String():
				default:
				}
			}
		}
	}()
	line := []byte(strings.Repeat("x", 99) + "\n")
	for i := 0; i < 3000; i++ { // more than a terminal holds unread, in many more frames than a command's input comes in
		if err := WriteFrame(host, FrameStdin, line); err != nil {
			t.Fatal(err)
		}
	}
	if err := WriteFrame(host, FrameSize, Size{41, 97}.bytes()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-shown:
	case <-time.After(10 * time.Second):
		t.Error("the resize never reached the terminal behind what was typed ahead")
	}
	host.Close()
	endsSoon(t, "the connection", pids...)
}

// What a command does not read is held for it only up to a bound: past it
// the host's typing waits, and the agent's memory does not grow with it.
func TestPTYTypedAheadIsHeldOnlySoFar(t *testing.T) {
	host, pids := started(t, Request{Op: OpPTY, Argv: []string{"sh", "-c", `echo $$; while :; do sleep 0.05; done`}, Rows: 24, Cols: 80})
	t.Cleanup(func() { _ = syscall.Kill(pids[0], syscall.SIGKILL) }) // a hang-up behind this much unread is not seen
	go func() {
		// The host reads what the terminal echoes.
		for {
			if _, _, err := ReadFrame(host); err != nil {
				return
			}
		}
	}()
	frame := []byte(strings.Repeat(strings.Repeat("x", 99)+"\n", 600))
	var taken atomic.Int64
	go func() {
		for i := 0; i < 3*inboxMax/len(frame); i++ {
			if WriteFrame(host, FrameStdin, frame) != nil {
				return
			}
			taken.Add(int64(len(frame)))
		}
	}()
	was, still := int64(-1), 0
	for end := time.Now().Add(20 * time.Second); still < 10 && time.Now().Before(end); time.Sleep(50 * time.Millisecond) {
		if now := taken.Load(); now == was {
			still++
		} else {
			was, still = now, 0
		}
	}
	if was < inboxMax/2 || was > inboxMax+(512<<10) {
		t.Errorf("the agent took %d bytes that the command never read, want about %d", was, inboxMax)
	}
}

// The host's end of input is not the terminal's end: the command goes on
// with its terminal.
func TestPTYEndOfInputLeavesTheTerminal(t *testing.T) {
	host, _ := started(t, Request{Op: OpPTY, Argv: []string{"sh", "-c", `echo $$; sleep 0.3; stty size && echo still-here`}, Rows: 31, Cols: 77})
	if err := WriteFrame(host, FrameEOF, nil); err != nil {
		t.Fatal(err)
	}
	var shown strings.Builder
	for {
		typ, p, err := ReadFrame(host)
		if err != nil {
			t.Fatalf("%v, after the terminal showed %q", err, shown.String())
		}
		if typ == FrameExit {
			var res Result
			if err := json.Unmarshal(p, &res); err != nil || res.Code != 0 || !strings.Contains(shown.String(), "31 77\r\nstill-here") {
				t.Errorf("result %+v, %v, the terminal showed %q", res, err, shown.String())
			}
			return
		}
		shown.Write(p)
	}
}

// An agent built before terminals knows no such op and runs nothing: the
// host says the image is the older one.
func TestPTYAnsweredByAnOlderAgent(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	go func() {
		defer b.Close()
		if _, p, err := ReadFrame(b); err == nil {
			var req Request
			_ = json.Unmarshal(p, &req)
			exit(b, Result{Code: 127, Err: "unknown op " + req.Op})
		}
	}()
	var shown bytes.Buffer
	code, err := DoPTY(a, Request{Argv: []string{"true"}}, func() Size { return Size{24, 80} }, nil, strings.NewReader(""), &shown)
	if err != nil || code != 127 || !strings.Contains(shown.String(), "gives no terminal") || !strings.Contains(shown.String(), "rebuild the image") {
		t.Errorf("code %d, err %v, the host said %q", code, err, shown.String())
	}
	// This agent's own answer to an op it does not know is that one.
	shown.Reset()
	if code, err := Do(serve(t, Hooks{}), Request{Op: "format-disk"}, nil, nil, &shown); err != nil || code != 127 || strings.TrimSpace(shown.String()) != "unknown op format-disk" {
		t.Errorf("an op this agent does not know: code %d, err %v, %q", code, err, shown.String())
	}
}

// What is typed arrives whole, however much it is and however the command
// reads it, and so does what the terminal shows.
func TestPTYManyBytesBothWays(t *testing.T) {
	line := strings.Repeat("0123456789abcdef", 6) + "\n"
	typed := strings.Repeat(line, 2100)
	s := open(t, Hooks{}, Size{Rows: 24, Cols: 80}, Request{Argv: []string{"sh", "-c", `stty -echo; echo ready; head -n 2100 | cksum; seq 1 20000; echo all-shown`}})
	s.until("ready")
	s.typed(typed)
	sum := func(in string) string {
		cmd := exec.Command("cksum")
		cmd.Stdin = strings.NewReader(in)
		out, err := cmd.Output()
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(string(out))
	}
	var seq strings.Builder
	for i := 1; i <= 20000; i++ {
		seq.WriteString(strconv.Itoa(i) + "\r\n")
	}
	s.until("all-shown")
	if got := s.screen(); !strings.Contains(got, sum(typed)) || !strings.Contains(got, seq.String()) {
		t.Errorf("the terminal showed %d bytes ending %q, want the sum %q of what was typed and every line of the 20000", len(got), got[max(0, len(got)-200):], sum(typed))
	}
}
