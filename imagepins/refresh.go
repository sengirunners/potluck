package imagepins

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// ImageDefaultKey is the binding config key share writes to say who chose a
// box binding's images: "true" when it lent Potluck's default pins (no
// --image, no --with), "named" when it lent a pin Potluck publishes, chosen by
// name (--with), "false" when the owner named the images themselves
// (--image). A binding bound before share wrote it has none; any other value
// counts as the owner's.
const ImageDefaultKey = "image_default"

// Values of ImageDefaultKey.
const (
	ImageDefault = "true"
	ImageNamed   = "named"
	ImageOwners  = "false"
)

// Move is how a box binding of Potluck's images stands under a pin table.
type Move struct {
	// Now is what the binding lends under the table: each image replaced, by
	// its repository (the part before "@"), with the table's pin of that
	// repository on the binding's layer, in the same order.
	Now []string
	// Behind is whether Now differs from what the binding lends.
	Behind bool
	// Owner, when set, is why only the machine's owner moves the binding (a
	// re-run of share, which checks the machine again): what share would
	// bind beside the new pins is not what the binding carries, or the
	// binding does not say Potluck chose its images. "": the daemon may
	// change its images alone, every other key as share bound it.
	Owner string
}

// Current is a box binding's Move under pins. ok is false when the images are
// not Potluck's choice: cfg's ImageDefaultKey says the owner chose them (set,
// and neither ImageDefault nor ImageNamed), or, with no key, one is from a
// repository pins publishes nowhere on layer (an image Potluck does not
// publish is the owner's), or the first image is a template's or a later one
// is not.
//
// A binding with no ImageDefaultKey (bound before share wrote one) is
// Potluck's when it also lends template images, which share adds only
// without --image; with its first image alone it may be an owner's --image
// of Potluck's repository, so it is behind with Owner set. A binding whose key
// says Potluck chose it and that lends a repository this table no longer
// publishes is behind with Owner set: share chooses its images again.
func Current(pins []DefaultPin, layer string, images []string, cfg map[string]string) (m Move, ok bool) {
	key, set := cfg[ImageDefaultKey]
	ours := key == ImageDefault || key == ImageNamed
	if len(images) == 0 || set && !ours {
		return Move{}, false
	}
	var now []DefaultPin
	var gone []string
	for i, im := range images {
		repo, _, cut := strings.Cut(im, "@")
		if !cut || repo == "" || !Pinned(im) {
			return Move{}, false
		}
		j := slices.IndexFunc(pins, func(p DefaultPin) bool {
			return p.Layer == layer && p.Image != "" && (p.Template == "") == (i == 0) && strings.HasPrefix(p.Image, repo+"@")
		})
		if j < 0 {
			if !ours {
				return Move{}, false
			}
			gone = append(gone, repo)
			continue
		}
		now = append(now, pins[j])
		m.Now = append(m.Now, pins[j].Image)
	}
	switch {
	case len(gone) > 0:
		m.Behind = true
		m.Owner = "this version no longer publishes " + strings.Join(gone, ", ") + ", so share chooses its images again"
	case !slices.Equal(m.Now, images):
		m.Behind = true
		m.Owner = ownerOnly(now, cfg)
	}
	return m, true
}

// ownerOnly is why a move to now must be the owner's share, "" when the
// keys share binds from an image's pin (runner_dir, image_name, the size
// floor) still hold for now.
func ownerOnly(now []DefaultPin, cfg map[string]string) string {
	if _, set := cfg[ImageDefaultKey]; !set && len(now) == 1 {
		return "it was bound before share recorded who chose its images, so it may be an image you named"
	}
	dir := now[0].RunnerDir
	for _, p := range now[1:] {
		if p.RunnerDir != dir {
			dir = ""
		}
	}
	// Share binds these from the pins (or an owner's --runner-dir); a
	// binding that differs from the new pins in either, a key added or
	// dropped among them, is not the daemon's to move.
	if r := cfg["runner_dir"]; r != dir {
		return fmt.Sprintf("its runner_dir is %q and this version's images keep GitHub's runner in %q", r, dir)
	}
	if n := cfg["image_name"]; n != now[0].Name {
		return fmt.Sprintf("its image_name is %q and this version names its image %q", n, now[0].Name)
	}
	for _, p := range now {
		cpu, _ := strconv.Atoi(cfg["cpu"])
		mem, _ := strconv.Atoi(cfg["memory"])
		if cpu < p.MinCPU || mem < p.MinMemoryMB {
			return fmt.Sprintf("this version's %s needs at least %d vCPUs and %d MB, more than the size has", p.Image, p.MinCPU, p.MinMemoryMB)
		}
	}
	return ""
}
