package rt

import (
	"testing"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/kit/project"
)

// [[run_targets]] with this module's section registered: parsed into
// bindings, validated, and written back the same.
func TestProjectFileRunTargetsRoundTrip(t *testing.T) {
	src := `id = "01J9Z"
slug = "orchid"
name = "Orchid"
remotes = ["https://github.com/acme/Orchid"]

[[run_targets]]
name = "mac-vm"
driver = "tart"
kind = "run"
isolation = "ephemeral"
credentials = false
repo = "https://github.com/acme/Orchid"
branch = "main"
scripts = ["test:e2e"]
budget = { unit = "vm_s", limit = 36000, period = "day" }
image = "macos-sequoia-xcode"
`
	f, err := project.ParseFile(src)
	if err != nil {
		t.Fatal(err)
	}
	ts, _ := f.Sections["run_targets"].([]computeapi.RunTarget)
	if len(ts) != 1 || ts[0].Name != "mac-vm" || ts[0].Budget == nil || ts[0].Budget.Limit != 36000 || ts[0].Config["image"] != "macos-sequoia-xcode" {
		t.Fatalf("parsed %+v", f.Sections)
	}
	out := f.Encode()
	g, err := project.ParseFile(out)
	if err != nil || g.Encode() != out {
		t.Fatalf("round trip differs: %v\n%s\n%s", err, out, g.Encode())
	}
	if _, err := project.ParseFile(src + "\n[[run_targets]]\nname = \"b\"\n"); err == nil {
		t.Error("an owned section is validated")
	}
}
