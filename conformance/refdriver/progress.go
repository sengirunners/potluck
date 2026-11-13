package refdriver

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/sengirunners/potluck/protocol"
)

// Acquire progress and cancel (docs/protocol.md): every acquire keeps a
// progress file by its request_id, under its own lock so progress and cancel
// answer while an acquire holds the state directory's. Config acquire_ms
// and acquire_gate hold a new resource's acquire in phase "booting", so a
// test can see it in flight; cancel during it stops it, and a cancel that
// lands after the resource was made ends it before acquire answers. A cancel
// of a request_id never seen is kept, so its acquire, when it comes, is
// refused.

type progressFile struct {
	Key, State, Phase, Message string
}

func (d *drv) progressServed() bool { return !d.is("no-acquire-progress") }

// progressListed: the verbs and the capability in describe (bug
// progress-cap-unlisted lists the verbs alone).
func (d *drv) progressDescribe(out map[string]any) {
	if !d.progressServed() {
		return
	}
	out["verbs"] = append(out["verbs"].([]string), protocol.VerbProgress, protocol.VerbCancel)
	if !d.is("progress-cap-unlisted") {
		out["capabilities"] = append(out["capabilities"].([]string), protocol.CapAcquireProgress)
	}
}

func (d *drv) progressPath(key string) string {
	sum := sha256.Sum256([]byte(key))
	return filepath.Join(d.state, "p-"+hex.EncodeToString(sum[:8])+".json")
}

func (d *drv) loadProgress(key string) (progressFile, bool) {
	var p progressFile
	b, err := os.ReadFile(d.progressPath(key))
	if err != nil || json.Unmarshal(b, &p) != nil || p.Key != key {
		return p, false
	}
	return p, true
}

func (d *drv) saveProgress(p progressFile) error {
	b, err := json.Marshal(p)
	if err != nil {
		return err
	}
	path := d.progressPath(p.Key)
	if err := os.WriteFile(path+".tmp", b, 0o600); err != nil {
		return err
	}
	return os.Rename(path+".tmp", path)
}

// progressLocked runs f with the progress lock held (not the state lock).
func (d *drv) progressLocked(f func() error) error {
	l, err := os.OpenFile(filepath.Join(d.state, "progress.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer l.Close()
	if err := syscall.Flock(int(l.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(l.Fd()), syscall.LOCK_UN)
	return f()
}

var errCancelled = refuse("conflict", "the acquire with this request_id was cancelled")

// progressBegin marks key's acquire starting; a cancelled key is refused,
// and anything an earlier acquire with the key made (one that died after
// making it, so its cancel never ended it) is ended first. One that cannot be
// ended is answered, as progressEnd does, so it is held rather than live with
// no holder.
func (d *drv) progressBegin(key string) error {
	return d.progressLocked(func() error {
		p, ok := d.loadProgress(key)
		if ok && p.State == protocol.AcquireCancelled && !d.is("cancel-reacquire") {
			for _, r := range records(d.state) {
				if r.Key != key || r.State != "ready" && r.State != "running" && r.State != "stopped" {
					continue
				}
				if _, err := d.release(request{ID: r.Handle}); err != nil {
					return d.saveProgress(progressFile{Key: key, State: protocol.AcquireReady})
				}
			}
			return errCancelled
		}
		if ok && p.State == protocol.AcquireReady {
			return nil
		}
		return d.saveProgress(progressFile{Key: key, State: protocol.AcquireStarting, Phase: "queued"})
	})
}

// progressPhase moves a starting acquire on; false when it was cancelled.
// Bug progress-quotes-jit puts the acquire's jit_config in the message, which
// the protocol forbids (a driver never repeats what it was sent).
func (d *drv) progressPhase(key, phase, jit string) bool {
	cancelled := false
	_ = d.progressLocked(func() error {
		p, ok := d.loadProgress(key)
		if ok && p.State == protocol.AcquireCancelled {
			cancelled = true
			return nil
		}
		if d.is("progress-no-phase") {
			phase = ""
		}
		var msg string
		if d.is("progress-quotes-jit") {
			msg = "starting runner " + jit
		}
		return d.saveProgress(progressFile{Key: key, State: protocol.AcquireStarting, Phase: phase, Message: msg})
	})
	return !cancelled || d.is("cancel-keeps-box")
}

// slowAcquire holds a resource not yet made in phase booting: for config
// acquire_ms, and while the file config acquire_gate names exists (a test
// opens the gate when it is done looking; at most a minute). It stops early
// when the acquire is cancelled.
func (d *drv) slowAcquire(key, jit string) error {
	ms, gate := atoi(d.cfg["acquire_ms"]), d.cfg["acquire_gate"]
	if ms <= 0 && gate == "" {
		return nil
	}
	for _, r := range records(d.state) {
		if r.Key == key {
			return nil // a repeat answers at once
		}
	}
	start := time.Now()
	for time.Since(start) < time.Minute {
		if d.progressServed() && !d.progressPhase(key, "booting", jit) {
			return errCancelled
		}
		_, err := os.Stat(gate)
		if time.Since(start) >= time.Duration(ms)*time.Millisecond && (gate == "" || err != nil) {
			return nil
		}
		time.Sleep(25 * time.Millisecond)
	}
	return nil
}

// progressEnd records how key's acquire ended. A cancel that landed while
// the resource was being made wins: the resource is ended and the acquire
// answers conflict.
func (d *drv) progressEnd(key string, res map[string]any, err error) (map[string]any, error) {
	var out error
	lerr := d.progressLocked(func() error {
		p, _ := d.loadProgress(key)
		if p.State == protocol.AcquireCancelled && !d.is("cancel-keeps-box") {
			if h, ok := res["handle"].(string); ok && err == nil {
				if _, rerr := d.release(request{ID: h}); rerr != nil {
					// It cannot be ended: answer it, so it is held and
					// released later rather than live with no holder.
					return d.saveProgress(progressFile{Key: key, State: protocol.AcquireReady})
				}
			}
			out = errCancelled
			return nil
		}
		if err != nil && p.State == protocol.AcquireReady && !d.is("failed-overwrites-ready") {
			return nil // a refused repeat: the key's resource stands
		}
		next := progressFile{Key: key, State: protocol.AcquireReady}
		if err != nil {
			next.State = protocol.AcquireFailed // no message: the refusal could quote what was sent
		}
		if d.is("progress-no-ready") {
			next.State, next.Phase = protocol.AcquireStarting, "booting"
		}
		return d.saveProgress(next)
	})
	if out != nil {
		return nil, out
	}
	if lerr != nil {
		return nil, lerr
	}
	return res, err
}

type progressArgs struct {
	RequestID string `json:"request_id"`
}

func (d *drv) progressKey(req request) (string, error) {
	var a progressArgs
	if err := json.Unmarshal(req.Args, &a); err != nil || a.RequestID == "" {
		return "", refuse("bad_request", "progress and cancel name a request_id")
	}
	return a.RequestID, nil
}

// waitWhileStarting is bugs progress-waits and cancel-waits: the verb waits
// for key's acquire to end, as a driver with one lock for every verb would.
func (d *drv) waitWhileStarting(key string) {
	for start := time.Now(); time.Since(start) < time.Minute; time.Sleep(25 * time.Millisecond) {
		var p progressFile
		var ok bool
		_ = d.progressLocked(func() error { p, ok = d.loadProgress(key); return nil })
		if !ok || p.State != protocol.AcquireStarting {
			return
		}
	}
}

func (d *drv) progress(req request) (any, error) {
	key, err := d.progressKey(req)
	if err != nil {
		return nil, err
	}
	if d.is("progress-waits") {
		d.waitWhileStarting(key)
	}
	var out protocol.AcquireProgress
	err = d.progressLocked(func() error {
		p, ok := d.loadProgress(key)
		if ok && p.State == protocol.AcquireStarting && d.is("progress-not-found-in-flight") {
			ok = false
		}
		if !ok {
			if d.is("progress-unknown-ok") {
				p = progressFile{Key: key, State: protocol.AcquireStarting, Phase: "queued"}
			} else {
				return refuse("not_found", "no acquire with request_id %q", key)
			}
		}
		out = protocol.AcquireProgress{RequestID: key, State: p.State, Phase: p.Phase, Message: p.Message}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (d *drv) cancel(req request) (any, error) {
	key, err := d.progressKey(req)
	if err != nil {
		return nil, err
	}
	if d.is("cancel-waits") {
		d.waitWhileStarting(key)
	}
	var out protocol.CancelAnswer
	err = d.progressLocked(func() error {
		p, ok := d.loadProgress(key)
		switch {
		case !ok && d.is("cancel-unknown-not-found"):
			return refuse("not_found", "no acquire with request_id %q", key)
		case ok && p.State == protocol.AcquireStarting && d.is("cancel-in-flight-says-ready"):
			out = protocol.CancelAnswer{Cancelled: false, State: protocol.AcquireReady}
		case ok && p.State == protocol.AcquireFailed && d.is("cancel-failed-false"):
			out = protocol.CancelAnswer{Cancelled: false, State: p.State}
		case p.State != protocol.AcquireReady:
			// Never seen, in flight, failed (it holds nothing) or cancelled:
			// the cancel stands for any acquire with the key still to come.
			out = protocol.CancelAnswer{Cancelled: true, State: protocol.AcquireCancelled}
			return d.saveProgress(progressFile{Key: key, State: protocol.AcquireCancelled, Message: "cancelled"})
		default:
			out = protocol.CancelAnswer{Cancelled: d.is("cancel-ready-true"), State: p.State}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
