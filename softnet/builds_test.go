package softnet

import (
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strconv"
	"strings"
	"testing"
)

func TestTheLicenseIsTheUpstreamOne(t *testing.T) {
	sum := sha256.Sum256([]byte(License))
	if got := hex.EncodeToString(sum[:]); got != licenseSHA256 {
		t.Fatalf("UPSTREAM-LICENSE has SHA-256 %s, want %s", got, licenseSHA256)
	}
	if !strings.Contains(License, "FSL-1.1-ALv2") {
		t.Fatal("UPSTREAM-LICENSE is not the Functional Source License")
	}
}

func TestEveryPinnedBuildIsWellFormed(t *testing.T) {
	seen := map[string]bool{}
	for _, b := range Builds {
		if err := b.valid(); err != nil {
			t.Error(err)
		}
		if seen[b.ID] || seen[b.SHA256] {
			t.Errorf("build %s is pinned twice", b.ID)
		}
		seen[b.ID], seen[b.SHA256] = true, true
		// The version names the ID and the source commit (upstream prints
		// its first seven characters).
		commit, ok := strings.CutPrefix(b.Version, "softnet "+b.ID+"-")
		if !ok || len(commit) < 7 || !strings.HasPrefix(b.Source.Commit, commit) {
			t.Errorf("build %s: version %q does not name its ID and source commit %s", b.ID, b.Version, b.Source.Commit)
		}
		if b.MinMacOS > b.MaxMacOS {
			t.Errorf("build %s covers macOS %d-%d", b.ID, b.MinMacOS, b.MaxMacOS)
		}
		if b.UpstreamRelease {
			// Only an unchanged upstream release, new enough to enforce
			// Potluck's rules, on the macOS upstream supports, from
			// upstream's own release, in its pinned archive.
			if b.MinMacOS < UpstreamMinMacOS || versionLess(b.ID, UpstreamMinVersion) || b.Source.Patch != "" ||
				b.URL != b.Source.Upstream+"/releases/download/"+b.ID+"/softnet.tar.gz" || b.Archive == nil {
				t.Errorf("upstream release %s: macOS %d+, patch %q, URL %s, archive %v", b.ID, b.MinMacOS, b.Source.Patch, b.URL, b.Archive)
			}
			continue
		}
		if b.MaxMacOS >= UpstreamMinMacOS {
			t.Errorf("build %s covers macOS %d-%d, overlapping upstream's %d+", b.ID, b.MinMacOS, b.MaxMacOS, UpstreamMinMacOS)
		}
		if b.URL != releaseURL(b.ID, b.Arch) || strings.HasPrefix(ReleaseTag(b.ID), "potluck-") {
			t.Errorf("build %s: URL %s", b.ID, b.URL)
		}
	}
}

// versionLess compares dotted release numbers ("0.26.0").
func versionLess(a, b string) bool {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(as) && i < len(bs); i++ {
		x, _ := strconv.Atoi(as[i])
		y, _ := strconv.Atoi(bs[i])
		if x != y {
			return x < y
		}
	}
	return len(as) < len(bs)
}

func TestForRoutesEachMacToASoftnetItCanRun(t *testing.T) {
	for _, c := range []struct {
		major     int
		arch      string
		kind      Kind
		candidate bool
		says      string
	}{
		{27, "arm64", Managed, false, "install upstream Softnet 0.26.0 with `potluck softnet install`"},
		{26, "arm64", Managed, false, "or install the latest upstream Softnet yourself (0.25.0 or newer"},
		{15, "arm64", Managed, true, "--candidate"},
		{14, "arm64", None, false, "no Softnet known"},
		{15, "amd64", None, false, "no Softnet known"},
		{0, "arm64", None, false, "unknown"},
	} {
		a := For(c.major, c.arch)
		if a.Kind != c.kind || a.Candidate != c.candidate {
			t.Errorf("macOS %d %s: %+v", c.major, c.arch, a)
		}
		text := a.Text(c.major, "potluck softnet")
		if !strings.Contains(text, c.says) {
			t.Errorf("macOS %d %s: %q does not say %q", c.major, c.arch, text, c.says)
		}
		// A Mac too old for upstream's filtering builds is never told to install the latest upstream release.
		if c.major < UpstreamMinMacOS && strings.Contains(text, "install the latest upstream") {
			t.Errorf("macOS %d is sent to the latest upstream release: %q", c.major, text)
		}
	}
	// macOS 15's route is the one candidate build, as before macOS 26 had
	// a pinned build.
	if a := For(15, "arm64"); a.Build.ID != "0.23.0-potluck-pr227-candidate.1" || a.Build.UpstreamRelease {
		t.Errorf("macOS 15: %+v", a)
	}
	old := Builds
	t.Cleanup(func() { Builds = old })
	validated, _ := ByID("0.23.0-potluck-pr227-candidate.1")
	validated.Validated = true
	Builds = []Build{validated}
	if a := For(15, "arm64"); a.Candidate || !strings.Contains(a.Text(15, "potluck softnet"), "`potluck softnet install`") {
		t.Errorf("a validated build: %+v %q", a, a.Text(15, "potluck softnet"))
	}
}

// macOS 26 and newer get the pinned upstream release through the managed
// install: an installable build, recommended without --candidate.
func TestForGivesMacOS26AnInstallableBuild(t *testing.T) {
	for _, major := range []int{26, 27, 30} {
		a := For(major, "arm64")
		if a.Kind != Managed || a.Candidate || !a.Build.UpstreamRelease || a.Build.Archive == nil {
			t.Fatalf("macOS %d: %+v", major, a)
		}
		if err := a.Build.valid(); err != nil {
			t.Fatalf("macOS %d: %v", major, err)
		}
	}
	old := Builds
	t.Cleanup(func() { Builds = old })
	var w []Build
	for _, b := range old {
		if b.UpstreamRelease {
			b.Withdrawn = "test"
		}
		w = append(w, b)
	}
	Builds = w
	if a := For(26, "arm64"); a.Kind != Upstream || !strings.Contains(a.Text(26, "potluck softnet"), "install the latest upstream Softnet") {
		t.Fatalf("macOS 26 with its pinned release withdrawn: %+v", a)
	}
}

func TestBootHintSendsOnlyNewMacsToUpstream(t *testing.T) {
	h := BootHint("potluck softnet")
	if !strings.Contains(h, "on macOS 26 or newer run `potluck softnet install`") || !strings.Contains(h, "`potluck softnet status`") {
		t.Fatalf("boot hint %q", h)
	}
}

// shippedIDs are the builds a merged potluck has pinned. A Mac may run any
// of them, so none ever leaves Builds: one that must stop running is
// withdrawn (Build.Withdrawn), which tells that Mac why.
var shippedIDs = []string{
	"0.23.0-potluck-pr227-candidate.1",
	"0.26.0",
}

func TestEveryShippedBuildStaysPinned(t *testing.T) {
	for _, id := range shippedIDs {
		if _, ok := ByID(id); !ok {
			t.Errorf("Softnet %s left Builds: mark it Withdrawn instead, so a Mac running it is told why", id)
		}
	}
	for _, b := range Builds {
		if !slices.Contains(shippedIDs, b.ID) {
			t.Errorf("Softnet %s is pinned but not in shippedIDs: add it", b.ID)
		}
	}
}

func TestAWithdrawnBuildIsNeverOffered(t *testing.T) {
	old := Builds
	t.Cleanup(func() { Builds = old })
	w, _ := ByID("0.23.0-potluck-pr227-candidate.1")
	w.Withdrawn = "test"
	Builds = []Build{w}
	if a := For(15, "arm64"); a.Kind != None {
		t.Fatalf("For offers a withdrawn build: %+v", a)
	}
}
