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
	"os/signal"
	"slices"
	"strings"
	"time"

	"github.com/sengirunners/potluck/boxpush"
	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/computeclient"
	"github.com/sengirunners/potluck/depscli"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/ids"
	"github.com/sengirunners/potluck/placement"
	"github.com/sengirunners/potluck/pushbase"
)

// Timing: the lease planned covers the push and the command with room to
// spare (run_lease.go asks it short and renews it); a command the driver
// did not end by its timeout is killed jobKillGrace later; output calls
// failing in a row for jobLostAfter mean the sandbox is gone.
var (
	jobLeaseSlack = 10 * time.Minute
	jobKillGrace  = 90 * time.Second
	jobLostAfter  = 2 * time.Minute
)

// boxJobOptions is what boxJob needs: where the box goes, how long the
// command may run, and where its output goes.
type boxJobOptions struct {
	started  time.Time // command entry, before local prompt/dependency preparation
	timing   *jobTiming
	base     *pushbase.Plan
	want     computeclient.BoxWant
	database string
	image    string
	timeout  time.Duration
	// finishGrace reserves time after the command deadline for in-box cleanup.
	finishGrace time.Duration
	// drain is how long a command sent TERM on Ctrl-C has to end by itself,
	// its output still followed, before KILL (0: stop following at once).
	drain time.Duration
	keep  bool
	// deps are the dependency layers the box is acquired with (depscli).
	deps      []computeapi.BoxDeps
	depsWaitS *int
	// room is how long the acquire waits while every machine that fits has
	// no room now (--wait; roomwait); 0 fails at once.
	room time.Duration
	// under is the project's directory in the checkout ("" its top) when a
	// layer is mounted there by default: run's command runs there, beside it.
	under         string
	out           io.Writer   // the command's stdout
	errw          io.Writer   // its stderr and the program's own lines
	stdoutDropped func(int64) // capture consumers need to know when stdout is incomplete
	p             prefix      // how the program names Potluck's commands (Host.Prefix)
	// collect, when set, runs once the command ended with an exit code (any,
	// 124 included; err is what following it answered), before the box is
	// released: the result and error it answers are boxJob's, and keep
	// true keeps the box as --keep does.
	collect func(ctx context.Context, bx acquiredBox, r jobResult, err error) (_ jobResult, keep bool, _ error)
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

// acquiredBox is a box boxJob holds: its binding, id and handle, and the
// acquire's request id (a spawn in it may reuse it as its own).
type acquiredBox struct{ target, id, handle, key string }

// boxStart starts a job's command in a box just acquired and answers its pid.
type boxStart func(ctx context.Context, bx acquiredBox) (pid string, err error)

// boxJob places and acquires a box, starts the command in it, follows its
// output until it ends, and releases the box (unless o.keep), Ctrl-C
// included. r carries what the caller filled; it comes back filled as far
// as it got.
func boxJob(ctx context.Context, c *client.Client, o boxJobOptions, in jobResult, start boxStart) (r jobResult, err error) {
	if o.timing == nil {
		o.timing = newJobTiming(o.started)
	}
	defer func() { o.timing.result(&r, err) }()
	o.timing.deps = len(o.deps) > 0
	o.timing.enter("acquire")
	if o.base != nil {
		plan := *o.base
		observe := plan.ObserveFill
		plan.ObserveFill = func(d time.Duration) {
			o.timing.fill(d)
			if observe != nil {
				observe(d)
			}
		}
		o.base = &plan
	}
	r = in // named: the deferred end of the sandbox below sets Kept and Released on what is returned
	key := "job-" + strings.ToLower(ids.New())
	areq := computeapi.AcquireRunTargetRequest{RequestID: key, Database: o.database, Image: o.image, Template: o.want.Template, Net: o.want.Net,
		Deps: o.deps, DepsWaitS: o.depsWaitS}
	var prepErr error // a refusal before the acquire: nothing was asked of that binding
	// planned is the lease the job is planned for (the end of the sandbox's
	// life); it is asked for a short lease of runLeaseTTL that this client
	// renews up to planned (run_lease.go), when the binding's driver renews
	// and the sandbox is not kept. leased says so; lifeCap is the binding's
	// lifetime cap (0: none known), which counts from the acquire.
	var planned, lifeCap time.Duration
	var leased bool
	var acquiring time.Time // when the binding that answered was asked: its lifetime cap counts from there
	prep := func(ctx context.Context, target string, ar *computeapi.AcquireRunTargetRequest) error {
		prepErr, leased, lifeCap, acquiring = nil, false, 0, time.Now()
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
			lifeCap = time.Duration(lim) * time.Second
			if capd := lifeCap; lim > 0 && ttl > capd {
				if o.timeout+o.finishGrace+2*time.Minute > capd {
					// Nothing asked of it: the next binding may fit.
					return &computeclient.Misfit{Err: api.Errorf(api.CodeBadRequest, "--timeout %v does not fit %s's lifetime cap of %v (with two minutes to start it)", o.timeout, target, capd)}
				}
				ttl = capd
			}
			leased = !o.keep && ttl > runLeaseTTL && v.Describe != nil && slices.Contains(v.Describe.Verbs, "renew") // the protocol's verb
		}
		if err := depscli.ImageOK(ctx, c, target, ar.Image, ar.Deps); err != nil {
			prepErr = err
			return err
		}
		planned = ttl
		if leased {
			ttl = runLeaseTTL
		}
		ar.TTLS = int(ttl / time.Second)
		// This client renews the short lease itself: said so, the daemon's
		// sweep (which renews a handle whose holder session lives) does not
		// keep the sandbox up after this process dies.
		ar.SelfRenew = leased
		return nil
	}
	// areq comes back as the last ask sent it (ttl_s and self_renew as prep
	// set them): the interrupt below asks it again, the same.
	target, h, err := acquireRoom(ctx, roomWait(o.room, o.errw, o.p.name()), c, o.want, &areq, prep, o.base) // room.go
	r.Target = target
	if err != nil && (target == "" || err == prepErr || computeclient.MadeNothing(err)) { // nothing was made: nothing to end
		return r.stopped(ctx, err)
	}
	if err != nil && (ctx.Err() != nil || mayHaveMade(err)) {
		o.timing.enter("cleanup")
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
		actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Minute+depscli.Within(o.deps, o.depsWaitS))
		defer cancel()
		actx, stopWaiting := o.untilAgain(actx)
		defer stopWaiting()
		stopHint := "Ctrl-C to stop waiting"
		if interrupted {
			stopHint = "Ctrl-C again to stop waiting"
		}
		fmt.Fprintf(o.errw, "%s: ending the sandbox that may have been made (request id %s; %s: it then ends at its lease)\n", o.p.name(), key, stopHint)
		if h, err = computeclient.AcquireRunTarget(actx, c, target, areq); err != nil {
			if computeclient.MadeNothing(err) {
				// The daemon forgot the request: nothing was made, nothing to end.
				return r.stopped(ctx, fmt.Errorf("%s (asked again, it made nothing): %w", what, err))
			}
			return r, fmt.Errorf("%s; the sandbox may still come up (request id %s, it ends at its lease): %w", what, key, err)
		}
		r.Handle = h.Handle
		_, id, _ := computeclient.SplitBoxHandle(h.Handle)
		if err := releaseBox(actx, c, target, id, nil); err != nil {
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
	r.Handle, r.Deps = h.Handle, h.Deps
	depscli.Print(o.errw, o.p.name(), o.deps, h.Deps)
	sid := sandboxID(h.Handle)
	_, id, _ := computeclient.SplitBoxHandle(h.Handle)
	stopRenew := func() error { return nil }
	if leased {
		end := time.Now().Add(planned) // the sandbox's life counts from when it was made
		if capEnd := acquiring.Add(lifeCap); lifeCap > 0 && capEnd.Before(end) {
			end = capEnd // the cap counts from the acquire, whenever the sandbox came up
		}
		stopRenew = keepLease(ctx, c, target, id, end)
	}
	defer func() {
		o.timing.enter("cleanup")
		if rerr := stopRenew(); rerr != nil {
			fmt.Fprintf(o.errw, "%s: the last renewal of %s's lease failed (a lease runs %v from the last that got through): %v\n", o.p.name(), sid, runLeaseTTL, rerr)
		}
		if o.keep {
			r.Kept = true
			fmt.Fprintf(o.errw, "%s: kept %s (%s -- ...; %s ends it)\n", o.p.name(), sid, o.p.cmd("sandbox exec "+sid), o.p.cmd("sandbox rm "+sid))
			return
		}
		// Ended whatever happened, Ctrl-C included: on a context of its own;
		// once the pool did not answer, a Ctrl-C stops asking again.
		if rerr := releaseBox(context.WithoutCancel(ctx), c, target, id, func(ctx context.Context) (context.Context, context.CancelFunc) {
			fmt.Fprintf(o.errw, "%s: the pool did not answer the end of %s; asking again for up to %v (Ctrl-C to stop: it then ends at its lease)\n", o.p.name(), sid, runReleaseFor)
			return o.untilAgain(ctx)
		}); rerr != nil {
			fmt.Fprintf(o.errw, "%s: could not end %s (it ends at its lease): %v\n", o.p.name(), sid, rerr)
			return
		}
		r.Released = true
	}()
	o.timing.enter("start")
	pid, err := start(ctx, acquiredBox{target: target, id: id, handle: h.Handle, key: key})
	if err != nil {
		return r.stopped(ctx, err)
	}
	r.PID = pid
	o.timing.spawned()
	if r, err = follow(ctx, c, target, id, o, time.Now().Add(o.timeout+o.finishGrace+jobKillGrace), r); o.collect == nil || r.Exit == nil {
		return r, err
	}
	var keep bool
	o.timing.enter("collect")
	r, keep, err = o.collect(ctx, acquiredBox{target: target, id: id, handle: h.Handle, key: key}, r, err)
	o.keep = o.keep || keep // read by the deferred end of the sandbox above
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
			if o.drain > 0 {
				var ended bool
				r.Exit, ended = drained(c, target, id, o, req, time.Now().Add(o.drain))
				if !ended {
					signal("KILL")
				}
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
				fmt.Fprintf(o.errw, "%s: %d bytes of %s are no longer kept\n", o.p.name(), d.n, d.what)
			}
		}
		if p.DroppedStdout > 0 && o.stdoutDropped != nil {
			o.stdoutDropped(p.DroppedStdout)
		}
		_, _ = io.WriteString(o.out, p.Stdout)
		_, _ = io.WriteString(o.errw, p.Stderr)
		req.StdoutFrom, req.StderrFrom = p.StdoutNext, p.StderrNext
		switch {
		case p.Running || p.Stdout != "" || p.Stderr != "":
			// A terminal status describes the process, not whether its output
			// fits this page. Drain through an empty terminal page.
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

// drained follows a command sent TERM until it ends (its exit, true), deadline
// passes, or another Ctrl-C comes, on a context of its own: the caller's is
// cancelled. A transient failure to read its output is tried again.
func drained(c *client.Client, target, id string, o boxJobOptions, req computeapi.OutputRunTargetRequest, deadline time.Time) (*int, bool) {
	fmt.Fprintf(o.errw, "%s: sent TERM; waiting up to %s for it to end (Ctrl-C again to stop it now)\n", o.p.name(), time.Until(deadline).Round(time.Second))
	again, stop := o.untilAgain(context.Background())
	defer stop()
	for time.Now().Before(deadline) && again.Err() == nil {
		req.WaitS = max(min(20, int(time.Until(deadline)/time.Second)), 0)
		ctx, cancel := context.WithDeadline(again, deadline)
		p, err := computeclient.RunTargetOutput(ctx, c, target, id, req)
		cancel()
		if err != nil {
			if again.Err() != nil || !boxpush.Transient(err) {
				return nil, false
			}
			select {
			case <-again.Done():
			case <-time.After(time.Second):
			}
			continue
		}
		if p.DroppedStdout > 0 && o.stdoutDropped != nil {
			o.stdoutDropped(p.DroppedStdout)
		}
		_, _ = io.WriteString(o.out, p.Stdout)
		_, _ = io.WriteString(o.errw, p.Stderr)
		req.StdoutFrom, req.StderrFrom = p.StdoutNext, p.StderrNext
		if !p.Running && p.Stdout == "" && p.Stderr == "" {
			return p.Exit, true
		}
	}
	return nil, false
}
