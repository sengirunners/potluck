package portstream

import (
	"sync"
	"time"
)

// An unexpired deadline keeps one cancellation channel across timer changes,
// so pending operations see shortening, extension and clearing immediately.
// Once a deadline has fired, future operations get a fresh channel when reset.
type deadline struct {
	mu      sync.Mutex
	timer   *time.Timer
	expired chan struct{}
}

func (d *deadline) wait() <-chan struct{} {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.expired == nil {
		d.expired = make(chan struct{})
	}
	return d.expired
}
func (d *deadline) set(t time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.expired == nil {
		d.expired = make(chan struct{})
	}
	if d.timer != nil && !d.timer.Stop() {
		<-d.expired
	}
	d.timer = nil
	closed := false
	select {
	case <-d.expired:
		closed = true
	default:
	}
	if t.IsZero() {
		if closed {
			d.expired = make(chan struct{})
		}
		return
	}
	if until := time.Until(t); until > 0 {
		if closed {
			d.expired = make(chan struct{})
		}
		ch := d.expired
		d.timer = time.AfterFunc(until, func() { close(ch) })
		return
	}
	if !closed {
		close(d.expired)
	}
}
