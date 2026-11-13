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
		var u *unix.Ucred
		if u, cerr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED); cerr == nil {
			uid, pid = int(u.Uid), int(u.Pid)
		}
	}); err != nil {
		return 0, 0, err
	}
	return uid, pid, cerr
}
