package hostprobe

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/sengirunners/potluck/computeapi"
)

// leaveOf is the Tart of r's Mac as the Potluck at testHome leaves it, with
// the other daemons' homes and bindings others answers.
func leaveOf(r Report, others Served, err error) TartLeave {
	return TartLeave{CLI: "/fake/tart", Dir: r.TartDir, Home: testHome, Others: func(context.Context) (Served, error) { return others, err }}
}

const ownDigest = "ghcr.io/me/mine@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

// The images a share of Potluck's pulled go, the current pin too; the
// owner's own, a tag, an image with no record and every VM stay.
func TestLeavingRemovesOnlyWhatPotluckPulled(t *testing.T) {
	ctx := context.Background()
	_, sequoia, small := tartPins(t)
	r, f := sequoiaMac(t), newFakeTart()
	f.image(sequoia.Image, 27)
	f.image(small.Image, 3) // there before Potluck: no record of a pull
	f.image(ownDigest, 9)
	f.image("ghcr.io/me/mine2:latest", 9)
	f.vm("my-vm", 40, true)
	f.vm("potluck-01234567-x", 5, false)
	recordShare(t, r, testHome, time.Now(), []string{sequoia.Image, small.Image}, sequoia.Image)
	l := leaveOf(r, Served{}, nil)
	now, err := l.Read(ctx, f)
	if err != nil || len(now.Removable()) != 1 || now.Empty() || len(now.Kept()) != 5 || len(f.deletes) != 0 {
		t.Fatalf("%+v %v", now, err)
	}
	c, err := l.RemoveImages(ctx, f)
	if err != nil || !slices.Equal(f.deletes, []string{sequoia.Image}) || len(c.Removals) != 1 || !c.Removals[0].Removed {
		t.Fatalf("%v %+v %v", f.deletes, c, err)
	}
	rec, err := readRecord(r.TartDir)
	if err != nil || len(rec.Shares) != 0 || !rec.Images[sequoia.Image].Pulled.IsZero() {
		t.Fatalf("record %+v %v", rec, err)
	}
	if _, err := l.RemoveImages(ctx, f); err != nil || len(f.deletes) != 1 {
		t.Fatalf("a second run: %v %v", f.deletes, err)
	}
}

func TestLeavingIsEmptyOncePotlucksImagesAreGone(t *testing.T) {
	_, sequoia, _ := tartPins(t)
	r, f := sequoiaMac(t), newFakeTart()
	f.image(sequoia.Image, 27)
	recordShare(t, r, testHome, time.Now(), []string{sequoia.Image}, sequoia.Image)
	if now, err := leaveOf(r, Served{}, nil).Read(context.Background(), f); err != nil || !now.Empty() {
		t.Fatalf("%+v %v", now, err)
	}
}

// What Tart's list cannot show keeps Tart: another directory's VMs, a VM
// being made.
func TestLeavingIsNotEmptyWithWhatTheListLacks(t *testing.T) {
	for name, set := range map[string]func(r Report, f *fakeTart){
		"no record here": func(r Report, _ *fakeTart) { _ = os.Remove(filepath.Join(r.TartDir, TartPullsFile)) },
		"a VM folder":    func(r Report, _ *fakeTart) { _ = os.MkdirAll(filepath.Join(r.TartDir, "vms", "new"), 0o755) },
	} {
		r, f := sequoiaMac(t), newFakeTart()
		recordShare(t, r, testHome, time.Now(), nil)
		l := leaveOf(r, Served{}, nil)
		if now, err := l.Read(context.Background(), f); err != nil || !now.Empty() {
			t.Fatalf("%s: before: %+v %v", name, now, err)
		}
		set(r, f)
		if now, err := l.Read(context.Background(), f); err != nil || now.Empty() {
			t.Errorf("%s: %+v %v", name, now, err)
		}
	}
}

// A home that stays may share again, so the record still names it.
func TestLeavingWithTheHomeKeptStaysInTheRecord(t *testing.T) {
	_, sequoia, _ := tartPins(t)
	r, f := sequoiaMac(t), newFakeTart()
	f.image(sequoia.Image, 27)
	recordShare(t, r, testHome, time.Now(), []string{sequoia.Image}, sequoia.Image)
	l := leaveOf(r, Served{}, nil)
	l.KeepHome = true
	if _, err := l.RemoveImages(context.Background(), f); err != nil || len(f.deletes) != 1 {
		t.Fatalf("%v %v", f.deletes, err)
	}
	if rec, err := readRecord(r.TartDir); err != nil || len(rec.Shares) != 1 {
		t.Fatalf("%+v %v", rec, err)
	}
}

func TestLeavingSeesARunningTart(t *testing.T) {
	r, f := sequoiaMac(t), newFakeTart()
	f.ps = "/sbin/launchd\n/Users/me/Applications/tart.app/Contents/MacOS/tart\n"
	if now, err := leaveOf(r, Served{}, nil).Read(context.Background(), f); err != nil || !now.Busy {
		t.Errorf("%+v %v", now, err)
	}
}

// Whatever may still need an image, or cannot be read, keeps it and Tart.
func TestLeavingKeepsWhatMayBeNeeded(t *testing.T) {
	_, sequoia, _ := tartPins(t)
	other := computeapi.RunTarget{Name: "theirs", Images: []string{sequoia.Image}}
	for name, c := range map[string]struct {
		others Served
		err    error
		set    func(r Report, f *fakeTart)
	}{
		"another daemon lends it":   {others: Served{Targets: []computeapi.RunTarget{other}, Homes: []string{"/h/zm"}}},
		"another home shared it":    {set: func(r Report, _ *fakeTart) { recordShare(t, r, "/h/zm", time.Now(), []string{sequoia.Image}) }},
		"another home shared since": {set: func(r Report, _ *fakeTart) { recordShare(t, r, "/h/zm", time.Now(), nil) }},
		"a daemon cannot be read":   {err: errors.New("no answer")},
		"a tag of its repository":   {set: func(_ Report, f *fakeTart) { f.image(imageRepository(sequoia.Image)+":latest", 27) }},
		"another daemon is here":    {others: Served{Homes: []string{"/h/zm"}}},
		"the owner pulled it again": {set: func(r Report, _ *fakeTart) {
			now := time.Now().Add(time.Minute)
			_ = os.Chtimes(filepath.Join(cachedAt(r, sequoia.Image), "config.json"), now, now)
		}},
		"its folder cannot be read":      {set: func(r Report, _ *fakeTart) { _ = os.RemoveAll(cachedAt(r, sequoia.Image)) }},
		"the record's claim was dropped": {set: func(r Report, _ *fakeTart) { forgetGone(r.TartDir, map[string]bool{}) }},
	} {
		r, f := sequoiaMac(t), newFakeTart()
		f.image(sequoia.Image, 27)
		recordShare(t, r, testHome, time.Now(), []string{sequoia.Image}, sequoia.Image)
		if c.set != nil {
			c.set(r, f)
		}
		l := leaveOf(r, c.others, c.err)
		now, err := l.Read(context.Background(), f)
		if err != nil || now.Empty() || len(now.Removable()) != 0 {
			t.Errorf("%s: %+v %v", name, now, err)
		}
		if _, err := l.RemoveImages(context.Background(), f); (err != nil) != (now.Unread != nil) || len(f.deletes) != 0 {
			t.Errorf("%s: deleted %v (%v)", name, f.deletes, err)
		}
	}
}

// What changes between the plan and a delete is read again under the lock.
func TestLeavingReadsAgainBeforeEachDelete(t *testing.T) {
	_, sequoia, small := tartPins(t)
	r, f := sequoiaMac(t), newFakeTart()
	f.image(sequoia.Image, 27)
	f.image(small.Image, 3)
	recordShare(t, r, testHome, time.Now(), []string{sequoia.Image, small.Image}, sequoia.Image, small.Image)
	first := min(sequoia.Image, small.Image)
	second := max(sequoia.Image, small.Image)
	f.onDelete = func(string) { f.image(imageRepository(second)+":mine", 1) }
	if _, err := leaveOf(r, Served{}, nil).RemoveImages(context.Background(), f); err != nil || !slices.Equal(f.deletes, []string{first}) {
		t.Fatalf("deleted %v (%v)", f.deletes, err)
	}
}

// An image whose files are there and cannot be read is unread, not changed: a
// removal stops to be run again.
func TestLeavingWithAnUnreadableConfigIsUnread(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads it")
	}
	_, sequoia, _ := tartPins(t)
	r, f := sequoiaMac(t), newFakeTart()
	f.image(sequoia.Image, 27)
	recordShare(t, r, testHome, time.Now(), []string{sequoia.Image}, sequoia.Image)
	dir := filepath.Dir(tartConfig(r.TartDir, sequoia.Image))
	if err := os.Chmod(dir, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	l := leaveOf(r, Served{}, nil)
	if now, err := l.Read(context.Background(), f); err != nil || now.Unread == nil || len(now.Removable()) != 0 {
		t.Fatalf("%+v %v", now, err)
	}
	if _, err := l.RemoveImages(context.Background(), f); err == nil || len(f.deletes) != 0 {
		t.Fatalf("%v %v", err, f.deletes)
	}
}
