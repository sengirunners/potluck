package conformance

import (
	"fmt"
	"maps"
	"strings"
	"time"

	"github.com/sengirunners/potluck/protocol"
)

// reclaim has the machine's owner take it back while a resource is live: the
// reclaim policy in --reclaim-config says no once "{window}" closes (SPEC 20,
// "Reclaim"). From the first status until the window closes the suite does
// not call the driver: a target that only reclaims when asked fails.
func (r *run) reclaim() {
	const ends = "a reclaim ends the resource with no call to the driver"
	if len(r.opt.ReclaimConfig) == 0 {
		r.skip(ends, "no --reclaim-config: how an owner takes a target back is the driver's config")
		return
	}
	closes := time.Now().Add(r.opt.ReclaimIn)
	window := closes.Add(-time.Hour).Format("15:04:05") + "-" + closes.Format("15:04:05")
	cfg := maps.Clone(r.cfg)
	for k, v := range r.opt.ReclaimConfig {
		cfg[k] = strings.ReplaceAll(v, "{window}", window)
	}
	h, err := r.acquireIn(cfg, r.longTTL())
	if !r.add("acquire while the owner lets it", err) {
		return
	}
	if h.beat != "" {
		err = r.waitBeat(h.beat)
	}
	if err == nil {
		err = r.alive(h)
	}
	if err == nil && !time.Now().Before(closes) {
		err = fmt.Errorf("the window closed before the resource was seen alive: raise --reclaim-in past an acquire")
	}
	r.add("alive before the owner takes it back", err)
	deadline := closes.Add(r.opt.Grace)
	r.sleep(time.Until(closes))
	switch {
	case h.beat != "":
		r.add(ends, r.waitStopped(deadline, h.beat))
	case r.kind == "db" && r.opt.DBExists != nil:
		for err = r.gone(h.acquired, false); err != nil && time.Now().Before(deadline) && r.ctx.Err() == nil; err = r.gone(h.acquired, false) {
			r.sleep(tick)
		}
		r.add(ends, err)
	default:
		r.skip(ends, "the resource cannot be seen here: only status is checked")
	}
	st, err := r.pollState(h.Handle, deadline, "reclaimed", "ended", "expired", "released", "lost")
	if err == nil {
		err = wantState(st, "reclaimed", nil)
		r.endSpend(st)
	}
	if r.add("status after a reclaim: reclaimed, no exit", err) {
		delete(r.live, h.Handle)
	}
	r.add("list shows it reclaimed", r.listed(h.Handle, h.key, "reclaimed"))
	rel, err := r.release(h.Handle)
	if err == nil {
		err = wantRelease(rel, false, nil)
	}
	r.add("release after a reclaim: released false, no exit", err)

	var d description
	err = must(r.callCfg(verbDescribe, "", nil, cfg, &d))
	if err == nil && (d.Available == nil || *d.Available || d.Why == "") {
		err = fmt.Errorf("available %v, why %q: want false, with why", deref2(d.Available), d.Why)
	}
	r.add("describe while the owner has it: available false, with why", err)
	key := r.key()
	sent := r.runArgs(key, r.shortTTL(), "exit", "0")
	switch r.kind {
	case "db":
		sent = r.dbArgs(key, r.dbName(), r.shortTTL())
	case "box":
		sent = r.boxArgs(key, r.shortTTL(), false)
	}
	a, e, err := r.acquire(sent, cfg)
	if err == nil && e == nil && a.Handle != "" {
		r.endHeld(held{acquired: a})
	}
	r.add("acquire while the owner has it answers unavailable", wantCode(e, err, protocol.CodeUnavailable))
}

func deref2(b *bool) any {
	if b == nil {
		return "absent"
	}
	return *b
}
