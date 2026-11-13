package vmhost

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// Root and Docker in a box (zm-eakh.12.5.26): root=on and docker=on are an
// oci binding's, need a runtime that honours setuid inside the box, and
// change exactly the create flags they name.
func TestOCIRootConfig(t *testing.T) {
	base := func(kv ...string) map[string]string {
		m := map[string]string{"target": "lx", "vm": "oci", "image": "zm/linux-ci", "remote": "https://github.com/o/r.git", "state_dir": t.TempDir(),
			"oci_runtime": "runsc-root"}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return m
	}
	c, err := parseConfig(base())
	if err != nil || c.Root || c.Docker {
		t.Fatalf("default: root %v docker %v %v", c.Root, c.Docker, err)
	}
	c, err = parseConfig(base("root", "on"))
	if err != nil || !c.Root || c.Docker {
		t.Fatalf("root=on: root %v docker %v %v", c.Root, c.Docker, err)
	}
	c, err = parseConfig(base("docker", "on"))
	if err != nil || !c.Root || !c.Docker {
		t.Fatalf("docker=on is root too: root %v docker %v %v", c.Root, c.Docker, err)
	}
	for _, r := range []string{"kata", "io.containerd.kata.v2", "runsc-root"} {
		if _, err := parseConfig(base("oci_runtime", r, "docker", "on")); err != nil {
			t.Errorf("docker=on with %s: %v", r, err)
		}
	}
	for _, c := range []struct {
		kv   []string
		want string
	}{
		{[]string{"oci_runtime", "runsc", "root", "on"}, "runsc-root"}, // gVisor ignores setuid bits without --allow-suid: sudo cannot work
		{[]string{"oci_runtime", "io.containerd.runsc.v1", "docker", "on"}, "runsc-root"},
		{[]string{"oci_cli", "podman", "root", "on"}, "docker"}, // runsc-root is a docker runtime entry
		{[]string{"root", "yes"}, "root is on or off"},
		{[]string{"docker", "1"}, "docker is on or off"},
		{[]string{"root", "off", "docker", "on"}, "docker=on needs root"},
	} {
		if _, err := parseConfig(base(c.kv...)); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%v: %v, want an error naming %q", c.kv, err, c.want)
		}
	}
	// Tart takes root=on for a box on Softnet (zm-eakh.12.54.14); never
	// docker, never a run binding, nat or Incus.
	tart := func(kv ...string) map[string]string {
		m := map[string]string{"target": "mac", "kind": "box", "vm": "tart", "size": "c4m16", "cpu": "4", "memory": "16384",
			"images": "zm/mac@sha256:" + strings.Repeat("ab", 32), "state_dir": t.TempDir(), "root": "on"}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return m
	}
	if c, err := parseConfig(tart()); err != nil || !c.Root || c.Docker {
		t.Errorf("root=on on a tart box: root %v docker %v %v", c.Root, c.Docker, err)
	}
	if c, err := parseConfig(tart("root", "off")); err != nil || c.Root {
		t.Errorf("root=off on a tart box: root %v %v", c.Root, err)
	}
	if c, err := parseConfig(tart()); err == nil {
		if again, err := parseConfig((&drv{cfg: c}).rawConfig()); err != nil || !again.Root {
			t.Errorf("tart rawConfig round trip: root %v %v", again.Root, err)
		}
	}
	for _, c := range []struct {
		kv   []string
		want string
	}{
		{[]string{"docker", "on"}, "docker is an oci binding's"},
		{[]string{"network", "nat"}, "softnet"},
		{[]string{"kind", "run", "remote", "https://github.com/o/r.git", "image", "zm/mac"}, "box binding's"},
		{[]string{"vm", "incus", "incus_profile", "zm"}, "docker is an oci binding's, and root an oci or tart one's"},
		{[]string{"root", "yes"}, "root is on or off"},
	} {
		if _, err := parseConfig(tart(c.kv...)); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("tart %v: %v, want an error naming %q", c.kv, err, c.want)
		}
	}
	// The supervisor rebuilds the same layer from the saved config.
	c, _ = parseConfig(base("docker", "on"))
	again, err := parseConfig((&drv{cfg: c}).rawConfig())
	if err != nil || !again.Root || !again.Docker {
		t.Fatalf("rawConfig round trip: root %v docker %v %v", again.Root, again.Docker, err)
	}
	c, _ = parseConfig(base())
	if again, err := parseConfig((&drv{cfg: c}).rawConfig()); err != nil || again.Root || again.Docker {
		t.Fatalf("rawConfig round trip of a default box: root %v docker %v %v", again.Root, again.Docker, err)
	}
}

func TestOCICloneRoot(t *testing.T) {
	create := func(o ociCLI, dir string) string {
		t.Helper()
		if err := o.Clone("img", "vm1", 1, 512); err != nil {
			t.Fatal(err)
		}
		b, _ := os.ReadFile(filepath.Join(dir, "c", "vm1", "args"))
		os.RemoveAll(filepath.Join(dir, "c", "vm1"))
		return "\n" + string(b)
	}
	o, dir := newFakeOCI(t, "mine")
	plain := create(o, dir)
	if !strings.Contains(plain, "\nno-new-privileges\n") || strings.Contains(plain, "zm-dockerd") || strings.Contains(plain, "DOCKER_HOST") {
		t.Fatalf("a default box keeps no-new-privileges and starts no docker: %s", plain)
	}
	o.root = true
	root := create(o, dir)
	if strings.Contains(root, "no-new-privileges") {
		t.Errorf("root=on: sudo is setuid, which no-new-privileges stops: %s", root)
	}
	if strings.Contains(root, "zm-dockerd") || strings.Contains(root, "DOCKER_HOST") {
		t.Errorf("root=on alone starts no docker: %s", root)
	}
	o.docker = true
	dock := create(o, dir)
	if strings.Contains(dock, "no-new-privileges") || !strings.Contains(dock, "\n--env\nDOCKER_HOST=unix://"+ociDockerSock+"\n") ||
		!strings.Contains(dock, "\n--env\nXDG_RUNTIME_DIR="+ociDockerRun+"\n") || !strings.Contains(dock, "zm-dockerd") {
		t.Errorf("docker=on: %s", dock)
	}
	// Nothing else moves: the same network flags, and never a capability,
	// a device or a host mount.
	none, _ := newFakeOCI(t, "none")
	none.root = true
	if a := create(none, os.Getenv("FAKE_OCI")); !strings.Contains(a, "\n--cap-drop\nNET_RAW\n") {
		t.Errorf("a root box without a network keeps NET_RAW: root holds the bounding set: %s", a)
	}
	for _, a := range []string{root, dock} {
		if !strings.Contains(a, "\n--cap-drop\nNET_RAW\n") || !strings.Contains(a, "\n--pids-limit\n4096\n") {
			t.Errorf("root lost a default flag: %s", a)
		}
		for _, never := range []string{"--cap-add", "--privileged", "--device", "--volume", "-v", "seccomp", "apparmor", "--userns"} {
			if strings.Contains(a, "\n"+never+"\n") || strings.Contains(a, never+"=") || strings.Contains(a, never+":") {
				t.Errorf("root=on adds %s: %s", never, a)
			}
		}
	}
}

func TestOCIRootDescribe(t *testing.T) {
	o, dir := newFakeOCI(t, "none")
	img := "zm/box@sha256:" + strings.Repeat("ab", 32)
	_ = os.MkdirAll(filepath.Join(dir, "images", "zm"), 0o700)
	if err := os.WriteFile(filepath.Join(dir, "images", img), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		root, docker string
		want         []string
		not          []string
	}{
		{"", "", nil, []string{"root", "docker"}},
		{"on", "", []string{"root"}, []string{"docker"}},
		{"", "on", []string{"root", "docker"}, nil},
	} {
		m := map[string]string{"target": "lx", "kind": "box", "vm": "oci", "oci_cli": o.bin, "oci_runtime": "runsc-root",
			"state_dir": t.TempDir(), "size": "c1m2", "cpu": "1", "memory": "512", "images": img, "root": c.root, "docker": c.docker}
		d, err := call(t, m, "describe", "", nil)
		if err != nil {
			t.Fatal(err)
		}
		caps := " " + strings.Trim(fmt.Sprint(d["capabilities"]), "[]") + " "
		for _, w := range c.want {
			if !strings.Contains(caps, " "+w+" ") {
				t.Errorf("root %q docker %q: capabilities %s lack %s", c.root, c.docker, caps, w)
			}
		}
		for _, n := range c.not {
			if strings.Contains(caps, " "+n+" ") {
				t.Errorf("root %q docker %q: capabilities %s list %s", c.root, c.docker, caps, n)
			}
		}
	}
}

// runsc-root is trusted by what docker says it is, not by its name: the
// same runsc as docker's runsc entry, with exactly --allow-suid.
func TestOCIRootRuntimeChecked(t *testing.T) {
	o, dir := newFakeOCI(t, "none")
	o.runtime, o.root = ociSudoRuntime, true
	for _, c := range []struct {
		runtimes, want string
	}{
		{fakeRuntimes, ""},
		{`{"runsc":{"path":"/usr/local/bin/runsc"},"runsc-root":{"path":"/usr/local/bin/runsc","runtimeArgs":["--allow-suid","--net-raw"]}}`, "--allow-suid"},
		{`{"runsc":{"path":"/usr/local/bin/runsc"},"runsc-root":{"path":"/usr/local/bin/runsc","runtimeArgs":["--allow-suid","--network=host"]}}`, "--allow-suid"},
		{`{"runsc":{"path":"/usr/local/bin/runsc"},"runsc-root":{"path":"/usr/local/bin/runsc"}}`, "--allow-suid"},
		{`{"runsc":{"path":"/usr/local/bin/runsc"},"runsc-root":{"path":"/usr/bin/runc","runtimeArgs":["--allow-suid"]}}`, "/usr/bin/runc"},
		{`{"runsc":{"path":"/usr/local/bin/runsc"},"runsc-root":{"path":"/opt/old/runsc","runtimeArgs":["--allow-suid"]}}`, "/opt/old/runsc"},
		{`{"runsc":{"path":"/usr/local/bin/runsc"}}`, "no runtime runsc-root"},
		{`not json`, "runsc-root"},
	} {
		if err := os.WriteFile(filepath.Join(dir, "runtimes"), []byte(strings.ReplaceAll(c.runtimes, "/usr/local/bin/runsc", filepath.Join(dir, "runsc"))), 0o600); err != nil {
			t.Fatal(err)
		}
		err := o.Clone("img", "vm1", 1, 512)
		made := false
		if _, e := os.Stat(filepath.Join(dir, "c", "vm1")); e == nil {
			made = true
			os.RemoveAll(filepath.Join(dir, "c", "vm1"))
		}
		switch {
		case c.want == "" && (err != nil || !made):
			t.Errorf("zm's runsc-root: %v (created %v)", err, made)
		case c.want != "" && (err == nil || made || !strings.Contains(err.Error(), c.want)):
			t.Errorf("%s: created %v, %v; want a refusal naming %q", c.runtimes, made, err, c.want)
		}
	}
}

// A kept box keeps the root and docker it was made with (its container,
// flags and all, is reused at start), so a start under a binding that now
// says otherwise is refused: either way the box would not be what describe
// says. Its disk stays kept until release or its time.
func TestOCIKeptBoxRootMustMatchTheBinding(t *testing.T) {
	o, dir := newFakeOCI(t, "none")
	img := "zm/box@sha256:" + strings.Repeat("ab", 32)
	_ = os.MkdirAll(filepath.Join(dir, "images", "zm"), 0o700)
	for f, b := range map[string]string{img: "", img + ".inspect": "runner\nHOME=/home/runner\n"} {
		if err := os.WriteFile(filepath.Join(dir, "images", f), []byte(b), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for i, c := range []struct{ from, to string }{{"on", "off"}, {"off", "on"}, {"on", ""}} {
		state := stateDir(t)
		m := map[string]string{"target": "lx", "kind": "box", "vm": "oci", "oci_cli": o.bin, "oci_runtime": ociSudoRuntime, "docker": c.from,
			"state_dir": state, "size": "c1m2", "cpu": "1", "memory": "512", "images": img, "boot_timeout_s": "20", "persist_min_free_gb": "0"}
		a, err := call(t, m, "acquire", "", map[string]any{"request_id": fmt.Sprint("k", i), "actor": "a", "ttl_s": 120, "persist": true})
		if err != nil {
			t.Fatalf("docker %q: acquire: %v", c.from, err)
		}
		h := a["handle"].(string)
		t.Cleanup(func() { _, _ = call(t, m, "release", h, map[string]any{}) }) // stateDir's cleanup then waits for its supervisor
		if _, err := call(t, m, "stop", h, nil); err != nil {
			t.Fatal(err)
		}
		m["docker"] = c.to
		_, err = call(t, m, "start", h, nil)
		if code(err) != "conflict" || !strings.Contains(err.Error(), "docker") {
			t.Errorf("made with docker %q, started with docker %q: %v, want a conflict naming docker", c.from, c.to, err)
		}
		if s, _ := call(t, m, "status", h, nil); s["state"] != stStopped {
			t.Errorf("after the refused start: %v, want stopped (its disk kept)", s["state"])
		}
		m["docker"] = c.from
		if c.from == "on" { // docker resolves runsc-root at every start: a start checks it again
			if _, err := call(t, m, "stop", h, nil); err != nil && code(err) != "conflict" {
				t.Fatal(err)
			}
			good, _ := os.ReadFile(filepath.Join(dir, "runtimes"))
			bad := strings.Replace(string(good), `["--allow-suid"]`, `["--allow-suid","--net-raw"]`, 1)
			if err := os.WriteFile(filepath.Join(dir, "runtimes"), []byte(bad), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := call(t, m, "start", h, nil); err == nil || !strings.Contains(err.Error(), "--allow-suid") {
				t.Errorf("start on a runsc-root that gained --net-raw: %v, want refused", err)
			}
			if s, _ := call(t, m, "status", h, nil); s["state"] != stStopped {
				t.Errorf("after the refused start: %v, want stopped", s["state"])
			}
			_ = os.WriteFile(filepath.Join(dir, "runtimes"), good, 0o600)
		}
		if _, err := call(t, m, "start", h, nil); err != nil {
			t.Errorf("docker %q both times: start: %v", c.from, err)
		}
	}
}

// docker info names runsc-root by what runsc install wrote (its resolved
// executable), runsc perhaps by a symlink or a bare name: both resolve first.
func TestOCIRootRuntimeSymlinked(t *testing.T) {
	bin := t.TempDir()
	real := filepath.Join(bin, "runsc-20261102")
	if err := os.WriteFile(real, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(bin, "runsc")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	for _, runsc := range []string{link, "runsc"} {
		js := `{"runsc":{"path":"` + runsc + `"},"runsc-root":{"path":"` + real + `","runtimeArgs":["--allow-suid"]}}`
		if why := sudoRuntimeWrong(js); why != "" {
			t.Errorf("runsc entry %s, runsc-root its target: %s", runsc, why)
		}
	}
	if why := sudoRuntimeWrong(`{"runsc":{"path":"` + link + `"},"runsc-root":{"path":"/usr/bin/runc","runtimeArgs":["--allow-suid"]}}`); why == "" {
		t.Error("runc as runsc-root passed")
	}
}

// Root drops MKNOD too: root holds the bounding set, and a box has no use
// for device nodes.
func TestOCIRootDropsMknod(t *testing.T) {
	o, dir := newFakeOCI(t, "none")
	o.root = true
	if err := o.Clone("img", "vm1", 1, 512); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "c", "vm1", "args")); !strings.Contains(string(b), "\n--cap-drop\nMKNOD\n") {
		t.Errorf("root=on keeps MKNOD: %s", b)
	}
	os.RemoveAll(filepath.Join(dir, "c", "vm1"))
	o.root = false
	if err := o.Clone("img", "vm1", 1, 512); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "c", "vm1", "args")); strings.Contains(string(b), "MKNOD") {
		t.Errorf("a default box's flags changed: %s", b)
	}
}

// A Tart box with root=on lists capability root for a macOS image only, and
// refuses to boot any other image (zm-eakh.12.54.14): a Linux guest's root
// holds NET_ADMIN and NET_RAW in its VM. Without root=on it lists none.
func TestTartRootDescribe(t *testing.T) {
	mac, linux := "mac/box@sha256:"+strings.Repeat("ab", 32), "fake/linux@sha256:"+strings.Repeat("cd", 32)
	for _, c := range []struct {
		root, images string
		listed       bool
		why          string
	}{
		{"on", mac, true, ""},
		{"", mac, false, ""},
		{"on", linux, false, "root=on is for macOS images"},
	} {
		_, _, m := fakeBinding(t, "linux-img")
		delete(m, "image")
		m["target"], m["kind"], m["size"], m["cpu"], m["memory"], m["images"], m["root"] = "t", "box", "small", "1", "512", c.images, c.root
		d, err := call(t, m, "describe", "", nil)
		if err != nil {
			t.Fatal(err)
		}
		caps := anyStrings(d["capabilities"])
		if slices.Contains(caps, "root") != c.listed {
			t.Errorf("root %q images %s: capabilities %v, want root listed %v", c.root, c.images, caps, c.listed)
		}
		why, _ := d["why"].(string)
		if c.why != "" && (d["available"] != false || !strings.Contains(why, c.why)) {
			t.Errorf("root %q images %s: available %v why %q, want unavailable naming %q", c.root, c.images, d["available"], why, c.why)
		}
	}
	_, _, m := fakeBinding(t, "linux-img")
	delete(m, "image")
	m["target"], m["kind"], m["size"], m["cpu"], m["memory"], m["images"], m["root"] = "t", "box", "small", "1", "512", mac+","+linux, "on"
	_, err := call(t, m, "acquire", "", map[string]any{"request_id": "k", "ttl_s": 120, "image": linux})
	if code(err) != "bad_request" || !strings.Contains(fmt.Sprint(err), "root=on is for macOS images") {
		t.Errorf("acquire of a Linux image on a root=on tart box: %v, want bad_request", err)
	}
}
