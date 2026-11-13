package refdriver

// A box's terminals (SPEC 20 "Boxes", terminals; zm-eakh.12.5.21): spawn
// with pty runs the command on a local pseudo-terminal (boxproc.StartPTY),
// and input writes to it and resizes it, with a Bug switch for each way a
// driver can get them wrong.

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/sengirunners/potluck/boxproc"
)

// ptyServed: this driver serves spawn's pty and input (whether or not it
// lists them).
func (d *drv) ptyServed() bool { return d.cfg["kind"] == "box" && !d.is("box-no-spawn", "box-no-pty") }

// ptyDescribe adds input and capability pty to describe, as the bug says.
func (d *drv) ptyDescribe(out map[string]any, caps []string) []string {
	if !d.ptyServed() || d.is("pty-unlisted") {
		return caps
	}
	if !d.is("pty-cap-only") {
		out["verbs"] = append(out["verbs"].([]string), "input")
	}
	return append(caps, "pty")
}

// spawnPTY reads spawn's pty for spawn: nil without one, or when the bug
// ignores it.
func (d *drv) spawnPTY(raw json.RawMessage) (*boxproc.PTY, error) {
	p, err := boxproc.ParsePTY(raw)
	if err != nil {
		return nil, asRefusal(err)
	}
	if p != nil && !d.ptyServed() {
		return nil, refuse("not_implemented", "no terminals")
	}
	if d.is("pty-no-tty") {
		return nil, nil
	}
	return p, nil
}

// holdPTY runs one process on a terminal and records its output and exit.
func holdPTY(d *drv, r record, pd boxproc.Dir, spec boxproc.Spec) int {
	return boxproc.Hold(pd, func(stdout, _ io.Writer) (func() int, error) {
		cmd := exec.Command(spec.Argv[0], spec.Argv[1:]...)
		cmd.Dir = boxDir(r, spec.Cwd)
		tmp := d.boxTmp(r) // volumes.go
		cmd.Env = append([]string{"PATH=" + os.Getenv("PATH"), "HOME=" + r.Dir, "TMPDIR=" + tmp, "LANG=C", "TERM=" + spec.PTY.Term}, d.boxSecretsEnv(r)...)
		wait, err := boxproc.StartPTY(pd, cmd, *spec.PTY, spec.Stdin, stdout, nil)
		if err != nil {
			return nil, err
		}
		mark := filepath.Join(r.Dir, ".execs", strconv.Itoa(cmd.Process.Pid)) // the box's end kills the group
		if os.MkdirAll(filepath.Dir(mark), 0o700) == nil {
			_ = os.WriteFile(mark, nil, 0o600)
		}
		d.publishGroup(pd, cmd.Process.Pid) // a new session: its group is its pid
		return func() int {
			var late atomic.Bool
			if spec.TimeoutS > 0 {
				t := time.AfterFunc(time.Duration(spec.TimeoutS)*time.Second, func() {
					late.Store(true)
					_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
				})
				defer t.Stop()
			}
			_ = wait()
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			_ = os.Remove(mark)
			if late.Load() {
				return 124
			}
			return boxproc.ExitCode(cmd.ProcessState)
		}, nil
	})
}

// boxDir is a command's directory in the box: cwd against the checkout
// (else the box's home), an absolute one inside the box.
func boxDir(r record, cwd string) string {
	dir := r.Dir
	if r.Checkout != "" {
		dir = r.Checkout
	}
	switch {
	case filepath.IsAbs(cwd):
		dir = filepath.Join(r.Dir, filepath.Clean(cwd))
	case cwd != "":
		dir = filepath.Join(dir, filepath.Clean("/"+cwd))
	}
	return dir
}

func (d *drv) input(req request) (any, error) {
	if !d.ptyServed() || d.is("pty-cap-only") {
		return nil, refuse("not_implemented", "verb %q not supported", req.Verb)
	}
	a, data, err := boxproc.ParseInput(req.Args)
	if err != nil {
		return nil, asRefusal(err)
	}
	if d.is("input-base64-as-text") && a.Data != nil {
		data = []byte(*a.Data)
	}
	r, err := d.readyBox(req.ID)
	if err != nil {
		return nil, err
	}
	pd, err := procsOf(d.state, r.Handle).Dir(a.PID)
	if err != nil {
		return nil, asRefusal(err)
	}
	if d.is("input-no-pty-ok") {
		if m, _ := pd.Meta(); !m.PTY {
			return map[string]any{"written": len(data), "running": pd.Running()}, nil
		}
	}
	if d.is("input-ignored") {
		return map[string]any{"written": len(data), "running": pd.Running()}, nil
	}
	switch {
	case d.is("resize-ignored"):
		a.Rows, a.Cols = 0, 0
	case d.is("input-drops-ctrl-c"):
		data = bytes.ReplaceAll(data, []byte{3}, nil)
	}
	if d.is("input-ended-error") && !pd.Running() {
		return nil, refuse("conflict", "%s ended", a.PID)
	}
	out, err := boxproc.Input(pd, a, data)
	return out, asRefusal(err)
}
