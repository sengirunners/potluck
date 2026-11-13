package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/runners"
	"gopkg.in/yaml.v3"
)

// fakeDoctorDaemon serves report as the daemon's doctor answer on a socket in a new home; the function it returns
// replaces the answer.
func fakeDoctorDaemon(t *testing.T, report runners.DoctorReport) func(runners.DoctorReport) {
	t.Helper()
	set := fakeDoctorReports(t, report)
	return func(r runners.DoctorReport) { set(r) }
}

// fakeDoctorReports is fakeDoctorDaemon answering with these reports, none at all when called with none.
func fakeDoctorReports(t *testing.T, reports ...runners.DoctorReport) func(...runners.DoctorReport) {
	t.Helper()
	home, err := os.MkdirTemp("", "pl")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	t.Setenv(EnvHome, home)
	t.Setenv("HOME", home)
	ln, err := net.Listen("unix", filepath.Join(home, SocketName))
	if err != nil {
		t.Fatal(err)
	}
	var body atomic.Value
	set := func(r ...runners.DoctorReport) {
		b, _ := json.Marshal(runners.DoctorResult{Reports: r})
		body.Store(b)
	}
	set(reports...)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(api.HeaderProtocol, api.WireProtocol)
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v0/runners/setup" {
			w.Write([]byte(`{"label":"pool"}`))
			return
		}
		w.Write(body.Load().([]byte))
	})}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return set
}

func passingReport(repo string) runners.DoctorReport {
	r := runners.DoctorReport{Repo: repo}
	for i := 1; i <= 17; i++ {
		r.Checks = append(r.Checks, runners.DoctorCheck{Number: i, Status: "pass", Message: "ok"})
	}
	return r
}

func runDoctor(t *testing.T, args ...string) (string, string, int) {
	t.Helper()
	var o, e bytes.Buffer
	code := Run(context.Background(), append([]string{"ci", "doctor"}, args...), strings.NewReader(""), &o, &e)
	return o.String(), e.String(), code
}

func probeCheckOf(t *testing.T, out string) runners.DoctorCheck {
	t.Helper()
	var r runners.DoctorResult
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	c := r.Reports[0].Checks
	if last := c[len(c)-1]; last.Number == 18 {
		return last
	}
	t.Fatalf("no check 18: %s", out)
	return runners.DoctorCheck{}
}

var scriptPathRe = regexp.MustCompile(`next: sh (\S+probe\.sh)`)

func TestCIDoctorProbePreparesAScriptForAPrivateRepo(t *testing.T) {
	fakeDoctorDaemon(t, passingReport("acme/app"))
	g := newFakeGH(t)
	g.answer(t, "repos/acme/app", map[string]any{"full_name": "acme/app", "private": true, "permissions": map[string]any{"push": true}})
	out, errout, code := runDoctor(t, "acme/app", "--probe", "--labels", "self-hosted,macos,pool")
	if code != 0 {
		t.Fatalf("exit %d: %s %s", code, out, errout)
	}
	for _, s := range []string{"18 warn Probe ready, not run", "self-hosted, macos, pool", "gh api --method POST \"repos/$repo/git/refs\""} {
		if !strings.Contains(out, s) {
			t.Errorf("missing %q in:\n%s", s, out)
		}
	}
	m := scriptPathRe.FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("no next step running the script:\n%s", out)
	}
	if fi, err := os.Stat(m[1]); err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("script %s: %v %v", m[1], fi, err)
	}
	// Preparing the probe writes nothing to GitHub.
	if log, _ := os.ReadFile(g.log); strings.Contains(string(log), "POST") || strings.Contains(string(log), "DELETE") {
		t.Fatalf("doctor wrote to GitHub:\n%s", log)
	}

	out, _, code = runDoctor(t, "acme/app", "--probe", "--labels", "self-hosted,macos,pool", "--json")
	if c := probeCheckOf(t, out); code != 0 || c.Status != "warn" || !strings.HasPrefix(c.Fix, "sh ") {
		t.Fatalf("exit %d, check 18 %+v", code, c)
	}
}

func TestCIDoctorProbeAsksFirstOnAPublicRepo(t *testing.T) {
	fakeDoctorDaemon(t, passingReport("acme/app"))
	g := newFakeGH(t)
	g.answer(t, "repos/acme/app", map[string]any{"full_name": "acme/app", "private": false, "permissions": map[string]any{"push": true}})
	out, errout, code := runDoctor(t, "acme/app", "--probe", "--labels", "self-hosted,macos,pool", "--json")
	c := probeCheckOf(t, out)
	if code == 0 || c.Status != "fail" || !strings.Contains(c.Message, "public") || !strings.HasSuffix(c.Fix, "potluck ci doctor acme/app --probe --labels self-hosted,macos,pool --public") {
		t.Fatalf("exit %d, check 18 %+v %s", code, c, errout)
	}
	if strings.Contains(out, "probe.sh") {
		t.Fatalf("a script was written without the owner's yes: %s", out)
	}
	out, _, code = runDoctor(t, "acme/app", "--probe", "--public", "--labels", "self-hosted,macos,pool", "--json")
	if c := probeCheckOf(t, out); code != 0 || c.Status != "warn" || !strings.HasPrefix(c.Fix, "sh ") {
		t.Fatalf("with --public: exit %d, check 18 %+v", code, c)
	}
}

func TestCIDoctorProbeRefusals(t *testing.T) {
	failing := passingReport("acme/app")
	failing.Checks[2].Status = "fail"
	answer := fakeDoctorDaemon(t, failing)
	g := newFakeGH(t)
	g.answer(t, "repos/acme/app", map[string]any{"full_name": "acme/app", "private": true, "permissions": map[string]any{"push": true}})
	out, _, code := runDoctor(t, "acme/app", "--probe", "--labels", "self-hosted,macos,pool", "--json")
	if c := probeCheckOf(t, out); code == 0 || c.Status != "warn" || !strings.Contains(c.Message, "not prepared") || strings.HasPrefix(c.Fix, "sh ") {
		t.Fatalf("a failing doctor still prepared the probe: exit %d, %+v", code, c)
	}

	answer(passingReport("acme/app"))
	g.answer(t, "repos/acme/app", map[string]any{"full_name": "acme/app", "private": true, "permissions": map[string]any{"push": false}})
	out, _, code = runDoctor(t, "acme/app", "--probe", "--labels", "self-hosted,macos,pool", "--json")
	if c := probeCheckOf(t, out); code == 0 || c.Status != "fail" || !strings.Contains(c.Message, "cannot push") {
		t.Fatalf("no push access: exit %d, %+v", code, c)
	}

	for _, args := range [][]string{{"--probe"}, {"acme/app", "--public"}, {"acme/app", "--labels", "x"}} {
		if _, errout, code := runDoctor(t, args...); code == 0 || !strings.Contains(errout, "--probe") {
			t.Errorf("%v: exit %d: %s", args, code, errout)
		}
	}
}

// probeGH is a gh standing in for GitHub while the probe script runs. FAKE_RUN_AFTER is how many run listings
// find nothing before run 42 appears (never, when empty); FAKE_RUN_STATE is that run's status and conclusion.
// FAKE_REF_LATE: the branch's push fails, and reads of the branch answer 404 that many times before it is there.
const probeGHScript = `#!/bin/sh
echo "$*" >> "$FAKE_GH_LOG"
case "$*" in
"api --method POST repos/acme/app/git/trees --input "*)
  cp "$6" "$FAKE_GH_DIR/tree.json"; echo tree1 ;;
"api --method POST repos/acme/app/git/commits "*) cat > "$FAKE_GH_DIR/commit.json"; echo commit1 ;;
"api --method POST repos/acme/app/git/refs "*)
  if [ -n "$FAKE_REF_FAIL" ]; then echo "gh: Resource not accessible (HTTP 403)" >&2; exit 1; fi
  if [ -n "$FAKE_REF_MADE" ] || [ -n "$FAKE_REF_LATE" ]; then echo "gh: gateway timeout (HTTP 504)" >&2; exit 1; fi; echo '{}' ;;
"api --method DELETE repos/acme/app/git/refs/heads/"*)
  if [ -n "$FAKE_REF_FAIL" ] || [ -n "$FAKE_DELETE_FAIL" ] || [ -n "$FAKE_OFFLINE_END" ]; then echo "gh: refused (HTTP 422)" >&2; exit 1; fi ;;
"api repos/acme/app/git/ref/heads/"*)
  if [ -n "$FAKE_REF_LATE" ]; then
    n=$(cat "$FAKE_GH_DIR/refreads" 2>/dev/null || echo 0); echo $((n + 1)) > "$FAKE_GH_DIR/refreads"
    if [ "$n" -ge "$FAKE_REF_LATE" ]; then echo '{}'; exit 0; fi
  fi
  if [ -n "$FAKE_DELETE_FAIL" ] || [ -n "$FAKE_REF_MADE" ]; then echo '{}'; exit 0; fi
  if [ -n "$FAKE_OFFLINE_END" ]; then echo "error connecting to api.github.com" >&2; exit 1; fi
  echo "gh: Not Found (HTTP 404)" >&2; exit 1 ;;
"run list "*)
  n=$(cat "$FAKE_GH_DIR/lists" 2>/dev/null || echo 0); echo $((n + 1)) > "$FAKE_GH_DIR/lists"
  if [ -n "$FAKE_LIST_FLAKY" ] && [ "$n" -eq 0 ]; then echo "HTTP 502" >&2; exit 1; fi
  if [ -n "$FAKE_RUN_AFTER" ] && [ "$n" -ge "$FAKE_RUN_AFTER" ]; then echo 42; fi ;;
"run view 42 -R acme/app --json "*) echo "$FAKE_RUN_STATE" ;;
"run view 42 -R acme/app --log")
  if [ -n "$FAKE_NO_LOG" ]; then exit 1; fi
  printf 'probe\tReport\t2026-11-12T10:00:00Z say() { printf %s-probe: %s\\n potluck "$1"; }\n'
  printf 'probe\tReport\t2026-11-12T10:00:01Z potluck-probe: os=Darwin 25.0 arm64\n'
  printf 'probe\tReport\t2026-11-12T10:00:01Z potluck-probe: xcode=Xcode 26.3\n' ;;
"run cancel 42 -R acme/app") if [ -n "$FAKE_CANCEL_FAIL" ]; then exit 1; fi ;;
*) echo "probe gh: unexpected $*" >&2; exit 2 ;;
esac
`

func runProbeScript(t *testing.T, after, state string, env ...string) (string, string, int) {
	t.Helper()
	fakeDoctorDaemon(t, passingReport("acme/app"))
	g := newFakeGH(t)
	g.answer(t, "repos/acme/app", map[string]any{"full_name": "acme/app", "private": true, "permissions": map[string]any{"push": true}})
	out, errout, code := runDoctor(t, "acme/app", "--probe", "--labels", "self-hosted,macos,pool")
	m := scriptPathRe.FindStringSubmatch(out)
	if code != 0 || m == nil {
		t.Fatalf("exit %d: %s %s", code, out, errout)
	}
	bin := filepath.Dir(firstPathGH(t))
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(probeGHScript), 0o755); err != nil {
		t.Fatal(err)
	}
	// A sleep that does not wait (the script's poll is at least a second), or fails when asked to, for a cleanup
	// whose commands fail.
	if err := os.WriteFile(filepath.Join(bin, "sleep"), []byte("#!/bin/sh\n[ -z \"$FAKE_SLEEP_FAIL\" ] || exit 1\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(g.log, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", m[1])
	cmd.Env = append(append(os.Environ(), "POTLUCK_PROBE_POLL=1", "FAKE_RUN_AFTER="+after, "FAKE_RUN_STATE="+state), env...)
	var o, e bytes.Buffer
	cmd.Stdout, cmd.Stderr = &o, &e
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(env, "INTERRUPT=1") {
		// Stop the script once it is waiting on the run, as a person pressing ^C would.
		for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(20 * time.Millisecond) {
			if b, _ := os.ReadFile(g.log); strings.Contains(string(b), "run view 42") {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("the script never waited on the run")
			}
		}
		cmd.Process.Signal(os.Interrupt)
	}
	err := cmd.Wait()
	code = 0
	if err != nil {
		ee, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatal(err)
		}
		code = ee.ExitCode()
	}
	log, _ := os.ReadFile(g.log)
	return o.String() + e.String(), string(log), code
}

// firstPathGH is the fake gh newFakeGH put first on PATH.
func firstPathGH(t *testing.T) string {
	t.Helper()
	p, err := exec.LookPath("gh")
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestProbeScriptPushesWaitsReportsAndDeletes(t *testing.T) {
	out, log, code := runProbeScript(t, "1", "completed success")
	if code != 0 {
		t.Fatalf("exit %d:\n%s\n%s", code, out, log)
	}
	for _, s := range []string{"  os=Darwin 25.0 arm64", "  xcode=Xcode 26.3", "pass: ", "Deleted branch potluck/probe-"} {
		if !strings.Contains(out, s) {
			t.Errorf("missing %q in:\n%s", s, out)
		}
	}
	if strings.Contains(out, "%s-probe") {
		t.Errorf("the job's script was reported as its output:\n%s", out)
	}
	// One commit with an empty parents list, sent as JSON: a parent could start other workflows, and
	// -f 'parents[]' needs a recent gh.
	var commit struct {
		Message string
		Tree    string
		Parents []string
	}
	b, err := os.ReadFile(filepath.Join(os.Getenv("FAKE_GH_DIR"), "commit.json"))
	if err != nil || json.Unmarshal(b, &commit) != nil || commit.Parents == nil || len(commit.Parents) != 0 ||
		commit.Tree != "tree1" || commit.Message == "" || strings.Contains(log, "parents[]") {
		t.Errorf("the probe commit is not one parentless commit of the tree: %s %v\n%s", b, err, log)
	}
	if !strings.Contains(log, "api --method DELETE repos/acme/app/git/refs/heads/potluck/probe-") || strings.Contains(log, "run cancel") {
		t.Errorf("branch not deleted, or a finished run cancelled:\n%s", log)
	}
	// The pushed tree is the one generated workflow, started by a push to the probe branch only.
	b, err = os.ReadFile(filepath.Join(os.Getenv("FAKE_GH_DIR"), "tree.json"))
	if err != nil {
		t.Fatal(err)
	}
	var tree struct {
		Tree []struct{ Path, Content string }
	}
	if err := json.Unmarshal(b, &tree); err != nil || len(tree.Tree) != 1 || tree.Tree[0].Path != runners.ProbeWorkflowPath {
		t.Fatalf("tree %s: %v", b, err)
	}
	var wf struct {
		On struct{ Push struct{ Branches []string } } `yaml:"on"`
	}
	if err := yaml.Unmarshal([]byte(tree.Tree[0].Content), &wf); err != nil || len(wf.On.Push.Branches) != 1 ||
		!strings.Contains(log, "ref=refs/heads/"+wf.On.Push.Branches[0]) {
		t.Fatalf("workflow branch %v not the pushed branch (%v):\n%s", wf.On.Push.Branches, err, log)
	}
}

// One failed read while waiting is tried again, not the end of the probe.
func TestProbeScriptRidesOutAFailedPoll(t *testing.T) {
	out, log, code := runProbeScript(t, "1", "completed success", "FAKE_LIST_FLAKY=1")
	if code != 0 || !strings.Contains(out, "pass: ") || !strings.Contains(out, "Deleted branch") {
		t.Fatalf("exit %d:\n%s\n%s", code, out, log)
	}
}

// A finished job whose log does not come back is still a pass, and says the report is missing.
func TestProbeScriptSaysWhenTheLogIsMissing(t *testing.T) {
	out, log, code := runProbeScript(t, "0", "completed success", "FAKE_NO_LOG=1")
	if code != 0 || !strings.Contains(out, "log did not come back") || strings.Count(log, "--log") != 5 {
		t.Fatalf("exit %d:\n%s\n%s", code, out, log)
	}
}

func TestProbeScriptCleansUpWhenTheJobFailsOrNeverRuns(t *testing.T) {
	for _, tt := range []struct {
		name, after, state, want string
		env                      []string
		cancel                   bool
		couldNot                 string // what cleanup could not do, when it fails
	}{
		{"job failed", "0", "completed failure", "ended failure", nil, false, ""},
		{"no run", "", "", "GitHub started no run", []string{"POTLUCK_PROBE_WAIT=0"}, false, ""},
		// The run appears only after the wait gave up: cleanup still finds and cancels it.
		{"run appears late", "1", "queued ", "GitHub started no run", []string{"POTLUCK_PROBE_WAIT=0"}, true, ""},
		{"no machine", "0", "queued ", "no machine finished it", []string{"POTLUCK_PROBE_WAIT=1", "POTLUCK_PROBE_POLL=1"}, true, ""},
		{"interrupted", "0", "queued ", "", []string{"POTLUCK_PROBE_POLL=1", "INTERRUPT=1"}, true, ""},
		// The branch was never made (no workflow scope): the delete GitHub refuses is no leftover.
		{"branch refused", "", "", "workflow scope", []string{"FAKE_REF_FAIL=1"}, false, ""},
		{"delete refused", "0", "completed failure", "ended failure", []string{"FAKE_DELETE_FAIL=1"}, false, "Could not delete branch"},
		// The network goes at the end: a delete and a check that both fail are a branch that may be left.
		{"offline at the end", "0", "completed failure", "ended failure", []string{"FAKE_OFFLINE_END=1"}, false, "Could not delete branch"},
		// gh's error decides, and a branch that was made though the request failed still has its run cancelled.
		{"branch made though the push failed", "1", "queued ", "did not accept the probe's branch: gh: gateway timeout", []string{"POTLUCK_PROBE_WAIT=0", "FAKE_REF_MADE=1"}, true, ""},
		// The push failed and GitHub saved the branch only after the first read answered 404: a 404 is read again
		// before it is trusted, so the branch's run is still cancelled.
		{"branch saved after a 404", "1", "queued ", "did not accept the probe's branch: gh: gateway timeout", []string{"POTLUCK_PROBE_WAIT=0", "FAKE_REF_LATE=1"}, true, ""},
		// A command in cleanup that fails (here every sleep) must not skip the branch's delete.
		{"cleanup command fails", "", "", "GitHub started no run", []string{"POTLUCK_PROBE_WAIT=0", "FAKE_SLEEP_FAIL=1"}, false, ""},
		{"cancel refused", "0", "queued ", "no machine finished it", []string{"POTLUCK_PROBE_WAIT=0", "FAKE_CANCEL_FAIL=1"}, true, "Could not cancel run 42"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			out, log, code := runProbeScript(t, tt.after, tt.state, tt.env...)
			if code == 0 || !strings.Contains(out, tt.want) {
				t.Fatalf("exit %d, want %q:\n%s", code, tt.want, out)
			}
			if !strings.Contains(log, "api --method DELETE repos/acme/app/git/refs/heads/potluck/probe-") {
				t.Errorf("branch left behind:\n%s", log)
			}
			if got := strings.Contains(out, "Could not"); got != (tt.couldNot != "") || !strings.Contains(out, tt.couldNot) {
				t.Errorf("cleanup reported a failure %v, want %q:\n%s", got, tt.couldNot, out)
			}
			if got := strings.Contains(log, "run cancel 42"); got != tt.cancel {
				t.Errorf("run cancelled %v, want %v:\n%s", got, tt.cancel, log)
			}
		})
	}
}

// A wait or poll that is no whole number, or a poll under a second, is refused before anything is pushed.
func TestProbeScriptRefusesBadWaitAndPoll(t *testing.T) {
	for _, env := range [][]string{{"POTLUCK_PROBE_WAIT=soon"}, {"POTLUCK_PROBE_WAIT=99999999999999999999"}, {"POTLUCK_PROBE_POLL=fast"}, {"POTLUCK_PROBE_POLL=1.5"},
		{"POTLUCK_PROBE_POLL=0"}} {
		t.Run(strings.Join(env, " "), func(t *testing.T) {
			out, log, code := runProbeScript(t, "1", "completed success", env...)
			if code == 0 || !strings.Contains(out, "POTLUCK_PROBE_") || strings.Contains(log, "POST") || strings.Contains(log, "DELETE") {
				t.Errorf("exit %d, wrote to GitHub or did not say why:\n%s\n%s", code, out, log)
			}
		})
	}
}

// The script removes the directory it was written in, so a probe that ran leaves nothing in the temp directory.
func TestProbeScriptRemovesItsFiles(t *testing.T) {
	for _, state := range []string{"completed success", "completed failure"} {
		t.Run(state, func(t *testing.T) {
			if _, log, code := runProbeScript(t, "0", state); code != 0 && state == "completed success" {
				t.Fatalf("exit %d:\n%s", code, log)
			}
			if left, _ := filepath.Glob(filepath.Join(os.Getenv("TMPDIR"), "potluck-ci-probe-*")); len(left) > 0 {
				t.Errorf("left behind: %v", left)
			}
		})
	}
}

func TestCIDoctorProbeSaysWhenItCannotPrepare(t *testing.T) {
	// No checks came back at all: nothing says the job would get a machine.
	answer := fakeDoctorReports(t)
	g := newFakeGH(t)
	g.answer(t, "repos/acme/app", map[string]any{"private": true, "permissions": map[string]any{"push": true}})
	out, _, code := runDoctor(t, "acme/app", "--probe", "--labels", "self-hosted,macos,pool", "--json")
	if c := probeCheckOf(t, out); c.Status != "warn" || !strings.Contains(c.Message, "not prepared") || strings.HasPrefix(c.Fix, "sh ") || code != 0 {
		t.Fatalf("an empty doctor answer still prepared the probe: exit %d, %+v", code, c)
	}

	// A private repository the gh sign-in cannot see answers 404: that is a failure, as no push access is.
	answer(passingReport("acme/hidden"))
	out, _, code = runDoctor(t, "acme/hidden", "--probe", "--labels", "self-hosted,macos,pool", "--json")
	if c := probeCheckOf(t, out); code == 0 || c.Status != "fail" || !strings.Contains(c.Message, "cannot see") {
		t.Fatalf("a repository gh cannot see: exit %d, %+v", code, c)
	}

	// An internal repository reads as private but its whole enterprise sees the job: it needs the owner's yes.
	answer(passingReport("acme/app"))
	g.answer(t, "repos/acme/app", map[string]any{"private": true, "visibility": "internal", "permissions": map[string]any{"push": true}})
	out, _, code = runDoctor(t, "acme/app", "--probe", "--labels", "self-hosted,macos,pool", "--json")
	if c := probeCheckOf(t, out); code == 0 || c.Status != "fail" || !strings.Contains(c.Message, "internal") || !strings.HasSuffix(c.Fix, "--public") {
		t.Fatalf("an internal repository: exit %d, %+v", code, c)
	}

	// Labels that do not name the pool let another machine take the job, which proves nothing about the lent Mac; the
	// fix's placeholder is quoted so a pasted line is no shell redirect.
	g.answer(t, "repos/acme/app", map[string]any{"private": true, "visibility": "private", "permissions": map[string]any{"push": true}})
	out, _, code = runDoctor(t, "acme/app", "--probe", "--labels", "macos,macos-14", "--json")
	if c := probeCheckOf(t, out); code == 0 || c.Status != "fail" || !strings.Contains(c.Message, "pool would not take") || !strings.HasSuffix(c.Fix, "--labels '<labels>'") {
		t.Fatalf("GitHub-hosted labels: exit %d, %+v", code, c)
	}

	// An empty --os names no OS.
	out, _, code = runDoctor(t, "acme/app", "--probe", "--os", "", "--json")
	if c := probeCheckOf(t, out); code == 0 || c.Status != "fail" || !strings.Contains(c.Message, "--os") {
		t.Fatalf("--os '': exit %d, %+v", code, c)
	}
}

// The command a fix names to run again asks for the same labels, size and OS, on a private repository too.
func TestCIDoctorProbeRerunKeepsWhatWasAsked(t *testing.T) {
	failing := passingReport("acme/app")
	failing.Checks[2].Status = "fail"
	fakeDoctorDaemon(t, failing)
	out, _, _ := runDoctor(t, "acme/app", "--probe", "--labels", "self-hosted,linux,pool", "--size", "c4", "--os", "linux", "--json")
	if c := probeCheckOf(t, out); c.Fix != "fix that, then potluck ci doctor acme/app --probe --labels self-hosted,linux,pool --size c4 --os linux" {
		t.Fatalf("fix %q", c.Fix)
	}
}
