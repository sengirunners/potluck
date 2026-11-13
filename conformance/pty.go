package conformance

import (
	"encoding/base64"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/sengirunners/potluck/protocol"
)

// Terminals in a box (SPEC 20 "Boxes", terminals; zm-eakh.12.5.21): spawn's
// pty and the optional input verb, listed together (capability pty, verb
// input). Seen through what the command on the terminal prints: its size,
// the input it reads, a resize, a Ctrl-C.

const (
	verbInput = "input"
	ptyCap    = "pty"
)

type inputResult struct {
	Written *int  `json:"written"`
	Running *bool `json:"running"`
}

func (r *run) input(h, pid string, kv ...any) (inputResult, error) {
	var in inputResult
	if err := must(r.call(verbInput, h, args(append([]any{"pid", pid}, kv...)...), &in)); err != nil {
		return in, err
	}
	if in.Written == nil || in.Running == nil {
		return in, fmt.Errorf("input answers no written or running")
	}
	return in, nil
}

// term follows a process's stdout until want shows, or it ends.
type term struct {
	r      *run
	h, pid string
	seen   strings.Builder
	next   int64
	exit   *int
}

func (t *term) until(want string, within time.Duration) error {
	deadline := time.Now().Add(within)
	for !strings.Contains(t.seen.String(), want) {
		if t.exit != nil || !time.Now().Before(deadline) {
			return fmt.Errorf("no %q in what the terminal showed: %q (exit %v)", want, clip(t.seen.String()), t.exit)
		}
		o, err := t.r.output(t.h, t.pid, "stdout_from", t.next, "wait_s", 2)
		if err != nil {
			return err
		}
		t.seen.WriteString(*o.Stdout)
		t.next = *o.StdoutNext
		if !*o.Running {
			t.exit = o.Exit
		}
	}
	return nil
}

// end waits for the process's exit.
func (t *term) end(within time.Duration) (int, error) {
	if t.exit == nil {
		o, err := t.r.waitEnd(t.h, t.pid, within)
		if err != nil {
			return 0, err
		}
		t.exit = o.Exit
	}
	return *t.exit, nil
}

func (r *run) pty() {
	if r.kind != "box" {
		return
	}
	const listed = "a driver listing input lists capability pty, and the other way round"
	caps := []string{}
	if r.desc.Capabilities != nil {
		caps = *r.desc.Capabilities
	}
	hasCap := slices.Contains(caps, ptyCap)
	if !r.has(verbInput) && !hasCap {
		if !r.has(verbSpawn) {
			r.skip("terminals", "describe lists no spawn")
			return
		}
		h, err := r.acquireAny(r.longTTL())
		if !r.add("acquire a box for terminals", err) {
			return
		}
		defer r.endHeld(h)
		r.add("input answers not_implemented when not listed",
			r.expect(verbInput, h.Handle, args("pid", "p000000000000", "data", "x"), protocol.CodeNotImplemented))
		r.skip("terminals", "describe lists no pty capability")
		return
	}
	if !r.add(listed, func() error {
		if !r.has(verbInput) || !hasCap || !r.has(verbSpawn) {
			return fmt.Errorf("verbs %v, capabilities %v: want input, spawn and pty together", r.desc.Verbs, caps)
		}
		return nil
	}()) {
		return
	}
	h, err := r.acquireAny(r.longTTL())
	if !r.add("acquire a box for terminals", err) {
		return
	}
	defer r.endHeld(h)
	tty := map[string]any{"rows": 24, "cols": 80, "term": "vt100"}

	// A terminal of the size asked, TERM set; input read; Ctrl-D ends it.
	cat := `test -t 0 && test -t 1 || exit 9; stty size; echo "term=$TERM"; cat; echo cat-done`
	pid, err := r.spawn(h.Handle, "argv", []string{"sh", "-c", cat}, "pty", tty, "request_id", r.key())
	t := &term{r: r, h: h.Handle, pid: pid}
	if err == nil {
		if err = t.until("24 80", 20*time.Second); err == nil {
			err = t.until("term=vt100", 5*time.Second)
		}
	}
	if !r.add("spawn with pty runs the command on a terminal of that size, TERM set", err) {
		return
	}
	var in inputResult
	// base64, which every driver serving input takes (SPEC 20).
	in, err = r.input(h.Handle, pid, "data", base64.StdEncoding.EncodeToString([]byte("zmconf-in-"+r.tag+"\n")), "encoding", "base64")
	if err == nil && (*in.Written != len("zmconf-in-"+r.tag+"\n") || !*in.Running) {
		err = fmt.Errorf("input answered written %d, running %v", *in.Written, *in.Running)
	}
	if err == nil {
		err = t.until("zmconf-in-"+r.tag, 10*time.Second)
	}
	if err == nil {
		_, err = r.input(h.Handle, pid, "data", "\x04")
	}
	if err == nil {
		err = t.until("cat-done", 10*time.Second)
	}
	if err == nil {
		var code int
		if code, err = t.end(10 * time.Second); err == nil && code != 0 {
			err = fmt.Errorf("exit %d after Ctrl-D, want 0", code)
		}
	}
	r.add("input (base64) reaches the terminal, and a Ctrl-D ends the command's input", err)

	// A resize, then a Ctrl-C to the terminal's foreground process.
	resize := `stty size; read x; stty size; read y; echo not-interrupted`
	pid2, err := r.spawn(h.Handle, "argv", []string{"sh", "-c", resize}, "pty", tty, "request_id", r.key())
	t2 := &term{r: r, h: h.Handle, pid: pid2}
	if err == nil {
		err = t2.until("24 80", 20*time.Second)
	}
	if err == nil {
		_, err = r.input(h.Handle, pid2, "rows", 30, "cols", 100, "data", "\n")
	}
	if err == nil {
		err = t2.until("30 100", 10*time.Second)
	}
	r.add("input's rows and cols resize the terminal", err)
	if err == nil {
		_, err = r.input(h.Handle, pid2, "data", "\x03")
	}
	if err == nil {
		var code int
		if code, err = t2.end(10 * time.Second); err == nil && code != 130 {
			err = fmt.Errorf("exit %d after Ctrl-C, want 130 (SIGINT)", code)
		}
	}
	if err != nil {
		r.signalKill(h.Handle, pid2)
	}
	r.add("a Ctrl-C on the terminal interrupts its command (exit 130)", err)

	// After the end, a process without a terminal, refusals.
	in, err = r.input(h.Handle, pid, "data", "late")
	if err == nil && (*in.Running || *in.Written != 0) {
		err = fmt.Errorf("input to an ended process answered written %d, running %v; want 0 and false", *in.Written, *in.Running)
	}
	var plain string
	if err == nil {
		plain, err = r.spawn(h.Handle, "argv", []string{"sleep", "20"}, "request_id", r.key())
	}
	if err == nil {
		if err = r.expect(verbInput, h.Handle, args("pid", plain, "data", "x"), protocol.CodeBadRequest); err != nil {
			err = fmt.Errorf("input to a process with no terminal: %v", err)
		}
		r.signalKill(h.Handle, plain)
	}
	for _, bad := range []struct {
		verb string
		a    []any
	}{
		{verbSpawn, []any{"argv", []string{"sh"}, "pty", map[string]any{"rows": 0, "cols": 80}, "request_id", r.key()}},
		{verbSpawn, []any{"argv", []string{"sh"}, "pty", tty, "secrets", map[string]string{"ZMCONF_S": "x"}, "request_id", r.key()}},
		{verbInput, []any{"pid", pid, "rows", 30}},
		{verbInput, []any{"pid", pid, "data", strings.Repeat("a", 64<<10+1)}},
	} {
		if err == nil {
			if err = r.expect(bad.verb, h.Handle, args(bad.a...), protocol.CodeBadRequest); err != nil {
				err = fmt.Errorf("%s %v: %v", bad.verb, bad.a[:min(len(bad.a), 4)], err)
			}
		}
	}
	if err == nil {
		err = r.expect(verbInput, h.Handle, args("pid", "p000000000000", "data", "x"), protocol.CodeNotFound)
	}
	r.add("input to an ended process answers running false; to one without a terminal, bad args or a pty with secrets: bad_request", err)
}

// signalKill ends a process a failed check left.
func (r *run) signalKill(h, pid string) {
	_, _ = r.call(verbSignal, h, args("pid", pid, "signal", "KILL"), nil)
}
