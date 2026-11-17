package cli

// potluck sandbox and potluck usage (copied from zm's cmd/zm sandbox.go,
// target.go and sandbox_cp.go). A sandbox is a box handle on a shared
// machine; people see it as "<machine-size>@<machine>/<id>", the handle
// without its "box:" prefix, and either form is taken back.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/sengirunners/potluck/boxcp"
	"github.com/sengirunners/potluck/boxpush"
	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/computeclient"
	"github.com/sengirunners/potluck/depscli"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/ids"
)

// sandboxID is a handle as people see it.
func sandboxID(handle string) string { return strings.TrimPrefix(handle, "box:") }

// handleOf takes a sandbox id (or a handle) back to a handle.
func handleOf(id string) string {
	if strings.HasPrefix(id, "box:") {
		return id
	}
	return "box:" + id
}

// sandboxClient is a client in the pool and the sandbox's machine and id.
func (a *app) sandboxClient(ctx context.Context, id string) (c *client.Client, target, bid string, err error) {
	if c, err = a.inPool(ctx); err != nil {
		return nil, "", "", err
	}
	target, bid, err = computeclient.BoxHandle(ctx, c, handleOf(id))
	return c, target, bid, err
}

func (a *app) sandboxCmd() *cobra.Command {
	sb := &cobra.Command{Use: "sandbox", Short: "Throwaway machines you create on the pool, run commands in and remove",
		Long: `A sandbox is a fresh VM (or isolated container) on a machine shared with the
pool, kept up for its lease, which only you run commands in. It ends at rm,
at its deadline, or when its machine's owner takes the machine back. Each
command names it by the id sandbox create printed (sandbox ls lists them).`}
	sb.AddCommand(a.sandboxCreateCmd(), a.sandboxLsCmd(), a.sandboxExecCmd(), a.sandboxLogsCmd(), a.sandboxShellCmd(),
		a.sandboxCpCmd(), a.sandboxPushCmd(), a.sandboxExtendCmd(), a.sandboxRmCmd(), a.sandboxSignalCmd(), a.sandboxPreviewCmd(),
		a.sandboxStopCmd(), a.sandboxStartCmd())
	sb.AddCommand(a.sandboxOldSpellings()...)
	return sb
}

func (a *app) sandboxCreateCmd() *cobra.Command {
	var req computeapi.AcquireRunTargetRequest
	var want computeclient.BoxWant
	var ttl time.Duration
	var push pushFlags
	var netOpt string
	var vols []string
	var df depscli.Flags
	cmd := &cobra.Command{Use: "create [--size c2m4] [--os linux] [--on <machine-size>] [--ttl 1h] [--ref HEAD [--dirty]] [--template agent|browser] [--net off] [--persist] [--volume name:path[:rw|ro|cow]]... [--deps npm[:path]]...",
		Short: "Start a sandbox on the smallest shared machine size that fits (or --on); prints its id",
		Long: `With --ref (or --dirty) the sandbox gets that commit of this checkout, sent
as a git bundle, checked out in ~/checkout (--dir); no credential enters it.

--template agent boots the coding-agent image (git, gh, Node, Go, Python, the
Claude Code and Codex CLIs). --template browser adds sandboxed Chromium,
Playwright and PNG screenshots. The machine must share the selected image.

On a trusted machine a Linux sandbox reaches the internet through the
machine's filtered network (never the machine, its LAN, cloud metadata or
outbound mail); --net off makes it with no network at all. A public
machine's sandbox has none.

--persist keeps the sandbox's disk when it stops (` + a.cmd("sandbox stop") + `, or
its lease running out): ` + a.cmd("sandbox start") + ` boots the same disk on the
same machine, for you only, until the machine's keep runs out (30 days by
default; ` + a.cmd("sandbox ls") + ` shows when). ` + a.cmd("sandbox rm") + ` deletes it.
Trusted machines whose driver keeps disks only.

--volume name:path[:mode] mounts a named volume of yours at path (relative
to the sandbox's HOME) that outlives the sandbox: the next one that names it
on the same machine sees what this one left. rw (the default) is one
sandbox's at a time; ro and cow are shared by any number of yours. Trusted
machines only; repeat for more.

--deps npm[:path] mounts a dependency layer of yours at path (default: the
checkout, --dir, plus this directory's place in its repository): node_modules, built once from the package-lock.json and
package.json files (and each workspace's package.json) of this directory,
and its .npmrc's resolution keys (legacy-peer-deps and the like; an .npmrc
with any other key, such as a registry or a token, is refused),
installed by the machine in a sandbox of yours with the network and no
secrets, and reused by your later sandboxes on that machine. ` + a.name() + ` prints
whether it was a hit (already built) or built. The sandbox sees it
copy-on-write: what it writes never reaches the layer or another sandbox.
Trusted machines only. --deps-wait S waits at most S seconds while another
sandbox builds the same layer (default 600; 0 takes the sandbox at once
without it); --deps-scripts off builds without running dependencies'
install scripts. The files come from this directory, not from --ref.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := a.inPool(cmd.Context())
			if err != nil {
				return err
			}
			if ttl < 0 {
				return fmt.Errorf("--ttl must not be negative")
			}
			req.TTLS = int(ttl / time.Second)
			if req.Net, err = netFlag(netOpt); err != nil {
				return err
			}
			want.Net = req.Net
			if req.Volumes, err = volumeFlags(vols); err != nil {
				return err
			}
			if err := df.Request(cmd.Context(), cmd.Flags(), push.repo, push.dir, &req); err != nil {
				return err
			}
			want.Volumes = len(req.Volumes) > 0 || len(req.Deps) > 0
			if req.RequestID == "" {
				req.RequestID = "cli-" + strings.ToLower(ids.New())
			}
			h, res, err := a.sandboxCreate(cmd.Context(), c, want, req, push)
			switch {
			case err != nil:
				return err
			case a.asJSON() && res != nil:
				return a.printJSON(struct {
					computeapi.RunTargetHandle
					Push boxpush.Result `json:"push"`
				}{h, *res})
			case res != nil:
				return nil // printed before the push
			}
			return a.printHandle(h, false)
		}}
	f := cmd.Flags()
	f.StringVar(&want.On, "on", "", "a shared machine size, as "+a.cmd("status")+" lists it (default: the smallest that fits)")
	f.StringVar(&want.Size, "size", "", "a size one of the pool's machines shares, as "+a.cmd("status")+" lists them (c<vCPU>m<GiB>, e.g. c2m4)")
	f.StringVar(&want.OS, "os", "", "linux or macos")
	f.DurationVar(&ttl, "ttl", 0, "lease length (default: the machine's)")
	f.StringVar(&req.Ref, "ref", "", "a commit of this checkout to send in (HEAD)")
	f.StringVar(&req.Image, "image", "", "one of the machine's images, by name (default the first)")
	f.StringVar(&want.Template, "template", "", "an image template: "+strings.Join(computeapi.BoxTemplates(), ", ")+" (default: the machine's first image)")
	f.StringVar(&req.RequestID, "request-id", "", "idempotency key: the same key answers the same sandbox")
	f.StringVar(&netOpt, "net", "on", netUsage)
	f.BoolVar(&req.Persist, "persist", false, "keep the sandbox's disk when it stops, for "+a.cmd("sandbox start")+" (a trusted machine whose driver keeps disks)")
	f.StringArrayVar(&vols, "volume", nil, "name:path[:rw|ro|cow]: a named volume of yours that outlives the sandbox, mounted at path (repeatable)")
	push.add(f)
	df.Add(f)
	return cmd
}

// volumeFlags reads --volume name:path[:mode].
func volumeFlags(vs []string) ([]computeapi.BoxVolume, error) {
	var out []computeapi.BoxVolume
	for _, v := range vs {
		parts := strings.Split(v, ":")
		if len(parts) < 2 || len(parts) > 3 || parts[0] == "" || parts[1] == "" {
			return nil, fmt.Errorf("--volume %q: name:path[:rw|ro|cow]", v)
		}
		bv := computeapi.BoxVolume{Name: parts[0], Path: parts[1], Mode: computeapi.VolumeRW}
		if len(parts) == 3 {
			bv.Mode = parts[2]
		}
		switch bv.Mode {
		case computeapi.VolumeRW, computeapi.VolumeRO, computeapi.VolumeCOW:
		default:
			return nil, fmt.Errorf("--volume %q: the mode is rw, ro or cow", v)
		}
		out = append(out, bv)
	}
	return out, nil
}

const netUsage = "on: the machine's filtered network on a trusted machine (internet, never the machine or its LAN); off: no network at all"

// netFlag is --net as the request's net: on is the default, sent as nothing.
func netFlag(v string) (string, error) {
	switch v {
	case "", computeapi.NetOn:
		return "", nil
	case computeapi.NetOff:
		return computeapi.NetOff, nil
	}
	return "", fmt.Errorf("--net is on or off, not %q", v)
}

// sandboxCreate places and starts a sandbox; with a ref (or --dirty) on a
// machine that does not check out refs itself, it pushes this checkout's
// commit into it, printing the id first (not with --json).
func (a *app) sandboxCreate(ctx context.Context, c *client.Client, want computeclient.BoxWant, req computeapi.AcquireRunTargetRequest, push pushFlags) (computeapi.RunTargetHandle, *boxpush.Result, error) {
	var h computeapi.RunTargetHandle
	req.Template = want.Template
	var b *boxpush.Bundle
	defer func() {
		if b != nil {
			b.Close()
		}
	}()
	ref := req.Ref // the push below clears the ref of a binding that does not check it out
	if len(req.Deps) > 0 && (ref != "" || push.dirty) {
		depscli.WarnMismatch(ctx, a.host.Stderr, a.name(), push.repo, ref, push.dirty, push.untracked, req.Deps)
	}
	pushing := false  // the binding asked checks out no refs: this checkout's commit is pushed in
	var prepErr error // a refusal before the acquire: nothing was asked of that binding
	prep := func(ctx context.Context, target string, r *computeapi.AcquireRunTargetRequest) error {
		pushing, prepErr = false, nil
		if prepErr = depscli.ImageOK(ctx, c, target, r.Image, r.Deps); prepErr != nil {
			return prepErr
		}
		if ref == "" && !push.dirty {
			return nil
		}
		self, err := boxpush.ChecksOutItself(ctx, c, target)
		switch {
		case err != nil:
			prepErr = err
		case self:
			if prepErr = boxpush.SelfMisfit(target, push.dirty, len(r.Deps) > 0); prepErr != nil {
				return prepErr
			}
		case !self && b == nil: // made before the sandbox, so a refusal costs nothing
			b, prepErr = push.bundle(ctx, ref, a.host.Stderr, a.name())
		}
		if prepErr == nil && !self {
			r.Ref, pushing = "", true
		}
		return prepErr
	}
	target, h, err := computeclient.AcquireBox(ctx, c, want, req, prep)
	switch {
	case err != nil && (target == "" || err == prepErr): // placement, or before the acquire: nothing was asked
		return h, nil, err
	case err != nil && computeclient.MadeNothing(err): // nothing was made: a retry needs no key
		return h, nil, err
	case err != nil:
		return h, nil, retryWithKey(err, req.RequestID)
	}
	depscli.Print(a.host.Stderr, a.name(), req.Deps, h.Deps)
	if !pushing {
		return h, nil, nil
	}
	if !a.asJSON() {
		_ = a.printHandle(h, false)
	}
	_, id, _ := computeclient.SplitBoxHandle(h.Handle)
	res, err := push.push(ctx, c, target, id, b, a.host.Stderr, a.name())
	if err != nil {
		s := sandboxID(h.Handle)
		return h, nil, fmt.Errorf("%w (the sandbox is kept: %s to resume, %s to end it)", err, a.cmd("sandbox push "+s), a.cmd("sandbox rm "+s))
	}
	return h, &res, nil
}

// pushFlags are the flags of a push of this checkout's code into a sandbox.
type pushFlags struct {
	dirty, full bool
	untracked   bool
	dir         string
	maxMiB      int
	repo        string // the checkout ("" = the working directory)
}

func (p *pushFlags) add(f *pflag.FlagSet) {
	f.BoolVar(&p.dirty, "dirty", false, "also send uncommitted changes to tracked files (a second commit on HEAD; implies --ref HEAD)")
	f.BoolVar(&p.untracked, "include-untracked", false, "with --dirty, also untracked files git does not ignore (each listed)")
	f.BoolVar(&p.full, "full", false, "send the whole history, not one commit")
	f.StringVar(&p.dir, "dir", boxpush.DefaultDest, "the checkout in the sandbox (relative to its HOME)")
	f.IntVar(&p.maxMiB, "max-mib", boxpush.DefaultMax>>20, "refuse a bundle over this many MiB")
}

// bundle makes the bundle of ref (default HEAD), listing on w every
// untracked file it carries.
// It reports progress on w, its lines starting with the program's name.
func (p *pushFlags) bundle(ctx context.Context, ref string, w io.Writer, name string) (*boxpush.Bundle, error) {
	switch {
	case p.maxMiB <= 0:
		return nil, fmt.Errorf("--max-mib must be positive")
	case p.untracked && !p.dirty:
		return nil, fmt.Errorf("--include-untracked goes with --dirty")
	}
	b, err := boxpush.Make(ctx, boxpush.MakeOptions{Dir: p.repo, Ref: ref, Dirty: p.dirty, Untracked: p.untracked, Full: p.full, Max: int64(p.maxMiB) << 20})
	if err != nil {
		return nil, err
	}
	for _, f := range b.Untracked {
		fmt.Fprintf(w, "%s: sending untracked %s\n", name, f)
	}
	return b, nil
}

// push sends b into the sandbox, with progress on w for a big one (its
// lines start with the program's name).
func (p *pushFlags) push(ctx context.Context, c *client.Client, target, id string, b *boxpush.Bundle, w io.Writer, name string) (boxpush.Result, error) {
	o := boxpush.PushOptions{Dest: p.dir}
	if b.Size > 4*boxpush.ChunkSize {
		o.Progress = func(done, total int) {
			fmt.Fprintf(w, "\r%s: sending %s: %d/%d chunks", name, shortSHA(b.Head), done, total)
		}
	}
	res, err := boxpush.Push(ctx, boxpush.Exec(c, target, id), b, o)
	if o.Progress != nil {
		fmt.Fprintln(w)
	}
	if err == nil {
		fmt.Fprintf(w, "%s: checked out %s at %s in the sandbox (%.1f MiB sent; exec --cwd %s)\n", name, shortSHA(res.Commit), res.Dest, float64(b.Size)/(1<<20), res.Dest)
	}
	return res, err
}

func shortSHA(c string) string { return c[:min(len(c), 12)] }

func (a *app) sandboxPushCmd() *cobra.Command {
	var push pushFlags
	cmd := &cobra.Command{Use: "push <sandbox> [ref]", Short: "Check out a commit of this checkout in a sandbox (default HEAD), sent as a git bundle",
		Long: `Sends the commit (and with --dirty your uncommitted changes) from this
checkout into the sandbox as a git bundle in chunks, checked against its
SHA-256 there, and checks it out in ~/checkout (--dir). No credential enters
the sandbox. A push cut off resumes when run again.`,
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, t, id, err := a.sandboxClient(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			b, err := push.bundle(cmd.Context(), append(args[1:], "")[0], a.host.Stderr, a.name())
			if err != nil {
				return err
			}
			defer b.Close()
			res, err := push.push(cmd.Context(), c, t, id, b, a.host.Stderr, a.name())
			if err != nil {
				return err
			}
			if a.asJSON() {
				return a.printJSON(res)
			}
			return nil
		}}
	push.add(cmd.Flags())
	return cmd
}

func (a *app) sandboxLsCmd() *cobra.Command {
	var live bool
	cmd := &cobra.Command{Use: "ls [--live]", Short: "Sandboxes you can see in the pool, newest first",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := a.inPool(cmd.Context())
			if err != nil {
				return err
			}
			hs, skipped, err := computeclient.Sandboxes(cmd.Context(), c, live)
			if err != nil {
				return err
			}
			for _, s := range skipped {
				fmt.Fprintln(a.host.Stderr, a.name()+": skipped", config.Text(s))
			}
			if len(hs) == 0 && len(skipped) > 0 {
				return exitWith{code: 1, err: fmt.Errorf("no shared machine answered")}
			}
			if a.asJSON() {
				return a.printJSON(computeapi.RunTargetHandles{Handles: hs})
			}
			for _, h := range hs {
				if err := a.printHandle(h, true); err != nil { // the pool's: say whose
					return err
				}
			}
			return nil
		}}
	cmd.Flags().BoolVar(&live, "live", false, "only sandboxes not yet ended")
	return cmd
}

// printHandle prints a sandbox on one line: when it ended (else when it
// expires) and, with who, who made it.
func (a *app) printHandle(h computeapi.RunTargetHandle, who bool) error {
	if a.asJSON() {
		return a.printJSON(h)
	}
	exit := ""
	if h.Exit != nil {
		exit = fmt.Sprintf(" exit=%d", *h.Exit)
	}
	commit := ""
	if h.Commit != "" {
		commit = " commit=" + shortSHA(h.Commit)
	}
	net := ""
	if h.Net != "" {
		net = " net=" + h.Net
	}
	by := ""
	if who {
		by = " by=" + h.Actor
	}
	when := " expires=" + h.ExpiresAt
	if h.Ended != "" {
		when = " ended=" + h.Ended
	}
	switch {
	case h.State == computeapi.StateStopped: // a stopped sandbox's expires is its kept disk's
		net += " persist (disk kept until expires; " + a.cmd("sandbox start "+sandboxID(h.Handle)) + ")"
	case h.Persist && h.Ended == "":
		net += " persist"
	}
	a.say("%s %s%s%s%s%s%s", sandboxID(h.Handle), h.State, by, exit, commit, when, net)
	if h.URL != "" {
		a.say("%s", h.URL)
	}
	return nil
}

func (a *app) usageCmd() *cobra.Command {
	var since string
	cmd := &cobra.Command{Use: "usage [--since 2026-11-01T00:00:00Z]", Short: "Ended sandboxes' minutes in the pool, newest first",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := a.inPool(cmd.Context())
			if err != nil {
				return err
			}
			l, err := computeclient.Usage(cmd.Context(), c, since)
			if err != nil {
				return err
			}
			if a.asJSON() {
				return a.printJSON(l)
			}
			for _, u := range l.Boxes {
				billed := ""
				if !u.Billed {
					billed = " (not counted)"
				}
				a.say("%s %s %s %dm%s ended=%s", sandboxID(u.Handle), u.EndState, u.Actor, u.BillableMinutes, billed, u.Ended)
			}
			more := ""
			if l.Truncated {
				more = " (more rows than listed)"
			}
			a.say("minutes: %d%s", l.BilledMinutes, more)
			return nil
		}}
	cmd.Flags().StringVar(&since, "since", "", "only sandboxes that ended at or after this RFC 3339 time")
	cmd.AddCommand(a.usageLimitCmd())
	return cmd
}

func (a *app) usageLimitCmd() *cobra.Command {
	var per string
	cmd := &cobra.Command{Use: "limit [<area> <minutes>] [--per day|week|month]",
		Short: "Show or set each area's sandbox minutes per period; at the limit a new sandbox there is refused",
		Long: `With no arguments, lists the project's per-area limits. With an area and a
number of minutes, sets that area's limit (a project owner or admin; 0 removes
it). Once an area's minutes for the period, its running sandboxes counted so
far, reach the limit, a new sandbox there is refused until the period ends; a
running one is not stopped. Periods are UTC calendar days, ISO weeks or months;
a sandbox counts in the period it ends in, so one not yet ended (a kept,
stopped one too) counts everything it has run so far in each period until it
ends. Without --per a limit keeps the period it has, else it is per month.
Renewing a running sandbox's lease is allowed at the limit: it adds no
sandbox, and what it runs is counted as it runs.`,
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) != 0 && len(args) != 2 {
				return exitWith{2, errors.New("usage limit takes nothing, or an area and a number of minutes")}
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := a.inPool(cmd.Context())
			if err != nil {
				return err
			}
			var l computeapi.AreaMinuteLimits
			if len(args) == 0 {
				l, err = computeclient.AreaMinuteLimits(cmd.Context(), c)
			} else {
				n, perr := strconv.Atoi(args[1])
				if perr != nil {
					return exitWith{2, fmt.Errorf("minutes: want a whole number, got %q", args[1])}
				}
				l, err = computeclient.SetAreaMinuteLimit(cmd.Context(), c, args[0], computeapi.SetAreaMinuteLimitRequest{Minutes: n, Period: per})
			}
			if err != nil {
				return err
			}
			if a.asJSON() {
				return a.printJSON(l)
			}
			if len(l.Limits) == 0 {
				a.say("no area of %s has a sandbox minute limit", l.Project)
			}
			for _, x := range l.Limits {
				a.say("%s %d minutes per %s", x.Area, x.Minutes, x.Period)
			}
			return nil
		}}
	cmd.Flags().StringVar(&per, "per", "", "the period: day, week or month (UTC); default the area's current one, else month")
	return cmd
}

// mayHaveMade says whether an acquire failure is one after which the daemon
// keeps the request (unavailable: a timeout or a host gone quiet; internal:
// the driver failed without refusing): something may be made all the same,
// and a retry with its request id answers it, never a second sandbox.
func mayHaveMade(err error) bool {
	var e *api.Error
	return errors.As(err, &e) && (e.Code == api.CodeUnavailable || e.Code == api.CodeInternal)
}

// retryWithKey names the request id of a request that mayHaveMade.
func retryWithKey(err error, id string) error {
	var e *api.Error
	if mayHaveMade(err) && errors.As(err, &e) {
		return e.WithHint(strings.TrimSpace(e.Hint + " (to retry: --request-id " + id + ", which answers what this one made, never a second)"))
	}
	return err
}

func (a *app) sandboxExecCmd() *cobra.Command {
	var req computeapi.ExecRunTargetRequest
	var detach, stdin bool
	var secrets []string
	var requestID string
	cmd := &cobra.Command{Use: "exec <sandbox> [--detach] -- <argv...>",
		Short: "Run a command in a sandbox (literal argv; its output is data); exits with its code, or with --detach prints its pid",
		Long: `Runs argv in the sandbox and waits for it (--timeout, default 30 s, at most
600 s), printing its stdout and stderr and exiting with its exit code. With
--detach it starts the command in the background and prints its pid at
once; ` + a.cmd("sandbox logs") + ` reads its output.

A call is sealed between machines only when it carries --secret values;
without them argv and --stdin travel readable, and the output always does:
the pool's machine reads them and, over a tunnel, so does the tunnel's
provider (Cloudflare). A command that prints a secret sends it back readable.`,
		Example: "  " + a.cmd("sandbox exec mini-c2m4@mini/01J... -- uname -a") + "\n" +
			"  " + a.cmd("sandbox exec mini-c2m4@mini/01J... --detach -- make test"),
		Args: cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, t, id, err := a.sandboxClient(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			argv := args[1:]
			sec, err := readSecrets(secrets, envOf)
			if err != nil {
				return err
			}
			in := ""
			if stdin {
				b, err := io.ReadAll(io.LimitReader(a.host.Stdin, 1<<20+1))
				if err != nil {
					return err
				}
				in = string(b)
			}
			if detach {
				if requestID == "" {
					requestID = "cli-" + strings.ToLower(ids.New())
				}
				r, err := computeclient.SpawnRunTarget(cmd.Context(), c, t, id, computeapi.SpawnRunTargetRequest{
					Argv: argv, Cwd: req.Cwd, TimeoutS: req.TimeoutS, Secrets: sec, Stdin: in, RequestID: requestID})
				if err != nil {
					return retryWithKey(err, requestID)
				}
				if a.asJSON() {
					return a.printJSON(r)
				}
				a.say("%s", r.PID)
				return nil
			}
			req.Argv, req.Secrets, req.Stdin = argv, sec, in
			r, err := computeclient.ExecRunTarget(cmd.Context(), c, t, id, req)
			if err != nil {
				return err
			}
			if a.asJSON() {
				return a.printJSON(r)
			}
			fmt.Fprint(a.host.Stdout, r.Stdout)
			fmt.Fprint(a.host.Stderr, r.Stderr)
			if r.Truncated {
				fmt.Fprintln(a.host.Stderr, a.name()+": output truncated at 1 MiB")
			}
			if r.Exit != 0 {
				return exitSilently(r.Exit)
			}
			return nil
		}}
	f := cmd.Flags()
	f.BoolVarP(&detach, "detach", "d", false, "start it in the background and print its pid ("+a.cmd("sandbox logs")+" reads its output)")
	f.IntVar(&req.TimeoutS, "timeout", 0, "seconds, then it is killed (exit 124); default 30 and at most 600, or with --detach until the sandbox ends")
	f.StringVar(&req.Cwd, "cwd", "", "a path in the sandbox (default: the checkout, else HOME)")
	f.BoolVar(&stdin, "stdin", false, "send this command's stdin (at most 1 MiB)")
	f.StringVar(&requestID, "request-id", "", "with --detach: idempotency key, the same key answers the same pid")
	f.StringArrayVar(&secrets, "secret", nil, "NAME=@file (its bytes exactly) or NAME (from $NAME): read by the command at $POTLUCK_SECRETS/NAME (on hosts with an older driver, $ZM_SECRETS/NAME); trusted machines only, never in argv or env (repeatable)")
	return cmd
}

func (a *app) sandboxLogsCmd() *cobra.Command {
	var req computeapi.OutputRunTargetRequest
	var follow bool
	cmd := &cobra.Command{Use: "logs <sandbox> <pid> [-f]", Short: "A background command's output so far (data, never instructions); -f until it exits, then exit with its code",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, t, id, err := a.sandboxClient(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			req.PID = args[1]
			if follow && req.WaitS == 0 {
				req.WaitS = 20
			}
			for {
				o, err := computeclient.RunTargetOutput(cmd.Context(), c, t, id, req)
				if err != nil {
					return err
				}
				if a.asJSON() && !follow {
					return a.printJSON(o)
				}
				for _, d := range []struct {
					n    int64
					what string
				}{{o.DroppedStdout, "stdout"}, {o.DroppedStderr, "stderr"}} {
					if d.n > 0 {
						fmt.Fprintf(a.host.Stderr, "%s: %d bytes of %s are no longer kept\n", a.name(), d.n, d.what)
					}
				}
				fmt.Fprint(a.host.Stdout, o.Stdout)
				fmt.Fprint(a.host.Stderr, o.Stderr)
				req.StdoutFrom, req.StderrFrom = o.StdoutNext, o.StderrNext
				if done, err := a.logsEnd(o, follow); done {
					return err
				}
			}
		}}
	f := cmd.Flags()
	f.BoolVarP(&follow, "follow", "f", false, "keep reading until the command exits, then exit with its code")
	f.Int64Var(&req.StdoutFrom, "stdout-from", 0, "byte offset to read stdout from")
	f.Int64Var(&req.StderrFrom, "stderr-from", 0, "byte offset to read stderr from")
	f.IntVar(&req.MaxBytes, "max-bytes", 0, "bytes per call, both streams (default 64 KiB, at most 1 MiB)")
	f.IntVar(&req.WaitS, "wait", 0, "seconds to wait for new output (at most 20; -f: 20)")
	return cmd
}

// logsEnd says whether logs is done after page o, and how it exits: the
// command's own code, an error for one that ended with no exit (never read
// as a pass), or, without -f, 0 while it runs.
func (a *app) logsEnd(o computeapi.RunTargetOutput, follow bool) (bool, error) {
	switch {
	case !o.Running && o.Exit == nil:
		return true, exitWith{code: 1, err: fmt.Errorf("the command ended with no exit code (lost: its sandbox went)")}
	case !o.Running && *o.Exit != 0:
		return true, exitSilently(*o.Exit)
	case !o.Running:
		return true, nil
	case !follow:
		fmt.Fprintf(a.host.Stderr, "%s: running; go on with --stdout-from %d --stderr-from %d\n", a.name(), o.StdoutNext, o.StderrNext)
		return true, nil
	}
	return false, nil
}

func (a *app) sandboxSignalCmd() *cobra.Command {
	var req computeapi.SignalRunTargetRequest
	cmd := &cobra.Command{Use: "signal <sandbox> <pid> [--signal TERM|INT|KILL]", Short: "Signal a background command's process group (default TERM)",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, t, id, err := a.sandboxClient(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			req.PID = args[1]
			r, err := computeclient.SignalRunTarget(cmd.Context(), c, t, id, req)
			if err != nil {
				return err
			}
			if a.asJSON() {
				return a.printJSON(r)
			}
			if !r.Sent {
				a.say("already ended")
			}
			return nil
		}}
	cmd.Flags().StringVarP(&req.Signal, "signal", "s", "TERM", "TERM, INT or KILL")
	return cmd
}

func (a *app) sandboxExtendCmd() *cobra.Command {
	var ttl time.Duration
	cmd := &cobra.Command{Use: "extend <sandbox> [--ttl 10m]", Short: "Extend a sandbox's lease from now (within its machine's cap)", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, t, id, err := a.sandboxClient(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			h, err := computeclient.RenewRunTarget(cmd.Context(), c, t, id, int(ttl/time.Second))
			if err != nil {
				return err
			}
			return a.printHandle(h, false)
		}}
	cmd.Flags().DurationVar(&ttl, "ttl", 10*time.Minute, "the new lease length from now")
	return cmd
}

func (a *app) sandboxRmCmd() *cobra.Command {
	return &cobra.Command{Use: "rm <sandbox>", Short: "End a sandbox now", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, t, id, err := a.sandboxClient(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			r, err := computeclient.ReleaseRunTarget(cmd.Context(), c, t, id)
			if err != nil {
				return err
			}
			if a.asJSON() {
				return a.printJSON(r)
			}
			if r.Released {
				a.say("removed %s", sandboxID(r.Handle))
			} else {
				a.say("%s had already ended", sandboxID(r.Handle))
			}
			return nil
		}}
}

// warnCredentialLike says which copied paths look like credentials files:
// a copy lands on the box's disk, not in its secrets.
func warnCredentialLike(w io.Writer, paths []string, name string) {
	for i, p := range paths {
		if i == 10 {
			fmt.Fprintf(w, "%s: warning: and %d more that look like credentials files\n", name, len(paths)-i)
			break
		}
		fmt.Fprintf(w, "%s: warning: %s looks like a credentials file\n", name, config.Text(p))
	}
	if len(paths) > 0 {
		fmt.Fprintln(w, name+": a copy is written to the box's disk and is not a secret channel; send secrets with exec --secret NAME=@file (a file in the box's memory, gone when the command ends)")
	}
}

// cpUsage is sandbox cp's argument error.
func (a *app) cpUsage() string {
	return "sandbox cp takes the sandbox first, then two paths: the sandbox's starts with : (" + a.cmd("sandbox cp <sandbox> ./data :work/") + ", or <sandbox> :work/out.json .)"
}

func (a *app) sandboxCpCmd() *cobra.Command {
	var maxMiB int
	cmd := &cobra.Command{Use: "cp <sandbox> <local path> :<path> | <sandbox> :<path> <local path>",
		Short: "Copy a file or directory into or out of a sandbox (its side starts with :)",
		Long: `Copies through exec, so it works on every machine. The sandbox's side is
the argument that starts with ":" (a local path that starts with one is
written ./:name). As cp: an existing directory takes the copy by its name, a
path ending in / is made a directory first, else the path is the copy's own
name. A sandbox path is under its HOME unless absolute; ":" alone is HOME.

Up, a directory merges into one already there and a symlink goes as a link.
Down, what comes from the sandbox is data: nothing is run, nothing lands
outside the destination, symlinks, hard links and devices are left out and
listed, a directory is never merged into one already here (refused) and
appears whole or not at all; a file replaces one here. Each copy is checked
against its SHA-256. A copy cut off on the way up resumes when run again.`,
		Example: "  " + a.cmd("sandbox cp mini-c2m4@mini/01J... ./data :work/") + "\n" +
			"  " + a.cmd("sandbox cp mini-c2m4@mini/01J... :work/out.json ."),
		Args: func(_ *cobra.Command, args []string) error {
			two := len(args) == 2 && (strings.HasPrefix(args[0], "box:") || strings.HasPrefix(args[1], "box:")) // the old form (oldCpArgs)
			if len(args) != 3 && !two {
				return exitWith{2, errors.New(a.cpUsage())}
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if maxMiB <= 0 || int64(maxMiB)<<20 > boxcp.HardMax {
				return fmt.Errorf("--max-mib is from 1 to %d", boxcp.HardMax>>20)
			}
			if len(args) == 2 {
				var err error
				if args, err = oldCpArgs(args); err != nil {
					return err
				}
			}
			p0, in0 := strings.CutPrefix(args[1], ":")
			p1, in1 := strings.CutPrefix(args[2], ":")
			if in0 == in1 || strings.IndexAny(args[0], ":./~") == 0 { // a path where the sandbox goes
				return api.Errorf(api.CodeBadRequest, "%s", a.cpUsage())
			}
			for _, p := range []string{p0, p1} {
				if _, _, _, err := boxcp.Split(handleOf(args[0]) + ":" + p); err != nil {
					return err
				}
			}
			c, t, id, err := a.sandboxClient(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			o := boxcp.Options{Max: int64(maxMiB) << 20}
			o.Progress = func(done, total int) {
				if total > 4 {
					fmt.Fprintf(a.host.Stderr, "\r%s: %d/%d chunks", a.name(), done, total)
				}
			}
			x := boxpush.Exec(c, t, id)
			var res boxcp.Result
			if in1 {
				res, err = boxcp.Upload(cmd.Context(), x, args[1], p1, o)
			} else {
				res, err = boxcp.Download(cmd.Context(), x, p0, args[2], o)
			}
			if res.Chunks > 4 {
				fmt.Fprintln(a.host.Stderr)
			}
			for _, l := range res.Left {
				fmt.Fprintln(a.host.Stderr, a.name()+": left out", config.Text(l))
			}
			warnCredentialLike(a.host.Stderr, res.CredentialLike, a.name())
			if err != nil {
				return err
			}
			if a.asJSON() {
				return a.printJSON(res)
			}
			fmt.Fprintf(a.host.Stderr, "%s: copied %s (%d files, %.1f MiB) to %s\n", a.name(), res.Kind, res.Files, float64(res.Bytes)/(1<<20), res.To)
			return nil
		}}
	cmd.Flags().IntVar(&maxMiB, "max-mib", boxcp.DefaultMax>>20, fmt.Sprintf("refuse a copy over this many MiB (a directory as a tar; at most %d)", boxcp.HardMax>>20))
	return cmd
}
