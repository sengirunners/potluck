package vmhost

// Background processes in a box (SPEC 20 "Boxes", background processes):
// spawn, output and signal. spawn starts a detached holder (this executable
// again, like a run's supervisor) that runs the command through the layer's
// Exec with execWrap, as exec does, and writes its output and exit under
// the box's procs directory (boxproc); output reads those
// files on the host, never asking the guest; signal is a short guest exec
// that signals the process group execWrap wrote to the guest's
// $TMPDIR/.zm-proc-<pid>. Processes end with the box: its VM goes, and the
// sweep deletes the directory. One file serves Tart, Incus and OCI.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/sengirunners/potluck/boxproc"
)

// flagHold re-executes the driver as one process's holder.
const flagHold = "--hold"

func (s store) procs(id string) boxproc.Procs {
	return boxproc.Procs(filepath.Join(s.dir, "procs", id))
}

// endProcs forgets an ended box's processes: a holder still there (its VM
// is gone, so its exec ends anyway) is killed first.
func (s store) endProcs(id string) {
	ps := s.procs(id)
	for _, pd := range ps.Running() {
		if m, err := pd.Meta(); err == nil && m.Holder > 0 {
			_ = syscall.Kill(m.Holder, syscall.SIGKILL)
		}
	}
	_ = os.RemoveAll(string(ps))
}

// spawn starts argv in a ready box and answers its pid; the same request_id
// answers the same pid.
func (d *drv) spawn(handle string, raw json.RawMessage) (any, error) {
	a, err := parseCmd(raw)
	if err != nil {
		return nil, err
	}
	var k struct {
		RequestID string `json:"request_id"`
	}
	_ = json.Unmarshal(raw, &k)
	timeout := 0
	if a.timeout != nil {
		timeout = *a.timeout
	}
	switch {
	case k.RequestID == "" || len(k.RequestID) > 200:
		return nil, refuse("bad_request", "spawn needs a request_id (at most 200 bytes)")
	case timeout < 0 || timeout > d.cfg.TTLMax:
		return nil, refuse("bad_request", "timeout_s %d is outside 0..%d", timeout, d.cfg.TTLMax)
	}
	p, err := d.spawnPTY(raw) // pty.go
	if err != nil {
		return nil, err
	}
	r, err := d.readyBox(handle)
	if err != nil {
		return nil, err
	}
	secrets, err := parseSecrets(a.secrets)
	if err != nil {
		return nil, err
	}
	if err := secretsListed(d.cfg, r, len(secrets)); err != nil {
		return nil, err
	}
	if len(secrets) > 0 { // a guest that takes none answers forbidden now, not a process exiting 125 (secrets.go)
		if err := d.secretsSafe(r); err != nil {
			return nil, err
		}
	}
	var pid string
	err = d.st.locked(func() error {
		ps := d.st.procs(r.ID)
		if p, ok := ps.Find(k.RequestID); ok {
			pid = p
			return nil
		}
		dir, err := ps.New(k.RequestID)
		if err != nil {
			return asRefusal(err)
		}
		if p != nil { // before the holder starts: it rewrites the record
			m, _ := dir.Meta()
			m.PTY = true
			if err := dir.SetMeta(m); err != nil {
				return err
			}
		}
		spec, _ := json.Marshal(boxproc.Spec{Argv: a.argv, Stdin: a.stdin, Cwd: a.cwd, TimeoutS: timeout, Secrets: secrets, PTY: p})
		holder, err := d.startHolder(r.ID, dir.ID(), spec)
		if err != nil {
			_ = os.RemoveAll(string(dir))
			return refuse("unavailable", "spawn in %s: %v", r.Handle, err)
		}
		m, _ := dir.Meta()
		m.Holder = holder
		pid = dir.ID()
		return dir.SetMeta(m)
	})
	if err != nil {
		return nil, err
	}
	return map[string]any{"pid": pid}, nil
}

// startHolder starts a detached holder; spec goes to it through a pipe the
// driver writes whole before it answers (a pipe keeps its bytes for the
// reader after the writer exits), never through argv, env or a file.
func (d *drv) startHolder(id, pid string, spec []byte) (int, error) {
	self, err := os.Executable()
	if err != nil {
		return 0, err
	}
	logf, err := os.OpenFile(filepath.Join(d.st.dir, "logs", id+".supervisor.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return 0, err
	}
	defer logf.Close()
	pr, pw, err := os.Pipe()
	if err != nil {
		return 0, err
	}
	defer pw.Close()
	cmd := exec.Command(self, flagHold, d.st.dir, id+"/"+pid)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = pr, logf, logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	err = cmd.Start()
	pr.Close()
	if err != nil {
		return 0, err
	}
	holder := cmd.Process.Pid
	_ = cmd.Process.Release()
	if _, err := pw.Write(spec); err != nil {
		_ = syscall.Kill(holder, syscall.SIGKILL)
		return 0, err
	}
	return holder, nil
}

// hold is one process's holder: it runs the spec in the box's VM and
// records its output and exit.
func hold(dir, idPid string) int {
	id, pid, _ := strings.Cut(idPid, "/")
	st := store{dir}
	r, err := st.load(id)
	if err != nil {
		return 1
	}
	pd, err := st.procs(id).Dir(pid)
	if err != nil {
		return 1
	}
	b, _ := io.ReadAll(io.LimitReader(os.Stdin, 4<<20))
	var spec boxproc.Spec
	cfg, cerr := parseConfig(r.Config)
	if err := json.Unmarshal(b, &spec); err != nil || cerr != nil || len(spec.Argv) == 0 {
		return boxproc.Hold(pd, func(io.Writer, io.Writer) (func() int, error) {
			return nil, fmt.Errorf("the spawn did not reach its holder whole")
		})
	}
	if spec.PTY != nil {
		return holdPTY(pd, r, cfg, spec, pid) // pty.go
	}
	return boxproc.Hold(pd, func(stdout, stderr io.Writer) (func() int, error) {
		var stdin io.Reader = bytes.NewReader(spec.Stdin)
		if len(spec.Secrets) > 0 {
			stdin = io.MultiReader(bytes.NewReader(frameSecrets(spec.Secrets)), stdin)
		}
		t := spec.TimeoutS
		if t == 0 {
			t = max(cfg.TTLMax, 1) // no timeout: it ends with the box, which never outlives the cap
		}
		cmd := r.bindLayer(newLayer(cfg)).Exec(r.VM, stdin, stdout, stderr, wrapped(r, spec.Cwd, t, len(spec.Secrets) > 0, ".zm-proc-"+pid, spec.Argv)...)
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		cmd.WaitDelay = 2 * time.Second
		limit := spec.TimeoutS
		spec = boxproc.Spec{} // the secrets live in stdin's reader only
		if err := cmd.Start(); err != nil {
			return nil, err
		}
		return func() int {
			if limit > 0 { // a backstop: the guest's own clock ends it first (exit 124)
				t := time.AfterFunc(time.Duration(limit)*time.Second+execHostMargin, func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) })
				defer t.Stop()
			}
			_ = cmd.Wait()
			code := cmd.ProcessState.ExitCode()
			if code < 0 {
				code = 128 + int(syscall.SIGKILL)
			}
			return code
		}, nil
	})
}

// output pages a process's stdout and stderr (boxproc.Output).
func (d *drv) output(handle string, raw json.RawMessage) (any, error) {
	a, err := boxproc.ParseOutput(raw)
	if err != nil {
		return nil, asRefusal(err)
	}
	r, err := d.readyBox(handle)
	if err != nil {
		return nil, err
	}
	pd, err := d.st.procs(r.ID).Dir(a.PID)
	if err != nil {
		return nil, asRefusal(err)
	}
	out, err := boxproc.Output(pd, a, func() error { _, err := d.readyBox(handle); return err })
	return out, asRefusal(err)
}

// signalProg signals the group named in $TMPDIR/$1 with $2: exit 3 when the
// file is not there (yet, or any more), 4 when the group is gone.
const signalProg = `f=${TMPDIR:-/tmp}/$1; [ -f "$f" ] || exit 3
exec perl -e 'open(my $f, "<", $ARGV[1]) or exit 3; my $p = <$f>; defined $p or exit 3; chomp $p; $p =~ /^[0-9]+$/ or exit 3; exit(kill($ARGV[0], -$p) ? 0 : 4)' "$2" "$f"`

// signal sends TERM, INT or KILL to a process's group, in the guest.
func (d *drv) signal(handle string, raw json.RawMessage) (any, error) {
	a, err := boxproc.ParseSignal(raw)
	if err != nil {
		return nil, asRefusal(err)
	}
	r, err := d.readyBox(handle)
	if err != nil {
		return nil, err
	}
	pd, err := d.st.procs(r.ID).Dir(a.PID)
	if err != nil {
		return nil, asRefusal(err)
	}
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(100 * time.Millisecond) {
		if !pd.Running() {
			return map[string]any{"sent": false}, nil
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		cmd := d.vm.Exec(r.VM, nil, io.Discard, io.Discard, "sh", "-c", signalProg, "sh", ".zm-proc-"+a.PID, a.Signal)
		stop := context.AfterFunc(ctx, func() {
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
		})
		err := cmd.Run()
		stop()
		cancel()
		code := 0
		if err != nil {
			code = -1
			if cmd.ProcessState != nil {
				code = cmd.ProcessState.ExitCode()
			}
		}
		switch {
		case code == 0:
			return map[string]any{"sent": true}, nil
		case code == 4:
			return map[string]any{"sent": false}, nil
		case code != 3 || !time.Now().Before(deadline): // the group file never showed: the process is not in the guest
			if !pd.Running() {
				return map[string]any{"sent": false}, nil
			}
			return nil, refuse("unavailable", "could not signal %s in %s", a.PID, r.Handle)
		}
	}
}
