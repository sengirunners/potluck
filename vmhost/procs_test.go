package vmhost

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func fakeBox(t *testing.T) (map[string]string, string) {
	t.Helper()
	_, _, m := fakeBinding(t, "linux-img")
	delete(m, "image")
	m["target"], m["kind"], m["size"], m["cpu"], m["memory"] = "t", "box", "small", "1", "512"
	m["images"] = "fake/linux@sha256:" + strings.Repeat("ab", 32)
	a, err := call(t, m, "acquire", "", map[string]any{"request_id": "k", "ttl_s": 120})
	if err != nil {
		t.Fatal(err)
	}
	return m, a["handle"].(string)
}

// spawn, output and signal over the fake layer: the holder runs the command
// through the layer's Exec and execWrap, output reads its files while it
// runs, signal reaches its group in the guest, and the box's end ends it.
func TestBoxBackgroundProcesses(t *testing.T) {
	m, h := fakeBox(t)
	d, _ := call(t, m, "describe", "", nil)
	if !strings.Contains(strings.Join(anyStrings(d["verbs"]), ","), "spawn,output,signal") || !strings.Contains(strings.Join(anyStrings(d["capabilities"]), ","), "base64") {
		t.Fatalf("describe: verbs %v, capabilities %v", d["verbs"], d["capabilities"])
	}
	pidf := filepath.Join(t.TempDir(), "child")
	sp, err := call(t, m, "spawn", h, map[string]any{"request_id": "s1",
		"argv": []string{"sh", "-c", `echo one; sleep 30 & echo $! > "$1"; echo two >&2; wait`, "sh", pidf}})
	if err != nil {
		t.Fatal(err)
	}
	pid := sp["pid"].(string)
	if again, err := call(t, m, "spawn", h, map[string]any{"request_id": "s1", "argv": []string{"true"}}); err != nil || again["pid"] != pid {
		t.Fatalf("the same request_id: %v %v, want %s", again, err, pid)
	}
	o, err := call(t, m, "output", h, map[string]any{"pid": pid, "wait_s": 5})
	for err == nil && (o["stdout"] != "one\n" || o["stderr"] != "two\n") && o["running"] == true {
		o, err = call(t, m, "output", h, map[string]any{"pid": pid, "wait_s": 5})
	}
	if err != nil || o["stdout"] != "one\n" || o["stderr"] != "two\n" || o["running"] != true || o["stdout_next"] != 4.0 {
		t.Fatalf("output while running: %v %v", o, err)
	}
	child := 0
	for range 50 {
		b, _ := os.ReadFile(pidf)
		if child, _ = strconv.Atoi(strings.TrimSpace(string(b))); child > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if s, err := call(t, m, "signal", h, map[string]any{"pid": pid, "signal": "TERM"}); err != nil || s["sent"] != true {
		t.Fatalf("signal: %v %v", s, err)
	}
	o, err = call(t, m, "output", h, map[string]any{"pid": pid, "stdout_from": 4, "stderr_from": 4, "wait_s": 10})
	if err != nil || o["running"] != false || o["exit"] != 143.0 {
		t.Fatalf("output after TERM: %v %v", o, err)
	}
	time.Sleep(200 * time.Millisecond)
	if child == 0 || syscall.Kill(child, 0) == nil {
		t.Errorf("the spawned command's child %d outlived a TERM to its group", child)
	}

	// base64 both ways, on exec and on a spawned process.
	raw := []byte{0, 0xff, 0xfe, '\n', 0x80}
	in := base64.StdEncoding.EncodeToString(raw)
	if res, err := call(t, m, "exec", h, map[string]any{"argv": []string{"cat"}, "stdin": in, "encoding": "base64"}); err != nil || res["stdout"] != in || res["encoding"] != "base64" {
		t.Errorf("exec base64: %v %v", res, err)
	}
	if _, err := call(t, m, "exec", h, map[string]any{"argv": []string{"cat"}, "stdin": "!!", "encoding": "base64"}); code(err) != "bad_request" {
		t.Errorf("exec with stdin that is not base64: %v", err)
	}

	// A process still running when the box is released ends with it.
	sp, err = call(t, m, "spawn", h, map[string]any{"request_id": "s2", "argv": []string{"sh", "-c", `sleep 30 & echo $! > "$1"; wait`, "sh", pidf + "2"}})
	if err != nil {
		t.Fatal(err)
	}
	for range 50 {
		if b, _ := os.ReadFile(pidf + "2"); len(b) > 0 {
			child, _ = strconv.Atoi(strings.TrimSpace(string(b)))
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if _, err := call(t, m, "release", h, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := call(t, m, "output", h, map[string]any{"pid": sp["pid"]}); code(err) != "conflict" {
		t.Errorf("output after release: %v, want conflict", err)
	}
	time.Sleep(300 * time.Millisecond)
	if child == 0 || syscall.Kill(child, 0) == nil {
		t.Errorf("a spawned process (%d) outlived its box", child)
	}
	if _, err := os.Stat(filepath.Join(m["state_dir"], "procs", strings.TrimPrefix(h, "box:t/"))); !os.IsNotExist(err) {
		t.Errorf("the box's process files outlived it: %v", err)
	}
}

func anyStrings(v any) []string {
	var out []string
	if xs, ok := v.([]any); ok {
		for _, x := range xs {
			out = append(out, x.(string))
		}
	}
	return out
}
