package cli

// A pool's members: invite, join, leave and remotes. The pool itself (create,
// its areas, host and host stop) is pool.go; the same commands' forms for
// any team project (invite --as, invite ls and rm, a join by name) are
// team.go.

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
	"github.com/sengirunners/potluck/kit/project"
)

func (a *app) inviteCmd() *cobra.Command {
	var host, allowTagged bool
	var trust, via, as, role string
	var ttl time.Duration
	var images []string
	cmd := &cobra.Command{
		Use:   "invite <name> [--host [--trust public]] | <project>[/<area>] --as <name>",
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

With --as the argument is a team project, or one of its areas, instead of
a friend's name: the code admits a machine there as the identity --as
names, with --role (member or guest) and, with --trust, how far that
machine is trusted (default: trusted for a member of the whole project,
public otherwise). A pool is a team project too; its own invites are the
form without --as.

The code is a secret until it is used: send it to that friend alone, with
the one line printed for them (the whole code, in single quotes), never to
a group chat or a support thread.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if as != "" {
				if host || len(images) > 0 {
					return exitWith{2, errors.New("--as invites into a team project; --host and --image are a pool's: leave them out, or leave out --as")}
				}
				if role == "" {
					role = api.RoleMember
				}
				switch trust {
				case "", api.MachineTrusted, api.MachinePublic:
				default:
					return fmt.Errorf("--trust is trusted or public, not %q", trust)
				}
				return a.inviteProject(cmd.Context(), projectInvite{where: args[0], as: as, role: role, trust: trust, via: via, ttl: ttl, allowTagged: allowTagged})
			}
			if role != "" {
				return exitWith{2, errors.New("--role goes with --as: a pool's invite makes a member, or with --host a machine that shares")}
			}
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
			if err := a.isPool(cmd.Context(), c, args[0]); err != nil {
				return err
			}
			// AddArea: a pool made before it had both areas is completed
			// here, where the invite needs one.
			req := api.InviteRequest{Project: c.Project, Area: TeamArea, As: args[0], Role: api.RoleMember, Via: via,
				TTLSeconds: int64(ttl / time.Second), AddArea: true, AllowTagged: allowTagged}
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
			if d := routesDown(r); d != "" {
				a.say("%s", strings.TrimSuffix(d, "\n"))
			}
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
	cmd.Flags().StringVar(&trust, "trust", "", "with --host: trusted (default) or public; with --as: trusted or public (default: trusted for a member of the whole project, public otherwise)")
	cmd.Flags().StringVar(&via, "via", "", "tailnet or tunnel (default: the tunnel while it answers as this machine, else the tailnet)")
	cmd.Flags().DurationVar(&ttl, "ttl", 24*time.Hour, "how long the code works")
	cmd.Flags().StringArrayVar(&images, "image", nil, "with --host: the image, name@sha256:<digest>, their machine shares, written into the command printed for them (repeat; default: none, so it shares the default)")
	cmd.Flags().StringVar(&as, "as", "", "the argument is a team project (or <project>/<area>): the identity the machine joins it as (made when it redeems the code, if new)")
	cmd.Flags().StringVar(&role, "role", "", "with --as: member (default) or guest")
	cmd.Flags().BoolVar(&allowTagged, "allow-tagged", false, "a tagged tailnet node (a CI or farm machine) may redeem the code")
	cmd.AddCommand(a.inviteSubCmds()...)
	return cmd
}

// isPool refuses a pool invite into a team project that is not a pool (it
// has neither of a pool's areas): one that would make a pool's area there.
// The project form (--as) is how to invite into it. A project with no
// record yet is the invite's to make or refuse; any other error stops it.
func (a *app) isPool(ctx context.Context, c *client.Client, name string) error {
	p, err := c.GetProject(ctx, c.Project)
	var e *api.Error
	if errors.As(err, &e) && e.Code == api.CodeNotFound {
		return nil
	}
	if err != nil {
		return err
	}
	for _, x := range p.Areas {
		if x.Name == TeamArea || x.Name == HostsArea {
			return nil
		}
	}
	return exitWith{2, api.Errorf(api.CodeBadRequest, "%s is a team project, not a pool (it has no %s or %s area)", p.Slug, TeamArea, HostsArea).
		WithHint("invite a machine into it as an identity: " + a.cmd("invite "+p.Slug+" --as "+shellQuote(name)))}
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
	return installScript + " --join " + singleQuoted(code)
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
	var f joinFlags
	cmd := &cobra.Command{
		Use:   "join '<invite link or code>' | <project>[/<area>]",
		Short: "Join a pool with the invite link or code its owner sent",
		Long: `Join takes the invite whole, as one word in single quotes, as the pool's
owner's ` + a.cmd("invite") + ` printed it. An invite cut short or changed by a copy
is refused before anything runs, saying what is wrong but never repeating
it: an invite is a secret until it is used.

A team project's name instead of an invite joins it with no code, where
the program that runs it grants GitHub accounts and yours has a grant
there (Potluck alone makes no such grants): that machine asks GitHub to
admit you (open the link printed and enter the code). <project>/<area>
with --from joins one area of it, over https too.

--github with an invite also signs you in with GitHub at the project's
machine once the invite is redeemed: open the link printed and enter the
code. Your identity there is then linked to your GitHub account, so a
grant its owner makes to your GitHub login (` + a.cmd("grant") + ` @<login> ...)
applies to you as it does to a GitHub join. It is optional: declined,
expired or with no grant yet, the invite's join goes on as it is.

--json prints what was joined (the remote); a join by name that needs
you first prints that first answer instead (the machines found, or the
code to enter). A tailnet login is waited for either way, its link on
stderr.`,
		Args: a.pastedArgs(1, 1, "one argument, the invite link or code in single quotes"),
		RunE: func(cmd *cobra.Command, args []string) error {
			req, err := a.joinRequest(args[0], f)
			if err != nil {
				return err
			}
			c, _, err := a.client()
			if err != nil {
				return err
			}
			r, err := a.join(cmd.Context(), c, req, nil)
			if err != nil {
				return err
			}
			if r.Remote == nil || (a.asJSON() && r.GitHub != nil) {
				if a.asJSON() { // a join waiting for a person (by name, or --github's sign-in): the first answer
					return a.printJSON(r)
				}
				return errors.New("nothing joined")
			}
			rem := *r.Remote
			if a.asJSON() {
				return a.printJSON(rem)
			}
			switch rem.Cap.Area {
			case HostsArea:
				a.say("next: %s, to share this machine with %s", a.cmd("host"), rem.Project.Slug)
			case TeamArea:
				a.printPoolMachines(cmd.Context(), inPool(c, rem.Project.Slug))
				a.say("next: %s, then %s", a.cmd("sandbox create"), a.cmd("sandbox exec <sandbox> -- uname -a"))
			default:
				a.projectJoined(r)
			}
			return nil
		},
	}
	f.add(cmd)
	pastedFlags(cmd)
	return cmd
}

// join sends req until it is done (joinUntilDone) and says what it joined.
// Under --json a join by name answers as soon as it needs a person.
// onLogin, when set, is told each new tailnet login URL the join waits for.
func (a *app) join(ctx context.Context, c *client.Client, req api.JoinRequest, onLogin func(url string)) (api.JoinResponse, error) {
	r, err := a.joinUntilDone(ctx, c, req, onLogin)
	var bad *api.Error
	if errors.As(err, &bad) && bad.Code == project.ErrBadCode.Code && bad.Message == project.ErrBadCode.Message {
		// kit's hint names only a team project's invite; a code may be a pool's.
		return r, bad.WithHint("ask the owner for a new code: " + a.newCode())
	}
	if err != nil || r.Remote == nil || a.asJSON() {
		return r, err
	}
	x := r.Remote
	kind, where := "project", x.Project.Slug
	if x.Cap.Area == TeamArea || x.Cap.Area == HostsArea {
		kind = "pool"
	} else if x.Cap.Area != "" {
		where += "/" + x.Cap.Area
	}
	a.say("joined %s %s on %s as %s (%s), machine %s", kind, where, x.Authority, x.Identity, x.Cap.Role, x.Machine)
	if r.SignIn != "" {
		a.say("  GitHub: %s", r.SignIn)
	}
	return r, nil
}

// poolJoin redeems an invite code (host's '<invite code>'); onLogin is
// join's.
func (a *app) poolJoin(ctx context.Context, c *client.Client, code, authKey string, onLogin func(url string)) (api.Remote, error) {
	if !project.LooksLikeJoinCode(code) {
		return api.Remote{}, fmt.Errorf("%q is not an invite code: the pool's owner prints one with %s", code, a.cmd("invite <you>"))
	}
	req := api.JoinRequest{Code: strings.TrimSpace(code), AuthKey: authKey, WaitMS: 10000}
	req.Cwd, _ = os.Getwd()
	r, err := a.join(ctx, c, req, onLogin)
	if err != nil {
		return api.Remote{}, err
	}
	if r.Remote == nil {
		return api.Remote{}, errors.New("nothing joined")
	}
	return *r.Remote, nil
}

// joinUntilDone sends the join, waiting out a tailnet login and, for a
// join by name, asking the person for the authority and GitHub's code
// (askForJoin; with --json it answers the first such answer instead).
// Each new login URL goes to stderr and to onLogin, when set.
func (a *app) joinUntilDone(ctx context.Context, c *client.Client, req api.JoinRequest, onLogin func(url string)) (api.JoinResponse, error) {
	deadline := time.Now().Add(10 * time.Minute)
	shown := ""
	for {
		r, err := c.Join(ctx, req)
		if err != nil {
			return r, err
		}
		if r.AuthURL == "" {
			if a.asJSON() {
				return r, nil
			}
			next, again, err := a.askForJoin(req, r)
			if err != nil || !again {
				return r, err
			}
			req = next
			continue
		}
		if time.Now().After(deadline) {
			return r, fmt.Errorf("no tailnet login within 10 minutes")
		}
		if r.AuthURL != shown {
			fmt.Fprintf(a.host.Stderr, "%s: log in to your tailnet to continue (waiting): %s\n", a.name(), r.AuthURL)
			shown = r.AuthURL
			if onLogin != nil {
				onLogin(r.AuthURL)
			}
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
		Long:  "Leave leaves the pool (or team project) named, or --pool, else the only pool this machine is in.",
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
		Short: "Pools (and team projects) this machine joined, and the machines that run them",
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
							fmt.Fprintf(&b, ": %s", x.LastError)
						}
						if x.OwnerNow != "" {
							fmt.Fprintf(&b, "\n  WARNING: the machine that runs it now belongs to tailnet owner %s, not %s as when joined", x.OwnerNow, x.AuthorityOwner)
						}
						b.WriteString(remoteCopy(x))
						b.WriteByte('\n')
					}
					return b.String()
				})
		},
	}
}
