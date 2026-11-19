package vmhost

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/protocol"
)

// The dials are written and read back by their owner; all zero removes the
// file; a file that cannot be trusted is an error, never "as shared".
func TestLendFileRoundTrip(t *testing.T) {
	p := filepath.Join(filepath.Dir(overrideAt(t)), "host-lend.json")
	if l, err := LendNow(os.Getuid()); err != nil || l != (HostLend{}) {
		t.Fatalf("no file: %+v %v", l, err)
	}
	l, err := SetLend(HostLend{Cores: 4, MemoryMB: 8192, KeepFreeGB: 50})
	if err != nil || l.Cores != 4 || l.MemoryMB != 8192 || l.KeepFreeGB != 50 || l.Since.IsZero() {
		t.Fatalf("set: %+v %v", l, err)
	}
	if got, err := LendNow(os.Getuid()); err != nil || got != l {
		t.Fatalf("read back: %+v %v", got, err)
	}
	if _, err := SetLend(HostLend{Cores: -1}); err == nil {
		t.Error("negative cores accepted")
	}
	if l, err := SetLend(HostLend{}); err != nil || l != (HostLend{}) {
		t.Fatalf("all as shared: %+v %v", l, err)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Errorf("all as shared left the file: %v", err)
	}
	for _, body := range []string{`{"cores":2}`, `{"cores":2,"since":"2026-11-19T00:00:00Z","x":1}`, `nope`, `{"cores":-2,"since":"2026-11-19T00:00:00Z"}`} {
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := LendNow(os.Getuid()); err == nil {
			t.Errorf("%s read as dials", body)
		}
	}
	if err := os.WriteFile(p, []byte(`{"cores":2,"since":"2026-11-19T00:00:00Z"}`), 0o666); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, 0o666); err != nil {
		t.Fatal(err)
	}
	if _, err := LendNow(os.Getuid()); err == nil {
		t.Error("a file others may write read as dials")
	}
}

// A cores or memory dial lower than the boxes running starts nothing new
// until they are under it, says so as a full slot (waiting lifts it), and
// leaves the running box alone; turned back up, a box starts again. An
// unreadable dial file refuses new work.
func TestLendDialsBoundNewBoxes(t *testing.T) {
	p := filepath.Join(filepath.Dir(overrideAt(t)), "host-lend.json")
	m := fakePersistBox(t)
	m["linux_slots"], m["cpu"], m["memory"] = "4", "2", "1024"
	a, err := call(t, m, "acquire", "", map[string]any{"request_id": "d1", "ttl_s": 120})
	if err != nil {
		t.Fatal(err)
	}
	h := a["handle"].(string)
	waitState(t, m, h, "ready")
	for _, dial := range []HostLend{{Cores: 3}, {MemoryMB: 1536}} {
		if _, err := SetLend(dial); err != nil {
			t.Fatal(err)
		}
		_, err := call(t, m, "acquire", "", map[string]any{"request_id": "d2", "ttl_s": 120})
		if code(err) != "unavailable" || !strings.Contains(err.Error(), protocol.NoFreeSlot("linux")+": the machine's owner lends") {
			t.Errorf("%+v: a second box past the dial: %v", dial, err)
		}
		d, err := call(t, m, "describe", "", nil)
		if err != nil {
			t.Fatal(err)
		}
		var desc computeapi.RunTargetDescribe
		remarshal(t, d, &desc)
		if desc.Available || !desc.FullNow() {
			t.Errorf("%+v: describe is not full for now: %+v", dial, desc)
		}
		if st, err := call(t, m, "status", h, nil); err != nil || st["state"] != "ready" {
			t.Errorf("%+v: the running box: %v %v", dial, st, err)
		}
	}
	if _, err := SetLend(HostLend{Cores: 4, MemoryMB: 2048}); err != nil {
		t.Fatal(err)
	}
	b, err := call(t, m, "acquire", "", map[string]any{"request_id": "d3", "ttl_s": 120})
	if err != nil {
		t.Fatalf("a second box inside the dial: %v", err)
	}
	if err := os.WriteFile(p, []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := call(t, m, "acquire", "", map[string]any{"request_id": "d4", "ttl_s": 120}); code(err) != "unavailable" || !strings.Contains(err.Error(), "lender's limits cannot be read") {
		t.Errorf("an unreadable dial file: %v", err)
	}
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	for _, x := range []string{h, b["handle"].(string)} {
		if _, err := call(t, m, "release", x, nil); err != nil {
			t.Error(err)
		}
	}
}

// keep_free_gb holds every new box, kept or not, and every start, while the
// disk has less free; persist_min_free_gb alone holds only kept disks.
func TestLendKeepFreeHoldsNewBoxes(t *testing.T) {
	overrideAt(t)
	m := fakePersistBox(t)
	a, err := call(t, m, "acquire", "", map[string]any{"request_id": "f1", "ttl_s": 120, "persist": true})
	if err != nil {
		t.Fatal(err)
	}
	h := a["handle"].(string)
	if _, err := call(t, m, "stop", h, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := SetLend(HostLend{KeepFreeGB: maxLendDiskGB}); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		verb, id string
		args     any
	}{{"acquire", "", map[string]any{"request_id": "f2", "ttl_s": 120}}, {"acquire", "", map[string]any{"request_id": "f3", "ttl_s": 120, "persist": true}}, {"start", h, nil}} {
		if _, err := call(t, m, c.verb, c.id, c.args); code(err) != "unavailable" || !strings.Contains(err.Error(), "keeps it free for their own use") {
			t.Errorf("%s %v below the lender's floor: %v", c.verb, c.args, err)
		}
	}
	if _, err := SetLend(HostLend{}); err != nil {
		t.Fatal(err)
	}
	if _, err := call(t, m, "start", h, nil); err != nil {
		t.Errorf("start with the floor off: %v", err)
	}
	_, _ = call(t, m, "release", h, nil)
}

// remarshal decodes a driver answer into v.
func remarshal(t *testing.T, from any, v any) {
	t.Helper()
	b, err := json.Marshal(from)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatal(err)
	}
}

// A dial below one box of the size refuses plainly (waiting never lifts
// it); one that fits starts boxes up to it.
func TestLendDialBelowASize(t *testing.T) {
	overrideAt(t)
	m := fakePersistBox(t)
	m["linux_slots"], m["cpu"] = "4", "4"
	if _, err := SetLend(HostLend{Cores: 2}); err != nil {
		t.Fatal(err)
	}
	_, err := call(t, m, "acquire", "", map[string]any{"request_id": "s1", "ttl_s": 120})
	if code(err) != "unavailable" || strings.Contains(err.Error(), protocol.NoFreeSlot("linux")) || !strings.Contains(err.Error(), "this size needs 4 cores, and the machine's owner lends only 2 cores now") {
		t.Errorf("a size larger than the dial: %v", err)
	}
	d, err := call(t, m, "describe", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	var desc computeapi.RunTargetDescribe
	remarshal(t, d, &desc)
	if desc.Available || desc.FullNow() {
		t.Errorf("describe waits for a dial that never fits: %+v", desc)
	}
	if _, err := SetLend(HostLend{Cores: 8}); err != nil {
		t.Fatal(err)
	}
	var hs []string
	for _, k := range []string{"s2", "s3"} {
		a, err := call(t, m, "acquire", "", map[string]any{"request_id": k, "ttl_s": 120})
		if err != nil {
			t.Fatalf("%s under the dial: %v", k, err)
		}
		hs = append(hs, a["handle"].(string))
	}
	if _, err := call(t, m, "acquire", "", map[string]any{"request_id": "s4", "ttl_s": 120}); code(err) != "unavailable" || !strings.Contains(err.Error(), "lends 8 cores now") {
		t.Errorf("a third past the dial: %v", err)
	}
	for _, h := range hs {
		_, _ = call(t, m, "release", h, nil)
	}
}
