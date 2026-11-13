package refdriver

import (
	"cmp"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
)

// Kept disks (SPEC 20 "Boxes", Kept disks; zm-eakh.12.5.23): a box acquired
// with persist keeps its directory when it stops, nothing running in it,
// until its holder starts it again or the kept disk expires. Verbs stop and
// start with capability persist, listed together.

const keepDefault = 30 * 24 * 3600

// persists says whether this driver keeps disks (bug no-persist: it does not).
func (d *drv) persists() bool {
	return d.cfg["kind"] == "box" && !d.is("no-persist") && d.cfg["persist"] != "0" // config persist=0: the daemon's tests
}

// keepS is how long a stopped box's disk is kept (config persist_keep_s).
func (d *drv) keepS() int {
	if n := atoi(d.cfg["persist_keep_s"]); n > 0 {
		return n
	}
	return keepDefault
}

func (d *drv) persistDescribe(out map[string]any, caps []string) []string {
	if !d.persists() {
		return caps
	}
	out["verbs"] = append(out["verbs"].([]string), "stop", "start")
	if d.is("persist-cap-unlisted") {
		return caps
	}
	return append(caps, "persist")
}

// stopNow stops a ready persistent box: its processes end, its directory
// stays until the kept disk expires. The lock is held.
func stopNow(state string, r record, why, bug string) {
	if r.State != "ready" {
		return
	}
	killExecs(r)
	endProcs(filepath.Dir(r.Dir), r.Handle)
	if bug != "box-secrets-kept-on-stop" { // a stop powers the guest off: its memory goes (secrets.go)
		endMem(state, r, bug)
	}
	now := time.Now().UTC()
	r.Ran += now.Sub(r.Seg).Seconds()
	r.State, r.Deadline = "stopped", now.Add(time.Duration(cmp.Or(r.Keep, keepDefault))*time.Second)
	_ = why // a real driver says why in status; the reference driver has nothing to add
	_ = save(state, r)
}

// killExecs ends every exec a box still runs (its process groups).
func killExecs(r record) {
	ps, _ := filepath.Glob(filepath.Join(r.Dir, ".execs", "*"))
	for _, p := range ps {
		if pid, err := strconv.Atoi(filepath.Base(p)); err == nil && pid > 0 {
			_ = syscall.Kill(-pid, syscall.SIGKILL)
		}
	}
}

func (d *drv) stop(req request) (any, error) {
	p, err := d.parse(req.ID)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	err = locked(d.state, func() error {
		r, err := load(p)
		if err != nil {
			return err
		}
		switch {
		case !r.Persist && !d.is("stop-any-box"):
			return refuse("bad_request", "%s was acquired without persist: it has no disk to keep (release ends it)", r.Handle)
		case r.State == "ready" && d.is("stop-still-ready"):
		case r.State == "ready":
			r.Persist = true
			stopNow(d.state, r, "", d.bug)
			if r, err = load(p); err != nil {
				return err
			}
		case r.State != "stopped":
			return refuse("conflict", "%s is %s", r.Handle, r.State)
		}
		out = map[string]any{"stopped": r.State == "stopped" || d.is("stop-still-ready"), "expires_at": r.Deadline.Format(time.RFC3339Nano)}
		if s := d.spend(r); s != nil {
			out["spend"] = s
		}
		return nil
	})
	return out, err
}

func (d *drv) startBox(req request) (any, error) {
	p, err := d.parse(req.ID)
	if err != nil {
		return nil, err
	}
	var a struct {
		TTL int `json:"ttl_s"`
	}
	_ = json.Unmarshal(req.Args, &a)
	ttl := cmp.Or(a.TTL, 60)
	if ttl < 0 || ttl > ttlMax {
		return nil, refuse("bad_request", "ttl_s past %d", ttlMax)
	}
	if closed(d.cfg["window"]) {
		return nil, refuse("unavailable", "reclaimed: outside the window %s", d.cfg["window"])
	}
	var out map[string]any
	err = locked(d.state, func() error {
		r, err := load(p)
		if err != nil {
			return err
		}
		switch {
		case !r.Persist:
			return refuse("bad_request", "%s was acquired without persist", r.Handle)
		case r.State == "ready": // started already: the same answer
		case r.State != "stopped":
			return refuse("conflict", "%s is %s: its disk is gone", r.Handle, r.State)
		default:
			if d.is("persist-wipes") {
				_ = os.RemoveAll(r.Dir)
				_ = os.MkdirAll(r.Dir, 0o700)
			}
			now := time.Now().UTC()
			r.State, r.Seg, r.Deadline = "ready", now, now.Add(time.Duration(ttl)*time.Second)
			if err := save(d.state, r); err != nil {
				return err
			}
		}
		out = d.answer(r)
		return nil
	})
	return out, err
}
