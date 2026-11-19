package hostprobe

import "testing"

// A first try lends a Mac the tart layer's small Linux image when it is
// published, else the macOS default and says the small one is not out yet;
// a layer whose default is Linux lends its default.
func TestChooseFirstImage(t *testing.T) {
	const (
		mac   = "ghcr.io/x/macos@sha256:1111111111111111111111111111111111111111111111111111111111111111"
		small = "ghcr.io/x/small@sha256:2222222222222222222222222222222222222222222222222222222222222222"
		oci   = "ghcr.io/x/oci@sha256:3333333333333333333333333333333333333333333333333333333333333333"
	)
	macReport := ShareOptions{Report: Report{OS: "darwin", MacOS: "26.0", Arch: "arm64", Layers: []Layer{{Name: "tart", Available: true, Supported: true}}}}
	linuxReport := ShareOptions{Report: Report{OS: "linux", Layers: []Layer{{Name: "oci", Available: true, Supported: true}}}}
	macDefault := DefaultPin{Layer: "tart", MinHostMacOS: 26, Arch: "arm64", OS: "darwin", Image: mac, PullGB: 27}
	smallPin := DefaultPin{Layer: "tart", MinHostMacOS: 26, Arch: "arm64", OS: "linux", Image: small, Name: SmallLinux, PullGB: 1}
	ociPin := DefaultPin{Layer: "oci", OS: "linux", Image: oci}

	withPins(t, macDefault, smallPin, ociPin)
	f, ok := ChooseFirstImage(macReport)
	if !ok || f.With != SmallLinux || f.Lent.Image != small || f.Default.Image != mac || f.Unpublished {
		t.Errorf("Mac, small image published: %+v %v", f, ok)
	}
	f, ok = ChooseFirstImage(linuxReport)
	if !ok || f.With != "" || f.Lent.Image != oci || f.Unpublished {
		t.Errorf("Linux: %+v %v", f, ok)
	}

	unpublished := smallPin
	unpublished.Image = ""
	withPins(t, macDefault, unpublished, ociPin)
	f, ok = ChooseFirstImage(macReport)
	if !ok || f.With != "" || f.Lent.Image != mac || !f.Unpublished {
		t.Errorf("Mac, small image not published: %+v %v", f, ok)
	}

	withPins(t, macDefault, ociPin)
	if f, ok = ChooseFirstImage(macReport); !ok || f.With != "" || f.Lent.Image != mac || f.Unpublished {
		t.Errorf("Mac, no small line: %+v %v", f, ok)
	}

	// Nothing to choose: the owner named an image, or no layer is ready.
	named := macReport
	named.Images = []string{mac}
	if _, ok := ChooseFirstImage(named); ok {
		t.Error("an owner's --image still chose")
	}
	if _, ok := ChooseFirstImage(ShareOptions{Report: Report{OS: "darwin", MacOS: "26.0", Arch: "arm64", Layers: []Layer{{Name: "tart", Supported: true, Problems: []string{"no tart"}}}}}); ok {
		t.Error("a Mac with no ready layer still chose")
	}
}

// The shipped table has the tart layer's small Linux line with the floor its
// build lowers to (cmd/potluck-driver-tart/images/tart-linux/build.sh), and
// both tart images say about how much a first pull downloads.
func TestSmallLinuxPin(t *testing.T) {
	pins, err := ParsePins(imagesPin)
	if err != nil {
		t.Fatal(err)
	}
	var small, mac DefaultPin
	for _, p := range pins {
		switch {
		case p.Layer == "tart" && p.Name == SmallLinux:
			small = p
		case p.Layer == "tart" && p.Name == "" && p.Template == "" && mac.Layer == "":
			mac = p
		}
	}
	if small.OS != "linux" || small.MinCPU != 1 || small.MinMemoryMB != 2048 || small.Root || small.PullGB == 0 {
		t.Errorf("tart's small Linux line: %+v, want linux, 1 vCPU, 2048 MB, no root, a pull size", small)
	}
	if mac.OS != "darwin" || mac.PullGB == 0 {
		t.Errorf("tart's default: %+v, want macOS with a pull size", mac)
	}
	if p, err := ParsePins("tart linux - name=small-linux pull_gb=1"); err != nil || p[0].PullGB != 1 {
		t.Errorf("pull_gb key: %+v %v", p, err)
	}
}
