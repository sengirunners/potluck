package vmhost

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// shelfWarmBox is a box binding over fakeOCI with warm=1 and its image.
func shelfWarmBox(t *testing.T) (m map[string]string, dir string) {
	t.Helper()
	o, dir := newFakeOCI(t, "none")
	img := "zm/box@sha256:" + strings.Repeat("ab", 32)
	_ = os.MkdirAll(filepath.Join(dir, "images", "zm"), 0o700)
	if err := os.WriteFile(filepath.Join(dir, "images", img), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	m = map[string]string{"target": "lx", "kind": "box", "vm": "oci", "oci_cli": o.bin, "oci_runtime": "runsc", "state_dir": stateDir(t),
		"size": "c1m2", "cpu": "1", "memory": "512", "images": img, "boot_timeout_s": "20", "warm": "1", "linux_slots": "2"}
	return m, dir
}

// hasShelfMount says whether box vm's create mounted the shelf as every box's.
func hasShelfMount(t *testing.T, dir, vm string) bool {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "c", vm, "args"))
	if err != nil {
		t.Fatalf("box %s was not created: %v", vm, err)
	}
	return slices.Contains(strings.Split(string(b), "\n"), ociCacheMount)
}

func listsShelf(t *testing.T, m map[string]string) bool {
	t.Helper()
	d, err := call(t, m, "describe", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	caps, _ := d["capabilities"].([]any)
	return slices.Contains(caps, any("shelf"))
}

// readyWarm is the one ready warm box's VM.
func readyWarm(t *testing.T, m map[string]string) string {
	t.Helper()
	waitWarm(t, m, 1)
	for _, r := range (store{m["state_dir"]}).all() {
		if r.Warm && r.State == stReady {
			return r.VM
		}
	}
	t.Fatal("no ready warm box")
	return ""
}

// boxOf is the VM behind acquired handle h.
func boxOf(t *testing.T, m map[string]string, h string) string {
	t.Helper()
	r, err := (store{m["state_dir"]}).load(h[strings.LastIndex(h, "/")+1:])
	if err != nil {
		t.Fatal(err)
	}
	return r.VM
}

func acquireBox(t *testing.T, m map[string]string, key string) (h, vm string) {
	t.Helper()
	a, err := call(t, m, "acquire", "", map[string]any{"request_id": key, "ttl_s": 60})
	if err != nil {
		t.Fatal(err)
	}
	h = a["handle"].(string)
	t.Cleanup(func() { _, _ = call(t, m, "release", h, map[string]any{}) })
	return h, boxOf(t, m, h)
}

func liveVM(m map[string]string, vm string) bool {
	for _, r := range (store{m["state_dir"]}).all() {
		if r.VM == vm && r.live() {
			return true
		}
	}
	return false
}

// zm-eakh.12.30.10: the shelf appearing replaces the warm pool, as does it
// going (the sweep's pass), and the refilled warm box is handed out.
func TestWarmPoolFollowsTheShelf(t *testing.T) {
	old := warmShelfEvery
	warmShelfEvery = 0
	t.Cleanup(func() { warmShelfEvery = old })
	m, dir := shelfWarmBox(t)
	before := readyWarm(t, m)
	if hasShelfMount(t, dir, before) || listsShelf(t, m) {
		t.Fatal("no volume, yet a shelf")
	}
	if err := os.WriteFile(filepath.Join(dir, "cache-volume"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if !listsShelf(t, m) {
		t.Fatal("the volume exists, yet describe lists no shelf")
	}
	if liveVM(m, before) {
		t.Error("the warm box made before the shelf outlived the sweep")
	}
	withShelf := readyWarm(t, m)
	if !hasShelfMount(t, dir, withShelf) {
		t.Fatal("the refilled warm box lacks the shelf")
	}
	h, vm := acquireBox(t, m, "k1")
	if vm != withShelf {
		t.Errorf("the acquire got %s, not the matching warm box %s", vm, withShelf)
	}
	release(t, m, h)

	again := readyWarm(t, m)
	if err := os.Remove(filepath.Join(dir, "cache-volume")); err != nil {
		t.Fatal(err)
	}
	if listsShelf(t, m) {
		t.Fatal("no volume, yet describe lists the shelf")
	}
	if liveVM(m, again) {
		t.Error("the warm box with the shelf outlived the sweep after the volume went")
	}
	h, vm = acquireBox(t, m, "k2")
	if hasShelfMount(t, dir, vm) {
		t.Errorf("describe lists no shelf, yet the acquire got %s with it", vm)
	}
	release(t, m, h)
}

// The take itself checks its box: with the sweep's pass not yet due, a warm
// box made before the shelf appeared is ended, never handed out.
func TestTakeWarmRefusesAStaleShelf(t *testing.T) {
	old := warmShelfEvery
	warmShelfEvery = time.Hour
	t.Cleanup(func() { warmShelfEvery = old })
	m, dir := shelfWarmBox(t)
	stale := readyWarm(t, m) // describe's sweeps stamp the pass: it is not due again
	if err := os.WriteFile(filepath.Join(dir, "cache-volume"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	_, vm := acquireBox(t, m, "k")
	if vm == stale || !hasShelfMount(t, dir, vm) {
		t.Errorf("describe lists the shelf, yet the acquire got %s (the stale warm box: %v) without it", vm, vm == stale)
	}
	if liveVM(m, stale) {
		t.Error("the stale warm box is still live")
	}
}

// A shelf whose state cannot be read fails the create: never a box without
// the mount describe would list. No volume (the list says so) is no mount.
func TestShelfUnreadableFailsTheCreate(t *testing.T) {
	o, dir := newFakeOCI(t, "none")
	touch := func(f string) {
		if err := os.WriteFile(filepath.Join(dir, f), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := o.Clone("zm-linux-ci", "a", 1, 512); err != nil || hasShelfMount(t, dir, "a") {
		t.Fatalf("no volume: %v, mount %v", err, err == nil && hasShelfMount(t, dir, "a"))
	}
	touch("cache-volume")
	touch("cache-inspect-fails")
	if err := o.Clone("zm-linux-ci", "b", 1, 512); err == nil {
		t.Error("the volume exists but inspect failed, yet the create worked")
	}
	if _, err := os.Stat(filepath.Join(dir, "c", "b")); err == nil {
		t.Error("a box was created without its shelf")
	}
	if on, err := o.ShelfMounted(); err == nil || on {
		t.Errorf("ShelfMounted %v %v with an unreadable volume", on, err)
	}
	if o.HasShelf() {
		t.Error("describe would list a shelf it cannot read")
	}
	_ = os.Remove(filepath.Join(dir, "cache-volume"))
	touch("ls-fails")
	if err := o.Clone("zm-linux-ci", "c", 1, 512); err == nil {
		t.Error("inspect and the volume list both failed, yet the create worked")
	}
}

// BoxShelf reads a box's mounts and refuses a shelf mount not read-only at
// its place: every tenant's box mounts it the same way.
func TestBoxShelfRefusesAnotherMount(t *testing.T) {
	o, dir := newFakeOCI(t, "none")
	for vm, mount := range map[string]string{"rw": "type=volume,source=potluck-cache,target=/opt/potluck-cache", "none": "type=volume,source=other,target=/x"} {
		if err := os.MkdirAll(filepath.Join(dir, "c", vm), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "c", vm, "args"), []byte("create\n--mount\n"+mount+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if on, err := o.BoxShelf("rw"); err == nil {
		t.Errorf("a writable shelf mount read as %v", on)
	}
	if on, err := o.BoxShelf("none"); err != nil || on {
		t.Errorf("a box without the shelf: %v %v", on, err)
	}
	if _, err := o.BoxShelf("gone"); err == nil {
		t.Error("a missing box read as fine")
	}
}
