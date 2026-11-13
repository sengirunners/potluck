package vmhost

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/sengirunners/potluck/protocol"
)

// Internal states besides SPEC 20's: starting (cloned, booting, fetching; on
// the wire it is running) and failed (it never started; acquire reports why
// and drops the record).
const (
	stStarting  = "starting"
	stRunning   = "running"
	stReady     = "ready" // a box: up, answering exec
	stEnded     = "ended"
	stExpired   = "expired"
	stReleased  = "released"
	stReclaimed = "reclaimed"
	stLost      = "lost"
	stFailed    = "failed"
	stStopped   = "stopped" // a persistent box: powered off, its disk kept until Deadline (persist.go)
)

// keepFor is how long an ended handle's record stays (SPEC 20: at least 24 h).
const keepFor = 7 * 24 * time.Hour

type record struct {
	Handle, Target, Key, ID string
	Kind                    string           // "run" ("" in older records) or "box"
	Digest                  string           // a box: the image_digest it booted
	JIT                     bool             // a box: acquire brought a jit_config (the value is never stored)
	JITRepo                 string           // a box's jit_guard: the repository its runner serves (lowercase owner/name)
	JITEvents               []string         // and the events whose runs it serves (none: any)
	JITOpenIfPrivate        bool             // and, beyond those, any run whose event payload says the repository is private
	JITRun                  *protocol.JITRun // and the one run it serves, held for the door's ready marker (nil: any run)
	Net                     string           // a box: acquire's net, "on", "off" or "" (unsaid: jobNet)
	Persist                 bool             // a box acquired with persist: a stop keeps its VM (persist.go)
	KeepS                   int              // a persistent box: how long a stopped disk is kept (persist_keep_s)
	Seg                     time.Time        // a persistent box: when its current time up began (zero: stopped)
	RanMs                   int64            // a persistent box: milliseconds up before Seg
	Starts                  int              // a persistent box: starts after a stop (0: the first boot, which clones)
	Finished                int              // a persistent box: 1 + the Starts whose supervisor finished with the VM
	State, Why              string
	OS, VM, Image           string
	Remote, Ref, Commit     string
	Script                  string
	Args                    []string
	TTL                     int
	Exit                    *int
	Start, RunStart         time.Time
	Deadline, End           time.Time
	Supervisor, RunPID      int
	Log                     string
	Config                  map[string]string
	Engine                  *dockerAllocation
	HostBudget              *hostBudget // immutable local allocation, independent of subsequent profile edits
	Volumes                 []volUse    // a box's named volumes (volume.go)
	VolOwner                string      // whose they are
	VolProject              string      // and the owner's project (volume.go caps)
	COWBytes                int64       // what its cow copies hold, as its supervisor last measured
	Warm                    bool        // a warm box no acquire has taken yet (warm.go): no handle on the wire
	Seal                    bool        // released with seal while ready: its build volumes are that release's to seal (volume_seal.go)
	Keep                    []string    // that release's keep: all its build volumes keep when sealed (volume_keep.go)

	asOf time.Time // never stored: the instant a spend is counted to while no End says (stamp)
}

func (r record) live() bool {
	return r.State == stStarting || r.State == stRunning || r.State == stReady
}

// wireState is the state SPEC 20 sees.
func (r record) wireState() string {
	if r.State == stStarting && r.Kind == "box" {
		return stReady
	}
	if r.State == stStarting {
		return stRunning
	}
	return r.State
}

type store struct{ dir string }

func (s store) file(id string) string { return filepath.Join(s.dir, "runs", id+".json") }

func (s store) load(id string) (record, error) {
	var r record
	b, err := os.ReadFile(s.file(id))
	if err != nil {
		return r, err
	}
	return r, json.Unmarshal(b, &r)
}

// read loads a record under the lock, stamped: what a verb answers from.
func (s store) read(id string) (record, error) {
	var r record
	err := s.locked(func() error {
		var err error
		if r, err = s.load(id); err == nil {
			s.stamp(&r)
		}
		return err
	})
	return r, err
}

// stamp sets the instant r's spend counts to while it has no End: now, or,
// for a persistent box that is up, its supervisor's last beat (never later
// than now), which is what a stop after that supervisor died counts to
// (persist.go stoppedAt). Stamped under the lock, it is no later than any
// End or stop a later change stamps (also under the lock, also from now or
// the last beat), so a reported spend never falls (zm-eakh.12.51).
func (s store) stamp(r *record) {
	r.asOf = time.Now().UTC()
	if r.Persist && !r.Seg.IsZero() {
		if b := s.lastBeat(r.ID); b.Before(r.asOf) {
			r.asOf = b
		}
	}
}

func (s store) save(r record) error {
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	p := s.file(r.ID)
	if err := os.WriteFile(p+".tmp", b, 0o600); err != nil {
		return err
	}
	return os.Rename(p+".tmp", p)
}

func (s store) remove(r record) {
	_ = os.Remove(s.file(r.ID))
	_ = os.Remove(s.beatFile(r.ID)) // persist.go
	_ = os.Remove(filepath.Join(s.dir, "logs", r.ID+".log"))
	_ = os.Remove(filepath.Join(s.dir, "logs", r.ID+".supervisor.log"))
	_ = os.Remove(filepath.Join(s.dir, "logs", r.ID+".secrets.lock")) // secrets.go
	s.endProcs(r.ID)
}

func (s store) all() []record {
	ps, _ := filepath.Glob(filepath.Join(s.dir, "runs", "*.json"))
	var out []record
	for _, p := range ps {
		var r record
		if b, err := os.ReadFile(p); err == nil && json.Unmarshal(b, &r) == nil {
			out = append(out, r)
		}
	}
	return out
}

// locked runs f holding the state directory's lock: every change to a record
// and every slot count happens under it. It makes the subdirectories but never
// the state directory itself (create makes that): a supervisor still running
// after the owner removed the directory must find it gone, not recreate it.
func (s store) locked(f func() error) error {
	if _, err := os.Stat(s.dir); err != nil {
		return fmt.Errorf("state directory: %w", err)
	}
	for _, d := range []string{"runs", "logs", "cache"} {
		if err := os.Mkdir(filepath.Join(s.dir, d), 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
			return err
		}
	}
	return flocked(filepath.Join(s.dir, "lock"), f)
}

// create makes the state directory, for the driver's entry point: the one
// place a missing directory is a first run, not a removal.
func (s store) create() error { return os.MkdirAll(s.dir, 0o700) }

func flocked(path string, f func() error) error {
	l, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
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

// update changes one record under the lock.
func (s store) update(id string, f func(*record) error) (record, error) {
	var r record
	err := s.locked(func() error {
		var err error
		if r, err = s.load(id); err != nil {
			return err
		}
		if err = f(&r); err != nil {
			return err
		}
		s.stamp(&r)
		return s.save(r)
	})
	return r, err
}

// vmPrefix named this state directory's VMs before the rename's step 2
// (zmrt-<hash>-); vmPrefixNew names new ones. A record keeps the name its VM
// was made under, and a sweep takes both as its own (ownVM).
func (s store) vmPrefix() string {
	h := sha256.Sum256([]byte(s.dir))
	return "zmrt-" + hex.EncodeToString(h[:4]) + "-"
}

// vmPrefixNew is the prefix of new VMs: potluck-<the same hash>-. The hash
// is of the state directory's path, as before, so an existing directory
// (~/.zm-run-tart, kept where it exists) gives the same hash.
func (s store) vmPrefixNew() string {
	return "potluck-" + strings.TrimPrefix(s.vmPrefix(), "zmrt-")
}

// ownVM reports whether a VM name is this state directory's (either prefix).
func (s store) ownVM(name string) bool {
	return strings.HasPrefix(name, s.vmPrefix()) || strings.HasPrefix(name, s.vmPrefixNew())
}

func alive(pid int) bool { return pid > 0 && syscall.Kill(pid, 0) == nil }
