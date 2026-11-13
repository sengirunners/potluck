package runtarget

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/sengirunners/potluck/computeapi"
)

// HostCommand runs only a driver already pinned, and only the pinned file.
func TestHostCommandPins(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(root, "home")
	dir := filepath.Join(home, DriversDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	drv := filepath.Join(dir, "potluck-driver-x")
	writeDriver(t, drv, "ran")
	tg := computeapi.RunTarget{Name: "b", Kind: "box", Driver: "x"}
	var out bytes.Buffer
	wantRefused(t, HostCommand(context.Background(), home, tg, []string{"shelf"}, &out, io.Discard), "not pinned yet", "driver pin x")
	if out.Len() > 0 {
		t.Fatalf("an unpinned driver ran: %q", out.String())
	}
	if _, err := PinDriver(home, DriverDirs(home), "x"); err != nil {
		t.Fatal(err)
	}
	if err := HostCommand(context.Background(), home, tg, []string{"shelf"}, &out, io.Discard); err != nil || out.String() != "ran\n" {
		t.Fatalf("pinned: %q %v", out.String(), err)
	}
	writeDriver(t, drv, "swapped")
	out.Reset()
	wantRefused(t, HostCommand(context.Background(), home, tg, []string{"shelf"}, &out, io.Discard), "not the file that was pinned")
	if out.Len() > 0 {
		t.Fatalf("a swapped driver ran: %q", out.String())
	}
}
