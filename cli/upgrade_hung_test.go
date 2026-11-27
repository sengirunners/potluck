package cli

import (
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sengirunners/potluck/kit/config"
)

// A new daemon that never answers is ended by a later upgrade, whether it
// obeys TERM or not; another program's process that holds the lock never is.
func TestEndUnanswering(t *testing.T) {
	if home := os.Getenv("HUNG_TEST_HOME"); home != "" { // the daemon
		if os.Getenv("HUNG_TEST_IGNORE") != "" {
			signal.Ignore(syscall.SIGTERM)
		}
		if _, err := config.LockHome(home); err != nil {
			fmt.Println(err)
			os.Exit(1)
		}
		fmt.Println("held")
		time.Sleep(time.Minute)
		os.Exit(1)
	}
	was := upgradeStopWait
	upgradeStopWait = 2 * time.Second
	t.Cleanup(func() { upgradeStopWait = was })
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name, ignore, program string
		ended                 bool
	}{
		{"ends when asked", "", self, true},
		{"never ends when asked", "1", self, true},
		{"another program's", "", "/bin/sh", false},
		{"no program known", "", "", false},
	} {
		home := t.TempDir()
		if _, gone := endUnanswering(home, self); !gone {
			t.Errorf("a home with no daemon: not gone")
		}
		hung := exec.Command(self, "-test.run=^TestEndUnanswering$")
		hung.Env = append(os.Environ(), "HUNG_TEST_HOME="+home, "HUNG_TEST_IGNORE="+c.ignore)
		out, err := hung.StdoutPipe()
		if err == nil {
			err = hung.Start()
		}
		if err != nil {
			t.Fatal(err)
		}
		done := make(chan struct{})
		b := make([]byte, 64)
		if n, _ := out.Read(b); !strings.Contains(string(b[:n]), "held") {
			t.Fatalf("%s: %s", c.name, b[:n])
		}
		go func() { _ = hung.Wait(); close(done) }()
		pid, gone := endUnanswering(home, filepath.Join(home, "none"), c.program)
		if gone != c.ended || gone == homeLockHeld(home) || pid != hung.Process.Pid {
			t.Errorf("%s: gone %t, held %t, pid %d of %d", c.name, gone, homeLockHeld(home), pid, hung.Process.Pid)
		}
		_ = hung.Process.Kill()
		<-done
	}

	// A lock whose pid is another process's ends none.
	home := t.TempDir()
	release, err := config.LockHome(home)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	other := exec.Command("/bin/sleep", "60")
	if err := other.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = other.Process.Kill(); _ = other.Wait() }()
	if err := os.WriteFile(filepath.Join(home, config.FileHomeLock), []byte(strconv.Itoa(other.Process.Pid)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, gone := endUnanswering(home, self); gone || syscall.Kill(other.Process.Pid, 0) != nil {
		t.Errorf("gone %t, the other process: %v", gone, syscall.Kill(other.Process.Pid, 0))
	}

	// One that takes the lock once the hung one was asked to end is not killed.
	hung := func() *exec.Cmd {
		c := exec.Command(self, "-test.run=^TestEndUnanswering$")
		c.Env = append(os.Environ(), "HUNG_TEST_HOME="+t.TempDir(), "HUNG_TEST_IGNORE=1")
		out, err := c.StdoutPipe()
		if err == nil {
			err = c.Start()
		}
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = c.Process.Kill(); _ = c.Wait() })
		_, _ = out.Read(make([]byte, 64)) // it ignores TERM by now
		return c
	}
	old, next := hung(), hung()
	holds := func(c *exec.Cmd) {
		if err := os.WriteFile(filepath.Join(home, config.FileHomeLock), []byte(strconv.Itoa(c.Process.Pid)), 0o600); err != nil {
			t.Error(err)
		}
	}
	holds(old)
	time.AfterFunc(upgradeStopWait/2, func() { holds(next) })
	if pid, gone := endUnanswering(home, self); gone || pid != next.Process.Pid {
		t.Errorf("gone %t, pid %d of %d", gone, pid, next.Process.Pid)
	}
	var ws syscall.WaitStatus
	if pid, err := syscall.Wait4(next.Process.Pid, &ws, syscall.WNOHANG, nil); err != nil || pid != 0 {
		t.Errorf("the next one ended: %v %v", ws, err)
	}
}
