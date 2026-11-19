package cli

// potluck agent-task: run a coding agent CLI once, non-interactively, in a
// fresh sandbox on this checkout's commit (agenttasks), and bring the
// commits it made back as a local ref. It is potluck run's box job (place,
// push, spawn, follow, release) with the agent's credential in the box's
// secrets directory and one more step before the release: the box's bundle of the
// agent's commits is copied back (boxcp) and fetched into
// refs/potluck/agent-task/<sandbox>, never checked out or run.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/sengirunners/potluck/agentrunners"
	"github.com/sengirunners/potluck/agenttasks"
	"github.com/sengirunners/potluck/boxcp"
	"github.com/sengirunners/potluck/boxpush"
	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/taskproxy"
)

// agentTaskPromptMax bounds a prompt; agentTaskLast is how many of the
// agent's last stdout lines --json carries.
const (
	agentTaskPromptMax = 1 << 20
	agentTaskLast      = 20
)

// AgentTaskRefs is where a task's commits land in the checkout: one ref per
// sandbox under it.
const AgentTaskRefs = "refs/potluck/agent-task/"

type agentTaskOptions struct {
	jobOptions
	adapter                   agenttasks.Adapter
	secretName                string
	secret                    string // the credential's bytes; sent only through the box secrets door
	model, effort, stdoutFile string
	prompt                    []byte
	proxy                     taskproxy.Options
}

// agentTaskResult is --json's answer: run's, with the agent, the ref its
// commits were fetched into ("" when it made none or they did not come
// back), how many there are, and the agent's last lines of stdout.
type agentTaskResult struct {
	jobResult
	Agent   string   `json:"agent"`
	Ref     string   `json:"ref,omitempty"`
	Commits int      `json:"commits"`
	Last    []string `json:"last,omitempty"`
	// Proxy records requested cooperative settings, not enforced network policy.
	Proxy *taskproxy.Options `json:"cooperative_proxy,omitempty"`
}

func (a *app) agentTaskCmd() *cobra.Command {
	var (
		o                                  agentTaskOptions
		secretFile, secretName, promptFile string
	)
	def := agenttasks.Default()
	cmd := &cobra.Command{
		Use:   "agent-task [" + strings.Join(agenttasks.Names(), "|") + "] --prompt-file <file|-> [--secret-file <file>] [--on <machine-size>] [--database <recipe>] [--ref HEAD] [--dirty] [--timeout 1h] [--keep]",
		Short: "Run a coding agent once in a fresh sandbox on this checkout's commit; its commits come back as a local ref",
		Long: `Places a sandbox from the agent template (network on; with --database, the
project's database recipe image instead) as ` + a.cmd("run") + ` does,
sends the commit (default HEAD; with --dirty your uncommitted changes too),
and runs the agent there non-interactively with the prompt on its stdin.
When it ends, what it left uncommitted becomes one more commit, and the
commits it made come back as a git bundle, fetched into
` + AgentTaskRefs + `<sandbox> in this checkout. Nothing is checked out
and nothing from the bundle runs here: look at it with git log, then merge,
cherry-pick or push it yourself. Exits with the agent's exit code; the
sandbox ends at the end, also on Ctrl-C, an error or the timeout (exit
124), unless --keep.

The credential is yours: --secret-file is read here (else $` + strings.Join(def.Secrets, ", else $") + `)
and sent through the sandbox secrets door, only to trusted machines; the sandbox
passes it to the agent through a file descriptor, never its environment.
A sandbox-scope secret grant is required. With no credential, nothing is placed: get one with
` + "`" + def.Login + "`" + ` on your machine. The machine that runs the sandbox
can read everything in it, the credential included (docs/agent-tasks.md).

Agents: ` + strings.Join(agenttasks.Names(), ", ") + ` (default ` + def.Name + `). The image must ship the agent's command.`,
		Example: "  " + a.cmd("agent-task --prompt-file task.md --secret-file ~/.config/claude-token") + "\n" +
			"  echo 'Fix the failing test in ./placement and commit.' | " + a.cmd("agent-task claude --prompt-file - --on mini-c2m8@mini") + "\n" +
			"  " + a.cmd("agent-task --json --prompt-file task.md --dirty --timeout 2h"),
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			o.started = time.Now()
			o.adapter = def
			if len(args) == 1 {
				ad, ok := agenttasks.Lookup(args[0])
				if !ok {
					return fmt.Errorf("no agent %q (agents: %s)", args[0], strings.Join(agenttasks.Names(), ", "))
				}
				o.adapter = ad
			}
			var err error
			if err = o.checkProxy(cmd); err != nil {
				return err
			}
			if _, err = o.adapter.Command(o.model, o.effort); err != nil {
				return err
			}
			if o.prompt, err = readPrompt(promptFile, a.host.Stdin); err != nil {
				return err
			}
			if o.secretName, o.secret, err = taskCredential(o.adapter, secretFile, secretName, envOf); err != nil {
				return err
			}
			c, err := a.inPool(cmd.Context())
			if err != nil {
				return err
			}
			o.want.Net, o.want.BoxSecrets = computeapi.NetOn, true
			if o.image == "" && o.database == "" {
				o.want.Template = o.adapter.Template
			}
			o.out, o.errw, o.p = a.host.Stdout, a.host.Stderr, prefix(a.host.Prefix)
			if a.asJSON() {
				o.out = a.host.Stderr // stdout carries only the result
			}
			if err := o.requestDeps(cmd); err != nil {
				return err
			}
			r, err := agentTask(cmd.Context(), c, o)
			code := r.code()
			if err != nil && code == 0 {
				code = 1
			}
			if a.asJSON() {
				if err != nil && r.Error == "" {
					r.Error = err.Error()
				}
				if perr := a.printJSON(r); perr != nil {
					return perr
				}
				if code == 0 {
					return nil
				}
				return exitSilently(code)
			}
			switch {
			case r.Ref != "":
				fmt.Fprintf(a.host.Stderr, "%s: the agent's %d commit(s) are at %s (git log %s..%s; nothing was checked out)\n",
					a.name(), r.Commits, r.Ref, shortSHA(r.Commit), r.Ref)
			case err == nil && r.State == "exited" && code != agenttasks.ExitNotStarted:
				fmt.Fprintf(a.host.Stderr, "%s: the agent made no changes\n", a.name())
			}
			if err != nil {
				return exitWith{code: code, err: err}
			}
			if code != 0 {
				return exitSilently(code)
			}
			return nil
		}}
	f := cmd.Flags()
	f.StringVar(&promptFile, "prompt-file", "", "the prompt, a file of yours (- for stdin); it reaches the agent on its stdin")
	f.StringVar(&secretFile, "secret-file", "", "the agent's credential, a file of yours read here (default: from the environment); never a value on the command line")
	f.StringVar(&secretName, "secret-name", "", "what the credential is, one of "+strings.Join(def.Secrets, ", ")+" (default: the first; with no --secret-file, the only variable read)")
	f.StringVar(&o.want.On, "on", "", "a shared machine size, as "+a.cmd("status")+" lists it (default: the smallest that fits)")
	f.StringVar(&o.want.Size, "size", "", "a size the machine states")
	f.StringVar(&o.image, "image", "", "one of the machine's images, by name, that ships the agent's command (default: the agent's template's, "+def.Name+": "+def.Template+"; with --database, the recipe's image)")
	f.StringVar(&o.ref, "ref", "", "the commit the agent starts from, in this checkout (default HEAD)")
	f.DurationVar(&o.timeout, "timeout", time.Hour, "kill the agent after this long (exit 124)")
	f.BoolVar(&o.keep, "keep", false, "leave the sandbox up afterwards, for "+a.cmd("sandbox exec")+" (it ends at its lease)")
	f.StringVar(&o.model, "model", "", "the agent's model (default: the agent's own)")
	f.StringVar(&o.effort, "effort", "", "the agent's reasoning effort (default: the agent's own)")
	f.StringVar(&o.stdoutFile, "stdout-file", "", "write the full stdout locally to a new file with mode 0600")
	o.addProxyFlags(cmd)
	o.df.Add(f)
	o.push.add(f)
	databaseFlags(cmd, &o.database)
	return cmd
}

// readPrompt reads the prompt from path ("-": stdin): at most
// agentTaskPromptMax bytes, not blank.
func readPrompt(path string, stdin io.Reader) ([]byte, error) {
	var r io.Reader
	switch path {
	case "":
		return nil, errors.New("--prompt-file is required: a file holding the prompt, or - to read it from stdin")
	case "-":
		r = stdin
	default:
		f, err := os.Open(path)
		if err != nil {
			return nil, fmt.Errorf("--prompt-file: %v", err)
		}
		defer f.Close()
		r = f
	}
	b, err := io.ReadAll(io.LimitReader(r, agentTaskPromptMax+1))
	switch {
	case err != nil:
		return nil, fmt.Errorf("--prompt-file: %v", err)
	case len(b) > agentTaskPromptMax:
		return nil, fmt.Errorf("--prompt-file: over %d KiB", agentTaskPromptMax>>10)
	case len(bytes.TrimSpace(b)) == 0:
		return nil, errors.New("--prompt-file: the prompt is empty")
	}
	return b, nil
}

// taskCredential is the credential a task sends, by name: file's bytes
// exactly when given (named name, default the adapter's first), else the
// first of the adapter's names (or name alone) set in the environment.
// Errors never show a value.
func taskCredential(ad agenttasks.Adapter, file, name string, getenv func(string) string) (string, string, error) {
	if name != "" && !ad.Accepts(name) {
		return "", "", fmt.Errorf("--secret-name %s: %s takes one of %s", name, ad.Name, strings.Join(ad.Secrets, ", "))
	}
	if file != "" {
		if name == "" {
			name = ad.Secrets[0]
		}
		b, err := readBoxSecretFile(file)
		if err != nil {
			return "", "", fmt.Errorf("--secret-file: %v", err)
		}
		if len(bytes.TrimSpace(b)) == 0 {
			return "", "", errors.New("--secret-file: the file is empty")
		}
		if err := ad.CheckSecret(name, string(b)); err != nil {
			return "", "", fmt.Errorf("--secret-file: %v", err)
		}
		return name, string(b), nil
	}
	names := ad.Secrets
	if name != "" {
		names = []string{name}
	}
	for _, n := range names {
		if v := getenv(n); strings.TrimSpace(v) != "" {
			if err := ad.CheckSecret(n, v); err != nil {
				return "", "", fmt.Errorf("$%s: %v", n, err)
			}
			return n, v, nil
		}
	}
	return "", "", fmt.Errorf("no credential for %s: give --secret-file <file> holding one (`%s` on your machine prints a token; sent as %s), or set $%s; nothing was placed",
		ad.Name, ad.Login, ad.Secrets[0], strings.Join(names, " or $"))
}

// agentTaskDir is a task's directory in the box, under its HOME.
func agentTaskDir(key string) string { return ".potluck/agent-task/" + key }

// agentTask runs one task. The result is filled as far as it got; err is
// set when potluck itself failed, or the agent's commits did not come back.
func agentTask(ctx context.Context, c *client.Client, o agentTaskOptions) (res agentTaskResult, err error) {
	o.timing = newJobTiming(o.started)
	res.Agent = o.adapter.Name
	defer func() { o.finish(&res.jobResult, err) }()
	r := jobResult{State: "failed"}
	argv, err := o.adapter.CredentialCommand(o.secretName, o.model, o.effort)
	if err != nil {
		res.jobResult = r
		return res, err
	}
	b, err := o.source(ctx)
	if err != nil {
		res.jobResult, err = r.stopped(ctx, err)
		return res, err
	}
	capture, err := openTaskOutput(o.stdoutFile)
	if err != nil {
		res.jobResult = r
		return res, err
	}
	if capture != nil {
		defer func() { err = errors.Join(err, capture.close()) }()
		o.out = io.MultiWriter(capture, o.out)
		o.stdoutDropped = func(n int64) { capture.dropped += n }
	}
	r.Commit = b.Head
	tail := &tailWriter{max: 16 << 10}
	o.out = io.MultiWriter(o.out, tail)
	o.finishGrace = time.Minute
	o.drain = o.finishGrace

	o.collect = func(ctx context.Context, bx acquiredBox, r jobResult, ferr error) (jobResult, bool, error) {
		if *r.Exit == agenttasks.ExitNotStarted {
			return r, false, ferr
		}
		// Cancellation stops the agent, not the bounded attempt to save its work.
		collectCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		collectCtx, stop := o.untilAgain(collectCtx)
		defer stop()
		var err error
		var changes taskChanges
		if changes, err = bringBack(collectCtx, c, bx, b); err != nil {
			// The agent's work is still in the box: keep it, to copy by hand.
			sid := sandboxID(bx.handle)
			return r, true, fmt.Errorf("%w; the sandbox is kept so its work is not lost: %s copies it here, %s ends it (it ends at its lease; %s gives it longer)", err,
				o.p.cmd("sandbox cp "+sid+" :"+agentTaskDir(bx.key)+"/"+agenttasks.OutDir+" ./agent-task-out"), o.p.cmd("sandbox rm "+sid), o.p.cmd("sandbox extend "+sid+" --ttl 1h"))
		}
		res.Ref, res.Commits = changes.ref, changes.commits
		if r.State == "interrupted" {
			return r, false, ferr
		}
		r.State = changes.state
		if r.State == "timeout" {
			return r, false, fmt.Errorf("the agent ran past --timeout %v (exit 124); its work was collected", o.timeout)
		}
		return r, false, nil
	}
	res.jobResult, err = boxJob(ctx, c, o.boxJobOptions, r, func(ctx context.Context, bx acquiredBox) (string, error) {
		pushed, err := o.pushInto(ctx, c, bx, b)
		if err != nil {
			return "", err
		}
		box := daemonBox{c, bx.target, bx.id}
		missing, err := agentrunners.Missing(ctx, box, o.adapter.Binary, "timeout")
		if err != nil {
			return "", err
		}
		if len(missing) > 0 {
			return "", fmt.Errorf("the sandbox has no %s on PATH: %s needs an image that ships it (the agent template's does)", strings.Join(missing, ", "), o.adapter.About)
		}
		dir := agentTaskDir(bx.key)
		taskArgv, err := o.prepareProxy(ctx, box, dir, argv)
		if err != nil {
			return "", err
		}
		if o.proxy.Profile != "" {
			res.Proxy = &o.proxy
		}
		if _, err := boxcp.WriteFile(ctx, boxpush.Exec(c, bx.target, bx.id), dir+"/prompt", o.prompt, boxcp.Options{}); err != nil {
			return "", fmt.Errorf("write the prompt into the sandbox: %w", err)
		}
		fmt.Fprintf(o.errw, "%s: %s: starting %s\n", o.p.name(), sandboxID(bx.handle), o.adapter.About)
		spawnArgv := append([]string{"sh", "-c", agenttasks.Entry, "potluck-agent-task", dir, pushed.Dir, b.Head, o.secretName, strconv.FormatFloat(o.timeout.Seconds(), 'f', -1, 64)}, taskArgv...)
		return spawnAgentTask(ctx, box, spawnArgv, o.secretName, o.secret, int((o.timeout+o.finishGrace+time.Second-1)/time.Second), bx.key)
	})
	res.Last = tail.lines(agentTaskLast)
	return res, err
}

// tailWriter keeps the last max bytes written to it.
type tailWriter struct {
	max int
	buf []byte
}

func (t *tailWriter) Write(p []byte) (int, error) {
	t.buf = append(t.buf, p...)
	if over := len(t.buf) - t.max; over > 0 {
		t.buf = t.buf[over:]
	}
	return len(p), nil
}

// lines are its last n lines that are not blank.
func (t *tailWriter) lines(n int) []string {
	var out []string
	for _, l := range strings.Split(string(t.buf), "\n") {
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	if len(out) > n {
		out = out[len(out)-n:]
	}
	return out
}
