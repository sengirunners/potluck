package conformance

import (
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sengirunners/potluck/protocol"
)

// Sealed volumes (capability volumes-seal, zm-eakh.12.65.3): a build box
// makes a new volume; its release with seal makes it sealed, and any other
// end deletes it. A sealed volume is never written again, and ro or cow with
// sealed answers not_found for a volume that is not sealed, so a half-built
// volume, or one a caller filled rw, never passes as sealed. Each check uses
// a volume of its own.
func (r *run) seals(owner, other, base string) {
	if r.desc.Capabilities == nil || !slices.Contains(*r.desc.Capabilities, protocol.CapVolumesSeal) {
		r.add("without volumes-seal, a volume asking sealed answers bad_request (never a plain volume)", r.sealUnknown(owner, base+"-s0"))
		r.add("without volumes-seal, a volume asking checkout answers bad_request", r.checkoutUnknown(owner, base+"-c0"))
		r.skip("sealed volumes", "describe lists no volumes-seal capability")
		return
	}
	n := 0
	fresh := func() string { n++; return base + "-s" + strconv.Itoa(n) }
	r.add("a build volume that is not sealed is gone (release without seal, and an expired builder)", r.sealUnsealedGone(owner, fresh(), fresh()))
	r.add("a sealed volume refuses rw and build", r.sealFrozen(owner, fresh()))
	r.add("sealed is refused for a volume that is not (a caller-filled rw volume of that name)", r.sealNotForRW(owner, fresh()))
	r.add("a second build waits (unavailable, reason building)", r.sealOneBuilder(owner, fresh()))
	r.add("another owner's sealed volume of the same name is another volume", r.sealPerOwner(owner, other, fresh()))
	r.add("a refused acquire leaves no build volume behind (a build beside a sealed volume that is not, then a build of that name)", r.sealRefusedLeavesNone(owner, fresh(), fresh()))
	r.add("sealed with rw or build, and build with persist, answer bad_request", r.sealBadArgs(owner, fresh()))
	r.add("a sealed volume carries no setuid or setgid file and no device node, FIFO or socket", r.sealClean(owner, fresh())) // volumes_clean.go
	r.checkouts(owner, fresh(), fresh())                                                                                      // volumes_checkout.go
	r.keeps(owner, fresh)                                                                                                     // volumes_keep.go
	const late = "a build box's processes end before its volume is sealed"
	if r.has(verbSpawn) {
		r.add(late, r.sealAfterProcs(owner, fresh()))
	} else {
		r.skip(late, "the driver serves no spawn")
	}
}

// build acquires a build box for name, writes v into the volume and ends
// it: released with seal when seal, plain when not.
func (r *run) build(owner, name, v string, seal bool) error {
	b, e, err := r.acquireVolWith(owner, name, protocol.VolumeBuild, false, r.longTTL())
	defer r.endHeld(b)
	if err = must(e, err); err == nil {
		err = volAnswered(b.acquired, name, protocol.VolumeBuild, true)
	}
	if err != nil {
		return fmt.Errorf("build: %v", err)
	}
	if code, err := r.volWrite(b.Handle, v); err != nil || code != 0 {
		return fmt.Errorf("a build volume took no write: exit %d %v", code, err)
	}
	return r.releaseSeal(b.Handle, seal)
}

// releaseSeal releases box h, with seal when seal.
func (r *run) releaseSeal(h string, seal bool) error {
	var a json.RawMessage
	if seal {
		a = args("seal", true)
	}
	var rel releaseReport
	if err := must(r.call(verbRelease, h, a, &rel)); err != nil {
		return fmt.Errorf("release (seal %v): %v", seal, err)
	}
	r.noteSpend(h, rel.Spend)
	if rel.Released == nil || !*rel.Released {
		return fmt.Errorf("release (seal %v) did not answer released true", seal)
	}
	delete(r.live, h)
	return nil
}

// sealedRead acquires owner's name ro with sealed and reads its file.
func (r *run) sealedRead(owner, name string) (string, error) {
	h, e, err := r.acquireVolWith(owner, name, protocol.VolumeRO, true, r.longTTL())
	defer r.endHeld(h)
	if err = must(e, err); err != nil {
		return "", fmt.Errorf("ro with sealed of a sealed volume: %v", err)
	}
	return r.volRead(h.Handle)
}

// notSealed wants ro and cow with sealed for owner's name to answer
// not_found.
func (r *run) notSealed(owner, name, why string) error {
	for _, mode := range []string{protocol.VolumeRO, protocol.VolumeCOW} {
		h, e, err := r.acquireVolWith(owner, name, mode, true, r.longTTL())
		r.endHeld(h)
		if err := wantCode(e, err, protocol.CodeNotFound); err != nil {
			return fmt.Errorf("%s with sealed of %s: %v", mode, why, err)
		}
	}
	return nil
}

// freshBuild wants a build of name to make an empty volume again, and ends
// that builder unsealed.
func (r *run) freshBuild(owner, name, why string) error {
	b, e, err := r.acquireVolWith(owner, name, protocol.VolumeBuild, false, r.longTTL())
	defer r.endHeld(b)
	if err = must(e, err); err == nil {
		err = volAnswered(b.acquired, name, protocol.VolumeBuild, true)
	}
	if err != nil {
		return fmt.Errorf("a build after %s: %v", why, err)
	}
	got, err := r.volRead(b.Handle)
	if err == nil && got != "" {
		err = fmt.Errorf("a build after %s reads %q: the old volume is still there", why, clip(got))
	}
	if err == nil {
		err = r.releaseSeal(b.Handle, false)
	}
	return err
}

func (r *run) sealUnsealedGone(owner, released, expired string) error {
	if err := r.build(owner, released, "partial-"+r.tag, false); err != nil {
		return err
	}
	if err := r.notSealed(owner, released, "a build released without seal"); err != nil {
		return err
	}
	if err := r.freshBuild(owner, released, "a release without seal"); err != nil {
		return err
	}

	ttl := r.shortTTL()
	b, e, err := r.acquireVolWith(owner, expired, protocol.VolumeBuild, false, ttl)
	defer r.endHeld(b)
	if err = must(e, err); err != nil {
		return fmt.Errorf("build with a short ttl: %v", err)
	}
	if code, err := r.volWrite(b.Handle, "partial-"+r.tag); err != nil || code != 0 {
		return fmt.Errorf("a build volume took no write: exit %d %v", code, err)
	}
	exp, err := wireTime(b.ExpiresAt)
	if err != nil {
		return err
	}
	r.sleep(time.Until(exp))
	if _, err := r.pollState(b.Handle, exp.Add(r.opt.Grace), "expired"); err != nil {
		return fmt.Errorf("a build box past its deadline: %v", err)
	}
	if err := r.notSealed(owner, expired, "an expired build"); err != nil {
		return err
	}
	return r.freshBuild(owner, expired, "an expired build")
}

func (r *run) sealFrozen(owner, name string) error {
	if err := r.build(owner, name, r.tag, true); err != nil {
		return err
	}
	got, err := r.sealedRead(owner, name)
	if err == nil && got != r.tag {
		err = fmt.Errorf("a sealed volume reads %q, want what its builder wrote, %q", clip(got), r.tag)
	}
	if err != nil {
		return err
	}
	for _, mode := range []string{protocol.VolumeRW, protocol.VolumeBuild} {
		h, e, err := r.acquireVolWith(owner, name, mode, false, r.longTTL())
		if err == nil && e == nil {
			_, _ = r.volWrite(h.Handle, "changed-"+r.tag)
		}
		r.endHeld(h)
		if err := wantCode(e, err, protocol.CodeConflict); err != nil {
			return fmt.Errorf("%s of a sealed volume: %v", mode, err)
		}
	}
	if got, err = r.sealedRead(owner, name); err == nil && got != r.tag {
		err = fmt.Errorf("after rw and build were asked, the sealed volume reads %q, want %q", clip(got), r.tag)
	}
	return err
}

func (r *run) sealNotForRW(owner, name string) error {
	w, e, err := r.acquireVol(owner, name, protocol.VolumeRW)
	defer r.endHeld(w)
	if err = must(e, err); err != nil {
		return err
	}
	if code, err := r.volWrite(w.Handle, r.tag); err != nil || code != 0 {
		return fmt.Errorf("a rw volume took no write: exit %d %v", code, err)
	}
	if err := r.releaseSeal(w.Handle, true); err != nil { // seal from a box that holds no build: no effect
		return err
	}
	if err := r.notSealed(owner, name, "a volume a caller filled rw (its box released with seal)"); err != nil {
		return err
	}
	b, e, err := r.acquireVolWith(owner, name, protocol.VolumeBuild, false, r.longTTL())
	r.endHeld(b)
	if err := wantCode(e, err, protocol.CodeConflict); err != nil {
		return fmt.Errorf("build of a volume a caller filled rw: %v", err)
	}
	// One box holding a volume rw beside one in build: its seal seals only the build volume.
	rw, built := name+"-rw", name+"-b"
	both := []map[string]any{{"name": built, "path": volPath, "mode": protocol.VolumeBuild}, {"name": rw, "path": volPath + "-rw", "mode": protocol.VolumeRW}}
	res, e, err := r.acquire(args("request_id", r.key(), "actor", actor, "ttl_s", r.longTTL(), "volume_owner", owner, "volumes", both), r.cfg)
	if err = must(e, err); err != nil {
		return fmt.Errorf("a box with a build and a rw volume: %v", err)
	}
	if err := r.releaseSeal(res.Handle, true); err != nil {
		return err
	}
	return r.notSealed(owner, rw, "a rw volume whose box also held one in build and released with seal")
}

func (r *run) sealOneBuilder(owner, name string) error {
	a, e, err := r.acquireVolWith(owner, name, protocol.VolumeBuild, false, r.longTTL())
	defer r.endHeld(a)
	if err = must(e, err); err != nil {
		return fmt.Errorf("build: %v", err)
	}
	b, e, err := r.acquireVolWith(owner, name, protocol.VolumeBuild, false, r.longTTL())
	r.endHeld(b)
	if err := wantCode(e, err, protocol.CodeUnavailable); err != nil {
		return fmt.Errorf("a second build while one is live: %v", err)
	}
	var busy protocol.Busy
	if len(e.Data) == 0 || json.Unmarshal(e.Data, &busy) != nil || busy.Reason != protocol.ReasonBuilding || busy.RetryS <= 0 {
		return fmt.Errorf("a second build answered data %s, want reason %q and retry_s above 0", e.Data, protocol.ReasonBuilding)
	}
	if err := r.notSealed(owner, name, "a volume still being built"); err != nil {
		return err
	}
	return r.releaseSeal(a.Handle, false)
}

func (r *run) sealPerOwner(owner, other, name string) error {
	if err := r.build(owner, name, r.tag, true); err != nil {
		return err
	}
	if err := r.notSealed(other, name, "another owner's sealed volume of that name"); err != nil {
		return err
	}
	if err := r.build(other, name, "other-"+r.tag, true); err != nil {
		return fmt.Errorf("another owner's own build of that name: %v", err)
	}
	for _, c := range []struct{ who, want string }{{owner, r.tag}, {other, "other-" + r.tag}} {
		got, err := r.sealedRead(c.who, name)
		if err == nil && got != c.want {
			err = fmt.Errorf("a sealed volume reads %q, want %q (each owner's own)", clip(got), c.want)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// sealRefusedLeavesNone: the refusal (not_found for the second volume) comes
// from the volume's state, after the build volume passed every check.
func (r *run) sealRefusedLeavesNone(owner, name, missing string) error {
	vols := []map[string]any{{"name": name, "path": volPath, "mode": protocol.VolumeBuild}, {"name": missing, "path": volPath + "-2", "mode": protocol.VolumeRO, "sealed": true}}
	res, e, err := r.acquire(args("request_id", r.key(), "actor", actor, "ttl_s", r.longTTL(), "volume_owner", owner, "volumes", vols), r.cfg)
	if e == nil && err == nil {
		r.end(res.Handle)
	}
	if err := wantCode(e, err, protocol.CodeNotFound); err != nil {
		return fmt.Errorf("a build beside a sealed volume that does not exist: %v", err)
	}
	return r.freshBuild(owner, name, "a refused acquire")
}

// sealUnknown: a driver without volumes-seal refuses sealed rather than
// mount a plain volume a caller would take for a sealed one.
func (r *run) sealUnknown(owner, name string) error {
	h, e, err := r.acquireVolWith(owner, name, protocol.VolumeRO, true, r.longTTL())
	r.endHeld(h)
	return wantCode(e, err, protocol.CodeBadRequest)
}

func (r *run) sealBadArgs(owner, name string) error {
	for _, c := range []struct {
		why  string
		args json.RawMessage
	}{
		{"sealed with rw", args("request_id", r.key(), "actor", actor, "ttl_s", r.longTTL(), "volume_owner", owner,
			"volumes", []map[string]any{{"name": name, "path": volPath, "mode": protocol.VolumeRW, "sealed": true}})},
		{"sealed with build", args("request_id", r.key(), "actor", actor, "ttl_s", r.longTTL(), "volume_owner", owner,
			"volumes", []map[string]any{{"name": name, "path": volPath, "mode": protocol.VolumeBuild, "sealed": true}})},
		{"build with persist", args("request_id", r.key(), "actor", actor, "ttl_s", r.longTTL(), "volume_owner", owner, "persist", true,
			"volumes", []map[string]any{{"name": name, "path": volPath, "mode": protocol.VolumeBuild}})},
	} {
		res, e, err := r.acquire(c.args, r.cfg)
		if e == nil && err == nil {
			r.end(res.Handle)
		}
		if err := wantCode(e, err, protocol.CodeBadRequest); err != nil {
			return fmt.Errorf("%s: %v", c.why, err)
		}
	}
	return nil
}

// sealAfterProcs: a process the builder started keeps adding files to the
// volume; once the builder is released with seal, the sealed volume no
// longer changes (two sealed reads apart in time list the same names).
func (r *run) sealAfterProcs(owner, name string) error {
	b, e, err := r.acquireVolWith(owner, name, protocol.VolumeBuild, false, r.longTTL())
	defer r.endHeld(b)
	if err = must(e, err); err != nil {
		return fmt.Errorf("build: %v", err)
	}
	if code, err := r.volWrite(b.Handle, r.tag); err != nil || code != 0 {
		return fmt.Errorf("a build volume took no write: exit %d %v", code, err)
	}
	loop := `cd "$HOME/$1" || exit 1; i=0; while [ $i -lt 400 ]; do printf x > "late-$i"; i=$((i+1)); sleep 0.05; done`
	if _, err := r.spawn(b.Handle, "argv", []string{"sh", "-c", loop, "sh", volPath}, "request_id", r.key()); err != nil {
		return fmt.Errorf("spawn in the build box: %v", err)
	}
	r.sleep(500 * time.Millisecond)
	if err := r.releaseSeal(b.Handle, true); err != nil {
		return err
	}
	list := func() (string, error) {
		h, e, err := r.acquireVolWith(owner, name, protocol.VolumeRO, true, r.longTTL())
		defer r.endHeld(h)
		if err = must(e, err); err != nil {
			return "", fmt.Errorf("ro with sealed of a sealed volume: %v", err)
		}
		res, err := r.sh(h.Handle, `ls -A "$HOME/$1" | sort | tr '\n' ' '`, volPath)
		if err != nil {
			return "", err
		}
		return *res.Stdout, nil
	}
	first, err := list()
	if err != nil {
		return err
	}
	r.sleep(1500 * time.Millisecond)
	second, err := list()
	if err == nil && first != second {
		err = fmt.Errorf("a sealed volume changed after its seal: a process of the build box still writes it (%d names, then %d)",
			len(strings.Fields(first)), len(strings.Fields(second)))
	}
	return err
}
