package boxproc

// Terminals (SPEC 20 "Boxes", terminals; zm-eakh.12.5.21): a background
// process spawned with a pty runs on a pseudo-terminal of the size asked,
// both its streams as stdout, and the optional input verb writes bytes to
// that terminal and resizes it. A Ctrl-C or Ctrl-D is a byte the box's own
// line discipline turns into SIGINT or an end of input, so nothing else
// crosses. The holder owns the terminal's host side and reads input from a
// FIFO in the process's directory (in.fifo); input writes frames of at most
// PIPE_BUF bytes, so a write is whole or not at all and a killed caller
// never leaves half a frame. Owned by run targets.

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"syscall"
	"time"

	"github.com/creack/pty"
	"golang.org/x/term"
)

// SPEC 20's limits on terminals.
const (
	// MaxInput bounds one input call's bytes (as sent, decoded).
	MaxInput = 64 << 10
	// MaxSide bounds rows and cols.
	MaxSide = 1000
	// DefaultTerm is TERM when spawn names none.
	DefaultTerm = "xterm-256color"
)

// PTY is spawn's pty: the terminal's size and its TERM.
type PTY struct {
	Rows int    `json:"rows"`
	Cols int    `json:"cols"`
	Term string `json:"term,omitempty"`
}

var termRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,63}$`)

// ParsePTY reads spawn's pty from its args: nil when absent. A pty with
// secrets is refused: they are framed on stdin, which a terminal echoes.
func ParsePTY(raw json.RawMessage) (*PTY, error) {
	var a struct {
		PTY     *PTY            `json:"pty"`
		Secrets json.RawMessage `json:"secrets"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, refuse("bad_request", "bad args: %v", err)
	}
	p := a.PTY
	if p == nil {
		return nil, nil
	}
	switch {
	case p.Rows < 1 || p.Rows > MaxSide || p.Cols < 1 || p.Cols > MaxSide:
		return nil, refuse("bad_request", "pty rows and cols are 1..%d", MaxSide)
	case p.Term != "" && !termRe.MatchString(p.Term):
		return nil, refuse("bad_request", "pty term is a terminal type name like xterm-256color")
	case len(a.Secrets) > 0 && string(a.Secrets) != "null" && string(a.Secrets) != "{}":
		return nil, refuse("bad_request", "a process on a terminal takes no secrets (the terminal would echo them)")
	}
	if p.Term == "" {
		p.Term = DefaultTerm
	}
	return p, nil
}

// InputArgs are input's args: data for the terminal (base64 with
// encoding), and rows and cols together to resize it, applied first.
type InputArgs struct {
	PID      string  `json:"pid"`
	Data     *string `json:"data"`
	Encoding string  `json:"encoding"`
	Rows     int     `json:"rows"`
	Cols     int     `json:"cols"`
}

// ParseInput reads and bounds input's args, answering the decoded data.
func ParseInput(raw json.RawMessage) (InputArgs, []byte, error) {
	var a InputArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return a, nil, refuse("bad_request", "bad args: %v", err)
	}
	switch {
	case a.PID == "":
		return a, nil, refuse("bad_request", "input names a pid")
	case (a.Rows == 0) != (a.Cols == 0) || a.Rows < 0 || a.Rows > MaxSide || a.Cols < 0 || a.Cols > MaxSide:
		return a, nil, refuse("bad_request", "rows and cols go together, each 1..%d", MaxSide)
	}
	b, err := Decode(a.Data, a.Encoding)
	if err != nil {
		return a, nil, err
	}
	if len(b) > MaxInput {
		return a, nil, refuse("bad_request", "data is over %d bytes", MaxInput)
	}
	return a, b, nil
}

func fifoPath(d Dir) string { return filepath.Join(string(d), "in.fifo") }

// frames: a type byte, a 2-byte big-endian length, the payload; each
// write at most pipeBuf bytes (POSIX's least PIPE_BUF), so it is atomic.
const (
	pipeBuf    = 512
	frameData  = 'd'
	frameSize  = 's'
	inputWait  = 10 * time.Second // how long input waits on a full FIFO
	startWait  = 5 * time.Second  // how long input waits for a new holder's FIFO
	drainAfter = 2 * time.Second  // how long a holder reads the terminal after its process ended
)

func frame(kind byte, p []byte) []byte {
	b := make([]byte, 3, 3+len(p))
	b[0] = kind
	binary.BigEndian.PutUint16(b[1:], uint16(len(p)))
	return append(b, p...)
}

// Input answers one input call: data written to the process's terminal
// and the size applied, or running false once it ended.
func Input(d Dir, a InputArgs, data []byte) (map[string]any, error) {
	m, err := d.Meta()
	if err != nil {
		return nil, err
	}
	if !m.PTY {
		return nil, refuse("bad_request", "process %s has no terminal (spawn it with pty)", d.ID())
	}
	ended := map[string]any{"written": 0, "running": false}
	var fd int
	for deadline := time.Now().Add(startWait); ; time.Sleep(20 * time.Millisecond) {
		if !d.Running() {
			return ended, nil
		}
		fd, err = syscall.Open(fifoPath(d), syscall.O_WRONLY|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.ENOENT) && !errors.Is(err, syscall.ENXIO) || !time.Now().Before(deadline) {
			if !d.Running() {
				return ended, nil
			}
			return nil, refuse("unavailable", "the terminal of %s takes no input: %v", d.ID(), err)
		}
	}
	defer syscall.Close(fd)
	lock, err := os.OpenFile(filepath.Join(string(d), "in.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return nil, err
	}
	var frames [][]byte
	if a.Rows > 0 {
		sz := make([]byte, 4)
		binary.BigEndian.PutUint16(sz, uint16(a.Rows))
		binary.BigEndian.PutUint16(sz[2:], uint16(a.Cols))
		frames = append(frames, frame(frameSize, sz))
	}
	for p := data; len(p) > 0; {
		n := min(len(p), pipeBuf-3)
		frames = append(frames, frame(frameData, p[:n]))
		p = p[n:]
	}
	written, deadline := 0, time.Now().Add(inputWait)
	for _, f := range frames {
		for {
			_, err := syscall.Write(fd, f)
			if err == nil {
				break
			}
			if !errors.Is(err, syscall.EAGAIN) || !time.Now().Before(deadline) || !d.Running() {
				// The rest is not sent: written says how much was.
				return map[string]any{"written": written, "running": d.Running()}, nil
			}
			time.Sleep(10 * time.Millisecond)
		}
		if f[0] == frameData {
			written += len(f) - 3
		}
	}
	return map[string]any{"written": written, "running": true}, nil
}

// StartPTY starts cmd on a new terminal of p's size (a new session, the
// terminal its controlling one unless attrs says otherwise), its output to
// out, input from the process's FIFO and stdin written first. With raw the
// terminal is raw before cmd starts: for a cmd that only carries the bytes
// to a terminal of its own somewhere else, so that this one echoes nothing,
// edits no line and reads no Ctrl-C, not even in what is typed first. wait
// answers once the process ended and its output was read (up to drainAfter
// more: something it left may hold the terminal).
func StartPTY(d Dir, cmd *exec.Cmd, p PTY, raw bool, stdin []byte, out io.Writer, attrs *syscall.SysProcAttr) (wait func() error, err error) {
	_ = os.Remove(fifoPath(d))
	if err := syscall.Mkfifo(fifoPath(d), 0o600); err != nil {
		return nil, err
	}
	in, err := os.OpenFile(fifoPath(d), os.O_RDWR, 0) // read and write: no end of input while no caller has it open
	if err != nil {
		return nil, err
	}
	if attrs == nil {
		attrs = &syscall.SysProcAttr{Setsid: true, Setctty: true}
	}
	master, err := startOn(cmd, p, raw, attrs)
	if err != nil {
		in.Close()
		_ = os.Remove(fifoPath(d))
		return nil, err
	}
	copied := make(chan struct{})
	go func() {
		_, _ = io.Copy(out, master)
		close(copied)
	}()
	go serveInput(in, master)
	if len(stdin) > 0 {
		go func() { _, _ = master.Write(stdin) }() // typed first; never holds the holder up
	}
	return func() error {
		err := cmd.Wait()
		select {
		case <-copied:
		case <-time.After(drainAfter):
		}
		_ = os.Remove(fifoPath(d)) // input answers running false from here
		in.Close()
		return err
	}, nil
}

// startOn starts cmd on a new terminal and answers the terminal's host side.
func startOn(cmd *exec.Cmd, p PTY, raw bool, attrs *syscall.SysProcAttr) (*os.File, error) {
	master, tty, err := pty.Open()
	if err != nil {
		return nil, err
	}
	defer tty.Close()
	err = pty.Setsize(master, &pty.Winsize{Rows: uint16(p.Rows), Cols: uint16(p.Cols)})
	if err == nil && raw {
		_, err = term.MakeRaw(int(tty.Fd()))
	}
	if err == nil {
		cmd.Stdin, cmd.Stdout, cmd.Stderr = tty, tty, tty
		cmd.SysProcAttr = attrs
		err = cmd.Start()
	}
	if err != nil {
		master.Close()
		return nil, err
	}
	return master, nil
}

// serveInput applies the FIFO's frames to the terminal until in closes.
func serveInput(in io.Reader, master *os.File) {
	r := bufio.NewReader(in)
	hdr := make([]byte, 3)
	for {
		if _, err := io.ReadFull(r, hdr); err != nil {
			return
		}
		p := make([]byte, binary.BigEndian.Uint16(hdr[1:]))
		if _, err := io.ReadFull(r, p); err != nil {
			return
		}
		switch {
		case hdr[0] == frameData:
			_, _ = master.Write(p)
		case hdr[0] == frameSize && len(p) == 4:
			_ = pty.Setsize(master, &pty.Winsize{Rows: binary.BigEndian.Uint16(p), Cols: binary.BigEndian.Uint16(p[2:])})
		}
	}
}

// ExitCode is a process's exit as SPEC 20 answers it: 128 + the signal
// that ended it.
func ExitCode(ps *os.ProcessState) int {
	if ps == nil {
		return 128 + int(syscall.SIGKILL)
	}
	if ws, ok := ps.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	return ps.ExitCode()
}
