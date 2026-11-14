package vmhost

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// fakeVolumeBox is a box binding over the fake layer with four Linux slots.
func fakeVolumeBox(t *testing.T) map[string]string {
	t.Helper()
	_, _, m := fakeBinding(t, "linux-img")
	delete(m, "image")
	m["target"], m["kind"], m["size"], m["cpu"], m["memory"] = "t", "box", "small", "1", "512"
	m["images"] = "fake/linux@sha256:" + strings.Repeat("ab", 32)
	m["linux_slots"] = "4"
	return m
}

func volArgs(key, owner string, vols ...map[string]any) map[string]any {
	return map[string]any{"request_id": key, "ttl_s": 120, "actor": "someone", "volume_owner": owner, "volume_project": "project-of-" + owner, "volumes": vols}
}

func vol(name, path, mode string) map[string]any {
	return map[string]any{"name": name, "path": path, "mode": mode}
}

// sh runs a shell line in box h: its exit and stdout.
func sh(t *testing.T, m map[string]string, h, line string) (float64, string) {
	t.Helper()
	res, err := call(t, m, "exec", h, map[string]any{"argv": []string{"sh", "-c", line}, "timeout_s": 10})
	if err != nil {
		t.Fatalf("exec %q: %v", line, err)
	}
	return res["exit"].(float64), res["stdout"].(string)
}

func acquireVol(t *testing.T, m map[string]string, a map[string]any) string {
	t.Helper()
	res, err := call(t, m, "acquire", "", a)
	if err != nil {
		t.Fatalf("acquire %v: %v", a["volumes"], err)
	}
	return res["handle"].(string)
}

func release(t *testing.T, m map[string]string, h string) {
	t.Helper()
	if _, err := call(t, m, "release", h, nil); err != nil {
		t.Fatalf("release %s: %v", h, err)
	}
}

// zm-eakh.12.32.1: a named volume outlives its box; rw is one box's, ro and
// cow are shared; an owner's volumes are its own; the answer says which are new.
func TestNamedVolumesOverAFakeLayer(t *testing.T) {
	m := fakeVolumeBox(t)
	d, err := call(t, m, "describe", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(","+strings.Join(anyStrings(d["capabilities"]), ",")+",", ",volumes,") {
		t.Fatalf("describe lists no volumes capability: %v", d["capabilities"])
	}
	if v, _ := d["volumes"].(map[string]any); v["max_gb"] != float64(volumeMaxGBDefault) || v["keep_s"] != float64(volumeKeepDefault) {
		t.Fatalf("describe volumes: %v", d["volumes"])
	}
	for name, a := range map[string]map[string]any{
		"a bad name":             volArgs("b1", "o", vol("Bad Name", "cache", "rw")),
		"a path with ..":         volArgs("b2", "o", vol("c", "../escape", "rw")),
		"a system path":          volArgs("b3", "o", vol("c", "/etc/x", "rw")),
		"an absolute path":       volArgs("b3a", "o", vol("c", "/home/x", "rw")),
		"the root":               volArgs("b3b", "o", vol("c", "/", "rw")),
		"a climb":                volArgs("b3c", "o", vol("c", "a/../../x", "rw")),
		"a dot segment":          volArgs("b3d", "o", vol("c", "./x", "rw")),
		"mount options":          volArgs("b3e", "o", vol("c", "x,readonly=false", "rw")),
		"a bad mode":             volArgs("b4", "o", vol("c", "cache", "rwx")),
		"overlapping paths":      volArgs("b5", "o", vol("c", "cache", "rw"), vol("d", "cache/sub", "ro")),
		"no owner":               {"request_id": "b6", "volumes": []any{vol("c", "cache", "rw")}},
		"with persist":           {"request_id": "b7", "actor": "a", "persist": true, "volumes": []any{vol("c", "cache", "rw")}},
		"a comma in the path":    volArgs("b8", "o", vol("c", "a,b", "rw")),
		"the same name twice":    volArgs("b9", "o", vol("c", "x", "rw"), vol("c", "y", "ro")),
		"with a GitHub runner's": {"request_id": "b10", "actor": "a", "jit_config": strings.Repeat("QUJD", 8), "volumes": []any{vol("c", "cache", "rw")}},
	} {
		if _, err := call(t, m, "acquire", "", a); code(err) != "bad_request" {
			t.Errorf("%s: %v, want bad_request", name, err)
		}
	}

	// rw: written in one box, read in the next; new only the first time.
	a1, err := call(t, m, "acquire", "", volArgs("k1", "alice", vol("cache", "cache", "")))
	if err != nil {
		t.Fatal(err)
	}
	h1 := a1["handle"].(string)
	if vs, _ := a1["volumes"].([]any); len(vs) != 1 || vs[0].(map[string]any)["new"] != true || vs[0].(map[string]any)["mode"] != "rw" {
		t.Fatalf("the first acquire's volumes: %v", a1["volumes"])
	}
	if code, _ := sh(t, m, h1, `echo kept > "$HOME/cache/f"`); code != 0 {
		t.Fatal("writing a rw volume failed")
	}
	if _, err := call(t, m, "acquire", "", volArgs("k1", "alice", vol("cache", "other", "rw"))); code(err) != "bad_request" {
		t.Errorf("the same request_id with other volumes: %v", err)
	}
	// rw is one box's: another rw, ro or cow waits.
	for _, mode := range []string{"rw", "ro", "cow"} {
		if _, err := call(t, m, "acquire", "", volArgs("busy-"+mode, "alice", vol("cache", "cache", mode))); code(err) != "unavailable" {
			t.Errorf("%s while another box holds it rw: %v, want unavailable", mode, err)
		}
	}
	release(t, m, h1)
	a2, err := call(t, m, "acquire", "", volArgs("k2", "alice", vol("cache", "cache", "rw")))
	if err != nil {
		t.Fatal(err)
	}
	h2 := a2["handle"].(string)
	if vs, _ := a2["volumes"].([]any); len(vs) != 1 || vs[0].(map[string]any)["new"] != false {
		t.Errorf("the second acquire's volumes: %v (want new false)", a2["volumes"])
	}
	if _, out := sh(t, m, h2, `cat "$HOME/cache/f"`); out != "kept\n" {
		t.Fatalf("the next box reads %q from the volume, want kept", out)
	}
	release(t, m, h2)

	// Another owner's volume of the same name is another volume.
	h3 := acquireVol(t, m, volArgs("k3", "bob", vol("cache", "cache", "rw")))
	if _, out := sh(t, m, h3, `cat "$HOME/cache/f" 2>/dev/null || echo none`); out != "none\n" {
		t.Errorf("bob reads alice's volume: %q", out)
	}
	release(t, m, h3)

	// ro and cow share; ro refuses writes; cow writes stay in the box; rw waits for them.
	r1 := acquireVol(t, m, volArgs("r1", "alice", vol("cache", "cache", "ro")))
	c1 := acquireVol(t, m, volArgs("c1", "alice", vol("cache", "cache", "cow")))
	c2 := acquireVol(t, m, volArgs("c2", "alice", vol("cache", "cache", "cow")))
	if code, _ := sh(t, m, r1, `echo x > "$HOME/cache/f"`); code == 0 {
		t.Error("a ro volume took a write")
	}
	if code, _ := sh(t, m, c1, `echo mine > "$HOME/cache/f" && cat "$HOME/cache/f"`); code != 0 {
		t.Error("a cow volume refused a write")
	}
	if _, out := sh(t, m, c2, `cat "$HOME/cache/f"`); out != "kept\n" {
		t.Errorf("another cow box sees %q, want the volume as it was", out)
	}
	if _, err := call(t, m, "acquire", "", volArgs("w", "alice", vol("cache", "cache", "rw"))); code(err) != "unavailable" {
		t.Errorf("rw while ro and cow boxes hold it: %v, want unavailable", err)
	}
	for _, h := range []string{r1, c1, c2} {
		release(t, m, h)
	}
	h4 := acquireVol(t, m, volArgs("k4", "alice", vol("cache", "cache", "rw")))
	if _, out := sh(t, m, h4, `cat "$HOME/cache/f"`); out != "kept\n" {
		t.Errorf("after the cow boxes the volume reads %q, want kept (their writes were theirs)", out)
	}
	release(t, m, h4)

	// Its size is measured on the host once a rw box ends.
	st := store{m["state_dir"]}
	id := volID("t", "alice", "cache")
	deadline := time.Now().Add(10 * time.Second)
	for {
		if v, ok := st.loadVol(id); ok && v.Bytes == int64(len("kept\n")) {
			break
		}
		if time.Now().After(deadline) {
			v, _ := st.loadVol(id)
			t.Fatalf("volume bytes after a rw box: %+v", v)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// volume_max_gb: a new volume past an owner's cap deletes the owner's least
// recently used volume no box holds, never another owner's or a held one;
// with every one held it is refused. volume_keep_s: the sweep deletes a
// volume no box named for that long.
func TestVolumeCapAndKeep(t *testing.T) {
	m := fakeVolumeBox(t)
	m["volume_max_gb"] = "1"
	st := store{m["state_dir"]}
	fake := dirLayer{filepath.Join(m["state_dir"], "fakevms")}
	old := time.Now().Add(-time.Hour).UTC()
	seed := func(owner, name string, gib int64, used time.Time) volMeta {
		v := volMeta{ID: volID("t", owner, name), Target: "t", Owner: owner, Project: "project-of-" + owner, Name: name, Layer: st.vmPrefix() + volID("t", owner, name),
			Created: used, Used: used, Bytes: gib << 30}
		if err := st.saveVol(v); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(fake.vol(v.Layer), 0o700); err != nil {
			t.Fatal(err)
		}
		return v
	}
	held := seed("alice", "held", 0, old)
	h := acquireVol(t, m, volArgs("h", "alice", vol("held", "held", "ro")))
	big := seed("alice", "big", 1, old.Add(time.Minute))
	bobs := seed("bob", "big", 5, old)
	n := acquireVol(t, m, volArgs("n", "alice", vol("fresh", "fresh", "rw")))
	if _, ok := st.loadVol(big.ID); ok {
		t.Error("the owner's least recently used free volume was kept past the cap")
	}
	if _, err := os.Stat(fake.vol(big.Layer)); err == nil {
		t.Error("its layer volume is still there")
	}
	for _, v := range []volMeta{held, bobs} {
		if _, ok := st.loadVol(v.ID); !ok {
			t.Errorf("%s/%s was deleted (held, or another owner's)", v.Owner, v.Name)
		}
	}
	release(t, m, n)
	// Every one of the owner's volumes held and the cap reached: refused.
	_ = seed("alice", "held", 2, time.Now().UTC())
	if _, err := call(t, m, "acquire", "", volArgs("x", "alice", vol("another", "a", "rw"))); code(err) != "unavailable" {
		t.Errorf("a new volume with the cap reached by a held one: %v, want unavailable", err)
	}
	release(t, m, h)

	// The sweep (any verb) deletes volumes unnamed for volume_keep_s.
	m["volume_keep_s"] = "60"
	stale := seed("carol", "stale", 0, time.Now().Add(-2*time.Minute).UTC())
	fresh := seed("carol", "recent", 0, time.Now().UTC())
	if _, err := call(t, m, "list", "", nil); err != nil {
		t.Fatal(err)
	}
	if _, ok := st.loadVol(stale.ID); ok {
		t.Error("a volume unnamed past volume_keep_s survived the sweep")
	}
	if _, ok := st.loadVol(fresh.ID); !ok {
		t.Error("a recently named volume was swept")
	}
}

// A layer that mounts no volumes, or volume_max_gb 0, lists no capability and refuses volumes.
func TestNoVolumesWithoutTheCapability(t *testing.T) {
	m := fakeVolumeBox(t)
	m["volume_max_gb"] = "0"
	d, err := call(t, m, "describe", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(anyStrings(d["capabilities"]), ","), "volumes") {
		t.Errorf("volume_max_gb 0 lists volumes: %v", d["capabilities"])
	}
	if _, err := call(t, m, "acquire", "", volArgs("k", "o", vol("c", "c", "rw"))); code(err) != "bad_request" {
		t.Errorf("volumes with volume_max_gb 0: %v, want bad_request", err)
	}
}

// zm-eakh.12.32.1 review: the cap holds on every writing acquire (not only a
// new volume's), an unmeasured volume counts, eviction never takes the
// volume the acquire mounts, and a box past the cap while it runs, rw or cow,
// is ended with why volume quota.
func TestVolumeQuota(t *testing.T) {
	m := fakeVolumeBox(t)
	m["volume_max_mb"], m["volume_check_s"] = "1", "1"
	st := store{m["state_dir"]}
	fake := dirLayer{filepath.Join(m["state_dir"], "fakevms")}
	seed := func(owner, name string, b int64, used time.Time) volMeta {
		v := volMeta{ID: volID("t", owner, name), Target: "t", Owner: owner, Project: "project-of-" + owner, Name: name, Layer: st.vmPrefix() + volID("t", owner, name),
			Created: used, Used: used, Bytes: b}
		if err := st.saveVol(v); err != nil {
			t.Fatal(err)
		}
		_ = os.MkdirAll(fake.vol(v.Layer), 0o700)
		return v
	}
	old := time.Now().Add(-time.Hour).UTC()
	// Over the cap with only the volume asked for: it is not evicted for itself, and the acquire is refused.
	big := seed("alice", "big", 2<<20, old)
	if _, err := call(t, m, "acquire", "", volArgs("k1", "alice", vol("big", "big", "rw"))); code(err) != "unavailable" || !strings.Contains(err.Error(), "volume quota") {
		t.Errorf("rw on an existing volume past the cap: %v, want unavailable volume quota", err)
	}
	if _, ok := st.loadVol(big.ID); !ok {
		t.Fatal("the acquire evicted the very volume it asked for")
	}
	if _, err := call(t, m, "acquire", "", volArgs("k2", "alice", vol("big", "big", "ro"))); err != nil {
		t.Errorf("ro writes nothing, so the cap does not refuse it: %v", err)
	}
	// An unmeasured volume counts (a quarter of the cap here): four fill it.
	for i, n := range []string{"u1", "u2", "u3", "u4"} {
		_ = seed("bob", n, -1, old.Add(time.Duration(i)*time.Minute))
	}
	h := acquireVol(t, m, volArgs("k3", "bob", vol("u4", "u4", "rw")))
	if _, ok := st.loadVol(volID("t", "bob", "u1")); ok {
		t.Error("four unmeasured volumes at the cap: the least recently used was kept")
	}
	release(t, m, h)

	// Past the cap while running: rw, then cow, ended as volume quota within volume_check_s.
	for _, mode := range []string{"rw", "cow"} {
		owner := "carol-" + mode
		h := acquireVol(t, m, volArgs("q-"+mode, owner, vol("q", "q", mode)))
		if code, _ := sh(t, m, h, `head -c 2097152 /dev/zero > "$HOME/q/big"`); code != 0 {
			t.Fatalf("%s: writing 2 MiB failed", mode)
		}
		deadline := time.Now().Add(15 * time.Second)
		for {
			s, err := call(t, m, "status", h, nil)
			if err == nil && s["state"] == "reclaimed" && strings.Contains(fmt.Sprint(s["why"]), "volume quota") {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s box 2 MiB past a 1 MiB cap: %v %v", mode, s, err)
			}
			time.Sleep(200 * time.Millisecond)
		}
	}
}

// A box whose supervisor died: the sweep measures what it wrote.
func TestSweepMeasuresADeadSupervisorsVolumes(t *testing.T) {
	m := fakeVolumeBox(t)
	h := acquireVol(t, m, volArgs("k", "dave", vol("c", "c", "rw")))
	if code, _ := sh(t, m, h, `printf 1234567 > "$HOME/c/f"`); code != 0 {
		t.Fatal("write")
	}
	st := store{m["state_dir"]}
	r, err := st.load(idOf(h))
	if err != nil {
		t.Fatal(err)
	}
	_ = syscall.Kill(r.Supervisor, syscall.SIGKILL) // a zombie of this test binary still answers kill 0: name a dead pid
	if _, err := st.update(r.ID, func(o *record) error { o.Supervisor = deadPID(t); return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := call(t, m, "list", "", nil); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(100 * time.Millisecond) { // measured detached, outside the lock
		v, _ := st.loadVol(volID("t", "dave", "c"))
		if v.Bytes == 7 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("after the sweep the volume reads %d bytes, want 7", v.Bytes)
		}
	}
}

func idOf(h string) string { return h[strings.LastIndex(h, "/")+1:] }

// A mount path that HOME makes a system directory is refused at clone; a
// volume a container still mounts, or a cow copy is layered on, is never deleted.
func TestOCIVolumeGuards(t *testing.T) {
	o, dir := newFakeOCI(t, "none")
	o.stateDir = t.TempDir()
	img := filepath.Join(dir, "images")
	for name, inspect := range map[string]string{"usr-img": "agent\nHOME=/usr/local/agent\n", "ok-img": "agent\nHOME=/home/agent\n"} {
		_ = os.WriteFile(filepath.Join(img, name), nil, 0o600)
		_ = os.WriteFile(filepath.Join(img, name+".inspect"), []byte(inspect), 0o600)
	}
	if err := o.CloneVolumes("usr-img", "zmrt-x-r1", 1, 512, []mount{{Vol: "v1", Path: "cache", Mode: volRW}}); err == nil || !strings.Contains(err.Error(), "system directory") {
		t.Errorf("a mount HOME puts under /usr: %v", err)
	}
	if err := o.CloneVolumes("ok-img", "zmrt-x-r2", 1, 512, []mount{{Vol: "v1", Path: "cache", Mode: volRW}}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "inuse-v1"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := o.DeleteVolume("v1"); err == nil {
		t.Error("a volume a container mounts was deleted")
	}
	os.Remove(filepath.Join(dir, "inuse-v1"))
	if err := o.DeleteVolume("v1"); err != nil {
		t.Errorf("a volume nothing mounts: %v", err)
	}
	if got := o.COWVolumes("vm", []mount{{Mode: volRW}, {Mode: volCOW}}); strings.Join(got, ",") != "vm-cow1u,vm-cow1w" {
		t.Errorf("COWVolumes: %v", got)
	}
}

// Review (zm-eakh.12.32.1): a box's measurement runs outside the state lock,
// so a slow one never stalls another verb; three failed in a row end the box.
func TestVolumeMeasureOutsideTheLock(t *testing.T) {
	m := fakeVolumeBox(t)
	m["volume_check_s"] = "1"
	root := filepath.Dir(filepath.Join(m["state_dir"], "fakevms"))
	h := acquireVol(t, m, volArgs("k", "erin", vol("c", "c", "rw")))
	if err := os.WriteFile(filepath.Join(root, "measure-delay"), []byte("3"), 0o600); err != nil {
		t.Fatal(err)
	}
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(200 * time.Millisecond) {
		start := time.Now()
		if _, err := call(t, m, "status", h, nil); err != nil {
			t.Fatal(err)
		}
		if took := time.Since(start); took > 1500*time.Millisecond {
			t.Fatalf("status waited %s behind a 3 s measurement", took)
		}
	}
	os.Remove(filepath.Join(root, "measure-delay"))
	if err := os.WriteFile(filepath.Join(root, "measure-fail"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(15 * time.Second); ; time.Sleep(200 * time.Millisecond) {
		s, err := call(t, m, "status", h, nil)
		if err == nil && s["state"] == "reclaimed" && strings.Contains(fmt.Sprint(s["why"]), "cannot measure") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("a box whose volumes cannot be measured: %v %v", s, err)
		}
	}
}

// Review: a project's owners share a cap (default 4x an owner's), and the
// binding has one (8x): many owners of one project cannot fill the host.
func TestVolumeProjectAndBindingCaps(t *testing.T) {
	m := fakeVolumeBox(t)
	m["volume_max_mb"] = "4" // owner 4 MiB, project 16 MiB, binding 32 MiB
	st := store{m["state_dir"]}
	seed := func(project, owner string, mib int64) {
		v := volMeta{ID: volID("t", owner, "c"), Target: "t", Owner: owner, Project: project, Name: "c", Layer: st.vmPrefix() + volID("t", owner, "c"),
			Created: time.Now().UTC(), Used: time.Now().UTC(), Bytes: mib << 20}
		if err := st.saveVol(v); err != nil {
			t.Fatal(err)
		}
	}
	for _, o := range []string{"p1", "p2", "p3", "p4", "p5"} {
		seed("P", o, 3)
	}
	args := func(key, project, owner string) map[string]any {
		a := volArgs(key, owner, vol("c", "c", "rw"))
		a["volume_project"] = project
		return a
	}
	if _, err := call(t, m, "acquire", "", args("k1", "P", "p6")); code(err) != "unavailable" || !strings.Contains(err.Error(), "this project's") {
		t.Errorf("a sixth owner of a project at its cap: %v, want unavailable (project cap)", err)
	}
	h := acquireVol(t, m, args("k2", "Q", "q1"))
	release(t, m, h)
	for _, o := range []string{"q2", "q3", "q4", "q5", "q6"} {
		seed("Q", o, 3)
	}
	for _, o := range []string{"r1", "r2", "r3"} {
		seed("R", o, 1)
	}
	if _, err := call(t, m, "acquire", "", args("k3", "S", "s1")); code(err) != "unavailable" || !strings.Contains(err.Error(), "this binding's") {
		t.Errorf("a new project's owner with the binding at its cap: %v, want unavailable (binding cap)", err)
	}
}

// Delta review: a failed measurement still checks the host's floor, and a
// volume whose last measurement failed counts as the whole cap, so a tree
// too slow to measure neither escapes the floor nor frees room for more.
func TestFailedMeasureCountsTheCapAndKeepsTheFloor(t *testing.T) {
	m := fakeVolumeBox(t)
	m["volume_check_s"], m["persist_min_free_gb"] = "1", "5"
	root := filepath.Dir(filepath.Join(m["state_dir"], "fakevms"))
	_ = os.MkdirAll(root, 0o700)
	if err := os.WriteFile(filepath.Join(root, "free-gib"), []byte("100"), 0o600); err != nil {
		t.Fatal(err)
	}
	h := acquireVol(t, m, volArgs("k1", "fay", vol("c", "c", "rw")))
	if err := os.WriteFile(filepath.Join(root, "measure-fail"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	st := store{m["state_dir"]}
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(100 * time.Millisecond) {
		if v, _ := st.loadVol(volID("t", "fay", "c")); v.MeasureFailed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("a failed measurement left no mark on the volume")
		}
	}
	if _, err := call(t, m, "acquire", "", volArgs("k2", "fay", vol("d", "d", "rw"))); code(err) != "unavailable" || !strings.Contains(err.Error(), "volume quota") {
		t.Errorf("a new volume beside one that could not be measured: %v, want unavailable volume quota (counted as the whole cap)", err)
	}
	if err := os.WriteFile(filepath.Join(root, "free-gib"), []byte("1"), 0o600); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(1500 * time.Millisecond); ; time.Sleep(100 * time.Millisecond) { // before a third failure
		s, err := call(t, m, "status", h, nil)
		if err == nil && s["state"] == "reclaimed" && strings.Contains(fmt.Sprint(s["why"]), "floor") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the host past its floor while the box's volumes cannot be measured: %v %v", s, err)
		}
	}
}

// zm-eakh.12.65.27: a volume whose measurement failed counts as the whole
// cap only until an acquire naming it can measure it (no live box writing
// it), so one failed measurement never holds its owner at the cap for good;
// while it still cannot be read, it stays at the cap.
func TestFailedMeasureIsMeasuredAgainByTheNextAcquire(t *testing.T) {
	m := fakeVolumeBox(t)
	root := filepath.Dir(filepath.Join(m["state_dir"], "fakevms"))
	_ = os.MkdirAll(root, 0o700)
	st := store{m["state_dir"]}
	h := acquireVol(t, m, volArgs("k1", "fay", vol("c", "c", "rw")))
	r, err := st.load(idOf(h))
	if err != nil {
		t.Fatal(err)
	}
	release(t, m, h)
	supervisorEnded(t, r.Supervisor) // its own measurement is done
	id := volID("t", "fay", "c")
	if err := st.locked(func() error {
		v, _ := st.loadVol(id)
		v.MeasureFailed = true
		return st.saveVol(v)
	}); err != nil {
		t.Fatal(err)
	}
	fail := filepath.Join(root, "measure-fail")
	if err := os.WriteFile(fail, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := call(t, m, "acquire", "", volArgs("k2", "fay", vol("c", "c", "rw"))); code(err) != "unavailable" || !strings.Contains(err.Error(), "volume quota") {
		t.Fatalf("a volume that still cannot be measured: %v, want unavailable volume quota", err)
	}
	if err := os.Remove(fail); err != nil {
		t.Fatal(err)
	}
	h = acquireVol(t, m, volArgs("k3", "fay", vol("c", "c", "rw")))
	release(t, m, h)
	if v, _ := st.loadVol(id); v.MeasureFailed || v.Bytes < 0 {
		t.Errorf("after an acquire measured it: %+v, want measured", v)
	}
}

// zm-eakh.12.65.27: the sweep deletes a layer volume of this state directory
// that no volume file names (left by a delete, or made again by a helper
// that mounted it by name), and a sealed volume with no measured size once
// no box holds it; never another's volume.
func TestSweepDeletesOrphanAndUnsizedLayerVolumes(t *testing.T) {
	b := newSealBox(t)
	h := acquireVol(t, b.m, volArgs("b", "o", build("lay")))
	if res, err := call(t, b.m, "release", h, map[string]any{"seal": true}); err != nil || res["released"] != true {
		t.Fatalf("release with seal: %v %v", res, err)
	}
	id := volID("t", "o", "lay")
	if err := b.st.locked(func() error {
		v, _ := b.st.loadVol(id)
		v.Bytes, v.MeasureFailed = -1, false // sealed before seals measured
		return b.st.saveVol(v)
	}); err != nil {
		t.Fatal(err)
	}
	orphan := b.dir("o", "gone")
	other := b.fake.vol("someone-else")
	for _, d := range []string{orphan, other} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	bad := filepath.Join(b.st.dir, "volumes", "vbad.json")
	if err := os.WriteFile(bad, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := call(t, b.m, "list", "", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(orphan); err != nil {
		t.Error("the sweep deleted a layer volume while a volume file it could not read might name it")
	}
	if err := os.Remove(bad); err != nil {
		t.Fatal(err)
	}
	if _, err := call(t, b.m, "list", "", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(orphan); err == nil {
		t.Error("the sweep left a layer volume no volume file names")
	}
	if _, err := os.Stat(other); err != nil {
		t.Error("the sweep deleted a volume that is not this state directory's layer volume")
	}
	if _, ok := b.st.loadVol(id); ok {
		t.Error("the sweep left a sealed volume with no measured size that no box holds")
	}
}
