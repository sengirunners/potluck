package swap

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/sengirunners/potluck/kit/api"
)

// FileLock is held (flock) by whoever stops, restarts or upgrades the
// daemon; its body is the api.SwapHolder.
const FileLock = "swap.lock"

// Lock is a held swap lock.
type Lock struct {
	f      *os.File
	Holder api.SwapHolder
}

// BusyError: another swap holds the lock.
type BusyError struct{ Holder api.SwapHolder }

func (e *BusyError) Error() string {
	h := e.Holder
	if h.PID == 0 {
		return "another stop, restart or upgrade of this daemon is in progress"
	}
	return fmt.Sprintf("a %s of this daemon is in progress: %s since %s (pid %d)", h.What, h.By,
		h.Since.Local().Format("15:04:05"), h.PID)
}

// Acquire takes home's swap lock without waiting, for h (its PID, Since and
// Nonce are filled in). A held lock is a *BusyError naming its holder.
func Acquire(home string, h api.SwapHolder) (*Lock, error) {
	f, err := os.OpenFile(filepath.Join(home, FileLock), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	// A Holder probe takes a shared lock for a moment; try a little before
	// calling the lock someone else's.
	for i := 0; ; i++ {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			f.Close()
			return nil, err
		}
		if i == 10 {
			f.Close()
			held, _ := Holder(home)
			return nil, &BusyError{Holder: held}
		}
		time.Sleep(10 * time.Millisecond)
	}
	h.PID, h.Since = os.Getpid(), time.Now().UTC()
	if h.Nonce == "" {
		var b [12]byte
		_, _ = rand.Read(b[:])
		h.Nonce = hex.EncodeToString(b[:])
	}
	body, _ := json.Marshal(h)
	_ = f.Truncate(0)
	if _, err := f.WriteAt(body, 0); err != nil {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
		return nil, err
	}
	return &Lock{f: f, Holder: h}, nil
}

// Release drops the lock.
func (l *Lock) Release() {
	if l == nil || l.f == nil {
		return
	}
	_ = l.f.Truncate(0)
	_ = syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	l.f.Close()
	l.f = nil
}

// Holder reports whether home's swap lock is held, and by whom (a zero
// holder when its body is not written yet).
func Holder(home string) (api.SwapHolder, bool) {
	path := filepath.Join(home, FileLock)
	f, err := os.Open(path)
	if err != nil {
		return api.SwapHolder{}, false
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB); err == nil {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		return api.SwapHolder{}, false
	}
	var h api.SwapHolder
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &h)
	}
	return h, true
}
