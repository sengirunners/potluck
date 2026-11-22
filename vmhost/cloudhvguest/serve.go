package cloudhvguest

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// Hooks are what the agent does besides running commands. A nil hook answers
// "not supported here" (tests, and a host standing in for a guest).
type Hooks struct {
	Squeeze func() error
	Woke    func(unixNano int64) error
	// Command sets who an exec's command runs as, and its environment and
	// directory. Nil runs it as the agent itself.
	Command func(*exec.Cmd)
	// Root gives the box's user sudo, and Docker starts the guest's Docker,
	// each if it is not done yet (an exec whose request has Root or Docker).
	Root   func() error
	Docker func() error
}

// Serve answers one connection's request and returns when it is done. An
// exec's command runs in its own process group, as Hooks.Command says; when
// the connection closes before the command ends, the group is killed.
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
	default:
		exit(conn, Result{Code: 127, Err: "unknown op " + req.Op})
	}
}

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

func serveExec(conn io.ReadWriter, req Request, h Hooks) {
	if len(req.Argv) == 0 {
		exit(conn, Result{Code: 127, Err: "no command"})
		return
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
			return
		}
	}
	var mu sync.Mutex
	out := lockedWriter{&mu, conn}
	cmd := exec.Command(req.Argv[0], req.Argv[1:]...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if h.Command != nil {
		h.Command(cmd)
	}
	var stdin io.WriteCloser
	if req.Stdin {
		var err error
		if stdin, err = cmd.StdinPipe(); err != nil {
			exit(out, Result{Code: 127, Err: err.Error()})
			return
		}
	}
	// Pipes of our own, not cmd.Stdout writers: Wait must not block on a
	// grandchild that kept the pipe after the command itself ended.
	or, ow, err := os.Pipe()
	if err != nil {
		exit(out, Result{Code: 127, Err: err.Error()})
		return
	}
	er, ew, err := os.Pipe()
	if err != nil {
		or.Close()
		ow.Close()
		exit(out, Result{Code: 127, Err: err.Error()})
		return
	}
	cmd.Stdout, cmd.Stderr = ow, ew
	if err := cmd.Start(); err != nil {
		for _, f := range []*os.File{or, ow, er, ew} {
			f.Close()
		}
		exit(out, Result{Code: 127, Err: err.Error()})
		return
	}
	ow.Close()
	ew.Close()
	// busy counts pumps sending a frame, moved is when one last finished.
	var busy atomic.Int32
	var moved atomic.Int64
	var pumps sync.WaitGroup
	pump := func(typ byte, r *os.File) {
		defer pumps.Done()
		defer r.Close()
		buf := make([]byte, 32<<10)
		for {
			n, err := r.Read(buf)
			if n > 0 {
				busy.Add(1)
				_ = WriteFrame(out, typ, buf[:n])
				moved.Store(time.Now().UnixNano())
				busy.Add(-1)
			}
			if err != nil {
				return
			}
		}
	}
	pumps.Add(2)
	go pump(FrameStdout, or)
	go pump(FrameStderr, er)

	// Stdin goes through a queue, so the goroutine reading the host's frames
	// is not stopped at once by a command that does not read its input: it
	// must still see the host hang up. The queue is 64 frames, so that holds
	// for the first 2 MiB or so a command leaves unread; past it the reader
	// waits for the command, and a hang-up is seen when the command reads on
	// or ends.
	var queue chan []byte
	if stdin != nil {
		queue = make(chan []byte, 64)
		go func(q chan []byte) {
			for p := range q {
				if _, err := stdin.Write(p); err != nil {
					break
				}
			}
			_ = stdin.Close()
			for range q { // the command reads no more
			}
		}(queue)
	}
	// The host's frames: stdin, and the connection closing.
	gone := make(chan struct{})
	go func() {
		defer close(gone)
		defer func() {
			if queue != nil {
				close(queue)
			}
		}()
		for {
			typ, p, err := ReadFrame(conn)
			if err != nil {
				return
			}
			switch typ {
			case FrameStdin:
				if queue != nil {
					queue <- p
				}
			case FrameEOF:
				if queue != nil {
					close(queue)
					queue = nil
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
	// close. A process it left behind can hold them open for good, so the
	// answer waits only while output is still moving: until nothing has been
	// sent for outputGrace.
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
