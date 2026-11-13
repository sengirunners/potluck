package vmhost

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func init() {
	layers["fakeawake"] = func(c config) vmLayer { return awakeDirLayer{dirLayer{filepath.Join(c.StateDir, "fakevms")}} }
}

// awakeDirLayer is the fake layer with Tart's sleep hold, so the supervisor's
// use of it is tested with no VM and no real caffeinate.
type awakeDirLayer struct{ dirLayer }

func (awakeDirLayer) KeepAwake(pid int) *exec.Cmd { return tartCLI{}.KeepAwake(pid) }

// fakeCaffeinate puts a caffeinate on PATH that logs "<its pid> <its args>"
// and, like the real one, runs until the process named by -w exits (a zombie
// has: the test process, its parent, never reaps a killed supervisor). The log
// is where the test reads who held what.
func fakeCaffeinate(t *testing.T) (log string) {
	t.Helper()
	dir := t.TempDir()
	log = filepath.Join(dir, "calls")
	script := "#!/bin/sh\necho \"$$ $*\" >> \"$FAKE_CAFFEINATE_LOG\"\nwhile s=$(ps -o stat= -p \"$3\" 2>/dev/null); [ -n \"$s\" ] && [ \"${s#Z}\" = \"$s\" ]; do sleep 0.1; done\n"
	if err := os.WriteFile(filepath.Join(dir, "caffeinate"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_CAFFEINATE_LOG", log)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return log
}

// running says whether pid is a live process: a zombie is not, whoever may
// never reap it.
func running(pid int) bool {
	out, err := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
	s := strings.TrimSpace(string(out))
	return err == nil && s != "" && s[0] != 'Z'
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for end := time.Now().Add(10 * time.Second); time.Now().Before(end); time.Sleep(50 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

// caffeinateCall reads the log: the pid and -w target of the n-th caffeinate started.
func caffeinateCall(t *testing.T, log string, n int) (pid, watched int) {
	t.Helper()
	waitFor(t, "caffeinate "+strconv.Itoa(n), func() bool {
		b, _ := os.ReadFile(log)
		return len(strings.Split(strings.TrimSpace(string(b)), "\n")) >= n && len(b) > 0
	})
	b, _ := os.ReadFile(log)
	f := strings.Fields(strings.Split(strings.TrimSpace(string(b)), "\n")[n-1])
	if len(f) != 4 || f[1] != "-i" || f[2] != "-w" {
		t.Fatalf("caffeinate was run as %q, want `-i -w <pid>`", f)
	}
	pid, _ = strconv.Atoi(f[0])
	watched, _ = strconv.Atoi(f[3])
	return pid, watched
}

func awakeBox(t *testing.T) map[string]string {
	t.Helper()
	m := fakePersistBox(t)
	m["vm"] = "fakeawake"
	delete(m, "persist_max")
	return m
}

func TestTartKeepAwakeCommand(t *testing.T) {
	fakeCaffeinate(t)
	cmd := tartCLI{}.KeepAwake(4242)
	if cmd == nil || filepath.Base(cmd.Path) != "caffeinate" || strings.Join(cmd.Args[1:], " ") != "-i -w 4242" {
		t.Fatalf("KeepAwake(4242) = %+v", cmd)
	}
	t.Setenv("PATH", t.TempDir())
	if cmd := (tartCLI{}).KeepAwake(4242); cmd != nil {
		t.Errorf("no caffeinate on PATH, yet %v", cmd.Args)
	}
}

// The Mac does not sleep while a supervisor serves a VM, and the hold ends
// with the supervisor however it ends: a release, a lease that runs out, or
// a kill that runs no cleanup. All three rest on caffeinate -w <supervisor's
// pid>, which the test checks the supervisor passes.
func TestSupervisorKeepsTheMacAwake(t *testing.T) {
	log := fakeCaffeinate(t)
	m := awakeBox(t)
	st := store{m["state_dir"]}
	acquire := func(id string, ttl int) (handle string, sup int) {
		a, err := call(t, m, "acquire", "", map[string]any{"request_id": id, "ttl_s": ttl})
		if err != nil {
			t.Fatal(err)
		}
		handle = a["handle"].(string)
		r, err := st.load(handle[strings.LastIndex(handle, "/")+1:])
		if err != nil {
			t.Fatal(err)
		}
		return handle, r.Supervisor
	}
	gone := func(pid int) {
		t.Helper()
		waitFor(t, "caffeinate "+strconv.Itoa(pid)+" to end", func() bool { return !running(pid) })
	}

	h, sup := acquire("a1", 120)
	cp, watched := caffeinateCall(t, log, 1)
	if watched != sup || !alive(cp) {
		t.Fatalf("caffeinate %d watches %d (supervisor %d), alive %v", cp, watched, sup, alive(cp))
	}
	if _, err := call(t, m, "release", h, nil); err != nil {
		t.Fatal(err)
	}
	gone(cp)

	_, sup = acquire("a2", 1) // the lease runs out
	cp, watched = caffeinateCall(t, log, 2)
	if watched != sup {
		t.Fatalf("caffeinate watches %d, supervisor %d", watched, sup)
	}
	gone(cp)

	_, sup = acquire("a3", 120) // the supervisor is killed: no cleanup runs
	cp, watched = caffeinateCall(t, log, 3)
	if watched != sup || !alive(cp) {
		t.Fatalf("caffeinate %d watches %d (supervisor %d), alive %v", cp, watched, sup, alive(cp))
	}
	if err := syscall.Kill(sup, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	gone(cp)
}
