package vmhost

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A sweep takes VMs named zmrt-<hash>- (a step 1 driver's) and
// potluck-<the same hash>- (this build makes them) as this state
// directory's; the hash is of the directory's path, unchanged for an
// existing ~/.zm-run-tart.
func TestVMPrefixes(t *testing.T) {
	s := store{"/home/u/.zm-run-tart"}
	if s.vmPrefix() != "zmrt-"+s.vmPrefix()[5:13]+"-" || len(s.vmPrefix()) != 14 {
		t.Fatalf("vmPrefix %q", s.vmPrefix())
	}
	if s.vmPrefixNew() != "potluck-"+s.vmPrefix()[5:] {
		t.Fatalf("vmPrefixNew %q", s.vmPrefixNew())
	}
	other := store{"/home/u/.potluck-driver-tart"}
	for name, own := range map[string]bool{
		s.vmPrefix() + "r1":        true,
		s.vmPrefixNew() + "r1":     true,
		other.vmPrefix() + "r1":    false,
		other.vmPrefixNew() + "r1": false,
		"zmrt-r1":                  false,
	} {
		if s.ownVM(name) != own {
			t.Errorf("ownVM(%q) = %v", name, !own)
		}
	}
}

// Incus List takes VMs labelled user.zm.run-target (a step 1 driver's) or
// user.potluck.run-target (this build's), nothing else.
func TestIncusListBothLabels(t *testing.T) {
	l, dir := newFakeIncus(t, "https://127.0.0.1:8443")
	for name, cfg := range map[string]string{"zmrt-a": `"user.zm.run-target":"1"`, "potluck-b": `"user.potluck.run-target":"1"`, "other": `"x":"y"`} {
		if err := os.MkdirAll(filepath.Join(dir, "i", name), 0o700); err != nil {
			t.Fatal(err)
		}
		for f, s := range map[string]string{"config": cfg, "xconfig": "{}", "devices": "{}"} {
			_ = os.WriteFile(filepath.Join(dir, "i", name, f), []byte(s), 0o600)
		}
	}
	vs, err := l.List()
	if err != nil || len(vs) != 2 {
		t.Fatalf("List = %+v %v", vs, err)
	}
	got := map[string]bool{}
	for _, v := range vs {
		got[v.Name] = true
	}
	if !got["zmrt-a"] || !got["potluck-b"] {
		t.Fatalf("List = %+v", vs)
	}
}

// OCI List asks for each label (a ps's label filters are all required) and
// takes each container once.
func TestOCIListBothLabels(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "docker")
	script := "#!/bin/sh\ncase \"$*\" in\n" +
		"*label=dev.zm.run-target=1*) printf 'zmrt-a\\tUp 1 second\\n' ;;\n" +
		"*label=dev.potluck.run-target=1*) printf 'potluck-b\\tCreated\\nzmrt-a\\tUp 1 second\\n' ;;\n" +
		"esac\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	vs, err := ociCLI{bin: bin}.List()
	if err != nil || len(vs) != 2 || vs[0].Name != "zmrt-a" || !vs[0].Running || vs[1].Name != "potluck-b" || vs[1].Running {
		t.Fatalf("List = %+v %v", vs, err)
	}
}

// Rename step 2 on a state directory a step 1 driver used: a new box's VM
// is potluck-<hash>- with the hash the old directory always had; the sweep
// deletes this directory's orphans under either name (a step 1 VM no record
// holds included) and nothing of another directory's; a record keeps the
// VM name it was made under.
func TestNewBoxesNewNamesOldOnesSwept(t *testing.T) {
	_, _, m := fakeBinding(t, "linux-img")
	state := m["state_dir"]
	st := store{state}
	if want := (store{state}).vmPrefix()[len("zmrt-"):]; st.vmPrefixNew() != "potluck-"+want {
		t.Fatalf("prefixes %s %s", st.vmPrefix(), st.vmPrefixNew())
	}
	delete(m, "image")
	m["target"], m["kind"], m["size"], m["cpu"], m["memory"] = "t", "box", "small", "1", "512"
	m["images"] = "fake/linux@sha256:" + strings.Repeat("ab", 32)
	a, err := call(t, m, "acquire", "", map[string]any{"request_id": "k", "ttl_s": 120})
	if err != nil {
		t.Fatal(err)
	}
	h := a["handle"].(string)
	defer func() { _, _ = call(t, m, "release", h, nil) }()
	var vm string
	for _, r := range st.all() {
		if r.live() {
			vm = r.VM
		}
	}
	if !strings.HasPrefix(vm, st.vmPrefixNew()) {
		t.Fatalf("new box's VM %q, want prefix %s", vm, st.vmPrefixNew())
	}
	root := filepath.Join(state, "fakevms")
	l := dirLayer{root}
	for _, name := range []string{st.vmPrefix() + "orphan", st.vmPrefixNew() + "orphan", "zmrt-00000000-foreign", "potluck-00000000-foreign"} {
		if err := l.Clone("linux-img", name, 0, 0); err != nil {
			t.Fatal(err)
		}
	}
	cfg, err := parseConfig(m)
	if err != nil {
		t.Fatal(err)
	}
	d := &drv{cfg: cfg, st: st, vm: l}
	if err := d.sweep(); err != nil {
		t.Fatal(err)
	}
	left := map[string]bool{}
	ents, _ := os.ReadDir(root)
	for _, e := range ents {
		left[e.Name()] = true
	}
	if left[st.vmPrefix()+"orphan"] || left[st.vmPrefixNew()+"orphan"] || !left["zmrt-00000000-foreign"] || !left["potluck-00000000-foreign"] || !left[vm] {
		t.Fatalf("after the sweep: %v", left)
	}
}

// A volume made before step 2 keeps its layer name (zmrt-<hash>-<id>): a box
// mounts the same data; a new volume gets the new prefix.
func TestVolumesKeepTheirLayerName(t *testing.T) {
	_, _, m := fakeBinding(t, "linux-img")
	m["target"] = "t"
	cfg, err := parseConfig(m)
	if err != nil {
		t.Fatal(err)
	}
	dir := m["state_dir"]
	st := store{dir}
	if err := os.MkdirAll(filepath.Join(dir, "volumes"), 0o700); err != nil {
		t.Fatal(err)
	}
	id := volID("t", "o", "data")
	if err := st.saveVol(volMeta{ID: id, Target: "t", Owner: "o", Name: "data", Layer: st.vmPrefix() + id, Bytes: -1}); err != nil {
		t.Fatal(err)
	}
	d := &drv{cfg: cfg, st: st, vm: dirLayer{filepath.Join(dir, "fakevms")}}
	uses, err := d.takeVolumes("rtest", "o", "p", []volReq{{Name: "data", Path: "data", Mode: "ro"}, {Name: "fresh", Path: "fresh", Mode: "ro"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(uses) != 2 || uses[0].Layer != st.vmPrefix()+id || uses[1].Layer != st.vmPrefixNew()+volID("t", "o", "fresh") || !uses[1].New {
		t.Fatalf("uses %+v", uses)
	}
	if m, _ := st.loadVol(id); m.Layer != st.vmPrefix()+id {
		t.Fatalf("meta %+v", m)
	}
}

// New OCI containers, overlays and Incus VMs carry the new labels.
func TestNewLabels(t *testing.T) {
	if ociLabelNew != "dev.potluck.run-target=1" || ociVolLabel != "dev.potluck.volume=1" || ociLowerLabelNew != "dev.potluck.lower" || incusLabelNew != "user.potluck.run-target" {
		t.Fatal("labels")
	}
}
