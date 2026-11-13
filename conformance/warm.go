package conformance

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/sengirunners/potluck/protocol"
)

// Warm boxes (SPEC 20 "Boxes", Warm boxes; zm-eakh.12.5.28): a box driver
// may keep boxes booted and idle and say so in describe's warm
// ({"target", "ready"}). A warm box is nobody's until an acquire takes it:
// list shows none, its lease starts at the take, it is a fresh box (nothing
// of an earlier box's), and the pool refills after a take.

type warmReport struct {
	Target *int `json:"target"`
	Ready  *int `json:"ready"`
}

// waitWarmReady waits for describe's warm.ready to reach at least one.
func (r *run) waitWarmReady() error {
	deadline := time.Now().Add(r.opt.RunWait)
	for {
		var d description
		e, err := r.call(verbDescribe, "", nil, &d)
		if err = must(e, err); err != nil {
			return err
		}
		if d.Warm != nil && d.Warm.Ready != nil && *d.Warm.Ready >= 1 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("describe's warm %+v: no warm box ready within %s", d.Warm, r.opt.RunWait)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func (r *run) warm() {
	if r.kind != "box" {
		return
	}
	if r.desc.Warm == nil {
		r.skip("warm boxes", "describe says no warm (the binding keeps none)")
		return
	}
	w := r.desc.Warm
	var shape error
	if w.Target == nil || w.Ready == nil || *w.Target < 1 || *w.Ready < 0 || *w.Ready > *w.Target {
		shape = fmt.Errorf("warm %+v: want target at least 1 and ready 0..target", w)
	}
	if !r.add("describe's warm says how many boxes it keeps warm and how many are ready", shape) {
		return
	}
	err := r.waitWarmReady()
	if err == nil {
		var es []listEntry
		if es, err = r.list(); err == nil {
			for _, e := range es {
				if !r.ours(e) {
					err = fmt.Errorf("list shows %s (request_id %q), which this run never acquired", e.Handle, e.RequestID)
					break
				}
			}
		}
	}
	if !r.add("a warm box is nobody's: list shows no handle for it", err) {
		return
	}
	time.Sleep(3 * time.Second)         // warm time is not the taker's: past acquireShape's 2 s slack
	a, err := r.acquireAny(r.longTTL()) // acquireShape: expires_at is ttl_s from the take
	defer r.endHeld(a)
	if !r.add("a taken warm box's lease starts at the take (expires_at ttl_s from the acquire)", err) {
		return
	}
	mark := "warm-" + r.tag
	dirs, err := r.leaveMark(a.Handle, mark) // leftover.go
	if err != nil {
		r.add("a warm box is a fresh box: nothing of an earlier box's", err)
		return
	}
	r.endHeld(a)
	err = r.waitWarmReady()
	r.add("the pool refills after a take", err)
	if err != nil {
		return
	}
	err = r.nextBoxes(func(h string) error { return r.noLeftover(h, mark, dirs) })
	r.add("a warm box is a fresh box: nothing of an earlier box's", err)
	const fresh = "a warm box with a shelf is a fresh box: no links, wants or HOME of an earlier box's, the same shelf"
	switch on, p, err := r.shelfOn(); { // shelf.go
	case !on:
		r.skip(fresh, "describe lists no shelf capability")
	case err != nil:
		r.add(fresh, err)
	default:
		r.add(fresh, r.warmShelf(p))
	}
	const succ = "a box with a volume is never a warm box's successor"
	if r.desc.Capabilities == nil || !slices.Contains(*r.desc.Capabilities, protocol.CapVolumes) {
		r.skip(succ, "describe lists no volumes capability")
		return
	}
	r.add(succ, r.warmAfterVolume())
}

// nextBoxes runs check on each of the next target+1 boxes, so a box put
// anywhere in the pool is reached. It first gives the pool up to RunWait to
// fill to target (a box put back late is in it then too), then holds the
// boxes at once. When the binding has no room for another, it ends the boxes
// it holds and takes the rest one at a time: a full binding never ends the
// takes early.
func (r *run) nextBoxes(check func(handle string) error) error {
	r.waitWarmFull()
	var hold []held
	defer func() {
		for _, n := range hold {
			r.endHeld(n)
		}
	}()
	take := func() (held, *protocol.Error, error) {
		n := held{key: r.key()}
		n.sent = r.boxArgs(n.key, r.longTTL(), false)
		a, e, err := r.acquire(n.sent, r.cfg)
		n.acquired = a
		return n, e, err
	}
	for i := 0; i <= *r.desc.Warm.Target; i++ {
		n, e, err := take()
		if err == nil && e != nil && e.Code == protocol.CodeUnavailable && len(hold) > 0 {
			for _, h := range hold {
				r.endHeld(h)
			}
			hold = nil
			if err = r.waitWarmReady(); err == nil {
				n, e, err = take()
			}
		}
		hold = append(hold, n)
		if err = must(e, err); err == nil {
			err = check(n.Handle)
		}
		if err != nil {
			return fmt.Errorf("box %d after it: %w", i+1, err)
		}
	}
	return nil
}

// waitWarmFull waits up to RunWait for describe's warm.ready to reach
// target. A binding with too few slots may never get there, so the wait
// ending is not a failure.
func (r *run) waitWarmFull() {
	deadline := time.Now().Add(r.opt.RunWait)
	for time.Now().Before(deadline) {
		var d description
		if e, err := r.call(verbDescribe, "", nil, &d); must(e, err) != nil ||
			d.Warm != nil && d.Warm.Ready != nil && d.Warm.Target != nil && *d.Warm.Ready >= *d.Warm.Target {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// warmAfterVolume: a box that cow-mounted a volume and wrote through it
// ends; none of the next boxes (nextBoxes) has anything at the mount path.
func (r *run) warmAfterVolume() error {
	name := strings.ToLower("zmconf-warm-" + r.tag)
	v, e, err := r.acquireVol("zmconf-warm-"+r.tag, name, "cow")
	defer r.endHeld(v)
	if err = must(e, err); err != nil {
		return err
	}
	mark := "warm-vol-" + r.tag
	if err := r.cowMark(v.Handle, mark); err != nil { // volumes_cow.go
		return err
	}
	r.endHeld(v)
	if err := r.waitWarmReady(); err != nil {
		return err
	}
	return r.nextBoxes(func(h string) error { return r.nothingAt(h, mark) })
}
