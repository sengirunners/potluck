package vmhost

import (
	"context"
	"crypto/sha512"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// putBlob puts b on the shelf as the publisher leaves it (0444, named by its
// sha512) with mtime at, and answers its hex.
func putBlob(t *testing.T, vol string, b []byte, at time.Time) string {
	t.Helper()
	hx := sum512(b)
	p := filepath.Join(vol, filepath.FromSlash(blobPath(hx)))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, b, 0o444); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, at, at); err != nil {
		t.Fatal(err)
	}
	return hx
}

// putOther writes a file that is not an npm blob, with mtime at.
func putOther(t *testing.T, vol, rel string, b []byte, at time.Time) string {
	t.Helper()
	p := filepath.Join(vol, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, b, 0o444); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, at, at); err != nil {
		t.Fatal(err)
	}
	return p
}

func onShelfVol(vol, hx string) bool {
	_, err := os.Lstat(filepath.Join(vol, filepath.FromSlash(blobPath(hx))))
	return err == nil
}

func mtimeOf(t *testing.T, vol, hx string) time.Time {
	t.Helper()
	fi, err := os.Stat(filepath.Join(vol, filepath.FromSlash(blobPath(hx))))
	if err != nil {
		t.Fatal(err)
	}
	return fi.ModTime()
}

func evictCfg(o ociCLI, keepS int) config {
	c := shelfFillCfg(o, 1000)
	c.Shelf.KeepS = keepS
	return c
}

func smallGiB(t *testing.T, n int64) {
	old := shelfGiB
	shelfGiB = n
	t.Cleanup(func() { shelfGiB = old })
}

// A blob or a Go module version older than shelf_keep_s goes, a version
// whole; newer ones stay; nothing else is deleted, however old (tools/,
// goproxy/'s list files, bin/).
func TestShelfEvictKeep(t *testing.T) {
	o, _, vol := newShelfOCI(t)
	now := time.Now()
	old := putBlob(t, vol, []byte("old tarball"), now.Add(-2*time.Hour))
	fresh := putBlob(t, vol, []byte("fresh tarball"), now.Add(-time.Minute))
	ancient := now.Add(-400 * 24 * time.Hour)
	others := []string{
		putOther(t, vol, "tools/go/1.25.0/amd64", []byte("go"), ancient),
		putOther(t, vol, "tools/go/1.25.0/amd64.complete", nil, ancient),
		putOther(t, vol, "goproxy/example.com/m/@v/list", []byte("v1.0.0\n"), ancient),
		putOther(t, vol, "goproxy/example.com/m/@v/v1.0.0.lock", nil, ancient),
		putOther(t, vol, shelfHelperPath, []byte("#!/bin/sh"), ancient),
		putOther(t, vol, shelfBlobDir+"/aa/bb/not-a-hash", []byte("x"), ancient),
	}
	var oldGo, freshGo []string
	for _, e := range []string{"zip", "ziphash", "mod", "info"} {
		oldGo = append(oldGo, putOther(t, vol, "goproxy/example.com/m/@v/v1.0.0."+e, []byte(e), ancient))
		freshGo = append(freshGo, putOther(t, vol, "goproxy/example.com/m/@v/v1.1.0."+e, []byte(e), now.Add(-time.Minute)))
	}
	res, err := evictShelf(evictCfg(o, 3600), o)
	if err != nil {
		t.Fatal(err)
	}
	if res.Deleted != 2 || onShelfVol(vol, old) || !onShelfVol(vol, fresh) {
		t.Fatalf("%+v: old there %v, fresh there %v", res, onShelfVol(vol, old), onShelfVol(vol, fresh))
	}
	for _, p := range oldGo {
		if _, err := os.Lstat(p); err == nil {
			t.Errorf("an old module version's %s stayed", filepath.Base(p))
		}
	}
	for _, p := range freshGo {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("a fresh module version's %s went", filepath.Base(p))
		}
	}
	for _, p := range others {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("eviction removed %s", p)
		}
	}
	if want := int64(len("fresh tarball") + len("go") + len("v1.0.0\n") + len("#!/bin/sh") + 1 + len("zipziphashmodinfo")); res.Bytes != want {
		t.Errorf("bytes after %d, want %d", res.Bytes, want)
	}
}

// Past shelf_max_gb the oldest blobs go first, until the shelf (other files
// counted) is under it.
func TestShelfEvictMax(t *testing.T) {
	o, _, vol := newShelfOCI(t)
	smallGiB(t, 30) // shelf_max_gb 1 is 30 bytes here
	now := time.Now()
	putOther(t, vol, "tools/t", make([]byte, 10), now.Add(-time.Hour))
	a := putBlob(t, vol, []byte("aaaaaaaaaa"), now.Add(-3*time.Minute)) // oldest
	b := putBlob(t, vol, []byte("bbbbbbbbbb"), now.Add(-2*time.Minute))
	c := putBlob(t, vol, []byte("cccccccccc"), now.Add(-time.Minute))
	res, err := evictShelf(evictCfg(o, shelfKeepDefault), o)
	if err != nil {
		t.Fatal(err)
	}
	if res.Deleted != 1 || res.Bytes != 30 || onShelfVol(vol, a) || !onShelfVol(vol, b) || !onShelfVol(vol, c) {
		t.Fatalf("%+v: a %v b %v c %v", res, onShelfVol(vol, a), onShelfVol(vol, b), onShelfVol(vol, c))
	}
	if _, err := os.Stat(filepath.Join(vol, "tools", "t")); err != nil {
		t.Error("tools/ was evicted")
	}
	// Under the cap, nothing goes.
	if res, err := evictShelf(evictCfg(o, shelfKeepDefault), o); err != nil || res.Deleted != 0 {
		t.Errorf("second eviction %+v %v", res, err)
	}
}

// A blob whose mtime moved after the listing (a touch, another binding's
// publish) is left, and the container deletes only real blob paths: a
// listed line naming anything else, or a blob behind a symlinked
// directory, deletes nothing.
func TestShelfDeleteGuards(t *testing.T) {
	o, _, vol := newShelfOCI(t)
	now := time.Now()
	hx := putBlob(t, vol, []byte("tarball"), now.Add(-time.Hour))
	gone, err := o.DeleteShelf("img", []shelfBlobAt{{Hex: hx, Mtime: now.Add(-2 * time.Hour).Unix()}})
	if err != nil || gone[hx] || !onShelfVol(vol, hx) {
		t.Fatalf("a moved blob was deleted: %v %v", gone, err)
	}
	if _, err := o.DeleteShelf("img", []shelfBlobAt{{Hex: "../../tools", Mtime: 0}}); err == nil {
		t.Error("a path that is not a hash reached the container")
	}
	// Straight to the script: lines the host would never send.
	tool := putOther(t, vol, "tools/x/y", []byte("tool"), now)
	outside := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(outside, []byte("v"), 0o644); err != nil {
		t.Fatal(err)
	}
	fake := strings.Repeat("e", 124)
	if err := os.MkdirAll(filepath.Join(vol, shelfBlobDir, "ee"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Dir(outside), filepath.Join(vol, shelfBlobDir, "ee", "ee")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(filepath.Dir(outside), fake)); err != nil {
		t.Fatal(err)
	}
	fiT, _ := os.Stat(tool)
	in := strings.Join([]string{
		itoa(fiT.ModTime().Unix()) + " ../../../tools/x/y",
		itoa(fiT.ModTime().Unix()) + " ee/ee/" + fake,
		"0 " + hx[:2] + "/" + hx[2:4] + "/" + hx[4:] + "/../..",
	}, "\n") + "\n"
	out, err := o.shelfRun("img", true, nil, shelfDeleteSh, strings.NewReader(in), shelfMeasureFor)
	if err != nil || strings.Contains(out, "gone") {
		t.Fatalf("out %q err %v", out, err)
	}
	if _, err := os.Stat(tool); err != nil {
		t.Error("tools/x/y deleted")
	}
	if _, err := os.Stat(outside); err != nil {
		t.Error("a file behind a symlinked directory was deleted")
	}
	// A symlinked npm/ refuses the whole run.
	o2, _, vol2 := newShelfOCI(t)
	if err := os.Symlink(t.TempDir(), filepath.Join(vol2, "npm")); err != nil {
		t.Fatal(err)
	}
	if _, err := o2.shelfRun("img", true, nil, shelfDeleteSh, strings.NewReader(""), shelfMeasureFor); err == nil {
		t.Error("a symlinked npm/ was not refused")
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// A want naming a blob already on the shelf moves its mtime forward, at most
// once a day, so eviction by keep time spares what boxes still ask for.
func TestShelfTouch(t *testing.T) {
	o, _, vol := newShelfOCI(t)
	r := newFakeRegistry(t)
	useRegistry(t, r)
	now := time.Now()
	stale, recent := []byte("asked for, two days old"), []byte("asked for, an hour old")
	sh := putBlob(t, vol, stale, now.Add(-48*time.Hour))
	rh := putBlob(t, vol, recent, now.Add(-time.Hour))
	cfg := evictCfg(o, 72*3600)
	res, err := shelfFill(context.Background(), cfg, o, []shelfWant{want(t, "s", "1.0.0", stale), want(t, "r", "1.0.0", recent)})
	if err != nil || res.Present != 2 || res.Touched != 1 || len(r.hits) != 0 {
		t.Fatalf("%+v %v hits %v", res, err, r.hits)
	}
	if m := mtimeOf(t, vol, sh); time.Since(m) > time.Minute {
		t.Errorf("the two-day-old blob was not touched: %v", m)
	}
	if m := mtimeOf(t, vol, rh); now.Sub(m) < 59*time.Minute {
		t.Errorf("the hour-old blob was touched again: %v", m)
	}
	// Another day on, the blob nobody asked for goes and the touched one stays.
	nobody := putBlob(t, vol, []byte("nobody asked"), now.Add(-48*time.Hour))
	cfg.Shelf.KeepS = 24 * 3600
	if _, err := evictShelf(cfg, o); err != nil {
		t.Fatal(err)
	}
	if onShelfVol(vol, nobody) || !onShelfVol(vol, sh) {
		t.Errorf("after eviction: unasked there %v, touched there %v", onShelfVol(vol, nobody), onShelfVol(vol, sh))
	}
	if d := shelfTouchAge(600); d != 5*time.Minute {
		t.Errorf("touch age for a 10-minute keep time: %v", d)
	}
}

// lowDisk is the shelf's layer on a host with free GiB free.
type lowDisk struct {
	ociCLI
	free float64
}

func (l lowDisk) FreeGiB() float64 { return l.free }

// Below persist_min_free_gb a fill fetches nothing.
func TestShelfFillFloor(t *testing.T) {
	o, _, vol := newShelfOCI(t)
	r := newFakeRegistry(t)
	useRegistry(t, r)
	a := []byte("tarball a")
	r.publish("a", "1.0.0", sri512(a), "", a)
	cfg := evictCfg(o, shelfKeepDefault)
	cfg.PersistMinFreeGB = 5
	res, err := shelfFill(context.Background(), cfg, lowDisk{o, 4.5}, []shelfWant{want(t, "a", "1.0.0", a)})
	if err != nil || res.Fetched != 0 || res.Dropped[dropFloor] != 1 || len(r.hits) != 0 || len(blobsOn(t, vol)) != 0 {
		t.Fatalf("%+v %v hits %v", res, err, r.hits)
	}
	res, err = shelfFill(context.Background(), cfg, lowDisk{o, 6}, []shelfWant{want(t, "a", "1.0.0", a)})
	if err != nil || res.Fetched != 1 || len(blobsOn(t, vol)) != 1 {
		t.Fatalf("above the floor: %+v %v", res, err)
	}
}

// A shelf at shelf_max_gb (its other files counted) takes no new tarball,
// and pushes no kept blob out for one.
func TestShelfFillFull(t *testing.T) {
	o, _, vol := newShelfOCI(t)
	r := newFakeRegistry(t)
	useRegistry(t, r)
	smallGiB(t, 30)
	kept := putBlob(t, vol, []byte("kept tarball, 20 b"), time.Now().Add(-time.Hour))
	putOther(t, vol, "tools/t", make([]byte, 10), time.Now())
	a := []byte("new one")
	r.publish("a", "1.0.0", sri512(a), "", a)
	res, err := shelfFill(context.Background(), evictCfg(o, shelfKeepDefault), o, []shelfWant{want(t, "a", "1.0.0", a)})
	if err != nil || res.Fetched != 0 || res.Dropped[dropFull] != 1 {
		t.Fatalf("%+v %v", res, err)
	}
	if !onShelfVol(vol, kept) || len(blobsOn(t, vol)) != 1 {
		t.Errorf("blobs %v", blobsOn(t, vol))
	}
	if staged, _ := os.ReadDir(shelfStaging(o.stateDir)); len(staged) != 0 {
		t.Errorf("a tarball past the cap was left in staging: %v", staged)
	}
}

// Deleting a blob under a reader: the open file reads whole, and a box's
// cache link to it dangles (npm's miss: shelfhelper's
// TestNpmCiRefetchesDanglingLink).
func TestShelfEvictUnderReader(t *testing.T) {
	o, _, vol := newShelfOCI(t)
	body := []byte(strings.Repeat("a tarball being read ", 1000))
	hx := putBlob(t, vol, body, time.Now().Add(-48*time.Hour))
	blob := filepath.Join(vol, filepath.FromSlash(blobPath(hx)))
	f, err := os.Open(blob)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	head := make([]byte, 10)
	if _, err := io.ReadFull(f, head); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "_cacache", filepath.FromSlash(blobPath(hx)))
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(blob, link); err != nil {
		t.Fatal(err)
	}
	if res, err := evictShelf(evictCfg(o, 3600), o); err != nil || res.Deleted != 1 {
		t.Fatalf("%+v %v", res, err)
	}
	rest, err := io.ReadAll(f)
	if err != nil {
		t.Fatal(err)
	}
	if h := sha512.Sum512(append(head, rest...)); hex.EncodeToString(h[:]) != hx {
		t.Error("the open file did not read whole after the delete")
	}
	if _, err := os.Stat(link); !os.IsNotExist(err) {
		t.Errorf("the cache link does not dangle: %v", err)
	}
	if _, err := os.Lstat(link); err != nil {
		t.Errorf("the box's own link went: %v", err)
	}
}

// The filler evicts as it starts, with shelf_fill=seed too; the sweep starts
// one at most every shelfEvictEvery.
func TestShelfFillerEvicts(t *testing.T) {
	o, _, vol := newShelfOCI(t)
	old := putBlob(t, vol, []byte("old tarball"), time.Now().Add(-40*24*time.Hour))
	cfg := evictCfg(o, shelfKeepDefault)
	cfg.StateDir = o.stateDir
	var log strings.Builder
	if code := shelfFiller(o.stateDir, strings.NewReader(rawConfigOf(t, cfg, o)), &log); code != 0 {
		t.Fatalf("filler exit %d: %s", code, log.String())
	}
	if onShelfVol(vol, old) {
		t.Error("the filler did not evict")
	}
	if !shelfEvictDue(cfg) {
		t.Error("no stamp, not due")
	}
	d := &drv{cfg: cfg, st: store{o.stateDir}, vm: o}
	d.sweepShelfQueue()
	if shelfEvictDue(cfg) {
		t.Error("due again right after the sweep started one")
	}
	cfg.Shelf.MaxGB = 0
	if shelfEvictDue(cfg) {
		t.Error("due with shelf_max_gb 0")
	}
}

// Eviction never makes the volume: with none, it lists nothing.
func TestShelfEvictNoVolume(t *testing.T) {
	o, dir, _ := newShelfOCI(t)
	if err := os.Remove(filepath.Join(dir, "cache-volume")); err != nil {
		t.Fatal(err)
	}
	if _, err := evictShelf(evictCfg(o, 3600), o); err == nil {
		t.Error("evicted with no volume")
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "calls")); strings.Contains(string(b), "run ") {
		t.Errorf("a helper container ran: %s", b)
	}
}

func TestShelfEvictPlan(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	l := shelfListing{Other: 5, Blobs: []shelfBlobAt{
		{Hex: "c", Mtime: now.Unix() - 10, Size: 10},
		{Hex: "a", Mtime: now.Unix() - 500, Size: 10},
		{Hex: "b", Mtime: now.Unix() - 10, Size: 10},
	}}
	names := func(bs []shelfBlobAt, gos []shelfGoAt) (s string) {
		for _, b := range bs {
			s += b.Hex
		}
		if len(gos) > 0 {
			panic("a Go version from a listing with none")
		}
		return s
	}
	for _, c := range []struct {
		keep time.Duration
		max  int64
		want string
	}{
		{time.Hour, 100, ""},
		{100 * time.Second, 100, "a"},
		{time.Hour, 25, "a"},
		{time.Hour, 24, "ab"}, // ties by hash
		{time.Hour, 6, "abc"},
		{time.Hour, 5, ""},          // other files alone reach it: no blob can help
		{100 * time.Second, 0, "a"}, // the keep time still holds
	} {
		if got := names(shelfEvictPlan(l, now, c.keep, c.max)); got != c.want {
			t.Errorf("keep %v max %d: %q, want %q", c.keep, c.max, got, c.want)
		}
	}
}

// What a failed publish left in staging counts against the room, and is
// published with the next fill.
func TestShelfFillCountsStaging(t *testing.T) {
	o, _, vol := newShelfOCI(t)
	r := newFakeRegistry(t)
	useRegistry(t, r)
	smallGiB(t, 30)
	oldHelper := shelfHelper
	shelfHelper = nil
	t.Cleanup(func() { shelfHelper = oldHelper })
	left := []byte("left in staging, 25 b....")
	stage(t, o, blobPath(sum512(left)), left)
	a := []byte("new, 10 b!")
	r.publish("a", "1.0.0", sri512(a), "", a)
	res, err := shelfFill(context.Background(), evictCfg(o, shelfKeepDefault), o, []shelfWant{want(t, "a", "1.0.0", a)})
	if err != nil || res.Fetched != 0 || res.Dropped[dropFull] != 1 {
		t.Fatalf("%+v %v", res, err)
	}
	if b := blobsOn(t, vol); len(b) != 1 || b[0] != blobPath(sum512(left)) {
		t.Errorf("blobs %v: want only the staged one published", b)
	}
}

// sinkingDisk has room for the first check and none after.
type sinkingDisk struct {
	ociCLI
	calls *int
}

func (l sinkingDisk) FreeGiB() float64 {
	*l.calls++
	if *l.calls > 1 {
		return 0.5
	}
	return 100
}

// A fill that reaches the floor after fetching publishes nothing; what it
// staged waits for a later fill.
func TestShelfFloorBeforePublish(t *testing.T) {
	o, _, vol := newShelfOCI(t)
	r := newFakeRegistry(t)
	useRegistry(t, r)
	a := []byte("tarball a")
	r.publish("a", "1.0.0", sri512(a), "", a)
	cfg := evictCfg(o, shelfKeepDefault)
	cfg.PersistMinFreeGB = 1
	n := 0
	res, err := shelfFill(context.Background(), cfg, sinkingDisk{o, &n}, []shelfWant{want(t, "a", "1.0.0", a)})
	if err == nil || res.Fetched != 1 || len(blobsOn(t, vol)) != 0 || shelfStagedBytes(o.stateDir) != int64(len(a)) {
		t.Fatalf("%+v %v blobs %v", res, err, blobsOn(t, vol))
	}
}

// A fill that met the floor after staging publishes that staging on a later
// fill, even with no room for new tarballs and nothing new fetched.
func TestShelfStagingPublishedLater(t *testing.T) {
	o, _, vol := newShelfOCI(t)
	r := newFakeRegistry(t)
	useRegistry(t, r)
	smallGiB(t, 20)
	oldHelper := shelfHelper
	shelfHelper = nil
	t.Cleanup(func() { shelfHelper = oldHelper })
	a := []byte("tarball a, 20 bytes!")
	r.publish("a", "1.0.0", sri512(a), "", a)
	cfg := evictCfg(o, shelfKeepDefault)
	cfg.PersistMinFreeGB = 1
	n := 0
	if _, err := shelfFill(context.Background(), cfg, sinkingDisk{o, &n}, []shelfWant{want(t, "a", "1.0.0", a)}); err == nil {
		t.Fatal("the floor did not stop the publish")
	}
	hits := len(r.hits)
	res, err := shelfFill(context.Background(), cfg, o, []shelfWant{want(t, "a", "1.0.0", a)})
	if err != nil || res.Published.Published != 1 || len(blobsOn(t, vol)) != 1 || len(r.hits) != hits {
		t.Fatalf("%+v %v blobs %v, %d new requests", res, err, blobsOn(t, vol), len(r.hits)-hits)
	}
	if shelfStagedBytes(o.stateDir) != 0 {
		t.Error("staging not emptied")
	}
}
