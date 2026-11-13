package server

import (
	"net"

	"golang.org/x/sys/unix"
)

// peerCred is the uid and pid of the process at the other end of c.
func peerCred(c *net.UnixConn) (uid, pid int, err error) {
	rc, err := c.SyscallConn()
	if err != nil {
		return 0, 0, err
	}
	var cerr error
	if err := rc.Control(func(fd uintptr) {
		var x *unix.Xucred
		if x, cerr = unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED); cerr != nil {
			return
		}
		uid = int(x.Uid)
		pid, cerr = unix.GetsockoptInt(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERPID)
	}); err != nil {
		return 0, 0, err
	}
	return uid, pid, cerr
}
