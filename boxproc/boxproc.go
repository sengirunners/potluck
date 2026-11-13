// Package boxproc is the host side of a box's background processes (SPEC 20
// "Boxes", background processes): the optional spawn, output and signal
// verbs. A process's output and exit live in files under the driver's state
// directory, so the one-shot driver verbs read them while a detached holder
// (one per process) writes them: each stream in 1 MiB segments at absolute
// byte offsets, the oldest deleted once more than KeepBytes are newer, and the
// exit written only after the output is complete. Starting the process and
// signalling its group are the driver's (a guest exec in vmhost, a local
// process group in refdriver); the layout, paging and limits are here so
// every driver in this repo answers alike. Owned by run targets.
package boxproc

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"
)

// SPEC 20's limits on background processes.
const (
	// KeepBytes is the least a driver keeps of each stream: the newest bytes.
	KeepBytes = 8 << 20
	// MaxBytes and DefaultBytes bound one output answer (stdout and stderr together).
	MaxBytes     = 1 << 20
	DefaultBytes = 64 << 10
	// MaxWaitS is the longest an output call waits for new bytes.
	MaxWaitS = 20
	// MaxProcs is how many processes a box remembers; spawn past it forgets
	// the oldest ended one, and refuses while all of them run.
	MaxProcs = 32
)

// segBytes is one log segment (a var: tests shrink it).
var segBytes int64 = 1 << 20

// Encoding "base64" carries bytes exactly; text (the default) replaces
// invalid UTF-8.
const Base64 = "base64"

// Refusal is a SPEC 11 error a driver passes on as its own.
type Refusal struct{ Code, Msg string }

func (e *Refusal) Error() string { return e.Code + ": " + e.Msg }

func refuse(code, format string, a ...any) error {
	return &Refusal{code, fmt.Sprintf(format, a...)}
}

var idRe = regexp.MustCompile(`^p[0-9a-f]{12}$`)

// NewID is a fresh process id.
func NewID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return "p" + hex.EncodeToString(b)
}

// Decode is stdin as sent: base64 or text.
func Decode(s *string, encoding string) ([]byte, error) {
	switch {
	case encoding != "" && encoding != Base64:
		return nil, refuse("bad_request", "encoding is base64 or absent")
	case s == nil:
		return nil, nil
	case encoding == Base64:
		b, err := base64.StdEncoding.DecodeString(*s)
		if err != nil {
			return nil, refuse("bad_request", "stdin is not base64")
		}
		return b, nil
	}
	return []byte(*s), nil
}

// Encode is output as answered: base64, or text with invalid UTF-8 replaced.
func Encode(b []byte, encoding string) string {
	if encoding == Base64 {
		return base64.StdEncoding.EncodeToString(b)
	}
	return strings.ToValidUTF8(string(b), "�")
}

// Spec is what spawn hands a process's holder on its stdin, never on disk
// (secrets may be in it).
type Spec struct {
	Argv     []string          `json:"argv"`
	Stdin    []byte            `json:"stdin,omitempty"`
	Cwd      string            `json:"cwd,omitempty"`
	TimeoutS int               `json:"timeout_s,omitempty"` // 0: until the box ends
	Secrets  map[string]string `json:"secrets,omitempty"`
	PTY      *PTY              `json:"pty,omitempty"` // on a terminal (pty.go)
}

// Meta is a process's record.
type Meta struct {
	RequestID string    `json:"request_id"`
	Holder    int       `json:"holder,omitempty"` // the host process writing its output
	Group     int       `json:"group,omitempty"`  // its process group, where the driver can see it
	PTY       bool      `json:"pty,omitempty"`    // it runs on a terminal: input takes it (pty.go)
	Start     time.Time `json:"start"`
}

// Procs is one box's processes: a directory of process directories.
type Procs string

// Dir is one process's directory.
func (p Procs) Dir(pid string) (Dir, error) {
	if !idRe.MatchString(pid) {
		return "", refuse("not_found", "no process %q in this box", pid)
	}
	d := Dir(filepath.Join(string(p), pid))
	if _, err := d.Meta(); err != nil {
		return "", refuse("not_found", "no process %s in this box", pid)
	}
	return d, nil
}

func (p Procs) all() []Dir {
	es, _ := os.ReadDir(string(p))
	var out []Dir
	for _, e := range es {
		if e.IsDir() && idRe.MatchString(e.Name()) {
			out = append(out, Dir(filepath.Join(string(p), e.Name())))
		}
	}
	return out
}

// Find is the process spawned with requestID, if the box remembers one.
func (p Procs) Find(requestID string) (string, bool) {
	for _, d := range p.all() {
		if m, err := d.Meta(); err == nil && m.RequestID == requestID {
			return d.ID(), true
		}
	}
	return "", false
}

// New makes a process's directory for requestID. Past MaxProcs it forgets
// the oldest ended process; with MaxProcs running it is conflict. The caller
// holds its state lock, so two spawns never race on one key.
func (p Procs) New(requestID string) (Dir, error) {
	ds := p.all()
	if len(ds) >= MaxProcs {
		var ended []Dir
		var at []time.Time
		for _, d := range ds {
			if !d.Running() {
				m, _ := d.Meta()
				ended, at = append(ended, d), append(at, m.Start)
			}
		}
		if len(ended) == 0 {
			return "", refuse("conflict", "%d processes are running in this box", MaxProcs)
		}
		i := slices.Index(at, slices.MinFunc(at, func(a, b time.Time) int { return a.Compare(b) }))
		_ = os.RemoveAll(string(ended[i]))
	}
	d := Dir(filepath.Join(string(p), NewID()))
	if err := os.MkdirAll(string(d), 0o700); err != nil {
		return "", err
	}
	return d, d.SetMeta(Meta{RequestID: requestID, Start: time.Now().UTC()})
}

// Running counts the box's processes still going.
func (p Procs) Running() []Dir {
	var out []Dir
	for _, d := range p.all() {
		if d.Running() {
			out = append(out, d)
		}
	}
	return out
}

// Dir is one process's directory: meta.json, exit.json, stdout.<offset>,
// stderr.<offset>.
type Dir string

// ID is the process's id (its pid on the wire).
func (d Dir) ID() string { return filepath.Base(string(d)) }

// Meta reads the record.
func (d Dir) Meta() (Meta, error) {
	var m Meta
	b, err := os.ReadFile(filepath.Join(string(d), "meta.json"))
	if err == nil {
		err = json.Unmarshal(b, &m)
	}
	return m, err
}

// SetMeta writes the record (atomically).
func (d Dir) SetMeta(m Meta) error { return writeJSON(filepath.Join(string(d), "meta.json"), m) }

// Exit is the process's exit code once its holder recorded it.
func (d Dir) Exit() *int {
	var x struct {
		Exit int `json:"exit"`
	}
	b, err := os.ReadFile(filepath.Join(string(d), "exit.json"))
	if err != nil || json.Unmarshal(b, &x) != nil {
		return nil
	}
	return &x.Exit
}

// Running reports whether the process may still write: no exit yet, and its
// holder alive (or not yet started).
func (d Dir) Running() bool {
	return d.Exit() == nil && d.holderUp()
}

// holderUp is Running without the exit file: the holder is alive or not yet started.
func (d Dir) holderUp() bool {
	m, err := d.Meta()
	return err == nil && (m.Holder == 0 && time.Since(m.Start) < 30*time.Second || m.Holder > 0 && syscall.Kill(m.Holder, 0) == nil)
}

func writeJSON(path string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	// A temporary name of its own: two writers of one file (a spawner and its
	// holder both write meta.json) must not share one, or the second rename
	// finds the first's file gone.
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	_, err = f.Write(b)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(f.Name(), path)
	}
	if err != nil {
		_ = os.Remove(f.Name())
	}
	return err
}

// Hold is a holder's whole life: start runs the process with its output to
// the two writers and answers a wait for its exit code; Hold records the exit
// once every byte is written. A start that fails records exit 127.
func Hold(d Dir, start func(stdout, stderr io.Writer) (wait func() int, err error)) int {
	out, eout := openLog(d, "stdout")
	errw, eerr := openLog(d, "stderr")
	code := 127
	if err := errors.Join(eout, eerr); err == nil {
		wait, err := start(out, errw)
		if err == nil {
			code = wait()
		} else {
			fmt.Fprintln(errw, "zm: the process did not start:", err)
		}
	}
	out.close()
	errw.close()
	if err := writeJSON(filepath.Join(string(d), "exit.json"), map[string]int{"exit": code}); err != nil {
		return 1
	}
	return 0
}

// log is one stream's writer: segments of segBytes, the oldest deleted once
// KeepBytes are newer. Write never fails the process: a full disk loses
// output, not the command.
type log struct {
	d      Dir
	stream string
	f      *os.File
	start  int64 // the current segment's offset
	n      int64 // bytes in it
}

func openLog(d Dir, stream string) (*log, error) {
	l := &log{d: d, stream: stream}
	return l, l.next()
}

func (l *log) seg(off int64) string {
	return filepath.Join(string(l.d), fmt.Sprintf("%s.%016d", l.stream, off))
}

func (l *log) next() error {
	if l.f != nil {
		l.f.Close()
	}
	f, err := os.OpenFile(l.seg(l.start), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	l.f = f
	if err == nil {
		for _, s := range segments(l.d, l.stream) {
			if s.off+segBytes <= l.start-KeepBytes { // every byte of it is past the newest KeepBytes
				_ = os.Remove(s.path)
			}
		}
	}
	return err
}

func (l *log) Write(p []byte) (int, error) {
	n := len(p)
	for len(p) > 0 && l.f != nil {
		k := min(int64(len(p)), segBytes-l.n)
		w, _ := l.f.Write(p[:k])
		l.n += int64(w)
		p = p[k:]
		if l.n >= segBytes {
			l.start += l.n
			l.n = 0
			if l.next() != nil {
				l.f = nil
			}
		}
	}
	return n, nil
}

func (l *log) close() {
	if l != nil && l.f != nil {
		l.f.Close()
	}
}

type segment struct {
	off  int64
	path string
}

func segments(d Dir, stream string) []segment {
	ps, _ := filepath.Glob(filepath.Join(string(d), stream+".*"))
	var out []segment
	for _, p := range ps {
		off, err := strconv.ParseInt(strings.TrimPrefix(filepath.Base(p), stream+"."), 10, 64)
		if err == nil {
			out = append(out, segment{off, p})
		}
	}
	slices.SortFunc(out, func(a, b segment) int { return int(a.off - b.off) })
	return out
}

// read answers up to max bytes of stream from offset from: where they start
// (past from when older bytes were dropped) and the stream's end so far.
func read(d Dir, stream string, from int64, max int) (data []byte, first, end int64, err error) {
	for try := 0; ; try++ {
		segs := segments(d, stream)
		if len(segs) == 0 {
			return nil, from, 0, nil
		}
		last := segs[len(segs)-1]
		st, serr := os.Stat(last.path)
		if serr != nil {
			if try < 3 {
				continue
			}
			return nil, from, 0, serr
		}
		end = last.off + st.Size()
		if from > end {
			return nil, from, end, refuse("bad_request", "%s_from %d is past the %d bytes written", stream, from, end)
		}
		first = max64(from, segs[0].off)
		data, err = readSegs(segs, first, max)
		if err == nil || try >= 3 {
			return data, first, max64(end, first+int64(len(data))), err
		}
	}
}

func readSegs(segs []segment, pos int64, max int) ([]byte, error) {
	var out []byte
	for i, s := range segs {
		if len(out) >= max {
			break
		}
		if i+1 < len(segs) && segs[i+1].off <= pos {
			continue
		}
		f, err := os.Open(s.path)
		if err != nil {
			return nil, err // deleted under us: read again
		}
		b := make([]byte, max-len(out))
		n, _ := f.ReadAt(b, pos-s.off)
		f.Close()
		out = append(out, b[:n]...)
		pos += int64(n)
	}
	return out, nil
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

// OutputArgs are output's args.
type OutputArgs struct {
	PID        string `json:"pid"`
	StdoutFrom int64  `json:"stdout_from"`
	StderrFrom int64  `json:"stderr_from"`
	MaxBytes   int    `json:"max_bytes"`
	WaitS      int    `json:"wait_s"`
	Encoding   string `json:"encoding"`
}

// ParseOutput reads and bounds output's args.
func ParseOutput(raw json.RawMessage) (OutputArgs, error) {
	var a OutputArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return a, refuse("bad_request", "bad args: %v", err)
	}
	switch {
	case a.PID == "":
		return a, refuse("bad_request", "output names a pid")
	case a.StdoutFrom < 0 || a.StderrFrom < 0:
		return a, refuse("bad_request", "stdout_from and stderr_from are byte offsets, 0 or more")
	case a.MaxBytes < 0 || a.MaxBytes > MaxBytes:
		return a, refuse("bad_request", "max_bytes is 1..%d", MaxBytes)
	case a.WaitS < 0 || a.WaitS > MaxWaitS:
		return a, refuse("bad_request", "wait_s is 0..%d", MaxWaitS)
	case a.Encoding != "" && a.Encoding != Base64:
		return a, refuse("bad_request", "encoding is base64 or absent")
	}
	if a.MaxBytes == 0 {
		a.MaxBytes = DefaultBytes
	}
	return a, nil
}

// Output answers one output call (SPEC 20): the bytes of each stream from
// its offset, at most a.MaxBytes in all, waiting up to a.WaitS for a new byte
// or the exit. ready is asked each tick: the box must still be up.
func Output(d Dir, a OutputArgs, ready func() error) (map[string]any, error) {
	deadline := time.Now().Add(time.Duration(a.WaitS) * time.Second)
	for {
		if err := ready(); err != nil {
			return nil, err
		}
		exit := d.Exit() // before the output: once it is there, the output is whole
		running := exit == nil && d.holderUp()
		if exit == nil && !running {
			exit = d.Exit() // it landed after the look above; the holder writes it before it goes
		}
		// Read a character past the page: a text page cut inside one is
		// trimmed to it, and a page too small for its first one gets it whole.
		oa, of, oend, err := read(d, "stdout", a.StdoutFrom, a.MaxBytes+utf8.UTFMax)
		if err != nil {
			return nil, err
		}
		ea, ef, eend, err := read(d, "stderr", a.StderrFrom, a.MaxBytes+utf8.UTFMax)
		if err != nil {
			return nil, err
		}
		o, e := split(oa[:min(len(oa), a.MaxBytes)], ea[:min(len(ea), a.MaxBytes)], a.MaxBytes)
		if a.Encoding != Base64 {
			o = textPage(o, oa, of+int64(len(o)) < oend || running, running)
			e = textPage(e, ea, ef+int64(len(e)) < eend || running, running)
		}
		if len(o) > 0 || len(e) > 0 || !running || !time.Now().Before(deadline) { // on the trimmed page: a cut character is nothing new
			out := map[string]any{"stdout": Encode(o, a.Encoding), "stderr": Encode(e, a.Encoding),
				"stdout_next": of + int64(len(o)), "stderr_next": ef + int64(len(e)), "running": running}
			if n := of - a.StdoutFrom; n > 0 {
				out["dropped_stdout"] = n
			}
			if n := ef - a.StderrFrom; n > 0 {
				out["dropped_stderr"] = n
			}
			if exit != nil {
				out["exit"] = *exit
			}
			if a.Encoding == Base64 {
				out["encoding"] = Base64
			}
			return out, nil
		}
		time.Sleep(pollTick(d))
	}
}

// pollTick is how often a waiting output call looks for new bytes: often
// on a terminal (a key's echo waits at most this), less for the rest.
func pollTick(d Dir) time.Duration {
	if m, err := d.Meta(); err == nil && m.PTY {
		return 10 * time.Millisecond
	}
	return 50 * time.Millisecond
}

// split shares max between the streams: all of both when they fit, else
// each at least half (the other's unused share goes to it).
func split(o, e []byte, max int) ([]byte, []byte) {
	if len(o)+len(e) <= max {
		return o, e
	}
	no := min(len(o), maxInt(max/2, max-len(e)))
	return o[:no], e[:min(len(e), max-no)]
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// textPage is a text page that never ends inside a UTF-8 character while
// more of the stream follows it (more: bytes past the page, or a running
// process that may write them). A page cut down to nothing that way, while
// avail (the bytes from its offset, a character past the page) holds a whole
// first character, answers that one character, past max_bytes by at most
// 3 bytes, so a small max_bytes never stalls; the last bytes of an ended
// stream go as they are.
func textPage(page, avail []byte, more, running bool) []byte {
	if !more || len(page) == 0 {
		return page
	}
	if n := whole(page); n > 0 {
		return page[:n]
	}
	if utf8.FullRune(avail) {
		_, n := utf8.DecodeRune(avail)
		return avail[:n]
	}
	if !running {
		return avail // a cut-off character at the end of an ended stream
	}
	return nil
}

// whole is the length of b without a cut-off UTF-8 sequence at its end.
func whole(b []byte) int {
	for i := len(b) - 1; i >= 0 && i >= len(b)-utf8.UTFMax+1; i-- {
		if utf8.RuneStart(b[i]) {
			if !utf8.FullRune(b[i:]) {
				return i
			}
			break
		}
	}
	return len(b)
}

// Signals are signal's names and numbers.
var Signals = map[string]syscall.Signal{"TERM": syscall.SIGTERM, "INT": syscall.SIGINT, "KILL": syscall.SIGKILL}

// SignalArgs are signal's args.
type SignalArgs struct {
	PID    string `json:"pid"`
	Signal string `json:"signal"`
}

// ParseSignal reads signal's args.
func ParseSignal(raw json.RawMessage) (SignalArgs, error) {
	var a SignalArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return a, refuse("bad_request", "bad args: %v", err)
	}
	if a.PID == "" {
		return a, refuse("bad_request", "signal names a pid")
	}
	if _, ok := Signals[a.Signal]; !ok {
		return a, refuse("bad_request", "signal is TERM, INT or KILL")
	}
	return a, nil
}
