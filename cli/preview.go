package cli

import (
	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/computeclient"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/spf13/cobra"
	"strconv"
)

func (a *app) sandboxPreviewCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "preview", Short: "Publish applications and manage their HTTPS preview links"}
	var public bool
	publish := &cobra.Command{Use: "publish <sandbox> <port> --public", Short: "Explicitly expose an HTTP port; each preview gets its own hostname", Args: cobra.ExactArgs(2), RunE: func(cmd *cobra.Command, args []string) error {
		port, err := strconv.Atoi(args[1])
		if err != nil {
			return err
		}
		c, target, id, err := a.sandboxClient(cmd.Context(), args[0])
		if err != nil {
			return err
		}
		out, err := computeclient.PublishRunTargetPreview(cmd.Context(), c, target, id, computeapi.PreviewRequest{Port: port, Public: public})
		if err != nil {
			return err
		}
		return a.printJSON(out)
	}}
	publish.Flags().BoolVar(&public, "public", false, "allow anyone with the returned URL to reach this HTTP port until lease end or revoke")
	list := &cobra.Command{Use: "ls <sandbox>", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		c, target, id, err := a.sandboxClient(cmd.Context(), args[0])
		if err != nil {
			return err
		}
		out, err := computeclient.RunTargetPreviews(cmd.Context(), c, target, id)
		if err != nil {
			return err
		}
		return a.printJSON(out)
	}}
	revoke := &cobra.Command{Use: "revoke <sandbox> <preview-id>", Args: cobra.ExactArgs(2), RunE: func(cmd *cobra.Command, args []string) error {
		c, target, id, err := a.sandboxClient(cmd.Context(), args[0])
		if err != nil {
			return err
		}
		return computeclient.RevokeRunTargetPreview(cmd.Context(), c, target, id, args[1])
	}}
	configure := &cobra.Command{Use: "configure <existing-domain>", Short: "Use an existing wildcard HTTPS ingress domain on this daemon; empty domain disables publishing", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		c, _, err := a.client()
		if err != nil {
			return err
		}
		var out api.TunnelStatus
		if err := c.Do(cmd.Context(), "POST", "/v0/tunnel/preview-domain", api.TunnelPreviewDomainRequest{Domain: args[0]}, &out); err != nil {
			return err
		}
		return a.printJSON(out)
	}}
	cmd.AddCommand(publish, list, revoke, configure)
	return cmd
}
