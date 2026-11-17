package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/sengirunners/potluck/runtarget"
)

// driverCmd is potluck driver: the driver programs the daemon runs, each
// pinned by sha256 (DRIVERS.md).
func (a *app) driverCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "driver",
		Short: "The driver programs this machine runs (DRIVERS.md): pin",
	}
	cmd.AddCommand(&cobra.Command{
		Use:   "pin <driver>...",
		Short: "Trust the driver file there now (after you upgraded it on purpose): record its path and sha256",
		Long: `Drivers run as you and see secrets, so the daemon runs one only from the
directory of the ` + a.name() + ` program or $POTLUCK_HOME/drivers, and only while
its sha256 is the one pinned. A driver is pinned the first time it runs;
after you upgrade or replace one on purpose, pin it again with this.`,
		Example: "  " + a.cmd("driver pin tart"),
		Args:    cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := a.notWhileAttached(a.cmd("driver pin")); err != nil {
				return err
			}
			cfg, err := a.loadClient()
			if err != nil {
				return err
			}
			var out []runtarget.DriverPin
			for _, d := range args {
				p, err := runtarget.PinDriver(cfg.Home, a.driverDirs(cfg), d)
				if err != nil {
					return err
				}
				out = append(out, p)
				if !a.asJSON() {
					fmt.Fprintf(a.host.Stdout, "pinned %s: %s sha256 %s\n", d, p.Path, p.SHA256)
				}
			}
			if a.asJSON() {
				return a.printJSON(out)
			}
			return nil
		},
	})
	return cmd
}
