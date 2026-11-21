//go:build !linux

package vmhost

import "syscall"

// The layer runs on Linux (KVM). Elsewhere this builds for the tests, whose
// hypervisor is a fake: nothing ties it to its monitor, and a pid is taken
// for the VM's while the VM's monitor holds its lock.
func cloudhvChildAttr() *syscall.SysProcAttr { return &syscall.SysProcAttr{} }

func cloudhvPidOf(pid int, dir string) bool {
	return pid > 1 && syscall.Kill(pid, 0) == nil && cloudhvMonAlive(dir)
}
