package runners

import (
	"slices"
	"testing"

	"github.com/sengirunners/potluck/imagepins"
)

// A named image's name is a runs-on label (runs-on: [pool, macos, xcode]),
// read before the OS and arch labels: a pin named like one, published or not
// yet, would turn that OS or arch label into an image choice.
func TestPinNamesAreNotRunnerLabels(t *testing.T) {
	for _, p := range imagepins.DefaultPins() {
		if p.Name != "" && (slices.Contains(osLabels, p.Name) || slices.Contains(archLabels, p.Name) || p.Name == "self-hosted") {
			t.Errorf("images.pin names an image %q, which is a runner label", p.Name)
		}
	}
}
