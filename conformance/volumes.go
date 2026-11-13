package conformance

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/sengirunners/potluck/protocol"
)

// Named volumes (SPEC 20 "Boxes", Named volumes; zm-eakh.12.32.1): acquire's
// volumes, on a driver listing capability volumes, mount directories of the
// volume's owner (volume_owner) that outlive the box. rw is one box's at a
// time; ro (read-only) and cow (the box's own copy) are shared. Seen through
// files the boxes write and read; nothing of the driver's word.
//
// The suite's volumes stay on the host until the driver's keep deletes them
// (SPEC 20 has no verb to delete a volume).

const volPath = "zmconf-vol"

type volAnswer struct {
	Name string `json:"name"`
	Path string `json:"path"`
	Mode string `json:"mode"`
	New  *bool  `json:"new"`
	// Checkout echoes a volume asked as the box's checkout (volumes_checkout.go).
	Checkout bool `json:"checkout"`
}

// acquireVol acquires a box with one volume of owner's in mode.
func (r *run) acquireVol(owner, name, mode string) (held, *protocol.Error, error) {
	return r.acquireVolWith(owner, name, mode, false, r.longTTL())
}

// acquireVolWith is acquireVol with sealed (volumes_seal.go) and a ttl.
func (r *run) acquireVolWith(owner, name, mode string, sealed bool, ttl int) (held, *protocol.Error, error) {
	h := held{key: r.key()}
	v := map[string]any{"name": name, "path": volPath, "mode": mode}
	if sealed {
		v["sealed"] = true
	}
	h.sent = args("request_id", h.key, "actor", actor, "ttl_s", ttl, "volume_owner", owner, "volumes", []map[string]any{v})
	before := time.Now()
	a, e, err := r.acquire(h.sent, r.cfg)
	h.acquired = a
	if err == nil && e == nil {
		err = r.acquireShape(a, ttl, before)
	}
	return h, e, err
}

// volAnswered wants acquire's volumes to name the one asked, with new as want.
func volAnswered(a acquired, name, mode string, isNew bool) error {
	if len(a.Volumes) != 1 {
		return fmt.Errorf("acquire answered volumes %v, want the one asked", a.Volumes)
	}
	v := a.Volumes[0]
	switch {
	case v.Name != name || v.Path != volPath || v.Mode != mode:
		return fmt.Errorf("acquire answered volume %+v, want %s at %s, %s", v, name, volPath, mode)
	case v.New == nil || *v.New != isNew:
		return fmt.Errorf("acquire answered volume new %v, want %v", v.New, isNew)
	}
	return nil
}

// volRead is the volume's file in box h ("" when there is none).
func (r *run) volRead(h string) (string, error) {
	res, err := r.sh(h, `cat "$HOME/$1/f" 2>/dev/null; true`, volPath)
	if err != nil {
		return "", err
	}
	return *res.Stdout, nil
}

// volWrite writes the volume's file in box h: its exit.
func (r *run) volWrite(h, v string) (int, error) {
	res, err := r.exec(h, []string{"sh", "-c", `printf '%s' "$2" > "$HOME/$1/f"`, "sh", volPath, v}, "", 0)
	if err != nil {
		return 0, err
	}
	return *res.Exit, nil
}

func (r *run) volumes() {
	if r.kind != "box" {
		return
	}
	if r.desc.Capabilities == nil || !slices.Contains(*r.desc.Capabilities, protocol.CapVolumes) {
		r.skip("named volumes", "describe lists no volumes capability")
		return
	}
	owner, other, name := "zmconf-owner-"+r.tag, "zmconf-other-"+r.tag, strings.ToLower("zmconf-"+r.tag)
	a, e, err := r.acquireVol(owner, name, "rw")
	defer r.endHeld(a)
	if err = must(e, err); err == nil {
		err = volAnswered(a.acquired, name, "rw", true)
	}
	if !r.add("acquire with a volume answers it (name, path, mode, new)", err) {
		return
	}
	if code, err := r.volWrite(a.Handle, r.tag); err != nil || code != 0 {
		r.add("a rw volume takes a write", fmt.Errorf("exit %d %v", code, err))
		return
	}
	var busy error
	for _, mode := range []string{"rw", "ro", "cow"} {
		b, e, err := r.acquireVol(owner, name, mode)
		r.endHeld(b)
		if err == nil && (e == nil || e.Code != protocol.CodeUnavailable) {
			busy = fmt.Errorf("%s while another box holds it rw answered %v, want unavailable", mode, errOf(e))
			break
		}
	}
	r.add("a rw volume is one box's: another acquire naming it (rw, ro or cow) answers unavailable", busy)
	r.endHeld(a)

	b, e, err := r.acquireVol(owner, name, "rw")
	if err = must(e, err); err == nil {
		err = volAnswered(b.acquired, name, "rw", false)
	}
	if err == nil {
		var got string
		if got, err = r.volRead(b.Handle); err == nil && got != r.tag {
			err = fmt.Errorf("the next box reads %q from the volume, want %q", clip(got), r.tag)
		}
	}
	r.endHeld(b)
	r.add("a volume outlives its box: the next box naming it reads what the last wrote (new false)", err)

	c, e, err := r.acquireVol(other, name, "rw")
	if err = must(e, err); err == nil {
		var got string
		if got, err = r.volRead(c.Handle); err == nil && got != "" {
			err = fmt.Errorf("another owner's box reads %q from a volume of the same name", clip(got))
		}
	}
	if err == nil {
		if code, werr := r.volWrite(c.Handle, "intruder-"+r.tag); werr != nil || code != 0 {
			err = fmt.Errorf("another owner's box could not write its own volume: exit %d %v", code, werr)
		}
	}
	r.endHeld(c)
	if err == nil {
		d, e, derr := r.acquireVol(owner, name, "ro")
		if err = must(e, derr); err == nil {
			var got string
			if got, err = r.volRead(d.Handle); err == nil && got != r.tag {
				err = fmt.Errorf("after another owner wrote a volume of the same name, the owner's reads %q, want %q", clip(got), r.tag)
			}
		}
		r.endHeld(d)
	}
	r.add("another owner's volume of the same name is another volume: it reads nothing of the owner's, and its writes never reach it", err)

	// A request_id is one acquire: the same key with other volumes, or another owner, is refused.
	k := r.key()
	first := args("request_id", k, "actor", actor, "ttl_s", r.longTTL(), "volume_owner", owner, "volumes", []map[string]string{{"name": name, "path": volPath, "mode": "ro"}})
	f, e, err := r.acquire(first, r.cfg)
	err = must(e, err)
	if err == nil {
		var bad error
		for _, again := range []json.RawMessage{
			args("request_id", k, "actor", actor, "ttl_s", r.longTTL(), "volume_owner", owner, "volumes", []map[string]string{{"name": name, "path": volPath + "-other", "mode": "ro"}}),
			args("request_id", k, "actor", actor, "ttl_s", r.longTTL(), "volume_owner", other, "volumes", []map[string]string{{"name": name, "path": volPath, "mode": "ro"}}),
		} {
			res, e, aerr := r.acquire(again, r.cfg)
			if e == nil && aerr == nil && res.Handle != f.Handle {
				r.end(res.Handle)
			}
			if werr := wantCode(e, aerr, protocol.CodeBadRequest, protocol.CodeConflict); werr != nil && bad == nil {
				bad = fmt.Errorf("the same request_id with other volumes or owner: %v", werr)
			}
		}
		err = bad
	}
	if f.Handle != "" {
		r.end(f.Handle)
	}
	r.add("a request_id reused with other volumes or another owner answers bad_request (or conflict)", err)

	ro, e, err := r.acquireVol(owner, name, "ro")
	defer r.endHeld(ro)
	cw, e2, err2 := r.acquireVol(owner, name, "cow")
	defer r.endHeld(cw)
	if err = cmpErr(must(e, err), must(e2, err2)); !r.add("ro and cow boxes share a volume", err) {
		return
	}
	w, e3, err3 := r.acquireVol(owner, name, "rw")
	r.endHeld(w)
	if err3 == nil && (e3 == nil || e3.Code != protocol.CodeUnavailable) {
		err3 = fmt.Errorf("rw beside ro and cow boxes answered %v, want unavailable", errOf(e3))
	}
	r.add("rw waits for readers: an rw acquire while ro or cow boxes hold the volume answers unavailable", err3)
	code, err := r.volWrite(ro.Handle, "ro-"+r.tag)
	if err == nil && code == 0 {
		err = fmt.Errorf("a write to a ro volume exited 0")
	}
	r.add("a ro volume refuses writes", err)
	code, err = r.volWrite(cw.Handle, "cow-"+r.tag)
	if err == nil && code != 0 {
		err = fmt.Errorf("a write to a cow volume exited %d", code)
	}
	if err == nil {
		var got string
		if got, err = r.volRead(ro.Handle); err == nil && got != r.tag {
			err = fmt.Errorf("beside a cow box's write a ro box reads %q, want %q", clip(got), r.tag)
		}
	}
	r.endHeld(ro)
	r.endHeld(cw)
	if err == nil {
		d, e, derr := r.acquireVol(owner, name, "rw")
		if err = must(e, derr); err == nil {
			var got string
			if got, err = r.volRead(d.Handle); err == nil && got != r.tag {
				err = fmt.Errorf("after a cow box ended the volume reads %q, want %q", clip(got), r.tag)
			}
		}
		r.endHeld(d)
	}
	r.add("a cow box's writes are its own: never the volume's, nor another box's", err)
	if err == nil {
		r.cowViews(owner, name) // volumes_cow.go
	}

	var bad error
	one := func(n, p, m string) []map[string]string {
		return []map[string]string{{"name": n, "path": p, "mode": m}}
	}
	for _, v := range [][]map[string]string{one(name, "../zmconf-escape", "rw"), one("Not A Name", volPath, "rw"), one(name, volPath, "rwx"),
		one(name, "/etc", "rw"), one(name, "/", "rw"), one(name, "a/../../x", "rw"), one(name, "x,readonly=false", "rw"), one(name, "/home/zmconf", "rw"),
		{{"name": name, "path": "zmconf-a", "mode": "rw"}, {"name": name + "-2", "path": "zmconf-a/b", "mode": "ro"}},
		{{"name": name, "path": "zmconf-a", "mode": "ro"}, {"name": name, "path": "zmconf-b", "mode": "ro"}}} {
		res, e, err := r.acquire(args("request_id", r.key(), "actor", actor, "volume_owner", owner, "volumes", v), r.cfg)
		if e == nil && err == nil {
			r.end(res.Handle) // a box the suite must not leave
		}
		if err := wantCode(e, err, protocol.CodeBadRequest); err != nil && bad == nil {
			bad = fmt.Errorf("volume %v: %v", v, err)
		}
	}
	r.add("a volume named, placed or moded badly answers bad_request (absolute, /, climbing or option-bearing paths, overlapping paths, a name twice)", bad)
	r.seals(owner, other, name) // volumes_seal.go
}

func errOf(e *protocol.Error) any {
	if e == nil {
		return "a box"
	}
	return e.Code
}

func cmpErr(errs ...error) error {
	for _, e := range errs {
		if e != nil {
			return e
		}
	}
	return nil
}
