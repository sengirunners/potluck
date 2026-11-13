package vmhost

import (
	"context"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// envTestRegistry is the registry a filler the tests' driver spawns uses
// (TestMain): a filler with none refuses to start, so no test reaches the
// public registry.
const envTestRegistry = "VMHOST_TEST_SHELF_REGISTRY"

// shelfBinding is a vm=oci box binding on newShelfOCI's fake CLI, with
// shelf_fill as given and its fills going to r.
func shelfBinding(t *testing.T, fill string, r *fakeRegistry) (m map[string]string, vol string) {
	t.Helper()
	o, dir, vol := newShelfOCI(t)
	img := "zm/box@sha256:" + strings.Repeat("ab", 32)
	_ = os.MkdirAll(filepath.Join(dir, "images", "zm"), 0o700)
	if err := os.WriteFile(filepath.Join(dir, "images", img), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	useRegistry(t, r)
	t.Setenv(envTestRegistry, r.srv.URL)
	old := shelfFillPoll
	shelfFillPoll = 50 * time.Millisecond
	t.Cleanup(func() { shelfFillPoll = old })
	m = map[string]string{"target": "lx", "kind": "box", "vm": "oci", "oci_cli": o.bin, "oci_runtime": "runsc", "oci_network": "none",
		"state_dir": stateDir(t), "size": "c1m2", "cpu": "1", "memory": "512", "images": img, "boot_timeout_s": "20",
		"shelf_max_gb": "1", "shelf_fill": fill, "shelf_fill_mbit": "1000"}
	return m, vol
}

// boxWants writes the wants file a box's helper would, in the fake
// guest's HOME (fakeOCI's exec runs on the host).
func boxWants(t *testing.T, lines ...string) {
	t.Helper()
	p := filepath.Join(os.Getenv("HOME"), ".potluck", "wants")
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(filepath.Dir(p)) })
}

// A box's wants reach the shelf after it ends, through the filler the
// sweep starts; junk lines are dropped; no log names a package.
func TestShelfBoxWantsFill(t *testing.T) {
	r := newFakeRegistry(t)
	m, vol := shelfBinding(t, "boxes", r)
	a, b := []byte("tarball a"), []byte("tarball b")
	r.publish("left-pad", "1.3.0", sri512(a), "", a)
	r.publish("@scope/pkg", "2.0.0", sri512(b), "", b)
	boxWants(t, want(t, "left-pad", "1.3.0", a).line(), "npm evil 1.0.0 sha512-short", "npm ../x 1.0.0 "+sri512(a),
		"rm -rf /", want(t, "@scope/pkg", "2.0.0", b).line())
	res, err := call(t, m, "acquire", "", map[string]any{"request_id": "k", "actor": "a", "ttl_s": 60})
	if err != nil {
		t.Fatal(err)
	}
	h, _ := res["handle"].(string)
	if _, err := call(t, m, "release", h, map[string]any{}); err != nil {
		t.Fatal(err)
	}
	if _, err := call(t, m, "describe", "", nil); err != nil { // its sweep starts the filler
		t.Fatal(err)
	}
	waitFor(t, "two blobs on the shelf", func() bool { return len(blobsOn(t, vol)) == 2 })
	waitFor(t, "the filler to end", func() bool { return !flockHeld(shelfFillLock(m["state_dir"])) })
	for _, p := range r.hits {
		if strings.Contains(p, "evil") || strings.Contains(p, "..") {
			t.Errorf("the filler asked for a junk line: %s", p)
		}
	}
	logs, _ := filepath.Glob(filepath.Join(m["state_dir"], "logs", "*"))
	for _, l := range logs {
		if b, _ := os.ReadFile(l); strings.Contains(string(b), "left-pad") || strings.Contains(string(b), "scope") {
			t.Errorf("%s names a package:\n%s", l, b)
		}
	}
}

// With shelf_fill=seed nothing a box wants is collected, and a queue left
// from boxes mode is dropped.
func TestShelfSeedCollectsNothing(t *testing.T) {
	r := newFakeRegistry(t)
	m, vol := shelfBinding(t, "seed", r)
	a := []byte("tarball a")
	r.publish("left-pad", "1.3.0", sri512(a), "", a)
	boxWants(t, want(t, "left-pad", "1.3.0", a).line())
	left := filepath.Join(shelfQueueDir(m["state_dir"]), "r000000000001")
	if err := os.MkdirAll(filepath.Dir(left), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(left, []byte(want(t, "left-pad", "1.3.0", a).line()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := call(t, m, "acquire", "", map[string]any{"request_id": "k", "actor": "a", "ttl_s": 60})
	if err != nil {
		t.Fatal(err)
	}
	h, _ := res["handle"].(string)
	if _, err := call(t, m, "release", h, map[string]any{}); err != nil {
		t.Fatal(err)
	}
	if _, err := call(t, m, "describe", "", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(shelfQueueDir(m["state_dir"])); !os.IsNotExist(err) {
		t.Errorf("a queue under shelf_fill=seed: %v", err)
	}
	time.Sleep(200 * time.Millisecond)
	if len(r.hits) > 0 || len(blobsOn(t, vol)) > 0 {
		t.Errorf("seed fetched what a box wanted: %v", r.hits)
	}
}

func fakeSRI(i int) string {
	h := sha512.Sum512([]byte(fmt.Sprint(i)))
	return "sha512-" + base64.StdEncoding.EncodeToString(h[:])
}

// A box queues only valid lines, each once, at most shelfBoxLines of them
// across reads; a cut last line is dropped.
func TestQueueShelfWants(t *testing.T) {
	dir := t.TempDir()
	id := "r0123456789ab"
	good := "npm a 1.0.0 " + fakeSRI(0)
	n, d, err := queueShelfWants(dir, id, []byte(good+"\njunk\n"+good+"\nnpm a 1.0.0 sha512-"), true)
	if err != nil || n != 1 || d != 2 {
		t.Fatalf("queued %d dropped %d %v", n, d, err)
	}
	var b strings.Builder
	for i := 0; i < shelfBoxLines+5; i++ {
		fmt.Fprintf(&b, "npm a 1.0.%d %s\n", i, fakeSRI(i))
	}
	n, d, err = queueShelfWants(dir, id, []byte(b.String()), false)
	if err != nil || n != shelfBoxLines-1 || d != 5 { // line 0 is new (another version); one slot was taken
		t.Fatalf("queued %d dropped %d %v", n, d, err)
	}
	got, _ := os.ReadFile(filepath.Join(shelfQueueDir(dir), id))
	if lines := strings.Count(string(got), "\n"); lines != shelfBoxLines {
		t.Errorf("%d lines queued for one box", lines)
	}
	if _, _, err := queueShelfWants(dir, "../x", []byte(good+"\n"), false); err == nil {
		t.Error("a bad record id was queued")
	}
}

// The queue as a whole is bounded on the host's disk.
func TestShelfQueueMax(t *testing.T) {
	dir := t.TempDir()
	old := shelfQueueMax
	shelfQueueMax = 300
	t.Cleanup(func() { shelfQueueMax = old })
	var b strings.Builder
	for i := 0; i < 5; i++ {
		fmt.Fprintf(&b, "npm a 1.0.%d %s\n", i, fakeSRI(i))
	}
	n, d, _ := queueShelfWants(dir, "r0123456789ab", []byte(b.String()), false)
	n2, d2, _ := queueShelfWants(dir, "r0123456789ac", []byte(b.String()), false)
	if n+n2 != 2 || d+d2 != 0 || shelfQueueBytes(dir) > 300 {
		t.Errorf("queued %d+%d dropped %d+%d, %d bytes", n, n2, d, d2, shelfQueueBytes(dir))
	}
}

func fillCfgBoxes(state string, o ociCLI) config {
	c := shelfFillCfg(o, 1000)
	c.StateDir, c.Shelf.Fill, c.Shelf.FillDelayS = state, "boxes", 300
	return c
}

// A box's new fetches stop at shelfBoxFetchMax, across passes.
func TestShelfBoxFetchCap(t *testing.T) {
	o, _, vol := newShelfOCI(t)
	r := newFakeRegistry(t)
	useRegistry(t, r)
	old := shelfBoxFetchMax
	shelfBoxFetchMax = 12
	t.Cleanup(func() { shelfBoxFetchMax = old })
	a, b, c := []byte("twelve bytes"), []byte("more bytes!!"), []byte("third bytes!")
	for i, body := range [][]byte{a, b, c} {
		r.publish(fmt.Sprint("p", i), "1.0.0", sri512(body), "", body)
	}
	cfg := fillCfgBoxes(o.stateDir, o)
	id := "r0123456789ab" // no record: an ended box
	if _, _, err := queueShelfWants(o.stateDir, id, []byte(want(t, "p0", "1.0.0", a).line()+"\n"+want(t, "p1", "1.0.0", b).line()+"\n"), false); err != nil {
		t.Fatal(err)
	}
	shelfFillPass(context.Background(), cfg, o, io.Discard)
	if n := len(blobsOn(t, vol)); n != 1 {
		t.Fatalf("%d blobs, want 1 under the box's cap", n)
	}
	if _, _, err := queueShelfWants(o.stateDir, id, []byte(want(t, "p2", "1.0.0", c).line()+"\n"), false); err != nil {
		t.Fatal(err)
	}
	shelfFillPass(context.Background(), cfg, o, io.Discard)
	if n := len(blobsOn(t, vol)); n != 1 {
		t.Fatalf("%d blobs: the cap started over", n)
	}
	// Another box has its own allowance.
	if _, _, err := queueShelfWants(o.stateDir, "r0123456789ac", []byte(want(t, "p2", "1.0.0", c).line()+"\n"), false); err != nil {
		t.Fatal(err)
	}
	shelfFillPass(context.Background(), cfg, o, io.Discard)
	if n := len(blobsOn(t, vol)); n != 2 {
		t.Fatalf("%d blobs, want 2", n)
	}
}

// A live box's wants wait shelf_fill_delay_s; an ended box's do not.
func TestShelfFillDelay(t *testing.T) {
	o, _, vol := newShelfOCI(t)
	r := newFakeRegistry(t)
	useRegistry(t, r)
	a := []byte("tarball a")
	r.publish("a", "1.0.0", sri512(a), "", a)
	cfg := fillCfgBoxes(o.stateDir, o)
	st := store{o.stateDir}
	if err := os.MkdirAll(filepath.Join(o.stateDir, "runs"), 0o700); err != nil {
		t.Fatal(err)
	}
	id := "r0123456789ab"
	if err := st.save(record{ID: id, Kind: "box", State: stReady}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := queueShelfWants(o.stateDir, id, []byte(want(t, "a", "1.0.0", a).line()+"\n"), false); err != nil {
		t.Fatal(err)
	}
	if !shelfFillPass(context.Background(), cfg, o, io.Discard) || len(r.hits) > 0 {
		t.Fatalf("a live box's want did not wait: %v", r.hits)
	}
	cfg.Shelf.FillDelayS = 0 // the delay passed
	if shelfFillPass(context.Background(), cfg, o, io.Discard) || len(blobsOn(t, vol)) != 1 {
		t.Fatal("not filled after the delay")
	}
	if shelfPending(o.stateDir) {
		t.Error("read lines still pending")
	}
	if err := st.save(record{ID: id, Kind: "box", State: stReleased, End: time.Now().Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	shelfFillPass(context.Background(), cfg, o, io.Discard)
	if _, err := os.Stat(filepath.Join(shelfQueueDir(o.stateDir), id)); !os.IsNotExist(err) {
		t.Errorf("a long-ended, drained box's queue file stays: %v", err)
	}
}

// One filler per state directory: a second finds the lock held, exits at
// once and reads nothing; the sweep starts none while one runs.
func TestShelfFillerSingle(t *testing.T) {
	o, _, _ := newShelfOCI(t)
	r := newFakeRegistry(t)
	useRegistry(t, r)
	a := []byte("tarball a")
	r.publish("a", "1.0.0", sri512(a), "", a)
	if _, _, err := queueShelfWants(o.stateDir, "r0123456789ab", []byte(want(t, "a", "1.0.0", a).line()+"\n"), false); err != nil {
		t.Fatal(err)
	}
	l, err := os.OpenFile(shelfFillLock(o.stateDir), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if err := syscall.Flock(int(l.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	cfg := fillCfgBoxes(o.stateDir, o)
	m := rawConfigOf(t, cfg, o)
	done := make(chan int, 1)
	go func() { done <- shelfFiller(o.stateDir, strings.NewReader(m), io.Discard) }()
	select {
	case code := <-done:
		if code != 0 || len(r.hits) > 0 || !shelfPending(o.stateDir) {
			t.Fatalf("second filler: exit %d, requests %v", code, r.hits)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a second filler waited for the lock")
	}
	if !flockHeld(shelfFillLock(o.stateDir)) { // what the sweep asks before it starts one
		t.Error("a running filler's lock reads as free")
	}
	_ = syscall.Flock(int(l.Fd()), syscall.LOCK_UN)
	if code := shelfFiller(o.stateDir, strings.NewReader(m), io.Discard); code != 0 || shelfPending(o.stateDir) {
		t.Fatalf("the filler, lock free: exit %d", code)
	}
}

// rawConfigOf is cfg as the filler reads it on stdin.
func rawConfigOf(t *testing.T, cfg config, o ociCLI) string {
	t.Helper()
	m := map[string]string{"target": "lx", "kind": "box", "vm": "oci", "oci_cli": o.bin, "oci_runtime": "runsc", "oci_network": "none",
		"state_dir": cfg.StateDir, "images": "zm/box@sha256:" + strings.Repeat("ab", 32), "size": "c1m2", "cpu": "1", "memory": "512", "shelf_max_gb": "1", "shelf_fill": cfg.Shelf.Fill, "shelf_fill_mbit": "1000",
		"shelf_fill_delay_s": fmt.Sprint(cfg.Shelf.FillDelayS)}
	if _, err := parseConfig(m); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(m)
	return string(b)
}

// shelfExecLayer is a layer with a shelf whose Exec runs on the host with
// the shelf at shelf and HOME at home: enough for the link step and the
// wants read.
type shelfExecLayer struct {
	vmLayer
	shelfer
	shelf, home string
}

func (l shelfExecLayer) Exec(_ string, stdin io.Reader, stdout, stderr io.Writer, argv ...string) *exec.Cmd {
	for i, s := range argv {
		argv[i] = strings.ReplaceAll(s, ociCacheDir, l.shelf)
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
	cmd.Env = append(os.Environ(), "HOME="+l.home)
	return cmd
}

// After a checkout the helper links in the checkout directory; its wants
// are read from the box's HOME and queued; a failing helper is logged and
// ignored; a box that streams more than the read cap is cut there.
func TestShelfLinkAndCollect(t *testing.T) {
	shelf, home, state := t.TempDir(), t.TempDir(), t.TempDir()
	if err := os.MkdirAll(filepath.Join(shelf, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	line := "npm a 1.0.0 " + fakeSRI(1)
	helper := "#!/bin/sh\necho \"$@ go=$POTLUCK_DEPS_GO\" > \"$HOME/args\"; mkdir -p \"$HOME/.potluck\"; echo '" + line + "' >> \"$HOME/.potluck/wants\"; echo junk >> \"$HOME/.potluck/wants\"\n"
	if err := os.WriteFile(filepath.Join(shelf, shelfHelperPath), []byte(helper), 0o755); err != nil {
		t.Fatal(err)
	}
	l := shelfExecLayer{shelf: shelf, home: home}
	var log strings.Builder
	shelfLink(l, "vm", "/tmp/zm-run/src", &log)
	if b, _ := os.ReadFile(filepath.Join(home, "args")); strings.TrimSpace(string(b)) != "link /tmp/zm-run/src go=off" { // names no Go module: the job may set GOPRIVATE later
		t.Fatalf("helper args %q, log %s", b, log.String())
	}
	cfg := config{StateDir: state, Shelf: shelfCfg{MaxGB: 1, Fill: "boxes"}}
	r := record{ID: "r0123456789ab", VM: "vm", Kind: "box"}
	collectShelfWants(cfg, l, r, &log, shelfWantsReadFor)
	got, _ := os.ReadFile(filepath.Join(shelfQueueDir(state), r.ID))
	if string(got) != line+"\n" {
		t.Fatalf("queued %q; log %s", got, log.String())
	}
	if !strings.Contains(log.String(), "1 lines invalid") || strings.Contains(log.String(), "npm a") {
		t.Errorf("log: %s", log.String())
	}

	// A deps layer's checkout (srcDir relative to HOME) is linked there.
	shelfLink(l, "vm", srcDir(record{Volumes: []volUse{{Path: "work/app", Mode: volCOW, Checkout: true}}}), &log)
	if b, _ := os.ReadFile(filepath.Join(home, "args")); strings.TrimSpace(string(b)) != "link "+home+"/work/app go=off" {
		t.Fatalf("helper args for a layer checkout %q, want link %s/work/app; log %s", b, home, log.String())
	}

	// A failing helper is logged; the step goes on.
	if err := os.WriteFile(filepath.Join(shelf, shelfHelperPath), []byte("#!/bin/sh\necho broken >&2; exit 3\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	log.Reset()
	shelfLink(l, "vm", "/x", &log)
	if !strings.Contains(log.String(), "linking failed (ignored)") || !strings.Contains(log.String(), "broken") {
		t.Errorf("log: %s", log.String())
	}

	// A wants "file" that never ends is cut at the cap and the time limit.
	if err := os.WriteFile(filepath.Join(home, ".potluck", "wants"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(filepath.Join(home, ".potluck", "wants"))
	if err := os.Symlink("/dev/zero", filepath.Join(home, ".potluck", "wants")); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	collectShelfWants(cfg, l, record{ID: "r0123456789ac", VM: "vm"}, io.Discard, shelfWantsReadFor)
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("an endless wants file held the host %s", d)
	}
	if _, err := os.Stat(filepath.Join(shelfQueueDir(state), "r0123456789ac")); !os.IsNotExist(err) {
		t.Errorf("an endless file queued something: %v", err)
	}

	// shelf_fill=seed reads nothing.
	cfg.Shelf.Fill = "seed"
	collectShelfWants(cfg, l, record{ID: "r0123456789ad", VM: "vm"}, io.Discard, shelfWantsReadFor)
	if _, err := os.Stat(filepath.Join(shelfQueueDir(state), "r0123456789ad")); !os.IsNotExist(err) {
		t.Errorf("seed queued: %v", err)
	}
}

// Boxes take turns a slice at a time, and the offset moves before the
// fetch: a box with many wants does not hold another's back, and a line is
// tried at most once. A stopped persistent box's drained file stays.
func TestShelfFillSlices(t *testing.T) {
	o, _, vol := newShelfOCI(t)
	r := newFakeRegistry(t)
	useRegistry(t, r)
	a := []byte("tarball a")
	r.publish("a", "1.0.0", sri512(a), "", a)
	cfg := fillCfgBoxes(o.stateDir, o)
	var b strings.Builder
	for i := 0; i < shelfFillSlice+50; i++ {
		fmt.Fprintf(&b, "npm missing 1.0.%d %s\n", i, fakeSRI(i))
	}
	big, small := "r0123456789ab", "r0123456789ac"
	if _, _, err := queueShelfWants(o.stateDir, big, []byte(b.String()), false); err != nil {
		t.Fatal(err)
	}
	if _, _, err := queueShelfWants(o.stateDir, small, []byte(want(t, "a", "1.0.0", a).line()+"\n"), false); err != nil {
		t.Fatal(err)
	}
	shelfFillPass(context.Background(), cfg, o, io.Discard)
	if len(blobsOn(t, vol)) != 1 {
		t.Fatal("the small box waited behind the big one")
	}
	off, _ := readShelfDone(filepath.Join(shelfQueueDir(o.stateDir), big+".done"))
	if want := int64(len(strings.Join(strings.SplitAfter(b.String(), "\n")[:shelfFillSlice], ""))); off != want {
		t.Fatalf("offset %d after one pass, want one slice (%d)", off, want)
	}
	shelfFillPass(context.Background(), cfg, o, io.Discard)
	if shelfPending(o.stateDir) {
		t.Fatal("lines pending after two passes")
	}

	st := store{o.stateDir}
	if err := os.MkdirAll(filepath.Join(o.stateDir, "runs"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := st.save(record{ID: big, Kind: "box", State: stStopped, Persist: true, End: time.Now().Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	shelfFillPass(context.Background(), cfg, o, io.Discard)
	if _, err := os.Stat(filepath.Join(shelfQueueDir(o.stateDir), big)); err != nil {
		t.Errorf("a stopped box's queue file went: its caps start over at the next start: %v", err)
	}
	if _, err := os.Stat(filepath.Join(shelfQueueDir(o.stateDir), small)); !os.IsNotExist(err) {
		t.Errorf("an ended box's drained file stays: %v", err)
	}
}

// A wants path that is a FIFO is not read: nothing waits on it.
func TestShelfWantsFIFO(t *testing.T) {
	home, state := t.TempDir(), t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".potluck"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(home, ".potluck", "wants"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := config{StateDir: state, Shelf: shelfCfg{MaxGB: 1, Fill: "boxes"}}
	start := time.Now()
	collectShelfWants(cfg, shelfExecLayer{shelf: t.TempDir(), home: home}, record{ID: "r0123456789ab", VM: "vm"}, io.Discard, shelfWantsReadFor)
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("a FIFO held the read %s", d)
	}
}

// Read lines do not hold the queue's room: a drained box (a stopped
// persistent one, say) leaves the whole budget to other boxes.
func TestShelfQueueRoomIsUnread(t *testing.T) {
	o, _, _ := newShelfOCI(t)
	r := newFakeRegistry(t)
	useRegistry(t, r)
	old := shelfQueueMax
	shelfQueueMax = 300
	t.Cleanup(func() { shelfQueueMax = old })
	line := func(i int) string { return fmt.Sprintf("npm missing 1.0.%d %s\n", i, fakeSRI(i)) }
	if n, _, _ := queueShelfWants(o.stateDir, "r0123456789ab", []byte(line(0)+line(1)), false); n != 2 {
		t.Fatalf("queued %d", n)
	}
	if n, _, _ := queueShelfWants(o.stateDir, "r0123456789ac", []byte(line(2)), false); n != 0 {
		t.Fatalf("queued %d past the queue's room", n)
	}
	shelfFillPass(context.Background(), fillCfgBoxes(o.stateDir, o), o, io.Discard)
	if n, _, _ := queueShelfWants(o.stateDir, "r0123456789ac", []byte(line(2)+line(3)), false); n != 2 {
		t.Fatalf("queued %d after the first box was read", n)
	}
}
