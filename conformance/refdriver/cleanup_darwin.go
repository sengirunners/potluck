package refdriver

import (
	"fmt"
	"time"

	"golang.org/x/sys/unix"
)

func observeSupervisorExit(pid int) (supervisorExit, error) {
	fd, err := unix.Kqueue()
	if err != nil {
		return supervisorExit{}, err
	}
	change := unix.Kevent_t{Ident: uint64(pid), Filter: unix.EVFILT_PROC,
		Flags: unix.EV_ADD | unix.EV_ENABLE | unix.EV_ONESHOT, Fflags: unix.NOTE_EXIT}
	if _, err := unix.Kevent(fd, []unix.Kevent_t{change}, nil, nil); err != nil {
		_ = unix.Close(fd)
		return supervisorExit{}, err
	}
	return supervisorExit{
		close: func() { _ = unix.Close(fd) },
		wait: func() error {
			events := make([]unix.Kevent_t, 1)
			deadline := time.Now().Add(5 * time.Second)
			var n int
			var err error
			for {
				left := time.Until(deadline)
				if left <= 0 {
					return fmt.Errorf("no exit event within 5s")
				}
				timeout := unix.NsecToTimespec(left.Nanoseconds())
				n, err = unix.Kevent(fd, nil, events, &timeout)
				if err != unix.EINTR {
					break
				}
			}
			if err != nil {
				return err
			}
			if n != 1 || events[0].Fflags&unix.NOTE_EXIT == 0 || events[0].Flags&unix.EV_ERROR != 0 {
				return fmt.Errorf("no exit event within 5s")
			}
			return nil
		},
	}, nil
}
