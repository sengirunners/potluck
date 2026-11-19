package vmhost

// The host override (zm-eakh.12.14): the owner of a shared host opens,
// drains or closes it now, whatever its bindings' policies say, with one
// host-wide file, {mode, until, since, deadline_s}, that `potluck-driver-tart
// host set` writes (hostset.go) and every binding's driver and supervisor
// reads:
//
//	open    new work starts and running work runs; the policy is not asked (caps and admission_url still are)
//	drain   no new work; running work finishes (drain.go), and is reclaimed once the drain deadline passes
//	closed  no new work; running work is reclaimed now
//	auto    the binding's policy (reclaim.go): owner input inside the window reclaims now, leaving the window drains
//
// No file is auto: the binding's policy alone. A file that cannot be
// read as one (bad JSON, an unknown mode or field, a symlink, writable by
// anyone but its owner) refuses new work and leaves running work alone: it
// never opens the host and never cuts a job over a typo, while the binding's
// policy still ends running work as it would with no file. `until`
// (absolute, resolved when written) returns the host to auto. The file lives
// outside any home, in a directory its owner made; both must belong to the
// host's owner (binding key host_owner_uid, default the driver's own uid) or
// root, so a run user other than the owner, or any other local user, may
// read it but cannot write or plant it.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/sengirunners/potluck/protocol"
)

const drainDeadlineDefault = 1800

// Override modes.
const (
	modeAuto   = protocol.HostAuto
	modeOpen   = protocol.HostOpen
	modeDrain  = protocol.HostDrain
	modeClosed = protocol.HostClosed
)

// hostOverridePath is the host-wide override file. Tests point it elsewhere
// (fake_test.go's TestMain); nothing else changes it.
var hostOverridePath = defaultOverridePath()

// HostOverrideFile is where the host override file is (or would be): the
// one file `potluck host set` and `potluck host pause` write, outside any
// home. `potluck uninstall` removes it.
func HostOverrideFile() string { return hostOverridePath }

func defaultOverridePath() string {
	if runtime.GOOS == "darwin" {
		return "/Users/Shared/Potluck/host-override.json"
	}
	return "/etc/potluck/host-override.json"
}

// override is the file's content.
type override = HostOverride // hostset.go: the one definition

// maxDrainDeadline bounds deadline_s and drain_deadline_s: a week.
const maxDrainDeadline = 7 * 24 * 3600

func (o override) check() error {
	switch o.Mode {
	case modeAuto, modeOpen, modeDrain, modeClosed:
	default:
		return fmt.Errorf("mode %q is not open, drain, closed or auto", o.Mode)
	}
	if o.Since.IsZero() {
		return errors.New("no since")
	}
	if o.Until != nil && !o.Until.After(o.Since) {
		return errors.New("until is not after since")
	}
	if o.DeadlineS != nil && (o.Mode != modeDrain || *o.DeadlineS < 0 || *o.DeadlineS > maxDrainDeadline) {
		return fmt.Errorf("deadline_s is a drain's, 0..%d", maxDrainDeadline)
	}
	return nil
}

// at is the mode in force at now: auto once until has passed.
func (o override) at(now time.Time) string {
	if o.Until != nil && !now.Before(*o.Until) {
		return modeAuto
	}
	return o.Mode
}

// endedAt is when auto took over from the override, at now (zero: no
// override, or not yet).
func (o override) endedAt(now time.Time) time.Time {
	switch {
	case o.Until != nil && !now.Before(*o.Until):
		return *o.Until
	case o.Mode == modeAuto:
		return o.Since
	}
	return time.Time{}
}

// readOverride reads the override file, which owner (or root) must own with
// its directory: none is {mode: auto}; anything it cannot trust is an error,
// which refuses new work.
func readOverride(path string, owner int) (override, error) {
	auto := override{Mode: modeAuto}
	fi, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		// No file is auto, unless others could have removed one.
		if di, derr := os.Lstat(filepath.Dir(path)); derr == nil {
			if err := ownersOnly(filepath.Dir(path), di, true, owner); err != nil {
				return override{}, err
			}
		} else if !errors.Is(derr, os.ErrNotExist) {
			return override{}, derr
		}
		return auto, nil
	}
	if err != nil {
		return override{}, err
	}
	if err := ownersOnly(path, fi, false, owner); err != nil {
		return override{}, err
	}
	if di, err := os.Lstat(filepath.Dir(path)); err != nil {
		return override{}, err
	} else if err := ownersOnly(filepath.Dir(path), di, true, owner); err != nil {
		return override{}, err
	}
	f, err := os.Open(path)
	if err != nil {
		return override{}, err
	}
	defer f.Close()
	if ofi, err := f.Stat(); err != nil || !os.SameFile(fi, ofi) {
		return override{}, fmt.Errorf("%s changed while it was read", path)
	}
	b, err := io.ReadAll(io.LimitReader(f, 4<<10))
	if err != nil {
		return override{}, err
	}
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	var o override
	if err := dec.Decode(&o); err != nil {
		return override{}, fmt.Errorf("%s: %v", path, err)
	}
	if dec.More() {
		return override{}, fmt.Errorf("%s: more than one JSON value", path)
	}
	if err := o.check(); err != nil {
		return override{}, fmt.Errorf("%s: %v", path, err)
	}
	return o, nil
}

// ownersOnly refuses a symlink, a file that is not regular (or, with dir, a
// directory), one owned by anyone but owner or root, or one anyone but its
// owner may write.
func ownersOnly(path string, fi os.FileInfo, dir bool, owner int) error {
	st, ok := fi.Sys().(*syscall.Stat_t)
	switch {
	case !ok:
		return fmt.Errorf("%s: its owner cannot be read", path)
	case int(st.Uid) != owner && st.Uid != 0:
		return fmt.Errorf("%s is owned by uid %d, not the host's owner (uid %d, host_owner_uid) or root", path, st.Uid, owner)
	case fi.Mode()&os.ModeSymlink != 0:
		return fmt.Errorf("%s is a symlink", path)
	case dir && !fi.IsDir():
		return fmt.Errorf("%s is not a directory", path)
	case !dir && !fi.Mode().IsRegular():
		return fmt.Errorf("%s is not a regular file", path)
	case fi.Mode().Perm()&0o022 != 0:
		return fmt.Errorf("%s may be written by others than its owner (mode %o)", path, fi.Mode().Perm())
	}
	return nil
}

// hostVerdict is what the host's state means for work.
type hostVerdict int

const (
	hostAdmits hostVerdict = iota // new work starts; running work runs
	hostHolds                     // no new work; running work runs (an override that cannot be read)
	hostDrains                    // no new work; running work finishes, idle work ends (drain.go)
	hostCloses                    // no new work; running work is reclaimed now
)

// drainCut is the why of work a drain's deadline reclaimed; describe counts them.
const drainCut = "the drain deadline passed"

// hostState is the host's state for one binding at one moment.
type hostState struct {
	verdict  hostVerdict
	mode     string    // the override's mode in force ("auto" with none), or "unreadable"
	until    time.Time // when the override returns to auto (zero: it does not)
	why      string    // why new work is refused ("" when it is not)
	since    time.Time // a drain's start
	deadline time.Duration
	rd       reading // the policy's reading, for reclaimTiming
}

// host reads the override and, in auto, the binding's policy.
func (c config) host() hostState {
	ov, err := readOverride(hostOverridePath, c.HostOwner)
	return c.hostAt(time.Now(), ov, err, c.Reclaim.check)
}

// hostAt decides the host's state from the override (or why it cannot be
// read) and, only in auto, the policy. An override that cannot be read
// holds the host, or does what the policy alone would when that is more.
func (c config) hostAt(now time.Time, ov override, ovErr error, policy func() (bool, string, reading)) hostState {
	if ovErr != nil {
		h := c.hostAt(now, override{Mode: modeAuto}, nil, policy)
		h.mode = protocol.HostUnreadable
		bad := "the host override cannot be read, so no new work starts: " + ovErr.Error()
		if h.verdict <= hostHolds {
			h.verdict, h.why = hostHolds, bad
		} else {
			h.why = bad + "; " + h.why
		}
		return h
	}
	h := hostState{mode: ov.at(now), rd: reading{at: now}}
	if ov.Until != nil && h.mode != modeAuto {
		h.until = *ov.Until
	}
	switch h.mode {
	case modeOpen:
		return h
	case modeClosed:
		h.verdict, h.why = hostCloses, "the host's owner closed it"
		return h
	case modeDrain:
		h.verdict, h.why, h.since = hostDrains, "the host's owner is draining it", ov.Since
		h.deadline = time.Duration(c.DrainDeadline) * time.Second
		if ov.DeadlineS != nil {
			h.deadline = time.Duration(*ov.DeadlineS) * time.Second
		}
	default: // auto
		if !c.Reclaim.on() {
			return h
		}
		ok, why, rd := policy()
		h.rd = rd
		if ok {
			return h
		}
		h.why = why
		if !rd.outside { // owner input inside the window, or an idle time unread: the host is theirs now
			h.verdict = hostCloses
			return h
		}
		// The drain began when the window closed, or when the override that
		// kept the host open ended, whichever is later.
		h.verdict, h.since = hostDrains, c.Reclaim.lastClose(rd.at)
		if end := ov.endedAt(now); end.After(h.since) {
			h.since = end
		}
		h.deadline = time.Duration(c.DrainDeadline) * time.Second
	}
	if !now.Before(h.since.Add(h.deadline)) {
		h.verdict, h.why = hostCloses, h.why+": "+drainCut
	}
	return h
}

// admitsNow says whether new work may start now, and why not.
func (c config) admitsNow() (bool, string) {
	h := c.host()
	return h.verdict == hostAdmits, h.refusal()
}

// refusal is why new work is refused ("" when it is not): the policy's
// reasons after "reclaimed: ".
func (h hostState) refusal() string {
	switch {
	case h.verdict == hostAdmits:
		return ""
	case h.mode == modeAuto:
		return "reclaimed: " + h.why
	}
	return h.why
}

// state is describe's word for the verdict.
func (h hostState) state() string {
	return [...]string{hostAdmits: protocol.HostAccepting, hostHolds: protocol.HostHolding, hostDrains: protocol.HostDraining, hostCloses: protocol.HostShut}[h.verdict]
}

// hostStatus is h as describe's host block, without the drain's cut count.
func (h hostState) hostStatus() protocol.HostStatus {
	out := protocol.HostStatus{Mode: h.mode, State: h.state()}
	if !h.until.IsZero() {
		out.Until = h.until.UTC().Format(time.RFC3339)
	}
	if !h.since.IsZero() {
		out.DrainSince = h.since.UTC().Format(time.RFC3339)
		out.DrainDeadline = h.since.Add(h.deadline).UTC().Format(time.RFC3339)
	}
	return out
}

// hostDescribe is describe's host: the override's mode, the state it puts the
// host in and, during a drain, when running work is cut and how much a drain
// has cut so far, and the admission controller's last refusal while it
// stands (admission_refusal.go). Reasons are generic (no app names).
func (d *drv) hostDescribe(h hostState) protocol.HostStatus {
	out := h.hostStatus()
	out.Refused = d.st.refusedNow(d.cfg.AdmissionURL, time.Now())
	if !h.since.IsZero() {
		cut := 0
		for _, r := range d.st.all() {
			// A kept box cut is stopped, its disk kept; End is the cut either way.
			if r.Target == d.cfg.Target && (r.State == stReclaimed || r.State == stStopped) && strings.HasSuffix(r.Why, drainCut) && !r.cutAt().Before(h.since) {
				cut++
			}
		}
		out.DrainCut = &cut
	}
	return out
}

// HostNow is the host switch as a binding with no schedule of its own reads
// it now, for a program that links vmhost and has no binding to describe:
// the override file read with the driver's own rules, owner (host_owner_uid)
// owning it, and drainDeadlineS a binding's drain_deadline_s (outside
// 0..a week: the default) for a drain that names no deadline. DrainCut is not counted. An
// override that cannot be read answers mode unreadable, state holding, and
// why.
func HostNow(owner, drainDeadlineS int) (protocol.HostStatus, error) {
	if drainDeadlineS < 0 || drainDeadlineS > maxDrainDeadline {
		drainDeadlineS = drainDeadlineDefault
	}
	c := config{HostOwner: owner, DrainDeadline: drainDeadlineS}
	ov, err := readOverride(hostOverridePath, owner)
	return c.hostAt(time.Now(), ov, err, nil).hostStatus(), err
}
