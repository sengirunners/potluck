package cli

// potluck disband: remove one pool this machine runs (made by create) and
// keep the rest of Potluck: this machine's identity, its other pools, the
// pools it joined and its daemon. uninstall removes everything and leave
// removes a pool someone else runs; nothing else ends a pool.

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/computeclient"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
)

type disbandFlags struct{ yes, dryRun, force bool }

func (a *app) disbandCmd() *cobra.Command {
	var f disbandFlags
	cmd := &cobra.Command{
		Use:   "disband <pool> [--yes] [--force] [--dry-run]",
		Short: "Remove a pool this machine runs, and keep the rest of Potluck",
		Long: `Disband removes one pool you made with ` + a.cmd("create") + `. It prints what it
will do and asks before it does anything (--dry-run only prints; --yes does
not ask):

  1. stops sharing the sizes this machine shares with the pool (they leave
     the machine's shared sizes too, not only the pool's list);
  2. cuts off every machine that joined the pool: its token ends, and the
     next time that machine reaches this one it is told the pool was removed
     (` + a.cmd("remotes") + ` there says so, and ` + a.cmd("leave") + ` forgets it);
  3. cancels the invites nobody used yet;
  4. removes the pool and everything it holds on this machine: its settings,
     grants, invites and the records of its sandboxes and their usage.

Sandboxes still running in the pool, or kept stopped with their disk, stop
it: end them first (the member who made them uses ` + a.cmd("sandbox rm") + `), or pass
--force, which removes the pool anyway and leaves them running until their
lease ends, out of their members' reach. The daemon makes the same
refusal when asked directly. A pool that GitHub Actions jobs run
on (` + a.cmd("runners") + `) is refused until that is ended.

A GitHub name given to the pool (@login/name) stays with it: a name is never
handed to another pool. If the pool is removed but a step
after that fails (a shared size or a file that stays), the answer lists what
is left, and the pool is gone either way.

It keeps this machine's identity, the other pools it runs, the pools it
joined, its daemon and the sizes it shares with other pools. Machines that
share a size with the pool keep that share until their owners run
` + a.cmd("host stop") + `. A pool you joined is left with ` + a.cmd("leave") + `, and everything
is removed with ` + a.cmd("uninstall") + `.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error { return a.disband(cmd.Context(), args[0], f) },
	}
	cmd.Flags().BoolVar(&f.yes, "yes", false, "do not ask")
	cmd.Flags().BoolVar(&f.dryRun, "dry-run", false, "print what it would do and change nothing")
	cmd.Flags().BoolVar(&f.force, "force", false, "remove the pool even while sandboxes run in it (they go on until their lease ends)")
	return cmd
}

// disbandPlan is what removing a pool does, read from the pool's machine.
type disbandPlan struct {
	Pool      api.Project                  `json:"pool"`
	Shared    []string                     `json:"shared"`    // sizes this machine shares with it
	Joined    []string                     `json:"joined"`    // the machines that joined, as "name (machine)"
	Unused    int                          `json:"unused"`    // invites nobody used yet
	Sandboxes []computeapi.RunTargetHandle `json:"sandboxes"` // running, or stopped with their disk kept
	Unread    []string                     `json:"unread"`    // sizes that did not say what runs on them
}

func (a *app) disband(ctx context.Context, name string, f disbandFlags) error {
	c, _, err := a.client()
	if err != nil {
		return err
	}
	pool, err := a.ownPool(ctx, c, name)
	if err != nil {
		return err
	}
	plan, err := a.disbandRead(ctx, c, pool)
	if err != nil {
		return err
	}
	a.sayDisbandPlan(plan, f.force)
	if f.dryRun {
		if a.asJSON() {
			return a.printJSON(plan)
		}
		return nil
	}
	if busy := plan.blocking(); busy != "" && !f.force {
		return fmt.Errorf("%s: end them first, or pass --force (they then run until their lease ends, out of their members' reach)\nnothing was removed", busy)
	}
	if !f.yes {
		if err := a.confirmYes("Type yes to remove pool "+pool.Slug+": ", "removed"); err != nil {
			return err
		}
	}
	// The daemon refuses a pool with sandboxes in it unless told to remove it
	// anyway: the plan above was read earlier, so it asks again.
	r, err := c.RemoveProject(ctx, pool.ID, f.force)
	if err != nil {
		return err
	}
	if a.asJSON() {
		if err := a.printJSON(r); err != nil {
			return err
		}
	} else {
		a.say("removed pool %s from this machine", r.Project.Slug)
		if len(r.Machines) > 0 {
			a.say("cut off: %s (each is told the pool was removed the next time it reaches this machine)", strings.Join(r.Machines, ", "))
		}
	}
	if len(r.Leftovers) > 0 {
		// The pool is gone, so running disband again says this machine runs no
		// pool of that name: say what is left, and that it is the owner's to finish.
		return fmt.Errorf("pool %s was removed, but not everything it held:\n  %s\nrunning disband again cannot finish this: the pool is gone",
			r.Project.Slug, strings.Join(r.Leftovers, "\n  "))
	}
	return nil
}

// ownPool is the pool this machine runs named name (its slug or id), or the
// reason there is none: a pool this machine only joined is left, not removed.
func (a *app) ownPool(ctx context.Context, c *client.Client, name string) (api.Project, error) {
	ps, err := c.LocalProjects(ctx)
	if err != nil {
		return api.Project{}, err
	}
	if i := slices.IndexFunc(ps, func(p api.Project) bool { return p.Slug == name || p.ID == name }); i >= 0 {
		p := ps[i]
		if !slices.ContainsFunc(p.Areas, func(x api.Area) bool { return x.Name == TeamArea || x.Name == HostsArea }) {
			return api.Project{}, exitWith{2, api.Errorf(api.CodeBadRequest, "%s is a team project, not a pool (it has no %s or %s area)", p.Slug, TeamArea, HostsArea)}
		}
		return p, nil
	}
	if rs, err := c.Remotes(ctx); err == nil && slices.ContainsFunc(rs, func(r api.Remote) bool { return r.Project.Slug == name || r.Project.ID == name }) {
		return api.Project{}, api.Errorf(api.CodeBadRequest, "this machine joined pool %s; it does not run it", name).
			WithHint(a.cmd("leave "+name) + " leaves it; only the machine that runs a pool removes it")
	}
	err = api.Errorf(api.CodeNotFound, "this machine runs no pool named %q", name)
	var runs []string
	for _, p := range ps {
		runs = append(runs, p.Slug)
	}
	if len(runs) > 0 {
		return api.Project{}, err.(*api.Error).WithHint("it runs: " + strings.Join(runs, ", "))
	}
	return api.Project{}, err
}

// disbandRead reads what removing pool does, changing nothing.
func (a *app) disbandRead(ctx context.Context, c *client.Client, pool api.Project) (disbandPlan, error) {
	plan := disbandPlan{Pool: pool, Shared: []string{}, Joined: []string{}, Sandboxes: []computeapi.RunTargetHandle{}, Unread: []string{}}
	pc := inPool(c, pool.Slug)
	sizes, err := poolSizes(ctx, pc)
	if err != nil {
		return plan, fmt.Errorf("the pool's sizes could not be read: %w", err)
	}
	for _, s := range sizes {
		if s.RunTarget.Host == "" { // this machine's own: not a member machine's advert
			plan.Shared = append(plan.Shared, s.Name)
		}
	}
	invs, err := c.Invites(ctx)
	if err != nil {
		return plan, err
	}
	now := time.Now()
	for _, inv := range invs {
		if inv.ProjectID != pool.ID {
			continue
		}
		switch {
		case inv.State == "redeemed" && !inv.Revoked:
			if who := fmt.Sprintf("%s (%s)", inv.As, inv.Machine); !slices.Contains(plan.Joined, who) {
				plan.Joined = append(plan.Joined, who)
			}
		case inv.State == "live" && inv.Redeemed == nil && inv.Expires.After(now):
			plan.Unused++
		}
	}
	hs, skipped, err := computeclient.Sandboxes(ctx, pc, computeclient.HandleFilter{Live: true})
	if err != nil {
		return plan, fmt.Errorf("the pool's sandboxes could not be listed: %w", err)
	}
	plan.Sandboxes, plan.Unread = hs, skipped
	if plan.Unread == nil {
		plan.Unread = []string{}
	}
	return plan, nil
}

// blocking says what stops the removal (sandboxes that exist, or sizes that
// did not say), "" when nothing does.
func (p disbandPlan) blocking() string {
	var parts []string
	if n := len(p.Sandboxes); n > 0 {
		stopped := 0
		for _, h := range p.Sandboxes {
			if h.State == computeapi.StateStopped {
				stopped++
			}
		}
		s := sandboxCount(n) + " still in the pool"
		if stopped > 0 {
			s += fmt.Sprintf(" (%d stopped, with its disk kept)", stopped)
		}
		parts = append(parts, s)
	}
	if len(p.Unread) > 0 {
		parts = append(parts, "could not tell what runs on "+strings.Join(p.Unread, "; "))
	}
	return strings.Join(parts, "; ")
}

func (a *app) sayDisbandPlan(p disbandPlan, force bool) {
	w := a.notes()
	say := func(format string, args ...any) { fmt.Fprintf(w, format+"\n", args...) }
	say("%s will remove pool %s from this machine:", a.cmd("disband "+p.Pool.Slug), p.Pool.Slug)
	if len(p.Shared) > 0 {
		say("  1. stop sharing this machine's sizes with the pool: %s", strings.Join(p.Shared, ", "))
	} else {
		say("  1. nothing of this machine is shared with the pool")
	}
	if len(p.Joined) > 0 {
		say("  2. cut off %s, each told the pool was removed the next time it reaches this machine: %s", plural(len(p.Joined), "machine"), strings.Join(p.Joined, ", "))
	} else {
		say("  2. no machine joined the pool, so none is cut off")
	}
	say("  3. cancel %s nobody used yet", plural(p.Unused, "invite"))
	say("  4. remove the pool and everything it holds on this machine: settings, grants, sandbox records and usage")
	if b := p.blocking(); b != "" {
		if force {
			say("with --force: %s; they go on until their lease ends, out of their members' reach", b)
		} else {
			say("refused until this is cleared: %s", b)
		}
	}
	say("it keeps this machine's identity, its other pools, the pools it joined and its daemon")
}

// plural is "1 invite" or "2 invites".
func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}
