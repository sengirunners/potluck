package hostprobe

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/imagepins"
)

func TestKeepEarlierImage(t *testing.T) {
	def, err := DefaultImage("tart")
	if err != nil {
		t.Fatal(err)
	}
	xcode, ok := imagepins.NamedPin(DefaultPins(), "tart", "xcode")
	if !ok {
		t.Fatal("no xcode pin")
	}
	older := strings.SplitN(def.Image, "@", 2)[0] + "@sha256:" + strings.Repeat("9", 64)
	for _, c := range []struct {
		name      string
		target    computeapi.RunTarget
		with      string
		images    []string
		runnerDir string
		ok        bool
	}{
		{"default pin", computeapi.RunTarget{Images: []string{def.Image}}, "", nil, "", true},
		{"older digest of the default upgrades as the default", computeapi.RunTarget{Images: []string{older}}, "", nil, "", true},
		{"named by its image_name", computeapi.RunTarget{Images: []string{pinA}, Config: map[string]string{"image_name": "xcode"}}, "xcode", nil, "", true},
		{"named by its pin", computeapi.RunTarget{Images: []string{xcode.Image}}, "xcode", nil, "", true},
		{"owner's own image keeps its images and runner_dir", computeapi.RunTarget{Images: []string{pinOwn}, Config: map[string]string{"runner_dir": "/opt/runner"}}, "", []string{pinOwn}, "/opt/runner", true},
		{"no image: nothing to repeat", computeapi.RunTarget{Config: map[string]string{"image_name": "xcode", "runner_dir": "/opt/runner"}}, "", nil, "", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			with, images, dir, ok := KeepEarlierImage(c.target)
			if with != c.with || !slices.Equal(images, c.images) || dir != c.runnerDir || ok != c.ok {
				t.Fatalf("got (%q, %v, %q, %v), want (%q, %v, %q, %v)", with, images, dir, ok, c.with, c.images, c.runnerDir, c.ok)
			}
		})
	}
}

func TestSelectedPin(t *testing.T) {
	def, _ := DefaultImage("tart")
	xcode, _ := imagepins.NamedPin(DefaultPins(), "tart", "xcode")
	for _, c := range []struct {
		name  string
		o     ShareOptions
		image string
		layer string
		ok    bool
	}{
		{"default of the best layer", ShareOptions{Report: macReport()}, def.Image, "tart", true},
		{"named image", ShareOptions{Report: macReport(), With: "xcode"}, xcode.Image, "tart", true},
		{"explicit layer", ShareOptions{Report: macReport(), Layer: "tart"}, def.Image, "tart", true},
		{"explicit layer the report lacks", ShareOptions{Layer: "tart"}, "", "", false},
		{"explicit layer that is not ready", ShareOptions{Report: Report{OS: "darwin", Layers: []Layer{{Name: "tart", Supported: true, Problems: []string{"no softnet"}}}}, Layer: "tart"}, "", "", false},
		{"own images select no pin", ShareOptions{Report: macReport(), Images: []string{pinOwn}}, "", "", false},
		{"unknown name", ShareOptions{Report: macReport(), With: "nope"}, "", "tart", false},
		{"no layer", ShareOptions{}, "", "", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			p, layer, ok := SelectedPin(c.o)
			if ok != c.ok || p.Image != c.image || layer != c.layer {
				t.Fatalf("got (%q, %q, %v), want (%q, %q, %v)", p.Image, layer, ok, c.image, c.layer, c.ok)
			}
		})
	}
}

func TestDiskNeedGB(t *testing.T) {
	xcode, _ := imagepins.NamedPin(DefaultPins(), "tart", "xcode")
	need, estimate := DiskNeedGB([]string{xcode.Image})
	if need != xcode.MinFreeGB || estimate != "" {
		t.Fatalf("pinned: need %d, estimate %q", need, estimate)
	}
	need, estimate = DiskNeedGB([]string{xcode.Image, pinOwn})
	if need < xcode.MinFreeGB || !strings.Contains(estimate, "reg.example/own") {
		t.Fatalf("own image: need %d, estimate %q", need, estimate)
	}
}

// Check accepts what KeepEarlierImage returns: repeating the earlier choice
// is never refused as a silent replacement.
func TestCheckAcceptsKeptEarlierImage(t *testing.T) {
	def, _ := DefaultImage("tart")
	xcode, _ := imagepins.NamedPin(DefaultPins(), "tart", "xcode")
	for _, img := range []string{def.Image, xcode.Image, pinOwn} {
		earlier := computeapi.RunTarget{Name: "mac-c2m4", Images: []string{img}, Config: map[string]string{"vm": "tart"}}
		with, images, _, _ := KeepEarlierImage(earlier)
		o := ShareOptions{Report: macReport(), Earlier: []computeapi.RunTarget{earlier}, NamePrefix: "mac", DryRun: true, With: with, Images: images}
		if _, err := checkOn(context.Background(), nil, o); err != nil {
			t.Errorf("%s: kept choice (%q, %v) refused: %v", img, with, images, err)
		}
	}
}
