package vmhost

// A VM's logs (docs/cloudhv.md, "Logs"): what the hypervisor says, what the
// guest writes to its serial console, and the monitor's record of naps and
// wakes. A guest can make each of them grow, so each is at most two files:
// the newer under its name and the one before it under name.1, neither
// past cloudhvLogMax.

import (
	"io"
	"os"
	"strings"
	"sync"
	"time"
)

// cloudhvLogMax is the most bytes one part of a log holds.
const cloudhvLogMax = 512 << 10

// The logs' names in a VM's directory.
const (
	cloudhvHypervisorLog = "hypervisor.log"
	cloudhvConsoleLog    = "console.log"
	cloudhvEventsLog     = "events.jsonl"
)

// cloudhvLogFiles are the names of the log name's two parts, the newer first.
func cloudhvLogFiles(name string) []string { return []string{name, name + ".1"} }

// cloudhvLogEnd is the last keep bytes of the log name, trimmed, for another
// log to quote.
func cloudhvLogEnd(name string, keep int64) string {
	var end []byte
	for _, part := range cloudhvLogFiles(name) {
		if keep -= int64(len(end)); keep <= 0 {
			break
		}
		f, err := os.Open(part)
		if err != nil {
			continue
		}
		if st, err := f.Stat(); err == nil && st.Size() > keep {
			_, _ = f.Seek(-keep, io.SeekEnd)
		}
		b, _ := io.ReadAll(f)
		f.Close()
		end = append(b, end...)
	}
	return strings.TrimSpace(string(end))
}

// cloudhvLog appends to the file Name in the monitor's directory. A write
// that would take the file past cloudhvLogMax first makes the file Name.1,
// in place of the one before it, and starts a new one; of one write larger
// than that, the end is kept. A write never fails: what the disk does not
// take is dropped, so the hypervisor is never told its log is broken. A file
// found under Name that is larger than a part may be is emptied.
type cloudhvLog struct {
	Name string

	mu  sync.Mutex
	f   *os.File
	n   int64 // the bytes in the file
	all int64 // the bytes this log has written, to this file and those before it
}

// Written is how many bytes the log has written since it was made.
func (l *cloudhvLog) Written() int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.all
}

func (l *cloudhvLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	said := len(p)
	if len(p) > cloudhvLogMax {
		p = p[len(p)-cloudhvLogMax:]
	}
	if l.f == nil && !l.open() {
		return said, nil
	}
	if l.n+int64(len(p)) > cloudhvLogMax {
		l.f.Close()
		l.f = nil
		if os.Rename(l.Name, cloudhvLogFiles(l.Name)[1]) != nil || !l.open() {
			return said, nil
		}
	}
	n, _ := l.f.Write(p)
	l.n += int64(n)
	l.all += int64(n)
	return said, nil
}

func (l *cloudhvLog) open() bool {
	f, err := os.OpenFile(l.Name, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return false
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return false
	}
	l.f, l.n = f, st.Size()
	if l.n > cloudhvLogMax && f.Truncate(0) == nil {
		l.n = 0
	}
	return true
}

// ReadFrom writes what r gives to the log until r ends (a command's output
// is copied through it). After a read that gave little it waits a moment
// for more: a hypervisor writes its serial port a byte at a time, and the
// log takes that a batch at a time.
func (l *cloudhvLog) ReadFrom(r io.Reader) (int64, error) {
	buf := make([]byte, 32<<10)
	var all int64
	for {
		n, err := r.Read(buf)
		all += int64(n)
		if n > 0 {
			_, _ = l.Write(buf[:n])
		}
		switch {
		case err == io.EOF:
			return all, nil
		case err != nil:
			return all, err
		case n < 4<<10:
			time.Sleep(2 * time.Millisecond)
		}
	}
}
