package vmhost

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const hostCleanupTimeout = 6 * time.Second

var hostCgroupRoot = "/sys/fs/cgroup"

// ownedHostParent has no process or policy of its own: it is a native systemd
// slice for this exact state directory/run generation. No shared slice is changed.
type ownedHostParent struct {
	name, path string
	budget     hostBudget
}

func hostParentName(state, vm string, starts int) string {
	sum := sha256.Sum256([]byte(state + "\x00" + vm + "\x00" + strconv.Itoa(starts)))
	return fmt.Sprintf("potluck-%x.slice", sum)
}

// boundedHostCommand also bounds child processes such as a stuck sudo/busctl.
func boundedHostCommand(wait time.Duration, name string, args ...string) ([]byte, error) {
	return boundedHostCommandEnv(wait, nil, name, args...)
}
func boundedHostCommandEnv(wait time.Duration, env []string, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = time.Second
	output := &hostCommandOutput{}
	cmd.Stdout = output
	cmd.Stderr = output
	err := cmd.Run()
	b := output.Bytes()
	if output.overflow {
		return nil, fmt.Errorf("%s: native command output exceeded 4MiB", name)
	}
	if err != nil {
		return b, fmt.Errorf("%s: %w: %s", name, err, strings.TrimSpace(string(b)))
	}
	return b, nil
}

// The native parent's root calls (here, in stop and in ociHostDeadman) run
// sudo -n -k: a ticket cached at the owner's terminal is ignored, so share's
// conformance run, which starts from that terminal, gets what the daemon's
// driver gets (no ticket), not a pass the daemon cannot repeat.
func nativeManager(method string, args ...string) ([]byte, error) {
	a := []string{"-n", "-k", "busctl", "call", "org.freedesktop.systemd1", "/org/freedesktop/systemd1", "org.freedesktop.systemd1.Manager", method}
	return boundedHostCommand(2*time.Second, "sudo", append(a, args...)...)
}

// ErrOCIHostNeedsSudo is a native parent root call (or, on a Docker that hides
// its runtime options, the journal read in oci_engine_active.go) that sudo -n
// -k refused: the lending user has no passwordless sudo for it. Potluck's root
// step does not grant it (sh as root is all of root), so no root step fixes it.
var ErrOCIHostNeedsSudo = errors.New("Kata boxes need passwordless sudo for busctl and sh (and journalctl where Docker hides its runtime options)")

// hostRootChecks are harmless forms of the native parent's root calls: each
// runs the program its call runs, as root, and changes nothing.
var hostRootChecks = [][]string{{"busctl", "--version"}, {"sh", "-c", ":"}}

// verifyHostRoot runs hostRootChecks as the native parent runs its calls
// (sudo -n -k): an error wrapping ErrOCIHostNeedsSudo when sudo refused one,
// so doctor and the driver say so before a box, not at its start.
func verifyHostRoot() error {
	for _, c := range hostRootChecks {
		if _, err := sudoNK(c...); err != nil {
			return err
		}
	}
	return nil
}

// sudoNK runs a command with sudo -n -k, sudo's words read in C, and returns
// its output. Only sudo's refusal is ever in the error: the journal read's
// output is Docker's configuration.
func sudoNK(args ...string) ([]byte, error) {
	b, err := boundedHostCommandEnv(2*time.Second, append(os.Environ(), "LC_ALL=C"), "sudo", append([]string{"-n", "-k"}, args...)...)
	if err == nil {
		return b, nil
	}
	var ec exitCoder
	if words := sudoRefusal(b); errors.As(err, &ec) && ec.ExitCode() == 1 && words != "" {
		return nil, fmt.Errorf("%w: sudo -n -k %s: %s", ErrOCIHostNeedsSudo, args[0], words)
	}
	return nil, fmt.Errorf("sudo -n -k %s failed; its output withheld", args[0])
}

// sudoRefusal is sudo's own refusal when it is the first line of a command's
// output (sudo refused before the command ran), else "".
func sudoRefusal(b []byte) string {
	l, _, _ := strings.Cut(strings.TrimSpace(string(b)), "\n")
	if strings.HasPrefix(l, "sudo: ") || strings.HasPrefix(l, "sudo-rs: ") || strings.Contains(l, " is not allowed to execute ") {
		return strings.TrimSpace(l)
	}
	return ""
}

func makeHostParent(name string, budget hostBudget) (*ownedHostParent, error) {
	if len(name) != len("potluck-")+64+len(".slice") || !strings.HasPrefix(name, "potluck-") || !strings.HasSuffix(name, ".slice") {
		return nil, fmt.Errorf("invalid owned native parent")
	}
	for _, c := range strings.TrimSuffix(strings.TrimPrefix(name, "potluck-"), ".slice") {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return nil, fmt.Errorf("invalid owned native parent")
		}
	}
	if budget.CPU <= 0 || budget.MemoryMiB <= 0 {
		return nil, fmt.Errorf("invalid native parent budget")
	}
	// fail, not replace: a collision is never permission to change another unit.
	_, err := nativeManager("StartTransientUnit", "ssa(sv)a(sa(sv))", name, "fail", "3",
		"CPUQuotaPerSecUSec", "t", strconv.FormatInt(int64(budget.CPU)*1_000_000, 10),
		"MemoryMax", "t", strconv.FormatInt(int64(budget.MemoryMiB)*1024*1024, 10),
		"Description", "s", "Potluck owned sandbox "+name, "0")
	if err != nil {
		return nil, err
	}
	p := &ownedHostParent{name: name, path: filepath.Join(hostCgroupRoot, "potluck.slice", name), budget: budget}
	// StartTransientUnit returns a job path; realization may finish after the reply.
	var verifyErr error
	for until := time.Now().Add(2 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		verifyErr = p.verify()
		if verifyErr == nil {
			return p, nil
		}
		if time.Now().After(until) {
			break
		}
	}
	_ = p.stop()
	return nil, verifyErr
}

func (p *ownedHostParent) verify() error {
	mem, err := os.ReadFile(filepath.Join(p.path, "memory.max"))
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(mem)) != strconv.FormatInt(int64(p.budget.MemoryMiB)*1024*1024, 10) {
		return fmt.Errorf("native parent memory limit does not match admitted host budget")
	}
	cpu, err := os.ReadFile(filepath.Join(p.path, "cpu.max"))
	if err != nil {
		return err
	}
	f := strings.Fields(string(cpu))
	if len(f) != 2 || f[0] == "max" {
		return fmt.Errorf("native parent has no finite CPU quota")
	}
	quota, e1 := strconv.ParseInt(f[0], 10, 64)
	period, e2 := strconv.ParseInt(f[1], 10, 64)
	if e1 != nil || e2 != nil || period <= 0 || quota != period*int64(p.budget.CPU) {
		return fmt.Errorf("native parent CPU limit does not match admitted host budget")
	}
	return nil
}

func (p *ownedHostParent) empty() bool {
	b, err := os.ReadFile(filepath.Join(p.path, "cgroup.events"))
	if os.IsNotExist(err) {
		return true
	}
	return err == nil && strings.Contains("\n"+string(b), "\npopulated 0\n")
}

func (p *ownedHostParent) stop() error {
	// cgroup.kill covers descendants even if Docker's attach/remove API hangs.
	_, _ = boundedHostCommand(2*time.Second, "sudo", "-n", "-k", "sh", "-c", `test ! -e "$1" || printf 1 > "$1/cgroup.kill"`, "sh", p.path)
	_, err := nativeManager("StopUnit", "ss", p.name, "replace")
	// A failed Docker command cannot hold this native stop behind it.
	for until := time.Now().Add(hostCleanupTimeout); time.Now().Before(until); time.Sleep(25 * time.Millisecond) {
		if p.empty() {
			return nil
		}
	}
	if err != nil {
		return err
	}
	return fmt.Errorf("owned native parent %s did not empty", p.name)
}

type hostCommandOutput struct {
	buffer   bytes.Buffer
	overflow bool
}

func (b *hostCommandOutput) Write(p []byte) (int, error) {
	n := len(p)
	room := (4 << 20) - b.buffer.Len()
	if len(p) > room {
		b.overflow = true
		p = p[:room]
	}
	_, _ = b.buffer.Write(p)
	return n, nil
}

func (b *hostCommandOutput) Bytes() []byte { return b.buffer.Bytes() }
