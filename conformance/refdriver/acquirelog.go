package refdriver

import (
	"os"
	"path/filepath"
	"strings"
)

// Every acquire the driver receives is logged by its request_id, before the
// driver decides anything about it (a refusal, a cancel, a gate), so a test
// can prove an acquire never reached the driver.

func acquireLogPath(state string) string { return filepath.Join(state, "acquires") }

// logAcquire appends key (one line: a request_id is one word) to the log.
func (d *drv) logAcquire(key string) error {
	f, err := os.OpenFile(acquireLogPath(d.state), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(key + "\n")
	return err
}

// Acquired lists, oldest first, the request_id of every acquire the driver
// keeping state received, one entry per call (a retry counts again).
func Acquired(state string) []string {
	b, err := os.ReadFile(acquireLogPath(state))
	if err != nil {
		return nil
	}
	return strings.Fields(string(b))
}
