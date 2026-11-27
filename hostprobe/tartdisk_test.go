package hostprobe

import (
	"context"
	"strings"
	"testing"
)

// notSetuidMac is a Mac with Softnet installed but not setuid (a root step
// away from ready) and freeGB free where Tart stores images.
func notSetuidMac(freeGB float64) Report {
	f := mac(true)
	f.disk["/Users/o/.tart"] = freeGB
	return Detect(context.Background(), macRunner("-rwxr-xr-x root", ""), f, "/state")
}

// Precheck, which `potluck host` runs before asking for sudo, reads Tart's
// cache as Share does: a cached default image on a small disk is not refused
// there, and an image it must pull is.
func TestPrecheckReadsTartsCacheLikeShare(t *testing.T) {
	pin, err := DefaultImage("tart")
	if err != nil {
		t.Fatal(err)
	}
	o := tartShareOptions(nil)
	o.Report, o.Images = notSetuidMac(5), nil
	if err := precheckOn(context.Background(), tartListing(t, pin.Image), o); err != nil {
		t.Errorf("cached image, 5 GB free: %v", err)
	}
	err = precheckOn(context.Background(), tartListing(t), o)
	if err == nil || !strings.Contains(err.Error(), "not enough disk") {
		t.Errorf("image not cached, 5 GB free: %v", err)
	}
}

// Unknown digest compatibility is refused even when the disk is large or
// unreadable; the disk budget cannot establish image host compatibility.
func TestShareRefusesUnknownImageBeforeDiskCheck(t *testing.T) {
	pin, _ := DefaultImage("tart")
	repo, _, _ := strings.Cut(pin.Image, "@")
	for _, image := range []string{repo + "@sha256:" + strings.Repeat("0", 64), "reg.example/other@sha256:" + strings.Repeat("1", 64)} {
		for _, free := range []float64{5, 100} {
			o := tartShareOptions(nil)
			o.Report, o.Images, o.DryRun = macReportWithDisk(t, free), []string{image}, true
			_, err := shareOn(context.Background(), neverRuns(t), nil, o)
			if err == nil || !strings.Contains(err.Error(), "unknown host compatibility") {
				t.Fatalf("%s: %v", image, err)
			}
		}
	}
}

// A pinned repository given by tag or name takes its pin's budget too.
func TestTartDiskBudgetsAPinnedRepositoryByTagOrName(t *testing.T) {
	pin, _ := DefaultImage("tart")
	repo, _, _ := strings.Cut(pin.Image, "@")
	for _, image := range []string{repo, repo + ":latest"} {
		r := macReportWithDisk(t, 5)
		_, err := tartDiskCheck(context.Background(), tartListing(t), r, "", []string{image})
		if err == nil || !strings.Contains(err.Error(), "at least 60 GB") {
			t.Errorf("%s: err %v", image, err)
		}
	}
}

// When Tart's image list cannot be read, the refusal says so, and Tart is
// asked once however many images are lent.
func TestShareRefusalSaysTartsListCouldNotBeRead(t *testing.T) {
	xcode, _ := selectedPin(macReport(), "tart", "xcode")
	pin, _ := DefaultImage("tart")
	for name, res := range map[string]Result{
		"exit":     {Exit: 1, Stderr: "boom"},
		"bad json": ok("not json"),
	} {
		runs := 0
		run := &fakeRunner{on: func(line string) (Result, bool) {
			if line != "/fake/tart list --format json" {
				t.Errorf("ran %q", line)
			}
			runs++
			return res, true
		}}
		o := tartShareOptions(nil)
		o.Report, o.Images, o.DryRun = macReportWithDisk(t, 5), []string{pin.Image, xcode.Image}, true
		_, err := shareOn(context.Background(), run, nil, o)
		if err == nil || !strings.Contains(err.Error(), "image list could not be read") {
			t.Errorf("%s: err %v", name, err)
		}
		if runs != 1 {
			t.Errorf("%s: tart list ran %d times", name, runs)
		}
	}
}

// The refusal says the daemon must see TART_HOME too, and names only a
// command that exists.
func TestShareDiskRefusalNamesTheDaemonsTartHome(t *testing.T) {
	o := tartShareOptions(nil)
	o.Report, o.Images, o.DryRun = macReportWithDisk(t, 5), nil, true
	_, err := shareOn(context.Background(), tartListing(t), nil, o)
	if err == nil || !strings.Contains(err.Error(), "daemon") || !strings.Contains(err.Error(), "TART_HOME") || !strings.Contains(err.Error(), "potluck daemon stop") {
		t.Errorf("err %v", err)
	}
}
