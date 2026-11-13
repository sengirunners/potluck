package vmhost

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The owner's cache volume (zm-eakh.12.43): a box gets it read-only at
// ociCacheDir when the owner made it, and no mount when not; with it, GOPROXY
// names the shelf's Go modules first, unless the image sets its own.
func TestOCICacheVolume(t *testing.T) {
	const want = "type=volume,source=potluck-cache,target=/opt/potluck-cache,readonly,volume-nocopy"
	var env []string
	mounts := func(o ociCLI, dir, vm string) (ms []string) {
		t.Helper()
		if err := o.Clone("zm-linux-ci", vm, 2, 4096); err != nil {
			t.Fatal(err)
		}
		b, _ := os.ReadFile(filepath.Join(dir, "c", vm, "args"))
		args := strings.Split(strings.TrimSpace(string(b)), "\n")
		env = nil
		for i, a := range args {
			if a == "--mount" && i+1 < len(args) {
				ms = append(ms, args[i+1])
			}
			if a == "-e" && i+1 < len(args) {
				env = append(env, args[i+1])
			}
		}
		if args[len(args)-3] != "zm-linux-ci" {
			t.Fatalf("create's image is not where it was: %v", args)
		}
		return ms
	}
	o, dir := newFakeOCI(t, "none")
	if ms := mounts(o, dir, "a"); len(ms) != 0 || len(env) != 0 {
		t.Errorf("no cache volume, yet mounts %v, env %v", ms, env)
	}
	if err := os.WriteFile(filepath.Join(dir, "cache-volume"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "images", "zm-linux-ci"), nil, 0o600); err != nil { // the GOPROXY check inspects it
		t.Fatal(err)
	}
	if ms := mounts(o, dir, "b"); !slices.Equal(ms, []string{want}) {
		t.Errorf("mounts %v, want %s", ms, want)
	}
	if goproxy := "GOPROXY=file:///opt/potluck-cache/goproxy,https://proxy.golang.org,direct"; !slices.Equal(env, []string{goproxy}) {
		t.Errorf("env %v, want %s", env, goproxy)
	}
	calls, _ := os.ReadFile(filepath.Join(dir, "calls"))
	if n := strings.Count(string(calls), "volume inspect potluck-cache\n"); n != 2 {
		t.Errorf("asked for the cache volume %d times in 2 creates:\n%s", n, calls)
	}
	// An image that sets GOPROXY keeps it; the mount stays.
	if err := os.WriteFile(filepath.Join(dir, "images", "zm-linux-ci.inspect"), []byte("\nPATH=/usr/bin\nGOPROXY=https://proxy.example\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if ms := mounts(o, dir, "c"); !slices.Equal(ms, []string{want}) || len(env) != 0 {
		t.Errorf("an image with its own GOPROXY: mounts %v, env %v", ms, env)
	}
}

// A runner box with the cache finds its tools where setup-* actions look
// (RUNNER_TOOL_CACHE/<tool>/<version>/<arch> and <arch>.complete) through a
// link per version in a tool directory the job can still write (a version
// the cache lacks installs beside the links). GOPROXY is the box's (set at
// create, cacheArgs), never the launch's. Without a cache, or with no such
// directory in it, the runner starts as before; what the image or the owner
// already set stays.
func TestJITLaunchUsesTheCache(t *testing.T) {
	launch := func(t *testing.T, cache string, env ...string) (dir string, got map[string]string) {
		t.Helper()
		dir, _ = filepath.EvalSymlinks(t.TempDir())
		runner := filepath.Join(dir, "actions-runner")
		if err := os.MkdirAll(runner, 0o700); err != nil {
			t.Fatal(err)
		}
		sh := "#!/bin/sh\nprintf 'tools=%s\\ngoproxy=%s\\n' \"${RUNNER_TOOL_CACHE-unset}\" \"${GOPROXY-unset}\"\n"
		if err := os.WriteFile(filepath.Join(runner, "run.sh"), []byte(sh), 0o755); err != nil {
			t.Fatal(err)
		}
		c := exec.Command("sh", "-c", jitLaunch, "sh", runner, "", "", "", cache)
		c.Stdin = strings.NewReader("eyJzZWtyaXQiOiJKSVQtY29uZmlnLXZhbHVlIn0=\n")
		c.Env = append([]string{"PATH=" + os.Getenv("PATH"), "HOME=" + dir}, env...)
		out, err := c.CombinedOutput()
		if err != nil {
			t.Fatalf("%v: %s", err, out)
		}
		got = map[string]string{}
		for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			k, v, _ := strings.Cut(l, "=")
			got[k] = v
		}
		return dir, got
	}
	cache, _ := filepath.EvalSymlinks(t.TempDir())
	// go 1.28.0 is half-filled (no x64.complete: an interrupted fill, or a box started mid-copy), as is 1.27.3's arm64.
	for _, d := range []string{"tools/go/1.27.3/x64/bin", "tools/go/1.27.3/arm64", "tools/go/1.28.0/x64", "tools/node/22.11.0/x64", "goproxy/golang.org"} {
		if err := os.MkdirAll(filepath.Join(cache, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{"tools/go/1.27.3/x64.complete", "tools/go/1.27.3/x64/bin/go", "tools/node/22.11.0/x64.complete", "tools/stray-file", "tools/go/stray.complete"} {
		if err := os.WriteFile(filepath.Join(cache, f), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ro := []string{"tools/go/1.27.3", "tools/go/1.28.0", "tools/go"} // as a read-only mount refuses writes
	for _, d := range ro {
		if err := os.Chmod(filepath.Join(cache, d), 0o555); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		for _, d := range ro {
			_ = os.Chmod(filepath.Join(cache, d), 0o755)
		}
	})

	dir, got := launch(t, cache)
	tools := filepath.Join(dir, "actions-runner", "_work", "_tool")
	if got["tools"] != tools || got["goproxy"] != "unset" {
		t.Errorf("the runner started with %v", got)
	}
	for _, f := range []string{"go/1.27.3/x64/bin/go", "go/1.27.3/x64.complete", "node/22.11.0/x64", "node/22.11.0/x64.complete"} { // what a setup-* action's find checks
		if _, err := os.Stat(filepath.Join(tools, f)); err != nil {
			t.Errorf("the tool directory lacks %s: %v", f, err)
		}
	}
	if l, err := os.Readlink(filepath.Join(tools, "go", "1.27.3", "x64")); err != nil || l != filepath.Join(cache, "tools/go/1.27.3/x64") {
		t.Errorf("go 1.27.3 x64 is %q (%v), want a link into the cache", l, err)
	}
	// Only what the cache marks complete is linked, and each version is a
	// directory of the box's own: a tool the cache holds half of (no
	// <arch>.complete), another arch of a cached version and a version it
	// lacks all install in the box, as a setup-* action does (it removes the
	// directory and the marker, then writes both).
	for _, f := range []string{"go/1.28.0", "go/1.27.3/arm64", "go/stray", "stray-file"} {
		if _, err := os.Lstat(filepath.Join(tools, f)); err == nil {
			t.Errorf("%s is in the tool directory, though the cache does not mark it complete", f)
		}
	}
	for _, d := range []string{"go/1.28.0/x64", "go/1.27.3/arm64", "go/1.29.0/x64"} {
		p := filepath.Join(tools, d)
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Errorf("%s cannot be installed in the box: %v", d, err)
		} else if err := os.WriteFile(p+".complete", nil, 0o644); err != nil {
			t.Errorf("%s cannot be marked complete: %v", d, err)
		}
	}
	// A cached one can be replaced too: the links are the box's to remove.
	if err := os.Remove(filepath.Join(tools, "go/1.27.3/x64.complete")); err != nil {
		t.Errorf("the box cannot replace a cached tool: %v", err)
	}

	// The image's own tool directory and an owner's GOPROXY stay.
	own := filepath.Join(t.TempDir(), "hostedtoolcache")
	_, got = launch(t, cache, "RUNNER_TOOL_CACHE="+own, "GOPROXY=https://proxy.example")
	if got["tools"] != own || got["goproxy"] != "https://proxy.example" {
		t.Errorf("with its own settings the runner started with %v", got)
	}
	if _, err := os.Stat(filepath.Join(own, "go/1.27.3/x64.complete")); err != nil {
		t.Errorf("the image's tool directory lacks the cache's go: %v", err)
	}

	// No cache (another layer, or the owner made none), or an empty one: as before.
	for _, c := range []string{"", filepath.Join(cache, "absent"), t.TempDir()} {
		dir, got := launch(t, c)
		if got["tools"] != "unset" || got["goproxy"] != "unset" {
			t.Errorf("cache %q: the runner started with %v", c, got)
		}
		if _, err := os.Stat(filepath.Join(dir, "actions-runner", "_work")); err == nil {
			t.Errorf("cache %q: a work directory was made", c)
		}
	}
}

// Only the oci layer mounts the cache, so only its launch names it.
func TestJITCacheDir(t *testing.T) {
	for vm, want := range map[string]string{"oci": ociCacheDir, "tart": "", "incus": ""} {
		if got := jitCache(config{VM: vm}); got != want {
			t.Errorf("vm %s: cache %q, want %q", vm, got, want)
		}
	}
}
