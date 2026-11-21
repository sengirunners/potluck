package vmhost

import (
	"os"
	"path/filepath"
	"strconv"
	"syscall"
)

// cloudhvChildAttr makes the hypervisor die with its monitor: no VM outlives
// the process that would nap, wake and delete it.
func cloudhvChildAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
}

// cloudhvPidOf says whether pid is a process of the VM in dir: the monitor
// and its hypervisor both run with that directory as their own.
func cloudhvPidOf(pid int, dir string) bool {
	if pid <= 1 {
		return false
	}
	cwd, err := os.Readlink("/proc/" + strconv.Itoa(pid) + "/cwd")
	if err != nil {
		return false
	}
	want, err := filepath.EvalSymlinks(dir)
	return err == nil && cwd == want
}
