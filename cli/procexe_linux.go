package cli

import (
	"os"
	"strconv"
)

// processExe is the program pid runs; " (deleted)" ends it when that file
// is gone (oldDaemonBin then falls back).
func processExe(pid int) (string, error) {
	return os.Readlink("/proc/" + strconv.Itoa(pid) + "/exe")
}
