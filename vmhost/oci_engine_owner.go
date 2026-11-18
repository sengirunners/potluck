package vmhost

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// The Docker ownership read: whether the local docker.service's main process
// is a root dockerd in this shell's mount and cgroup namespaces, and which
// listening Unix sockets it holds. Only root can read another process's exe,
// namespaces and descriptors, so the driver runs it with sudo -n -k (no
// prompt, no cached ticket) on every Docker connection.
//
// A host whose user has no passwordless sudo installs it once (OCIOwnerRootSteps)
// as a root-owned helper at a fixed path and a sudoers rule allowing exactly
// that path with no arguments. The helper takes nothing from its caller: it
// reads docker.service's main PID itself and uses /proc and a fixed PATH, so
// the rule gives the user one read-only answer and nothing more. Without the
// helper the driver runs the same script inline (sudo -n -k sh -c), which
// works where the user already has passwordless sudo.

// DockerOwnerHelper is where the root step installs the ownership read.
const DockerOwnerHelper = "/usr/local/libexec/potluck/docker-owner"

// dockerOwnerSudoers is the drop-in allowing a lending user that helper, with
// "-<uid>" added (no dot: sudo's includedir skips names with one).
const dockerOwnerSudoers = "/etc/sudoers.d/potluck-docker-owner"

// ErrOCIOwnerNeedsRoot is a Docker ownership read that could not run: sudo -n
// refused it, or the helper is missing its trusted install. OCIOwnerRootSteps
// fixes it. A read that ran and found Docker not owned is a different error.
var ErrOCIOwnerNeedsRoot = errors.New("reading Docker's socket owner needs root")

// The helper's PATH and /proc, and the owner its file must have. Tests point
// them at fixtures; production never changes them.
var (
	dockerOwnerPath      = "/usr/sbin:/usr/bin:/sbin:/bin"
	dockerOwnerHelper    = DockerOwnerHelper
	dockerOwnerHelperUID = uint32(0)
	dockerProcRoot       = "/proc"
)

// notOwnedExit is the script's exit code for "read, and not owned": any other
// failure is sudo or the shell not running it.
const notOwnedExit = 3

// dockerOwnerScript prints "dockerd <pid>", then one line per listening
// Unix socket with a path: "held <path>" when that pid holds it, else
// "other <path>". When the service's main process is not a root dockerd in
// this shell's namespaces it says why and exits notOwnedExit.
func dockerOwnerScript(proc, path string) string {
	return `set -u
[ $# -eq 0 ] || exit 2
PATH=` + shQuote(path) + `; export PATH; LC_ALL=C; export LC_ALL
proc=` + shQuote(proc) + `
no() { echo "not owned: $1"; exit ` + strconv.Itoa(notOwnedExit) + `; }
pid=$(systemctl show docker.service -p MainPID 2>/dev/null | sed -n 's/^MainPID=//p')
case $pid in ''|0|1|*[!0-9]*) no "docker.service has no main PID";; esac
[ "$(basename "$(readlink "$proc/$pid/exe")")" = dockerd ] || no "docker.service's main process is not dockerd"
[ "$(awk '/^Uid:/{print $2}' "$proc/$pid/status")" = 0 ] || no "dockerd does not run as root"
for ns in mnt cgroup; do
 a=$(readlink "$proc/self/ns/$ns") && [ -n "$a" ] && [ "$a" = "$(readlink "$proc/$pid/ns/$ns")" ] || no "dockerd is in another $ns namespace"
done
echo "dockerd $pid"
{ echo held-inodes; for fd in "$proc/$pid/fd/"*; do readlink "$fd" 2>/dev/null || :; done; } |
 sed -n -e 's/^held-inodes$/&/p' -e 's/^socket:\[\([0-9]*\)\]$/\1/p' |
 awk 'NR==FNR{if($1!="held-inodes")held[$1]=1;next} $4=="00010000" && $5=="0001" && $6=="01" && NF>=8 {print (($7 in held)?"held ":"other ") $8}' - "$proc/net/unix"
`
}

// dockerOwnerHelperBody is the helper file's exact content.
func dockerOwnerHelperBody(proc, path string) string {
	return "#!/bin/sh\n" + dockerOwnerScript(proc, path)
}

// OCIOwnerRootSteps is the one root step that installs the helper and its
// sudoers rule for the user running it with sudo (SUDO_USER), named by uid
// (a name sudoers could read as ALL or an alias grants nothing wider), in a
// drop-in of that uid's own (another lender's rule stays). Every command is
// chained with && (a step joined to others by && ignores set -e): it refuses
// without a plain user or when a directory above the helper is not root's
// alone, checks the rule with visudo before installing it, and removes it
// again unless the whole sudoers still checks and that user can now run the
// helper with sudo -n, cached credentials ignored (a sudoers without
// /etc/sudoers.d, or a later rule asking a password, would not).
func OCIOwnerRootSteps() []string {
	d := filepath.Dir(DockerOwnerHelper)
	rule := `#%s ALL=(root) NOPASSWD: ` + DockerOwnerHelper + ` ""\n`
	rootDir := func(dirs string) string {
		return `for p in ` + dirs + `; do [ -n "$(find "$p" -maxdepth 0 -type d -user 0 ! -perm -020 ! -perm -002)" ] || { echo "potluck: $p must be a directory only root can change" >&2; exit 1; }; done`
	}
	dropIn := dockerOwnerSudoers + `-"$uid"`
	step := `(export LC_ALL=C && umask 022 && u="${SUDO_USER:-}" && ` +
		`case "$u" in ""|root|[!A-Za-z_]*|*[!A-Za-z0-9_.-]*) echo "potluck: run this with sudo as the user who lends; SUDO_USER is \"$u\"" >&2; exit 1;; esac && ` +
		`uid=$(id -u "$u") && case "$uid" in ""|0|*[!0-9]*) echo "potluck: $u has no plain non-root uid" >&2; exit 1;; esac && ` +
		`mkdir -p ` + filepath.Dir(d) + ` && ` + rootDir("/ /usr /usr/local "+filepath.Dir(d)) + ` && ` +
		`install -d -o root -g root -m 0755 ` + d + ` && ` + rootDir(d) + ` && ` +
		`t=$(mktemp) && trap 'rm -f "$t"' EXIT && ` +
		`printf '%s' ` + shQuote(dockerOwnerHelperBody("/proc", dockerOwnerPath)) + ` >"$t" && install -o root -g root -m 0755 "$t" ` + DockerOwnerHelper + ` && ` +
		`printf ` + shQuote(rule) + ` "$uid" >"$t" && visudo -cqf "$t" && install -o root -g root -m 0440 "$t" ` + dropIn + ` && ` +
		`{ visudo -cq && { sudo -u "$u" sudo -n -k ` + DockerOwnerHelper + ` >/dev/null 2>&1; s=$?; [ "$s" -eq 0 ] || [ "$s" -eq ` + strconv.Itoa(notOwnedExit) + ` ]; } || ` +
		`{ rm -f ` + dropIn + `; echo "potluck: sudo still asks $u for a password for ` + DockerOwnerHelper + ` (does /etc/sudoers include /etc/sudoers.d, and does a later rule ask a password?), so the rule was removed" >&2; exit 1; }; })`
	return []string{step}
}

// trustedDockerOwnerHelper is nil when the helper is installed as the root
// step leaves it: a regular file with the current content, owned by root and
// writable by no one else, in directories only root can change.
// fs.ErrNotExist when there is none.
func trustedDockerOwnerHelper() error {
	st, err := os.Lstat(dockerOwnerHelper)
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", dockerOwnerHelper)
	}
	if err := rootOnly(dockerOwnerHelper, st); err != nil {
		return err
	}
	for dir := filepath.Dir(dockerOwnerHelper); ; dir = filepath.Dir(dir) {
		ds, err := os.Lstat(dir)
		if err != nil {
			return err
		}
		if !ds.IsDir() {
			return fmt.Errorf("%s is not a directory", dir)
		}
		if err := rootOnly(dir, ds); err != nil {
			return err
		}
		if dir == filepath.Dir(dir) {
			break
		}
	}
	b, err := os.ReadFile(dockerOwnerHelper)
	if err != nil {
		return err
	}
	if !bytes.Equal(b, []byte(dockerOwnerHelperBody(dockerProcRoot, dockerOwnerPath))) {
		return fmt.Errorf("%s is not this version's helper", dockerOwnerHelper)
	}
	return nil
}

// rootOnly: owned by root, and writable by no one else (a sticky directory,
// such as /tmp above a test's fixture, lets others add entries but not move
// root's).
func rootOnly(path string, st fs.FileInfo) error {
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok || (sys.Uid != 0 && sys.Uid != dockerOwnerHelperUID) {
		return fmt.Errorf("%s is not owned by root", path)
	}
	if st.Mode().Perm()&0o022 != 0 && !(st.IsDir() && st.Mode()&os.ModeSticky != 0) {
		return fmt.Errorf("%s is writable by others than root", path)
	}
	return nil
}

// readDockerOwner runs the ownership read as root: the helper when it is
// installed as the root step leaves it, else the same script inline (which
// works where the user has passwordless sudo, also when an older helper is
// installed). It returns the read's output; an error wrapping
// ErrOCIOwnerNeedsRoot when sudo refused it; the reason Docker is not owned;
// or why the read failed otherwise (a timeout is not a missing root step).
//
// It runs sudo -n -k: cached credentials ignored, as the daemon's driver has
// no terminal and no ticket. Without -k, a password typed at this terminal
// minutes ago would make the read work for doctor and host share, which then
// skip the root step the daemon needs.
func readDockerOwner() (string, error) {
	args := []string{"-n", "-k", dockerOwnerHelper}
	helperErr := trustedDockerOwnerHelper()
	if helperErr != nil {
		args = []string{"-n", "-k", "sh", "-c", dockerOwnerScript(dockerProcRoot, dockerOwnerPath), "sh"}
	}
	// sudo's own words are read below: in C, whatever the host's locale.
	b, err := boundedHostCommandEnv(2*time.Second, append(os.Environ(), "LC_ALL=C"), "sudo", args...)
	var ec exitCoder
	switch {
	case err == nil:
		return string(b), nil
	case errors.As(err, &ec) && ec.ExitCode() == notOwnedExit:
		return "", fmt.Errorf("local Docker is not this host's engine: %s", strings.TrimSpace(string(b)))
	case errors.Is(err, exec.ErrNotFound) || sudoRefused(b, ec):
		if helperErr != nil && !errors.Is(helperErr, fs.ErrNotExist) {
			return "", fmt.Errorf("%w: %v (the installed helper is not used: %v)", ErrOCIOwnerNeedsRoot, err, helperErr)
		}
		return "", fmt.Errorf("%w: %v", ErrOCIOwnerNeedsRoot, err)
	}
	return "", fmt.Errorf("the ownership read failed: %w", err)
}

// sudoRefused: sudo itself would not run the command (exit 1, its own words).
func sudoRefused(out []byte, ec exitCoder) bool {
	s := string(out)
	return ec != nil && ec.ExitCode() == 1 && (strings.Contains(s, "sudo: ") || strings.Contains(s, "sudo-rs: ") || strings.Contains(s, " is not allowed to execute "))
}

type exitCoder interface{ ExitCode() int }

// shQuote single-quotes s for a POSIX shell.
func shQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// dockerOwnerHolds reports whether the read names pid as dockerd and socket
// as exactly one listener, held by that pid (two listeners at one path is
// not a socket anyone can vouch for).
func dockerOwnerHolds(out string, pid int, socket string) bool {
	// sudo or systemd may warn on stderr before the read's first line.
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	i := slices.IndexFunc(lines, func(l string) bool { return strings.HasPrefix(l, "dockerd ") })
	if i < 0 || lines[i] != "dockerd "+strconv.Itoa(pid) {
		return false
	}
	lines = lines[i:]
	held, n := false, 0
	for _, l := range lines[1:] {
		switch l {
		case "held " + socket:
			held, n = true, n+1
		case "other " + socket:
			n++
		}
	}
	return held && n == 1
}

// VerifyOCIEngine runs the driver's check of the local Docker engine and the
// runtime it would use, as allocation does: doctor calls it so that "ready"
// means the driver will accept this engine. For Kata it also runs the native
// parent's root check (verifyHostRoot), as the driver does before every Kata
// box, even when the engine check failed, so doctor names every fix at once
// (errors.Join: errors.Is finds each).
func VerifyOCIEngine(cli, runtime string) error {
	id, err := inspectDockerRuntime(cli, runtime)
	if id.kata || strings.Contains(runtime, "kata") {
		err = errors.Join(err, verifyHostRoot())
	}
	return err
}
