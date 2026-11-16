package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/computeclient"
)

// sealingCmd is potluck sealing: the pool's sealing mode and the machines
// that cannot seal yet (the pre-check), and potluck sealing set to change it
// (docs/daemon.md, "Sealing modes").
func (a *app) sealingCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "sealing", Short: "The pool's sealing mode, and the machines that cannot seal secrets yet",
		Long: `Every machine seals secrets and runner configs to the next machine's key when
that machine can. The pool's mode says what happens with a machine too old to
seal: "preferred" sends to it as before, in the clear over the tailnet, and
warns; "required" refuses. A pool started by this build begins "required"; a
pool from before keeps "preferred" until you upgrade every machine, check that
this command lists none that cannot seal, then: potluck sealing set required.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := a.inPool(cmd.Context())
			if err != nil {
				return err
			}
			v, err := computeclient.Sealing(cmd.Context(), c)
			if err != nil {
				return err
			}
			if a.json {
				return a.printJSON(v)
			}
			a.say("%s", sealingText(v))
			return nil
		}}
	var force bool
	set := &cobra.Command{Use: "set preferred|required", Short: "Set the pool's sealing mode (an admin of the pool); required is refused while a machine cannot seal, unless --force",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := a.inPool(cmd.Context())
			if err != nil {
				return err
			}
			v, err := computeclient.SetSealing(cmd.Context(), c, computeapi.SetSealingRequest{Mode: args[0], Force: force})
			if err != nil {
				return err
			}
			if a.json {
				return a.printJSON(v)
			}
			a.say("%s", sealingText(v))
			return nil
		}}
	set.Flags().BoolVar(&force, "force", false, "set required even while machines cannot seal (they then get no secrets or runner jobs)")
	forget := &cobra.Command{Use: "forget", Short: "Forget that the pool's machine named a seal key (this machine's owner), after it was put back to an older build on purpose",
		Long: `Once a pool's machine has named a seal key to this machine in a pool that
requires sealing, this machine never sends it secrets in the clear again (nor
takes an op holding them in the clear from it), even if it later names none:
a downgrade, or the pool's machine put back to a build from before sealing.
(In a pool that only prefers sealing, such a rollback falls back to the clear
by itself, with a warning on every call.) If its owner put it back on
purpose, forget it here. This forgets that pool's machine for every pool it
serves, not only the one named. On a record of pool machines that cannot be
read, it starts a new, empty one (every pool machine is forgotten). Logged.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, _, err := a.client()
			if err != nil {
				return err
			}
			pool := a.project
			if pool == "" {
				return fmt.Errorf("name the pool: potluck sealing forget -P <pool>")
			}
			v, err := computeclient.ForgetSealedHome(cmd.Context(), c, pool)
			if err != nil {
				return err
			}
			if a.json {
				return a.printJSON(v)
			}
			if v.Forgot {
				a.say("forgot that pool %s's machine named a seal key", v.Pool)
			} else {
				a.say("pool %s's machine had not named a seal key to this machine", v.Pool)
			}
			return nil
		}}
	cmd.AddCommand(set, forget)
	return cmd
}

// sealingText is v for a person.
func sealingText(v computeapi.Sealing) string {
	var b strings.Builder
	fmt.Fprintf(&b, "pool %s: sealing %s\n", v.Project, v.Mode)
	for _, p := range v.Peers {
		can := "seals"
		if !p.Seals {
			can = "CANNOT SEAL: update it"
		}
		role := "member"
		if p.Lends {
			role = "host"
		}
		fmt.Fprintf(&b, "  %-20s %-7s %s (seen %s)\n", p.Machine, role, can, p.Seen.Format("2006-01-02 15:04:05Z"))
	}
	switch {
	case len(v.CannotSeal) > 0 && v.Mode == computeapi.SealingRequired:
		fmt.Fprintf(&b, "%d machine(s) cannot seal and get no secrets or runner jobs until updated", len(v.CannotSeal))
	case len(v.CannotSeal) > 0:
		fmt.Fprintf(&b, "%d machine(s) cannot seal: secrets reach them in the clear over the tailnet; update them before setting required", len(v.CannotSeal))
	case v.Mode == computeapi.SealingPreferred:
		fmt.Fprintf(&b, "every machine heard from since %s can seal: potluck sealing set required", v.Since.Format("2006-01-02 15:04Z"))
	default:
		b.WriteString("every machine heard from can seal")
	}
	return b.String()
}

// poolSealing is the pool's mode for status, or "" when its machine does
// not answer one (an older build).
func poolSealing(ctx context.Context, a *app, pool string) string {
	c, _, err := a.client()
	if err != nil {
		return ""
	}
	v, err := computeclient.Sealing(ctx, inPool(c, pool))
	if err != nil {
		return ""
	}
	if len(v.CannotSeal) > 0 {
		return fmt.Sprintf("%s (%d cannot seal)", v.Mode, len(v.CannotSeal))
	}
	return v.Mode
}
