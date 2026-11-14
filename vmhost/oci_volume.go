package vmhost

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Named volumes on the OCI layer (volume.go): each is a named volume of the
// container CLI's (as a kept box's HOME is, oci_persist.go), labelled
// ociVolLabel, mounted with the same isolation as any of its volumes: the
// runtime's own mount (gVisor's gofer, Kata's virtiofs), nothing of the
// host's but that directory, never a host path a renter names.
//   - rw and build mount the volume itself, volume-nocopy: the volume holds
//     only what its boxes wrote, never a copy of the image's files at that
//     path. build makes a new volume: one the CLI has already is refused
//     (volume_seal.go). A mount asked sealed is never made: the volume must
//     be there.
//   - ro mounts a view of its own, made for this box: a volume of the local
//     driver's that binds the named volume's directory ro,nosuid,nodev
//     (mounted by the CLI's daemon, which applies the flags with a remount),
//     labelled as layered on the volume.
//   - cow mounts a volume of its own, made for this box: an overlay (the
//     local driver's type=overlay, nosuid,nodev) whose lower directory is
//     the named volume and whose upper and work directories are two more
//     volumes of the box's. The named volume never changes under a copy,
//     since volume.go keeps rw away while any cow box holds it.
//
// Destroy deletes a box's views and copies. Docker's local driver makes
// them; rootless Podman or nerdctl may refuse the mount, and the box then
// fails to start.
//
// A view gives its box no privilege a fresh box lacks from a setuid or
// setgid file or a device node a build left in the volume. The cleanup at
// seal deletes those from a sealed volume (volume_keep.go), and the views
// are inert to any that are there:
//   - The CLI's mount of each view is nosuid,nodev on the host.
//   - Only runsc and Kata pass verifyEngine, and Kata mounts no outer
//     volumes, so every view is the gVisor sentry's own mount, which a
//     Docker volume mount cannot mark nosuid. The sentry ignores setuid and
//     setgid bits unless its runtime has --allow-suid, which only root's
//     runsc-root has, and its box's user has sudo already; no box can make
//     a device node (root's lack MKNOD, the others any capability).
//   - Any other runtime would run its programs on the host's kernel, where
//     the container's bind of the CLI's mount keeps nosuid,nodev unless the
//     runtime drops them: once the guest answers, checkViews reads the
//     guest's own mount table with the image's shell and fails the box if a
//     view lacks them. That is a backstop for a runtime that drops them, run
//     after the box's entrypoint started, not a guarantee against a hostile
//     image (images are the binding's own pins).
//
// The mount's directory is root's when the CLI makes it, so once the guest
// answers, the image's user is given it (and the directories made for it
// under HOME), when they are root's: OwnMounts.

const ociVolLabel = "dev.potluck.volume=1" // before the rename's step 2: dev.zm.volume=1 (never read)

// ociLowerLabel names, on a cow overlay volume, the named volume it is layered on.
const ociLowerLabel = "dev.zm.lower"

// ociLowerLabelNew is what this driver sets on a new overlay; ociLowerLabel
// is on those made before the rename's step 2. An overlay under either is
// layered on the volume.
const ociLowerLabelNew = "dev.potluck.lower"

// ociCOWFile lists a box's cow volumes, for Destroy.
func (o ociCLI) cowFile(vm string) string { return filepath.Join(o.stateDir, "oci-cow", vm) }

// mountPath is a mount's absolute path in the guest: a relative one is under HOME.
func mountPath(home, p string) string {
	if path.IsAbs(p) {
		return p
	}
	return path.Join(home, p)
}

func (o ociCLI) CloneVolumes(image, vm string, cpu, memory int, ms []mount) error {
	var err error
	o, err = o.outerVolumeEngine()
	if err != nil {
		return err
	}
	home, err := o.imageHome(image)
	if err != nil {
		return err
	}
	var extra, cow []string
	defer func() {
		if err != nil && len(cow) > 0 {
			o.dropCOW(vm) // the file stays if they cannot go: the sweep tries again
		}
	}()
	for i, m := range ms {
		target := mountPath(home, m.Path)
		if volSystemPath(target) { // HOME / makes "usr" /usr (volume.go)
			err = fmt.Errorf("volume path %s is under HOME %s: %s is the box's own system directory", m.Path, home, target)
			return err
		}
		if err = o.namedVolume(m); err != nil {
			return err
		}
		switch m.Mode {
		case volRW, volBuild:
			extra = append(extra, "--mount", "type=volume,source="+m.Vol+",target="+target+",volume-nocopy")
		case volRO:
			rv := fmt.Sprintf("%s-ro%d", vm, i)
			cow = append(cow, rv)
			if err = o.noteCOW(vm, cow); err != nil {
				return err
			}
			var dirs []string
			if dirs, err = o.mountpoints(m.Vol); err != nil {
				return err
			}
			if _, err = o.run("volume", "create", "--label", ociVolLabel, "--label", ociLowerLabelNew+"="+m.Vol, "--driver", "local", "--opt", "type=none", "--opt", "device="+dirs[0], "--opt", "o=bind,ro,nosuid,nodev", rv); err != nil {
				return err
			}
			extra = append(extra, "--mount", "type=volume,source="+rv+",target="+target+",readonly,volume-nocopy")
		case volCOW:
			ov, up, wk := fmt.Sprintf("%s-cow%d", vm, i), fmt.Sprintf("%s-cow%du", vm, i), fmt.Sprintf("%s-cow%dw", vm, i)
			cow = append(cow, ov, up, wk)
			if err = o.noteCOW(vm, cow); err != nil {
				return err
			}
			for _, v := range []string{up, wk} {
				if _, err = o.run("volume", "create", "--label", ociVolLabel, v); err != nil {
					return err
				}
			}
			var dirs []string
			if dirs, err = o.mountpoints(m.Vol, up, wk); err != nil {
				return err
			}
			opt := "o=lowerdir=" + dirs[0] + ",upperdir=" + dirs[1] + ",workdir=" + dirs[2] + ",nosuid,nodev"
			if _, err = o.run("volume", "create", "--label", ociVolLabel, "--label", ociLowerLabelNew+"="+m.Vol, "--driver", "local", "--opt", "type=overlay", "--opt", "device=overlay", "--opt", opt, ov); err != nil {
				return err
			}
			extra = append(extra, "--mount", "type=volume,source="+ov+",target="+target+",volume-nocopy")
		default:
			return fmt.Errorf("volume mode %q", m.Mode)
		}
	}
	err = o.clone(image, vm, cpu, memory, extra)
	return err
}

// namedVolume makes (or finds) a mount's named volume: create answers the
// same when it is there already, so a build first asks the CLI's list (a
// volume of that name the driver has no record for, left by an earlier end
// that could not delete it, is never built on: the box fails, and its end
// deletes the leftover),
// and a sealed mount only checks it is there (never an empty one for it).
func (o ociCLI) namedVolume(m mount) error {
	if m.Sealed {
		if _, err := o.run("volume", "inspect", m.Vol); err != nil {
			return fmt.Errorf("sealed volume %s is not there: %w", m.Vol, err)
		}
		return nil
	}
	if m.Mode == volBuild {
		names, err := o.volumeNames()
		if err != nil {
			return err
		}
		for _, n := range names {
			if n == m.Vol {
				return fmt.Errorf("build volume %s is there already: build makes a new one", m.Vol)
			}
		}
	}
	_, err := o.run("volume", "create", "--label", ociVolLabel, m.Vol)
	return err
}

// noteCOW writes the box's cow volumes before they exist, so Destroy finds
// them, after a first line naming the CLI that makes them: bindings may share
// a state directory, and only that CLI can say whether they are in use.
func (o ociCLI) noteCOW(vm string, vols []string) error {
	p := o.cowFile(vm)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	return os.WriteFile(p, []byte(cowCLIPrefix+o.bin+"\n"+strings.Join(vols, "\n")+"\n"), 0o600)
}

const cowCLIPrefix = "cli="

// readCOW reads a box's oci-cow file: the CLI that made its volumes ("" in
// a file written before the CLI was noted) and the volumes.
func (o ociCLI) readCOW(vm string) (cli string, vols []string, err error) {
	b, err := os.ReadFile(o.cowFile(vm))
	if err != nil {
		return "", nil, err
	}
	for _, line := range strings.Split(string(b), "\n") {
		if c, ok := strings.CutPrefix(line, cowCLIPrefix); ok {
			cli = c
		} else if line = strings.TrimSpace(line); line != "" {
			vols = append(vols, line)
		}
	}
	return cli, vols, nil
}

// dropCOW deletes a box's cow volumes (Destroy, once the container is gone).
func (o ociCLI) dropCOW(vm string) {
	if o.stateDir == "" || filepath.Base(vm) != vm {
		return
	}
	_, vols, err := o.readCOW(vm)
	if err != nil {
		return
	}
	if len(vols) > 0 {
		if o.inUse(vols...) { // a container still names one: never pulled from under it
			fmt.Fprintln(os.Stderr, "potluck-driver-tart: cow volumes of", vm, "are still in use; kept for the next sweep")
			return
		}
		if err := o.rmVolumes(vols...); err != nil {
			fmt.Fprintln(os.Stderr, "potluck-driver-tart:", err)
			return // the file stays: the next sweep tries again (sweepCOW)
		}
	}
	_ = os.Remove(o.cowFile(vm))
}

// sweepCOW deletes the cow volumes listed under oci-cow for each box keep
// does not name: its container is gone (keep names every container the CLI
// lists and every box a live record or a running supervisor holds), so no
// Destroy will come for it. Only files this CLI wrote: another binding's
// volumes are on a daemon this one cannot see; a file that names no CLI is
// this one's when its daemon has the first volume. dropCOW still keeps any a
// container mounts.
func (o ociCLI) sweepCOW(keep func(vm string) bool) {
	if o.stateDir == "" {
		return
	}
	es, err := os.ReadDir(filepath.Join(o.stateDir, "oci-cow"))
	if err != nil {
		return
	}
	for _, e := range es {
		vm := e.Name()
		if !e.Type().IsRegular() || keep(vm) {
			continue
		}
		cli, vols, err := o.readCOW(vm)
		if err != nil || len(vols) == 0 {
			continue
		}
		if cli == "" { // written before the CLI was noted: this CLI's if its daemon has the volumes
			if _, err := o.run("volume", "inspect", vols[0]); err == nil {
				cli = o.bin
			}
		}
		if cli == o.bin {
			o.dropCOW(vm)
		}
	}
}

// mountpoints are the volumes' directories on the host, in order.
func (o ociCLI) mountpoints(vols ...string) ([]string, error) {
	out, err := o.run(append([]string{"volume", "inspect", "--format", "{{.Mountpoint}}"}, vols...)...)
	if err != nil {
		return nil, err
	}
	dirs := strings.Fields(out)
	if len(dirs) != len(vols) {
		return nil, fmt.Errorf("%s volume inspect answered %d mountpoints for %d volumes", o.bin, len(dirs), len(vols))
	}
	for _, d := range dirs {
		if !path.IsAbs(d) || strings.ContainsAny(d, ",=:") {
			return nil, fmt.Errorf("volume mountpoint %q is not a plain absolute path", d)
		}
	}
	return dirs, nil
}

// ociOwnMounts gives user ($1) each writable mount ($3...) and the
// directories made for it under HOME ($2), where they are root's.
const ociOwnMounts = `u=$1; h=$2; shift 2
for p; do q=$p; while [ "$q" != "$h" ] && [ "$q" != / ] && [ "$q" != . ]; do
  case "$q" in "$h"/*|"$p") [ "$(stat -c %u "$q" 2>/dev/null)" = 0 ] && chown "$u" "$q" ;; esac; q=$(dirname "$q"); done; done; true`

// ociCheckViews fails unless each view ($2 its mode, $3 its path, and so on)
// is the top mount at its path in the mount table $1 (the guest's
// /proc/self/mountinfo: the last line for a path is the one on top) with
// nosuid and nodev among its options, and ro too for an ro view. A path the
// table escapes (a space) never matches: the box fails.
const ociCheckViews = `tab=$1; shift
while [ $# -gt 1 ]; do k=$1 t=$2; shift 2; o=
  while read -r _ _ _ _ p f _; do [ "$p" != "$t" ] || o=$f; done < "$tab"
  for w in nosuid nodev; do case ,$o, in *,$w,*) ;; *) echo "$t is mounted ${o:-not at all}, not $w" >&2; exit 1 ;; esac; done
  [ "$k" != ro ] || case ,$o, in *,ro,*) ;; *) echo "$t is mounted ${o:-not at all}, not ro" >&2; exit 1 ;; esac
done`

// checkViews fails unless the guest mounts each ro and cow view nosuid,nodev
// (and an ro one ro), as CloneVolumes made it; not on gVisor, whose mount
// table never shows them and which needs them not (see the top of this file).
func (o ociCLI) checkViews(vm, home string, ms []mount) error {
	if o.gvisor() {
		return nil
	}
	a := []string{"exec", "-u", "0", vm, "/bin/sh", "-c", ociCheckViews, "sh", "/proc/self/mountinfo"}
	n := len(a)
	for _, m := range ms {
		if m.Mode == volRO || m.Mode == volCOW {
			a = append(a, m.Mode, mountPath(home, m.Path))
		}
	}
	if len(a) == n {
		return nil
	}
	if _, err := o.run(a...); err != nil {
		return fmt.Errorf("the box's volume views are not mounted nosuid,nodev, which this engine and runtime must apply: %w", err)
	}
	return nil
}

// OwnMounts checks the box's views (checkViews), then gives the image's
// user each writable mount's directory and those made for it under HOME.
func (o ociCLI) OwnMounts(image, vm string, ms []mount) error {
	home, err := o.imageHome(image)
	if err != nil {
		return err
	}
	if err := o.checkViews(vm, home, ms); err != nil {
		return err
	}
	user, err := o.imageUser(image)
	if err != nil || user == "" || user == "root" || user == "0" {
		return err // the guest is root: it owns them already
	}
	a := []string{"exec", "-u", "0", vm, "/bin/sh", "-c", ociOwnMounts, "sh", user, home}
	for _, m := range ms {
		if m.Mode != volRO { // rw, build and cow
			a = append(a, mountPath(home, m.Path))
		}
	}
	if len(a) == 10 {
		return nil
	}
	_, err = o.run(a...)
	return err
}

// imageUser is the image's user, from its config ("" is root).
func (o ociCLI) imageUser(image string) (string, error) {
	out, err := o.run("image", "inspect", "--format", "{{.Config.User}}", image)
	if err != nil {
		return "", err
	}
	u, _, _ := strings.Cut(strings.TrimSpace(out), ":")
	return u, nil
}

// DeleteVolume deletes a named volume no container mounts and no cow overlay
// is layered on (whatever the state directory's records say).
func (o ociCLI) DeleteVolume(vol string) error {
	if o.inUse(vol) || o.layeredOn(vol) {
		return fmt.Errorf("volume %s is in use by a container or has a cow copy layered on it (or the CLI did not say)", vol)
	}
	return o.rmVolumes(vol)
}

// VolumeInUse says whether any container, running or not, but this driver's
// own measuring helpers (VolumeBytes: read-only mounts, no network, nothing
// of a box's) mounts vol, or a cow overlay is layered on it; a CLI that does
// not answer is in use. A seal waits for it; a measurement, which can take
// minutes on a large volume, never holds a seal back.
func (o ociCLI) VolumeInUse(vol string) bool {
	out, err := o.run("ps", "-a", "--filter", "volume="+vol, "--format", "{{.Names}}")
	if err != nil {
		return true
	}
	for _, n := range strings.Fields(out) {
		if !strings.HasPrefix(n, ociMeasurePrefix) {
			return true
		}
	}
	return o.layeredOn(vol)
}

// layeredOn says whether a cow overlay is layered on vol (a CLI that does not
// answer: yes).
func (o ociCLI) layeredOn(vol string) bool {
	for _, label := range []string{ociLowerLabel, ociLowerLabelNew} {
		if out, err := o.run("volume", "ls", "-q", "--filter", "label="+label+"="+vol); err != nil || strings.TrimSpace(out) != "" {
			return true
		}
	}
	return false
}

// inUse says whether any container, running or not, mounts one of vols (an
// error reading is in use: nothing is deleted on a guess).
func (o ociCLI) inUse(vols ...string) bool {
	for _, v := range vols {
		out, err := o.run("ps", "-a", "-q", "--filter", "volume="+v)
		if err != nil || strings.TrimSpace(out) != "" {
			return true
		}
	}
	return false
}

// COWVolumes are a box's cow copies' upper and work volumes (CloneVolumes' names).
func (o ociCLI) COWVolumes(vm string, ms []mount) []string {
	var out []string
	for i, m := range ms {
		if m.Mode == volCOW {
			out = append(out, fmt.Sprintf("%s-cow%du", vm, i), fmt.Sprintf("%s-cow%dw", vm, i))
		}
	}
	return out
}

// VolumeBytes refuses native/bounded profiles until an admitted helper exists.
// For a verified ordinary runtime it measures exactly vols, never other volumes: one
// throwaway helper container from the box's own image under the binding's
// runtime (a sandbox, never the host's kernel alone) with no network, a
// read-only root, every capability dropped but DAC_READ_SEARCH (to read what
// the guest's user made private), each volume mounted read-only, running
// du -sxb on them. Bounded by ociMeasureFor; a name it does not answer is
// one it cannot measure (the caller counts failures).
func (o ociCLI) VolumeBytes(image string, vols []string) map[string]int64 {
	out := map[string]int64{}
	if len(vols) == 0 {
		return out
	}
	var err error
	o, err = o.outerVolumeEngine()
	if err != nil {
		return out
	}
	a := []string{"--network", "none", "--runtime", o.runtime, "--read-only", "--user", "0",
		"--security-opt", "no-new-privileges", "--cap-drop", "ALL", "--cap-add", "DAC_READ_SEARCH", "--pids-limit", "64", "--entrypoint", "du"}
	paths := make([]string, len(vols))
	for i, v := range vols {
		paths[i] = fmt.Sprintf("/potluck-measure/%d", i)
		a = append(a, "--mount", "type=volume,source="+v+",target="+paths[i]+",readonly,volume-nocopy")
	}
	raw, err := o.helper(ociMeasurePrefix, ociMeasureFor, append(append(a, image, "-sxb"), paths...))
	if err != nil {
		fmt.Fprintln(os.Stderr, "potluck-driver-tart: measuring volumes:", err)
		return out
	}
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		n, p, ok := strings.Cut(line, "\t")
		b, perr := strconv.ParseInt(strings.TrimSpace(n), 10, 64)
		if !ok || perr != nil {
			continue
		}
		for i, q := range paths {
			if strings.TrimSpace(p) == q {
				out[vols[i]] = b
			}
		}
	}
	return out
}

// helper runs one throwaway container of the driver's own, named prefix and
// random hex, with run args a (everything after the name), bounded by wait,
// and removes it: its stdout. A container whose id the engine never wrote
// is never removed by name (a name that collided is not this one's).
func (o ociCLI) helper(prefix string, wait time.Duration, a []string) ([]byte, error) {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp("", prefix)
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	cidFile := filepath.Join(dir, "cid")
	raw, err := o.boundedCommand(wait, append([]string{"run", "--rm", "--cidfile", cidFile, "--name", prefix + hex.EncodeToString(b)}, a...)...)
	// Docker writes cidfile after create and before start. An empty/missing CID
	// gives no ownership: never remove a name whose creation might have collided.
	cid, readErr := os.ReadFile(cidFile)
	container := strings.TrimSpace(string(cid))
	decoded, decodeErr := hex.DecodeString(container)
	if readErr != nil || decodeErr != nil || len(decoded) != 32 {
		return nil, fmt.Errorf("%s helper: no container id (%v)", prefix, err)
	}
	_, cleanupErr := o.boundedCommand(2*time.Second, "rm", "-f", container)
	if cleanupErr != nil && !strings.Contains(strings.ToLower(cleanupErr.Error()), "no such container") {
		return nil, fmt.Errorf("%s helper: removing it: %v", prefix, cleanupErr)
	}
	return raw, err
}

// ociMeasurePrefix begins every measuring helper's container name.
const ociMeasurePrefix = "potluck-measure-"

// ociMeasureFor bounds one measurement.
var ociMeasureFor = 5 * time.Minute

var _ volumer = ociCLI{}

// errNoOuterVolumes is outerVolumeEngine's refusal for a layer that never
// mounts outer volumes (a bounded profile, native Kata).
var errNoOuterVolumes = errors.New("this OCI layer mounts no outer named volumes")

// A separately admitted helper lifecycle is required before the new native
// profile can offer outer volumes. This gate protects all measurement callers,
// including legacy records and direct missing-profile/alias entry paths.
// A refusal for the layer's kind (not a failure to reach the engine) wraps
// errNoOuterVolumes.
func (o ociCLI) outerVolumeEngine() (ociCLI, error) {
	if o.nativeBound || o.host != nil {
		return o, fmt.Errorf("%w: bounded OCI profiles do not support outer named volumes: measurement requires its own admitted lifecycle", errNoOuterVolumes)
	}
	id, err := o.verifyEngine()
	if err != nil {
		return o, err
	}
	if id.kata {
		return o, fmt.Errorf("%w: native Kata outer volumes require a separately admitted measurement lifecycle", errNoOuterVolumes)
	}
	o.engineHost = id.endpoint
	return o, nil
}

// CleanSeal cleans vol for its seal (volume_keep.go: keep, then normalise)
// with one throwaway helper container from the box's own image under the
// binding's runtime, like VolumeBytes': no network, a read-only root, every
// capability dropped but those that let root delete and chmod what the
// guest's user made private, vol the one mount. The same helper then
// measures it (du -sxb, as VolumeBytes does): no second container, and the
// release's seal waits for one helper only. Bounded by ociKeepFor. An error
// leaves the volume unsealed.
func (o ociCLI) CleanSeal(image, vol string, keep []string) (int64, error) {
	o, err := o.outerVolumeEngine()
	if err != nil {
		return 0, err
	}
	const target = "/potluck-keep"
	a := []string{"--network", "none", "--runtime", o.runtime, "--read-only", "--user", "0",
		"--security-opt", "no-new-privileges", "--cap-drop", "ALL", "--cap-add", "DAC_OVERRIDE", "--cap-add", "DAC_READ_SEARCH", "--cap-add", "FOWNER",
		"--pids-limit", "64", "--entrypoint", "sh",
		"--mount", "type=volume,source=" + vol + ",target=" + target + ",volume-nocopy",
		image, "-c", sealCleanScript + ociSealMeasure, "sh", target}
	raw, err := o.helper(ociKeepPrefix, ociKeepFor, append(a, sealCleanArgs(keep)...))
	if err != nil {
		return 0, err
	}
	return duBytes(raw, target)
}

// ociSealMeasure follows sealCleanScript in the cleaning helper: the
// cleaned volume's size, as VolumeBytes reads one.
const ociSealMeasure = `du -sxb "$1"
`

// duBytes reads du -sb's line for path out of a helper's stdout.
func duBytes(raw []byte, path string) (int64, error) {
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		n, p, ok := strings.Cut(line, "\t")
		if b, err := strconv.ParseInt(strings.TrimSpace(n), 10, 64); ok && err == nil && b >= 0 && strings.TrimSpace(p) == path {
			return b, nil
		}
	}
	return 0, fmt.Errorf("the cleaning helper did not say %s's size", path)
}

// ociKeepPrefix begins every cleaning helper's container name; unlike a
// measuring helper's, it writes, so VolumeInUse counts it.
const ociKeepPrefix = "potluck-keep-"

// ociKeepFor bounds one cleanup: well inside sealWindow, which a cleanup
// that starts up to sealSettle after the release must finish within, or the
// volume is deleted instead of sealed. A layer's prune lists only the
// directories on the way to its kept trees; the normalising walks are three
// finds over what is left.
var ociKeepFor = 45 * time.Second

// LayerVolumes are every volume the CLI lists.
func (o ociCLI) LayerVolumes() ([]string, error) {
	o, err := o.outerVolumeEngine()
	if err != nil {
		return nil, err
	}
	return o.volumeNames()
}
