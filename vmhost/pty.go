package vmhost

// A box's terminals (SPEC 20 "Boxes", terminals; zm-eakh.12.5.21): a
// process spawned with pty runs through the layer's own terminal exec
// (docker exec -it, incus exec --force-interactive, tart exec -i -t) whose
// host side is a pseudo-terminal the holder owns (boxproc); the layer
// carries its size into the guest, so a resize on the host side reaches the
// guest. input writes to it and resizes it. No ssh key, port or credential:
// it is the exec channel, as exec's is.

import (
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/sengirunners/potluck/boxproc"
)

// ttyLayer is a layer whose exec can give the guest command a terminal:
// argv runs on a guest pseudo-terminal whose size follows the host
// terminal the caller attaches as stdin, stdout and stderr (left unset).
type ttyLayer interface {
	ExecTTY(vm string, argv ...string) *exec.Cmd
}

func (t tartCLI) ExecTTY(vm string, argv ...string) *exec.Cmd {
	return exec.Command(t.bin, append([]string{"exec", "-i", "-t", vm}, argv...)...)
}

func (o ociCLI) ExecTTY(vm string, argv ...string) *exec.Cmd {
	return o.command(append([]string{"exec", "-i", "-t", vm}, argv...)...)
}

func (l incusCLI) ExecTTY(vm string, argv ...string) *exec.Cmd {
	return l.command(append([]string{"exec", l.ref(vm), "--project", l.project, "--force-interactive", "--"}, argv...)...)
}

// ttyWrap runs in the guest for a process on a terminal: HOME, TMPDIR,
// TERM and the cwd as execWrap sets them, its pid (its terminal session's
// group, which the layer's exec made) written to $TMPDIR/$3 for signal,
// then argv itself, so the terminal is the command's own. $1 is the
// default cwd ("" = HOME), $2 the cwd asked for, $4 TERM, $5 guestMem: a
// terminal takes no secrets of its own, but sees the box's (secrets.go)
// when it is a directory (not a link) of the user the terminal runs as.
const ttyWrap = `base=$1; cwd=$2; pf=$3; TERM=$4; mem=$5; export TERM; shift 5
unset ZM_SECRETS POTLUCK_SECRETS
b=$mem/potluck-secrets
if [ -d "$b" ] && [ ! -L "$b" ] && [ -O "$b" ]; then POTLUCK_SECRETS=$b; ZM_SECRETS=$b; export POTLUCK_SECRETS ZM_SECRETS; fi
` + guestHome + `TMPDIR=${TMPDIR:-/tmp}; export TMPDIR
` + baseInHome + `[ -n "$base" ] || base=$HOME
case $cwd in "") d=$base ;; /*) d=$cwd ;; *) d=$base/$cwd ;; esac
cd "$d" 2>/dev/null || { echo "potluck-driver-tart: no directory $d in the box" >&2; exit 125; }
echo $$ > "$TMPDIR/$pf.$$" && mv -f "$TMPDIR/$pf.$$" "$TMPDIR/$pf"
exec "$@"`

// ttyWrapped is the guest argv for argv on a terminal.
func ttyWrapped(r record, cwd, pf, term string, argv []string) []string {
	base := ""
	if r.Commit != "" {
		base = srcDir(r)
	}
	return databaseWrapped(r, append([]string{"sh", "-c", ttyWrap, "sh", base, cwd, pf, term, guestMem}, argv...))
}

// spawnPTY reads spawn's pty: nil without one; not_implemented on a layer
// with no terminal exec.
func (d *drv) spawnPTY(raw json.RawMessage) (*boxproc.PTY, error) {
	p, err := boxproc.ParsePTY(raw)
	if err != nil || p == nil {
		return nil, asRefusal(err)
	}
	if _, ok := d.vm.(ttyLayer); !ok {
		return nil, refuse("not_implemented", "the %s layer gives no terminal", d.cfg.VM)
	}
	return p, nil
}

// holdPTY is the holder of a process on a terminal.
func holdPTY(pd boxproc.Dir, r record, cfg config, spec boxproc.Spec, pid string) int {
	l := r.bindLayer(newLayer(cfg))
	tl, ok := l.(ttyLayer)
	return boxproc.Hold(pd, func(stdout, _ io.Writer) (func() int, error) {
		if !ok {
			return nil, fmt.Errorf("the %s layer gives no terminal", cfg.VM)
		}
		cmd := tl.ExecTTY(r.VM, ttyWrapped(r, spec.Cwd, ".zm-proc-"+pid, spec.PTY.Term, spec.Argv)...)
		wait, err := boxproc.StartPTY(pd, cmd, *spec.PTY, spec.Stdin, stdout, nil)
		if err != nil {
			return nil, err
		}
		limit := spec.TimeoutS
		return func() int {
			var late atomic.Bool
			if limit > 0 {
				t := time.AfterFunc(time.Duration(limit)*time.Second, func() {
					late.Store(true)
					// The guest command first (the layer's exec may leave it running), then the exec.
					_ = l.Exec(r.VM, nil, io.Discard, io.Discard, "sh", "-c", signalProg, "sh", ".zm-proc-"+pid, "KILL").Run()
					_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
				})
				defer t.Stop()
			}
			_ = wait()
			if late.Load() {
				return 124
			}
			return boxproc.ExitCode(cmd.ProcessState)
		}, nil
	})
}

// input writes to a process's terminal and resizes it (boxproc.Input).
func (d *drv) input(handle string, raw json.RawMessage) (any, error) {
	a, data, err := boxproc.ParseInput(raw)
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
	out, err := boxproc.Input(pd, a, data)
	return out, asRefusal(err)
}

// ptyVerbs are what describe adds for a layer with a terminal exec.
func (d *drv) ptyVerbs() bool {
	_, ok := d.vm.(ttyLayer)
	return ok && d.cfg.Kind == "box"
}
