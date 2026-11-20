package conformancecmd

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sengirunners/potluck/conformance"
	"github.com/sengirunners/potluck/conformance/refdriver"
)

func TestMain(m *testing.M) {
	if code, ok := refdriver.Main(); ok {
		os.Exit(code)
	}
	home, err := os.MkdirTemp("", "zmrc-home") // drivers and git never see the real HOME
	if err != nil {
		panic(err)
	}
	os.Setenv("HOME", home)
	os.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	code := m.Run()
	os.RemoveAll(home)
	os.Exit(code)
}

func TestUsage(t *testing.T) {
	for _, argv := range [][]string{nil, {"--config", "novalue", "x"}, {"no-such-driver-anywhere"}, {"a", "b"}} {
		if code := run(argv); code != 2 {
			t.Fatalf("%v: %d", argv, code)
		}
	}
	if code := run([]string{"--print-probe"}); code != 0 {
		t.Fatalf("--print-probe: %d", code)
	}
}

func ref(t *testing.T, bug string) (string, refdriver.Repo) {
	t.Helper()
	dir := t.TempDir()
	repo, err := refdriver.NewRepo(dir, conformance.ProbeScript)
	if err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(dir, "state")
	p, err := refdriver.Install(dir, state, bug)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { refdriver.KillAll(state) })
	return p, repo
}

func TestRunNeedsItsFlags(t *testing.T) {
	p, _ := ref(t, "")
	// The kind comes from describe; a run target then needs a commit.
	if code := run([]string{p, "--ref", "main", "--script", "probe.sh"}); code != 2 {
		t.Fatalf("no --commit: %d", code)
	}
	if code := run([]string{p, "--kind", "run", "--ref", "main", "--commit", "x", "--script", "s", "--only", "nope"}); code != 2 {
		t.Fatalf("bad --only: %d", code)
	}
}

// The box-authority flags go together, on a box target, with a file that
// is a box credential; anything else is a usage error before any call.
func TestBoxAuthorityFlags(t *testing.T) {
	p, _ := ref(t, "")
	notCred := filepath.Join(t.TempDir(), "cred")
	if err := os.WriteFile(notCred, []byte("potluck_not-a-credential"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, argv := range map[string][]string{
		"client without a credential": {p, "--kind", "box", "--box-client", "potluck status", "--box-want", "orchid"},
		"credential without a client": {p, "--kind", "box", "--box-credential", notCred, "--box-want", "orchid"},
		"not a credential":            {p, "--kind", "box", "--box-credential", notCred, "--box-client", "potluck status", "--box-want", "orchid"},
		"no such file":                {p, "--kind", "box", "--box-credential", notCred + "-gone", "--box-client", "potluck status", "--box-want", "orchid"},
	} {
		if code := run(argv); code != 2 {
			t.Errorf("%s: %d, want 2", name, code)
		}
	}
}

func TestExitCodes(t *testing.T) {
	argv := func(p string, repo refdriver.Repo, extra ...string) []string {
		return append([]string{p, "--kind", "run", "--config", "remote=" + repo.Remote, "--ref", "main", "--commit", repo.Commit,
			"--script", "probe.sh", "--ttl-short", "2", "--grace", "3s", "--run-wait", "10s", "--json"}, extra...)
	}
	p, repo := ref(t, "")
	if code := run(argv(p, repo, "--only", "lifecycle,code")); code != 0 {
		t.Fatalf("good driver: %d", code)
	}
	p, repo = ref(t, "wrong-head")
	if code := run(argv(p, repo, "--only", "code")); code != 1 {
		t.Fatalf("broken driver: %d", code)
	}
}

// The kind describe found goes with every call after it.
func TestKindFromDescribeIsSent(t *testing.T) {
	p, _ := ref(t, "log-kind")
	if code := run([]string{p, "--only", "handles", "--json", "--grace", "3s", "--ref", "main", "--commit", "x", "--script", "probe.sh"}); code != 0 {
		t.Fatalf("exit %d", code)
	}
	b, err := os.ReadFile(filepath.Join(filepath.Dir(p), "state", "kinds"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	for _, l := range lines[1:] { // the first is the describe that finds the kind
		if !strings.HasPrefix(l, " ") && !strings.HasSuffix(l, " kind=run") { // " kind=": the malformed request
			t.Fatalf("a call without kind: %q in\n%s", l, b)
		}
	}
}

// SIGTERM puts a dirtied --worktree back and ends what the run made.
func TestSIGTERMCleansUp(t *testing.T) {
	p, repo := ref(t, "slow-status")
	done := make(chan int, 1)
	go func() {
		done <- run([]string{p, "--kind", "run", "--config", "remote=" + repo.Remote, "--ref", "main", "--commit", repo.Commit,
			"--script", "probe.sh", "--worktree", repo.Worktree, "--only", "code", "--run-wait", "60s", "--json"})
	}()
	dirtied := filepath.Join(repo.Worktree, "zm-run-conformance-dirty")
	for deadline := time.Now().Add(90 * time.Second); ; time.Sleep(100 * time.Millisecond) {
		if _, err := os.Stat(dirtied); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the worktree was never dirtied")
		}
	}
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("run did not return after SIGTERM")
	}
	out, err := exec.Command("git", "-C", repo.Worktree, "status", "--porcelain").Output()
	if err != nil || len(out) > 0 {
		t.Fatalf("worktree not put back: %v %s", err, out)
	}
	if live := liveRecords(filepath.Join(filepath.Dir(p), "state")); len(live) > 0 {
		t.Fatalf("left live: %v", live)
	}
}

func liveRecords(state string) []string {
	ps, _ := filepath.Glob(filepath.Join(state, "h-*.json"))
	var live []string
	for _, p := range ps {
		var r struct{ Handle, State string }
		b, _ := os.ReadFile(p)
		if json.Unmarshal(b, &r) == nil && (r.State == "ready" || r.State == "running") {
			live = append(live, r.Handle)
		}
	}
	return live
}

func TestDBProbe(t *testing.T) {
	p, _ := ref(t, "")
	code := run([]string{p, "--kind", "db", "--only", "lifecycle", "--json", "--grace", "3s",
		"--db-probe", `test -e "${ZM_CONFORMANCE_URL##*file=}"`})
	if code != 0 {
		t.Fatalf("good db driver: %d", code)
	}
	p, _ = ref(t, "release-noop")
	code = run([]string{p, "--kind", "db", "--only", "lifecycle", "--json", "--grace", "3s",
		"--db-probe", `test -e "${ZM_CONFORMANCE_URL##*file=}"`})
	if code != 1 {
		t.Fatalf("a release that drops nothing: %d", code)
	}
}

// capture runs argv with stdout and stderr captured.
func capture(argv []string) (code int, out, errOut string) {
	var o, e strings.Builder
	stdout, stderr = &o, &e
	defer func() { stdout, stderr = os.Stdout, os.Stderr }()
	code = run(argv)
	return code, o.String(), e.String()
}

// --strict turns a skipped network or expiry check into exit 1 with the
// reason; without it the same run exits 0. Skips are listed by name either
// way, on stderr with --json, where stdout stays one array with each group.
func TestStrict(t *testing.T) {
	p, repo := ref(t, "")
	argv := []string{p, "--kind", "box", "--config", "remote=" + repo.Remote, "--only", "net,expiry", "--ttl-short", "2", "--grace", "2s"}
	code, out, _ := capture(argv)
	if code != 0 || !strings.Contains(out, "2 check(s) skipped: not proven\n  - a short ttl ends the resource with no release and no call to the driver (expiry)\n  - net (net)\n") {
		t.Fatalf("not strict: exit %d\n%s", code, out)
	}
	code, out, _ = capture(append(argv, "--strict"))
	if code != 1 || !strings.Contains(out, "FAIL  "+conformance.StrictCheck) || !strings.Contains(out, `--strict failed: not proven: "net" skipped`) {
		t.Fatalf("strict: exit %d\n%s", code, out)
	}
	code, out, errOut := capture(append(argv, "--strict", "--json"))
	var res []conformance.Result
	if err := json.Unmarshal([]byte(out), &res); err != nil || code != 1 {
		t.Fatalf("--json: exit %d, %v\n%s", code, err, out)
	}
	var sawGroup, sawStrict bool
	for _, r := range res {
		sawGroup = sawGroup || r.Name == "net" && r.Skipped && r.Group == "net"
		sawStrict = sawStrict || r.Name == conformance.StrictCheck && !r.OK
	}
	if !sawGroup || !sawStrict || !strings.Contains(errOut, "  - net (net)") || !strings.Contains(errOut, "--strict failed:") {
		t.Fatalf("--json: group %v strict %v\nstdout %s\nstderr %s", sawGroup, sawStrict, out, errOut)
	}
}

// --json-out writes the results and the skipped checks' names to a file, for a failing run too, and leaves the exit
// code and the printed output as they were.
func TestJSONOut(t *testing.T) {
	read := func(path string) Report {
		t.Helper()
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var r Report
		if err := json.Unmarshal(b, &r); err != nil {
			t.Fatalf("%v\n%s", err, b)
		}
		return r
	}
	p, repo := ref(t, "")
	out := filepath.Join(t.TempDir(), "results.json")
	argv := []string{p, "--kind", "box", "--config", "remote=" + repo.Remote, "--only", "net,expiry", "--ttl-short", "2", "--grace", "2s", "--json-out", out}
	code, stdout, _ := capture(argv)
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, stdout)
	}
	r := read(out)
	if r.Version != ReportVersion || r.Kind != "box" || r.Driver != p || !r.Passed || r.Failed != 0 || len(r.Results) == 0 {
		t.Fatalf("report: %+v", r)
	}
	want := []SkippedCheck{{Name: "a short ttl ends the resource with no release and no call to the driver", Group: "expiry"}, {Name: "net", Group: "net"}}
	if len(r.Skipped) != len(want) || r.Skipped[0] != want[0] || r.Skipped[1] != want[1] {
		t.Fatalf("skipped: %+v, want %+v", r.Skipped, want)
	}
	// What is printed is the same with and without the file.
	_, plain, _ := capture(argv[:len(argv)-2])
	if plain != stdout {
		t.Fatalf("--json-out changed the output:\n%s\nvs\n%s", plain, stdout)
	}

	// A failed run still writes its report, and exits 1 as before.
	p, repo = ref(t, "wrong-head")
	bad := filepath.Join(t.TempDir(), "bad.json")
	code, _, _ = capture([]string{p, "--kind", "run", "--config", "remote=" + repo.Remote, "--ref", "main", "--commit", repo.Commit,
		"--script", "probe.sh", "--ttl-short", "2", "--grace", "3s", "--run-wait", "10s", "--only", "code", "--json-out", bad})
	if code != 1 {
		t.Fatalf("broken driver: exit %d", code)
	}
	if r := read(bad); r.Passed || r.Failed == 0 {
		t.Fatalf("a failed run reported as passed: %+v", r)
	}

	// With no skipped check the list is [], not null.
	b, _ := json.Marshal(newReport("d", "run", false, nil))
	if !strings.Contains(string(b), `"skipped":[]`) {
		t.Fatalf("%s", b)
	}

	// A directory that is not there is a usage error before any call.
	p, repo = ref(t, "")
	box := []string{p, "--kind", "box", "--config", "remote=" + repo.Remote, "--only", "net,expiry", "--ttl-short", "2", "--grace", "2s"}
	code, _, errOut := capture(append(box, "--json-out", filepath.Join(t.TempDir(), "gone", "r.json")))
	if code != 2 || !strings.Contains(errOut, "--json-out") {
		t.Fatalf("missing directory: exit %d\n%s", code, errOut)
	}

	// A report that cannot be written (the path is a directory) is said so, and the exit code stays the run's own.
	code, stdout, _ = capture(append(box, "--json-out", t.TempDir()))
	if code != 0 || !strings.Contains(stdout, "--json-out:") {
		t.Fatalf("unwritable report: exit %d\n%s", code, stdout)
	}
}
