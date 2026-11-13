package vmhost

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakePersistBox is a box binding over the fake layer, one Linux slot, two
// kept disks at most.
func fakePersistBox(t *testing.T) map[string]string {
	t.Helper()
	_, _, m := fakeBinding(t, "linux-img")
	delete(m, "image")
	m["target"], m["kind"], m["size"], m["cpu"], m["memory"] = "t", "box", "small", "1", "512"
	m["images"] = "fake/linux@sha256:" + strings.Repeat("ab", 32)
	m["linux_slots"], m["persist_max"] = "1", "2"
	return m
}

// zm-eakh.12.5.23: a stopped box holds its disk, not a slot; its start needs
// one; persist_max counts kept disks; a kept disk past its time is deleted
// by the sweep; describe says so; a runner box is never persistent.
func TestKeptDisksOverAFakeLayer(t *testing.T) {
	m := fakePersistBox(t)
	d, err := call(t, m, "describe", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(","+strings.Join(anyStrings(d["capabilities"]), ",")+",", ",persist,") ||
		!strings.Contains(strings.Join(anyStrings(d["verbs"]), ","), "stop,start") {
		t.Fatalf("describe: verbs %v, capabilities %v", d["verbs"], d["capabilities"])
	}
	if disks, _ := d["disks"].(map[string]any); disks["max"] != 2.0 || disks["kept"] != 0.0 || disks["keep_s"] != float64(persistKeepDefault) {
		t.Fatalf("describe disks: %v", d["disks"])
	}
	if _, err := call(t, m, "acquire", "", map[string]any{"request_id": "j", "persist": true, "jit_config": strings.Repeat("QUJD", 8)}); code(err) != "bad_request" {
		t.Errorf("persist with a jit_config: %v", err)
	}
	a, err := call(t, m, "acquire", "", map[string]any{"request_id": "k1", "ttl_s": 120, "persist": true})
	if err != nil {
		t.Fatal(err)
	}
	h1 := a["handle"].(string)
	if _, err := call(t, m, "acquire", "", map[string]any{"request_id": "k1", "ttl_s": 120}); code(err) != "bad_request" {
		t.Errorf("the same request_id without persist: %v", err)
	}
	if _, err := call(t, m, "acquire", "", map[string]any{"request_id": "k2", "ttl_s": 120}); code(err) != "unavailable" {
		t.Fatalf("a second box with the one slot taken: %v", err)
	}
	if s, err := call(t, m, "stop", h1, nil); err != nil || s["stopped"] != true {
		t.Fatalf("stop: %v %v", s, err)
	}
	// Stopped, it holds no slot: another box comes up, and the stopped one cannot start.
	b, err := call(t, m, "acquire", "", map[string]any{"request_id": "k2", "ttl_s": 120, "persist": true})
	if err != nil {
		t.Fatalf("a box while the other is stopped: %v", err)
	}
	if _, err := call(t, m, "start", h1, nil); code(err) != "unavailable" {
		t.Errorf("start with no free slot: %v", err)
	}
	if _, err := call(t, m, "release", b["handle"].(string), nil); err != nil {
		t.Fatal(err)
	}
	// persist_max: one kept (stopped) disk, a second live, a third refused.
	c, err := call(t, m, "acquire", "", map[string]any{"request_id": "k3", "ttl_s": 120, "persist": true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := call(t, m, "stop", c["handle"].(string), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := call(t, m, "acquire", "", map[string]any{"request_id": "k4", "ttl_s": 120, "persist": true}); code(err) != "unavailable" {
		t.Errorf("a third kept disk past persist_max 2: %v", err)
	}
	if _, err := call(t, m, "acquire", "", map[string]any{"request_id": "k5", "ttl_s": 1}); err != nil {
		t.Errorf("a throwaway box beside two kept disks: %v", err)
	}

	// The kept disk's time runs out: the sweep deletes it and the box is expired.
	st := store{m["state_dir"]}
	id := h1[strings.LastIndex(h1, "/")+1:]
	r, err := st.update(id, func(r *record) error { r.Deadline = time.Now().Add(-time.Second); return nil })
	if err != nil || r.State != stStopped {
		t.Fatalf("the stopped record: %+v %v", r, err)
	}
	vm := filepath.Join(m["state_dir"], "fakevms", r.VM)
	if _, err := os.Stat(vm); err != nil {
		t.Fatalf("a stopped box's disk is gone before its time: %v", err)
	}
	s, err := call(t, m, "status", h1, nil)
	if err != nil || s["state"] != stExpired {
		t.Fatalf("status past the kept disk's time: %v %v", s, err)
	}
	if _, err := os.Stat(vm); !os.IsNotExist(err) {
		t.Errorf("the expired disk is still there: %v", err)
	}
	if _, err := call(t, m, "start", h1, nil); code(err) != "conflict" {
		t.Errorf("start of an expired box: %v", err)
	}
	// Release deletes a stopped box's disk at once.
	rc, _ := st.load(c["handle"].(string)[strings.LastIndex(c["handle"].(string), "/")+1:])
	if rel, err := call(t, m, "release", c["handle"].(string), nil); err != nil || rel["released"] != true {
		t.Fatalf("release of a stopped box: %v %v", rel, err)
	}
	if _, err := os.Stat(filepath.Join(m["state_dir"], "fakevms", rc.VM)); !os.IsNotExist(err) {
		t.Errorf("a released stopped box's disk is still there: %v", err)
	}
}

// Secrets never land on a disk, a kept one or not: execWrap is told the
// guest's memory filesystem and its mount table, and takes secrets there or
// nowhere (no TMPDIR fallback).
func TestExecSecretsAreMemoryOnly(t *testing.T) {
	for _, c := range []struct {
		persist, sec bool
		flag         string
	}{{false, false, "0"}, {false, true, "1"}, {true, false, "0"}, {true, true, "1"}} {
		argv := wrapped(record{Persist: c.persist}, "", 30, c.sec, "", []string{"true"})
		if argv[7] != c.flag || argv[9] != guestMem || argv[10] != guestMounts || argv[11] != guestSwaps || argv[12] != "true" {
			t.Errorf("persist %v, secrets %v: %q, want flag %q, then %s, %s and %s", c.persist, c.sec, argv[4:], c.flag, guestMem, guestMounts, guestSwaps)
		}
	}
	for _, s := range []string{"TMPDIR} ||", `DIR => $base`, "/dev/shm"} {
		if strings.Contains(execWrap, s) {
			t.Errorf("execWrap still holds %q: a secret could reach a disk", s)
		}
	}
	if !strings.Contains(execWrap, "nomem() unless memfs($mem, $mnt)") || !strings.Contains(execWrap, "nomem() unless memfs($dir, $mnt)") {
		t.Error("execWrap does not refuse secrets where the mount table shows no memory filesystem")
	}
}

// The OCI layer keeps a box as the same container: HOME on a volume of its
// own (read from the image's config, never by running it), a guard that
// stops rather than removes it, and Destroy removing both.
func TestOCIKeptBox(t *testing.T) {
	o, dir := newFakeOCI(t, "none")
	img := filepath.Join(dir, "images")
	for name, inspect := range map[string]string{"runner-img": "runner\nPATH=/usr/bin\n", "home-img": "1001\nHOME=/work/me\n",
		"root-img": "\nPATH=/bin\n", "uid-img": "1001\n", "odd-img": "x\nHOME=/a,b\n"} {
		if err := os.WriteFile(filepath.Join(img, name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(img, name+".inspect"), []byte(inspect), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for image, want := range map[string]string{"runner-img": "/home/runner", "home-img": "/work/me", "root-img": "/root", "uid-img": "", "odd-img": ""} {
		home, err := o.imageHome(image)
		if home != want || (want == "") != (err != nil) {
			t.Errorf("%s: HOME %q %v, want %q", image, home, err, want)
		}
	}
	if err := o.CloneKept("runner-img", "zmrt-x-r1", 1, 512); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "c", "zmrt-x-r1", "args"))
	if !strings.Contains(string(b), "--mount\ntype=volume,source=zmrt-x-r1-home,target=/home/runner\n") || !strings.Contains(string(b), "--annotation\ndev.gvisor.flag.overlay2=none\n") {
		t.Fatalf("a kept runsc box's create: %s", b)
	}
	gv := o
	gv.runtime = "runsc"
	if err := gv.CloneKept("runner-img", "zmrt-x-r2", 1, 512); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "c", "zmrt-x-r2", "args")); !strings.Contains(string(b), "--annotation\ndev.gvisor.flag.overlay2=none\n") {
		t.Fatalf("a kept runsc box keeps its root (overlay2 none): %s", b)
	}
	gv.Destroy("zmrt-x-r2", 0)
	cmd, held, err := o.BootKept("zmrt-x-r1", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited) }()
	if err := waitReady(o, "zmrt-x-r1", time.Now().Add(5*time.Second), exited); err != nil {
		t.Fatal(err)
	}
	held.Close() // the supervisor goes: the container stops and stays
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		t.Fatal("start -a outlived the pipe")
	}
	if vs, err := o.List(); err != nil || len(vs) != 1 || vs[0].Running {
		t.Fatalf("a kept box after its supervisor: %+v %v, want one stopped container", vs, err)
	}
	o.PowerOff("zmrt-x-r1", 0) // off already: fine
	o.Destroy("zmrt-x-r1", 0)
	calls, _ := os.ReadFile(filepath.Join(dir, "calls"))
	if !strings.Contains(string(calls), "volume rm zmrt-x-r1-home") {
		t.Errorf("Destroy left the HOME volume: %s", calls)
	}
	if vs, _ := o.List(); len(vs) != 0 {
		t.Errorf("Destroy left %+v", vs)
	}
	for _, c := range []struct {
		bin, runtime string
		root         bool
	}{{"docker", "io.containerd.kata.v2", true}, {"/usr/bin/docker", "runsc", true}, {"podman", "runsc", true},
		{"nerdctl", "runsc", false}, {"nerdctl", "kata", true}} {
		if got := (ociCLI{bin: c.bin, runtime: c.runtime}).KeepsRoot(); got != c.root {
			t.Errorf("KeepsRoot %s %s = %v, want %v (nerdctl sets no runsc annotation: HOME only)", c.bin, c.runtime, got, c.root)
		}
	}
}

// Kept disks live for weeks: below persist_min_free_gb free under the
// layer's disk directory, a persist acquire and a start are refused; a
// throwaway box is not.
func TestKeptDiskFloor(t *testing.T) {
	m := fakePersistBox(t)
	a, err := call(t, m, "acquire", "", map[string]any{"request_id": "k1", "ttl_s": 120, "persist": true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := call(t, m, "stop", a["handle"].(string), nil); err != nil {
		t.Fatal(err)
	}
	m["persist_min_free_gb"] = "100000000" // more than any disk
	if _, err := call(t, m, "acquire", "", map[string]any{"request_id": "k2", "ttl_s": 120, "persist": true}); code(err) != "unavailable" || !strings.Contains(err.Error(), "persist_min_free_gb") {
		t.Errorf("a kept disk below the floor: %v", err)
	}
	if _, err := call(t, m, "start", a["handle"].(string), nil); code(err) != "unavailable" {
		t.Errorf("a start below the floor: %v", err)
	}
	if b, err := call(t, m, "acquire", "", map[string]any{"request_id": "k3", "ttl_s": 120}); err != nil {
		t.Errorf("a throwaway box below the floor: %v", err)
	} else {
		_, _ = call(t, m, "release", b["handle"].(string), nil)
	}
}

// A persistent box whose supervisor died is stopped by the sweep, its time
// up counted to the supervisor's last beat, not through the host's downtime.
func TestKeptBoxDeadSupervisorCountsToItsLastBeat(t *testing.T) {
	m := cfg(t)
	now := time.Now().UTC()
	r := put(t, m, record{ID: "r000000000001", State: stReady, Persist: true, KeepS: 3600, Supervisor: deadPID(t),
		Start: now.Add(-2 * time.Hour), Seg: now.Add(-time.Hour), RanMs: 1000, RunStart: now.Add(-time.Hour), Deadline: now.Add(time.Hour)})
	st := store{m["state_dir"]}
	beat := now.Add(-50 * time.Minute)
	if err := os.WriteFile(st.beatFile(r.ID), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(st.beatFile(r.ID), beat, beat); err != nil {
		t.Fatal(err)
	}
	s, err := call(t, m, "status", r.Handle, nil)
	if err != nil || s["state"] != stStopped {
		t.Fatalf("status after the supervisor died: %v %v", s, err)
	}
	got, _ := st.load(r.ID)
	if want := int64(1000 + 10*60*1000); got.RanMs < want-2000 || got.RanMs > want+2000 {
		t.Errorf("time up %d ms, want about %d (to the last beat)", got.RanMs, want)
	}
}

// A runsc that refuses the root's annotation fails the kept box's boot, and
// the error says so and names the runsc; other runtimes' errors pass as they are.
func TestOCIKeptBootErrNamesRunsc(t *testing.T) {
	o, _ := newFakeOCI(t, "none")
	boot := errors.New("the VM exited while booting")
	o.runtime = "kata"
	if err := o.KeptBootErr(boot); err != boot {
		t.Errorf("Kata: %v", err)
	}
	o.runtime = "runsc"
	err := o.KeptBootErr(boot)
	if !strings.Contains(err.Error(), ociRunscKeepRoot) || !strings.Contains(err.Error(), "runsc (") || !errors.Is(err, boot) {
		t.Errorf("runsc: %v", err)
	}
}
