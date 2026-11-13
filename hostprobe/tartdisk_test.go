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

// An image with no pin never needs 0 GB by silence: a digest of a pinned
// repository takes that pin's budget, any other image the default image's,
// and share's notes (or the refusal) say the budget is an estimate.
func TestShareBudgetsAnImageWithNoPin(t *testing.T) {
	pin, err := DefaultImage("tart")
	if err != nil {
		t.Fatal(err)
	}
	repo, _, _ := strings.Cut(pin.Image, "@")
	older := repo + "@sha256:" + strings.Repeat("0", 64)
	other := "reg.example/other@sha256:" + strings.Repeat("1", 64)
	for _, image := range []string{older, other} {
		o := tartShareOptions(nil)
		o.Report, o.Images, o.DryRun = macReportWithDisk(t, 5), []string{image}, true
		_, err := shareOn(context.Background(), tartListing(t), nil, o)
		if err == nil || !strings.Contains(err.Error(), "at least 60 GB") || !strings.Contains(err.Error(), "estimate") {
			t.Errorf("%s, 5 GB free: err %v", image, err)
		}
		o.Report = macReportWithDisk(t, 100)
		res, err := shareOn(context.Background(), tartListing(t), nil, o)
		if err != nil || !strings.Contains(strings.Join(res.Notes, "|"), "estimate") {
			t.Errorf("%s, 100 GB free: err %v notes %q", image, err, res.Notes)
		}
	}
	// An unreadable disk names the estimate, never "about 0 GB".
	o := tartShareOptions(nil)
	r := Detect(context.Background(), macRunner("-rwsr-xr-x root", ""), unreadableDisk{mac(true)}, "/state")
	o.Report, o.Images, o.DryRun = r, []string{other}, true
	res, err := shareOn(context.Background(), neverRuns(t), nil, o)
	if n := strings.Join(res.Notes, "|"); err != nil || strings.Contains(n, "about 0 GB") || !strings.Contains(n, "about 60 GB") || !strings.Contains(n, "estimate") {
		t.Errorf("unreadable disk: err %v notes %q", err, res.Notes)
	}
}

// A pinned repository given by tag or name takes its pin's budget too.
func TestTartDiskBudgetsAPinnedRepositoryByTagOrName(t *testing.T) {
	pin, _ := DefaultImage("tart")
	repo, _, _ := strings.Cut(pin.Image, "@")
	for _, image := range []string{repo, repo + ":latest"} {
		r := macReportWithDisk(t, 5)
		_, err := tartDiskCheck(context.Background(), tartListing(t), r, []string{image})
		if err == nil || !strings.Contains(err.Error(), "at least 60 GB") {
			t.Errorf("%s: err %v", image, err)
		}
	}
}

// When Tart's image list cannot be read, the refusal says so, and Tart is
// asked once however many images are lent.
func TestShareRefusalSaysTartsListCouldNotBeRead(t *testing.T) {
	pin, _ := DefaultImage("tart")
	for name, res := range map[string]Result{
		"exit":     {Exit: 1, Stderr: "boom"},
		"bad json": ok("not json"),
	} {
		runs := 0
		run := &fakeRunner{on: func(line string) (Result, bool) {
			if line != "tart list --format json" {
				t.Errorf("ran %q", line)
			}
			runs++
			return res, true
		}}
		o := tartShareOptions(nil)
		o.Report, o.Images, o.DryRun = macReportWithDisk(t, 5), []string{pin.Image, pin.Image[:len(pin.Image)-1] + "0"}, true
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
