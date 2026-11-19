package cli

import (
	"context"
	"fmt"

	"github.com/sengirunners/potluck/agentrunners"
	"github.com/sengirunners/potluck/boxcp"
	"github.com/sengirunners/potluck/boxpush"
	"github.com/sengirunners/potluck/taskproxy"
	"github.com/spf13/cobra"
)

func (o *agentTaskOptions) addProxyFlags(cmd *cobra.Command) {
	f := cmd.Flags()
	f.StringVar(&o.proxy.Profile, "cooperative-proxy", "", "development: use an in-sandbox HTTP proxy with a host allowlist; clients can bypass it (default: disabled)")
	f.IntVar(&o.proxy.DownloadMbit, "proxy-download-mbit", 48, "cooperative proxy's shared download cap in Mbit/s (0: unlimited); excludes sandbox setup")
	f.StringSliceVar(&o.proxy.AllowHosts, "proxy-allow-host", nil, "additional DNS names or scoped *.domain entries allowed by the cooperative proxy (repeatable; no IPs or shared hosting wildcards)")
}

func (o *agentTaskOptions) checkProxy(cmd *cobra.Command) error {
	if o.proxy.Profile == "" {
		if cmd.Flags().Changed("proxy-download-mbit") || cmd.Flags().Changed("proxy-allow-host") {
			return fmt.Errorf("proxy options require --cooperative-proxy development")
		}
		return nil
	}
	_, err := o.proxy.Config()
	return err
}

func (o agentTaskOptions) prepareProxy(ctx context.Context, box daemonBox, dir string, argv []string) ([]string, error) {
	if o.proxy.Profile == "" {
		return argv, nil
	}
	config, err := o.proxy.Config()
	if err != nil {
		return nil, err
	}
	missing, err := agentrunners.Missing(ctx, box, "python3")
	if err != nil {
		return nil, err
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("the cooperative task proxy needs python3 in the sandbox image")
	}
	for _, file := range []struct {
		name string
		body []byte
	}{{"proxy.py", []byte(taskproxy.Script)}, {"proxy.json", config}} {
		if _, err := boxcp.WriteFile(ctx, boxpush.Exec(box.c, box.target, box.id), dir+"/"+file.name, file.body, boxcp.Options{}); err != nil {
			return nil, fmt.Errorf("write the cooperative task proxy: %w", err)
		}
	}
	return o.adapter.ProxyCommand(dir, argv)
}
