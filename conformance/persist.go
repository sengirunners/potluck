package conformance

import (
	"fmt"
	"slices"
	"time"

	"github.com/sengirunners/potluck/protocol"
)

// Kept disks (SPEC 20 "Boxes", Kept disks; zm-eakh.12.5.23): a box acquired
// with persist keeps its disk across stop and start, and its lease's end
// stops it rather than wiping it. Verbs stop and start with capability
// persist, listed together. Seen through a file in the box's HOME.

const (
	verbStop  = "stop"
	verbStart = "start"
)

type stopReport struct {
	Stopped   *bool        `json:"stopped"`
	ExpiresAt string       `json:"expires_at"`
	Spend     *spendReport `json:"spend"`
}

// acquirePersist acquires a box with persist and checks the answer.
func (r *run) acquirePersist(ttl int) (held, error) {
	h := held{key: r.key()}
	h.sent = args("request_id", h.key, "actor", actor, "ttl_s", ttl, "persist", true)
	before := time.Now()
	a, e, err := r.acquire(h.sent, r.cfg)
	h.acquired = a
	if err = must(e, err); err != nil {
		return h, err
	}
	return h, r.acquireShape(a, ttl, before)
}

// stop stops a box and checks the answer: stopped, and when its disk goes.
func (r *run) stop(h string) error {
	var s stopReport
	if err := must(r.call(verbStop, h, nil, &s)); err != nil {
		return err
	}
	r.noteSpend(h, s.Spend)
	if s.Stopped == nil || !*s.Stopped {
		return fmt.Errorf("stop answered stopped %v", s.Stopped)
	}
	return r.stoppedNow(h)
}

// stoppedNow wants status stopped with the kept disk's deadline ahead, and
// exec refused.
func (r *run) stoppedNow(h string) error {
	st, err := r.status(h)
	if err == nil {
		err = wantState(st, protocol.StateStopped, nil)
	}
	if err == nil {
		if exp, _ := wireTime(st.ExpiresAt); !exp.After(time.Now()) {
			err = fmt.Errorf("a stopped box's expires_at (when its disk goes) is %s, not ahead", st.ExpiresAt)
		}
	}
	if err == nil {
		if err = r.expect(verbExec, h, args("argv", []string{"true"}), protocol.CodeConflict); err != nil {
			err = fmt.Errorf("exec in a stopped box: %v", err)
		}
	}
	return err
}

// rootProbe is a file outside HOME a box writes before a stop, checked after
// a start when describe says disks.root.
const rootProbe = "/var/tmp/zmconf-kept-root"

func (r *run) claimsRoot() bool { return r.desc.Disks != nil && r.desc.Disks.Root }

// writeKept writes the probe files a start must bring back: in HOME, and
// outside it when the driver says it keeps the root.
func (r *run) writeKept(h, v string) error {
	line := `printf '%s' "$1" > "$HOME/zmconf-kept"`
	if r.claimsRoot() {
		line += ` && printf '%s' "$1" > ` + rootProbe + "-" + r.tag
	}
	_, err := r.sh(h, line, v)
	return err
}

// keepsFile wants the file a box wrote before it stopped, after a start.
func (r *run) keepsFile(h, ttl string) error {
	var a acquired
	before := time.Now()
	if err := must(r.call(verbStart, h, args("ttl_s", r.longTTL()), &a)); err != nil {
		return err
	}
	if err := r.acquireShape(a, r.longTTL(), before); err != nil {
		return fmt.Errorf("start: %v", err)
	}
	st, err := r.status(h)
	if err == nil {
		err = wantState(st, "ready", nil)
	}
	if err != nil {
		return err
	}
	res, err := r.sh(h, `cat "$HOME/zmconf-kept"`, "")
	if err == nil && *res.Stdout != ttl {
		err = fmt.Errorf("after start the file written before the stop reads %q, want %q", clip(*res.Stdout), ttl)
	}
	if err == nil && r.claimsRoot() {
		res, err = r.sh(h, `cat "$1"; rm -f "$1"`, rootProbe+"-"+r.tag)
		if err == nil && *res.Stdout != ttl {
			err = fmt.Errorf("describe says disks.root, but after start %s reads %q, want %q (only HOME was kept)", rootProbe, clip(*res.Stdout), ttl)
		}
	}
	return err
}

func (r *run) persist() {
	if r.kind != "box" {
		return
	}
	caps := []string{}
	if r.desc.Capabilities != nil {
		caps = *r.desc.Capabilities
	}
	hasCap := slices.Contains(caps, protocol.CapPersist)
	if !r.has(verbStop) && !r.has(verbStart) && !hasCap {
		r.skip("kept disks", "describe lists no persist capability")
		return
	}
	if !r.add("a driver listing stop and start lists capability persist, and the other way round", func() error {
		if !r.has(verbStop) || !r.has(verbStart) || !hasCap {
			return fmt.Errorf("verbs %v, capabilities %v: want stop, start and persist together", r.desc.Verbs, caps)
		}
		return nil
	}()) {
		return
	}
	plain, err := r.acquireAny(r.longTTL())
	var h held
	if err == nil {
		h, err = r.acquirePersist(r.longTTL())
	}
	defer r.endHeld(plain)
	defer r.endHeld(h)
	if !r.add("acquire a box for kept disks, with and without persist", err) {
		return
	}
	r.add("stop of a box acquired without persist answers bad_request (it has no disk to keep)",
		r.expect(verbStop, plain.Handle, nil, protocol.CodeBadRequest))

	err = r.writeKept(h.Handle, r.tag)
	kept, kerr := r.boxSecretsBeforeStop(h.Handle) // boxsecrets.go
	if err == nil {
		err = r.stop(h.Handle)
	}
	if !r.add("stop keeps a persistent box's disk: status stopped, expires_at ahead (when the disk goes), exec conflict", err) {
		return
	}
	r.add("start brings the same disk back: a new lease, status ready, the file written before the stop", r.keepsFile(h.Handle, r.tag))
	r.boxSecretsAfterStart(h.Handle, kept, kerr)

	err = r.stop(h.Handle)
	var rel releaseReport
	if err == nil {
		if rel, err = r.release(h.Handle); err == nil {
			err = wantRelease(rel, true, nil)
		}
	}
	if err == nil {
		delete(r.live, h.Handle)
		var st statusReport
		if st, err = r.status(h.Handle); err == nil {
			err = wantState(st, "released", nil)
		}
	}
	r.add("release of a stopped box: released true, status released (its disk deleted)", err)

	// The lease's end stops the box: nothing is lost to a deadline.
	ttl := r.shortTTL()
	h2, err := r.acquirePersist(ttl)
	defer r.endHeld(h2)
	if err == nil {
		err = r.writeKept(h2.Handle, "deadline-"+r.tag)
	}
	if err == nil {
		_, err = r.pollState(h2.Handle, time.Now().Add(time.Duration(ttl)*time.Second+r.opt.Grace), protocol.StateStopped)
	}
	if err == nil {
		err = r.stoppedNow(h2.Handle)
	}
	if err == nil {
		err = r.keepsFile(h2.Handle, "deadline-"+r.tag)
	}
	r.add("a persistent box's deadline stops it (not expired), and start brings its disk back", err)
}
