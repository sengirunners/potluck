package cli

// Pools: create, invite, join, leave, remotes, host and host stop (copied
// from zm's cmd/zm pool.go and team.go). A pool is a team project this
// machine runs (its authority) with two areas: members join "team" and run
// sandboxes; machines that share themselves join "hosts" as guests, trusted
// (a friend) or public (a stranger) as their invite says.

import (
	"context"
	"errors"
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
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/config"
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
		Short: "Start a pool on this machine, which runs it; then invite friends with potluck invite",
		Long: `Create makes the pool (a team project with the areas "team" and "hosts"),
makes this machine the one that runs it and brings its tailnet node up
(the first time, it prints a login URL and waits). Each friend's machine must
reach this one: on your tailnet, or with this machine shared into theirs.
With --via tunnel no tailnet is needed: this machine's tunnel (config.json
"tunnel") carries the pool. Running it again on an existing pool is safe.`,
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
	if !api.ValidSlug(slug) {
		return fmt.Errorf("pool name %q: use lowercase letters, digits and dashes", slug)
	}
	p, err := c.GetProject(ctx, slug)
	if api.IsCode(err, api.CodeNotFound) {
		p, err = c.CreateProject(ctx, api.NewProjectRequest{Slug: slug})
	}
	if err != nil {
		return err
	}
	for _, area := range []string{HostsArea, TeamArea} {
		if !slices.ContainsFunc(p.Areas, func(x api.Area) bool { return x.Name == area }) {
			if p, err = c.AddArea(ctx, slug, api.AddAreaRequest{Name: area}); err != nil {
				return fmt.Errorf("area %s: %w", area, err)
			}
		}
	}
	if _, err := c.ProjectTeam(ctx, slug); err != nil {
		return err
	}
	if via != api.ViaTunnel {
		if err := ensureTailnet(ctx, c, a.stdout); err != nil {
			return err
		}
	}
	if a.json {
		return a.printJSON(p)
	}
	a.say("pool %s is ready; this machine runs it", slug)
	if m := poolSealing(ctx, a, slug); strings.HasPrefix(m, computeapi.SealingRequired) {
		a.say("sealing: %s (a new pool starts there: a machine too old to seal gets no secrets; potluck sealing lists them)", m)
	} else if m != "" {
		a.say("sealing: %s (potluck sealing lists the machines that cannot seal yet)", m)
	}
	a.say("next: potluck invite <name> for a friend who runs sandboxes, potluck invite <name> --host for one who shares a machine")
	a.say("share this machine too: potluck host")
	return nil
}

// ensureTailnet brings this machine's tailnet node up, waiting out the first
// login (its URL printed once).
func ensureTailnet(ctx context.Context, c *client.Client, w io.Writer) error {
	st, err := c.Tailnet(ctx)
	if err == nil && st.Running {
		return nil
	}
	if st, err = c.TailnetUp(ctx, api.TailnetUpRequest{AuthKey: os.Getenv("TS_AUTHKEY")}); err != nil {
		return err
	}
	shown, deadline := "", time.Now().Add(tailnetWait)
	for !st.Running {
		switch {
		case st.Error != "":
			return fmt.Errorf("tailnet: %s", st.Error)
		case time.Now().After(deadline):
			return errors.New("no tailnet login in time: run this again")
		case st.AuthURL != "" && st.AuthURL != shown:
			fmt.Fprintf(w, "log in to your tailnet (waiting): %s\n", st.AuthURL)
			shown = st.AuthURL
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(3 * time.Second):
		}
		if st, err = c.Tailnet(ctx); err != nil {
			return err
		}
	}
	return nil
}

func (a *app) inviteCmd() *cobra.Command {
	var host bool
	var trust, via string
	var ttl time.Duration
	cmd := &cobra.Command{
		Use:   "invite <name> [--host [--trust public]]",
		Short: "Print a one-time invite code for a friend: to run sandboxes, or with --host to share a machine",
		Long: `Without --host the friend joins the pool's team and runs sandboxes on its
machines. With --host they join to share a machine (potluck host), and see
nothing else of the pool. A shared machine is trusted (a friend's: sandboxes
are placed on it unasked, and it may get members' secrets and run GitHub
Actions jobs) unless --trust public (a stranger's: only sandboxes a member
names, no secrets, no Actions jobs). The trust is yours to choose here; the
machine cannot change it. Codes work once and expire (--ttl).`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if trust != "" && !host {
				return errors.New("--trust goes with --host: it is how far you trust the machine they share")
			}
			switch trust {
			case "", api.MachineTrusted, api.MachinePublic:
			default:
				return fmt.Errorf("--trust is trusted or public, not %q", trust)
			}
			c, err := a.inPool(cmd.Context())
			if err != nil {
				return err
			}
			req := api.InviteRequest{Project: c.Project, Area: TeamArea, As: args[0], Role: api.RoleMember, Via: via,
				TTLSeconds: int64(ttl / time.Second)}
			if host {
				req.Area, req.Role, req.Class = HostsArea, "guest", trust
				if req.Class == "" {
					req.Class = api.MachineTrusted
				}
			}
			r, err := c.Invite(cmd.Context(), req)
			if err != nil {
				return err
			}
			if a.json {
				return a.printJSON(r)
			}
			until := r.Invite.Expires.Local().Format("Jan 2 15:04")
			if host {
				a.say("host %s (one use, until %s), on the machine they share:\n  potluck host '%s'\n  (or potluck join '%s', then potluck host)\n  their machine will be %s\n%s",
					args[0], until, r.Code, r.Code, classMeans[req.Class], tailnetStep(args[0], r.Code))
			} else {
				a.say("member %s (one use, until %s), on their machine:\n  potluck join '%s'\n%s", args[0], until, r.Code, tailnetStep(args[0], r.Code))
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&host, "host", false, "the friend shares a machine with the pool")
	cmd.Flags().StringVar(&trust, "trust", "", "with --host: trusted (default) or public")
	cmd.Flags().StringVar(&via, "via", "", "tailnet or tunnel (default: the tailnet when it runs)")
	cmd.Flags().DurationVar(&ttl, "ttl", 24*time.Hour, "how long the code works")
	return cmd
}

// tailnetStep is what the pool's owner does so a friend's machine reaches
// this one with a tailnet code, in plain words; "" for a tunnel code.
func tailnetStep(name, code string) string {
	if strings.HasPrefix(code, "https://") || strings.HasPrefix(code, "http://") {
		return ""
	}
	node := "this machine's node" // its name in the admin console: the code's node name
	if jc, err := project.ParseJoinCode(code); err == nil {
		prefix := jc.NodePrefix
		if prefix == "" {
			prefix = api.NodePrefix
		}
		node = prefix + jc.Machine
	}
	return fmt.Sprintf("  before %s runs it, their machine must reach this one over Tailscale: if they are not on your tailnet,\n"+
		"  share this machine with them at https://login.tailscale.com/admin/machines (%s, then the ... menu, Share)\n"+
		"  and send them the link, or invite them to your tailnet at https://login.tailscale.com/admin/users", name, node)
}

func (a *app) joinCmd() *cobra.Command {
	var authKey string
	cmd := &cobra.Command{
		Use:   "join '<invite code>'",
		Short: "Join a pool with the invite code its owner sent",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, _, err := a.client()
			if err != nil {
				return err
			}
			rem, err := a.poolJoin(cmd.Context(), c, args[0], authKey)
			if err != nil {
				return err
			}
			if a.json {
				return a.printJSON(rem)
			}
			if rem.Cap.Area == HostsArea {
				a.say("next: potluck host, to share this machine with %s", rem.Project.Slug)
				return nil
			}
			a.printPoolMachines(cmd.Context(), inPool(c, rem.Project.Slug))
			a.say("next: potluck sandbox create, then potluck sandbox exec <sandbox> -- uname -a")
			return nil
		},
	}
	cmd.Flags().StringVar(&authKey, "auth-key", "", "a Tailscale auth key for this machine's node (else a login URL is printed); never saved")
	return cmd
}

// poolJoin redeems an invite code.
func (a *app) poolJoin(ctx context.Context, c *client.Client, code, authKey string) (api.Remote, error) {
	if !project.LooksLikeJoinCode(code) {
		return api.Remote{}, fmt.Errorf("%q is not an invite code: the pool's owner prints one with potluck invite <you>", code)
	}
	req := api.JoinRequest{Code: strings.TrimSpace(code), AuthKey: authKey, WaitMS: 10000}
	req.Cwd, _ = os.Getwd()
	r, err := a.joinUntilDone(ctx, c, req)
	if err != nil {
		return api.Remote{}, err
	}
	if r.Remote == nil {
		return api.Remote{}, errors.New("nothing joined")
	}
	x := r.Remote
	if !a.json {
		a.say("joined pool %s on %s as %s (%s), machine %s", x.Project.Slug, x.Authority, x.Identity, x.Cap.Role, x.Machine)
	}
	return *x, nil
}

// joinUntilDone sends the join, waiting out a tailnet login.
func (a *app) joinUntilDone(ctx context.Context, c *client.Client, req api.JoinRequest) (api.JoinResponse, error) {
	deadline := time.Now().Add(10 * time.Minute)
	shown := ""
	for {
		r, err := c.Join(ctx, req)
		if err != nil || r.AuthURL == "" {
			return r, err
		}
		if time.Now().After(deadline) {
			return r, fmt.Errorf("no tailnet login within 10 minutes")
		}
		if r.AuthURL != shown {
			fmt.Fprintf(a.stderr, "potluck: log in to your tailnet to continue (waiting): %s\n", r.AuthURL)
			shown = r.AuthURL
		}
		req.AuthKey = "" // used once
		select {
		case <-ctx.Done():
			return r, ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
}

func (a *app) leaveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "leave [pool]",
		Short: "Leave a pool this machine joined (deletes its token; asks the pool's machine to revoke it)",
		Long:  "Leave leaves the pool named (or --pool), else the only pool this machine is in.",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return call(a, cmd, func(ctx context.Context, c *client.Client) (api.LeaveResponse, error) {
				pool := a.project
				if len(args) == 1 {
					pool = args[0]
				}
				if pool == "" {
					p, err := onlyPool(ctx, c)
					if err != nil {
						return api.LeaveResponse{}, err
					}
					pool = p
				}
				return c.Leave(ctx, pool)
			},
				func(r api.LeaveResponse) string {
					s := "left " + r.Project.Slug
					if !r.Revoked {
						s += " (the pool's machine did not answer: its owner can revoke this machine's token there)"
					}
					return s
				})
		},
	}
}

func (a *app) remotesCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "remotes",
		Short: "Pools this machine joined, and the machines that run them",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return call(a, cmd, func(ctx context.Context, c *client.Client) ([]api.Remote, error) { return c.Remotes(ctx) },
				func(rs []api.Remote) string {
					if len(rs) == 0 {
						return "this machine joined no pool"
					}
					var b strings.Builder
					for _, x := range rs {
						where := x.Project.Slug
						if x.Cap.Area != "" {
							where += "/" + x.Cap.Area
						}
						via := x.Via
						if via == "" {
							via = api.ViaTailnet
						}
						fmt.Fprintf(&b, "%-20s %-7s on %-12s via %-7s as %-10s %s", where, x.Cap.Role, x.Authority, via, x.Identity, x.State)
						if x.LastError != "" {
							fmt.Fprintf(&b, ": %s", config.Text(x.LastError))
						}
						if x.OwnerNow != "" {
							fmt.Fprintf(&b, "\n  WARNING: the pool's machine now belongs to tailnet owner %s, not %s as when joined", x.OwnerNow, x.AuthorityOwner)
						}
						b.WriteByte('\n')
					}
					return b.String()
				})
		},
	}
}

// poolMachines are the names of the pool's shared machines (as sandbox
// create --on takes them).
func poolMachines(ctx context.Context, c *client.Client) ([]string, error) {
	l, err := computeclient.RunTargets(ctx, c, false)
	if err != nil {
		return nil, err
	}
	names := []string{}
	for _, t := range l.Targets {
		if t.Kind == "box" {
			names = append(names, t.Name)
		}
	}
	return names, nil
}

// printPoolMachines says which machines c's pool can run sandboxes on.
func (a *app) printPoolMachines(ctx context.Context, c *client.Client) {
	names, err := poolMachines(ctx, c)
	switch {
	case err != nil:
		a.say("machines: not listed (%v)", err)
	case len(names) == 0:
		a.say("no machine is shared with the pool yet: a member shares one with potluck host")
	default:
		a.say("sandboxes can run on: %s", strings.Join(names, ", "))
	}
}

func (a *app) hostCmd() *cobra.Command {
	var s shareFlags
	cmd := &cobra.Command{
		Use:   "host ['<invite code>' | <pool>] [--image name@sha256:<digest>] [flags]",
		Short: "Share this machine with a pool: check it, join with the invite code (unless joined), and wait until the pool lists it",
		Long: `Host checks this machine (a missing root step it runs with sudo at a
terminal, which asks for your password; else, or with --no-sudo, it prints
the one sudo line), pulls the images (without --image, Potluck's default for
the layer) and runs a quick conformance check per size. Only when a size
passes does it join the pool with the invite code (or take a pool this
machine already joined, or runs) and share each size that passed; then it
waits until the pool lists them. A machine that fails never joins, and its
invite stays unspent. With no argument it shares with the only pool this
machine is in. On a Mac a sandbox ends when you touch the keyboard or pointer
(--reclaim-idle-s). potluck host stop undoes it.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			o, err := a.shareOptions(cmd, &s)
			if err != nil {
				return err
			}
			arg := a.project
			if len(args) == 1 {
				arg = args[0]
			}
			var c *client.Client
			if !o.DryRun { // a dry run never starts the daemon, nor needs the pool
				if c, _, err = a.client(); err != nil {
					return err
				}
				if arg == "" {
					if arg, err = onlyPool(cmd.Context(), c); err != nil {
						return err
					}
				}
			}
			err = a.poolHost(cmd.Context(), c, shareRunner, arg, o, a.stdout, a.rootStep(cmd.Context(), &s))
			if sc := (*spentCodeError)(nil); errors.As(err, &sc) {
				sc.Flags = shareFlagArgs(cmd)
			}
			return err
		},
	}
	s.add(cmd)
	cmd.AddCommand(a.hostStopCmd(), a.hostShelfCmd(), a.hostStatusCmd(), a.hostSetCmd())
	return cmd
}

// poolHost shares this machine with the pool. root, when set, runs the
// root step once the pool is known and the share's own checks pass.
func (a *app) poolHost(ctx context.Context, c *client.Client, run hostprobe.Runner, arg string, o hostprobe.ShareOptions, w io.Writer, root func(*hostprobe.ShareOptions) error) error {
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
		return a.reportShare(res, w)
	}
	byCode := project.LooksLikeJoinCode(arg)
	pool, machine := arg, ""
	if !byCode { // a pool named: known before anything runs
		rems, err := c.Remotes(ctx)
		if err != nil {
			return err
		}
		for _, r := range rems {
			if r.Project.Slug == pool {
				machine = r.Machine
			}
		}
		if machine == "" {
			if _, err := c.GetProject(ctx, pool); err != nil {
				return fmt.Errorf("%s is no pool this machine joined or runs: pass the invite code, potluck host '<code>'", pool)
			}
		}
	}
	// What this run replaces, in its scope.
	var l computeapi.RunTargetList
	var err error
	if byCode || machine != "" {
		l, err = computeclient.HostRunTargets(ctx, c)
	} else {
		l, err = computeclient.RunTargets(ctx, inPool(c, pool), false)
	}
	if err != nil {
		o.EarlierUnread = err.Error() // fail closed in Check
	} else {
		o.Earlier = targetsOf(l)
	}
	if err := root(&o); err != nil {
		return err
	}
	// Doctor, pulls and conformance before the join: a machine that cannot
	// share, or fails the security checks, never spends the invite.
	res, err := hostprobe.Check(ctx, run, o)
	if err != nil {
		return err
	}
	if !slices.ContainsFunc(res.Sizes, func(s hostprobe.SizeResult) bool { return s.Passed }) {
		var nb hostprobe.Binder // shares nothing: says each size is not shared
		no := o
		if !byCode {
			nb = poolBinder(c, &no, pool, machine)
		}
		res = hostprobe.Bind(ctx, nb, no, res)
		_ = a.reportShare(res, w)
		return errors.New("no size passed conformance: this machine did not join the pool")
	}
	if byCode {
		rem, err := a.poolJoin(ctx, c, arg, "")
		if err != nil {
			if ours := joinedAtAuthority(ctx, c, arg); len(ours) > 0 && api.IsCode(err, api.CodeForbidden) {
				return &spentCodeError{err: err, Pools: ours}
			}
			return err
		}
		pool, machine = rem.Project.Slug, rem.Machine
	}
	res = hostprobe.Bind(ctx, poolBinder(c, &o, pool, machine), o, res)
	if err := a.reportShare(res, w); err != nil {
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
	return awaitListed(ctx, c, pool, want, w)
}

// spentCodeError is a join code that failed on a machine that already
// joined pools at the code's machine: it names them and the command that
// shares with one.
type spentCodeError struct {
	err   error
	Pools []string
	Flags string // the flags the user passed, as they would retype them
}

func (e *spentCodeError) Unwrap() error { return e.err }

func (e *spentCodeError) Error() string {
	var b strings.Builder
	b.WriteString(e.err.Error())
	fmt.Fprintf(&b, "\nthis machine already joined %s there; to share it with one, run:", strings.Join(e.Pools, ", "))
	for _, p := range e.Pools {
		fmt.Fprintf(&b, "\n  potluck host %s%s", p, e.Flags)
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
func shareFlagArgs(cmd *cobra.Command) string {
	var b strings.Builder
	cmd.Flags().Visit(func(f *pflag.Flag) {
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

func shellQuote(v string) string {
	if v != "" && strings.Trim(v, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789@%+=:,./-_") == "" {
		return v
	}
	return "'" + strings.ReplaceAll(v, "'", `'\''`) + "'"
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

// awaitListed waits up to advertWait for the pool to list want.
func awaitListed(ctx context.Context, c *client.Client, pool string, want []string, w io.Writer) error {
	deadline := time.Now().Add(advertWait)
	for {
		names, _ := poolMachines(ctx, inPool(c, pool))
		missing := slices.DeleteFunc(slices.Clone(want), func(n string) bool { return slices.Contains(names, n) })
		if len(missing) == 0 {
			fmt.Fprintf(w, "shared with pool %s: %s\nmembers: potluck sandbox create\n", pool, strings.Join(want, ", "))
			return nil
		}
		if time.Now().After(deadline) {
			fmt.Fprintf(w, "shared, but the pool does not list %s yet: it does once this machine's heartbeat reaches the pool's machine (potluck status)\n", strings.Join(missing, ", "))
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
		Short: "Stop sharing this machine: remove every size potluck host shared from it (refused for a size with a sandbox still running, unless --force)",
		Long: `Removes every size potluck host shared from this machine. A size with
sandboxes still running on it is refused and named: end them first
(potluck sandbox rm, by the member who made them) or pass --force. With
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
			removed, err := hostStop(cmd.Context(), c, a.project, force, a.stderr)
			if a.json {
				if perr := a.printJSON(map[string]any{"removed": removed}); perr != nil {
					return perr
				}
			} else if len(removed) > 0 {
				a.say("no longer shared: %s", strings.Join(removed, ", "))
			} else if err == nil {
				a.say("this machine shares nothing")
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
func hostStop(ctx context.Context, c *client.Client, only string, force bool, warn io.Writer) (removed []string, first error) {
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
			fmt.Fprintf(warn, "potluck: %s: could not tell whether sandboxes run on it (%s); removed anyway (--force)\n", t.Name, t.Error)
		case force && len(t.Live) > 0:
			fmt.Fprintf(warn, "potluck: %s: %s still running; removed anyway (--force): they go on until their lease ends\n", t.Name, sandboxCount(len(t.Live)))
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
