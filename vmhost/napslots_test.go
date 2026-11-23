package vmhost

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sengirunners/potluck/protocol"
)

// A layer that keeps nap slots (napper): a napping box holds no slot, the
// slots bound the boxes awake and slots and nap slots together every box,
// and a command for a napping box the host has no slot to wake in is
// refused, not answered as a command that ran. Without nap slots a napping
// box keeps its slot.
func TestANappingBoxHoldsNoSlot(t *testing.T) {
	_, _, m := fakeBinding(t, "linux-img")
	delete(m, "image")
	m["target"], m["kind"], m["size"], m["cpu"], m["memory"] = "t", "box", "small", "1", "512"
	m["images"] = "fake/linux@sha256:" + strings.Repeat("ab", 32)
	m["linux_slots"] = "1"
	free := func() int {
		t.Helper()
		d, err := call(t, m, "describe", "", nil)
		if err != nil {
			t.Fatal(err)
		}
		return int(d["slots"].(map[string]any)["free"].(float64))
	}
	box := func(key string) (h, dir string) {
		t.Helper()
		a, err := call(t, m, "acquire", "", map[string]any{"request_id": key, "ttl_s": 120})
		if err != nil {
			t.Fatal(err)
		}
		h = a["handle"].(string)
		waitState(t, m, h, "ready")
		return h, filepath.Join(m["state_dir"], "fakevms", boxOf(t, m, h))
	}
	mark := func(dir, name string, on bool) {
		t.Helper()
		p := filepath.Join(dir, name)
		if !on {
			_ = os.Remove(p)
		} else if err := os.WriteFile(p, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	full := func(key, words string) {
		t.Helper()
		_, err := call(t, m, "acquire", "", map[string]any{"request_id": key, "ttl_s": 120})
		if code(err) != "unavailable" || !strings.Contains(err.Error(), protocol.NoFreeSlot("linux")) || !strings.Contains(err.Error(), words) {
			t.Fatalf("acquire %s on a full host: %v, want unavailable, %q", key, err, words)
		}
	}

	ha, a := box("a")
	mark(a, "napping", true)
	if n := free(); n != 0 {
		t.Fatalf("no nap slots: %d free with the one slot's box napping, want 0", n)
	}
	full("x1", "1 in use")
	if err := os.WriteFile(filepath.Join(m["state_dir"], fakeNapSlots), []byte("1"), 0o600); err != nil {
		t.Fatal(err)
	}
	if n := free(); n != 1 {
		t.Fatalf("one nap slot: %d free with the one box napping, want 1", n)
	}
	_, b := box("b")
	if n := free(); n != 0 {
		t.Fatalf("%d free with one box awake of one at once, want 0", n)
	}
	full("x2", "1 awake of 1 at once, 2 boxes of 2 in all")
	mark(b, "napping", true)
	if n := free(); n != 0 {
		t.Fatalf("%d free with two boxes napping and one nap slot, want 0: slots and nap slots bound every box", n)
	}
	full("x3", "0 awake of 1 at once, 2 boxes of 2 in all")

	run := func(argv ...string) (map[string]any, error) {
		return call(t, m, "exec", ha, map[string]any{"argv": argv})
	}
	mark(a, "wake-refused", true)
	if _, err := run("true"); code(err) != "unavailable" || !strings.Contains(err.Error(), protocol.NoFreeSlot("linux")) || !strings.Contains(err.Error(), "nothing ran") {
		t.Errorf("a command for a napping box that cannot be woken: %v, want unavailable", err)
	}
	mark(a, "wake-refused", false)
	mark(a, "napping", false)
	if res, err := run("true"); err != nil || res["exit"] != float64(0) {
		t.Errorf("a command once the box woke: %v %v", res, err)
	}
	// The refusal's words from a command that ran are the command's own.
	if res, err := run("sh", "-c", `echo "`+cloudhvNapsOn+`" >&2; exit 255`); err != nil || res["exit"] != float64(255) {
		t.Errorf("a command that exits 255 saying a refusal's words in a box that is awake: %v %v, want its own exit", res, err)
	}
}
