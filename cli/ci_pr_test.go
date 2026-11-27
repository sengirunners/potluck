package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sengirunners/potluck/internal/forkguard"
	"github.com/sengirunners/potluck/internal/workflowlint"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/tailnet/tailnettest"
)

// fakeGH is a gh on PATH standing in for GitHub: `gh api --method GET <path>` answers the file named for the path
// (each of / ? = & : % as _) in its directory, or a 404; every call is logged; `gh pr create` prints a URL.
type fakeGH struct{ dir, log string }

const fakeGHScript = `#!/bin/sh
echo "$*" >> "$FAKE_GH_LOG"
if [ "$1" = pr ] && [ "$2" = create ]; then echo https://github.com/acme/app/pull/7; exit 0; fi
if [ "$1" = api ] && [ "$2" = --method ] && [ "$3" = GET ]; then
  f="$FAKE_GH_DIR/$(printf %s "$4" | tr '/?=&:%' '______')"
  if [ -f "$f" ]; then cat "$f"; exit 0; fi
  echo "gh: Not Found (HTTP 404)" >&2; exit 1
fi
if [ "$1" = api ]; then echo '{}'; exit 0; fi
echo "fake gh: unexpected $*" >&2; exit 2
`

func newFakeGH(t *testing.T) *fakeGH {
	t.Helper()
	dir := t.TempDir()
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(fakeGHScript), 0o755); err != nil {
		t.Fatal(err)
	}
	g := &fakeGH{dir: dir, log: filepath.Join(t.TempDir(), "log")}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_GH_DIR", dir)
	t.Setenv("FAKE_GH_LOG", g.log)
	t.Setenv("TMPDIR", t.TempDir())
	g.answer(t, "repos/acme/app", map[string]any{"default_branch": "main"})
	g.answer(t, "repos/acme/app/git/ref/heads/main", map[string]any{"object": map[string]any{"sha": "1111111111111111111111111111111111111111"}})
	g.answer(t, "repos/acme/app/pulls?state=open&head=acme%3Apotluck%2Fruns-on-native", []any{})
	return g
}

func (g *fakeGH) answer(t *testing.T, path string, v any) {
	t.Helper()
	b, _ := json.Marshal(v)
	name := strings.NewReplacer("/", "_", "?", "_", "=", "_", "&", "_", ":", "_", "%", "_").Replace(path)
	if err := os.WriteFile(filepath.Join(g.dir, name), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// workflows serves these files as acme/app's .github/workflows at main.
func (g *fakeGH) workflows(t *testing.T, files map[string]string) {
	t.Helper()
	var list []any
	for name, text := range files {
		p := ".github/workflows/" + name
		list = append(list, map[string]any{"name": name, "path": p, "type": "file"})
		g.answer(t, "repos/acme/app/contents/"+p+"?ref=main", map[string]any{"path": p, "sha": "b10b", "encoding": "base64", "content": base64.StdEncoding.EncodeToString([]byte(text))})
	}
	g.answer(t, "repos/acme/app/contents/.github/workflows?ref=main", list)
}

const prWorkflow = `name: ui
on:
  pull_request:
  push:
    branches: [main]
permissions:
  contents: read
jobs:
  native:
    runs-on: macos-15 # Xcode
    steps:
      - run: xcodebuild
  web:
    runs-on: ubuntu-latest
    steps:
      - run: true
`

var prLabels = []string{"self-hosted", "macos", "pool:c4m16"}

func runCI(t *testing.T, args ...string) (string, string, int) {
	t.Helper()
	var o, e bytes.Buffer
	code := Run(context.Background(), append([]string{"ci", "pr"}, args...), strings.NewReader(""), &o, &e)
	return o.String(), e.String(), code
}

func finalLine(s string) string {
	l := strings.Split(strings.TrimRight(s, "\n"), "\n")
	return l[len(l)-1]
}

func TestCIPRDryRunPrintsTheDiff(t *testing.T) {
	g := newFakeGH(t)
	g.workflows(t, map[string]string{"ui.yml": prWorkflow, "other.yml": "on: push\njobs:\n  x:\n    runs-on: ubuntu-latest\n", "x&echo$IFS.yml": prWorkflow})
	out, errout, code := runCI(t, "acme/app", "--job", "native", "--labels", strings.Join(prLabels, ","), "--dry-run")
	if code != 0 {
		t.Fatalf("exit %d: %s %s", code, out, errout)
	}
	want, _ := forkguard.Expression("macos-15", prLabels)
	for _, s := range []string{"--- a/.github/workflows/ui.yml", "-    runs-on: macos-15 # Xcode", "+    runs-on: " + want + " # Xcode", "     steps:", "GitHub's macos-15"} {
		if !strings.Contains(out, s) {
			t.Errorf("missing %q in:\n%s", s, out)
		}
	}
	if !strings.Contains(out, `note: skipped ".github/workflows/x&echo$IFS.yml"`) {
		t.Errorf("a file name the script cannot carry was not skipped:\n%s", out)
	}
	if got := finalLine(out); got != "next: potluck ci pr acme/app --job native --labels self-hosted,macos,pool:c4m16 --workflow ui.yml" {
		t.Errorf("last line %q", got)
	}
	if b, _ := os.ReadFile(g.log); strings.Contains(string(b), "POST") || strings.Contains(string(b), "pr create") {
		t.Errorf("a dry run wrote to GitHub:\n%s", b)
	}
}

func TestCIPROpensThePullRequestWithGh(t *testing.T) {
	g := newFakeGH(t)
	// A temporary directory whose path needs shell quoting: the script's cleanup must still remove exactly it.
	spaced := filepath.Join(os.Getenv("TMPDIR"), "x y")
	if err := os.Mkdir(spaced, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", spaced)
	g.workflows(t, map[string]string{"ui.yml": prWorkflow})
	g.answer(t, "repos/acme/app/branches/potluck/runs-on-native", map[string]any{}) // an old branch: the next name is used
	g.answer(t, "repos/acme/app/pulls?state=open&head=acme%3Apotluck%2Fruns-on-native-2", []any{})
	out, errout, code := runCI(t, "acme/app", "--job", "native", "--labels", strings.Join(prLabels, ","))
	if code != 0 {
		t.Fatalf("exit %d: %s %s", code, out, errout)
	}
	script, ok := strings.CutPrefix(finalLine(out), "next: sh ")
	if !ok {
		t.Fatalf("last line is not the script: %s", out)
	}
	script = strings.Trim(script, "'")
	if !strings.Contains(out, "gh pr create -R acme/app --base main --head potluck/runs-on-native-2") {
		t.Fatalf("script not printed:\n%s", out)
	}
	if b, _ := os.ReadFile(g.log); strings.Contains(string(b), "POST") {
		t.Fatalf("the command wrote to GitHub itself:\n%s", b)
	}
	dir := filepath.Dir(script)
	var ref, put map[string]string
	for name, v := range map[string]*map[string]string{"ref.json": &ref, "put.json": &put} {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil || json.Unmarshal(b, v) != nil {
			t.Fatalf("%s: %v %s", name, err, b)
		}
	}
	cmd := exec.Command("sh", script)
	if b, err := cmd.CombinedOutput(); err != nil || !strings.Contains(string(b), "/pull/7") {
		t.Fatalf("script: %v %s", err, b)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("the script left its directory behind: %v", err)
	}
	log, _ := os.ReadFile(g.log)
	for _, s := range []string{"api --method POST repos/acme/app/git/refs --input", "api --method PUT repos/acme/app/contents/.github/workflows/ui.yml --input", "pr create -R acme/app --base main --head potluck/runs-on-native-2"} {
		if !strings.Contains(string(log), s) {
			t.Errorf("script did not run %q:\n%s", s, log)
		}
	}
	if ref["ref"] != "refs/heads/potluck/runs-on-native-2" || ref["sha"] != "1111111111111111111111111111111111111111" {
		t.Errorf("ref %v", ref)
	}
	text, _ := base64.StdEncoding.DecodeString(put["content"])
	c, _ := forkguard.Apply([]byte(prWorkflow), "native", prLabels)
	if put["sha"] != "b10b" || put["branch"] != "potluck/runs-on-native-2" || string(text) != string(c.Text) {
		t.Errorf("put %v\n%s", put, text)
	}
	if p := workflowlint.Lint(string(text)); len(p) > 0 {
		t.Errorf("committed workflow fails the lint: %v", p)
	}
}

func TestCIPRIdempotentAndOpen(t *testing.T) {
	g := newFakeGH(t)
	c, err := forkguard.Apply([]byte(prWorkflow), "native", prLabels)
	if err != nil {
		t.Fatal(err)
	}
	g.workflows(t, map[string]string{"ui.yml": string(c.Text)})
	out, errout, code := runCI(t, "acme/app", "--job", "native", "--labels", strings.Join(prLabels, ","))
	if code != 0 || finalLine(out) != "done" {
		t.Fatalf("already changed: exit %d %s %s", code, out, errout)
	}

	g.workflows(t, map[string]string{"ui.yml": prWorkflow})
	g.answer(t, "repos/acme/app/pulls?state=open&head=acme%3Apotluck%2Fruns-on-native", []any{map[string]any{"html_url": "https://github.com/acme/app/pull/3"}})
	out, errout, code = runCI(t, "acme/app", "--job", "native", "--labels", strings.Join(prLabels, ","))
	if code != 0 || finalLine(out) != "next: merge https://github.com/acme/app/pull/3" {
		t.Fatalf("open pull request: exit %d %s %s", code, out, errout)
	}
}

func TestCIPRRefusals(t *testing.T) {
	g := newFakeGH(t)
	g.workflows(t, map[string]string{
		"ui.yml":  strings.Replace(prWorkflow, "runs-on: macos-15 # Xcode", "runs-on: ${{ matrix.os }}", 1),
		"two.yml": "on: push\njobs:\n  web:\n    runs-on: ubuntu-latest\n",
	})
	for _, tt := range []struct {
		args []string
		want string
	}{
		{[]string{"--job", "native"}, "matrix"},
		{[]string{"--job", "nope"}, "no workflow in acme/app"},
		{[]string{"--job", "web"}, "several workflows"},
		{[]string{"--job", "web", "--workflow", "missing.yml"}, "not found on GitHub"},
		{[]string{"--job", "nope", "--workflow", "two.yml"}, "no such job"},
		{[]string{"--job", "web", "--workflow", "../x.yml"}, ".github/workflows"},
		{[]string{"--job", "web", "--workflow", "put.json"}, ".github/workflows"},
		{[]string{"--job", "web", "--labels", "self-hosted,pool"}, "must name the OS"},
		{[]string{}, "runs on a GitHub-hosted macOS label"}, // the matrix job is no macOS job
		{[]string{"--workflow", "two.yml"}, "runs on a GitHub-hosted macOS label"},
	} {
		out, errout, code := runCI(t, append([]string{"acme/app", "--labels", "self-hosted,macos,pool"}, tt.args...)...)
		if code == 0 || !strings.Contains(errout, tt.want) {
			t.Errorf("%v: exit %d, want %q: %s %s", tt.args, code, tt.want, out, errout)
		}
	}
}

// Without --labels the labels are this machine's pool's, as the broker reads them.
func TestCIPRUsesThePoolLabel(t *testing.T) {
	m := startMachine(t, tailnettest.New(), "mini", "judy@example.com")
	if _, err := client.NewUnix(m.dm.Deps.Config.SocketPath()).CreateProject(context.Background(), api.NewProjectRequest{Slug: "pool"}); err != nil {
		t.Fatal(err)
	}
	m.ok(t, "runners", "setup", "pool", "--url", "https://pool.example")
	g := newFakeGH(t)
	g.workflows(t, map[string]string{"ui.yml": prWorkflow})
	t.Setenv(EnvHome, m.home)
	out, errout, code := runCI(t, "acme/app", "--job", "native", "--size", "c4m16", "--dry-run")
	if code != 0 || !strings.Contains(out, `'["self-hosted","macos","pool:c4m16"]'`) {
		t.Fatalf("exit %d: %s %s", code, out, errout)
	}
	if _, errout, code := runCI(t, "acme/app", "--job", "native", "--os", "solaris", "--dry-run"); code == 0 || !strings.Contains(errout, "solaris") {
		t.Fatalf("an OS the pool does not serve was accepted: %s", errout)
	}
}

// --labels may be a label of the pool's runners.labels map alone when its entry names the OS, as the broker reads
// it; one whose entry names none, or the pool's own label, is refused for lacking an OS, as before.
func TestCIPRLabelsAcceptsAMappedLabelThatNamesTheOS(t *testing.T) {
	m := startMachine(t, tailnettest.New(), "mini", "judy@example.com")
	if _, err := client.NewUnix(m.dm.Deps.Config.SocketPath()).CreateProject(context.Background(), api.NewProjectRequest{Slug: "pool"}); err != nil {
		t.Fatal(err)
	}
	m.ok(t, "runners", "setup", "pool", "--url", "https://pool.example")
	m.dm.Deps.Config.File.Runners.Labels = map[string]config.RunnersLabel{
		"mac-box": {Size: "c4m16", OS: "macos"},
		"big-box": {Size: "c4m16"},
	}
	g := newFakeGH(t)
	g.workflows(t, map[string]string{"ui.yml": prWorkflow})
	t.Setenv(EnvHome, m.home)
	out, errout, code := runCI(t, "acme/app", "--job", "native", "--labels", "mac-box", "--dry-run")
	if code != 0 {
		t.Fatalf("a mapped label that names the OS: exit %d: %s %s", code, out, errout)
	}
	if !strings.Contains(out, `'["mac-box"]'`) || !strings.Contains(out, "fromJSON") {
		t.Errorf("the guarded runs-on does not ask for the mapped label alone, with the fork guard:\n%s", out)
	}
	m.dm.Deps.Config.File.Runners.Labels["broken"] = config.RunnersLabel{Size: "c4m16", OS: "plan9"}
	if _, errout, code := runCI(t, "acme/app", "--job", "native", "--labels", "broken", "--dry-run"); code == 0 || !strings.Contains(errout, "plan9") {
		t.Errorf("a mapped entry the broker cannot serve: exit %d, want its reason: %s", code, errout)
	}
	for _, labels := range []string{"big-box", "self-hosted,pool", "nope"} {
		if out, errout, code := runCI(t, "acme/app", "--job", "native", "--labels", labels, "--dry-run"); code == 0 || !strings.Contains(errout, "must name the OS") {
			t.Errorf("--labels %s: exit %d, want a refusal naming the OS: %s %s", labels, code, out, errout)
		}
	}
}

// Without --job the one job on a GitHub-hosted macOS label is the one; none or several is refused, saying which.
func TestCIPRPicksTheMacJob(t *testing.T) {
	g := newFakeGH(t)
	g.workflows(t, map[string]string{"ui.yml": prWorkflow, "web.yml": "on: push\njobs:\n  web:\n    runs-on: ubuntu-latest\n"})
	out, errout, code := runCI(t, "acme/app", "--labels", strings.Join(prLabels, ","), "--dry-run")
	if code != 0 {
		t.Fatalf("exit %d: %s %s", code, out, errout)
	}
	if got := finalLine(out); got != "next: potluck ci pr acme/app --labels self-hosted,macos,pool:c4m16 --job native --workflow ui.yml" {
		t.Errorf("last line %q", got)
	}

	// The same id on another runner in another file does not hide the picked job.
	g.workflows(t, map[string]string{"ui.yml": prWorkflow, "web.yml": "on: push\njobs:\n  native:\n    runs-on: ubuntu-latest\n"})
	if out, errout, code := runCI(t, "acme/app", "--labels", strings.Join(prLabels, ","), "--dry-run"); code != 0 || !strings.Contains(out, "--- a/.github/workflows/ui.yml") {
		t.Errorf("a job id shared with another file: exit %d %s %s", code, out, errout)
	}

	g.workflows(t, map[string]string{"ui.yml": prWorkflow, "two.yml": "on: push\njobs:\n  ios:\n    runs-on: macos-26\n"})
	_, errout, code = runCI(t, "acme/app", "--labels", strings.Join(prLabels, ","), "--dry-run")
	if code == 0 || !strings.Contains(errout, "2 jobs") || !strings.Contains(errout, "native (.github/workflows/ui.yml)") || !strings.Contains(errout, "ios (.github/workflows/two.yml)") {
		t.Errorf("two macOS jobs: exit %d %s", code, errout)
	}
}

type ciPRJSON struct {
	State, Repo, Job, Workflow, URL, Script, Next, Diff string
	Labels, Notes                                       []string
}

func ciJSON(t *testing.T, args ...string) ciPRJSON {
	t.Helper()
	out, errout, code := runCI(t, append([]string{"acme/app", "--json", "--labels", strings.Join(prLabels, ",")}, args...)...)
	if code != 0 {
		t.Fatalf("exit %d: %s %s", code, out, errout)
	}
	var r ciPRJSON
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		t.Fatalf("stdout is not one JSON object: %v\n%s", err, out)
	}
	return r
}

// With --json every end of the command is one object, so a caller reads fields and parses no text.
func TestCIPRAnswersJSON(t *testing.T) {
	g := newFakeGH(t)
	g.workflows(t, map[string]string{"ui.yml": prWorkflow})

	r := ciJSON(t, "--dry-run")
	if r.State != "dry_run" || r.Job != "native" || r.Workflow != ".github/workflows/ui.yml" || !strings.Contains(r.Diff, "+    runs-on: ") || !strings.HasSuffix(r.Next, "--workflow ui.yml") {
		t.Errorf("dry run: %+v", r)
	}
	// next runs as given: no global flag in it.
	if strings.Contains(r.Next, "json") {
		t.Errorf("next carries --json: %q", r.Next)
	}
	args := strings.Fields(strings.TrimPrefix(r.Next, "potluck ci pr "))
	if _, errout, code := runCI(t, append(args, "--dry-run")...); code != 0 {
		t.Errorf("next does not run: %q: %s", r.Next, errout)
	}

	r = ciJSON(t)
	if r.State != "script" || r.Job != "native" || r.Next != "sh "+shellQuote(r.Script) || !strings.HasSuffix(r.Script, "open-pr.sh") || !strings.Contains(r.Diff, "--- a/") {
		t.Errorf("script: %+v", r)
	}
	if _, err := os.Stat(r.Script); err != nil {
		t.Errorf("script path is not a file: %v", err)
	}

	g.answer(t, "repos/acme/app/pulls?state=open&head=acme%3Apotluck%2Fruns-on-native", []any{map[string]any{"html_url": "https://github.com/acme/app/pull/3"}})
	r = ciJSON(t)
	if r.State != "merge" || r.URL != "https://github.com/acme/app/pull/3" || r.Script != "" {
		t.Errorf("merge: %+v", r)
	}

	c, _ := forkguard.Apply([]byte(prWorkflow), "native", prLabels)
	g.workflows(t, map[string]string{"ui.yml": string(c.Text)})
	r = ciJSON(t)
	if r.State != "done" || r.Job != "native" || r.Diff != "" || r.Next != "" {
		t.Errorf("done: %+v", r)
	}

	// A refusal is still an error on stderr, never an object on stdout.
	out, errout, code := runCI(t, "acme/app", "--json", "--job", "nope", "--labels", strings.Join(prLabels, ","))
	if code == 0 || out != "" || !strings.Contains(errout, "no workflow") {
		t.Errorf("refusal: exit %d %q %q", code, out, errout)
	}
}
