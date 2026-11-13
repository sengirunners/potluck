package refdriver

import (
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"time"
)

// Warm boxes (SPEC 20 "Boxes", Warm boxes; zm-eakh.12.5.28): config warm=N
// keeps N box directories made ahead, nobody's (no handle on the wire). An
// acquire a warm box can serve (no commit, persist, volumes, image or net)
// takes one: its lease and clock start at the take; the pool refills at once.

func (d *drv) warmN() int { return atoi(d.cfg["warm"]) }

// fillWarm makes warm boxes up to warm. The lock is held.
func (d *drv) fillWarm() {
	n := d.warmN()
	if d.cfg["kind"] != "box" || n <= 0 {
		return
	}
	marker := filepath.Join(d.state, "warm-filled")
	if _, err := os.Stat(marker); err == nil && d.is("warm-no-refill") {
		return
	}
	have := 0
	for _, r := range records(d.state) {
		if r.State == "warm" {
			have++
		}
	}
	for i := 0; have < n; i++ {
		id := "w" + strconv.FormatInt(time.Now().UnixNano(), 36) + strconv.Itoa(i)
		r := record{Handle: "box:" + d.cfg["target"] + "/" + id, Kind: "box", State: "warm", Dir: filepath.Join(d.state, "box-"+id), Start: time.Now().UTC()}
		if os.MkdirAll(r.Dir, 0o700) != nil || save(d.state, r) != nil {
			return
		}
		have++
	}
	_ = os.WriteFile(marker, nil, 0o600)
}

func (d *drv) warmDescribe(out map[string]any) {
	if d.warmN() <= 0 {
		return
	}
	ready := 0
	_ = locked(d.state, func() error {
		d.fillWarm()
		for _, r := range records(d.state) {
			if r.State == "warm" {
				ready++
			}
		}
		return nil
	})
	out["warm"] = map[string]any{"target": d.warmN(), "ready": ready}
	if d.is("warm-bad-describe") {
		out["warm"] = map[string]any{"ready": ready}
	}
}

// boxRoom refuses a box when config boxes=N live boxes are up already (warm
// boxes are not counted): a binding with few slots and a pool bigger than
// them. The lock is held.
func (d *drv) boxRoom(all []record) error {
	n := atoi(d.cfg["boxes"])
	for _, r := range all {
		if n > 0 && r.Kind == "box" && r.State == "ready" {
			n--
			if n == 0 {
				return refuse("unavailable", "no room: %s boxes are up", d.cfg["boxes"])
			}
		}
	}
	return nil
}

// takeWarm gives an acquire a warm box when one can serve it. The lock is held.
func (d *drv) takeWarm(a acquireArgs) (map[string]any, bool) {
	if d.warmN() <= 0 || a.Commit != "" || a.Persist || len(a.Volumes) > 0 || a.Net != "" || a.Image != "" && a.Image != boxImage {
		return nil, false
	}
	rs := records(d.state)
	if d.is("warm-reused-late") { // oldest first, so an ended box put back is taken second
		sort.SliceStable(rs, func(i, j int) bool { return rs[i].Start.Before(rs[j].Start) })
	}
	for _, r := range rs {
		if r.State != "warm" {
			continue
		}
		now := time.Now().UTC()
		ttl := time.Duration(a.TTL) * time.Second
		r.Key, r.State, r.Window, r.Warm = a.RequestID, "ready", d.cfg["window"], true
		r.Start, r.Seg, r.Deadline = now, now, now.Add(ttl)
		if d.is("warm-old-clock") { // the lease counted from when the box was made
			r.Deadline = r.Start.Add(ttl)
			if b, err := load(recordFile(d.state, r.Handle)); err == nil {
				r.Start, r.Seg, r.Deadline = b.Start, b.Start, b.Start.Add(ttl)
			}
		}
		if save(d.state, r) != nil || d.superviseRecord(r) != nil {
			return nil, false
		}
		d.fillWarm()
		if r, err := load(recordFile(d.state, r.Handle)); err == nil {
			return d.answer(r), true
		}
	}
	return nil, false
}

// poolTakes is the bug warm-volume-reused: an ended box that had a volume
// joins the warm pool, its directory as it is. The lock is held.
func (d *drv) poolTakes(r record) {
	_ = save(d.state, record{Handle: r.Handle + "w", Kind: "box", State: "warm", Dir: r.Dir, Start: time.Now().UTC()})
}
