package cli

// A pool's members: invite, join, leave and remotes. The pool itself (create,
// its areas, host and host stop) is pool.go.

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/sengirunners/potluck/hostprobe"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/project"
)

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
you own the pool there.

The code is a secret until it is used: send it to that friend alone, with
the one line printed for them (the whole code, in single quotes), never to
a group chat or a support thread.`,
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
					img += " --image " + shellQuote(im)
				}
				run := "host " + singleQuoted(r.Code) + img
				a.say("host %s (one use, until %s), on the machine they share, as one line (all of it, quotes included):\n  %s\n  (or %s, then %s)\n%s  their machine will be %s\n%s",
					args[0], until, friendCmd(run), friendCmd("join "+singleQuoted(r.Code)), friendCmd("host"+img), a.friendNote(r.Code, run, "host"+img),
					classMeans[req.Class], tailnetStep(args[0], r.Code))
			} else if link != "" {
				a.say("member %s (one use, until %s). Send them this link; it shows the one line that installs %s and joins:\n  %s\n"+
					"If %s is already on their machine, this does the same:\n  %s\n%s%s%s",
					args[0], until, programName, link, programName, friendCmd("join "+singleQuoted(r.Code)), a.otherSpelling("join "+singleQuoted(r.Code)), quickTunnelNote(r.Code), tailnetStep(args[0], r.Code))
			} else {
				run := "join " + singleQuoted(r.Code)
				a.say("member %s (one use, until %s), on their machine, as one line (all of it, quotes included):\n  %s\n%s%s%s", args[0], until, friendCmd(run), a.friendNote(r.Code, run, ""),
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
	return "curl -fsSL https://potluck.sh/install.sh | sh -s -- --join " + singleQuoted(code)
}

// friendCmd is a command for a friend's machine, which needs only Potluck:
// the potluck program's spelling, whatever program printed the invite.
func friendCmd(words string) string { return programName + " " + words }

// friendNote is the lines under a friend's command run: how to install
// Potluck and join in one line (then the command then names, if any), and
// otherSpelling of run.
func (a *app) friendNote(code, run, then string) string {
	n := "  no " + programName + " there yet: " + installJoin(code)
	if then != "" {
		n += ", then " + friendCmd(then)
	}
	return n + "\n" + a.otherSpelling(run)
}

// otherSpelling is the line that gives a friend's command words whole in
// this program's spelling where it is not potluck (a friend who runs it
// copies one line, never rebuilds one), else "".
func (a *app) otherSpelling(words string) string {
	if p := prefix(a.host.Prefix); string(p) != programName {
		return "  where they run " + p.name() + " instead, the same as one line: " + p.cmd(words) + "\n"
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
		Long: `Join takes the invite whole, as one word in single quotes, as the pool's
owner's ` + a.cmd("invite") + ` printed it. An invite cut short or changed by a copy
is refused before anything runs, saying what is wrong but never repeating
it: an invite is a secret until it is used.`,
		Args: a.pastedArgs(1, 1, "one argument, the invite link or code in single quotes"),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := a.checkInvite(args[0], "invite <you>"); err != nil {
				return err
			}
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
	pastedFlags(cmd)
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
