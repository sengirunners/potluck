package imagepins

import (
	"strings"
	"testing"
)

func pinOf(repo, hex string) string { return repo + "@sha256:" + strings.Repeat(hex, 32) }

func TestCurrent(t *testing.T) {
	pins, err := ParsePins(strings.Join([]string{
		"oci linux " + pinOf("ghcr.io/p/box-linux", "aa") + " runner_dir=/home/runner",
		"oci linux " + pinOf("ghcr.io/p/box-agent", "bb") + " runner_dir=/home/runner template=agent",
		"oci linux " + pinOf("ghcr.io/p/box-browser", "cc") + " runner_dir=/opt/runner template=browser",
		"tart linux " + pinOf("ghcr.io/p/tart-linux", "dd") + " name=small-linux min_cpu=1 min_memory_mb=2048",
	}, "\n"))
	if err != nil {
		t.Fatal(err)
	}
	newLinux, newAgent := pinOf("ghcr.io/p/box-linux", "aa"), pinOf("ghcr.io/p/box-agent", "bb")
	oldLinux, oldAgent := pinOf("ghcr.io/p/box-linux", "11"), pinOf("ghcr.io/p/box-agent", "22")
	dflt := map[string]string{ImageDefaultKey: ImageDefault, "runner_dir": "/home/runner"}
	for _, c := range []struct {
		name   string
		layer  string
		images []string
		cfg    map[string]string
		now    []string
		behind bool
		owner  string // a word Move.Owner must hold; "" for none
		ok     bool
	}{
		{"a default and a template behind", "oci", []string{oldLinux, oldAgent}, dflt, []string{newLinux, newAgent}, true, "", true},
		{"bound before share wrote the key, with a template", "oci", []string{oldLinux, oldAgent}, map[string]string{"runner_dir": "/home/runner"}, []string{newLinux, newAgent}, true, "", true},
		{"bound before share wrote the key, alone", "oci", []string{oldLinux}, map[string]string{"runner_dir": "/home/runner"}, []string{newLinux}, true, "may be an image you named", true},
		{"current", "oci", []string{newLinux}, dflt, []string{newLinux}, false, "", true},
		{"the runner moved", "oci", []string{oldLinux, pinOf("ghcr.io/p/box-browser", "33")}, map[string]string{ImageDefaultKey: ImageDefault, "runner_dir": "/home/runner"},
			[]string{newLinux, pinOf("ghcr.io/p/box-browser", "cc")}, true, "runner", true},
		{"the runner stays", "oci", []string{oldLinux, oldAgent}, map[string]string{ImageDefaultKey: ImageDefault, "runner_dir": "/home/runner"}, []string{newLinux, newAgent}, true, "", true},
		{"a runner_dir the new pin adds", "oci", []string{oldLinux}, map[string]string{ImageDefaultKey: ImageDefault}, []string{newLinux}, true, "runner_dir", true},
		{"a repository this version no longer publishes", "oci", []string{oldLinux, pinOf("ghcr.io/p/box-gone", "33")}, dflt, []string{newLinux}, true, "no longer publishes ghcr.io/p/box-gone", true},
		{"a key value share never writes", "oci", []string{oldLinux}, map[string]string{ImageDefaultKey: "no", "runner_dir": "/home/runner"}, nil, false, "", false},
		{"the owner's --image of Potluck's repository", "oci", []string{oldLinux}, map[string]string{ImageDefaultKey: ImageOwners}, nil, false, "", false},
		{"the owner's own image", "oci", []string{pinOf("example.com/mine", "11")}, nil, nil, false, "", false},
		{"one of the owner's beside Potluck's", "oci", []string{oldLinux, pinOf("example.com/mine", "11")}, nil, nil, false, "", false},
		{"a template image first", "oci", []string{oldAgent}, nil, nil, false, "", false},
		{"a default image after the first", "oci", []string{oldLinux, pinOf("ghcr.io/p/box-linux", "33")}, nil, nil, false, "", false},
		{"another layer's repository", "tart", []string{oldLinux}, nil, nil, false, "", false},
		{"a named image", "tart", []string{pinOf("ghcr.io/p/tart-linux", "44")}, map[string]string{ImageDefaultKey: ImageNamed, "image_name": "small-linux", "cpu": "2", "memory": "4096"},
			[]string{pinOf("ghcr.io/p/tart-linux", "dd")}, true, "", true},
		{"under the new floor", "tart", []string{pinOf("ghcr.io/p/tart-linux", "44")}, map[string]string{ImageDefaultKey: ImageDefault, "image_name": "small-linux", "cpu": "1", "memory": "1024"},
			[]string{pinOf("ghcr.io/p/tart-linux", "dd")}, true, "needs at least", true},
		{"not pinned", "oci", []string{"ghcr.io/p/box-linux:latest"}, nil, nil, false, "", false},
		{"no images", "oci", nil, nil, nil, false, "", false},
	} {
		m, ok := Current(pins, c.layer, c.images, c.cfg)
		if strings.Join(m.Now, ",") != strings.Join(c.now, ",") || m.Behind != c.behind || ok != c.ok ||
			(c.owner == "") != (m.Owner == "") || !strings.Contains(m.Owner, c.owner) {
			t.Errorf("%s: Current = %+v %v, want %v %v owner %q %v", c.name, m, ok, c.now, c.behind, c.owner, c.ok)
		}
	}
}
