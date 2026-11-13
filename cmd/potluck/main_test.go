package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/potluck/kit/client"
)

// The test binary is the potluck binary when POTLUCK_TEST_MAIN=1.
func TestMain(m *testing.M) {
	if os.Getenv("POTLUCK_TEST_MAIN") == "1" {
		os.Args = append([]string{"potluck"}, os.Args[1:]...)
		main()
	}
	os.Exit(m.Run())
}

// potluck runs this binary as potluck with env (and nothing else from this
// process's POTLUCK_*, ZM_* or HOME).
func potluck(env []string, args ...string) *exec.Cmd {
	c := exec.Command(os.Args[0], args...)
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if k == "HOME" || strings.HasPrefix(k, "POTLUCK_") || strings.HasPrefix(k, "ZM_") {
			continue
		}
		c.Env = append(c.Env, kv)
	}
	c.Env = append(c.Env, "POTLUCK_TEST_MAIN=1", "POTLUCK_NO_AUTOSTART=1")
	c.Env = append(c.Env, env...)
	return c
}

// daemonAt starts `potluck daemon` with env, waits until its socket in
// home answers, and checks `potluck status --json` names that home; the
// returned func stops it with `potluck daemon stop`.
func daemonAt(t *testing.T, env []string, home string) func() {
	t.Helper()
	d := potluck(env, "daemon")
	logf, err := os.Create(filepath.Join(t.TempDir(), "daemon.log"))
	if err != nil {
		t.Fatal(err)
	}
	d.Stdout, d.Stderr = logf, logf
	if err := d.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- d.Wait() }()
	stopped := false
	stop := func() {
		if stopped {
			return
		}
		stopped = true
		out, err := potluck(env, "daemon", "stop").CombinedOutput()
		if err != nil || !strings.Contains(string(out), "potluck daemon stopped") {
			t.Errorf("daemon stop: %v\n%s", err, out)
			_ = d.Process.Kill()
		}
		select {
		case <-exited:
		case <-time.After(15 * time.Second):
			_ = d.Process.Kill()
			t.Errorf("the daemon did not exit after daemon stop")
		}
	}
	t.Cleanup(stop)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := client.NewUnix(filepath.Join(home, "potluck.sock")).WaitReady(ctx, 20*time.Second); err != nil {
		b, _ := os.ReadFile(logf.Name())
		t.Fatalf("no daemon answered at %s: %v\n%s", home, err, b)
	}
	out, err := potluck(env, "status", "--json").Output()
	if err != nil {
		t.Fatalf("status: %v\n%s", err, out)
	}
	var st struct {
		Home string `json:"home"`
	}
	if err := json.Unmarshal(out, &st); err != nil || st.Home != home {
		t.Fatalf("status names home %q, want %q (%v)\n%s", st.Home, home, err, out)
	}
	return stop
}

// potluck's home is $POTLUCK_HOME, else ~/.potluck. ZM_HOME never moves it,
// and the binary never creates or touches the ZM_HOME directory.
func TestHome(t *testing.T) {
	user, err := os.MkdirTemp("", "plh") // short: the socket path has a limit
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(user)
	user, _ = filepath.EvalSymlinks(user)
	zmHome := filepath.Join(user, "zmhome")
	if err := os.Mkdir(zmHome, 0o700); err != nil {
		t.Fatal(err)
	}
	env := []string{"HOME=" + user, "ZM_HOME=" + zmHome}

	// No POTLUCK_HOME: ~/.potluck, whatever ZM_HOME says.
	stop := daemonAt(t, env, filepath.Join(user, ".potluck"))
	stop()
	if _, err := os.Stat(filepath.Join(user, ".potluck", "registry.db")); err != nil {
		t.Errorf("the daemon kept nothing in ~/.potluck: %v", err)
	}

	// POTLUCK_HOME set: there.
	own := filepath.Join(user, "own")
	stop = daemonAt(t, append(env, "POTLUCK_HOME="+own), own)
	stop()

	if ents, err := os.ReadDir(zmHome); err != nil || len(ents) != 0 {
		t.Fatalf("the ZM_HOME directory was touched: %v %v", ents, err)
	}
	if _, err := os.Stat(filepath.Join(user, ".zm")); !os.IsNotExist(err) {
		t.Fatalf("~/.zm exists: %v", err)
	}
}
