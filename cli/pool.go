package cli

// Pools: create, invite, join, leave, remotes, host and host stop (copied
// from zm's cmd/zm pool.go and team.go). A pool is a team project this
// machine runs (its authority) with two areas: members join "team" and run
// sandboxes; machines that share themselves join "hosts" as guests, trusted
// (a friend) or public (a stranger) as their invite says.

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
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

func (a *app) inviteCmd() *cobra.Command {
	var host bool
	var trust, via string
	var ttl time.Duration
	var images []string
	cmd := &cobra.Command{
		Use:   "invite <name> [--host [--trust public]]",
		Short: "Print a one-time invite code for a friend: to run sandboxes, or with --host to share a machine",
		Long: `Without --host the friend joins the pool's team and runs sandboxes on its
machines. With --host they join to share a machine (` + a.cmd("host") + `), and see
nothing else of the pool. A shared machine is trusted (a friend's: sandboxes
are placed on it unasked, and it may get members' secrets and run GitHub
Actions jobs) unless --trust public (a stranger's: only sandboxes a member
names, no secrets, no Actions jobs). The trust is yours to choose here; the
machine cannot change it. Codes work once and expire (--ttl). Run it on the
machine that runs the pool, or on any machine of yours that joined it if
you own the pool there.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if trust != "" && !host {
				return errors.New("--trust goes with --host: it is how far you trust the machine they share")
			}
			if len(images) > 0 && !host {
				return errors.New("--image goes with --host: it is the image their machine shares")
			}
			for _, im := range images {
				if !hostprobe.Pinned(im) {
					return fmt.Errorf("--image %q is not pinned: want name@sha256:<64 hex digits>", im)
				}
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
			// AddArea: a pool made before it had both areas is completed
			// here, where the invite needs one.
			req := api.InviteRequest{Project: c.Project, Area: TeamArea, As: args[0], Role: api.RoleMember, Via: via,
				TTLSeconds: int64(ttl / time.Second), AddArea: true}
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
			link := "" // a link only joins: a host invite has none
			if a.host.JoinLink != "" && !host {
				link = project.JoinLink(a.host.JoinLink, r.Code)
			}
			if a.asJSON() {
				return a.printJSON(inviteAnswer{InviteResponse: r, Link: link})
			}
			until := r.Invite.Expires.Local().Format("Jan 2 15:04")
			if host {
				img := ""
				for _, im := range images {
					img += " --image " + im
				}
				a.say("host %s (one use, until %s), on the machine they share:\n  %s\n  (or %s, then %s)\n%s  their machine will be %s\n%s",
					args[0], until, friendCmd("host '"+r.Code+"'"+img), friendCmd("join '"+r.Code+"'"), friendCmd("host"+img), a.friendNote(r.Code, "host"+img),
					classMeans[req.Class], tailnetStep(args[0], r.Code))
			} else if link != "" {
				a.say("member %s (one use, until %s). Send them this link; it shows the one line that installs %s and joins:\n  %s\n"+
					"If %s is already on their machine, this does the same:\n  %s\n%s%s%s",
					args[0], until, programName, link, programName, friendCmd("join '"+r.Code+"'"), a.otherSpelling(), quickTunnelNote(r.Code), tailnetStep(args[0], r.Code))
			} else {
				a.say("member %s (one use, until %s), on their machine:\n  %s\n%s%s%s", args[0], until, friendCmd("join '"+r.Code+"'"), a.friendNote(r.Code, ""),
					quickTunnelNote(r.Code), tailnetStep(args[0], r.Code))
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&host, "host", false, "the friend shares a machine with the pool")
	cmd.Flags().StringVar(&trust, "trust", "", "with --host: trusted (default) or public")
	cmd.Flags().StringVar(&via, "via", "", "tailnet or tunnel (default: the tailnet when it runs)")
	cmd.Flags().DurationVar(&ttl, "ttl", 24*time.Hour, "how long the code works")
	cmd.Flags().StringArrayVar(&images, "image", nil, "with --host: the image, name@sha256:<digest>, their machine shares, written into the command printed for them (repeat; default: none, so it shares the default)")
	return cmd
}

// inviteAnswer is invite's --json: the pool's answer and, for a host with a
// join page, the invite link.
type inviteAnswer struct {
	api.InviteResponse
	Link string `json:"link,omitempty"`
}

// quickTunnelNote warns that a quick tunnel's address changes when
// cloudflared restarts (README): its codes then stop working.
func quickTunnelNote(code string) string {
	u, err := url.Parse(code)
	if err != nil || !strings.HasSuffix(u.Hostname(), ".trycloudflare.com") {
		return ""
	}
	return "  (the pool's tunnel is a quick one: its address changes when cloudflared restarts, and this invite then stops working)\n"
}

// installJoin installs Potluck and redeems code in one line (install.sh
// --join): all a friend's machine needs.
func installJoin(code string) string {
	return "curl -fsSL https://potluck.sh/install.sh | sh -s -- --join '" + code + "'"
}

// friendCmd is a command for a friend's machine, which needs only Potluck:
// the potluck program's spelling, whatever program printed the invite.
func friendCmd(words string) string { return programName + " " + words }

// friendNote is the lines under a friend's command: how to install Potluck
// and join in one line (then the command then names, if any), and
// otherSpelling.
func (a *app) friendNote(code, then string) string {
	n := "  no " + programName + " there yet: " + installJoin(code)
	if then != "" {
		n += ", then " + friendCmd(then)
	}
	return n + "\n" + a.otherSpelling()
}

// otherSpelling is the line naming this program's spelling of a friend's
// command where it is not potluck (a friend who runs it types the same
// words after its prefix), else "".
func (a *app) otherSpelling() string {
	if p := prefix(a.host.Prefix); string(p) != programName {
		return "  (where they run " + p.name() + ": " + p.cmd("<the same words>") + ")\n"
	}
	return ""
}

// tailnetStep is what the pool's owner does so a friend's machine reaches
// the pool's machine (the one that runs it, which the code names: this one,
// or the authority of a pool this machine joined) with a tailnet code, in
// plain words; "" for a tunnel code.
func tailnetStep(name, code string) string {
	if strings.HasPrefix(code, "https://") || strings.HasPrefix(code, "http://") {
		return ""
	}
	node := "the node of the machine that runs the pool" // its name in the admin console: the code's node name
	if jc, err := project.ParseJoinCode(code); err == nil {
		prefix := jc.NodePrefix
		if prefix == "" {
			prefix = api.NodePrefix
		}
		node = prefix + jc.Machine
	}
	return fmt.Sprintf("  before %s runs it, their machine must reach the machine that runs the pool over Tailscale: if they are not on your tailnet,\n"+
		"  share that machine with them at https://login.tailscale.com/admin/machines (%s, then the ... menu, Share)\n"+
		"  and send them the link, or invite them to your tailnet at https://login.tailscale.com/admin/users", name, node)
}

func (a *app) joinCmd() *cobra.Command {
	var authKey string
	cmd := &cobra.Command{
		Use:   "join '<invite link or code>'",
		Short: "Join a pool with the invite link or code its owner sent",
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
			if a.asJSON() {
				return a.printJSON(rem)
			}
			if rem.Cap.Area == HostsArea {
				a.say("next: %s, to share this machine with %s", a.cmd("host"), rem.Project.Slug)
				return nil
			}
			a.printPoolMachines(cmd.Context(), inPool(c, rem.Project.Slug))
			a.say("next: %s, then %s", a.cmd("sandbox create"), a.cmd("sandbox exec <sandbox> -- uname -a"))
			return nil
		},
	}
	cmd.Flags().StringVar(&authKey, "auth-key", "", "a Tailscale auth key for this machine's node (else a login URL is printed); never saved")
	return cmd
}

// poolJoin redeems an invite code.
func (a *app) poolJoin(ctx context.Context, c *client.Client, code, authKey string) (api.Remote, error) {
	if !project.LooksLikeJoinCode(code) {
		return api.Remote{}, fmt.Errorf("%q is not an invite code: the pool's owner prints one with %s", code, a.cmd("invite <you>"))
	}
	req := api.JoinRequest{Code: strings.TrimSpace(code), AuthKey: authKey, WaitMS: 10000}
	req.Cwd, _ = os.Getwd()
	r, err := a.joinUntilDone(ctx, c, req)
	var bad *api.Error
	if errors.As(err, &bad) && bad.Code == project.ErrBadCode.Code && bad.Message == project.ErrBadCode.Message {
		// kit's hint names zm's team invite; a pool's is invite.
		return api.Remote{}, bad.WithHint("ask the pool's owner for a new code: " + a.cmd("invite <you>"))
	}
	if err != nil {
		return api.Remote{}, err
	}
	if r.Remote == nil {
		return api.Remote{}, errors.New("nothing joined")
	}
	x := r.Remote
	if !a.asJSON() {
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
			fmt.Fprintf(a.host.Stderr, "%s: log in to your tailnet to continue (waiting): %s\n", a.name(), r.AuthURL)
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
				pool := a.pool()
				if len(args) == 1 {
					pool = args[0]
				}
				if pool == "" {
					p, err := a.onlyPool(ctx, c)
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
the layer) and runs a quick conformance check per size. Only when a size
passes does it join the pool with the invite code (or take a pool this
machine already joined, or runs) and share each size that passed; then it
waits until the pool lists them. A machine that fails never joins, and its
invite stays unspent. With no argument it shares with the only pool this
machine is in. On a Mac a sandbox ends when you touch the keyboard or pointer
(--reclaim-idle-s). ` + a.cmd("host stop") + ` undoes it.

Run again where this machine already shares sizes with the pool and with no
flag that changes the share (--image, --sizes, --docker and the like), host
only prints what it shares and what the run would do, changes nothing, and
asks for --yes; ` + a.cmd("host status") + ` is how to look.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			arg := a.pool()
			if len(args) == 1 {
				arg = args[0]
			}
			return a.runHost(cmd, &s, arg, !yes && !a.shapesShare(cmd))
		},
	}
	s.add(cmd, a)
	cmd.Flags().BoolVar(&yes, "yes", false, "share again where this machine already shares sizes with the pool, with no flag that changes the share (such a run otherwise only says what it would do)")
	cmd.AddCommand(a.hostStopCmd(), a.hostShelfCmd(), a.hostStatusCmd(), a.hostSetCmd(), a.hostPauseCmd(), a.hostResumeCmd())
	cmd.AddCommand(a.hostOldSpellings()...)
	return cmd
}

// noSizePassed is host's answer when every size failed its checks: what
// happened, and what to do next in plain words. The checks run after the
// pulls, and a pulled image stays on this machine, so a second run starts at
// the checks; a check can fail once on a busy or just-booted machine.
func (a *app) noSizePassed(byCode bool) error {
	unspent := ""
	if byCode {
		unspent = ", and the invite is not spent"
	}
	return fmt.Errorf("no size passed conformance: this machine did not join the pool%s.\n"+
		"Next: run the same %s command again. The images it pulled stay on this machine, so it starts at the checks, not the download.\n"+
		"If the same check fails again, it is not a one-off: keep the output above, which names the check that failed.", unspent, a.cmd("host"))
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
	// Doctor, pulls and conformance before the join: a machine that cannot
	// share, or fails the security checks, never spends the invite.
	stopBeat := startBeat(&o)
	res, err := hostprobe.Check(ctx, run, o)
	stopBeat()
	if err != nil {
		return err
	}
	byCode := sc.byCode
	if !slices.ContainsFunc(res.Sizes, func(s hostprobe.SizeResult) bool { return s.Passed }) {
		var nb hostprobe.Binder // shares nothing: says each size is not shared
		no := o
		if !byCode {
			nb = poolBinder(c, &no, pool, machine)
		}
		res = hostprobe.Bind(ctx, nb, no, res)
		_ = a.reportShare(res, w)
		return a.noSizePassed(byCode)
	}
	if byCode {
		rem, err := a.poolJoin(ctx, c, arg, "")
		if err != nil {
			if ours := joinedAtAuthority(ctx, c, arg); len(ours) > 0 && api.IsCode(err, api.CodeForbidden) {
				return &spentCodeError{err: err, Pools: ours, p: prefix(a.host.Prefix)}
			}
			return err
		}
		pool, machine = rem.Project.Slug, rem.Machine
	}
	res = hostprobe.Bind(ctx, poolBinder(c, &o, pool, machine), o, res)
	if err := a.reportShare(res, w); err != nil {
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
	if err := awaitListed(ctx, c, pool, want, res.RunnerDir != "", lw, prefix(a.host.Prefix)); err != nil {
		return err
	}
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
				w := io.Writer(words{a.host.Stdout})
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
