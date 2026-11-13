package vmhost

import (
	"archive/tar"
	"bytes"
	"crypto/sha512"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sengirunners/potluck/vmhost/shelfhelper"
)

// fakeShelfRun stands in front of fakeOCI for run: the helper container's
// script runs on the host with the volume a directory, $FAKE_OCI/vol/<source>,
// in place of its mount target. Every other verb goes to fakeOCI.
const fakeShelfRun = `#!/bin/sh
d=$FAKE_OCI
[ "$1" = run ] || exec "$FAKE_OCI_BASE" "$@"
echo "$*" >> "$d/calls"
shift; src=; script=
while [ $# -gt 0 ]; do
  case $1 in
  --mount) src=$(printf %s "$2" | sed -n 's/.*source=\([^,]*\).*/\1/p'); shift 2 ;;
  -c) script=$2; shift 3; break ;;
  *) shift ;;
  esac
done
mkdir -p "$d/vol/$src"
PATH=$FAKE_SHIMS:$PATH exec sh -c "$script" sh "$d/vol/$src"
`

// statShim is GNU stat -c's formats the shelf's scripts use, on BSD stat.
const statShim = `#!/bin/sh
[ "$1" = -c ] || exec /usr/bin/stat "$@"
f=$(printf %s "$2" | sed 's/%Y/%m/g; s/%s/%z/g; s/%n/%N/g'); shift 2
exec /usr/bin/stat -f "$f" "$@"
`

// newShelfOCI is fakeOCI with the shelf: the volume exists (as a
// directory), and run works as fakeShelfRun says.
func newShelfOCI(t *testing.T) (o ociCLI, dir, vol string) {
	t.Helper()
	o, dir = newFakeOCI(t, "none")
	o.stateDir = t.TempDir()
	shims := t.TempDir()
	if _, err := exec.LookPath("sha512sum"); err != nil {
		if err := os.WriteFile(filepath.Join(shims, "sha512sum"), []byte("#!/bin/sh\nexec shasum -a 512 \"$@\"\n"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if exec.Command("stat", "-c", "%Y", "/").Run() != nil { // BSD stat: the scripts use stat -c
		if err := os.WriteFile(filepath.Join(shims, "stat"), []byte(statShim), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	front := filepath.Join(t.TempDir(), "docker")
	if err := os.WriteFile(front, []byte(fakeShelfRun), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_OCI_BASE", o.bin)
	t.Setenv("FAKE_SHIMS", shims)
	o.bin = front
	vol = filepath.Join(dir, "vol", ShelfVolume)
	if err := os.MkdirAll(filepath.Join(vol, "tools"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cache-volume"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { // the publisher leaves 0444 files and a 0700 directory
		_ = filepath.WalkDir(vol, func(p string, d fs.DirEntry, err error) error {
			if err == nil && d.IsDir() {
				_ = os.Chmod(p, 0o755)
			}
			return nil
		})
	})
	return o, dir, vol
}

func sum512(b []byte) string {
	h := sha512.Sum512(b)
	return hex.EncodeToString(h[:])
}

func blobPath(hexsum string) string {
	return shelfBlobDir + "/" + hexsum[:2] + "/" + hexsum[2:4] + "/" + hexsum[4:]
}

// stage writes b into the state directory's staging at name.
func stage(t *testing.T, o ociCLI, name string, b []byte) {
	t.Helper()
	p := filepath.Join(shelfStaging(o.stateDir), filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// blobsOn lists the regular files under the volume's blob directory.
func blobsOn(t *testing.T, vol string) (out []string) {
	t.Helper()
	root := filepath.Join(vol, filepath.FromSlash(shelfBlobDir))
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			rel, _ := filepath.Rel(vol, p)
			out = append(out, filepath.ToSlash(rel))
		}
		return nil
	})
	return out
}

func tmpEntries(t *testing.T, vol string) []string {
	t.Helper()
	es, _ := os.ReadDir(filepath.Join(vol, filepath.FromSlash(shelfTmpDir)))
	var out []string
	for _, e := range es {
		out = append(out, e.Name())
	}
	return out
}

// A blob is published 0444 under its own sha512; one whose bytes are not
// its name is never published, whether the host or the container finds it;
// staging is emptied of what was taken; describe.shelf counts the result.
func TestShelfPublish(t *testing.T) {
	o, dir, vol := newShelfOCI(t)
	good, bad := []byte("a real tarball"), []byte("not what its name says")
	goodP, badP := blobPath(sum512(good)), blobPath(sum512([]byte("something else")))
	stage(t, o, goodP, good)
	stage(t, o, badP, bad)
	res, err := o.PublishShelf("zm-linux-ci")
	if err != nil {
		t.Fatal(err)
	}
	if res.Published != 1 || res.Dropped != 1 {
		t.Errorf("published %d dropped %d, want 1 and 1", res.Published, res.Dropped)
	}
	if got := blobsOn(t, vol); !slices.Equal(got, []string{goodP}) {
		t.Fatalf("the shelf holds %v, want only %s", got, goodP)
	}
	fi, err := os.Stat(filepath.Join(vol, filepath.FromSlash(goodP)))
	if err != nil || fi.Mode().Perm() != 0o444 {
		t.Errorf("published blob: %v %v, want mode 0444", fi.Mode(), err)
	}
	if b, _ := os.ReadFile(filepath.Join(vol, filepath.FromSlash(goodP))); !bytes.Equal(b, good) {
		t.Errorf("published bytes %q", b)
	}
	if es := tmpEntries(t, vol); len(es) != 0 {
		t.Errorf("npm/.tmp holds %v after a publish", es)
	}
	if fi, err := os.Stat(filepath.Join(vol, filepath.FromSlash(shelfTmpDir))); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("npm/.tmp: %v %v, want 0700", fi, err)
	}
	if es, _ := os.ReadDir(shelfStaging(o.stateDir)); len(es) != 0 {
		t.Errorf("staging still holds %v", es)
	}
	if b, _ := os.ReadFile(filepath.Join(vol, shelfHelperPath)); !bytes.Equal(b, shelfhelper.Script) || len(b) == 0 {
		t.Errorf("the helper on the shelf is %d bytes, not shelfhelper.Script", len(b))
	}
	if res.Stats.Files != 2 || res.Stats.Bytes <= 0 { // the blob and the helper
		t.Errorf("stats %+v", res.Stats)
	}
	// Published again: already there, by its name the same bytes.
	stage(t, o, goodP, good)
	if res, err := o.PublishShelf("zm-linux-ci"); err != nil || res.Published != 0 || res.Present != 1 {
		t.Errorf("a second publish of the same blob: %+v %v", res, err)
	}

	// The container checks again, whatever reached it: a spool that names
	// a blob by the wrong hash publishes nothing of it.
	var spool bytes.Buffer
	tw := tar.NewWriter(&spool)
	other := []byte("smuggled")
	wrong := blobPath(sum512([]byte("innocent")))
	for _, e := range []struct {
		name string
		b    []byte
	}{{wrong, other}, {"npm/stray", other}, {shelfEnd, nil}} {
		if err := tw.WriteHeader(shelfHeader(e.name, 0o444, int64(len(e.b)))); err != nil {
			t.Fatal(err)
		}
		_, _ = tw.Write(e.b)
	}
	_ = tw.Close()
	out, err := o.shelfRun("zm-linux-ci", true, nil, shelfPublishSh, &spool, shelfPublishFor)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "published 0 present 0 dropped 2") {
		t.Errorf("the container said %q", out)
	}
	if got := blobsOn(t, vol); !slices.Equal(got, []string{goodP}) {
		t.Errorf("after a wrong-hash spool the shelf holds %v", got)
	}

	// describe.shelf: path, bytes, files, no names; nothing without the volume.
	d := &drv{cfg: config{StateDir: o.stateDir, Image: "zm-linux-ci"}, vm: o}
	_ = os.Remove(shelfStatsFile(o.stateDir))
	s, ok := d.shelfDescribe()
	if b, _ := s["bytes"].(int64); !ok || s["path"] != ociCacheDir || s["files"] != int64(2) || b <= 0 || len(s) != 3 {
		t.Errorf("describe.shelf %v %v", s, ok)
	}
	if err := os.Remove(filepath.Join(dir, "cache-volume")); err != nil {
		t.Fatal(err)
	}
	if s, ok := d.shelfDescribe(); ok {
		t.Errorf("no volume, yet describe.shelf %v", s)
	}
	if _, err := o.PublishShelf("zm-linux-ci"); err == nil {
		t.Error("a publish with no volume worked")
	}
}

// A publish cut off anywhere (the host killed, the CLI gone, the stream
// short) leaves nothing visible: not a partial blob, not a whole one whose
// stream did not finish. A later publish clears what it left in npm/.tmp
// once it is stale, and leaves a fresh one (another publisher's) alone.
func TestShelfPublishKilled(t *testing.T) {
	o, _, vol := newShelfOCI(t)
	a, b := bytes.Repeat([]byte("a"), 4000), bytes.Repeat([]byte("b"), 4000)
	stage(t, o, blobPath(sum512(a)), a)
	stage(t, o, blobPath(sum512(b)), b)
	if err := os.MkdirAll(shelfDir(o.stateDir), 0o700); err != nil {
		t.Fatal(err)
	}
	f, err := os.CreateTemp(t.TempDir(), "spool")
	if err != nil {
		t.Fatal(err)
	}
	pr, pw := io.Pipe()
	go func() {
		tw := tar.NewWriter(pw)
		pw.CloseWithError(errors.Join(tw.AddFS(os.DirFS(shelfStaging(o.stateDir))), tw.Close()))
	}()
	if _, _, err := shelfSpool(pr, f); err != nil {
		t.Fatal(err)
	}
	full, _ := os.ReadFile(f.Name())
	// Cut mid-blob, and exactly after the first whole blob (512-byte header,
	// 4000 bytes padded to 4096): neither carries the end marker.
	for _, n := range []int{len(full) / 2, 512 + 4096, len(full) - 1024 - 512} {
		if _, err := o.shelfRun("zm-linux-ci", true, nil, shelfPublishSh, bytes.NewReader(full[:n]), shelfPublishFor); err == nil {
			t.Errorf("a spool cut at %d of %d bytes was published", n, len(full))
		}
		if got := blobsOn(t, vol); len(got) != 0 {
			t.Fatalf("a spool cut at %d of %d bytes left %v visible", n, len(full), got)
		}
	}
	left := tmpEntries(t, vol)
	if len(left) != 3 {
		t.Fatalf("the cut publishes left %v in npm/.tmp, want 3 directories", left)
	}
	old := time.Now().Add(-time.Duration(shelfStaleMin+5) * time.Minute)
	young := time.Now().Add(-time.Duration(shelfStaleMin-5) * time.Minute) // a long publish still running
	for i, e := range left {
		at := old
		if i == 2 {
			at = young
		}
		if err := os.Chtimes(filepath.Join(vol, filepath.FromSlash(shelfTmpDir), e), at, at); err != nil {
			t.Fatal(err)
		}
	}
	res, err := o.PublishShelf("zm-linux-ci")
	if err != nil || res.Published != 2 {
		t.Fatalf("the next publish: %+v %v", res, err)
	}
	if es := tmpEntries(t, vol); !slices.Equal(es, left[2:]) {
		t.Errorf("npm/.tmp holds %v, want only the one under %d minutes old, %v", es, shelfStaleMin, left[2:])
	}
}

// The staging tar is refused whole, nothing sent, on a link, a name outside
// the tree, a device or fifo, or a file at any path but a blob's (sparse
// entries: internal/tarsafe's test).
func TestShelfSpoolRefuses(t *testing.T) {
	blob := []byte("x")
	ok := blobPath(sum512(blob))
	cases := map[string]tar.Header{
		"symlink":     {Typeflag: tar.TypeSymlink, Name: ok, Linkname: "/etc/passwd"},
		"hard link":   {Typeflag: tar.TypeLink, Name: ok, Linkname: "npm/other"},
		"dotdot":      {Typeflag: tar.TypeReg, Name: "../" + ok, Size: 1},
		"inner":       {Typeflag: tar.TypeReg, Name: shelfBlobDir + "/../../../" + ok, Size: 1},
		"absolute":    {Typeflag: tar.TypeReg, Name: "/" + ok, Size: 1},
		"fifo":        {Typeflag: tar.TypeFifo, Name: ok},
		"device":      {Typeflag: tar.TypeChar, Name: ok},
		"not a blob":  {Typeflag: tar.TypeReg, Name: "goproxy/x/@v/list", Size: 1},
		"the helper":  {Typeflag: tar.TypeReg, Name: shelfHelperPath, Size: 1},
		"end marker":  {Typeflag: tar.TypeReg, Name: shelfEnd, Size: 1},
		"short hash":  {Typeflag: tar.TypeReg, Name: ok[:len(ok)-1], Size: 1},
		"upper hex":   {Typeflag: tar.TypeReg, Name: strings.ToUpper(ok), Size: 1},
		"one level":   {Typeflag: tar.TypeReg, Name: shelfBlobDir + "/" + sum512(blob), Size: 1},
		"blob in tmp": {Typeflag: tar.TypeReg, Name: shelfTmpDir + "/" + sum512(blob), Size: 1},
	}
	for what, h := range cases {
		var in bytes.Buffer
		tw := tar.NewWriter(&in)
		good := tar.Header{Typeflag: tar.TypeReg, Name: ok, Size: 1, Mode: 0o644}
		_ = tw.WriteHeader(&good)
		_, _ = tw.Write(blob)
		if h.Mode == 0 {
			h.Mode = 0o644
		}
		if err := tw.WriteHeader(&h); err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		if h.Size > 0 {
			_, _ = tw.Write(bytes.Repeat([]byte("y"), int(h.Size)))
		}
		_ = tw.Close()
		f, err := os.CreateTemp(t.TempDir(), "spool")
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := shelfSpool(&in, f); !errors.Is(err, errShelfRefused) {
			t.Errorf("%s: %v, want a refusal", what, err)
		}
		f.Close()
	}

	// From the staging directory itself: a symlink there refuses the
	// publish before any container runs, and staging is left as it was.
	o, dir, vol := newShelfOCI(t)
	stage(t, o, ok, blob)
	link := filepath.Join(shelfStaging(o.stateDir), filepath.FromSlash(blobPath(sum512([]byte("y")))))
	if err := os.MkdirAll(filepath.Dir(link), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/passwd", link); err != nil {
		t.Fatal(err)
	}
	if _, err := o.PublishShelf("zm-linux-ci"); !errors.Is(err, errShelfRefused) {
		t.Fatalf("a symlink in staging: %v", err)
	}
	if got := blobsOn(t, vol); len(got) != 0 {
		t.Errorf("a refused publish put %v on the shelf", got)
	}
	if _, err := os.Stat(filepath.Join(shelfStaging(o.stateDir), filepath.FromSlash(ok))); err != nil {
		t.Errorf("a refused publish took staging's blob: %v", err)
	}
	calls, _ := os.ReadFile(filepath.Join(dir, "calls"))
	if strings.Contains(string(calls), "run ") {
		t.Errorf("a refused publish ran a container:\n%s", calls)
	}
	// A symlink inside the volume where the publisher makes a directory
	// (only the owner's fill script could put one there) stops the publish
	// before anything is written through it.
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	for _, at := range []string{"npm", "npm/content-v2", shelfBlobDir, shelfTmpDir, "bin", shelfBlobDir + "/" + ok[len(shelfBlobDir)+1:len(shelfBlobDir)+3],
		filepath.ToSlash(filepath.Dir(ok))} {
		_ = os.RemoveAll(filepath.Join(vol, "npm"))
		_ = os.RemoveAll(filepath.Join(vol, "bin"))
		lp := filepath.Join(vol, filepath.FromSlash(at))
		if err := os.MkdirAll(filepath.Dir(lp), 0o755); err != nil {
			t.Fatal(err)
		}
		away := t.TempDir()
		if err := os.Symlink(away, lp); err != nil {
			t.Fatal(err)
		}
		if _, err := o.PublishShelf("zm-linux-ci"); err == nil || !strings.Contains(err.Error(), "symlink") {
			t.Errorf("a symlink at %s: %v", at, err)
		}
		if es, _ := os.ReadDir(away); len(es) != 0 {
			t.Errorf("a symlink at %s: the publisher wrote through it: %v", at, es)
		}
		_ = os.Remove(lp)
	}
	// A fifo is not a file AddFS takes: refused too.
	if err := syscall.Mkfifo(link, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := o.PublishShelf("zm-linux-ci"); !errors.Is(err, errShelfRefused) {
		t.Errorf("a fifo in staging: %v", err)
	}
}

// The publisher is the only writer: its container has no network and the
// shelf rw; a box's mount of the shelf stays exactly as it was, read-only.
// The helper, when there is one, is installed 0555.
func TestShelfMounts(t *testing.T) {
	o, dir, vol := newShelfOCI(t)
	old := shelfHelper
	shelfHelper = []byte("#!/bin/sh\necho helper\n")
	t.Cleanup(func() { shelfHelper = old })
	blob := []byte("tarball")
	stage(t, o, blobPath(sum512(blob)), blob)
	if _, err := o.PublishShelf("zm-linux-ci"); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(vol, shelfHelperPath))
	if err != nil || fi.Mode().Perm() != 0o555 {
		t.Errorf("helper: %v %v, want 0555", fi, err)
	}
	if b, _ := os.ReadFile(filepath.Join(vol, shelfHelperPath)); !bytes.Equal(b, shelfHelper) {
		t.Errorf("helper bytes %q", b)
	}
	if err := os.WriteFile(filepath.Join(dir, "images", "zm-linux-ci"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := o.Clone("zm-linux-ci", "box1", 0, 0); err != nil {
		t.Fatal(err)
	}
	args, _ := os.ReadFile(filepath.Join(dir, "c", "box1", "args"))
	const ro = "type=volume,source=potluck-cache,target=/opt/potluck-cache,readonly,volume-nocopy"
	if !slices.Contains(strings.Split(strings.TrimSpace(string(args)), "\n"), ro) {
		t.Errorf("the box's mount changed:\n%s", args)
	}
	calls, _ := os.ReadFile(filepath.Join(dir, "calls"))
	rw := 0
	for _, l := range strings.Split(string(calls), "\n") {
		if !strings.Contains(l, "source="+ShelfVolume) {
			continue
		}
		if strings.Contains(l, ",readonly") {
			continue
		}
		rw++
		if !strings.HasPrefix(l, "run ") || !strings.Contains(l, "--network none") || !strings.Contains(l, "--read-only") || !strings.Contains(l, "--cap-drop ALL") {
			t.Errorf("a writable mount of the shelf outside the publisher's sealed container: %s", l)
		}
	}
	if rw != 1 {
		t.Errorf("%d writable mounts of the shelf in one publish, want 1:\n%s", rw, calls)
	}
}

// The shelf's keys: defaults, checks, and the round trip a supervisor makes.
func TestShelfConfig(t *testing.T) {
	base := func(kv ...string) map[string]string {
		m := map[string]string{"target": "lx", "kind": "box", "vm": "oci", "images": "zm/linux-ci@sha256:" + strings.Repeat("a", 64), "remote": "https://github.com/o/r.git",
			"state_dir": t.TempDir(), "oci_runtime": "runsc", "size": "s", "cpu": "2", "memory": "4096"}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return m
	}
	c, err := parseConfig(base())
	if err != nil {
		t.Fatal(err)
	}
	if c.Shelf != (shelfCfg{MaxGB: 0, KeepS: 30 * 24 * 3600, Fill: "seed", FillMbit: 16, FillDelayS: 300}) {
		t.Errorf("defaults %+v", c.Shelf)
	}
	c, err = parseConfig(base("shelf_max_gb", "40", "shelf_keep_s", "86400", "shelf_fill", "boxes", "shelf_fill_mbit", "50", "shelf_fill_delay_s", "0"))
	if err != nil {
		t.Fatal(err)
	}
	want := shelfCfg{MaxGB: 40, KeepS: 86400, Fill: "boxes", FillMbit: 50, FillDelayS: 0}
	if c.Shelf != want {
		t.Errorf("set %+v", c.Shelf)
	}
	if again, err := parseConfig((&drv{cfg: c}).rawConfig()); err != nil || again.Shelf != want {
		t.Errorf("rawConfig round trip: %+v %v", again.Shelf, err)
	}
	for _, kv := range [][]string{{"shelf_fill", "all"}, {"shelf_keep_s", "59"}, {"shelf_fill_mbit", "0"}, {"shelf_max_gb", "-1"}, {"shelf_fill_delay_s", "x"},
		{"vm", "tart", "shelf_max_gb", "1"}} {
		if _, err := parseConfig(base(kv...)); err == nil || !strings.Contains(err.Error(), "shelf_") {
			t.Errorf("%v: %v", kv, err)
		}
	}
}

// Capability shelf is listed exactly when describe.shelf is: when the cache
// volume exists, so every box mounts it.
func TestDescribeListsShelf(t *testing.T) {
	o, dir := newFakeOCI(t, "mine")
	img := "zm/box@sha256:" + strings.Repeat("ab", 32)
	_ = os.MkdirAll(filepath.Join(dir, "images", "zm"), 0o700)
	if err := os.WriteFile(filepath.Join(dir, "images", img), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	m := map[string]string{"target": "lx", "kind": "box", "vm": "oci", "oci_cli": o.bin, "oci_runtime": "runsc", "oci_network": "mine",
		"state_dir": stateDir(t), "size": "c1m2", "cpu": "1", "memory": "512", "images": img, "boot_timeout_s": "20"}
	for _, vol := range []bool{false, true} {
		if vol {
			if err := os.WriteFile(filepath.Join(dir, "cache-volume"), nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		d, err := call(t, m, "describe", "", nil)
		if err != nil {
			t.Fatal(err)
		}
		listed := slices.Contains(anyStrings(d["capabilities"]), "shelf")
		_, given := d["shelf"]
		if listed != vol || given != vol {
			t.Errorf("volume %v: capability shelf %v, describe.shelf %v (%v)", vol, listed, given, d["capabilities"])
		}
	}
}
