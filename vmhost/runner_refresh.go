package vmhost

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/sengirunners/potluck/protocol"
)

// A GitHub just-in-time runner never updates itself, and GitHub refuses a
// runner it has deprecated ("Runner version vX is deprecated and cannot
// receive messages") after registering it: the job then waits queued on a box
// that will never take it. So before a box's runner starts, the supervisor
// reads its version and, below runnerMinVersion, installs the pinned release
// over it (zm-eakh.12.54.15). Images keep whatever runner they were built
// with (Cirrus's macos-tahoe-xcode 26.3 ships v2.332.0).
//
// The release is pinned by version and by each tarball's SHA-256, copied from
// the release notes (github.com/actions/runner/releases/tag/v2.338.2, "SHA-256
// Checksums") and checked against the downloaded tarballs. To bump: change
// runnerPinVersion and every sha below, and runnerMinVersion if GitHub has
// deprecated the old minimum. Nothing is installed whose hash is not one of
// these.
const (
	runnerMinVersion = "2.338.2" // below this a runner is refreshed
	runnerPinVersion = "2.338.2" // what a refresh installs
)

// runnerSHA256 is the pinned release's tarball hash per platform (GitHub's
// names). Only platforms a box can be: macOS on Apple silicon (Tart), Linux
// on arm64 (Tart, Incus, OCI) and x64 (Incus, OCI).
var runnerSHA256 = map[string]string{
	"osx-arm64":   "25a065af409ffa317ab4d5a3b614dd15fc9c40f2172bc1b35a1b1258e919a388",
	"linux-arm64": "670adb2f268fe21dcbb58b3db0e0aa9d5b3bddfe2c651481336e561ff86de945",
	"linux-x64":   "baebf204307bac6193480852732f0aba2ae97b6c5597c244f4d5558390147c40",
}

// runnerMaxBytes bounds a download (the largest pinned tarball is 228 MB).
const runnerMaxBytes = 512 << 20

// runnerTarball is the pinned release's file name for a platform.
func runnerTarball(plat string) string {
	return "actions-runner-" + plat + "-" + runnerPinVersion + ".tar.gz"
}

// runnerURL is where GitHub publishes it. Always https, always the pinned tag.
func runnerURL(plat string) string {
	return "https://github.com/actions/runner/releases/download/v" + runnerPinVersion + "/" + runnerTarball(plat)
}

// parseRunnerVersion reads "2.332.0" (a leading v allowed) as its three numbers.
func parseRunnerVersion(v string) ([3]int, error) {
	var n [3]int
	f := strings.Split(strings.TrimPrefix(strings.TrimSpace(v), "v"), ".")
	if len(f) != 3 {
		return n, fmt.Errorf("runner version %q is not MAJOR.MINOR.PATCH", v)
	}
	for i, s := range f {
		x, err := strconv.Atoi(s)
		if err != nil || x < 0 || s == "" || len(s) > 6 {
			return n, fmt.Errorf("runner version %q is not MAJOR.MINOR.PATCH", v)
		}
		n[i] = x
	}
	return n, nil
}

// runnerTooOld says whether a runner of version have needs the refresh: older
// than runnerMinVersion, or a version that does not read as one.
func runnerTooOld(have string) bool {
	h, err := parseRunnerVersion(have)
	if err != nil {
		return true
	}
	m, _ := parseRunnerVersion(runnerMinVersion)
	for i := range h {
		if h[i] != m[i] {
			return h[i] < m[i]
		}
	}
	return false
}

// runnerPlatform maps the guest's uname -s and uname -m to GitHub's platform
// name, for the pinned platforms only.
func runnerPlatform(sys, machine string) (string, error) {
	var family string
	switch strings.ToLower(sys) {
	case "darwin":
		family = "osx"
	case "linux":
		family = "linux"
	default:
		return "", fmt.Errorf("no pinned GitHub runner for a %s guest", sys)
	}
	var arch string
	switch strings.ToLower(machine) {
	case "arm64", "aarch64":
		arch = "arm64"
	case "x86_64", "amd64":
		arch = "x64"
	default:
		return "", fmt.Errorf("no pinned GitHub runner for a %s %s guest", sys, machine)
	}
	p := family + "-" + arch
	if _, ok := runnerSHA256[p]; !ok {
		return "", fmt.Errorf("no pinned GitHub runner for a %s %s guest", sys, machine)
	}
	return p, nil
}

// guessRunnerPlatform is the platform a box of this OS probably is (its
// architecture the host's: Tart runs Apple silicon guests; Incus and OCI the
// host's own), so the host cache can be filled while the VM boots. The guest's
// own answer decides.
func guessRunnerPlatform(osName string) string {
	sys := "linux"
	if strings.EqualFold(osName, "macos") || strings.EqualFold(osName, "darwin") {
		sys = "darwin"
	}
	p, err := runnerPlatform(sys, runtime.GOARCH)
	if err != nil {
		return ""
	}
	return p
}

// verifyRunnerSHA checks r's SHA-256 against the pinned one for plat.
func verifyRunnerSHA(plat string, r io.Reader) error {
	want, ok := runnerSHA256[plat]
	if !ok {
		return fmt.Errorf("no pinned GitHub runner for %s", plat)
	}
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(r, runnerMaxBytes+1))
	if err != nil {
		return err
	}
	if n > runnerMaxBytes {
		return fmt.Errorf("%s is over %d bytes", runnerTarball(plat), runnerMaxBytes)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		return fmt.Errorf("%s has sha256 %s, not the pinned %s", runnerTarball(plat), got, want)
	}
	return nil
}

// runnerHTTP fetches the pinned tarball; a variable for tests.
var runnerHTTP = func(ctx context.Context, url string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	c := &http.Client{CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if req.URL.Scheme != "https" {
			return fmt.Errorf("redirect to %s is not https", req.URL.Scheme)
		}
		if len(via) > 5 {
			return errors.New("too many redirects")
		}
		return nil
	}}
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return resp.Body, nil
}

// hostRunner is the host's verified copy of the pinned tarball for plat,
// under dir (the driver's state dir): downloaded once, its sha256 checked
// before it gets its name, so a file at that name was verified. One fetch at
// a time per state dir; a second caller finds the first's file.
func hostRunner(ctx context.Context, dir, plat string) (string, error) {
	if _, ok := runnerSHA256[plat]; !ok {
		return "", fmt.Errorf("no pinned GitHub runner for %s", plat)
	}
	cache := filepath.Join(dir, "cache", "runner")
	if err := os.MkdirAll(cache, 0o700); err != nil {
		return "", failWhy(protocol.WhyHostCache, err)
	}
	final := filepath.Join(cache, runnerTarball(plat))
	err := flocked(filepath.Join(cache, "lock"), func() error {
		if fi, err := os.Stat(final); err == nil && fi.Mode().IsRegular() {
			return nil
		}
		body, err := runnerHTTP(ctx, runnerURL(plat))
		if err != nil {
			return failWhy(protocol.WhyDownloadFailed, err)
		}
		defer body.Close()
		tmp, err := os.CreateTemp(cache, "fetch-*")
		if err != nil {
			return failWhy(protocol.WhyHostCache, err)
		}
		defer os.Remove(tmp.Name())
		h := sha256.New()
		n, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(body, runnerMaxBytes+1))
		if cerr := tmp.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return failWhy(protocol.WhyDownloadFailed, fmt.Errorf("downloading %s: %v", runnerTarball(plat), err))
		}
		if n > runnerMaxBytes {
			return failWhy(protocol.WhyDownloadFailed, fmt.Errorf("%s is over %d bytes", runnerTarball(plat), runnerMaxBytes))
		}
		if got := hex.EncodeToString(h.Sum(nil)); got != runnerSHA256[plat] {
			return failWhy(protocol.WhyChecksumMismatch, fmt.Errorf("downloaded %s has sha256 %s, not the pinned %s", runnerTarball(plat), got, runnerSHA256[plat]))
		}
		return failWhy(protocol.WhyHostCache, os.Rename(tmp.Name(), final))
	})
	if err != nil {
		return "", failWhy(protocol.WhyHostCache, err) // the lock itself
	}
	return final, nil
}

// runnerProbe prints the guest's uname -s, uname -m and its runner's version
// ("-" when the runner gives none), or exits 127 when the directory has no
// runner (the launch then says so).
const runnerProbe = `case "$1" in /*) cd "$1" ;; *) cd "$HOME/$1" ;; esac 2>/dev/null || exit 127
[ -x ./run.sh ] || exit 127
# The stock image logs HostContext messages on stdout before the version.
# Quiet this probe only; the runner that serves the job keeps its setting.
v=$(ACTIONS_RUNNER_PRINT_LOG_TO_STDOUT=0 ./bin/Runner.Listener --version 2>/dev/null | head -n 1)
printf '%s %s %s\n' "$(uname -s)" "$(uname -m)" "${v:--}"`

// runnerInstall puts the pinned tarball in the runner's directory: from
// stdin ($4 "host": the host's verified copy) or downloaded in the guest
// ($4 "guest", from $3, an https URL), checked against the pinned sha256 ($2)
// before anything is extracted. Exit 21 is a download that failed and 22 a
// checksum that did not match (installRunner's reasons). Only the release's bin/ and externals/ are
// replaced; the image's .env, .path and _work stay. Nothing is executed from
// the download before its hash matches.
const runnerInstall = `set -e
case "$1" in /*) cd "$1" ;; *) cd "$HOME/$1" ;; esac
t=$(mktemp "${TMPDIR:-/tmp}/zm-runner.XXXXXX")
trap 'rm -f "$t"' EXIT
if [ "$4" = host ]; then
  cat >"$t"
else
  command -v curl >/dev/null 2>&1 || { echo "no curl in the guest to download the runner" >&2; exit 21; }
  curl -fsSL --proto '=https' --proto-redir '=https' --retry 2 --max-time 600 --max-filesize 536870912 -o "$t" "$3" || exit 21
fi
if command -v sha256sum >/dev/null 2>&1; then h=$(sha256sum "$t"); elif command -v shasum >/dev/null 2>&1; then h=$(shasum -a 256 "$t"); else echo "no sha256sum or shasum in the guest" >&2; exit 1; fi
h=${h%% *}
[ "$h" = "$2" ] || { echo "the runner tarball has sha256 $h, not the pinned $2" >&2; exit 22; }
rm -rf bin externals
tar -xzf "$t"`

// refreshRunner makes sure the runner in dir (in the guest) of a box running
// image is one GitHub still serves: read its version, and below
// runnerMinVersion install the pinned release, from the host's cache (fetched
// while the VM booted) or, failing that, downloaded in the guest. An error
// says plainly why the box has no usable runner. What it found and did is kept
// for describe (runner_status.go) under the image, and a box whose image has
// no runner clears that image's entry.
func (s *sup) refreshRunner(ctx context.Context, image, dir string, prefetch <-chan hostFetch) error {
	if skipRunnerRefresh(s.cfg) {
		return nil
	}
	st, err := s.checkRunner(ctx, image, dir, prefetch)
	if st != nil {
		s.st.recordRunner(*st)
	} else {
		s.st.clearRunner(image)
	}
	return err
}

// checkRunner does refreshRunner's work and says its result: nil when there is
// no runner in dir.
func (s *sup) checkRunner(ctx context.Context, image, dir string, prefetch <-chan hostFetch) (*protocol.RunnerStatus, error) {
	var out strings.Builder
	cmd := s.vm.Exec(s.name, nil, &out, io.Discard, "sh", "-c", runnerProbe, "sh", dir)
	stop := context.AfterFunc(ctx, func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
	})
	err := cmd.Run()
	stop()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && ee.ExitCode() == 127 {
			return nil, nil // no runner: the launch says so
		}
		if ctx.Err() != nil {
			return failedRunner(image, "", protocol.WhyTimedOut, fmt.Errorf("could not read the image's GitHub runner version: out of time (acquire waits boot_timeout_s + 15 s for the box): %v", err))
		}
		return failedRunner(image, "", protocol.WhyVersionUnreadable, fmt.Errorf("could not read the image's GitHub runner version: %v", err))
	}
	f := strings.Fields(out.String())
	if len(f) != 3 {
		return failedRunner(image, "", protocol.WhyVersionUnreadable, fmt.Errorf("could not read the image's GitHub runner version (the guest said %q)", strings.TrimSpace(out.String())))
	}
	have := f[2]
	if !runnerTooOld(have) {
		fmt.Fprintf(s.log, "potluck-driver-tart: the image's GitHub runner is v%s (minimum v%s)\n", strings.TrimPrefix(have, "v"), runnerMinVersion)
		return &protocol.RunnerStatus{Image: image, Version: runnerVersionOf(have), Refresh: protocol.RunnerNotNeeded, At: time.Now().UTC()}, nil
	}
	tooOld := fmt.Sprintf("the image's GitHub runner is %s, older than v%s (GitHub refuses deprecated runners and a JIT runner never updates itself)", runnerVersionWords(have), runnerMinVersion)
	plat, err := runnerPlatform(f[0], f[1])
	if err != nil {
		return failedRunner(image, have, protocol.WhyUnsupportedPlatform, fmt.Errorf("%s, and it could not be refreshed: %v", tooOld, err))
	}
	fmt.Fprintf(s.log, "potluck-driver-tart: %s: installing the pinned v%s (%s)\n", tooOld, runnerPinVersion, plat)
	start := time.Now()
	var hostErr error
	path := ""
	if prefetch != nil {
		select {
		case r, ok := <-prefetch:
			if ok && r.plat == plat {
				path, hostErr = r.path, r.err
			}
		case <-ctx.Done():
			return failedRunner(image, have, protocol.WhyTimedOut, fmt.Errorf("%s, and refreshing it ran out of time waiting for the host's copy (acquire waits boot_timeout_s + 15 s for the box)", tooOld))
		}
	}
	if path == "" && hostErr == nil {
		path, hostErr = hostRunner(ctx, s.st.dir, plat)
	}
	if hostErr == nil {
		hostErr = s.installRunner(ctx, dir, plat, path)
		if whyOf(hostErr, "") == protocol.WhyChecksumMismatch {
			_ = os.Remove(path) // a cached copy that no longer matches: fetched again next time
		}
	}
	if hostErr != nil && ctx.Err() != nil {
		return failedRunner(image, have, protocol.WhyTimedOut, fmt.Errorf("%s, and refreshing it ran out of time (acquire waits boot_timeout_s + 15 s for the box): from the host: %v", tooOld, hostErr))
	}
	if hostErr != nil {
		fmt.Fprintf(s.log, "potluck-driver-tart: the host's copy of the runner did not install (%v); downloading it in the guest\n", hostErr)
		if gerr := s.installRunner(ctx, dir, plat, ""); gerr != nil {
			late, why := "", whyOf(gerr, protocol.WhyInstallFailed)
			if ctx.Err() != nil {
				late, why = " (out of time: acquire waits boot_timeout_s + 15 s for the box)", protocol.WhyTimedOut
			}
			return failedRunner(image, have, why, fmt.Errorf("%s, and refreshing it failed%s: from the host: %v; in the guest: %v", tooOld, late, hostErr, gerr))
		}
	}
	// The install is checked by what the runner now says it is.
	out.Reset()
	cmd = s.vm.Exec(s.name, nil, &out, io.Discard, "sh", "-c", runnerProbe, "sh", dir)
	stop = context.AfterFunc(ctx, func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
	})
	err = cmd.Run()
	stop()
	if err != nil {
		if ctx.Err() != nil {
			return failedRunner(image, have, protocol.WhyTimedOut, fmt.Errorf("%s, and checking the runner after refreshing it ran out of time (acquire waits boot_timeout_s + 15 s for the box): %v", tooOld, err))
		}
		return failedRunner(image, have, protocol.WhyRunnerUnusable, fmt.Errorf("%s, and after refreshing it the runner does not start: %v", tooOld, err))
	}
	f = strings.Fields(out.String())
	if len(f) != 3 || runnerTooOld(f[2]) {
		return failedRunner(image, have, protocol.WhyRunnerUnusable, fmt.Errorf("%s, and after refreshing it the runner says %q", tooOld, strings.TrimSpace(out.String())))
	}
	fmt.Fprintf(s.log, "potluck-driver-tart: the GitHub runner is now v%s (%.1f s)\n", runnerPinVersion, time.Since(start).Seconds())
	return &protocol.RunnerStatus{Image: image, Version: runnerVersionOf(f[2]), ImageVersion: runnerVersionOf(have), Refresh: protocol.RunnerRefreshed, At: time.Now().UTC()}, nil
}

func runnerVersionWords(v string) string {
	if v == "-" || v == "" {
		return "of no version it will say"
	}
	return "v" + strings.TrimPrefix(v, "v")
}

// installRunner runs runnerInstall: with path, the host's copy on stdin; else
// the guest downloads it. A failure says its reason (runnerInstall's exit
// codes): the host's copy missing is the host cache's.
func (s *sup) installRunner(ctx context.Context, dir, plat, path string) error {
	var stdin io.Reader
	mode := "guest"
	if path != "" {
		f, err := os.Open(path)
		if err != nil {
			return failWhy(protocol.WhyHostCache, err)
		}
		defer f.Close()
		stdin, mode = f, "host"
	}
	err := s.guest(ctx, stdin, "installing the GitHub runner", "sh", "-c", runnerInstall, "sh", dir, runnerSHA256[plat], runnerURL(plat), mode)
	var ee *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &ee) && ee.ExitCode() == 21:
		err = failWhy(protocol.WhyDownloadFailed, err)
	case errors.As(err, &ee) && ee.ExitCode() == 22:
		err = failWhy(protocol.WhyChecksumMismatch, err)
	default:
		err = failWhy(protocol.WhyInstallFailed, err)
	}
	return err
}

// skipRunnerRefresh says a binding's boxes keep their image's runner as it
// is; only tests' fake layer, whose fake runners have no version, sets it.
var skipRunnerRefresh = func(config) bool { return false }

// hostFetch is a prefetch's result.
type hostFetch struct {
	plat, path string
	err        error
}

// prefetchRunner fills the host cache for the platform a box of osName
// probably is, while its VM boots. Nil when there is no guess.
func prefetchRunner(dir, osName string) <-chan hostFetch {
	plat := guessRunnerPlatform(osName)
	if plat == "" {
		return nil
	}
	ch := make(chan hostFetch, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		p, err := hostRunner(ctx, dir, plat)
		ch <- hostFetch{plat, p, err}
		close(ch)
	}()
	return ch
}
