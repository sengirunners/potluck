package refdriver

import (
	"errors"
	"fmt"
	"syscall"
)

// A supervisor is detached, so it cannot be joined with waitpid. Observe its
// exit before signaling it, including supervisors that exit on their own.
type supervisorExit struct {
	wait  func() error
	close func()
}

var observeSupervisor = observeSupervisorExit
var signalSupervisor = func(pid int) error { return syscall.Kill(pid, syscall.SIGKILL) }

func stopSupervisor(pid int) error {
	exit, err := observeSupervisor(pid)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("observe supervisor %d: %w", pid, err)
	}
	defer exit.close()
	if err := signalSupervisor(pid); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("kill supervisor %d: %w", pid, err)
	}
	if err := exit.wait(); err != nil {
		return fmt.Errorf("join supervisor %d: %w", pid, err)
	}
	return nil
}
