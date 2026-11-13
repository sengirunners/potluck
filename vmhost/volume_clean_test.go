package vmhost

import (
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// special lists dir's setuid and setgid entries and its device nodes, FIFOs
// and sockets, without following links.
func special(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(dir, func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		fi, err := os.Lstat(p)
		if err != nil {
			return err
		}
		if fi.Mode()&(fs.ModeSetuid|fs.ModeSetgid|fs.ModeDevice|fs.ModeCharDevice|fs.ModeNamedPipe|fs.ModeSocket) != 0 {
			r, _ := filepath.Rel(dir, p)
			out = append(out, r+" "+fi.Mode().String())
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// A seal, with keep or without, clears every setuid and setgid bit and
// deletes every FIFO and socket the builder left (device nodes too, which a
// test cannot make), keeps what the files hold, and never chmods through a
// link: a setuid file outside the volume that a link in it points at keeps
// its bit.
func TestSealStripsSetuidAndSpecialFiles(t *testing.T) {
	outside := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(outside, []byte("v"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(outside, 0o755|fs.ModeSetuid); err != nil {
		t.Fatal(err)
	}
	for _, keep := range [][]string{nil, {"node_modules"}} {
		b := newSealBox(t)
		h := acquireVol(t, b.m, volArgs("k", "o", build("lay")))
		fill := `set -e; cd "$HOME/dep"
mkdir -p node_modules/.bin node_modules/d
echo u > node_modules/.bin/u; chmod 4755 node_modules/.bin/u
echo g > node_modules/g; chmod 2755 node_modules/g; chmod 2775 node_modules/d
mkfifo node_modules/fifo
perl -MIO::Socket::UNIX -e 'IO::Socket::UNIX->new(Type => SOCK_STREAM, Local => $ARGV[0], Listen => 1) or die' node_modules/sock
ln -s ` + outside + ` node_modules/.bin/v
`
		if c, out := sh(t, b.m, h, fill); c != 0 {
			t.Fatalf("filling the build volume: %v %s", c, out)
		}
		a := map[string]any{"seal": true}
		if keep != nil {
			a["keep"] = keep
		}
		if res, err := call(t, b.m, "release", h, a); err != nil || res["released"] != true {
			t.Fatalf("release %v: %v %v", a, res, err)
		}
		if v, ok := b.st.loadVol(volID("t", "o", "lay")); !ok || !v.Sealed {
			t.Fatalf("keep %v: not sealed: %+v %v", keep, v, ok)
		}
		dir := b.dir("o", "lay")
		if got := special(t, dir); len(got) != 0 {
			t.Errorf("keep %v: the sealed volume still holds %v", keep, got)
		}
		want := []string{"node_modules", "node_modules/.bin", "node_modules/.bin/u", "node_modules/.bin/v@", "node_modules/d", "node_modules/g"}
		if got := tree(t, dir); !slices.Equal(got, want) {
			t.Errorf("keep %v: the sealed volume holds %v, want %v", keep, got, want)
		}
		if f, _ := os.ReadFile(filepath.Join(dir, "node_modules/.bin/u")); string(f) != "u\n" {
			t.Errorf("keep %v: the setuid file now holds %q", keep, f)
		}
		if fi, err := os.Stat(outside); err != nil || fi.Mode()&fs.ModeSetuid == 0 {
			t.Errorf("keep %v: the cleanup changed the file a link points at: %v %v", keep, fi.Mode(), err)
		}
	}
}

// The cleanup refuses a volume it cannot clean: one it cannot walk (an
// unreadable directory) fails, so the seal deletes it instead.
func TestSealCleanFailsOnWhatItCannotWalk(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads every directory")
	}
	dir := t.TempDir()
	locked := filepath.Join(dir, "node_modules", "locked")
	if err := os.MkdirAll(locked, 0o700); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(locked, "u"), nil, 0o755)
	_ = os.Chmod(filepath.Join(locked, "u"), 0o755|fs.ModeSetuid)
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o700) })
	out, err := exec.Command("sh", append([]string{"-c", sealCleanScript, "sh", dir}, sealCleanArgs(nil)...)...).CombinedOutput()
	if err == nil {
		t.Fatalf("the cleanup passed a volume it could not walk: %s", out)
	}
}

// The cleanup runs the system's find, chmod and rm, never one the build left
// in the volume where the image's PATH looks (./node_modules/.bin).
func TestSealCleanIgnoresPlantedTools(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "node_modules", ".bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, tool := range []string{"find", "chmod", "rm"} {
		_ = os.WriteFile(filepath.Join(bin, tool), []byte("#!/bin/sh\nexit 0\n"), 0o755)
	}
	u := filepath.Join(dir, "node_modules", "u")
	_ = os.WriteFile(u, []byte("u"), 0o755)
	if err := os.Chmod(u, 0o755|fs.ModeSetuid); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", append([]string{"-c", sealCleanScript, "sh", dir}, sealCleanArgs(nil)...)...)
	cmd.Env = append(os.Environ(), "PATH=./node_modules/.bin:"+os.Getenv("PATH"))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("cleanup: %v %s", err, out)
	}
	if got := special(t, dir); len(got) != 0 {
		t.Errorf("a planted find was run: the volume still holds %v", got)
	}
}

// ociCheckViews passes only when each view's top mount in the table is
// nosuid and nodev (and ro for an ro view).
func TestOCICheckViewsScript(t *testing.T) {
	const good = "36 25 0:32 / /home/u/a ro,nosuid,nodev,relatime - none none rw\n" +
		"37 25 0:33 / /home/u/b rw,nosuid,nodev,relatime - overlay overlay rw\n"
	for _, c := range []struct {
		table string
		views []string
		ok    bool
	}{
		{good, []string{"ro", "/home/u/a", "cow", "/home/u/b"}, true},
		{good, []string{"cow", "/home/u/a"}, true},
		{good, []string{"ro", "/home/u/b"}, false},                                                                // a rw mount for an ro view
		{good, []string{"cow", "/home/u/c"}, false},                                                               // not mounted
		{good + "38 37 0:34 / /home/u/b rw,relatime - overlay overlay rw\n", []string{"cow", "/home/u/b"}, false}, // a mount on top without them
		{"36 25 0:32 / /home/u/a ro,nodev - none none rw\n", []string{"ro", "/home/u/a"}, false},
		{"36 25 0:32 / /home/u/a ro,nosuid - none none rw\n", []string{"ro", "/home/u/a"}, false},
		{"36 25 0:32 / /home/u/a ro,nosuidx,nodev - none none rw\n", []string{"ro", "/home/u/a"}, false},
	} {
		tab := filepath.Join(t.TempDir(), "mountinfo")
		if err := os.WriteFile(tab, []byte(c.table), 0o600); err != nil {
			t.Fatal(err)
		}
		out, err := exec.Command("sh", append([]string{"-c", ociCheckViews, "sh", tab}, c.views...)...).CombinedOutput()
		if (err == nil) != c.ok {
			t.Errorf("views %v over\n%s: passed %v, want %v (%s)", c.views, c.table, err == nil, c.ok, strings.TrimSpace(string(out)))
		}
	}
}

// OCI: an ro view is a volume of the box's own that binds the named
// volume's directory ro,nosuid,nodev, mounted readonly, and a cow copy's
// overlay is nosuid,nodev; both are labelled as layered on the volume (so it is in use)
// and listed for Destroy. Every runtime verifyEngine passes for volumes is
// gVisor's, so no guest check runs (TestOCICheckViews covers the others).
func TestOCIViewsAreNosuidNodev(t *testing.T) {
	o, dir := newFakeOCI(t, "none")
	o.stateDir = t.TempDir()
	check := `exec) for arg; do printf '%s\n' "$arg"; done >> "$d/execargs"; exit 0;;`
	if err := os.WriteFile(o.bin, []byte(fakeOCICase(t, fakeOCI, check)), 0o700); err != nil {
		t.Fatal(err)
	}
	img := filepath.Join(dir, "images")
	_ = os.WriteFile(filepath.Join(img, "ok-img"), nil, 0o600)
	_ = os.WriteFile(filepath.Join(img, "ok-img.inspect"), []byte("agent\nHOME=/home/agent\n"), 0o600)
	ms := []mount{{Vol: "lay", Path: "a", Mode: volRO, Sealed: true}, {Vol: "lay", Path: "b", Mode: volCOW, Sealed: true}, {Vol: "w", Path: "c", Mode: volRW}}
	if err := o.CloneVolumes("ok-img", "zmrt-x-r1", 1, 512, ms); err != nil {
		t.Fatal(err)
	}
	calls, _ := os.ReadFile(filepath.Join(dir, "calls"))
	for _, w := range []string{
		"volume create --label " + ociVolLabel + " --label " + ociLowerLabelNew + "=lay --driver local --opt type=none --opt device=/var/lib/fake/lay/_data --opt o=bind,ro,nosuid,nodev zmrt-x-r1-ro0",
		"volume create --label " + ociVolLabel + " --label " + ociLowerLabelNew + "=lay --driver local --opt type=overlay --opt device=overlay --opt o=lowerdir=/var/lib/fake/lay/_data,upperdir=/var/lib/fake/zmrt-x-r1-cow1u/_data,workdir=/var/lib/fake/zmrt-x-r1-cow1w/_data,nosuid,nodev zmrt-x-r1-cow1",
	} {
		if !strings.Contains(string(calls), w) {
			t.Errorf("the CLI was never asked %q:\n%s", w, calls)
		}
	}
	args, _ := os.ReadFile(filepath.Join(dir, "c", "zmrt-x-r1", "args"))
	for _, w := range []string{"type=volume,source=zmrt-x-r1-ro0,target=/home/agent/a,readonly,volume-nocopy\n", "type=volume,source=zmrt-x-r1-cow1,target=/home/agent/b,volume-nocopy\n", "type=volume,source=w,target=/home/agent/c,volume-nocopy\n"} {
		if !strings.Contains(string(args), w) {
			t.Errorf("the box does not mount %q:\n%s", w, args)
		}
	}
	if strings.Contains(string(args), "source=lay,") {
		t.Errorf("the box mounts the named volume itself:\n%s", args)
	}
	if _, vols, err := o.readCOW("zmrt-x-r1"); err != nil || !slices.Contains(vols, "zmrt-x-r1-ro0") {
		t.Errorf("the ro view is not listed for Destroy: %v %v", vols, err)
	}
	if err := o.OwnMounts("ok-img", "zmrt-x-r1", ms); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "execargs")); strings.Contains(string(got), ociCheckViews) {
		t.Errorf("gVisor: the views check ran: %s", got)
	}
}

// Off gVisor, OwnMounts first checks the guest's mount table for the ro and
// cow views (the script: TestOCICheckViewsScript), and a box whose views
// lack nosuid,nodev fails.
func TestOCICheckViews(t *testing.T) {
	o, dir := newFakeOCI(t, "none")
	o.runtime = "crun"
	check := `exec) for arg; do printf '%s\n' "$arg"; done >> "$d/execargs"; [ ! -e "$d/views-fail" ]; exit;;`
	if err := os.WriteFile(o.bin, []byte(fakeOCICase(t, fakeOCI, check)), 0o700); err != nil {
		t.Fatal(err)
	}
	img := filepath.Join(dir, "images")
	_ = os.WriteFile(filepath.Join(img, "ok-img"), nil, 0o600)
	_ = os.WriteFile(filepath.Join(img, "ok-img.inspect"), []byte("root\nHOME=/root\n"), 0o600)
	ms := []mount{{Vol: "lay", Path: "a", Mode: volRO, Sealed: true}, {Vol: "lay", Path: "b", Mode: volCOW, Sealed: true}, {Vol: "w", Path: "c", Mode: volRW}}
	if err := o.OwnMounts("ok-img", "vm1", ms); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "execargs"))
	if want := "exec\n-u\n0\nvm1\n/bin/sh\n-c\n" + ociCheckViews + "\nsh\n/proc/self/mountinfo\nro\n/root/a\ncow\n/root/b\n"; !strings.HasPrefix(string(got), want) {
		t.Errorf("the views check ran as\n%s\nwant\n%s", got, want)
	}
	_ = os.WriteFile(filepath.Join(dir, "views-fail"), nil, 0o600)
	if err := o.OwnMounts("ok-img", "vm1", ms); err == nil || !strings.Contains(err.Error(), "nosuid,nodev") {
		t.Errorf("views the guest does not see nosuid,nodev: %v, want the box failed", err)
	}
	_ = os.Remove(filepath.Join(dir, "views-fail"))
	_ = os.Remove(filepath.Join(dir, "execargs"))
	if err := o.OwnMounts("ok-img", "vm1", ms[2:]); err != nil {
		t.Errorf("no views: %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "execargs")); strings.Contains(string(got), ociCheckViews) {
		t.Errorf("no views, yet the views check ran: %s", got)
	}
}
