package hostprobe

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// cutPull leaves what a killed pull of image leaves in dir's pull home.
func cutPull(t *testing.T, dir, image string) string {
	t.Helper()
	p := filepath.Join(tartPartials(dir), partialName(image))
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p, "disk.img"), []byte("half"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func there(path string) bool { _, err := os.Lstat(path); return err == nil }

// The name is Tart's own: a pull of this image by Tart 2.41.1, killed, left
// this directory.
func TestPartialNameIsTarts(t *testing.T) {
	if got := partialName("ghcr.io/cirruslabs/macos-tahoe-base@sha256:65c3133a633fda8753cf1f71f2d55727792d534a2125b0032466563dcc8e58fd"); got != "1bcadcaf71d0372b323ff947bc36a335" {
		t.Errorf("%s", got)
	}
}

// fakeTartScript is a Tart as measured on 2.41.1: a pull keeps what it has in
// TART_HOME's tmp, goes on from there, and moves the whole into the cache;
// any other command first deletes what is in TART_HOME's tmp.
const fakeTartScript = `#!/bin/sh
home=${TART_HOME:?}
if [ "$1" = pull ]; then
  part=$home/tmp/$(printf %s "$3" | md5 -q 2>/dev/null || printf %s "$3" | md5sum | cut -d' ' -f1)
  mkdir -p "$part"
  echo "run" >> "$part/runs"
  if [ "$(wc -l < "$part/runs")" -lt 2 ]; then kill -9 $$; fi
  mkdir -p "$home/cache/OCIs" && mv "$part" "$home/cache/OCIs/done"
  exit 0
fi
rm -rf "$home"/tmp/*
`

// A pull killed partway goes on from where it stopped, though Tart ran in
// between, and its image ends in Tart's own cache.
func TestKilledPullGoesOn(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tart") // Tart never ran here
	cli := filepath.Join(t.TempDir(), "tart")
	if err := os.WriteFile(cli, []byte(fakeTartScript), 0o755); err != nil {
		t.Fatal(err)
	}
	const image = "reg.example/mac@sha256:1"
	pull := func() error {
		cmd, err := readyTartPull(dir, []string{cli, "pull", "--", image})
		if err != nil {
			t.Fatal(err)
		}
		return exec.Command(cmd[0], cmd[1:]...).Run()
	}
	if err := pull(); err == nil {
		t.Fatal("the first pull was not killed, so this test shows nothing")
	}
	list := exec.Command(cli, "list")
	list.Env = append(os.Environ(), "TART_HOME="+dir)
	if err := list.Run(); err != nil {
		t.Fatal(err)
	}
	if err := pull(); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "cache", "OCIs", "done", "runs")); err != nil || string(b) != "run\nrun\n" {
		t.Errorf("the second pull did not go on from the first: %q %v", b, err)
	}
}

// Tart deletes no image of the person's for a resumed download.
func TestTartPullCommand(t *testing.T) {
	dir, plain := t.TempDir(), []string{"/bin/tart", "pull", "--", "r/a@sha256:1"}
	if got, err := readyTartPull(dir, plain); err != nil || strings.Join(got, " ") != "/usr/bin/env TART_HOME="+dir+"/potluck-pull TART_NO_AUTO_PRUNE=1 /bin/tart pull -- r/a@sha256:1" {
		t.Error(got, err)
	}
	if got, err := readyTartPull("", plain); err != nil || !slices.Equal(got, plain) {
		t.Error(got, err)
	}
}

// Where the pull home cannot be made, the pull is a plain one.
func TestNoPullHomeIsAPlainPull(t *testing.T) {
	for name, spoil := range map[string]func(dir string) error{
		"a file there": func(dir string) error {
			return os.WriteFile(filepath.Join(dir, TartPullHome), nil, 0o600)
		},
		"a link elsewhere": func(dir string) error {
			if err := os.Mkdir(filepath.Join(dir, TartPullHome), 0o755); err != nil {
				return err
			}
			return os.Symlink("/elsewhere", filepath.Join(dir, TartPullHome, "cache"))
		},
		"a cache of its own": func(dir string) error {
			return os.MkdirAll(filepath.Join(dir, TartPullHome, "cache"), 0o755)
		},
		"the home a link": func(dir string) error {
			return os.Symlink(t.TempDir(), filepath.Join(dir, TartPullHome))
		},
		"its tmp a link": func(dir string) error {
			if err := os.Mkdir(filepath.Join(dir, TartPullHome), 0o755); err != nil {
				return err
			}
			return os.Symlink(t.TempDir(), tartPartials(dir))
		},
	} {
		dir := t.TempDir()
		if err := spoil(dir); err != nil {
			t.Fatal(err)
		}
		cmd, err := readyTartPull(dir, []string{"tart", "pull", "--", "r/a@sha256:1"})
		if err == nil || strings.Join(cmd, " ") != "tart pull -- r/a@sha256:1" {
			t.Errorf("%s: %v %v", name, cmd, err)
		}
	}
}

// A share deletes the unfinished downloads nothing goes on with before it
// pulls; its own, another home's, and one that goes on stay.
func TestShareDropsDeadDownloads(t *testing.T) {
	withTartImages(t, pinA)
	dir := t.TempDir()
	// The last home is gone from this Mac: its record keeps nothing.
	for home, im := range map[string]string{"/h/potluck": "r/old@sha256:1", t.TempDir(): "r/theirs@sha256:2", "/h/gone": "r/unknown@sha256:3"} {
		if err := StartTartShare(dir, home, []string{im}, time.Now().Add(-time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	old, unknown := cutPull(t, dir, "r/old@sha256:1"), cutPull(t, dir, "r/unknown@sha256:3")
	mine, theirs, busy := cutPull(t, dir, pinA), cutPull(t, dir, "r/theirs@sha256:2"), cutPull(t, dir, "r/busy@sha256:4")
	lock, err := lockPartial(busy)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	run := &fakeRunner{on: func(line string) (Result, bool) {
		if line == "tart list --format json" {
			return ok("[]"), true
		}
		if strings.Contains(line, " pull ") && (there(old) || there(unknown)) {
			t.Error("pulled with a dead download still there")
		}
		return ok(""), true
	}}
	report := macReport()
	report.TartDir, report.TartFreeGB = dir, 500
	res, err := checkOn(context.Background(), run, ShareOptions{Report: report, Home: "/h/potluck", Images: []string{pinA}, NamePrefix: "mac", Sizes: []string{"c2m4"}})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(run.calls, "/usr/bin/env TART_HOME="+filepath.Join(dir, TartPullHome)+" TART_NO_AUTO_PRUNE=1 tart pull -- "+pinA) {
		t.Errorf("%v", run.calls)
	}
	for p, want := range map[string]bool{old: false, unknown: false, mine: true, theirs: true, busy: true} {
		if there(p) != want {
			t.Errorf("%s there %v (%v)", p, !want, res.Notes)
		}
	}
}

// So does one that pulls nothing: what is here is no longer to download.
func TestShareOfImagesHereDropsDeadDownloads(t *testing.T) {
	withTartImages(t, pinA)
	dir := t.TempDir()
	other, here := cutPull(t, dir, "r/other@sha256:1"), cutPull(t, dir, pinA)
	run := &fakeRunner{on: func(line string) (Result, bool) {
		if line == "tart list --format json" {
			return ok(`[{"Source":"OCI","Name":"` + pinA + `","State":"stopped"}]`), true
		}
		return ok(""), true
	}}
	report := macReport()
	report.TartDir, report.TartFreeGB = dir, 500
	if _, err := checkOn(context.Background(), run, ShareOptions{Report: report, Home: "/h/potluck", Images: []string{pinA}, NamePrefix: "mac", Sizes: []string{"c2m4"}}); err != nil {
		t.Fatal(err)
	}
	if slices.ContainsFunc(run.calls, func(c string) bool { return strings.Contains(c, " pull ") }) {
		t.Fatalf("pulled: %v", run.calls)
	}
	if there(other) || there(here) {
		t.Errorf("still there: another's %v, this image's %v", there(other), there(here))
	}
}

// The plan's command, which a person may paste, is a pull into Tart's own
// home: only a run makes the pull home.
func TestPlannedPullIsPlain(t *testing.T) {
	withTartImages(t, pinA)
	report := macReport()
	report.TartDir, report.TartFreeGB = t.TempDir(), 500
	run := &fakeRunner{on: func(line string) (Result, bool) { return ok("[]"), true }}
	res, err := checkOn(context.Background(), run, ShareOptions{Report: report, Home: "/h/potluck", Images: []string{pinA}, NamePrefix: "mac", Sizes: []string{"c2m4"}, DryRun: true})
	if err != nil || len(res.Pulls) != 1 || strings.Join(res.Pulls[0].Command, " ") != "tart pull -- "+pinA {
		t.Errorf("%v %v", res.Pulls, err)
	}
	if there(filepath.Join(report.TartDir, TartPullHome)) {
		t.Error("a plan made the pull home")
	}
}

// What cut-off downloads took of the disk is not asked for again: this
// image's is there to use, and a dead one goes first. One another home wants
// is not counted.
func TestACutOffDownloadCountsAsDiskThatIsThere(t *testing.T) {
	dir := t.TempDir()
	pin, _ := DefaultImage("tart")
	need := func(home string) int {
		_, n, err := tartPullNeed(context.Background(), tartListing(t), dir, home, "/fake/tart", []string{pin.Image})
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := need("/h/potluck"); n != pin.MinFreeGB {
		t.Fatalf("need %d", n)
	}
	mine := cutPull(t, dir, pin.Image)
	f, err := os.OpenFile(filepath.Join(mine, "disk.img"), os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(50 << 30); err != nil { // sparse: it takes next to nothing
		t.Fatal(err)
	}
	f.Close()
	if n := need("/h/potluck"); n != pin.MinFreeGB {
		t.Errorf("a sparse file counts: need %d", n)
	}
	dead := cutPull(t, dir, "r/dead@sha256:1")
	theirs := cutPull(t, dir, "r/theirs@sha256:2")
	if err := StartTartShare(dir, t.TempDir(), []string{"r/theirs@sha256:2"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	old := diskTaken
	defer func() { diskTaken = old }()
	diskTaken = func(p string) int64 { return map[string]int64{mine: 12, dead: 3, theirs: 5}[p] << 30 }
	if n := need("/h/potluck"); n != pin.MinFreeGB-15 {
		t.Errorf("need %d", n)
	}
	if n := need(""); n != pin.MinFreeGB-12 { // a share on no home deletes none
		t.Errorf("need %d", n)
	}
	// An unfinished download of an image Tart has since is a dead one too.
	const here = "r/here@sha256:5"
	had := cutPull(t, dir, here)
	diskTaken = func(p string) int64 { return map[string]int64{had: 7}[p] << 30 }
	_, n, err := tartPullNeed(context.Background(), tartListing(t, here), dir, "/h/potluck", "/fake/tart", []string{here, pin.Image})
	if err != nil || n != pin.MinFreeGB-7 {
		t.Errorf("need %d %v", n, err)
	}
}

// A Mac Potluck leaves keeps no unfinished download; another home's stays,
// and so does all of Tart's own.
func TestDropPartials(t *testing.T) {
	old := partialWait
	defer func() { partialWait = old }()
	partialWait = 50 * time.Millisecond
	dir := t.TempDir()
	home, other := filepath.Join(dir, "home"), filepath.Join(dir, "other")
	if _, err := readyTartPull(dir, []string{"tart", "pull", "--", "r/a@sha256:1"}); err != nil {
		t.Fatal(err)
	}
	cached, tmp := filepath.Join(dir, "cache", "img"), filepath.Join(dir, "tmp", partialName("r/a@sha256:1"))
	for _, p := range []string{cached, tmp} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(other, 0o700); err != nil {
		t.Fatal(err)
	}
	for h, im := range map[string]string{home: "r/ours@sha256:1", other: "r/theirs@sha256:2"} {
		if err := StartTartShare(dir, h, []string{im}, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	ours, theirs, busy := cutPull(t, dir, "r/ours@sha256:1"), cutPull(t, dir, "r/theirs@sha256:2"), cutPull(t, dir, "r/busy@sha256:3")
	pull, err := lockPartial(busy)
	if err != nil {
		t.Fatal(err)
	}
	if err := (TartLeave{Dir: dir, Home: home}).DropPartials(context.Background()); err == nil || there(ours) || !there(theirs) || !there(busy) {
		t.Fatalf("with a pull going on: %v, ours %v", err, there(ours))
	}
	pull.Close()
	if err := (TartLeave{Dir: dir, Home: home}).DropPartials(context.Background()); err != nil || there(busy) || !there(theirs) {
		t.Fatalf("with another home here: %v", err)
	}
	if err := editTartPulls(dir, func(rec *tartPullRecord) error { rec.forget(other); return nil }); err != nil {
		t.Fatal(err)
	}
	l := TartLeave{Dir: dir, Home: home}
	if err := l.DropPartials(context.Background()); err != nil || l.HasPullHome() || !there(cached) || !there(tmp) {
		t.Fatalf("the last home: %v, pull home %v, cache %v, tmp %v", err, l.HasPullHome(), there(cached), there(tmp))
	}
	if err := l.DropPartials(context.Background()); err != nil {
		t.Errorf("again: %v", err)
	}
}

// With the record unread nothing is deleted: another home may want it.
func TestUnreadRecordKeepsDownloads(t *testing.T) {
	dir := t.TempDir()
	p := cutPull(t, dir, "r/a@sha256:1")
	if err := os.WriteFile(filepath.Join(dir, TartPullsFile), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := dropDeadPartials(dir, "/h", nil); err == nil || !there(p) {
		t.Errorf("share: %v", err)
	}
	if err := (TartLeave{Dir: dir, Home: "/h"}).DropPartials(context.Background()); err == nil || !there(p) {
		t.Errorf("uninstall: %v", err)
	}
}

// Only what a pull makes is deleted: never what a link of the person's leads
// to, nor an image in a cache the pull home has of its own.
func TestDropPartialsDeletesNothingElse(t *testing.T) {
	for name, at := range map[string]func(dir, theirs string) error{
		"the home a link": func(dir, theirs string) error {
			return os.Symlink(theirs, filepath.Join(dir, TartPullHome))
		},
		"its tmp a link": func(dir, theirs string) error {
			if err := os.Mkdir(filepath.Join(dir, TartPullHome), 0o755); err != nil {
				return err
			}
			return os.Symlink(theirs, tartPartials(dir))
		},
	} {
		dir, theirs := t.TempDir(), t.TempDir()
		file, sub := filepath.Join(theirs, "mine.txt"), filepath.Join(theirs, "tmp", "mine")
		if err := os.MkdirAll(sub, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := at(dir, theirs); err != nil {
			t.Fatal(err)
		}
		_ = dropDeadPartials(dir, "/h", nil)
		if err := (TartLeave{Dir: dir, Home: "/h"}).DropPartials(context.Background()); err == nil || !there(file) || !there(sub) {
			t.Errorf("%s: %v, file %v, directory %v", name, err, there(file), there(sub))
		}
		if readUnfinishedPulls(dir) != nil {
			t.Errorf("%s: counted as downloads", name)
		}
	}
	dir := t.TempDir()
	img := filepath.Join(dir, TartPullHome, "cache", "img")
	if err := os.MkdirAll(img, 0o755); err != nil {
		t.Fatal(err)
	}
	_ = (TartLeave{Dir: dir, Home: "/h"}).DropPartials(context.Background())
	if !there(img) {
		t.Error("an image in the pull home's own cache was deleted")
	}
}

// A download whose lock cannot be read is no download that goes on.
func TestUnreadableDownloadIsNotBusy(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root reads everything")
	}
	dir := t.TempDir()
	p := cutPull(t, dir, "r/a@sha256:1")
	if err := os.Chmod(p, 0); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(p, 0o700)
	start := time.Now()
	if err := (TartLeave{Dir: dir, Home: "/h"}).DropPartials(context.Background()); err == nil || time.Since(start) > partialWait/2 {
		t.Errorf("%v after %s", err, time.Since(start))
	}
}

// host images says what unfinished downloads take.
func TestInventoryNamesUnfinishedDownloads(t *testing.T) {
	dir := t.TempDir()
	if readUnfinishedPulls(dir) != nil {
		t.Error("none is there")
	}
	cutPull(t, dir, "r/a@sha256:1")
	if u := readUnfinishedPulls(dir); u == nil || u.GB != 1 || u.Dir != tartPartials(dir) {
		t.Errorf("%+v", u)
	}
}
