package refdriver

// A box's background processes (SPEC 20 "Boxes", background processes):
// spawn, output and signal over boxproc, with a Bug
// switch for each way a driver can get them wrong. A process runs in its
// own process group in the box's directory under a detached holder; the
// box's end kills the group (its .execs mark) and forgets the directory.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/sengirunners/potluck/boxproc"
)

const flagHold = "-zm-ref-hold"

var procVerbs = []string{"spawn", "output", "signal"}

func procsOf(state, handle string) boxproc.Procs {
	return boxproc.Procs(filepath.Join(state, "procs-"+strings.NewReplacer("/", "_", ":", "_").Replace(handle)))
}

func asRefusal(err error) error {
	var e *boxproc.Refusal
	if errors.As(err, &e) {
		return refuse(e.Code, "%s", e.Msg)
	}
	return err
}

// readyBox loads the box behind req.ID that takes commands now.
func (d *drv) readyBox(id string) (record, error) {
	p, err := d.parse(id)
	if err != nil {
		return record{}, err
	}
	r, err := load(p)
	if err != nil {
		return r, err
	}
	if (r.State != "ready" || time.Now().After(r.Deadline)) && !d.is("procs-after-release") {
		return r, refuse("conflict", "%s is %s, not ready", r.Handle, r.State)
	}
	return r, nil
}

func (d *drv) spawn(req request) (any, error) {
	var a struct {
		Argv      json.RawMessage   `json:"argv"`
		Stdin     *string           `json:"stdin"`
		Encoding  string            `json:"encoding"`
		Cwd       string            `json:"cwd"`
		Timeout   int               `json:"timeout_s"`
		Secrets   map[string]string `json:"secrets"`
		RequestID string            `json:"request_id"`
	}
	if err := json.Unmarshal(req.Args, &a); err != nil {
		return nil, refuse("bad_request", "bad args: %v", err)
	}
	var argv []string
	if json.Unmarshal(a.Argv, &argv) != nil || len(argv) == 0 {
		return nil, refuse("bad_request", "argv must be a non-empty array of strings")
	}
	if a.RequestID == "" || a.Timeout < 0 || a.Timeout > ttlMax {
		return nil, refuse("bad_request", "spawn needs a request_id, and timeout_s 0..%d", ttlMax)
	}
	stdin, err := boxproc.Decode(a.Stdin, a.Encoding)
	if err != nil {
		return nil, asRefusal(err)
	}
	if err := d.checkSecrets(a.Secrets); err != nil {
		return nil, err
	}
	p, err := d.spawnPTY(req.Args) // pty.go
	if err != nil {
		return nil, err
	}
	r, err := d.readyBox(req.ID)
	if err != nil {
		return nil, err
	}
	ps := procsOf(d.state, r.Handle)
	if err := os.MkdirAll(string(ps), 0o700); err != nil {
		return nil, err
	}
	var pid string
	err = locked(d.state, func() error {
		if p, ok := ps.Find(a.RequestID); ok && !d.is("spawn-not-idempotent") {
			pid = p
			return nil
		}
		if d.is("spawn-same-pid") {
			if rs := ps.Running(); len(rs) > 0 {
				pid = rs[0].ID()
				return nil
			}
		}
		pd, err := ps.New(a.RequestID)
		if err != nil {
			return asRefusal(err)
		}
		if p != nil { // before the holder starts: it rewrites the record
			m, _ := pd.Meta()
			m.PTY = true
			if err := pd.SetMeta(m); err != nil {
				return err
			}
		}
		spec, _ := json.Marshal(boxproc.Spec{Argv: argv, Stdin: stdin, Cwd: a.Cwd, TimeoutS: a.Timeout, Secrets: a.Secrets, PTY: p})
		holder, err := d.startHolder(r.Handle, pd.ID(), spec)
		if err != nil {
			_ = os.RemoveAll(string(pd))
			return err
		}
		m, _ := pd.Meta()
		m.Holder, pid = holder, pd.ID()
		return pd.SetMeta(m)
	})
	if err != nil {
		return nil, err
	}
	if d.is("spawn-no-pid") {
		return map[string]any{}, nil
	}
	return map[string]any{"pid": pid}, nil
}

// startHolder starts a detached holder, the spec on a pipe written whole.
func (d *drv) startHolder(handle, pid string, spec []byte) (int, error) {
	self, err := os.Executable()
	if err != nil {
		return 0, err
	}
	pr, pw, err := os.Pipe()
	if err != nil {
		return 0, err
	}
	defer pw.Close()
	cmd := exec.Command(self, flagHold, d.state, handle, pid, d.bug)
	cmd.Stdin = pr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	err = cmd.Start()
	pr.Close()
	if err != nil {
		return 0, err
	}
	holder := cmd.Process.Pid
	_ = cmd.Process.Release()
	_, err = pw.Write(spec)
	return holder, err
}

// publishGroup shares the spawning driver's lock with both holder paths.
// Read fresh metadata after acquiring it so the parent's Holder survives.
func (d *drv) publishGroup(pd boxproc.Dir, group int) {
	_ = locked(d.state, func() error {
		m, err := pd.Meta()
		if err != nil {
			return err
		}
		m.Group = group
		return pd.SetMeta(m)
	})
}

// hold runs one process and records its output and exit.
func hold(state, handle, pid, bug string) int {
	d := &drv{state: state, bug: bug}
	r, err := load(recordFile(state, handle))
	if err != nil {
		return 1
	}
	pd, err := procsOf(state, handle).Dir(pid)
	if err != nil {
		return 1
	}
	b, _ := io.ReadAll(os.Stdin)
	var spec boxproc.Spec
	_ = json.Unmarshal(b, &spec)
	if spec.PTY != nil && len(spec.Argv) > 0 {
		return holdPTY(d, r, pd, spec) // pty.go
	}
	return boxproc.Hold(pd, func(stdout, stderr io.Writer) (func() int, error) {
		if len(spec.Argv) == 0 {
			return nil, fmt.Errorf("no spec")
		}
		cmd := exec.Command(spec.Argv[0], spec.Argv[1:]...)
		cmd.Dir = boxDir(r, spec.Cwd)
		tmp := d.boxTmp(r) // volumes.go
		cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + r.Dir, "TMPDIR=" + tmp, "LANG=C"}
		sdir := ""
		if len(spec.Secrets) > 0 {
			var env []string
			if sdir, env, err = d.writeSecrets(r, spec.Secrets); err != nil {
				return nil, err
			}
			cmd.Env = append(cmd.Env, env...)
		} else {
			cmd.Env = append(cmd.Env, d.boxSecretsEnv(r)...) // secrets.go
		}
		cmd.Stdin = strings.NewReader(string(spec.Stdin))
		cmd.Stdout, cmd.Stderr = stdout, stderr
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err := cmd.Start(); err != nil {
			return nil, err
		}
		mark := filepath.Join(r.Dir, ".execs", strconv.Itoa(cmd.Process.Pid)) // the box's end kills the group
		if os.MkdirAll(filepath.Dir(mark), 0o700) == nil {
			_ = os.WriteFile(mark, nil, 0o600)
		}
		d.publishGroup(pd, cmd.Process.Pid)
		if sdir != "" && d.is("spawn-secrets-early") { // gone a second in, the process still running
			time.AfterFunc(time.Second, func() { _ = os.RemoveAll(sdir) })
		}
		return func() int {
			late := false
			if spec.TimeoutS > 0 {
				t := time.AfterFunc(time.Duration(spec.TimeoutS)*time.Second, func() {
					late = true
					_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
				})
				defer t.Stop()
			}
			_ = cmd.Wait()
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) // what it left in its group, as exec's end does
			_ = os.Remove(mark)
			if sdir != "" && !d.is("spawn-secrets-left") {
				_ = os.RemoveAll(sdir)
			}
			if late {
				return 124
			}
			return exitCode(cmd.ProcessState)
		}, nil
	})
}

func exitCode(ps *os.ProcessState) int {
	if ws, ok := ps.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	return ps.ExitCode()
}

func (d *drv) output(req request) (any, error) {
	a, err := boxproc.ParseOutput(req.Args)
	if err != nil {
		return nil, asRefusal(err)
	}
	r, err := d.readyBox(req.ID)
	if err != nil {
		return nil, err
	}
	pd, err := procsOf(d.state, r.Handle).Dir(a.PID)
	if err != nil {
		return nil, asRefusal(err)
	}
	switch d.bug {
	case "output-no-wait":
		a.WaitS = 0
	case "output-no-cap":
		a.MaxBytes = 64 << 20
	case "output-blocks": // nothing until the process ends
		for deadline := time.Now().Add(25 * time.Second); pd.Running() && time.Now().Before(deadline); {
			time.Sleep(50 * time.Millisecond)
		}
	case "output-keep-little": // only the newest 2 MiB kept
		if !pd.Running() {
			ps, _ := filepath.Glob(filepath.Join(string(pd), "stdout.*"))
			for len(ps) > 2 {
				_ = os.Remove(ps[0])
				ps = ps[1:]
			}
		}
	case "base64-text-out":
		a.Encoding = ""
	}
	out, err := boxproc.Output(pd, a, func() error { _, err := d.readyBox(req.ID); return err })
	if err != nil {
		return nil, asRefusal(err)
	}
	if d.is("output-drop-unreported") {
		delete(out, "dropped_stdout")
	}
	return out, nil
}

func (d *drv) signal(req request) (any, error) {
	a, err := boxproc.ParseSignal(req.Args)
	if err != nil {
		return nil, asRefusal(err)
	}
	r, err := d.readyBox(req.ID)
	if err != nil {
		return nil, err
	}
	pd, err := procsOf(d.state, r.Handle).Dir(a.PID)
	if err != nil {
		return nil, asRefusal(err)
	}
	for deadline := time.Now().Add(3 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if !pd.Running() {
			return map[string]any{"sent": d.is("signal-sent-always")}, nil
		}
		m, _ := pd.Meta()
		if m.Group > 0 {
			target := -m.Group
			if d.is("signal-leader-only") {
				target = m.Group
			}
			sig := boxproc.Signals[a.Signal]
			if d.is("signal-term-as-kill") && a.Signal == "TERM" { // no time to drain
				sig = syscall.SIGKILL
			}
			return map[string]any{"sent": syscall.Kill(target, sig) == nil}, nil
		}
		if !time.Now().Before(deadline) {
			return nil, refuse("unavailable", "%s has not started", a.PID)
		}
	}
}

// endProcs kills what a box's holders still hold and forgets them.
func endProcs(state, handle string) {
	ps := procsOf(state, handle)
	for _, pd := range ps.Running() {
		if m, err := pd.Meta(); err == nil {
			if m.Group > 0 {
				_ = syscall.Kill(-m.Group, syscall.SIGKILL)
			}
			if m.Holder > 0 {
				_ = syscall.Kill(m.Holder, syscall.SIGKILL)
			}
		}
	}
	_ = os.RemoveAll(string(ps))
}
