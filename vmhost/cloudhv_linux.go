package vmhost

import (
	"errors"
	"fmt"
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

// cloudhvShm is /dev/shm when it is memory (tmpfs or ramfs).
func cloudhvShm() (string, error) {
	if err := cloudhvMemFS("/dev/shm"); err != nil {
		return "", err
	}
	return "/dev/shm", nil
}

// cloudhvMemFS is nil when dir is on a filesystem that is memory.
func cloudhvMemFS(dir string) error {
	const tmpfs, ramfs = 0x01021994, 0x858458f6
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return fmt.Errorf("%s: %v", dir, err)
	}
	if t := uint32(st.Type); t != tmpfs && t != ramfs {
		return fmt.Errorf("%s is not a memory filesystem (tmpfs or ramfs)", dir)
	}
	return nil
}

// cloudhvDataRanges calls each for every run of f that holds data, in
// order, and for none of its holes. A filesystem that cannot say where its
// holes are has the whole file given as one run.
func cloudhvDataRanges(f *os.File, size int64, each func(off, end int64) error) error {
	const seekData, seekHole = 3, 4
	for off := int64(0); off < size; {
		start, err := f.Seek(off, seekData)
		if errors.Is(err, syscall.ENXIO) {
			return nil // nothing but a hole is left
		}
		if err != nil {
			return each(off, size)
		}
		end, err := f.Seek(start, seekHole)
		if err != nil || end > size {
			end = size
		}
		if end <= start {
			return each(start, size)
		}
		if err := each(start, end); err != nil {
			return err
		}
		off = end
	}
	return nil
}
