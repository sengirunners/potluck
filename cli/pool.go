package cli

// Pools: create, host and host stop; the members' commands (invite, join,
// leave, remotes) are pool_members.go. A pool is a team project this machine
// runs (its authority) with two areas: members join "team" and run
// sandboxes; machines that share themselves join "hosts" as guests, trusted
// (a friend) or public (a stranger) as their invite says.

import (
	"cmp"
	"context"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/computeclient"
	"github.com/sengirunners/potluck/hostprobe"
	"github.com/sengirunners/potluck/hostsetup"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/project"
)

// The pool's areas.
const (
	HostsArea = "hosts"
	TeamArea  = "team"
)

// tailnetWait bounds a first tailnet login.
var tailnetWait = 10 * time.Minute

// advertWait is how long potluck host waits for the pool to list this
// machine: a host's heartbeat carries it every 15 s.
var advertWait = 45 * time.Second

// classMeans is what a shared machine's trust means, in plain words.
var classMeans = map[string]string{
	api.MachineTrusted: "trusted (a friend): sandboxes are placed on it unasked, and it may run GitHub Actions jobs and get members' secrets",
	api.MachinePublic:  "public (a stranger): it runs only sandboxes a member names, never GitHub Actions jobs, never any secret",
}

func (a *app) createCmd() *cobra.Command {
	var via string
	cmd := &cobra.Command{
		Use:   "create <pool> [--via tunnel]",
		Short: "Start a pool on this machine, which runs it; then invite friends with " + a.cmd("invite"),
		Long: `Create makes a pool: the group of machines you and your friends lend and
use. This machine runs it. By default machines reach each other over
Tailscale (a free Tailscale account, not a Potluck one): the first time,
create prints a login link and waits for you to sign in. Friends' machines
must reach this one: on your tailnet, or with this machine shared into theirs.
With --via tunnel no Tailscale is needed: a tunnel you configure (config.json
"tunnel") carries the pool to friends. With none configured the pool works on
this machine alone, which is enough to try Potluck by yourself (` + a.cmd("host") + `
--reclaim-idle-s 0, then ` + a.cmd("sandbox create") + `; on a Mac a sandbox otherwise
waits until you have been idle for five minutes and ends when you touch the
keyboard). Running create again on an existing pool is safe.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, _, err := a.client()
			if err != nil {
				return err
			}
			return a.poolCreate(cmd.Context(), c, args[0], via)
		},
	}
	cmd.Flags().StringVar(&via, "via", "", "tailnet or tunnel (default: the tailnet, started here if it is not)")
	return cmd
}

func (a *app) poolCreate(ctx context.Context, c *client.Client, slug, via string) error {
	p, err := a.makePool(ctx, c, slug, via)
	if err != nil {
		return err
	}
	if a.asJSON() {
		return a.printJSON(p)
	}
	a.say("pool %s is ready; this machine runs it", slug)
	if m := poolSealing(ctx, a, slug); strings.HasPrefix(m, computeapi.SealingRequired) {
		a.say("sealing: %s (a new pool starts there: a machine too old to seal gets no secrets; %s lists them)", m, a.cmd("sealing"))
	} else if m != "" {
		a.say("sealing: %s (%s lists the machines that cannot seal yet)", m, a.cmd("sealing"))
	}
	a.say("next: %s for a friend who runs sandboxes, %s for one who shares a machine", a.cmd("invite <name>"), a.cmd("invite <name> --host"))
	a.say("share this machine too: %s", a.cmd("host"))
	return nil
}

// makePool makes pool slug run by this machine, or completes one made
// before (its areas, its team): create's work, without its words.
func (a *app) makePool(ctx context.Context, c *client.Client, slug, via string) (api.Project, error) {
	if !api.ValidSlug(slug) {
		return api.Project{}, fmt.Errorf("pool name %q: use lowercase letters, digits and dashes", slug)
	}
	p, err := c.GetProject(ctx, slug)
	missing := api.IsCode(err, api.CodeNotFound)
	if err != nil && !missing {
		return p, err
	}
	// The tailnet login before anything is made: a create cancelled while it
	// waits leaves no half-made pool behind.
	if via != api.ViaTunnel {
		if err := a.ensureTailnet(ctx, c); err != nil {
			return p, err
		}
	}
	if missing {
		if p, err = c.CreateProject(ctx, api.NewProjectRequest{Slug: slug}); err != nil {
			return p, err
		}
	}
	for _, area := range []string{HostsArea, TeamArea} {
		if !slices.ContainsFunc(p.Areas, func(x api.Area) bool { return x.Name == area }) {
			if p, err = c.AddArea(ctx, slug, api.AddAreaRequest{Name: area}); err != nil {
				return p, fmt.Errorf("area %s: %w", area, err)
			}
		}
	}
	if _, err := c.ProjectTeam(ctx, slug); err != nil {
		return p, err
	}
	return p, nil
}

// ensureTailnet brings this machine's tailnet node up, waiting out the first
// login (its URL printed once, on stderr under --json). When it gives up
// before the login (Ctrl-C, the timeout, a node error) it stops a node this
// run turned on, unless the node got logged in meanwhile, so the daemon does
// not keep asking for a login; and it says nothing was made.
func (a *app) ensureTailnet(ctx context.Context, c *client.Client) error {
	w := a.notes()
	st, err := c.Tailnet(ctx)
	if err == nil && st.Running {
		return nil
	}
	startedHere := err == nil && !st.Enabled // the node was off: this run turns it on
	giveUp := func(why string) error {
		note := "the tailnet node was already on and is left as it was"
		if startedHere {
			stop, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			defer cancel()
			if now, err := c.Tailnet(stop); err == nil && now.Running {
				note = "the tailnet node is logged in now; run this again"
			} else if _, err := c.TailnetDown(stop); err != nil {
				note = "stopping the tailnet node it started failed (" + err.Error() + ")"
			} else {
				note = "the tailnet node it started is stopped"
			}
		}
		return fmt.Errorf("%s: nothing was made; %s", why, note)
	}
	const cancelled = "cancelled while waiting for the Tailscale login"
	if st, err = c.TailnetUp(ctx, api.TailnetUpRequest{AuthKey: os.Getenv("TS_AUTHKEY")}); err != nil {
		if ctx.Err() != nil {
			return giveUp(cancelled)
		}
		return giveUp("tailnet: " + err.Error())
	}
	shown, deadline := "", time.Now().Add(tailnetWait)
	for !st.Running {
		switch {
		case st.Error != "":
			return giveUp("tailnet: " + st.Error)
		case time.Now().After(deadline):
			return giveUp(fmt.Sprintf("no Tailscale login within %s; log in, then run this again (or set TS_AUTHKEY)", tailnetWait))
		case st.AuthURL != "" && st.AuthURL != shown:
			if shown == "" {
				fmt.Fprintln(w, "this pool needs this machine on a tailnet (a free Tailscale account); nothing is made until you log in (Ctrl-C cancels):")
			}
			fmt.Fprintf(w, "log in to your tailnet (waiting): %s\n", st.AuthURL)
			shown = st.AuthURL
		}
		select {
		case <-ctx.Done():
			return giveUp(cancelled)
		case <-time.After(3 * time.Second):
		}
		if st, err = c.Tailnet(ctx); err != nil {
			if ctx.Err() != nil {
				return giveUp(cancelled)
			}
			return giveUp("tailnet: " + err.Error())
		}
	}
	return nil
}

// poolMachines are the names of the pool's shared machines (as sandbox
// create --on takes them).
func poolMachines(ctx context.Context, c *client.Client) ([]string, error) {
	views, err := poolSizes(ctx, c)
	names := []string{}
	for _, t := range views {
		names = append(names, t.Name)
	}
	return names, err
}

// poolSizes are the sizes shared with c's pool, as its machine lists them.
func poolSizes(ctx context.Context, c *client.Client) ([]computeapi.RunTargetView, error) {
	l, err := computeclient.RunTargets(ctx, c, false)
	if err != nil {
		return nil, err
	}
	return slices.DeleteFunc(l.Targets, func(t computeapi.RunTargetView) bool { return t.Kind != "box" }), nil
}

// printPoolMachines says which machines c's pool can run sandboxes on.
func (a *app) printPoolMachines(ctx context.Context, c *client.Client) {
	names, err := poolMachines(ctx, c)
	switch {
	case err != nil:
		a.say("machines: not listed (%v)", err)
	case len(names) == 0:
		a.say("no machine is shared with the pool yet: a member shares one with %s", a.cmd("host"))
	default:
		a.say("sandboxes can run on: %s", strings.Join(names, ", "))
	}
}

func (a *app) hostCmd() *cobra.Command {
	var s shareFlags
	var yes bool
	cmd := &cobra.Command{
		Use:   "host ['<invite code>' | <pool>] [--image name@sha256:<digest>] [flags]",
		Short: "Share this machine with a pool: check it, join with the invite code (unless joined), and wait until the pool lists it",
		Long: `Host checks this machine (a missing root step it runs with sudo at a
terminal, which asks for your password; else, or with --no-sudo, it prints
the one sudo line), pulls the images (without --image, Potluck's default for
the layer; one already here at its exact digest is not pulled again) and
runs a quick conformance check per size. Only when a size passes does it
join the pool with the invite code (or take a pool this machine already
joined, or runs) and share each size that passed; then it
waits until the pool lists them. A machine that fails never joins, and its
invite stays unspent. With no argument it shares with the only pool this
machine is in. On a Mac a sandbox ends when you touch the keyboard or pointer
(--reclaim-idle-s). ` + a.cmd("host stop") + ` undoes it.

Run again where this machine already shares sizes with the pool and with no
flag that changes the share (--image, --sizes, --docker and the like), host
only prints what it shares and what the run would do, changes nothing, and
asks for --yes; ` + a.cmd("host status") + ` is how to look.

The invite goes whole, as one word in single quotes: copy the one line the
invite printed. An invite cut short (only its last part, the secret) or
changed by a copy (a no-break space, a curly quote, a long dash) is refused
before anything runs or downloads, saying what is wrong but never repeating
it. Host's output holds no invite, so it is safe to share when asking for
help; the invite is not.`,
		Args: a.pastedArgs(0, 1, "at most one argument, the invite in single quotes or a pool's name"),
		RunE: func(cmd *cobra.Command, args []string) error {
			arg := a.pool()
			if len(args) == 1 {
				arg = args[0]
			}
			return a.runHost(cmd, &s, arg, !yes && !a.shapesShare(cmd))
		},
	}
	s.add(cmd, a)
	pastedFlags(cmd)
	cmd.Flags().BoolVar(&yes, "yes", false, "share again where this machine already shares sizes with the pool, with no flag that changes the share (such a run otherwise only says what it would do)")
	cmd.AddCommand(a.hostStopCmd(), a.hostShelfCmd(), a.hostImagesCmd(), a.hostStatusCmd(), a.hostJobsCmd(), a.hostLendCmd(), a.hostSetCmd(), a.hostPauseCmd(), a.hostResumeCmd())
	cmd.AddCommand(a.hostOldSpellings()...)
	return cmd
}

// noSizePassedError is host's answer when every size failed its checks:
// what happened, and what to do next in plain words. The checks run after
// the pulls, and a pulled image stays on this machine, so a second run
// starts at the checks; a check can fail once on a busy or just-booted
// machine. A run for a pool (not an invite) leaves the sizes an earlier run
// shared bound unless Bind unbinds them (no admission keys, unfit), so it
// says which of them still lend, read back after Bind, and then names the
// flags given that shape the share (runHost sets them): none took effect.
type noSizePassedError struct {
	byCode bool
	pool   string
	still  []string // the sizes shared with pool after this run
	unread string   // why still could not be read ("": it was)
	flags  []string // runHost: the flags given that shape the share
	p      prefix
}

func (e *noSizePassedError) Error() string {
	var b strings.Builder
	b.WriteString("no size passed conformance: ")
	switch {
	case e.byCode:
		b.WriteString("this machine did not join the pool, and the invite is not spent")
	case e.unread != "":
		fmt.Fprintf(&b, "nothing new is shared with pool %s (what this machine lends there now could not be read: %s)", e.pool, e.unread)
	case len(e.still) > 0:
		fmt.Fprintf(&b, "nothing changed: the earlier sizes still lend to pool %s (%s)", e.pool, strings.Join(e.still, ", "))
		if len(e.flags) > 0 {
			fl := make([]string, len(e.flags))
			for i, f := range e.flags {
				fl[i] = "--" + f
			}
			fmt.Fprintf(&b, ", as they were: %s did not take effect", strings.Join(fl, ", "))
		}
	default:
		fmt.Fprintf(&b, "this machine shares nothing with pool %s", e.pool)
	}
	fmt.Fprintf(&b, ".\n"+
		"Next: run the same %s command again. The images it pulled stay on this machine, so it starts at the checks, not the download.\n"+
		"If the same check fails again, it is not a one-off: keep the output above, which names the check that failed.", e.p.cmd("host"))
	return b.String()
}

// poolHost shares this machine with the pool. root, when set, runs the
// root step once the pool is known and the share's own checks pass; after,
// when set, runs once a size is shared (the host's AfterLend).
func (a *app) poolHost(ctx context.Context, c *client.Client, run hostprobe.Runner, arg string, o hostprobe.ShareOptions, w io.Writer, root func(*hostprobe.ShareOptions) error, after func()) error {
	if root == nil {
		root = func(*hostprobe.ShareOptions) error { return nil }
	}
	if o.DryRun {
		if err := root(&o); err != nil {
			return err
		}
		res, err := hostprobe.Share(ctx, run, nil, o)
		if err != nil {
			return err
		}
		if err := a.reportShare(res, w); err != nil {
			return err
		}
		if res.RunnerDir != "" && !a.asJSON() {
			fmt.Fprintln(w, "note: only a trusted machine serves GitHub Actions jobs: one whose invite made it public serves none")
		}
		return nil
	}
	sc, err := a.hostScope(ctx, c, arg, &o)
	if err != nil {
		return err
	}
	pool, machine := sc.pool, sc.machine
	if err := root(&o); err != nil {
		return err
	}
	say := func(line string) {
		if o.Progress != nil {
			o.Progress(line)
		}
	}
	// Doctor, pulls and conformance before the join: a machine that cannot
	// share, or fails the security checks, never spends the invite.
	stopBeat := startBeat(&o)
	res, err := hostprobe.Check(ctx, run, o)
	stopBeat()
	if err != nil {
		return err
	}
	byCode := sc.byCode
	if words, ok := validationWords(res); ok { // else the error below blocks it
		say(o.Stages.Succeed(hostsetup.Validation, words))
	}
	if !slices.ContainsFunc(res.Sizes, func(s hostprobe.SizeResult) bool { return s.Passed }) {
		var nb hostprobe.Binder // shares nothing: says each size is not shared
		no := o
		if !byCode {
			nb = poolBinder(c, &no, pool, machine)
		}
		res = hostprobe.Bind(ctx, nb, no, res)
		_ = a.reportShare(res, w)
		e := &noSizePassedError{byCode: byCode, pool: pool, p: prefix(a.host.Prefix)}
		if !byCode { // read back what lends now: Bind may have unbound a keyless or unfit size
			now := o
			now.Earlier, now.EarlierUnread = nil, ""
			nsc, err := a.hostScope(ctx, c, arg, &now)
			switch {
			case err != nil:
				e.unread = err.Error()
			case now.EarlierUnread != "":
				e.unread = now.EarlierUnread
			default:
				e.still = nsc.sharedAgain(now)
			}
		}
		return e
	}
	if byCode {
		say(o.Stages.Begin(hostsetup.Join, "joining the pool of your invite"))
		rem, err := a.poolJoin(ctx, c, arg, "")
		if err != nil {
			if ours := joinedAtAuthority(ctx, c, arg); len(ours) > 0 && api.IsCode(err, api.CodeForbidden) {
				return &spentCodeError{err: err, Pools: ours, p: prefix(a.host.Prefix)}
			}
			return err
		}
		pool, machine = rem.Project.Slug, rem.Machine
		say(o.Stages.Succeed(hostsetup.Join, "joined pool "+pool))
		o.Stages.SetAgain(strings.Replace(o.Stages.Run().Again, inviteWord, pool, 1)) // the flags stay
	} else if machine != "" {
		say(o.Stages.Succeed(hostsetup.Join, "already in pool "+pool))
	} else {
		say(o.Stages.Succeed(hostsetup.Join, "pool "+pool+" runs on this machine"))
	}
	res = hostprobe.Bind(ctx, poolBinder(c, &o, pool, machine), o, res)
	if err := a.reportShare(res, w); err != nil {
		o.Stages.Block(hostsetup.Capacity, capacityWords(res, pool))
		if after != nil && lent(res) { // a size that failed leaves the rest shared
			after()
		}
		return err
	}
	var want []string
	for _, s := range res.Sizes {
		if s.Bound {
			if machine != "" {
				want = append(want, s.Binding+"@"+machine)
			} else {
				want = append(want, s.Binding)
			}
		}
	}
	lw := w // stdout stays the result's JSON
	if a.asJSON() {
		lw = a.host.Stderr
	}
	say(o.Stages.Begin(hostsetup.Capacity, "waiting for pool "+pool+" to list "+strings.Join(want, ", ")))
	if err := awaitListed(ctx, c, pool, want, res.RunnerDir != "", lw, prefix(a.host.Prefix)); err != nil {
		return err
	}
	say(o.Stages.Succeed(hostsetup.Capacity, capacityWords(res, pool)))
	if after != nil {
		after()
	}
	return nil
}

// hostScope is where a host run of arg (an invite code, or a pool) shares:
// a pool named is known before anything runs (machine is set for a pool
// this machine joined); the shared sizes it replaces there are read into o
// (or why not, which Check refuses).
type hostScope struct {
	pool, machine string
	byCode        bool
	poolID        string // a joined pool's id at its machine
	// shared is each machine-level binding's share list (a joined pool's
	// scope), by name.
	shared map[string][]string
}

func (a *app) hostScope(ctx context.Context, c *client.Client, arg string, o *hostprobe.ShareOptions) (hostScope, error) {
	sc := hostScope{pool: arg, byCode: project.LooksLikeJoinCode(arg)}
	if !sc.byCode {
		rems, err := c.Remotes(ctx)
		if err != nil {
			return sc, err
		}
		for _, r := range rems {
			if r.Project.Slug == sc.pool {
				sc.machine, sc.poolID = r.Machine, r.Project.ID
			}
		}
		if sc.machine == "" {
			if _, err := c.GetProject(ctx, sc.pool); err != nil {
				return sc, fmt.Errorf("%s is no pool this machine joined or runs: pass the invite code, %s", sc.pool, a.cmd("host '<code>'"))
			}
		}
	}
	var l computeapi.RunTargetList
	var err error
	if sc.byCode || sc.machine != "" {
		l, err = computeclient.HostRunTargets(ctx, c)
	} else {
		l, err = computeclient.RunTargets(ctx, inPool(c, sc.pool), false)
	}
	if err != nil {
		o.EarlierUnread = err.Error() // fail closed in Check
	} else {
		o.Earlier = targetsOf(l)
		sc.shared = map[string][]string{}
		for _, v := range l.Targets {
			sc.shared[v.Name] = v.Shared
		}
	}
	return sc, nil
}

// lent reports whether any size of res is shared.
func lent(res hostprobe.ShareResult) bool {
	return slices.ContainsFunc(res.Sizes, func(s hostprobe.SizeResult) bool { return s.Bound })
}

// spentCodeError is a join code that failed on a machine that already
// joined pools at the code's machine: it names them and the command that
// shares with one.
type spentCodeError struct {
	err   error
	Pools []string
	Flags string // the flags the user passed, as they would retype them
	p     prefix // how the program names Potluck's commands
}

func (e *spentCodeError) Unwrap() error { return e.err }

func (e *spentCodeError) Error() string {
	var b strings.Builder
	b.WriteString(e.err.Error())
	fmt.Fprintf(&b, "\nthis machine already joined %s there; to share it with one, run:", strings.Join(e.Pools, ", "))
	for _, p := range e.Pools {
		fmt.Fprintf(&b, "\n  %s%s", e.p.cmd("host "+p), e.Flags)
	}
	return b.String()
}

// joinedAtAuthority is the pools this machine holds a healthy remote for at
// the machine a join code names.
func joinedAtAuthority(ctx context.Context, c *client.Client, code string) []string {
	jc, err := project.ParseJoinCode(code)
	if err != nil {
		return nil
	}
	rems, err := c.Remotes(ctx)
	if err != nil {
		return nil
	}
	var pools []string
	for _, r := range rems {
		if r.State == api.RemoteOK && strings.EqualFold(r.Fingerprint, jc.Fingerprint) && (jc.Machine == "" || r.Authority == jc.Machine) {
			pools = append(pools, r.Project.Slug)
		}
	}
	return pools
}

// shareFlagArgs renders the flags the user set as arguments to retype.
func shareFlagArgs(cmd *cobra.Command, skip ...string) string {
	var b strings.Builder
	cmd.Flags().Visit(func(f *pflag.Flag) {
		if slices.Contains(skip, f.Name) {
			return
		}
		vals := []string{f.Value.String()}
		if sv, ok := f.Value.(pflag.SliceValue); ok {
			vals = sv.GetSlice()
		}
		for _, v := range vals {
			if f.Value.Type() == "bool" {
				fmt.Fprintf(&b, " --%s=%s", f.Name, v)
			} else {
				fmt.Fprintf(&b, " --%s %s", f.Name, shellQuote(v))
			}
		}
	})
	return b.String()
}

// poolBinder is where host shares: machine-level, with the pool's machine
// (o.Pool), for a pool this machine joined; else, on the pool's own
// machine, in the pool.
func poolBinder(c *client.Client, o *hostprobe.ShareOptions, pool, machine string) hostprobe.Binder {
	if machine != "" {
		o.Pool = pool
		return computeclient.Owner{C: c}
	}
	return computeclient.Owner{C: inPool(c, pool)}
}

// awaitListed waits up to advertWait for the pool to list want. Sizes that
// would serve GitHub runners (runners) on a machine the pool holds public
// serve none: the pool gives a runner only to a trusted machine, so it says
// so.
func awaitListed(ctx context.Context, c *client.Client, pool string, want []string, runners bool, w io.Writer, p prefix) error {
	deadline := time.Now().Add(advertWait)
	for {
		views, _ := poolSizes(ctx, inPool(c, pool))
		var names []string
		public := ""
		for _, v := range views {
			names = append(names, v.Name)
			if slices.Contains(want, v.Name) && v.HostClass != api.MachineTrusted {
				public = cmp.Or(v.Host, v.Name)
			}
		}
		missing := slices.DeleteFunc(slices.Clone(want), func(n string) bool { return slices.Contains(names, n) })
		if len(missing) == 0 {
			fmt.Fprintf(w, "shared with pool %s: %s\nmembers: %s\n", pool, strings.Join(want, ", "), p.cmd("sandbox create"))
			if runners && public != "" {
				fmt.Fprintf(w, "note: pool %s holds %s public, so its sandboxes serve no GitHub Actions jobs: a runner goes only to a trusted machine, which the pool's owner chooses on the invite (%s)\n",
					pool, public, p.cmd("invite <name> --host"))
			}
			return nil
		}
		if time.Now().After(deadline) {
			fmt.Fprintf(w, "shared, but the pool does not list %s yet: it does once this machine's heartbeat reaches the pool's machine (%s)\n", strings.Join(missing, ", "), p.cmd("status"))
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// targetsOf is a list's entries on this machine (not another host's).
func targetsOf(l computeapi.RunTargetList) []computeapi.RunTarget {
	var out []computeapi.RunTarget
	for _, v := range l.Targets {
		if v.RunTarget.Host == "" {
			out = append(out, v.RunTarget)
		}
	}
	return out
}

func (a *app) hostStopCmd() *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "stop [--force]",
		Short: "Stop sharing this machine: remove every size " + a.cmd("host") + " shared from it (refused for a size with a sandbox still running, unless --force)",
		Long: `Removes every size ` + a.cmd("host") + ` shared from this machine. A size with
sandboxes still running on it is refused and named: end them first
(` + a.cmd("sandbox rm") + `, by the member who made them) or pass --force. With
--force a size shared with a pool this machine joined is removed anyway, and
its running sandboxes go on until their lease ends, out of reach of the
members who made them. In a pool this machine runs, a size with running
sandboxes is always refused.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, _, err := a.client()
			if err != nil {
				return err
			}
			removed, err := hostStop(cmd.Context(), c, a.pool(), force, a.host.Stderr, a.name())
			if !a.asJSON() {
				if len(removed) > 0 {
					a.say("no longer shared: %s", strings.Join(removed, ", "))
				} else if err == nil {
					a.say("this machine shares nothing")
				}
			}
			was := ""
			if err == nil && a.pool() == "" && len(removed) > 0 { // this run left nothing shared: a pause has nothing left to keep
				w := a.host.Stdout
				if a.asJSON() {
					w = a.host.Stderr
				}
				was, err = unpauseAfterStop(w)
			}
			if a.asJSON() {
				ans := map[string]any{"removed": removed}
				if was != "" {
					ans["switch_was"] = was
				}
				if perr := a.printJSON(ans); perr != nil {
					return perr
				}
			}
			return err
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "remove a size shared with a joined pool even while sandboxes run on it (they go on until their lease ends)")
	return cmd
}

// hostStop removes this machine's shared sizes: those shared with pools it
// joined (machine-level) and those in pools it runs (only pool, when set).
// It goes on past a refusal and returns the first error. The daemon refuses
// to remove a size in a pool this machine runs while sandboxes run on it,
// but not a machine-level one (the sandboxes' records are at the pools'
// machines), so for those the drivers' own lists are asked here first.
func hostStop(ctx context.Context, c *client.Client, only string, force bool, warn io.Writer, name string) (removed []string, first error) {
	fail := func(err error) {
		if first == nil {
			first = err
		}
	}
	l, err := computeclient.HostRunTargetsLive(ctx, c)
	if err != nil {
		return nil, err
	}
	for _, t := range l.Targets {
		if t.RunTarget.Host != "" || t.Kind != "box" || (only != "" && !slices.Contains(t.Shared, only)) {
			continue
		}
		switch {
		case force && t.Error != "":
			fmt.Fprintf(warn, "%s: %s: could not tell whether sandboxes run on it (%s); removed anyway (--force)\n", name, t.Name, t.Error)
		case force && len(t.Live) > 0:
			fmt.Fprintf(warn, "%s: %s: %s still running; removed anyway (--force): they go on until their lease ends\n", name, t.Name, sandboxCount(len(t.Live)))
		case t.Error != "":
			fail(fmt.Errorf("%s: could not tell whether sandboxes run on it (%s): try again, or pass --force", t.Name, t.Error))
			continue
		case len(t.Live) > 0:
			fail(fmt.Errorf("%s still running on %s: stop them first or pass --force", sandboxCount(len(t.Live)), t.Name))
			continue
		}
		if err := computeclient.UnbindHostRunTarget(ctx, c, t.Name); err != nil {
			fail(fmt.Errorf("%s: %w", t.Name, err))
			continue
		}
		removed = append(removed, t.Name)
	}
	ps, err := c.LocalProjects(ctx)
	if err != nil {
		fail(err)
		return removed, first
	}
	for _, p := range ps {
		if only != "" && p.Slug != only {
			continue
		}
		pc := inPool(c, p.Slug)
		l, err := computeclient.RunTargets(ctx, pc, false)
		if err != nil {
			fail(fmt.Errorf("pool %s: %w", p.Slug, err))
			continue
		}
		for _, t := range targetsOf(l) {
			if t.Kind != "box" {
				continue
			}
			if err := computeclient.UnbindRunTarget(ctx, pc, t.Name); err != nil {
				fail(fmt.Errorf("%s in %s: %w", t.Name, p.Slug, err))
				continue
			}
			removed = append(removed, t.Name+" in "+p.Slug)
		}
	}
	return removed, first
}

func sandboxCount(n int) string {
	if n == 1 {
		return "1 sandbox is"
	}
	return fmt.Sprintf("%d sandboxes are", n)
}
