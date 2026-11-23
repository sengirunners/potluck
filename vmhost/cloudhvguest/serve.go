package cloudhvguest

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"
)

// Hooks are what the agent does besides running commands. A nil hook answers
// "not supported here" (tests, and a host standing in for a guest).
type Hooks struct {
	Squeeze func() error
	Woke    func(unixNano int64) error
	// Command sets who a command runs as, and its environment and directory.
	// Nil runs it as the agent itself.
	Command func(*exec.Cmd)
	// Root gives the box's user sudo, and Docker starts the guest's Docker,
	// each if it is not done yet (a command whose request has Root or Docker).
	Root   func() error
	Docker func() error
}

// Serve answers one connection's request and returns when it is done. An
// exec's command runs in its own process group, as Hooks.Command says; when
// the connection closes before the command ends, the group is killed. A
// command on a terminal (OpPTY) leads a session of its own on it.
func Serve(conn io.ReadWriteCloser, h Hooks) {
	defer conn.Close()
	typ, p, err := ReadFrame(conn)
	if err != nil || typ != FrameHello {
		return
	}
	var req Request
	if err := json.Unmarshal(p, &req); err != nil {
		exit(conn, Result{Code: 127, Err: "bad request: " + err.Error()})
		return
	}
	switch req.Op {
	case OpPing:
		exit(conn, Result{})
	case OpSqueeze:
		exit(conn, hook(h.Squeeze))
	case OpWoke:
		var f func() error
		if h.Woke != nil {
			f = func() error { return h.Woke(req.UnixNano) }
		}
		exit(conn, hook(f))
	case OpExec:
		serveExec(conn, req, h)
	case OpPTY:
		servePTY(conn, req, h)
	default:
		exit(conn, Result{Code: 127, Err: unknownOp + req.Op})
	}
}

// unknownOp starts the answer to a request whose op the agent does not know.
const unknownOp = "unknown op "

func hook(f func() error) Result {
	if f == nil {
		return Result{Code: 1, Err: "not supported in this guest"}
	}
	if err := f(); err != nil {
		return Result{Code: 1, Err: err.Error()}
	}
	return Result{}
}

func exit(w io.Writer, r Result) {
	b, _ := json.Marshal(r)
	_ = WriteFrame(w, FrameExit, b)
}

// lockedWriter keeps two streams' frames whole on one connection.
type lockedWriter struct {
	mu *sync.Mutex
	w  io.Writer
}

func (l lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// ready gives the box what its binding does before a command of req's runs,
// and answers the host itself when the command cannot run.
func ready(conn io.Writer, req Request, h Hooks) bool {
	if len(req.Argv) == 0 {
		exit(conn, Result{Code: 127, Err: "no command"})
		return false
	}
	// What the binding gives the box comes before its command, and a guest
	// that cannot give it says so: the command does not run without.
	for _, want := range []struct {
		on   bool
		name string
		f    func() error
	}{{req.Root, "root", h.Root}, {req.Docker, "docker", h.Docker}} {
		if !want.on {
			continue
		}
		if r := hook(want.f); r.Code != 0 {
			exit(conn, Result{Code: 127, Err: want.name + ": " + r.Err})
			return false
		}
	}
	return true
}

func serveExec(conn io.ReadWriter, req Request, h Hooks) {
	if !ready(conn, req, h) {
		return
	}
	cmd := exec.Command(req.Argv[0], req.Argv[1:]...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if h.Command != nil {
		h.Command(cmd)
	}
	var stdin io.WriteCloser
	if req.Stdin {
		var err error
		if stdin, err = cmd.StdinPipe(); err != nil {
			exit(conn, Result{Code: 127, Err: err.Error()})
			return
		}
	}
	// Pipes of our own, not cmd.Stdout writers: Wait must not block on a
	// grandchild that kept the pipe after the command itself ended.
	or, ow, err := os.Pipe()
	if err != nil {
		exit(conn, Result{Code: 127, Err: err.Error()})
		return
	}
	er, ew, err := os.Pipe()
	if err != nil {
		or.Close()
		ow.Close()
		exit(conn, Result{Code: 127, Err: err.Error()})
		return
	}
	cmd.Stdout, cmd.Stderr = ow, ew
	if err := cmd.Start(); err != nil {
		for _, f := range []*os.File{or, ow, er, ew} {
			f.Close()
		}
		exit(conn, Result{Code: 127, Err: err.Error()})
		return
	}
	ow.Close()
	ew.Close()
	relay(conn, req, cmd, stdin, nil, stream{FrameStdout, or, true}, stream{FrameStderr, er, true})
}

// servePTY is serveExec for a command on a terminal: a pseudo-terminal of
// the size asked, which the command leads a session on, so a Ctrl-C typed on
// it is the command's signal. The terminal is the command's user's own, as
// a login's is: programs open their terminal by its name.
func servePTY(conn io.ReadWriter, req Request, h Hooks) {
	if !ready(conn, req, h) {
		return
	}
	master, slave, err := openTerminal(Size{req.Rows, req.Cols})
	if err != nil {
		exit(conn, Result{Code: 127, Err: "no terminal: " + err.Error()})
		return
	}
	defer master.Close() // what the command left on the terminal has none from here
	cmd := exec.Command(req.Argv[0], req.Argv[1:]...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true} // its stdin, the terminal
	if h.Command != nil {
		h.Command(cmd)
	}
	if c := cmd.SysProcAttr.Credential; c != nil {
		if err = slave.Chown(int(c.Uid), -1); err != nil {
			err = fmt.Errorf("it could not be given to the command's user: %w", err)
		}
	}
	if err != nil {
		slave.Close()
		exit(conn, Result{Code: 127, Err: "no terminal: " + err.Error()})
		return
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
	err = cmd.Start()
	slave.Close()
	if err != nil {
		exit(conn, Result{Code: 127, Err: err.Error()})
		return
	}
	relay(conn, req, cmd, typed{master}, func(s Size) { _ = setSize(master, s) }, stream{FrameStdout, master, false})
}

// openTerminal opens a pseudo-terminal of size and answers its two sides.
// The master is one whose Close ends a read of it at once (the library's is
// not), so closing it takes the terminal from whatever a command left on it.
func openTerminal(size Size) (master, slave *os.File, err error) {
	m, slave, err := pty.Open()
	if err != nil {
		return nil, nil, err
	}
	defer m.Close()
	fd, err := unix.FcntlInt(m.Fd(), unix.F_DUPFD_CLOEXEC, 0)
	if err == nil {
		if err = unix.SetNonblock(fd, true); err != nil {
			_ = unix.Close(fd)
		}
	}
	if err != nil {
		slave.Close()
		return nil, nil, err
	}
	master = os.NewFile(uintptr(fd), "ptmx")
	if err := setSize(master, size); err != nil {
		master.Close()
		slave.Close()
		return nil, nil, err
	}
	return master, slave, nil
}

// setSize sets a terminal's size through its master, held for as long as
// that takes: a resize that comes as the terminal closes finds it closed,
// never another file that took its number.
func setSize(master *os.File, s Size) error {
	rc, err := master.SyscallConn()
	if err != nil {
		return err
	}
	var failed error
	if err := rc.Control(func(fd uintptr) {
		failed = unix.IoctlSetWinsize(int(fd), unix.TIOCSWINSZ, &unix.Winsize{Row: s.Rows, Col: s.Cols})
	}); err != nil {
		return err
	}
	return failed
}

// typed is a terminal as a command's input: the host's end of input is not
// the terminal's end, which outlives it.
type typed struct{ io.Writer }

func (typed) Close() error { return nil }

// stream is one of a started command's outputs: what is read from r goes to
// the host in frames of typ. closes: r is closed when it has no more.
type stream struct {
	typ    byte
	r      *os.File
	closes bool
}

// relay carries a started command to its end: the host's stdin frames to
// stdin (nil: it reads none) and its Size frames to resize (nil: it has no
// terminal), the outputs to the host, then the Exit frame. A host that hangs
// up first ends the command and everything in its process group.
func relay(conn io.ReadWriter, req Request, cmd *exec.Cmd, stdin io.WriteCloser, resize func(Size), outs ...stream) {
	var mu sync.Mutex
	out := lockedWriter{&mu, conn}
	// busy counts pumps sending a frame, moved is when one last finished.
	var busy atomic.Int32
	var moved atomic.Int64
	var pumps sync.WaitGroup
	pump := func(o stream) {
		defer pumps.Done()
		if o.closes {
			defer o.r.Close()
		}
		buf := make([]byte, 32<<10)
		for {
			n, err := o.r.Read(buf)
			if n > 0 {
				busy.Add(1)
				_ = WriteFrame(out, o.typ, buf[:n])
				moved.Store(time.Now().UnixNano())
				busy.Add(-1)
			}
			if err != nil {
				return
			}
		}
	}
	pumps.Add(len(outs))
	for _, o := range outs {
		go pump(o)
	}

	var typedAhead *inbox
	if stdin != nil {
		typedAhead = newInbox()
		go typedAhead.feed(stdin)
	}
	// The host's frames: stdin, a new size, and the connection closing.
	gone := make(chan struct{})
	go func() {
		defer close(gone)
		defer func() {
			if typedAhead != nil {
				typedAhead.end()
			}
		}()
		for {
			typ, p, err := ReadFrame(conn)
			if err != nil {
				return
			}
			switch typ {
			case FrameStdin:
				if typedAhead != nil {
					typedAhead.put(p)
				}
			case FrameEOF:
				if typedAhead != nil {
					typedAhead.end()
				}
			case FrameSize:
				if s, ok := sizeOf(p); ok && resize != nil {
					resize(s)
				}
			}
		}
	}()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var werr error
	select {
	case werr = <-done:
	case <-gone: // the host hung up: end the command and everything it started
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-done
		return
	}
	// The command ended. Its output is whole once the pumps see the pipes
	// (or the terminal) close. A process it left behind can hold them open
	// for good, so the answer waits only while output is still moving: until
	// nothing has been sent for outputGrace.
	ended := time.Now().UnixNano()
	flushed := make(chan struct{})
	go func() { pumps.Wait(); close(flushed) }()
	for quiet := false; !quiet; {
		select {
		case <-flushed:
			quiet = true
		case <-gone:
			quiet = true
		case <-time.After(outputGrace / 10):
			last := max(ended, moved.Load())
			quiet = busy.Load() == 0 && time.Since(time.Unix(0, last)) >= outputGrace
		}
	}
	exit(out, Result{Code: exitCode(werr), Root: req.Root, Docker: req.Docker})
}

// inbox is what the host sent for a command's input that the command has
// not read yet. It is held here so that the goroutine reading the host's
// frames is not stopped by a command that does not read: it must still see a
// new size and the host hanging up. That holds for the first inboxMax bytes
// a command leaves unread; past it the reader waits for the command, and a
// hang-up is seen when the command reads on or ends.
type inbox struct {
	mu    sync.Mutex
	moved *sync.Cond
	held  [][]byte
	n     int
	ended bool // the host sends no more
	deaf  bool // the command takes no more
}

const inboxMax = 2 << 20

func newInbox() *inbox {
	q := &inbox{}
	q.moved = sync.NewCond(&q.mu)
	return q
}

func (q *inbox) put(p []byte) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for q.n > 0 && q.n+len(p) > inboxMax && !q.deaf && !q.ended {
		q.moved.Wait()
	}
	if q.deaf || q.ended {
		return
	}
	q.held, q.n = append(q.held, p), q.n+len(p)
	q.moved.Broadcast()
}

func (q *inbox) end() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.ended = true
	q.moved.Broadcast()
}

// feed writes what the inbox holds to w until it has ended and is empty, or
// w takes no more, and then closes w.
func (q *inbox) feed(w io.WriteCloser) {
	defer w.Close()
	for {
		q.mu.Lock()
		for len(q.held) == 0 && !q.ended {
			q.moved.Wait()
		}
		if len(q.held) == 0 {
			q.mu.Unlock()
			return
		}
		p := q.held[0]
		q.held, q.n = q.held[1:], q.n-len(p)
		q.moved.Broadcast()
		q.mu.Unlock()
		if _, err := w.Write(p); err != nil {
			q.mu.Lock()
			q.deaf, q.held, q.n = true, nil, 0
			q.moved.Broadcast()
			q.mu.Unlock()
			return
		}
	}
}

// outputGrace is how long an ended command's output may stand still before
// the answer goes without the rest: what a process it left behind still holds.
var outputGrace = 500 * time.Millisecond

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return 128 + int(ws.Signal())
		}
		return ee.ExitCode()
	}
	return 127
}
