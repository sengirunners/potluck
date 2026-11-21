// Package cloudhvguest is the guest side of the cloud-hypervisor VM layer
// (vmhost/cloudhv.go, docs/cloudhv.md): the wire format between the host and
// potluck-guest, the small agent that runs inside each VM and answers on the
// VM's vsock.
//
// One connection carries one request. The host sends a Hello frame (JSON),
// then, for an exec, stdin frames; the guest answers with stdout and stderr
// frames and one Exit frame. Closing the connection before Exit ends the
// guest command (its whole process group is killed).
package cloudhvguest

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
)

// Port is the vsock port the agent listens on. Below 1024, so only root in
// the guest can bind it: if the agent ever stops, the box's user cannot
// answer the host in its place.
const Port = 1000

// Frame types.
const (
	FrameHello  byte = 'H' // host: Request as JSON
	FrameStdin  byte = 'I' // host: bytes for the command's stdin
	FrameEOF    byte = 'E' // host: stdin is done
	FrameStdout byte = 'O' // guest: the command's stdout
	FrameStderr byte = 'R' // guest: the command's stderr
	FrameExit   byte = 'X' // guest: Result as JSON, the last frame
)

// maxFrame bounds one frame's payload.
const maxFrame = 1 << 20

// Ops a Request names.
const (
	OpExec    = "exec"    // run Argv
	OpPing    = "ping"    // answer at once
	OpSqueeze = "squeeze" // hand memory the guest is not using back to the host (before a nap, and when idle)
	OpWoke    = "woke"    // the VM was just restored from a nap: set the clock to UnixNano, reseed randomness
)

// Request is the Hello frame's payload.
type Request struct {
	Op    string   `json:"op"`
	Argv  []string `json:"argv,omitempty"`
	Stdin bool     `json:"stdin,omitempty"` // stdin frames follow; else the command reads nothing
	// UnixNano is the host's clock (OpWoke).
	UnixNano int64 `json:"unix_nano,omitempty"`
}

// Result is the Exit frame's payload. Code is the command's exit code (128
// plus the signal when one killed it, 127 when it could not start).
type Result struct {
	Code int    `json:"code"`
	Err  string `json:"err,omitempty"`
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
			if res.Err != "" && stderr != nil {
				fmt.Fprintln(stderr, res.Err)
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
