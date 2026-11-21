package cli

// A team project's members (kit's teams; a pool is a team project with two
// areas, pool_members.go): the project form of invite (invite
// <project>[/<area>] --as <name>), invite ls and rm, a join by the
// project's name (a GitHub login at its authority, no code), what join
// prints for a project that is not a pool, and a remote's copy in remotes.
// A program built on Potluck whose daemon serves team projects (kit's
// teamlink and owners modules) has these commands from Commands, not
// copies of its own.

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/project"
)

// projectInvite is invite's project form: where, as whom, and how far the
// machine that redeems it is trusted.
type projectInvite struct {
	where       string // <project> or <project>/<area>
	as, role    string
	trust, via  string
	ttl         time.Duration
	allowTagged bool
}

// inviteProject prints a one-time code that admits a machine into a team
// project, or one of its areas, as an identity (kit's invite): what
// invite's --as runs.
func (a *app) inviteProject(ctx context.Context, in projectInvite) error {
	slug, area, _ := strings.Cut(in.where, "/")
	if !api.ValidSlug(slug) || area != "" && !api.ValidSlug(area) {
		return exitWith{2, fmt.Errorf("with --as, invite takes <project> or <project>/<area>, not %q", in.where)}
	}
	switch in.role {
	case api.RoleMember, "guest":
	default:
		return exitWith{2, fmt.Errorf("--role is member or guest, not %q", in.role)}
	}
	c, _, err := a.client()
	if err != nil {
		return err
	}
	req := api.InviteRequest{Project: slug, Area: area, As: in.as, Role: in.role, Via: in.via, Class: in.trust,
		TTLSeconds: int64(in.ttl / time.Second), AllowTagged: in.allowTagged}
	r, err := c.Invite(ctx, req)
	if err != nil {
		return err
	}
	if a.asJSON() {
		return a.printJSON(inviteAnswer{InviteResponse: r})
	}
	where := r.Invite.Project
	if r.Invite.Area != "" {
		where += "/" + r.Invite.Area
	}
	how := "logged in to the same tailnet"
	if strings.HasPrefix(r.Code, "https://") || strings.HasPrefix(r.Code, "http://") {
		how = "no tailnet needed"
	}
	class := ""
	if r.Invite.Class != "" {
		class = ", its machine " + r.Invite.Class
	}
	a.say("%sinvite %s: %s as %s in %s%s, until %s\n\n  %s\n\non %s's machine (%s), as one line (all of it, quotes included):\n  %s",
		routesDown(r), r.Invite.ID, r.Invite.As, r.Invite.Role, where, class, r.Invite.Expires.Local().Format("Jan 2 15:04"), singleQuoted(r.Code),
		r.Invite.As, how, a.cmd("join "+singleQuoted(r.Code)))
	return nil
}

// routesDown says which routes set up here the code does not use because
// they do not work now (InviteResponse.NotUsed), and which route it uses
// instead, as lines before the invite; "" when it uses the first.
func routesDown(r api.InviteResponse) string {
	if len(r.NotUsed) == 0 {
		return ""
	}
	var b strings.Builder
	for _, d := range r.NotUsed {
		b.WriteString("not by the " + d.Via + ": " + d.Reason + "\n")
		if d.Hint != "" {
			b.WriteString("  to fix it: " + d.Hint + "\n")
		}
	}
	if strings.HasPrefix(r.Code, "https://") || strings.HasPrefix(r.Code, "http://") {
		b.WriteString("so this code is for the tunnel instead.\n\n")
	} else {
		b.WriteString("so this code is for the tailnet instead: it works only for a machine on your tailnet (or one you share this machine with).\n\n")
	}
	return b.String()
}

// inviteSubCmds are invite's ls and rm: the invites this machine made.
func (a *app) inviteSubCmds() []*cobra.Command {
	ls := &cobra.Command{
		Use:   "ls",
		Short: "Invites this machine made (live, redeemed, expired)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return call(a, cmd, func(ctx context.Context, c *client.Client) ([]api.Invite, error) { return c.Invites(ctx) },
				func(is []api.Invite) string {
					var b strings.Builder
					for _, i := range is {
						b.WriteString(inviteLine(i))
					}
					return strings.TrimSuffix(b.String(), "\n")
				})
		},
	}
	rm := &cobra.Command{
		Use:   "rm <id>",
		Short: "Remove an invite (a machine that already redeemed it keeps its token)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return call(a, cmd, func(ctx context.Context, c *client.Client) (api.Invite, error) { return c.RemoveInvite(ctx, args[0]) },
				func(i api.Invite) string { return "removed invite " + i.ID })
		},
	}
	return []*cobra.Command{ls, rm}
}

// inviteLine is one invite in invite ls: where it admits, until when (or
// the machine that redeemed it, and whether that machine still holds a live
// token there), and its class.
func inviteLine(i api.Invite) string {
	where := i.Project
	if i.Area != "" {
		where += "/" + i.Area
	}
	extra := "until " + i.Expires.Local().Format("Jan 2 15:04")
	if i.State == api.InviteRedeemed {
		extra = "by " + i.Machine
		switch {
		case i.Revoked:
			extra += " (left: no live token now)"
		case i.Used:
			extra += " (in use)"
		}
	}
	if i.Class != "" {
		extra += ", " + i.Class
	}
	return fmt.Sprintf("%s  %-8s %-10s %-7s %-20s %s\n", i.ID, i.State, i.As, i.Role, where, extra)
}

// joinFlags are join's flags beyond the invite: for a join by name (from,
// role), this machine's name and checkouts there, and a tunnel home behind
// Cloudflare Access.
type joinFlags struct {
	authKey, from, role, machine string
	paths                        []string
	wait                         time.Duration
	accessID, accessSecretFile   string
	github                       bool
}

func (f *joinFlags) add(cmd *cobra.Command) {
	fl := cmd.Flags()
	fl.StringVar(&f.authKey, "auth-key", "", "a Tailscale auth key for this machine's node (else a login URL is printed); never saved")
	fl.StringVar(&f.from, "from", "", "with a project's name: the machine that runs it (asked for on a terminal; required elsewhere), or its tunnel URL with its key fingerprint, https://<host>#<fingerprint>, to join over https with no tailnet. --json prints the first answer: the machines found, or the code to enter")
	fl.StringVar(&f.role, "role", "", "with <project>/<area>: the most you ask for (guest, member or owner on the tailnet; over https only guest), when your GitHub grants give it")
	fl.StringVar(&f.machine, "machine", "", "this machine's name at the project's machine (default: this daemon's machine name)")
	fl.StringSliceVar(&f.paths, "path", nil, "your checkout of the project (repeatable; default: this repo when its remote is the project's)")
	fl.DurationVar(&f.wait, "wait", 10*time.Second, "how long to wait for the tailnet node each try")
	fl.StringVar(&f.accessID, "cf-access-id", "", "a tunnel home behind Cloudflare Access: the service token's client id")
	fl.StringVar(&f.accessSecretFile, "cf-access-secret-file", "", "a file holding that service token's client secret")
	fl.BoolVar(&f.github, "github", false, "with an invite: also sign in with GitHub at the project's machine (open the link printed and enter the code), so grants its owner makes to your GitHub login apply to you; optional, and the invite works without it")
}

// joinRequest is the join arg asks for: an invite code or link, a
// project's name (a GitHub join at its authority), or <project>/<area>
// with --from (a GitHub join scoped to one area).
func (a *app) joinRequest(arg string, f joinFlags) (api.JoinRequest, error) {
	req := api.JoinRequest{AuthKey: f.authKey, From: f.from, Role: f.role, Machine: f.machine,
		WaitMS: int(f.wait / time.Millisecond), AccessID: f.accessID, GitHub: f.github}
	arg = strings.TrimSpace(arg)
	switch proj, area, ok := strings.Cut(arg, "/"); {
	case ok && !strings.Contains(arg, ":"):
		if !api.ValidSlug(proj) || !api.ValidSlug(area) {
			return req, exitWith{2, fmt.Errorf("join: that is not <project>/<area>")}
		}
		if req.From == "" {
			return req, exitWith{2, fmt.Errorf("join <project>/<area> names an area: also say where, --from https://<host>#<fingerprint> (or --from <machine> on the tailnet)")}
		}
		req.Project, req.Area = proj, area
	case project.LooksLikeJoinCode(arg):
		if err := a.checkInvite(arg, a.newCode()); err != nil {
			return req, err
		}
		req.Code = arg
	default:
		req.Project = arg
	}
	if req.GitHub && req.Code == "" {
		return req, exitWith{2, fmt.Errorf("join: --github goes with an invite link or code; joining by a project's name already signs in with GitHub")}
	}
	if f.accessSecretFile != "" {
		b, err := os.ReadFile(f.accessSecretFile)
		if err != nil {
			return req, err
		}
		req.AccessSecret = strings.TrimSpace(string(b))
	}
	for _, p := range f.paths {
		abs, err := filepath.Abs(p) // the daemon runs in another directory
		if err != nil {
			return req, err
		}
		req.Paths = append(req.Paths, abs)
	}
	req.Cwd, _ = os.Getwd()
	return req, nil
}

// askForJoin is the person's part of a join with no code, in joinUntilDone:
// choosing the authority when several answer, and GitHub's code. It
// answers the request to send next, or false when there is nothing to ask.
func (a *app) askForJoin(req api.JoinRequest, r api.JoinResponse) (api.JoinRequest, bool, error) {
	if len(r.Authorities) > 0 && req.From == "" {
		from, err := a.pickAuthority(req.Project, r, onATerminal(), a.host.Stdin, a.host.Stderr)
		if err != nil {
			return req, false, err
		}
		req.From = from
		return req, true, nil
	}
	if g := r.GitHub; g != nil {
		if req.Login == "" {
			a.showSkipped(a.host.Stderr, r.Skipped)
			who := "tailnet owner " + g.Owner
			switch {
			case g.Owner == "" && req.Code != "":
				who = "the invite's home"
			case g.Owner == "":
				_, fp, _ := strings.Cut(req.From, "#")
				who = "home key " + fp
			}
			n, until := a.name(), g.Expires.Local().Format("15:04")
			same := ""
			if g.Resumed {
				same = " (the same code as before)"
			}
			fmt.Fprintf(a.host.Stderr, "%s: authority %s (%s) asks GitHub to admit %s.\n%s: open %s and enter %s%s (waiting, until %s; if this command stops, this machine's daemon goes on with the join: run it again to see it).\n%s: enter a code only if you started this yourself.\n",
				n, g.Authority, who, g.For, n, g.VerificationURI, g.UserCode, same, until, n)
		}
		if note := strings.Join(r.Notes, "; "); req.Login != "" && note != a.joinNote {
			if note != "" { // while it waits: an authority that does not answer
				fmt.Fprintf(a.host.Stderr, "%s: %s\n", a.name(), note)
			}
			a.joinNote = note
		}
		req.Login = g.Login
		return req, true, nil
	}
	return req, false, nil
}

// showSkipped lists the machines a join with no code did not use, and why.
func (a *app) showSkipped(w io.Writer, skipped []api.JoinSkipped) {
	for _, k := range skipped {
		fmt.Fprintf(w, "%s: skipped %s: %s\n", a.name(), k.Machine, k.Reason)
	}
}

// pickAuthority is the person's choice of authority for a join with no
// code: any machine can claim to be one, so even a sole answer is shown
// with its node's tailnet owner and confirmed on a terminal; elsewhere
// --from must name it.
func (a *app) pickAuthority(proj string, r api.JoinResponse, terminal bool, in io.Reader, msg io.Writer) (string, error) {
	a.showSkipped(msg, r.Skipped)
	var names []string
	for _, x := range r.Authorities {
		names = append(names, fmt.Sprintf("%s (tailnet owner %s)", x.Machine, x.Owner))
	}
	if len(r.Authorities) != 1 || !terminal {
		return "", fmt.Errorf("machines that run GitHub logins for %s: %s; choose one: %s", proj, strings.Join(names, ", "), a.cmd("join "+proj+" --from <machine>"))
	}
	x := r.Authorities[0]
	fmt.Fprintf(msg, "%s: join %s at %s (tailnet owner %s)? [y/N] ", a.name(), proj, x.Machine, x.Owner)
	line, _ := bufio.NewReader(in).ReadString('\n')
	if ans := strings.ToLower(strings.TrimSpace(line)); ans != "y" && ans != "yes" {
		return "", errors.New("not joined")
	}
	return x.Machine, nil
}

// projectJoined is what join prints for a project that is not a pool's
// area: what this machine sees there, and the host program's next step.
func (a *app) projectJoined(r api.JoinResponse) {
	x := r.Remote
	var b strings.Builder
	if len(x.Areas) > 0 {
		fmt.Fprintf(&b, "  areas you see: %s\n", strings.Join(x.Areas, ", "))
	}
	if r.Items > 0 {
		fmt.Fprintf(&b, "  open work items you see: %d\n", r.Items)
	}
	if len(x.Paths) > 0 {
		fmt.Fprintf(&b, "  checkouts: %s\n", strings.Join(x.Paths, ", "))
	}
	fmt.Fprintf(&b, "  token: %s (works only from this machine's daemon)\n", x.TokenFile)
	for _, n := range r.Notes {
		fmt.Fprintf(&b, "  note: %s\n", n)
	}
	a.say("%s", strings.TrimSuffix(b.String(), "\n"))
	if a.host.AfterJoin != nil {
		a.host.AfterJoin(a.host.Stdout, r)
	}
}

// newCode is the owner's commands for a new code, for a join whose code
// failed: join cannot tell a pool's code from a team project's.
func (a *app) newCode() string {
	return a.cmd("invite <you>") + " for a pool, " + a.cmd("invite <project> --as <you>") + " for a team project"
}

// remoteCopy is a remote's local copy of its project, for remotes: its
// state, how old it is, and the writes it holds for the authority; "" for
// a remote with none.
func remoteCopy(x api.Remote) string {
	var b strings.Builder
	if x.Unsent != "" {
		fmt.Fprintf(&b, "\n  %d unsent writes saved in %s (re-send after joining again)", x.UnsentCount, x.Unsent)
	}
	v := x.Replica
	if v == nil {
		return b.String()
	}
	fmt.Fprintf(&b, "\n  copy %s", v.State)
	if v.Synced != nil {
		fmt.Fprintf(&b, ", synced %s ago", time.Duration(v.AgeSecs*float64(time.Second)).Round(time.Second))
	}
	if !v.Reachable {
		b.WriteString(", authority unreachable")
	}
	if v.Pending > 0 || v.Conflicts > 0 {
		fmt.Fprintf(&b, ", outbox %d pending", v.Pending)
		if v.Conflicts > 0 {
			fmt.Fprintf(&b, ", %d refused", v.Conflicts)
			if !config.Missing("outbox") {
				fmt.Fprintf(&b, " (%s)", config.Command("outbox"))
			}
		}
	}
	if v.LastError != "" && x.LastError == "" {
		fmt.Fprintf(&b, ": %s", v.LastError)
	}
	return b.String()
}
