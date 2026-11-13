package vmhost

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// boxBindingBooting is a box binding over the fake layer whose boots take
// bootMs, so a test can act while an acquire is in flight.
func boxBindingBooting(t *testing.T, bootMs string) (string, map[string]string) {
	t.Helper()
	drv, _, m := fakeBinding(t, "linux-img")
	delete(m, "image")
	m["target"], m["kind"], m["size"], m["cpu"], m["memory"] = "t", "box", "small", "1", "512"
	m["images"] = "fake/linux@sha256:" + strings.Repeat("ab", 32)
	if err := os.WriteFile(filepath.Join(m["state_dir"], fakeBootMs), []byte(bootMs), 0o600); err != nil {
		t.Fatal(err)
	}
	return drv, m
}

// acquireProcess starts an acquire in a driver process of its own, which the
// test may stop or kill; its answer is not read.
func acquireProcess(t *testing.T, drv string, m map[string]string, args map[string]any) *exec.Cmd {
	t.Helper()
	req, _ := json.Marshal(map[string]any{"verb": "acquire", "args": args, "config": m})
	cmd := exec.Command(drv)
	cmd.Stdin = bytes.NewReader(req)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	return cmd
}

// stopUnlocked stops cmd at an instant it holds neither the state lock nor
// the progress lock, so the box's supervisor still goes on.
func stopUnlocked(t *testing.T, cmd *exec.Cmd, state string) {
	t.Helper()
	free := func(name string) bool {
		f, err := os.OpenFile(filepath.Join(state, name), os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		if syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
			return false
		}
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		return true
	}
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(5 * time.Millisecond) {
		if err := cmd.Process.Signal(syscall.SIGSTOP); err != nil {
			t.Fatal(err)
		}
		if procStopped(cmd.Process.Pid) && free("lock") && free("progress.lock") {
			return
		}
		_ = cmd.Process.Signal(syscall.SIGCONT)
	}
	t.Fatal("the acquire never stopped outside the locks")
}

// procStopped says whether pid is stopped (ps state T), so it can take no lock.
func procStopped(pid int) bool {
	out, err := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
	return err == nil && strings.HasPrefix(strings.TrimSpace(string(out)), "T")
}

// kill ends the acquire's process and reaps it: a zombie still answers kill 0.
func kill(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	_ = cmd.Process.Signal(syscall.SIGKILL)
	_ = cmd.Wait()
}

func waitPhase(t *testing.T, m map[string]string, key, want string) {
	t.Helper()
	var phase string
	for end := time.Now().Add(15 * time.Second); phase != want; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(end) {
			t.Fatalf("progress never said %s (last %q)", want, phase)
		}
		if p, err := call(t, m, "progress", "", map[string]any{"request_id": key}); err == nil {
			phase, _ = p["phase"].(string)
		}
	}
}

func progressState(t *testing.T, m map[string]string, key string) string {
	t.Helper()
	p, err := call(t, m, "progress", "", map[string]any{"request_id": key})
	if err != nil {
		t.Fatalf("progress: %v", err)
	}
	return p["state"].(string)
}

// keyHandles is list's handles for key.
func keyHandles(t *testing.T, m map[string]string, key string) []string {
	t.Helper()
	l, err := call(t, m, "list", "", nil)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var hs []string
	for _, e := range l["handles"].([]any) {
		if e := e.(map[string]any); e["request_id"] == key {
			hs = append(hs, e["handle"].(string))
		}
	}
	return hs
}

// cancelEndsAll cancels key, expects cancelled true, then nothing listed for
// the key and no VM left.
func cancelEndsAll(t *testing.T, m map[string]string, key string) {
	t.Helper()
	c, err := call(t, m, "cancel", "", map[string]any{"request_id": key})
	if err != nil || c["cancelled"] != true {
		t.Fatalf("cancel: %v %v", c, err)
	}
	for end := time.Now().Add(10 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		hs := keyHandles(t, m, key)
		vs, _ := (dirLayer{filepath.Join(m["state_dir"], "fakevms")}).List()
		if len(hs) == 0 && len(vs) == 0 {
			return
		}
		if time.Now().After(end) {
			t.Fatalf("after the cancel: list %v, VMs %v", hs, vs)
		}
	}
}

// An acquire killed after its box went ready, before it recorded ready: a
// later cancel answers cancelled and the box's supervisor ends the box.
func TestCancelEndsABoxWhoseAcquireDiedAfterReady(t *testing.T) {
	drv, m := boxBindingBooting(t, "3000") // room to stop the acquire while it boots
	cmd := acquireProcess(t, drv, m, map[string]any{"request_id": "k", "ttl_s": 600})
	waitPhase(t, m, "k", "booting")
	stopUnlocked(t, cmd, m["state_dir"]) // it never records ready
	for end := time.Now().Add(15 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		if hs := keyHandles(t, m, "k"); len(hs) == 1 {
			if _, err := call(t, m, "exec", hs[0], map[string]any{"argv": []string{"true"}}); err == nil {
				break // ready: it answers exec
			}
		}
		if time.Now().After(end) {
			t.Fatal("the box never went ready")
		}
	}
	kill(t, cmd)
	if s := progressState(t, m, "k"); s != "starting" {
		t.Fatalf("progress %q, want starting: the acquire died before recording ready", s)
	}
	cancelEndsAll(t, m, "k")
	if s := progressState(t, m, "k"); s != "cancelled" {
		t.Errorf("progress after the cancel: %q", s)
	}
}

// An acquire killed while its VM boots: a refused repeat is not held up by
// the dead acquire's starting (progress says failed), and a cancel then ends
// the VM: its supervisor fails the start once the boot is done.
func TestKilledAcquireThenRefusedRepeatThenCancel(t *testing.T) {
	drv, m := boxBindingBooting(t, "4000")
	cmd := acquireProcess(t, drv, m, map[string]any{"request_id": "k", "ttl_s": 600})
	waitPhase(t, m, "k", "booting")
	kill(t, cmd)
	if _, err := call(t, m, "acquire", "", map[string]any{"request_id": "k", "ttl_s": 1 << 30}); code(err) != "bad_request" {
		t.Fatalf("a repeat past ttl_max: %v, want bad_request", err)
	}
	if s := progressState(t, m, "k"); s != "failed" {
		t.Fatalf("progress after a refused repeat of a dead acquire: %q, want failed", s)
	}
	cancelEndsAll(t, m, "k")
}

// A refused repeat while the first acquire is in flight leaves that acquire's
// progress: starting, then ready when it answers.
func TestRefusedRepeatWhileInFlight(t *testing.T) {
	_, m := boxBindingBooting(t, "1500")
	done := make(chan map[string]any, 1)
	go func() {
		a, err := call(t, m, "acquire", "", map[string]any{"request_id": "k", "ttl_s": 600})
		if err != nil {
			t.Errorf("acquire: %v", err)
		}
		done <- a
	}()
	waitPhase(t, m, "k", "booting")
	if _, err := call(t, m, "acquire", "", map[string]any{"request_id": "k", "ttl_s": 1 << 30}); code(err) != "bad_request" {
		t.Fatalf("a repeat past ttl_max: %v, want bad_request", err)
	}
	if s := progressState(t, m, "k"); s != "starting" {
		t.Fatalf("progress after the refused repeat: %q, want starting", s)
	}
	var a map[string]any
	select {
	case a = <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("the first acquire did not answer")
	}
	if s := progressState(t, m, "k"); s != "ready" {
		t.Fatalf("progress after the first acquire answered: %q, want ready", s)
	}
	if h, ok := a["handle"].(string); ok {
		_, _ = call(t, m, "release", h, nil)
	}
}
