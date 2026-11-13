package protocol

import (
	"time"
	"unicode/utf8"
)

// RunnerRefresh is what a box driver's GitHub runner refresh found and did
// for one image (RunnerStatus.Refresh). The set is closed: a reader treats
// any other value as no result.
type RunnerRefresh string

const (
	RunnerNotNeeded RunnerRefresh = "not_needed" // the image's runner was recent enough
	RunnerRefreshed RunnerRefresh = "refreshed"  // it was too old and the driver replaced it
	RunnerFailed    RunnerRefresh = "failed"     // it could not be read, or replacing it failed: that box got no runner
)

// Valid says whether r is one of the defined states.
func (r RunnerRefresh) Valid() bool {
	switch r {
	case RunnerNotNeeded, RunnerRefreshed, RunnerFailed:
		return true
	}
	return false
}

// RunnerWhy is why a refresh failed, as a short fixed reason. It never holds
// a path or a command's output: the detail goes to the caller of acquire and
// the host's own log. The set is closed.
type RunnerWhy string

const (
	WhyVersionUnreadable   RunnerWhy = "version unreadable"
	WhyUnsupportedPlatform RunnerWhy = "no pinned runner for this platform"
	WhyDownloadFailed      RunnerWhy = "download failed"
	WhyChecksumMismatch    RunnerWhy = "checksum mismatch"
	WhyHostCache           RunnerWhy = "host cache unavailable"
	WhyInstallFailed       RunnerWhy = "install failed in the box"
	WhyRunnerUnusable      RunnerWhy = "runner unusable after install"
	WhyTimedOut            RunnerWhy = "timed out"
)

// Valid says whether w is one of the defined reasons.
func (w RunnerWhy) Valid() bool {
	switch w {
	case WhyVersionUnreadable, WhyUnsupportedPlatform, WhyDownloadFailed, WhyChecksumMismatch,
		WhyHostCache, WhyInstallFailed, WhyRunnerUnusable, WhyTimedOut:
		return true
	}
	return false
}

// RunnerStatus is the newest GitHub runner refresh result for one image of a
// box binding, as describe's "runners" lists it (one entry per image). The
// driver owns the minimum version and the release it installs; this is only
// the result.
type RunnerStatus struct {
	// Image is the binding's image the box ran, as the binding names it
	// (its pinned reference).
	Image string `json:"image"`
	// Version is the runner the box ended up with; empty when it could not be
	// read, and when Refresh is "failed" (the box did not end with a working
	// runner).
	Version string `json:"version"`
	// ImageVersion is what the image shipped: set with "refreshed" and with
	// "failed" when the image's version could be read.
	ImageVersion string        `json:"image_version,omitempty"`
	Refresh      RunnerRefresh `json:"refresh"`
	// Why is set when Refresh is "failed".
	Why RunnerWhy `json:"why,omitempty"`
	At  time.Time `json:"at"`
}

// Bounds on what a reader keeps of a status a host wrote.
const (
	// MaxRunners is how many results one describe lists.
	MaxRunners = 32
	// MaxRunnerImage is the longest image reference a result may name. A
	// driver refuses a binding whose image is longer (vmhost checks it where
	// it checks the pin), so a valid image can always be reported.
	MaxRunnerImage   = 300
	maxRunnerVersion = 64
)

// plainText says s is valid UTF-8 with no control character.
func plainText(s string) bool {
	if !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) {
			return false
		}
	}
	return true
}

// PlainRunnerVersion says v is a version a result may carry: bounded, one
// line, no control characters ("" is none).
func PlainRunnerVersion(v string) bool { return len(v) <= maxRunnerVersion && plainText(v) }

// Valid says whether s is a result a reader may use: a known state, a time,
// an image and versions of bounded plain text, and a reason only from the
// closed set.
func (s RunnerStatus) Valid() bool {
	return s.Refresh.Valid() && !s.At.IsZero() && s.Image != "" && len(s.Image) <= MaxRunnerImage && plainText(s.Image) &&
		PlainRunnerVersion(s.Version) && PlainRunnerVersion(s.ImageVersion) &&
		(s.Why == "" || s.Why.Valid()) && (s.Refresh == RunnerFailed || s.Why == "")
}

// CleanRunners keeps the valid entries of in, one per image (the first), at
// most max, in their order. When more than max are valid, the cut keeps the
// "failed" ones first (a failure is what a reader must not miss), then the
// rest in order. It returns a new slice, nil when none are left.
func CleanRunners(in []RunnerStatus, max int) []RunnerStatus {
	var valid []RunnerStatus
	seen := map[string]bool{}
	for _, s := range in {
		if !s.Valid() || seen[s.Image] {
			continue
		}
		seen[s.Image] = true
		valid = append(valid, s)
	}
	if len(valid) <= max {
		return valid
	}
	keep := make([]bool, len(valid))
	room := max
	for _, failed := range []bool{true, false} {
		for i, s := range valid {
			if room > 0 && (s.Refresh == RunnerFailed) == failed {
				keep[i] = true
				room--
			}
		}
	}
	var out []RunnerStatus
	for i, s := range valid {
		if keep[i] {
			out = append(out, s)
		}
	}
	return out
}
