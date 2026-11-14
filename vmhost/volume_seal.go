package vmhost

import (
	"fmt"
	"os"
	"time"

	"github.com/sengirunners/potluck/protocol"
)

// Sealed volumes (capability volumes-seal; docs/protocol.md "Sealed
// volumes"). A volume acquired in mode build is new, and its volMeta names
// the box that builds it (Build). That box's release with seal, once the box
// is destroyed and no container references the volume, clears Build and sets
// Sealed: the driver's own record, never a file in the volume. Every other
// end of the builder (a plain release, expiry, reclaim, a quota reclaim, a
// crash, an acquire that is refused or never starts) deletes the volume. So
// a volume whose meta has Build set is either being built by a live box or
// waiting to be deleted, and no acquire mounts it in any mode.
//
// A sealed volume is never written again: rw and build are conflict, and the
// keep time, the caps and the LRU delete it only whole, clearing Sealed
// before the layer's volume goes (so a crash never leaves a sealed record
// over a missing or partial volume). A volume asked with sealed (ro or cow)
// needs its owner's volume of that name to be sealed, else not_found.

const (
	volBuild = protocol.VolumeBuild

	// buildRetryS is the retry_s of an acquire refused because the volume
	// is being built.
	buildRetryS = 5
	// sealSettle is how long a release waits for its build volumes to be
	// free of every container before it gives up sealing (or deleting):
	// what is left then is the sweep's, and is never sealed.
	sealSettle = 60 * time.Second
	// sealWindow is how long after a release with seal began the sweep
	// leaves the box's build volumes to that release; after it, they are
	// deleted like any other builder's.
	sealWindow = 2 * sealSettle
)

// writesItself says whether a mode writes the volume itself (rw and build),
// not a copy of its own (cow) or nothing (ro).
func writesItself(mode string) bool { return mode == volRW || mode == volBuild }

// builds says whether a box holds any volume in build.
func (r record) builds() bool {
	for _, v := range r.Volumes {
		if v.Mode == volBuild {
			return true
		}
	}
	return false
}

// volRefusal is acquire's answer for one volume against its state, in the
// order docs/protocol.md gives: not_found (sealed asked, not sealed),
// conflict (rw or build of a sealed volume), unavailable (being built, or a
// live box holds it in a mode the lock forbids), conflict (build of a volume
// that exists). m and ok are its meta; holders the modes live boxes hold it
// in. Lock held.
func volRefusal(v volReq, m volMeta, ok bool, holders []string) error {
	sealed := ok && m.Sealed
	switch {
	case v.Sealed && !sealed:
		return refuse("not_found", "volume %s is not sealed", v.Name)
	case writesItself(v.Mode) && sealed:
		return refuse("conflict", "volume %s is sealed: ro or cow only", v.Name)
	case ok && m.Build != "": // its builder is live, or its unsealed volume is not deleted yet
		return refusal{code: "unavailable", msg: "volume " + v.Name + " is being built by another box",
			data: protocol.Busy{Reason: protocol.ReasonBuilding, RetryS: buildRetryS}}
	}
	for _, h := range holders {
		switch {
		case h == volBuild:
			return refusal{code: "unavailable", msg: "volume " + v.Name + " is being built by another box",
				data: protocol.Busy{Reason: protocol.ReasonBuilding, RetryS: buildRetryS}}
		case writesItself(v.Mode):
			return refuse("unavailable", "volume %s is in use by another box (%s): %s is one box's at a time", v.Name, h, v.Mode)
		case h == volRW:
			return refuse("unavailable", "volume %s is being written by another box (rw): ro and cow wait for it", v.Name)
		}
	}
	if v.Mode == volBuild && ok {
		return refuse("conflict", "volume %s exists: build makes a new one", v.Name)
	}
	return nil
}

// endBuilds settles a released box's build volumes before release answers:
// sealed when its release asked seal (record.Seal), once no container
// references the volume and it is cleaned (volume_keep.go: pruned to the
// release's keep, if it named one, and normalised) and then measured, so a
// sealed volume always counts its real size (nothing writes it again, so no
// later measurement would correct a failed one); a volume whose cleanup or
// measurement fails is deleted; else deleted. Each pass is under the lock, a cleanup
// between passes; between passes it waits, up to sealSettle. What it cannot
// settle is left unsealed for the sweep (sweepBuilds), which deletes it.
func (d *drv) endBuilds(r record) {
	if !r.builds() {
		return
	}
	v, _ := d.vm.(volumer)
	kept := map[string]int64{} // volumes cleaned and measured once free of every container: their bytes, or -1 when that failed
	for deadline := time.Now().Add(sealSettle); ; time.Sleep(200 * time.Millisecond) {
		left := false
		var clean []string
		_ = d.st.locked(func() error { left, clean = d.settleBuilds(r.ID, true, kept); return nil })
		cleaned := false            // a volume just cleaned gets its pass to seal, past the deadline too
		for _, vol := range clean { // outside the lock: a helper container, maybe minutes
			kept[vol], cleaned = cleanAndMeasure(v, r.Image, vol, r.Keep), true
		}
		if !left || time.Now().After(deadline) && !cleaned {
			return
		}
	}
}

// cleanAndMeasure cleans vol for its seal and measures it (CleanSeal),
// outside the lock: its bytes, or -1 when that failed (it is deleted, never
// sealed).
func cleanAndMeasure(v volumer, image, vol string, keep []string) int64 {
	b, err := v.CleanSeal(image, vol, keep)
	if err != nil {
		fmt.Fprintf(os.Stderr, "potluck-driver-tart: cleaning and measuring %s for its seal (it is deleted, never sealed): %v\n", vol, err)
		return -1
	}
	return b
}

// settleBuilds is one pass of endBuilds over record id's build volumes (seal
// false: never seal, only delete what is not the release's to seal). It
// answers whether any is left, and the volumes ready to seal but for their
// cleanup and measurement (those not in kept); one whose cleanup or
// measurement failed (-1 in kept) is deleted. Lock held.
func (d *drv) settleBuilds(id string, seal bool, kept map[string]int64) (left bool, clean []string) {
	r, err := d.st.load(id)
	if err != nil || r.live() {
		return false, nil
	}
	v, ok := d.vm.(volumer)
	if !ok {
		return false, nil
	}
	sealing := r.Seal && time.Since(r.End) < sealWindow
	for _, u := range r.Volumes {
		m, found := d.st.loadVol(u.ID)
		if u.Mode != volBuild || !found || m.Build != r.ID {
			continue // sealed already, or gone
		}
		switch {
		case sealing && !seal: // the release's to seal
		case sealing && v.VolumeInUse(m.Layer):
			left = true
		case sealing && kept[m.Layer] < 0: // its cleanup or measurement failed: never sealed
			if !d.dropVolume(m) {
				left = true
			}
		case sealing:
			b, tried := kept[m.Layer]
			if !tried {
				left = true
				clean = append(clean, m.Layer)
				continue
			}
			now := time.Now().UTC()
			m.Build, m.Sealed, m.Used = "", true, now
			m.Bytes, m.BytesAt, m.MeasureFailed = b, now, false
			if d.st.saveVol(m) != nil {
				left = true
			}
		case !d.dropVolume(m):
			left = true
		}
	}
	return left, clean
}

// sweepBuilds deletes every build volume of this binding whose builder is
// not live, is not being sealed by its release (sealWindow), and has no
// supervisor running (that one may still be making the volume, and deletes
// it itself when the box is gone: dropBuilds): a builder that crashed, an
// acquire that never started, a release or supervisor that could not
// finish. Lock held.
func (d *drv) sweepBuilds() {
	if _, ok := d.vm.(volumer); !ok {
		return
	}
	for _, m := range d.st.allVols() {
		if m.Target != d.cfg.Target || m.Build == "" {
			continue
		}
		r, err := d.st.load(m.Build)
		if err == nil && (r.live() || r.Seal && time.Since(r.End) < sealWindow || alive(r.Supervisor)) {
			continue
		}
		d.dropVolume(m)
	}
}

// dropBuilds is the supervisor's: once its box is gone, the build volumes
// that are not its release's to seal are deleted at once.
func (s *sup) dropBuilds(r record) {
	if !r.builds() {
		return
	}
	d := &drv{cfg: s.cfg, st: s.st, vm: s.vm}
	_ = s.st.locked(func() error { d.settleBuilds(r.ID, false, nil); return nil })
}
