package cli

// potluck agent-runner: keep a coding agent vendor's self-hosted runners
// going in fresh sandboxes (agentrunners). Each job is one box running one
// runner, which serves one session and exits; the box then ends and, unless
// --once, a fresh one takes its place. The vendor key is the renter's own:
// read here from --secret-file and sent as the runner's spawn secret, never
// kept by a daemon or written into pool config.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/sengirunners/potluck/agentrunners"
	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/computeclient"
	"github.com/sengirunners/potluck/kit/client"
)

// A worker whose job ends before agentRunnerQuick waits before the next,
// agentRunnerBackoff doubling up to agentRunnerBackoffMax; agentRunnerGiveUp
// jobs in a row that failed quickly (no runner started, or it exited
// non-zero before agentRunnerQuick: a revoked key, a claude without the
// runner) end it. A runner sent TERM has agentRunnerDrain to hand its
// session back before KILL.
var (
	agentRunnerQuick      = 2 * time.Minute
	agentRunnerBackoff    = 15 * time.Second
	agentRunnerBackoffMax = 5 * time.Minute
	agentRunnerGiveUp     = 5
	agentRunnerDrain      = 90 * time.Second
)

type agentRunnerOptions struct {
	boxJobOptions
	adapter agentrunners.Adapter
	opts    agentrunners.Options
	secret  string
	count   int
	once    bool
}

func (a *app) agentRunnerCmd() *cobra.Command {
	var (
		o          agentRunnerOptions
		secretFile string
	)
	cmd := &cobra.Command{
		Use:   "agent-runner <adapter> --secret-file <file> [--count 1] [--once] [--timeout 1h] [--allow-tool T]...",
		Short: "Keep a coding agent's self-hosted runners going, one fresh sandbox per job (adapters: " + strings.Join(agentrunners.Names(), ", ") + ")",
		Long: `Starts a vendor's self-hosted runner in a fresh sandbox and keeps --count of
them going. A runner takes one session from the vendor and exits when it
ends; its sandbox then ends and a fresh one takes its place (--once: no
replacement). Nothing calls in: the runner reaches the vendor over HTTPS.

The vendor key is yours: --secret-file is read here and sent to the runner
as its command's secret (a file in the sandbox's memory, gone with the
command), through the pool's secret grants and only to trusted machines.
The machine that runs the sandbox can read everything in it, the key
included; see docs/agent-runners.md.

Adapters:
` + adapterList() + `
The sandbox's image must ship the adapter's command (claude: Claude Code).`,
		Example: "  " + a.cmd("agent-runner claude --secret-file ~/.config/claude/env-secret") + "\n" +
			"  " + a.cmd("agent-runner claude --secret-file key --count 3 --timeout 4h --allow-tool 'Bash(go test:*)'"),
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ad, ok := agentrunners.Lookup(args[0])
			if !ok {
				return fmt.Errorf("no agent runner adapter %q (adapters: %s)", args[0], strings.Join(agentrunners.Names(), ", "))
			}
			o.adapter = ad
			if secretFile == "" {
				return fmt.Errorf("--secret-file is required: the %s key, in a file of yours", ad.Secret)
			}
			b, err := readBoxSecretFile(secretFile)
			if err != nil {
				return fmt.Errorf("--secret-file: %v", err)
			}
			if len(bytes.TrimSpace(b)) == 0 {
				return fmt.Errorf("--secret-file: the file is empty")
			}
			o.secret = string(b)
			if err := ad.CheckSecret(o.secret); err != nil {
				return fmt.Errorf("--secret-file: %v", err)
			}
			switch {
			case o.count < 1 || o.count > 32:
				return fmt.Errorf("--count %d: 1 to 32", o.count)
			case o.timeout < 5*time.Minute:
				return fmt.Errorf("--timeout %v: at least 5m", o.timeout)
			}
			if err := o.opts.Check(); err != nil {
				return err
			}
			if _, err := ad.Files(o.opts); err != nil { // the adapter's own refusals, before any box is taken
				return err
			}
			c, err := a.inPool(cmd.Context())
			if err != nil {
				return err
			}
			o.want.Net, o.want.Secrets = computeapi.NetOn, true
			o.out, o.errw, o.p, o.drain = a.host.Stdout, a.host.Stderr, prefix(a.host.Prefix), agentRunnerDrain
			return agentRunners(cmd.Context(), c, o)
		}}
	f := cmd.Flags()
	f.StringVar(&secretFile, "secret-file", "", "the vendor key (Claude: the environment secret), a file of yours read here; never a value on the command line")
	f.IntVar(&o.count, "count", 1, "how many runners to keep going at once (1 to 32)")
	f.BoolVar(&o.once, "once", false, "run each runner once, with no fresh one after it")
	f.DurationVar(&o.timeout, "timeout", time.Hour, "a job's time limit; the runner stops taking work 1/4 of it (at most 10m) before, and is killed at it")
	f.StringVar(&o.opts.Mode, "permission-mode", "auto", "the mode sessions start in, as the vendor names it (claude: auto, acceptEdits, plan, default; \"\" pins none); read docs/agent-runners.md before auto")
	f.DurationVar(&o.opts.Idle, "idle", 0, "end a runner no session reached in this long (0: never)")
	f.StringArrayVar(&o.opts.Allow, "allow-tool", nil, "a tool sessions may use without asking, as the vendor names it (repeatable)")
	f.StringArrayVar(&o.opts.Deny, "deny-tool", nil, "a tool sessions may never use (repeatable)")
	f.StringVar(&o.want.On, "on", "", a.onUsage())
	f.StringVar(&o.want.Size, "size", "", "a size the machine states")
	f.StringVar(&o.want.OS, "os", "", "linux or macos")
	f.StringVar(&o.image, "image", "", "one of the machine's images, by name")
	return cmd
}

func adapterList() string {
	var b strings.Builder
	for _, n := range agentrunners.Names() {
		ad, _ := agentrunners.Lookup(n)
		fmt.Fprintf(&b, "  %-8s %s; key file name %s\n", n, ad.About, ad.Secret)
	}
	return b.String()
}

// agentRunners keeps o.count workers each running one job after another
// until Ctrl-C (or, with --once, one job each). Its error is the last
// worker's that gave up, or the one job's with --once and --count 1.
func agentRunners(ctx context.Context, c *client.Client, o agentRunnerOptions) error {
	var (
		mu      sync.Mutex // the shared writers and errs
		errs    []error
		wg      sync.WaitGroup
		results []jobResult
	)
	for i := 1; i <= o.count; i++ {
		wo := o
		if o.count > 1 {
			wo.out = &linePrefix{w: o.out, mu: &mu, prefix: fmt.Sprintf("[%d] ", i)}
			wo.errw = &linePrefix{w: o.errw, mu: &mu, prefix: fmt.Sprintf("[%d] ", i)}
		} else {
			wo.out = &linePrefix{w: o.out, mu: &mu}
			wo.errw = &linePrefix{w: o.errw, mu: &mu}
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := agentRunnerWorker(ctx, c, wo)
			wo.out.(*linePrefix).flush()
			wo.errw.(*linePrefix).flush()
			mu.Lock()
			defer mu.Unlock()
			results = append(results, r)
			if err != nil {
				errs = append(errs, err)
			}
		}()
	}
	wg.Wait()
	switch {
	case ctx.Err() != nil:
		return exitWith{code: 130, err: errors.New("interrupted: the runners were stopped and their sandboxes ended")}
	case o.once:
		code := 0 // the worst job's
		for _, r := range results {
			code = max(code, r.code())
		}
		if len(errs) > 0 {
			return exitWith{code: max(code, 1), err: errs[len(errs)-1]}
		}
		if code != 0 {
			return exitSilently(code)
		}
		return nil
	case len(errs) > 0:
		return exitWith{code: 1, err: errs[len(errs)-1]}
	}
	return nil
}

// agentRunnerWorker runs jobs one after another, backing off after quick
// ends, until Ctrl-C, --once, or agentRunnerGiveUp jobs in a row that
// failed quickly.
func agentRunnerWorker(ctx context.Context, c *client.Client, o agentRunnerOptions) (jobResult, error) {
	wait, quickFails := agentRunnerBackoff, 0
	for {
		start := time.Now()
		r, err := agentRunnerJob(ctx, c, o)
		switch {
		case ctx.Err() != nil:
			return r, nil
		case err != nil:
			fmt.Fprintf(o.errw, "%s: job %s: %s: %v\n", o.p.name(), cmpStr(r.Handle, "(no sandbox)"), r.State, err)
		default:
			fmt.Fprintf(o.errw, "%s: job %s: %s, exit %d, after %s\n", o.p.name(), sandboxID(r.Handle), r.State, r.code(), time.Since(start).Round(time.Second))
		}
		if o.once {
			return r, err
		}
		quick := time.Since(start) < agentRunnerQuick
		if r.PID == "" || quick && r.code() != 0 {
			if quickFails++; quickFails >= agentRunnerGiveUp {
				if err == nil {
					err = fmt.Errorf("the last runner exited %d", r.code())
				}
				return r, fmt.Errorf("%d jobs in a row failed within %s or started no runner; the last: %v", quickFails, agentRunnerQuick, err)
			}
		} else {
			quickFails = 0
		}
		if !quick {
			wait = agentRunnerBackoff
			continue
		}
		fmt.Fprintf(o.errw, "%s: the job ended within %s; the next in %s\n", o.p.name(), agentRunnerQuick, wait)
		select {
		case <-ctx.Done():
			return r, nil
		case <-time.After(wait):
		}
		wait = min(wait*2, agentRunnerBackoffMax)
	}
}

// agentRunnerJob is one job: a fresh box, the runner started in it with the
// key as its spawn's secret, followed until it exits, then the box ended.
func agentRunnerJob(ctx context.Context, c *client.Client, o agentRunnerOptions) (jobResult, error) {
	return boxJob(ctx, c, o.boxJobOptions, jobResult{State: "failed"}, func(ctx context.Context, bx acquiredBox) (string, error) {
		fmt.Fprintf(o.errw, "%s: %s: starting the %s runner\n", o.p.name(), sandboxID(bx.handle), o.adapter.Name)
		opts := o.opts
		opts.RetireAt = time.Now().Add(o.timeout - min(10*time.Minute, o.timeout/4))
		return agentrunners.Start(ctx, daemonBox{c, bx.target, bx.id}, agentrunners.Job{Adapter: o.adapter, Options: opts,
			Secret: o.secret, Key: bx.key, TimeoutS: int((o.timeout + time.Second - 1) / time.Second)})
	})
}

// daemonBox is agentrunners.Box over this machine's daemon.
type daemonBox struct {
	c          *client.Client
	target, id string
}

func (b daemonBox) Exec(ctx context.Context, argv []string, stdin string) (int, string, string, error) {
	r, err := computeclient.ExecRunTarget(ctx, b.c, b.target, b.id, computeapi.ExecRunTargetRequest{Argv: argv, Stdin: stdin, TimeoutS: 60})
	return r.Exit, r.Stdout, r.Stderr, err
}

func (b daemonBox) Spawn(ctx context.Context, argv []string, secrets map[string]string, timeoutS int, key string) (string, error) {
	r, err := computeclient.SpawnRunTarget(ctx, b.c, b.target, b.id, computeapi.SpawnRunTargetRequest{Argv: argv, Secrets: secrets, TimeoutS: timeoutS, RequestID: key})
	return r.PID, err
}

// linePrefix writes whole lines, each starting with prefix, under a lock
// shared by every worker's writers, so lines of jobs side by side never mix.
type linePrefix struct {
	w      io.Writer
	mu     *sync.Mutex
	prefix string
	buf    []byte
}

func (l *linePrefix) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.buf = append(l.buf, p...)
	for {
		i := bytes.IndexByte(l.buf, '\n')
		if i < 0 {
			break
		}
		_, _ = io.WriteString(l.w, l.prefix)
		_, _ = l.w.Write(l.buf[:i+1])
		l.buf = l.buf[i+1:]
	}
	return len(p), nil
}

// flush writes a last line that has no newline.
func (l *linePrefix) flush() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.buf) > 0 {
		_, _ = io.WriteString(l.w, l.prefix)
		_, _ = l.w.Write(append(l.buf, '\n'))
		l.buf = nil
	}
}

func cmpStr(s, alt string) string {
	if s == "" {
		return alt
	}
	return sandboxID(s)
}
