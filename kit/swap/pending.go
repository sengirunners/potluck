package swap

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// FilePending is an upgrade past its commit (zm-4b8g.1): written under
// swap.lock before the first file moves, removed when the swap ends
// verified, rolled back or rolled forward (or put everything back). Found
// with swap.lock free, its swapper died mid-swap (SIGKILL, a service
// manager's stop timeout, the OOM killer): the build it placed may run,
// never verified.
const FilePending = "swap-pending.json"

// Unfinished is the upgrade FilePending describes.
type Unfinished struct {
	What string    `json:"what"`
	From string    `json:"from,omitempty"` // the build that ran before
	To   string    `json:"to"`             // the staged build (its preflight's build)
	PID  int       `json:"pid"`            // the swapper's
	At   time.Time `json:"at"`
}

// Version is To's version (a build reads "version (commit)").
func (p Unfinished) Version() string {
	v, _, _ := strings.Cut(p.To, " (")
	return v
}

// WritePending records p in home durably: it must outlive a kill.
func WritePending(home string, p Unfinished) error {
	b, err := json.Marshal(p)
	if err != nil {
		return err
	}
	path := filepath.Join(home, FilePending)
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(b)
	if err == nil {
		err = f.Sync()
	}
	if e := f.Close(); err == nil {
		err = e
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		os.Remove(tmp)
	}
	return err
}

// ReadPending is home's pending upgrade, if one is recorded. An unreadable
// one is an error: it is never taken as no swap.
func ReadPending(home string) (Unfinished, bool, error) {
	var p Unfinished
	b, err := os.ReadFile(filepath.Join(home, FilePending))
	if errors.Is(err, os.ErrNotExist) {
		return p, false, nil
	}
	if err != nil {
		return p, false, err
	}
	if err := json.Unmarshal(b, &p); err != nil {
		return p, false, fmt.Errorf("%s: %w", FilePending, err)
	}
	return p, true, nil
}

// ClearPending removes home's pending upgrade.
func ClearPending(home string) error {
	err := os.Remove(filepath.Join(home, FilePending))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
