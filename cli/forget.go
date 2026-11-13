package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
)

// forgetCmd is potluck forget: this machine's owner forgets the authority
// key a pool's id is bound to (kit/teamlink/project_keys.go), so the pool's
// own machine can be joined again after it got a new key or is reached
// another way. It asks first, and takes no default for what this machine
// kept for the pool as a host.
func (a *app) forgetCmd() *cobra.Command {
	var keep, drop, yes bool
	cmd := &cobra.Command{
		Use:   "forget <pool id> (--keep-host-state | --drop-host-state) [--yes]",
		Short: "Forget the key a pool's id is tied to, so its machine can be joined at a new key",
		Long: `The first join of a pool ties the pool's id to the key of the machine that
runs it. A later join that presents the same id at another key is refused,
so another machine cannot take over the pool's sandboxes and shares here.
When the pool's own machine got a new key (a new tailnet node) or is now
reached another way (a tunnel code), forget the old key here; the next join
of the id ties it again, to whatever key that join pins.

Leave the pool first. Say what becomes of what this machine kept for the pool
as a host: --keep-host-state keeps its shares and the pool's sandboxes here
for whichever machine joins the id next; --drop-host-state takes the id off
every share, forgets the pool's sandboxes here (they keep running until you
remove them) and gives the id a new key tag: its volumes on this machine stay
on disk, but a machine shared with the id again meets none of them and none of
its old request ids. Kept, a machine shared with the id again meets them. It
asks before it forgets; --yes does not ask.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if keep == drop {
				return api.Errorf(api.CodeBadRequest, "say what becomes of what this machine kept for the pool as a host").
					WithHint("pass --keep-host-state or --drop-host-state")
			}
			state := api.ForgetKeepHostState
			if drop {
				state = api.ForgetDropHostState
			}
			return call(a, cmd, func(ctx context.Context, c *client.Client) (api.ForgetProjectKeyResponse, error) {
				k, err := c.ProjectKey(ctx, args[0])
				if err != nil {
					return api.ForgetProjectKeyResponse{}, err
				}
				if !yes {
					if err := a.confirmForget(k, state); err != nil {
						return api.ForgetProjectKeyResponse{}, err
					}
				}
				return c.ForgetProjectKey(ctx, k.Project, api.ForgetProjectKeyRequest{HostState: state, Fingerprint: k.Fingerprint})
			}, func(r api.ForgetProjectKeyResponse) string {
				s := fmt.Sprintf("forgot key %s over %s for pool %s; the next join of the id ties it again", r.Key.Fingerprint, r.Key.Via, r.Key.Project)
				switch {
				case r.HostState == api.ForgetKeepHostState:
					s += "\nkept this machine's shares and the pool's sandboxes here"
				case len(r.Dropped) == 0:
					s += "\nthis machine kept nothing for the pool as a host"
				default:
					s += "\ndropped: " + strings.Join(r.Dropped, ", ")
				}
				return s
			})
		},
	}
	cmd.Flags().BoolVar(&keep, "keep-host-state", false, "keep this machine's shares and the pool's sandboxes here for the pool's id")
	cmd.Flags().BoolVar(&drop, "drop-host-state", false, "drop this machine's shares, the pool's sandboxes here and its key tag for the pool's id")
	cmd.Flags().BoolVar(&yes, "yes", false, "do not ask")
	return cmd
}

// confirmForget shows the bound key and reads "yes" from stdin.
func (a *app) confirmForget(k api.ProjectKey, state string) error {
	what := "keeps this machine's shares and the pool's sandboxes here"
	if state == api.ForgetDropHostState {
		what = "drops this machine's shares, the pool's sandboxes here and the pool's key tag (volumes made under it stay, out of reach)"
	}
	fmt.Fprintf(a.stderr, "pool %s is tied to the key of %s (%s over %s, since %s).\n"+
		"Forgetting it lets the next join of this id tie it to any key, and %s.\n"+
		"Type yes to forget it: ", k.Project, k.Authority, k.Fingerprint, k.Via, k.Bound.Format("2006-01-02 15:04"), what)
	line, err := bufio.NewReader(a.stdin).ReadString('\n')
	if strings.TrimSpace(line) == "yes" {
		return nil
	}
	if err != nil && line == "" {
		err = errors.New("no answer")
	} else {
		err = errors.New("the answer was not yes")
	}
	return api.Errorf(api.CodeBadRequest, "nothing was forgotten: %v", err).WithHint("run it again and type yes, or pass --yes")
}
