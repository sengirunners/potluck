//go:build linux

package vmhost

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func localDockerPeer(c net.Conn) (dockerEngineOwner, error) {
	var owner dockerEngineOwner
	u, ok := c.(*net.UnixConn)
	if !ok {
		return owner, fmt.Errorf("Docker connection is not a local Unix socket")
	}
	raw, err := u.SyscallConn()
	if err != nil {
		return owner, err
	}
	var cred *syscall.Ucred
	var peerErr error
	if err := raw.Control(func(fd uintptr) {
		cred, peerErr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	}); err != nil {
		return owner, err
	}
	if peerErr != nil {
		return owner, peerErr
	}
	if cred == nil || cred.Uid != 0 || cred.Pid <= 0 {
		return owner, fmt.Errorf("Docker socket peer is not a local root engine")
	}
	return verifyDockerSocketOwner(u.RemoteAddr().String(), int(cred.Pid), cred.Uid)
}

func verifyDockerSocketOwner(socket string, peerPID int, peerUID uint32) (dockerEngineOwner, error) {
	var owner dockerEngineOwner
	if peerUID != 0 || peerPID <= 0 {
		return owner, fmt.Errorf("Docker socket peer is not a local root engine")
	}
	b, err := boundedHostCommand(2*time.Second, "systemctl", "show", "docker.service", "-p", "MainPID", "-p", "InvocationID", "-p", "ExecMainStartTimestampMonotonic")
	if err != nil {
		return owner, err
	}
	props := systemdProperties(string(b))
	owner.pid, err = strconv.Atoi(props["MainPID"])
	if err != nil || owner.pid <= 1 {
		return owner, fmt.Errorf("local Docker service has no active main PID")
	}
	owner.started, err = strconv.ParseUint(props["ExecMainStartTimestampMonotonic"], 10, 64)
	if err != nil || owner.started == 0 {
		return owner, fmt.Errorf("local Docker service start identity is missing")
	}
	owner.invocation = props["InvocationID"]
	if len(owner.invocation) != 32 {
		return owner, fmt.Errorf("local Docker invocation identity is missing")
	}
	boot, err := os.ReadFile(filepath.Join(dockerProcRoot, "sys/kernel/random/boot_id"))
	if err != nil {
		return owner, err
	}
	owner.boot = strings.ReplaceAll(strings.TrimSpace(string(boot)), "-", "")
	socket, err = filepath.EvalSymlinks(socket)
	if err != nil {
		return owner, err
	}
	if peerPID != owner.pid {
		if peerPID != 1 {
			return owner, fmt.Errorf("Docker socket is owned by a forwarding process")
		}
		b, err = boundedHostCommand(2*time.Second, "systemctl", "show", "docker.socket", "-p", "Listen", "-p", "Triggers", "-p", "ActiveState")
		if err != nil {
			return owner, err
		}
		p := systemdProperties(string(b))
		if p["ActiveState"] != "active" || p["Triggers"] != "docker.service" || p["Listen"] != socket+" (Stream)" {
			return owner, fmt.Errorf("Docker socket activation does not belong to this local service")
		}
	}
	out, err := readDockerOwner()
	if err != nil {
		return owner, fmt.Errorf("cannot establish local dockerd/socket namespace ownership: %w", err)
	}
	if !dockerOwnerHolds(out, owner.pid, socket) {
		return owner, fmt.Errorf("cannot establish local dockerd/socket namespace ownership: docker.service's dockerd does not hold %s", socket)
	}
	return owner, nil
}
