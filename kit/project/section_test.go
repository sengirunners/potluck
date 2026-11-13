package project

import (
	"strings"
	"testing"
)

// A project.toml with [[run_targets]], parsed by a binary where no module
// owns that section: it is kept and written back unchanged.
const sectionToml = `id = "01J9Z"
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

func TestUnownedSectionRoundTrip(t *testing.T) {
	if sectionOf("run_targets") != nil {
		t.Skip("run_targets is owned in this binary")
	}
	f, err := ParseFile(sectionToml)
	if err != nil {
		t.Fatal(err)
	}
	out := f.Encode()
	for _, want := range []string{"[[run_targets]]", `name = "mac-vm"`, "budget = { limit = 36000, period = \"day\", unit = \"vm_s\" }", `scripts = ["test:e2e"]`, "credentials = false", `image = "macos-sequoia-xcode"`} {
		if !strings.Contains(out, want) {
			t.Errorf("encode lost %q:\n%s", want, out)
		}
	}
	g, err := ParseFile(out)
	if err != nil || g.Encode() != out {
		t.Fatalf("second round trip differs: %v\n%s", err, g.Encode())
	}
	if _, err := ParseFile(sectionToml + "\n[[run_targets]]\nname = \"b\"\n"); err != nil {
		t.Errorf("an unowned section is not validated here: %v", err)
	}
	if f, err := ParseFile("slug = \"x\"\nbogus = 1\n"); err != nil || !strings.Contains(f.Encode(), "bogus = 1\n") {
		t.Errorf("an unknown root key is kept too (keys_test.go): %v", err)
	}
}
