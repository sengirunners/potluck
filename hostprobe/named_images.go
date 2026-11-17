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
func selectedPin(layer, name string) (DefaultPin, error) {
	if name == "" {
		return DefaultImage(layer)
	}
	if p, ok := imagepins.NamedPin(pinTable(), layer, name); ok {
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
	pin, err = selectedPin(l.Name, o.With)
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

// tartCachedImages reads Tart's OCI cache once: the digest references it
// holds. It is the one home for that read. Tart encodes Source and Name in
// Commands/List.swift (VMInfo):
// https://github.com/cirruslabs/tart/blob/main/Sources/tart/Commands/List.swift
// Only an OCI cache entry with the exact digest reference can replace a pull.
// A mutable local VM or a tag with the same repository is not the pinned image.
// An unreadable listing returns an error, and callers fall back to pulling and
// the normal conformance checks.
func tartCachedImages(ctx context.Context, run Runner) (map[string]bool, error) {
	if run == nil {
		return nil, errors.New("no way to run tart list")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	r, err := run.Run(ctx, "tart", "list", "--format", "json")
	if err != nil {
		return nil, err
	}
	if r.Exit != 0 {
		return nil, fmt.Errorf("tart list exited %d", r.Exit)
	}
	var entries []struct{ Source, Name string }
	if err := json.Unmarshal([]byte(r.Stdout), &entries); err != nil {
		return nil, fmt.Errorf("tart list gave no JSON list: %v", err)
	}
	cached := map[string]bool{}
	for _, e := range entries {
		if strings.EqualFold(e.Source, "oci") {
			cached[e.Name] = true
		}
	}
	return cached, nil
}

// tartImagePresent reports whether Tart's cache holds image; an unreadable
// cache says false.
func tartImagePresent(ctx context.Context, run Runner, image string) bool {
	cached, err := tartCachedImages(ctx, run)
	return err == nil && cached[image]
}
