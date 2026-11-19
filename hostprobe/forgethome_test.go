package hostprobe_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/potluck/hostprobe"
)

// ForgetTartHome forgets only a home whose own name is missing while the
// directory that held it is there: a home under a missing directory (a disk
// not mounted), a disk's mount point, a link to nothing, an unreadable
// place, a running daemon's home and a home the record does not name are
// refused with the record unchanged. Two spellings of one path are one home.
func TestForgetTartHomeRefusals(t *testing.T) {
	root := t.TempDir()
	tartDir := filepath.Join(root, "tart")
	if err := os.Mkdir(tartDir, 0o700); err != nil {
		t.Fatal(err)
	}
	at := time.Now().Add(-time.Hour)
	unmounted := filepath.Join(root, "disk", "home") // root/disk never made: as an unmounted disk
	link := filepath.Join(root, "link")
	if err := os.Symlink(filepath.Join(root, "nowhere"), link); err != nil {
		t.Fatal(err)
	}
	locked := filepath.Join(root, "locked")
	if err := os.Mkdir(locked, 0o700); err != nil {
		t.Fatal(err)
	}
	behind := filepath.Join(locked, "home")
	gone := filepath.Join(root, "gone")
	running := filepath.Join(root, "running")
	for _, h := range []string{unmounted, link, behind, gone + "/", running, "/Volumes/nodisk"} {
		if err := hostprobe.StartTartShare(tartDir, h, []string{"img@sha256:" + strings.Repeat("a", 64)}, at); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(locked, 0o700) })
	read := func() string {
		b, err := os.ReadFile(filepath.Join(tartDir, hostprobe.TartPullsFile))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	for home, why := range map[string]string{
		unmounted:                      "is missing too",
		link:                           "exists",
		"/Volumes/nodisk":              "where a disk mounts",
		running:                        "a daemon runs on",
		filepath.Join(root, "unknown"): "does not name",
		"relative/home":                "not an absolute path",
	} {
		before := read()
		if _, err := hostprobe.ForgetTartHome(tartDir, home, []string{running}); err == nil || !strings.Contains(err.Error(), why) {
			t.Errorf("%s: %v, want refused with %q", home, err, why)
		}
		if read() != before {
			t.Errorf("%s: the record changed on a refusal", home)
		}
	}
	if os.Geteuid() != 0 { // root reads past the mode
		if _, err := hostprobe.ForgetTartHome(tartDir, behind, nil); err == nil || !strings.Contains(err.Error(), "cannot be read") {
			t.Errorf("unreadable: %v, want refused", err)
		}
	}
	f, err := hostprobe.ForgetTartHome(tartDir, gone, nil)
	if err != nil || f.Home != gone+"/" || len(f.Images) != 1 || strings.Contains(read(), `"`+gone) {
		t.Fatalf("gone: %+v %v, record:\n%s", f, err, read())
	}
}
