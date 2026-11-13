package conformance

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/sengirunners/potluck/protocol"
)

// Background processes in a box (SPEC 20 "Boxes", background processes):
// the optional spawn, output and signal verbs, and exec's base64 encoding.
// All of it is seen through the verbs and exec: a process's own output, a
// file it writes, a child it leaves.

const (
	verbSpawn  = "spawn"
	verbOutput = "output"
	verbSignal = "signal"
	// base64Cap is the capability saying exec, spawn and output take encoding "base64".
	base64Cap = "base64"
	// procKeep is SPEC 20's least kept of each stream; outputDefault the
	// default page.
	procKeep      = 8 << 20
	outputDefault = 64 << 10
	outputMax     = 1 << 20
)

type outputResult struct {
	Stdout        *string `json:"stdout"`
	Stderr        *string `json:"stderr"`
	StdoutNext    *int64  `json:"stdout_next"`
	StderrNext    *int64  `json:"stderr_next"`
	DroppedStdout int64   `json:"dropped_stdout"`
	DroppedStderr int64   `json:"dropped_stderr"`
	Running       *bool   `json:"running"`
	Exit          *int    `json:"exit"`
}

func (r *run) spawn(h string, kv ...any) (string, error) {
	var res struct {
		PID string `json:"pid"`
	}
	if err := must(r.call(verbSpawn, h, args(kv...), &res)); err != nil {
		return "", err
	}
	if res.PID == "" {
		return "", fmt.Errorf("spawn answers no pid")
	}
	return res.PID, nil
}

// output sends one output call and checks the answer's shape.
func (r *run) output(h, pid string, kv ...any) (outputResult, error) {
	var o outputResult
	if err := must(r.call(verbOutput, h, args(append([]any{"pid", pid}, kv...)...), &o)); err != nil {
		return o, err
	}
	switch {
	case o.Stdout == nil || o.Stderr == nil || o.StdoutNext == nil || o.StderrNext == nil || o.Running == nil:
		return o, fmt.Errorf("output answers no stdout, stderr, stdout_next, stderr_next or running")
	case *o.Running && o.Exit != nil:
		return o, fmt.Errorf("output answers an exit while running")
	case !*o.Running && o.Exit == nil:
		return o, fmt.Errorf("output answers neither running nor an exit")
	}
	return o, nil
}

// waitEnd polls (without reading) until pid is no longer running.
func (r *run) waitEnd(h, pid string, within time.Duration) (outputResult, error) {
	deadline := time.Now().Add(within)
	for {
		o, err := r.output(h, pid, "max_bytes", 1)
		if err != nil || !*o.Running {
			return o, err
		}
		if !time.Now().Before(deadline) {
			return o, fmt.Errorf("%s still running after %s", pid, within)
		}
		r.sleep(200 * time.Millisecond)
	}
}

func (r *run) procs() {
	if r.kind != "box" {
		return
	}
	const listed = "a driver listing spawn lists output and signal"
	h, err := r.acquireAny(r.longTTL())
	if !r.add("acquire a box for background processes", err) {
		return
	}
	defer r.endHeld(h)
	if !r.has(verbSpawn) {
		r.add("spawn answers not_implemented when not listed",
			r.expect(verbSpawn, h.Handle, args("argv", []string{"true"}, "request_id", r.key()), protocol.CodeNotImplemented))
		r.skip("background processes", "describe lists no spawn")
		return
	}
	if !r.add(listed, missing(r.desc.Verbs, verbOutput, verbSignal)) {
		return
	}
	// A 20+ s process writing a line a second, and a quiet one with a child.
	chatty := `echo x >> "$HOME/zmconf-spawns-$1"; i=0; while [ $i -lt 22 ]; do printf 'line-%02d\n' $i; i=$((i+1)); sleep 1; done; printf 'err-%s' "$1" >&2; exit 7`
	key := r.key()
	a, err := r.spawn(h.Handle, "argv", []string{"sh", "-c", chatty, "sh", r.tag}, "request_id", key)
	if !r.add("spawn answers a pid", err) {
		return
	}
	again, idem := r.spawn(h.Handle, "argv", []string{"sh", "-c", chatty, "sh", r.tag}, "request_id", key)
	if idem == nil && again != a {
		idem = fmt.Errorf("the same request_id answered %s, then %s", a, again)
	}
	// The process outlives a TERM to itself (a trap), so only a signal to its
	// group, which ends its child, lets it exit at once.
	quiet := `trap : TERM; sleep 25 & echo $! > "$HOME/zmconf-child-$1"; wait; wait; exit 3`
	b, err := r.spawn(h.Handle, "argv", []string{"sh", "-c", quiet, "sh", r.tag}, "request_id", r.key())
	if err == nil && b == a {
		err = fmt.Errorf("another request_id answered the same pid %s", a)
	}
	if !r.add("another request_id answers another pid", err) {
		return
	}
	r.procWait(h.Handle, b)
	r.procSignal(h.Handle, b)
	r.procBounds(h.Handle)
	r.procDropped(h.Handle)
	r.procBase64(h.Handle)
	r.procSecrets(h.Handle)
	r.procPages(h.Handle, a)
	if idem == nil {
		idem = r.execExit(h.Handle, 0, "sh", "-c", `[ "$(wc -l < "$HOME/zmconf-spawns-$1")" -eq 1 ]`, "sh", r.tag)
		if idem != nil {
			idem = fmt.Errorf("the command ran more than once: %v", idem)
		}
	}
	r.add("the same request_id answers the same pid and starts the command once", idem)

	rel, err := r.release(h.Handle)
	if err == nil {
		err = wantRelease(rel, true, nil)
	}
	if err == nil {
		delete(r.live, h.Handle)
		err = r.expect(verbSpawn, h.Handle, args("argv", []string{"true"}, "request_id", r.key()), protocol.CodeConflict)
	}
	if err == nil {
		err = r.expect(verbOutput, h.Handle, args("pid", a), protocol.CodeConflict)
	}
	if err == nil {
		err = r.expect(verbSignal, h.Handle, args("pid", b, "signal", "KILL"), protocol.CodeConflict)
	}
	r.add("spawn, output and signal after release answer conflict", err)
}

func missing(have []string, want ...string) error {
	for _, w := range want {
		if !slices.Contains(have, w) {
			return fmt.Errorf("verbs %v lack %s", have, w)
		}
	}
	return nil
}

// procWait: output with wait_s on a quiet process waits, then answers empty.
func (r *run) procWait(h, pid string) {
	start := time.Now()
	o, err := r.output(h, pid, "wait_s", 2)
	took := time.Since(start)
	switch {
	case err != nil:
	case !*o.Running || *o.Stdout != "" || *o.StdoutNext != 0:
		err = fmt.Errorf("running %v, stdout %q, stdout_next %d: want a quiet running process", *o.Running, clip(*o.Stdout), *o.StdoutNext)
	case took < 1500*time.Millisecond:
		err = fmt.Errorf("answered after %s for wait_s 2 with nothing new", took.Round(time.Millisecond))
	case took > 7*time.Second:
		err = fmt.Errorf("answered after %s for wait_s 2", took.Round(time.Second))
	}
	r.add("output waits up to wait_s for a new byte, then answers what there is", err)
}

// procSignal: TERM reaches the whole group; a second signal is not sent.
func (r *run) procSignal(h, pid string) {
	var sent struct {
		Sent *bool `json:"sent"`
	}
	err := must(r.call(verbSignal, h, args("pid", pid, "signal", "TERM"), &sent))
	if err == nil && (sent.Sent == nil || !*sent.Sent) {
		err = fmt.Errorf("signal of a running process answers sent %v", sent.Sent)
	}
	var o outputResult
	if err == nil {
		o, err = r.waitEnd(h, pid, 10*time.Second)
	}
	if err == nil && *o.Exit != 3 {
		err = fmt.Errorf("exit %d after TERM, want 3 (its own, once its child ended)", *o.Exit)
	}
	if err == nil {
		err = r.execExit(h, 0, "sh", "-c", `p=$(cat "$HOME/zmconf-child-$1") || exit 1; ! kill -0 "$p" 2>/dev/null || ps -o stat= -p "$p" | grep -q Z`, "sh", r.tag)
		if err != nil {
			err = fmt.Errorf("the process's child is still running: %v", err)
		}
	}
	r.add("signal TERM reaches the process group: the process's child ends with it", err)
	sent.Sent = nil
	err = must(r.call(verbSignal, h, args("pid", pid, "signal", "KILL"), &sent))
	if err == nil && (sent.Sent == nil || *sent.Sent) {
		err = fmt.Errorf("signal of an ended process answers sent %v, want false", sent.Sent)
	}
	if err == nil {
		err = r.expect(verbSignal, h, args("pid", pid, "signal", "HUP"), protocol.CodeBadRequest)
	}
	r.add("signal after the end answers sent false; a signal not TERM, INT or KILL is bad_request", err)
}

// procBounds: the default page, max_bytes, offsets, refusals.
func (r *run) procBounds(h string) {
	const total = 20000 * 8
	pid, err := r.spawn(h, "argv", []string{"awk", `BEGIN { for (i = 0; i < 20000; i++) printf "%07d\n", i }`}, "request_id", r.key())
	if err == nil {
		_, err = r.waitEnd(h, pid, 20*time.Second)
	}
	want := numbered(total)
	page := func(from int64, kv ...any) (outputResult, error) {
		return r.output(h, pid, append([]any{"stdout_from", from}, kv...)...)
	}
	check := func(o outputResult, from int64, n int) error {
		switch {
		case len(*o.Stdout) != n:
			return fmt.Errorf("from %d: %d bytes, want %d", from, len(*o.Stdout), n)
		case *o.Stdout != want[from:from+int64(n)]:
			return fmt.Errorf("from %d: not the bytes written there", from)
		case *o.StdoutNext != from+int64(n):
			return fmt.Errorf("from %d: stdout_next %d, want %d", from, *o.StdoutNext, from+int64(n))
		}
		return nil
	}
	var o outputResult
	if err == nil {
		if o, err = page(0); err == nil {
			err = check(o, 0, outputDefault)
		}
	}
	if err == nil {
		if o, err = page(100, "max_bytes", 1000); err == nil {
			err = check(o, 100, 1000)
		}
	}
	if err == nil {
		if o, err = page(total); err == nil {
			err = check(o, total, 0)
		}
		if err == nil && (*o.Running || *o.Exit != 0) {
			err = fmt.Errorf("at the end: running %v, exit %v, want ended with 0", *o.Running, o.Exit)
		}
	}
	for _, bad := range []json.RawMessage{
		args("pid", pid, "max_bytes", outputMax+1), args("pid", pid, "wait_s", 21),
		args("pid", pid, "stdout_from", -1), args("pid", pid, "stdout_from", total+1),
	} {
		if err == nil {
			if err = r.expect(verbOutput, h, bad, protocol.CodeBadRequest); err != nil {
				err = fmt.Errorf("output %s: %v", bad, err)
			}
		}
	}
	if err == nil {
		err = r.expect(verbOutput, h, args("pid", "p000000000000"), protocol.CodeNotFound)
	}
	for _, bad := range []json.RawMessage{
		args("argv", []string{"true"}), args("argv", "true", "request_id", r.key()), args("argv", []string{}, "request_id", r.key()),
	} {
		if err == nil {
			if err = r.expect(verbSpawn, h, bad, protocol.CodeBadRequest); err != nil {
				err = fmt.Errorf("spawn %s: %v", bad, err)
			}
		}
	}
	r.add("output pages 64 KiB by default and at most max_bytes, from in-range offsets; bad output and spawn args refused", err)
}

// procDropped: past 8 MiB a stream keeps at least its newest 8 MiB, and
// says how much before them it dropped.
func (r *run) procDropped(h string) {
	const lines = 1400000
	const total = lines * 8
	pid, err := r.spawn(h, "argv", []string{"awk", fmt.Sprintf(`BEGIN { for (i = 0; i < %d; i++) printf "%%07d\n", i }`, lines)}, "request_id", r.key())
	if err == nil {
		_, err = r.waitEnd(h, pid, 60*time.Second)
	}
	want := numbered(total)
	var o outputResult
	if err == nil {
		o, err = r.output(h, pid, "max_bytes", outputMax)
	}
	if err == nil {
		d := o.DroppedStdout
		switch {
		case d <= 0 || d > total-procKeep:
			err = fmt.Errorf("dropped_stdout %d for %d bytes written, want 1..%d", d, total, total-procKeep)
		case len(*o.Stdout) == 0 || *o.Stdout != want[d:d+int64(len(*o.Stdout))]:
			err = fmt.Errorf("the bytes answered are not those at offset %d", d)
		case *o.StdoutNext != d+int64(len(*o.Stdout)):
			err = fmt.Errorf("stdout_next %d, want %d", *o.StdoutNext, d+int64(len(*o.Stdout)))
		}
	}
	if err == nil {
		from := int64(total - procKeep)
		o, err = r.output(h, pid, "stdout_from", from, "max_bytes", outputMax)
		if err == nil && (o.DroppedStdout != 0 || len(*o.Stdout) != outputMax || *o.Stdout != want[from:from+outputMax]) {
			err = fmt.Errorf("from the newest 8 MiB: dropped %d, %d bytes: want none dropped and those bytes", o.DroppedStdout, len(*o.Stdout))
		}
	}
	r.add("output keeps a stream's newest 8 MiB and counts the bytes it dropped", err)
}

// procBase64: with the capability, bytes cross exec and spawn exactly.
func (r *run) procBase64(h string) {
	const name = "encoding base64: stdin and output cross spawn and output exactly"
	if r.desc.Capabilities == nil || !slices.Contains(*r.desc.Capabilities, base64Cap) {
		r.skip(name, "describe lists no base64 capability")
		return
	}
	all := make([]byte, 256)
	for i := range all {
		all[i] = byte(i)
	}
	in := base64.StdEncoding.EncodeToString(all)
	pid, err := r.spawn(h, "argv", []string{"cat"}, "stdin", in, "encoding", "base64", "request_id", r.key())
	if err == nil {
		_, err = r.waitEnd(h, pid, 20*time.Second)
	}
	var o outputResult
	if err == nil {
		o, err = r.output(h, pid, "encoding", "base64")
	}
	if err == nil && *o.Stdout != in {
		err = fmt.Errorf("stdout %q, want the 256 bytes sent", clip(*o.Stdout))
	}
	r.add(name, err)
}

// execBase64: with the capability, exec's stdin and output in base64.
func (r *run) execBase64(h string) {
	const name = "exec encoding base64: every byte value crosses stdin and stdout exactly"
	if r.desc.Capabilities == nil || !slices.Contains(*r.desc.Capabilities, base64Cap) {
		r.skip(name, "describe lists no base64 capability")
		return
	}
	all := make([]byte, 256)
	for i := range all {
		all[i] = byte(255 - i)
	}
	in := base64.StdEncoding.EncodeToString(all)
	res, err := r.exec2(h, args("argv", []string{"cat"}, "stdin", in, "encoding", "base64"), 0)
	if err == nil && (*res.Stdout != in || *res.Exit != 0) {
		err = fmt.Errorf("exit %d, stdout %q: want 0 and the bytes sent", *res.Exit, clip(*res.Stdout))
	}
	r.add(name, err)
}

// procSecrets: a spawned process gets exec's secrets, gone once it ends.
func (r *run) procSecrets(h string) {
	const name = "spawn secrets: a file in $ZM_SECRETS while the process runs, gone once it ends"
	if r.desc.Capabilities == nil || !slices.Contains(*r.desc.Capabilities, secretsCap) {
		r.skip(name, "describe lists no secrets capability")
		return
	}
	val := "zmconf-secret-" + r.tag + "-" + randomTag()
	r.secrets = append(r.secrets, val)
	r.sawSecret = true
	line := `d=${POTLUCK_SECRETS:-$ZM_SECRETS}; printf '%s\n' "$d"; od -An -tx1 "$d/ZMCONF_S" | tr -d ' \n'`
	pid, err := r.spawn(h, "argv", []string{"sh", "-c", line}, "secrets", map[string]string{"ZMCONF_S": val}, "request_id", r.key())
	var o outputResult
	if err == nil {
		if _, err = r.waitEnd(h, pid, 20*time.Second); err == nil {
			o, err = r.output(h, pid)
		}
	}
	if err == nil {
		dir, got, _ := strings.Cut(*o.Stdout, "\n")
		switch {
		case *o.Exit != 0 || !strings.HasPrefix(dir, "/"):
			err = fmt.Errorf("exit %d, ZM_SECRETS %q (stderr %q)", *o.Exit, dir, clip(*o.Stderr))
		case got != hex.EncodeToString([]byte(val)):
			err = fmt.Errorf("the file does not hold the value sent")
		default:
			if err = r.execExit(h, 0, "sh", "-c", `test ! -e "$1"`, "sh", dir); err != nil {
				err = fmt.Errorf("%s is still there after the process: %v", dir, err)
			}
		}
	}
	r.add(name, err)
}

// procPages reads the 20+ s process in pages while it runs, then its exit.
func (r *run) procPages(h, pid string) {
	var out, errs strings.Builder
	var so, se int64
	running := 0
	deadline := time.Now().Add(45 * time.Second)
	var err error
	for {
		var o outputResult
		if o, err = r.output(h, pid, "stdout_from", so, "stderr_from", se, "wait_s", 5); err != nil {
			break
		}
		if *o.StdoutNext != so+int64(len(*o.Stdout)) || *o.StderrNext != se+int64(len(*o.Stderr)) || o.DroppedStdout != 0 {
			err = fmt.Errorf("from %d: %d bytes, stdout_next %d, dropped %d", so, len(*o.Stdout), *o.StdoutNext, o.DroppedStdout)
			break
		}
		out.WriteString(*o.Stdout)
		errs.WriteString(*o.Stderr)
		so, se = *o.StdoutNext, *o.StderrNext
		if *o.Running && *o.Stdout != "" {
			running++
		}
		if !*o.Running {
			var want strings.Builder
			for i := range 22 {
				fmt.Fprintf(&want, "line-%02d\n", i)
			}
			switch {
			case running < 2:
				err = fmt.Errorf("only %d pages with new output while it ran, want 2 or more", running)
			case out.String() != want.String():
				err = fmt.Errorf("the pages make %q, want 22 lines", clip(out.String()))
			case errs.String() != "err-"+r.tag || *o.Exit != 7:
				err = fmt.Errorf("stderr %q, exit %d: want %q and 7", clip(errs.String()), *o.Exit, "err-"+r.tag)
			}
			break
		}
		if !time.Now().Before(deadline) {
			err = fmt.Errorf("still running after 45 s")
			break
		}
	}
	r.add("output pages a 20 s process while it runs, by offset, then answers its exit", err)
}
