package vmhost

import (
	"fmt"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
)

// A box's own volumes on the OCI layer, besides its cow copies (oci_volume.go):
//   - <vm>-home, a kept box's HOME (oci_persist.go);
//   - <vm>-anon<N>, one per VOLUME directory of the image, which docker,
//     podman and nerdctl would otherwise give an anonymous volume: a volume
//     named for its box, so a sweep can tell whose it is after the container
//     is gone (an anonymous one names nothing; its container's rm -v is the
//     only thing that ever deletes it). Under gVisor /tmp is one too
//     (ociDiskTmp).
//
// Destroy deletes them once the container is gone, and the sweep deletes
// those of any box it does not keep, so one that outlived a failed Destroy or
// a container removed some other way goes too. The name is the proof: a box
// name starts with its state directory's prefix, and no other volume of this
// driver's ends in -home or -anon<N> (a layer volume is <prefix>v<hex>, a cow
// copy <vm>-cow<N>[uw], an ro view <vm>-ro<N>).

// ociAnonVolume is the volume a box gets at the image's i-th VOLUME directory.
func ociAnonVolume(vm string, i int) string { return vm + "-anon" + strconv.Itoa(i) }

// ociBoxOfVolume is the box whose own volume vol is (its HOME or a VOLUME
// directory's), or false.
func ociBoxOfVolume(vol string) (string, bool) {
	vm, ok := strings.CutSuffix(vol, ociHomeVolume(""))
	if !ok {
		i := strings.LastIndex(vol, "-anon")
		if i < 0 {
			return "", false
		}
		n := vol[i+len("-anon"):]
		if n == "" || strings.Trim(n, "0123456789") != "" || len(n) > 1 && n[0] == '0' {
			return "", false
		}
		vm = vol[:i]
	}
	if vm == "" || strings.HasSuffix(vm, "-") { // a prefix alone names no box
		return "", false
	}
	return vm, true
}

// volumeNames lists the CLI's volumes.
func (o ociCLI) volumeNames() ([]string, error) {
	out, err := o.run("volume", "ls", "-q")
	if err != nil {
		return nil, err
	}
	return strings.Fields(out), nil
}

// boxVolumes are the CLI's volumes that are boxes' own, by box.
func (o ociCLI) boxVolumes() (map[string][]string, error) {
	names, err := o.volumeNames()
	if err != nil {
		return nil, err
	}
	by := map[string][]string{}
	for _, v := range names {
		if vm, ok := ociBoxOfVolume(v); ok {
			by[vm] = append(by[vm], v)
		}
	}
	return by, nil
}

// rmVolumes deletes vols, one at a time, never with -f: on podman, volume
// rm -f also removes every container using the volume, so a box that came to
// mount one between a check and the delete would be killed. Without it every
// CLI refuses a volume a container uses. A volume the CLI says it cannot
// delete counts as deleted only when its list no longer has it.
func (o ociCLI) rmVolumes(vols ...string) error {
	var failed []string
	var last error
	for _, v := range vols {
		if _, err := o.run("volume", "rm", v); err != nil {
			failed, last = append(failed, v), err
		}
	}
	if len(failed) == 0 {
		return nil
	}
	names, err := o.volumeNames()
	if err != nil {
		return last
	}
	have := map[string]bool{}
	for _, n := range names {
		have[n] = true
	}
	for _, v := range failed {
		if have[v] {
			return last
		}
	}
	return nil // gone all the same (deleted by another sweep, or never made)
}

// dropBoxVolumes deletes a box's own volumes (Destroy, once the container is
// gone), unless a container still mounts one: the sweep retries what is left.
func (o ociCLI) dropBoxVolumes(vm string) {
	by, err := o.boxVolumes()
	if err != nil {
		fmt.Fprintln(os.Stderr, "potluck-driver-tart: volumes of", vm, "kept for the next sweep:", err)
		return
	}
	o.dropOwn(vm, by[vm])
}

func (o ociCLI) dropOwn(vm string, vols []string) {
	if len(vols) == 0 {
		return
	}
	if o.inUse(vols...) {
		fmt.Fprintln(os.Stderr, "potluck-driver-tart: volumes of", vm, "are still in use; kept for the next sweep")
		return
	}
	if err := o.rmVolumes(vols...); err != nil {
		fmt.Fprintln(os.Stderr, "potluck-driver-tart:", err)
	}
}

// SweepLeaks deletes what boxes keep does not name left behind on the CLI:
// their cow copies (sweepCOW) and their own volumes. keep names every box of
// another state directory, every container the CLI lists and every box a
// live record or a running supervisor holds; a volume a container mounts is
// kept too. Only this CLI's volumes are seen: another binding's are on a
// daemon this one cannot list.
func (o ociCLI) SweepLeaks(keep func(vm string) bool) {
	o.sweepCOW(keep)
	by, err := o.boxVolumes()
	if err != nil {
		return
	}
	vms := make([]string, 0, len(by))
	for vm := range by {
		vms = append(vms, vm)
	}
	sort.Strings(vms)
	for _, vm := range vms {
		if !keep(vm) {
			o.dropOwn(vm, by[vm])
		}
	}
}

// ociDiskTmp is the directory a gVisor box gets a volume of its own at, as
// if its image named it a VOLUME. runsc mounts a tmpfs of the sentry's at
// /tmp whenever the container's spec mounts nothing there and the image's
// /tmp is empty, and a tmpfs page is the sandbox's memory, charged to the
// box's memory limit and never given back while the file is there: a job
// that writes its build cache or test files under /tmp (a HOME from mktemp
// -d) was OOM-killed at the box's size with most of it in that tmpfs
// (zm-eakh.12.47.10.10.13). With a mount there, runsc leaves /tmp to the
// gofer, and the volume's files are on the CLI's disk: page cache the
// kernel writes back and reclaims. The root itself needs nothing: runsc's
// root overlay (overlay2 root:self) keeps its upper layer in a file on that
// disk, which reclaims the same way. Kata's /tmp is its guest kernel's and
// is left alone.
const ociDiskTmp = "/tmp"

// anonArgs mount a named volume of the box's own at each of the image's
// VOLUME directories (and, under gVisor, ociDiskTmp) that the other create
// flags (a) do not mount already, so none is anonymous. Each is mounted as an anonymous one is: the image's
// files there copied in (docker and podman do this for a named volume as for
// an anonymous one). Only --mount flags' target= counts as taken: clone gives
// every mount that way.
func (o ociCLI) anonArgs(image, vm string, a []string) ([]string, error) {
	out, err := o.run("image", "inspect", "--format", `{{range $p, $_ := .Config.Volumes}}{{println $p}}{{end}}`, image)
	if err != nil {
		return nil, err
	}
	taken := map[string]bool{}
	for i := 0; i+1 < len(a); i++ {
		if a[i] != "--mount" {
			continue
		}
		for _, f := range strings.Split(a[i+1], ",") {
			if t, ok := strings.CutPrefix(f, "target="); ok {
				taken[path.Clean(t)] = true
			}
		}
	}
	var dirs []string
	if o.gvisor() {
		out += "\n" + ociDiskTmp
	}
	for _, d := range strings.Split(out, "\n") {
		if d = strings.TrimSpace(d); d == "" {
			continue
		}
		raw := d
		if d = path.Clean(d); !path.IsAbs(d) || d == "/" || strings.ContainsAny(d, ",=\"") {
			return nil, fmt.Errorf("image %s's VOLUME %q is not a plain absolute path", image, raw)
		}
		if !taken[d] {
			taken[d] = true // and two spellings of one directory get one volume
			dirs = append(dirs, d)
		}
	}
	sort.Strings(dirs)
	var m []string
	for i, d := range dirs {
		m = append(m, "--mount", "type=volume,source="+ociAnonVolume(vm, i)+",target="+d)
	}
	return m, nil
}
