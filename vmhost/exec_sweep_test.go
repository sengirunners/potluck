package vmhost

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// readyFakeBox acquires a box on a fake layer and answers its handle and the
// binding's config.
func readyFakeBox(t *testing.T) (string, map[string]string) {
	t.Helper()
	_, _, m := fakeBinding(t, "linux-img")
	delete(m, "image")
	m["target"], m["kind"], m["size"], m["cpu"], m["memory"] = "t", "box", "small", "1", "512"
	m["images"] = "fake/linux@sha256:" + strings.Repeat("ab", 32)
	a, err := call(t, m, "acquire", "", map[string]any{"request_id": "k", "ttl_s": 120})
	if err != nil {
		t.Fatal(err)
	}
	h := a["handle"].(string)
	t.Cleanup(func() { _, _ = call(t, m, "release", h, nil) })
	return h, m
}

// An exec on a box whose supervisor runs does not sweep: a file copy sends
// hundreds of them, and a sweep each (the state lock, the VM layer's list)
// made them wait in line and cost about a second apiece. The same box's
// status still sweeps, which is how this test knows the stale record was
// there to be swept.
func TestExecOfAReadyBoxDoesNotSweep(t *testing.T) {
	h, m := readyFakeBox(t)
	old := time.Now().Add(-2 * keepFor)
	stale := put(t, m, record{ID: "r0000000stale", State: stReleased, Start: old, End: old, Deadline: old})
	staleFile := filepath.Join(m["state_dir"], "runs", stale.ID+".json")

	res, err := call(t, m, "exec", h, map[string]any{"argv": []string{"echo", "hi"}})
	if err != nil || res["exit"] != 0.0 || res["stdout"] != "hi\n" {
		t.Fatalf("exec: %v %v, want exit 0, stdout hi", res, err)
	}
	if _, err := os.Stat(staleFile); err != nil {
		t.Fatalf("exec swept the state directory (an ended record past keepFor is gone): %v", err)
	}
	if _, err := call(t, m, "status", h, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(staleFile); !os.IsNotExist(err) {
		t.Fatalf("status did not sweep the stale record: %v", err)
	}
}

// A box whose supervisor is gone is still found lost by the exec that meets
// it: skipping the sweep is for a box that is running.
func TestExecOfABoxWithoutSupervisorSweeps(t *testing.T) {
	h, m := readyFakeBox(t)
	dead := exec.Command("true")
	if err := dead.Run(); err != nil {
		t.Fatal(err)
	}
	st := store{m["state_dir"]}
	for _, r := range st.all() {
		r.Supervisor = dead.Process.Pid
		if err := st.locked(func() error { return st.save(r) }); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := call(t, m, "exec", h, map[string]any{"argv": []string{"echo", "hi"}}); code(err) != "conflict" {
		t.Fatalf("exec in a box whose supervisor died: %v, want conflict", err)
	}
}
