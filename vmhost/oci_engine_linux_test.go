//go:build linux

package vmhost

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Socket activation uses peer PID1; the supported ownership proof still needs
// the exact local service invocation and dockerd's listener FD. All metadata
// below is private fixture state; sudo/systemctl are fake executables.
func TestDockerSocketActivationOwnership(t *testing.T) {
	for _, mode := range []string{"activated", "direct", "wrong-fd", "wrong-namespace", "wrong-service", "forwarder"} {
		t.Run(mode, func(t *testing.T) {
			owner, _, systemd := fakeEngineEvidence(t)
			if err := os.WriteFile(systemd, []byte("MainPID=1817\nInvocationID="+owner.invocation+"\nExecMainStartTimestampMonotonic=100\n"), 0600); err != nil {
				t.Fatal(err)
			}
			proc := t.TempDir()
			old := dockerProcRoot
			dockerProcRoot = proc
			t.Cleanup(func() { dockerProcRoot = old })
			for _, dir := range []string{"1817/ns", "1817/fd", "self/ns", "net", "sys/kernel/random"} {
				_ = os.MkdirAll(filepath.Join(proc, dir), 0700)
			}
			_ = os.WriteFile(filepath.Join(proc, "sys/kernel/random/boot_id"), []byte(owner.boot), 0600)
			_ = os.WriteFile(filepath.Join(proc, "1817/status"), []byte("Uid:\t0\t0\t0\t0\n"), 0600)
			_ = os.Symlink("/usr/bin/dockerd", filepath.Join(proc, "1817/exe"))
			inode := "36356"
			if mode == "wrong-fd" {
				inode = "99999"
			}
			_ = os.Symlink("socket:["+inode+"]", filepath.Join(proc, "1817/fd/4"))
			for _, ns := range []string{"mnt", "cgroup"} {
				target := ns + "[123]"
				_ = os.Symlink(target, filepath.Join(proc, "self/ns", ns))
				if mode == "wrong-namespace" {
					target = ns + "[456]"
				}
				_ = os.Symlink(target, filepath.Join(proc, "1817/ns", ns))
			}
			socket := filepath.Join(t.TempDir(), "docker.sock")
			_ = os.WriteFile(socket, nil, 0600)
			_ = os.WriteFile(filepath.Join(proc, "net/unix"), []byte("Num RefCount Protocol Flags Type St Inode Path\n0000: 00000002 00000000 00010000 0001 01 36356 "+socket+"\n"), 0600)
			socketProps := filepath.Join(filepath.Dir(systemd), "socket")
			trigger := "docker.service"
			if mode == "wrong-service" {
				trigger = "other.service"
			}
			_ = os.WriteFile(socketProps, []byte("ActiveState=active\nTriggers="+trigger+"\nListen="+socket+" (Stream)\n"), 0600)
			t.Setenv("ENGINE_SOCKET", socketProps)
			bin := strings.Split(os.Getenv("PATH"), ":")[0]
			oldPath, oldHelper := dockerOwnerPath, dockerOwnerHelper
			dockerOwnerPath, dockerOwnerHelper = bin+":/usr/bin:/bin", filepath.Join(t.TempDir(), "no-helper")
			t.Cleanup(func() { dockerOwnerPath, dockerOwnerHelper = oldPath, oldHelper })
			_ = os.WriteFile(filepath.Join(bin, "systemctl"), []byte("#!/bin/sh\nif [ \"$2\" = docker.socket ];then cat \"$ENGINE_SOCKET\";else cat \"$ENGINE_SYSTEMD\";fi\n"), 0700)
			peer := 1
			if mode == "direct" {
				peer = 1817
			}
			if mode == "forwarder" {
				peer = 999
			}
			got, err := verifyDockerSocketOwner(socket, peer, 0)
			valid := mode == "activated" || mode == "direct"
			if valid && (err != nil || got != owner) {
				t.Fatalf("supported native socket owner: %+v %v", got, err)
			}
			if !valid && err == nil {
				t.Fatal("unowned or foreign engine accepted")
			}
		})
	}
}
