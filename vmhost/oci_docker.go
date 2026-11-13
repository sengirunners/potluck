package vmhost

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

const ociGuestTun = "/dev/net/tun"

// ociKataDockerProc unmasks a Kata Docker box's guest /proc and /sys. Nested
// containers need a fresh procfs, and the kernel refuses one inside
// RootlessKit's user namespace while any masked or read-only path covers the
// box's /proc (locked mounts: mnt_too_revealing). dockerd also sets its
// bridge's ip_forward through /proc/sys. Root in the box can then reconfigure
// its own guest kernel, so the box's network fence must not rely on that guest:
// the host's per-port anti-spoofing guard on Potluck's bridge has to
// be active before such a box is made (ociGuardReady). The VM boundary, the
// host-side egress filter and the host-side cgroup ceilings stay outside it.
// Never applied under a host-kernel runtime (gVisor, runc).
var ociKataDockerProc = []string{"--security-opt", "systempaths=unconfined"}

// ociBoxGuards are the units that keep each Potluck network's per-port
// anti-spoofing guard (hostprobe boxguard.go, installed by potluck host share
// --docker on Kata), by network, as ociBoxNets names their filters.
var ociBoxGuards = map[string]string{
	"potluck-box": "potluck-box-guard.service",
	"zm-box":      "zm-box-guard.service",
}

// ociGuardReady says whether a Kata Docker box may be made on network:
// with no network it reaches no neighbour; on a Potluck network only while
// its guard unit is active (a guard that failed fails closed); on any other
// network never, since no guard of Potluck's watches it. Tests swap it.
var ociGuardReady = func(network string) error {
	if network == "none" {
		return nil
	}
	unit, own := ociBoxGuards[network]
	if !own {
		return fmt.Errorf("a Kata box with docker controls its own guest kernel, so it needs a Potluck network with its anti-spoofing guard, or no network; oci_network %s has no such guard", network)
	}
	out, _ := exec.Command("systemctl", "is-active", unit).Output()
	if s := strings.TrimSpace(string(out)); s != "active" {
		return fmt.Errorf("a Kata box with docker controls its own guest kernel, so it starts on %s only while %s keeps that bridge's per-port anti-spoofing; the unit is %q (potluck host share --docker installs it; journalctl -u %s says why it stopped)", network, unit, s, unit)
	}
	return nil
}

// guardBoot refuses to start a Kata container whose guest /proc is unmasked
// unless every network it is on may hold such a box now (ociGuardReady). It
// reads the container, not the binding: a kept box made on a network starts
// there again whatever a later request says, and a guard that has since
// failed must stop it as it stops a new one.
func (o ociCLI) guardBoot(vm string) error {
	if !ociKataRuntime(o.runtime) {
		return nil
	}
	out, err := o.run("container", "inspect", "--format", "{{json .HostConfig.SecurityOpt}} {{range $k, $v := .NetworkSettings.Networks}}{{$k}} {{end}}", vm)
	if err != nil {
		return err
	}
	opts, nets, _ := strings.Cut(strings.TrimSpace(out), " ")
	if !strings.Contains(opts, ociKataDockerProc[1]) {
		return nil
	}
	for _, n := range strings.Fields(nets) {
		if err := ociGuardReady(n); err != nil {
			return err
		}
	}
	return nil
}

var ociTunLstat = os.Lstat

// Docker turns this fixed host node's metadata into an OCI LinuxDevice.
// Verified Kata ignores non-VFIO character devices for host hotplug; its
// guest agent creates char 10:200 in the VM before dropping workload caps.
// This must never be requested under a host-kernel container runtime.
func checkKataTun() error {
	fi, err := ociTunLstat(ociGuestTun)
	if err != nil {
		return fmt.Errorf("Kata Docker needs %s character device 10:200: %w", ociGuestTun, err)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if fi.Mode()&os.ModeType != os.ModeDevice|os.ModeCharDevice || !ok || st == nil ||
		unix.Major(uint64(st.Rdev)) != 10 || unix.Minor(uint64(st.Rdev)) != 200 {
		return fmt.Errorf("Kata Docker needs %s to be a non-symlink character device 10:200", ociGuestTun)
	}
	return nil
}

// ociKataRuntime names the native guest-kernel runtimes accepted for sudo.
func ociKataRuntime(rt string) bool {
	return strings.HasPrefix(rt, "kata") || strings.Contains(rt, ".kata")
}

// dockerBoot owns the rootless daemon's boot environment. Kata's native
// kernel supports bridges and NAT inside RootlessKit's user/network namespace.
// The outer box still lacks NET_ADMIN and NET_RAW; no shared-bridge packet
// capability is granted. gVisor stays in its socket-only, no-bridge mode.
func (o ociCLI) dockerBoot(args []string, pid1 string) ([]string, string) {
	if !o.docker {
		return args, pid1
	}
	mode := "none"
	if ociKataRuntime(o.runtime) {
		mode = "bridge"
	}
	args = append(args, "--env", "DOCKER_HOST=unix://"+ociDockerSock,
		"--env", "XDG_RUNTIME_DIR="+ociDockerRun, "--env", "ZM_DOCKER_NETWORK="+mode)
	return args, "zm-dockerd >/tmp/zm-dockerd.log 2>&1 & " + pid1
}
