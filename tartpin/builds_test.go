package tartpin

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"slices"
	"strings"
	"testing"
)

func TestTheLicenseIsTheUpstreamOne(t *testing.T) {
	sum := sha256.Sum256([]byte(License))
	if got := hex.EncodeToString(sum[:]); got != licenseSHA256 {
		t.Fatalf("UPSTREAM-LICENSE has SHA-256 %s, want %s", got, licenseSHA256)
	}
	if !strings.Contains(License, "FSL-1.1-ALv2") || !strings.Contains(License, "OpenAI") {
		t.Fatal("UPSTREAM-LICENSE is not OpenAI's Functional Source License")
	}
}

var hexSHA = regexp.MustCompile(`^[0-9a-f]{64}$`)

func TestEveryPinnedBuildIsWellFormed(t *testing.T) {
	seen := map[string]bool{}
	for i, b := range Builds {
		if i > 0 {
			if c, ok := compareVersions(Builds[i-1].Version, b.Version); !ok || c <= 0 {
				t.Errorf("Tart %s is listed before %s: newest first", Builds[i-1].Version, b.Version)
			}
		}
		if seen[b.Version] || seen[b.SHA256] {
			t.Errorf("Tart %s is pinned twice", b.Version)
		}
		seen[b.Version], seen[b.SHA256] = true, true
		if b.URL != releaseURL(b.Version) || !strings.HasPrefix(b.URL, "https://github.com/openai/tart/releases/download/") {
			t.Errorf("Tart %s: URL %s is not the upstream release's tart.tar.gz", b.Version, b.URL)
		}
		if !hexSHA.MatchString(b.SHA256) || !hexSHA.MatchString(b.ProgramSHA256) || b.Bytes <= 0 {
			t.Errorf("Tart %s: sha256 %q, program sha256 %q, %d bytes", b.Version, b.SHA256, b.ProgramSHA256, b.Bytes)
		}
		if b.MinMacOS < 11 || !slices.Contains(b.Arches, "arm64") {
			t.Errorf("Tart %s: macOS %d+, %v", b.Version, b.MinMacOS, b.Arches)
		}
		if b.LicenseSHA256 != licenseSHA256 {
			t.Errorf("Tart %s: licence %s is not UPSTREAM-LICENSE's", b.Version, b.LicenseSHA256)
		}
	}
	if _, ok := Latest(); !ok {
		t.Error("no pinned Tart may be installed")
	}
}

// shippedVersions are the releases a merged potluck has pinned. A Mac may
// have any of them installed, so none ever leaves Builds: one that must not
// be installed again is withdrawn (Build.Withdrawn).
var shippedVersions = []string{
	"2.42.0",
}

func TestEveryShippedBuildStaysPinned(t *testing.T) {
	for _, v := range shippedVersions {
		if _, ok := ByVersion(v); !ok {
			t.Errorf("Tart %s left Builds: mark it Withdrawn instead", v)
		}
	}
	for _, b := range Builds {
		if !slices.Contains(shippedVersions, b.Version) {
			t.Errorf("Tart %s is pinned but not in shippedVersions: add it", b.Version)
		}
	}
}

func TestAWithdrawnBuildIsNotLatest(t *testing.T) {
	old := Builds
	t.Cleanup(func() { Builds = old })
	w := old[0]
	w.Withdrawn = "test"
	Builds = []Build{w}
	if b, ok := Latest(); ok {
		t.Fatalf("Latest offers a withdrawn build: %+v", b)
	}
}

func TestRuns(t *testing.T) {
	b := Builds[0]
	for _, c := range []struct {
		major int
		arch  string
		want  bool
	}{{26, "arm64", true}, {b.MinMacOS, "arm64", true}, {b.MinMacOS - 1, "arm64", false}, {0, "arm64", false}, {26, "riscv64", false}} {
		if got := b.Runs(c.major, c.arch); got != c.want {
			t.Errorf("macOS %d %s: %v, want %v", c.major, c.arch, got, c.want)
		}
	}
}

func TestVersionsOrderByNumber(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want int
		ok   bool
	}{
		{"2.9.0", "2.10.0", -1, true},
		{"2.42.0", "2.42", 0, true},
		{"3.0.0", "2.99.99", 1, true},
		{"2.42.0-beta", "2.42.0", 0, false},
		{"", "1", 0, false},
	} {
		got, ok := compareVersions(c.a, c.b)
		if got != c.want || ok != c.ok {
			t.Errorf("%q vs %q: %d %v, want %d %v", c.a, c.b, got, ok, c.want, c.ok)
		}
	}
}
