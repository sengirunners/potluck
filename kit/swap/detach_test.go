package swap

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func detachHome(t *testing.T) string {
	t.Helper()
	h, err := os.MkdirTemp("", "zm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(h) })
	return h
}

// stopPid ends a process a test started, and waits for it to go.
func stopPid(t *testing.T, p *Detached) {
	t.Helper()
	_ = p.Signal(syscall.SIGKILL)
	select {
	case <-p.Exited:
	case <-time.After(5 * time.Second):
		t.Errorf("pid %d did not exit", p.Pid)
	}
}

func TestLaunchPlistEscapesAndRunsOnce(t *testing.T) {
	got := launchPlist("dev.zm.launch.x", []string{"/a b/zm", LaunchArg, "/h/<&>.json"}, "/h/daemon.log")
	for _, want := range []string{
		"<string>dev.zm.launch.x</string>", "<string>/a b/zm</string>", "<string>/h/&lt;&amp;&gt;.json</string>",
		"<key>KeepAlive</key><false/>", "<key>LaunchOnlyOnce</key><true/>", "<key>AbandonProcessGroup</key><true/>",
		"<key>ProcessType</key><string>Interactive</string>",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("plist lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "EnvironmentVariables") {
		t.Error("the environment must stay out of the job's definition")
	}
}

func TestLauncherStartsDetachedWritesPidAndDeletesSpec(t *testing.T) {
	h := detachHome(t)
	spec := filepath.Join(h, "launch-t.json")
	pidFile := filepath.Join(h, "launch-t.pid")
	b, _ := json.Marshal(launchSpec{Argv: []string{"/bin/sleep", "30"}, Env: []string{"A=1"}, Dir: h,
		Log: filepath.Join(h, "daemon.log"), PidFile: pidFile})
	if err := os.WriteFile(spec, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if code := launchMain([]string{spec}); code != 0 {
		t.Fatalf("launcher exit %d", code)
	}
	if _, err := os.Stat(spec); !os.IsNotExist(err) {
		t.Errorf("spec (with the environment) left on disk: %v", err)
	}
	raw, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	p := &Detached{Pid: pid, Exited: watchGone(pid)}
	defer stopPid(t, p)
	if sid, err := syscall.Getpgid(pid); err != nil || sid != pid {
		t.Errorf("daemon not in its own session/group: pgid %d, %v", sid, err)
	}
}

func TestLauncherRefusesBadSpec(t *testing.T) {
	h := detachHome(t)
	spec := filepath.Join(h, "bad.json")
	_ = os.WriteFile(spec, []byte(`{"argv":[]}`), 0o600)
	if code := launchMain([]string{spec}); code == 0 {
		t.Fatal("launcher accepted an empty argv")
	}
	if code := launchMain(nil); code != 2 {
		t.Fatalf("usage exit %d, want 2", code)
	}
}

func TestStartDetachedDirectReportsExit(t *testing.T) {
	old := launchctl
	launchctl = ""
	defer func() { launchctl = old }()
	h := detachHome(t)
	p, err := StartDetached(context.Background(), Detach{Argv: []string{"/bin/sh", "-c", "exit 3"},
		Log: filepath.Join(h, "daemon.log"), Home: h})
	if err != nil {
		t.Fatal(err)
	}
	if p.Via != "direct" {
		t.Errorf("via %q", p.Via)
	}
	select {
	case <-p.Exited:
	case <-time.After(5 * time.Second):
		t.Fatal("exit not reported")
	}
}

// TestStartDetachedThroughLaunchd runs where a GUI launchd domain takes
// jobs (a logged-in Mac): the daemon is the launcher's child, so after the
// launcher exits its parent is launchd, and nothing of the job is left.
func TestStartDetachedThroughLaunchd(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("launchd only")
	}
	if exec.Command(launchctl, "print", "gui/"+strconv.Itoa(os.Getuid())).Run() != nil {
		t.Skip("no GUI launchd domain (not a logged-in session)")
	}
	h := detachHome(t)
	p, err := StartDetached(context.Background(), Detach{Argv: []string{"/bin/sleep", "30"},
		Log: filepath.Join(h, "daemon.log"), Home: h})
	if err != nil {
		t.Fatal(err)
	}
	defer stopPid(t, p)
	if p.Via != "launchd" {
		log, _ := os.ReadFile(filepath.Join(h, "daemon.log"))
		t.Fatalf("via %q; log:\n%s", p.Via, log)
	}
	if ppid := parentOf(t, p.Pid); ppid != 1 {
		t.Errorf("parent %d, want launchd (1)", ppid)
	}
	ents, _ := os.ReadDir(h)
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), "launch-") {
			t.Errorf("left %s", e.Name())
		}
	}
	if out, _ := exec.Command(launchctl, "list").Output(); strings.Contains(string(out), "dev.zm.launch.") {
		t.Error("launch job left loaded")
	}
}

func parentOf(t *testing.T, pid int) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		out, err := exec.Command("ps", "-o", "ppid=", "-p", strconv.Itoa(pid)).Output()
		if err != nil {
			t.Fatal(err)
		}
		ppid, _ := strconv.Atoi(strings.TrimSpace(string(out)))
		if ppid == 1 || time.Now().After(deadline) {
			return ppid
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestStartDetachedFallsBackWhenLaunchdRefuses: no GUI domain (ssh) starts
// the daemon directly, once, and says so in its log.
func TestStartDetachedFallsBackWhenLaunchdRefuses(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("launchd only")
	}
	old := launchctl
	launchctl = "/usr/bin/false"
	defer func() { launchctl = old }()
	h := detachHome(t)
	logPath := filepath.Join(h, "daemon.log")
	p, err := StartDetached(context.Background(), Detach{Argv: []string{"/bin/sleep", "30"}, Log: logPath, Home: h})
	if err != nil {
		t.Fatal(err)
	}
	defer stopPid(t, p)
	if !strings.HasPrefix(p.Via, "direct: ") {
		t.Errorf("via %q", p.Via)
	}
	if log, _ := os.ReadFile(logPath); !strings.Contains(string(log), "started it directly") {
		t.Errorf("log does not say it started directly:\n%s", log)
	}
}

// TestLauncherLeavesSignalsDefaultInTheDaemon: the launcher holds off TERM
// while it works, and the daemon still gets TERM's default action.
func TestLauncherLeavesSignalsDefaultInTheDaemon(t *testing.T) {
	h := detachHome(t)
	spec := filepath.Join(h, "launch-s.json")
	pidFile := filepath.Join(h, "launch-s.pid")
	b, _ := json.Marshal(launchSpec{Argv: []string{"/bin/sleep", "30"}, Dir: h, Log: filepath.Join(h, "daemon.log"), PidFile: pidFile})
	if err := os.WriteFile(spec, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if code := launchMain([]string{spec}); code != 0 {
		t.Fatalf("launcher exit %d", code)
	}
	p := readPid(pidFile)
	if p == nil {
		t.Fatal("no pid")
	}
	_ = p.Signal(syscall.SIGTERM)
	select {
	case <-p.Exited:
	case <-time.After(5 * time.Second):
		stopPid(t, p)
		t.Fatal("the daemon ignored TERM")
	}
}
