package refdriver

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"

	"github.com/sengirunners/potluck/protocol"
)

// Named volumes (SPEC 20 "Boxes", Named volumes; zm-eakh.12.32.1): a volume
// is a directory in the state directory, its owner's (volume_owner, else
// actor); a box mounts it under its HOME: rw as a symlink to it, ro as a copy
// with no write bits, cow as a copy of its own. rw is one box's at a time; ro
// and cow share. Config volumes=0 lists no capability (the daemon's tests).
//
// Sealed volumes (capability volumes-seal, zm-eakh.12.65.3): build is rw on a
// volume that did not exist; the builder's release with seal writes the
// sealed record, a file beside the volume's directory (never in it), after
// the box is gone; any other end deletes the volume. Config seal=0 lists no
// capability.

// rvol is one of acquire's volumes, and a box's mount of it.
type rvol struct {
	Name  string `json:"name"`
	Path  string `json:"path"`
	Mode  string `json:"mode"`
	Dir   string `json:"dir,omitempty"`   // the volume's directory (never on the wire)
	Owner string `json:"owner,omitempty"` // whose (never on the wire)
	New   bool   `json:"new,omitempty"`
	// Sealed asks for a sealed volume (ro or cow): not_found unless it is.
	Sealed bool `json:"sealed,omitempty"`
	// Checkout (cow, at most one a box, with volumes-seal): the box's
	// checkout goes here (checkoutInto).
	Checkout bool `json:"checkout,omitempty"`
}

var refVolName = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)

func (d *drv) volumesOn() bool {
	return d.cfg["kind"] == "box" && d.cfg["volumes"] != "0" && !d.is("no-volumes")
}

func (d *drv) volumeDescribe(caps []string) []string {
	if !d.volumesOn() {
		return caps
	}
	caps = append(caps, protocol.CapVolumes)
	if d.sealOn() {
		caps = append(caps, protocol.CapVolumesSeal)
	}
	if d.keepOn() {
		caps = append(caps, protocol.CapVolumesKeep)
	}
	return caps
}

func (d *drv) sealOn() bool { return d.volumesOn() && d.cfg["seal"] != "0" }

// keepOn: capability volumes-keep (config keep=0 lists none).
func (d *drv) keepOn() bool { return d.sealOn() && d.cfg["keep"] != "0" }

// takeVolumes checks acquire's volumes and the lock rules against the live
// boxes in all. The lock is held.
func (d *drv) takeVolumes(a acquireArgs, all []record) ([]rvol, error) {
	if len(a.Volumes) == 0 {
		return nil, nil
	}
	if !d.volumesOn() {
		return nil, refuse("bad_request", "this binding mounts no volumes")
	}
	owner := a.VolumeOwner
	if owner == "" {
		owner = a.Actor
	}
	if owner == "" {
		return nil, refuse("bad_request", "volumes need volume_owner")
	}
	if d.is("volume-shared-owners") {
		owner = "everyone"
	}
	var out []rvol
	for i, v := range a.Volumes {
		if v.Mode == "" {
			v.Mode = protocol.VolumeRW
		}
		clean := path.Clean(v.Path)
		modeOK := v.Mode == protocol.VolumeRW || v.Mode == protocol.VolumeRO || v.Mode == protocol.VolumeCOW || v.Mode == protocol.VolumeBuild && d.sealOn()
		if !d.is("volume-any-path") && (!refVolName.MatchString(v.Name) || !modeOK || !protocol.VolumePathOK(v.Path)) {
			return nil, refuse("bad_request", "volume %q at %q (%s): a plain name, a path under HOME, rw ro or cow (or build)", v.Name, v.Path, v.Mode)
		}
		if v.Sealed && !d.sealOn() && d.is("seal-ignored-without-cap") { // a plain volume for a caller asking a sealed one
			v.Sealed = false
		}
		if v.Sealed && (!d.sealOn() || v.Mode != protocol.VolumeRO && v.Mode != protocol.VolumeCOW) && !d.is("seal-bad-args-ok") {
			return nil, refuse("bad_request", "volume %s: sealed is for ro or cow, on a driver listing volumes-seal", v.Name)
		}
		if v.Checkout && !d.sealOn() && d.is("checkout-ignored-without-cap") { // the driver's own place, as if not asked
			v.Checkout = false
		}
		if v.Checkout && (!d.sealOn() || v.Mode != protocol.VolumeCOW || checkoutVol(a.Volumes[:i]) >= 0) && !d.is("checkout-bad-args-ok") {
			return nil, refuse("bad_request", "volume %s: checkout is for one cow volume, on a driver listing volumes-seal", v.Name)
		}
		if v.Mode == protocol.VolumeBuild && a.Persist && !d.is("seal-bad-args-ok") {
			return nil, refuse("bad_request", "volume %s: a build box is never persist", v.Name)
		}
		for _, o := range a.Volumes[:i] {
			if !d.is("volume-any-path") && (o.Name == v.Name || protocol.VolumePathsOverlap(path.Clean(o.Path), clean)) {
				return nil, refuse("bad_request", "volumes %s and %s: a name twice, or paths that overlap", o.Name, v.Name)
			}
		}
		v.Dir = volDir(d.state, owner, v.Name)
		if v.Sealed {
			if d.is("seal-cross-owner") { // any owner's sealed volume of the name
				v.Dir = sealedByName(d.state, v.Name, v.Dir)
			}
			if !sealed(v.Dir) && !d.is("seal-not-checked") {
				return nil, refuse("not_found", "volume %s is not sealed", v.Name)
			}
		}
		if (v.Mode == protocol.VolumeRW || v.Mode == protocol.VolumeBuild) && sealed(v.Dir) && !d.is("seal-rw-allowed") {
			return nil, refuse("conflict", "volume %s is sealed: ro or cow only", v.Name)
		}
		twoBuilders := v.Mode == protocol.VolumeBuild && d.is("seal-two-builders")
		for _, r := range all {
			if r.State != "ready" || d.is("volume-rw-shared") {
				continue
			}
			for _, u := range r.Volumes {
				switch {
				case u.Dir != v.Dir:
				case u.Mode == protocol.VolumeBuild && twoBuilders:
				case u.Mode == protocol.VolumeBuild:
					return nil, refusal{"unavailable", fmt.Sprintf("volume %s is being built by another box", v.Name),
						protocol.Busy{Reason: protocol.ReasonBuilding, RetryS: buildRetryS}}
				case v.Mode == protocol.VolumeRW && !d.is("volume-rw-ignores-readers") || v.Mode == protocol.VolumeBuild ||
					u.Mode == protocol.VolumeRW || d.is("volume-readers-exclusive"):
					return nil, refuse("unavailable", "volume %s is held by another box (%s)", v.Name, u.Mode)
				}
			}
		}
		if _, err := os.Stat(v.Dir); err == nil && v.Mode == protocol.VolumeBuild && !twoBuilders && !(d.is("seal-rw-allowed") && sealed(v.Dir)) {
			return nil, refuse("conflict", "volume %s exists: build makes a new one", v.Name)
		}
		if d.is("volume-wipes") {
			_ = os.RemoveAll(v.Dir)
		}
		if _, err := os.Stat(v.Dir); err != nil {
			v.New = true
			if d.is("seal-failed-acquire-kept") { // made before the rest of the acquire is checked
				_ = os.MkdirAll(v.Dir, 0o700)
			}
		}
		v.Path, v.Owner = clean, owner
		out = append(out, v)
	}
	// Made only once every volume passed, so a refused acquire makes nothing.
	for i, v := range out {
		if !v.New {
			continue
		}
		if err := os.MkdirAll(v.Dir, 0o700); err != nil {
			dropNewBuilds(out[:i], "")
			return nil, err
		}
	}
	return out, nil
}

// dropNewBuilds deletes the build volumes an acquire made, when the box it
// was for never started (with seal-failed-acquire-kept, it keeps them).
func dropNewBuilds(vols []rvol, bug string) {
	if bug == "seal-failed-acquire-kept" {
		return
	}
	for _, v := range vols {
		if v.New && v.Mode == protocol.VolumeBuild {
			_ = os.RemoveAll(v.Dir)
		}
	}
}

// checkoutVol is the index of the volume in vs marked checkout, or -1.
func checkoutVol(vs []rvol) int {
	for i, v := range vs {
		if v.Checkout {
			return i
		}
	}
	return -1
}

// mountVolumes puts a box's volumes under its HOME.
func (d *drv) mountVolumes(r record) error {
	for _, v := range r.Volumes {
		dst := filepath.Join(r.Dir, v.Path)
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			return err
		}
		mode := v.Mode
		if mode == protocol.VolumeRO && d.is("volume-ro-writable") || mode == protocol.VolumeCOW && d.is("volume-cow-writes-through") ||
			v.Checkout && d.is("checkout-volume-writes-through") {
			mode = protocol.VolumeRW
		}
		var err error
		switch {
		case mode == protocol.VolumeRW || mode == protocol.VolumeBuild:
			err = os.Symlink(v.Dir, dst)
		case mode == protocol.VolumeCOW && d.is("volume-cow-shared-upper"): // one copy for every live cow box of the volume
			upper := v.Dir + "-cow"
			if _, serr := os.Stat(upper); serr != nil {
				err = exec.Command("cp", "-R", v.Dir, upper).Run()
			}
			if err == nil {
				err = os.Symlink(upper, dst)
			}
		default:
			src := v.Dir
			if _, serr := os.Stat(v.Dir + "-last"); serr == nil && (mode == protocol.VolumeCOW && d.is("volume-cow-reused") || mode == protocol.VolumeRO && d.is("volume-ro-sees-last-cow")) {
				src = v.Dir + "-last" // the last cow box's copy
			}
			err = exec.Command("cp", "-R", src, dst).Run()
			if err == nil && d.is("volume-copy-follows-links") {
				err = followLinks(dst)
			}
			if err == nil && mode == protocol.VolumeRO {
				err = exec.Command("chmod", "-R", "a-w", dst).Run()
			}
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// sameVolumes says whether a repeated acquire asks for the volumes the first
// did, for the same owner.
func sameVolumes(had []rvol, a acquireArgs) bool {
	owner := a.VolumeOwner
	if owner == "" {
		owner = a.Actor
	}
	if len(had) != len(a.Volumes) {
		return false
	}
	for i, v := range a.Volumes {
		mode := v.Mode
		if mode == "" {
			mode = protocol.VolumeRW
		}
		if had[i].Name != v.Name || had[i].Path != path.Clean(v.Path) || had[i].Mode != mode || had[i].Owner != owner || had[i].Sealed != v.Sealed || had[i].Checkout != v.Checkout {
			return false
		}
	}
	return true
}

func volAnswers(r record) []map[string]any {
	var out []map[string]any
	for _, v := range r.Volumes {
		a := map[string]any{"name": v.Name, "path": v.Path, "mode": v.Mode, "new": v.New}
		if v.Sealed {
			a["sealed"] = true
		}
		if v.Checkout {
			a["checkout"] = true
		}
		out = append(out, a)
	}
	return out
}

// unlockTree gives the owner write on everything under dir, so it can go.
func unlockTree(dir string) {
	_ = filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error {
		if err == nil && fi.IsDir() && fi.Mode()&os.ModeSymlink == 0 {
			_ = os.Chmod(p, fi.Mode().Perm()|0o700)
		}
		return nil
	})
}

// hasCOW says whether a box mounts a volume cow.
func hasCOW(r record) bool {
	for _, v := range r.Volumes {
		if v.Mode == protocol.VolumeCOW {
			return true
		}
	}
	return false
}

// hasBuild says whether a box holds a volume in build.
func hasBuild(r record) bool {
	for _, v := range r.Volumes {
		if v.Mode == protocol.VolumeBuild {
			return true
		}
	}
	return false
}

// followLinks is the bug volume-copy-follows-links: each symlink under dir
// that names a regular file becomes a copy of that file's bytes.
func followLinks(dir string) error {
	return filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error {
		if err != nil || fi.Mode()&os.ModeSymlink == 0 {
			return err
		}
		if t, serr := os.Stat(p); serr != nil || !t.Mode().IsRegular() {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if err := os.Remove(p); err != nil {
			return err
		}
		return os.WriteFile(p, b, 0o644)
	})
}

// endCOW is what the cow bugs do with an ending box's cow views: drop the
// shared copy (volume-cow-shared-upper), keep the box's copy as the next cow
// box's start (volume-cow-reused) or the next ro box's
// (volume-ro-sees-last-cow), or keep it in the /tmp every box shares
// (volume-cow-left-in-tmp: the box's TMPDIR; volume-cow-left-in-host-tmp:
// config host_tmp, a host directory outside every box's HOME and TMPDIR).
func endCOW(r record, bug string) {
	for _, v := range r.Volumes {
		switch {
		case v.Mode != protocol.VolumeCOW:
		case bug == "volume-cow-shared-upper":
			_ = os.RemoveAll(v.Dir + "-cow")
		case bug == "volume-cow-reused" || bug == "volume-ro-sees-last-cow":
			_ = os.RemoveAll(v.Dir + "-last")
			_ = exec.Command("cp", "-R", filepath.Join(r.Dir, v.Path), v.Dir+"-last").Run()
		case bug == "volume-cow-left-in-tmp":
			_ = exec.Command("cp", "-R", filepath.Join(r.Dir, v.Path), filepath.Join(sharedTmp(r), filepath.Base(r.Dir))).Run()
		case bug == "volume-cow-left-in-host-tmp" && r.HostTmp != "":
			_ = exec.Command("cp", "-R", filepath.Join(r.Dir, v.Path), filepath.Join(r.HostTmp, filepath.Base(r.Dir))).Run()
		}
	}
}

// boxTmp is a box's TMPDIR, made if it is not there: a VM's /tmp is its
// own, so here it is under the box's directory. With the bug
// volume-cow-left-in-tmp it is one directory every box shares, as in a VM
// handed from box to box with its /tmp kept.
func (d *drv) boxTmp(r record) string {
	tmp := filepath.Join(r.Dir, "tmp")
	if d.is("volume-cow-left-in-tmp") {
		tmp = sharedTmp(r)
	}
	_ = os.MkdirAll(tmp, 0o700)
	return tmp
}

// sharedTmp is the TMPDIR every box shares under volume-cow-left-in-tmp.
func sharedTmp(r record) string { return filepath.Join(filepath.Dir(r.Dir), "tmp-shared") }

// buildRetryS is the retry_s a refused second build answers.
const buildRetryS = 5

// volDir is owner's volume name's directory.
func volDir(state, owner, name string) string {
	h := sha256.Sum256([]byte(owner + "\x00" + name))
	return filepath.Join(state, "vol-"+hex.EncodeToString(h[:8]))
}

// sealedRec is the sealed record: a file beside the volume's directory, so
// nothing a box writes in the volume can make it.
type sealedRec struct{ Owner, Name string }

func sealedFile(dir string) string { return dir + ".sealed" }

// sealed says whether dir is a sealed volume: its record and the volume
// itself are both there (a record without its volume seals nothing).
func sealed(dir string) bool {
	_, err := os.Stat(sealedFile(dir))
	fi, derr := os.Stat(dir)
	return err == nil && derr == nil && fi.IsDir()
}

// sealedByName is the bug seal-cross-owner: the first sealed volume of that
// name, whoever's it is (else dir).
func sealedByName(state, name, dir string) string {
	files, _ := filepath.Glob(filepath.Join(state, "vol-*.sealed"))
	for _, f := range files {
		var s sealedRec
		if b, err := os.ReadFile(f); err == nil && json.Unmarshal(b, &s) == nil && s.Name == name {
			return f[:len(f)-len(".sealed")]
		}
	}
	return dir
}

// endBuilds ends an ended box's build volumes, after its directory and
// processes are gone: sealed when its release said seal, else deleted
// (with seal-partial-kept, sealed on any end).
func endBuilds(r record, bug string) {
	for _, v := range r.Volumes {
		if v.Mode != protocol.VolumeBuild && !(bug == "seal-every-volume" && r.Seal && hasBuild(r)) {
			continue
		}
		if r.Seal && len(r.KeepPaths) > 0 && bug != "seal-keep-ignored" {
			if err := keepOnly(v.Dir, r.KeepPaths, bug == "seal-keep-follows-links"); err != nil {
				dropVolume(v.Dir) // never sealed
				continue
			}
		}
		if r.Seal && bug != "seal-keeps-setuid" {
			if err := normalise(v.Dir); err != nil {
				dropVolume(v.Dir) // never sealed
				continue
			}
		}
		if r.Seal || bug == "seal-partial-kept" {
			writeSealed(v)
			continue
		}
		dropVolume(v.Dir)
	}
}

// writeSealed writes v's sealed record whole: a temporary file renamed
// into place, so a crash never leaves a part record that reads as sealed.
func writeSealed(v rvol) {
	b, _ := json.Marshal(sealedRec{Owner: v.Owner, Name: v.Name})
	tmp := sealedFile(v.Dir) + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		_ = os.Rename(tmp, sealedFile(v.Dir))
	}
}

// dropVolume deletes a volume whole: its sealed record first, so nothing
// ever finds a record whose volume is going.
func dropVolume(dir string) {
	_ = os.Remove(sealedFile(dir))
	unlockTree(dir)
	_ = os.RemoveAll(dir)
}

// normalise clears every setuid and setgid bit under root and deletes every
// device node, FIFO and socket, never following a link (with the
// seal-keeps-setuid bug, a seal skips it).
func normalise(root string) error {
	return filepath.WalkDir(root, func(p string, e os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		t := e.Type()
		switch {
		case t&os.ModeSymlink != 0:
			return nil
		case t&(os.ModeDevice|os.ModeCharDevice|os.ModeNamedPipe|os.ModeSocket) != 0:
			return os.Remove(p)
		}
		fi, err := os.Lstat(p)
		if err != nil {
			return err
		}
		if m := fi.Mode(); m&(os.ModeSetuid|os.ModeSetgid) != 0 {
			return os.Chmod(p, m&^(os.ModeSetuid|os.ModeSetgid))
		}
		return nil
	})
}

// keepOnly deletes from the volume directory root every entry but keep's
// paths and the directories on the way to them (protocol.KeepDirs); a kept
// path or one on the way that is not a real directory goes, and no link is
// followed (with follow, the seal-keep-follows-links bug, a link to a
// directory stands for one).
func keepOnly(root string, keep []string, follow bool) error {
	kept, on := map[string]bool{}, map[string]bool{}
	for _, k := range keep {
		kept[k] = true
	}
	for _, k := range protocol.KeepDirs(keep) {
		on[k] = true
	}
	var prune func(rel string) error
	prune = func(rel string) error {
		es, err := os.ReadDir(filepath.Join(root, rel))
		if err != nil {
			return err
		}
		for _, e := range es {
			r := path.Join(rel, e.Name())
			p := filepath.Join(root, r)
			dir := e.IsDir() // ReadDir does not follow a link
			if follow && e.Type()&os.ModeSymlink != 0 {
				if fi, err := os.Stat(p); err == nil && fi.IsDir() {
					dir = true
				}
			}
			switch {
			case kept[r] && dir:
			case on[r] && dir:
				if err := prune(r); err != nil {
					return err
				}
			default:
				if err := os.RemoveAll(p); err != nil {
					return err
				}
			}
		}
		return nil
	}
	return prune("")
}
