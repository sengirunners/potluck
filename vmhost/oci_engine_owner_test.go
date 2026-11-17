package vmhost

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// fakeSudo stands in for sudo -n [-k] <command>. SUDO_POLICY is the user's
// sudoers: "nopasswd" (the default) runs anything; "helper" runs only the
// helper with no arguments (the root step's rule) without a password;
// "password" runs nothing without one. A command that needs a password runs
// when SUDO_TICKET (a cached ticket, as after sudo -v) is set and -k is not
// given, as real sudo does; else it refuses as sudo -n does. SUDO_REFUSE
// refuses everything, SUDO_EXIT exits with that code, SUDO_WARN warns first.
const fakeSudo = `#!/bin/sh
[ "$1" = -n ] || exit 2
shift
k=
if [ "$1" = -k ]; then k=1; shift; fi
refuse() { echo 'sudo: a password is required' >&2; exit 1; }
[ -z "${SUDO_REFUSE:-}" ] || refuse
ticket=
[ -n "$k" ] || [ -z "${SUDO_TICKET:-}" ] || ticket=1
case "${SUDO_POLICY:-nopasswd}" in
nopasswd) ;;
helper) [ "$*" = "$FAKE_HELPER" ] || [ -n "$ticket" ] || refuse;;
password) [ -n "$ticket" ] || refuse;;
*) exit 2;;
esac
if [ -n "${SUDO_EXIT:-}" ]; then exit "$SUDO_EXIT"; fi
if [ -n "${SUDO_WARN:-}" ]; then echo 'sudo: unable to resolve host x' >&2; fi
echo "$*" >>"$SUDO_LOG"
exec "$@"
`

// fakeDockerProc builds a private /proc where docker.service's main PID 1817
// is a root dockerd holding a listener on the returned socket, then breaks it
// as mode says. It puts fake sudo (fakeSudo) and systemctl first on PATH and
// points the ownership read's /proc, PATH and helper at the fixture. The
// helper file does not exist.
func fakeDockerProc(t *testing.T, mode string) (socket string) {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	proc, bin := filepath.Join(dir, "proc"), filepath.Join(dir, "bin")
	for _, d := range []string{"1817/ns", "1817/fd", "self/ns", "net", "sys/kernel/random", "../bin"} {
		if err := os.MkdirAll(filepath.Join(proc, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	uid := "0"
	if mode == "not-root" {
		uid = "1000"
	}
	exe := "/usr/bin/dockerd"
	if mode == "not-dockerd" {
		exe = "/usr/bin/socat"
	}
	inode := "36356"
	if mode == "wrong-fd" {
		inode = "99999"
	}
	_ = os.WriteFile(filepath.Join(proc, "1817/status"), []byte("Uid:\t"+uid+"\t0\t0\t0\n"), 0o600)
	_ = os.Symlink(exe, filepath.Join(proc, "1817/exe"))
	_ = os.Symlink("pipe:[1]", filepath.Join(proc, "1817/fd/3"))
	_ = os.Symlink("socket:["+inode+"]", filepath.Join(proc, "1817/fd/4"))
	for _, ns := range []string{"mnt", "cgroup"} {
		target := ns + ":[123]"
		_ = os.Symlink(target, filepath.Join(proc, "self/ns", ns))
		if mode == "wrong-namespace" {
			target = ns + ":[456]"
		}
		_ = os.Symlink(target, filepath.Join(proc, "1817/ns", ns))
	}
	socket = filepath.Join(dir, "docker.sock")
	unix := "Num RefCount Protocol Flags Type St Inode Path\n" +
		"0000: 00000002 00000000 00010000 0001 01 11111 /run/other.sock\n" +
		"0000: 00000002 00000000 00010000 0001 01 36356 " + socket + "\n"
	if mode == "two-listeners" {
		unix += "0000: 00000002 00000000 00010000 0001 01 22222 " + socket + "\n"
	}
	_ = os.WriteFile(filepath.Join(proc, "net/unix"), []byte(unix), 0o600)
	mainPID := "1817"
	if mode == "no-service" {
		mainPID = "0"
	}
	for name, body := range map[string]string{
		"sudo":      fakeSudo,
		"systemctl": "#!/bin/sh\necho MainPID=" + mainPID + "\necho InvocationID=x\n",
	} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	t.Setenv("SUDO_LOG", filepath.Join(dir, "sudo.log"))
	oldProc, oldPath, oldHelper, oldUID := dockerProcRoot, dockerOwnerPath, dockerOwnerHelper, dockerOwnerHelperUID
	dockerProcRoot, dockerOwnerPath = proc, bin+":/usr/bin:/bin"
	dockerOwnerHelper, dockerOwnerHelperUID = filepath.Join(dir, "libexec/potluck/docker-owner"), uint32(os.Getuid())
	t.Setenv("FAKE_HELPER", dockerOwnerHelper)
	t.Cleanup(func() {
		dockerProcRoot, dockerOwnerPath, dockerOwnerHelper, dockerOwnerHelperUID = oldProc, oldPath, oldHelper, oldUID
	})
	return socket
}

// installFakeHelper installs the helper as the root step would, at the
// fixture's path.
func installFakeHelper(t *testing.T, body string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(dockerOwnerHelper), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dockerOwnerHelper, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dockerOwnerHelper, mode); err != nil {
		t.Fatal(err)
	}
}

func sudoRan(t *testing.T) string {
	b, _ := os.ReadFile(os.Getenv("SUDO_LOG"))
	return string(b)
}

// The read accepts only the service's root dockerd in this shell's
// namespaces holding the socket, through the helper or inline alike.
func TestDockerOwnerRead(t *testing.T) {
	for _, helper := range []bool{false, true} {
		for _, mode := range []string{"owned", "wrong-fd", "wrong-namespace", "not-dockerd", "not-root", "no-service", "two-listeners"} {
			t.Run(map[bool]string{false: "inline/", true: "helper/"}[helper]+mode, func(t *testing.T) {
				socket := fakeDockerProc(t, mode)
				if helper {
					installFakeHelper(t, dockerOwnerHelperBody(dockerProcRoot, dockerOwnerPath), 0o755)
				}
				out, err := readDockerOwner()
				if ran := sudoRan(t); helper != strings.HasPrefix(ran, dockerOwnerHelper+"\n") {
					t.Fatalf("sudo ran %q", ran)
				}
				if errors.Is(err, ErrOCIOwnerNeedsRoot) {
					t.Fatalf("a read that ran is not a missing root step: %v", err)
				}
				held := err == nil && dockerOwnerHolds(out, 1817, socket)
				if held != (mode == "owned") {
					t.Fatalf("held %v: %q %v", held, out, err)
				}
				if mode == "owned" && dockerOwnerHolds(out, 1817, "/run/other.sock") {
					t.Fatal("a listener dockerd does not hold counted as held")
				}
				if mode == "owned" && dockerOwnerHolds(out, 1818, socket) {
					t.Fatal("another pid accepted")
				}
			})
		}
	}
}

// sudo refusing is a missing root step. A helper not installed as the root
// step leaves it never runs: the script runs inline instead, so a host whose
// user has passwordless sudo keeps working with an older helper.
func TestDockerOwnerNeedsRoot(t *testing.T) {
	for _, mode := range []string{"absent", "current", "stale", "group-writable", "symlink", "dir-writable"} {
		t.Run(mode, func(t *testing.T) {
			socket := fakeDockerProc(t, "owned")
			body := dockerOwnerHelperBody(dockerProcRoot, dockerOwnerPath)
			switch mode {
			case "current":
				installFakeHelper(t, body, 0o755)
			case "stale":
				installFakeHelper(t, "#!/bin/sh\necho dockerd 1817\necho held "+socket+"\n", 0o755)
			case "group-writable":
				installFakeHelper(t, body, 0o775)
			case "symlink":
				installFakeHelper(t, body, 0o755)
				real := dockerOwnerHelper + ".real"
				_ = os.Rename(dockerOwnerHelper, real)
				_ = os.Symlink(real, dockerOwnerHelper)
			case "dir-writable":
				installFakeHelper(t, body, 0o755)
				_ = os.Chmod(filepath.Dir(dockerOwnerHelper), 0o777)
			}
			out, err := readDockerOwner()
			if err != nil || !dockerOwnerHolds(out, 1817, socket) {
				t.Fatalf("with sudo: %q %v", out, err)
			}
			if helper := strings.HasPrefix(sudoRan(t), dockerOwnerHelper+"\n"); helper != (mode == "current") {
				t.Fatalf("sudo ran %q", sudoRan(t))
			}
			t.Setenv("SUDO_REFUSE", "1")
			if _, err := readDockerOwner(); !errors.Is(err, ErrOCIOwnerNeedsRoot) {
				t.Fatalf("sudo refused: want a root step, got %v", err)
			}
		})
	}
}

// The read answers what the daemon's driver gets: it has no terminal and no
// cached sudo ticket. A ticket left by a password typed at this terminal
// minutes ago (sudo -v) does not make the read work, so doctor and share
// still find the root step; once the root step's helper and rule are in, the
// read works with or without a ticket. General passwordless sudo needs no
// helper.
func TestDockerOwnerReadIgnoresCachedTicket(t *testing.T) {
	socket := fakeDockerProc(t, "owned")
	body := dockerOwnerHelperBody(dockerProcRoot, dockerOwnerPath)
	for _, c := range []struct {
		name, policy           string
		ticket, helper, worked bool
	}{
		{"password/ticket", "password", true, false, false},
		{"password/no-ticket", "password", false, false, false},
		{"helper-rule/ticket", "helper", true, true, true},
		{"helper-rule/no-ticket", "helper", false, true, true},
		{"helper-rule/helper-gone", "helper", true, false, false},
		{"nopasswd/no-ticket", "nopasswd", false, false, true},
		{"nopasswd/ticket", "nopasswd", true, false, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			_ = os.Remove(dockerOwnerHelper)
			if c.helper {
				installFakeHelper(t, body, 0o755)
			}
			t.Setenv("SUDO_POLICY", c.policy)
			t.Setenv("SUDO_TICKET", map[bool]string{true: "1"}[c.ticket])
			out, err := readDockerOwner()
			if !c.worked {
				if !errors.Is(err, ErrOCIOwnerNeedsRoot) {
					t.Fatalf("want a root step, got %q %v", out, err)
				}
				return
			}
			if err != nil || !dockerOwnerHolds(out, 1817, socket) {
				t.Fatalf("%q %v", out, err)
			}
		})
	}
}

// A warning sudo prints before the read does not hide its answer.
func TestDockerOwnerReadAfterWarning(t *testing.T) {
	socket := fakeDockerProc(t, "owned")
	t.Setenv("SUDO_WARN", "1")
	if out, err := readDockerOwner(); err != nil || !dockerOwnerHolds(out, 1817, socket) {
		t.Fatalf("%q %v", out, err)
	}
}

// A read that failed for another reason (a timeout, a broken shell) is not a
// root step: running one would change nothing.
func TestDockerOwnerOtherFailure(t *testing.T) {
	fakeDockerProc(t, "owned")
	for _, code := range []string{"1", "2", "137"} {
		t.Setenv("SUDO_EXIT", code)
		if _, err := readDockerOwner(); err == nil || errors.Is(err, ErrOCIOwnerNeedsRoot) {
			t.Fatalf("exit %s: %v", code, err)
		}
	}
}

// The root step installs exactly the helper the driver trusts and a sudoers
// rule for SUDO_USER allowing only that path with no arguments, checked
// before it is installed. Joined to other steps with && (as RootScript joins
// them, where set -e does nothing) any failure stops it and the steps after
// it, and leaves no rule behind.
func TestOCIOwnerRootStep(t *testing.T) {
	for _, c := range []struct{ name, user, bad string }{
		{"ok", "alice", ""}, {"helper-owned", "alice", "exit0"}, {"dotted", "Al.ice", ""}, {"alias-like", "ALL", ""}, {"empty", "", ""}, {"root", "root", ""}, {"space", "a b", ""}, {"dash", "-x", ""}, {"percent", "al%ce", ""},
		{"visudo-file", "bob", "visudo-cqf"}, {"visudo-all", "bob", "visudo-cq"}, {"not-granted", "bob", "sudo"}, {"helper-install", "bob", "install-helper"},
		{"mktemp", "bob", "mktemp"}, {"parent-dir", "bob", "find-/usr/local"}, {"own-dir", "bob", "find-" + filepath.Dir(DockerOwnerHelper)},
	} {
		t.Run(c.name, func(t *testing.T) {
			root, bin := t.TempDir(), t.TempDir()
			// Every absolute path the step writes lands under root; BAD makes one command fail.
			fakes := map[string]string{
				"install": `eval "dst=\${$#}"; case $dst in */docker-owner) [ "$BAD" != install-helper ] || exit 1;; esac
for a; do [ "$a" != -d ] || { /bin/mkdir -p "$ROOT$dst"; exit; }; done
eval "src=\${$(($#-1))}"; /bin/mkdir -p "$ROOT$(dirname "$dst")"; cp "$src" "$ROOT$dst"`,
				"mkdir":  `[ "$1" = -p ] && /bin/mkdir -p "$ROOT$2"`,
				"rm":     `[ "$1" = -f ] && case $2 in "$ROOT"/*) /bin/rm -f "$2";; *) /bin/rm -f "$ROOT$2";; esac`,
				"find":   `[ "$BAD" != "find-$1" ] && echo "$1"`,
				"mktemp": `[ "$BAD" != mktemp ] && /usr/bin/mktemp "$ROOT/tmp.XXXXXX"`,
				"visudo": `echo "$*" >>"$ROOT/visudo.log"; [ "$BAD" != "visudo$1" ]`,
				"id":     `[ "$*" = "-u $SUDO_USER" ] && echo 1234`,
				"sudo":   `[ "$*" = "-u $SUDO_USER sudo -n -k ` + DockerOwnerHelper + `" ] && [ "$BAD" != sudo ] && exit "${HELPER_EXIT:-3}"`,
			}
			for name, body := range fakes {
				_ = os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"+body+"\n"), 0o700)
			}
			steps := OCIOwnerRootSteps()
			if len(steps) != 1 {
				t.Fatalf("steps %q", steps)
			}
			cmd := exec.Command("sh", "-c", "true && "+steps[0]+" && echo next-step")
			cmd.Env = append(os.Environ(), "PATH="+bin+":/usr/bin:/bin", "ROOT="+root, "SUDO_USER="+c.user, "BAD="+c.bad, "HELPER_EXIT="+map[bool]string{true: "0", false: "3"}[c.bad == "exit0"])
			out, err := cmd.CombinedOutput()
			rule, _ := os.ReadFile(filepath.Join(root, dockerOwnerSudoers+"-1234"))
			if c.name != "ok" && c.name != "helper-owned" && c.name != "dotted" && c.name != "alias-like" {
				if err == nil || len(rule) > 0 || strings.Contains(string(out), "next-step") {
					t.Fatalf("went on for %q / %q: %s rule %q", c.user, c.bad, out, rule)
				}
				return
			}
			if err != nil || !strings.Contains(string(out), "next-step") {
				t.Fatalf("%v: %s", err, out)
			}
			if want := "#1234 ALL=(root) NOPASSWD: " + DockerOwnerHelper + " \"\"\n"; string(rule) != want {
				t.Fatalf("rule %q, want %q", rule, want)
			}
			helper, _ := os.ReadFile(filepath.Join(root, DockerOwnerHelper))
			if string(helper) != dockerOwnerHelperBody("/proc", "/usr/sbin:/usr/bin:/sbin:/bin") {
				t.Fatalf("helper is not the driver's script:\n%s", helper)
			}
			log, _ := os.ReadFile(filepath.Join(root, "visudo.log"))
			if !strings.HasPrefix(string(log), "-cqf ") || !strings.HasSuffix(string(log), "\n-cq\n") {
				t.Fatalf("visudo did not check the rule first and then the whole sudoers: %q", log)
			}
		})
	}
}
