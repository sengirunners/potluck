package cli

// potluck run (copied from zm's cmd/zm job.go, "zm job run"): run a command
// at this checkout's commit in a fresh sandbox and wait for it. Place and
// start a sandbox (as sandbox create), push the commit (boxpush), start the
// command in the background, follow its output, answer its exit code, end
// the sandbox.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"os/signal"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/sengirunners/potluck/boxpush"
	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/computeclient"
	"github.com/sengirunners/potluck/depscli"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/ids"
	"github.com/sengirunners/potluck/placement"
)

// JobFile is the script run runs when no command is given, at the shipped
// commit: it lives with the code, so a commit always means one job.
const JobFile = ".potluck/job"

// Timing: the lease covers the push and the command with room to spare; a
// command the driver did not end by its timeout is killed jobKillGrace
// later; output calls failing in a row for jobLostAfter mean the sandbox is
// gone.
var (
	jobLeaseSlack = 10 * time.Minute
	jobKillGrace  = 90 * time.Second
	jobLostAfter  = 2 * time.Minute
)

type jobOptions struct {
	boxJobOptions
	repo string // the checkout ("" = the working directory)
	ref  string
	push pushFlags
	argv []string
	net  string // --net, as given
	df   depscli.Flags
}

// boxJobOptions is what boxJob needs: where the box goes, how long the
// command may run, and where its output goes.
type boxJobOptions struct {
	want    computeclient.BoxWant
	image   string
	timeout time.Duration
	// drain is how long a command sent TERM on Ctrl-C has to end by itself,
	// its output still followed, before KILL (0: stop following at once).
	drain time.Duration
	keep  bool
	// deps are the dependency layers the box is acquired with (depscli).
	deps      []computeapi.BoxDeps
	depsWaitS *int
	out       io.Writer // the command's stdout
	errw      io.Writer // its stderr and potluck's own lines
	// again, when set, is how boxJob learns of another Ctrl-C while it ends a
	// sandbox that may have been made: ctx comes back cancelled by it (nil:
	// the process's signals, caughtSignals).
	again func(ctx context.Context) (context.Context, context.CancelFunc)
}

// untilAgain is ctx, cancelled by another Ctrl-C (o.again).
func (o boxJobOptions) untilAgain(ctx context.Context) (context.Context, context.CancelFunc) {
	if o.again != nil {
		return o.again(ctx)
	}
	return signal.NotifyContext(ctx, caughtSignals()...)
}

// jobResult is --json's answer. State: exited (Exit is the command's),
// timeout (killed at --timeout, exit 124), lost (it ended with no exit code:
// its sandbox went; never a pass), interrupted (Ctrl-C), failed (potluck
// could not start it; Error says why).
type jobResult struct {
	Handle   string  `json:"handle,omitempty"`
	Target   string  `json:"target,omitempty"`
	Commit   string  `json:"commit,omitempty"`
	PID      string  `json:"pid,omitempty"`
	State    string  `json:"state"`
	Exit     *int    `json:"exit,omitempty"`
	Error    string  `json:"error,omitempty"`
	Kept     bool    `json:"kept"`
	Released bool    `json:"released"`
	Seconds  float64 `json:"seconds"`
}

// code is the command's exit code for r.
func (r jobResult) code() int {
	switch {
	case r.State == "exited" && r.Exit != nil:
		return *r.Exit
	case r.State == "timeout":
		return 124
	case r.State == "interrupted":
		return 130
	}
	return 1
}

func (a *app) runCmd() *cobra.Command {
	var o jobOptions
	cmd := &cobra.Command{Use: "run [--on <machine-size> | --size S --os O] [--ref HEAD] [--dirty] [--timeout 30m] [--keep] [-- <argv...>]",
		Short: "Run a command (default: the commit's .potluck/job) at this checkout's commit in a fresh sandbox; exits with its code",
		Long: `Places a sandbox as potluck sandbox create does, sends the commit (default
HEAD; with --dirty your uncommitted changes too) as a git bundle, starts the
command in the checkout, streams its output as it comes and exits with its
exit code. The sandbox ends at the end, also on Ctrl-C, an error or the
timeout (exit 124), unless --keep. A command that ends with no exit code
(its sandbox went) fails. No command: the commit's .potluck/job.`,
		Example: `  potluck run -- make test
  potluck run --os macos --timeout 45m --dirty
  potluck run --json -- go test ./... 2>log`,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := a.inPool(cmd.Context())
			if err != nil {
				return err
			}
			o.argv = args
			o.out, o.errw = a.stdout, a.stderr
			if o.want.Net, err = netFlag(o.net); err != nil {
				return err
			}
			var dreq computeapi.AcquireRunTargetRequest
			if err := o.df.Request(cmd.Context(), cmd.Flags(), o.repo, o.push.dir, &dreq); err != nil {
				return err
			}
			o.deps, o.depsWaitS = dreq.Deps, dreq.DepsWaitS
			o.want.Volumes = len(o.deps) > 0
			if len(o.deps) > 0 {
				depscli.WarnMismatch(cmd.Context(), a.stderr, "potluck", o.repo, o.ref, o.push.dirty, o.push.untracked, o.deps)
			}
			if a.json {
				o.out = a.stderr // stdout carries only the result
			}
			r, err := jobRun(cmd.Context(), c, o)
			if a.json {
				if err != nil && r.Error == "" {
					r.Error = err.Error()
				}
				if perr := a.printJSON(r); perr != nil {
					return perr
				}
				if r.code() == 0 {
					return nil
				}
				return exitSilently(r.code())
			}
			if err != nil {
				return exitWith{code: r.code(), err: err}
			}
			if r.code() != 0 {
				return exitSilently(r.code())
			}
			return nil
		}}
	f := cmd.Flags()
	f.StringVar(&o.want.On, "on", "", "a shared machine size, as potluck status lists it (default: the smallest that fits)")
	f.StringVar(&o.want.Size, "size", "", "a size the machine states")
	f.StringVar(&o.want.OS, "os", "", "linux or macos")
	f.StringVar(&o.image, "image", "", "one of the machine's images, by name")
	f.StringVar(&o.ref, "ref", "", "the commit to run, in this checkout (default HEAD)")
	f.DurationVar(&o.timeout, "timeout", 30*time.Minute, "kill the command after this long (exit 124)")
	f.BoolVar(&o.keep, "keep", false, "leave the sandbox up afterwards, for potluck sandbox exec (it ends at its lease)")
	f.StringVar(&o.net, "net", "on", netUsage)
	o.push.add(f)
	o.df.Add(f)
	return cmd
}

// jobRun runs one command. The result is filled as far as it got; err is
// set when potluck itself failed (state failed, or the sandbox went: lost).
func jobRun(ctx context.Context, c *client.Client, o jobOptions) (r jobResult, err error) {
	start := time.Now()
	defer func() { r.Seconds = time.Since(start).Round(time.Millisecond).Seconds() }()
	r.State = "failed"
	if o.timeout < time.Second {
		return r, fmt.Errorf("--timeout must be at least 1s")
	}
	if o.push.maxMiB <= 0 {
		o.push.maxMiB = boxpush.DefaultMax >> 20
	}
	o.push.repo = o.repo
	b, err := o.push.bundle(ctx, o.ref, o.errw)
	if err != nil {
		return r.stopped(ctx, err)
	}
	defer b.Close()
	r.Commit = b.Head
	argv := o.argv
	if len(argv) == 0 {
		if argv, err = jobFileArgv(ctx, b); err != nil {
			return r.stopped(ctx, err)
		}
	}
	return boxJob(ctx, c, o.boxJobOptions, r, func(ctx context.Context, bx acquiredBox) (string, error) {
		fmt.Fprintf(o.errw, "potluck: %s at %s\n", sandboxID(bx.handle), shortSHA(b.Head))
		pushed, err := boxpush.Push(ctx, boxpush.Exec(c, bx.target, bx.id), b, boxpush.PushOptions{Dest: o.push.dir})
		if err != nil {
			return "", err
		}
		// The checkout's path as the sandbox's shell has it ($HOME/<dir>).
		sp, err := computeclient.SpawnRunTarget(ctx, c, bx.target, bx.id, computeapi.SpawnRunTargetRequest{Argv: append([]string{"sh", "-c", jobCd, "potluck-run", pushed.Dir}, argv...),
			TimeoutS: int((o.timeout + time.Second - 1) / time.Second), RequestID: bx.key})
		if err != nil {
			return "", err
		}
		return sp.PID, nil
	})
}

// acquiredBox is a box boxJob holds: its binding, id and handle, and the
// acquire's request id (a spawn in it may reuse it as its own).
type acquiredBox struct{ target, id, handle, key string }

// boxStart starts a job's command in a box just acquired and answers its pid.
type boxStart func(ctx context.Context, bx acquiredBox) (pid string, err error)

// boxJob places and acquires a box, starts the command in it, follows its
// output until it ends, and releases the box (unless o.keep), Ctrl-C
// included. r carries what the caller filled; it comes back filled as far
// as it got.
func boxJob(ctx context.Context, c *client.Client, o boxJobOptions, r jobResult, start boxStart) (jobResult, error) {
	key := "job-" + strings.ToLower(ids.New())
	areq := computeapi.AcquireRunTargetRequest{RequestID: key, Image: o.image, Net: o.want.Net, Deps: o.deps, DepsWaitS: o.depsWaitS}
	var prepErr error // a refusal before the acquire: nothing was asked of that binding
	prep := func(ctx context.Context, target string, ar *computeapi.AcquireRunTargetRequest) error {
		prepErr = nil
		ttl := o.timeout + jobLeaseSlack
		v, err := computeclient.DescribeRunTarget(ctx, c, target)
		if ctx.Err() != nil { // Ctrl-C before the acquire: nothing was asked
			prepErr = ctx.Err()
			return prepErr
		}
		if err == nil {
			lim := v.TTLMaxS
			if v.Describe != nil {
				lim = placement.LifeCap(lim, v.Describe.TTLMaxS)
			}
			if capd := time.Duration(lim) * time.Second; lim > 0 && ttl > capd {
				if o.timeout+2*time.Minute > capd {
					// Nothing asked of it: the next binding may fit.
					return &computeclient.Misfit{Err: api.Errorf(api.CodeBadRequest, "--timeout %v does not fit %s's lifetime cap of %v (with two minutes to start it)", o.timeout, target, capd)}
				}
				ttl = capd
			}
		}
		ar.TTLS = int(ttl / time.Second)
		areq.TTLS = ar.TTLS // as this binding was asked: the interrupt below asks it again, the same
		return nil
	}
	target, h, err := computeclient.AcquireBox(ctx, c, o.want, areq, prep)
	r.Target = target
	if err != nil && (target == "" || err == prepErr || computeclient.MadeNothing(err)) { // nothing was made: nothing to end
		return r.stopped(ctx, err)
	}
	if err != nil && (ctx.Err() != nil || mayHaveMade(err)) {
		// Ctrl-C while the sandbox was being made, or a failure after which
		// the daemon kept the request: it may be made all the same. The same
		// key answers that same sandbox: end it (run takes no key to retry with).
		interrupted := ctx.Err() != nil
		if interrupted {
			r.State = "interrupted"
		}
		first, what := err, err.Error()
		if interrupted {
			what = "interrupted while the sandbox was being made"
		}
		actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Minute)
		defer cancel()
		actx, stopWaiting := o.untilAgain(actx)
		defer stopWaiting()
		stopHint := "Ctrl-C to stop waiting"
		if interrupted {
			stopHint = "Ctrl-C again to stop waiting"
		}
		fmt.Fprintf(o.errw, "potluck: ending the sandbox that may have been made (request id %s; %s: it then ends at its lease)\n", key, stopHint)
		if h, err = computeclient.AcquireRunTarget(actx, c, target, areq); err != nil {
			if computeclient.MadeNothing(err) {
				// The daemon forgot the request: nothing was made, nothing to end.
				return r.stopped(ctx, fmt.Errorf("%s (asked again, it made nothing): %w", what, err))
			}
			return r, fmt.Errorf("%s; the sandbox may still come up (request id %s, it ends at its lease): %w", what, key, err)
		}
		r.Handle = h.Handle
		_, id, _ := computeclient.SplitBoxHandle(h.Handle)
		if _, err := computeclient.ReleaseRunTarget(actx, c, target, id); err != nil {
			return r, fmt.Errorf("%s; could not end %s (it ends at its lease): %w", what, sandboxID(h.Handle), err)
		}
		r.Released = true
		if interrupted {
			return r, errInterrupted
		}
		return r, fmt.Errorf("%w (the sandbox it made is ended)", first)
	}
	if err != nil {
		return r, err
	}
	r.Handle = h.Handle
	depscli.Print(o.errw, "potluck", o.deps, h.Deps)
	sid := sandboxID(h.Handle)
	_, id, _ := computeclient.SplitBoxHandle(h.Handle)
	defer func() {
		if o.keep {
			r.Kept = true
			fmt.Fprintf(o.errw, "potluck: kept %s (potluck sandbox exec %s -- ...; potluck sandbox rm %s ends it)\n", sid, sid, sid)
			return
		}
		// Ended whatever happened, Ctrl-C included: on a context of its own.
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 60*time.Second)
		defer cancel()
		if _, rerr := computeclient.ReleaseRunTarget(rctx, c, target, id); rerr != nil {
			fmt.Fprintf(o.errw, "potluck: could not end %s (it ends at its lease): %v\n", sid, rerr)
			return
		}
		r.Released = true
	}()
	pid, err := start(ctx, acquiredBox{target: target, id: id, handle: h.Handle, key: key})
	if err != nil {
		return r.stopped(ctx, err)
	}
	r.PID = pid
	return follow(ctx, c, target, id, o, time.Now().Add(o.timeout+jobKillGrace), r)
}

// jobCd runs "$@" in the checkout ($1, absolute or under HOME).
const jobCd = `case "$1" in /*) d="$1" ;; *) d="$HOME/$1" ;; esac
cd "$d" || exit 127
shift
exec "$@"`

var errInterrupted = errors.New("interrupted before the command started")

// stopped is r for an error before the command ran: on Ctrl-C it is
// interrupted (exit 130), whatever the cut-off step said.
func (r jobResult) stopped(ctx context.Context, err error) (jobResult, error) {
	if ctx.Err() != nil {
		r.State = "interrupted"
		return r, errInterrupted
	}
	return r, err
}

// follow pages the command's output until it ends, Ctrl-C, or deadline.
func follow(ctx context.Context, c *client.Client, target, id string, o boxJobOptions, deadline time.Time, r jobResult) (jobResult, error) {
	req := computeapi.OutputRunTargetRequest{PID: r.PID, WaitS: 20, MaxBytes: 256 << 10}
	signal := func(sig string) {
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
		defer cancel()
		_, _ = computeclient.SignalRunTarget(sctx, c, target, id, computeapi.SignalRunTargetRequest{PID: r.PID, Signal: sig})
	}
	var failing time.Time
	for {
		if ctx.Err() != nil {
			signal("TERM")
			r.State = "interrupted"
			if o.drain > 0 && !drained(c, target, id, o, req, time.Now().Add(o.drain)) {
				signal("KILL")
			}
			return r, errors.New("interrupted: the command was stopped")
		}
		if time.Now().After(deadline) {
			signal("KILL")
			r.State = "timeout"
			return r, fmt.Errorf("the command outlived --timeout %v and was killed", o.timeout)
		}
		req.WaitS = max(min(20, int(time.Until(deadline)/time.Second)), 0)
		p, err := computeclient.RunTargetOutput(ctx, c, target, id, req)
		if err != nil {
			if ctx.Err() != nil {
				continue
			}
			if failing.IsZero() {
				failing = time.Now()
			}
			if !boxpush.Transient(err) || time.Since(failing) > jobLostAfter {
				r.State = "lost"
				return r, fmt.Errorf("the command's output is gone (lost, never a pass): %w", err)
			}
			select {
			case <-ctx.Done():
			case <-time.After(time.Second):
			}
			continue
		}
		failing = time.Time{}
		for _, d := range []struct {
			n    int64
			what string
		}{{p.DroppedStdout, "stdout"}, {p.DroppedStderr, "stderr"}} {
			if d.n > 0 {
				fmt.Fprintf(o.errw, "potluck: %d bytes of %s are no longer kept\n", d.n, d.what)
			}
		}
		_, _ = io.WriteString(o.out, p.Stdout)
		_, _ = io.WriteString(o.errw, p.Stderr)
		req.StdoutFrom, req.StderrFrom = p.StdoutNext, p.StderrNext
		switch {
		case p.Running:
		case p.Exit == nil:
			r.State = "lost"
			return r, errors.New("the command ended with no exit code (lost: its sandbox went); not a pass")
		case *p.Exit == 124:
			r.State, r.Exit = "timeout", p.Exit
			return r, fmt.Errorf("the command ran past --timeout %v (exit 124)", o.timeout)
		default:
			r.State, r.Exit = "exited", p.Exit
			return r, nil
		}
	}
}

// drained follows a command sent TERM until it ends (true), deadline
// passes, or another Ctrl-C comes, on a context of its own: the caller's is
// cancelled. A transient failure to read its output is tried again.
func drained(c *client.Client, target, id string, o boxJobOptions, req computeapi.OutputRunTargetRequest, deadline time.Time) bool {
	fmt.Fprintf(o.errw, "potluck: sent TERM; waiting up to %s for it to end (Ctrl-C again to stop it now)\n", time.Until(deadline).Round(time.Second))
	again, stop := o.untilAgain(context.Background())
	defer stop()
	for time.Now().Before(deadline) && again.Err() == nil {
		req.WaitS = max(min(20, int(time.Until(deadline)/time.Second)), 0)
		ctx, cancel := context.WithTimeout(again, time.Duration(req.WaitS+20)*time.Second)
		p, err := computeclient.RunTargetOutput(ctx, c, target, id, req)
		cancel()
		if err != nil {
			if again.Err() != nil || !boxpush.Transient(err) {
				return false
			}
			select {
			case <-again.Done():
			case <-time.After(time.Second):
			}
			continue
		}
		_, _ = io.WriteString(o.out, p.Stdout)
		_, _ = io.WriteString(o.errw, p.Stderr)
		req.StdoutFrom, req.StderrFrom = p.StdoutNext, p.StderrNext
		if !p.Running {
			return true
		}
	}
	return false
}

// jobFileArgv is the argv of the commit's JobFile: run directly when
// committed executable (its shebang decides), else with sh.
func jobFileArgv(ctx context.Context, b *boxpush.Bundle) ([]string, error) {
	out, err := exec.CommandContext(ctx, "git", "-C", b.Repo, "ls-tree", b.Head, "--", JobFile).Output()
	mode, _, _ := strings.Cut(string(out), " ")
	switch {
	case err != nil || mode == "":
		return nil, fmt.Errorf("no command given and no %s at %s: pass one after -- (potluck run -- make test) or commit a %s script", JobFile, shortSHA(b.Head), JobFile)
	case mode == "100755":
		return []string{"./" + JobFile}, nil
	}
	return []string{"sh", JobFile}, nil
}
