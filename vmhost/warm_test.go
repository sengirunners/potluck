package vmhost

import (
	"context"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/potluck/conformance"
)

// waitWarm waits for describe's warm.ready to reach n.
func waitWarm(t *testing.T, m map[string]string, n int) map[string]any {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		d, err := call(t, m, "describe", "", nil)
		if err != nil {
			t.Fatal(err)
		}
		w, _ := d["warm"].(map[string]any)
		if w != nil && w["ready"] == float64(n) {
			return d
		}
		if time.Now().After(deadline) {
			t.Fatalf("warm boxes ready never reached %d: %v", n, d["warm"])
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// zm-eakh.12.5.28: warm=1 keeps one box booted; an acquire takes it at once,
// as a fresh box with its own lease and clock; the pool refills; a warm box
// has no handle on the wire.
func TestWarmBoxesOverAFakeLayer(t *testing.T) {
	m := fakeVolumeBox(t)
	m["warm"], m["linux_slots"] = "1", "2"
	if l, err := call(t, m, "list", "", nil); err != nil || len(l["handles"].([]any)) != 0 {
		t.Fatalf("list with only a warm box: %v %v", l, err)
	}
	d := waitWarm(t, m, 1)
	if w := d["warm"].(map[string]any); w["target"] != 1.0 {
		t.Fatalf("describe warm: %v", w)
	}
	if s := d["slots"].(map[string]any); s["free"] != 2.0 || d["available"] != true {
		t.Errorf("a warm box is free to take: slots %v available %v", s, d["available"])
	}
	if l, _ := call(t, m, "list", "", nil); len(l["handles"].([]any)) != 0 {
		t.Fatalf("a warm box is listed: %v", l)
	}
	var warmID string
	for _, r := range (store{m["state_dir"]}).all() {
		if r.Warm {
			warmID = r.ID
		}
	}
	if _, err := call(t, m, "status", "box:t/"+warmID, nil); code(err) != "not_found" {
		t.Errorf("status of a warm box: %v, want not_found", err)
	}
	time.Sleep(1200 * time.Millisecond) // warm time is not the taker's
	start := time.Now()
	a, err := call(t, m, "acquire", "", map[string]any{"request_id": "k1", "ttl_s": 60})
	if err != nil {
		t.Fatal(err)
	}
	took := time.Since(start)
	h := a["handle"].(string)
	if !strings.HasSuffix(h, "/"+warmID) {
		t.Fatalf("acquire made %s, not the warm box %s", h, warmID)
	}
	if took > 500*time.Millisecond {
		t.Errorf("taking a warm box took %s", took)
	}
	if ttl := a["ttl_s"].(float64); ttl < 59 || ttl > 61 {
		t.Errorf("a taken warm box's ttl_s %v, want 60 from the take", ttl)
	}
	if sp := a["spend"].(map[string]any)["amount"].(float64); sp > 0.01 {
		t.Errorf("a taken warm box's spend %v minutes counts its warm time", sp)
	}
	if code, out := sh(t, m, h, `echo hi > "$HOME/mark"; cat "$HOME/mark"`); code != 0 || out != "hi\n" {
		t.Fatalf("exec in a taken warm box: %v %q", code, out)
	}
	waitWarm(t, m, 1) // refilled in the other slot
	release(t, m, h)
	b, err := call(t, m, "acquire", "", map[string]any{"request_id": "k2", "ttl_s": 60})
	if err != nil {
		t.Fatal(err)
	}
	if _, out := sh(t, m, b["handle"].(string), `cat "$HOME/mark" 2>/dev/null || echo fresh`); out != "fresh\n" {
		t.Errorf("the next warm box holds the last box's file: %q", out)
	}
	// What a warm box cannot serve is a cold box: persist here.
	c, err := call(t, m, "acquire", "", map[string]any{"request_id": "k3", "ttl_s": 60, "persist": true})
	if err != nil {
		t.Fatalf("a cold acquire beside a taken box and a warm one: %v", err)
	}
	if strings.HasSuffix(c["handle"].(string), "/"+warmID) {
		t.Error("a persist acquire took a warm box")
	}
	release(t, m, b["handle"].(string))
	release(t, m, c["handle"].(string))
}

// A cold acquire with every slot held by warm boxes ends one and takes its
// slot: warm boxes are the first reclaimed.
func TestColdAcquirePreemptsAWarmBox(t *testing.T) {
	m := fakeVolumeBox(t)
	m["warm"], m["linux_slots"] = "1", "1"
	waitWarm(t, m, 1)
	c, err := call(t, m, "acquire", "", map[string]any{"request_id": "p", "ttl_s": 60, "persist": true})
	if err != nil {
		t.Fatalf("a cold acquire with the one slot warm: %v", err)
	}
	if n, _ := (&drv{cfg: config{Target: "t"}, st: store{m["state_dir"]}}).warmLive(); n != 0 {
		t.Errorf("%d warm boxes live beside a cold box in the one slot", n)
	}
	release(t, m, c["handle"].(string))
}

// A warm box holds no admission lease until it is taken (warm.go): it never
// keeps the host controller's room.
func TestWarmBoxLeasesAtTheTake(t *testing.T) {
	ctl, srv := newController(4)
	defer srv.Close()
	m := fakeVolumeBox(t)
	m["warm"], m["admission_url"], m["cpu"], m["memory"] = "1", srv.URL, "2", "2048"
	waitWarm(t, m, 1)
	if n := ctl.live(); n != 0 {
		t.Fatalf("a warm box holds %d admission leases before its take", n)
	}
	a, err := call(t, m, "acquire", "", map[string]any{"request_id": "k", "ttl_s": 60})
	if err != nil {
		t.Fatal(err)
	}
	if n := ctl.live(); n != 1 {
		t.Errorf("a taken warm box holds %d leases, want 1", n)
	}
	release(t, m, a["handle"].(string))
	// No room at the take: the acquire is unavailable, as a cold one would be.
	waitWarm(t, m, 1)
	for deadline := time.Now().Add(10 * time.Second); ctl.live() != 0; time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the released box's lease stayed")
		}
	}
	ctl.mu.Lock() // after the release's lease is back: its DELETE gives its room back
	ctl.room = 0
	ctl.mu.Unlock()
	if _, err := call(t, m, "acquire", "", map[string]any{"request_id": "k2", "ttl_s": 60}); code(err) != "unavailable" {
		t.Errorf("taking a warm box with no room: %v, want unavailable", err)
	}
	// A refused take backs the refill off: the box booted at that take is the last (review: churn).
	waitWarm(t, m, 1)
	if _, err := call(t, m, "acquire", "", map[string]any{"request_id": "k3", "ttl_s": 60}); code(err) != "unavailable" {
		t.Errorf("a second take with no room: %v", err)
	}
	for i := 0; i < 5; i++ {
		if _, err := call(t, m, "describe", "", nil); err != nil {
			t.Fatal(err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if n, _ := (&drv{cfg: config{Target: "t"}, st: store{m["state_dir"]}}).warmLive(); n != 0 {
		t.Errorf("%d warm boxes booted right after a refused take", n)
	}
}

// Lowering warm ends the warm boxes past it (on the next call).
func TestLoweringWarmEndsTheExtra(t *testing.T) {
	m := fakeVolumeBox(t)
	m["warm"], m["linux_slots"] = "2", "2"
	waitWarm(t, m, 2)
	m["warm"] = "0"
	if _, err := call(t, m, "list", "", nil); err != nil {
		t.Fatal(err)
	}
	if n, _ := (&drv{cfg: config{Target: "t"}, st: store{m["state_dir"]}}).warmLive(); n != 0 {
		t.Errorf("%d warm boxes live after warm went to 0", n)
	}
}

// The conformance warm group (and lifecycle, whose boxes are then warm ones)
// over the fake layer with warm=1.
func TestWarmConformanceOverAFakeLayer(t *testing.T) {
	if testing.Short() {
		t.Skip("runs conformance groups")
	}
	drv, repo, m := fakeBinding(t, "linux-img")
	delete(m, "image")
	m["kind"], m["size"], m["cpu"], m["memory"], m["warm"], m["linux_slots"] = "box", "small", "1", "512", "1", "3"
	m["images"] = "fake/linux@sha256:" + strings.Repeat("ab", 32)
	// The net group too: its boxes are warm ones (review: the network of a taken warm box).
	open, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer open.Close()
	go func() {
		for c, err := open.Accept(); err == nil; c, err = open.Accept() {
			c.Close()
		}
	}()
	forbid := guestForbid(t)
	opt := conformance.Options{Kind: "box", Target: "fake", Config: m, Ref: "main", Commit: repo.Commit, Repo: repo.Remote, LeftoverRoots: []string{fakeRoot},
		ShortTTL: 2, Grace: 3 * time.Second, RunWait: 30 * time.Second, Only: []string{"warm", "lifecycle", "net"},
		NetAllow: []string{open.Addr().String()}, NetForbid: []string{forbid}, NetPeerPort: guestPeerPort(t),
		NetResolve: []string{"localhost:" + strconv.Itoa(open.Addr().(*net.TCPAddr).Port)}}
	res := conformance.Run(context.Background(), drv, opt)
	out := conformance.Format(res)
	if n := conformance.Failed(res); n > 0 {
		t.Fatalf("%d failed:\n%s", n, out)
	}
	if !strings.Contains(out, "PASS  a warm box is a fresh box") || !strings.Contains(out, "PASS  a box reaches "+open.Addr().String()) {
		t.Fatalf("the warm checks did not run:\n%s", out)
	}
	t.Log("\n" + out)
}
