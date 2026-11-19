package vmhost

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeInspect writes the state container inspect answers for box vm.
func fakeInspect(t *testing.T, dir, vm string, running, oom bool, exit int, memory int64) {
	t.Helper()
	j := fmt.Sprintf(`[{"State":{"Running":%v,"OOMKilled":%v,"ExitCode":%d,"Pid":0},"HostConfig":{"Memory":%d}}]`, running, oom, exit, memory)
	if err := os.WriteFile(filepath.Join(dir, "c", vm, "inspect"), []byte(j+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// Why a box ended is read from the container's state, and for a box under an
// owned host parent from that cgroup's memory.events, and said in one set of
// words.
func TestOCIEndOf(t *testing.T) {
	o, dir := newFakeOCI(t, "none")
	if err := os.MkdirAll(filepath.Join(dir, "c", "b"), 0o700); err != nil {
		t.Fatal(err)
	}
	const mb = 1 << 20
	for _, c := range []struct {
		name    string
		running bool
		oom     bool
		exit    int
		memory  int64
		ended   bool
		want    string
	}{
		{"still running", true, false, 0, 2048 * mb, false, ""},
		{"killed for memory", false, true, 137, 2048 * mb, true, "the box was killed: out of memory (limit 2048 MB)"},
		{"SIGKILL under a limit, not recorded as memory", false, false, 137, 512 * mb, true,
			"the box was killed (SIGKILL, exit 137): most likely out of memory (limit 512 MB), though its runtime did not record it as that"},
		{"killed by TERM", false, false, 143, 512 * mb, true, "the box was killed by signal 15 (exit 143)"},
		{"a plain stop (or never started) names no cause", false, false, 0, 0, false, ""},
		{"a runtime's own exit names no cause", false, false, 127, 512 * mb, false, ""},
	} {
		fakeInspect(t, dir, "b", c.running, c.oom, c.exit, c.memory)
		why, ended := boxEnd(o, "b")
		if ended != c.ended || why != c.want {
			t.Errorf("%s: %q %v, want %q %v", c.name, why, ended, c.want, c.ended)
		}
	}
	if why, ended := boxEnd(o, "gone"); ended {
		t.Errorf("a container inspect cannot find: %q", why)
	}
	if why, ended := boxEnd(tartCLI{}, "b"); ended {
		t.Errorf("a layer that reads no end: %q", why)
	}

	// Under an owned host parent the cgroup's oom_kill counts, and its
	// memory.max is the limit when the container names none.
	cg := t.TempDir()
	if err := os.WriteFile(filepath.Join(cg, "memory.events"), []byte("low 0\nhigh 0\nmax 4\noom 1\noom_kill 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cg, "memory.max"), []byte(fmt.Sprint(1024*mb)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	o.host = &ociHostLease{parent: &ownedHostParent{name: "p.slice", path: cg}}
	fakeInspect(t, dir, "b", false, false, 128, 0)
	if why, ended := boxEnd(o, "b"); !ended || why != "the box was killed: out of memory (limit 1024 MB)" {
		t.Errorf("parent's oom_kill: %q %v", why, ended)
	}
}

// ociBox is a box binding over fakeOCI, its box acquired and ready.
func ociBox(t *testing.T) (m map[string]string, dir, h, vm string) {
	t.Helper()
	o, dir := newFakeOCI(t, "none")
	img := "zm/box@sha256:" + strings.Repeat("ab", 32)
	_ = os.MkdirAll(filepath.Join(dir, "images", "zm"), 0o700)
	if err := os.WriteFile(filepath.Join(dir, "images", img), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	m = map[string]string{"target": "lx", "kind": "box", "vm": "oci", "oci_cli": o.bin, "oci_runtime": "runsc", "state_dir": stateDir(t),
		"size": "c1m2", "cpu": "1", "memory": "512", "images": img, "boot_timeout_s": "20", "linux_slots": "2"}
	h, vm = acquireBox(t, m, "k")
	waitState(t, m, h, "ready")
	return m, dir, h, vm
}

// A box killed for memory says so: the exec that was running answers it,
// status's why records it, and a later exec's refusal repeats it.
func TestOCIBoxKilledForMemorySaysSo(t *testing.T) {
	m, dir, h, vm := ociBox(t)
	const oom = "the box was killed: out of memory (limit 512 MB)"
	fakeInspect(t, dir, vm, false, true, 137, 512<<20)
	// The CLI's exec fails as the container goes (gVisor: a urpc EOF, exit 128).
	_, err := call(t, m, "exec", h, map[string]any{"argv": []string{"sh", "-c", "echo 'urpc method failed: EOF' >&2; exit 128"}})
	if code(err) != "conflict" || !strings.Contains(err.Error(), "ended while the command ran: "+oom) {
		t.Fatalf("exec as the box died: %v", err)
	}
	_ = os.Remove(filepath.Join(dir, "c", vm, "up")) // the container ends: Boot's process exits
	st := waitState(t, m, h, "lost")
	if st["why"] != oom {
		t.Errorf("status why = %q, want %q", st["why"], oom)
	}
	if _, err := call(t, m, "exec", h, map[string]any{"argv": []string{"true"}}); code(err) != "conflict" || !strings.Contains(err.Error(), oom) {
		t.Errorf("exec on the lost box: %v", err)
	}
}

// A box killed for another reason says that reason, never memory.
func TestOCIBoxKilledOtherwiseSaysWhy(t *testing.T) {
	m, dir, h, vm := ociBox(t)
	fakeInspect(t, dir, vm, false, false, 143, 0)
	_ = os.Remove(filepath.Join(dir, "c", vm, "up"))
	st := waitState(t, m, h, "lost")
	if why, _ := st["why"].(string); why != "the box was killed by signal 15 (exit 143)" {
		t.Errorf("status why = %q", why)
	}
}

// A command's own exit of 125 and up in a box that still runs is its own:
// no refusal.
func TestOCIExecHighExitInALiveBox(t *testing.T) {
	m, dir, h, vm := ociBox(t)
	fakeInspect(t, dir, vm, true, false, 0, 512<<20)
	x, err := call(t, m, "exec", h, map[string]any{"argv": []string{"sh", "-c", "exit 137"}})
	if err != nil || x["exit"] != float64(137) {
		t.Fatalf("exec = %v %v", x, err)
	}
}

// A start that fails after boot because its box died leads with why, and
// its kept report records it.
func TestFailStartedSaysTheBoxDied(t *testing.T) {
	o, dir := newFakeOCI(t, "none")
	if err := os.MkdirAll(filepath.Join(dir, "c", "v"), 0o700); err != nil {
		t.Fatal(err)
	}
	fakeInspect(t, dir, "v", false, true, 137, 2048<<20)
	st := store{t.TempDir()} // no supervisor starts here
	if err := os.MkdirAll(filepath.Join(st.dir, "runs"), 0o700); err != nil {
		t.Fatal(err)
	}
	r := record{ID: "r0123456789ab", Handle: "box:lx/r0123456789ab", VM: "v", State: stStarting}
	if err := st.save(r); err != nil {
		t.Fatal(err)
	}
	s := &sup{st: st, vm: o, name: "v", id: r.ID}
	s.failStarted(r, errors.New("a guest command exited 128"))
	got, err := st.load(r.ID)
	if err != nil {
		t.Fatal(err)
	}
	const want = "the box was killed: out of memory (limit 2048 MB): a guest command exited 128"
	if got.State != stFailed || !strings.HasPrefix(got.Why, want+"; report: ") {
		t.Fatalf("record: %s %q", got.State, got.Why)
	}
	_, path := StartAdvice(got.Why) // the report's path, which its machine's name follows
	rep, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(rep), want) {
		t.Errorf("report: %v %q", err, rep)
	}

	// A container that never ran (created, then the start failed) adds
	// nothing to the start's own words.
	fakeInspect(t, dir, "v", false, false, 0, 2048<<20)
	r.ID, r.Handle = "r0123456789ac", "box:lx/r0123456789ac"
	if err := st.save(r); err != nil {
		t.Fatal(err)
	}
	s.id = r.ID
	s.failStarted(r, errors.New("the runtime refused the box"))
	if got, _ := st.load(r.ID); !strings.HasPrefix(got.Why, "the runtime refused the box; report: ") {
		t.Errorf("never-started record: %q", got.Why)
	}
}

// An exec cut by a release is the release's, never a memory kill: the box
// ended on purpose.
func TestOCIExecInReleasedBoxIsNotOOM(t *testing.T) {
	m, dir, h, vm := ociBox(t)
	fakeInspect(t, dir, vm, false, false, 137, 512<<20) // what a SIGKILLed container shows
	st := store{m["state_dir"]}
	id := h[strings.LastIndex(h, "/")+1:]
	go func() { // released while the command runs, as release records it before it ends the container
		time.Sleep(300 * time.Millisecond)
		_, _ = st.update(id, func(r *record) error { r.State, r.End = stReleased, time.Now().UTC(); return nil })
	}()
	_, err := call(t, m, "exec", h, map[string]any{"argv": []string{"sh", "-c", "sleep 1; exit 137"}})
	if code(err) != "conflict" || strings.Contains(err.Error(), "memory") || !strings.Contains(err.Error(), "released") {
		t.Errorf("exec cut by a release: %v", err)
	}
}
