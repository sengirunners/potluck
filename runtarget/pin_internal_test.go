package runtarget

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/potluck/kit/swap"
)

// pinFixture is a home and a driver directory with potluck-driver-x, a
// script that prints what.
func pinFixture(t *testing.T, what string) (home, dir, drv string) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	home, dir = filepath.Join(root, "home"), filepath.Join(root, "bin")
	for _, d := range []string{home, dir} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	drv = filepath.Join(dir, "potluck-driver-x")
	writeDriver(t, drv, what)
	return home, dir, drv
}

// writeDriver replaces p (by rename: a new inode) with a script printing what.
func writeDriver(t *testing.T, p, what string) {
	t.Helper()
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, []byte("#!/bin/sh\necho "+what+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(tmp, 0o755); err != nil { // whatever the umask
		t.Fatal(err)
	}
	if err := os.Rename(tmp, p); err != nil {
		t.Fatal(err)
	}
}

func openX(p *pins, dir string) (*verified, error) {
	cand, err := FindDriver([]string{dir}, "x")
	return p.open("mac-vm", "x", cand, err)
}

func wantRefused(t *testing.T, err error, parts ...string) {
	t.Helper()
	if err == nil {
		t.Fatalf("not refused; want %q", parts)
	}
	for _, s := range parts {
		if !strings.Contains(err.Error(), s) {
			t.Fatalf("refusal %q does not say %q", err, s)
		}
	}
}

// A driver with no pin (a binding from before pins) is pinned on first use,
// with one log line; a swapped or missing file is refused, naming the
// binding, the path, both hashes and the re-pin command, until it is
// re-pinned.
func TestDriverPins(t *testing.T) {
	home, dir, drv := pinFixture(t, "one")
	var logs []string
	p := newPins(home, func(msg string, _ ...any) { logs = append(logs, msg) })
	for range 2 {
		v, err := openX(p, dir)
		if err != nil {
			t.Fatal(err)
		}
		v.Close()
	}
	if len(logs) != 1 || !strings.Contains(logs[0], "pinned on first use") {
		t.Fatalf("logs: %q", logs)
	}
	m, _ := readPins(home)
	first := m["x"]
	if first.Path != drv || len(first.SHA256) != 64 {
		t.Fatalf("pin: %+v", first)
	}

	writeDriver(t, drv, "two")
	_, err := openX(p, dir)
	wantRefused(t, err, `run target "mac-vm"`, drv, "expected "+first.SHA256[:12], "driver pin x")

	if _, err := PinDriver(home, []string{dir}, "x"); err != nil {
		t.Fatal(err)
	}
	v, err := openX(p, dir)
	if err != nil {
		t.Fatalf("after re-pin: %v", err)
	}
	v.Close()

	if err := os.Remove(drv); err != nil {
		t.Fatal(err)
	}
	_, err = openX(p, dir)
	wantRefused(t, err, `run target "mac-vm"`, drv, "no longer finds", "driver pin x")
	if len(logs) != 1 {
		t.Fatalf("a refusal or a re-pin logged a first use: %q", logs)
	}
}

// A file or directory someone else could write is refused, pinned or not.
func TestDriverPinsUnsafeModes(t *testing.T) {
	home, dir, drv := pinFixture(t, "one")
	p := newPins(home, nil)
	if err := os.Chmod(dir, 0o770); err != nil {
		t.Fatal(err)
	}
	_, err := openX(p, dir)
	wantRefused(t, err, dir+" is writable by group", "chmod go-w")
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(drv, 0o757); err != nil {
		t.Fatal(err)
	}
	_, err = openX(p, dir)
	wantRefused(t, err, drv+" is writable by group or others")
	if m, _ := readPins(home); len(m) != 0 {
		t.Fatalf("an unsafe driver was pinned: %v", m)
	}
}

// The hash cache never takes a same-size swap for the pinned file: a write
// in place changes the ctime even when the mtime is set back, and a rename
// changes the inode.
func TestDriverPinsCache(t *testing.T) {
	home, dir, drv := pinFixture(t, "one")
	p := newPins(home, nil)
	p.now = func() time.Time { return time.Now().Add(time.Hour) } // old enough to cache
	v, err := openX(p, dir)
	if err != nil {
		t.Fatal(err)
	}
	v.Close()
	if _, ok := p.sums[drv]; !ok {
		t.Fatal("the sum was not cached")
	}
	st, _ := os.Stat(drv)
	time.Sleep(10 * time.Millisecond)                                                 // a later ctime even on a coarse clock tick
	if err := os.WriteFile(drv, []byte("#!/bin/sh\necho two\n"), 0o755); err != nil { // same size
		t.Fatal(err)
	}
	if err := os.Chtimes(drv, st.ModTime(), st.ModTime()); err != nil {
		t.Fatal(err)
	}
	_, err = openX(p, dir)
	wantRefused(t, err, "not the file that was pinned")

	writeDriver(t, drv, "one") // back to the pinned bytes, by rename
	v, err = openX(p, dir)
	if err != nil {
		t.Fatal(err)
	}
	v.Close()
	writeDriver(t, drv, "two")
	if err := os.Chtimes(drv, st.ModTime(), st.ModTime()); err != nil {
		t.Fatal(err)
	}
	_, err = openX(p, dir)
	wantRefused(t, err, "not the file that was pinned")
}

// What runs is the file that was hashed: on Linux through its descriptor
// even when the path was swapped after the check; elsewhere the swap is
// caught by the last check before the exec.
func TestDriverExecVerified(t *testing.T) {
	home, dir, drv := pinFixture(t, "one")
	p := newPins(home, nil)
	v, err := openX(p, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	run := func() (string, error) {
		cmd := exec.CommandContext(context.Background(), v.path)
		if err := execVerified(cmd, v); err != nil {
			return "", err
		}
		var out bytes.Buffer
		cmd.Stdout = &out
		err := cmd.Run()
		return strings.TrimSpace(out.String()), err
	}
	if out, err := run(); err != nil || out != "one" {
		t.Fatalf("run: %q %v", out, err)
	}
	writeDriver(t, drv, "two")
	out, err := run()
	if runtime.GOOS == "linux" {
		if err != nil || out != "one" {
			t.Fatalf("after a swap: %q %v (want the hashed file's output)", out, err)
		}
	} else if err == nil {
		t.Fatalf("after a swap ran %q", out)
	}
}

// An updater re-pins the drivers it placed (swap.Placed.Sums), only those.
func TestRepinPlaced(t *testing.T) {
	home, dir, drv := pinFixture(t, "one")
	other := filepath.Join(dir, "potluck-driver-y")
	writeDriver(t, other, "y")
	p := newPins(home, nil)
	for _, d := range []string{"x", "y"} {
		cand, err := FindDriver([]string{dir}, d)
		v, err := p.open("t", d, cand, err)
		if err != nil {
			t.Fatal(err)
		}
		v.Close()
	}
	src := t.TempDir()
	writeDriver(t, filepath.Join(src, "potluck-driver-x"), "two")
	placed, err := swap.Place(src, dir, []string{"potluck-driver-x"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = openX(p, dir)
	wantRefused(t, err, "not the file that was pinned")
	writeDriver(t, other, "y2") // not placed by the updater: stays refused
	done, err := RepinPlaced(home, dir, placed.Sums)
	if err != nil || len(done) != 1 || done[0] != "x" {
		t.Fatalf("repin: %v %v", done, err)
	}
	if v, err := openX(p, dir); err != nil {
		t.Fatal(err)
	} else {
		v.Close()
	}
	_, err = p.open("t", "y", other, nil)
	wantRefused(t, err, "not the file that was pinned")
	_ = drv
}

// A link is judged by the directory it sits in, not only where it points: a
// link to /bin/sh dropped in a world-writable (even sticky) driver directory
// is refused before first use, and a new-name link beside a pinned old-name
// driver is refused after.
func TestDriverPinsSymlinks(t *testing.T) {
	home, dir, _ := pinFixture(t, "one")
	p := newPins(home, nil)
	open := filepath.Join(filepath.Dir(dir), "open")
	if err := os.Mkdir(open, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(open, 0o1777); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/bin/sh", filepath.Join(open, "potluck-driver-x")); err != nil {
		t.Fatal(err)
	}
	_, err := openX(p, open)
	wantRefused(t, err, open+" is writable by group or others")
	if m, _ := readPins(home); len(m) != 0 {
		t.Fatalf("a planted link was pinned: %v", m)
	}

	old := filepath.Join(dir, "potluck-driver-x")
	if err := os.Rename(old, filepath.Join(dir, "zm-run-x")); err != nil {
		t.Fatal(err)
	}
	v, err := openX(p, dir) // pins zm-run-x
	if err != nil {
		t.Fatal(err)
	}
	v.Close()
	if err := os.Symlink("/bin/sh", old); err != nil {
		t.Fatal(err)
	}
	_, err = openX(p, dir)
	wantRefused(t, err, "is pinned at "+filepath.Join(dir, "zm-run-x"), "now finds /", "driver pin x")
}

// No pins at all (a missing file, null, {} or {"drivers":null}) reads as
// empty: what warnUnpinned warns about when run targets exist.
func TestReadPinsEmpty(t *testing.T) {
	home := t.TempDir()
	for _, body := range []string{"", "null", "{}", `{"drivers":null}`} {
		if body != "" {
			if err := os.WriteFile(filepath.Join(home, PinFile), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if m, err := readPins(home); err != nil || m == nil || len(m) != 0 {
			t.Errorf("%q: %v %v", body, m, err)
		}
	}
}

// PinnedDriver (the block test's driver) never pins: an unpinned driver, a
// swapped file and a missing one are refused; only the pinned file passes.
func TestPinnedDriver(t *testing.T) {
	home, dir, drv := pinFixture(t, "one")
	_, err := PinnedDriver(home, []string{dir}, "x")
	wantRefused(t, err, "has no pin", "driver pin x")
	if m, _ := readPins(home); len(m) != 0 {
		t.Fatalf("PinnedDriver pinned: %+v", m)
	}
	want, err := PinDriver(home, []string{dir}, "x")
	if err != nil {
		t.Fatal(err)
	}
	got, err := PinnedDriver(home, []string{dir}, "x")
	if err != nil || got != want {
		t.Fatalf("pinned file: %+v, %v; want %+v", got, err, want)
	}
	writeDriver(t, drv, "two")
	_, err = PinnedDriver(home, []string{dir}, "x")
	wantRefused(t, err, "not the file that was pinned", "driver pin x")
	if err := os.Remove(drv); err != nil {
		t.Fatal(err)
	}
	_, err = PinnedDriver(home, []string{dir}, "x")
	wantRefused(t, err, "not found", "driver pin x")
}
