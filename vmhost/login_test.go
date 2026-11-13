package vmhost

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// loginLayer is the fake layer with a login to close: its step leaves a mark
// in the guest's HOME, or fails for an image named *sshopen*.
type loginLayer struct {
	dirLayer
	open bool
}

func (l loginLayer) CloseLogin() []string {
	if l.open {
		return []string{"sh", "-c", `echo "port 22 still answers" >&2; exit 1`}
	}
	return []string{"sh", "-c", `touch "$HOME/logins-closed"`}
}

func init() {
	layers["fakelogin"] = func(c config) vmLayer {
		return loginLayer{dirLayer{filepath.Join(c.StateDir, "fakevms")}, strings.Contains(c.Image, "sshopen")}
	}
}

// A layer with a login to close (Tart) runs its step in each guest before a
// box is answered ready, and a guest whose step fails is never lent.
func TestBoxClosesGuestLoginsBeforeReady(t *testing.T) {
	_, _, m := fakeBinding(t, "linux-img")
	delete(m, "image")
	m["vm"], m["target"], m["kind"], m["size"], m["cpu"], m["memory"] = "fakelogin", "t", "box", "small", "1", "512"
	m["images"] = "fake/linux@sha256:" + strings.Repeat("ab", 32)
	a, err := call(t, m, "acquire", "", map[string]any{"request_id": "k", "ttl_s": 60})
	if err != nil {
		t.Fatal(err)
	}
	h := a["handle"].(string)
	marks, _ := filepath.Glob(filepath.Join(m["state_dir"], "fakevms", "*", "home", "logins-closed"))
	if _, err := call(t, m, "release", h, nil); err != nil {
		t.Error(err)
	}
	if len(marks) != 1 {
		t.Fatalf("a ready box whose logins were not closed (marks %v)", marks)
	}

	m["images"] = "fake/sshopen@sha256:" + strings.Repeat("cd", 32)
	if _, err := call(t, m, "acquire", "", map[string]any{"request_id": "k2", "ttl_s": 60}); err == nil ||
		!strings.Contains(err.Error(), "closing the guest's logins") {
		t.Fatalf("a guest that kept its login open was lent: %v", err)
	}
}

// Tart's step stops sshd and Screen Sharing as root (sudo -n) and fails while
// port 22 or 5900 still answers. It runs here against stub commands only,
// first on PATH: nothing real is stopped.
func TestTartCloseLoginStopsSSHAndChecksThePorts(t *testing.T) {
	if len(tartCLI{}.CloseLogin()) != 3 {
		t.Fatal("CloseLogin is not sh -c <program>")
	}
	run := func(t *testing.T, osName string, stuckOpen bool) (string, string, error) {
		t.Helper()
		stub := t.TempDir()
		log := filepath.Join(stub, "log")
		// nc answers while the service on that port has not been stopped
		nc := `p=$(eval echo \${$#}); case $p in 22) s=sshd ;; 5900) s=screensharing ;; esac
grep -Eq "bootout system/com.(openssh|apple).$s|disable --now ssh.socket" "$LOG" 2>/dev/null && [ -z "$STUCK" ] && exit 1; exit 0`
		for name, body := range map[string]string{
			"uname": "echo " + osName, "id": "echo 501", "sudo": `echo "sudo $*" >>"$LOG"; [ "$1" = -n ] && shift; exec "$@"`,
			"launchctl": `echo "launchctl $*" >>"$LOG"`, "systemctl": `echo "systemctl $*" >>"$LOG"`, "nc": nc,
		} {
			if err := os.WriteFile(filepath.Join(stub, name), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		a := tartCLI{}.CloseLogin()
		cmd := exec.Command("/bin/sh", a[1:]...)
		cmd.Env = []string{"PATH=" + stub + ":/bin:/usr/bin", "LOG=" + log}
		if stuckOpen {
			cmd.Env = append(cmd.Env, "STUCK=1")
		}
		var errb strings.Builder
		cmd.Stderr = &errb
		err := cmd.Run()
		b, _ := os.ReadFile(log)
		return string(b), errb.String(), err
	}
	calls, _, err := run(t, "Darwin", false)
	for _, want := range []string{"sudo -n launchctl bootout system/com.openssh.sshd", "sudo -n launchctl disable system/com.openssh.sshd",
		"sudo -n launchctl bootout system/com.apple.screensharing"} {
		if !strings.Contains(calls, want) {
			t.Errorf("macOS: no %q in\n%s", want, calls)
		}
	}
	if err != nil {
		t.Errorf("macOS with both stopped: %v", err)
	}
	calls, _, err = run(t, "Linux", false)
	if !strings.Contains(calls, "sudo -n systemctl disable --now ssh.socket") || !strings.Contains(calls, "systemctl disable --now ssh.service") || err != nil {
		t.Errorf("Linux: %v\n%s", err, calls)
	}
	if _, stderr, err := run(t, "Darwin", true); err == nil || !strings.Contains(stderr, "port 22 or 5900") {
		t.Errorf("SSH still answering: %v %q", err, stderr)
	}
}
