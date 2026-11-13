package rt

import (
	"strings"
	"testing"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/kit/ids"
)

const hostPin = "ref-image@sha256:0000000000000000000000000000000000000000000000000000000000000001"

func hostBox(name string, share ...string) computeapi.HostRunTarget {
	return computeapi.HostRunTarget{RunTarget: computeapi.RunTarget{Name: name, Driver: "ref", Kind: computeapi.RunTargetKindBox,
		Isolation: computeapi.RunTargetIsolationEphemeral, Images: []string{hostPin}, Config: map[string]string{"cpus": "2"}}, Share: share}
}

var orchid, acme = ids.New(), ids.New()

func TestHostRunTargetsRoundTrip(t *testing.T) {
	in := []computeapi.HostRunTarget{hostBox("sandbox", orchid, acme), hostBox("big", orchid)}
	out, legacy, err := ParseHostRunTargets(EncodeHostRunTargets(in))
	if err != nil || legacy {
		t.Fatal(legacy, err)
	}
	if len(out) != 2 || out[0].Name != "sandbox" || strings.Join(out[0].Share, ",") != orchid+","+acme || out[0].Config["cpus"] != "2" || out[1].Images[0] != hostPin {
		t.Fatalf("round trip: %+v", out)
	}
	if out, _, err := ParseHostRunTargets(""); err != nil || len(out) != 0 {
		t.Fatalf("empty file: %v %v", out, err)
	}
}

// legacyFile is a run_targets.toml as a build that keyed shares by slug
// wrote it: no format line.
func legacyFile(ts ...computeapi.HostRunTarget) string {
	return strings.Replace(EncodeHostRunTargets(ts), "format = 2\n", "", 1)
}

// A file's format, not the shape of a share, says whether its shares are
// slugs: a 26-digit slug in an old file is a slug, and a slug in a file of
// this format is refused.
func TestHostRunTargetsFormat(t *testing.T) {
	digits := strings.Repeat("1", 26)
	if !ids.Valid(digits) {
		t.Fatal("the test wants a slug that looks like an id")
	}
	out, legacy, err := ParseHostRunTargets(legacyFile(hostBox("sandbox", "acme", digits)))
	if err != nil || !legacy || strings.Join(out[0].Share, ",") != "acme,"+digits {
		t.Fatalf("an old file: %+v %v %v", out, legacy, err)
	}
	if _, _, err := ParseHostRunTargets(legacyFile(hostBox("sandbox", orchid))); err == nil {
		t.Error("an old file whose share is an id (upper case, no slug) parsed")
	}
	if _, _, err := ParseHostRunTargets(EncodeHostRunTargets([]computeapi.HostRunTarget{hostBox("sandbox", "acme")})); err == nil {
		t.Error("a slug share in a file of this format parsed")
	}
	if _, _, err := ParseHostRunTargets(strings.Replace(EncodeHostRunTargets([]computeapi.HostRunTarget{hostBox("sandbox", orchid)}), "format = 2", "format = 3", 1)); err == nil {
		t.Error("a file of another format parsed")
	}
}

func TestHostRunTargetRefusals(t *testing.T) {
	bad := map[string]func(h *computeapi.HostRunTarget){
		"kind run":        func(h *computeapi.HostRunTarget) { h.Kind = computeapi.RunTargetKindRun },
		"a repo":          func(h *computeapi.HostRunTarget) { h.Repo, h.Branch = "https://example.com/a/b", "main" },
		"no share":        func(h *computeapi.HostRunTarget) { h.Share = nil },
		"a slug":          func(h *computeapi.HostRunTarget) { h.Share = []string{"acme"} },
		"credentials":     func(h *computeapi.HostRunTarget) { h.Credentials = true },
		"unpinned image":  func(h *computeapi.HostRunTarget) { h.Images = []string{"ref-image:latest"} },
		"a secret value":  func(h *computeapi.HostRunTarget) { h.Config = map[string]string{"api_token": "x"} },
		"a share config":  func(h *computeapi.HostRunTarget) { h.Config = map[string]string{"share": "x"} },
		"an @ in a name":  func(h *computeapi.HostRunTarget) { h.Name = "a@b" },
		"zm's own config": func(h *computeapi.HostRunTarget) { h.Config = map[string]string{"kind": "x"} },
	}
	for name, mut := range bad {
		h := hostBox("sandbox", orchid)
		mut(&h)
		if err := ValidateHostRunTarget(h); err == nil {
			t.Errorf("%s: accepted", name)
		}
		// A file holding it is refused as the API is.
		if _, _, err := ParseHostRunTargets(EncodeHostRunTargets([]computeapi.HostRunTarget{h})); err == nil && name != "an @ in a name" {
			t.Errorf("%s: a file holding it parsed", name)
		}
	}
	dup := EncodeHostRunTargets([]computeapi.HostRunTarget{hostBox("a", orchid), hostBox("a", orchid)})
	if _, _, err := ParseHostRunTargets(dup); err == nil {
		t.Error("a name twice parsed")
	}
	if _, _, err := ParseHostRunTargets("[[run_targets]]\nname = \"x\"\nbogus = [1]\n"); err == nil {
		t.Error("a bad config type parsed")
	}
}
