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
