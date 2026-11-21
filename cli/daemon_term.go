package cli

// potluck daemon freeze and unfreeze: this machine's owner stops the home
// for a move at its epoch, and serves it again at the same epoch if the
// move is called off (kit/hometerm). A frozen home's daemon is parked: it
// answers only its health and its term (kit/server term.go).

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/hometerm"
)

func (a *app) freezeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "freeze",
		Short: "Stop serving this machine's home for a move, at its epoch: its daemon parks (it answers only its health and term) until unfreeze",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.changeTerm(cmd.Context(), "freeze", (*client.Client).FreezeHome, hometerm.Term.Freeze)
		},
	}
}

func (a *app) unfreezeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "unfreeze",
		Short: "Serve this machine's frozen home again at the same epoch (the move was called off)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.changeTerm(cmd.Context(), "unfreeze", (*client.Client).UnfreezeHome, hometerm.Term.Unfreeze)
		},
	}
}

// changeTerm freezes or unfreezes this machine's home (what). With a
// daemon running, the daemon changes its term (ask) and stops; this waits
// for it to go and starts it again, with the same listeners, parked after
// a freeze and serving after an unfreeze. With none, the term changes under the home's lock
// (move), as the daemon would change it.
func (a *app) changeTerm(ctx context.Context, what string, ask func(*client.Client, context.Context) (api.AuthorityTerm, error),
	move func(hometerm.Term) (hometerm.Term, error)) error {
	if err := a.notWhileAttached(a.cmd("daemon " + what)); err != nil {
		return err
	}
	cfg, err := a.loadClient()
	if err != nil {
		return err
	}
	c := client.NewUnix(cfg.SocketPath())
	h, err := c.Health(ctx)
	if client.NotRunning(err) {
		t, err := applyTermOffline(cfg.Home, move)
		if err != nil {
			return err
		}
		return a.sayTerm(api.AuthorityTerm{State: string(t.State), Epoch: t.Epoch}, "no daemon was running")
	}
	if err != nil {
		return err
	}
	t, err := ask(c, ctx)
	if err != nil {
		return err
	}
	if !waitGone(ctx, c, h.PID, stopWait) {
		return fmt.Errorf("this home is %s at epoch %d, but its daemon (pid %d) has not stopped %s after; stop it (%s), then start it (%s)",
			t.State, t.Epoch, h.PID, stopWait, a.cmd("daemon stop"), a.cmd("daemon --detach"))
	}
	// The daemon after keeps this one's listeners: a home whose move is
	// called off is reachable on its tailnet again.
	if _, err := a.startDetached(ctx, cfg, h.Tailnet); err != nil {
		return fmt.Errorf("this home is %s at epoch %d, but its daemon did not start again: %w", t.State, t.Epoch, err)
	}
	return a.sayTerm(t, "its daemon was started again")
}

// applyTermOffline changes the term of home, whose daemon is not running,
// through move under the home's daemon and writer locks: a daemon starting
// meanwhile, or another process changing the home, refuses it or is refused.
func applyTermOffline(home string, move func(hometerm.Term) (hometerm.Term, error)) (hometerm.Term, error) {
	release, err := config.LockHome(home)
	if err != nil {
		return hometerm.Term{}, err
	}
	defer release()
	unlock, err := config.LockWriter(home)
	if err != nil {
		return hometerm.Term{}, err
	}
	defer unlock()
	return hometerm.Apply(home, move)
}

// sayTerm prints the home's term after a freeze or unfreeze.
func (a *app) sayTerm(t api.AuthorityTerm, daemon string) error {
	if a.asJSON() {
		return a.printJSON(t)
	}
	if t.State == api.HQServing {
		a.say("this home serves again at epoch %d (%s)", t.Epoch, daemon)
		return nil
	}
	a.say("this home is %s at epoch %d: its daemon answers only its health and term until %s (%s)", t.State, t.Epoch, a.cmd("daemon unfreeze"), daemon)
	return nil
}
