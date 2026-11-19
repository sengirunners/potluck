package vmhost

import (
	"encoding/json"
	"fmt"
	"github.com/sengirunners/potluck/softnet"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// tartCLI is the Tart VM layer: it runs the tart binary.
type tartCLI struct{ bin, softnetPath string }

func (t tartCLI) run(a ...string) (string, error) {
	out, err := exec.Command(t.bin, a...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("tart %s: %v: %s", strings.Join(a, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

func (t tartCLI) List() ([]vmInfo, error) {
	out, err := exec.Command(t.bin, "list", "--format", "json").Output()
	if err != nil {
		return nil, fmt.Errorf("tart list: %v", err)
	}
	var ts []struct {
		Name, Source string
		Running      bool
	}
	if err := json.Unmarshal(out, &ts); err != nil {
		return nil, err
	}
	vs := make([]vmInfo, len(ts))
	for i, v := range ts {
		vs[i] = vmInfo{Name: v.Name, Running: v.Running, Local: v.Source == "local"}
	}
	return vs, nil
}

func (t tartCLI) OSOf(name string) (string, error) {
	out, err := exec.Command(t.bin, "get", name, "--format", "json").Output()
	if err != nil && strings.Contains(name, "@sha256:") {
		return t.pulledOS(name)
	}
	if err != nil {
		return "", fmt.Errorf("no Tart image %q here (tart pull or tart clone it first)", name)
	}
	var g struct{ OS string }
	if err := json.Unmarshal(out, &g); err != nil || g.OS == "" {
		return "", fmt.Errorf("tart get %s: no OS", name)
	}
	return g.OS, nil
}

// Clone is an APFS copy-on-write clone with its own MAC.
func (t tartCLI) Clone(image, vm string, cpu, memory int) error {
	if _, err := t.run("clone", image, vm); err != nil {
		return err
	}
	set := []string{"set", vm, "--random-mac"}
	if cpu > 0 {
		set = append(set, "--cpu", strconv.Itoa(cpu))
	}
	if memory > 0 {
		set = append(set, "--memory", strconv.Itoa(memory))
	}
	_, err := t.run(set...)
	return err
}

// deadman runs tart run under a guard: when the supervisor's end of stdin
// closes (it exited or was killed), the guard kills the VM and deletes the
// clone, so no VM outlives its supervisor. Its exit is tart run's.
// (sh gives a background job /dev/null as stdin, hence fd 3.)
const deadman = `vm=$1; bin=$2; shift 2; exec 3<&0
"$bin" "$@" 3<&- & p=$!
( cat <&3; kill -KILL $p; "$bin" delete "$vm" ) >/dev/null 2>&1 & g=$!; exec 3<&-
wait $p; s=$?; kill $g 2>/dev/null; exit $s`

// softnetArgs isolate a Tart guest. Softnet passes the gateway (this Mac) by
// default: block it but DNS, so a guest reaches no host service either; and
// block outbound mail (tcp 25, 465, 587, to anywhere; zm-eakh.12.5.13), so a
// box cannot send spam from the lender's IP. Softnet's rule syntax, which
// tart passes through as --block/--allow: "out proto tcp port 25" (target
// 0.0.0.0/0 when omitted); a rule with a port wins over one without, and
// block wins ties. Softnet filters IPv4 only and lets no IPv6 out.
var softnetArgs = []string{"--net-softnet",
	"--net-softnet-block=" + SoftnetBlockRules(),
	"--net-softnet-allow=" + SoftnetAllowRules()}

// SoftnetBlockRules and SoftnetAllowRules are the isolation policy used both
// at boot and by the host's capability preflight. Keep the probe and boot on
// the same rules, including the host DNS exception and outbound mail blocks.
func SoftnetBlockRules() string { return "@host," + softnetMailBlock }
func SoftnetAllowRules() string { return "out proto udp @host port 53,out proto tcp @host port 53" }

// TartBootArguments is the production headless boot policy; preflight checks
// that the selected Tart understands each option before pulling an image.
func TartBootArguments(isolated bool) []string {
	a := []string{"--no-graphics", "--no-audio", "--no-clipboard"}
	if isolated {
		a = append(a, softnetArgs...)
	}
	return a
}

// softnetMailBlock is one block rule per mail port.
var softnetMailBlock = func() string {
	rs := make([]string, len(mailPorts))
	for i, p := range mailPorts {
		rs[i] = "out proto tcp port " + strconv.Itoa(p)
	}
	return strings.Join(rs, ",")
}()

// mailPorts are outbound mail's TCP ports (SMTP, SMTPS, submission): every
// layer blocks them for an isolated VM.
var mailPorts = []int{25, 465, 587}

// managedSoftnet is the Mac's managed Softnet store (a test replaces it):
// a boot that pins its build checks the build again first.
var managedSoftnet = softnet.System

// Boot runs tart run with no window, audio, clipboard or --dir, under the
// deadman guard; isolated is Softnet.
func (t tartCLI) Boot(vm string, isolated bool, log io.Writer) (*exec.Cmd, io.WriteCloser, error) {
	return t.boot(vm, isolated, log, deadman)
}

func (t tartCLI) boot(vm string, isolated bool, log io.Writer, guard string) (*exec.Cmd, io.WriteCloser, error) {
	helper := t.softnetPath
	if isolated && helper != "" {
		if s := managedSoftnet(); helper == s.Current() {
			id, err := s.CheckCurrent()
			switch {
			case id == "":
				return nil, nil, fmt.Errorf("the managed Softnet Potluck checked is gone (%v): run `potluck host` again so it checks the Softnet this Mac has now", err)
			case err != nil:
				return nil, nil, fmt.Errorf("the managed Softnet %s cannot be used: %v. If `%s status` says it is sound, this daemon's potluck is older than the one that installed it: upgrade or restart the daemon", id, err, softnet.Command)
			}
			// Tart runs exactly the build just checked, even if current
			// moves before it starts.
			helper = s.Build(id)
		}
		info, err := os.Stat(helper)
		if err != nil {
			return nil, nil, fmt.Errorf("checked Softnet at %s is unavailable: %w", helper, err)
		}
		if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
			return nil, nil, fmt.Errorf("checked Softnet at %s is not executable", helper)
		}
	}
	a := append([]string{"run"}, TartBootArguments(isolated)...)
	cmd := exec.Command("sh", append([]string{"-c", guard, "sh", vm, t.bin}, append(a, vm)...)...)
	if isolated && t.softnetPath != "" {
		// Tart resolves softnet through PATH. Use the helper preflight checked,
		// even when the daemon was started from a different shell.
		cmd.Env = append(os.Environ(), "PATH="+filepath.Dir(helper)+string(os.PathListSeparator)+os.Getenv("PATH"))
	}
	cmd.Stdout, cmd.Stderr = log, log
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	held, err := cmd.StdinPipe()
	if err != nil {
		return nil, nil, err
	}
	return cmd, held, cmd.Start()
}

// Components names what a Tart boot runs, for a failed boot's words
// (componentNamer): tart's version, Softnet's (isolated) with its path, and
// the macOS release. A program whose version cannot be read says so; each
// read is bounded (2 s), so a wedged binary cannot hold the failure up.
func (t tartCLI) Components(isolated bool) string {
	cs := []string{"tart " + versionOf(t.bin)}
	if isolated {
		// The helper Boot ran is the checked one when pinned (boot puts its
		// directory first on PATH), else the daemon's PATH one.
		p, err := t.softnetPath, error(nil)
		if p == "" {
			p, err = exec.LookPath("softnet")
		}
		if err != nil {
			cs = append(cs, "no softnet on PATH")
		} else {
			cs = append(cs, "softnet "+strings.TrimPrefix(versionOf(p), "softnet ")+" at "+tilde(p))
		}
	}
	if out, err := boundedHostCommand(2*time.Second, "sw_vers", "-productVersion"); err == nil {
		if v := strings.TrimSpace(string(out)); v != "" {
			cs = append(cs, "macOS "+v)
		}
	}
	return strings.Join(cs, ", ")
}

// versionOf is the first line bin --version prints, bounded, or "(version
// unreadable)".
func versionOf(bin string) string {
	out, err := boundedHostCommand(2*time.Second, bin, "--version")
	v, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
	if err != nil || v == "" {
		return "(version unreadable)"
	}
	if len(v) > 60 {
		v = v[:60] + "..."
	}
	return strings.ToValidUTF8(v, "?")
}

// CloseLogin is the step run in every Tart guest once its agent answers and
// before it serves anything (loginCloser). zm drives a guest only through the
// guest agent, never SSH; Cirrus Labs' images keep SSH and macOS Screen
// Sharing on with admin/admin, and Softnet takes inbound connections, so a
// device on the lender's LAN could log in to a renter's VM. It stops them
// (sshd through launchd or systemd, macOS Screen Sharing) and fails while
// port 22 or 5900 still answers in the guest. It changes no password: a
// macOS login keychain would no longer open with it.
func (tartCLI) CloseLogin() []string { return []string{"sh", "-c", closeLogin} }

const closeLogin = `root() { if [ "$(id -u)" = 0 ]; then "$@"; else sudo -n "$@"; fi; }
case "$(uname -s)" in
Darwin) for s in com.openssh.sshd com.apple.screensharing; do
	root launchctl bootout "system/$s"; root launchctl disable "system/$s"; done ;;
*) for u in ssh.socket ssh.service sshd.service; do root systemctl disable --now "$u"; done ;;
esac >/dev/null 2>&1
answers() {
	if command -v nc >/dev/null 2>&1; then nc -z -w 2 127.0.0.1 "$1" </dev/null >/dev/null 2>&1
	elif command -v bash >/dev/null 2>&1; then bash -c 'exec 3<>"/dev/tcp/127.0.0.1/$0"' "$1" 2>/dev/null
	else return 1; fi
}
i=0
while [ "$i" -lt 20 ]; do
	answers 22 || answers 5900 || exit 0
	i=$((i + 1)); sleep 0.25
done
echo "potluck-driver-tart: the guest still accepts SSH or Screen Sharing logins (port 22 or 5900); potluck lends no VM a LAN device can log in to" >&2
exit 1`

// Exec goes through the Tart guest agent (tart exec, over virtio-vsock).
func (t tartCLI) Exec(vm string, stdin io.Reader, stdout, stderr io.Writer, argv ...string) *exec.Cmd {
	a := []string{"exec"}
	if stdin != nil {
		a = append(a, "-i")
	}
	cmd := exec.Command(t.bin, append(append(a, vm), argv...)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
	return cmd
}

// Destroy kills the tart run process group (powering the VM off) and deletes
// the clone; with no runPID it stops the VM through tart.
func (t tartCLI) Destroy(vm string, runPID int) {
	if !t.powerOff(vm, runPID) {
		return // no such VM
	}
	if _, err := t.run("delete", vm); err != nil {
		fmt.Fprintln(os.Stderr, "potluck-driver-tart:", err)
	}
}

// powerOff stops the VM and says whether it exists.
func (t tartCLI) powerOff(vm string, runPID int) bool {
	if runPID > 0 {
		_ = syscall.Kill(-runPID, syscall.SIGKILL)
	}
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		vs, err := t.List()
		if err != nil {
			break
		}
		running, found := false, false
		for _, v := range vs {
			if v.Name == vm {
				found, running = true, v.Running
			}
		}
		if !found {
			return false
		}
		if !running {
			break
		}
		if runPID <= 0 { // a VM no supervisor of ours holds: stop it through tart
			_, _ = t.run("stop", "--timeout", "0", vm)
		}
	}
	return true
}

// pulledOS reads a pulled OCI image's OS from Tart's cache, which tart get
// does not show: $TART_HOME/cache/OCIs/<repository>/<digest>/config.json.
func (t tartCLI) pulledOS(ref string) (string, error) {
	repo, digest, _ := strings.Cut(ref, "@")
	home := os.Getenv("TART_HOME")
	if home == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		home = filepath.Join(h, ".tart")
	}
	if strings.Contains(repo, "..") || strings.ContainsAny(digest, "/.") {
		return "", fmt.Errorf("image %q is not an OCI reference", ref)
	}
	b, err := os.ReadFile(filepath.Join(home, "cache", "OCIs", filepath.FromSlash(repo), digest, "config.json"))
	if err != nil {
		return "", fmt.Errorf("image %s is not pulled here (tart pull %s)", ref, ref)
	}
	var c struct {
		OS string `json:"os"`
	}
	if json.Unmarshal(b, &c) != nil || c.OS == "" {
		return "", fmt.Errorf("image %s: no OS in its config", ref)
	}
	return c.OS, nil
}

// keptDeadman is deadman whose guard powers the VM off and keeps it (persist.go).
const keptDeadman = `vm=$1; bin=$2; shift 2; exec 3<&0
"$bin" "$@" 3<&- & p=$!
( cat <&3; kill -KILL $p ) >/dev/null 2>&1 & g=$!; exec 3<&-
wait $p; s=$?; kill $g 2>/dev/null; exit $s`

// CloneKept: a Tart VM is its own disk; a kept one is cloned the same way.
func (t tartCLI) CloneKept(image, vm string, cpu, memory int) error {
	return t.Clone(image, vm, cpu, memory)
}

func (t tartCLI) BootKept(vm string, isolated bool, log io.Writer) (*exec.Cmd, io.WriteCloser, error) {
	return t.boot(vm, isolated, log, keptDeadman)
}

func (t tartCLI) PowerOff(vm string, runPID int) { t.powerOff(vm, runPID) }

// KeepsRoot: the whole VM disk stays.
func (tartCLI) KeepsRoot() bool { return true }

// DiskDir is Tart's home, where its VMs' disks live ($TART_HOME, else ~/.tart).
func (t tartCLI) DiskDir() string {
	if h := os.Getenv("TART_HOME"); h != "" {
		return h
	}
	h, _ := os.UserHomeDir()
	return filepath.Join(h, ".tart")
}
