package vmhost

import (
	"fmt"
	"os"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

const ociGuestTun = "/dev/net/tun"

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
