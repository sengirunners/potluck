package tartpin_test

import (
	"archive/tar"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/potluck/tartpin"
	"github.com/sengirunners/potluck/tartpin/tartpintest"
)

func place(t *testing.T) tartpin.Place {
	dir := t.TempDir()
	return tartpin.Place{AppsDir: filepath.Join(dir, "Applications"), RecordFile: filepath.Join(dir, "support", "tart-install.json")}
}

// nothingInstalled fails unless AppsDir holds nothing: no tart.app and no
// staging folder left behind.
func nothingInstalled(t *testing.T, p tartpin.Place) {
	t.Helper()
	ents, err := os.ReadDir(p.AppsDir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	for _, e := range ents {
		t.Errorf("%s was left in %s", e.Name(), p.AppsDir)
	}
	if _, ok := p.Installed(); ok {
		t.Error("a record says Potluck installed Tart")
	}
}

func TestInstallPutsTheBundleInPlaceAndRecordsIt(t *testing.T) {
	p := place(t)
	tarball, _, b := tartpintest.Release(t, "1.0.0", tartpintest.Whole())
	res, err := p.Install(tarball, b)
	if err != nil || res.Already || res.Replaced != "" || res.Path != p.App() {
		t.Fatalf("install: %+v, %v", res, err)
	}
	got, err := os.ReadFile(p.Program())
	if err != nil || string(got) != tartpintest.ProgramOf("1.0.0") {
		t.Fatalf("program: %q, %v", got, err)
	}
	if st, _ := os.Stat(p.Program()); st.Mode().Perm()&0o100 == 0 {
		t.Fatalf("program mode %v cannot run", st.Mode())
	}
	if _, err := os.Stat(filepath.Join(p.AppsDir, "LICENSE")); err == nil {
		t.Fatal("the release's LICENSE landed in Applications")
	}
	if rec, ok := p.Installed(); !ok || rec.Version != "1.0.0" {
		t.Fatalf("record %+v %v", rec, ok)
	}
	if ents, _ := os.ReadDir(p.AppsDir); len(ents) != 1 {
		t.Fatalf("Applications holds %v, want only tart.app", ents)
	}
	if res, err := p.Install(tarball, b); err != nil || !res.Already {
		t.Fatalf("second install: %+v, %v", res, err)
	}
}

func TestInstallReplacesOnlyItsOwnCopy(t *testing.T) {
	p := place(t)
	old, _, ob := tartpintest.Release(t, "1.0.0", tartpintest.Whole())
	if _, err := p.Install(old, ob); err != nil {
		t.Fatal(err)
	}
	entries := append(tartpintest.Whole(), tartpintest.Entry{Name: "tart.app/Contents/new", Body: "2"})
	nw, _, nb := tartpintest.Release(t, "2.0.0", entries)
	res, err := p.Install(nw, nb)
	if err != nil || res.Replaced != "1.0.0" {
		t.Fatalf("upgrade: %+v, %v", res, err)
	}
	if _, err := os.Stat(filepath.Join(p.App(), "Contents", "new")); err != nil {
		t.Fatalf("the new bundle is not in place: %v", err)
	}
	if ents, _ := os.ReadDir(p.AppsDir); len(ents) != 1 {
		t.Fatalf("Applications holds %v after the swap", ents)
	}

	// The lender changes the program: the copy is theirs now.
	if err := os.WriteFile(p.Program(), []byte("mine"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, ok := p.Installed(); ok {
		t.Fatal("a changed copy still counts as Potluck's")
	}
	if _, err := p.Install(old, ob); !errors.Is(err, tartpin.ErrNotOurs) {
		t.Fatalf("replaced a copy Potluck no longer owns: %v", err)
	}
	if got, _ := os.ReadFile(p.Program()); string(got) != "mine" {
		t.Fatalf("the lender's copy was changed: %q", got)
	}
}

func TestInstallLeavesALendersOwnCopy(t *testing.T) {
	p := place(t)
	own := filepath.Join(p.App(), "Contents", "MacOS", "tart")
	if err := os.MkdirAll(filepath.Dir(own), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(own, []byte("own"), 0o755); err != nil {
		t.Fatal(err)
	}
	tarball, _, b := tartpintest.Release(t, "1.0.0", tartpintest.Whole())
	if _, err := p.Install(tarball, b); !errors.Is(err, tartpin.ErrNotOurs) {
		t.Fatalf("install over a lender's tart.app: %v", err)
	}
	if got, _ := os.ReadFile(own); string(got) != "own" {
		t.Fatalf("the lender's copy was changed: %q", got)
	}
}

func TestInstallRefusesAWrongDownload(t *testing.T) {
	tarball, _, b := tartpintest.Release(t, "1.0.0", tartpintest.Whole())
	wrongSize, wrongSum := b, b
	wrongSize.Bytes++
	wrongSum.SHA256 = strings.Repeat("0", 64)
	wrongProgram := b
	wrongProgram.ProgramSHA256 = strings.Repeat("1", 64)
	for name, c := range map[string]struct {
		b    tartpin.Build
		says string
	}{
		"size":    {wrongSize, "bytes"},
		"sha256":  {wrongSum, "SHA-256"},
		"program": {wrongProgram, "program has SHA-256"},
	} {
		t.Run(name, func(t *testing.T) {
			p := place(t)
			if _, err := p.Install(tarball, c.b); err == nil || !strings.Contains(err.Error(), c.says) || !strings.Contains(err.Error(), "Nothing was installed") {
				t.Fatalf("install: %v", err)
			}
			nothingInstalled(t, p)
		})
	}
}

func TestInstallRefusesEntriesOutsideTheBundle(t *testing.T) {
	prog := tartpintest.Entry{Name: "tart.app/Contents/MacOS/tart", Body: tartpintest.Program, Mode: 0o755}
	for name, bad := range map[string]tartpintest.Entry{
		"parent":              {Name: "../escape", Body: "x"},
		"parent inside":       {Name: "tart.app/../../escape", Body: "x"},
		"absolute":            {Name: "/tmp/escape", Body: "x"},
		"beside the bundle":   {Name: "other.app/x", Body: "x"},
		"symlink":             {Name: "tart.app/Contents/link", Type: tar.TypeSymlink, Linkname: "/etc"},
		"symlink to parent":   {Name: "tart.app/Contents/up", Type: tar.TypeSymlink, Linkname: "../../.."},
		"hard link":           {Name: "tart.app/Contents/hard", Type: tar.TypeLink, Linkname: "tart.app/Contents/MacOS/tart"},
		"device":              {Name: "tart.app/Contents/dev", Type: tar.TypeChar},
		"twice":               prog,
		"a LICENSE that runs": {Name: "LICENSE", Type: tar.TypeSymlink, Linkname: "/etc/passwd"},
	} {
		t.Run(name, func(t *testing.T) {
			p := place(t)
			tarball, _, b := tartpintest.Release(t, "1.0.0", []tartpintest.Entry{prog, bad})
			_, err := p.Install(tarball, b)
			if err == nil || !strings.Contains(err.Error(), "Nothing was installed") {
				t.Fatalf("install: %v", err)
			}
			t.Log(err)
			nothingInstalled(t, p)
			if _, err := os.Lstat(filepath.Join(filepath.Dir(p.AppsDir), "escape")); err == nil {
				t.Fatal("an entry escaped")
			}
		})
	}
}

func TestFetchChecksBeforeKeepingTheFile(t *testing.T) {
	_, data, b := tartpintest.Release(t, "1.0.0", tartpintest.Whole())
	serve := data
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(serve) }))
	defer srv.Close()
	b.URL = srv.URL + "/tart.tar.gz"
	dir := t.TempDir()
	good := filepath.Join(dir, "good.tar.gz")
	if err := tartpin.Fetch(context.Background(), srv.Client(), b, good); err != nil {
		t.Fatalf("fetch: %v", err)
	}
	for name, body := range map[string][]byte{
		"longer":   append(append([]byte{}, data...), 'x'),
		"shorter":  data[:len(data)-1],
		"tampered": append(append([]byte{}, data[:len(data)-1]...), data[len(data)-1]^1),
	} {
		serve = body
		path := filepath.Join(dir, name)
		if err := tartpin.Fetch(context.Background(), srv.Client(), b, path); err == nil || !strings.Contains(err.Error(), "Nothing was installed") {
			t.Errorf("%s: fetch %v", name, err)
		}
		if _, err := os.Stat(path); err == nil {
			t.Errorf("%s: the refused download was kept", name)
		}
	}
}

func TestAnOlderPotluckNeverDowngrades(t *testing.T) {
	p := place(t)
	nw, _, nb := tartpintest.Release(t, "2.43.0", tartpintest.Whole())
	if _, err := p.Install(nw, nb); err != nil {
		t.Fatal(err)
	}
	old, _, ob := tartpintest.Release(t, "2.42.0", append(tartpintest.Whole(), tartpintest.Entry{Name: "tart.app/Contents/old", Body: "x"}))
	res, err := p.Install(old, ob)
	if err != nil || !res.Already || res.Version != "2.43.0" {
		t.Fatalf("an older release over a newer one: %+v, %v", res, err)
	}
	if _, err := os.Stat(filepath.Join(p.App(), "Contents", "old")); err == nil {
		t.Fatal("the older release was swapped in")
	}
	if rec, _ := p.Installed(); rec.Version != "2.43.0" {
		t.Fatalf("record %+v", rec)
	}
}

func TestAWithdrawnInstalledReleaseIsReplaced(t *testing.T) {
	p := place(t)
	nw, _, nb := tartpintest.Release(t, "2.43.0", tartpintest.Whole())
	if _, err := p.Install(nw, nb); err != nil {
		t.Fatal(err)
	}
	restore := tartpin.Builds
	t.Cleanup(func() { tartpin.Builds = restore })
	w := nb
	w.Withdrawn = "test"
	tartpin.Builds = []tartpin.Build{w}
	old, _, ob := tartpintest.Release(t, "2.42.0", tartpintest.Whole())
	if res, err := p.Install(old, ob); err != nil || res.Replaced != "2.43.0" {
		t.Fatalf("over a withdrawn release: %+v, %v", res, err)
	}
}

// The lender drags their own tart.app in while an upgrade unpacks: theirs
// is put back, not deleted.
func TestASwapNeverTakesALendersCopy(t *testing.T) {
	p := place(t)
	old, _, ob := tartpintest.Release(t, "1.0.0", tartpintest.Whole())
	if _, err := p.Install(old, ob); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(tartpin.SetBeforeMove(func() {
		if err := os.WriteFile(p.Program(), []byte("mine"), 0o755); err != nil {
			t.Fatal(err)
		}
	}))
	nw, _, nb := tartpintest.Release(t, "2.0.0", append(tartpintest.Whole(), tartpintest.Entry{Name: "tart.app/Contents/new", Body: "2"}))
	if _, err := p.Install(nw, nb); !errors.Is(err, tartpin.ErrNotOurs) {
		t.Fatalf("install: %v", err)
	}
	if got, _ := os.ReadFile(p.Program()); string(got) != "mine" {
		t.Fatalf("the lender's copy is gone: %q", got)
	}
	if ents, _ := os.ReadDir(p.AppsDir); len(ents) != 1 {
		t.Fatalf("Applications holds %v", ents)
	}
}

func TestAnInterruptedInstallsFolderIsSwept(t *testing.T) {
	p := place(t)
	stale := filepath.Join(p.AppsDir, ".tart.app.potluck-123")
	fresh := filepath.Join(p.AppsDir, ".tart.app.potluck-456")
	for _, d := range []string{stale, fresh} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	long := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(stale, long, long); err != nil {
		t.Fatal(err)
	}
	tarball, _, b := tartpintest.Release(t, "1.0.0", tartpintest.Whole())
	if _, err := p.Install(tarball, b); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); err == nil {
		t.Error("a stale stage folder was kept")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Error("a running install's stage folder was removed")
	}
}

// Interrupted installs' folders are swept whatever they unpacked; a folder
// a swap used is swept only when it holds a bundle Potluck made.
func TestTheSweepKeepsALendersCopy(t *testing.T) {
	p := place(t)
	tarball, _, b := tartpintest.Release(t, "1.0.0", tartpintest.Whole())
	if _, err := p.Install(tarball, b); err != nil {
		t.Fatal(err)
	}
	long := time.Now().Add(-2 * time.Hour)
	dir := func(name string) string {
		d := filepath.Join(p.AppsDir, name)
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
		return d
	}
	write := func(d string) {
		prog := filepath.Join(d, "tart.app", "Contents", "MacOS", "tart")
		os.MkdirAll(filepath.Dir(prog), 0o755)
		if err := os.WriteFile(prog, []byte("half"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	partial, lenders, ours := dir(".tart.app.potluck-1"), dir(".tart.app.swap-2"), dir(".tart.app.swap-3")
	write(partial)
	write(lenders)
	if err := os.Rename(p.App(), filepath.Join(ours, "tart.app")); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{partial, lenders, ours} {
		os.Chtimes(d, long, long)
	}
	if err := p.Sweep(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(lenders); err != nil {
		t.Error("the sweep removed a lender's copy")
	}
	for _, d := range []string{partial, ours} {
		if _, err := os.Stat(d); err == nil {
			t.Errorf("the sweep kept %s", d)
		}
	}
}

// The record is there before the move, so an install that dies at any
// point leaves a bundle a later release still replaces.
func TestTheRecordComesBeforeTheMove(t *testing.T) {
	p := place(t)
	old, _, ob := tartpintest.Release(t, "1.0.0", tartpintest.Whole())
	if _, err := p.Install(old, ob); err != nil {
		t.Fatal(err)
	}
	var atMove []byte
	restore := tartpin.SetBeforeMove(func() { atMove, _ = os.ReadFile(p.RecordFile) })
	nw, _, nb := tartpintest.Release(t, "2.0.0", tartpintest.Whole())
	_, err := p.Install(nw, nb)
	restore()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.RecordFile, atMove, 0o600); err != nil {
		t.Fatal(err)
	}
	next, _, b3 := tartpintest.Release(t, "3.0.0", tartpintest.Whole())
	if res, err := p.Install(next, b3); err != nil || res.Replaced != "2.0.0" {
		t.Fatalf("after a crash at the move: %+v, %v", res, err)
	}
}

// A record that cannot be written stops the install before the move.
func TestNoRecordNoInstall(t *testing.T) {
	p := place(t)
	if err := os.MkdirAll(p.RecordFile, 0o755); err != nil {
		t.Fatal(err)
	}
	tarball, _, b := tartpintest.Release(t, "1.0.0", tartpintest.Whole())
	if _, err := p.Install(tarball, b); err == nil {
		t.Fatal("installed without a record")
	}
	nothingInstalled(t, p)
}

// A lender's own bundle of the very release Potluck installed, one with any
// file changed, and a link are all the lender's.
func TestOnlyTheVeryBundlePotluckMadeIsIts(t *testing.T) {
	for name, change := range map[string]func(t *testing.T, app string) error{
		"a copy of the same release": func(t *testing.T, app string) error {
			moved := filepath.Join(t.TempDir(), "keep")
			if err := os.Rename(app, moved); err != nil {
				return err
			}
			return os.CopyFS(app, os.DirFS(moved))
		},
		"another file added": func(t *testing.T, app string) error {
			return os.WriteFile(filepath.Join(app, "Contents", "notes"), []byte("mine"), 0o644)
		},
		"a link to it": func(t *testing.T, app string) error {
			moved := filepath.Join(t.TempDir(), "keep")
			if err := os.Rename(app, moved); err != nil {
				return err
			}
			return os.Symlink(moved, app)
		},
	} {
		t.Run(name, func(t *testing.T) {
			p := place(t)
			old, _, ob := tartpintest.Release(t, "1.0.0", tartpintest.Whole())
			if _, err := p.Install(old, ob); err != nil {
				t.Fatal(err)
			}
			if err := change(t, p.App()); err != nil {
				t.Fatal(err)
			}
			nw, _, nb := tartpintest.Release(t, "2.0.0", tartpintest.Whole())
			if _, err := p.Install(nw, nb); !errors.Is(err, tartpin.ErrNotOurs) {
				t.Fatalf("install: %v", err)
			}
		})
	}
}

// An empty tart.app that appears during a first install is not overwritten.
func TestAFreshInstallNeverReplacesWhatAppeared(t *testing.T) {
	p := place(t)
	t.Cleanup(tartpin.SetBeforeMove(func() { os.Mkdir(p.App(), 0o755) }))
	tarball, _, b := tartpintest.Release(t, "1.0.0", tartpintest.Whole())
	if _, err := p.Install(tarball, b); err == nil {
		t.Fatal("installed over what appeared")
	}
	if ents, err := os.ReadDir(p.App()); err != nil || len(ents) != 0 {
		t.Fatalf("%v %v", ents, err)
	}
}

// A swap back that fails leaves the lender's bundle, even one with no
// program, where no later install sweeps it, and the error names the place.
func TestAFailedSwapBackNeverLosesALendersCopy(t *testing.T) {
	p := place(t)
	old, _, ob := tartpintest.Release(t, "1.0.0", tartpintest.Whole())
	if _, err := p.Install(old, ob); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(tartpin.FailSwapBack())
	restore := tartpin.SetBeforeMove(func() {
		if err := os.RemoveAll(p.App()); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(p.App(), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(p.App(), "mine"), []byte("mine"), 0o644); err != nil {
			t.Fatal(err)
		}
	})
	defer restore()
	nw, _, nb := tartpintest.Release(t, "2.0.0", append(tartpintest.Whole(), tartpintest.Entry{Name: "tart.app/Contents/new", Body: "2"}))
	_, err := p.Install(nw, nb)
	restore()
	kept, _ := filepath.Glob(filepath.Join(p.AppsDir, "tart put aside *", "tart.app", "mine"))
	if err == nil || len(kept) != 1 || !strings.Contains(err.Error(), filepath.Dir(kept[0])) {
		t.Fatalf("install: %v; kept %v", err, kept)
	}
	long := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(filepath.Dir(filepath.Dir(kept[0])), long, long); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(p.App()); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Install(nw, nb); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(kept[0]); err != nil {
		t.Fatal("a later install removed the lender's copy")
	}
}

// A process killed right after the swap left a lender's bundle in the work
// folder: the sweep keeps it. (The folder the swap really used is put back
// with that bundle in it.)
func TestAKillAfterTheSwapNeverLosesALendersCopy(t *testing.T) {
	p := place(t)
	old, _, ob := tartpintest.Release(t, "1.0.0", tartpintest.Whole())
	if _, err := p.Install(old, ob); err != nil {
		t.Fatal(err)
	}
	var name string
	restore := tartpin.OnSwap(func(int) {
		dirs, _ := filepath.Glob(filepath.Join(p.AppsDir, ".tart.app.*"))
		if len(dirs) == 1 {
			name = dirs[0]
		}
	})
	nw, _, nb := tartpintest.Release(t, "2.0.0", tartpintest.Whole())
	_, err := p.Install(nw, nb)
	restore()
	if err != nil || name == "" {
		t.Fatalf("%v %q", err, name)
	}
	mine := filepath.Join(name, "tart.app", "mine")
	if err := os.MkdirAll(filepath.Dir(mine), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mine, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	long := time.Now().Add(-2 * time.Hour)
	os.Chtimes(name, long, long)
	if err := p.Sweep(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(mine); err != nil {
		t.Fatal("the sweep removed a lender's copy")
	}
}

// The lender is still copying when the swap back comes: what it takes out
// is put aside, not deleted.
func TestASwapBackKeepsWhatItTakesOut(t *testing.T) {
	p := place(t)
	old, _, ob := tartpintest.Release(t, "1.0.0", tartpintest.Whole())
	if _, err := p.Install(old, ob); err != nil {
		t.Fatal(err)
	}
	defer tartpin.OnSwap(func(n int) {
		os.WriteFile(filepath.Join(p.App(), "half"+string(rune('0'+n))), nil, 0o644)
	})()
	nw, _, nb := tartpintest.Release(t, "2.0.0", tartpintest.Whole())
	if _, err := p.Install(nw, nb); !errors.Is(err, tartpin.ErrNotOurs) {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(p.App(), "half1")); err != nil {
		t.Error(err)
	}
	if kept, _ := filepath.Glob(filepath.Join(p.AppsDir, "tart put aside *", "tart.app", "half2")); len(kept) != 1 {
		t.Errorf("kept %v", kept)
	}
}
