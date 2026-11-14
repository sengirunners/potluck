package vmhost

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/sengirunners/potluck/protocol"
)

// Named volumes (zm-eakh.12.32.1): a box may be
// acquired with volumes, each a directory of its holder's that outlives the
// box and is mounted at a path in the next box that names it. A volume is
// its owner's: acquire's volume_owner (the daemon sends the holder's identity,
// never the renter's word), else its actor. Two owners naming the same volume
// get two volumes; there is no sharing between owners. A volume lives on this
// host and binding (its state directory and target): placement pins to it.
//
// Concurrency is a readers-writer lock per volume, decided at acquire under
// the state lock and held while the box is live:
//   - rw: the box writes the volume itself, and is its only box: an acquire
//     naming a volume another live box holds, in any mode, is unavailable.
//   - ro: read-only, shared by any number of the owner's boxes; unavailable
//     while a box holds it rw.
//   - cow: the box sees the volume as it was and writes a copy of its own
//     (an overlay on OCI), dropped with the box; shared like ro, so a writer
//     never changes what a copy is layered on.
//
// So a cache is filled once by one rw box and then read, or copied on write,
// by every other box at once (the farm's node_modules per lockfile hash).
//
// Bytes: each volume's size is measured on the host (never the guest's word)
// when a box that wrote it (rw) ends, and an owner's volumes on a binding are
// capped at volume_max_gb: a new volume past it first deletes the owner's
// least recently used volumes no box holds, else it is refused. Writable
// mounts (rw, cow) keep the host's free-disk floor (persist_min_free_gb), as
// kept disks do. A volume no box has named for volume_keep_s (default 30
// days, as kept disks) is deleted by the sweep. A box with volumes is never
// persistent and never a GitHub runner's.

const (
	volumeMaxGBDefault = 20
	volumeKeepDefault  = 30 * 24 * 3600
	volumeCheckDefault = 60 // s: a supervisor measures a box's writable volumes this often
	volumesMax         = 8  // per box
)

var (
	volNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)
)

// Volume modes (protocol/volume.go).
const (
	volRW  = protocol.VolumeRW
	volRO  = protocol.VolumeRO
	volCOW = protocol.VolumeCOW
)

// volReq is one of acquire's volumes.
type volReq struct {
	Name string `json:"name"`
	Path string `json:"path"` // relative to the box user's HOME (protocol.VolumePathOK)
	Mode string `json:"mode"` // rw (default), ro, cow or build (volume_seal.go)
	// Sealed (ro or cow only) needs the volume to be sealed: not_found
	// unless it is (volume_seal.go).
	Sealed bool `json:"sealed,omitempty"`
	// Checkout (cow only, at most one a box) makes this mount the box's
	// checkout directory (srcDir, supervise.go).
	Checkout bool `json:"checkout,omitempty"`
}

// volUse is a box's mount of a volume (on its record).
type volUse struct {
	ID, Name, Path, Mode string
	Layer                string // the layer's own name for the volume
	New                  bool   // made by this acquire
	Sealed               bool   // asked sealed: the acquire found it sealed (volume_seal.go)
	Checkout             bool   // the box's checkout directory (srcDir, supervise.go)
}

// volMeta is a volume's file in the state directory.
type volMeta struct {
	ID, Target, Owner, Name string
	Project                 string // the owner's project, as the daemon (or the host relaying for it) keys it
	MeasureFailed           bool   // its last measurement failed: it counts as the whole cap until one succeeds (an acquire naming it measures it again: remeasure)
	Layer                   string
	Created, Used           time.Time
	Bytes                   int64 // -1: not measured yet
	BytesAt                 time.Time
	// Build is the record id of the box building it (mode build), until
	// that box's release seals it or the volume is deleted; Sealed is set
	// only then, by the driver (volume_seal.go). Nothing in the volume
	// sets either.
	Build  string `json:",omitempty"`
	Sealed bool   `json:",omitempty"`
}

// mount is what a layer mounts: its volume, the path in the guest, the mode,
// and whether it was asked sealed (the layer then mounts only a volume that
// is there, never a new empty one).
type mount struct {
	Vol, Path, Mode string
	Sealed          bool
}

// volumer is a layer that mounts named volumes (OCI today; Tart and Incus not
// yet: no volumes capability there).
type volumer interface {
	// CloneVolumes is Clone with mounts (each layer volume made if missing;
	// a cow mount's copy made here, deleted by Destroy).
	CloneVolumes(image, vm string, cpu, memory int, ms []mount) error
	// OwnMounts gives the guest's user each writable mount's directory (and
	// the directories made for it under HOME), once the guest answers.
	OwnMounts(image, vm string, ms []mount) error
	// DeleteVolume deletes a layer volume (no box holds it).
	DeleteVolume(vol string) error
	// VolumeInUse says whether any container but the driver's own read-only
	// measuring helpers (or anything of the layer's, as a cow copy layered
	// on it) references a layer volume; a guess is in use. A build volume is
	// sealed only when it is not (volume_seal.go).
	VolumeInUse(vol string) bool
	// VolumeBytes measures layer volumes on the host, these and no others
	// (image: one the layer may run a measuring helper from): name to
	// bytes, a name missing when it cannot say. Never called under the
	// state lock.
	VolumeBytes(image string, vols []string) map[string]int64
	// CleanSeal cleans a layer volume no box holds for its seal
	// (volume_keep.go): deletes every entry but keep's paths and the
	// directories on the way to them (none: keeps all), then clears every
	// setuid and setgid bit and deletes every device node, FIFO and socket;
	// an error means it is not clean (image: one the layer may run a helper
	// from). It answers the cleaned volume's size in bytes, and an error
	// when it cannot say. Never called under the state lock.
	CleanSeal(image, vol string, keep []string) (int64, error)
	// COWVolumes are the layer volumes a box's cow copies write (OCI: each
	// overlay's upper and work), for VolumeBytes.
	COWVolumes(vm string, ms []mount) []string
	// SweepLeaks deletes the cow copies and other volumes of this state
	// directory's boxes that outlived them (a Destroy that could not delete
	// them, a container removed some other way), except those of the VMs
	// keep names, and never one a container still mounts.
	SweepLeaks(keep func(vm string) bool)
	// LayerVolumes are every named volume the layer lists (the sweep
	// deletes this state directory's layer volumes that no volume file
	// names: dropOrphans).
	LayerVolumes() ([]string, error)
}

// volumes says whether this binding mounts named volumes (describe's
// capability volumes).
func (d *drv) volumes() bool {
	if d.cfg.VM == "oci" {
		if _, err := d.requestedHostBudget(); err != nil || d.nativeRuntime || d.cfg.OCIHostProfile != "" {
			return false
		}
	}
	_, ok := d.vm.(volumer)
	return ok && d.cfg.Kind == "box" && d.cfg.volumeCap() > 0
}

// checkVolumes is acquire's check of its volumes, before any lock.
func checkVolumes(vs []volReq) error {
	if len(vs) > volumesMax {
		return refuse("bad_request", "at most %d volumes", volumesMax)
	}
	paths := map[string]bool{}
	names := map[string]bool{}
	checkout := ""
	for i := range vs {
		v := &vs[i]
		if v.Mode == "" {
			v.Mode = volRW
		}
		clean := path.Clean(v.Path)
		switch {
		case !volNameRe.MatchString(v.Name):
			return refuse("bad_request", "volume name %q: lowercase letters, digits, . _ -, at most 63", v.Name)
		case v.Mode != volRW && v.Mode != volRO && v.Mode != volCOW && v.Mode != volBuild:
			return refuse("bad_request", "volume %s: mode is rw, ro, cow or build", v.Name)
		case v.Sealed && v.Mode != volRO && v.Mode != volCOW:
			return refuse("bad_request", "volume %s: sealed is for ro or cow", v.Name)
		case v.Checkout && v.Mode != volCOW:
			return refuse("bad_request", "volume %s: checkout is for cow", v.Name)
		case v.Checkout && checkout != "":
			return refuse("bad_request", "volumes %s and %s: checkout is for one volume", checkout, v.Name)
		case !volRelPath(v.Path):
			return refuse("bad_request", "volume %s: path %q must be a plain path relative to HOME (never absolute; no ., .., , or =)", v.Name, v.Path)
		case names[v.Name]:
			return refuse("bad_request", "volume %s named twice", v.Name)
		}
		v.Path = clean
		for p := range paths {
			if protocol.VolumePathsOverlap(p, clean) {
				return refuse("bad_request", "volume paths %s and %s overlap", p, clean)
			}
		}
		paths[clean], names[v.Name] = true, true
		if v.Checkout {
			checkout = v.Name
		}
	}
	return nil
}

// volRelPath is the one rule for a volume's path, here and in conformance's
// reference driver (protocol.VolumePathOK): relative to HOME, plain segments.
func volRelPath(p string) bool { return protocol.VolumePathOK(p) }

// volSystemPaths are the guest directories a mount must never cover, be
// inside, or be an ancestor of (guestDir: the driver's working directory,
// which holds the checkout bundle and srcDir). A layer checks a path once
// HOME is known (HOME / makes "usr" /usr).
var volSystemPaths = []string{"/proc", "/sys", "/dev", "/etc", "/bin", "/sbin", "/lib", "/lib64", "/usr", "/boot", "/run", guestDir}

// volSystemPath says whether absolute guest path p is, is inside, or holds a
// system directory, or is / itself.
func volSystemPath(p string) bool {
	p = path.Clean(p)
	if p == "/" {
		return true
	}
	for _, s := range volSystemPaths {
		if p == s || strings.HasPrefix(p, s+"/") || strings.HasPrefix(s, p+"/") {
			return true
		}
	}
	return false
}

func volID(target, owner, name string) string {
	h := sha256.Sum256([]byte(target + "\x00" + owner + "\x00" + name))
	return "v" + hex.EncodeToString(h[:8])
}

func (s store) volFile(id string) string { return filepath.Join(s.dir, "volumes", id+".json") }

func (s store) loadVol(id string) (volMeta, bool) {
	var m volMeta
	b, err := os.ReadFile(s.volFile(id))
	if err != nil || json.Unmarshal(b, &m) != nil {
		return m, false
	}
	return m, true
}

func (s store) saveVol(m volMeta) error {
	if err := os.MkdirAll(filepath.Join(s.dir, "volumes"), 0o700); err != nil {
		return err
	}
	b, _ := json.Marshal(m)
	p := s.volFile(m.ID)
	if err := os.WriteFile(p+".tmp", b, 0o600); err != nil {
		return err
	}
	return os.Rename(p+".tmp", p)
}

func (s store) allVols() []volMeta {
	out, _ := s.readVols()
	return out
}

// readVols reads every volume file it can, and the first error: a reader
// that deletes what no file names (dropOrphans) acts only on a whole read.
func (s store) readVols() ([]volMeta, error) {
	es, err := os.ReadDir(filepath.Join(s.dir, "volumes"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	var out []volMeta
	for _, e := range es {
		n := e.Name()
		if !strings.HasPrefix(n, "v") || !strings.HasSuffix(n, ".json") {
			continue
		}
		var m volMeta
		b, rerr := os.ReadFile(filepath.Join(s.dir, "volumes", n))
		if rerr == nil {
			rerr = json.Unmarshal(b, &m)
		}
		if rerr != nil {
			err = cmp.Or(err, rerr)
			continue
		}
		out = append(out, m)
	}
	return out, err
}

// volHolders are the modes live boxes hold each volume in. Lock held.
func (d *drv) volHolders() map[string][]string {
	h := map[string][]string{}
	for _, r := range d.st.all() {
		if !r.live() {
			continue
		}
		for _, v := range r.Volumes {
			h[v.ID] = append(h[v.ID], v.Mode)
		}
	}
	return h
}

// takeVolumes decides acquire's volumes for owner, for box rid: the sealed
// rules and the lock rules (volRefusal), the byte cap and the floor. Only
// once every volume passed does it write each volume's file (made or used
// now; a build's names rid) and answer the record's mounts. Lock held.
func (d *drv) takeVolumes(rid, owner, project string, vs []volReq) ([]volUse, error) {
	held := d.volHolders()
	var uses []volUse
	var fresh []volReq
	writes := false
	for _, v := range vs {
		id := volID(d.cfg.Target, owner, v.Name)
		m, ok := d.st.loadVol(id)
		if err := volRefusal(v, m, ok, held[id]); err != nil { // volume_seal.go
			return nil, err
		}
		if !ok {
			fresh = append(fresh, v)
		}
		writes = writes || v.Mode != volRO
		// A volume keeps the layer name it was made under (an older one's
		// zmrt-<hash>-<id>); a new one gets the new prefix.
		layer := d.st.vmPrefixNew() + id
		if ok && m.Layer != "" {
			layer = m.Layer
		}
		uses = append(uses, volUse{ID: id, Name: v.Name, Path: v.Path, Mode: v.Mode, Layer: layer, New: !ok, Sealed: v.Sealed, Checkout: v.Checkout})
	}
	for _, u := range uses { // eviction never takes what this acquire mounts
		held[u.ID] = append(held[u.ID], u.Mode)
	}
	if writes || len(fresh) > 0 {
		if err := d.diskFloor("a volume"); err != nil {
			return nil, err
		}
		if err := d.volumeRoom(owner, project, held, len(fresh)); err != nil { // every writing acquire, not a new volume's only
			return nil, err
		}
	}
	now := time.Now().UTC()
	for _, u := range uses {
		m, ok := d.st.loadVol(u.ID)
		if !ok {
			m = volMeta{ID: u.ID, Target: d.cfg.Target, Owner: owner, Project: project, Name: u.Name, Layer: u.Layer, Created: now, Bytes: -1}
			if u.Mode == volBuild {
				m.Build = rid
			}
		}
		m.Used = now
		if err := d.st.saveVol(m); err != nil {
			return nil, err
		}
	}
	return uses, nil
}

// remeasure measures again, outside the lock, the volumes of owner's an
// acquire names whose last measurement failed and that no live box writes
// (a sealed one is the sweep's: sweepVolumes)
// (a failed one counts as the whole cap: volCount), and records what it
// reads, so one failed measurement never holds the owner at the cap for
// good. One it still cannot read stays failed: fail closed.
func (d *drv) remeasure(owner string, vs []volReq) {
	v, ok := d.vm.(volumer)
	if !ok {
		return
	}
	ids := map[string]string{}     // layer volume to volume id
	used := map[string]time.Time{} // and its last use then: one used since keeps its own measurement
	_ = d.st.locked(func() error {
		held := d.volHolders()
		for _, q := range vs {
			id := volID(d.cfg.Target, owner, q.Name)
			if m, found := d.st.loadVol(id); found && m.MeasureFailed && !m.Sealed && m.Build == "" && !writtenLive(held[id]) {
				ids[m.Layer], used[m.Layer] = id, m.Used
			}
		}
		return nil
	})
	if len(ids) == 0 {
		return
	}
	layers := make([]string, 0, len(ids))
	for l := range ids {
		layers = append(layers, l)
	}
	sizes := v.VolumeBytes(d.cfg.Image, layers)
	_ = d.st.locked(func() error {
		held := d.volHolders()
		now := time.Now().UTC()
		for layer, id := range ids {
			b, read := sizes[layer]
			m, found := d.st.loadVol(id)
			if !read || b < 0 || !found || !m.MeasureFailed || m.Sealed || m.Layer != layer || !m.Used.Equal(used[layer]) || writtenLive(held[id]) {
				continue
			}
			m.Bytes, m.BytesAt, m.MeasureFailed = b, now, false
			_ = d.st.saveVol(m)
		}
		return nil
	})
}

// writtenLive says whether any of a volume's live holders writes it itself.
func writtenLive(modes []string) bool {
	return slices.ContainsFunc(modes, writesItself)
}

// volumeRoom keeps an acquire that writes (rw, cow, or new volumes) under
// the caps: the owner's (volume_max_gb, or volume_max_mb), its project's
// (volume_project_max_gb, default 4x the owner's) and the binding's
// (volume_binding_max_gb, default 8x). Each counts volumes (unmeasured ones
// as volUnmeasured), live boxes' cow copies and the fresh new ones. Past a
// cap, only the owner's own least recently used volumes no box holds (nor
// this acquire) are deleted, never another owner's, and never a build
// volume (its builder's end settles it: volume_seal.go); else "volume
// quota". Lock held.
func (d *drv) volumeRoom(owner, project string, held map[string][]string, fresh int) error {
	var mine []volMeta
	for _, m := range d.st.allVols() {
		if m.Target == d.cfg.Target && m.Owner == owner {
			mine = append(mine, m)
		}
	}
	sort.Slice(mine, func(i, j int) bool { return mine[i].Used.Before(mine[j].Used) })
	extra := int64(fresh) * volUnmeasured(d.cfg.volumeCap())
	for _, m := range mine {
		if d.volumeOver(owner, project, extra) == "" {
			return nil
		}
		if len(held[m.ID]) == 0 && m.Build == "" {
			d.dropVolume(m)
		}
	}
	if over := d.volumeOver(owner, project, extra); over != "" {
		return refuse("unavailable", "volume quota: %s: release a box, or name a volume you have", over)
	}
	return nil
}

// volumeOver says which cap owner's (and project's, and the binding's)
// volumes and copies pass with extra more bytes: "" when none. Lock held.
func (d *drv) volumeOver(owner, project string, extra int64) string {
	limit := d.cfg.volumeCap()
	var mine, proj, all int64 = extra, extra, extra
	for _, m := range d.st.allVols() {
		if m.Target != d.cfg.Target {
			continue
		}
		n := volCount(m, limit)
		all += n
		if m.Project == project {
			proj += n
		}
		if m.Owner == owner {
			mine += n
		}
	}
	for _, r := range d.st.all() {
		if !r.live() || r.Target != d.cfg.Target || r.COWBytes == 0 {
			continue
		}
		all += r.COWBytes
		if r.VolProject == project {
			proj += r.COWBytes
		}
		if r.VolOwner == owner {
			mine += r.COWBytes
		}
	}
	gib := func(n int64) string {
		if n < 1<<30 {
			return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
		}
		return fmt.Sprintf("%.2f GiB", float64(n)/(1<<30))
	}
	switch {
	case mine >= limit:
		return fmt.Sprintf("your volumes and copies on this host hold %s, at its cap of %s", gib(mine), gib(limit))
	case proj >= d.cfg.volumeProjectCap():
		return fmt.Sprintf("this project's volumes and copies on this host hold %s, at its cap of %s", gib(proj), gib(d.cfg.volumeProjectCap()))
	case all >= d.cfg.volumeBindingCap():
		return fmt.Sprintf("this binding's volumes and copies hold %s, at its cap of %s", gib(all), gib(d.cfg.volumeBindingCap()))
	}
	return ""
}

// volumeProjectCap and volumeBindingCap are the project's and the binding's
// caps in bytes (volume_project_max_gb, volume_binding_max_gb; unset, 4x and
// 8x the owner's).
func (c config) volumeProjectCap() int64 {
	if c.VolumeProjectMaxGB > 0 {
		return int64(c.VolumeProjectMaxGB) << 30
	}
	return 4 * c.volumeCap()
}

func (c config) volumeBindingCap() int64 {
	if c.VolumeBindingMaxGB > 0 {
		return int64(c.VolumeBindingMaxGB) << 30
	}
	return 8 * c.volumeCap()
}

// volumeCap is an owner's cap in bytes: volume_max_mb when set, else volume_max_gb.
func (c config) volumeCap() int64 {
	if c.VolumeMaxMB > 0 {
		return int64(c.VolumeMaxMB) << 20
	}
	return int64(c.VolumeMaxGB) << 30
}

// volUnmeasured is what a volume not measured yet counts: never 0, a quarter
// of the cap at most (1 GiB under the default cap).
func volUnmeasured(limit int64) int64 { return min(int64(1)<<30, max(limit/4, 1)) }

func volCount(m volMeta, limit int64) int64 {
	if m.MeasureFailed {
		return limit
	}
	if m.Bytes < 0 {
		return volUnmeasured(limit)
	}
	return m.Bytes
}

// cowBytes is what owner's live boxes' cow copies hold, as their supervisors
// last measured. Lock held.
func (d *drv) cowBytes(owner string) int64 {
	var n int64
	for _, r := range d.st.all() {
		if r.live() && r.VolOwner == owner && r.Target == d.cfg.Target {
			n += r.COWBytes
		}
	}
	return n
}

// dropVolume deletes a volume no box holds: the layer's, then its file. A
// sealed volume goes only whole: its sealed record is cleared first, while
// nothing references it, so a delete that stops half way leaves a plain
// volume, never a sealed one over a missing or partial volume. Lock held.
func (d *drv) dropVolume(m volMeta) bool {
	if v, ok := d.vm.(volumer); ok {
		if m.Sealed {
			if v.VolumeInUse(m.Layer) {
				return false
			}
			m.Sealed = false
			if d.st.saveVol(m) != nil {
				return false
			}
		}
		if err := v.DeleteVolume(m.Layer); err != nil {
			return false
		}
	}
	return os.Remove(d.st.volFile(m.ID)) == nil
}

// sweepVolumes deletes this binding's build volumes whose builder is gone
// (volume_seal.go) and the volumes no box has named for volume_keep_s.
// Lock held.
func (d *drv) sweepVolumes() {
	if _, ok := d.vm.(volumer); !ok {
		return
	}
	d.sweepBuilds()
	keep := time.Duration(d.cfg.VolumeKeepS) * time.Second
	var held map[string][]string
	for _, m := range d.st.allVols() {
		// A sealed volume with no measured size (sealed before seals
		// measured, or its measurement failed then) is never written
		// again, so nothing would ever measure it: it goes once no box
		// holds it, as a layer the next build makes again.
		unsized := m.Sealed && (m.MeasureFailed || m.Bytes < 0)
		if m.Target != d.cfg.Target || m.Build != "" || time.Since(m.Used) <= keep && !unsized {
			continue
		}
		if held == nil {
			held = d.volHolders()
		}
		if len(held[m.ID]) == 0 {
			d.dropVolume(m)
		}
	}
	d.dropOrphans()
}

// dropOrphans deletes this state directory's layer volumes (<prefix>v<hex>,
// volID) that no volume file names: a layer volume is made only after its
// file is written (takeVolumes, under the lock), so one with none is left
// from a delete, or made again by a helper that mounted it by name after
// its delete (the CLIs make a missing named volume on run --mount). Never
// one a container mounts (DeleteVolume refuses it). Lock held.
func (d *drv) dropOrphans() {
	v, ok := d.vm.(volumer)
	if !ok {
		return
	}
	metas, err := d.st.readVols()
	if err != nil {
		return // a file it cannot read may name a volume: nothing is deleted on a guess
	}
	names, err := v.LayerVolumes()
	if err != nil {
		return
	}
	known := map[string]bool{}
	for _, m := range metas {
		known[m.Layer] = true
	}
	for _, n := range names {
		if !known[n] && d.st.ownLayerVolume(n) {
			_ = v.DeleteVolume(n)
		}
	}
}

// ownLayerVolume says whether a layer volume name is one of this state
// directory's named volumes: either VM prefix, then a volID.
func (s store) ownLayerVolume(name string) bool {
	for _, p := range []string{s.vmPrefix(), s.vmPrefixNew()} {
		if id, ok := strings.CutPrefix(name, p); ok && volIDRe.MatchString(id) {
			return true
		}
	}
	return false
}

// volIDRe is volID's form.
var volIDRe = regexp.MustCompile(`^v[0-9a-f]{16}$`)

// volAnswer is acquire's account of a box's volumes.
func volAnswer(r record) []map[string]any {
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

// mounts are a record's volumes as its layer mounts them.
func (r record) mounts() []mount {
	var ms []mount
	for _, v := range r.Volumes {
		ms = append(ms, mount{Vol: v.Layer, Path: v.Path, Mode: v.Mode, Sealed: v.Sealed})
	}
	return ms
}

// measureVolumes records the size of the volumes a box wrote (rw, build), on the
// host, once the box is gone (its supervisor's last step): measured outside
// the state lock, recorded under it.
func (s *sup) measureVolumes(r record) {
	if v, ok := s.vm.(volumer); ok {
		_ = s.st.locked(func() error { r = endMeasured(s.st, r); return nil })
		if sizes, _, ok := measure(v, r, false); ok {
			_ = s.st.locked(func() error { recordRW(s.st, r, sizes); return nil })
		} else {
			_ = s.st.locked(func() error { markUnmeasured(s.st, r, s.cfg.volumeCap()); return nil })
		}
	}
}

// endMeasured is an ended box r with only the volumes its end measures: rw
// volumes that still have their file. A build volume is its seal's to
// measure (endBuilds) or is deleted, and a volume deleted since is never
// measured: the helper would mount it by name, and the CLIs make a missing
// named volume on run --mount, so it would come back with no file to say
// whose it is. Lock held.
func endMeasured(st store, r record) record {
	var keep []volUse
	for _, u := range r.Volumes {
		if _, found := st.loadVol(u.ID); found && u.Mode == volRW {
			keep = append(keep, u)
		}
	}
	r.Volumes = keep
	return r
}

// measure measures a record's rw and build volumes (and withCOW its cow copies' upper
// and work volumes) on the host, never under the state lock: rw sizes by
// layer volume, the copies' total, and ok false when any could not be read.
func measure(v volumer, r record, withCOW bool) (map[string]int64, int64, bool) {
	var names []string
	for _, u := range r.Volumes {
		if writesItself(u.Mode) {
			names = append(names, u.Layer)
		}
	}
	var uppers []string
	if withCOW {
		uppers = v.COWVolumes(r.VM, r.mounts())
	}
	if len(names)+len(uppers) == 0 {
		return nil, 0, true
	}
	sizes := v.VolumeBytes(r.Image, append(append([]string{}, names...), uppers...))
	ok := true
	var cow int64
	for _, n := range uppers {
		b, found := sizes[n]
		ok = ok && found && b >= 0
		cow += max(b, 0)
	}
	rw := map[string]int64{}
	for _, n := range names {
		b, found := sizes[n]
		ok = ok && found && b >= 0
		if found && b >= 0 {
			rw[n] = b
		}
	}
	return rw, cow, ok
}

// recordRW records measured rw and build sizes on their volumes' files. Lock held.
func recordRW(st store, r record, sizes map[string]int64) {
	now := time.Now().UTC()
	for _, u := range r.Volumes {
		b, ok := sizes[u.Layer]
		m, found := st.loadVol(u.ID)
		if !writesItself(u.Mode) || !ok || !found || !measuredBy(m, u, r) {
			continue
		}
		m.Bytes, m.BytesAt, m.Used, m.MeasureFailed = b, now, now, false
		_ = st.saveVol(m)
	}
}

// measuredBy says whether box r's measurement of its volume u may be
// recorded on m: never on a sealed volume (its seal measured it, after its
// cleanup) or on a build volume that is no longer r's (deleted and made
// again since the measurement began). Lock held.
func measuredBy(m volMeta, u volUse, r record) bool {
	return !m.Sealed && (u.Mode != volBuild || m.Build == r.ID)
}

// markUnmeasured records that a box's measurement failed: its rw volumes and
// its cow copies count as the whole cap until one succeeds. Lock held.
func markUnmeasured(st store, r record, limit int64) {
	for _, u := range r.Volumes {
		if m, found := st.loadVol(u.ID); found && writesItself(u.Mode) && measuredBy(m, u, r) {
			m.MeasureFailed = true
			_ = st.saveVol(m)
		}
	}
	for _, u := range r.Volumes {
		if u.Mode == volCOW {
			if cur, err := st.load(r.ID); err == nil {
				cur.COWBytes = limit
				_ = st.save(cur)
			}
			break
		}
	}
}

// measureLeft measures, detached from the driver (the sweep starts it, so
// the state lock is never held through a measurement), the rw volumes of a
// box whose supervisor died, and records them.
func measureLeft(dir, id string) int {
	st := store{dir}
	r, err := st.load(id)
	if err != nil {
		return 1
	}
	c, err := parseConfig(r.Config)
	if err != nil {
		return 1
	}
	v, ok := r.bindLayer(newLayer(c)).(volumer)
	if !ok {
		return 0
	}
	_ = st.locked(func() error { r = endMeasured(st, r); return nil })
	sizes, _, ok := measure(v, r, false)
	if !ok {
		_ = st.locked(func() error { markUnmeasured(st, r, c.volumeCap()); return nil })
		return 1
	}
	_ = st.locked(func() error { recordRW(st, r, sizes); return nil })
	return 0
}

// volCheckEvery is how often a supervisor measures a box's writable volumes
// (volume_check_s).
func (c config) volCheckEvery() time.Duration { return time.Duration(c.VolumeCheckS) * time.Second }

// volMeasureFails is how many measurements in a row may fail before the box
// ends: an unmeasurable box is never left writing unbounded.
const volMeasureFails = 3

// quotaCheck measures a live box's rw volumes and cow copies (outside the
// state lock; under it only to record and sum), and answers why the box must
// end: its owner, its project or the binding past a cap, the host past its
// free-disk floor (persist_min_free_gb), or volMeasureFails failed
// measurements in a row. "" while it may go on.
func (s *sup) quotaCheck() string {
	v, ok := s.vm.(volumer)
	r, err := s.st.load(s.id)
	if !ok || err != nil || !r.live() || !r.writesVolumes() {
		return ""
	}
	sizes, cow, measured := measure(v, r, true)
	d := &drv{cfg: s.cfg, st: s.st, vm: s.vm}
	if !measured {
		// Fails closed: what could not be measured counts as the whole cap,
		// and the host's floor holds whatever the measurement did.
		_ = s.st.locked(func() error { markUnmeasured(s.st, r, s.cfg.volumeCap()); return nil })
		if err := d.diskFloor("volumes"); err != nil {
			return "volume quota: " + err.Error()
		}
		if s.measureFails++; s.measureFails >= volMeasureFails {
			return fmt.Sprintf("volume quota: cannot measure the box's volumes (%d tries in a row)", s.measureFails)
		}
		return ""
	}
	s.measureFails = 0
	why := ""
	_ = s.st.locked(func() error {
		recordRW(s.st, r, sizes)
		if cur, err := s.st.load(s.id); err == nil {
			cur.COWBytes = cow
			_ = s.st.save(cur)
		}
		if over := d.volumeOver(r.VolOwner, r.VolProject, 0); over != "" {
			why = "volume quota: " + over
		} else if err := d.diskFloor("volumes"); err != nil {
			why = "volume quota: " + err.Error()
		}
		return nil
	})
	return why
}

// writesVolumes says whether a box writes any volume (rw) or copy (cow).
func (r record) writesVolumes() bool {
	for _, v := range r.Volumes {
		if v.Mode != volRO {
			return true
		}
	}
	return false
}

// volDescribe is describe's account of volumes.
func (d *drv) volDescribe() map[string]any {
	return map[string]any{"max_gb": d.cfg.VolumeMaxGB, "max_bytes": d.cfg.volumeCap(), "check_s": d.cfg.VolumeCheckS, "keep_s": d.cfg.VolumeKeepS, "modes": []string{volRW, volRO, volCOW, volBuild}}
}

// sameVolumes says whether a repeated acquire asks for the volumes the first did.
func sameVolumes(had []volUse, asked []volReq) bool {
	if len(had) != len(asked) {
		return false
	}
	for i, v := range asked {
		mode := cmp.Or(v.Mode, volRW)
		if had[i].Name != v.Name || had[i].Path != path.Clean(v.Path) || had[i].Mode != mode || had[i].Sealed != v.Sealed || had[i].Checkout != v.Checkout {
			return false
		}
	}
	return true
}

// spawnDetached re-executes the driver with flag, detached, for record id
// (its output to the record's supervisor log).
func (d *drv) spawnDetached(flag, id string) {
	self, err := os.Executable()
	if err != nil {
		return
	}
	cmd := exec.Command(self, flag, d.st.dir, id)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if logf, err := os.OpenFile(filepath.Join(d.st.dir, "logs", id+".supervisor.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); err == nil {
		defer logf.Close()
		cmd.Stdout, cmd.Stderr = logf, logf
	}
	if cmd.Start() == nil {
		_ = cmd.Process.Release()
	}
}
