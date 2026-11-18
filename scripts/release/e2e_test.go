package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/release"
)

// The whole path a stranger takes, on this machine's platform, with
// throwaway keys:
//   - install.sh refuses forged releases whose potluck is replaced by a
//     program that writes a canary file (no signature, another key's
//     signature, a tarball the signed manifest does not name), both on a
//     first install and with a potluck installed (the script checks the
//     signature itself either way; the tarball's hash is the installed
//     potluck's to check); none ever runs;
//   - with this machine's own tools (ssh-keygen, and an openssl that cannot
//     check Ed25519, as on a stock Mac) the SSHSIG signature is checked;
//   - install.sh installs release A, a daemon runs on it, and potluck
//     upgrade --release refuses forgeries of B, then installs B and
//     restarts the daemon on it, re-pinning the driver it placed;
//   - a new daemon that answers with the wrong commit, or never answers
//     (and ignores TERM), is stopped, the old programs go back, and the old
//     daemon starts again from its own program (even outside the install
//     directory);
//   - install.sh's GitHub path works against a fake GitHub over TLS: a
//     private repository through the API with a token, and a public one with
//     no token while every anonymous API call is refused (a spent per-address
//     rate limit), through the release links alone.
func TestInstallAndUpgradeEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("builds two releases")
	}
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	if _, err := exec.LookPath("ssh-keygen"); err != nil {
		t.Skip("no ssh-keygen: install.sh would refuse every release (install_test.go covers OpenSSL alone)")
	}
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)
	target := runtime.GOOS + "/" + runtime.GOARCH

	seed, pub, _ := release.NewKey()
	key, _ := release.ParseSeed(seed)
	otherSeed, _, _ := release.NewKey()
	otherKey, _ := release.ParseSeed(otherSeed)

	const verA, verB = "main-aaaaaaaaaaaa", "main-bbbbbbbbbbbb"
	commitA, commitB := strings.Repeat("a", 40), strings.Repeat("b", 40)
	tmp := t.TempDir()
	relA, relB := filepath.Join(tmp, "a"), filepath.Join(tmp, "b")
	for _, r := range []struct {
		dir, version, commit string
		created              time.Time
	}{{relA, verA, commitA, time.Now().Add(-time.Hour)}, {relB, verB, commitB, time.Now()}} {
		if err := os.MkdirAll(r.dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := run(r.version, r.commit, r.dir, []string{target}, ""); err != nil {
			t.Fatalf("build %s: %v", r.version, err)
		}
		if _, err := release.Potluck.SignDir(r.dir, r.version, r.commit, r.created, key); err != nil {
			t.Fatal(err)
		}
	}

	// Forgeries: a potluck that only writes the canary.
	canary := filepath.Join(tmp, "canary")
	evil := map[string]string{"potluck": "#!/bin/sh\necho ran >" + canary + "\n"}
	forged := func(name, version, commit string) (unsigned, wrongKey, notInManifest string) {
		unsigned = writeRelease(t, filepath.Join(tmp, name+"-unsigned"), version, commit, time.Now(), evil, nil)
		wrongKey = writeRelease(t, filepath.Join(tmp, name+"-wrongkey"), version, commit, time.Now(), evil, otherKey)
		// The real signed manifest, with the tarball (and SHA256SUMS) replaced.
		src := map[string]string{"A": relA, "B": relB}[name]
		notInManifest = copyDir(t, src, filepath.Join(tmp, name+"-notinmanifest"))
		e := writeRelease(t, filepath.Join(tmp, name+"-evil"), version, commit, time.Now(), evil, nil)
		for _, f := range []string{release.Potluck.Tarball(version, runtime.GOOS, runtime.GOARCH), "SHA256SUMS"} {
			b, _ := os.ReadFile(filepath.Join(e, f))
			_ = os.WriteFile(filepath.Join(notInManifest, f), b, 0o644)
		}
		return
	}
	noCanary := func(what string) {
		t.Helper()
		if _, err := os.Stat(canary); err == nil {
			t.Fatalf("%s: the forged potluck ran", what)
		}
	}

	home, err := os.MkdirTemp("", "plu") // short: the socket path has a limit
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	cfg, _ := json.Marshal(map[string]any{"update_keys": []string{pub}})
	if err := os.WriteFile(filepath.Join(home, "config.json"), cfg, 0o600); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(tmp, "bin")
	// PATH: the install dir, an openssl that can check Ed25519 (else none),
	// and the system's tools; no potluck from this machine.
	tools := filepath.Join(tmp, "tools")
	_ = os.MkdirAll(tools, 0o755)
	ossl := capableOpenSSL(t)
	if ossl != "" {
		_ = os.Symlink(ossl, filepath.Join(tools, "openssl"))
	}
	sysPath := "/usr/bin:/bin:/usr/sbin:/sbin"
	// Every daemon here is slow to exit: for 2 s after it stops answering its
	// process still holds the home's lock. An upgrade, a rollback and a
	// daemon stop must each wait for that before the next daemon starts.
	env := cleanEnv(t, "HOME="+filepath.Join(tmp, "home"), "POTLUCK_HOME="+home, "POTLUCK_INSTALL_DIR="+bin,
		"POTLUCK_UPDATE_KEY="+pub, "PATH="+bin+":"+tools+":"+sysPath, "POTLUCK_TEST_EXIT_DELAY=2s")
	install := func(env []string, args ...string) (string, error) {
		c := exec.Command("sh", append([]string{filepath.Join(root, "install.sh")}, args...)...)
		c.Env = env
		out, err := c.CombinedOutput()
		return string(out), err
	}
	potluck := func(args ...string) (string, error) {
		c := exec.Command(filepath.Join(bin, "potluck"), args...)
		c.Env = env
		out, err := c.CombinedOutput()
		return string(out), err
	}
	empty := func(dir string) {
		t.Helper()
		if ents, _ := os.ReadDir(dir); len(ents) != 0 {
			t.Fatalf("a refused install left %d files in %s", len(ents), dir)
		}
	}

	unsigned, wrongKey, notInManifest := forged("A", verA, commitA)
	home5, err := os.MkdirTemp("", "plu")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home5) })
	_ = os.WriteFile(filepath.Join(home5, "config.json"), cfg, 0o600)
	sideEnv := func(installDir, path string) []string {
		return cleanEnv(t, "HOME="+filepath.Join(tmp, "home"), "POTLUCK_HOME="+home5, "POTLUCK_INSTALL_DIR="+installDir,
			"POTLUCK_UPDATE_KEY="+pub, "PATH="+path)
	}

	// --from reads each file from the directory once: a directory that
	// serves the release on the first read of each file and a forgery on
	// any later one (FIFOs, as a swapping loop or a hostile file server
	// would) installs the release, and nothing forged runs; also when an
	// installed potluck does the install.
	{
		fifos := filepath.Join(tmp, "fifos")
		_ = os.MkdirAll(fifos, 0o755)
		opens := map[string]*atomic.Int32{}
		for _, f := range []string{release.ManifestFile, release.SignatureFile, release.SSHSIGFile, release.Potluck.Tarball(verA, runtime.GOOS, runtime.GOARCH)} {
			genuine, _ := os.ReadFile(filepath.Join(relA, f))
			forgery, _ := os.ReadFile(filepath.Join(unsigned, f))
			if f == release.SignatureFile || f == release.SSHSIGFile {
				forgery, _ = os.ReadFile(filepath.Join(wrongKey, f))
			}
			opens[f] = serveFIFO(t, filepath.Join(fifos, f), genuine, forgery)
		}
		bin5 := filepath.Join(tmp, "bin5")
		for _, what := range []string{"first install", "over an install"} {
			out, err := install(sideEnv(bin5, tools+":"+sysPath), "--from", fifos)
			if err != nil {
				for f, n := range opens {
					t.Logf("%s read %d times", f, n.Load())
				}
				t.Fatalf("--from a changing directory, %s (%v):\n%s", what, err, out)
			}
			noCanary("--from a changing directory, " + what)
			for f, n := range opens {
				if got := n.Swap(0); got != 1 {
					t.Errorf("%s: install.sh read %s from --from %d times, want once", what, f, got)
				}
			}
		}
	}

	// A potluck found on PATH in the release's directory, or through an
	// empty or "." PATH entry, is never run as the installed potluck.
	{
		dir := copyDir(t, relA, filepath.Join(tmp, "withprog"))
		if err := os.WriteFile(filepath.Join(dir, "potluck"), []byte(evil["potluck"]), 0o755); err != nil {
			t.Fatal(err)
		}
		for i, path := range []string{".:" + tools + ":" + sysPath, ":" + tools + ":" + sysPath, dir + ":" + tools + ":" + sysPath} {
			c := exec.Command("sh", filepath.Join(root, "install.sh"), "--from", ".")
			c.Dir = dir
			c.Env = sideEnv(filepath.Join(tmp, fmt.Sprintf("bin6-%d", i)), path)
			o, _ := c.CombinedOutput()
			noCanary(fmt.Sprintf("PATH %q:\n%s", path, o))
		}
	}

	// An openssl that says yes to everything is not trusted to check
	// signatures: a forged --from is refused.
	{
		yes := filepath.Join(tmp, "yes")
		_ = os.MkdirAll(yes, 0o755)
		_ = os.WriteFile(filepath.Join(yes, "openssl"), []byte("#!/bin/sh\nexit 0\n"), 0o755)
		bin7 := filepath.Join(tmp, "bin7")
		if out, err := install(sideEnv(bin7, yes+":"+sysPath), "--from", wrongKey); err == nil || !strings.Contains(out, "does not verify") {
			t.Fatalf("an openssl that always succeeds was trusted (%v):\n%s", err, out)
		}
		empty(bin7)
		noCanary("an openssl that always succeeds")
	}

	// The machine's own tools and an openssl that cannot check Ed25519 (a
	// stand-in that fails, as LibreSSL does on a stock Mac): ssh-keygen
	// checks the SSHSIG signature.
	{
		weak := filepath.Join(tmp, "weak")
		_ = os.MkdirAll(weak, 0o755)
		_ = os.WriteFile(filepath.Join(weak, "openssl"), []byte("#!/bin/sh\nexit 1\n"), 0o755)
		bin3 := filepath.Join(tmp, "bin3")
		env3 := cleanEnv(t, "HOME="+filepath.Join(tmp, "home"), "POTLUCK_HOME="+home, "POTLUCK_INSTALL_DIR="+bin3,
			"POTLUCK_UPDATE_KEY="+pub, "PATH="+weak+":"+sysPath)
		if out, err := install(env3, "--from", relA); err != nil || !strings.Contains(out, "SSHSIG, checked with ssh-keygen") {
			t.Fatalf("--from with ssh-keygen and no Ed25519 openssl (%v):\n%s", err, out)
		}
		// A malformed trusted key refuses before anything else.
		bin8 := filepath.Join(tmp, "bin8")
		env8 := cleanEnv(t, "HOME="+filepath.Join(tmp, "home"), "POTLUCK_HOME="+home, "POTLUCK_INSTALL_DIR="+bin8,
			"POTLUCK_UPDATE_KEY=ssh-ed25519", "PATH="+weak+":"+sysPath)
		if out, err := install(env8, "--from", relA); err == nil || !strings.Contains(out, "is not ed25519:<base64>") {
			t.Fatalf("a malformed POTLUCK_UPDATE_KEY was accepted (%v):\n%s", err, out)
		}
		empty(bin8)
	}

	// A first install refuses all three forgeries; nothing from them runs.
	for _, c := range []struct{ dir, want string }{
		{unsigned, "has no signature"},
		{wrongKey, "does not verify with any release key"},
		{notInManifest, "does not match the signed manifest"},
	} {
		if out, err := install(env, "--from", c.dir); err == nil || !strings.Contains(out, c.want) {
			t.Fatalf("install.sh --from %s (%v), want %q:\n%s", filepath.Base(c.dir), err, c.want, out)
		}
		empty(bin)
		noCanary("first install from " + filepath.Base(c.dir))
	}

	out, err := install(env, "--from", relA)
	if err != nil {
		t.Fatalf("install.sh: %v\n%s", err, out)
	}
	if !strings.Contains(out, "signature verified") {
		t.Fatalf("install.sh did not check the signature:\n%s", out)
	}
	if !strings.Contains(out, "potluck create <pool>") || !strings.Contains(out, "potluck join '<invite code>'") {
		t.Fatalf("install.sh did not print the next commands:\n%s", out)
	}
	for _, n := range []string{"potluck", "potluck-driver-local", "potluck-conformance", "potluck-e2b"} {
		if _, err := os.Stat(filepath.Join(bin, n)); err != nil {
			t.Errorf("%s not installed: %v", n, err)
		}
	}
	if out, _ := potluck("--version"); !strings.Contains(out, verA+" ("+commitA+")") {
		t.Fatalf("installed potluck --version: %s", out)
	}
	otherA := filepath.Join(tmp, "src", "potluck") // A, as if built from source elsewhere
	copyFile2(t, filepath.Join(bin, "potluck"), otherA)

	// With potluck installed, install.sh still checks the signature, then
	// has that potluck check the tarball: the forgeries are refused and
	// never run.
	unsignedB, wrongKeyB, notInManifestB := forged("B", verB, commitB)
	for _, c := range []struct{ dir, want string }{
		{unsignedB, "has no signature"},
		{wrongKeyB, "does not verify with any release key"},
		{notInManifestB, "already installed"},
		{notInManifestB, "does not match the signed manifest"},
	} {
		if out, err := install(env, "--from", c.dir); err == nil || !strings.Contains(out, c.want) {
			t.Fatalf("install.sh over an install, --from %s (%v), want %q:\n%s", filepath.Base(c.dir), err, c.want, out)
		}
		noCanary("install over an install from " + filepath.Base(c.dir))
	}
	// With no tool here that can check (no ssh-keygen, no Ed25519 openssl:
	// a Mac before its release carries an SSHSIG), the installed potluck's
	// own check is the one made: it refuses the forgeries, which never run.
	{
		noTools := toolDir(t, filepath.Join(tmp, "notools"), map[string]string{"openssl": "#!/bin/sh\nexit 1\n"})
		envNT := append(append([]string(nil), env...), "PATH="+bin+":"+noTools)
		for _, c := range []struct{ dir, want string }{
			{wrongKeyB, "does not verify with any configured update key"},
			{notInManifestB, "does not match the signed manifest"},
		} {
			out, err := install(envNT, "--from", c.dir)
			if err == nil || !strings.Contains(out, "this script cannot check the signature here") || !strings.Contains(out, "already installed") ||
				!strings.Contains(out, c.want) {
				t.Fatalf("install.sh over an install with no tool that can check, --from %s (%v), want %q:\n%s", filepath.Base(c.dir), err, c.want, out)
			}
			noCanary("install over an install with no tool that can check, from " + filepath.Base(c.dir))
		}
	}
	if out, _ := potluck("--version"); !strings.Contains(out, verA) {
		t.Fatalf("a refused install changed the install: %s", out)
	}

	// The local driver is pinned to the old release's file.
	if out, err := potluck("driver", "pin", "local"); err != nil {
		t.Fatalf("driver pin: %v\n%s", err, out)
	}

	// A daemon runs on the old release.
	stopDaemon := startDaemon(t, filepath.Join(bin, "potluck"), env, home)
	defer stopDaemon()
	c := client.NewUnix(filepath.Join(home, "potluck.sock"))
	h0, err := c.Health(context.Background())
	if err != nil || h0.Version != verA {
		t.Fatalf("daemon before: %+v %v", h0, err)
	}

	// potluck upgrade refuses the forgeries: nothing changes, the daemon runs on.
	for _, c := range []struct{ dir, want string }{
		{wrongKeyB, "does not verify with any configured update key"},
		{notInManifestB, "does not match the signed manifest"},
	} {
		if out, err := potluck("upgrade", "--release", c.dir); err == nil || !strings.Contains(out, c.want) {
			t.Fatalf("upgrade --release %s (%v), want %q:\n%s", filepath.Base(c.dir), err, c.want, out)
		}
		noCanary("upgrade from " + filepath.Base(c.dir))
	}
	if out, err := potluck("upgrade", "--release", relB, "--dry-run"); err != nil || !strings.Contains(out, "would restart the daemon") {
		t.Fatalf("upgrade --dry-run (%v):\n%s", err, out)
	}
	if out, _ := potluck("--version"); !strings.Contains(out, verA) {
		t.Fatalf("a refused or dry-run upgrade changed the install: %s", out)
	}
	if h, err := c.Health(context.Background()); err != nil || h.PID != h0.PID {
		t.Fatalf("a refused upgrade touched the daemon: %+v %v", h, err)
	}

	// The real upgrade: new programs, old ones kept, the daemon restarted on it.
	if out, err := potluck("upgrade", "--release", relB); err != nil || !strings.Contains(out, "restarted the daemon on "+verB) {
		t.Fatalf("upgrade with the old daemon (pid %d) slow to exit (%v):\n%s", h0.PID, err, out)
	}
	if out, _ := potluck("--version"); !strings.Contains(out, verB+" ("+commitB+")") {
		t.Fatalf("upgraded potluck --version: %s", out)
	}
	if _, err := os.Stat(filepath.Join(bin, "potluck"+".prev")); err != nil {
		t.Fatalf("the old potluck was not kept: %v", err)
	}
	hB, err := c.Health(context.Background())
	if err != nil || hB.Version != verB || hB.PID == h0.PID {
		t.Fatalf("daemon after the upgrade: %+v %v", hB, err)
	}
	// The upgrade re-pinned the driver it placed.
	var pins struct {
		Drivers map[string]struct{ SHA256 string } `json:"drivers"`
	}
	b, _ := os.ReadFile(filepath.Join(home, "driver_pins.json"))
	if err := json.Unmarshal(b, &pins); err != nil {
		t.Fatal(err)
	}
	drv, _ := os.ReadFile(filepath.Join(bin, "potluck-driver-local"))
	if sum := sha256.Sum256(drv); pins.Drivers["local"].SHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("the local driver's pin is %q, not the placed file's: %s", pins.Drivers["local"].SHA256, b)
	}
	// Going back to the older release is refused.
	if out, err := potluck("upgrade", "--release", relA); err == nil || !strings.Contains(out, "refusing to go back") {
		t.Fatalf("upgrade went back to an older release (%v):\n%s", err, out)
	}
	if out, err := potluck("upgrade", "--release", relB); err != nil || !strings.Contains(out, "already installed") {
		t.Fatalf("upgrade to the running release (%v):\n%s", err, out)
	}

	// Rollbacks. Release C's potluck is a script: its preflight is B's (a
	// copy outside the install directory), and its daemon either is B
	// (answering with the wrong commit) or never answers and ignores TERM.
	realB := filepath.Join(tmp, "realB", "potluck")
	copyFile2(t, filepath.Join(bin, "potluck"), realB)
	pidFile := filepath.Join(tmp, "newdaemon.pid")
	script := func(daemon string) map[string]string {
		return map[string]string{"potluck": fmt.Sprintf(`#!/bin/sh
if [ "$1" = daemon ] && [ $# -eq 1 ]; then
	echo $$ >%s
	%s
fi
exec %s "$@"
`, pidFile, daemon, realB)}
	}
	gone := func(what string) {
		t.Helper()
		b, err := os.ReadFile(pidFile)
		pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
		if err != nil || pid <= 0 {
			t.Fatalf("%s: the new daemon never started (%v)", what, err)
		}
		for deadline := time.Now().Add(5 * time.Second); syscall.Kill(pid, 0) == nil; time.Sleep(50 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("%s: the failed new daemon (pid %d) still runs", what, pid)
			}
		}
		os.Remove(pidFile)
	}

	// C1: the new daemon answers with another commit.
	relC1 := writeRelease(t, filepath.Join(tmp, "c1"), "main-c1c1c1c1c1c1", strings.Repeat("c", 40), time.Now().Add(time.Hour), script(""), key)
	out, err = potluck("upgrade", "--release", relC1)
	if err == nil || !strings.Contains(out, "old programs are back") || !strings.Contains(out, "not the release's") {
		t.Fatalf("upgrade to a daemon with the wrong commit (%v):\n%s", err, out)
	}
	gone("wrong commit")
	if out, _ := potluck("--version"); !strings.Contains(out, verB) {
		t.Fatalf("after the rollback, potluck --version: %s", out)
	}
	if h, err := c.Health(context.Background()); err != nil || h.Version != verB || h.PID == hB.PID {
		t.Fatalf("after the rollback the old daemon does not run again: %+v %v", h, err)
	}

	// C2: the running daemon is A, built elsewhere (outside the install
	// directory); the new one never answers and ignores TERM. The old one
	// comes back from its own program: A, not the install directory's B.
	stopDaemon()
	stopOther := startDaemon(t, otherA, env, home)
	defer stopOther()
	hA, err := c.Health(context.Background())
	if err != nil || hA.Version != verA {
		t.Fatalf("daemon from another directory: %+v %v", hA, err)
	}
	relC2 := writeRelease(t, filepath.Join(tmp, "c2"), "main-c2c2c2c2c2c2", strings.Repeat("d", 40), time.Now().Add(2*time.Hour),
		script("trap '' TERM; while :; do sleep 1; done"), key)
	c2 := exec.Command(filepath.Join(bin, "potluck"), "upgrade", "--release", relC2)
	c2.Env = append(append([]string(nil), env...), "POTLUCK_UPGRADE_START_WAIT=2s")
	o, err := c2.CombinedOutput()
	if err == nil || !strings.Contains(string(o), "old programs are back") || !strings.Contains(string(o), "did not answer") {
		t.Fatalf("upgrade to a daemon that never answers (%v):\n%s", err, o)
	}
	gone("never answers")
	if out, _ := potluck("--version"); !strings.Contains(out, verB) {
		t.Fatalf("after the rollback, potluck --version: %s", out)
	}
	if h, err := c.Health(context.Background()); err != nil || h.Version != verA || h.PID == hA.PID {
		t.Fatalf("the old daemon did not come back from its own program (want %s): %+v %v", verA, h, err)
	}

	t.Run("install.sh from GitHub", func(t *testing.T) {
		srv, ca := fakeGitHub(t, relB, "potluck-"+verB, "tok", false)
		bin2 := filepath.Join(tmp, "bin2")
		home2, _ := os.MkdirTemp("", "plu")
		defer os.RemoveAll(home2)
		_ = os.WriteFile(filepath.Join(home2, "config.json"), cfg, 0o600)
		// ghEnv points install.sh's API and web host at s.
		ghEnv := func(s *httptest.Server, ca, dir, path string) []string {
			return cleanEnv(t, "HOME="+filepath.Join(tmp, "home"), "POTLUCK_HOME="+home2, "POTLUCK_INSTALL_DIR="+dir,
				"POTLUCK_UPDATE_KEY="+pub, "POTLUCK_GITHUB_API="+s.URL, "POTLUCK_GITHUB="+s.URL, "CURL_CA_BUNDLE="+ca, "PATH="+path+":"+sysPath)
		}
		gh := func(extra ...string) (string, error) {
			c := exec.Command("sh", filepath.Join(root, "install.sh"))
			c.Env = append(ghEnv(srv, ca, bin2, tools), extra...)
			out, err := c.CombinedOutput()
			return string(out), err
		}
		if out, err := gh(); err == nil || !strings.Contains(out, "answered 404 with no token") {
			t.Fatalf("no token (%v):\n%s", err, out)
		}
		out, err := gh("GH_TOKEN=tok", "SHELL=/bin/zsh")
		if err != nil || !strings.Contains(out, "installed potluck "+verB) || !strings.Contains(out, "added "+bin2+" to PATH in ~/.zshrc") {
			t.Fatalf("with a token (%v):\n%s", err, out)
		}
		// zsh does not read ~/.profile: the PATH line goes in ~/.zshrc, and the
		// verified line names no temporary directory.
		if rc, _ := os.ReadFile(filepath.Join(tmp, "home", ".zshrc")); !strings.Contains(string(rc), `export PATH="`+bin2+`:$PATH"`) ||
			strings.Contains(out, ".profile") || strings.Contains(out, "/release:") {
			t.Fatalf("the PATH line or the verified line:\n%s\n.zshrc:\n%s", out, rc)
		}
		if !strings.Contains(out, "signature verified") {
			t.Fatalf("the download's signature was not checked:\n%s", out)
		}
		// Another shell, another startup file (fish quotes the directory).
		fo, ferr := gh("GH_TOKEN=tok", "SHELL=/usr/bin/fish")
		if fish, _ := os.ReadFile(filepath.Join(tmp, "home", ".config", "fish", "conf.d", "potluck.fish")); ferr != nil ||
			!strings.Contains(string(fish), `fish_add_path "`+bin2+`"`) {
			t.Fatalf("fish (%v):\n%s\npotluck.fish:\n%s", ferr, fo, fish)
		}
		// With no openssl that checks Ed25519 (a stock Mac), ssh-keygen checks
		// the download's SSHSIG signature.
		weakEnv := append(ghEnv(srv, ca, filepath.Join(tmp, "bin4"), filepath.Join(tmp, "weak")), "GH_TOKEN=tok")
		c := exec.Command("sh", filepath.Join(root, "install.sh"))
		c.Env = weakEnv
		o, err := c.CombinedOutput()
		if err != nil || !strings.Contains(string(o), "SSHSIG, checked with ssh-keygen") {
			t.Fatalf("no Ed25519 openssl, from GitHub (%v):\n%s", err, o)
		}
		// A public repository behind an address whose anonymous API allowance
		// is spent: with no token, install.sh takes no API call.
		pubSrv, pubCA := fakeGitHub(t, relB, "potluck-"+verB, "tok", true)
		pc := exec.Command("sh", filepath.Join(root, "install.sh"))
		pc.Env = ghEnv(pubSrv, pubCA, filepath.Join(tmp, "bin-public"), tools)
		if po, perr := pc.CombinedOutput(); perr != nil || !strings.Contains(string(po), "installed potluck "+verB) ||
			!strings.Contains(string(po), "signature verified") {
			t.Fatalf("public, no token, API refused (%v):\n%s", perr, po)
		}
	})
}

// serveFIFO makes path a FIFO that hands first to its first reader and
// later to every one after, and counts the reads.
func serveFIFO(t *testing.T, path string, first, later []byte) *atomic.Int32 {
	t.Helper()
	if err := syscall.Mkfifo(path, 0o644); err != nil {
		t.Fatal(err)
	}
	var n atomic.Int32
	var stop atomic.Bool
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			f, err := os.OpenFile(path, os.O_WRONLY, 0)
			if err != nil || stop.Load() {
				if f != nil {
					f.Close()
				}
				return
			}
			data := later
			if n.Add(1) == 1 {
				data = first
			}
			_, _ = f.Write(data)
			f.Close()
			// Wait until that reader has closed (a writer may open
			// without blocking only while a reader holds it), so the next
			// open counted is a new read, not the same one going on.
			for !stop.Load() {
				fd, err := syscall.Open(path, syscall.O_WRONLY|syscall.O_NONBLOCK, 0)
				if err != nil {
					break
				}
				syscall.Close(fd)
				time.Sleep(10 * time.Millisecond)
			}
		}
	}()
	t.Cleanup(func() {
		stop.Store(true)
		if r, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0); err == nil { // unblocks the writer
			r.Close()
		}
		<-done
	})
	return &n
}

// writeRelease writes a release of progs (name -> body) for this platform
// into dir: the tarball, SHA256SUMS and a manifest, signed with key, or
// unsigned (no manifest.json.sig) when key is nil.
func writeRelease(t *testing.T, dir, version, commit string, created time.Time, progs map[string]string, key ed25519.PrivateKey) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	name := release.Potluck.Tarball(version, runtime.GOOS, runtime.GOARCH)
	prefix := strings.TrimSuffix(name, ".tar.gz")
	var b bytes.Buffer
	gz := gzip.NewWriter(&b)
	tw := tar.NewWriter(gz)
	_ = tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: prefix + "/", Mode: 0o755})
	for n, body := range progs {
		_ = tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: prefix + "/" + n, Mode: 0o755, Size: int64(len(body))})
		_, _ = tw.Write([]byte(body))
	}
	tw.Close()
	gz.Close()
	if err := os.WriteFile(filepath.Join(dir, name), b.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(b.Bytes())
	sums := []byte(hex.EncodeToString(sum[:]) + "  " + name + "\n")
	if err := os.WriteFile(filepath.Join(dir, "SHA256SUMS"), sums, 0o644); err != nil {
		t.Fatal(err)
	}
	if key != nil {
		if _, err := release.Potluck.SignDir(dir, version, commit, created, key); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	m, err := release.Potluck.FromSums(version, commit, created, sums)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.MarshalIndent(m, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, release.ManifestFile), append(body, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// capableOpenSSL is an openssl on this machine that verifies Ed25519
// signatures as install.sh asks it to, else "".
func capableOpenSSL(t *testing.T) string {
	t.Helper()
	pub, priv, _ := ed25519.GenerateKey(nil)
	der, _ := x509.MarshalPKIXPublicKey(pub)
	dir := t.TempDir()
	msg, keyPEM, sig := filepath.Join(dir, "m"), filepath.Join(dir, "k.pem"), filepath.Join(dir, "s")
	_ = os.WriteFile(msg, []byte("probe\n"), 0o644)
	_ = os.WriteFile(keyPEM, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}), 0o644)
	_ = os.WriteFile(sig, ed25519.Sign(priv, []byte("probe\n")), 0o644)
	var cands []string
	if p, err := exec.LookPath("openssl"); err == nil {
		cands = append(cands, p)
	}
	cands = append(cands, "/opt/homebrew/bin/openssl", "/usr/local/bin/openssl", "/usr/bin/openssl")
	for _, p := range cands {
		if exec.Command(p, "pkeyutl", "-verify", "-pubin", "-inkey", keyPEM, "-rawin", "-in", msg, "-sigfile", sig).Run() == nil {
			return p
		}
	}
	return ""
}

// startDaemon runs bin's daemon on home in the foreground and waits until it
// answers; the func stops whichever daemon then runs on home.
func startDaemon(t *testing.T, bin string, env []string, home string) func() {
	t.Helper()
	d := exec.Command(bin, "daemon")
	d.Env = env
	logf, err := os.Create(filepath.Join(t.TempDir(), "daemon.log"))
	if err != nil {
		t.Fatal(err)
	}
	d.Stdout, d.Stderr = logf, logf
	if err := d.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { _ = d.Wait() }()
	stop := func() {
		c := exec.Command(bin, "daemon", "stop")
		c.Env = env
		_ = c.Run()
	}
	c := client.NewUnix(filepath.Join(home, "potluck.sock"))
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		if _, err := c.Health(context.Background()); err == nil {
			return stop
		}
		if time.Now().After(deadline) {
			stop()
			b, _ := os.ReadFile(logf.Name())
			t.Fatalf("daemon did not answer:\n%s", b)
		}
	}
}

// cleanEnv is this process's environment without HOME, PATH (unless set
// in extra), tokens or any POTLUCK_ or ZM_ variable, plus extra.
func cleanEnv(t *testing.T, extra ...string) []string {
	t.Helper()
	var env []string
	hasPath := false
	for _, kv := range extra {
		hasPath = hasPath || strings.HasPrefix(kv, "PATH=")
	}
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if k == "HOME" || k == "GH_TOKEN" || k == "GITHUB_TOKEN" || k == "CURL_CA_BUNDLE" || (k == "PATH" && hasPath) ||
			strings.HasPrefix(k, "POTLUCK_") || strings.HasPrefix(k, "ZM_") {
			continue
		}
		env = append(env, kv)
	}
	return append(append(env, "POTLUCK_NO_AUTOSTART=1"), extra...)
}

func copyDir(t *testing.T, src, dst string) string {
	t.Helper()
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatal(err)
	}
	ents, err := os.ReadDir(src)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		copyFile2(t, filepath.Join(src, e.Name()), filepath.Join(dst, e.Name()))
	}
	return dst
}

func copyFile2(t *testing.T, src, dst string) {
	t.Helper()
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, b, 0o755); err != nil {
		t.Fatal(err)
	}
}

// fakeGitHub serves dir's files as release tag of sengirunners/potluck over
// TLS, through GitHub's API and through github.com's release links (each
// redirecting to the next, then to storage, as GitHub does). Private: the API
// 404s without token, and so does every release link. Public: the release
// links answer anyone, and the API answers 403 to every call without token,
// as it does once an address has spent its 60 anonymous calls an hour. It
// returns the server and its certificate's PEM file.
func fakeGitHub(t *testing.T, dir, tag, token string, public bool) (*httptest.Server, string) {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var srv *httptest.Server
	has := func(name string) bool {
		for _, e := range ents {
			if e.Name() == name {
				return true
			}
		}
		return false
	}
	srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		web := "/sengirunners/potluck/releases/"
		if rest, ok := strings.CutPrefix(r.URL.Path, web); ok || strings.HasPrefix(r.URL.Path, "/storage/") {
			name, isLatest := strings.CutPrefix(rest, "latest/download/")
			atTag, isTag := strings.CutPrefix(rest, "download/"+tag+"/")
			stored, isStored := strings.CutPrefix(r.URL.Path, "/storage/")
			switch {
			case !public:
				http.NotFound(w, r)
			case isLatest && has(name):
				http.Redirect(w, r, web+"download/"+tag+"/"+name, http.StatusFound)
			case isTag && has(atTag):
				http.Redirect(w, r, "/storage/"+atTag, http.StatusFound)
			case isStored && has(stored):
				http.ServeFile(w, r, filepath.Join(dir, stored))
			default:
				http.NotFound(w, r)
			}
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+token {
			if public {
				http.Error(w, `{"message":"API rate limit exceeded"}`, http.StatusForbidden)
				return
			}
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
			return
		}
		base := "/repos/sengirunners/potluck/releases"
		if r.URL.Path == base+"/latest" {
			var assets []map[string]any
			for i, e := range ents {
				assets = append(assets, map[string]any{"url": fmt.Sprintf("%s%s/assets/%d", srv.URL, base, i+1), "id": i + 1,
					"name": e.Name(), "uploader": map[string]any{"login": "x", "url": srv.URL + "/users/x"},
					"browser_download_url": srv.URL + "/download/" + e.Name()})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"url": srv.URL + base + "/1", "tag_name": tag, "name": tag, "assets": assets})
			return
		}
		var i int
		if _, err := fmt.Sscanf(r.URL.Path, base+"/assets/%d", &i); err == nil && i >= 1 && i <= len(ents) &&
			r.Header.Get("Accept") == "application/octet-stream" {
			http.ServeFile(w, r, filepath.Join(dir, ents[i-1].Name()))
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	ca := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o644); err != nil {
		t.Fatal(err)
	}
	return srv, ca
}
