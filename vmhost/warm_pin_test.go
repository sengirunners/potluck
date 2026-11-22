package vmhost

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A rebind that moves the binding's default image (the daemon refreshing a
// host's pins, or a re-run of potluck host) ends the warm boxes booted on the
// old one: a box handed out from them would boot a digest none of the
// binding's pins name. The acquire after it gets a box of the new image.
func TestWarmBoxOfAnOldPinIsNeverHandedOut(t *testing.T) {
	m, dir := shelfWarmBox(t)
	stale := readyWarm(t, m)
	next := "zm/box@sha256:" + strings.Repeat("cd", 32)
	if err := os.WriteFile(filepath.Join(dir, "images", next), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	m["images"] = next
	h, vm := acquireBox(t, m, "k")
	if vm == stale {
		t.Fatalf("the acquire got the warm box booted on the old image after the binding moved to %s", next)
	}
	r, err := (store{m["state_dir"]}).load(h[strings.LastIndex(h, "/")+1:])
	if err != nil {
		t.Fatal(err)
	}
	if r.Image != next || r.Digest != "sha256:"+strings.Repeat("cd", 32) {
		t.Errorf("the box booted %s (%s), want %s", r.Image, r.Digest, next)
	}
	if liveVM(m, stale) {
		t.Error("the warm box of the old pin is still live")
	}
}

// A rebind that changes the binding's root or docker ends the warm boxes made
// before it: a box keeps the root it was made with, so one handed out after
// the rebind would have root where describe lists none, or none where it
// lists root.
func TestWarmBoxOfAnotherRootIsNeverHandedOut(t *testing.T) {
	for _, c := range []struct{ from, to string }{{"on", "off"}, {"off", "on"}} {
		m, dir := shelfWarmBox(t)
		if err := os.WriteFile(filepath.Join(dir, "images", m["images"]+".inspect"), []byte("runner\nHOME=/home/runner\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		m["oci_runtime"], m["docker"] = ociSudoRuntime, c.from
		stale := readyWarm(t, m)
		m["docker"] = c.to
		if _, vm := acquireBox(t, m, "k"); vm == stale {
			t.Errorf("the acquire got the warm box made with docker %s after the binding moved to docker %s", c.from, c.to)
		}
		if liveVM(m, stale) {
			t.Errorf("the warm box made with docker %s is still live under docker %s", c.from, c.to)
		}
	}
}
