package vmhost

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

// Go wants: what parses, what does not, and the line each one is.
func TestParseGoWant(t *testing.T) {
	for _, l := range []string{
		"go github.com/BurntSushi/toml v1.3.2",
		"go golang.org/x/mod v0.14.0/go.mod",
		"go gopkg.in/yaml.v3 v3.0.1",
		"go example.com/a/b/v2 v2.0.0-20230101120000-abcdef123456",
		"go github.com/x/y v2.1.0+incompatible",
		"go example.com/x v1.0.0-rc.1/go.mod",
		"go example.com/tilde~_x v1.0.0",
	} {
		w, ok := parseShelfWant(l)
		if !ok || w.Kind != shelfKindGo || w.goLine() != l || w.Hex != "" {
			t.Errorf("%q: %+v %v", l, w, ok)
		}
	}
	if w, _ := parseShelfWant("go golang.org/x/mod v0.14.0/go.mod"); !w.GoMod || w.Version != "v0.14.0" {
		t.Errorf("go.mod want %+v", w)
	}
	for _, l := range []string{
		"go example.com/x 1.0.0",                                 // no v
		"go example.com/x v1.0",                                  // not canonical
		"go example.com/x v1.0.0+meta",                           // build metadata other than +incompatible
		"go example.com/x v1.0.0/go.sum",                         // not /go.mod
		"go example.com/x v1.0.0 h1:abc",                         // a hash field
		"go Example.com/x v1.0.0",                                // upper case in the first element
		"go examplecom/x v1.0.0",                                 // no dot in the first element
		"go -example.com/x v1.0.0",                               // leading dash
		"go example.com//x v1.0.0",                               // empty element
		"go example.com/../x v1.0.0",                             // dots
		"go example.com/.x v1.0.0",                               // leading dot
		"go example.com/x. v1.0.0",                               // trailing dot
		"go example.com/x@v1 v1.0.0",                             // @
		"go example.com/x+y v1.0.0",                              // +
		"go example.com/x%2f v1.0.0",                             // %
		"go example.com/x v1.0.0 ",                               // trailing space
		"go  example.com/x v1.0.0",                               // double space
		"go example.com/x",                                       // no version
		"go example.com/x v1.0.0\x00",                            // NUL
		"go example.com/" + strings.Repeat("a", 500) + " v1.0.0", // too long
		"gomod example.com/x v1.0.0",
	} {
		if w, ok := parseShelfWant(l); ok {
			t.Errorf("%q parsed: %+v", l, w)
		}
	}
}

// goEscape and parseGoFile round-trip, and parseGoFile refuses what Go
// could not have written.
func TestGoFilePaths(t *testing.T) {
	p := goFile("github.com/BurntSushi/toml", "v1.0.0-RC", "zip")
	if p != "github.com/!burnt!sushi/toml/@v/v1.0.0-!r!c.zip" {
		t.Errorf("escaped %q", p)
	}
	if m, v, e, ok := parseGoFile(p); !ok || m != "github.com/BurntSushi/toml" || v != "v1.0.0-RC" || e != "zip" {
		t.Errorf("parsed %q %q %q %v", m, v, e, ok)
	}
	for _, p := range []string{"github.com/BurntSushi/toml/@v/v1.0.0.zip", "example.com/x/@v/v1.0.0.list", "example.com/x/@v/v1.0.0.lock",
		"example.com/x/@v/list", "example.com/x!/@v/v1.0.0.zip", "example.com/x/@v/v1.0.0/x.zip", "example.com/x/@v/../v1.0.0.zip",
		"example.com/x@v/v1.0.0.zip", "x/@v/v1.0.0.zip"} {
		if _, _, _, ok := parseGoFile(p); ok {
			t.Errorf("%q parsed", p)
		}
	}
}

// goPrivate is Go's MatchPrefixPatterns.
func TestGoPrivate(t *testing.T) {
	for _, c := range []struct {
		globs, mod string
		want       bool
	}{
		{"corp.example", "corp.example/x/y", true},
		{"corp.example/", "corp.example/x", true},
		{"*.corp.example", "git.corp.example/x", true},
		{"*.corp.example", "corp.example/x", false},
		{"github.com/acme", "github.com/acme/tool", true},
		{"github.com/acme", "github.com/acmex/tool", false},
		{"github.com/acme/*/v2", "github.com/acme/a/v2/b", true},
		{"github.com/acme/*/v2", "github.com/acme/a", false},
		{",,a.example,b.example", "b.example/c", true},
		{"", "a.example/b", false},
	} {
		if got := goPrivate(c.globs, c.mod); got != c.want {
			t.Errorf("goPrivate(%q, %q) = %v", c.globs, c.mod, got)
		}
	}
}

// goSumWants: a version wanted whole is not wanted again for its go.mod;
// private and malformed lines are left out.
func TestGoSumWants(t *testing.T) {
	sum := `github.com/a/b v1.0.0 h1:x=
github.com/a/b v1.0.0/go.mod h1:y=
golang.org/x/mod v0.14.0/go.mod h1:z=
corp.example/secret v1.0.0 h1:x=
corp.example/secret v1.0.0/go.mod h1:x=
github.com/a/b v1.0.0 h1:x=
not a line
github.com/c/d 1.0.0 h1:x=
`
	ws, err := goSumWants(strings.NewReader(sum), "corp.example")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, w := range ws {
		got = append(got, w.goLine())
	}
	if want := []string{"go github.com/a/b v1.0.0", "go golang.org/x/mod v0.14.0/go.mod"}; !slices.Equal(got, want) {
		t.Errorf("wants %q, want %q", got, want)
	}
}

// goMod1 is Go's h1 of a go.mod alone (a go.sum /go.mod line's hash).
func goMod1(mod []byte) string {
	h := sha256.Sum256(mod)
	s := sha256.Sum256([]byte(fmt.Sprintf("%x  go.mod\n", h)))
	return "h1:" + base64.StdEncoding.EncodeToString(s[:])
}

// goModule is a fixture module version: its proxy files.
type goModule struct {
	path, version  string
	info, mod, zip []byte
	ziphash        string
}

func newGoModule(t *testing.T, path, version string, files map[string]string) goModule {
	t.Helper()
	var buf bytes.Buffer
	z := zip.NewWriter(&buf)
	for _, n := range slices.Sorted(func(yield func(string) bool) {
		for k := range files {
			if !yield(k) {
				return
			}
		}
	}) {
		w, err := z.Create(path + "@" + version + "/" + n)
		if err != nil {
			t.Fatal(err)
		}
		w.Write([]byte(files[n]))
	}
	if err := z.Close(); err != nil {
		t.Fatal(err)
	}
	h1, err := goZipHash(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatal(err)
	}
	return goModule{path: path, version: version, info: []byte(`{"Version":"` + version + `","Time":"2026-01-01T00:00:00Z"}`),
		mod: []byte(files["go.mod"]), zip: buf.Bytes(), ziphash: h1}
}

// writeTo puts m's files in a module cache's download tree at root (only
// .info and .mod when modOnly).
func (m goModule) writeTo(t *testing.T, root string, modOnly bool) {
	t.Helper()
	files := map[string][]byte{"info": m.info, "mod": m.mod}
	if !modOnly {
		files["zip"], files["ziphash"] = m.zip, []byte(m.ziphash)
	}
	for ext, b := range files {
		p := filepath.Join(root, filepath.FromSlash(goFile(m.path, m.version, ext)))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// fakeGo stands in for the shelf's toolchain in the fetch container: it logs
// its environment and arguments and copies a fixture version from
// $FAKE_GO_SRC (whole for mod download, .info and .mod for list -m) into
// the module cache, with the .lock and list files Go leaves beside them.
const fakeGo = `#!/bin/sh
echo "GOPROXY=$GOPROXY GOSUMDB=$GOSUMDB GONOSUMDB=$GONOSUMDB GOPRIVATE=$GOPRIVATE GONOPROXY=$GONOPROXY GOINSECURE=$GOINSECURE GOFLAGS=$GOFLAGS GOTOOLCHAIN=$GOTOOLCHAIN GOENV=$GOENV | $*" >> "$FAKE_GO_LOG"
case "$1 $2" in
"mod download") mv=$3; exts="info mod zip ziphash" ;;
"list -m") mv=$4; exts="info mod" ;;
*) exit 2 ;;
esac
m=${mv%@*}; v=${mv##*@}
[ -f "$FAKE_GO_SRC/$m/@v/$v.info" ] || { echo "go: $mv: not found" >&2; exit 1; }
d=$GOMODCACHE/cache/download/$m/@v
mkdir -p "$d" "$GOMODCACHE/cache/download/sumdb/sum.golang.org/lookup"
for e in $exts; do cp "$FAKE_GO_SRC/$m/@v/$v.$e" "$d/"; done
: > "$d/$v.lock"; echo "$v" > "$d/list"; : > "$GOMODCACHE/cache/download/sumdb/sum.golang.org/lookup/x"
`

// shelfGoOCI is newShelfOCI with a box network, the fake toolchain on the
// shelf for this machine's arch, and a fixture source; it answers the fake
// go's log.
func shelfGoOCI(t *testing.T) (o ociCLI, dir, vol, src, log string) {
	t.Helper()
	o, dir, vol = newShelfOCI(t)
	o.network = "zm-test"
	arch := map[string]string{"amd64": "x64", "arm64": "arm64"}[runtime.GOARCH]
	if arch == "" {
		t.Skip("no shelf toolchain layout for", runtime.GOARCH)
	}
	bin := filepath.Join(vol, "tools", "go", "1.27.4", arch, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "go"), []byte(fakeGo), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(vol, "tools", "go", "1.27.4", arch+".complete"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	src, log = t.TempDir(), filepath.Join(t.TempDir(), "go.log")
	t.Setenv("FAKE_GO_SRC", src)
	t.Setenv("FAKE_GO_LOG", log)
	return o, dir, vol, src, log
}

func goWant(t *testing.T, line string) shelfWant {
	t.Helper()
	w, ok := parseShelfWant(line)
	if !ok {
		t.Fatalf("%q does not parse", line)
	}
	return w
}

// A Go fill: the fetch container is a box of the binding's (its network, the
// shelf read-only, nobody's user) whose go asks only the public proxy and
// checksum database, never direct and with nothing private; what it sends
// back is kept only for the versions asked for, whole and checked; the
// publisher puts each file on the shelf 0444. Then a box with GOPROXY as
// cacheArgs sets it builds against the shelved module with no network at
// all, and a second fill of the same wants downloads nothing.
func TestShelfGoFill(t *testing.T) {
	o, dir, vol, src, log := shelfGoOCI(t)
	hello := newGoModule(t, "example.com/hello", "v1.0.0", map[string]string{
		"go.mod":   "module example.com/hello\n\ngo 1.21\n",
		"hello.go": "package hello\n\nfunc Hello() string { return \"hi from the shelf\" }\n",
	})
	hello.writeTo(t, src, false)
	modonly := newGoModule(t, "example.com/modonly", "v0.1.0", map[string]string{"go.mod": "module example.com/modonly\n"})
	modonly.writeTo(t, src, true)
	bad := newGoModule(t, "example.com/bad", "v1.0.0", map[string]string{"go.mod": "module example.com/bad\n"})
	bad.ziphash = hello.ziphash // the zip is not the one its hash names
	bad.writeTo(t, src, false)
	wants := []shelfWant{goWant(t, "go example.com/hello v1.0.0"), goWant(t, "go example.com/modonly v0.1.0/go.mod"),
		goWant(t, "go example.com/bad v1.0.0"), goWant(t, "go example.com/absent v1.0.0"), goWant(t, "go example.com/hello v1.0.0")}

	res, err := shelfFill(context.Background(), shelfFillCfg(o, 1000), o, wants)
	if err != nil {
		t.Fatal(err)
	}
	if res.Wanted != 4 || res.Fetched != 2 || res.Present != 0 || res.Dropped[dropMismatch] != 1 || res.Dropped[dropMissing] != 1 {
		t.Errorf("fill: %+v", res)
	}
	if res.Published.Published != 6 || res.Published.Dropped != 0 {
		t.Errorf("published %+v", res.Published)
	}
	if strings.Contains(res.String(), "example.com") {
		t.Errorf("the fill's line names a module: %s", res)
	}

	// The fetch container's flags, and go's settings in it.
	calls, _ := os.ReadFile(filepath.Join(dir, "calls"))
	var fetch string
	for _, l := range strings.Split(string(calls), "\n") {
		if strings.Contains(l, "potluck-gofetch-") && strings.HasPrefix(l, "run ") {
			fetch = l
		}
	}
	for _, f := range []string{"--network zm-test", "--user 65534:65534", "--cap-drop ALL", "--security-opt no-new-privileges",
		"source=" + ShelfVolume + ",target=/shelf,readonly", " img -c "} {
		if !strings.Contains(fetch, f) {
			t.Errorf("the fetch container lacks %q: %s", f, fetch)
		}
	}
	lb, _ := os.ReadFile(log)
	lines := strings.Split(strings.TrimSpace(string(lb)), "\n")
	const env = "GOPROXY=https://proxy.golang.org GOSUMDB=sum.golang.org GONOSUMDB= GOPRIVATE= GONOPROXY= GOINSECURE= GOFLAGS=-mod=mod GOTOOLCHAIN=local GOENV=off | "
	wantCalls := []string{env + "mod download example.com/hello@v1.0.0", env + "list -m -json example.com/modonly@v0.1.0",
		env + "mod download example.com/bad@v1.0.0", env + "mod download example.com/absent@v1.0.0"}
	if !slices.Equal(lines, wantCalls) {
		t.Errorf("go ran as\n%s\nwant\n%s", strings.Join(lines, "\n"), strings.Join(wantCalls, "\n"))
	}

	// On the shelf: hello whole, modonly's .info and .mod, nothing of bad
	// or absent, no .lock, list or sumdb file; every file 0444.
	var got []string
	_ = filepath.WalkDir(filepath.Join(vol, shelfGoproxyDir), func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			rel, _ := filepath.Rel(filepath.Join(vol, shelfGoproxyDir), p)
			got = append(got, filepath.ToSlash(rel))
			if fi, _ := d.Info(); fi.Mode().Perm() != 0o444 {
				t.Errorf("%s is %v, want 0444", rel, fi.Mode())
			}
		}
		return nil
	})
	want := []string{"example.com/hello/@v/v1.0.0.info", "example.com/hello/@v/v1.0.0.mod", "example.com/hello/@v/v1.0.0.zip",
		"example.com/hello/@v/v1.0.0.ziphash", "example.com/modonly/@v/v0.1.0.info", "example.com/modonly/@v/v0.1.0.mod"}
	if !slices.Equal(got, want) {
		t.Errorf("goproxy/ holds %q, want %q", got, want)
	}
	if es, _ := os.ReadDir(shelfStaging(o.stateDir)); len(es) != 0 {
		t.Errorf("staging kept %v", es)
	}
	if es, _ := os.ReadDir(shelfFetching(o.stateDir)); len(es) != 0 {
		t.Errorf("fetching kept %v", es)
	}

	// Again: both on the shelf, no go run.
	res, err = shelfFill(context.Background(), shelfFillCfg(o, 1000), o, wants[:2])
	if err != nil || res.Present != 2 || res.Fetched != 0 {
		t.Errorf("second fill: %+v %v", res, err)
	}
	if lb2, _ := os.ReadFile(log); !bytes.Equal(lb, lb2) {
		t.Errorf("a fill of shelved modules ran go again:\n%s", lb2)
	}

	// A box resolves the shelved module with no network: GOPROXY as
	// cacheArgs sets it (the shelf at its path here), and every network
	// request sent to a dead proxy.
	if _, err := exec.LookPath("go"); err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("go not on PATH in CI")
		}
		t.Skip("go not on PATH")
	}
	proj, home := t.TempDir(), t.TempDir()
	for n, b := range map[string]string{
		"go.mod":  "module consumer\n\ngo 1.21\n\nrequire example.com/hello v1.0.0\n",
		"main.go": "package main\n\nimport (\n\t\"fmt\"\n\n\t\"example.com/hello\"\n)\n\nfunc main() { fmt.Println(hello.Hello()) }\n",
		"go.sum":  "example.com/hello v1.0.0 " + hello.ziphash + "\nexample.com/hello v1.0.0/go.mod " + goMod1(hello.mod) + "\n",
	} {
		if err := os.WriteFile(filepath.Join(proj, n), []byte(b), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command("go", "run", ".")
	cmd.Dir = proj
	cmd.Env = append(os.Environ(), "HOME="+home, "GOPATH="+filepath.Join(home, "go"), "GOMODCACHE="+filepath.Join(home, "mod"),
		"GOCACHE="+filepath.Join(home, "build"), "GOENV=off", "GOFLAGS=-mod=mod -modcacherw", "GOTOOLCHAIN=local",
		"GOPROXY="+strings.Replace(ociCacheGoproxy, ociCacheDir, vol, 1), "GOSUMDB=sum.golang.org", "GONOSUMDB=", "GOPRIVATE=", "GONOPROXY=",
		"HTTPS_PROXY=http://127.0.0.1:9", "HTTP_PROXY=http://127.0.0.1:9", "NO_PROXY=", "GIT_CONFIG_NOSYSTEM=1", "GOVCS=*:off")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil || strings.TrimSpace(string(out)) != "hi from the shelf" {
		t.Errorf("go run against the shelf: %v\n%s%s", err, out, stderr.Bytes())
	}
}

// A binding whose boxes have no network, or a shelf with no Go toolchain,
// fetches nothing and drops the wants by that reason; it is not an error.
func TestShelfGoFillCannot(t *testing.T) {
	o, _, vol, _, log := shelfGoOCI(t)
	w := []shelfWant{goWant(t, "go example.com/hello v1.0.0")}
	o.network = "none"
	if res, err := shelfFill(context.Background(), shelfFillCfg(o, 1000), o, w); err != nil || res.Dropped[dropNetwork] != 1 {
		t.Errorf("no network: %+v %v", res, err)
	}
	o.network = "zm-test"
	if err := os.RemoveAll(filepath.Join(vol, "tools")); err != nil {
		t.Fatal(err)
	}
	if res, err := shelfFill(context.Background(), shelfFillCfg(o, 1000), o, w); err != nil || res.Dropped[dropToolchain] != 1 {
		t.Errorf("no toolchain: %+v %v", res, err)
	}
	if _, err := os.Stat(log); err == nil {
		t.Error("go ran")
	}
}

// The publisher checks Go files again: a .zip goes on the shelf only with the
// .ziphash naming its own hash right after it, a .info only naming its
// version, and a path that is not a GOPROXY file's refuses the publish.
func TestShelfSpoolGo(t *testing.T) {
	o, _, vol := newShelfOCI(t)
	good := newGoModule(t, "example.com/good", "v1.0.0", map[string]string{"go.mod": "module example.com/good\n"})
	nohash := newGoModule(t, "example.com/nohash", "v1.0.0", map[string]string{"go.mod": "module example.com/nohash\n"})
	wrong := newGoModule(t, "example.com/wrong", "v1.0.0", map[string]string{"go.mod": "module example.com/wrong\n"})
	wrong.ziphash = good.ziphash
	other := newGoModule(t, "example.com/other", "v1.0.0", map[string]string{"go.mod": "module example.com/other\n"})
	other.info = []byte(`{"Version":"v9.9.9"}`)
	staged := filepath.Join(shelfStaging(o.stateDir), shelfGoproxyDir)
	for _, m := range []goModule{good, nohash, wrong, other} {
		m.writeTo(t, staged, false)
	}
	if err := os.Remove(filepath.Join(staged, filepath.FromSlash(goFile(nohash.path, nohash.version, "ziphash")))); err != nil {
		t.Fatal(err)
	}
	res, err := o.PublishShelf("img")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	_ = filepath.WalkDir(filepath.Join(vol, shelfGoproxyDir), func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			rel, _ := filepath.Rel(filepath.Join(vol, shelfGoproxyDir), p)
			got = append(got, filepath.ToSlash(rel))
		}
		return nil
	})
	want := []string{"example.com/good/@v/v1.0.0.info", "example.com/good/@v/v1.0.0.mod", "example.com/good/@v/v1.0.0.zip", "example.com/good/@v/v1.0.0.ziphash",
		"example.com/nohash/@v/v1.0.0.info", "example.com/nohash/@v/v1.0.0.mod",
		"example.com/other/@v/v1.0.0.mod", "example.com/other/@v/v1.0.0.zip", "example.com/other/@v/v1.0.0.ziphash",
		"example.com/wrong/@v/v1.0.0.info", "example.com/wrong/@v/v1.0.0.mod"}
	if !slices.Equal(got, want) {
		t.Errorf("the shelf holds\n%q\nwant\n%q", got, want)
	}
	if res.Dropped != 4 { // nohash's zip, wrong's zip and ziphash, other's info
		t.Errorf("publish %+v", res)
	}
	if es, _ := os.ReadDir(shelfStaging(o.stateDir)); len(es) != 0 {
		t.Errorf("staging kept %v", es)
	}

	for _, bad := range []string{"example.com/x/@v/list", "example.com/x/@v/v1.0.0.lock", "Example.com/x/@v/v1.0.0.mod", "x/@v/v1.0.0.mod"} {
		stage(t, o, shelfGoproxyDir+"/"+bad, []byte("x"))
		if _, err := o.PublishShelf("img"); err == nil || !strings.Contains(err.Error(), "GOPROXY") {
			t.Errorf("staging %s: %v", bad, err)
		}
		_ = os.RemoveAll(filepath.Join(shelfStaging(o.stateDir), shelfGoproxyDir))
	}
}

// A symlink anywhere on a Go file's way into goproxy/ stops the publish,
// and the refusal names no module.
func TestShelfPublishGoSymlink(t *testing.T) {
	o, _, vol := newShelfOCI(t)
	m := newGoModule(t, "example.com/linked", "v1.0.0", map[string]string{"go.mod": "module example.com/linked\n"})
	m.writeTo(t, filepath.Join(shelfStaging(o.stateDir), shelfGoproxyDir), false)
	elsewhere := t.TempDir()
	if err := os.MkdirAll(filepath.Join(vol, shelfGoproxyDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, filepath.Join(vol, shelfGoproxyDir, "example.com")); err != nil {
		t.Fatal(err)
	}
	_, err := o.PublishShelf("img")
	if err == nil || !strings.Contains(err.Error(), "symlink") || strings.Contains(err.Error(), "linked") {
		t.Errorf("publish through a symlink: %v", err)
	}
	if es, _ := os.ReadDir(elsewhere); len(es) != 0 {
		t.Errorf("the publish wrote through the symlink: %v", es)
	}
}

// putGoVersion puts m's files on the shelf as the publisher leaves them,
// with mtime at, and answers their paths.
func putGoVersion(t *testing.T, vol string, m goModule, at time.Time) (ps []string) {
	t.Helper()
	m.writeTo(t, filepath.Join(vol, shelfGoproxyDir), false)
	for _, e := range []string{"info", "mod", "zip", "ziphash"} {
		p := filepath.Join(vol, shelfGoproxyDir, filepath.FromSlash(goFile(m.path, m.version, e)))
		if err := os.Chtimes(p, at, at); err != nil {
			t.Fatal(err)
		}
		ps = append(ps, p)
	}
	return ps
}

func allGone(ps []string) bool {
	for _, p := range ps {
		if _, err := os.Lstat(p); err == nil {
			return false
		}
	}
	return true
}

// Past shelf_max_gb the oldest Go version goes, whole; one whose .info
// moved since the listing stays.
func TestShelfGoEvict(t *testing.T) {
	o, _, vol := newShelfOCI(t)
	now := time.Now()
	a := newGoModule(t, "example.com/a", "v1.0.0", map[string]string{"go.mod": "module example.com/a\n"})
	b := newGoModule(t, "example.com/b", "v1.0.0", map[string]string{"go.mod": "module example.com/b\n"})
	pa := putGoVersion(t, vol, a, now.Add(-2*time.Hour))
	pb := putGoVersion(t, vol, b, now.Add(-time.Hour))
	l, err := o.ListShelf("img")
	if err != nil {
		t.Fatal(err)
	}
	slices.SortFunc(l.Gos, func(x, y shelfGoAt) int { return strings.Compare(x.Base, y.Base) }) // the listing is in the filesystem's order
	if len(l.Gos) != 2 || l.Gos[0].Base != "goproxy/example.com/a/@v/v1.0.0" || l.Gos[0].Size != int64(len(a.info)+len(a.mod)+len(a.zip)+len(a.ziphash)) {
		t.Fatalf("listing %+v", l)
	}
	smallGiB(t, l.bytes()-1) // shelf_max_gb 1: one byte under what is there
	res, err := evictShelf(evictCfg(o, 3600*24), o)
	if err != nil || res.Deleted != 1 || !allGone(pa) || allGone(pb) {
		t.Errorf("%+v %v: a gone %v, b gone %v", res, err, allGone(pa), allGone(pb))
	}
	// A recency that moved after the listing (a touch, a publish) keeps it.
	moved := l.Gos[1]
	moved.Mtime--
	gone, err := o.DeleteShelfGo("img", []shelfGoAt{moved})
	if err != nil || len(gone) != 0 || allGone(pb) {
		t.Errorf("a moved version: gone %v %v", gone, err)
	}
	if _, err := o.DeleteShelfGo("img", []shelfGoAt{{Base: "goproxy/../npm/x"}}); err == nil {
		t.Error("deleted a path outside goproxy/")
	}
}

// A want for a version on the shelf moves its .info's mtime forward (at
// most once per shelfTouchAge); a version that would pass the room under
// shelf_max_gb is dropped as full and stages nothing.
func TestShelfGoTouchAndRoom(t *testing.T) {
	o, _, vol, src, _ := shelfGoOCI(t)
	old := time.Now().Add(-48 * time.Hour)
	a := newGoModule(t, "example.com/a", "v1.0.0", map[string]string{"go.mod": "module example.com/a\n"})
	pa := putGoVersion(t, vol, a, old)
	res, err := shelfFill(context.Background(), shelfFillCfg(o, 1000), o, []shelfWant{goWant(t, "go example.com/a v1.0.0")})
	if err != nil || res.Present != 1 || res.Touched != 1 {
		t.Errorf("fill %+v %v", res, err)
	}
	if fi, _ := os.Stat(pa[0]); time.Since(fi.ModTime()) > time.Hour {
		t.Errorf("the .info's mtime is still %v", fi.ModTime())
	}
	if fi, _ := os.Stat(pa[2]); !fi.ModTime().Equal(old) {
		t.Errorf("the .zip's mtime moved to %v", fi.ModTime())
	}

	big := newGoModule(t, "example.com/big", "v1.0.0", map[string]string{"go.mod": "module example.com/big\n", "x.go": strings.Repeat("x", 4096)})
	big.writeTo(t, src, false)
	l, err := o.ListShelf("img")
	if err != nil {
		t.Fatal(err)
	}
	smallGiB(t, l.bytes()+100) // room for 100 bytes
	res, err = shelfFill(context.Background(), shelfFillCfg(o, 1000), o, []shelfWant{goWant(t, "go example.com/big v1.0.0")})
	if err != nil || res.Dropped[dropFull] != 1 || res.Fetched != 0 {
		t.Errorf("fill past the room %+v %v", res, err)
	}
	if n := shelfStagedBytes(o.stateDir); n != 0 {
		t.Errorf("%d bytes staged past the room", n)
	}
}

// A go.mod want and a whole want of one version in one fill are one fetch:
// the version goes on the shelf whole.
func TestShelfGoFillModAndWhole(t *testing.T) {
	o, _, vol, src, log := shelfGoOCI(t)
	m := newGoModule(t, "example.com/m", "v1.0.0", map[string]string{"go.mod": "module example.com/m\n"})
	m.writeTo(t, src, false)
	res, err := shelfFill(context.Background(), shelfFillCfg(o, 1000), o,
		[]shelfWant{goWant(t, "go example.com/m v1.0.0/go.mod"), goWant(t, "go example.com/m v1.0.0")})
	if err != nil || res.Wanted != 1 || res.Fetched != 1 || len(res.Dropped) != 0 {
		t.Errorf("fill %+v %v", res, err)
	}
	if _, err := os.Stat(filepath.Join(vol, shelfGoproxyDir, filepath.FromSlash(goFile(m.path, m.version, "zip")))); err != nil {
		t.Errorf("no zip on the shelf: %v", err)
	}
	if b, _ := os.ReadFile(log); strings.Count(string(b), "\n") != 1 || !strings.Contains(string(b), "mod download") {
		t.Errorf("go ran as:\n%s", b)
	}
}

// The fetch container runs under the binding's runtime; many versions go
// in batches, each under the fill's cap.
func TestShelfGoFillBatches(t *testing.T) {
	o, dir, _, src, log := shelfGoOCI(t)
	var wants []shelfWant
	for i := range shelfGoBatch + 2 {
		m := newGoModule(t, fmt.Sprintf("example.com/m%d", i), "v1.0.0", map[string]string{"go.mod": "module x\n"})
		m.writeTo(t, src, false)
		wants = append(wants, goWant(t, "go "+m.path+" v1.0.0"))
	}
	res, err := shelfFill(context.Background(), shelfFillCfg(o, 1000), o, wants)
	if err != nil || res.Fetched != shelfGoBatch+2 {
		t.Errorf("fill %+v %v", res, err)
	}
	calls, _ := os.ReadFile(filepath.Join(dir, "calls"))
	if n := strings.Count(string(calls), "--runtime runsc --network zm-test"); n != 2 {
		t.Errorf("%d fetch containers under the runtime, want 2:\n%s", n, calls)
	}
	if b, _ := os.ReadFile(log); strings.Count(string(b), "\n") != shelfGoBatch+2 {
		t.Errorf("go ran %d times", strings.Count(string(b), "\n"))
	}
}

// Pairing a zip with its .ziphash is by name: v1.0.0-a.zip's files sort
// between v1.0.0-a's .zip and .ziphash, and both versions go on the shelf.
// An older fill's list, lock and sumdb files are removed by the publish.
func TestShelfSpoolGoPairsByName(t *testing.T) {
	o, _, vol := newShelfOCI(t)
	staged := filepath.Join(shelfStaging(o.stateDir), shelfGoproxyDir)
	for _, v := range []string{"v1.0.0-a", "v1.0.0-a.zip"} {
		newGoModule(t, "example.com/p", v, map[string]string{"go.mod": "module example.com/p\n"}).writeTo(t, staged, false)
	}
	old := []string{"goproxy/example.com/p/@v/list", "goproxy/example.com/p/@v/v0.9.0.lock", "goproxy/sumdb/sum.golang.org/lookup/x"}
	for _, p := range old {
		putOther(t, vol, p, []byte("x"), time.Now())
	}
	res, err := o.PublishShelf("img")
	if err != nil || res.Published != 8 || res.Dropped != 0 {
		t.Fatalf("publish %+v %v", res, err)
	}
	for _, v := range []string{"v1.0.0-a", "v1.0.0-a.zip"} {
		if _, err := os.Stat(filepath.Join(vol, shelfGoproxyDir, filepath.FromSlash(goFile("example.com/p", v, "zip")))); err != nil {
			t.Errorf("%s's zip: %v", v, err)
		}
	}
	for _, p := range old {
		if _, err := os.Lstat(filepath.Join(vol, filepath.FromSlash(p))); err == nil {
			t.Errorf("%s stayed", p)
		}
	}
}
