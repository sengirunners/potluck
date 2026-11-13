package shelfhelper_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/sengirunners/potluck/vmhost/shelfhelper"
)

// box is one run of the helper: a fake shelf, a home, an npm cache and a project directory, all
// under t.TempDir.
type box struct {
	t                        *testing.T
	shelf, home, cache, proj string
	noCacheEnv               bool // leave npm_config_cache unset
}

func newBox(t *testing.T) *box {
	t.Helper()
	if _, err := exec.LookPath("node"); err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("node not on PATH; CI must provide it (.github/workflows/ci.yml)")
		}
		t.Skip("node not on PATH")
	}
	root := t.TempDir()
	b := &box{t: t, shelf: filepath.Join(root, "shelf"), home: filepath.Join(root, "home"),
		cache: filepath.Join(root, "home", "npm-cache"), proj: filepath.Join(root, "proj")}
	for _, d := range []string{b.shelf, b.home, b.proj} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return b
}

func needNpm(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("npm"); err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("npm not on PATH; CI must provide it (.github/workflows/ci.yml)")
		}
		t.Skip("npm not on PATH")
	}
}

// tree maps every path under root to its mode and contents (a symlink's target).
func tree(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		v := fi.Mode().String()
		switch {
		case fi.Mode()&os.ModeSymlink != 0:
			target, _ := os.Readlink(p)
			v += " -> " + target
		case fi.Mode().IsRegular():
			data, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			sum := sha512.Sum512(data)
			v += " " + hex.EncodeToString(sum[:])
		}
		out[p] = v
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// pkg is a fake tarball and its lockfile integrity.
type pkg struct {
	data []byte
	b64  string
	hex  string
}

func newPkg(content string) pkg {
	sum := sha512.Sum512([]byte(content))
	return pkg{data: []byte(content), b64: base64.StdEncoding.EncodeToString(sum[:]), hex: hex.EncodeToString(sum[:])}
}

func (p pkg) integrity() string { return "sha512-" + p.b64 }
func (p pkg) rel() string {
	return filepath.Join("content-v2", "sha512", p.hex[:2], p.hex[2:4], p.hex[4:])
}

// shelve puts p on the shelf as the publisher will: a 0444 file in read-only directories.
func (b *box) shelve(p pkg) {
	b.t.Helper()
	path := filepath.Join(b.shelf, "npm", p.rel())
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		b.t.Fatal(err)
	}
	if err := os.WriteFile(path, p.data, 0o444); err != nil {
		b.t.Fatal(err)
	}
}

// seal makes the shelf read-only, as the box sees it, and restores it for TempDir's cleanup.
func (b *box) seal() {
	b.t.Helper()
	var dirs []string
	filepath.WalkDir(b.shelf, func(p string, d os.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			dirs = append(dirs, p)
		}
		return nil
	})
	for _, d := range dirs {
		os.Chmod(d, 0o555)
	}
	b.t.Cleanup(func() {
		for _, d := range dirs {
			os.Chmod(d, 0o755)
		}
	})
}

func (b *box) writeJSON(name string, v any) {
	b.t.Helper()
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		b.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(b.proj, name), data, 0o644); err != nil {
		b.t.Fatal(err)
	}
}

func (b *box) env() []string {
	env := []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + b.home,
		"POTLUCK_SHELF=" + b.shelf,
		"npm_config_update_notifier=false",
	}
	if !b.noCacheEnv {
		env = append(env, "npm_config_cache="+b.cache)
	}
	return env
}

// run runs the helper with sh and returns its stderr; it fails the test on a non-zero exit.
func (b *box) run(args ...string) string {
	b.t.Helper()
	script := filepath.Join(b.t.TempDir(), shelfhelper.Name)
	if err := os.WriteFile(script, shelfhelper.Script, 0o755); err != nil {
		b.t.Fatal(err)
	}
	cmd := exec.Command("sh", append([]string{script}, args...)...)
	cmd.Dir = b.proj
	cmd.Env = b.env()
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		b.t.Fatalf("potluck-deps %v: %v\n%s", args, err, stderr.String())
	}
	return stderr.String()
}

func (b *box) wants() []string {
	b.t.Helper()
	data, err := os.ReadFile(filepath.Join(b.home, ".potluck", "wants"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		b.t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
}

// linked lists the cache's symlinks, relative to _cacache, with where each points.
func (b *box) linked() map[string]string {
	b.t.Helper()
	out := map[string]string{}
	root := filepath.Join(b.cache, "_cacache")
	filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.Type()&os.ModeSymlink == 0 {
			return nil
		}
		target, _ := os.Readlink(p)
		rel, _ := filepath.Rel(root, p)
		out[rel] = target
		return nil
	})
	return out
}

func want(name, version string, p pkg) string {
	return "npm " + name + " " + version + " " + p.integrity()
}

func reg(name, version string) string {
	base := name
	if i := strings.LastIndex(name, "/"); i >= 0 {
		base = name[i+1:]
	}
	return "https://registry.npmjs.org/" + name + "/-/" + base + "-" + version + ".tgz"
}

// The packages every lockfile shape below names, and what the helper must make of them.
var (
	onShelf  = newPkg("left-pad 1.3.0")
	missing  = newPkg("is-odd 3.0.1")
	scoped   = newPkg("@types/node 22.0.0")
	aliased  = newPkg("string-width 4.2.3")
	both     = newPkg("ms 2.1.3")
	private  = newPkg("@acme/secret 1.0.0")
	gitDep   = newPkg("from-git 1.0.0")
	sha1Only = "sha1-2jmj7l5rSw0yVb/vlWAYkK/YBwk="
)

func shapeWants() []string {
	return []string{
		want("left-pad", "1.3.0", onShelf),
		want("is-odd", "3.0.1", missing),
		want("@types/node", "22.0.0", scoped),
		want("string-width", "4.2.3", aliased),
		want("ms", "2.1.3", both),
	}
}

func shapeLinks(b *box) map[string]string {
	return map[string]string{
		onShelf.rel(): filepath.Join(b.shelf, "npm", onShelf.rel()),
		scoped.rel():  filepath.Join(b.shelf, "npm", scoped.rel()),
		both.rel():    filepath.Join(b.shelf, "npm", both.rel()),
	}
}

func shelveShape(b *box) {
	for _, p := range []pkg{onShelf, scoped, both, private, gitDep} {
		b.shelve(p)
	}
	b.seal()
}

// packages is the "packages" map of a v2 or v3 lockfile holding every case once.
func packages() map[string]any {
	return map[string]any{
		"": map[string]any{"name": "app", "version": "1.0.0"},
		"node_modules/left-pad": map[string]any{
			"version": "1.3.0", "resolved": reg("left-pad", "1.3.0"), "integrity": onShelf.integrity()},
		"node_modules/is-odd": map[string]any{
			"version": "3.0.1", "resolved": reg("is-odd", "3.0.1"), "integrity": missing.integrity()},
		"node_modules/@types/node": map[string]any{
			"version": "22.0.0", "resolved": "https://registry.npmjs.org/@types%2fnode/-/node-22.0.0.tgz", "integrity": scoped.integrity()},
		// npm alias: installed as "sw", fetched as string-width.
		"node_modules/sw": map[string]any{
			"name": "string-width", "version": "4.2.3", "resolved": reg("string-width", "4.2.3"),
			"integrity": aliased.integrity()},
		// sha512 listed after a sha1; the same package again nested under another.
		"node_modules/ms": map[string]any{
			"version": "2.1.3", "resolved": reg("ms", "2.1.3"), "integrity": sha1Only + " " + both.integrity()},
		"node_modules/debug/node_modules/ms": map[string]any{
			"version": "2.1.3", "resolved": reg("ms", "2.1.3"), "integrity": both.integrity()},
		// Another registry, on the shelf or not: never named, never linked.
		"node_modules/@acme/secret": map[string]any{
			"version": "1.0.0", "resolved": "https://npm.pkg.github.com/download/@acme/secret/1.0.0/abc",
			"integrity": private.integrity()},
		"node_modules/lookalike": map[string]any{
			"version": "1.0.0", "resolved": "https://registry.npmjs.org.evil.example/lookalike/-/lookalike-1.0.0.tgz",
			"integrity": private.integrity()},
		"node_modules/from-git": map[string]any{
			"version": "1.0.0", "resolved": "git+ssh://git@github.com/acme/from-git.git#0123456789abcdef",
			"integrity": gitDep.integrity()},
		"node_modules/local": map[string]any{"version": "1.0.0", "resolved": "file:../local"},
		"node_modules/old": map[string]any{
			"version": "1.0.0", "resolved": reg("old", "1.0.0"), "integrity": sha1Only},
		// The name and version come from the URL; one that would break a wants line is left out.
		"node_modules/bad-name": map[string]any{
			"version": "1.0.0", "resolved": reg("bad\nname", "1.0.0"), "integrity": onShelf.integrity()},
		// %2f decodes only as a scope's separator.
		"node_modules/slash": map[string]any{
			"version": "1.0.0", "resolved": "https://registry.npmjs.org/foo%2fbar/-/foo%2fbar-1.0.0.tgz", "integrity": onShelf.integrity()},
		"node_modules/@s/slash": map[string]any{
			"version": "1.0.0", "resolved": "https://registry.npmjs.org/@s/a%2fb/-/a%2fb-1.0.0.tgz", "integrity": onShelf.integrity()},
		"node_modules/not-a-tarball": map[string]any{
			"version": "1.0.0", "resolved": "https://registry.npmjs.org/x/-/../../evil-1.0.0.tgz", "integrity": onShelf.integrity()},
	}
}

func checkShape(t *testing.T, b *box) {
	t.Helper()
	got := b.wants()
	exp := shapeWants()
	slices.Sort(got)
	slices.Sort(exp)
	if !slices.Equal(got, exp) {
		t.Errorf("wants:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(exp, "\n"))
	}
	links := b.linked()
	for rel, target := range shapeLinks(b) {
		if links[rel] != target {
			t.Errorf("link %s = %q, want %q", rel, links[rel], target)
		}
		delete(links, rel)
	}
	for rel, target := range links {
		t.Errorf("unexpected link %s -> %s", rel, target)
	}
}

func TestLinkV3(t *testing.T) {
	b := newBox(t)
	shelveShape(b)
	b.writeJSON("package-lock.json", map[string]any{"name": "app", "lockfileVersion": 3, "packages": packages()})
	b.run("link")
	checkShape(t, b)
}

func TestLinkV2(t *testing.T) {
	b := newBox(t)
	shelveShape(b)
	// v2 carries both maps; "dependencies" here disagrees, to show "packages" is the one read.
	b.writeJSON("package-lock.json", map[string]any{
		"name": "app", "lockfileVersion": 2, "packages": packages(),
		"dependencies": map[string]any{"decoy": map[string]any{
			"version": "1.0.0", "resolved": reg("decoy", "1.0.0"), "integrity": private.integrity()}},
	})
	b.run("link", b.proj)
	checkShape(t, b)
}

func TestLinkV1(t *testing.T) {
	b := newBox(t)
	shelveShape(b)
	b.writeJSON("package-lock.json", map[string]any{
		"name": "app", "lockfileVersion": 1,
		"dependencies": map[string]any{
			"left-pad": map[string]any{"version": "1.3.0", "resolved": reg("left-pad", "1.3.0"), "integrity": onShelf.integrity()},
			"debug": map[string]any{"version": "4.0.0", "resolved": "https://npm.pkg.github.com/debug.tgz",
				"integrity": private.integrity(),
				"dependencies": map[string]any{
					"ms": map[string]any{"version": "2.1.3", "resolved": reg("ms", "2.1.3"), "integrity": both.integrity()},
				}},
			"old": map[string]any{"version": "1.0.0", "resolved": reg("old", "1.0.0"), "integrity": sha1Only},
			// npm alias in v1: the version field is "npm:<name>@<version>"; the URL names the package.
			"sw": map[string]any{"version": "npm:string-width@4.2.3", "resolved": reg("string-width", "4.2.3"),
				"integrity": aliased.integrity()},
		},
	})
	b.run("link")
	got := b.wants()
	exp := []string{want("left-pad", "1.3.0", onShelf), want("ms", "2.1.3", both), want("string-width", "4.2.3", aliased)}
	slices.Sort(got)
	slices.Sort(exp)
	if !slices.Equal(got, exp) {
		t.Errorf("wants = %q, want %q", got, exp)
	}
	if n := len(b.linked()); n != 2 {
		t.Errorf("%d links, want 2: %v", n, b.linked())
	}
}

func TestLinkWorkspaces(t *testing.T) {
	b := newBox(t)
	shelveShape(b)
	b.writeJSON("package-lock.json", map[string]any{
		"name": "mono", "lockfileVersion": 3,
		"packages": map[string]map[string]any{
			"":                {"name": "mono", "workspaces": []string{"packages/*"}},
			"packages/a":      {"name": "a", "version": "0.1.0", "dependencies": map[string]string{"ms": "^2"}},
			"node_modules/a":  {"resolved": "packages/a", "link": true},
			"node_modules/b":  {"resolved": "packages/b", "link": true},
			"packages/b":      {"name": "b", "version": "0.1.0"},
			"node_modules/ms": {"version": "2.1.3", "resolved": reg("ms", "2.1.3"), "integrity": both.integrity()},
			"packages/a/node_modules/left-pad": {
				"version": "1.3.0", "resolved": reg("left-pad", "1.3.0"), "integrity": onShelf.integrity()},
		},
	})
	b.run("link")
	got := b.wants()
	exp := []string{want("ms", "2.1.3", both), want("left-pad", "1.3.0", onShelf)}
	slices.Sort(got)
	slices.Sort(exp)
	if !slices.Equal(got, exp) {
		t.Errorf("wants = %q, want %q", got, exp)
	}
	if n := len(b.linked()); n != 2 {
		t.Errorf("%d links, want 2: %v", n, b.linked())
	}
}

// npm reads npm-shrinkwrap.json before package-lock.json; so does the helper.
func TestLinkPrefersShrinkwrap(t *testing.T) {
	b := newBox(t)
	shelveShape(b)
	lock := func(name string, p pkg) map[string]any {
		return map[string]any{"lockfileVersion": 3, "packages": map[string]any{
			"node_modules/" + name: map[string]any{"version": "1.0.0", "resolved": reg(name, "1.0.0"), "integrity": p.integrity()}}}
	}
	b.writeJSON("package-lock.json", lock("from-lock", missing))
	b.writeJSON("npm-shrinkwrap.json", lock("from-shrinkwrap", onShelf))
	b.run("link")
	if got, exp := b.wants(), []string{want("from-shrinkwrap", "1.0.0", onShelf)}; !slices.Equal(got, exp) {
		t.Errorf("wants = %q, want %q", got, exp)
	}
}

// Without a mounted shelf the helper does nothing at all: no wants, no cache.
func TestLinkNoShelf(t *testing.T) {
	b := newBox(t)
	b.writeJSON("package-lock.json", map[string]any{"lockfileVersion": 3, "packages": packages()})
	if err := os.Remove(b.shelf); err != nil {
		t.Fatal(err)
	}
	b.run("link")
	if w := b.wants(); w != nil {
		t.Errorf("wants written without a shelf: %q", w)
	}
	if _, err := os.Stat(b.cache); !os.IsNotExist(err) {
		t.Errorf("cache touched without a shelf: %v", err)
	}
}

// A shelf with no npm blobs yet still records wants, so the host can fill it.
func TestLinkEmptyShelf(t *testing.T) {
	b := newBox(t)
	b.writeJSON("package-lock.json", map[string]any{"lockfileVersion": 3, "packages": packages()})
	b.run("link")
	got, exp := b.wants(), shapeWants()
	slices.Sort(got)
	slices.Sort(exp)
	if !slices.Equal(got, exp) {
		t.Errorf("wants = %q, want %q", got, exp)
	}
	if l := b.linked(); len(l) != 0 {
		t.Errorf("links from an empty shelf: %v", l)
	}
}

func TestLinkNoLockfile(t *testing.T) {
	b := newBox(t)
	b.shelve(onShelf)
	b.run("link")
	if w := b.wants(); w != nil {
		t.Errorf("wants without a lockfile: %q", w)
	}
}

// Whatever the box's cache already holds at a blob's path is left as it is.
func TestLinkKeepsCachedFile(t *testing.T) {
	b := newBox(t)
	shelveShape(b)
	b.writeJSON("package-lock.json", map[string]any{"lockfileVersion": 3, "packages": packages()})
	own := filepath.Join(b.cache, "_cacache", onShelf.rel())
	if err := os.MkdirAll(filepath.Dir(own), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(own, onShelf.data, 0o644); err != nil {
		t.Fatal(err)
	}
	b.run("link")
	if fi, err := os.Lstat(own); err != nil || !fi.Mode().IsRegular() {
		t.Errorf("box's own cache file replaced: %v %v", fi, err)
	}
}

func TestUsage(t *testing.T) {
	b := newBox(t)
	for _, args := range [][]string{nil, {"unlink"}, {"link", "a", "b"}} {
		cmd := exec.Command("sh", append([]string{"-c", string(shelfhelper.Script), shelfhelper.Name}, args...)...)
		cmd.Env = b.env()
		err := cmd.Run()
		if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 2 {
			t.Errorf("potluck-deps %q: %v, want exit 2", args, err)
		}
	}
}

// tgz is an npm package tarball holding only package/package.json.
func tgz(t *testing.T, name, version string) []byte {
	t.Helper()
	manifest, _ := json.Marshal(map[string]string{"name": name, "version": version})
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: "package/package.json", Mode: 0o644, Size: int64(len(manifest))}); err != nil {
		t.Fatal(err)
	}
	tw.Write(manifest)
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

// The layout works with the npm on PATH: after link, npm ci --offline installs from a cache that
// holds nothing but symlinks to read-only shelf blobs (no index entries).
func TestNpmCiOfflineFromLinks(t *testing.T) {
	b := newBox(t)
	needNpm(t)
	names := []string{"potluck-shelf-probe-a", "@potluck-probe/b"}
	lockPkgs := map[string]any{"": map[string]any{"name": "probe", "version": "1.0.0",
		"dependencies": map[string]string{names[0]: "1.0.0", names[1]: "1.0.0"}}}
	for _, name := range names {
		data := tgz(t, name, "1.0.0")
		sum := sha512.Sum512(data)
		p := pkg{data: data, b64: base64.StdEncoding.EncodeToString(sum[:]), hex: hex.EncodeToString(sum[:])}
		b.shelve(p)
		lockPkgs["node_modules/"+name] = map[string]any{"version": "1.0.0", "resolved": reg(name, "1.0.0"), "integrity": p.integrity()}
	}
	b.seal()
	b.writeJSON("package.json", map[string]any{"name": "probe", "version": "1.0.0", "private": true,
		"dependencies": map[string]string{names[0]: "1.0.0", names[1]: "1.0.0"}})
	b.writeJSON("package-lock.json", map[string]any{"name": "probe", "version": "1.0.0", "lockfileVersion": 3,
		"requires": true, "packages": lockPkgs})
	before := tree(t, b.shelf)
	b.run("link")
	if n := len(b.linked()); n != 2 {
		t.Fatalf("%d links, want 2", n)
	}
	cmd := exec.Command("npm", "ci", "--offline", "--ignore-scripts", "--no-audit", "--no-fund")
	cmd.Dir = b.proj
	cmd.Env = b.env()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("npm ci --offline: %v\n%s", err, out)
	}
	for _, name := range names {
		if _, err := os.Stat(filepath.Join(b.proj, "node_modules", name, "package.json")); err != nil {
			t.Errorf("%s not installed: %v", name, err)
		}
	}
	// Read-only modes do not bind root; show the shelf is untouched either way.
	if after := tree(t, b.shelf); !maps.Equal(before, after) {
		t.Errorf("shelf changed by link and npm ci:\nbefore %v\nafter  %v", before, after)
	}
}

// A blob evicted from the shelf after link left the box's cache link dangling: npm ci reads it as a
// cache miss (ENOTCACHED under --offline, not another error) and otherwise fetches that package from
// the registry (a local one here: npm sends registry.npmjs.org's resolved URLs to --registry) while
// the other still comes from the shelf.
func TestNpmCiRefetchesDanglingLink(t *testing.T) {
	b := newBox(t)
	needNpm(t)
	names := []string{"potluck-shelf-probe-kept", "potluck-shelf-probe-evicted"}
	tarballs := map[string][]byte{}
	var hits []string
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits = append(hits, r.URL.Path)
		mu.Unlock()
		for _, name := range names {
			if r.URL.Path == "/"+name+"/-/"+name+"-1.0.0.tgz" {
				w.Write(tarballs[name])
				return
			}
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	lockPkgs := map[string]any{"": map[string]any{"name": "probe", "version": "1.0.0",
		"dependencies": map[string]string{names[0]: "1.0.0", names[1]: "1.0.0"}}}
	var evicted pkg
	for _, name := range names {
		data := tgz(t, name, "1.0.0")
		tarballs[name] = data
		sum := sha512.Sum512(data)
		p := pkg{data: data, b64: base64.StdEncoding.EncodeToString(sum[:]), hex: hex.EncodeToString(sum[:])}
		b.shelve(p)
		evicted = p
		lockPkgs["node_modules/"+name] = map[string]any{"version": "1.0.0", "resolved": reg(name, "1.0.0"), "integrity": p.integrity()}
	}
	b.writeJSON("package.json", map[string]any{"name": "probe", "version": "1.0.0", "private": true,
		"dependencies": map[string]string{names[0]: "1.0.0", names[1]: "1.0.0"}})
	b.writeJSON("package-lock.json", map[string]any{"name": "probe", "version": "1.0.0", "lockfileVersion": 3,
		"requires": true, "packages": lockPkgs})
	b.run("link")
	if n := len(b.linked()); n != 2 {
		t.Fatalf("%d links, want 2", n)
	}
	if err := os.Remove(filepath.Join(b.shelf, "npm", evicted.rel())); err != nil { // the eviction
		t.Fatal(err)
	}
	b.seal()
	npmCi := func(args ...string) ([]byte, error) {
		cmd := exec.Command("npm", append([]string{"ci", "--ignore-scripts", "--no-audit", "--no-fund", "--registry=" + srv.URL + "/"}, args...)...)
		cmd.Dir = b.proj
		cmd.Env = b.env()
		return cmd.CombinedOutput()
	}
	if out, err := npmCi("--offline"); err == nil || !strings.Contains(string(out), "ENOTCACHED") {
		t.Fatalf("npm ci --offline with a dangling link: %v, want ENOTCACHED\n%s", err, out)
	}
	if out, err := npmCi(); err != nil {
		t.Fatalf("npm ci: %v\n%s", err, out)
	}
	for _, name := range names {
		if _, err := os.Stat(filepath.Join(b.proj, "node_modules", name, "package.json")); err != nil {
			t.Errorf("%s not installed: %v", name, err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if !slices.Contains(hits, "/"+names[1]+"/-/"+names[1]+"-1.0.0.tgz") || slices.Contains(hits, "/"+names[0]+"/-/"+names[0]+"-1.0.0.tgz") {
		t.Errorf("registry requests %v: want only the evicted package's tarball", hits)
	}
}

// Running link again (before every npm ci, say) adds no line already in the wants file.
func TestLinkWantsOnce(t *testing.T) {
	b := newBox(t)
	shelveShape(b)
	b.writeJSON("package-lock.json", map[string]any{"lockfileVersion": 3, "packages": packages()})
	b.run("link")
	b.run("link")
	if got := b.wants(); len(got) != len(shapeWants()) {
		t.Errorf("%d wants lines after two runs, want %d:\n%s", len(got), len(shapeWants()), strings.Join(got, "\n"))
	}
}

// A sha512 that is not 64 bytes in canonical base64 is left out. A newline in an integrity string only
// separates hashes, as npm reads it: it cannot add a wants line.
func TestLinkBadIntegrity(t *testing.T) {
	b := newBox(t)
	short := sha512.Sum512([]byte("x"))
	canonical := onShelf.b64
	// The last base64 character carries 2 unused bits; flipping them decodes to the same bytes.
	alt := []byte(canonical)
	alt[len(alt)-3] ^= 1
	lock := map[string]any{}
	for i, integ := range []string{
		"sha512-" + base64.StdEncoding.EncodeToString(short[:63]),
		"sha512-" + string(alt),
		"sha512-" + strings.TrimRight(canonical, "="),
		"sha512-" + canonical + "\nnpm evil 1.0.0",
		"sha384-" + canonical,
	} {
		name := "p" + string(rune('a'+i))
		lock["node_modules/"+name] = map[string]any{"version": "1.0.0", "resolved": reg(name, "1.0.0"), "integrity": integ}
	}
	b.shelve(onShelf)
	b.writeJSON("package-lock.json", map[string]any{"lockfileVersion": 3, "packages": lock})
	b.run("link")
	if got, exp := b.wants(), []string{"npm pd 1.0.0 " + onShelf.integrity()}; !slices.Equal(got, exp) {
		t.Errorf("wants = %q, want %q", got, exp)
	}
	if l := b.linked(); len(l) != 1 {
		t.Errorf("links = %v, want only pd's", l)
	}
}

// A symlink on the shelf is not followed, and a dangling symlink already in the box's cache is left.
func TestLinkSymlinks(t *testing.T) {
	b := newBox(t)
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, onShelf.data, 0o444); err != nil {
		t.Fatal(err)
	}
	onShelfPath := filepath.Join(b.shelf, "npm", onShelf.rel())
	os.MkdirAll(filepath.Dir(onShelfPath), 0o755)
	if err := os.Symlink(outside, onShelfPath); err != nil {
		t.Fatal(err)
	}
	b.shelve(both)
	dangling := filepath.Join(b.cache, "_cacache", both.rel())
	gone := filepath.Join(t.TempDir(), "gone")
	os.MkdirAll(filepath.Dir(dangling), 0o755)
	if err := os.Symlink(gone, dangling); err != nil {
		t.Fatal(err)
	}
	b.writeJSON("package-lock.json", map[string]any{"lockfileVersion": 3, "packages": map[string]any{
		"node_modules/left-pad": map[string]any{"version": "1.3.0", "resolved": reg("left-pad", "1.3.0"), "integrity": onShelf.integrity()},
		"node_modules/ms":       map[string]any{"version": "2.1.3", "resolved": reg("ms", "2.1.3"), "integrity": both.integrity()},
	}})
	b.run("link")
	links := b.linked()
	if _, ok := links[onShelf.rel()]; ok {
		t.Errorf("linked through a symlink on the shelf: %v", links)
	}
	if links[both.rel()] != gone {
		t.Errorf("dangling cache entry replaced: %v", links)
	}
	if len(b.wants()) != 2 {
		t.Errorf("wants = %q, want both packages named", b.wants())
	}
}

// A relative npm_config_cache is relative to dir, where npm ci runs.
func TestLinkRelativeCache(t *testing.T) {
	b := newBox(t)
	shelveShape(b)
	sub := filepath.Join(b.proj, "sub")
	os.MkdirAll(sub, 0o755)
	data, _ := json.Marshal(map[string]any{"lockfileVersion": 3, "packages": packages()})
	if err := os.WriteFile(filepath.Join(sub, "package-lock.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	b.cache = "rel-cache"
	b.run("link", "sub")
	b.cache = filepath.Join(sub, "rel-cache")
	checkShape(t, b)
}

// Without npm_config_cache the helper asks npm, which reads the project's .npmrc.
func TestLinkCacheFromNpmrc(t *testing.T) {
	b := newBox(t)
	needNpm(t)
	shelveShape(b)
	b.writeJSON("package-lock.json", map[string]any{"lockfileVersion": 3, "packages": packages()})
	b.cache = filepath.Join(t.TempDir(), "npmrc-cache")
	if err := os.WriteFile(filepath.Join(b.proj, ".npmrc"), []byte("cache="+b.cache+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	b.noCacheEnv = true
	b.run("link")
	checkShape(t, b)
}

func TestHelp(t *testing.T) {
	b := newBox(t)
	b.run("help")
}

// Without node the helper links nothing, names nothing, and still exits 0.
func TestLinkNoNode(t *testing.T) {
	b := newBox(t)
	shelveShape(b)
	b.writeJSON("package-lock.json", map[string]any{"lockfileVersion": 3, "packages": packages()})
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(sh, "-c", string(shelfhelper.Script), shelfhelper.Name, "link")
	cmd.Dir = b.proj
	cmd.Env = append(b.env(), "PATH="+t.TempDir()) // later PATH wins: an empty directory
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("potluck-deps link without node: %v\n%s", err, out)
	}
	if w := b.wants(); w != nil {
		t.Errorf("wants without node: %q", w)
	}
}
