package vmhost

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// fakeVols are the fake CLI's volumes now.
func fakeVols(t *testing.T, dir string) map[string]bool {
	t.Helper()
	b, _ := os.ReadFile(filepath.Join(dir, "vols"))
	have := map[string]bool{}
	for _, v := range strings.Fields(string(b)) {
		have[v] = true
	}
	return have
}

func addFakeVols(t *testing.T, dir string, vols ...string) {
	t.Helper()
	f, err := os.OpenFile(filepath.Join(dir, "vols"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for _, v := range vols {
		if _, err := f.WriteString(v + "\n"); err != nil {
			t.Fatal(err)
		}
	}
}

// noForcedVolumeRm fails on any volume rm -f: on podman it removes the
// containers using the volume.
func noForcedVolumeRm(t *testing.T, dir string) {
	t.Helper()
	b, _ := os.ReadFile(filepath.Join(dir, "calls"))
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "volume rm") && strings.Contains(" "+line+" ", " -f ") || strings.Contains(line, "--force") {
			t.Errorf("a volume was removed with force: %q", line)
		}
	}
}

// A box's HOME and VOLUME-directory volumes that outlived it (a failed
// Destroy, a container removed some other way) are deleted by the sweep
// (zm-eakh.12.65.10, .12); a live box's, a released one whose supervisor
// still runs, a listed container's, one a container mounts, another state
// directory's, a layer volume, a cow copy and any volume this driver cannot
// name as a box's own are kept; a delete that fails is retried by the next
// sweep; once all is gone a sweep deletes nothing more.
func TestSweepDropsLeakedBoxVolumes(t *testing.T) {
	o, dir := newFakeOCI(t, "none")
	state := t.TempDir()
	o.stateDir = state
	st := store{state}
	if err := os.MkdirAll(filepath.Join(state, "runs"), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg, err := parseConfig(map[string]string{"target": "lx", "vm": "oci", "image": "zm/linux-ci", "remote": "https://github.com/o/r.git",
		"state_dir": state, "oci_runtime": "runsc"})
	if err != nil {
		t.Fatal(err)
	}
	d := &drv{cfg: cfg, st: st, vm: o}

	p, old := st.vmPrefixNew(), st.vmPrefix()
	gone, oldGone, failing := p+"gone", old+"gone", p+"failing"
	leased, released, running, stopped, mounted := p+"leased", p+"released", p+"running", p+"stopped", p+"mounted"
	foreign := "potluck-00000000-gone"
	gones := []string{gone + "-home", gone + "-anon0", gone + "-anon1", oldGone + "-home", oldGone + "-anon0"}
	kept := []string{
		leased + "-home", released + "-anon0", running + "-home", running + "-anon0", stopped + "-home", mounted + "-home",
		foreign + "-home", foreign + "-anon0",
		p + "v0123456789abcdef",         // a layer volume its file names (one none names is dropOrphans')
		gone + "-cow0", gone + "-cow0u", // cow copies: oci-cow names them, and none is noted
		"3f9c1e0d2b7a6c5e4f3a2b1c0d9e8f7a6b5c", // an anonymous volume: whose, nobody can say
		p + "x-anon01", p + "x-anon", p + "-home", "potluck-cache",
	}
	addFakeVols(t, dir, gones...)
	addFakeVols(t, dir, kept...)
	addFakeVols(t, dir, failing+"-home")
	for _, f := range []string{"inuse-" + mounted + "-home", "rmfail-" + failing + "-home"} {
		if err := os.WriteFile(filepath.Join(dir, f), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.saveVol(volMeta{ID: "v0123456789abcdef", Target: "lx", Owner: "o", Name: "n", Layer: p + "v0123456789abcdef", Used: time.Now().UTC(), Bytes: 1}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for _, r := range []record{
		{ID: "r-leased", VM: leased, State: stStarting, Supervisor: os.Getpid(), Start: now, Deadline: now.Add(time.Hour)},
		{ID: "r-released", VM: released, State: stReleased, Supervisor: os.Getpid(), Start: now, End: now, Deadline: now.Add(time.Hour)},
		{ID: "r-running", VM: running, State: stReady, Supervisor: os.Getpid(), Start: now, Deadline: now.Add(time.Hour)},
		{ID: "r-stopped", VM: stopped, State: stStopped, Persist: true, Start: now, End: now, Deadline: now.Add(time.Hour)},
	} {
		if err := st.save(r); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(dir, "c", running), 0o700); err != nil {
		t.Fatal(err)
	}

	if err := d.sweep(); err != nil {
		t.Fatal(err)
	}
	have := fakeVols(t, dir)
	for _, v := range gones {
		if have[v] {
			t.Errorf("the leaked box volume %s is still there", v)
		}
	}
	for _, v := range kept {
		if !have[v] {
			t.Errorf("%s was removed: its box is live, busy, kept or mounted, or it is not a box's own volume of this directory", v)
		}
	}
	if !have[failing+"-home"] {
		t.Errorf("%s-home went though the CLI failed to delete it", failing)
	}
	if _, err := os.Stat(filepath.Join(dir, "c", running)); err != nil {
		t.Errorf("the running box's container went: %v", err)
	}

	// The CLI recovers: the next sweep deletes what it could not.
	if err := os.Remove(filepath.Join(dir, "rmfail-"+failing+"-home")); err != nil {
		t.Fatal(err)
	}
	if err := d.sweep(); err != nil {
		t.Fatal(err)
	}
	if fakeVols(t, dir)[failing+"-home"] {
		t.Errorf("the next sweep did not retry %s-home", failing)
	}

	before, _ := os.ReadFile(filepath.Join(dir, "calls"))
	if err := d.sweep(); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(filepath.Join(dir, "calls"))
	if extra := string(after[len(before):]); strings.Contains(extra, "volume rm") || strings.Contains(extra, "rm -f") {
		t.Errorf("a third sweep removed more:\n%s", extra)
	}
	var left []string
	for v := range fakeVols(t, dir) {
		left = append(left, v)
	}
	sort.Strings(left)
	sort.Strings(kept)
	if strings.Join(left, " ") != strings.Join(kept, " ") {
		t.Errorf("left %v, want %v", left, kept)
	}
	noForcedVolumeRm(t, dir)
}

// A box's VOLUME directories get named volumes of its own, never anonymous
// ones (zm-eakh.12.65.12), one per directory however the image spells it
// (a CLI's warnings on stderr are not read as one), except one another
// mount already covers (a kept box's HOME); Destroy deletes them and its
// HOME, without force (zm-eakh.12.65.11), and leaves the volumes of other
// boxes alone.
func TestOCIBoxVolumesNamedAndDestroyed(t *testing.T) {
	o, dir := newFakeOCI(t, "none")
	o.stateDir = t.TempDir()
	img := filepath.Join(dir, "images")
	for f, b := range map[string]string{"img": "", "img.inspect": "\nHOME=/home/u\n", "img.volumes": "/var/lib/data/\n/home/u\n//cache\n/cache\n"} {
		if err := os.WriteFile(filepath.Join(img, f), []byte(b), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := o.CloneKept("img", "b1", 0, 0); err != nil {
		t.Fatal(err)
	}
	args, err := os.ReadFile(filepath.Join(dir, "c", "b1", "args"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"type=volume,source=b1-anon0,target=/cache", "type=volume,source=b1-anon1,target=/var/lib/data",
		"type=volume,source=b1-home,target=/home/u"} {
		if !strings.Contains(string(args), want+"\n") {
			t.Errorf("create args lack %q:\n%s", want, args)
		}
	}
	if strings.Contains(string(args), "b1-anon2") {
		t.Errorf("HOME, already a volume, got a second one:\n%s", args)
	}
	addFakeVols(t, dir, "b2-home", "b2-anon0", "b1x-home")
	o.Destroy("b1", 0)
	have := fakeVols(t, dir)
	for _, v := range []string{"b1-home", "b1-anon0", "b1-anon1"} {
		if have[v] {
			t.Errorf("Destroy left %s", v)
		}
	}
	for _, v := range []string{"b2-home", "b2-anon0", "b1x-home"} {
		if !have[v] {
			t.Errorf("Destroy of b1 removed %s", v)
		}
	}
	noForcedVolumeRm(t, dir)
}

// A VOLUME path that cannot go into a --mount flag as it is refuses the box.
func TestOCIBoxVolumesRefuseOddPaths(t *testing.T) {
	o, dir := newFakeOCI(t, "none")
	for _, bad := range []string{"/a,readonly", "/a=b", "rel", "/..", "/"} {
		if err := os.WriteFile(filepath.Join(dir, "images", "img.volumes"), []byte(bad+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := o.Clone("img", "b1", 0, 0); err == nil {
			t.Errorf("VOLUME %q was mounted", bad)
		}
	}
}

// A volume that a container came to mount after the check is never deleted:
// without force every CLI refuses it, and the error is kept.
func TestOCIRmVolumesKeepsAMountedVolume(t *testing.T) {
	o, dir := newFakeOCI(t, "none")
	addFakeVols(t, dir, "v1", "v2")
	if err := os.WriteFile(filepath.Join(dir, "inuse-v1"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := o.rmVolumes("v1", "v2", "never-made"); err == nil {
		t.Error("rmVolumes said a mounted volume went")
	}
	have := fakeVols(t, dir)
	if !have["v1"] || have["v2"] {
		t.Errorf("volumes left %v, want v1 only", have)
	}
	if err := o.rmVolumes("never-made"); err != nil {
		t.Errorf("a volume that is not there: %v", err)
	}
	noForcedVolumeRm(t, dir)
}
