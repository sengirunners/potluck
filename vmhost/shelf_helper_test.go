package vmhost

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A freshly made shelf (the volume, nothing published, nothing staged, no
// box has asked for anything) gets the helper from the filler's first start,
// written by the publisher 0555, with shelf_fill=boxes and seed alike; the
// sweep stops asking for it once it is published, and the filler puts it
// back when the volume lost it. With shelf_fill=off nothing is written.
func TestFreshShelfGetsTheHelper(t *testing.T) {
	for _, fill := range []string{"boxes", "seed"} {
		t.Run(fill, func(t *testing.T) {
			o, dir, vol := newShelfOCI(t)
			cfg := shelfFillCfg(o, 1000)
			cfg.Shelf.Fill = fill
			if !shelfHelperDue(cfg) {
				t.Fatal("a fresh shelf: the helper is not due")
			}
			filler := func() {
				t.Helper()
				var log strings.Builder
				if code := shelfFiller(o.stateDir, strings.NewReader(rawConfigOf(t, cfg, o)), &log); code != 0 {
					t.Fatalf("filler exit %d: %s", code, log.String())
				}
			}
			filler()
			p := filepath.Join(vol, shelfHelperPath)
			fi, err := os.Stat(p)
			if err != nil {
				t.Fatalf("no helper on a fresh shelf after the filler started: %v", err)
			}
			if fi.Mode().Perm() != 0o555 {
				t.Errorf("helper mode %v, want 0555", fi.Mode().Perm())
			}
			if b, _ := os.ReadFile(p); !bytes.Equal(b, shelfHelper) {
				t.Error("the helper on the shelf is not shelfHelper")
			}
			if b, _ := os.ReadFile(filepath.Join(dir, "calls")); !strings.Contains(string(b), "--network none") {
				t.Errorf("the helper was not written by the publisher's container: %s", b)
			}
			if shelfHelperDue(cfg) {
				t.Error("still due after it was published")
			}
			if err := os.Remove(p); err != nil {
				t.Fatal(err)
			}
			filler()
			if _, err := os.Stat(p); err != nil {
				t.Errorf("the filler did not put back a helper the shelf lost: %v", err)
			}
		})
	}
	t.Run("off", func(t *testing.T) {
		o, _, vol := newShelfOCI(t)
		cfg := shelfFillCfg(o, 1000)
		cfg.Shelf.Fill = "off"
		if shelfHelperDue(cfg) {
			t.Error("due with shelf_fill=off")
		}
		ensureShelfHelper(cfg, o, &strings.Builder{})
		if _, err := os.Stat(filepath.Join(vol, shelfHelperPath)); err == nil {
			t.Error("helper written with shelf_fill=off")
		}
	})
}

// An upgraded driver's helper (other bytes than the one stamped) is due
// again, so the sweep starts a filler that replaces it; the sweep does not
// start one on every call while the helper cannot be published.
func TestShelfHelperUpgradeAndRetry(t *testing.T) {
	o, _, vol := newShelfOCI(t)
	cfg := shelfFillCfg(o, 1000)
	ensureShelfHelper(cfg, o, &strings.Builder{})
	if shelfHelperDue(cfg) {
		t.Fatal("due right after publishing")
	}
	old := shelfHelper
	shelfHelper = []byte("#!/bin/sh\necho newer helper\n")
	t.Cleanup(func() { shelfHelper = old })
	if !shelfHelperDue(cfg) {
		t.Fatal("a new helper is not due")
	}
	stampShelfHelperTry(cfg.StateDir)
	if shelfHelperDue(cfg) {
		t.Error("due again right after a filler was started for it")
	}
	ensureShelfHelper(cfg, o, &strings.Builder{})
	if b, _ := os.ReadFile(filepath.Join(vol, shelfHelperPath)); !bytes.Equal(b, shelfHelper) {
		t.Error("the new helper did not replace the old one")
	}
}

// A helper with other bytes on the volume (a volume made again, another
// size's older driver) is replaced once the stamp is shelfHelperCheck old;
// a publisher that refuses leaves no stamp, so the helper stays due; the
// sweep starts a filler for a due helper and not again within
// shelfHelperRetry.
func TestShelfHelperFollowsTheVolume(t *testing.T) {
	o, _, vol := newShelfOCI(t)
	cfg := shelfFillCfg(o, 1000)
	ensureShelfHelper(cfg, o, &strings.Builder{})
	p := filepath.Join(vol, shelfHelperPath)
	if err := os.Chmod(p, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("#!/bin/sh\necho old\n"), 0o555); err != nil {
		t.Fatal(err)
	}
	if shelfHelperDue(cfg) {
		t.Fatal("due with a fresh stamp")
	}
	old := time.Now().Add(-shelfHelperCheck - time.Minute)
	if err := os.Chtimes(shelfHelperStamp(cfg.StateDir), old, old); err != nil {
		t.Fatal(err)
	}
	if !shelfHelperDue(cfg) {
		t.Fatal("not due with an old stamp")
	}
	ensureShelfHelper(cfg, o, &strings.Builder{})
	if b, _ := os.ReadFile(p); !bytes.Equal(b, shelfHelper) {
		t.Error("other bytes on the volume were not replaced")
	}

	o2, _, vol2 := newShelfOCI(t)
	cfg2 := shelfFillCfg(o2, 1000)
	stage(t, o2, "not-a-blob", []byte("x")) // the publisher refuses this staging
	var log strings.Builder
	ensureShelfHelper(cfg2, o2, &log)
	if _, err := os.Stat(filepath.Join(vol2, shelfHelperPath)); err == nil {
		t.Error("helper written by a refused publish")
	}
	if !shelfHelperDue(cfg2) || !strings.Contains(log.String(), "publishing the helper") {
		t.Errorf("a refused publish: due %v, log %s", shelfHelperDue(cfg2), log.String())
	}
	d := &drv{cfg: cfg2, st: store{o2.stateDir}, vm: o2}
	stampShelfEvict(cfg2.StateDir) // only the helper makes the sweep start one
	d.sweepShelfQueue()
	if _, err := os.Stat(shelfHelperTry(cfg2.StateDir)); err != nil {
		t.Errorf("the sweep did not start a filler for a due helper: %v", err)
	}
	if shelfHelperDue(cfg2) {
		t.Error("due again right after the sweep started one")
	}
}
