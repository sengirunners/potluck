package vmhost

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// ociHostLease belongs to one existing supervisor. The admission controller
// remains unchanged; neither it nor the OCI workload adds this budget again.
type ociHostLease struct {
	profile *ociHostProfile
	budget  hostBudget
	parent  *ownedHostParent
	mu      sync.Mutex
	expires time.Time
	pipe    *hostGuardPipe
}

func hostLeaseFor(c config) (*ociHostLease, error) {
	if c.VM != "oci" {
		return nil, nil
	}
	id, err := inspectDockerRuntime(c.OCICLI, c.OCIRuntime)
	if err != nil {
		return nil, err
	}
	if !id.kata && c.OCIHostProfile == "" {
		return nil, nil
	}
	if c.OCIHostProfile == "" {
		return nil, fmt.Errorf("Kata requires oci_host_profile: its workload limits do not bound the host VMM")
	}
	if filepath.Base(c.OCICLI) != "docker" || c.AdmissionURL == "" || c.Warm > 0 {
		return nil, fmt.Errorf("oci_host_profile requires Docker, admission, and no unleased warm boxes")
	}
	p, err := loadOCIHostProfile(c.OCIHostProfile, c.OCIRuntime)
	if err != nil {
		return nil, err
	}
	b, err := p.budget(c.CPU, c.Memory)
	if err != nil {
		return nil, err
	}
	for _, bin := range []string{"bash", "timeout", "sudo", "busctl"} {
		if _, err := exec.LookPath(bin); err != nil {
			return nil, err
		}
	}
	if _, err := boundedHostCommand(2*time.Second, "bash", "-c", `(( BASH_VERSINFO[0] >= 4 ))`); err != nil {
		return nil, fmt.Errorf("bounded OCI host guard requires Bash 4 or newer for fractional read deadlines")
	}
	return &ociHostLease{profile: p, budget: b}, nil
}

func (h *ociHostLease) confirm(exp time.Time) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, err := hostDeadline(exp, time.Duration(h.profile.CleanupReserveS)*time.Second); err != nil {
		return err
	}
	if h.pipe != nil {
		if err := h.pipe.confirm(exp); err != nil {
			return err
		}
	}
	h.expires = exp
	return nil
}

func (o ociCLI) bootHost(vm string, isolated bool, log io.Writer) (*exec.Cmd, io.WriteCloser, error) {
	if isolated && !ociIsolatedNet(o.network) {
		return nil, nil, fmt.Errorf("unfiltered OCI network")
	}
	h := o.host
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, err := h.profile.verifyRuntime(); err != nil {
		return nil, nil, err
	}
	reserve := time.Duration(h.profile.CleanupReserveS) * time.Second
	d, err := hostDeadline(h.expires, reserve)
	if err != nil {
		return nil, nil, err
	}
	if err := h.parent.verify(); err != nil {
		return nil, nil, err
	}
	cmd := exec.Command("bash", "-c", ociHostDeadman, "potluck-host-guard", vm, o.bin, h.parent.name, h.parent.path, fmt.Sprint(d), hostCgroupRoot)
	cmd.Env = dockerEngineEnv(o.engineHost)
	cmd.Stdout, cmd.Stderr = log, log
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	p, err := cmd.StdinPipe()
	if err != nil {
		return nil, nil, err
	}
	if err = cmd.Start(); err != nil {
		_ = p.Close()
		return nil, nil, err
	}
	h.pipe = &hostGuardPipe{WriteCloser: p, reserve: reserve}
	return cmd, h.pipe, nil
}

func (o ociCLI) destroyHost(vm string, runPID int) {
	// Parent kill is independent of a stuck Docker CLI. Empty readback, rather
	// than Docker's exit alone, is the release predicate in releaseRoom.
	if err := o.host.parent.stop(); err != nil {
		fmt.Fprintln(os.Stderr, err)
	}
	_, _ = o.boundedCommand(2*time.Second, "rm", "-f", vm)
	if runPID > 0 && o.host.parent.empty() {
		// Only a guard whose exact argv still names this VM may be signalled.
		if b, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", runPID)); err == nil && strings.Contains(string(b), "\x00"+vm+"\x00") {
			_ = syscall.Kill(-runPID, syscall.SIGKILL)
		}
	}
	if o.stateDir != "" && filepath.Base(vm) == vm {
		_ = os.Remove(o.resolvPath(vm))
	}
}
