package depscli_test

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/pflag"

	"github.com/sengirunners/potluck/boxpush"
	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/depscli"
)

const lockV3 = `{"name":"app","lockfileVersion":3,"packages":{"":{"name":"app","workspaces":["packages/*"]},"packages/a":{"name":"a"},"node_modules/a":{"resolved":"packages/a","link":true},"node_modules/left-pad":{"version":"1.3.0"}}}`

// npmProject writes a project directory: files is path -> content.
func npmProject(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for p, c := range files {
		full := filepath.Join(dir, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// depsRequest runs the flags as the commands do and answers the request.
func depsRequest(t *testing.T, dir string, args ...string) (computeapi.AcquireRunTargetRequest, error) {
	t.Helper()
	var d depscli.Flags
	f := pflag.NewFlagSet("t", pflag.ContinueOnError)
	d.Add(f)
	if err := f.Parse(args); err != nil {
		t.Fatal(err)
	}
	var req computeapi.AcquireRunTargetRequest
	err := d.Request(context.Background(), f, dir, "checkout", &req)
	return req, err
}

func TestDepsFlagsReadTheProject(t *testing.T) {
	dir := npmProject(t, map[string]string{
		"package-lock.json":       lockV3,
		"package.json":            `{"name":"app"}`,
		"packages/a/package.json": `{"name":"a"}`,
		"packages/b/package.json": `{"name":"b"}`, // not in the lockfile: not sent
		"node_modules/x/index.js": "x",
		"src/index.js":            "not sent",
		"packages/a/src/index.js": "not sent",
	})
	req, err := depsRequest(t, dir, "--deps", "npm", "--deps-scripts", "off", "--deps-wait", "0")
	if err != nil {
		t.Fatal(err)
	}
	if len(req.Deps) != 1 || req.Deps[0].Kind != "npm" || req.Deps[0].Path != "checkout" || req.Deps[0].Scripts != computeapi.DepsScriptsOff {
		t.Fatalf("deps: %+v", req.Deps)
	}
	var got []string
	for _, f := range req.Deps[0].Files {
		got = append(got, f.Path)
	}
	if want := "package-lock.json package.json packages/a/package.json"; strings.Join(got, " ") != want {
		t.Fatalf("files %q, want %q", got, want)
	}
	if req.DepsWaitS == nil || *req.DepsWaitS != 0 {
		t.Fatalf("deps_wait_s %v: --deps-wait 0 must be sent as 0", req.DepsWaitS)
	}
	// Defaults: scripts on (sent as nothing), the daemon's wait (sent as its default).
	req, err = depsRequest(t, dir, "--deps", "npm:work/app")
	if err != nil {
		t.Fatal(err)
	}
	if req.Deps[0].Path != "work/app" || req.Deps[0].Scripts != "" || *req.DepsWaitS != computeapi.DepsWaitDefaultS {
		t.Fatalf("defaults: %+v wait %d", req.Deps[0], *req.DepsWaitS)
	}
	// No --deps: the request is untouched.
	if req, err = depsRequest(t, dir); err != nil || req.Deps != nil || req.DepsWaitS != nil {
		t.Fatalf("no deps: %+v %v", req, err)
	}
}

func TestDepsFlagsRefusals(t *testing.T) {
	ok := map[string]string{"package-lock.json": lockV3, "package.json": "{}", "packages/a/package.json": "{}"}
	outside := npmProject(t, map[string]string{"package-lock.json": lockV3, "package.json": "{}", "a/package.json": "{}"})
	escape := npmProject(t, ok)
	for name, link := range map[string][2]string{
		"package.json":             {filepath.Join(outside, "package.json"), "package.json"},
		"package-lock.json":        {filepath.Join(outside, "package-lock.json"), "package-lock.json"},
		"a workspace directory":    {filepath.Join(outside, "a"), "packages/a"},
		"a workspace's file":       {filepath.Join(outside, "a/package.json"), "packages/a/package.json"},
		"a symlink to an absolute": {"/etc/passwd", "package.json"},
	} {
		d := npmProject(t, ok)
		full := filepath.Join(d, link[1])
		if err := os.RemoveAll(full); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(link[0], full); err != nil {
			t.Fatal(err)
		}
		if req, err := depsRequest(t, d, "--deps", "npm"); err == nil {
			t.Errorf("%s: a symlink out of the project was followed: %+v", name, req.Deps)
		}
	}
	_ = escape

	cases := []struct {
		name  string
		files map[string]string
		args  []string
		want  string
	}{
		{"no lockfile", map[string]string{"package.json": "{}"}, []string{"--deps", "npm"}, "package-lock.json"},
		{"lockfile v1", map[string]string{"package-lock.json": `{"lockfileVersion":1}`, "package.json": "{}"}, []string{"--deps", "npm"}, "version"},
		{"workspace outside", map[string]string{"package-lock.json": `{"lockfileVersion":3,"packages":{"":{},"../x":{}}}`, "package.json": "{}"}, []string{"--deps", "npm"}, "outside the project"},
		{"workspace missing", map[string]string{"package-lock.json": lockV3, "package.json": "{}"}, []string{"--deps", "npm"}, "packages/a/package.json"},
		{"unknown kind", ok, []string{"--deps", "gem"}, `no dependency layer kind "gem"`},
		{"absolute path", ok, []string{"--deps", "npm:/tmp/x"}, "not usable as a path"},
		{"dotdot path", ok, []string{"--deps", "npm:../x"}, "not usable as a path"},
		{"scripts", ok, []string{"--deps", "npm", "--deps-scripts", "maybe"}, "on or off"},
		{"wait too long", ok, []string{"--deps", "npm", "--deps-wait", "99999"}, "--deps-wait"},
		{"wait negative", ok, []string{"--deps", "npm", "--deps-wait", "-1"}, "--deps-wait"},
		{"wait alone", ok, []string{"--deps-wait", "5"}, "goes with --deps"},
		{"scripts alone", ok, []string{"--deps-scripts", "off"}, "goes with --deps"},
		{"too many", ok, []string{"--deps", "npm:a", "--deps", "npm:b", "--deps", "npm:c", "--deps", "npm:d", "--deps", "npm:e"}, "at most"},
		{"overlap is the daemon's", ok, []string{"--deps", "npm:a", "--deps", "npm:a/b"}, ""},
	}
	for _, c := range cases {
		_, err := depsRequest(t, npmProject(t, c.files), c.args...)
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%s: %v", c.name, err)
		case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
			t.Errorf("%s: error %v, want %q", c.name, err, c.want)
		}
	}
	// A directory where a file belongs is not read.
	d := npmProject(t, ok)
	_ = os.Remove(filepath.Join(d, "package.json"))
	if err := os.Mkdir(filepath.Join(d, "package.json"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := depsRequest(t, d, "--deps", "npm"); err == nil || !strings.Contains(err.Error(), "regular file") {
		t.Fatalf("a directory as package.json: %v", err)
	}
}

// A symlink that stays inside the project is the project's own file.
func TestDepsFlagsFollowSymlinksInsideOnly(t *testing.T) {
	d := npmProject(t, map[string]string{"package-lock.json": lockV3, "package.json": "{}", "shared/a.json": `{"name":"a"}`})
	if err := os.MkdirAll(filepath.Join(d, "packages/a"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../../shared/a.json", filepath.Join(d, "packages/a/package.json")); err != nil {
		t.Fatal(err)
	}
	req, err := depsRequest(t, d, "--deps", "npm")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range req.Deps[0].Files {
		if f.Path == "packages/a/package.json" && string(f.Data) == `{"name":"a"}` {
			return
		}
	}
	t.Fatalf("the inner link's content was not sent: %+v", req.Deps[0].Files)
}

// The limits hold all layers together, as the daemon's do.
func TestDepsFlagsLimitsCountAllLayers(t *testing.T) {
	big := `{"name":"app","pad":"` + strings.Repeat("x", 9<<20) + `"}`
	d := npmProject(t, map[string]string{"package-lock.json": `{"lockfileVersion":3,"packages":{"":{}}}`, "package.json": big})
	if _, err := depsRequest(t, d, "--deps", "npm:a"); err != nil {
		t.Fatal(err)
	}
	if _, err := depsRequest(t, d, "--deps", "npm:a", "--deps", "npm:b"); err == nil || !strings.Contains(err.Error(), "16 MiB") {
		t.Fatalf("two 9 MiB layers: %v", err)
	}
}

func TestPrintDeps(t *testing.T) {
	asked := []computeapi.BoxDeps{{Kind: "npm", Path: "checkout"}}
	key := strings.Repeat("ab", 32)
	cases := []struct {
		name string
		got  []computeapi.BoxDepsAnswer
		want string
	}{
		{"hit", []computeapi.BoxDepsAnswer{{Kind: "npm", Key: key, Hit: true}}, "potluck: deps npm at checkout: hit (key abababababab)\n"},
		{"built", []computeapi.BoxDepsAnswer{{Kind: "npm", Key: key, Built: true}}, "potluck: deps npm at checkout: built (key abababababab)\n"},
		{"neither", []computeapi.BoxDepsAnswer{{Kind: "npm", Key: key}}, "potluck: deps npm at checkout: not mounted (install for yourself)\n"},
		{"unreported", nil, "potluck: deps npm at checkout: not reported (not mounted: install for yourself)\n"},
		{"another kind", []computeapi.BoxDepsAnswer{{Kind: "go", Key: key, Hit: true}}, "potluck: deps npm at checkout: not reported (not mounted: install for yourself)\n"},
	}
	for _, c := range cases {
		var w bytes.Buffer
		depscli.Print(&w, "potluck", asked, c.got)
		if w.String() != c.want {
			t.Errorf("%s: %q, want %q", c.name, w.String(), c.want)
		}
	}
	var w bytes.Buffer
	depscli.Print(&w, "potluck", nil, []computeapi.BoxDepsAnswer{{Kind: "npm", Hit: true}})
	if w.Len() != 0 {
		t.Fatalf("no deps asked, printed %q", w.String())
	}
	depscli.Print(&w, "potluck", []computeapi.BoxDeps{{Kind: "npm", Path: "a"}, {Kind: "npm", Path: "b"}},
		[]computeapi.BoxDepsAnswer{{Kind: "npm", Key: key, Hit: true}, {Kind: "npm", Key: key, Built: true}})
	if strings.Count(w.String(), "\n") != 1 || !strings.Contains(w.String(), "a: hit") || !strings.Contains(w.String(), "b: built") {
		t.Fatalf("two layers on one line: %q", w.String())
	}
}

// gitProject makes a git checkout of files (committed) and returns a runner of git in dir.
func gitProject(t *testing.T, files map[string]string) (dir string, git func(args ...string)) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	dir = npmProject(t, files)
	git = func(args ...string) {
		t.Helper()
		c := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)...)
		c.Dir = dir
		c.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-q")
	git("add", ".")
	git("commit", "-q", "-m", "x")
	return dir, git
}

func TestDepsMismatchWarns(t *testing.T) {
	d, _ := gitProject(t, map[string]string{"package-lock.json": lockV3, "package.json": "{}", "packages/a/package.json": "{}"})
	warn := func(ref string, dirty, untracked bool) string {
		t.Helper()
		req, err := depsRequest(t, d, "--deps", "npm")
		if err != nil {
			t.Fatal(err)
		}
		var w bytes.Buffer
		depscli.WarnMismatch(context.Background(), &w, "potluck", d, ref, dirty, untracked, req.Deps)
		return w.String()
	}
	if w := warn("", false, false); w != "" {
		t.Fatalf("a clean tree warned: %q", w)
	}
	// A changed tracked file: warns, unless --dirty sends it.
	if err := os.WriteFile(filepath.Join(d, "package.json"), []byte(`{"dependencies":{"x":"1"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if w := warn("", false, false); !strings.Contains(w, "warning") || !strings.Contains(w, "package.json") {
		t.Fatalf("a changed package.json did not warn: %q", w)
	}
	if w := warn("", true, false); w != "" {
		t.Fatalf("--dirty sends a tracked change, yet: %q", w)
	}
}

// A file git does not track, or ignores, is not in the checkout whatever the
// tree says; --dirty sends untracked ones only with --include-untracked.
func TestMismatchUntrackedAndIgnored(t *testing.T) {
	d, git := gitProject(t, map[string]string{"package.json": "{}", ".gitignore": "ignored.json\n"})
	for _, f := range []string{"new.json", "ignored.json"} {
		if err := os.WriteFile(filepath.Join(d, f), []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ctx := context.Background()
	paths := []string{"package.json", "new.json", "ignored.json"}
	got := func(dirty, untracked bool) string {
		t.Helper()
		bad, err := boxpush.Mismatch(ctx, d, "", dirty, untracked, paths)
		if err != nil {
			t.Fatal(err)
		}
		return strings.Join(bad, " ")
	}
	if g := got(false, false); g != "new.json ignored.json" {
		t.Errorf("committed push: %q", g)
	}
	if g := got(true, false); g != "new.json ignored.json" {
		t.Errorf("--dirty: %q", g)
	}
	if g := got(true, true); g != "ignored.json" { // an ignored file is never sent
		t.Errorf("--dirty --include-untracked: %q", g)
	}
	git("add", "new.json")
	if g := got(true, false); g != "ignored.json" {
		t.Errorf("--dirty with new.json staged: %q", g)
	}
}

// A committed symlink in the project is the same file on a clean tree.
func TestMismatchSymlinkInTheProject(t *testing.T) {
	d, _ := gitProject(t, map[string]string{"real/package.json": "{}"})
	if err := os.Symlink("real/package.json", filepath.Join(d, "package.json")); err != nil {
		t.Fatal(err)
	}
	c := exec.Command("git", "-c", "user.name=t", "-c", "user.email=t@example.com", "add", "-A")
	c.Dir = d
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	c = exec.Command("git", "-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false", "commit", "-q", "-m", "link")
	c.Dir = d
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	if bad, err := boxpush.Mismatch(context.Background(), d, "", false, false, []string{"package.json"}); err != nil || len(bad) != 0 {
		t.Fatalf("a clean symlink: %v %v", bad, err)
	}
	if err := os.Remove(filepath.Join(d, "package.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real/other.json", filepath.Join(d, "package.json")); err != nil {
		t.Fatal(err)
	}
	if bad, _ := boxpush.Mismatch(context.Background(), d, "", false, false, []string{"package.json"}); len(bad) != 1 {
		t.Fatalf("a retargeted symlink is not the commit's: %v", bad)
	}
}

// A ref is a ref, never an option, and a path is a path, never git's pathspec.
func TestMismatchTakesNoOptionsFromRefOrPath(t *testing.T) {
	d, _ := gitProject(t, map[string]string{"package.json": "{}"})
	out := filepath.Join(t.TempDir(), "written")
	if _, err := boxpush.Mismatch(context.Background(), d, "--output="+out, false, false, []string{"package.json"}); err == nil {
		t.Error("an option as the ref was taken")
	}
	if _, err := os.Stat(out); err == nil {
		t.Fatal("git wrote a file named by --ref")
	}
	if bad, err := boxpush.Mismatch(context.Background(), d, "", false, false, []string{"*.json", ":(top)package.json"}); err != nil || len(bad) != 2 {
		t.Errorf("pathspec magic read as a path: %v %v", bad, err)
	}
}

// Run from a project's directory inside a repository, the layer goes where
// the push puts that directory's files.
func TestDepsFlagsMountAtTheProjectsPlaceInTheRepo(t *testing.T) {
	d, _ := gitProject(t, map[string]string{"app/package-lock.json": `{"lockfileVersion":3,"packages":{"":{}}}`, "app/package.json": "{}"})
	req, err := depsRequest(t, filepath.Join(d, "app"), "--deps", "npm")
	if err != nil {
		t.Fatal(err)
	}
	if req.Deps[0].Path != "checkout/app" {
		t.Fatalf("path %q", req.Deps[0].Path)
	}
	if req, err = depsRequest(t, filepath.Join(d, "app"), "--deps", "npm:mine"); err != nil || req.Deps[0].Path != "mine" {
		t.Fatalf("a named path is taken as is: %+v %v", req.Deps, err)
	}
}

// A layer asked without a path goes at the checkout plus the directory's
// place in its repository; an absolute checkout is taken as it is.
func TestDefaultPath(t *testing.T) {
	d, _ := gitProject(t, map[string]string{"app/package.json": "{}"})
	ctx := context.Background()
	for _, c := range []struct{ dir, dest, want string }{
		{filepath.Join(d, "app"), "checkout", "checkout/app"},
		{filepath.Join(d, "app"), "checkout/", "checkout/app"},
		{d, "checkout", "checkout"},
		{filepath.Join(d, "app"), "/abs/co", "/abs/co"},
		{t.TempDir(), "checkout", "checkout"}, // not in a repository
	} {
		if got := depscli.DefaultPath(ctx, c.dir, c.dest); got != c.want {
			t.Errorf("DefaultPath(%s, %q) = %q, want %q", c.dir, c.dest, got, c.want)
		}
	}
	var w bytes.Buffer
	depscli.Print(&w, "zm", []computeapi.BoxDeps{{Kind: "npm", Path: "a"}}, []computeapi.BoxDepsAnswer{{Kind: "npm", Key: "k", Hit: true}})
	if !strings.HasPrefix(w.String(), "zm: deps npm at a: hit") {
		t.Errorf("Print as zm: %q", w.String())
	}
}
