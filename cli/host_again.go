package cli

// host run again: a run where this machine already shares sizes with the
// pool and nothing on the command line changes the share is someone
// looking, more often than someone sharing again. It pulls, re-checks and
// re-binds every size, so it only says what it would do and asks for
// --yes; host status is the look. A join code is a join, and a run with a
// flag that shapes the share says what to change: both go ahead. The old
// spelling host share, which programs run, never asks.

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/sengirunners/potluck/hostprobe"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/project"
)

// sameShare are host's flags that change how a run goes (or, for
// --invite-stdin, where its argument comes from), not what it shares: a
// run with only these (and the host program's AfterLend skip) is a bare
// re-run.
var sameShare = []string{"yes", "no-sudo", "pull-timeout", "show-commands", "detach", "wait", flagInviteStdin}

// shapesShare says whether cmd was given one of its own flags that shapes
// the share (the program's --json and pool flags are not its own).
func (a *app) shapesShare(cmd *cobra.Command) bool {
	return len(a.shapingFlags(cmd)) > 0
}

// shapingFlags are the flags given that shapesShare counts, by name.
func (a *app) shapingFlags(cmd *cobra.Command) []string {
	same := sameShare
	if al := a.host.AfterLend; al != nil && al.Skip != "" {
		same = append(slices.Clone(same), al.Skip)
	}
	local := cmd.LocalFlags()
	var shaped []string
	cmd.Flags().Visit(func(f *pflag.Flag) {
		if local.Lookup(f.Name) != nil && !slices.Contains(same, f.Name) {
			shaped = append(shaped, f.Name)
		}
	})
	return shaped
}

// sharedAgain are the sizes an earlier share made that a run for sc shares
// again: in a pool this machine runs, its sizes there; in a pool it
// joined, the machine-level sizes shared with that pool (a size shared
// only with another pool is a first share here).
func (sc hostScope) sharedAgain(o hostprobe.ShareOptions) []string {
	var out []string
	for _, t := range o.Lent() {
		if sc.machine != "" {
			sh := sc.shared[t.Name]
			if !slices.Contains(sh, sc.pool) && (sc.poolID == "" || !slices.Contains(sh, sc.poolID)) {
				continue
			}
		}
		out = append(out, t.Name)
	}
	return out
}

// askAgain refuses a bare re-run for arg where this machine already shares
// sizes with the pool: it prints them and the run's plan (a dry run's), and
// changes nothing. Under --json stdout stays empty and the refusal is the
// answer. nil: not a re-run, so the run goes ahead.
func (a *app) askAgain(ctx context.Context, c *client.Client, arg string, o hostprobe.ShareOptions) error {
	if project.LooksLikeJoinCode(arg) {
		return nil
	}
	sc, err := a.hostScope(ctx, c, arg, &o)
	if err != nil {
		return err
	}
	again := sc.sharedAgain(o)
	if len(again) == 0 {
		return nil
	}
	look := a.cmd("host status") + " shows what this machine lends and runs now"
	e := &api.Error{Code: api.CodeConflict,
		Message: fmt.Sprintf("this machine already shares %s with pool %s; nothing changed", strings.Join(again, ", "), sc.pool),
		Hint:    "to pull, re-check and share them again, run it again with --yes; " + look}
	o.DryRun, o.Progress = true, nil
	res, err := hostprobe.Check(ctx, shareRunner, o)
	var nr *hostprobe.NotReadyError
	switch {
	case errors.As(err, &nr): // the admin step a run with --yes takes first would make it ready
		e.Hint = fmt.Sprintf("to share them again, run it again with --yes: it first runs this machine's admin step (%s); %s", nr.RootCommand, look)
		return e
	case err != nil:
		e.Hint = fmt.Sprintf("running it again would fail: %v; %s", err, look)
		return e
	}
	if !a.asJSON() {
		w := a.host.Stdout
		fmt.Fprintf(w, "this machine already shares %s with pool %s; %s again would do this:\n", strings.Join(again, ", "), sc.pool, a.cmd("host"))
		printShare(w, res, prefix(a.host.Prefix))
	}
	return e
}

// hostManyPools is host's answer when onlyPool found several pools: it
// names the forms host takes, since a program that mounts host may have no
// --pool.
func (a *app) hostManyPools(err error) error {
	var mp *manyPoolsError
	if !errors.As(err, &mp) {
		return err
	}
	return fmt.Errorf("this machine is in more than one pool (%s): %s shares it with one; %s shows what it lends now",
		strings.Join(mp.Pools, ", "), a.cmd("host <pool>"), a.cmd("host status"))
}
