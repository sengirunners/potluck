package hostprobe

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sengirunners/potluck/computeapi"
)

// fakeTart is Tart's list and delete over a map of entries: delete removes
// an entry unless fail names it (then it exits 1 with that stderr); gone
// names entries another process removes while a delete of it runs.
type fakeTart struct {
	mu       sync.Mutex
	entries  map[string]map[string]any
	fail     map[string]string
	gone     map[string]bool
	deletes  []string
	onDelete func(name string) // runs inside a delete, before it removes the entry
}

func newFakeTart() *fakeTart {
	return &fakeTart{entries: map[string]map[string]any{}, fail: map[string]string{}, gone: map[string]bool{}}
}

func (f *fakeTart) image(name string, size any) {
	f.entries[name] = map[string]any{"Source": "OCI", "Name": name, "Size": size, "Running": false}
}

func (f *fakeTart) vm(name string, size int, running bool) {
	f.entries[name] = map[string]any{"Source": "local", "Name": name, "Size": size, "Running": running}
}

func (f *fakeTart) Run(_ context.Context, name string, args ...string) (Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if name != "/fake/tart" {
		return Result{}, errors.New("fake tart: ran " + name)
	}
	switch {
	case slices.Equal(args, []string{"list", "--format", "json"}):
		var list []map[string]any
		for _, k := range slices.Sorted(func(yield func(string) bool) {
			for k := range f.entries {
				if !yield(k) {
					return
				}
			}
		}) {
			list = append(list, f.entries[k])
		}
		b, _ := json.Marshal(list)
		return ok(string(b)), nil
	case len(args) == 2 && args[0] == "delete":
		f.deletes = append(f.deletes, args[1])
		if f.onDelete != nil {
			f.onDelete(args[1])
		}
		if f.gone[args[1]] {
			delete(f.entries, args[1])
			return Result{Exit: 1, Stderr: "the specified VM \"" + args[1] + "\" does not exist"}, nil
		}
		if why, bad := f.fail[args[1]]; bad {
			return Result{Exit: 1, Stderr: why}, nil
		}
		delete(f.entries, args[1])
		return Result{}, nil
	}
	return Result{}, errors.New("fake tart: no such command " + strings.Join(args, " "))
}

// sequoiaMac is a macOS 15.6 Mac on Apple silicon, as Detect reads one,
// with its Tart directory (and so Potluck's record of its pulls) in a temp
// dir.
func sequoiaMac(t *testing.T) Report {
	return Report{OS: "darwin", Arch: "arm64", MacOS: "15.6", TartDir: t.TempDir(), TartFreeGB: 40,
		Layers: []Layer{{Name: "tart", Available: true, Supported: true, CLI: "/fake/tart"}}}
}

// testHome is the Potluck home the tests' shares ran on.
const testHome = "/h/potluck"

// recordShare writes Potluck's record of a share on home at start that used
// images, pulling those in pulled.
func recordShare(t *testing.T, r Report, home string, start time.Time, images []string, pulled ...string) {
	t.Helper()
	if err := StartTartShare(r.TartDir, home, images, start); err != nil {
		t.Fatal(err)
	}
	for _, im := range pulled {
		if err := RecordTartPull(r.TartDir, im, home, start, start); err != nil {
			t.Fatal(err)
		}
	}
}

// recordPulls writes Potluck's record that an earlier share on testHome
// pulled images, and that a later one there used none of them.
func recordPulls(t *testing.T, r Report, images ...string) {
	t.Helper()
	at := time.Now().Add(-time.Hour)
	recordShare(t, r, testHome, at, images, images...)
	recordShare(t, r, testHome, at.Add(time.Minute), nil)
}

// recordOnDisk is Potluck's record of Tart's images as its file
// (TartPullsFile) holds it.
type recordOnDisk struct {
	Images map[string]TartUse   `json:"images"`
	Shares map[string]time.Time `json:"shares"`
}

// readRecord reads the record's file in tartDir.
func readRecord(tartDir string) (recordOnDisk, error) {
	var rec recordOnDisk
	b, err := os.ReadFile(filepath.Join(tartDir, TartPullsFile))
	if err != nil {
		return rec, err
	}
	return rec, json.Unmarshal(b, &rec)
}

// served is a BindingsReader that always answers ts, read from testHome's
// daemon.
func served(ts ...computeapi.RunTarget) BindingsReader {
	return func(context.Context) (Served, error) { return Served{Targets: ts, Homes: []string{testHome}}, nil }
}

// tartPins are the shipped pins a macOS 15 Mac's cleanup turns on.
func tartPins(t *testing.T) (tahoe, sequoia, small DefaultPin) {
	t.Helper()
	tahoe, err := DefaultImage("tart")
	if err != nil || tahoe.MinHostMacOS <= 15 {
		t.Fatalf("the tart default is not a macOS 16+ image: %+v %v", tahoe, err)
	}
	pins := pinTable()
	var ok1, ok2 bool
	for _, p := range pins {
		switch {
		case p.Layer == "tart" && p.Name == "sequoia":
			sequoia, ok1 = p, true
		case p.Layer == "tart" && p.Name == SmallLinux:
			small, ok2 = p, true
		}
	}
	if !ok1 || !ok2 {
		t.Fatal("images.pin has no sequoia or small-linux Tart line")
	}
	return tahoe, sequoia, small
}

func states(inv TartInventory) map[string]TartState {
	m := map[string]TartState{}
	for _, it := range inv.Items {
		m[it.Name] = it.State
	}
	return m
}

// A Mac whose setup pulled Tahoe on macOS 15, then lent Sequoia: the
// inventory keeps what a retry and the shared sizes need (a box binding's
// images, a run binding's config image), marks removable only the unneeded
// images Potluck's record says it pulled, keeps a pinned repository's digest
// it has no record of, leaves VMs and others' images alone, and a removal
// deletes exactly the removable ones and takes them out of the record; a
// second removal deletes nothing.
func TestTartCleanupMixedResources(t *testing.T) {
	tahoe, sequoia, small := tartPins(t)
	repo := imageRepository(sequoia.Image)
	olderSequoia := repo + "@sha256:" + strings.Repeat("0", 64)
	olderLent := repo + "@sha256:" + strings.Repeat("1", 64)
	runLent := repo + "@sha256:" + strings.Repeat("5", 64)
	unrecorded := repo + "@sha256:" + strings.Repeat("4", 64)
	owners := "reg.example/acme@sha256:" + strings.Repeat("2", 64)
	ownersTag := "reg.example/acme:latest"
	other := "reg.example/other@sha256:" + strings.Repeat("3", 64)
	f := newFakeTart()
	f.image(tahoe.Image, 31)
	f.image(sequoia.Image, 28)
	f.image(olderSequoia, nil) // Tart gave no size: the pin's pull_gb, an estimate
	f.image(olderLent, 27)
	f.image(runLent, 27)
	f.image(unrecorded, 27)
	f.image(owners, 5)
	f.image(ownersTag, 5)
	f.image(other, 3)
	f.vm("potluck-0123abcd-box", 29, true)
	f.vm("zmrt-89abcdef-kept", 29, false)
	f.vm("tahoe-diagnostic", 31, false)
	r := sequoiaMac(t)
	recordPulls(t, r, tahoe.Image, sequoia.Image, olderSequoia, olderLent, runLent)
	bindings := served(
		computeapi.RunTarget{Name: "mac-m", Kind: "box", Images: []string{sequoia.Image}, Config: map[string]string{"vm": "tart"}},
		computeapi.RunTarget{Name: "mac-old", Kind: "box", Images: []string{olderLent}},
		computeapi.RunTarget{Name: "runner", Kind: "run", Config: map[string]string{"image": runLent}}, // vm defaults to tart
		computeapi.RunTarget{Name: "acme", Kind: "box", Images: []string{owners}, Config: map[string]string{"vm": "tart"}},
		computeapi.RunTarget{Name: "far@m2", Kind: "box", Host: "m2", Images: []string{other}}, // another machine's: lends nothing here
	)
	ctx := context.Background()
	inv, err := ReadTartInventory(ctx, f, r, bindings)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]TartState{
		tahoe.Image: TartRemovable, olderSequoia: TartRemovable,
		sequoia.Image: TartInUse, olderLent: TartInUse, runLent: TartInUse, owners: TartInUse,
		unrecorded:  TartRetained,
		small.Image: TartAbsent,
		other:       TartNotPotluck, ownersTag: TartNotPotluck, "tahoe-diagnostic": TartNotPotluck,
		"potluck-0123abcd-box": TartInUse, "zmrt-89abcdef-kept": TartInUse,
	}
	if got := states(inv); !maps.Equal(got, want) {
		t.Fatalf("states\n got %v\nwant %v", got, want)
	}
	if gb, est := inv.ReclaimGB(); gb != 31+sequoia.PullGB || !est {
		t.Errorf("reclaim %d GB (estimated %t), want %d and an estimate", gb, est, 31+sequoia.PullGB)
	}
	for _, it := range inv.Items {
		switch it.Name {
		case tahoe.Image:
			if !strings.Contains(it.Why, "needs macOS 26+") || !strings.Contains(it.Why, "macOS 15.6") || !it.Measured || it.GB != 31 {
				t.Errorf("tahoe: %+v", it)
			}
		case small.Image:
			if it.Measured || it.GB != small.PullGB || !strings.Contains(it.Why, "a retry downloads it") {
				t.Errorf("small linux: %+v", it)
			}
		case unrecorded:
			if !it.Unclaimed || !strings.Contains(it.Why, "no record that it pulled") || !strings.Contains(it.Why, "name it to remove it") {
				t.Errorf("an unrecorded digest: %+v", it)
			}
		case runLent:
			if !strings.Contains(it.Why, "lent by runner") {
				t.Errorf("a run binding's image: %+v", it)
			}
		}
	}

	c, err := RemoveTartImages(ctx, f, r, bindings)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{olderSequoia, tahoe.Image}; !slices.Equal(f.deletes, want) {
		t.Fatalf("deleted %v, want only %v", f.deletes, want)
	}
	for _, rm := range c.Removals {
		if !rm.Removed || rm.Kept || rm.Note != "" {
			t.Errorf("removal %+v", rm)
		}
	}
	for _, kept := range []string{sequoia.Image, olderLent, runLent, unrecorded, owners, ownersTag, other, "potluck-0123abcd-box", "zmrt-89abcdef-kept", "tahoe-diagnostic"} {
		if f.entries[kept] == nil {
			t.Errorf("%s was removed", kept)
		}
	}
	rec, err := readRecord(r.TartDir)
	if err != nil || !rec.Images[tahoe.Image].Pulled.IsZero() || !rec.Images[olderSequoia].Pulled.IsZero() || rec.Images[sequoia.Image].Pulled.IsZero() {
		t.Errorf("record after the removal: %+v, %v", rec, err)
	}

	f.deletes = nil
	c, err = RemoveTartImages(ctx, f, r, bindings)
	if err != nil || len(c.Removals) != 0 || len(f.deletes) != 0 {
		t.Fatalf("second removal: %+v, %v, deleted %v", c, err, f.deletes)
	}
}

// The pinned repositories are public: an owner who ran
// `tart clone ghcr.io/cirruslabs/macos-tahoe-base:latest mine` holds a tag
// and its digest, and deleting the digest would drop the tag. A digest with
// a tag of its repository in Tart's list is kept, even one Potluck's record
// says it pulled.
func TestTartCleanupKeepsADigestATagMayName(t *testing.T) {
	tahoe, _, _ := tartPins(t)
	tag := imageRepository(tahoe.Image) + ":latest"
	f := newFakeTart()
	f.image(tahoe.Image, 31)
	f.image(tag, 31)
	f.vm("mine", 31, false)
	r := sequoiaMac(t)
	recordPulls(t, r, tahoe.Image)
	ctx := context.Background()
	inv, err := ReadTartInventory(ctx, f, r, served())
	if err != nil {
		t.Fatal(err)
	}
	if got := states(inv); got[tahoe.Image] != TartRetained || got[tag] != TartNotPotluck || got["mine"] != TartNotPotluck {
		t.Fatalf("states %v", got)
	}
	for _, it := range inv.Items {
		if it.Name == tahoe.Image && !strings.Contains(it.Why, tag) {
			t.Errorf("tahoe kept without naming the tag: %+v", it)
		}
	}
	if c, err := RemoveTartImages(ctx, f, r, served()); err != nil || len(c.Removals) != 0 || len(f.deletes) != 0 {
		t.Fatalf("removal %+v, %v, deleted %v", c, err, f.deletes)
	}
	if _, err := RemoveTartImages(ctx, f, r, served(), tahoe.Image); err == nil || !strings.Contains(err.Error(), tag) || len(f.deletes) != 0 {
		t.Fatalf("naming a digest a tag may name: %v, deleted %v", err, f.deletes)
	}
}

// A Mac whose failed setup pulled Tahoe on macOS 15 before Potluck kept a
// record of its pulls: Tahoe passes every check but the record, so it is
// retained and unclaimed; --remove alone deletes nothing, and naming it
// deletes it and nothing else. Naming an image something needs, or one Tart
// does not hold, deletes nothing at all; one a share began using since the
// first read is kept.
func TestTartCleanupRemovesAnUnclaimedImageOnlyByName(t *testing.T) {
	tahoe, sequoia, _ := tartPins(t)
	lent := imageRepository(sequoia.Image) + "@sha256:" + strings.Repeat("1", 64)
	setup := func() (*fakeTart, Report) {
		f := newFakeTart()
		f.image(tahoe.Image, 31)
		f.image(sequoia.Image, 28)
		f.image(lent, 27)
		return f, sequoiaMac(t) // no record: none existed when Tahoe was pulled
	}
	bindings := served(computeapi.RunTarget{Name: "mac-old", Kind: "box", Images: []string{lent}})
	ctx := context.Background()
	f, r := setup()
	inv, err := ReadTartInventory(ctx, f, r, bindings)
	if err != nil {
		t.Fatal(err)
	}
	if got := states(inv); got[tahoe.Image] != TartRetained || got[sequoia.Image] != TartRetained || got[lent] != TartInUse {
		t.Fatalf("states %v", got)
	}
	if un := inv.Unclaimed(); len(un) != 1 || un[0].Name != tahoe.Image || len(inv.Removable()) != 0 {
		t.Fatalf("unclaimed %+v, removable %+v", un, inv.Removable())
	}
	if c, err := RemoveTartImages(ctx, f, r, bindings); err != nil || len(c.Removals) != 0 || len(f.deletes) != 0 {
		t.Fatalf("--remove alone: %+v, %v, deleted %v", c, err, f.deletes)
	}
	for _, names := range [][]string{{lent}, {tahoe.Image, sequoia.Image}, {"reg.example/nope@sha256:" + strings.Repeat("9", 64)}} {
		if c, err := RemoveTartImages(ctx, f, r, bindings, names...); err == nil || !strings.HasPrefix(err.Error(), "nothing removed") || len(c.Removals) != 0 || len(f.deletes) != 0 {
			t.Fatalf("naming %v: %+v, %v, deleted %v", names, c, err, f.deletes)
		}
	}
	c, err := RemoveTartImages(ctx, f, r, bindings, tahoe.Image, tahoe.Image)
	if err != nil || !slices.Equal(f.deletes, []string{tahoe.Image}) || len(c.Removals) != 1 || !c.Removals[0].Removed {
		t.Fatalf("naming tahoe: %+v, %v, deleted %v", c, err, f.deletes)
	}

	// A size that began lending Tahoe after the first read keeps it.
	f, r = setup()
	reads := 0
	read := func(ctx context.Context) (Served, error) {
		if reads++; reads > 1 {
			return served(computeapi.RunTarget{Name: "mac-new", Kind: "box", Images: []string{tahoe.Image}})(ctx)
		}
		return bindings(ctx)
	}
	c, err = RemoveTartImages(ctx, f, r, read, tahoe.Image)
	if err != nil || len(f.deletes) != 0 || len(c.Removals) != 1 || !c.Removals[0].Kept || !strings.Contains(c.Removals[0].Note, "lent by mac-new") {
		t.Fatalf("a size began lending it: %+v, %v, deleted %v", c, err, f.deletes)
	}

	// A share of a newer build that found Tahoe already there (no claim)
	// and is its home's latest uses it: not unclaimed, and naming it is refused.
	f, r = setup()
	recordShare(t, r, testHome, time.Now(), []string{tahoe.Image})
	if _, err := RemoveTartImages(ctx, f, r, bindings, tahoe.Image); err == nil || !strings.Contains(err.Error(), "the latest share from "+testHome+" uses it") || len(f.deletes) != 0 {
		t.Fatalf("naming what a latest share uses: %v, deleted %v", err, f.deletes)
	}
}

// What a removal reads first can change before a delete: just before each
// delete it reads the inventory again, and an image a size began lending
// meanwhile is kept (no failure), while the rest are removed.
func TestTartCleanupReadsAgainBeforeEachDelete(t *testing.T) {
	tahoe, sequoia, _ := tartPins(t)
	older := imageRepository(sequoia.Image) + "@sha256:" + strings.Repeat("0", 64)
	f := newFakeTart()
	f.image(tahoe.Image, 31)
	f.image(older, 25)
	r := sequoiaMac(t)
	recordPulls(t, r, tahoe.Image, older)
	reads := 0
	read := func(ctx context.Context) (Served, error) {
		reads++
		if reads == 1 {
			return served()(ctx)
		}
		return served(computeapi.RunTarget{Name: "mac-new", Kind: "box", Images: []string{older}})(ctx)
	}
	c, err := RemoveTartImages(context.Background(), f, r, read)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(f.deletes, []string{tahoe.Image}) {
		t.Fatalf("deleted %v, want only %s", f.deletes, tahoe.Image)
	}
	for _, rm := range c.Removals {
		switch rm.Name {
		case older:
			if rm.Removed || !rm.Kept || !strings.Contains(rm.Note, "lent by mac-new") {
				t.Errorf("an image lent meanwhile: %+v", rm)
			}
		case tahoe.Image:
			if !rm.Removed || rm.Kept {
				t.Errorf("tahoe: %+v", rm)
			}
		}
	}
}

// A delete that fails is reported as it happened: an image still there is
// an error naming Tart's answer; one that went meanwhile is said to have
// been gone already, and is no error.
func TestTartCleanupReportsFailuresAndAbsence(t *testing.T) {
	tahoe, sequoia, _ := tartPins(t)
	older := imageRepository(sequoia.Image) + "@sha256:" + strings.Repeat("0", 64)
	f := newFakeTart()
	f.image(tahoe.Image, 31)
	f.image(older, 25)
	f.fail[tahoe.Image] = "Error: permission denied"
	f.gone[older] = true
	r := sequoiaMac(t)
	recordPulls(t, r, tahoe.Image, older)
	c, err := RemoveTartImages(context.Background(), f, r, served())
	if err == nil || !strings.Contains(err.Error(), tahoe.Image+" was not removed") || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("err %v", err)
	}
	for _, rm := range c.Removals {
		switch rm.Name {
		case tahoe.Image:
			if rm.Removed {
				t.Errorf("a failed delete counts as removed: %+v", rm)
			}
		case older:
			if !rm.Removed || !strings.Contains(rm.Note, "already gone") {
				t.Errorf("an image gone meanwhile: %+v", rm)
			}
		}
	}
	if f.entries[tahoe.Image] == nil {
		t.Error("the fake lost the image it refused to delete")
	}
}

// An image is removable only while every Potluck home whose share pulled
// or reused it was read: a stopped daemon's sizes, or another program's,
// may lend it. One Potluck found already there (reused, never pulled) is
// never removable, though a later share may have used it too.
func TestTartCleanupNeedsEveryHomeRead(t *testing.T) {
	tahoe, sequoia, _ := tartPins(t)
	older := imageRepository(sequoia.Image) + "@sha256:" + strings.Repeat("0", 64)
	f := newFakeTart()
	f.image(tahoe.Image, 31)
	f.image(older, 25)
	third := imageRepository(sequoia.Image) + "@sha256:" + strings.Repeat("6", 64)
	f.image(third, 25)
	r := sequoiaMac(t)
	recordPulls(t, r, tahoe.Image, third) // only testHome's shares name third
	at := time.Now().Add(-time.Hour)
	recordShare(t, r, "/h/zm", at, []string{tahoe.Image})
	recordShare(t, r, "/h/zm", at.Add(time.Minute), nil)
	recordShare(t, r, testHome, at.Add(2*time.Minute), []string{older}) // reused, never pulled
	recordShare(t, r, testHome, at.Add(3*time.Minute), nil)
	ctx := context.Background()
	inv, err := ReadTartInventory(ctx, f, r, served())
	if err != nil {
		t.Fatal(err)
	}
	if got := states(inv); got[tahoe.Image] != TartRetained || got[older] != TartRetained || got[third] != TartRetained {
		t.Fatalf("states %v", got)
	}
	for _, it := range inv.Items {
		if (it.Name == tahoe.Image || it.Name == third) && !strings.Contains(it.Why, "/h/zm, which no longer exists here") {
			t.Errorf("kept without naming the unread home: %+v", it)
		}
	}
	if c, err := RemoveTartImages(ctx, f, r, served()); err != nil || len(c.Removals) != 0 || len(f.deletes) != 0 {
		t.Fatalf("removal %+v, %v, deleted %v", c, err, f.deletes)
	}
	both := func(context.Context) (Served, error) { return Served{Homes: []string{testHome, "/h/zm"}}, nil }
	if inv, err := ReadTartInventory(ctx, f, r, both); err != nil || states(inv)[tahoe.Image] != TartRemovable || states(inv)[third] != TartRemovable || states(inv)[older] != TartRetained {
		t.Fatalf("both homes read: %v, %v", states(inv), err)
	}
}

// Each program judges by its own pins: an image this one takes for an
// earlier digest may be what another program's latest share on its home
// lends (its retry after a failed share, or a share still running). While
// any home's latest share uses an image it is kept; once a later share
// there uses others, it is removable.
func TestTartCleanupKeepsWhatALatestShareUses(t *testing.T) {
	_, sequoia, _ := tartPins(t)
	theirs := imageRepository(sequoia.Image) + "@sha256:" + strings.Repeat("7", 64)
	f := newFakeTart()
	f.image(theirs, 27)
	r := sequoiaMac(t)
	at := time.Now().Add(-time.Hour)
	recordShare(t, r, "/h/zm", at, []string{theirs}, theirs)
	read := func(context.Context) (Served, error) { return Served{Homes: []string{testHome, "/h/zm"}}, nil }
	ctx := context.Background()
	inv, err := ReadTartInventory(ctx, f, r, read)
	if err != nil || states(inv)[theirs] != TartRetained || !strings.Contains(inv.Items[0].Why, "the latest share from /h/zm uses it") {
		t.Fatalf("%+v, %v", inv, err)
	}
	if c, err := RemoveTartImages(ctx, f, r, read); err != nil || len(c.Removals) != 0 || len(f.deletes) != 0 {
		t.Fatalf("removal %+v, %v, deleted %v", c, err, f.deletes)
	}
	recordShare(t, r, "/h/zm", at.Add(time.Minute), []string{sequoia.Image})
	if inv, err := ReadTartInventory(ctx, f, r, read); err != nil || states(inv)[theirs] != TartRemovable {
		t.Fatalf("after a later share: %v, %v", states(inv), err)
	}
}

// What Tart no longer holds loses Potluck's claim, not the homes that used
// it: a stopped daemon's sizes may lend it again once it is pulled back,
// and that home stays one whose daemon must be read.
func TestTartCleanupForgetsOnlyTheClaim(t *testing.T) {
	tahoe, sequoia, _ := tartPins(t)
	gone := imageRepository(sequoia.Image) + "@sha256:" + strings.Repeat("8", 64)
	f := newFakeTart()
	f.image(tahoe.Image, 31)
	r := sequoiaMac(t)
	recordPulls(t, r, tahoe.Image, gone)
	at := time.Now().Add(-time.Hour)
	recordShare(t, r, "/h/zm", at, []string{gone})
	recordShare(t, r, "/h/zm", at.Add(time.Minute), nil)
	if _, err := RemoveTartImages(context.Background(), f, r, served()); err != nil {
		t.Fatal(err)
	}
	rec, err := readRecord(r.TartDir)
	if u := rec.Images[gone]; err != nil || !u.Pulled.IsZero() || u.Homes["/h/zm"].IsZero() || u.Homes[testHome].IsZero() {
		t.Fatalf("record %+v, %v", rec, err)
	}
}

// A share that starts during an image's last read before its delete marks
// its images only once the delete is done (it then finds the image gone, and
// pulls it): the removal holds the record's lock across that read and the
// delete.
func TestTartCleanupHoldsTheRecordAcrossADelete(t *testing.T) {
	tahoe, _, _ := tartPins(t)
	f := newFakeTart()
	f.image(tahoe.Image, 31)
	r := sequoiaMac(t)
	recordPulls(t, r, tahoe.Image)
	marked := make(chan error, 1)
	reads := 0
	read := func(ctx context.Context) (Served, error) {
		if reads++; reads == 2 { // the last read before the delete
			go func() { marked <- StartTartShare(r.TartDir, testHome, []string{tahoe.Image}, time.Now()) }()
			time.Sleep(200 * time.Millisecond) // time to mark, were the record not locked
		}
		return served()(ctx)
	}
	f.onDelete = func(string) {
		select {
		case <-marked:
			t.Error("a share marked its images between the last read and the delete")
		default:
		}
	}
	if _, err := RemoveTartImages(context.Background(), f, r, read); err != nil || !slices.Equal(f.deletes, []string{tahoe.Image}) {
		t.Fatalf("removal: %v, deleted %v", err, f.deletes)
	}
	select {
	case err := <-marked:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the share never marked its images")
	}
}

// When what this machine shares cannot be read, an image a size may lend
// would look unused: none is removable and a removal deletes nothing. Nor
// when Potluck's record of its pulls cannot be read. A Mac whose macOS is
// unknown is refused: which pins it can lend is unknown.
func TestTartCleanupFailsClosed(t *testing.T) {
	tahoe, _, _ := tartPins(t)
	f := newFakeTart()
	f.image(tahoe.Image, 31)
	ctx := context.Background()
	r := sequoiaMac(t)
	recordPulls(t, r, tahoe.Image)
	unread := func(context.Context) (Served, error) { return Served{}, errors.New("daemon not running") }
	inv, err := ReadTartInventory(ctx, f, r, unread)
	if err != nil || states(inv)[tahoe.Image] != TartRetained || len(inv.Removable()) != 0 {
		t.Fatalf("bindings unread: %+v, %v", inv, err)
	}
	if c, err := RemoveTartImages(ctx, f, r, unread); err != nil || len(c.Removals) != 0 || len(f.deletes) != 0 {
		t.Fatalf("bindings unread: removal %+v, %v, deleted %v", c, err, f.deletes)
	}
	bad := sequoiaMac(t)
	if err := os.WriteFile(filepath.Join(bad.TartDir, TartPullsFile), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if inv, err := ReadTartInventory(ctx, f, bad, served()); err != nil || states(inv)[tahoe.Image] != TartRetained {
		t.Fatalf("record unread: %+v, %v", inv, err)
	}
	r.MacOS = ""
	if _, err := RemoveTartImages(ctx, f, r, served()); err == nil || len(f.deletes) != 0 {
		t.Fatalf("unknown macOS: %v, deleted %v", err, f.deletes)
	}
}
