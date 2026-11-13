package cli

import (
	"bytes"
	"errors"

	"golang.org/x/sys/unix"
)

// processExe is the program pid runs (kern.procargs2: argc, then the
// executable's path).
func processExe(pid int) (string, error) {
	b, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil {
		return "", err
	}
	if len(b) < 4 {
		return "", errors.New("kern.procargs2: short")
	}
	b = b[4:]
	if i := bytes.IndexByte(b, 0); i > 0 {
		return string(b[:i]), nil
	}
	return "", errors.New("kern.procargs2: no path")
}
