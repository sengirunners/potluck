package tailnet

import (
	"fmt"
	"log"
	"strings"
	"sync"
	"time"
)

// loginLinePrefix starts the line tsnet logs every 5 s, with the login URL,
// for as long as a node waits for a login (tsnet.Server.printAuthURLLoop).
const loginLinePrefix = "To start this tsnet server"

// loginReminders are the gaps before tsnet's login line is logged again with
// the same URL: the first gap, then the second, then the last one repeated.
var loginReminders = []time.Duration{time.Minute, 10 * time.Minute, time.Hour}

// quietLogin wraps logf so tsnet's login line reaches it once per distinct
// line (a new URL logs at once), then again only after each gap in
// loginReminders, instead of every 5 s. Every other line goes through as it
// is. A nil logf means log.Printf, tsnet's own default, quieted the same way.
func quietLogin(logf func(string, ...any), now func() time.Time) func(string, ...any) {
	if logf == nil {
		logf = log.Printf
	}
	var (
		mu   sync.Mutex
		last string    // the login line last logged
		next time.Time // when last may be logged again
		step int       // index into loginReminders of the gap that ends at next
	)
	return func(format string, args ...any) {
		line := fmt.Sprintf(format, args...)
		if !strings.HasPrefix(line, loginLinePrefix) {
			logf(format, args...)
			return
		}
		mu.Lock()
		t := now()
		switch {
		case line != last:
			last, step = line, 0
		case t.Before(next):
			mu.Unlock()
			return
		default:
			step = min(step+1, len(loginReminders)-1)
		}
		next = t.Add(loginReminders[step])
		mu.Unlock()
		logf(format, args...)
	}
}
