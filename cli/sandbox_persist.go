package cli

// potluck sandbox stop and start: a sandbox created with --persist keeps its
// disk while it is stopped (docs/daemon.md); start boots that disk again on
// the same machine with a new lease.

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/sengirunners/potluck/computeclient"
)

func (a *app) sandboxStopCmd() *cobra.Command {
	return &cobra.Command{Use: "stop <sandbox>", Short: "Stop a sandbox created with --persist: nothing runs, its disk is kept for " + a.cmd("sandbox start"),
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, t, id, err := a.sandboxClient(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			h, err := computeclient.StopRunTarget(cmd.Context(), c, t, id)
			if err != nil {
				return err
			}
			return a.printHandle(h, false)
		}}
}

func (a *app) sandboxStartCmd() *cobra.Command {
	var ttl time.Duration
	cmd := &cobra.Command{Use: "start <sandbox> [--ttl 1h]", Short: "Start a stopped sandbox again: the same disk, on the same machine, with a new lease",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if ttl < 0 {
				return fmt.Errorf("--ttl must not be negative")
			}
			c, t, id, err := a.sandboxClient(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			h, err := computeclient.StartRunTarget(cmd.Context(), c, t, id, int(ttl/time.Second))
			if err != nil {
				return err
			}
			return a.printHandle(h, false)
		}}
	cmd.Flags().DurationVar(&ttl, "ttl", 0, "lease length (default: the machine's)")
	return cmd
}
