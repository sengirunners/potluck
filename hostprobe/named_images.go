package hostprobe

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/imagepins"
	"strings"
	"time"
)

// selectedPin keeps named images separate from sandbox templates.
func selectedPin(report Report, layer, name string) (DefaultPin, error) {
	if name == "" {
		return HostDefaultImage(report, layer)
	}
	if p, ok := imagepins.NamedPin(pinTable(), layer, name); ok {
		if layer == "tart" {
			if err := CheckTartImages(report, []string{p.Image}); err != nil {
				return DefaultPin{}, err
			}
		}
		return p, nil
	}
	return DefaultPin{}, fmt.Errorf("--with %q: no published named image for layer %s", name, layer)
}

// SelectedPin is the pinned image o lends when it names no image of its own:
// the --with named image, else the layer's default, on the layer Check would
// choose (chooseLayer: o.Layer when it is ready, else the report's best).
// ok is false when o names its own images, Check would refuse every layer, or
// the pin table has no such image. It is the plan's view of the choice Check
// makes; Check adds the layer's own readiness rules on top.
func SelectedPin(o ShareOptions) (pin DefaultPin, layer string, ok bool) {
	if len(o.Images) > 0 {
		return DefaultPin{}, "", false
	}
	l, err := chooseLayer(o)
	if err != nil {
		return DefaultPin{}, "", false
	}
	pin, err = selectedPin(o.Report, l.Name, o.With)
	if err != nil {
		return DefaultPin{}, l.Name, false
	}
	return pin, l.Name, true
}

// earlierImage is what an earlier binding t lent, as the pin table knows it
// now: the named image it was lent by (its image_name, else its pin's name,
// "" for the default or an image the table does not know). An older version of
// a published pin upgrades as that pin (imagepins.PinForRepository); known is
// false for an image no pin covers, and for a binding with no image.
func earlierImage(t computeapi.RunTarget) (name string, known bool) {
	if len(t.Images) == 0 {
		return "", false
	}
	pin, known := PinForImage(t.Images[0])
	if !known {
		pin, known = imagepins.PinForRepository(t.Images[0])
	}
	if known && t.Config["image_default"] == "true" {
		return "", true
	}
	name = t.Config["image_name"]
	if name == "" {
		name = pin.Name
	}
	return name, known
}

// KeepEarlierImage is the image choice a re-lend repeats from an earlier
// binding t: a named image by name, the default as the default (with and
// images empty), and the owner's own images, with the binding's runner_dir,
// only when the pin table does not know the image. It is the rule Check
// enforces, so a caller's plan matches what Check accepts. ok is false for a
// binding with no image: it lent nothing to repeat.
func KeepEarlierImage(t computeapi.RunTarget) (with string, images []string, runnerDir string, ok bool) {
	if len(t.Images) == 0 {
		return "", nil, "", false
	}
	name, known := earlierImage(t)
	if name != "" {
		return name, nil, "", true
	}
	if !known {
		return "", t.Images, t.Config["runner_dir"], true
	}
	return "", nil, "", true
}

// keepFlags are the share flags that lend again the images of t, a binding
// whose image the pin table does not know, as KeepEarlierImage repeats them:
// its images and its runner_dir.
func keepFlags(t computeapi.RunTarget) string {
	_, images, runnerDir, _ := KeepEarlierImage(t)
	var f []string
	for _, im := range images {
		f = append(f, "--image "+im)
	}
	if runnerDir != "" {
		f = append(f, "--runner-dir "+runnerDir)
	}
	return strings.Join(f, " ")
}

func imageRoot(images []string) bool {
	if len(images) == 0 {
		return false
	}
	for _, image := range images {
		p, found := PinForImage(image)
		if !found || !p.Root {
			return false
		}
	}
	return true
}

// tartEntry is one entry of Tart's list: a pulled OCI image (OCI) or a local
// VM, such as a clone.
type tartEntry struct {
	Name    string
	OCI     bool
	Running bool
	// SizeGB is what Tart says the entry takes on disk, in whole GB
	// (10^9 bytes, rounded down); ok is false when it says nothing usable.
	SizeGB int
	SizeOK bool
}

// tartList reads Tart's list once. It is the one home for that read. Tart
// encodes Source, Name, Size (allocated bytes / 10^9, a whole number) and
// Running in Commands/List.swift (VMInfo):
// https://github.com/cirruslabs/tart/blob/main/Sources/tart/Commands/List.swift
// A Size that is missing or not a number leaves SizeOK false rather than
// failing the read. An unreadable listing returns an error.
func tartList(ctx context.Context, run Runner, cli string) ([]tartEntry, error) {
	if run == nil {
		return nil, errors.New("no way to run tart list")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if cli == "" {
		cli = "tart"
	}
	r, err := run.Run(ctx, cli, "list", "--format", "json")
	if err != nil {
		return nil, err
	}
	if r.Exit != 0 {
		return nil, fmt.Errorf("tart list exited %d", r.Exit)
	}
	var raw []struct {
		Source, Name string
		Running      bool
		Size         json.RawMessage
	}
	if err := json.Unmarshal([]byte(r.Stdout), &raw); err != nil {
		return nil, fmt.Errorf("tart list gave no JSON list: %v", err)
	}
	entries := make([]tartEntry, 0, len(raw))
	for _, e := range raw {
		t := tartEntry{Name: e.Name, OCI: strings.EqualFold(e.Source, "oci"), Running: e.Running}
		var gb float64
		if len(e.Size) > 0 && string(e.Size) != "null" && json.Unmarshal(e.Size, &gb) == nil && gb >= 0 {
			t.SizeGB, t.SizeOK = int(gb), true
		}
		entries = append(entries, t)
	}
	return entries, nil
}

// tartCachedImages is the digest references Tart's OCI cache holds
// (tartList). Only an OCI cache entry with the exact digest reference can
// replace a pull. A mutable local VM or a tag with the same repository is not
// the pinned image. An unreadable listing returns an error, and callers fall
// back to pulling and the normal conformance checks.
func tartCachedImages(ctx context.Context, run Runner, cli string) (map[string]bool, error) {
	entries, err := tartList(ctx, run, cli)
	if err != nil {
		return nil, err
	}
	cached := map[string]bool{}
	for _, e := range entries {
		if e.OCI {
			cached[e.Name] = true
		}
	}
	return cached, nil
}

// SmallLinux is the name of a layer's small Linux image in images.pin (on
// tart: the image a first try lends a Mac instead of macOS's 27 GB).
const SmallLinux = "small-linux"

// FirstImage is what a first try (`potluck try`) lends with o.
type FirstImage struct {
	// With is the named image to lend ("" for the layer's default).
	With string
	// Lent is the pin of the image lent; Default the layer's default, which
	// is Lent unless With is set.
	Lent, Default DefaultPin
	// Unpublished: the layer's default is a macOS guest and images.pin has a
	// small Linux line for the layer whose image is not published yet, so
	// the default is lent.
	Unpublished bool
}

// ChooseFirstImage is the image a first try lends with o, which names no
// image of its own: on the layer Check would choose (SelectedPin), the
// layer's published small Linux image when its default is a macOS guest,
// else the default. ok is false when SelectedPin has no answer (o names
// images or --with, no layer is ready, or the layer has no default): the
// host run then says why.
func ChooseFirstImage(o ShareOptions) (FirstImage, bool) {
	if o.With != "" {
		return FirstImage{}, false
	}
	def, layer, ok := SelectedPin(o)
	if !ok {
		return FirstImage{}, false
	}
	f := FirstImage{Lent: def, Default: def}
	if def.OS != "darwin" {
		return f, true
	}
	switch p, ok := imagepins.NamedLine(pinTable(), layer, SmallLinux); {
	case ok && p.Image != "":
		f.With, f.Lent = SmallLinux, p
	case ok:
		f.Unpublished = true
	}
	return f, true
}
