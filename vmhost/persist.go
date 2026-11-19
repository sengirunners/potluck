package vmhost

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
)

// Kept disks (SPEC 20 "Boxes", Kept disks; zm-eakh.12.5.23): a box acquired
// with persist is powered off rather than deleted when it stops (the stop
// verb, its lease's end, a reclaim, its supervisor dying, the guest shutting
// itself down), and start boots the same VM again under a new supervisor
// with a new lease. A stopped box holds no slot, only its disk, until
// Deadline (persist_keep_s after the stop, 30 days by default), when the
// sweep deletes it; release deletes it at once. At most persist_max
// persistent boxes live in a state directory, stopped or not. The VM still
// never runs without its supervisor: the guard of a kept boot powers the VM
// off (keeping it) when the supervisor's pipe closes.

const (
	persistMaxDefault  = 4
	persistKeepDefault = 30 * 24 * 3600
	// persistMinFreeDefault is the free disk (GiB) a host keeps beside kept disks.
	persistMinFreeDefault = 10
	// syncFor bounds the guest's sync before a stop powers it off.
	syncFor = 10 * time.Second
)

// keeper is a layer that can keep a VM's disk across a power-off: Tart (the
// VM's own disk) and OCI (the same container; its HOME on a volume, kept
// under any runtime). Incus does not yet.
type keeper interface {
	// CloneKept is Clone for a box whose disk is kept.
	CloneKept(image, vm string, cpu, memory int) error
	// BootKept is Boot, but when the pipe closes the guard powers the VM
	// off and keeps it.
	BootKept(vm string, isolated bool, log io.Writer) (*exec.Cmd, io.WriteCloser, error)
	// PowerOff stops the VM (runPID as Destroy's) and keeps its disk. It is
	// safe to call twice and on a VM that is off.
	PowerOff(vm string, runPID int)
	// KeepsRoot says whether everything on the guest's disk survives a
	// power-off, or only HOME.
	KeepsRoot() bool
}

// persists says whether this binding keeps disks (describe's persist).
func (d *drv) persists() bool {
	// A kept container fixes its cgroup parent at create time. Do not claim
	// safe restart until it can receive a newly admitted parent and guard.
	if d.cfg.VM == "oci" {
		if _, err := d.requestedHostBudget(); err != nil {
			return false
		}
		if d.nativeRuntime || d.cfg.OCIHostProfile != "" {
			return false
		}
	}
	_, ok := d.vm.(keeper)
	return ok && d.cfg.Kind == "box" && d.cfg.PersistMax > 0
}

// kept counts this state directory's persistent boxes, stopped or not.
// Lock held (or a snapshot for describe).
func (d *drv) kept() int {
	n := 0
	for _, r := range d.st.all() {
		if r.Persist && (r.live() || r.State == stStopped) {
			n++
		}
	}
	return n
}

// disks is describe's account of kept disks.
func (d *drv) disks() map[string]any {
	k, _ := d.vm.(keeper)
	return map[string]any{"kept": d.kept(), "max": d.cfg.PersistMax, "keep_s": d.cfg.PersistKeepS, "root": k != nil && k.KeepsRoot()}
}

func (d *drv) powerOff(vm string, runPID int) {
	if k, ok := d.vm.(keeper); ok {
		k.PowerOff(vm, runPID)
	}
}

// since is when a record's lifetime cap counts from: its acquire, or a
// persistent box's last start.
func (r record) since() time.Time {
	if r.Persist && !r.Seg.IsZero() {
		return r.Seg
	}
	return r.Start
}

// stopped makes a live persistent record stopped: its time up so far is
// counted, and its disk is kept for KeepS from now.
func stopped(r *record, why string) { stoppedAt(r, why, time.Now().UTC()) }

// stoppedAt is stopped whose time up ended at upTo (a dead supervisor's last
// beat: the host's downtime is not time up); the disk's keep runs from now.
func stoppedAt(r *record, why string, upTo time.Time) {
	now := time.Now().UTC()
	if upTo.After(now) {
		upTo = now
	}
	if !r.Seg.IsZero() && upTo.After(r.Seg) {
		r.RanMs += upTo.Sub(r.Seg).Milliseconds()
	}
	r.State, r.Why, r.Seg, r.RunPID = stStopped, why, time.Time{}, 0
	r.Deadline = now.Add(r.keptFor())
}

// keptFor is how long a stopped box's disk is kept from its stop.
func (r record) keptFor() time.Duration { return time.Duration(max(r.KeepS, 60)) * time.Second }

// cutAt is when r ended, or for a stopped box when it stopped.
func (r record) cutAt() time.Time {
	if r.State == stStopped {
		return r.Deadline.Add(-r.keptFor())
	}
	return r.End
}

// guestSync flushes the guest's writes to its disk before a power-off.
func guestSync(l vmLayer, vm string) {
	ctx, cancel := context.WithTimeout(context.Background(), syncFor)
	defer cancel()
	cmd := l.Exec(vm, nil, io.Discard, io.Discard, "sync")
	if cmd.Start() != nil {
		return
	}
	stop := context.AfterFunc(ctx, func() { _ = cmd.Process.Kill() })
	defer stop()
	_ = cmd.Wait()
}

// stop powers a persistent box off and keeps its disk. A stopped box
// answers as it is.
func (d *drv) stop(handle string) (any, error) {
	r, err := d.find(handle)
	if err != nil {
		return nil, err
	}
	if !r.Persist {
		return nil, refuse("bad_request", "%s was acquired without persist: it has no disk to keep (release ends it)", r.Handle)
	}
	if r.State == stReady {
		guestSync(d.vm, r.VM)
	}
	var off *record
	r, err = d.st.update(r.ID, func(r *record) error {
		switch {
		case r.State == stStopped:
		case r.live():
			cp := *r // its boot process, which stopped clears
			off = &cp
			stopped(r, "")
		default:
			return refuse("conflict", "%s is %s", r.Handle, r.wireState())
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if off != nil { // the supervisor sees the state too; powering off here makes stop synchronous
		d.powerOff(off.VM, off.RunPID)
	}
	return map[string]any{"stopped": true, "expires_at": r.Deadline.Format(time.RFC3339Nano), "spend": d.spend(r)}, nil
}

// start boots a stopped box's kept VM again: a new supervisor, a new lease
// of ttl_s (default as acquire's), its lifetime cap counted from now.
func (d *drv) start(handle string, raw json.RawMessage) (any, error) {
	r, err := d.find(handle)
	if err != nil {
		return nil, err
	}
	var a struct {
		TTL *int `json:"ttl_s"`
	}
	if len(raw) > 0 && string(raw) != "null" {
		if err := json.Unmarshal(raw, &a); err != nil {
			return nil, refuse("bad_request", "bad args: %v", err)
		}
	}
	ttl := min(defaultTTL, d.cfg.TTLMax)
	if a.TTL != nil {
		ttl = *a.TTL
	}
	switch {
	case !r.Persist:
		return nil, refuse("bad_request", "%s was acquired without persist", r.Handle)
	case ttl <= 0 || ttl > d.cfg.TTLMax:
		return nil, refuse("bad_request", "ttl_s %d is outside 1..%d", ttl, d.cfg.TTLMax)
	}
	// The last supervisor is done with the VM first: its guard would power
	// off a VM booted under it.
	busy := func(r record) bool { return r.Finished != r.Starts+1 && alive(r.Supervisor) }
	for wait := time.Now().Add(10 * time.Second); r.State == stStopped && busy(r) && time.Now().Before(wait); {
		time.Sleep(50 * time.Millisecond)
		if o, err := d.st.load(r.ID); err == nil {
			r = o
		}
	}
	if r.State == stStopped && busy(r) {
		return nil, refuse("unavailable", "%s is still stopping; start it again in a moment", r.Handle)
	}
	starts := 0
	err = d.st.locked(func() error {
		r, err = d.st.load(r.ID)
		switch {
		case err != nil:
			return err
		case r.live():
			d.st.stamp(&r)
			return nil // started already: the same answer
		case r.State != stStopped:
			return refuse("conflict", "%s is %s: its disk is gone", r.Handle, r.wireState())
		}
		if d.cfg.VM == "oci" { // oci_root.go: the container keeps the flags it was made with
			if why := keptRootDiffers(r.Config, d.cfg); why != "" {
				return refuse("conflict", "%s: %s cannot start as the binding now says (its disk stays kept; zm sandbox rm deletes it)", why, r.Handle)
			}
			// docker resolves runsc-root at every start, so it is checked again.
			if o, ok := d.vm.(ociCLI); ok && o.root && o.runtime == ociSudoRuntime {
				if err := o.checkSudoRuntime(); err != nil {
					return refuse("unavailable", "%v (its disk stays kept)", err)
				}
			}
		}
		if ok, why := d.cfg.admitsNow(); !ok { // hostmode.go
			return refuse("unavailable", "%s (its disk stays kept)", why)
		}
		free, err := d.free(r.OS)
		if err != nil {
			return refuse("unavailable", "%v", err)
		}
		if free == 0 {
			return refuse("unavailable", "%s (%d in use); its disk stays kept", NoFreeSlot(r.OS), d.cfg.slots(r.OS))
		}
		if err := d.diskRoom(); err != nil { // a booted box writes again
			return err
		}
		r.Starts++
		starts = r.Starts
		r.State, r.Why, r.TTL, r.Seg = stStarting, "", ttl, time.Now().UTC()
		r.Config = d.rawConfig() // the binding as it is now (its slots, reclaim policy)
		if err := d.st.save(r); err != nil {
			return err
		}
		pid, err := d.spawnSupervisor(r.ID, "")
		if err != nil {
			stopped(&r, "its supervisor did not start: "+err.Error())
			_ = d.st.save(r)
			return err
		}
		r.Supervisor = pid
		return d.st.save(r)
	})
	if err != nil {
		return nil, err
	}
	if starts > 0 {
		if r, err = d.waitRestarted(r.ID, starts); err != nil {
			return nil, err
		}
	}
	return d.answer(r), nil
}

// waitRestarted waits for a start's supervisor to make the box ready; a
// start that failed leaves it stopped, its disk kept, with why.
func (d *drv) waitRestarted(id string, starts int) (record, error) {
	deadline := time.Now().Add(time.Duration(d.cfg.BootTimeout+15) * time.Second)
	for {
		r, err := d.st.read(id)
		switch {
		case err != nil:
			return r, err
		case r.Starts != starts || r.State == stReady:
			return r, nil
		case r.State == stStopped:
			return r, refuse("unavailable", "the box did not start (its disk is kept): %s", r.Why)
		case !r.live():
			return r, refuse("unavailable", "the box did not start: it is %s", r.wireState())
		case !time.Now().Before(deadline):
			return r, refuse("internal", "the box did not start in time; its supervisor may still make it ready") // not a refusal: the box may still come up
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// persistent says whether the supervisor's box keeps its disk.
func (s *sup) persistent() bool {
	r, err := s.st.load(s.id)
	return err == nil && r.Persist
}

// finished records that this supervisor is done with the VM (a start waits
// for it).
func (s *sup) finished() {
	_, _ = s.st.update(s.id, func(r *record) error {
		if r.Starts == s.starts {
			r.Finished = s.starts + 1
		}
		return nil
	})
}

// powerOff powers the supervisor's VM off at once, keeping it.
func (s *sup) powerOff() {
	if k, ok := s.vm.(keeper); ok {
		k.PowerOff(s.name, s.runPID)
	}
}

// beatEvery is how often a supervisor marks itself alive (beat), so a stop
// after it died counts time up only to its last beat.
const beatEvery = 5 * time.Second

func (st store) beatFile(id string) string { return filepath.Join(st.dir, "runs", id+".beat") }

// beat touches the supervisor's beat file, at most every beatEvery.
func (s *sup) beat() {
	if time.Since(s.lastBeat) < beatEvery {
		return
	}
	s.lastBeat = time.Now()
	p := s.st.beatFile(s.id)
	if err := os.Chtimes(p, s.lastBeat, s.lastBeat); err != nil {
		_ = os.WriteFile(p, nil, 0o600)
	}
}

// lastBeat is when a record's supervisor last marked itself alive (zero:
// never, so no time up is counted past the record's last change).
func (st store) lastBeat(id string) time.Time {
	if fi, err := os.Stat(st.beatFile(id)); err == nil {
		return fi.ModTime()
	}
	return time.Time{}
}

// ociCLI's free disk and a refused kept boot (persist.go's keeper extras).

// diskDir is where a layer keeps VM disks, for the free-disk floor.
type diskDir interface{ DiskDir() string }

// freeGiB is the free space under dir, in GiB (-1 when it cannot be read).
func freeGiB(dir string) float64 {
	var fs syscall.Statfs_t
	if dir == "" || syscall.Statfs(dir, &fs) != nil {
		return -1
	}
	return float64(fs.Bavail) * float64(fs.Bsize) / (1 << 30)
}

// diskRoom refuses a kept disk when the layer's disk directory has less free
// space than persist_min_free_gb: kept disks live for weeks.
func (d *drv) diskRoom() error { return d.diskFloor("kept disks") }

// diskFloor refuses what writes for weeks (kept disks, volumes) below
// persist_min_free_gb free under the layer's disk directory.
func (d *drv) diskFloor(what string) error { return diskFloorOf(d.cfg, d.vm, what)() }

// diskFloorOf is the floor for what on layer vm (diskFloor's; the shelf's
// filler has no drv): the answer checks the free disk again each time it is
// called, under the disk directory the layer named once.
func diskFloorOf(cfg config, vm any, what string) func() error {
	dd, ok := vm.(diskDir)
	if !ok || cfg.PersistMinFreeGB <= 0 {
		return func() error { return nil }
	}
	dir := dd.DiskDir()
	return func() error {
		free := freeGiB(dir)
		if f, ok := vm.(interface{ FreeGiB() float64 }); ok { // a layer that knows its own free disk (the tests' fake)
			free = f.FreeGiB()
		}
		if free >= 0 && free < float64(cfg.PersistMinFreeGB) {
			return refuse("unavailable", "this host has %.1f GiB free under %s, below its floor for %s (persist_min_free_gb %d)", free, dir, what, cfg.PersistMinFreeGB)
		}
		return nil
	}
}
