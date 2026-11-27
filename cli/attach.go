package cli

// Which daemon the potluck command uses: one daemon per machine
// (docs/daemon.md "One daemon per machine"). A daemon built on Potluck's kit
// is the machine's Potluck daemon, whichever program runs it; with no home
// of its own, potluck uses a running one of another program (its machine
// record, kit/daemonrecord) and never starts a second daemon beside it.

import (
	"context"
	"errors"
	"fmt"
	"github.com/sengirunners/potluck/attach"
	"path/filepath"

	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/daemonrecord"
	"github.com/sengirunners/potluck/protocol"
)

// daemonHome is where one run's commands find the daemon (package attach).
type daemonHome = attach.Home

// home is this run's daemonHome, decided once per run: attach.Choose's in
// the potluck program's own run; in a mounted run, the home of the host's
// client config (the host's client decides which daemon, so this rule is
// the potluck program's alone).
func (a *app) home() (daemonHome, error) {
	if a.homeDone {
		return a.homeV, a.homeErr
	}
	a.homeDone = true
	if !a.own {
		_, cfg, err := a.host.Client()
		if err == nil && (cfg == nil || cfg.Home == "") {
			err = errors.New("the host's client gave no home")
		}
		a.homeV, a.homeErr = daemonHome{Cfg: cfg, Existed: true}, err
		return a.homeV, a.homeErr
	}
	ctx := context.Background()
	if a.running != nil && a.running.Context() != nil {
		ctx = a.running.Context()
	}
	a.homeV, a.homeErr = attach.Resolve(ctx, attach.Vars{Home: EnvHome, BoxCredential: EnvBoxCredential})
	return a.homeV, a.homeErr
}

// loadClient is this run's client config (home): potluck's own home's, or
// the home of the other program's daemon it uses.
func (a *app) loadClient() (*config.Config, error) {
	h, err := a.home()
	return h.Cfg, err
}

// attachedTo is the other program's daemon this run uses (nil: potluck's own).
func (a *app) attachedTo() *daemonrecord.Record {
	h, err := a.home()
	if err != nil {
		return nil
	}
	return h.Other
}

// othersHome says, when this run's home is another program's, whose and
// whether its daemon runs ("" when the home is potluck's).
func othersHome(h daemonHome) string {
	switch {
	case h.Other != nil:
		return fmt.Sprintf("this machine's Potluck daemon is %s's (home %s, pid %d), and potluck's commands use it",
			h.Other.Program, attach.Tilde(h.Other.Home), h.Other.PID)
	case h.Owner != "":
		return fmt.Sprintf("%s is %s's home, and no daemon of it that serves potluck's commands runs there; potluck never starts a daemon on it",
			attach.Tilde(h.Cfg.Home), h.Owner)
	}
	return ""
}

// notWhileAttached refuses a command that would start or stop a daemon, or
// write the home's files itself rather than through the daemon, while the
// home is another program's: that program owns them.
func (a *app) notWhileAttached(what string) error {
	h, err := a.home()
	if err != nil {
		return err
	}
	why := othersHome(h)
	if why == "" {
		return nil
	}
	return fmt.Errorf("%s: %s; that belongs to %s: use that program's command (or set %s=<dir> for a separate "+programName+" daemon)",
		what, why, h.Owner, EnvHome)
}

// driverDirs are the directories the daemon runs drivers from
// (protocol.DriverDirs): its program's directory, then <home>/drivers. For
// another program's daemon that is its binary's directory, not potluck's.
func (a *app) driverDirs(cfg *config.Config) []string {
	if o := a.attachedTo(); o != nil {
		var dirs []string
		if o.Exe != "" {
			dirs = append(dirs, filepath.Dir(o.Exe))
		}
		return append(dirs, filepath.Join(o.Home, protocol.DriversDir))
	}
	return protocol.DriverDirs(cfg.Home)
}

// refuseBesideOther refuses `potluck daemon` where potluck's commands use
// another program's daemon (the machine has its Potluck daemon already),
// or on another program's home.
func (a *app) refuseBesideOther() error {
	h, err := a.home()
	if err != nil {
		return err
	}
	switch {
	case h.Other != nil:
		return fmt.Errorf("this machine already has a Potluck daemon: %s's (home %s, pid %d), and potluck's commands use it; "+
			"to run a separate "+programName+" daemon on purpose, set %s=<dir>", h.Other.Program, attach.Tilde(h.Other.Home), h.Other.PID, EnvHome)
	case h.Owner != "":
		return fmt.Errorf("%s; start it with that program's command, or set %s=<dir> for a separate "+programName+" daemon",
			othersHome(h), EnvHome)
	}
	return nil
}

// otherStarting is the daemon of another program that is restarting or still
// starting while potluck has no home yet (attach.Starting); nil: none.
func (a *app) otherStarting(ctx context.Context) *daemonrecord.Record {
	h, err := a.home()
	if err != nil {
		return nil
	}
	return attach.Starting(ctx, attach.Vars{Home: EnvHome, BoxCredential: EnvBoxCredential}, h)
}
