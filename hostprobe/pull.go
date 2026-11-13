package hostprobe

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"
)

// StreamRunner is a Runner that can also hand a command's output on as it
// arrives. ExecRunner is one; a Runner that is not gets the output at the end.
type StreamRunner interface {
	Runner
	RunStream(ctx context.Context, w io.Writer, name string, args ...string) (Result, error)
}

// RunStream runs a real command, copying stdout and stderr to w as they come
// and keeping them in the Result too.
func (ExecRunner) RunStream(ctx context.Context, w io.Writer, name string, args ...string) (Result, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.WaitDelay = 5 * time.Second // a child that keeps the pipes open must not outlive the timeout
	var out, errb bytes.Buffer
	sw := &syncWriter{w: w}
	cmd.Stdout, cmd.Stderr = io.MultiWriter(&out, sw), io.MultiWriter(&errb, sw)
	err := cmd.Run()
	res := Result{Stdout: out.String(), Stderr: errb.String()}
	if ee, ok := err.(*exec.ExitError); ok && ctx.Err() == nil {
		res.Exit = ee.ExitCode()
		return res, nil
	}
	if err != nil && ctx.Err() != nil {
		return res, ctx.Err()
	}
	return res, err
}

type syncWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}

// DefaultPullTimeout is how long one image pull may take.
const DefaultPullTimeout = 60 * time.Minute

func pullTimeout(o ShareOptions) time.Duration {
	if o.PullTimeout > 0 {
		return o.PullTimeout
	}
	return DefaultPullTimeout
}

// pullProgressEvery is the least time between two progress lines of a pull:
// Tart redraws its progress many times a second.
const pullProgressEvery = 5 * time.Second

// runPull runs one pull under its timeout. When o.Progress is set and the
// Runner can stream, each line the pull prints (a carriage return ends one
// too, as a progress bar redraws) goes to Progress, at most one per
// pullProgressEvery.
func runPull(ctx context.Context, run Runner, o ShareOptions, cmd []string) (Result, error) {
	parent := ctx
	ctx, cancel := context.WithTimeout(ctx, pullTimeout(o))
	defer cancel()
	var res Result
	var err error
	if sr, ok := run.(StreamRunner); ok && o.Progress != nil {
		lw := &lineWriter{emit: func(line string) { o.Progress(pullLine(cmd[len(cmd)-1], line)) }, every: pullProgressEvery, now: time.Now}
		res, err = sr.RunStream(ctx, lw, cmd[0], cmd[1:]...)
		lw.flush()
	} else {
		res, err = run.Run(ctx, cmd[0], cmd[1:]...)
	}
	if ctx.Err() == context.DeadlineExceeded && parent.Err() == nil { // killed at the pull's own limit
		return res, context.DeadlineExceeded
	}
	return res, err
}

var pullPercent = regexp.MustCompile(`(\d{1,3}(?:\.\d+)?)\s*%`)

// pullLine is one progress line of pulling image, in plain words with the
// percentage Tart's progress bar shows ("pulling macos-tahoe-base: 12%"). A
// line with no percentage (the manifest, a layer count) goes on as Tart
// printed it.
func pullLine(image, line string) string {
	m := pullPercent.FindAllStringSubmatch(line, -1)
	if m == nil {
		return line
	}
	name, _, _ := strings.Cut(image, "@")
	return fmt.Sprintf("pulling %s: %s%%", name, m[len(m)-1][1])
}

// lineWriter turns a byte stream into lines at \n or \r and emits the
// non-empty ones, at most one per every (the last line is kept for flush).
type lineWriter struct {
	emit  func(string)
	every time.Duration
	now   func() time.Time
	buf   []byte
	last  time.Time
	held  string
}

func (l *lineWriter) Write(p []byte) (int, error) {
	l.buf = append(l.buf, p...)
	for {
		i := bytes.IndexAny(l.buf, "\r\n")
		if i < 0 {
			break
		}
		line := strings.TrimSpace(string(l.buf[:i]))
		l.buf = l.buf[i+1:]
		if line == "" {
			continue
		}
		if t := l.now(); l.last.IsZero() || t.Sub(l.last) >= l.every {
			l.last, l.held = t, ""
			l.emit(line)
		} else {
			l.held = line
		}
	}
	return len(p), nil
}

// flush emits the last line a throttle held back, or an unfinished one.
func (l *lineWriter) flush() {
	if rest := strings.TrimSpace(string(l.buf)); rest != "" {
		l.held = rest
	}
	l.buf = nil
	if l.held != "" {
		l.emit(l.held)
		l.held = ""
	}
}
