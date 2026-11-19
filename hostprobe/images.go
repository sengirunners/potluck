package hostprobe

import (
	"fmt"

	"github.com/sengirunners/potluck/imagepins"
	"github.com/sengirunners/potluck/kit/config"
)

type DefaultPin = imagepins.DefaultPin

// pinTable is replaceable by tests; the published table lives in imagepins.
var pinTable = imagepins.DefaultPins
var imagesPin = imagepins.Table()

func DefaultPins() []DefaultPin                { return pinTable() }
func ParsePins(s string) ([]DefaultPin, error) { return imagepins.ParsePins(s) }
func PlainRunnerDir(d string) bool             { return imagepins.PlainRunnerDir(d) }

// osName is how share's messages name a guest OS.
var osName = map[string]string{"linux": "Linux", "darwin": "macOS"}

// DefaultImage is the image a layer lends when the owner names none: the
// layer's first plain line (no template) in images.pin. It is an error,
// naming --image, when the layer has none or zm's own image for it is not
// published yet.
func DefaultImage(layer string) (DefaultPin, error) {
	for _, p := range pinTable() {
		if p.Layer != layer || p.Template != "" || p.Name != "" {
			continue
		}
		if p.Image == "" {
			return p, fmt.Errorf("%s has no default %s image for %s boxes yet (%[1]s's own is not published): pass --image name@sha256:<64 hex>, an image with sh, bash, perl, git, nc, ip and ping", config.Program, osName[p.OS], layer)
		}
		return p, nil
	}
	return DefaultPin{}, fmt.Errorf("%s has no default image for %s boxes: pass --image name@sha256:<64 hex>", config.Program, layer)
}

// LayerTemplates are the template lines of a layer, in images.pin's order
// (published or not: Image "" is not published yet).
func LayerTemplates(layer string) []DefaultPin {
	var out []DefaultPin
	for _, p := range pinTable() {
		if p.Layer == layer && p.Template != "" {
			out = append(out, p)
		}
	}
	return out
}

// ImageMinimums is the largest floor among images that images.pin knows (an
// explicit --image of a pinned one included); 0 for what it does not know.
func ImageMinimums(images []string) (cpu, memoryMB int) {
	for _, im := range images {
		p, _ := PinForImage(im)
		cpu, memoryMB = max(cpu, p.MinCPU), max(memoryMB, p.MinMemoryMB)
	}
	return cpu, memoryMB
}

// ImageRunnerDir is where images that images.pin knows keep GitHub's runner:
// their runner_dir when every one has the same, else "". An image zm does not
// know is never guessed at: its owner names the runner with --runner-dir.
func ImageRunnerDir(images []string) string {
	dir := ""
	for i, im := range images {
		p, _ := PinForImage(im)
		d := p.RunnerDir
		if d == "" || i > 0 && d != dir {
			return ""
		}
		dir = d
	}
	return dir
}

// PinForImage returns metadata only for an exact published digest reference.
func PinForImage(image string) (DefaultPin, bool) {
	return imagepins.Lookup(pinTable(), image)
}
