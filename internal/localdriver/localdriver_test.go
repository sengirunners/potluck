package localdriver

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sengirunners/potluck/conformance"
	"github.com/sengirunners/potluck/conformance/refdriver"
)

const flagDriver = "-zm-run-local-driver"

// TestMain lets the test binary be the driver (behind a wrapper script, as
// the daemon finds zm-run-local) and its supervisor.
func TestMain(m *testing.M) {
	switch {
	case len(os.Args) == 3 && os.Args[1] == flagSupervise:
		os.Exit(supervise(os.Args[2]))
	case len(os.Args) == 2 && os.Args[1] == flagDriver:
		os.Args = os.Args[:1]
		os.Exit(Main())
	}
	home, err := os.MkdirTemp("", "zm")
	if err != nil {
		panic(err)
	}
	os.Setenv("HOME", home) // the driver passes HOME on, so git reads no real user config
	code := m.Run()
	os.RemoveAll(home)
	os.Exit(code)
}

type fixture struct {
	driver, state string
	repo          refdriver.Repo
	cfg           map[string]string
}

func newFixture(t *testing.T, probe string) fixture {
	t.Helper()
	dir, err := os.MkdirTemp("", "zmrl")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { // after killAll (cleanups run last first): supervisors gone, then the files
		for i := 0; i < 50; i++ {
			if os.RemoveAll(dir) == nil {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Errorf("could not remove %s", dir)
	})
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	f := fixture{driver: filepath.Join(dir, "zm-run-local"), state: filepath.Join(dir, "state")}
	if err := os.WriteFile(f.driver, []byte(fmt.Sprintf("#!/bin/sh\nexec %q %s\n", self, flagDriver)), 0o755); err != nil {
		t.Fatal(err)
	}
	if f.repo, err = refdriver.NewRepo(dir, probe); err != nil {
		t.Fatal(err)
	}
	f.cfg = map[string]string{"target": "home", "kind": "run", "remote": f.repo.Remote, "state_dir": f.state}
	t.Cleanup(func() { killAll(f.state) })
	return f
}

// killAll ends every run and supervisor a test left, and waits until their
// locks say they are gone (a supervisor may still be removing a checkout).
func killAll(state string) {
	ps, _ := filepath.Glob(filepath.Join(state, "bindings", "*", "h-*.json"))
	var rs []record
	for _, p := range ps {
		if r, err := load(p); err == nil {
			rs = append(rs, r)
			if r.PID > 0 && runAlive(r) {
				_ = syscall.Kill(-r.PID, syscall.SIGKILL)
			}
			if r.Supervisor > 0 && supAlive(r) {
				_ = syscall.Kill(r.Supervisor, syscall.SIGKILL)
			}
		}
	}
	for end := time.Now().Add(10 * time.Second); time.Now().Before(end); time.Sleep(50 * time.Millisecond) {
		if !slices.ContainsFunc(rs, func(r record) bool { return supAlive(r) || runAlive(r) }) {
			return
		}
	}
}

// call runs the driver as the daemon does: one envelope on stdin, the
// environment allowlist only, its own process group.
func (f fixture) call(t *testing.T, verb, id string, args any, cfg map[string]string) map[string]any {
	t.Helper()
	env := map[string]any{"zm": "zm/v0", "verb": verb, "id": id, "config": cfg}
	if args != nil {
		env["args"] = args
	}
	in, _ := json.Marshal(env)
	cmd := exec.Command(f.driver)
	cmd.Stdin = strings.NewReader(string(in))
	cmd.Env = allowedEnv()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("%s: %v", verb, err)
	}
	var ans map[string]any
	if err := json.Unmarshal(out, &ans); err != nil {
		t.Fatalf("%s: %v: %s", verb, err, out)
	}
	return ans
}

func code(ans map[string]any) string {
	if e, ok := ans["error"].(map[string]any); ok {
		return e["code"].(string)
	}
	return ""
}

func result(t *testing.T, ans map[string]any) map[string]any {
	t.Helper()
	if ans["ok"] != true {
		t.Fatalf("refused: %v", ans["error"])
	}
	return ans["result"].(map[string]any)
}

// TestConformance runs SPEC 20's suite against the driver.
func TestConformance(t *testing.T) {
	if testing.Short() {
		t.Skip("runs real processes")
	}
	f := newFixture(t, conformance.ProbeScript)
	opt := conformance.Options{Kind: "run", Target: "home", ShortTTL: 2, Grace: 3 * time.Second, RunWait: 20 * time.Second,
		Ref: "main", Commit: f.repo.Commit, Repo: f.repo.Remote, Script: "probe.sh",
		Config: map[string]string{"remote": f.repo.Remote, "state_dir": f.state}}
	res := conformance.Run(context.Background(), f.driver, opt)
	for _, r := range res {
		if !r.OK {
			t.Errorf("%s: %s", r.Name, r.Detail)
		}
	}
	if t.Failed() {
		t.Log(conformance.Format(res))
	}
}

// A run sees only the allowlist, a clean checkout of the commit, no stdin,
// and its output lands in the log; the checkout is gone after.
func TestRunEnvironmentAndOutput(t *testing.T) {
	probe := "#!/bin/sh\nenv | sort\necho \"cwd=$(pwd)\"\nread x && echo got-stdin\necho err >&2\nexit 3\n"
	f := newFixture(t, probe)
	t.Setenv("ZM_SECRET_TEST", "leak")
	a := result(t, f.call(t, "acquire", "", map[string]any{"request_id": "r1", "actor": "a", "ref": "main",
		"commit": f.repo.Commit, "script": "probe.sh"}, f.cfg))
	h := a["handle"].(string)
	st := waitEnd(t, f, h)
	if st["state"] != "ended" || st["exit"] != float64(3) {
		t.Fatalf("status %v", st)
	}
	b, _ := os.ReadFile(a["log"].(string))
	out := string(b)
	if strings.Contains(out, "ZM_SECRET_TEST") || strings.Contains(out, "got-stdin") || !strings.Contains(out, "err") {
		t.Fatalf("output:\n%s", out)
	}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		k, _, _ := strings.Cut(line, "=")
		switch k {
		case "PATH", "HOME", "LANG", "TERM", "TZ", "PWD", "SHLVL", "_", "OLDPWD", "cwd", "err":
		default:
			t.Errorf("unexpected env %q", line)
		}
	}
	var cwd string
	for _, line := range strings.Split(out, "\n") {
		if c, ok := strings.CutPrefix(line, "cwd="); ok {
			cwd = c
		}
	}
	if cwd == "" {
		t.Fatal("no cwd line")
	}
	if _, err := os.Stat(cwd); !os.IsNotExist(err) {
		t.Fatalf("checkout %s still there: %v", cwd, err)
	}
}

func waitEnd(t *testing.T, f fixture, h string) map[string]any {
	t.Helper()
	for end := time.Now().Add(20 * time.Second); time.Now().Before(end); time.Sleep(100 * time.Millisecond) {
		st := result(t, f.call(t, "status", h, nil, f.cfg))
		if st["state"] != "running" {
			return st
		}
	}
	t.Fatal("run did not end")
	return nil
}

// A commit the ref's history on the remote does not hold is refused, as is a
// file that is not executable, and a shell string for a script.
func TestAcquireRefusals(t *testing.T) {
	f := newFixture(t, "#!/bin/sh\nexit 0\n")
	base := map[string]any{"request_id": "x", "actor": "a", "ref": "main", "commit": f.repo.Commit, "script": "probe.sh"}
	with := func(k string, v any) map[string]any {
		m := map[string]any{}
		for a, b := range base {
			m[a] = b
		}
		m[k] = v
		m["request_id"] = fmt.Sprintf("%s-%v", k, v)
		return m
	}
	for _, c := range []struct {
		args map[string]any
		want string
	}{
		{with("commit", strings.Repeat("a", 40)), "forbidden"},
		{with("script", "tip.txt"), "bad_request"},
		{with("script", "probe.sh; rm -rf /"), "bad_request"},
		{with("script", "../probe.sh"), "bad_request"},
		{with("ref", "refs/heads/main"), "bad_request"},
		{with("ttl_s", 99999999), "bad_request"},
		{with("script", "missing.sh"), "bad_request"},
	} {
		if got := code(f.call(t, "acquire", "", c.args, f.cfg)); got != c.want {
			t.Errorf("%v: got %q, want %q", c.args, got, c.want)
		}
	}
	// A refused acquire leaves no live handle and no checkout.
	l := result(t, f.call(t, "list", "", nil, f.cfg))
	for _, h := range l["handles"].([]any) {
		if s := h.(map[string]any)["state"]; s == "running" {
			t.Errorf("live handle after refusals: %v", h)
		}
	}
	co, _ := filepath.Glob(filepath.Join(f.state, "bindings", "home", "r*", "checkout"))
	if len(co) > 0 {
		t.Errorf("checkouts left: %v", co)
	}
}

// One binding's handles are not another's, and the config is checked.
func TestBindingsAndConfig(t *testing.T) {
	f := newFixture(t, "#!/bin/sh\nexit 0\n")
	a := result(t, f.call(t, "acquire", "", map[string]any{"request_id": "b1", "actor": "a", "ref": "main",
		"commit": f.repo.Commit, "script": "probe.sh"}, f.cfg))
	other := map[string]string{"target": "other", "kind": "run", "remote": f.repo.Remote, "state_dir": f.state}
	if got := code(f.call(t, "status", a["handle"].(string), nil, other)); got != "bad_request" {
		t.Errorf("another binding's handle: %q", got)
	}
	if got := code(f.call(t, "status", "run:home/r00000000000000000000", nil, f.cfg)); got != "not_found" {
		t.Errorf("unissued handle: %q", got)
	}
	for _, cfg := range []map[string]string{
		{"target": "home", "kind": "run", "state_dir": f.state},
		{"target": "home", "kind": "db", "remote": f.repo.Remote, "state_dir": f.state},
		{"target": "home", "kind": "run", "remote": f.repo.Remote, "state_dir": "relative"},
		{"target": "Home/x", "kind": "run", "remote": f.repo.Remote, "state_dir": f.state},
	} {
		if got := code(f.call(t, "describe", "", nil, cfg)); got != "bad_request" {
			t.Errorf("config %v: %q", cfg, got)
		}
	}
	d := result(t, f.call(t, "describe", "", nil, f.cfg))
	if d["isolation"] != "none" || d["credentials"] != true {
		t.Errorf("describe class %v", d)
	}
}

// A supervisor that dies leaves the run lost, never running forever.
func TestLostSupervisor(t *testing.T) {
	f := newFixture(t, "#!/bin/sh\nsleep 30\n")
	a := result(t, f.call(t, "acquire", "", map[string]any{"request_id": "l1", "actor": "a", "ref": "main",
		"commit": f.repo.Commit, "script": "probe.sh"}, f.cfg))
	h := a["handle"].(string)
	p := f.recordPath(h)
	r, err := load(p)
	if err != nil || r.Supervisor == 0 {
		t.Fatalf("record %v %v", r, err)
	}
	_ = syscall.Kill(r.Supervisor, syscall.SIGKILL)
	for end := time.Now().Add(5 * time.Second); supAlive(r) && time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
	}
	if st := result(t, f.call(t, "status", h, nil, f.cfg)); st["state"] != "lost" {
		t.Fatalf("status %v", st)
	}
	gone := false
	for end := time.Now().Add(3 * time.Second); !gone && time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		gone = syscall.Kill(-r.PID, 0) != nil // a zombie still answers until it is reaped
	}
	if !gone {
		t.Fatal("the script outlived its lost run")
	}
}

func (f fixture) recordPath(h string) string {
	return filepath.Join(f.state, "bindings", "home", "h-"+h[strings.LastIndex(h, "/")+1:]+".json")
}

func (f fixture) acquire(t *testing.T, key string, extra map[string]any) map[string]any {
	t.Helper()
	a := map[string]any{"request_id": key, "actor": "a", "ref": "main", "commit": f.repo.Commit, "script": "probe.sh"}
	for k, v := range extra {
		a[k] = v
	}
	return f.call(t, "acquire", "", a, f.cfg)
}

// The deadline ends a run with exit 124, and release ends one at once; both
// kill the script and leave no checkout.
func TestDeadlineAndRelease(t *testing.T) {
	f := newFixture(t, "#!/bin/sh\nsleep 60\n")
	exp := result(t, f.acquire(t, "d1", map[string]any{"ttl_s": 1}))
	rel := result(t, f.acquire(t, "d2", nil))
	if r := result(t, f.call(t, "release", rel["handle"].(string), nil, f.cfg)); r["released"] != true {
		t.Fatalf("release %v", r)
	}
	st := waitEnd(t, f, exp["handle"].(string))
	if st["state"] != "expired" || st["exit"] != float64(124) {
		t.Fatalf("expired run: %v", st)
	}
	if st := result(t, f.call(t, "status", rel["handle"].(string), nil, f.cfg)); st["state"] != "released" {
		t.Fatalf("released run: %v", st)
	}
	for _, h := range []string{exp["handle"].(string), rel["handle"].(string)} {
		r, _ := load(f.recordPath(h))
		for end := time.Now().Add(10 * time.Second); time.Now().Before(end); time.Sleep(50 * time.Millisecond) {
			if _, err := os.Stat(r.Checkout); os.IsNotExist(err) && syscall.Kill(-r.PID, 0) != nil {
				break
			}
		}
		if _, err := os.Stat(r.Checkout); !os.IsNotExist(err) {
			t.Errorf("%s: checkout left", h)
		}
		if syscall.Kill(-r.PID, 0) == nil {
			t.Errorf("%s: script still running", h)
		}
	}
}

// A refused acquire answers the same refusal when asked again with its
// request id, and leaves no handle behind.
func TestRefusalRepeats(t *testing.T) {
	f := newFixture(t, "#!/bin/sh\nexit 0\n")
	bad := map[string]any{"commit": strings.Repeat("b", 40)}
	for range 2 {
		if got := code(f.acquire(t, "same", bad)); got != "forbidden" {
			t.Fatalf("got %q", got)
		}
	}
	if l := result(t, f.call(t, "list", "", nil, f.cfg)); len(l["handles"].([]any)) != 0 {
		t.Fatalf("refusals left handles: %v", l)
	}
}

// An acquire killed while it prepared (the daemon's time limit) leaves a
// record a repeat of the same request takes over and starts.
func TestRepeatTakesOverAStrandedPrepare(t *testing.T) {
	f := newFixture(t, "#!/bin/sh\nexit 5\n")
	first := result(t, f.acquire(t, "s1", nil))
	h := first["handle"].(string)
	waitEnd(t, f, h)
	// Make it look as if its acquire was killed mid checkout: no supervisor,
	// nobody holding prep.lock, and a half-made checkout left behind.
	r, _ := load(f.recordPath(h))
	r.State, r.Supervisor, r.PID, r.Exit, r.Ended = "starting", 0, 0, nil, time.Time{}
	if err := save(f.recordPath(h), r); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(r.Checkout, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(r.Checkout, "half"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	// and git's record of it, still locked "initializing" as a killed add leaves it
	wt := filepath.Join(r.Mirror, "worktrees", "checkout-killed")
	for name, body := range map[string]string{"gitdir": filepath.Join(r.Checkout, ".git") + "\n", "locked": "initializing\n",
		"commondir": "../..\n", "HEAD": r.Commit + "\n"} {
		if err := os.MkdirAll(wt, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(wt, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	again := result(t, f.acquire(t, "s1", nil))
	if again["handle"] != h {
		t.Fatalf("a new handle %v for the same request", again["handle"])
	}
	if st := waitEnd(t, f, h); st["state"] != "ended" || st["exit"] != float64(5) {
		t.Fatalf("status %v", st)
	}
}

// A supervisor that is alive but never ends its run is overruled once the
// deadline is well past: the run is expired, not running forever.
func TestHungSupervisorPastDeadline(t *testing.T) {
	f := newFixture(t, "#!/bin/sh\nsleep 60\n")
	h := result(t, f.acquire(t, "h1", nil))["handle"].(string)
	r, _ := load(f.recordPath(h))
	_ = syscall.Kill(r.Supervisor, syscall.SIGSTOP) // hung
	t.Cleanup(func() { _ = syscall.Kill(r.Supervisor, syscall.SIGKILL) })
	r, _ = load(f.recordPath(h))
	r.Expires = time.Now().Add(-time.Hour)
	if err := save(f.recordPath(h), r); err != nil {
		t.Fatal(err)
	}
	if st := result(t, f.call(t, "status", h, nil, f.cfg)); st["state"] != "expired" || st["exit"] != float64(124) {
		t.Fatalf("status %v", st)
	}
}

// A recorded PID that is no longer the run's (the supervisor died, and its
// PID and the script's group now belong to strangers) is never signalled.
func TestStrangerPIDsAreNotSignalled(t *testing.T) {
	f := newFixture(t, "#!/bin/sh\nexit 0\n")
	h := result(t, f.acquire(t, "p1", nil))["handle"].(string)
	waitEnd(t, f, h)
	stranger := exec.Command("sleep", "30")
	stranger.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := stranger.Start(); err != nil {
		t.Fatal(err)
	}
	r, _ := load(f.recordPath(h))
	r.State, r.Exit, r.Ended = "running", nil, time.Time{}
	r.Supervisor, r.PID = stranger.Process.Pid, stranger.Process.Pid
	r.Expires = time.Now().Add(-time.Hour)
	if err := save(f.recordPath(h), r); err != nil {
		t.Fatal(err)
	}
	if st := result(t, f.call(t, "status", h, nil, f.cfg)); st["state"] != "lost" {
		t.Fatalf("status %v", st)
	}
	// Wait, not kill(pid, 0): a killed child stays a zombie until reaped.
	exited := make(chan struct{})
	go func() { _ = stranger.Wait(); close(exited) }()
	select {
	case <-exited:
		t.Fatal("the stranger was signalled")
	case <-time.After(500 * time.Millisecond):
	}
	_ = stranger.Process.Kill()
	<-exited
}

// A mirror busy with another run's git never delays a verdict or a status.
func TestBusyMirrorDelaysNothing(t *testing.T) {
	f := newFixture(t, "#!/bin/sh\nsleep 1\nexit 4\n")
	h := result(t, f.acquire(t, "b1", nil))["handle"].(string)
	r, _ := load(f.recordPath(h))
	l := hold(r.Mirror + ".lock") // another run's clone, say
	if l == nil {
		t.Fatal("mirror lock taken")
	}
	defer l.Close()
	start := time.Now()
	st := waitEnd(t, f, h)
	if st["state"] != "ended" || st["exit"] != float64(4) || time.Since(start) > 8*time.Second {
		t.Fatalf("status %v after %s", st, time.Since(start))
	}
	if _, err := os.Stat(r.Checkout); !os.IsNotExist(err) {
		t.Fatalf("checkout files left: %v", err)
	}
}

// publishRun acquires a run of probe with ZM_PUBLISH_FILE=file in the driver's
// environment (file "" leaves it unset) and answers its status once it ended.
func publishRun(t *testing.T, probe, file string) (fixture, map[string]any, map[string]any) {
	t.Helper()
	f := newFixture(t, probe)
	if file != "" {
		t.Setenv("ZM_PUBLISH_FILE", file)
	}
	a := result(t, f.call(t, "acquire", "", map[string]any{"request_id": "p1", "actor": "a", "ref": "main",
		"commit": f.repo.Commit, "script": "probe.sh"}, f.cfg))
	return f, a, waitEnd(t, f, a["handle"].(string))
}

func TestDescribeListsRunPublish(t *testing.T) {
	f := newFixture(t, "#!/bin/sh\nexit 0\n")
	d := result(t, f.call(t, "describe", "", nil, f.cfg))
	if !slices.Contains(toStrings(d["capabilities"]), "run-publish") {
		t.Errorf("capabilities %v", d["capabilities"])
	}
}

func toStrings(v any) []string {
	var out []string
	for _, x := range v.([]any) {
		out = append(out, x.(string))
	}
	return out
}

// An ended run's status carries the JSON its script wrote to the file
// ZM_PUBLISH_FILE named at acquire; a status after release answers the same.
func TestPublishOnEndedStatus(t *testing.T) {
	f, a, st := publishRun(t, "#!/bin/sh\nmkdir -p out\nprintf '{ \"url\": \"https://x.test/a\",\n \"n\": 2 }\\n' > out/pub.json\nexit 3\n", "out/pub.json")
	want := map[string]any{"url": "https://x.test/a", "n": float64(2)}
	if st["state"] != "ended" || st["exit"] != float64(3) {
		t.Fatalf("status %v", st)
	}
	if !reflect.DeepEqual(st["publish"], want) {
		t.Errorf("publish %v, want %v", st["publish"], want)
	}
	if rel := result(t, f.call(t, "release", a["handle"].(string), nil, f.cfg)); rel["released"] != false {
		t.Errorf("release %v", rel)
	}
	again := result(t, f.call(t, "status", a["handle"].(string), nil, f.cfg))
	if !reflect.DeepEqual(again["publish"], want) {
		t.Errorf("later status %v", again)
	}
}

// The script is told the file by absolute path, inside its checkout.
func TestPublishFileEnvInScript(t *testing.T) {
	f, a, _ := publishRun(t, "#!/bin/sh\necho \"pub=$ZM_PUBLISH_FILE\"\necho \"cwd=$(pwd -P)\"\n", "a/b.json")
	b, _ := os.ReadFile(a["log"].(string))
	var pub, cwd string
	for _, line := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(line, "pub="); ok {
			pub = v
		}
		if v, ok := strings.CutPrefix(line, "cwd="); ok {
			cwd = v
		}
	}
	state, err := filepath.EvalSymlinks(f.state) // a temporary directory may sit behind a link (macOS /var)
	if err != nil {
		t.Fatal(err)
	}
	rest, ok := strings.CutPrefix(pub, f.state)
	if cwd == "" || !ok || state+rest != filepath.Join(cwd, "a/b.json") {
		t.Errorf("ZM_PUBLISH_FILE %q in cwd %q", pub, cwd)
	}
}

// Without ZM_PUBLISH_FILE at acquire nothing is published and the script is told of no file.
func TestPublishOffWithoutEnv(t *testing.T) {
	_, a, st := publishRun(t, "#!/bin/sh\necho \"pub=[$ZM_PUBLISH_FILE]\"\necho '{\"a\":1}' > pub.json\n", "")
	if _, ok := st["publish"]; ok {
		t.Errorf("publish %v", st["publish"])
	}
	if b, _ := os.ReadFile(a["log"].(string)); !strings.Contains(string(b), "pub=[]") {
		t.Errorf("log %s", b)
	}
}

// A file that is absent, too big, not JSON, a directory, a fifo or a link
// out of the checkout publishes nothing; the run still ends with its exit.
func TestPublishOmitted(t *testing.T) {
	secret := filepath.Join(t.TempDir(), "secret.json")
	if err := os.WriteFile(secret, []byte(`{"stolen":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"absent":    "exit 0",
		"oversize":  "{ printf '{\"x\":\"'; head -c 70000 /dev/zero | tr '\\0' a; printf '\"}'; } > p.json",
		"not json":  "echo '{nope' > p.json",
		"two":       "echo '{} {}' > p.json",
		"null":      "echo null > p.json",
		"directory": "mkdir p.json",
		"fifo":      "mkfifo p.json",
		"link out":  "ln -s " + secret + " p.json",
		"dir link":  "mkdir d; ln -s " + filepath.Dir(secret) + " d/x; : > /dev/null",
	} {
		t.Run(name, func(t *testing.T) {
			file := "p.json"
			if name == "dir link" {
				file = "d/x/secret.json"
			}
			_, _, st := publishRun(t, "#!/bin/sh\n"+body+"\nexit 0\n", file)
			if st["state"] != "ended" || st["exit"] != float64(0) {
				t.Fatalf("status %v", st)
			}
			if _, ok := st["publish"]; ok {
				t.Errorf("publish %v", st["publish"])
			}
		})
	}
}

// A file the commit carries at the publish path is not published: only what
// the script writes is.
func TestPublishIgnoresACommittedFile(t *testing.T) {
	f := newFixture(t, "#!/bin/sh\nexit 0\n")
	gitIn := func(a ...string) string {
		t.Helper()
		out, err := exec.Command("git", append([]string{"-C", f.repo.Worktree, "-c", "user.name=t", "-c", "user.email=t@example.invalid", "-c", "commit.gpgsign=false"}, a...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", a, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	if err := os.WriteFile(filepath.Join(f.repo.Worktree, "p.json"), []byte(`{"stale":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn("add", "p.json")
	gitIn("commit", "-q", "-m", "stale publish")
	gitIn("push", "-q", f.repo.Remote, "HEAD:refs/heads/main")
	t.Setenv("ZM_PUBLISH_FILE", "p.json")
	a := result(t, f.call(t, "acquire", "", map[string]any{"request_id": "p1", "actor": "a", "ref": "main",
		"commit": gitIn("rev-parse", "HEAD"), "script": "probe.sh"}, f.cfg))
	st := waitEnd(t, f, a["handle"].(string))
	if st["exit"] != float64(0) {
		t.Fatalf("status %v", st)
	}
	if _, ok := st["publish"]; ok {
		t.Errorf("a committed file was published: %v", st["publish"])
	}
}

// A publish path the commit makes unreachable (a directory above it that is
// a link out of the checkout) or a directory does not stop the run: it ends
// with its exit and publishes nothing.
func TestPublishPathTheCommitBlocks(t *testing.T) {
	for name, file := range map[string]string{"link above": "out/p.json", "directory": "dir"} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, "#!/bin/sh\nexit 0\n")
			gitIn := func(a ...string) string {
				t.Helper()
				out, err := exec.Command("git", append([]string{"-C", f.repo.Worktree, "-c", "user.name=t", "-c", "user.email=t@example.invalid", "-c", "commit.gpgsign=false"}, a...)...).CombinedOutput()
				if err != nil {
					t.Fatalf("git %v: %v: %s", a, err, out)
				}
				return strings.TrimSpace(string(out))
			}
			if err := os.Symlink(t.TempDir(), filepath.Join(f.repo.Worktree, "out")); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Join(f.repo.Worktree, "dir"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(f.repo.Worktree, "dir", "keep"), []byte("{}"), 0o644); err != nil {
				t.Fatal(err)
			}
			gitIn("add", "out", "dir")
			gitIn("commit", "-q", "-m", "blocking paths")
			gitIn("push", "-q", f.repo.Remote, "HEAD:refs/heads/main")
			t.Setenv("ZM_PUBLISH_FILE", file)
			a := result(t, f.call(t, "acquire", "", map[string]any{"request_id": "p1", "actor": "a", "ref": "main",
				"commit": gitIn("rev-parse", "HEAD"), "script": "probe.sh"}, f.cfg))
			st := waitEnd(t, f, a["handle"].(string))
			if _, ok := st["publish"]; ok || st["exit"] != float64(0) {
				t.Errorf("status %v", st)
			}
		})
	}
}

// A run that did not end on its own (released) publishes nothing.
func TestPublishNotOnReleasedRun(t *testing.T) {
	f := newFixture(t, "#!/bin/sh\necho '{\"a\":1}' > p.json\nsleep 60\n")
	t.Setenv("ZM_PUBLISH_FILE", "p.json")
	a := result(t, f.call(t, "acquire", "", map[string]any{"request_id": "p2", "actor": "a", "ref": "main",
		"commit": f.repo.Commit, "script": "probe.sh"}, f.cfg))
	h := a["handle"].(string)
	rel := result(t, f.call(t, "release", h, nil, f.cfg))
	st := result(t, f.call(t, "status", h, nil, f.cfg))
	if rel["released"] != true || st["state"] == "ended" {
		t.Errorf("release %v, status %v", rel, st)
	}
	if _, ok := st["publish"]; ok {
		t.Errorf("status %v", st)
	}
}

// A publish path that is not clean and inside the checkout is refused at acquire, and makes no run.
func TestPublishPathRefused(t *testing.T) {
	for _, p := range []string{"/etc/passwd", "../x.json", "a/../b", "a b\nc"} {
		f := newFixture(t, "#!/bin/sh\nexit 0\n")
		t.Setenv("ZM_PUBLISH_FILE", p)
		got := code(f.call(t, "acquire", "", map[string]any{"request_id": "p3", "actor": "a", "ref": "main",
			"commit": f.repo.Commit, "script": "probe.sh"}, f.cfg))
		if got != "bad_request" {
			t.Errorf("%q: %q", p, got)
		}
	}
}

// A repeat of an acquire answers its run whatever ZM_PUBLISH_FILE says now:
// the run keeps the file it was acquired with.
func TestPublishRepeatKeepsTheRunsFile(t *testing.T) {
	f := newFixture(t, "#!/bin/sh\nmkdir -p out\necho '{\"a\":1}' > out/p.json\n")
	t.Setenv("ZM_PUBLISH_FILE", "out/p.json")
	args := map[string]any{"request_id": "p4", "actor": "a", "ref": "main", "commit": f.repo.Commit, "script": "probe.sh"}
	first := result(t, f.call(t, "acquire", "", args, f.cfg))
	t.Setenv("ZM_PUBLISH_FILE", "../elsewhere.json")
	again := result(t, f.call(t, "acquire", "", args, f.cfg))
	if again["handle"] != first["handle"] {
		t.Fatalf("repeat answered %v, first %v", again, first)
	}
	if st := waitEnd(t, f, first["handle"].(string)); !reflect.DeepEqual(st["publish"], map[string]any{"a": float64(1)}) {
		t.Errorf("status %v", st)
	}
}
