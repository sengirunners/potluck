package refdriver

import (
	"fmt"
	"time"

	"golang.org/x/sys/unix"
)

func observeSupervisorExit(pid int) (supervisorExit, error) {
	fd, err := unix.PidfdOpen(pid, 0)
	if err != nil {
		return supervisorExit{}, err
	}
	return supervisorExit{
		close: func() { _ = unix.Close(fd) },
		wait: func() error {
			events := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
			deadline := time.Now().Add(5 * time.Second)
			var n int
			var err error
			for {
				left := time.Until(deadline)
				if left <= 0 {
					return fmt.Errorf("no exit event within 5s")
				}
				n, err = unix.Poll(events, int((left+time.Millisecond-1)/time.Millisecond))
				if err != unix.EINTR {
					break
				}
			}
			if err != nil {
				return err
			}
			if n != 1 || events[0].Revents&(unix.POLLIN|unix.POLLHUP) == 0 {
				return fmt.Errorf("no exit event within 5s (events=%d)", events[0].Revents)
			}
			return nil
		},
	}, nil
}
