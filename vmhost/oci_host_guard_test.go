package vmhost

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

// This is an actual child-process/pipe harness, with fake native/Docker APIs.
// It never starts a container or calls the system manager.
func TestHostGuardCrashDeadlineAndCLIHang(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the Kata guard uses Linux monotonic uptime")
	}
	for _, mode := range []string{"owner-killed", "owner-stopped", "attach-fails", "attach-exits-during-cleanup", "renew-confirmed", "stale-frame", "invalid-parent"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			bin := filepath.Join(root, "bin")
			_ = os.Mkdir(bin, 0700)
			name := hostParentName(root, "own-vm", 0)
			cg := filepath.Join(root, "potluck.slice", name)
			_ = os.MkdirAll(cg, 0700)
			_ = os.WriteFile(filepath.Join(cg, "cgroup.events"), []byte("populated 1\n"), 0600)
			neighbor := filepath.Join(root, "neighbor")
			_ = os.WriteFile(neighbor, []byte("alive"), 0600)
			killed := filepath.Join(root, "native-killed")
			calls := filepath.Join(root, "calls")
			sudo := `#!/bin/sh
echo native >> "$GUARD_CALLS"
case "$2" in
sh) printf 'populated 0\n' > "$GUARD_CG/cgroup.events"; touch "$GUARD_KILLED";;
busctl) :;;
*) exit 2;;
esac
`
			docker := `#!/bin/sh
echo "$1" >> "$GUARD_CALLS"
case "$1" in
start) [ "$GUARD_MODE" != attach-fails ] || exit 7; while [ ! -e "$GUARD_KILLED" ]; do sleep .01; done;;
rm) if [ "$GUARD_MODE" = attach-exits-during-cleanup ]; then sleep 30; fi;;
*) exit 2;;
esac
`
			_ = os.WriteFile(filepath.Join(bin, "sudo"), []byte(sudo), 0700)
			_ = os.WriteFile(filepath.Join(bin, "docker"), []byte(docker), 0700)
			r, w, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			defer w.Close()
			duration := 700 * time.Millisecond
			if mode == "owner-killed" {
				duration = 4 * time.Second
			}
			deadline, err := hostDeadline(time.Now().Add(duration), 0)
			if err != nil {
				t.Fatal(err)
			}
			argName := name
			if mode == "invalid-parent" {
				argName = "potluck-not-owned.slice"
			}
			guard := exec.Command("bash", "-c", ociHostDeadman, "guard", "own-vm", filepath.Join(bin, "docker"), argName, cg, fmt.Sprint(deadline), root)
			guard.Stdin = r
			guard.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			guard.WaitDelay = time.Second
			guard.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "GUARD_CG="+cg, "GUARD_KILLED="+killed, "GUARD_CALLS="+calls, "GUARD_MODE="+mode)
			var log bytes.Buffer
			guard.Stdout = &log
			guard.Stderr = &log
			if err := guard.Start(); err != nil {
				t.Fatal(err)
			}
			defer syscall.Kill(-guard.Process.Pid, syscall.SIGKILL)
			done := make(chan error, 1)
			go func() { done <- guard.Wait() }()
			if mode == "owner-killed" || mode == "owner-stopped" {
				owner := exec.Command("sleep", "30")
				owner.ExtraFiles = []*os.File{w}
				if err := owner.Start(); err != nil {
					t.Fatal(err)
				}
				defer func() { _ = owner.Process.Kill(); _ = owner.Wait() }()
				_ = w.Close()
				if mode == "owner-stopped" {
					_ = owner.Process.Signal(syscall.SIGSTOP)
				} else {
					_ = owner.Process.Kill()
				}
			}
			if mode == "renew-confirmed" {
				time.Sleep(300 * time.Millisecond)
				pipe := hostGuardPipe{WriteCloser: w}
				if err := pipe.confirm(time.Now().Add(time.Second)); err != nil {
					t.Fatal(err)
				}
				time.Sleep(500 * time.Millisecond)
				if _, err := os.Stat(killed); err == nil {
					t.Fatal("confirmed renewal did not extend the old bound")
				}
			}
			if mode == "stale-frame" {
				_, _ = fmt.Fprintln(w, 1)
			}
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatalf("guard/CLI cleanup hung: %s", log.String())
			}
			if mode == "attach-fails" && guard.ProcessState.ExitCode() != 7 {
				t.Fatalf("attach exit status lost: %v (%s)", guard.ProcessState, log.String())
			}
			if _, err := os.Stat(neighbor); err != nil {
				t.Fatal("another guest was touched")
			}
			b, _ := os.ReadFile(calls)
			if mode == "invalid-parent" {
				if len(b) > 0 {
					t.Fatalf("invalid ownership reached native/Docker API: %s", b)
				}
				return
			}
			if _, err := os.Stat(killed); err != nil {
				t.Fatalf("no native kill before guard exit: %s (%s)", b, log.String())
			}
			cleanupCalls := strings.ReplaceAll(string(b), "start\n", "")
			if !strings.Contains(cleanupCalls, "native\nnative\nrm\n") {
				t.Fatalf("cleanup did not independently kill/stop native parent before Docker removal: %s", b)
			}
		})
	}
}
