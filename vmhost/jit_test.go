package vmhost

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sengirunners/potluck/protocol"
)

// A box acquired with a jit_config starts the GitHub runner as its first
// command, the config reaching it on stdin only: never in a record, a log or
// argv. Deregistration is the authority's, so the driver calls no GitHub.
func TestBoxJITConfigStartsTheRunner(t *testing.T) {
	_, _, m := fakeBinding(t, "linux-img")
	delete(m, "image")
	dir := t.TempDir()
	runner := filepath.Join(dir, "actions-runner")
	if err := os.MkdirAll(runner, 0o700); err != nil {
		t.Fatal(err)
	}
	got := filepath.Join(dir, "started")
	// A runner stays up, listening for its job (release ends it).
	if err := os.WriteFile(filepath.Join(runner, "run.sh"), []byte(fmt.Sprintf("#!/bin/sh\necho \"$*\" >%q\nexec sleep 60\n", got)), 0o755); err != nil {
		t.Fatal(err)
	}
	m["target"], m["kind"], m["size"], m["cpu"], m["memory"] = "t", "box", "small", "1", "512"
	m["images"] = "fake/linux@sha256:" + strings.Repeat("ab", 32)
	const jit = "eyJzZWtyaXQiOiJKSVQtY29uZmlnLXZhbHVlIn0="
	if _, err := call(t, m, "acquire", "", map[string]any{"request_id": "k", "ttl_s": 60, "jit_config": jit}); code(err) != "bad_request" {
		t.Fatalf("jit_config on a binding without runner_dir: %v, want bad_request", err)
	}
	m["runner_dir"] = runner
	d, _ := call(t, m, "describe", "", nil)
	if !strings.Contains(fmt.Sprint(d["capabilities"]), "github-jit") {
		t.Errorf("describe capabilities %v, want github-jit", d["capabilities"])
	}
	for _, bad := range []string{"short", "has spaces in it, really", strings.Repeat("a", jitMax+1)} {
		if _, err := call(t, m, "acquire", "", map[string]any{"request_id": "b", "ttl_s": 60, "jit_config": bad}); code(err) != "bad_request" {
			t.Errorf("jit_config %.20q: %v, want bad_request", bad, err)
		}
	}
	a, err := call(t, m, "acquire", "", map[string]any{"request_id": "k", "ttl_s": 60, "jit_config": jit})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = call(t, m, "release", a["handle"].(string), nil) }()
	for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		if b, _ := os.ReadFile(got); strings.TrimSpace(string(b)) == "--jitconfig "+jit {
			break
		}
		if time.Now().After(deadline) {
			b, _ := os.ReadFile(got)
			t.Fatalf("the runner started with %q, want --jitconfig <the config>", b)
		}
	}
	_ = filepath.WalkDir(m["state_dir"], func(p string, de fs.DirEntry, err error) error {
		if err == nil && !de.IsDir() {
			if b, _ := os.ReadFile(p); strings.Contains(string(b), jit) {
				t.Errorf("the jit_config is in %s", p)
			}
		}
		return nil
	})
	if st, _ := call(t, m, "status", a["handle"].(string), nil); st["state"] != "ready" {
		t.Errorf("status %v, want ready", st)
	}
	if _, err := call(t, m, "acquire", "", map[string]any{"request_id": "k", "ttl_s": 60}); code(err) != "bad_request" {
		t.Errorf("the same request_id without its jit_config: %v, want bad_request", err)
	}
	// A runner that dies at start fails the acquire.
	if err := os.WriteFile(filepath.Join(runner, "run.sh"), []byte("#!/bin/sh\nexit 3\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := call(t, m, "acquire", "", map[string]any{"request_id": "dies", "ttl_s": 60, "jit_config": jit}); code(err) != "unavailable" || !strings.Contains(err.Error(), "exited 3") {
		t.Errorf("a runner that exits at start: %v, want unavailable naming exit 3", err)
	}
	if _, err := call(t, map[string]string{"target": "t", "image": "img", "remote": "/srv/r.git", "state_dir": t.TempDir(), "tart": "/nonexistent"},
		"acquire", "", map[string]any{"request_id": "r", "ttl_s": 60, "commit": strings.Repeat("a", 40), "script": "s", "jit_config": jit}); code(err) != "bad_request" {
		t.Errorf("jit_config on a run: %v, want bad_request", err)
	}
}

// zm sends a box's image by name (SPEC 20); the driver boots the pin.
func TestBoxImageByName(t *testing.T) {
	c := config{Images: []string{"ghcr.io/o/a@sha256:" + strings.Repeat("ab", 32), "ghcr.io/o/b@sha256:" + strings.Repeat("cd", 32)}}
	for name, want := range map[string]string{"ghcr.io/o/b": c.Images[1], c.Images[0]: c.Images[0], "ghcr.io/o/c": "", "ghcr.io/o": ""} {
		if got, ok := c.pinned(name); got != want || ok != (want != "") {
			t.Errorf("pinned(%q) = %q %v, want %q", name, got, ok, want)
		}
	}
}

// fakeRunnerSh models GitHub's runner for the job-started hook: it waits for
// a job (a file of GITHUB_* values), runs the hook as the runner runs a .js
// one (its node, waiting for it), then the job's steps: an if: always() step
// runs whatever the hook answered, since a hook's non-zero exit only fails
// the job. Exiting 1 is not enough: a refusing hook must end the runner.
func fakeRunnerSh(node, out, errs, pidf, job string) string {
	return fakeRunnerShPath(node, out, errs, pidf, job, "")
}

// fakeRunnerShPath is fakeRunnerSh running the hook with PATH hookPath (set),
// and writing the worker's pid to pidf.worker.
func fakeRunnerShPath(node, out, errs, pidf, job, hookPath string) string {
	run := fmt.Sprintf("%q", node)
	if hookPath != "" {
		run = fmt.Sprintf("PATH=%q %q", hookPath, node)
	}
	return fmt.Sprintf(`#!/bin/sh
echo $$ >%[4]q
h="$ACTIONS_RUNNER_HOOK_JOB_STARTED"
case "$h" in "$PWD"/*) echo "hook in the runner's directory" >>%[2]q; exec sleep 60 ;; /*.js) ;; *) echo "no hook" >>%[2]q; exec sleep 60 ;; esac
while [ ! -f %[5]q ]; do sleep 0.05; done
. %[5]q
(
	sh -c 'echo $PPID' >%[4]q.worker
	%[1]s "$h" 2>>%[3]q; echo "hook $?" >>%[2]q
	echo "always() step ran" >>%[2]q
)
exec sleep 60
`, run, out, errs, pidf, job)
}

// runJob gives the fake runner (pid) its job and answers what it wrote once
// the job ran or the hook stopped it.
func runJob(t *testing.T, name, env, out, errs, job string, pid int) (string, bool) {
	t.Helper()
	read := func(p string) string { b, _ := os.ReadFile(p); return string(b) }
	if err := os.WriteFile(job, []byte("unset GITHUB_RUN_ID GITHUB_WORKFLOW_REF GITHUB_REF GITHUB_REPOSITORY GITHUB_EVENT_NAME GITHUB_EVENT_PATH; export "+env+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(10 * time.Second); !strings.Contains(read(out), "always() step ran") &&
		!(strings.Contains(read(errs), "zm:") && syscall.Kill(pid, 0) != nil); time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("%s: no step ran and the runner lives on: out %q, hook stderr %q", name, read(out), read(errs))
		}
	}
	time.Sleep(200 * time.Millisecond) // a step the hook let through would show by now
	return read(out), syscall.Kill(pid, 0) == nil
}

// The job-started hook jitLaunch installs applies the broker's rules as the
// box can check them (the job's repository, its run's event, a pull request's
// head repository from the event payload): a job they refuse is stopped
// before any step, the runner killed; a job they serve runs, whatever its run.
func TestJITHookStopsWhatTheBrokerRefuses(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("no node here (GitHub's runner brings its own for a .js hook)")
	}
	broker := "push pull_request workflow_dispatch schedule merge_group release repository_dispatch"
	payload := func(dir, body string) string {
		p := filepath.Join(dir, "event.json")
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return " GITHUB_EVENT_PATH=" + p
	}
	pr := func(head string, fork bool) string {
		return fmt.Sprintf(`{"pull_request":{"title":"\"fork\": false","head":{"repo":{"full_name":%q,"fork":%v}}},"repository":{"full_name":"Acme/App"}}`, head, fork)
	}
	for _, c := range []struct {
		name, events, env, payload string
		served                     bool
	}{
		{"a pull_request_target run", broker, "GITHUB_EVENT_NAME=pull_request_target GITHUB_REPOSITORY=Acme/App", pr("Acme/App", false), false},
		{"a pull request from a fork", broker, "GITHUB_EVENT_NAME=pull_request GITHUB_REPOSITORY=Acme/App", pr("stranger/app", true), false},
		// A repository that is itself a fork (alice/widget): its own head
		// repository says fork true on every run, and it is served.
		{"a pull request from a repository that is itself a fork, its own branch", broker, "GITHUB_EVENT_NAME=pull_request GITHUB_REPOSITORY=Acme/App", pr("Acme/App", true), true},
		{"a push in a repository that is itself a fork", broker, "GITHUB_EVENT_NAME=push GITHUB_REPOSITORY=Acme/App", `{"repository":{"full_name":"Acme/App","fork":true}}`, true},
		{"a manual run in a repository that is itself a fork", broker, "GITHUB_EVENT_NAME=workflow_dispatch GITHUB_REPOSITORY=Acme/App", `{"repository":{"full_name":"Acme/App","fork":true}}`, true},
		{"a schedule in a repository that is itself a fork", broker, "GITHUB_EVENT_NAME=schedule GITHUB_REPOSITORY=Acme/App", `{"repository":{"full_name":"Acme/App","fork":true}}`, true},
		{"a pull request into a fork from a fork of it", broker, "GITHUB_EVENT_NAME=pull_request GITHUB_REPOSITORY=Acme/App", pr("stranger/app", true), false},
		{"a pull request into a fork from its upstream (not a fork)", broker, "GITHUB_EVENT_NAME=pull_request GITHUB_REPOSITORY=Acme/App", pr("Upstream/App", false), false},
		{"a pull request from another repository that is not a fork", broker, "GITHUB_EVENT_NAME=pull_request GITHUB_REPOSITORY=Acme/App", pr("other/app", false), false},
		{"a pull_request_target run from the repository's own branch", broker, "GITHUB_EVENT_NAME=pull_request_target GITHUB_REPOSITORY=Acme/App", pr("Acme/App", true), false},
		{"a pull request whose head repository has no name", broker, "GITHUB_EVENT_NAME=pull_request GITHUB_REPOSITORY=Acme/App", `{"pull_request":{"head":{"repo":{"fork":false}}}}`, false},
		{"a pull request whose fork is gone", broker, "GITHUB_EVENT_NAME=pull_request GITHUB_REPOSITORY=Acme/App", `{"pull_request":{"head":{"repo":null}}}`, false},
		{"a pull request with no payload", broker, "GITHUB_EVENT_NAME=pull_request GITHUB_REPOSITORY=Acme/App", "", false},
		{"an issue_comment run", broker, "GITHUB_EVENT_NAME=issue_comment GITHUB_REPOSITORY=Acme/App", "{}", false},
		{"a run with no event name", broker, "GITHUB_REPOSITORY=Acme/App", "{}", false},
		{"another repository's push", broker, "GITHUB_EVENT_NAME=push GITHUB_REPOSITORY=Acme/Other", "{}", false},
		{"another repository's job, any event allowed", "", "GITHUB_EVENT_NAME=push GITHUB_REPOSITORY=Acme/Other", "{}", false},
		{"a push of another run", broker, "GITHUB_EVENT_NAME=push GITHUB_RUN_ID=999 GITHUB_REPOSITORY=Acme/App", "{}", true},
		{"a pull request from the repository itself", broker, "GITHUB_EVENT_NAME=pull_request GITHUB_REPOSITORY=acme/app", pr("ACME/app", false), true},
		{"a pull_request_target run, any event allowed (allow_fork_prs)", "", "GITHUB_EVENT_NAME=pull_request_target GITHUB_REPOSITORY=Acme/App", pr("stranger/app", true), true},
	} {
		dir, _ := filepath.EvalSymlinks(t.TempDir())
		runner := filepath.Join(dir, "actions-runner")
		for _, d := range []string{runner, filepath.Join(dir, "tmp")} {
			if err := os.MkdirAll(d, 0o700); err != nil {
				t.Fatal(err)
			}
		}
		out, errs, pidf, job := filepath.Join(dir, "out"), filepath.Join(dir, "hook-stderr"), filepath.Join(dir, "pid"), filepath.Join(dir, "job.env")
		if err := os.WriteFile(filepath.Join(runner, "run.sh"), []byte(fakeRunnerSh(node, out, errs, pidf, job)), 0o755); err != nil {
			t.Fatal(err)
		}
		launch := exec.Command("sh", "-c", jitLaunch, "sh", runner, "acme/app", c.events, "")
		launch.Stdin = strings.NewReader("eyJzZWtyaXQiOiJKSVQtY29uZmlnLXZhbHVlIn0=\n")
		launch.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + dir, "TMPDIR=" + filepath.Join(dir, "tmp")}
		launch.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err := launch.Start(); err != nil {
			t.Fatal(err)
		}
		pid := launch.Process.Pid
		go func() { _ = launch.Wait() }()
		env := c.env
		if c.payload != "" {
			env += payload(dir, c.payload)
		}
		got, alive := runJob(t, c.name, env, out, errs, job, pid)
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		switch {
		case c.served && got != "hook 0\nalways() step ran\n":
			t.Errorf("%s: the runner wrote %q, want the hook to pass and the steps to run", c.name, got)
		case !c.served && (got != "" || alive):
			t.Errorf("%s: the runner wrote %q (alive %t), want it killed by the hook before any step", c.name, got, alive)
		}
	}
}

// An open guard (jit_guard.open_if_private, $11 "private") serves a job its
// events refuse only while the job's own event payload says the repository
// is private (private true, visibility private): a fork's job on a runner
// minted while the repository was private is stopped before its first step
// once the repository is public or internal, whatever the broker knows.
func TestJITHookOpenIfPrivateChecksThePayload(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("no node here (GitHub's runner brings its own for a .js hook)")
	}
	broker := "push pull_request workflow_dispatch schedule merge_group release repository_dispatch"
	fork := func(repo string) string {
		return `{"pull_request":{"head":{"repo":{"full_name":"stranger/app","fork":true}}},"repository":` + repo + `}`
	}
	const prEnv, targetEnv = "GITHUB_EVENT_NAME=pull_request GITHUB_REPOSITORY=Acme/App", "GITHUB_EVENT_NAME=pull_request_target GITHUB_REPOSITORY=Acme/App"
	for _, c := range []struct {
		name, open, env, payload string
		served                   bool
	}{
		{"a fork's pull request, private", "private", prEnv, fork(`{"full_name":"Acme/App","private":true,"visibility":"private"}`), true},
		{"a pull_request_target run, private", "private", targetEnv, fork(`{"full_name":"Acme/App","private":true,"visibility":"private"}`), true},
		{"a fork's pull request, made public", "private", prEnv, fork(`{"full_name":"Acme/App","private":false,"visibility":"public"}`), false},
		{"a fork's pull request, made internal", "private", prEnv, fork(`{"full_name":"Acme/App","private":true,"visibility":"internal"}`), false},
		{"a fork's pull request, visibility unsaid", "private", prEnv, fork(`{"full_name":"Acme/App","private":true}`), false},
		{"a fork's pull request, no repository in the payload", "private", prEnv, `{"pull_request":{"head":{"repo":{"full_name":"stranger/app"}}}}`, false},
		{"a fork's pull request, no payload", "private", prEnv, "", false},
		{"a fork's pull request, private, guard not open", "", prEnv, fork(`{"full_name":"Acme/App","private":true,"visibility":"private"}`), false},
		{"an own push, made public", "private", "GITHUB_EVENT_NAME=push GITHUB_REPOSITORY=Acme/App", `{"repository":{"full_name":"Acme/App","private":false,"visibility":"public"}}`, true},
		{"another repository's private job", "private", "GITHUB_EVENT_NAME=pull_request GITHUB_REPOSITORY=Acme/Other", fork(`{"full_name":"Acme/Other","private":true,"visibility":"private"}`), false},
	} {
		dir, _ := filepath.EvalSymlinks(t.TempDir())
		runner := filepath.Join(dir, "actions-runner")
		for _, d := range []string{runner, filepath.Join(dir, "tmp")} {
			if err := os.MkdirAll(d, 0o700); err != nil {
				t.Fatal(err)
			}
		}
		out, errs, pidf, job := filepath.Join(dir, "out"), filepath.Join(dir, "hook-stderr"), filepath.Join(dir, "pid"), filepath.Join(dir, "job.env")
		if err := os.WriteFile(filepath.Join(runner, "run.sh"), []byte(fakeRunnerSh(node, out, errs, pidf, job)), 0o755); err != nil {
			t.Fatal(err)
		}
		launch := exec.Command("sh", "-c", jitLaunch, "sh", runner, "acme/app", broker, "", "", "", "", "", "", "", c.open)
		launch.Stdin = strings.NewReader("eyJzZWtyaXQiOiJKSVQtY29uZmlnLXZhbHVlIn0=\n")
		launch.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + dir, "TMPDIR=" + filepath.Join(dir, "tmp")}
		launch.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err := launch.Start(); err != nil {
			t.Fatal(err)
		}
		pid := launch.Process.Pid
		go func() { _ = launch.Wait() }()
		env := c.env
		if c.payload != "" {
			p := filepath.Join(dir, "event.json")
			if err := os.WriteFile(p, []byte(c.payload), 0o600); err != nil {
				t.Fatal(err)
			}
			env += " GITHUB_EVENT_PATH=" + p
		}
		got, alive := runJob(t, c.name, env, out, errs, job, pid)
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		switch {
		case c.served && got != "hook 0\nalways() step ran\n":
			t.Errorf("%s: the runner wrote %q, want the hook to pass and the steps to run", c.name, got)
		case !c.served && (got != "" || alive):
			t.Errorf("%s: the runner wrote %q (alive %t), want it killed by the hook before any step", c.name, got, alive)
		}
	}
}

// Where the image has no ps, or a BusyBox ps that takes no -p, the hook
// still stops a refused job: it kills its parent (Runner.Worker) first and
// walks /proc for the rest. A launch that cannot find its process group at
// all (no /proc, no ps -p) starts no runner.
func TestJITHookStopsTheWorkerWithoutPs(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("no node here (GitHub's runner brings its own for a .js hook)")
	}
	read := func(p string) string { b, _ := os.ReadFile(p); return string(b) }
	busybox := t.TempDir()
	if err := os.WriteFile(filepath.Join(busybox, "ps"), []byte("#!/bin/sh\necho \"ps: unrecognized option: p\" >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	setup := func() (dir, runner string) {
		dir, _ = filepath.EvalSymlinks(t.TempDir())
		runner = filepath.Join(dir, "actions-runner")
		for _, d := range []string{runner, filepath.Join(dir, "tmp")} {
			if err := os.MkdirAll(d, 0o700); err != nil {
				t.Fatal(err)
			}
		}
		return dir, runner
	}
	start := func(dir, runner, path string) *exec.Cmd {
		launch := exec.Command("sh", "-c", jitLaunch, "sh", runner, "acme/app", "push pull_request", "")
		launch.Stdin = strings.NewReader("eyJzZWtyaXQiOiJKSVQtY29uZmlnLXZhbHVlIn0=\n")
		launch.Env = []string{"PATH=" + path, "HOME=" + dir, "TMPDIR=" + filepath.Join(dir, "tmp")}
		launch.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		return launch
	}
	for name, hookPath := range map[string]string{"no ps": t.TempDir(), "a BusyBox ps": busybox} {
		dir, runner := setup()
		out, errs, pidf, job := filepath.Join(dir, "out"), filepath.Join(dir, "hook-stderr"), filepath.Join(dir, "pid"), filepath.Join(dir, "job.env")
		if err := os.WriteFile(filepath.Join(runner, "run.sh"), []byte(fakeRunnerShPath(node, out, errs, pidf, job, hookPath)), 0o755); err != nil {
			t.Fatal(err)
		}
		launch := start(dir, runner, os.Getenv("PATH"))
		if err := launch.Start(); err != nil {
			t.Fatal(err)
		}
		go func() { _ = launch.Wait() }()
		if err := os.WriteFile(job, []byte("export GITHUB_EVENT_NAME=pull_request_target GITHUB_REPOSITORY=acme/app\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		worker := func() int { w, _ := strconv.Atoi(strings.TrimSpace(read(pidf + ".worker"))); return w }
		for deadline := time.Now().Add(10 * time.Second); !strings.Contains(read(out), "always() step ran") &&
			!(strings.Contains(read(errs), "zm:") && worker() > 1 && syscall.Kill(worker(), 0) != nil); time.Sleep(20 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("%s: out %q, hook stderr %q", name, read(out), read(errs))
			}
		}
		time.Sleep(200 * time.Millisecond)
		if got := read(out); got != "" || syscall.Kill(worker(), 0) == nil {
			t.Errorf("%s: the runner wrote %q (worker alive %t), want the worker killed before any step", name, got, syscall.Kill(worker(), 0) == nil)
		}
		_ = syscall.Kill(-launch.Process.Pid, syscall.SIGKILL)
	}
	// The launch itself with no ps, then a BusyBox ps: it needs /proc or ps -p to find its group.
	tools := t.TempDir()
	for _, n := range []string{"grep", "mktemp", "cat", "sleep", "sh", "tr", "chmod"} {
		if p, err := exec.LookPath(n); err == nil {
			_ = os.Symlink(p, filepath.Join(tools, n))
		}
	}
	_, procErr := os.Stat("/proc/self/stat")
	for name, path := range map[string]string{"no ps": tools, "a BusyBox ps": busybox + ":" + tools} {
		dir, runner := setup()
		pidf := filepath.Join(dir, "pid")
		if err := os.WriteFile(filepath.Join(runner, "run.sh"), []byte(fmt.Sprintf("#!/bin/sh\necho $$ >%q\nexec sleep 60\n", pidf)), 0o755); err != nil {
			t.Fatal(err)
		}
		launch := start(dir, runner, path)
		var stderr strings.Builder
		launch.Stderr = &stderr
		if procErr != nil { // macOS: no /proc
			err := launch.Run()
			var ee *exec.ExitError
			if !errors.As(err, &ee) || ee.ExitCode() != 125 || !strings.Contains(stderr.String(), "cannot find the runner's process group") || read(pidf) != "" {
				t.Errorf("a launch with %s and no /proc: %v %q (runner pid %q), want exit 125 and no runner", name, err, stderr.String(), read(pidf))
			}
			continue
		}
		if err := launch.Start(); err != nil { // Linux: /proc gives the group
			t.Fatal(err)
		}
		go func() { _ = launch.Wait() }()
		eventually := time.Now().Add(5 * time.Second)
		for read(pidf) == "" && time.Now().Before(eventually) {
			time.Sleep(20 * time.Millisecond)
		}
		if read(pidf) == "" {
			t.Errorf("a launch with %s and /proc: no runner (%q)", name, stderr.String())
		}
		_ = syscall.Kill(-launch.Process.Pid, syscall.SIGKILL)
	}
}

// A box acquired with a jit_guard starts its runner with the hook: through
// the driver, a refused job is stopped. Only a VM of its own is powered off.
func TestBoxJITGuardReachesTheRunner(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("no node here (GitHub's runner brings its own for a .js hook)")
	}
	_, _, m := fakeBinding(t, "linux-img")
	delete(m, "image")
	dir := t.TempDir()
	runner := filepath.Join(dir, "actions-runner")
	if err := os.MkdirAll(runner, 0o700); err != nil {
		t.Fatal(err)
	}
	out, errs, pidf, job := filepath.Join(dir, "out"), filepath.Join(dir, "hook-stderr"), filepath.Join(dir, "pid"), filepath.Join(dir, "job.env")
	if err := os.WriteFile(filepath.Join(runner, "run.sh"), []byte(fakeRunnerSh(node, out, errs, pidf, job)), 0o755); err != nil {
		t.Fatal(err)
	}
	m["target"], m["kind"], m["size"], m["cpu"], m["memory"] = "t", "box", "small", "1", "512"
	m["images"], m["runner_dir"] = "fake/linux@sha256:"+strings.Repeat("ab", 32), runner
	const jit = "eyJzZWtyaXQiOiJKSVQtY29uZmlnLXZhbHVlIn0="
	if d, _ := call(t, m, "describe", "", nil); !strings.Contains(fmt.Sprint(d["capabilities"]), "github-jit-guard") {
		t.Errorf("describe capabilities %v, want github-jit-guard", d["capabilities"])
	}
	guard := map[string]any{"repo": "Acme/App", "events": []string{"push", "pull_request"}}
	for _, bad := range []map[string]any{
		{"request_id": "b1", "ttl_s": 60, "jit_guard": guard}, // no jit_config
		{"request_id": "b2", "ttl_s": 60, "jit_config": jit, "jit_guard": map[string]any{"repo": "acme/app'; x"}},
		{"request_id": "b3", "ttl_s": 60, "jit_config": jit, "jit_guard": map[string]any{"repo": "acme/app", "events": []string{"push'"}}},
		{"request_id": "b4", "ttl_s": 60, "jit_config": jit, "jit_guard": map[string]any{"repo": "acme/app", "open_if_private": true}}, // widens no events
	} {
		if _, err := call(t, m, "acquire", "", bad); code(err) != "bad_request" {
			t.Errorf("acquire %v: %v, want bad_request", bad["jit_guard"], err)
		}
	}
	a, err := call(t, m, "acquire", "", map[string]any{"request_id": "k", "ttl_s": 60, "jit_config": jit, "jit_guard": guard})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = call(t, m, "release", a["handle"].(string), nil) }()
	b, _ := os.ReadFile(pidf)
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	if pid <= 1 {
		t.Fatal("the runner wrote no pid")
	}
	if got, alive := runJob(t, "a pull_request_target run", "GITHUB_EVENT_NAME=pull_request_target GITHUB_REPOSITORY=acme/app", out, errs, job, pid); got != "" || alive {
		t.Errorf("a pull_request_target run on a guarded box: the runner wrote %q (alive %t), want it killed before any step", got, alive)
	}
	if _, err := call(t, m, "acquire", "", map[string]any{"request_id": "k", "ttl_s": 60, "jit_config": jit,
		"jit_guard": map[string]any{"repo": "acme/app"}}); code(err) != "bad_request" {
		t.Errorf("the same request_id with another guard: %v, want bad_request", err)
	}
	// An image whose runner .env names its own hook would replace ours: no box.
	if err := os.WriteFile(filepath.Join(runner, ".env"), []byte("ACTIONS_RUNNER_HOOK_JOB_STARTED=/opt/hook.sh\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := call(t, m, "acquire", "", map[string]any{"request_id": "env", "ttl_s": 60, "jit_config": jit, "jit_guard": guard}); code(err) != "unavailable" {
		t.Errorf("a runner .env with its own hook: %v, want unavailable", err)
	}
	for vm, want := range map[string]string{"tart": "off", "incus": "off", "oci": "", "fake": ""} {
		if got := jitStop(config{VM: vm}); got != want {
			t.Errorf("jitStop(%s) = %q, want %q", vm, got, want)
		}
	}
}

// A box acquired with jit_guard.open_if_private starts its runner with the
// hook widened: through the driver, a fork's job of a repository its payload
// says is private runs; the same request_id without it is another guard.
func TestBoxJITGuardOpenIfPrivateReachesTheRunner(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("no node here (GitHub's runner brings its own for a .js hook)")
	}
	_, _, m := fakeBinding(t, "linux-img")
	delete(m, "image")
	dir := t.TempDir()
	runner := filepath.Join(dir, "actions-runner")
	if err := os.MkdirAll(runner, 0o700); err != nil {
		t.Fatal(err)
	}
	out, errs, pidf, job := filepath.Join(dir, "out"), filepath.Join(dir, "hook-stderr"), filepath.Join(dir, "pid"), filepath.Join(dir, "job.env")
	if err := os.WriteFile(filepath.Join(runner, "run.sh"), []byte(fakeRunnerSh(node, out, errs, pidf, job)), 0o755); err != nil {
		t.Fatal(err)
	}
	m["target"], m["kind"], m["size"], m["cpu"], m["memory"] = "t", "box", "small", "1", "512"
	m["images"], m["runner_dir"] = "fake/linux@sha256:"+strings.Repeat("ab", 32), runner
	const jit = "eyJzZWtyaXQiOiJKSVQtY29uZmlnLXZhbHVlIn0="
	if d, _ := call(t, m, "describe", "", nil); !strings.Contains(fmt.Sprint(d["capabilities"]), protocol.CapJITGuardPrivate) {
		t.Errorf("describe capabilities %v, want %s", d["capabilities"], protocol.CapJITGuardPrivate)
	}
	guard := map[string]any{"repo": "Acme/App", "events": []string{"push", "pull_request"}, "open_if_private": true}
	a, err := call(t, m, "acquire", "", map[string]any{"request_id": "k", "ttl_s": 60, "jit_config": jit, "jit_guard": guard})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = call(t, m, "release", a["handle"].(string), nil) }()
	b, _ := os.ReadFile(pidf)
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	if pid <= 1 {
		t.Fatal("the runner wrote no pid")
	}
	ev := filepath.Join(dir, "event.json")
	if err := os.WriteFile(ev, []byte(`{"pull_request":{"head":{"repo":{"full_name":"stranger/app"}}},"repository":{"full_name":"Acme/App","private":true,"visibility":"private"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, _ := runJob(t, "a fork's pull request, private", "GITHUB_EVENT_NAME=pull_request GITHUB_REPOSITORY=acme/app GITHUB_EVENT_PATH="+ev, out, errs, job, pid); got != "hook 0\nalways() step ran\n" {
		t.Errorf("a fork's pull request of a private repository on an open box: the runner wrote %q, want its steps run", got)
	}
	if _, err := call(t, m, "acquire", "", map[string]any{"request_id": "k", "ttl_s": 60, "jit_config": jit,
		"jit_guard": map[string]any{"repo": "acme/app", "events": []string{"push", "pull_request"}}}); code(err) != "bad_request" {
		t.Errorf("the same request_id without open_if_private: %v, want bad_request", err)
	}
}

// fakeRunnerShEnv is fakeRunnerSh that first writes the secrets directory
// variables it was started with (POTLUCK_SECRETS, ZM_SECRETS) to envf.
func fakeRunnerShEnv(node, out, errs, pidf, job, envf string) string {
	return strings.Replace(fakeRunnerSh(node, out, errs, pidf, job), "#!/bin/sh\n",
		fmt.Sprintf("#!/bin/sh\necho \"${POTLUCK_SECRETS-unset} ${ZM_SECRETS-unset}\" >%q\n", envf), 1)
}

// A runner held to one run (jit_guard.run) serves only that run's job, of
// its workflow at its branch, and holds it before its first step until the
// door's ready marker holds the run's id: a marker of another run, or none
// within the hold, stops the job as a refused one is stopped. A job of
// another run that the strict guard serves otherwise is held for a re-guard
// marker naming its own run, workflow and ref (protocol.ReguardMarker), and
// passes on that alone; anything else stops it, killed before any step (its
// if: always() step included), and an open guard never holds one.
func TestJITHookHoldsARunForItsSecrets(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("no node here (GitHub's runner brings its own for a .js hook)")
	}
	read := func(p string) string { b, _ := os.ReadFile(p); return string(b) }
	const (
		wf    = "acme/app/.github/workflows/ci.yml@refs/heads/main"
		ours  = "GITHUB_EVENT_NAME=push GITHUB_REPOSITORY=Acme/App GITHUB_RUN_ID=4242 GITHUB_REF=refs/heads/main GITHUB_WORKFLOW_REF="
		other = "GITHUB_EVENT_NAME=push GITHUB_REPOSITORY=Acme/App GITHUB_RUN_ID=4243 GITHUB_REF=refs/heads/main GITHUB_WORKFLOW_REF="
		rel   = "acme/app/.github/workflows/release.yml@refs/heads/main"
	)
	for _, c := range []struct {
		name, env, marker string
		after             time.Duration // the marker is written this long after the job starts
		hold              string
		served, door      bool
		over              bool // held for the broker's re-guard (a job of another run)
		private           bool // ${11} "private": an open guard
	}{
		{"the run, its marker there", ours + wf, "4242\n", 0, "5", true, false, false, false},
		{"the run, its marker coming during the hold", ours + wf, "4242", 700 * time.Millisecond, "5", true, false, false, false},
		{"the run, its workflow ref in GitHub's case", ours + "Acme/App/.github/workflows/ci.yml@refs/heads/main", "4242", 0, "5", true, false, false, false},
		{"another run, the box's run's marker", other + wf, "4242", 0, "1", false, false, true, false},
		{"another run, its re-guard marker there", other + rel, "4243 " + rel + " refs/heads/main\n", 0, "5", true, false, false, false},
		{"another run, its re-guard marker coming during the hold", other + rel, "4243 " + rel + " refs/heads/main", 700 * time.Millisecond, "5", true, false, false, false},
		{"another run, its workflow ref in GitHub's case", other + "Acme/App/.github/workflows/release.yml@refs/heads/main", "4243 " + rel + " refs/heads/main", 0, "5", true, false, false, false},
		{"another run of the box's workflow, its re-guard marker", other + wf, "4243 " + wf + " refs/heads/main", 0, "5", true, false, false, false},
		{"another run, the plain marker of its run", other + rel, "4243", 0, "1", false, false, true, false},
		{"another run, a re-guard marker naming another workflow", other + rel, "4243 " + wf + " refs/heads/main", 0, "1", false, false, true, false},
		{"another run, a re-guard marker naming another ref", other + rel, "4243 " + rel + " refs/heads/dev", 0, "1", false, false, true, false},
		{"another run, a re-guard marker naming another run", other + rel, "4244 " + rel + " refs/heads/main", 0, "1", false, false, true, false},
		{"another run, a re-guard marker naming its run within more", other + rel, "4243 " + rel + " refs/heads/main x", 0, "1", false, false, true, false},
		{"another run, no marker in the hold", other + rel, "", 0, "1", false, false, true, false},
		{"another run on an open guard, its re-guard marker there", other + rel, "4243 " + rel + " refs/heads/main", 0, "5", false, false, false, true},
		{"another run of an event the guard refuses, its re-guard marker there", "GITHUB_EVENT_NAME=workflow_dispatch GITHUB_REPOSITORY=Acme/App GITHUB_RUN_ID=4243 GITHUB_REF=refs/heads/main GITHUB_WORKFLOW_REF=" + rel, "4243 " + rel + " refs/heads/main", 0, "5", false, false, false, false},
		{"another run of another repository, its re-guard marker there", "GITHUB_EVENT_NAME=push GITHUB_REPOSITORY=Acme/Other GITHUB_RUN_ID=4243 GITHUB_REF=refs/heads/main GITHUB_WORKFLOW_REF=acme/other/.github/workflows/release.yml@refs/heads/main", "4243 acme/other/.github/workflows/release.yml@refs/heads/main refs/heads/main", 0, "5", false, false, false, false},
		{"another run of a tag, its re-guard marker there", "GITHUB_EVENT_NAME=push GITHUB_REPOSITORY=Acme/App GITHUB_RUN_ID=4243 GITHUB_REF=refs/tags/main GITHUB_WORKFLOW_REF=acme/app/.github/workflows/release.yml@refs/tags/main", "4243 acme/app/.github/workflows/release.yml@refs/tags/main refs/tags/main", 0, "1", false, false, false, false},
		{"the run, a re-guard marker naming it", ours + wf, "4242 " + wf + " refs/heads/main", 0, "1", false, true, false, false},
		{"another workflow of the run's repository", ours + "acme/app/.github/workflows/release.yml@refs/heads/main", "4242", 0, "5", false, false, false, false},
		{"no workflow ref", "GITHUB_EVENT_NAME=push GITHUB_REPOSITORY=Acme/App GITHUB_RUN_ID=4242 GITHUB_REF=refs/heads/main", "4242", 0, "5", false, false, false, false},
		{"a tag of the branch's name", "GITHUB_EVENT_NAME=push GITHUB_REPOSITORY=Acme/App GITHUB_RUN_ID=4242 GITHUB_REF=refs/tags/main GITHUB_WORKFLOW_REF=" + wf, "4242", 0, "5", false, false, false, false},
		{"another branch", "GITHUB_EVENT_NAME=push GITHUB_REPOSITORY=Acme/App GITHUB_RUN_ID=4242 GITHUB_REF=refs/heads/dev GITHUB_WORKFLOW_REF=" + wf, "4242", 0, "5", false, false, false, false},
		{"the run of another repository", "GITHUB_EVENT_NAME=push GITHUB_REPOSITORY=Acme/Other GITHUB_RUN_ID=4242 GITHUB_REF=refs/heads/main GITHUB_WORKFLOW_REF=" + wf, "4242", 0, "5", false, false, false, false},
		{"the run, a marker of another run", ours + wf, "4243", 0, "1", false, true, false, false},
		{"the run, a marker naming the run within more", ours + wf, "42424", 0, "1", false, true, false, false},
		{"the run, no marker in the hold", ours + wf, "", 0, "1", false, true, false, false},
	} {
		dir, _ := filepath.EvalSymlinks(t.TempDir())
		runner, secrets := filepath.Join(dir, "actions-runner"), filepath.Join(dir, "secrets")
		for _, d := range []string{runner, secrets, filepath.Join(dir, "tmp")} {
			if err := os.MkdirAll(d, 0o700); err != nil {
				t.Fatal(err)
			}
		}
		out, errs, pidf, job, envf := filepath.Join(dir, "out"), filepath.Join(dir, "hook-stderr"), filepath.Join(dir, "pid"), filepath.Join(dir, "job.env"), filepath.Join(dir, "env")
		if err := os.WriteFile(filepath.Join(runner, "run.sh"), []byte(fakeRunnerShEnv(node, out, errs, pidf, job, envf)), 0o755); err != nil {
			t.Fatal(err)
		}
		marker := filepath.Join(secrets, protocol.SecretNameReady)
		if c.marker != "" && c.after == 0 {
			if err := os.WriteFile(marker, []byte(c.marker), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		private := ""
		if c.private {
			private = "private"
		}
		launch := exec.Command("sh", "-c", jitLaunch, "sh", runner, "acme/app", "push", "", "", "4242", wf, "refs/heads/main", c.hold, secrets, private)
		launch.Stdin = strings.NewReader("eyJzZWtyaXQiOiJKSVQtY29uZmlnLXZhbHVlIn0=\n")
		launch.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + dir, "TMPDIR=" + filepath.Join(dir, "tmp")}
		launch.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err := launch.Start(); err != nil {
			t.Fatal(err)
		}
		pid := launch.Process.Pid
		go func() { _ = launch.Wait() }()
		if c.after > 0 {
			go func() {
				for read(job) == "" { // runJob writes it: the job starts
					time.Sleep(10 * time.Millisecond)
				}
				time.Sleep(c.after)
				_ = os.WriteFile(marker, []byte(c.marker), 0o600)
			}()
		}
		got, alive := runJob(t, c.name, c.env, out, errs, job, pid)
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		switch {
		case c.served && got != "hook 0\nalways() step ran\n":
			t.Errorf("%s: the runner wrote %q (hook stderr %q), want the hook to pass and the steps to run", c.name, got, read(errs))
		case !c.served && (got != "" || alive):
			t.Errorf("%s: the runner wrote %q (alive %t), want it killed by the hook before any step", c.name, got, alive)
		case c.over && !strings.Contains(read(errs), "did not take this box over for in "+c.hold+"s"):
			t.Errorf("%s: hook stderr %q, want it held for the broker's re-guard for the hold", c.name, read(errs))
		case !c.over && strings.Contains(read(errs), "take this box over"):
			t.Errorf("%s: hook stderr %q, want it not held for a re-guard", c.name, read(errs))
		case c.door && !strings.Contains(read(errs), "Potluck secrets door, which did not deliver them in "+c.hold+"s"):
			t.Errorf("%s: hook stderr %q, want it to name the door and the hold", c.name, read(errs))
		case !c.served && !c.door && strings.Contains(read(errs), "secrets door"):
			t.Errorf("%s: hook stderr %q, want the job refused by the guard, not held", c.name, read(errs))
		}
		if e := strings.TrimSpace(read(envf)); e != secrets+" "+secrets {
			t.Errorf("%s: run.sh got POTLUCK_SECRETS and ZM_SECRETS %q, want %s for both", c.name, e, secrets)
		}
	}
	// No run: the runner gets no secrets directory, and a job is not held.
	dir, _ := filepath.EvalSymlinks(t.TempDir())
	runner := filepath.Join(dir, "actions-runner")
	if err := os.MkdirAll(runner, 0o700); err != nil {
		t.Fatal(err)
	}
	envf := filepath.Join(dir, "env")
	if err := os.WriteFile(filepath.Join(runner, "run.sh"), []byte(fmt.Sprintf("#!/bin/sh\necho \"${POTLUCK_SECRETS-unset} ${ZM_SECRETS-unset}\" >%q\n", envf)), 0o755); err != nil {
		t.Fatal(err)
	}
	launch := exec.Command("sh", "-c", jitLaunch, "sh", runner, "acme/app", "push", "", "", "", "", "", "", "")
	launch.Stdin = strings.NewReader("eyJzZWtyaXQiOiJKSVQtY29uZmlnLXZhbHVlIn0=\n")
	launch.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + dir, "TMPDIR=" + dir}
	if err := launch.Run(); err != nil {
		t.Fatal(err)
	}
	if e := strings.TrimSpace(read(envf)); e != "unset unset" {
		t.Errorf("a runner held to no run got POTLUCK_SECRETS and ZM_SECRETS %q, want neither", e)
	}
	// A run without its guard or its directory starts no runner.
	for _, args := range [][]string{{"", "", "", "", "4242", "x", "y", "1", dir}, {"acme/app", "", "", "", "4242", "x", "y", "1", ""}} {
		_ = os.Remove(envf)
		launch := exec.Command("sh", append([]string{"-c", jitLaunch, "sh", runner}, args...)...)
		launch.Stdin = strings.NewReader("eyJzZWtyaXQiOiJKSVQtY29uZmlnLXZhbHVlIn0=\n")
		launch.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + dir, "TMPDIR=" + dir}
		var ee *exec.ExitError
		if err := launch.Run(); !errors.As(err, &ee) || ee.ExitCode() != 125 || read(envf) != "" {
			t.Errorf("a launch with a run and args %q: %v (runner ran %t), want exit 125 and no runner", args, err, read(envf) != "")
		}
	}
}

// A box acquired with jit_guard.run takes it only where box secrets are
// served and only as a run of its guard's repository; the run reaches the
// runner's hook (the run's own job runs once the marker is in the box's
// secrets directory) and the runner gets that directory. A request_id reused with another run is another request.
func TestBoxJITRunGuard(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("no node here (GitHub's runner brings its own for a .js hook)")
	}
	read := func(p string) string { b, _ := os.ReadFile(p); return string(b) }
	const jit = "eyJzZWtyaXQiOiJKSVQtY29uZmlnLXZhbHVlIn0="
	run := map[string]any{"id": 4242, "workflow_ref": "Acme/App/.github/workflows/ci.yml@refs/heads/main", "ref": "refs/heads/main", "hold_s": 1}
	guard := map[string]any{"repo": "Acme/App", "events": []string{"push"}, "run": run}
	binding := func(images string) (map[string]string, string) {
		_, _, m := fakeBinding(t, "linux-img")
		delete(m, "image")
		dir := t.TempDir()
		runner := filepath.Join(dir, "actions-runner")
		if err := os.MkdirAll(runner, 0o700); err != nil {
			t.Fatal(err)
		}
		m["target"], m["kind"], m["size"], m["cpu"], m["memory"] = "t", "box", "small", "1", "512"
		m["images"], m["runner_dir"] = images, runner
		return m, dir
	}
	// A guest without the memory-fs secrets directory (macOS) cannot hold a job for its secrets.
	mac, _ := binding("mac/sonoma@sha256:" + strings.Repeat("ab", 32))
	if d, _ := call(t, mac, "describe", "", nil); strings.Contains(fmt.Sprint(d["capabilities"]), protocol.CapJITGuardRun) {
		t.Errorf("a macOS binding's describe capabilities %v, want no %s", d["capabilities"], protocol.CapJITGuardRun)
	}
	if _, err := call(t, mac, "acquire", "", map[string]any{"request_id": "m", "ttl_s": 60, "jit_config": jit, "jit_guard": guard}); code(err) != "bad_request" {
		t.Errorf("a run guard on a box without box secrets: %v, want bad_request", err)
	}

	m, dir := binding("fake/linux@sha256:" + strings.Repeat("ab", 32))
	out, errs, pidf, job, envf := filepath.Join(dir, "out"), filepath.Join(dir, "hook-stderr"), filepath.Join(dir, "pid"), filepath.Join(dir, "job.env"), filepath.Join(dir, "env")
	if err := os.WriteFile(filepath.Join(m["runner_dir"], "run.sh"), []byte(fakeRunnerShEnv(node, out, errs, pidf, job, envf)), 0o755); err != nil {
		t.Fatal(err)
	}
	if d, _ := call(t, m, "describe", "", nil); !strings.Contains(fmt.Sprint(d["capabilities"]), protocol.CapJITGuardRun) {
		t.Errorf("describe capabilities %v, want %s", d["capabilities"], protocol.CapJITGuardRun)
	}
	with := func(k string, v any) map[string]any {
		r := map[string]any{}
		for k, v := range run {
			r[k] = v
		}
		r[k] = v
		return map[string]any{"repo": "Acme/App", "events": []string{"push"}, "run": r}
	}
	for name, bad := range map[string]map[string]any{
		"a run of another repository": with("workflow_ref", "acme/other/.github/workflows/ci.yml@refs/heads/main"),
		"a tag ref":                   with("ref", "refs/tags/main"),
		"a quote in the workflow ref": with("workflow_ref", "acme/app/.github/workflows/c'i.yml@refs/heads/main"),
		"no run id":                   with("id", 0),
		"a hold over the most":        with("hold_s", protocol.JITHoldMax+1),
	} {
		if _, err := call(t, m, "acquire", "", map[string]any{"request_id": "bad", "ttl_s": 60, "jit_config": jit, "jit_guard": bad}); code(err) != "bad_request" {
			t.Errorf("acquire with %s: %v, want bad_request", name, err)
		}
	}
	a, err := call(t, m, "acquire", "", map[string]any{"request_id": "k", "ttl_s": 60, "jit_config": jit, "jit_guard": guard})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = call(t, m, "release", a["handle"].(string), nil) }()
	for name, g := range map[string]any{"another run": with("id", 4243), "no run": map[string]any{"repo": "Acme/App", "events": []string{"push"}}} {
		if _, err := call(t, m, "acquire", "", map[string]any{"request_id": "k", "ttl_s": 60, "jit_config": jit, "jit_guard": g}); code(err) != "bad_request" {
			t.Errorf("the same request_id with %s: %v, want bad_request", name, err)
		}
	}
	if _, err := call(t, m, "acquire", "", map[string]any{"request_id": "k", "ttl_s": 60, "jit_config": jit, "jit_guard": guard}); err != nil {
		t.Errorf("the same request_id with the same run: %v, want the box", err)
	}
	// The fake layer puts the guest's /dev/shm in the VM's directory. The
	// runner starts after acquire answers: wait for it to have written both.
	waitFor(t, "the runner's environment", func() bool { return read(pidf) != "" && len(strings.Fields(read(envf))) == 2 })
	vars := strings.Fields(read(envf))
	if len(vars) != 2 || vars[0] != vars[1] || !strings.HasSuffix(vars[0], "/shm/potluck-secrets") {
		t.Fatalf("run.sh got POTLUCK_SECRETS and ZM_SECRETS %q, want the box's %s for both", vars, protocol.BoxSecretsDir)
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(read(pidf)))
	if pid <= 1 {
		t.Fatal("the runner wrote no pid")
	}
	// The run's own job, the door's marker in the box: it runs.
	if err := os.MkdirAll(vars[0], 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(vars[0], protocol.SecretNameReady), []byte("4242\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := "GITHUB_EVENT_NAME=push GITHUB_REPOSITORY=acme/app GITHUB_RUN_ID=4242 GITHUB_REF=refs/heads/main GITHUB_WORKFLOW_REF=acme/app/.github/workflows/ci.yml@refs/heads/main"
	if got, _ := runJob(t, "the run's job", env, out, errs, job, pid); got != "hook 0\nalways() step ran\n" {
		t.Errorf("the run's job with its marker: the runner wrote %q (hook stderr %q), want the hook to pass and the steps to run", got, read(errs))
	}
}

// The host-side runner mode is gone: a binding still naming it is refused,
// never served as a plain run binding.
func TestRunnerGitHubBindingIsRefused(t *testing.T) {
	m := map[string]string{"target": "t", "image": "img", "remote": "/srv/r.git", "state_dir": t.TempDir(),
		"runner": "github", "github_repo": "o/r", "github_token_file": "/x"}
	if _, err := parseConfig(m); err == nil || !strings.Contains(err.Error(), "runner=github is gone") {
		t.Errorf("a runner=github binding: %v, want a refusal naming runner=github", err)
	}
	delete(m, "runner")
	if _, err := parseConfig(m); err != nil {
		t.Errorf("the same binding without runner: %v", err)
	}
}
