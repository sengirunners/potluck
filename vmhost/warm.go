package vmhost

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"time"
)

// Warm boxes (zm-eakh.12.5.28, zm-eakh.12.26): a box binding with warm=N
// keeps up to N boxes booted and idle, each under its own supervisor as any
// box is. An acquire that a warm box can serve (the default image, the
// binding's own network, no persist, volumes or jit_config; a commit is
// checked out at the take) takes one at once, and the pool refills in the
// background on the same call. A warm box is nobody's: it has no handle on
// the wire (list, status and the rest do not see it), its time is not spend
// (a taken box's lease, lifetime cap and spend start at the take), and its
// lifetime is the binding's ttl_max_s, after which a fresh one replaces it.
//
// Admission: a warm box holds no admission lease (admission_url) until it is
// taken; its supervisor asks for room at the take, and a refusal ends the
// warm box and answers the acquire unavailable, exactly as a cold acquire
// would. So a warm box never holds a host controller's room (on a host that also
// runs other CI): what it costs the host idle is its runtime's own memory
// (gVisor: about 40 MB host RSS, 9 MiB in its cgroup, measured on a small Linux host),
// not the size's. It does hold one of the binding's slots, so warm boxes
// never add to what the binding may run; a cold acquire that finds no slot
// free ends a warm box that is still booting, or the oldest ready one, and
// takes its slot: warm boxes are the first reclaimed. The owner's reclaim
// policy (window, idle) ends them as it ends any box, and no warm box is made
// while it says no.

// warmPoll is how often a warm box's supervisor looks for its take, and acquire for the box's ready.
const warmPoll = 20 * time.Millisecond

// warmBackoff is how long a warm box that failed to boot stops the pool refilling.
const warmBackoff = time.Minute

// warmAcquire is what of an acquire a warm box must match.
type warmAcquire struct {
	image    string
	jit      bool
	persist  bool
	volumes  bool
	net      string
	ref, cmt string
}

// warmServes says whether a warm box can serve the acquire.
func (d *drv) warmServes(a warmAcquire) bool {
	return d.cfg.Kind == "box" && d.cfg.Warm > 0 && a.image == d.cfg.Image && !a.jit && !a.persist && !a.volumes &&
		d.cfg.jobNet(false, a.net).OCINetwork == d.cfg.jobNet(false, "").OCINetwork
}

// takeWarm gives a ready warm box to an acquire: its key, lease and clock
// from now. Its supervisor sees the take (waitTaken). Lock held.
func (d *drv) takeWarm(key string, ttl int, a warmAcquire) (record, bool) {
	want, err := d.shelfNow()
	if err != nil {
		return record{}, false // the cold path fails as a create would
	}
	for _, r := range d.st.all() {
		if !r.Warm || r.State != stReady || r.Target != d.cfg.Target || !alive(r.Supervisor) {
			continue
		}
		if r.Image != d.cfg.Image { // booted before a rebind moved the pin: its digest is off the binding's pins
			d.endWarm(r, warmOffPin)
			continue
		}
		if !d.warmShelfMatches(r, want) {
			continue
		}
		now := time.Now().UTC()
		r.Warm, r.Key, r.TTL, r.Net, r.Ref, r.Commit = false, key, ttl, a.net, a.ref, a.cmt
		r.State, r.Start, r.RunStart = stStarting, now, time.Time{}
		if d.st.save(r) != nil {
			continue
		}
		return r, true
	}
	return record{}, false
}

// warmLive counts this binding's warm boxes, booting or ready. Lock held.
func (d *drv) warmLive() (n, ready int) {
	for _, r := range d.st.all() {
		if r.Warm && r.live() && r.Target == d.cfg.Target {
			n++
			if r.State == stReady {
				ready++
			}
		}
	}
	return n, ready
}

// fillWarm starts warm boxes up to warm, in free slots, while the reclaim
// policy allows and no warm box failed lately. Lock held.
func (d *drv) fillWarm() {
	if d.cfg.Kind != "box" {
		return
	}
	d.endOffPinWarm()
	d.replaceStaleWarm()
	n, _ := d.warmLive()
	for ; n > d.cfg.Warm && d.preemptWarm(""); n-- { // the owner lowered warm
	}
	if d.cfg.Warm <= 0 || n >= d.cfg.Warm {
		return
	}
	if ok, _ := d.cfg.admitsNow(); !ok {
		return
	}
	for _, r := range d.st.all() {
		if r.Warm && r.State == stFailed && time.Since(r.End) < warmBackoff {
			return
		}
	}
	if fi, err := os.Stat(d.st.warmRefusedFile()); err == nil && time.Since(fi.ModTime()) < warmBackoff {
		return // a take was refused (no room at the take): booting more now would only churn
	}
	os, err := d.vm.OSOf(d.cfg.Image)
	if err != nil {
		return
	}
	for ; n < d.cfg.Warm; n++ {
		if free, err := d.free(os); err != nil || free == 0 || d.keepFree() != nil {
			return
		}
		b := make([]byte, 6)
		_, _ = rand.Read(b)
		id := "r" + hex.EncodeToString(b)
		r := record{Handle: d.cfg.Kind + ":" + d.cfg.Target + "/" + id, Kind: d.cfg.Kind, Target: d.cfg.Target, ID: id, Warm: true,
			State: stStarting, OS: os, VM: d.st.vmPrefixNew() + id, Image: d.cfg.Image, Remote: d.cfg.Remote, TTL: d.cfg.TTLMax,
			Start: time.Now().UTC(), Log: filepath.Join(d.st.dir, "logs", id+".log"), Config: d.rawConfig(), Digest: digestOf(d.cfg.Image)}
		if d.st.save(r) != nil {
			return
		}
		pid, err := d.spawnSupervisor(id, "")
		if err != nil {
			d.st.remove(r)
			return
		}
		r.Supervisor = pid
		_ = d.st.save(r)
	}
}

// warmOffPin is why a warm box booted on an image its binding no longer lends ended.
const warmOffPin = "the binding's image changed since it booted"

// endOffPinWarm ends each live warm box of this binding booted on an image
// other than the binding's default now: a rebind moved the pin, and a box
// handed out from the old one would boot a digest none of the binding's pins
// name, and a pool's authority does not check a plain acquire's digest.
// Lock held.
func (d *drv) endOffPinWarm() {
	for _, r := range d.st.all() {
		if r.Warm && r.live() && r.Target == d.cfg.Target && r.Image != d.cfg.Image {
			d.endWarm(r, warmOffPin)
		}
	}
}

// preemptWarm ends one warm box (of os; "" any) so a cold acquire gets its
// slot: one still booting first, else the oldest ready one. Lock held.
func (d *drv) preemptWarm(os string) bool {
	var pick *record
	for _, r := range d.st.all() {
		if !r.Warm || !r.live() || os != "" && r.OS != os || r.Target != d.cfg.Target {
			continue
		}
		if pick == nil || pick.State == stReady && (r.State != stReady || r.Start.Before(pick.Start)) {
			c := r
			pick = &c
		}
	}
	if pick == nil {
		return false
	}
	return d.endWarm(*pick, "a cold acquire took its slot")
}

// endWarm ends warm box r. Lock held.
func (d *drv) endWarm(r record, why string) bool {
	r.State, r.End, r.Why = stReleased, time.Now().UTC(), why
	if d.st.save(r) != nil {
		return false
	}
	r.bindLayer(d.vm).Destroy(r.VM, r.RunPID)
	return true
}

// warmShelfEvery is how often fillWarm (every verb's sweep) compares the
// ready warm boxes with the shelf: it costs a CLI call per box, under the
// lock. The take checks its box every time, so this only paces replacement.
var warmShelfEvery = 10 * time.Second

func (s store) warmShelfFile() string { return filepath.Join(s.dir, "warm-shelf-checked") }

// replaceStaleWarm ends ready warm boxes made for the other shelf state (the
// volume appeared or went since), at most once per warmShelfEvery, so the
// pool refills with boxes that match describe. Lock held.
func (d *drv) replaceStaleWarm() {
	if _, ready := d.warmLive(); ready == 0 {
		return
	}
	if fi, err := os.Stat(d.st.warmShelfFile()); err == nil && time.Since(fi.ModTime()) < warmShelfEvery {
		return
	}
	_ = os.WriteFile(d.st.warmShelfFile(), nil, 0o600)
	want, err := d.shelfNow()
	if err != nil {
		return
	}
	for _, r := range d.st.all() {
		if r.Warm && r.State == stReady && r.Target == d.cfg.Target && alive(r.Supervisor) {
			d.warmShelfMatches(r, want)
		}
	}
}

// shelfNow is whether a box made now gets the shelf, as describe lists it
// (shelf.go); false on a layer with no shelf.
func (d *drv) shelfNow() (bool, error) {
	if sh, ok := d.vm.(shelfer); ok {
		return sh.ShelfMounted()
	}
	return false, nil
}

// warmShelfMatches says whether ready warm box r has the shelf exactly when
// want says a box made now would, and ends it when not: a warm box made
// before the shelf appeared, or after it went, is never handed out, nor one
// whose shelf mount BoxShelf refuses. Mounts it cannot read make it false but
// leave the box (the error may pass; its lifetime still ends it). Lock held.
func (d *drv) warmShelfMatches(r record, want bool) bool {
	sh, ok := r.bindLayer(d.vm).(shelfer)
	if !ok {
		return true
	}
	has, err := sh.BoxShelf(r.VM)
	if err != nil && !errors.Is(err, errShelfMountWrong) {
		return false
	}
	if err != nil || has != want {
		d.endWarm(r, "its shelf is not the binding's now")
		return false
	}
	return true
}

func (s store) warmRefusedFile() string { return filepath.Join(s.dir, "warm-refused") }

// warmRefused marks a refused take (its admission said no room, or its
// checkout failed): fillWarm waits warmBackoff before booting more.
func (s store) warmRefused() { _ = os.WriteFile(s.warmRefusedFile(), nil, 0o600) }

// warmDescribe is describe's account of the pool.
func (d *drv) warmDescribe() map[string]any {
	n, ready := 0, 0
	_ = d.st.locked(func() error { n, ready = d.warmLive(); return nil })
	return map[string]any{"target": d.cfg.Warm, "ready": ready, "booting": n - ready}
}

// waitTaken holds a ready warm box until an acquire takes it (true), or it
// ends (false): released (preempted), its lifetime up, a reclaim, the VM gone.
func (s *sup) waitTaken(exited <-chan struct{}) bool {
	lastPolicy := time.Time{}
	for {
		r, err := s.st.load(s.id)
		switch {
		case err != nil || !r.live():
			return false
		case !r.Warm:
			return r.State == stStarting
		case time.Now().After(r.Deadline):
			s.vm.Destroy(r.VM, s.runPID)
			s.end(stExpired, nil, "")
			return false
		}
		select {
		case <-exited:
			s.end(stLost, nil, "the VM stopped")
			return false
		default:
		}
		if time.Since(lastPolicy) >= time.Second {
			lastPolicy = time.Now()
			// hostmode.go: an idle warm box ends in a drain as on a close.
			if h := s.cfg.host(); h.verdict == hostDrains || h.verdict == hostCloses {
				s.vm.Destroy(r.VM, s.runPID)
				s.end(stReclaimed, nil, h.why)
				return false
			}
		}
		time.Sleep(warmPoll)
	}
}

// taken makes a warm box its taker's: room asked now (it held none), the
// commit checked out, then ready with the taker's lease.
func (s *sup) taken(exited <-chan struct{}) error {
	r, err := s.st.load(s.id)
	if err != nil {
		return err
	}
	if err := s.refusesNew(); err != nil { // drain.go: the host drained or closed since acquire
		return err
	}
	if err := s.takeRoom(r); err != nil {
		return err
	}
	if r.Commit != "" {
		if !s.st.setPhase(r.Target, r.Key, "checkout") { // progress.go
			return errAcquireCancelled
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Duration(s.cfg.BootTimeout+10)*time.Second)
		err := s.checkout(ctx, r)
		cancel()
		if err != nil {
			return err
		}
	}
	cancelled := false
	_, err = s.st.update(s.id, func(r *record) error {
		if r.State != stStarting {
			return errors.New("released while starting")
		}
		if cancelled = s.cancelledAtReady(r); cancelled { // progress.go
			return nil
		}
		now := time.Now().UTC()
		r.State, r.RunStart = stReady, now
		r.Deadline = now.Add(time.Duration(r.TTL) * time.Second)
		if lim := r.Start.Add(time.Duration(s.cfg.TTLMax) * time.Second); r.Deadline.After(lim) {
			r.Deadline = lim
		}
		return nil
	})
	if err == nil && cancelled {
		err = errAcquireCancelled
	}
	return err
}
