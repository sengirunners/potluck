// Package cloudhvguest is the guest side of the cloud-hypervisor VM layer
// (vmhost/cloudhv.go, docs/cloudhv.md): the wire format between the host and
// potluck-guest, the small agent that runs inside each VM and answers on the
// VM's vsock.
//
// One connection carries one request. The host sends a Hello frame (JSON),
// then, for an exec, stdin frames; the guest answers with stdout and stderr
// frames and one Exit frame. Closing the connection before Exit ends the
// guest command (its whole process group is killed).
//
// A command on a terminal (OpPTY) is an exec whose three streams are one
// pseudo-terminal the agent opens in the guest: stdin frames are what is
// typed on it, stdout frames what it shows, and a Size frame resizes it.
package cloudhvguest

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
)

// Port is the vsock port the agent listens on. Below 1024, so only root in
// the guest can bind it: in a box without root, if the agent ever stops, the
// box's user cannot answer the host in its place. In a box with root
// (Request.Root) the user can, so the host takes a guest's answers as the
// box's own word: a command's output and exit code, and nothing it acts on
// for anyone else.
const Port = 1000

// Frame types.
const (
	FrameHello  byte = 'H' // host: Request as JSON
	FrameStdin  byte = 'I' // host: bytes for the command's stdin
	FrameEOF    byte = 'E' // host: stdin is done
	FrameStdout byte = 'O' // guest: the command's stdout
	FrameStderr byte = 'R' // guest: the command's stderr
	FrameExit   byte = 'X' // guest: Result as JSON, the last frame
	FrameSize   byte = 'W' // host: the terminal's new size (OpPTY): rows, then cols, two bytes each
)

// maxFrame bounds one frame's payload.
const maxFrame = 1 << 20

// Ops a Request names.
const (
	OpExec    = "exec"    // run Argv
	OpPTY     = "pty"     // run Argv on a terminal of Rows by Cols
	OpPing    = "ping"    // answer at once
	OpSqueeze = "squeeze" // hand memory the guest is not using back to the host (before a nap, and when idle)
	OpWoke    = "woke"    // the VM was just restored from a nap: set the clock to UnixNano, reseed randomness
)

// Request is the Hello frame's payload.
type Request struct {
	Op    string   `json:"op"`
	Argv  []string `json:"argv,omitempty"`
	Stdin bool     `json:"stdin,omitempty"` // stdin frames follow; else the command reads nothing
	// Root: the VM's binding has root=on. The agent gives the box's user
	// sudo the first time a command says so; the image itself gives it none.
	// Docker: the binding has docker=on, and the agent starts the image's
	// Docker likewise, and waits for it. A command that says either fails in
	// a guest that cannot do it, and in one whose agent does not know the
	// word (Result.Root, Result.Docker).
	Root   bool `json:"root,omitempty"`
	Docker bool `json:"docker,omitempty"`
	// UnixNano is the host's clock (OpWoke).
	UnixNano int64 `json:"unix_nano,omitempty"`
	// Rows and Cols are the terminal's size when the command starts (OpPTY).
	Rows uint16 `json:"rows,omitempty"`
	Cols uint16 `json:"cols,omitempty"`
}

// Size is a terminal's size.
type Size struct{ Rows, Cols uint16 }

func (s Size) bytes() []byte {
	return binary.BigEndian.AppendUint16(binary.BigEndian.AppendUint16(nil, s.Rows), s.Cols)
}

// sizeOf reads a Size frame's payload.
func sizeOf(p []byte) (Size, bool) {
	if len(p) != 4 {
		return Size{}, false
	}
	return Size{binary.BigEndian.Uint16(p), binary.BigEndian.Uint16(p[2:])}, true
}

// Result is the Exit frame's payload. Code is the command's exit code (128
// plus the signal when one killed it, 127 when it could not start).
type Result struct {
	Code int    `json:"code"`
	Err  string `json:"err,omitempty"`
	// Root and Docker: the agent gave what the Request asked before it ran
	// the command. An agent built before those words leaves them out.
	Root   bool `json:"root,omitempty"`
	Docker bool `json:"docker,omitempty"`
}

// WriteFrame writes one frame.
func WriteFrame(w io.Writer, typ byte, p []byte) error {
	if len(p) > maxFrame {
		return fmt.Errorf("frame of %d bytes is over the limit", len(p))
	}
	b := make([]byte, 5+len(p))
	b[0] = typ
	binary.BigEndian.PutUint32(b[1:5], uint32(len(p)))
	copy(b[5:], p)
	_, err := w.Write(b)
	return err
}

// ReadFrame reads one frame.
func ReadFrame(r io.Reader) (byte, []byte, error) {
	var h [5]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(h[1:5])
	if n > maxFrame {
		return 0, nil, errors.New("frame over the limit")
	}
	p := make([]byte, n)
	if _, err := io.ReadFull(r, p); err != nil {
		return 0, nil, err
	}
	return h[0], p, nil
}

// frameWriter turns writes into frames of one type.
type frameWriter struct {
	w   io.Writer
	typ byte
}

func (f frameWriter) Write(p []byte) (int, error) {
	for off := 0; off < len(p); {
		n := min(len(p)-off, 32<<10)
		if err := WriteFrame(f.w, f.typ, p[off:off+n]); err != nil {
			return off, err
		}
		off += n
	}
	return len(p), nil
}

// Do sends req on conn and returns the guest's exit code: stdin goes to the
// command (nil: none), its stdout and stderr to the writers. An error means
// the connection failed, not that the command did.
func Do(conn io.ReadWriter, req Request, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	req.Stdin = stdin != nil
	b, err := json.Marshal(req)
	if err != nil {
		return 0, err
	}
	if err := WriteFrame(conn, FrameHello, b); err != nil {
		return 0, err
	}
	if stdin != nil {
		go func() {
			_, _ = io.Copy(frameWriter{conn, FrameStdin}, stdin)
			_ = WriteFrame(conn, FrameEOF, nil)
		}()
	}
	return answer(conn, req, stdout, stderr)
}

// DoPTY is Do for a command on a terminal (OpPTY): in is what is typed on
// it, out what it shows. size answers the size the terminal should have
// now. It is asked at the start, at each tick of resized, and before
// anything typed is sent on, so a command that reads what was typed after a
// resize reads it on a terminal that already has the new size. The terminal
// is the guest's, so in's bytes go as they are: the guest's terminal echoes
// them, edits the line and turns a Ctrl-C into the signal.
func DoPTY(conn io.ReadWriter, req Request, size func() Size, resized <-chan struct{}, in io.Reader, out io.Writer) (int, error) {
	sent := size()
	req.Op, req.Stdin, req.Rows, req.Cols = OpPTY, true, sent.Rows, sent.Cols
	b, err := json.Marshal(req)
	if err != nil {
		return 0, err
	}
	var mu, sizing sync.Mutex
	w := lockedWriter{&mu, conn} // typing and resizes are two writers
	if err := WriteFrame(w, FrameHello, b); err != nil {
		return 0, err
	}
	resize := func() {
		sizing.Lock()
		defer sizing.Unlock()
		if now := size(); now != sent {
			_ = WriteFrame(w, FrameSize, now.bytes())
			sent = now
		}
	}
	go func() { _, _ = io.Copy(typing{frameWriter{w, FrameStdin}, resize}, in) }()
	done := make(chan struct{})
	defer close(done)
	go func() {
		for {
			select {
			case _, ok := <-resized:
				if !ok {
					return
				}
				resize()
			case <-done:
				return
			}
		}
	}()
	return answer(conn, req, out, out)
}

// typing sends what is typed on a terminal, each piece after the size the
// terminal has when the piece is sent.
type typing struct {
	w      io.Writer
	resize func()
}

func (t typing) Write(p []byte) (int, error) {
	t.resize()
	return t.w.Write(p)
}

// oldAgent is what the host says of an image whose agent does not know a
// word of the request.
const oldAgent = ": it was built from an older source than this driver; rebuild the image"

// answer reads the guest's frames for req to its Exit frame.
func answer(conn io.Reader, req Request, stdout, stderr io.Writer) (int, error) {
	for {
		typ, p, err := ReadFrame(conn)
		if err != nil {
			return 0, fmt.Errorf("the guest went away: %w", err)
		}
		switch typ {
		case FrameStdout:
			if stdout != nil {
				_, _ = stdout.Write(p)
			}
		case FrameStderr:
			if stderr != nil {
				_, _ = stderr.Write(p)
			}
		case FrameExit:
			var res Result
			if err := json.Unmarshal(p, &res); err != nil {
				return 0, err
			}
			if req.Op == OpPTY && res.Err == unknownOp+OpPTY {
				res.Err = "the image's potluck-guest gives no terminal" + oldAgent
			}
			if res.Err != "" {
				if stderr != nil {
					fmt.Fprintln(stderr, res.Err)
				}
				return res.Code, nil
			}
			if req.Root && !res.Root || req.Docker && !res.Docker {
				if stderr != nil {
					fmt.Fprintln(stderr, "the image's potluck-guest gives no root or docker"+oldAgent)
				}
				return 127, nil
			}
			return res.Code, nil
		}
	}
}

// GuestAddr is the address a guest with this context id takes: the host
// makes the id from the guest's IPv4 address (cloudhv.go), so an image needs
// no per-VM configuration. ok is false for an id that is not such an address.
func GuestAddr(cid uint32) (ip, gw net.IP, ok bool) {
	b := make(net.IP, 4)
	binary.BigEndian.PutUint32(b, cid)
	if b[0] != 10 || b[3]&3 != 2 { // a private /30's second host address
		return nil, nil, false
	}
	g := make(net.IP, 4)
	binary.BigEndian.PutUint32(g, cid-1)
	return b, g, true
}
