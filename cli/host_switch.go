package cli

import (
	"cmp"
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/computeclient"
	"github.com/sengirunners/potluck/hostprobe"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/menubar"
	"github.com/sengirunners/potluck/protocol"
	"github.com/sengirunners/potluck/vmhost"
)

// The host switch from the potluck command: what this machine lends and
// whether it takes new work (potluck host status, which the macOS menu bar
// item reads with --json), and setting it (potluck host set, the same code
// as potluck-driver-tart host set). Neither starts the daemon.

func (a *app) hostStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "How many of this machine's cores are lent and in use, and whether it takes new work (the menu bar item's facts)",
		Long: `Status reads the sizes this machine shares (with pools it joined, and in
pools it runs) from the daemon, each with the sandboxes running on it, and the
host switch (potluck host set). Lent cores are, per driver state directory,
its slots times its largest size's cores. With --json it prints the menu
the macOS menu bar item draws (docs/menubar.md). It never starts the daemon.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			st := menubar.Build(hostFacts(cmd.Context()))
			if a.json {
				return a.printJSON(st)
			}
			m := st.Menu
			lines := []string{m.Title}
			for _, s := range []string{m.Detail, m.Note, m.Cores} {
				if s != "" {
					lines = append(lines, s)
				}
			}
			lines = append(lines, fmt.Sprintf("host switch: %s (%s)", st.Mode, st.State))
			a.say("%s", strings.Join(lines, "\n"))
			return nil
		},
	}
}

func (a *app) hostSetCmd() *cobra.Command {
	var forD, until, deadline string
	cmd := &cobra.Command{
		Use:   "set <open|drain|closed|auto> [--for 2h | --until 23:00] [--deadline 30m]",
		Short: "Set this machine's host switch: open, drain (wind down), closed (stop now) or auto (your rules)",
		Long: `Set writes the host switch every size on this machine obeys, the same as
potluck-driver-tart host set: open (take work whatever the schedule says),
drain (no new work; running work finishes, and is cut at the deadline),
closed (no new work; running work ends now) or auto (the sizes' own rules).
--for and --until return it to auto then; --deadline is a drain's.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			o, err := vmhost.SetHost(args[0], forD, until, deadline)
			if err != nil {
				return err
			}
			if a.json {
				return a.printJSON(o)
			}
			line := o.Summary()
			a.say("%s", line)
			return nil
		},
	}
	cmd.Flags().StringVar(&forD, "for", "", "return to auto after this long (30m, 2h)")
	cmd.Flags().StringVar(&until, "until", "", "return to auto at this local HH:MM or RFC 3339 time")
	cmd.Flags().StringVar(&deadline, "deadline", "", "a drain's: how long running work has before it is cut")
	return cmd
}

// hostNow reads the host switch file; tests stand in for it.
var hostNow = vmhost.HostNow

// hostFacts gathers what potluck host status reports: the daemon's bindings
// on this machine with their live boxes, the host switch, and the cores.
func hostFacts(ctx context.Context) menubar.Facts {
	f := menubar.Facts{Cores: hostprobe.SystemFacts{}.Cores(), Loc: time.Local}
	targets, running, err := liveBindings(ctx)
	switch {
	case !running:
		f.Daemon = err.Error()
	case err != nil:
		f.ListErr = err.Error()
	}
	f.Targets = targets
	// The host switch as the first binding that describes it reads it (its
	// policy included), with that binding's owner and drain deadline for the
	// file itself; the file says why it cannot be read, and stands in when
	// no binding says.
	owner, deadline := os.Getuid(), -1
	for _, t := range targets {
		if t.Describe == nil || t.Describe.Host == nil {
			continue
		}
		f.Host = protocol.CleanHost(t.Describe.Host)
		if n, err := strconv.Atoi(t.Config["host_owner_uid"]); err == nil {
			owner = n
		}
		if n, err := strconv.Atoi(t.Config["drain_deadline_s"]); err == nil {
			deadline = n
		}
		break
	}
	h, err := hostNow(owner, deadline)
	if err != nil || f.Host == nil {
		f.Host = &h // unreadable: no binding's older reading stands in
	}
	if err != nil {
		f.HostErr = err.Error()
	}
	return f
}

// liveBindings is this machine's own bindings (machine-level ones and those
// in pools it runs), described, with their live handles, and whether the
// daemon answered at all. It never starts the daemon; it goes on past a
// pool it cannot list and returns the first error.
func liveBindings(ctx context.Context) (out []computeapi.RunTargetView, running bool, first error) {
	cfg, err := config.LoadClient()
	if err != nil {
		return nil, false, err
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	c := client.NewUnix(cfg.SocketPath())
	if _, err := c.Health(ctx); err != nil {
		return nil, false, err
	}
	if l, err := computeclient.HostRunTargetsLive(ctx, c); err != nil {
		first = err
	} else {
		out = ownViews(l)
	}
	ps, err := c.LocalProjects(ctx)
	if err != nil {
		return out, true, cmp.Or(first, err)
	}
	for _, p := range ps {
		pl, err := computeclient.RunTargetsLive(ctx, inPool(c, p.Slug))
		if err != nil {
			first = cmp.Or(first, fmt.Errorf("pool %s: %w", p.Slug, err))
			continue
		}
		out = append(out, ownViews(pl)...)
	}
	return out, true, first
}

// ownViews is a list's bindings this machine serves.
func ownViews(l computeapi.RunTargetList) []computeapi.RunTargetView {
	var out []computeapi.RunTargetView
	for _, v := range l.Targets {
		if v.RunTarget.Host == "" {
			out = append(out, v)
		}
	}
	return out
}
