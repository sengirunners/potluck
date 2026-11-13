package runtarget

import (
	"context"
	"slices"
	"sort"
)

// dropForgotten is a teamlink Hooks.DropForgotten: this machine's owner
// forgot project id's authority key and chose to drop what this host kept
// for the id. The id's key tag changes first (raiseKeyEpoch), so whoever the
// id is shared with next reaches none of its old volumes and repeats none of
// its request keys; then the id leaves every binding's share list, and its
// handles leave the ledger, so an authority that joins the id later reaches
// no binding until the owner shares one with it again. Not changed: the
// boxes behind those handles and the volumes under the old tag (not
// removed here; their drivers still list them to this machine's owner). A
// host whose files are not migrated, or cannot be read, fails; each step
// is written before the next, so a failure leaves the steps before it done
// and says so (the key stays bound, and the forget can run again).
func (s *Service) dropForgotten(ctx context.Context, project string) ([]string, error) {
	if err := s.hostReady(); err != nil {
		return nil, err
	}
	if err := s.raiseKeyEpoch(ctx, project); err != nil {
		return nil, err
	}
	dropped := []string{"the pool's key tag here (volumes made under it stay, out of reach)"}
	s.host.mu.Lock()
	ts, err := s.hostBindings()
	if err == nil {
		changed := false
		for i, t := range ts {
			if sharedWith(t, project) {
				ts[i].Share = slices.DeleteFunc(slices.Clone(t.Share), func(p string) bool { return p == project })
				dropped = append(dropped, "share of host binding "+t.Name)
				changed = true
			}
		}
		if changed {
			err = s.saveHostBindings(ts)
		}
	}
	s.host.mu.Unlock()
	if err != nil {
		return dropped, err
	}
	s.host.ledgerMu.Lock()
	defer s.host.ledgerMu.Unlock()
	led, err := s.ledgerLoad()
	if err != nil {
		return dropped, err
	}
	var handles []string
	for h, e := range led.Handles {
		if e.Project == project {
			handles = append(handles, h)
			delete(led.Handles, h)
		}
	}
	if len(handles) == 0 {
		return dropped, nil
	}
	if err := s.ledgerWrite(led); err != nil {
		return dropped, err
	}
	sort.Strings(handles)
	for _, h := range handles {
		dropped = append(dropped, "handle "+h)
	}
	return dropped, nil
}
