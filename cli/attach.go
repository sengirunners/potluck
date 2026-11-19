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
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/daemonrecord"
	"github.com/sengirunners/potluck/protocol"
	"github.com/sengirunners/potluck/wiring"
)

// daemonHome is where one run's commands find the daemon.
type daemonHome struct {
	// cfg is the client config: potluck's own home's, or (other set) the
	// other program's home's, read only.
	cfg *config.Config
	// other is the running daemon of another program this run uses; nil:
	// potluck's own daemon on cfg's home.
	other *daemonrecord.Record
	// owner is the program, not potluck, whose home cfg's is ("": potluck's
	// or no program's yet): other's, or the program that claimed the home
	// POTLUCK_HOME names while its daemon is not running.
	owner string
	// existed: cfg's home was there before this run.
	existed bool
}

// daemonFacts are what chooseDaemon decides from, read from the
// environment and the disk by resolveHome.
type daemonFacts struct {
	BoxCredential bool   // POTLUCK_BOX_CREDENTIAL is set
	HomeEnv       string // POTLUCK_HOME's directory, absolute ("" when unset)
	OwnHome       bool   // potluck's own home directory (~/.potluck) exists
}

// liveFunc lists the running daemons' records that keep accepts
// (daemonrecord.Live).
type liveFunc func(keep func(daemonrecord.Record) bool) ([]daemonrecord.Record, error)

// chooseDaemon is the one rule for which daemon potluck's commands use:
//
//  1. POTLUCK_BOX_CREDENTIAL set: the authority it names (nil here: no
//     machine daemon at all).
//  2. POTLUCK_HOME set: that home; when a daemon of another program that
//     serves compute runs on it, that daemon (so POTLUCK_HOME chooses
//     between such daemons), else potluck's own on it (also when the
//     records cannot be read: the person named the home).
//  3. Potluck's own home exists: potluck's own.
//  4. Exactly one daemon of another program that serves compute runs: that
//     one. Two or more: refused, naming them.
//  5. Else potluck's own home (made on first use).
//
// live is called only when the rule needs the records, and asked only
// about the daemons the rule could choose.
func chooseDaemon(f daemonFacts, live liveFunc) (*daemonrecord.Record, error) {
	if f.BoxCredential || (f.HomeEnv == "" && f.OwnHome) {
		return nil, nil
	}
	others, err := live(func(r daemonrecord.Record) bool {
		return r.Program != config.Program && r.Serves(wiring.ComputeModules...) && (f.HomeEnv == "" || daemonrecord.SameHome(r.Home, f.HomeEnv))
	})
	switch {
	case err != nil && f.HomeEnv != "":
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("read this machine's daemon records: %w", err)
	}
	switch {
	case len(others) == 0:
		return nil, nil
	case len(others) == 1 || f.HomeEnv != "":
		return &others[0], nil
	}
	var names []string
	for _, r := range others {
		names = append(names, fmt.Sprintf("%s's at %s", r.Program, tildePath(r.Home)))
	}
	return nil, fmt.Errorf("this machine runs more than one Potluck daemon (%s) and potluck has no home of its own: "+
		"choose one with %s=<its home>, or set %s to a new directory for potluck's own daemon",
		strings.Join(names, ", "), EnvHome, EnvHome)
}

// home is this run's daemonHome, decided once per run: chooseDaemon's in
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
		a.homeV, a.homeErr = daemonHome{cfg: cfg, existed: true}, err
		return a.homeV, a.homeErr
	}
	ctx := context.Background()
	if a.running != nil && a.running.Context() != nil {
		ctx = a.running.Context()
	}
	a.homeV, a.homeErr = resolveHome(ctx)
	return a.homeV, a.homeErr
}

// resolveHome reads chooseDaemon's facts and loads the client config of
// the home it chooses.
func resolveHome(ctx context.Context) (daemonHome, error) {
	own, err := config.HomeDir()
	if err != nil {
		return daemonHome{}, err
	}
	isDir := func(p string) bool { fi, err := os.Stat(p); return err == nil && fi.IsDir() }
	f := daemonFacts{BoxCredential: os.Getenv(EnvBoxCredential) != ""}
	if os.Getenv(EnvHome) != "" {
		f.HomeEnv = own
	} else {
		f.OwnHome = isDir(own)
	}
	other, err := chooseDaemon(f, func(keep func(daemonrecord.Record) bool) ([]daemonrecord.Record, error) {
		return daemonrecord.Live(ctx, keep)
	})
	if err != nil {
		return daemonHome{}, err
	}
	if other == nil {
		cfg, err := config.LoadClientFrom(own)
		h := daemonHome{cfg: cfg, existed: isDir(own)}
		if o := config.HomeOwner(own); o != "" && o != config.Program {
			h.owner = o
		}
		return h, err
	}
	cfg, err := config.LoadClientFrom(other.Home)
	if err != nil {
		return daemonHome{}, err
	}
	cfg.Socket = other.Socket
	return daemonHome{cfg: cfg, other: other, owner: other.Program, existed: true}, nil
}

// loadClient is this run's client config (home): potluck's own home's, or
// the home of the other program's daemon it uses.
func (a *app) loadClient() (*config.Config, error) {
	h, err := a.home()
	return h.cfg, err
}

// attachedTo is the other program's daemon this run uses (nil: potluck's own).
func (a *app) attachedTo() *daemonrecord.Record {
	h, err := a.home()
	if err != nil {
		return nil
	}
	return h.other
}

// othersHome says, when this run's home is another program's, whose and
// whether its daemon runs ("" when the home is potluck's).
func (h daemonHome) othersHome() string {
	switch {
	case h.other != nil:
		return fmt.Sprintf("this machine's Potluck daemon is %s's (home %s, pid %d), and potluck's commands use it",
			h.other.Program, tildePath(h.other.Home), h.other.PID)
	case h.owner != "":
		return fmt.Sprintf("%s is %s's home, and no daemon of it that serves potluck's commands runs there; potluck never starts a daemon on it",
			tildePath(h.cfg.Home), h.owner)
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
	why := h.othersHome()
	if why == "" {
		return nil
	}
	return fmt.Errorf("%s: %s; that belongs to %s: use that program's command (or set %s=<dir> for a separate "+programName+" daemon)",
		what, why, h.owner, EnvHome)
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
	case h.other != nil:
		return fmt.Errorf("this machine already has a Potluck daemon: %s's (home %s, pid %d), and potluck's commands use it; "+
			"to run a separate "+programName+" daemon on purpose, set %s=<dir>", h.other.Program, tildePath(h.other.Home), h.other.PID, EnvHome)
	case h.owner != "":
		return fmt.Errorf("%s; start it with that program's command, or set %s=<dir> for a separate "+programName+" daemon",
			h.othersHome(), EnvHome)
	}
	return nil
}

// otherWait bounds how long a command that would make potluck's first home
// waits for a daemon of another program to appear in the records.
var otherWait = 3 * time.Second

// otherStarting is the daemon of another program that serves compute and
// appears in the records within otherWait, when potluck has no home yet
// and the records directory exists (a daemon built on Potluck has run on
// this machine): one restarting, or still starting, must not lose the
// machine to a second daemon of potluck's, which its new home would then
// keep (chooseDaemon's rule 3). nil: none, or potluck's home exists.
func (a *app) otherStarting(ctx context.Context) *daemonrecord.Record {
	h, err := a.home()
	if err != nil || h.existed || h.other != nil || os.Getenv(EnvHome) != "" {
		return nil
	}
	if fi, err := os.Stat(daemonrecord.Dir()); err != nil || !fi.IsDir() {
		return nil
	}
	keep := func(r daemonrecord.Record) bool {
		return r.Program != config.Program && r.Serves(wiring.ComputeModules...)
	}
	for deadline := time.Now().Add(otherWait); ; {
		if live, err := daemonrecord.Live(ctx, keep); err == nil && len(live) > 0 {
			return &live[0]
		}
		if !time.Now().Before(deadline) {
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(200 * time.Millisecond):
		}
	}
}
