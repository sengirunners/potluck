package vmhost

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/sengirunners/potluck/protocol"
)

// Acquire progress and cancel (docs/protocol.md): every acquire keeps a
// progress file by its target and request_id, under a lock of its own
// (progress.lock), so progress and cancel answer while an acquire or a sweep
// holds the state directory's lock. The supervisor names the phase as it goes
// (cloning, booting, checkout). A cancel is kept: the acquire in flight sees
// it (waitStarted, the supervisor at each phase) and ends what it made, and a
// later acquire with the key is refused. A cancel that comes after the box
// went ready but before any acquire recorded ready (the acquire died between)
// is seen by the box's supervisor, which ends it (endedByCancel).
//
// Lock order: the state lock may be held when the progress lock is taken,
// never the other way round.

type progressFile struct {
	Target, Key, State, Phase string
	// PID is the driver process making a starting acquire: a repeat counts
	// that acquire in flight only while it lives. 0 (a file older than the
	// field) counts as alive.
	PID int `json:",omitempty"`
}

// inFlight says whether p is an acquire still being made by a live process.
func (p progressFile) inFlight() bool {
	return p.State == protocol.AcquireStarting && (p.PID == 0 || alive(p.PID))
}

// progressKeep is how long a progress file outlives its last change: a
// cancel is kept at least 24 hours (docs/protocol.md).
const progressKeep = 48 * time.Hour

var errCancelled = refuse("conflict", "the acquire with this request_id was cancelled")

func (s store) progressPath(target, key string) string {
	sum := sha256.Sum256([]byte(target + "\x00" + key))
	return filepath.Join(s.dir, "progress", hex.EncodeToString(sum[:12])+".json")
}

// progressLocked runs f holding the progress lock (not the state lock). Like
// locked, it never makes the state directory itself.
func (s store) progressLocked(f func() error) error {
	if err := os.Mkdir(filepath.Join(s.dir, "progress"), 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	return flocked(filepath.Join(s.dir, "progress.lock"), f)
}

func (s store) loadProgress(target, key string) (progressFile, bool) {
	var p progressFile
	b, err := os.ReadFile(s.progressPath(target, key))
	if err != nil || json.Unmarshal(b, &p) != nil || p.Target != target || p.Key != key {
		return progressFile{}, false
	}
	return p, true
}

func (s store) saveProgress(p progressFile) error {
	b, err := json.Marshal(p)
	if err != nil {
		return err
	}
	path := s.progressPath(p.Target, p.Key)
	if err := os.WriteFile(path+".tmp", b, 0o600); err != nil {
		return err
	}
	return os.Rename(path+".tmp", path)
}

// progressState is key's progress state; found false when no acquire with
// the key kept progress.
func (s store) progressState(target, key string) (state string, found bool) {
	var p progressFile
	_ = s.progressLocked(func() error { p, found = s.loadProgress(target, key); return nil })
	return p.State, found
}

// cancelled reports whether key's acquire was cancelled.
func (s store) cancelled(target, key string) bool {
	if key == "" {
		return false
	}
	state, _ := s.progressState(target, key)
	return state == protocol.AcquireCancelled
}

// setPhase moves key's starting acquire to phase; false when it was
// cancelled. A record with no acquire to report (a warm box, a start of a
// kept disk) changes nothing.
func (s store) setPhase(target, key, phase string) bool {
	if key == "" {
		return true
	}
	ok := true
	_ = s.progressLocked(func() error {
		p, found := s.loadProgress(target, key)
		switch {
		case !found:
		case p.State == protocol.AcquireCancelled:
			ok = false
		case p.State == protocol.AcquireStarting:
			p.Phase = phase
			return s.saveProgress(p)
		}
		return nil
	})
	return ok
}

// sweepProgress forgets progress files untouched for progressKeep whose key
// no record holds any more: a held handle's ready stands as long as it does.
// State lock held.
func (s store) sweepProgress() {
	held := map[[2]string]bool{}
	for _, r := range s.all() {
		held[[2]string{r.Target, r.Key}] = true
	}
	_ = s.progressLocked(func() error {
		dir := filepath.Join(s.dir, "progress")
		es, _ := os.ReadDir(dir)
		for _, e := range es {
			i, err := e.Info()
			if err != nil || time.Since(i.ModTime()) <= progressKeep {
				continue
			}
			var p progressFile
			if b, err := os.ReadFile(filepath.Join(dir, e.Name())); err == nil && json.Unmarshal(b, &p) == nil && held[[2]string{p.Target, p.Key}] {
				continue
			}
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
		return nil
	})
}

// acquireTracked is acquire with its progress kept: starting when it
// arrives, then ready or failed; a cancel at any point wins, and what the
// acquire made is ended before it answers conflict.
// prepare (dispatch's sweep) runs after the acquire is marked starting, so
// progress knows it however long the state lock is held.
func (d *drv) acquireTracked(raw json.RawMessage, prepare func() error) (any, error) {
	var a struct {
		RequestID string `json:"request_id"`
	}
	if json.Unmarshal(raw, &a) != nil || a.RequestID == "" {
		if err := prepare(); err != nil {
			return nil, err
		}
		return d.acquire(raw) // refused there: nothing to track
	}
	key, target := a.RequestID, d.cfg.Target
	cancelled, inFlight, err := d.progressBegin(key)
	if err != nil {
		return nil, err
	}
	var res any
	aerr := prepare()
	if aerr == nil && cancelled {
		// An earlier acquire with the key may have made its resource and
		// died before it saw the cancel: end that first.
		if ans, ok := d.endKey(key); !ok {
			return ans, nil // it cannot be ended: answered, so it is held (progress says ready)
		}
		return nil, errCancelled
	}
	if cancelled {
		return nil, errCancelled
	}
	if aerr == nil {
		res, aerr = d.acquire(raw)
	}
	var answered bool
	if err := d.st.progressLocked(func() error {
		p, _ := d.st.loadProgress(target, key)
		switch {
		case p.State == protocol.AcquireCancelled:
			return nil
		case aerr != nil && (p.State == protocol.AcquireReady || inFlight && p.State == protocol.AcquireStarting):
			answered = true // a refused repeat: the key's resource, or the acquire making it, stands
			return nil
		}
		answered = true
		next := progressFile{Target: target, Key: key, State: protocol.AcquireReady}
		if aerr != nil {
			next.State = protocol.AcquireFailed // no message: the refusal could quote what was sent
		}
		return d.st.saveProgress(next)
	}); err != nil {
		return res, aerr // unrecorded, but a handle made is still answered: held, never live with no holder
	}
	if answered {
		return res, aerr
	}
	if ans, ok := d.endKey(key); !ok {
		return ans, nil // it cannot be ended: answered as the key's handle (progress says ready)
	}
	return nil, errCancelled
}

// progressBegin marks key's acquire starting, unless it was cancelled
// (cancelled true), is in flight already (inFlight true) or made its
// resource (left as is). A starting acquire whose process died is not in
// flight: this one takes its place.
func (d *drv) progressBegin(key string) (cancelled, inFlight bool, err error) {
	err = d.st.progressLocked(func() error {
		p, ok := d.st.loadProgress(d.cfg.Target, key)
		switch {
		case ok && p.State == protocol.AcquireCancelled:
			cancelled = true
			return nil
		case ok && p.inFlight():
			inFlight = true // a repeat: the first acquire's phase stands
			return nil
		case ok && p.State == protocol.AcquireReady:
			return nil
		}
		return d.st.saveProgress(progressFile{Target: d.cfg.Target, Key: key, State: protocol.AcquireStarting, Phase: protocol.AcquirePhaseQueued, PID: os.Getpid()})
	})
	return cancelled, inFlight, err
}

// endKey releases everything key's acquires made on this target. A record
// gone or no longer live meanwhile (its supervisor failed the cancelled
// start) counts as ended. One that cannot be ended is marked ready and its
// handle answered (ok false), so it is held rather than live with no holder;
// one still starting is answered once its supervisor has it ready, with its
// lease. endKey never makes anything.
func (d *drv) endKey(key string) (answer any, ok bool) {
	for _, r := range d.st.all() {
		if r.Target != d.cfg.Target || r.Key != key || !r.live() && r.State != stStopped {
			continue
		}
		if _, err := d.release(r.Handle, nil); err == nil {
			continue
		}
		o, lerr := d.st.read(r.ID)
		if lerr != nil || !o.live() && o.State != stStopped {
			continue
		}
		mark := func(state string) {
			_ = d.st.progressLocked(func() error {
				return d.st.saveProgress(progressFile{Target: d.cfg.Target, Key: key, State: state})
			})
		}
		mark(protocol.AcquireReady)
		if o.State == stStarting { // its lease is set when it goes ready
			w, err := d.waitStarted(o.ID)
			if err != nil { // it never started: nothing is held, and the cancel stands
				mark(protocol.AcquireCancelled)
				return nil, true
			}
			o = w
		}
		return d.answer(o), false
	}
	return nil, true
}

func (d *drv) progressKey(raw json.RawMessage) (string, error) {
	var a struct {
		RequestID string `json:"request_id"`
	}
	if json.Unmarshal(raw, &a) != nil || a.RequestID == "" {
		return "", refuse("bad_request", "progress and cancel name a request_id")
	}
	return a.RequestID, nil
}

func (d *drv) progress(raw json.RawMessage) (any, error) {
	key, err := d.progressKey(raw)
	if err != nil {
		return nil, err
	}
	var out protocol.AcquireProgress
	err = d.st.progressLocked(func() error {
		p, ok := d.st.loadProgress(d.cfg.Target, key)
		if !ok {
			return refuse("not_found", "no acquire with request_id %q", key)
		}
		out = protocol.AcquireProgress{RequestID: key, State: p.State}
		if p.State == protocol.AcquireStarting {
			out.Phase = p.Phase
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// cancel stops key's acquire: one in flight sees the kept cancel and ends
// what it made; one never received, failed or cancelled already is refused
// when it comes. Only an acquire that answered its handle (ready) is not
// cancelled: its caller releases the handle.
func (d *drv) cancel(raw json.RawMessage) (any, error) {
	key, err := d.progressKey(raw)
	if err != nil {
		return nil, err
	}
	var out protocol.CancelAnswer
	err = d.st.progressLocked(func() error {
		p, ok := d.st.loadProgress(d.cfg.Target, key)
		if ok && p.State == protocol.AcquireReady {
			out = protocol.CancelAnswer{Cancelled: false, State: p.State}
			return nil
		}
		out = protocol.CancelAnswer{Cancelled: true, State: protocol.AcquireCancelled}
		return d.st.saveProgress(progressFile{Target: d.cfg.Target, Key: key, State: protocol.AcquireCancelled})
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// phase names the step r's acquire is in, for progress; false
// when that acquire was cancelled, which fails the start (its VM ends with
// this process). A start of a kept disk has no acquire to report.
func (s *sup) phase(r record, p string) bool {
	if r.Starts > 0 || s.st.setPhase(r.Target, r.Key, p) {
		return true
	}
	fail(s.st, r, errAcquireCancelled)
	return false
}

// errAcquireCancelled fails a start whose acquire was cancelled.
var errAcquireCancelled = errors.New("its acquire was cancelled")

// cancelledAtReady, under the state lock as r is about to go ready or
// running, fails r when its acquire was cancelled meanwhile: with the
// acquire gone (killed, crashed) nothing else would end it.
func (s *sup) cancelledAtReady(r *record) bool {
	if r.Starts > 0 || !s.st.cancelled(r.Target, r.Key) {
		return false
	}
	r.State, r.Why, r.End = stFailed, errAcquireCancelled.Error(), time.Now().UTC()
	return true
}

// endedByCancel, in the watch loop of a live box or run, says whether its
// acquire was cancelled after it went ready: only an acquire that died before
// recording ready leaves that, and nothing else would end it. Once progress
// says ready a cancel no longer changes it, so the check stops there. A key
// with no progress at all had no acquire tracked (a cancel would only make
// one up): the check stops there too.
func (s *sup) endedByCancel(r record) bool {
	if s.keyDone || r.Starts > 0 || r.Key == "" {
		return false
	}
	state, found := s.st.progressState(r.Target, r.Key)
	s.keyDone = !found || state == protocol.AcquireReady
	return found && state == protocol.AcquireCancelled
}
