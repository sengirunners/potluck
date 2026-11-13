//go:build !linux

package runtarget

import "os/exec"

// execVerified runs v by its path, after checking once more that the path is
// still the file hashed (same dev, inode, size, mtime and ctime). macOS has
// no exec through a descriptor (fexecve), so a window remains between that
// check and the kernel opening the path in the child: microseconds, and only
// for someone who can write the driver's directory, which safePath allows
// only to the daemon's user and root.
func execVerified(cmd *exec.Cmd, v *verified) error {
	cmd.Args[0] = v.path
	cmd.Path = v.path
	return v.recheck()
}
