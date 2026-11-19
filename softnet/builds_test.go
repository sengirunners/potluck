package softnet

import (
	"crypto/sha256"
	"encoding/hex"
	"slices"
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
		if !strings.HasPrefix(b.Version, "softnet "+b.ID+"-") || !strings.HasSuffix(b.Version, b.Source.Commit) {
			t.Errorf("build %s: version %q does not name its ID and source commit %s", b.ID, b.Version, b.Source.Commit)
		}
		if b.MaxMacOS >= UpstreamMinMacOS || b.MinMacOS > b.MaxMacOS {
			t.Errorf("build %s covers macOS %d-%d, overlapping upstream's %d+", b.ID, b.MinMacOS, b.MaxMacOS, UpstreamMinMacOS)
		}
		if b.URL != releaseURL(b.ID, b.Arch) || strings.HasPrefix(ReleaseTag(b.ID), "potluck-") {
			t.Errorf("build %s: URL %s", b.ID, b.URL)
		}
	}
}

func TestForRoutesEachMacToASoftnetItCanRun(t *testing.T) {
	for _, c := range []struct {
		major     int
		arch      string
		kind      Kind
		candidate bool
		says      string
	}{
		{27, "arm64", Upstream, false, "latest upstream Softnet"},
		{26, "arm64", Upstream, false, "latest upstream Softnet"},
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
	old := Builds
	t.Cleanup(func() { Builds = old })
	validated := old[0]
	validated.Validated = true
	Builds = []Build{validated}
	if a := For(15, "arm64"); a.Candidate || !strings.Contains(a.Text(15, "potluck softnet"), "`potluck softnet install`") {
		t.Errorf("a validated build: %+v %q", a, a.Text(15, "potluck softnet"))
	}
}

func TestBootHintSendsOnlyNewMacsToUpstream(t *testing.T) {
	h := BootHint("potluck softnet")
	if !strings.Contains(h, "on macOS 26 or newer install the latest") || !strings.Contains(h, "`potluck softnet status`") {
		t.Fatalf("boot hint %q", h)
	}
}

// shippedIDs are the builds a merged potluck has pinned. A Mac may run any
// of them, so none ever leaves Builds: one that must stop running is
// withdrawn (Build.Withdrawn), which tells that Mac why.
var shippedIDs = []string{
	"0.23.0-potluck-pr227-candidate.1",
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
	w := old[0]
	w.Withdrawn = "test"
	Builds = []Build{w}
	if a := For(15, "arm64"); a.Kind != None {
		t.Fatalf("For offers a withdrawn build: %+v", a)
	}
}
