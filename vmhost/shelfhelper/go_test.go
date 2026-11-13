package shelfhelper_test

import (
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sengirunners/potluck/vmhost/shelfhelper"
)

// goSum is a go.sum naming public modules, private ones, a module whose
// go.mod alone is used, and lines the helper must skip.
const goSum = `github.com/BurntSushi/toml v1.3.2 h1:o7IhLm0Msx3BaB+n3Ag7L8EVlByGnpq14C4YWiu/gL8=
github.com/BurntSushi/toml v1.3.2/go.mod h1:CxXYINrC8qIiEnFrOxCa7Jy5BFHlXnUU2pbicEuybxQ=
golang.org/x/mod v0.14.0/go.mod h1:hTbmBsO62+eylJbnUtE2MGJUyE7QWk4xUqPFrRgJ+7c=
git.corp.example/team/secret v1.0.0 h1:aaaa=
git.corp.example/team/secret v1.0.0/go.mod h1:aaaa=
github.com/acme/internal-tool v0.2.0 h1:aaaa=
noproxy.example/x v1.0.0 h1:aaaa=
nosumdb.example/x v1.0.0 h1:aaaa=
insecure.example/x v1.0.0 h1:aaaa=
not a go.sum line
github.com/a/b v1.0.0 sha256:notanh1
`

// goBox is a project with a go.sum, a home and a shelf, and a fake go on
// PATH whose go env answers from the FAKE_* variables.
type goBox struct {
	t                      *testing.T
	shelf, home, proj, bin string
	tools                  string // the host's /usr/bin and /bin tools, never a go
	env                    []string
}

// linkHostTools fills dir with a link to every tool in /usr/bin and /bin but
// the Go toolchain, so a "no go" case holds on a host that has a real go in
// those directories.
func linkHostTools(t *testing.T, dir string) {
	t.Helper()
	for _, from := range []string{"/usr/bin", "/bin"} {
		entries, err := os.ReadDir(from)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			if e.Name() == "go" || e.Name() == "gofmt" {
				continue
			}
			// The first wins: a name in both directories fails here and is skipped.
			if err := os.Symlink(filepath.Join(from, e.Name()), filepath.Join(dir, e.Name())); err != nil && !errors.Is(err, fs.ErrExist) {
				t.Fatal(err)
			}
		}
	}
}

func newGoBox(t *testing.T) *goBox {
	t.Helper()
	root := t.TempDir()
	b := &goBox{t: t, shelf: filepath.Join(root, "shelf"), home: filepath.Join(root, "home"), proj: filepath.Join(root, "proj"), bin: filepath.Join(root, "bin"), tools: filepath.Join(root, "tools")}
	for _, d := range []string{b.shelf, b.home, b.proj, b.bin, b.tools} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	linkHostTools(t, b.tools)
	if err := os.WriteFile(filepath.Join(b.proj, "go.sum"), []byte(goSum), 0o644); err != nil {
		t.Fatal(err)
	}
	b.fakeGo(`#!/bin/sh
[ "$1" = env ] || exit 2
[ -n "${FAKE_FAIL:-}" ] && exit 1
printf '%s\n' "${FAKE_GOPROXY-https://proxy.golang.org,direct}" "${FAKE_GOSUMDB-sum.golang.org}" "${FAKE_GOPRIVATE-}" "${FAKE_GONOPROXY-}" "${FAKE_GONOSUMDB-}" "${FAKE_GOINSECURE-}" "$HOME/go/pkg/mod"
`)
	// The box's module cache holds every version go.sum names but nosumdb.example's: Go downloaded them.
	for _, f := range []string{"github.com/!burnt!sushi/toml/@v/v1.3.2.info", "github.com/!burnt!sushi/toml/@v/v1.3.2.zip",
		"github.com/!burnt!sushi/toml/@v/v1.3.2.mod", "golang.org/x/mod/@v/v0.14.0.mod", // a go.mod Go loaded alone: no .info
		"git.corp.example/team/secret/@v/v1.0.0.info", "git.corp.example/team/secret/@v/v1.0.0.zip", "git.corp.example/team/secret/@v/v1.0.0.mod",
		"github.com/acme/internal-tool/@v/v0.2.0.info", "github.com/acme/internal-tool/@v/v0.2.0.zip",
		"noproxy.example/x/@v/v1.0.0.info", "noproxy.example/x/@v/v1.0.0.zip", "insecure.example/x/@v/v1.0.0.info", "insecure.example/x/@v/v1.0.0.zip"} {
		p := filepath.Join(b.home, "go", "pkg", "mod", "cache", "download", filepath.FromSlash(f))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return b
}

func (b *goBox) fakeGo(script string) {
	b.t.Helper()
	if err := os.WriteFile(filepath.Join(b.bin, "go"), []byte(script), 0o755); err != nil {
		b.t.Fatal(err)
	}
}

// run runs the helper with PATH holding only the fake go and the system's
// tools (no node, so only the Go part runs), and answers the wants file.
func (b *goBox) run(env ...string) []string {
	b.t.Helper()
	script := filepath.Join(b.t.TempDir(), shelfhelper.Name)
	if err := os.WriteFile(script, shelfhelper.Script, 0o755); err != nil {
		b.t.Fatal(err)
	}
	cmd := exec.Command("/bin/sh", script, "link")
	cmd.Dir = b.proj
	cmd.Env = append([]string{"PATH=" + b.bin + ":" + b.tools, "HOME=" + b.home, "POTLUCK_SHELF=" + b.shelf}, env...)
	if out, err := cmd.CombinedOutput(); err != nil {
		b.t.Fatalf("potluck-deps link: %v\n%s", err, out)
	}
	data, err := os.ReadFile(filepath.Join(b.home, ".potluck", "wants"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		b.t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
}

var goPrivateEnv = []string{"FAKE_GOPRIVATE=*.corp.example,github.com/acme", "FAKE_GONOPROXY=noproxy.example", "FAKE_GONOSUMDB=nosumdb.example",
	"FAKE_GOINSECURE=insecure.example"}

// Public modules are named, once each (a version used whole is not named
// again for its go.mod); no module the box's private settings match ever is.
func TestGoWants(t *testing.T) {
	b := newGoBox(t)
	want := []string{"go github.com/BurntSushi/toml v1.3.2", "go golang.org/x/mod v0.14.0/go.mod"}
	if got := b.run(goPrivateEnv...); !slices.Equal(got, want) {
		t.Errorf("wants %q, want %q", got, want)
	}
	if got := b.run(goPrivateEnv...); !slices.Equal(got, want) {
		t.Errorf("a second run: wants %q", got)
	}
	data, _ := os.ReadFile(filepath.Join(b.home, ".potluck", "wants"))
	for _, s := range []string{"corp.example", "acme", "noproxy", "nosumdb", "insecure"} {
		if strings.Contains(string(data), s) {
			t.Errorf("the wants file names %s:\n%s", s, data)
		}
	}
	if fi, err := os.Stat(filepath.Join(b.home, ".potluck", "wants")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("wants file %v %v, want 0600", fi, err)
	}
}

// Settings the helper cannot vouch for name no Go module at all: a GOPROXY
// without the public proxy (or off before it), a GOSUMDB other than the
// public one, a pattern with a class or an escape, a failing go env, and,
// with no go, a go env file it cannot read.
func TestGoWantsNone(t *testing.T) {
	for name, env := range map[string][]string{
		"own proxy":    {"FAKE_GOPROXY=https://athens.corp.example,direct"},
		"off first":    {"FAKE_GOPROXY=off,https://proxy.golang.org"},
		"sumdb off":    {"FAKE_GOSUMDB=off"},
		"other sumdb":  {"FAKE_GOSUMDB=sum.golang.google.cn"},
		"class":        {"FAKE_GOPRIVATE=[a-z]*.corp.example"},
		"escape":       {"FAKE_GOPRIVATE=corp\\.example"},
		"go env fails": {"FAKE_FAIL=1"},
	} {
		t.Run(name, func(t *testing.T) {
			if got := newGoBox(t).run(env...); got != nil {
				t.Errorf("wants %q", got)
			}
		})
	}
	t.Run("no go, an env file", func(t *testing.T) {
		b := newGoBox(t)
		os.Remove(filepath.Join(b.bin, "go"))
		envFile := filepath.Join(b.home, ".config", "go", "env")
		os.MkdirAll(filepath.Dir(envFile), 0o755)
		os.WriteFile(envFile, []byte("GOPRIVATE=github.com/BurntSushi\n"), 0o644)
		if got := b.run(); got != nil {
			t.Errorf("wants %q", got)
		}
	})
	t.Run("no go, the environment", func(t *testing.T) {
		b := newGoBox(t)
		os.Remove(filepath.Join(b.bin, "go"))
		got := b.run("GOPRIVATE=github.com/BurntSushi")
		if want := []string{"go golang.org/x/mod v0.14.0/go.mod", "go github.com/acme/internal-tool v0.2.0", "go noproxy.example/x v1.0.0",
			"go insecure.example/x v1.0.0", "go git.corp.example/team/secret v1.0.0"}; len(got) != len(want) {
			t.Errorf("wants %q", got)
		}
		for _, l := range got {
			if strings.Contains(l, "BurntSushi") {
				t.Errorf("named %q", l)
			}
		}
	})
	t.Run("no shelf", func(t *testing.T) {
		b := newGoBox(t)
		os.Remove(b.shelf)
		if got := b.run(); got != nil {
			t.Errorf("wants %q", got)
		}
	})
}

// The proxy order: a file:// tree may come first, then the public proxy
// must; direct or another proxy first names nothing. POTLUCK_DEPS_GO=off
// (the driver's own link step) names nothing.
func TestGoWantsProxyOrder(t *testing.T) {
	for env, named := range map[string]bool{
		"FAKE_GOPROXY=file:///opt/potluck-cache/goproxy,https://proxy.golang.org,direct": true,
		"FAKE_GOPROXY=https://proxy.golang.org|direct":                                   true,
		"FAKE_GOPROXY=direct,https://proxy.golang.org":                                   false,
		"FAKE_GOPROXY=https://athens.corp.example,https://proxy.golang.org":              false,
		"FAKE_GOPROXY=file:///a,file:///b,https://proxy.golang.org":                      false,
		"POTLUCK_DEPS_GO=off": false,
	} {
		if got := newGoBox(t).run(env); (got != nil) != named {
			t.Errorf("%s: wants %q", env, got)
		}
	}
}

// Only versions the box's module cache holds are named: a go.sum line for a
// module Go never downloaded here (vendored, or written on another machine)
// is not.
func TestGoWantsOnlyFetched(t *testing.T) {
	b := newGoBox(t)
	if err := os.RemoveAll(filepath.Join(b.home, "go", "pkg", "mod", "cache", "download", "golang.org")); err != nil {
		t.Fatal(err)
	}
	if got, want := b.run(goPrivateEnv...), []string{"go github.com/BurntSushi/toml v1.3.2"}; !slices.Equal(got, want) {
		t.Errorf("wants %q, want %q", got, want)
	}
	if err := os.RemoveAll(filepath.Join(b.home, "go")); err != nil {
		t.Fatal(err)
	}
	b2 := newGoBox(t)
	os.RemoveAll(filepath.Join(b2.home, "go"))
	if got := b2.run(goPrivateEnv...); got != nil {
		t.Errorf("with an empty module cache: wants %q", got)
	}
}
