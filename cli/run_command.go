package cli

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/sengirunners/potluck/boxpush"
	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/computeclient"
	"github.com/sengirunners/potluck/depscli"
	"github.com/sengirunners/potluck/kit/client"
)

// JobFile is the script run runs when no command is given, at the shipped
// commit: it lives with the code, so a commit always means one job.
// OldJobFile is its old name, run when a commit has no JobFile.
const (
	JobFile    = ".potluck/job"
	OldJobFile = ".zm/job"
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

func (a *app) runCmd() *cobra.Command {
	var o jobOptions
	cmd := &cobra.Command{Use: "run [--on <machine-size> | --size S --os O] [--database <recipe>] [--ref HEAD] [--dirty] [--timeout 30m] [--keep] [-- <argv...>]",
		Short: "Run a command (default: the commit's .potluck/job) at this checkout's commit in a fresh sandbox; exits with its code",
		Long: `Places a sandbox as ` + a.cmd("sandbox create") + ` does, sends the commit (default
HEAD; with --dirty your uncommitted changes too) as a git bundle, starts the
command in the checkout, streams its output as it comes and exits with its
exit code. The sandbox ends at the end, also on Ctrl-C, an error or the
timeout (exit 124), unless --keep. A command that ends with no exit code
(its sandbox went) fails. No command: the commit's .potluck/job.

--deps npm[:path] runs it beside a dependency layer of yours (node_modules
built once from this directory's package-lock.json and package.json files,
then reused; ` + a.cmd("sandbox create --help") + ` says more), mounted copy-on-write at
the checkout plus this directory's place in its repository, where the push
puts the code; the command (the commit's .potluck/job too) runs there. On a
machine of another member's, name a --template, not an --image.`,
		Example: "  " + a.cmd("run -- make test") + "\n" +
			"  " + a.cmd("run --deps npm --template agent -- npm test") + "\n" +
			"  " + a.cmd("run --os macos --timeout 45m --dirty") + "\n" +
			"  " + a.cmd("run --json -- go test ./... 2>log"),
		RunE: func(cmd *cobra.Command, args []string) error {
			o.started = time.Now()
			c, err := a.inPool(cmd.Context())
			if err != nil {
				return err
			}
			o.argv = args
			o.out, o.errw, o.p = a.host.Stdout, a.host.Stderr, prefix(a.host.Prefix)
			if o.want.Net, err = netFlag(o.net); err != nil {
				return err
			}
			if err := o.requestDeps(cmd); err != nil {
				return err
			}
			if a.asJSON() {
				o.out = a.host.Stderr // stdout carries only the result
			}
			r, err := jobRun(cmd.Context(), c, o)
			if a.asJSON() {
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
	f.StringVar(&o.want.On, "on", "", a.onUsage())
	f.StringVar(&o.want.Size, "size", "", "a size the machine states")
	f.StringVar(&o.want.OS, "os", "", "linux or macos")
	f.StringVar(&o.image, "image", "", "one of the machine's images, by name")
	f.StringVar(&o.want.Template, "template", "", "an image template: "+strings.Join(computeapi.BoxTemplates(), ", ")+" (default: the machine's first image)")
	f.StringVar(&o.ref, "ref", "", "the commit to run, in this checkout (default HEAD)")
	f.DurationVar(&o.timeout, "timeout", 30*time.Minute, "kill the command after this long (exit 124)")
	f.BoolVar(&o.keep, "keep", false, "leave the sandbox up afterwards, for "+a.cmd("sandbox exec")+" (it ends at its lease)")
	f.StringVar(&o.net, "net", "on", netUsage)
	o.push.add(f)
	o.df.Add(f)
	databaseFlags(cmd, &o.database)
	return cmd
}

// jobRun runs one command. The result is filled as far as it got; err is
// set when potluck itself failed (state failed, or the sandbox went: lost).
func jobRun(ctx context.Context, c *client.Client, o jobOptions) (r jobResult, err error) {
	o.timing = newJobTiming(o.started)
	defer func() { o.finish(&r, err) }()
	r.State = "failed"
	src, err := o.source(ctx)
	if err != nil {
		return r.stopped(ctx, err)
	}
	r.Commit = src.Head
	argv := o.argv
	if len(argv) == 0 {
		if argv, err = jobFileArgv(ctx, src, o.under, o.p); err != nil {
			return r.stopped(ctx, err)
		}
	}
	return boxJob(ctx, c, o.boxJobOptions, r, func(ctx context.Context, bx acquiredBox) (string, error) {
		pushed, err := o.pushInto(ctx, c, bx, src)
		if err != nil {
			return "", err
		}
		// The checkout's path as the sandbox's shell has it ($HOME/<dir>),
		// and the project's directory in it when a layer is mounted there.
		cwd := pushed.Dir
		if o.under != "" {
			cwd = strings.TrimSuffix(cwd, "/") + "/" + o.under
		}
		sp, err := computeclient.SpawnRunTarget(ctx, c, bx.target, bx.id, computeapi.SpawnRunTargetRequest{Argv: append([]string{"sh", "-c", jobCd, "potluck-run", cwd}, argv...),
			TimeoutS: int((o.timeout + time.Second - 1) / time.Second), RequestID: bx.key})
		if err != nil {
			return "", err
		}
		return sp.PID, nil
	})
}

// source checks the timeout and resolves o's commit, before any box is
// asked for; it is bundled once the box has said what it holds.
func (o *jobOptions) source(ctx context.Context) (*boxpush.Source, error) {
	if o.timeout < time.Second {
		return nil, fmt.Errorf("--timeout must be at least 1s")
	}
	if o.push.maxMiB <= 0 {
		o.push.maxMiB = boxpush.DefaultMax >> 20
	}
	o.push.repo = o.repo
	src, err := o.push.source(ctx, o.ref, o.errw, o.p.name())
	if err == nil {
		o.base = o.push.hostBase(ctx, src)
	}
	return src, err
}

// pushInto names the box src goes to and pushes it there (only what the box
// lacks), answering where it was checked out.
func (o jobOptions) pushInto(ctx context.Context, c *client.Client, bx acquiredBox, src *boxpush.Source) (boxpush.Result, error) {
	o.timing.enter("push")
	defer o.timing.enter("start")
	fmt.Fprintf(o.errw, "%s: %s at %s\n", o.p.name(), sandboxID(bx.handle), shortSHA(src.Head))
	return boxpush.PushSource(ctx, boxpush.Exec(c, bx.target, bx.id), src, boxpush.PushOptions{Dest: o.push.dir, NoBase: o.push.noBase || o.push.full}, nil)
}

// jobCd runs "$@" in the checkout ($1, absolute or under HOME).
const jobCd = `case "$1" in /*) d="$1" ;; *) d="$HOME/$1" ;; esac
cd "$d" || exit 127
shift
exec "$@"`

// jobFileArgv is the argv of the commit's JobFile (else its OldJobFile):
// run directly when committed executable (its shebang decides), else with
// sh; from under (the command's directory below the checkout's top), by a
// path up to the top.
func jobFileArgv(ctx context.Context, b *boxpush.Source, under string, p prefix) ([]string, error) {
	for _, file := range []string{JobFile, OldJobFile} {
		out, err := exec.CommandContext(ctx, "git", "-C", b.Repo, "ls-tree", b.Head, "--", file).Output()
		mode, _, _ := strings.Cut(string(out), " ")
		if err != nil || mode == "" {
			continue
		}
		path := "./" + file
		if under != "" {
			path = strings.Repeat("../", strings.Count(under, "/")+1) + file
		}
		if mode == "100755" {
			return []string{path}, nil
		}
		return []string{"sh", path}, nil
	}
	return nil, fmt.Errorf("no command given and no %s at %s: pass one after -- (%s) or commit a %s script", JobFile, shortSHA(b.Head), p.cmd("run -- make test"), JobFile)
}
