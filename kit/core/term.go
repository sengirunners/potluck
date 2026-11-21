package core

// The home's term (kit/hometerm) as its daemon changes it, and the keys
// the home answers by.

import (
	"context"
	"fmt"
	"sync"

	"github.com/sengirunners/potluck/kit/hometerm"
)

// termLock makes the daemon's changes to its home's term go one at a time.
// The daemon holds the home's lock (config.LockHome), so no other process
// writes the term meanwhile.
type termLock struct{ mu sync.Mutex }

// ApplyTerm changes this home's term through move (hometerm.Apply), one
// change at a time within this daemon: every change a daemon makes to its
// term goes through here.
func (d *Deps) ApplyTerm(move func(hometerm.Term) (hometerm.Term, error)) (hometerm.Term, error) {
	d.term.mu.Lock()
	defer d.term.mu.Unlock()
	return hometerm.Apply(d.Config.Home, move)
}

// HomeKeyer is a transport that can say the fingerprint a member pins this
// home by on it (the tailnet: its node's; the tunnel: its key's).
type HomeKeyer interface {
	HomeFingerprint(ctx context.Context) (string, error)
}

// HomeFingerprint is the fingerprint a member pins this home by on via: an
// error when no such transport is wired, it cannot say, or it is not up.
func (d *Deps) HomeFingerprint(ctx context.Context, via string) (string, error) {
	k, ok := d.Transport(via).(HomeKeyer)
	if !ok {
		return "", fmt.Errorf("this daemon has no %q transport that names its key", via)
	}
	fp, err := k.HomeFingerprint(ctx)
	if err == nil && fp == "" {
		err = fmt.Errorf("the %s transport named no key", via)
	}
	return fp, err
}
