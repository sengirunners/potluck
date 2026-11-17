package cli

import (
	"context"
	"os"
	"strings"

	"github.com/sengirunners/potluck/kit/client"
	sandboxmcp "github.com/sengirunners/potluck/mcp"
	"github.com/spf13/cobra"
)

func (a *app) mcpCmd() *cobra.Command {
	var as string
	cmd := &cobra.Command{Use: "mcp", Short: "Serve sandbox tools over MCP on standard input and output", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// Read only Potluck's identity variables. An inherited agent session in
			// another product must never silently select this caller.
			c, _, err := a.client()
			if err != nil {
				return err
			}
			c.Autostart = nil
			c.As = as
			if os.Getenv(EnvBoxCredential) == "" { // a box's client carries its own token
				c.Token = os.Getenv("POTLUCK_TOKEN")
			}
			return sandboxmcp.Serve(cmd.Context(), func(ctx context.Context) (*client.Client, error) {
				if c.Project != "" {
					return c, nil
				}
				p, err := onlyPool(ctx, c)
				if err != nil {
					return nil, err
				}
				return inPool(c, p), nil
			}, a.stdin, a.stdout, a.prog.Tools...)
		}}
	cmd.Flags().StringVar(&as, "as", strings.TrimSpace(os.Getenv("POTLUCK_AS")), "caller address (default: POTLUCK_AS, else your local identity)")
	return cmd
}
