package vmhost

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sengirunners/potluck/protocol"
)

// sealBox is a fake-layer box binding for the seal tests: its state, its
// fake layer and the build volume's id and layer directory for owner o's
// volume name.
type sealBox struct {
	m    map[string]string
	st   store
	fake dirLayer
}

func newSealBox(t *testing.T) sealBox {
	m := fakeVolumeBox(t)
	return sealBox{m: m, st: store{m["state_dir"]}, fake: dirLayer{filepath.Join(m["state_dir"], "fakevms")}}
}

func (b sealBox) dir(owner, name string) string {
	return b.fake.vol(b.st.vmPrefixNew() + volID("t", owner, name))
}

// gone waits until owner's volume name has neither its record nor its
// directory; within, without any other verb (the supervisor's end deletes it).
func (b sealBox) gone(t *testing.T, owner, name, why string, within time.Duration) {
	t.Helper()
	for deadline := time.Now().Add(within); ; time.Sleep(100 * time.Millisecond) {
		_, ok := b.st.loadVol(volID("t", owner, name))
		_, err := os.Stat(b.dir(owner, name))
		if !ok && err != nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: the unsealed build volume is still there (record %v, directory %v)", why, ok, err == nil)
		}
	}
}

func build(name string) map[string]any { return vol(name, "dep", protocol.VolumeBuild) }

func sealedRO(name string) map[string]any {
	return map[string]any{"name": name, "path": "dep", "mode": protocol.VolumeRO, "sealed": true}
}

// The supervisor deletes an unsealed build volume when its box expires or is
// reclaimed (volume quota), with no other verb; the sweep deletes it when the
// supervisor died.
func TestBuildVolumeGoesWithItsBox(t *testing.T) {
	b := newSealBox(t)
	b.m["volume_max_mb"], b.m["volume_check_s"] = "1", "1"

	a := volArgs("exp", "o", build("exp"))
	a["ttl_s"] = 2
	h := acquireVol(t, b.m, a)
	if c, _ := sh(t, b.m, h, `echo part > "$HOME/dep/f"`); c != 0 {
		t.Fatal("write")
	}
	b.gone(t, "o", "exp", "an expired builder", 10*time.Second)

	h = acquireVol(t, b.m, volArgs("quota", "o", build("quota")))
	if c, _ := sh(t, b.m, h, `head -c 2097152 /dev/zero > "$HOME/dep/big"`); c != 0 {
		t.Fatal("write")
	}
	b.gone(t, "o", "quota", "a builder reclaimed past its quota", 15*time.Second)
	if s, err := call(t, b.m, "status", h, nil); err != nil || s["state"] != "reclaimed" {
		t.Fatalf("the quota builder: %v %v, want reclaimed", s, err)
	}

	// Crash: the supervisor is gone; the next verb's sweep ends the box and deletes the volume.
	h = acquireVol(t, b.m, volArgs("crash", "o", build("crash")))
	if c, _ := sh(t, b.m, h, `echo part > "$HOME/dep/f"`); c != 0 {
		t.Fatal("write")
	}
	r, err := b.st.load(idOf(h))
	if err != nil {
		t.Fatal(err)
	}
	_ = syscall.Kill(r.Supervisor, syscall.SIGKILL)
	if _, err := b.st.update(r.ID, func(o *record) error { o.Supervisor = deadPID(t); return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := call(t, b.m, "list", "", nil); err != nil {
		t.Fatal(err)
	}
	b.gone(t, "o", "crash", "a builder whose supervisor died", time.Second)
	if _, err := call(t, b.m, "acquire", "", volArgs("crash-ro", "o", sealedRO("crash"))); code(err) != "not_found" {
		t.Errorf("sealed after a crashed builder: %v, want not_found", err)
	}
}

// A builder past its deadline that its supervisor has not reaped yet has
// expired: its release with seal deletes the volume, never seals it.
func TestBuildPastItsDeadlineIsNeverSealed(t *testing.T) {
	b := newSealBox(t)
	h := acquireVol(t, b.m, volArgs("late", "o", build("late")))
	if c, _ := sh(t, b.m, h, `echo part > "$HOME/dep/f"`); c != 0 {
		t.Fatal("write")
	}
	r, err := b.st.load(idOf(h))
	if err != nil {
		t.Fatal(err)
	}
	_ = syscall.Kill(r.Supervisor, syscall.SIGSTOP) // it never sees the deadline pass
	defer syscall.Kill(r.Supervisor, syscall.SIGCONT)
	if _, err := b.st.update(r.ID, func(o *record) error { o.Deadline = time.Now().Add(-time.Second).UTC(); return nil }); err != nil {
		t.Fatal(err)
	}
	if res, err := call(t, b.m, "release", h, map[string]any{"seal": true}); err != nil || res["released"] != true {
		t.Fatalf("release: %v %v", res, err)
	}
	if _, ok := b.st.loadVol(volID("t", "o", "late")); ok {
		t.Fatal("release with seal of a box past its deadline left its build volume (or sealed it)")
	}
	if _, err := os.Stat(b.dir("o", "late")); err == nil {
		t.Fatal("its directory is still there")
	}
	if _, err := call(t, b.m, "acquire", "", volArgs("late-ro", "o", sealedRO("late"))); code(err) != "not_found" {
		t.Errorf("sealed of it: %v, want not_found", err)
	}
}

// A seal waits for nothing to reference the volume; while it waits, the
// volume is neither sealed nor anyone's, and the sweep leaves it to the
// release. A sealed volume is then kept by the keep time while a box holds
// it, and deleted whole: its sealed record first.
func TestSealWaitsAndSealedGoesWhole(t *testing.T) {
	b := newSealBox(t)
	h := acquireVol(t, b.m, volArgs("b", "o", build("lay")))
	if c, _ := sh(t, b.m, h, `echo built > "$HOME/dep/f"`); c != 0 {
		t.Fatal("write")
	}
	layer := b.st.vmPrefixNew() + volID("t", "o", "lay")
	inuse := filepath.Join(filepath.Dir(b.fake.root), "inuse-"+layer)
	if err := os.WriteFile(inuse, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := call(t, b.m, "release", h, map[string]any{"seal": true}); done <- err }()
	time.Sleep(time.Second)
	select {
	case err := <-done:
		t.Fatalf("release answered while a container referenced the volume: %v", err)
	default:
	}
	if v, _ := b.st.loadVol(volID("t", "o", "lay")); v.Sealed {
		t.Fatal("sealed while a container referenced it")
	}
	if _, err := call(t, b.m, "list", "", nil); err != nil { // the sweep leaves it to the release
		t.Fatal(err)
	}
	for _, mode := range []string{"ro", "cow", "rw", "build"} {
		_, err := call(t, b.m, "acquire", "", volArgs("w-"+mode, "o", vol("lay", "dep", mode)))
		if code(err) != "unavailable" {
			t.Errorf("%s while its seal waits: %v, want unavailable (building)", mode, err)
		}
	}
	os.Remove(inuse)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	v, ok := b.st.loadVol(volID("t", "o", "lay"))
	if !ok || !v.Sealed || v.Build != "" {
		t.Fatalf("after the seal: %+v %v", v, ok)
	}

	// Held ro by a box, past volume_keep_s: kept.
	b.m["volume_keep_s"] = "60"
	ro := acquireVol(t, b.m, volArgs("ro", "o", sealedRO("lay")))
	v.Used = time.Now().Add(-time.Hour).UTC()
	_ = b.st.saveVol(v)
	if _, err := call(t, b.m, "list", "", nil); err != nil {
		t.Fatal(err)
	}
	if v, ok := b.st.loadVol(volID("t", "o", "lay")); !ok || !v.Sealed {
		t.Fatal("the keep time deleted a sealed volume a box holds")
	}
	release(t, b.m, ro)

	// Deleted whole: a delete that cannot finish leaves no sealed record.
	d := &drv{cfg: config{Target: "t"}, st: b.st, vm: b.fake}
	_ = os.WriteFile(inuse, nil, 0o600)
	v, _ = b.st.loadVol(volID("t", "o", "lay"))
	if d.dropVolume(v) {
		t.Fatal("a sealed volume a container references was deleted")
	}
	if v, _ := b.st.loadVol(volID("t", "o", "lay")); !v.Sealed {
		t.Fatal("a refused delete cleared the sealed record of a volume in use")
	}
	os.Remove(inuse)
	if !d.dropVolume(v) {
		t.Fatal("a sealed volume nothing references was kept")
	}
	if _, ok := b.st.loadVol(volID("t", "o", "lay")); ok {
		t.Fatal("its record is still there")
	}
}

// The LRU and the keep time never delete a live build volume.
func TestLiveBuildIsNeverEvicted(t *testing.T) {
	b := newSealBox(t)
	b.m["volume_keep_s"] = "60"
	h := acquireVol(t, b.m, volArgs("b", "o", build("live")))
	defer release(t, b.m, h)
	v, _ := b.st.loadVol(volID("t", "o", "live"))
	v.Used, v.Bytes = time.Now().Add(-time.Hour).UTC(), 1<<40
	_ = b.st.saveVol(v)
	if _, err := call(t, b.m, "list", "", nil); err != nil {
		t.Fatal(err)
	}
	d := &drv{cfg: config{Target: "t", VolumeMaxGB: 1}, st: b.st, vm: b.fake}
	_ = b.st.locked(func() error { _ = d.volumeRoom("o", "project-of-o", map[string][]string{}, 0); return nil })
	if _, ok := b.st.loadVol(volID("t", "o", "live")); !ok {
		t.Fatal("a live build volume was deleted by the keep time or the LRU")
	}
}

// The busy answer carries its data on the wire (protocol.Busy).
func TestBuildingAnswerData(t *testing.T) {
	e := volRefusal(volReq{Name: "x", Mode: volRO}, volMeta{Build: "r1"}, true, nil)
	var r refusal
	if !errors.As(e, &r) || r.code != "unavailable" {
		t.Fatalf("%v", e)
	}
	b, _ := json.Marshal(r.data)
	if string(b) != `{"reason":"building","retry_s":5}` {
		t.Fatalf("data %s", b)
	}
}

// OCI: a build never builds on a volume the CLI has already, a sealed mount
// is never made empty, and a volume in use (or under a cow copy) is in use.
func TestOCISealMounts(t *testing.T) {
	o, dir := newFakeOCI(t, "none")
	o.stateDir = t.TempDir()
	img := filepath.Join(dir, "images")
	_ = os.WriteFile(filepath.Join(img, "ok-img"), nil, 0o600)
	_ = os.WriteFile(filepath.Join(img, "ok-img.inspect"), []byte("agent\nHOME=/home/agent\n"), 0o600)
	if err := o.CloneVolumes("ok-img", "zmrt-x-r1", 1, 512, []mount{{Vol: "b1", Path: "dep", Mode: volBuild}}); err != nil {
		t.Fatal(err)
	}
	args, _ := os.ReadFile(filepath.Join(dir, "c", "zmrt-x-r1", "args"))
	if !strings.Contains(string(args), "type=volume,source=b1,target=/home/agent/dep,volume-nocopy") || strings.Contains(string(args), "source=b1,target=/home/agent/dep,readonly") {
		t.Errorf("a build mount is the volume itself, writable: %s", args)
	}
	if err := o.CloneVolumes("ok-img", "zmrt-x-r2", 1, 512, []mount{{Vol: "b1", Path: "dep", Mode: volBuild}}); err == nil || !strings.Contains(err.Error(), "there already") {
		t.Errorf("a build of a volume the CLI has: %v", err)
	}
	_ = os.WriteFile(filepath.Join(dir, "novol-s1"), nil, 0o600)
	if err := o.CloneVolumes("ok-img", "zmrt-x-r3", 1, 512, []mount{{Vol: "s1", Path: "dep", Mode: volRO, Sealed: true}}); err == nil {
		t.Error("a sealed mount of a volume that is not there was made")
	}
	if calls, _ := os.ReadFile(filepath.Join(dir, "calls")); strings.Contains(string(calls), "volume create --label "+ociVolLabel+" s1") {
		t.Error("a sealed mount created its volume")
	}
	if o.VolumeInUse("b2") {
		t.Error("a volume nothing references is in use")
	}
	_ = os.WriteFile(filepath.Join(dir, "inuse-b2"), nil, 0o600)
	if !o.VolumeInUse("b2") {
		t.Error("a volume a container mounts is not in use")
	}
}

// OCI: the driver's own measuring helper (read-only) never holds a seal
// back, but still keeps a delete off the volume.
func TestOCIMeasureDoesNotHoldASeal(t *testing.T) {
	o, dir := newFakeOCI(t, "none")
	_ = os.WriteFile(filepath.Join(dir, "measuring-b3"), nil, 0o600)
	if o.VolumeInUse("b3") {
		t.Error("a measuring helper counts as a writer: a long measurement would hold the seal back")
	}
	if err := o.DeleteVolume("b3"); err == nil {
		t.Error("a volume a measuring helper mounts was deleted")
	}
}

// The sweep leaves a build volume to its box's supervisor while that runs
// (a box released while starting: its supervisor may still be making the
// volume), and deletes it once the supervisor is gone.
func TestSweepLeavesABuildToItsSupervisor(t *testing.T) {
	b := newSealBox(t)
	if _, err := call(t, b.m, "list", "", nil); err != nil { // makes the state directory
		t.Fatal(err)
	}
	now := time.Now().UTC()
	r := record{ID: "rstarting01", Target: "t", Kind: "box", State: stReleased, Supervisor: os.Getpid(), Start: now, End: now,
		Volumes: []volUse{{ID: volID("t", "o", "s"), Name: "s", Path: "dep", Mode: volBuild, Layer: b.st.vmPrefixNew() + volID("t", "o", "s"), New: true}}}
	if err := b.st.save(r); err != nil {
		t.Fatal(err)
	}
	m := volMeta{ID: r.Volumes[0].ID, Target: "t", Owner: "o", Name: "s", Layer: r.Volumes[0].Layer, Created: now, Used: now, Bytes: -1, Build: r.ID}
	if err := b.st.saveVol(m); err != nil {
		t.Fatal(err)
	}
	d := &drv{cfg: config{Target: "t"}, st: b.st, vm: b.fake}
	_ = b.st.locked(func() error { d.sweepBuilds(); return nil })
	if _, ok := b.st.loadVol(m.ID); !ok {
		t.Fatal("the sweep deleted a build volume whose supervisor still runs")
	}
	if _, err := b.st.update(r.ID, func(o *record) error { o.Supervisor = deadPID(t); return nil }); err != nil {
		t.Fatal(err)
	}
	_ = b.st.locked(func() error { d.sweepBuilds(); return nil })
	if _, ok := b.st.loadVol(m.ID); ok {
		t.Fatal("the sweep kept a build volume whose builder and supervisor are gone")
	}
}
