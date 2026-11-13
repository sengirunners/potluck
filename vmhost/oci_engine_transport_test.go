package vmhost

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestKnownRunscAcquirePinsHandleEndpoint(t *testing.T) {
	o, dir := newFakeOCI(t, "none")
	endpoint := os.Getenv("DOCKER_HOST")
	script := strings.Replace(fakeOCI, "d=$FAKE_OCI; echo", `d=$FAKE_OCI; if [ "$DOCKER_HOST" != "$FAKE_EXPECT_ENGINE" ];then echo wrong-engine >> "$d/calls"; exit 9;fi; echo`, 1)
	if err := os.WriteFile(o.bin, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_EXPECT_ENGINE", endpoint)
	image := "img@sha256:" + strings.Repeat("ab", 32)
	_ = os.WriteFile(filepath.Join(dir, "images", image), nil, 0600)
	_, srv := newController(4)
	defer srv.Close()
	m := map[string]string{"target": "pinned", "kind": "box", "vm": "oci", "oci_cli": o.bin, "oci_runtime": "runsc", "state_dir": stateDir(t), "size": "c1m2", "cpu": "1", "memory": "2048", "images": image, "admission_url": srv.URL, "boot_timeout_s": "10", "admission_ttl_s": "120"}
	a, err := call(t, m, "acquire", "", map[string]any{"request_id": "valid-local", "ttl_s": 60})
	if err != nil {
		t.Fatal(err)
	}
	handle := a["handle"].(string)
	defer call(t, m, "release", handle, nil)
	records := (store{m["state_dir"]}).all()
	if len(records) != 1 || records[0].Engine == nil || records[0].Engine.Endpoint != endpoint {
		t.Fatal("owned allocation did not store the verified engine")
	}
	// The next invocation receives another endpoint; handles must not rediscover
	// that selection or send their transport/cleanup to it.
	t.Setenv("DOCKER_HOST", "tcp://remote.invalid:2375")
	if err := os.WriteFile(filepath.Join(dir, "runtimes"), []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	result, err := call(t, m, "exec", handle, map[string]any{"argv": []string{"sh", "-c", "printf pinned"}})
	if err != nil || result["stdout"] != "pinned" || result["exit"] != float64(0) {
		t.Fatalf("pinned existing handle: %v %v", result, err)
	}
	sp, err := call(t, m, "spawn", handle, map[string]any{"request_id": "pinned-pty", "pty": map[string]any{"rows": 24, "cols": 80, "term": "vt100"}, "argv": []string{"sh", "-c", "test -t 0 && test -t 1 || exit 9; printf 'pinned-pty\\n'"}})
	if err != nil {
		t.Fatal(err)
	}
	var terminal strings.Builder
	next := 0.0
	for until := time.Now().Add(5 * time.Second); ; {
		out, err := call(t, m, "output", handle, map[string]any{"pid": sp["pid"], "stdout_from": next, "wait_s": 1})
		if err != nil {
			t.Fatal(err)
		}
		terminal.WriteString(out["stdout"].(string))
		next = out["stdout_next"].(float64)
		if out["running"] == false {
			if out["exit"] != float64(0) || !strings.Contains(terminal.String(), "pinned-pty") {
				t.Fatalf("pinned PTY: %v %q", out, terminal.String())
			}
			break
		}
		if time.Now().After(until) {
			t.Fatal("pinned PTY did not finish")
		}
	}
	if _, err := call(t, m, "release", handle, nil); err != nil {
		t.Fatal(err)
	}
	calls, _ := os.ReadFile(filepath.Join(dir, "calls"))
	if strings.Contains(string(calls), "wrong-engine") {
		t.Fatalf("existing handle followed context drift: %s", calls)
	}
}
