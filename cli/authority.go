package cli

// potluck authority: where a team's home (HQ) lives, its standby, and a move
// or takeover. Each command reads the status first and sends what it showed
// (HQ's machine and epoch, the plan, the move), so a press on an out-of-date
// view is refused as stale, as on the owner's screen. The daemon checks who
// may press (kit/teamlink/home_control.go); these commands hold no state.

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
)

func (a *app) authorityCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "authority",
		Short: "Show where your team's home (HQ) lives, its standby and any move",
		Long: `HQ is the machine that holds a team's projects for every member. Its
standby holds a copy and can take over. This shows both, any move running
or stopped, and whether you may change them: only an owner of every team
project the home holds, on HQ's own socket or from their own machine.

Nothing here moves HQ by itself: every change is an owner's press.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return call(a, cmd, func(ctx context.Context, c *client.Client) (api.AuthorityStatus, error) {
				return c.Authority(ctx)
			}, authorityText)
		},
	}
	cmd.AddCommand(a.authorityCandidatesCmd(), a.authorityStandbyCmd(), a.authorityReleaseCmd(), a.authorityMoveCmd(),
		a.authorityControlCmd("abort", "Call off the stopped move: the old HQ serves again, nothing lost",
			func(ctx context.Context, c *client.Client, q api.MoveControl) (api.HomeMove, error) {
				return c.AbortMove(ctx, q)
			}),
		a.authorityControlCmd("resume", "Try the stopped move again from the step that stopped",
			func(ctx context.Context, c *client.Client, q api.MoveControl) (api.HomeMove, error) {
				return c.ResumeMove(ctx, q)
			}),
		a.authorityPromoteCmd(), a.authorityRetireCmd())
	return cmd
}

func (a *app) authorityCandidatesCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "candidates",
		Short: "List the machines that could be HQ's standby, and why not",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return call(a, cmd, func(ctx context.Context, c *client.Client) (api.CandidateList, error) {
				return c.AuthorityCandidates(ctx)
			}, func(l api.CandidateList) string {
				var b strings.Builder
				fmt.Fprintf(&b, "the standby holds a copy of %d team project(s) and %d secret(s)", l.Preview.Projects, len(l.Preview.Secrets))
				for _, x := range l.Candidates {
					fmt.Fprintf(&b, "\n%s: %s", x.Machine, reasonsText(x.Reasons, "can be the standby"))
				}
				return b.String()
			})
		},
	}
}

func (a *app) authorityStandbyCmd() *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "standby <machine> [--yes]",
		Short: "Make a machine HQ's standby: it copies the home and can take over",
		Long: `Asks a machine to become HQ's standby. It stops the agent sessions running
on it, then holds a copy of every team project this home holds and of its
secrets, and can take over. It asks first; --yes does not ask.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return call(a, cmd, func(ctx context.Context, c *client.Client) (api.AuthorityStatus, error) {
				st, err := c.Authority(ctx)
				if err != nil {
					return st, err
				}
				if !yes {
					l, err := c.AuthorityCandidates(ctx)
					if err != nil {
						return st, err
					}
					i := slices.IndexFunc(l.Candidates, func(x api.Candidate) bool { return x.Machine == args[0] })
					if i < 0 {
						return st, api.Errorf(api.CodeNotFound, "%s is not one of the machines that could be the standby", args[0]).
							WithHint(a.cmd("authority candidates") + " lists them")
					}
					if err := a.confirmYes(fmt.Sprintf("%s becomes HQ's standby: it stops its %d agent session(s), then holds a copy of %d team project(s) and %d secret(s).\nType yes to make it the standby: ",
						args[0], l.Candidates[i].SessionsRunning, l.Preview.Projects, len(l.Preview.Secrets)), "changed"); err != nil {
						return st, err
					}
				}
				return c.SetStandby(ctx, api.StandbyRequest{Machine: args[0], Shown: api.NewShown(st.Machine, st.Epoch)})
			}, authorityText)
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "do not ask")
	return cmd
}

func (a *app) authorityReleaseCmd() *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "release-standby [--yes]",
		Short: "Stop using the standby: no machine can take over until another is set",
		Long: `Stops using HQ's standby: it deletes its copy of the home and of its
secrets, and no machine can take over until another is set. It asks first;
--yes does not ask.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return call(a, cmd, func(ctx context.Context, c *client.Client) (api.AuthorityStatus, error) {
				st, err := c.Authority(ctx)
				if err != nil {
					return st, err
				}
				if st.Standby.Machine == "" {
					return st, api.Errorf(api.CodeBadRequest, "HQ %s has no standby", st.Machine)
				}
				if !yes {
					l, err := c.AuthorityCandidates(ctx)
					if err != nil {
						return st, err
					}
					if err := a.confirmYes(fmt.Sprintf("%s stops being the standby: its copy of %d team project(s) and %d secret(s) is deleted, and no machine can take over until another is set.\nType yes to release it: ",
						st.Standby.Machine, l.Preview.Projects, len(l.Preview.Secrets)), "changed"); err != nil {
						return st, err
					}
				}
				return c.ReleaseStandby(ctx, api.StandbyRequest{Machine: st.Standby.Machine, Shown: api.NewShown(st.Machine, st.Epoch)})
			}, authorityText)
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "do not ask")
	return cmd
}

func (a *app) authorityMoveCmd() *cobra.Command {
	var check, yes bool
	cmd := &cobra.Command{
		Use:   "move <machine> [--check] [--yes]",
		Short: "Move HQ to its standby, a planned move in which nothing is lost",
		Long: `Moves HQ to the standby: HQ stops, the standby takes its last changes and
serves, and every member follows. It checks first and shows what the move
will do; --check stops there. It asks before it starts; --yes does not ask.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return call(a, cmd, func(ctx context.Context, c *client.Client) (api.MoveAnswer, error) {
				st, err := c.Authority(ctx)
				if err != nil {
					return api.MoveAnswer{}, err
				}
				plan, err := c.MoveHQ(ctx, api.MoveRequest{To: args[0], CheckOnly: true})
				if err != nil || check {
					return plan, err
				}
				if failed := failedChecks(plan.Preflight); failed != "" {
					return plan, api.Errorf(api.CodeConflict, "HQ cannot move to %s now: %s", args[0], failed).
						WithHint("fix what failed and run " + a.cmd("authority move "+args[0]) + " again")
				}
				if !yes {
					fmt.Fprintln(a.host.Stderr, preflightText(plan.Preflight))
					if err := a.confirmYes(fmt.Sprintf("Type yes to move HQ from %s to %s: ", plan.Preflight.From, plan.Preflight.To), "moved"); err != nil {
						return plan, err
					}
				}
				return c.MoveHQ(ctx, api.MoveRequest{To: args[0], PlanID: plan.Preflight.ID, Shown: api.NewShown(st.Machine, st.Epoch)})
			}, func(m api.MoveAnswer) string {
				if m.Move == nil {
					return preflightText(m.Preflight)
				}
				return moveText(*m.Move)
			})
		},
	}
	cmd.Flags().BoolVar(&check, "check", false, "only check, and show what the move would do")
	cmd.Flags().BoolVar(&yes, "yes", false, "do not ask")
	return cmd
}

// authorityControlCmd is abort or resume: on the old HQ's own socket, of the
// move its status shows.
func (a *app) authorityControlCmd(use, short string, press func(context.Context, *client.Client, api.MoveControl) (api.HomeMove, error)) *cobra.Command {
	return &cobra.Command{
		Use:   use,
		Short: short,
		Long:  short + ". Run it on the old HQ, which waits parked while its move is stopped.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return call(a, cmd, func(ctx context.Context, c *client.Client) (api.HomeMove, error) {
				st, err := c.Authority(ctx)
				if err != nil {
					return api.HomeMove{}, err
				}
				if st.Move == nil {
					return api.HomeMove{}, api.Errorf(api.CodeBadRequest, "no move is running or stopped here")
				}
				return press(ctx, c, api.MoveControl{MoveID: st.Move.ID})
			}, moveText)
		},
	}
}

func (a *app) authorityPromoteCmd() *cobra.Command {
	var down, noSecrets, force, yes bool
	cmd := &cobra.Command{
		Use:   "promote --old-home-is-down [--without-secrets] [--yes]",
		Short: "Make this standby HQ because the old HQ is gone: writes after its last copy are lost",
		Long: `Run it on the standby, when HQ is gone for good. The standby serves from
its newest copy: whatever HQ took after that copy is lost. Check HQ is
really down first and say so with --old-home-is-down. When HQ comes back it
does not serve: it finds it was replaced and waits for you to retire it.

--without-secrets takes over with no secrets, when the standby holds none.
--force (the runbook's) takes over while HQ still answers the standby,
having HQ stop for good first. It asks before it takes over; --yes does not
ask. Only a person presses it: never a session or an agent.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !down && !force {
				return api.Errorf(api.CodeBadRequest, "check the old HQ is down first").
					WithHint("pass --old-home-is-down once it is; " + a.cmd("authority move <machine>") + " moves a running HQ with nothing lost")
			}
			return call(a, cmd, func(ctx context.Context, c *client.Client) (api.HomeMove, error) {
				st, err := c.Authority(ctx)
				if err != nil {
					return api.HomeMove{}, err
				}
				if st.ServedBy.State == api.HQServing {
					return api.HomeMove{}, api.Errorf(api.CodeConflict, "%s serves HQ now: a takeover is run on the standby", st.ServedBy.Machine).
						WithHint(a.cmd("authority move <machine>") + " moves a running HQ with nothing lost")
				}
				if !yes {
					if err := a.confirmYes(promoteText(st, noSecrets, force)+"Type yes to take over: ", "taken over"); err != nil {
						return api.HomeMove{}, err
					}
				}
				return c.PromoteStandby(ctx, api.PromoteRequest{Machine: st.ServedBy.Machine, Shown: api.NewShown(st.Machine, st.Epoch),
					OldHomeIsDown: down, WithoutSecrets: noSecrets, Force: force})
			}, moveText)
		},
	}
	cmd.Flags().BoolVar(&down, "old-home-is-down", false, "you checked the old HQ is down")
	cmd.Flags().BoolVar(&noSecrets, "without-secrets", false, "take over without the secrets, when the standby holds none")
	cmd.Flags().BoolVar(&force, "force", false, "take over while the old HQ still answers, having it stop for good first (the runbook's)")
	cmd.Flags().BoolVar(&yes, "yes", false, "do not ask")
	return cmd
}

func (a *app) authorityRetireCmd() *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "retire [--yes]",
		Short: "Drop this replaced old HQ's copy of the home",
		Long: `Run it on an old HQ another machine replaced. It drops this machine's copy
of the home; it never serves the home again. It asks first; --yes does not ask.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return call(a, cmd, func(ctx context.Context, c *client.Client) (api.HomeMove, error) {
				st, err := c.Authority(ctx)
				if err != nil {
					return api.HomeMove{}, err
				}
				if st.ServedBy.State == api.HQServing {
					return api.HomeMove{}, api.Errorf(api.CodeConflict, "%s serves HQ now: retire is run on an old HQ another machine replaced", st.ServedBy.Machine)
				}
				if !yes {
					if err := a.confirmYes(fmt.Sprintf("%s drops its copy of the home %s now serves.\nType yes to retire it: ", st.ServedBy.Machine, st.Machine), "retired"); err != nil {
						return api.HomeMove{}, err
					}
				}
				return c.RetireHQ(ctx, api.RetireRequest{Machine: st.ServedBy.Machine, Shown: api.NewShown(st.Machine, st.Epoch)})
			}, moveText)
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "do not ask")
	return cmd
}

// promoteText is a takeover's confirm, but its question: who takes over
// from whom, the writes it loses, and what --without-secrets and --force do.
func promoteText(st api.AuthorityStatus, noSecrets, force bool) string {
	lost := "an unknown stretch"
	if s := st.Silent; s != nil && s.LossS != nil {
		lost = "about " + (time.Duration(*s.LossS) * time.Second).String()
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s takes over from %s. What %s took after %s's newest copy is lost: %s of writes.\n",
		st.ServedBy.Machine, st.Machine, st.Machine, st.ServedBy.Machine, lost)
	if noSecrets {
		fmt.Fprintf(&b, "It takes over without the secrets: whatever needs them stops working until an owner adds them on %s again.\n", st.ServedBy.Machine)
	}
	if force {
		fmt.Fprintf(&b, "%s still answers: it is stopped for good first, and never serves this home again.\n", st.Machine)
	}
	return b.String()
}

// authorityText is a status for people.
func authorityText(st api.AuthorityStatus) string {
	var b strings.Builder
	fmt.Fprintf(&b, "HQ: %s (%s, epoch %d)", st.Machine, st.State, st.Epoch)
	if st.ServedBy.Machine != st.Machine {
		fmt.Fprintf(&b, "\nanswered by %s (%s)", st.ServedBy.Machine, st.ServedBy.State)
	}
	if s := st.Silent; s != nil {
		since := "a while"
		if s.Since != nil {
			since = s.Since.Format(time.DateTime)
		}
		fmt.Fprintf(&b, "\nHQ has not answered since %s; the standby sees it: %s", since, s.StandbyProbe)
	}
	if st.Standby.State == api.StandbyNone {
		b.WriteString("\nstandby: none")
	} else {
		fmt.Fprintf(&b, "\nstandby: %s (%s)", st.Standby.Machine, st.Standby.State)
		for _, r := range st.Standby.Reasons {
			fmt.Fprintf(&b, "\n  %s", reasonText(r))
		}
	}
	if s := st.Setup; s != nil {
		fmt.Fprintf(&b, "\nsetting up %s as the standby: %s", s.Machine, s.Step)
		if s.Failure != nil {
			fmt.Fprintf(&b, " (%s)", reasonText(*s.Failure))
		}
	}
	if st.Move != nil {
		b.WriteString("\n" + moveText(*st.Move))
	}
	if f := st.FormerHQ; f != nil {
		fmt.Fprintf(&b, "\nformer HQ: %s (stopped for good: %t)", f.Machine, f.Fenced)
	}
	if !st.CanManage {
		b.WriteString("\nyou may not change these: only an owner of every team project this home holds")
	}
	return b.String()
}

// moveText is a move for people: one line, then its steps.
func moveText(m api.HomeMove) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s: %s to %s, %s", m.Kind, m.ID, m.From, m.To, m.State)
	if m.Failure != "" {
		fmt.Fprintf(&b, " (%s)", m.Failure)
	}
	for _, s := range m.Steps {
		fmt.Fprintf(&b, "\n  %s: %s", s.ID, s.State)
		if s.Detail != "" {
			fmt.Fprintf(&b, " (%s)", s.Detail)
		}
	}
	if f := m.Follow; f != nil {
		fmt.Fprintf(&b, "\n  %d of %d machines follow", f.Reconnected, f.Total)
		for _, s := range f.Stragglers {
			fmt.Fprintf(&b, "\n  %s: %s", s.Machine, reasonText(s.Reason))
		}
	}
	return b.String()
}

// preflightText is a move's checks for people.
func preflightText(p api.HomePreflight) string {
	var b strings.Builder
	fmt.Fprintf(&b, "move HQ from %s to %s: %d team project(s), %d session(s) stopped", p.From, p.To, len(p.Projects), p.SessionsStopped)
	if p.DownS != nil {
		fmt.Fprintf(&b, ", down about %s", time.Duration(*p.DownS)*time.Second)
	}
	for _, c := range p.Checks {
		mark := "ok"
		if !c.OK {
			mark = "FAILED"
		}
		fmt.Fprintf(&b, "\n  %s %s", mark, c.Text)
		if !c.OK && c.Fix != "" {
			fmt.Fprintf(&b, " (fix: %s)", c.Fix)
		}
	}
	if pa := p.PublicAddress; pa != nil {
		fmt.Fprintf(&b, "\n  %d member(s) connect through the public address %s: they are cut off after the move until it points at %s", pa.Members, pa.Address, p.To)
	}
	for _, u := range p.Unseen {
		fmt.Fprintf(&b, "\n  not seen lately: %s", u.Machine)
	}
	return b.String()
}

// failedChecks names the checks that failed ("" when none did).
func failedChecks(p api.HomePreflight) string {
	var out []string
	for _, c := range p.Checks {
		if !c.OK {
			out = append(out, c.Code)
		}
	}
	return strings.Join(out, ", ")
}

func reasonText(r api.Reason) string {
	if r.Text != "" {
		return r.Text
	}
	return r.Code
}

func reasonsText(rs []api.Reason, none string) string {
	if len(rs) == 0 {
		return none
	}
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = reasonText(r)
	}
	return strings.Join(out, "; ")
}
