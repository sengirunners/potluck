// Package attach is the one rule for which daemon a Potluck client uses: one
// daemon per machine (docs/daemon.md "One daemon per machine"). A daemon built
// on Potluck's kit is the machine's Potluck daemon, whichever program runs it;
// with no home of its own, a client of Potluck's (the potluck command, the
// E2B gateway) uses a running one of another program (its machine record,
// kit/daemonrecord) and never starts, or makes a home for, a second daemon
// beside it.
package attach

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/daemonrecord"
	"github.com/sengirunners/potluck/wiring"
)

// Home is where one run's commands find the daemon.
type Home struct {
	// Cfg is the client config: potluck's own home's, or (Other set) the
	// other program's home's, read only.
	Cfg *config.Config
	// Other is the running daemon of another program this run uses; nil:
	// potluck's own daemon on Cfg's home.
	Other *daemonrecord.Record
	// Owner is the program, not potluck, whose home Cfg is ("": potluck's
	// or no program's yet): Other's, or the program that claimed the home
	// POTLUCK_HOME names while its daemon is not running.
	Owner string
	// Existed: Cfg's home was there before this run.
	Existed bool
}

// Facts are what Choose decides from, read from the
// environment and the disk by Resolve.
type Facts struct {
	BoxCredential bool   // POTLUCK_BOX_CREDENTIAL is set
	HomeEnv       string // POTLUCK_HOME's directory, absolute ("" when unset)
	OwnHome       bool   // potluck's own home directory (~/.potluck) exists
}

// LiveFunc lists the running daemons' records that keep accepts
// (daemonrecord.Live).
type LiveFunc func(keep func(daemonrecord.Record) bool) ([]daemonrecord.Record, error)

// Choose is the one rule for which daemon potluck's commands use:
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
func Choose(f Facts, live LiveFunc) (*daemonrecord.Record, error) {
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
		names = append(names, fmt.Sprintf("%s's at %s", r.Program, Tilde(r.Home)))
	}
	return nil, fmt.Errorf("this machine runs more than one Potluck daemon (%s) and potluck has no home of its own: "+
		"choose one with %s=<its home>, or set %s to a new directory for potluck's own daemon",
		strings.Join(names, ", "), config.HomeEnv(), config.HomeEnv())
}

// Vars name the environment variables Resolve reads: the one that sets the
// home (POTLUCK_HOME: potluck's commands and the gateway both take it from
// there) and the one that holds a box credential ("" when the caller has
// none).
type Vars struct{ Home, BoxCredential string }

// Resolve reads Choose's facts and loads the client config of the home it
// chooses. It creates nothing: a caller that writes state decides where, and
// never under another program's home (Home.Other, Home.Owner).
func Resolve(ctx context.Context, v Vars) (Home, error) {
	own, err := config.HomeDir()
	if err != nil {
		return Home{}, err
	}
	isDir := func(p string) bool { fi, err := os.Stat(p); return err == nil && fi.IsDir() }
	f := Facts{BoxCredential: v.BoxCredential != "" && os.Getenv(v.BoxCredential) != ""}
	if v.Home != "" && os.Getenv(v.Home) != "" {
		f.HomeEnv = own
	} else {
		f.OwnHome = isDir(own)
	}
	other, err := Choose(f, func(keep func(daemonrecord.Record) bool) ([]daemonrecord.Record, error) {
		return daemonrecord.Live(ctx, keep)
	})
	if err != nil {
		return Home{}, err
	}
	if other == nil {
		cfg, err := config.LoadClientFrom(own)
		h := Home{Cfg: cfg, Existed: isDir(own)}
		if o := config.HomeOwner(own); o != "" && o != config.Program {
			h.Owner = o
		}
		return h, err
	}
	cfg, err := config.LoadClientFrom(other.Home)
	if err != nil {
		return Home{}, err
	}
	cfg.Socket = other.Socket
	return Home{Cfg: cfg, Other: other, Owner: other.Program, Existed: true}, nil
}

// StartWait bounds how long Starting waits for a daemon of another program to
// appear in the records.
var StartWait = 3 * time.Second

// Starting is the daemon of another program that serves compute and appears
// in the records within StartWait, when potluck has no home yet (h.Existed
// false, no other chosen, the home variable unset) and the records directory
// exists (a daemon built on Potluck has run on this machine): one
// restarting, or still starting, must not lose the machine to a second
// daemon of potluck's, whose new home Choose's rule 3 would then keep. A
// caller that would make potluck's home asks first. nil: none, or potluck's
// home exists.
func Starting(ctx context.Context, v Vars, h Home) *daemonrecord.Record {
	if h.Existed || h.Other != nil || (v.Home != "" && os.Getenv(v.Home) != "") {
		return nil
	}
	if fi, err := os.Stat(daemonrecord.Dir()); err != nil || !fi.IsDir() {
		return nil
	}
	keep := func(r daemonrecord.Record) bool {
		return r.Program != config.Program && r.Serves(wiring.ComputeModules...)
	}
	for deadline := time.Now().Add(StartWait); ; {
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

// Tilde shows p under the user's home as ~/...
func Tilde(p string) string {
	h, err := os.UserHomeDir()
	if err != nil || h == "" {
		return p
	}
	if rel, err := filepath.Rel(h, p); err == nil && rel != ".." && !strings.HasPrefix(rel, "../") {
		if rel == "." {
			return "~"
		}
		return "~/" + rel
	}
	return p
}
