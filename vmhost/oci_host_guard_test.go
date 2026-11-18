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
if [ "$2" = -k ]; then shift; fi
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

// The Kata native parent's root calls (start, stop and the deadman's
// cleanup) ignore a sudo ticket cached at the owner's terminal: share's
// conformance run starts from that terminal, and a pass it got from the
// ticket is one the daemon's driver, which has none, cannot repeat. With
// passwordless sudo they run.
func TestHostParentIgnoresCachedSudoTicket(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the Kata guard uses Linux monotonic uptime")
	}
	for _, policy := range []string{"nopasswd", "password"} {
		for _, path := range []string{"start", "stop", "guard"} {
			t.Run(policy+"/"+path, func(t *testing.T) {
				root := t.TempDir()
				bin := filepath.Join(root, "bin")
				_ = os.Mkdir(bin, 0o700)
				busctl := `#!/bin/sh
[ "$5" = StartTransientUnit ] || exit 0
cg="$HOST_TEST_ROOT/potluck.slice/$7"; mkdir -p "$cg"
printf '1048576\n' > "$cg/memory.max"; printf '100000 100000\n' > "$cg/cpu.max"; printf 'populated 0\n' > "$cg/cgroup.events"
`
				for name, body := range map[string]string{"sudo": fakeSudo, "busctl": busctl, "docker": "#!/bin/sh\n[ \"$1\" != start ] || exec sleep 30\n"} {
					if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0o700); err != nil {
						t.Fatal(err)
					}
				}
				log := filepath.Join(root, "sudo.log")
				t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
				t.Setenv("SUDO_LOG", log)
				t.Setenv("SUDO_POLICY", policy)
				t.Setenv("SUDO_TICKET", "1") // as after sudo -v at this terminal
				t.Setenv("HOST_TEST_ROOT", root)
				old := hostCgroupRoot
				hostCgroupRoot = root
				t.Cleanup(func() { hostCgroupRoot = old })
				name := hostParentName(root, "own-vm", 0)
				cg := filepath.Join(root, "potluck.slice", name)
				var err error
				switch path {
				case "start":
					_, err = makeHostParent(name, hostBudget{CPU: 1, MemoryMiB: 1})
				case "stop":
					_ = os.MkdirAll(cg, 0o700)
					_ = os.WriteFile(filepath.Join(cg, "cgroup.events"), []byte("populated 0\n"), 0o600)
					err = (&ownedHostParent{name: name, path: cg, budget: hostBudget{CPU: 1, MemoryMiB: 1}}).stop()
				case "guard":
					_ = os.MkdirAll(cg, 0o700)
					_ = os.WriteFile(filepath.Join(cg, "cgroup.events"), []byte("populated 0\n"), 0o600)
					deadline, derr := hostDeadline(time.Now().Add(300*time.Millisecond), 0)
					if derr != nil {
						t.Fatal(derr)
					}
					guard := exec.Command("bash", "-c", ociHostDeadman, "guard", "own-vm", filepath.Join(bin, "docker"), name, cg, fmt.Sprint(deadline), root)
					guard.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
					guard.WaitDelay = time.Second
					r, w, perr := os.Pipe()
					if perr != nil {
						t.Fatal(perr)
					}
					guard.Stdin = r
					if err := guard.Start(); err != nil {
						t.Fatal(err)
					}
					r.Close()
					done := make(chan struct{})
					go func() { _ = guard.Wait(); close(done) }()
					select {
					case <-done:
					case <-time.After(5 * time.Second):
						t.Error("guard did not end at its deadline")
					}
					_ = syscall.Kill(-guard.Process.Pid, syscall.SIGKILL)
					w.Close()
				}
				b, _ := os.ReadFile(log)
				ran := string(b)
				if policy == "password" {
					if ran != "" {
						t.Fatalf("a cached ticket ran the native call as root: %q", ran)
					}
					if path == "start" && err == nil {
						t.Fatal("the native parent started on a ticket the daemon would not have")
					}
					return
				}
				if err != nil {
					t.Fatalf("passwordless sudo: %v", err)
				}
				want := []string{"busctl call"}
				if path != "start" {
					want = append(want, "sh -c")
				}
				for _, s := range want {
					if !strings.Contains(ran, s) {
						t.Fatalf("passwordless sudo did not run %q: %q", s, ran)
					}
				}
			})
		}
	}
}
