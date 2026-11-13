package agentrunners_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/potluck/agentrunners"
)

// localBox is a box that is a directory on this machine: exec and spawn run
// sh with HOME there and PATH the test's bin directory first.
type localBox struct {
	t     *testing.T
	home  string
	path  string
	procs map[string]*exec.Cmd
	sdirs map[string]string
}

func newBox(t *testing.T, bin string) *localBox {
	return &localBox{t: t, home: t.TempDir(), path: bin + ":/usr/bin:/bin", procs: map[string]*exec.Cmd{}, sdirs: map[string]string{}}
}

func (b *localBox) cmd(ctx context.Context, argv []string, stdin string) *exec.Cmd {
	c := exec.CommandContext(ctx, argv[0], argv[1:]...)
	c.Dir = b.home
	c.Env = []string{"PATH=" + b.path, "HOME=" + b.home}
	c.Stdin = strings.NewReader(stdin)
	return c
}

func (b *localBox) Exec(ctx context.Context, argv []string, stdin string) (int, string, string, error) {
	c := b.cmd(ctx, argv, stdin)
	var out, errb bytes.Buffer
	c.Stdout, c.Stderr = &out, &errb
	err := c.Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode(), out.String(), errb.String(), nil
	}
	return 0, out.String(), errb.String(), err
}

func (b *localBox) Spawn(ctx context.Context, argv []string, secrets map[string]string, timeoutS int, key string) (string, error) {
	c := b.cmd(context.Background(), argv, "")
	if len(secrets) > 0 {
		d, err := os.MkdirTemp(b.t.TempDir(), "sec")
		if err != nil {
			return "", err
		}
		for k, v := range secrets {
			if err := os.WriteFile(filepath.Join(d, k), []byte(v), 0o600); err != nil {
				return "", err
			}
		}
		c.Env = append(c.Env, "POTLUCK_SECRETS="+d)
		b.sdirs[key] = d
	}
	var out bytes.Buffer
	c.Stdout, c.Stderr = &out, &out
	if err := c.Start(); err != nil {
		return "", err
	}
	b.procs[key] = c
	return key, nil
}

func (b *localBox) wait(pid string) int {
	c := b.procs[pid]
	_ = c.Wait()
	return c.ProcessState.ExitCode()
}

// fakeClaude is a claude on PATH: as the runner it records its argv and
// its secret file's bytes, then starts the --exec-path wrapper as a runner
// would, with arguments of its own (one a --permission-mode the wrapper must
// override) and CLAUDE_RUNNER_CLAUDE_BIN naming a recorder.
const fakeClaude = `#!/bin/sh
rec=$(dirname "$0")/rec
printf '%s\n' "$@" > "$rec/runner"
while [ $# -gt 0 ]; do
  case "$1" in
    --environment-secret-file) cat "$2" > "$rec/secret"; shift ;;
    --exec-path) wrap=$2; shift ;;
  esac
  shift
done
CLAUDE_RUNNER_CLAUDE_BIN=$(dirname "$0")/session-bin exec "$wrap" --print --permission-mode default
`

const sessionBin = `#!/bin/sh
printf '%s\n' "$@" > "$(dirname "$0")/rec/session"
`

func fakeBin(t *testing.T) (bin, rec string) {
	bin = t.TempDir()
	rec = filepath.Join(bin, "rec")
	if err := os.Mkdir(rec, 0o700); err != nil {
		t.Fatal(err)
	}
	for n, s := range map[string]string{"claude": fakeClaude, "session-bin": sessionBin} {
		if err := os.WriteFile(filepath.Join(bin, n), []byte(s), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return bin, rec
}

func lines(t *testing.T, path string) []string {
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
}

// TestClaudeJob: the claude adapter starts one runner (capacity 1) with the
// secret's file, the job's base directory and its own session wrapper, and
// the wrapper runs each session with the runner's arguments first and the
// pinned ones last: auto mode overriding the runner's, then the tools, as
// given (a tool that looks like shell is an argument, never run).
func TestClaudeJob(t *testing.T) {
	a, ok := agentrunners.Lookup("claude")
	if !ok {
		t.Fatal("no claude adapter")
	}
	bin, rec := fakeBin(t)
	b := newBox(t, bin)
	retire := time.Unix(1893456000, 0)
	tricky := "it's $(touch " + filepath.Join(rec, "ran") + ") `x` \"q\""
	j := agentrunners.Job{Adapter: a, Key: "job-1", Secret: "ccenv-sekrit\n", TimeoutS: 60, Options: agentrunners.Options{Mode: "auto",
		Allow: []string{"Bash(git push:*)", tricky}, Deny: []string{"WebFetch"}, Idle: 90 * time.Second, RetireAt: retire}}
	pid, err := agentrunners.Start(context.Background(), b, j)
	if err != nil {
		t.Fatal(err)
	}
	if code := b.wait(pid); code != 0 {
		t.Fatalf("exit %d", code)
	}
	dir := filepath.Join(b.home, ".potluck", "agent-runner", "job-1")
	want := []string{"self-hosted-runner", "--environment-secret-file", filepath.Join(b.sdirs["job-1"], "CLAUDE_ENVIRONMENT_SECRET"),
		"--base-dir", b.home + "/claude-work", "--capacity", "1", "--exec-path", dir + "/session",
		"--exit-if-unused-min", "2", "--retire-at", strconv.FormatInt(retire.Unix(), 10)}
	if got := lines(t, filepath.Join(rec, "runner")); strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("runner argv:\n%q\nwant\n%q", got, want)
	}
	if s, _ := os.ReadFile(filepath.Join(rec, "secret")); string(s) != j.Secret {
		t.Fatalf("the runner read %q from its secret file", s)
	}
	want = []string{"--print", "--permission-mode", "default", "--permission-mode", "auto",
		"--allowed-tools", "Bash(git push:*)", "--allowed-tools", tricky, "--disallowed-tools", "WebFetch"}
	if got := lines(t, filepath.Join(rec, "session")); strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("session argv:\n%q\nwant\n%q", got, want)
	}
	if _, err := os.Stat(filepath.Join(rec, "ran")); err == nil {
		t.Fatal("a tool name ran as shell")
	}
	for _, f := range []string{"run", "session", "runner-args", "session-args"} {
		st, err := os.Stat(filepath.Join(dir, f))
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode().Perm()&0o077 != 0 {
			t.Fatalf("%s is mode %v: others can read it", f, st.Mode())
		}
		if b, _ := os.ReadFile(filepath.Join(dir, f)); strings.Contains(string(b), "sekrit") {
			t.Fatalf("the secret is in the job's %s", f)
		}
	}
}

// TestNoIdleNoRetire: without them the runner gets no such flags; the
// session gets the mode asked.
func TestNoIdleNoRetire(t *testing.T) {
	a, _ := agentrunners.Lookup("claude")
	bin, rec := fakeBin(t)
	b := newBox(t, bin)
	pid, err := agentrunners.Start(context.Background(), b, agentrunners.Job{Adapter: a, Key: "k", Secret: "s", Options: agentrunners.Options{Mode: "auto"}})
	if err != nil {
		t.Fatal(err)
	}
	b.wait(pid)
	if got := lines(t, filepath.Join(rec, "runner")); len(got) != 9 {
		t.Fatalf("runner argv %q", got)
	}
	if got := lines(t, filepath.Join(rec, "session")); strings.Join(got[len(got)-2:], " ") != "--permission-mode auto" {
		t.Fatalf("session argv %q", got)
	}
}

// TestNoMode: no mode pins none; the session gets the runner's arguments
// alone.
func TestNoMode(t *testing.T) {
	a, _ := agentrunners.Lookup("claude")
	bin, rec := fakeBin(t)
	b := newBox(t, bin)
	pid, err := agentrunners.Start(context.Background(), b, agentrunners.Job{Adapter: a, Key: "k", Secret: "s"})
	if err != nil {
		t.Fatal(err)
	}
	b.wait(pid)
	if got := lines(t, filepath.Join(rec, "session")); strings.Join(got, " ") != "--print --permission-mode default" {
		t.Fatalf("session argv %q", got)
	}
}

// TestStartRefuses: a box without the adapter's command is refused before
// anything is written or started, naming it; so are an empty key, a bad
// job key and a tool of two lines.
func TestStartRefuses(t *testing.T) {
	a, _ := agentrunners.Lookup("claude")
	empty := t.TempDir()
	b := newBox(t, empty)
	b.path = empty // no claude anywhere
	_, err := agentrunners.Start(context.Background(), b, agentrunners.Job{Adapter: a, Key: "k", Secret: "s"})
	if err == nil || !strings.Contains(err.Error(), "no claude on PATH") {
		t.Fatalf("err %v", err)
	}
	if _, serr := os.Stat(filepath.Join(b.home, ".potluck")); serr == nil || len(b.procs) > 0 {
		t.Fatal("something was written or started in a box that lacks claude")
	}
	bin, _ := fakeBin(t)
	for _, j := range []agentrunners.Job{
		{Adapter: a, Key: "k"},
		{Adapter: a, Key: "../k", Secret: "s"},
		{Adapter: a, Key: "k", Secret: "s", Options: agentrunners.Options{Allow: []string{"a\nb"}}},
		{Adapter: a, Key: "k", Secret: strings.Repeat("x", 64<<10)},
		{Adapter: a, Key: "k", Secret: "s", Options: agentrunners.Options{Mode: "bypassPermissions"}},
	} {
		if _, err := agentrunners.Start(context.Background(), newBox(t, bin), j); err == nil {
			t.Errorf("%+v started", j.Options)
		}
	}
}

// TestAdapters: every adapter offered passes its own check.
func TestAdapters(t *testing.T) {
	if len(agentrunners.Names()) == 0 {
		t.Fatal("no adapters")
	}
	for _, n := range agentrunners.Names() {
		a, ok := agentrunners.Lookup(n)
		if !ok {
			t.Fatalf("%s not found", n)
		}
		if err := a.Check(); err != nil {
			t.Error(err)
		}
	}
	if _, ok := agentrunners.Lookup("nope"); ok {
		t.Fatal("found an adapter that does not exist")
	}
}
