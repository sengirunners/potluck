package vmhost

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sengirunners/potluck/protocol"
)

// In tests the fake layers' runners have no version, so they are left as they
// are, unless a test names a fake release (ZMRT_RUNNER_TARBALL): then every
// pinned sha256 is that file's and the host "downloads" ZMRT_RUNNER_SERVE (else
// the same file), or fails to with ZMRT_RUNNER_HOSTFAIL set. Set here, so the
// supervisor (this binary, TestMain) does the same.
func init() {
	skipRunnerRefresh = func(config) bool { return os.Getenv("ZMRT_RUNNER_TARBALL") == "" } // no test reaches GitHub
	p := os.Getenv("ZMRT_RUNNER_TARBALL")
	if p == "" {
		return
	}
	b, err := os.ReadFile(p)
	if err != nil {
		panic(err)
	}
	sum := sha256.Sum256(b)
	for k := range runnerSHA256 {
		runnerSHA256[k] = hex.EncodeToString(sum[:])
	}
	runnerHTTP = func(context.Context, string) (io.ReadCloser, error) {
		if os.Getenv("ZMRT_RUNNER_HOSTFAIL") != "" {
			return nil, errors.New("the host is offline")
		}
		if s := os.Getenv("ZMRT_RUNNER_SERVE"); s != "" {
			return os.Open(s)
		}
		return os.Open(p)
	}
}

func TestRunnerTooOld(t *testing.T) {
	for v, want := range map[string]bool{
		"2.332.0": true, "v2.332.0": true, "2.338.1": true, "1.999.999": true, "2.9.0": true,
		runnerMinVersion: false, "v" + runnerMinVersion: false, "2.338.3": false, "2.339.0": false, "3.0.0": false, "2.1000.0": false,
		"": true, "-": true, "2.338": true, "2.338.2.1": true, "two.338.2": true, "2.-1.0": true, "2.338.2-beta": true,
	} {
		if got := runnerTooOld(v); got != want {
			t.Errorf("runnerTooOld(%q) = %v, want %v", v, got, want)
		}
	}
}

func TestRunnerPlatform(t *testing.T) {
	for in, want := range map[[2]string]string{
		{"Darwin", "arm64"}: "osx-arm64", {"Linux", "aarch64"}: "linux-arm64", {"Linux", "arm64"}: "linux-arm64",
		{"Linux", "x86_64"}: "linux-x64", {"Darwin", "x86_64"}: "", {"Linux", "armv7l"}: "", {"FreeBSD", "amd64"}: "",
	} {
		got, err := runnerPlatform(in[0], in[1])
		if got != want || (err == nil) != (want != "") {
			t.Errorf("runnerPlatform(%q, %q) = %q, %v; want %q", in[0], in[1], got, err, want)
		}
	}
}

// Every pin is a whole sha256 for an https URL on the pinned tag, and the
// release installed is never below the minimum it is installed to meet.
func TestRunnerPins(t *testing.T) {
	if os.Getenv("ZMRT_RUNNER_TARBALL") != "" {
		t.Skip("the pins are a test's")
	}
	if runnerTooOld(runnerPinVersion) {
		t.Fatalf("runnerPinVersion %s is below runnerMinVersion %s", runnerPinVersion, runnerMinVersion)
	}
	hexRe := regexp.MustCompile(`^[0-9a-f]{64}$`)
	for _, p := range []string{"osx-arm64", "linux-arm64", "linux-x64"} {
		if !hexRe.MatchString(runnerSHA256[p]) {
			t.Errorf("%s: sha256 %q", p, runnerSHA256[p])
		}
		if u := runnerURL(p); u != "https://github.com/actions/runner/releases/download/v"+runnerPinVersion+"/actions-runner-"+p+"-"+runnerPinVersion+".tar.gz" {
			t.Errorf("%s: url %s", p, u)
		}
	}
	if len(runnerSHA256) != 3 {
		t.Errorf("pinned platforms %v", runnerSHA256)
	}
}

// withPin pins plat to sum for one test.
func withPin(t *testing.T, plat string, content []byte) {
	t.Helper()
	sum := sha256.Sum256(content)
	old, had := runnerSHA256[plat]
	runnerSHA256[plat] = hex.EncodeToString(sum[:])
	t.Cleanup(func() {
		if had {
			runnerSHA256[plat] = old
		} else {
			delete(runnerSHA256, plat)
		}
	})
}

func TestVerifyRunnerSHA(t *testing.T) {
	withPin(t, "test-plat", []byte("the release"))
	if err := verifyRunnerSHA("test-plat", strings.NewReader("the release")); err != nil {
		t.Errorf("the pinned bytes: %v", err)
	}
	if err := verifyRunnerSHA("test-plat", strings.NewReader("the release, changed")); err == nil || !strings.Contains(err.Error(), "not the pinned") {
		t.Errorf("other bytes: %v, want not the pinned", err)
	}
	if err := verifyRunnerSHA("no-such-plat", strings.NewReader("x")); err == nil {
		t.Error("an unpinned platform verified")
	}
}

// The host keeps a downloaded tarball only when its sha256 is the pin's, and
// downloads it once.
func TestHostRunnerCachesOnlyVerified(t *testing.T) {
	withPin(t, "test-plat", []byte("the release"))
	serve := "tampered"
	var gets atomic.Int32
	old := runnerHTTP
	runnerHTTP = func(_ context.Context, url string) (io.ReadCloser, error) {
		if !strings.HasPrefix(url, "https://github.com/actions/runner/releases/download/v"+runnerPinVersion+"/") {
			t.Errorf("fetched %s", url)
		}
		gets.Add(1)
		return io.NopCloser(strings.NewReader(serve)), nil
	}
	t.Cleanup(func() { runnerHTTP = old })
	dir := t.TempDir()
	if _, err := hostRunner(context.Background(), dir, "test-plat"); err == nil || !strings.Contains(err.Error(), "not the pinned") {
		t.Fatalf("a tampered download: %v, want not the pinned", err)
	}
	if es, _ := os.ReadDir(filepath.Join(dir, "cache", "runner")); len(es) != 1 || es[0].Name() != "lock" {
		t.Errorf("after a tampered download the cache holds %v", es)
	}
	serve = "the release"
	p, err := hostRunner(context.Background(), dir, "test-plat")
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(p); string(b) != "the release" {
		t.Errorf("cached %q", b)
	}
	if _, err := hostRunner(context.Background(), dir, "test-plat"); err != nil || gets.Load() != 2 {
		t.Errorf("a second call: %v, %d downloads in all, want 2", err, gets.Load())
	}
	if _, err := hostRunner(context.Background(), dir, "no-such-plat"); err == nil {
		t.Error("an unpinned platform fetched")
	}
}

// imageRunnerListener models the stdout logger enabled by GitHub's image.
func imageRunnerListener(version string) string {
	return fmt.Sprintf(`#!/bin/sh
if [ "$ACTIONS_RUNNER_PRINT_LOG_TO_STDOUT" = 1 ]; then
 echo '[RUNNER 2026-11-12 01:21:02Z INFO HostContext] No proxy settings were found based on environmental variables (http_proxy/https_proxy/HTTP_PROXY/HTTPS_PROXY)'
fi
printf '%%s\n' %s
`, version)
}

// fakeRelease is a runner tarball whose Runner.Listener says version and
// whose run.sh records its arguments and runtime logging flag, then stays up.
func fakeRelease(t *testing.T, version, started string) string {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, f := range []struct{ name, body string }{
		{"./bin/Runner.Listener", imageRunnerListener(version)},
		{"./externals/node24/bin/node", "#!/bin/sh\n"},
		{"./run.sh", fmt.Sprintf("#!/bin/sh\nprintf '%%s' \"$ACTIONS_RUNNER_PRINT_LOG_TO_STDOUT\" >%q\necho \"$*\" >%q\nexec sleep 60\n", started+".stdout-log-setting", started)},
		{"./config.sh", "#!/bin/sh\n"},
	} {
		if err := tw.WriteHeader(&tar.Header{Name: f.name, Mode: 0o755, Size: int64(len(f.body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		_, _ = tw.Write([]byte(f.body))
	}
	_ = tw.Close()
	_ = gz.Close()
	p := filepath.Join(t.TempDir(), "release.tar.gz")
	if err := os.WriteFile(p, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// oldRunner is an image's runner dir: version, its own .env, an old binary.
func oldRunner(t *testing.T, version string) string {
	t.Helper()
	r := filepath.Join(t.TempDir(), "actions-runner")
	for name, body := range map[string]string{
		"run.sh":                "#!/bin/sh\necho old runner >&2\nexec sleep 60\n",
		"bin/Runner.Listener":   "#!/bin/sh\necho " + version + "\n",
		"bin/old-only.dll":      "",
		".env":                  "ImageOS=macos26\n",
		"_work/_tool/go/keepme": "",
	} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(r, name)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(r, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return r
}

// curlShim puts a curl first on PATH that "downloads" serve (or fails with
// serve empty) and counts its calls in the returned file.
func curlShim(t *testing.T, serve string) string {
	t.Helper()
	d := t.TempDir()
	calls := filepath.Join(d, "calls")
	body := fmt.Sprintf("#!/bin/sh\necho \"$*\" >>%q\n", calls)
	if serve == "" {
		body += "echo 'curl: (6) Could not resolve host' >&2\nexit 6\n"
	} else {
		body += fmt.Sprintf("o=\nwhile [ $# -gt 0 ]; do [ \"$1\" = -o ] && o=$2; shift; done\ncat %q >\"$o\"\n", serve)
	}
	if err := os.WriteFile(filepath.Join(d, "curl"), []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", d+string(os.PathListSeparator)+os.Getenv("PATH"))
	return calls
}

// The install script checks the sha256 before it touches the runner, and
// replaces only bin/ and externals/.
func TestRunnerInstallScript(t *testing.T) {
	rel := fakeRelease(t, runnerPinVersion, filepath.Join(t.TempDir(), "started"))
	b, _ := os.ReadFile(rel)
	sum := sha256.Sum256(b)
	good := hex.EncodeToString(sum[:])
	run := func(dir, sha, mode string, stdin io.Reader) error {
		cmd := exec.Command("sh", "-c", runnerInstall, "sh", dir, sha, "https://example.invalid/r.tar.gz", mode)
		cmd.Stdin = stdin
		out, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("%v: %s", err, out)
		}
		return nil
	}
	listener := func(dir string) string {
		out, _ := exec.Command(filepath.Join(dir, "bin", "Runner.Listener")).Output()
		return strings.TrimSpace(string(out))
	}

	dir := oldRunner(t, "2.332.0")
	if err := run(dir, strings.Repeat("0", 64), "host", bytes.NewReader(b)); err == nil || !strings.Contains(err.Error(), "not the pinned") {
		t.Fatalf("a wrong sha: %v, want not the pinned", err)
	}
	if v := listener(dir); v != "2.332.0" {
		t.Errorf("a refused tarball changed the runner: it says %q", v)
	}
	if err := run(dir, good, "host", bytes.NewReader(b)); err != nil {
		t.Fatal(err)
	}
	if v := listener(dir); v != runnerPinVersion {
		t.Errorf("after the install the runner says %q", v)
	}
	for _, keep := range []string{".env", "_work/_tool/go/keepme"} {
		if _, err := os.Stat(filepath.Join(dir, keep)); err != nil {
			t.Errorf("the image's %s went: %v", keep, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "bin", "old-only.dll")); err == nil {
		t.Error("the old bin/ survived the install")
	}

	// In the guest: the same check on what curl got.
	dir = oldRunner(t, "2.332.0")
	other := filepath.Join(t.TempDir(), "other")
	_ = os.WriteFile(other, []byte("not the release"), 0o600)
	curlShim(t, other)
	if err := run(dir, good, "guest", nil); err == nil || !strings.Contains(err.Error(), "not the pinned") {
		t.Errorf("a tampered guest download: %v, want not the pinned", err)
	}
	if v := listener(dir); v != "2.332.0" {
		t.Errorf("a tampered guest download changed the runner: %q", v)
	}
	curlShim(t, rel)
	if err := run(dir, good, "guest", nil); err != nil || listener(dir) != runnerPinVersion {
		t.Errorf("a guest download: %v, runner says %q", err, listener(dir))
	}
}

// jitRefreshBox is a fake JIT box binding whose image's runner is old.
func jitRefreshBox(t *testing.T, version string) (map[string]string, string, string) {
	t.Helper()
	_, _, m := fakeBinding(t, "linux-img")
	delete(m, "image")
	runner := oldRunner(t, version)
	started := filepath.Join(t.TempDir(), "started")
	m["target"], m["kind"], m["size"], m["cpu"], m["memory"] = "t", "box", "small", "1", "512"
	m["images"] = "fake/linux@sha256:" + strings.Repeat("ab", 32)
	m["runner_dir"] = runner
	return m, runner, started
}

// The pinned images the box bindings of these tests have; each acquire runs
// one (the first by default).
var (
	imgA = "fake/a@sha256:" + strings.Repeat("ab", 32)
	imgB = "fake/b@sha256:" + strings.Repeat("cd", 32)
)

// describedRunners is describe's "runners" for a binding, by image.
func describedRunners(t *testing.T, m map[string]string) map[string]map[string]any {
	t.Helper()
	d, err := call(t, m, "describe", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, old := d["runner"]; old {
		t.Errorf("describe still has the single runner: %v", d["runner"])
	}
	out := map[string]map[string]any{}
	list, _ := d["runners"].([]any)
	for _, e := range list {
		r, _ := e.(map[string]any)
		image, _ := r["image"].(string)
		if _, dup := out[image]; dup {
			t.Errorf("describe.runners has %s twice: %v", image, list)
		}
		out[image] = r
	}
	return out
}

// wantRunner checks one image's entry: version is what the box ended with,
// image the image's own version, why the closed reason of a failure.
func wantRunner(t *testing.T, got map[string]map[string]any, image, version, imageVersion string, refresh protocol.RunnerRefresh, why protocol.RunnerWhy) {
	t.Helper()
	r := got[image]
	if r == nil {
		t.Fatalf("describe.runners has no %s, want %s: %v", image, refresh, got)
	}
	gotImage, _ := r["image_version"].(string)
	gotWhy, _ := r["why"].(string)
	if r["version"] != version || r["refresh"] != string(refresh) || gotImage != imageVersion || gotWhy != string(why) {
		t.Errorf("describe.runners[%s] = %v, want version %q image_version %q refresh %q why %q", image, r, version, imageVersion, refresh, why)
	}
	if at, _ := r["at"].(string); at == "" {
		t.Errorf("describe.runners[%s] has no at: %v", image, r)
	}
}

// wantNoRunner checks describe lists no result for image.
func wantNoRunner(t *testing.T, got map[string]map[string]any, image string) {
	t.Helper()
	if r := got[image]; r != nil {
		t.Errorf("describe.runners lists %s: %v", image, r)
	}
}

// writeRunnerStatus puts raw text where the driver keeps its results.
func writeRunnerStatus(t *testing.T, m map[string]string, raw string) {
	t.Helper()
	if err := os.MkdirAll(m["state_dir"], 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store{m["state_dir"]}.runnerStatusFile(), []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
}

// Describe says nothing about a runner before any box has started one.
func TestDescribeRunnerBeforeAnyBox(t *testing.T) {
	m, _, _ := jitRefreshBox(t, "2.332.0")
	if r := describedRunners(t, m); len(r) != 0 {
		t.Errorf("describe.runners = %v before any box", r)
	}
}

// A binding's boxes run different images and each image has its own result: a
// current image's box does not hide another image's failing refresh, and an
// image the binding no longer has is not shown.
func TestDescribeRunnersPerImage(t *testing.T) {
	m, runner, _ := jitRefreshBox(t, "2.332.0")
	m["images"] = imgA + "," + imgB
	started := filepath.Join(t.TempDir(), "started")
	t.Setenv("ZMRT_RUNNER_TARBALL", fakeRelease(t, runnerPinVersion, started))
	tampered := filepath.Join(t.TempDir(), "tampered")
	_ = os.WriteFile(tampered, []byte("a different tarball"), 0o600)
	t.Setenv("ZMRT_RUNNER_SERVE", tampered)
	curlShim(t, tampered)

	// Image A's runner is old and cannot be refreshed.
	_, err := call(t, m, "acquire", "", map[string]any{"request_id": "a", "ttl_s": 60, "jit_config": testJIT, "image": "fake/a"})
	if code(err) != "unavailable" {
		t.Fatalf("acquire of the image that cannot refresh: %v", err)
	}
	// Image B's is current. B finishes last.
	_ = os.WriteFile(filepath.Join(runner, "bin", "Runner.Listener"), []byte(imageRunnerListener(runnerMinVersion)), 0o755)
	_ = os.WriteFile(filepath.Join(runner, "run.sh"), []byte(fmt.Sprintf("#!/bin/sh\necho \"$*\" >%q\nexec sleep 60\n", started)), 0o755)
	b, err := call(t, m, "acquire", "", map[string]any{"request_id": "b", "ttl_s": 60, "jit_config": testJIT, "image": "fake/b"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = call(t, m, "release", b["handle"].(string), nil) }()
	waitStartedWith(t, started, "--jitconfig "+testJIT)

	got := describedRunners(t, m)
	wantRunner(t, got, imgA, "", "2.332.0", protocol.RunnerFailed, protocol.WhyChecksumMismatch)
	wantRunner(t, got, imgB, runnerMinVersion, "", protocol.RunnerNotNeeded, "")
	if len(got) != 2 {
		t.Errorf("describe.runners = %v, want the two images", got)
	}
	// The list follows the binding's images, in its order.
	d, _ := call(t, m, "describe", "", nil)
	if l, _ := d["runners"].([]any); len(l) != 2 || l[0].(map[string]any)["image"] != imgA || l[1].(map[string]any)["image"] != imgB {
		t.Errorf("describe.runners is not in the binding's image order: %v", d["runners"])
	}

	// The binding drops image A: only B is shown.
	m2 := map[string]string{}
	for k, v := range m {
		m2[k] = v
	}
	m2["images"] = imgB
	got = describedRunners(t, m2)
	wantNoRunner(t, got, imgA)
	wantRunner(t, got, imgB, runnerMinVersion, "", protocol.RunnerNotNeeded, "")
}

// A box whose image has no runner reports nothing for that image: the result
// of an earlier box of it does not stay.
func TestDescribeRunnerClearedByAnImageWithNone(t *testing.T) {
	m, runner, _ := jitRefreshBox(t, runnerMinVersion)
	t.Setenv("ZMRT_RUNNER_TARBALL", fakeRelease(t, runnerPinVersion, filepath.Join(t.TempDir(), "x")))
	a, err := call(t, m, "acquire", "", map[string]any{"request_id": "k", "ttl_s": 60, "jit_config": testJIT})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = call(t, m, "release", a["handle"].(string), nil)
	got := describedRunners(t, m)
	wantRunner(t, got, m["images"], runnerMinVersion, "", protocol.RunnerNotNeeded, "")

	if err := os.Remove(filepath.Join(runner, "run.sh")); err != nil {
		t.Fatal(err)
	}
	a, err = call(t, m, "acquire", "", map[string]any{"request_id": "k2", "ttl_s": 60, "jit_config": testJIT})
	if err == nil {
		_, _ = call(t, m, "release", a["handle"].(string), nil)
	}
	wantNoRunner(t, describedRunners(t, m), m["images"])
}

// A runner whose version the box cannot read fails with that reason, and
// nothing the guest printed reaches describe.
func TestDescribeRunnerVersionUnreadable(t *testing.T) {
	m, runner, started := jitRefreshBox(t, "2.332.0")
	t.Setenv("ZMRT_RUNNER_TARBALL", fakeRelease(t, runnerPinVersion, started))
	_ = os.WriteFile(filepath.Join(runner, "bin", "Runner.Listener"), []byte("#!/bin/sh\necho 'secret words the guest printed'\n"), 0o755)
	_, err := call(t, m, "acquire", "", map[string]any{"request_id": "k", "ttl_s": 60, "jit_config": testJIT})
	if code(err) != "unavailable" {
		t.Fatalf("acquire with an unreadable runner version: %v", err)
	}
	got := describedRunners(t, m)
	wantRunner(t, got, m["images"], "", "", protocol.RunnerFailed, protocol.WhyVersionUnreadable)
	if b, _ := json.Marshal(got); strings.Contains(string(b), "secret words") {
		t.Errorf("the guest's output reached describe: %s", b)
	}
}

// A status file that is corrupt, half written or holds results no reader may
// use reads as absent, and the next box writes a good one.
func TestDescribeRunnerStatusFileRead(t *testing.T) {
	m, _, started := jitRefreshBox(t, runnerMinVersion)
	image := m["images"]
	at := time.Now().UTC().Format(time.RFC3339)
	for name, raw := range map[string]string{
		"garbage":       "not json at all",
		"half written":  `{"` + image + `":{"image":"` + image + `","version":"` + runnerMinVersion + `","refresh":"not_n`,
		"empty":         "",
		"not an object": `[1,2]`,
		"old format":    `{"version":"2.338.2","refresh":"not_needed","at":"` + at + `"}`,
		"unknown state": `{"` + image + `":{"image":"` + image + `","version":"` + runnerMinVersion + `","refresh":"great","at":"` + at + `"}}`,
		"no time":       `{"` + image + `":{"image":"` + image + `","version":"` + runnerMinVersion + `","refresh":"not_needed"}}`,
		"zero time":     `{"` + image + `":{"image":"` + image + `","version":"` + runnerMinVersion + `","refresh":"not_needed","at":"0001-01-01T00:00:00Z"}}`,
		"free-text why": `{"` + image + `":{"image":"` + image + `","refresh":"failed","why":"open /Users/x/y: no","at":"` + at + `"}}`,
		"wrong image":   `{"` + image + `":{"image":"other","version":"` + runnerMinVersion + `","refresh":"not_needed","at":"` + at + `"}}`,
	} {
		writeRunnerStatus(t, m, raw)
		if got := describedRunners(t, m); len(got) != 0 {
			t.Errorf("%s: describe.runners = %v, want none", name, got)
		}
	}
	// And a good result is written over the bad file.
	_ = os.WriteFile(filepath.Join(m["runner_dir"], "run.sh"), []byte(fmt.Sprintf("#!/bin/sh\necho \"$*\" >%q\nexec sleep 60\n", started)), 0o755)
	t.Setenv("ZMRT_RUNNER_TARBALL", fakeRelease(t, runnerPinVersion, filepath.Join(t.TempDir(), "x")))
	a, err := call(t, m, "acquire", "", map[string]any{"request_id": "k", "ttl_s": 60, "jit_config": testJIT})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = call(t, m, "release", a["handle"].(string), nil) }()
	waitStartedWith(t, started, "--jitconfig "+testJIT)
	wantRunner(t, describedRunners(t, m), image, runnerMinVersion, "", protocol.RunnerNotNeeded, "")
	if fi, err := os.Stat(store{m["state_dir"]}.runnerStatusFile()); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("the status file: %v %v, want mode 0600", fi, err)
	}
}

// A result the current minimum no longer vouches for (a driver upgrade raised
// it) is not shown; a failure stays whatever the minimum.
func TestDescribeRunnerBelowTheMinimum(t *testing.T) {
	m, _, _ := jitRefreshBox(t, runnerMinVersion)
	image := m["images"]
	st := store{m["state_dir"]}
	if err := os.MkdirAll(st.dir, 0o700); err != nil {
		t.Fatal(err)
	}
	st.recordRunner(protocol.RunnerStatus{Image: image, Version: "2.0.1", Refresh: protocol.RunnerNotNeeded, At: time.Now().UTC()})
	wantNoRunner(t, describedRunners(t, m), image)
	st.recordRunner(protocol.RunnerStatus{Image: image, ImageVersion: "2.0.1", Refresh: protocol.RunnerFailed, Why: protocol.WhyDownloadFailed, At: time.Now().UTC()})
	wantRunner(t, describedRunners(t, m), image, "", "2.0.1", protocol.RunnerFailed, protocol.WhyDownloadFailed)
}

// Only a binding that starts a GitHub runner describes runner results.
func TestDescribeRunnerNeedsAGitHubRunner(t *testing.T) {
	m, _, _ := jitRefreshBox(t, runnerMinVersion)
	image := m["images"]
	st := store{m["state_dir"]}
	if err := os.MkdirAll(st.dir, 0o700); err != nil {
		t.Fatal(err)
	}
	st.recordRunner(protocol.RunnerStatus{Image: image, Version: runnerMinVersion, Refresh: protocol.RunnerNotNeeded, At: time.Now().UTC()})
	wantRunner(t, describedRunners(t, m), image, runnerMinVersion, "", protocol.RunnerNotNeeded, "")
	delete(m, "runner_dir")
	if got := describedRunners(t, m); len(got) != 0 {
		t.Errorf("a binding with no GitHub runner describes runners: %v", got)
	}
}

const testJIT = "eyJzZWtyaXQiOiJKSVQtY29uZmlnLXZhbHVlIn0="

func waitStartedWith(t *testing.T, started, want string) {
	t.Helper()
	for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		if b, _ := os.ReadFile(started); strings.TrimSpace(string(b)) == want {
			return
		}
		if time.Now().After(deadline) {
			b, _ := os.ReadFile(started)
			t.Fatalf("the refreshed runner started with %q, want %q", b, want)
		}
	}
}

// A JIT box whose image ships a runner GitHub has deprecated gets the pinned
// release before the runner starts: from the host's verified copy, or, when
// the host has none, downloaded in the guest. The image's .env stays.
func TestBoxRefreshesAnOldRunner(t *testing.T) {
	m, runner, started := jitRefreshBox(t, "2.332.0")
	rel := fakeRelease(t, runnerPinVersion, started)
	t.Setenv("ZMRT_RUNNER_TARBALL", rel)
	curlCalls := curlShim(t, "") // the guest cannot download: the host's copy must serve
	a, err := call(t, m, "acquire", "", map[string]any{"request_id": "k", "ttl_s": 60, "jit_config": testJIT})
	if err != nil {
		t.Fatal(err)
	}
	waitStartedWith(t, started, "--jitconfig "+testJIT)
	_, _ = call(t, m, "release", a["handle"].(string), nil)
	if _, err := os.Stat(curlCalls); err == nil {
		t.Error("the guest downloaded although the host had the release")
	}
	wantRunner(t, describedRunners(t, m), m["images"], runnerPinVersion, "2.332.0", protocol.RunnerRefreshed, "")
	if b, _ := os.ReadFile(filepath.Join(runner, ".env")); string(b) != "ImageOS=macos26\n" {
		t.Errorf("the image's .env is %q", b)
	}
	if _, err := os.Stat(filepath.Join(m["state_dir"], "cache", "runner", runnerTarball("linux-x64"))); err != nil {
		if _, err2 := os.Stat(filepath.Join(m["state_dir"], "cache", "runner", runnerTarball("osx-arm64"))); err2 != nil {
			t.Errorf("no host copy cached: %v", err)
		}
	}
	logs, _ := filepath.Glob(filepath.Join(m["state_dir"], "logs", "*"))
	var log strings.Builder
	for _, l := range logs {
		b, _ := os.ReadFile(l)
		log.Write(b)
	}
	if !strings.Contains(log.String(), "older than v"+runnerMinVersion) || !strings.Contains(log.String(), "is now v"+runnerPinVersion) {
		t.Errorf("the run log does not say the runner was refreshed:\n%s", log.String())
	}
	if strings.Contains(log.String(), testJIT) {
		t.Error("the jit_config is in a log")
	}

	// The host is offline: the guest downloads it, with the same check.
	m, _, started = jitRefreshBox(t, "2.332.0")
	rel = fakeRelease(t, runnerPinVersion, started)
	t.Setenv("ZMRT_RUNNER_TARBALL", rel)
	t.Setenv("ZMRT_RUNNER_HOSTFAIL", "1")
	curlCalls = curlShim(t, rel)
	a, err = call(t, m, "acquire", "", map[string]any{"request_id": "k", "ttl_s": 60, "jit_config": testJIT})
	if err != nil {
		t.Fatal(err)
	}
	waitStartedWith(t, started, "--jitconfig "+testJIT)
	_, _ = call(t, m, "release", a["handle"].(string), nil)
	if b, _ := os.ReadFile(curlCalls); !strings.Contains(string(b), runnerURL("linux-x64")) && !strings.Contains(string(b), runnerURL("osx-arm64")) {
		t.Errorf("the guest's curl was called with %q", b)
	}
}

// A refresh that cannot install the pinned release fails the acquire, saying
// why, and the old runner never starts (GitHub would register and then refuse
// it, leaving the job queued).
func TestBoxOldRunnerRefreshFailsPlainly(t *testing.T) {
	m, runner, started := jitRefreshBox(t, "2.332.0")
	t.Setenv("ZMRT_RUNNER_TARBALL", fakeRelease(t, runnerPinVersion, started))
	tampered := filepath.Join(t.TempDir(), "tampered")
	_ = os.WriteFile(tampered, []byte("a different tarball"), 0o600)
	t.Setenv("ZMRT_RUNNER_SERVE", tampered)
	curlShim(t, tampered)
	_, err := call(t, m, "acquire", "", map[string]any{"request_id": "k", "ttl_s": 60, "jit_config": testJIT})
	if code(err) != "unavailable" || !strings.Contains(err.Error(), "older than v"+runnerMinVersion) || !strings.Contains(err.Error(), "refreshing it failed") ||
		!strings.Contains(err.Error(), "not the pinned") {
		t.Fatalf("acquire with only tampered releases: %v, want unavailable saying the refresh failed on the sha", err)
	}
	if _, err := os.Stat(started); err == nil {
		t.Error("a runner started")
	}
	got := describedRunners(t, m)
	wantRunner(t, got, m["images"], "", "2.332.0", protocol.RunnerFailed, protocol.WhyChecksumMismatch)
	if b, _ := json.Marshal(got); strings.Contains(string(b), m["state_dir"]) || strings.Contains(string(b), runner) || strings.Contains(string(b), "not the pinned") {
		t.Errorf("describe.runners has a path or the guest's output: %s", b)
	}
	if out, _ := exec.Command(filepath.Join(runner, "bin", "Runner.Listener")).Output(); strings.TrimSpace(string(out)) != "2.332.0" {
		t.Errorf("a tampered release reached the runner: it says %q", out)
	}
	if es, _ := os.ReadDir(filepath.Join(m["state_dir"], "cache", "runner")); len(es) > 1 {
		t.Errorf("the host cached a tampered release: %v", es)
	}
}

// A runner at the minimum is left alone: nothing is downloaded.
func TestBoxCurrentRunnerUntouched(t *testing.T) {
	m, runner, _ := jitRefreshBox(t, runnerMinVersion)
	started := filepath.Join(t.TempDir(), "started")
	_ = os.WriteFile(filepath.Join(runner, "run.sh"), []byte(fmt.Sprintf("#!/bin/sh\necho \"$*\" >%q\nexec sleep 60\n", started)), 0o755)
	t.Setenv("ZMRT_RUNNER_TARBALL", fakeRelease(t, runnerPinVersion, filepath.Join(t.TempDir(), "not-this")))
	t.Setenv("ZMRT_RUNNER_HOSTFAIL", "1")
	calls := curlShim(t, "")
	a, err := call(t, m, "acquire", "", map[string]any{"request_id": "k", "ttl_s": 60, "jit_config": testJIT})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = call(t, m, "release", a["handle"].(string), nil) }()
	waitStartedWith(t, started, "--jitconfig "+testJIT)
	wantRunner(t, describedRunners(t, m), m["images"], runnerMinVersion, "", protocol.RunnerNotNeeded, "")
	if _, err := os.Stat(calls); err == nil {
		t.Error("a current runner was downloaded again")
	}
	if _, err := os.Stat(filepath.Join(runner, "bin", "old-only.dll")); err != nil {
		t.Error("a current runner was replaced")
	}
}

// GitHub's stock runner image enables stdout logging. HostContext logs before
// --version prints its answer, so the probe must quiet only that invocation;
// the actual job runner must keep the image's logging setting.
func TestBoxRunnerProbeWithImageLogging(t *testing.T) {
	m, runner, _ := jitRefreshBox(t, runnerMinVersion)
	started := filepath.Join(t.TempDir(), "started-with-env")
	listener := imageRunnerListener(runnerMinVersion)
	if err := os.WriteFile(filepath.Join(runner, "bin", "Runner.Listener"), []byte(listener), 0755); err != nil {
		t.Fatal(err)
	}
	launch := fmt.Sprintf("#!/bin/sh\nprintf '%%s|%%s\\n' \"$ACTIONS_RUNNER_PRINT_LOG_TO_STDOUT\" \"$*\" >%q\nexec sleep 60\n", started)
	if err := os.WriteFile(filepath.Join(runner, "run.sh"), []byte(launch), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ACTIONS_RUNNER_PRINT_LOG_TO_STDOUT", "1")
	t.Setenv("ZMRT_RUNNER_TARBALL", fakeRelease(t, runnerPinVersion, filepath.Join(t.TempDir(), "must-not-refresh")))
	t.Setenv("ZMRT_RUNNER_HOSTFAIL", "1")
	curls := curlShim(t, "")
	a, err := call(t, m, "acquire", "", map[string]any{"request_id": "image-logging", "ttl_s": 60, "jit_config": testJIT})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = call(t, m, "release", a["handle"].(string), nil) }()
	waitStartedWith(t, started, "1|--jitconfig "+testJIT)
	if _, err := os.Stat(curls); err == nil {
		t.Error("a current runner was downloaded")
	}
	if _, err := os.Stat(filepath.Join(runner, "bin", "old-only.dll")); err != nil {
		t.Error("a current runner was replaced")
	}
}

// The same probe also verifies the installed replacement. Start with a quiet
// old listener so this regression isolates the noisy post-install check.
func TestBoxRefreshedRunnerProbeWithImageLogging(t *testing.T) {
	m, _, started := jitRefreshBox(t, "2.332.0")
	t.Setenv("ACTIONS_RUNNER_PRINT_LOG_TO_STDOUT", "1")
	t.Setenv("ZMRT_RUNNER_TARBALL", fakeRelease(t, runnerPinVersion, started))
	curls := curlShim(t, "")
	a, err := call(t, m, "acquire", "", map[string]any{"request_id": "refreshed-image-logging", "ttl_s": 60, "jit_config": testJIT})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = call(t, m, "release", a["handle"].(string), nil) }()
	waitStartedWith(t, started, "--jitconfig "+testJIT)
	if b, err := os.ReadFile(started + ".stdout-log-setting"); err != nil || string(b) != "1" {
		t.Fatalf("replacement runtime logging flag:%q %v", b, err)
	}
	if _, err := os.Stat(curls); err == nil {
		t.Error("the guest downloaded although the host's verified copy served")
	}
}
