package main

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/potluck/kit/release"
)

// sysPath is a stock machine's PATH: on a Mac its openssl is LibreSSL (no
// Ed25519) and its ssh-keygen is OpenSSH's.
const sysPath = "/usr/bin:/bin:/usr/sbin:/sbin"

// scriptVar is install.sh's NAME="..." value (it may span lines).
func scriptVar(t *testing.T, script, name string) string {
	t.Helper()
	m := regexp.MustCompile(`(?ms)^` + name + `="([^"]*)"$`).FindStringSubmatch(script)
	if m == nil {
		t.Fatalf("install.sh sets no %s", name)
	}
	return m[1]
}

// stockCmd is a command from the stock PATH only (never Homebrew's), else "".
func stockCmd(name string) string {
	for _, d := range filepath.SplitList(sysPath) {
		if fi, err := os.Stat(filepath.Join(d, name)); err == nil && !fi.IsDir() {
			return filepath.Join(d, name)
		}
	}
	return ""
}

// toolDir makes dir a PATH directory with the stock commands install.sh
// needs (curl included), then progs: name -> "" (left out), "-" (left out,
// even curl), a body starting #! (a stand-in script) or a program's path.
func toolDir(t *testing.T, dir string, progs map[string]string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, c := range []string{"sed", "awk", "tr", "head", "cat", "cp", "mkdir", "mktemp", "rm", "wc", "grep", "tar",
		"gzip", "sha256sum", "shasum", "uname", "sysctl", "dirname", "base64", "curl"} {
		if _, set := progs[c]; set {
			continue
		}
		if p := stockCmd(c); p != "" {
			_ = os.Symlink(p, filepath.Join(dir, c))
		}
	}
	for n, v := range progs {
		switch {
		case v == "" || v == "-":
		case strings.HasPrefix(v, "#!"):
			_ = os.WriteFile(filepath.Join(dir, n), []byte(v), 0o755)
		default:
			_ = os.Symlink(v, filepath.Join(dir, n))
		}
	}
	return dir
}

// install.sh's probes are a real key's signatures of its probe message (a
// broken probe would make every machine "unable to check"), and its SSHSIG
// principal's namespace is Potluck's channel's.
func TestInstallScriptProbesAndNamespace(t *testing.T) {
	b, err := os.ReadFile("../../install.sh")
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if got := scriptVar(t, s, "NAMESPACE"); got != release.Potluck.Namespace {
		t.Errorf("install.sh NAMESPACE = %q, want kit/release.Potluck.Namespace %q", got, release.Potluck.Namespace)
	}
	key, err := base64.StdEncoding.DecodeString(scriptVar(t, s, "PROBE_KEY"))
	if err != nil || len(key) != ed25519.PublicKeySize {
		t.Fatalf("PROBE_KEY: %v", err)
	}
	sig, err := base64.StdEncoding.DecodeString(scriptVar(t, s, "PROBE_SIG"))
	if err != nil {
		t.Fatal(err)
	}
	msg := []byte(scriptVar(t, s, "PROBE_MSG") + "\n")
	if !ed25519.Verify(key, msg, sig) {
		t.Error("PROBE_SIG is not PROBE_KEY's signature of PROBE_MSG")
	}
	kg, err := exec.LookPath("ssh-keygen")
	if err != nil {
		t.Skip("no ssh-keygen here to check PROBE_SSHSIG")
	}
	dir := t.TempDir()
	allowed, sshsig := filepath.Join(dir, "allowed"), filepath.Join(dir, "probe.sshsig")
	_ = os.WriteFile(allowed, []byte(release.Potluck.AllowedSigner(scriptVar(t, s, "PRINCIPAL"), key)+"\n"), 0o644)
	_ = os.WriteFile(sshsig, []byte(scriptVar(t, s, "PROBE_SSHSIG")+"\n"), 0o644)
	c := exec.Command(kg, "-Y", "verify", "-f", allowed, "-I", scriptVar(t, s, "PRINCIPAL"), "-n", release.Potluck.Namespace, "-s", sshsig)
	c.Stdin = bytes.NewReader(msg)
	if out, err := c.CombinedOutput(); err != nil {
		t.Errorf("PROBE_SSHSIG does not verify: %v\n%s", err, out)
	}
}

// install.sh checks a release's signature with the tools a stock machine
// has, under /bin/sh and dash, against throwaway releases whose potluck is a
// stand-in: it installs a good release (SSHSIG with ssh-keygen, or the raw
// signature with OpenSSL 3), and refuses a tampered manifest, another key's
// signature, a tarball the manifest does not name, an unsigned release, a
// release other than the one asked for, a tool that says yes to anything,
// and, with no tool that can check, everything but an explicit
// --unsafe-skip-signature-check download over HTTPS.
func TestInstallScriptChecksSignatures(t *testing.T) {
	if _, err := exec.LookPath("ssh-keygen"); err != nil {
		t.Skip("no ssh-keygen here")
	}
	var shells []string
	for _, sh := range []string{"/bin/sh", "dash"} {
		if p, err := exec.LookPath(sh); err == nil {
			shells = append(shells, p)
		}
	}
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	tmp, err := os.MkdirTemp("", "plu") // short: ssh-keygen and the shells see these paths
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(tmp) })

	seed, pub, _ := release.NewKey()
	key, _ := release.ParseSeed(seed)
	otherSeed, _, _ := release.NewKey()
	otherKey, _ := release.ParseSeed(otherSeed)
	const version = "main-0123456789ab"
	commit := strings.Repeat("0123456789", 4)
	stand := func(tag string) map[string]string {
		return map[string]string{"potluck": "#!/bin/sh\ncase \"$1\" in\n--version) echo \"potluck version " + version + " " + tag + "\" ;;\n" +
			"upgrade) [ \"$2\" = --help ] && exit 0; mkdir -p \"$5\" && cp \"$0\" \"$5/potluck\" && echo \"installed potluck " + version + "\" ;;\nesac\n"}
	}
	rel := writeRelease(t, filepath.Join(tmp, "rel"), version, commit, time.Now(), stand("good"), key)
	variant := func(name string, edit func(dir string)) string {
		dir := copyDir(t, rel, filepath.Join(tmp, name))
		edit(dir)
		return dir
	}
	tarball := release.Potluck.Tarball(version, runtime.GOOS, runtime.GOARCH)
	tampered := variant("tampered", func(d string) {
		m, _ := os.ReadFile(filepath.Join(d, release.ManifestFile))
		_ = os.WriteFile(filepath.Join(d, release.ManifestFile), bytes.Replace(m, []byte(commit), []byte(strings.Repeat("f", 40)), 1), 0o644)
	})
	wrongKey := writeRelease(t, filepath.Join(tmp, "wrongkey"), version, commit, time.Now(), stand("good"), otherKey)
	misSum := variant("missum", func(d string) {
		e := writeRelease(t, filepath.Join(tmp, "evil"), version, commit, time.Now(), stand("evil"), nil)
		copyFile2(t, filepath.Join(e, tarball), filepath.Join(d, tarball))
	})
	unsigned := writeRelease(t, filepath.Join(tmp, "unsigned"), version, commit, time.Now(), stand("good"), nil)
	noSSHSIG := variant("nosshsig", func(d string) { os.Remove(filepath.Join(d, release.SSHSIGFile)) })
	otherSSHSIG := variant("othersshsig", func(d string) {
		m, _ := os.ReadFile(filepath.Join(d, release.ManifestFile))
		_ = os.WriteFile(filepath.Join(d, release.SSHSIGFile), release.Potluck.SSHSign(otherKey, m), 0o644)
	})

	find := stockCmd
	realKeygen := find("ssh-keygen")
	openssl3 := capableOpenSSL(t)
	tools := func(name, keygen, openssl string) string {
		return toolDir(t, filepath.Join(tmp, "path-"+name), map[string]string{"ssh-keygen": keygen, "openssl": openssl})
	}
	failing := "#!/bin/sh\nexit 1\n"
	yes := "#!/bin/sh\nexit 0\n"
	keygenOnly := tools("keygen", realKeygen, failing)
	neither := tools("neither", "", failing)

	runs := 0
	type result struct {
		out string
		err error
		bin string
	}
	install := func(t *testing.T, sh, path string, extra []string, args ...string) result {
		t.Helper()
		runs++
		dir := filepath.Join(tmp, fmt.Sprintf("r%d", runs))
		c := exec.Command(sh, append([]string{filepath.Join(root, "install.sh")}, args...)...)
		c.Env = append([]string{"HOME=" + filepath.Join(dir, "home"), "POTLUCK_HOME=" + filepath.Join(dir, "potluck"),
			"POTLUCK_INSTALL_DIR=" + filepath.Join(dir, "bin"), "POTLUCK_UPDATE_KEY=" + pub, "POTLUCK_NO_AUTOSTART=1",
			"PATH=" + path, "TMPDIR=" + tmp}, extra...)
		out, err := c.CombinedOutput()
		return result{string(out), err, filepath.Join(dir, "bin")}
	}
	installed := func(r result) bool {
		b, err := os.ReadFile(filepath.Join(r.bin, "potluck"))
		return err == nil && strings.Contains(string(b), " good\"")
	}
	ok := func(t *testing.T, r result, want string) {
		t.Helper()
		if r.err != nil || !strings.Contains(r.out, want) || !installed(r) {
			t.Fatalf("want an install with %q (%v):\n%s", want, r.err, r.out)
		}
	}
	refused := func(t *testing.T, r result, want ...string) {
		t.Helper()
		if r.err == nil {
			t.Fatalf("installed, want a refusal with %q:\n%s", want, r.out)
		}
		for _, w := range want {
			if !strings.Contains(r.out, w) {
				t.Fatalf("want a refusal with %q (%v):\n%s", w, r.err, r.out)
			}
		}
		if ents, _ := os.ReadDir(r.bin); len(ents) != 0 {
			t.Fatalf("a refused install placed %d files:\n%s", len(ents), r.out)
		}
	}

	for _, sh := range shells {
		t.Run(filepath.Base(sh), func(t *testing.T) {
			from := func(t *testing.T, path, dir string, args ...string) result {
				t.Helper()
				return install(t, sh, path, nil, append([]string{"--from", dir}, args...)...)
			}
			t.Run("stock tools", func(t *testing.T) { ok(t, from(t, sysPath, rel), "SSHSIG, checked with ssh-keygen") })
			t.Run("ssh-keygen without an Ed25519 openssl", func(t *testing.T) {
				ok(t, from(t, keygenOnly, rel), "SSHSIG, checked with ssh-keygen")
			})
			t.Run("tampered manifest", func(t *testing.T) { refused(t, from(t, sysPath, tampered), "does not verify") })
			t.Run("another key", func(t *testing.T) { refused(t, from(t, sysPath, wrongKey), "does not verify") })
			t.Run("tarball not in the manifest", func(t *testing.T) {
				refused(t, from(t, sysPath, misSum), "does not match the signed manifest")
			})
			t.Run("unsigned", func(t *testing.T) { refused(t, from(t, sysPath, unsigned), "has no signature") })
			t.Run("another release than asked for", func(t *testing.T) {
				refused(t, from(t, sysPath, rel, "--version", "potluck-main-ffffffffffff"), "but the signed manifest is release potluck-"+version)
			})
			t.Run("ssh-keygen that says yes to anything", func(t *testing.T) {
				refused(t, from(t, tools("yes-keygen", yes, failing), rel), "cannot check the release's signature")
			})
			t.Run("ssh-keygen that passes the probe and says yes to the rest", func(t *testing.T) {
				liar := "#!/bin/sh\ncase \"$*\" in *probe_signers*) exec " + realKeygen + " \"$@\" ;; esac\nexit 0\n"
				refused(t, from(t, tools("liar-keygen", liar, failing), tampered), "ssh-keygen here accepts an altered manifest")
			})
			t.Run("no tool that can check", func(t *testing.T) {
				r := from(t, neither, rel)
				refused(t, r, "cannot check the release's signature", "openssh-client")
				if strings.Contains(r.out, "--unsafe-skip-signature-check") {
					t.Fatalf("offered the unsafe flag for --from:\n%s", r.out)
				}
			})
			t.Run("a release with no SSHSIG and no Ed25519 openssl", func(t *testing.T) {
				refused(t, from(t, keygenOnly, noSSHSIG), "no SSHSIG signature")
			})
			t.Run("unsafe flag with --from", func(t *testing.T) {
				refused(t, from(t, neither, rel, "--unsafe-skip-signature-check"), "not --from")
			})
			t.Run("OpenSSL 3", func(t *testing.T) {
				if openssl3 == "" {
					t.Skip("no OpenSSL 3 here")
				}
				ok(t, from(t, tools("ossl", "", openssl3), rel), "Ed25519, checked with OpenSSL")
				ok(t, from(t, tools("ossl", "", openssl3), noSSHSIG), "Ed25519, checked with OpenSSL")
				refused(t, from(t, tools("ossl", "", openssl3), tampered), "does not verify")
				refused(t, from(t, tools("ossl-yes", "", yes), rel), "cannot check the release's signature")
				liar := "#!/bin/sh\ncase \"$1 $*\" in base64*|*probe*) exec " + openssl3 + " \"$@\" ;; esac\nexit 0\n"
				refused(t, from(t, tools("ossl-liar", "", liar), tampered), "openssl here accepts an altered manifest")
				// Either signature of a pinned key is enough: an SSHSIG of another key fails, the raw one verifies.
				refused(t, from(t, keygenOnly, otherSSHSIG), "does not verify")
				ok(t, from(t, tools("both", realKeygen, openssl3), otherSSHSIG), "Ed25519, checked with OpenSSL")
			})
			t.Run("wget without --https-only", func(t *testing.T) {
				// A BusyBox-like wget (no --https-only in its help) that serves the files of $WGET_DIR by name.
				stand := "#!/bin/sh\nout=; url=\nwhile [ $# -gt 0 ]; do case \"$1\" in\n" +
					"--help) echo 'Usage: wget [-q] [-O FILE] [--header STR] URL'; exit 0 ;;\n" +
					"-O) out=$2; shift ;;\n--https-only) exit 1 ;;\n-*) ;;\n*) url=$1 ;;\nesac; shift; done\n" +
					"f=\"$WGET_DIR/${url##*/}\"; [ -f \"$f\" ] || exit 8; cat \"$f\" >\"$out\"\n"
				wg := func(t *testing.T, path, dir string, args ...string) result {
					t.Helper()
					return install(t, sh, path, []string{"WGET_DIR=" + dir}, args...)
				}
				keygenWget := toolDir(t, filepath.Join(tmp, "path-wget-keygen"), map[string]string{"curl": "-", "wget": stand, "ssh-keygen": realKeygen, "openssl": failing})
				refused(t, wg(t, keygenWget, rel), "this wget cannot insist on HTTPS", "--version")
				refused(t, wg(t, keygenWget, rel, "--version", "potluck-"+version, "--unsafe-skip-signature-check"), "needs curl or a wget that has --https-only")
				ok(t, wg(t, keygenWget, rel, "--version", "potluck-"+version), "SSHSIG, checked with ssh-keygen")
				// A missing .sshsig: wget cannot tell "not found" from a network failure, so the script says it could
				// not download it, and falls back to the raw signature when OpenSSL 3 is here.
				refused(t, wg(t, keygenWget, noSSHSIG, "--version", "potluck-"+version), "could not download manifest.json.sshsig")
				if openssl3 == "" {
					t.Skip("no OpenSSL 3 here for the raw-signature fallback")
				}
				osslWget := toolDir(t, filepath.Join(tmp, "path-wget-ossl"), map[string]string{"curl": "-", "wget": stand, "ssh-keygen": realKeygen, "openssl": openssl3})
				ok(t, wg(t, osslWget, noSSHSIG, "--version", "potluck-"+version), "Ed25519, checked with OpenSSL")
			})
			t.Run("empty --version", func(t *testing.T) {
				refused(t, from(t, sysPath, rel, "--version", ""), "--version needs a release")
				refused(t, from(t, sysPath, rel, "--version", "potluck-"), "--version needs a release")
			})
			t.Run("from GitHub", func(t *testing.T) {
				if find("curl") == "" {
					t.Skip("no curl")
				}
				srv, ca := fakeGitHub(t, rel, "potluck-"+version, "tok", true)
				gh := func(t *testing.T, path string, args ...string) result {
					t.Helper()
					return install(t, sh, path, []string{"POTLUCK_GITHUB_API=" + srv.URL, "POTLUCK_GITHUB=" + srv.URL, "CURL_CA_BUNDLE=" + ca}, args...)
				}
				// No token, every anonymous API call refused: the release links alone.
				ok(t, gh(t, sysPath), "SSHSIG, checked with ssh-keygen")
				ok(t, gh(t, sysPath, "--version", "potluck-"+version), "SSHSIG, checked with ssh-keygen")
				refused(t, gh(t, sysPath, "--version", "main-ffffffffffff"), "has no release potluck-main-ffffffffffff")
				r := gh(t, neither)
				refused(t, r, "cannot check the release's signature", "--unsafe-skip-signature-check")
				ok(t, gh(t, neither, "--unsafe-skip-signature-check"), "WARNING: THE RELEASE SIGNATURE WAS NOT CHECKED")
				// The flag never overrides a signature that does not verify.
				bad, badCA := fakeGitHub(t, tampered, "potluck-"+version, "tok", true)
				refused(t, install(t, sh, sysPath, []string{"POTLUCK_GITHUB_API=" + bad.URL, "POTLUCK_GITHUB=" + bad.URL, "CURL_CA_BUNDLE=" + badCA},
					"--unsafe-skip-signature-check"), "does not verify")
				// A tag whose files are another release's.
				odd, oddCA := fakeGitHub(t, rel, "potluck-main-ffffffffffff", "tok", true)
				refused(t, install(t, sh, sysPath, []string{"POTLUCK_GITHUB_API=" + odd.URL, "POTLUCK_GITHUB=" + odd.URL, "CURL_CA_BUNDLE=" + oddCA},
					"--version", "potluck-main-ffffffffffff"), "but the signed manifest is release potluck-"+version)
			})
		})
	}
}
