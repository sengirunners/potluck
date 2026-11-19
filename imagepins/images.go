// Package imagepins owns the published image table and its metadata.
package imagepins

import (
	_ "embed"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

//go:embed images.pin
var table string

// Table returns the embedded pin file for validation and tooling.
func Table() string { return table }

var defaultPins = mustParsePins(table)
var templateRe = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
var pinned = regexp.MustCompile(`^[a-z0-9][^\s@,]*@sha256:[0-9a-f]{64}$`)

func Pinned(image string) bool  { return pinned.MatchString(image) }
func DefaultPins() []DefaultPin { return append([]DefaultPin(nil), defaultPins...) }

// DefaultPin is one line of images.pin. Image is "" while zm's own image for
// the layer is not published yet. MinCPU and MinMemoryMB are the image's own
// floor (Tart's cpuCountMin and memorySizeMin: tart set refuses less), 0 when
// it has none; a published macOS Tart pin always has both (ParsePins). RunnerDir is where the image keeps GitHub's runner (a box's
// runner_dir: relative to the guest user's home unless absolute), "" when it
// has none. Template names a template image (template=NAME; zm-eakh.12.5.20:
// "agent", zm's coding-agent image), "" for the layer's plain default; a
// template line is never a layer's default, and share lends each published
// one beside it. Named images are selected alone with --with. Root defaults
// passwordless sudo on inside the VM; an explicit --root=false overrides it.
type DefaultPin struct {
	Layer, OS, Image    string
	MinCPU, MinMemoryMB int
	RunnerDir           string
	Template            string
	Name                string // selected with --with; never lent beside the default
	MinFreeGB           int    // disk budget for pull and boxes
	PullMinutes         int    // pull budget; 0 uses the caller's default
	PullGB              int    // about how many GB a first pull downloads, for the words that warn of it; 0 unknown
	Root                bool   // keep passwordless sudo inside the VM by default
	MinHostMacOS        int    // supported host floor for this exact Tart digest; 0 means unknown
	Arch                string // guest architecture; empty means unknown
	HostDefault         bool   // named alternative eligible as an OS-compatible host default
}

// ParsePins reads images.pin: "layer os image [min_cpu=N] [min_memory_mb=N]
// [runner_dir=PATH] [template=NAME|name=NAME] [root=on] [min_free_gb=N] [pull_minutes=N] [pull_gb=N]
// [arch=arm64|amd64] [min_host_macos=N] [host_default=on]" per line, image pinned by digest or "-",
// with # comments and blank lines. One plain line per layer and OS, and one
// per template name beside it.
func ParsePins(s string) ([]DefaultPin, error) {
	var out []DefaultPin
	seen := map[string]bool{}
	for n, line := range strings.Split(s, "\n") {
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		if len(f) < 3 {
			return nil, fmt.Errorf("images.pin line %d: want layer, os and image", n+1)
		}
		p := DefaultPin{Layer: f[0], OS: f[1], Image: f[2]}
		for _, kv := range f[3:] {
			k, v, _ := strings.Cut(kv, "=")
			if k == "host_default" {
				if v != "on" {
					return nil, fmt.Errorf("images.pin line %d: want host_default=on", n+1)
				}
				p.HostDefault = true
				continue
			}
			if k == "arch" {
				if v != "arm64" && v != "amd64" {
					return nil, fmt.Errorf("images.pin line %d: want arch=arm64 or amd64", n+1)
				}
				p.Arch = v
				continue
			}
			if k == "root" {
				if v != "on" {
					return nil, fmt.Errorf("images.pin line %d: want root=on", n+1)
				}
				p.Root = true
				continue
			}
			if k == "name" {
				if !templateRe.MatchString(v) || p.Name != "" {
					return nil, fmt.Errorf("images.pin line %d: want one name=<lowercase letters, digits and ->", n+1)
				}
				p.Name = v
				continue
			}
			if k == "template" {
				if !templateRe.MatchString(v) || p.Template != "" {
					return nil, fmt.Errorf("images.pin line %d: %q: want one template=<lowercase letters, digits and ->", n+1, kv)
				}
				p.Template = v
				continue
			}
			if k == "runner_dir" {
				if !PlainRunnerDir(v) {
					return nil, fmt.Errorf("images.pin line %d: %q: want runner_dir=<a plain path>", n+1, kv)
				}
				p.RunnerDir = v
				continue
			}
			m, err := strconv.Atoi(v)
			if err != nil || m <= 0 || (k != "min_cpu" && k != "min_memory_mb" && k != "min_free_gb" && k != "pull_minutes" && k != "pull_gb" && k != "min_host_macos") {
				return nil, fmt.Errorf("images.pin line %d: %q: want min_cpu=N, min_memory_mb=N, min_free_gb=N, pull_minutes=N, pull_gb=N, runner_dir=PATH, template=NAME, name=NAME, root=on, arch=arm64|amd64, min_host_macos=N or host_default=on", n+1, kv)
			}
			switch k {
			case "min_host_macos":
				p.MinHostMacOS = m
			case "min_cpu":
				p.MinCPU = m
			case "min_memory_mb":
				p.MinMemoryMB = m
			case "min_free_gb":
				p.MinFreeGB = m
			case "pull_minutes":
				p.PullMinutes = m
			case "pull_gb":
				p.PullGB = m
			}
		}
		if p.Layer != "tart" && p.Layer != "oci" && p.Layer != "incus" {
			return nil, fmt.Errorf("images.pin line %d: unknown layer %q", n+1, p.Layer)
		}
		if p.OS != "linux" && p.OS != "darwin" {
			return nil, fmt.Errorf("images.pin line %d: os %q: want linux or darwin", n+1, p.OS)
		}
		if p.Image == "-" {
			p.Image = ""
		} else if !Pinned(p.Image) {
			return nil, fmt.Errorf("images.pin line %d: %q is not pinned by a full digest (name@sha256:<64 hex>)", n+1, p.Image)
		}
		if p.Name != "" && p.Template != "" {
			return nil, fmt.Errorf("images.pin line %d: name and template cannot be combined", n+1)
		}
		if p.Image != "" && p.Layer == "tart" && p.OS == "darwin" && (p.MinCPU == 0 || p.MinMemoryMB == 0) {
			return nil, fmt.Errorf("images.pin line %d: a macOS Tart image needs min_cpu and min_memory_mb (tart set refuses a macOS guest under its own floor, and a line without one would lend sizes that never boot)", n+1)
		}
		if p.HostDefault && (p.Layer != "tart" || p.Name == "" || p.MinHostMacOS == 0 || p.Arch == "") {
			return nil, fmt.Errorf("images.pin line %d: host_default needs a named Tart image with arch and min_host_macos", n+1)
		}
		key := p.Layer + " " + p.OS
		if p.Template != "" {
			key += " template=" + p.Template
		}
		if p.Name != "" {
			key += " name=" + p.Name
		}
		if seen[key] {
			return nil, fmt.Errorf("images.pin line %d: %s twice", n+1, key)
		}
		seen[key] = true
		out = append(out, p)
	}
	return out, nil
}

func mustParsePins(s string) []DefaultPin {
	p, err := ParsePins(s)
	if err != nil {
		panic(err)
	}
	return p
}

// PlainRunnerDir is a runner_dir zm-run-tart takes: a path with no quote,
// dollar, backtick, backslash or blank (the guest's shell gets it as a word).
func PlainRunnerDir(d string) bool {
	return d != "" && d[0] != '-' && !strings.ContainsAny(d, "\"'`$\\ \t\n")
}

// PinForImage returns metadata only for an exact published digest reference.
func PinForImage(image string) (DefaultPin, bool) {
	return Lookup(defaultPins, image)
}

// Lookup finds an exact published digest in a pin table.
func Lookup(pins []DefaultPin, image string) (DefaultPin, bool) {
	for _, p := range pins {
		if image != "" && p.Image == image {
			return p, true
		}
	}
	return DefaultPin{}, false
}

// PinForRepository finds the current published pin whose image comes from the
// same repository as image (the part before "@"), so a digest from an earlier
// version of a published pin is known by its pin. Template lines are not
// matched: they are never a binding's first image.
func PinForRepository(image string) (DefaultPin, bool) {
	repo, _, ok := strings.Cut(image, "@")
	if !ok || repo == "" {
		return DefaultPin{}, false
	}
	for _, p := range defaultPins {
		if p.Template == "" && strings.HasPrefix(p.Image, repo+"@") {
			return p, true
		}
	}
	return DefaultPin{}, false
}

// NamedPin finds a published named image, optionally restricted to a layer.
func NamedPin(pins []DefaultPin, layer, name string) (DefaultPin, bool) {
	for _, p := range pins {
		if name != "" && p.Name == name && p.Image != "" && (layer == "" || p.Layer == layer) {
			return p, true
		}
	}
	return DefaultPin{}, false
}

// NamedLine finds a named line, published or not ("-": Image ""), on a layer.
func NamedLine(pins []DefaultPin, layer, name string) (DefaultPin, bool) {
	for _, p := range pins {
		if name != "" && p.Name == name && p.Layer == layer {
			return p, true
		}
	}
	return DefaultPin{}, false
}

// NamedImage reports whether a name is a published named image.
func NamedImage(name string) bool {
	_, ok := NamedPin(defaultPins, "", name)
	return ok
}
