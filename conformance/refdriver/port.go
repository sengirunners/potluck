package refdriver

import (
	"context"
	"encoding/json"
	"github.com/sengirunners/potluck/boxport"
	"github.com/sengirunners/potluck/protocol"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

func (d *drv) port(req request) (any, error) {
	r, err := d.readyBox(req.ID)
	if err != nil {
		if d.is("port-after-release") {
			return protocol.PortResult{}, nil
		}
		return nil, err
	}
	var a protocol.PortRequest
	if json.Unmarshal(req.Args, &a) != nil {
		return nil, refuse("bad_request", "invalid port request")
	}
	if d.is("port-any-port") && a.Op == "open" && (a.Port < 1 || a.Port > 65535) {
		return protocol.PortResult{Stream: strings.Repeat("0", 48)}, nil
	}
	if d.is("port-no-page-cap") && len(a.Data) > protocol.PortPage {
		return protocol.PortResult{}, nil
	}
	if d.is("port-half-duplex") {
		if err := portDirection(r.Dir, a); err != nil {
			return nil, err
		}
	}
	if d.is("port-close-ignored") && a.Op == "close" {
		return protocol.PortResult{}, nil
	}
	if d.is("port-cross-stream") && a.Op == "read" && len(a.Stream) == 48 {
		if _, err := os.Stat(filepath.Join(r.Dir, ".potluck-port-"+a.Stream)); os.IsNotExist(err) {
			return protocol.PortResult{}, nil
		}
	}
	out, err := boxport.Exchange(context.Background(), a, r.Deadline, func(in io.Reader, out, errout io.Writer, argv ...string) *exec.Cmd {
		cmd := exec.Command(argv[0], argv[1:]...)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = in, out, errout
		cmd.Env = append(os.Environ(), "TMPDIR="+r.Dir)
		return cmd
	})
	if d.is("port-half-duplex") && a.Op == "close" && err == nil {
		_ = os.Remove(filepath.Join(r.Dir, "port-direction-"+a.Stream))
	}
	if err != nil {
		if e, ok := err.(*protocol.Error); ok {
			return nil, refuse(e.Code, "%s", e.Message)
		}
	}
	return out, err
}

// A genuinely half-duplex stream chooses one direction on its first real I/O
// and refuses the opposite direction until close. The state lives in the
// guest directory, so release also clears it. No payload-specific behavior.
func portDirection(dir string, a protocol.PortRequest) error {
	if a.Op != "read" && a.Op != "write" {
		return nil
	}
	if len(a.Stream) != 48 || strings.Trim(a.Stream, "0123456789abcdef") != "" {
		return nil
	}
	if _, err := os.Stat(filepath.Join(dir, ".potluck-port-"+a.Stream)); err != nil {
		return nil
	}
	f, err := os.OpenFile(filepath.Join(dir, "port-direction-"+a.Stream), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	selected := []byte{0}
	n, err := f.ReadAt(selected, 0)
	if err != nil && err != io.EOF {
		return err
	}
	direction := a.Op[0]
	if n > 0 && selected[0] != direction {
		return refuse("conflict", "port stream selected the opposite I/O direction")
	}
	if n == 0 {
		_, err = f.WriteAt([]byte{direction}, 0)
	}
	return err
}
