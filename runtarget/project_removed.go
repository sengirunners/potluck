package runtarget

import (
	"context"
	"database/sql"
	"fmt"
	"slices"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/project"
	"github.com/sengirunners/potluck/runtarget/rt"
)

// refuseLive stops the removal of a pool (Registry.OnRemove) while sandboxes
// still run in it, or stay stopped with their disk kept, unless the caller
// forced it. It is here, not in the command, so a sandbox started after the
// command's plan, or a direct call to the API, meets it too. A forced
// removal leaves those sandboxes to run until their lease ends, out of their
// members' reach (their size, bound in the pool's project.toml, goes with it).
func (s *Service) refuseLive(ctx context.Context, _ *sql.Tx, id string, opts project.RemoveOptions) error {
	// An acquire in progress has no row yet, and a box it makes after the
	// removal has none to be found by: hold each binding's lock while counting,
	// and refuse when one is taken.
	for _, name := range s.bindingNames(id) {
		unlock, ok := s.tryLock(id, name)
		if !ok {
			if opts.Force {
				continue // the caller chose to leave what is running
			}
			return api.Errorf(api.CodeConflict, "a sandbox is being made in pool %s (size %s)", s.d.Projects.Slug(id), name).
				WithHint("try again in a moment")
		}
		defer unlock()
	}
	l, err := s.liveIn(ctx, id, "")
	if err != nil {
		return err
	}
	if l.live == 0 || opts.Force {
		return nil
	}
	what := fmt.Sprintf("%d sandbox is", l.live)
	if l.live != 1 {
		what = fmt.Sprintf("%d sandboxes are", l.live)
	}
	if l.stopped > 0 {
		what += fmt.Sprintf(" (%d stopped, with its disk kept)", l.stopped)
	}
	return api.Errorf(api.CodeConflict, "%s still in pool %s", what, s.d.Projects.Slug(id)).
		WithHint("end them first (the member who made each uses sandbox rm), or remove the pool with force: they then run until their lease ends, out of their members' reach")
}

// bindingNames are the names of the bindings project id's sandboxes run on:
// its own and this machine's machine-level ones shared with it.
func (s *Service) bindingNames(id string) []string {
	var names []string
	for _, b := range rt.ProjectTargets(s.d.Projects, id) {
		names = append(names, b.Name)
	}
	s.host.mu.Lock()
	ts, err := s.hostBindings()
	s.host.mu.Unlock()
	if err == nil {
		for _, t := range ts {
			if sharedWith(t, id) {
				names = append(names, t.Name)
			}
		}
	}
	slices.Sort(names)
	return slices.Compact(names)
}

// dropShares ends the machine-level shares of a pool that was removed
// (Registry.OnRemoved): the pool leaves every binding's share list, a binding
// left shared with no pool goes (the file holds none like that), and the
// pool's handles leave the host's ledger, so nothing of this machine still
// names an id that answers to nothing. A pool this machine runs is served
// from its own project.toml, which went with its folder; these are the
// entries that name it from outside (a size shared with it by hand). A host
// whose files are not migrated, or cannot be read, fails: the removal stands
// and the failure is a leftover.
func (s *Service) dropShares(_ context.Context, removed api.ProjectRef) error {
	stop := " (" + config.PotluckCommand("host stop") + " removes them)"
	if err := s.hostReady(); err != nil {
		return fmt.Errorf("this machine's shared sizes could not be checked for pool %s: %w%s", removed.Slug, err, stop)
	}
	s.host.mu.Lock()
	ts, err := s.hostBindings()
	if err != nil {
		s.host.mu.Unlock()
		return fmt.Errorf("this machine's shared sizes could not be read: %w%s", err, stop)
	}
	var gone []string
	var kept []computeapi.HostRunTarget
	changed := false
	for _, t := range ts {
		if !sharedWith(t, removed.ID) {
			kept = append(kept, t)
			continue
		}
		changed = true
		t.Share = slices.DeleteFunc(slices.Clone(t.Share), func(p string) bool { return p == removed.ID })
		if len(t.Share) == 0 {
			gone = append(gone, t.Name)
			continue
		}
		kept = append(kept, t)
	}
	if changed {
		err = s.saveHostBindings(kept)
	}
	s.host.mu.Unlock()
	if err != nil {
		return fmt.Errorf("this machine's shared sizes could not be written; sizes still list pool %s: %w%s", removed.Slug, err, stop)
	}
	for _, n := range gone {
		s.host.cache.Delete(n)
	}
	if _, err := s.dropLedgerHandles(removed.ID); err != nil {
		return fmt.Errorf("this machine's record of boxes made for pool %s could not be cleared: %w", removed.Slug, err)
	}
	return nil
}
