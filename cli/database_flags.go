package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

// databaseFlags owns the shared request flag and its CLI conflicts. The
// daemon resolves the name; a caller never supplies a manifest or image pin.
// Placement asks for a database-capable binding from the request itself
// (computeclient.AcquireBox).
func databaseFlags(cmd *cobra.Command, name *string) {
	cmd.Flags().StringVar(name, "database", "", "a project-owned database recipe; starts private Postgres and sets DATABASE_URL")
	for _, other := range []string{"image", "template", "persist"} {
		if cmd.Flags().Lookup(other) != nil {
			cmd.MarkFlagsMutuallyExclusive("database", other)
		}
	}
	cmd.PreRunE = func(cmd *cobra.Command, args []string) error {
		if cmd.Flags().Changed("database") && (*name == "" || strings.TrimSpace(*name) != *name) {
			return fmt.Errorf("--database needs a project recipe name, with no spaces around it")
		}
		return nil
	}
}
