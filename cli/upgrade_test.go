package cli

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sengirunners/potluck/kit/api"
)

// A daemon whose tailnet node runs without config enabling it was started
// with --tailnet: the restarted one gets it too, and only then.
func TestRestartKeepsTailnet(t *testing.T) {
	for _, c := range []struct {
		ts         api.TailnetStatus
		err        error
		configured bool
		want       bool
	}{
		{api.TailnetStatus{Running: true}, nil, false, true},
		{api.TailnetStatus{Running: true, Enabled: true}, nil, true, false}, // config brings it up again
		{api.TailnetStatus{Running: false}, nil, false, false},
		{api.TailnetStatus{}, errors.New("no route"), false, false},
	} {
		if got := keepTailnet(c.ts, c.err, c.configured); got != c.want {
			t.Errorf("keepTailnet(%+v, %v, %v) = %v", c.ts, c.err, c.configured, got)
		}
	}
	if got := (daemonRun{bin: "/x/potluck", tailnet: true}).argv(); !slices.Equal(got, []string{"/x/potluck", "daemon", "--tailnet"}) {
		t.Fatalf("argv %v", got)
	}
	if got := (daemonRun{bin: "/x/potluck"}).argv(); !slices.Equal(got, []string{"/x/potluck", "daemon"}) {
		t.Fatalf("argv %v", got)
	}
}

// The restarted daemon never inherits the token the upgrade downloaded with.
func TestUpgradeEnvDropsTokens(t *testing.T) {
	env := upgradeEnv([]string{"PATH=/bin", "GH_TOKEN=a", "GITHUB_TOKEN=b", "POTLUCK_HOME=/old", EnvUpgradeStartWait + "=1s"}, "/home")
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		if k == "GH_TOKEN" || k == "GITHUB_TOKEN" || k == EnvUpgradeStartWait || kv == "POTLUCK_HOME=/old" {
			t.Fatalf("restarted daemon gets %s: %v", kv, env)
		}
	}
	if !slices.Contains(env, "POTLUCK_HOME=/home") || !slices.Contains(env, "PATH=/bin") {
		t.Fatalf("env %v", env)
	}
}

// processExe names the program a pid runs (the old daemon is restarted
// from its own program).
func TestProcessExe(t *testing.T) {
	got, err := processExe(os.Getpid())
	if err != nil {
		t.Skip("not read on this system:", err)
	}
	self, _ := os.Executable()
	a, _ := filepath.EvalSymlinks(got)
	b, _ := filepath.EvalSymlinks(self)
	if a != b {
		t.Fatalf("processExe = %q, want %q", got, self)
	}
}

func TestOneWaySwap(t *testing.T) {
	if oneWaySwap(api.Preflight{Notes: []string{"no daemon answers"}}) {
		t.Fatal("one-way without migrations")
	}
	if !oneWaySwap(api.Preflight{Notes: []string{"this build applies 1 migrations at its first start (x): the swap is one-way; ..."}}) {
		t.Fatal("pending migrations not one-way")
	}
}

// The old daemon restarts from the program it ran, unless that path is not
// absolute or was deleted: then from the install directory's potluck.
func TestOldDaemonBin(t *testing.T) {
	const fallback = "/install/potluck"
	for _, c := range []struct {
		exe  string
		err  error
		want string
	}{
		{"/src/potluck", nil, "/src/potluck"},
		{"potluck", nil, fallback},
		{"./bin/potluck", nil, fallback},
		{"/src/potluck (deleted)", nil, fallback},
		{"", errors.New("no such process"), fallback},
	} {
		if got := oldDaemonBin(c.exe, c.err, fallback); got != c.want {
			t.Errorf("oldDaemonBin(%q, %v) = %q, want %q", c.exe, c.err, got, c.want)
		}
	}
}
