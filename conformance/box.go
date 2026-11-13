package conformance

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/sengirunners/potluck/protocol"
)

// A box (SPEC 20, kind "box") is a VM kept up for its lease; its holder sends
// commands with exec. What a box does is seen through exec itself: a command
// whose output no canned answer gives, a file written by one exec and read by
// the next, a marker the next box must not see.

// exec sends one exec and checks the answer's shape.
func (r *run) exec(h string, argv []string, stdin string, timeout int) (execResult, error) {
	a := []any{"argv", argv}
	if stdin != "" {
		a = append(a, "stdin", stdin)
	}
	if timeout > 0 {
		a = append(a, "timeout_s", timeout)
	}
	var res execResult
	if err := must(r.call(verbExec, h, args(a...), &res)); err != nil {
		return res, err
	}
	if res.Exit == nil || res.Stdout == nil || res.Stderr == nil || res.Truncated == nil {
		return res, fmt.Errorf("exec answers no exit, stdout, stderr or truncated")
	}
	return res, nil
}

// execExit runs argv and wants exit code want.
func (r *run) execExit(h string, want int, argv ...string) error {
	res, err := r.exec(h, argv, "", 0)
	if err == nil && *res.Exit != want {
		err = fmt.Errorf("%v exited %d, want %d (stderr %q)", argv, *res.Exit, want, clip(*res.Stderr))
	}
	return err
}

// sh runs a shell line inside the box, with $1 set to arg.
func (r *run) sh(h, line, arg string) (execResult, error) {
	return r.exec(h, []string{"sh", "-c", line, "sh", arg}, "", 0)
}

func clip(s string) string {
	if len(s) > 200 {
		return s[:200] + "..."
	}
	return s
}

// boot is the short box check: a box of this config comes up, answers its
// size and exec, and releases. The names are lifecycle's, which proves them.
func (r *run) boot() {
	if r.kind != "box" {
		r.skip("boot", "box targets only")
		return
	}
	h, err := r.acquireAny(r.longTTL())
	if err == nil {
		err = r.execExit(h.Handle, 0, "true")
	}
	if !r.add("acquire answers handle, expires_at and ttl_s once the box answers exec", err) {
		return
	}
	var want error
	switch {
	case r.desc.Size != nil && h.Size != r.desc.Size.Name:
		want = fmt.Errorf("size %q, describe says %q", h.Size, r.desc.Size.Name)
	case h.Digest == "":
		want = fmt.Errorf("no image_digest")
	}
	r.add("acquire answers its size and image_digest", want)
	r.add("exec runs the command: its stdout, stderr and exit code", r.execRuns(h.Handle))
	rel, err := r.release(h.Handle)
	if err == nil {
		err = wantRelease(rel, true, nil)
	}
	if r.add("release of a ready box: released true, no exit", err) {
		delete(r.live, h.Handle)
	}
}

func (r *run) lifecycleBox() {
	h, err := r.acquireAny(r.longTTL())
	if err == nil {
		err = r.execExit(h.Handle, 0, "true")
	}
	if !r.add("acquire answers handle, expires_at and ttl_s once the box answers exec", err) {
		r.skip("lifecycle", "acquire failed")
		return
	}
	var want error
	switch {
	case r.desc.Size != nil && h.Size != r.desc.Size.Name:
		want = fmt.Errorf("size %q, describe says %q", h.Size, r.desc.Size.Name)
	case h.Digest == "":
		want = fmt.Errorf("no image_digest")
	}
	r.add("acquire answers its size and image_digest", want)
	st, err := r.status(h.Handle)
	if err == nil {
		err = wantState(st, "ready", nil)
	}
	r.add("status answers ready, no exit", err)
	r.add("list shows the handle and its request_id", r.listed(h.Handle, h.key, "ready"))

	r.add("exec runs the command: its stdout, stderr and exit code", r.execRuns(h.Handle))

	res, err := r.exec(h.Handle, []string{"cat"}, "in-"+r.tag, 0)
	if err == nil && *res.Stdout != "in-"+r.tag {
		err = fmt.Errorf("cat answered %q for stdin %q", clip(*res.Stdout), "in-"+r.tag)
	}
	r.add("exec passes stdin", err)

	_, err = r.sh(h.Handle, `printf '%s' "$1" > "$HOME/zmconf-keep"`, r.tag)
	if err == nil {
		res, err = r.sh(h.Handle, `cat "$HOME/zmconf-keep"`, "")
	}
	if err == nil && *res.Stdout != r.tag {
		err = fmt.Errorf("a file one exec wrote reads %q in the next, want %q", clip(*res.Stdout), r.tag)
	}
	r.add("the box keeps its files across exec", err)

	// The command's child holds the output open: a driver killing only the
	// leader answers late, or leaves the child running.
	start := time.Now()
	res, err = r.exec(h.Handle, []string{"sh", "-c", `sleep 8 & echo $! > "$HOME/zmconf-pid"; wait`}, "", 1)
	switch {
	case err != nil:
	case *res.Exit != exitExpired:
		err = fmt.Errorf("exit %d, want %d", *res.Exit, exitExpired)
	case time.Since(start) > 6*time.Second:
		err = fmt.Errorf("answered after %s for timeout_s 1", time.Since(start).Round(time.Second))
	default:
		err = r.execExit(h.Handle, 0, "sh", "-c", `p=$(cat "$HOME/zmconf-pid"); ! kill -0 "$p" 2>/dev/null || ps -o stat= -p "$p" | grep -q Z`)
		if err != nil {
			err = fmt.Errorf("the timed-out command's child is still running: %v", err)
		}
	}
	r.add("exec past timeout_s answers exit 124 in time and ends what the command started", err)

	r.add("exec keeps the first 1 MiB of stdout and of stderr and says truncated", r.execCaps(h.Handle))
	r.execBase64(h.Handle)
	r.boxSecrets(h.Handle)

	again, e, err := r.acquire(h.sent, r.cfg)
	err = must(e, err)
	if err == nil && again.Handle != h.Handle {
		err = fmt.Errorf("handle %q, first answer %q", again.Handle, h.Handle)
	}
	if err == nil {
		err = r.oneFor(h.key)
	}
	r.add("the same request_id gives the same handle and one resource", err)

	left := "zmconf-left-" + r.tag
	dirs, lerr := r.leaveMark(h.Handle, left) // leftover.go
	rel, err := r.release(h.Handle)
	if err == nil {
		err = wantRelease(rel, true, nil)
	}
	if r.add("release of a ready box: released true, no exit", err) {
		delete(r.live, h.Handle)
	}
	r.add("exec after release answers conflict", r.expect(verbExec, h.Handle, args("argv", []string{"true"}), protocol.CodeConflict))
	r.afterEnd(h, "released", nil)

	const wiped = "a new box does not see the last box's files"
	if lerr != nil {
		r.add(wiped, lerr)
		return
	}
	next, err := r.acquireAny(r.longTTL())
	if err == nil {
		err = r.noLeftover(next.Handle, left, dirs)
	}
	r.add(wiped, err)
	r.endHeld(next)
}

// execRuns sends a command whose output no canned answer gives.
func (r *run) execRuns(h string) error {
	res, err := r.sh(h, `printf '%s-%s' "$((6*7))" "$1"; printf oops >&2; exit 5`, r.tag)
	if err == nil {
		switch {
		case *res.Exit != 5:
			err = fmt.Errorf("exit %d, want 5", *res.Exit)
		case *res.Stdout != "42-"+r.tag:
			err = fmt.Errorf("stdout %q, want %q", clip(*res.Stdout), "42-"+r.tag)
		case *res.Stderr != "oops":
			err = fmt.Errorf("stderr %q, want %q", clip(*res.Stderr), "oops")
		case *res.Truncated:
			err = fmt.Errorf("truncated true for 3 bytes")
		}
	}
	return err
}

// codeBox checks a box asked for a ref and commit holds exactly that commit.
func (r *run) codeBox() {
	const head = "a box's checkout is the commit sent, clean"
	if r.opt.Ref == "" || r.opt.Commit == "" {
		r.skip(head, "no --ref and --commit: a box's checkout is optional")
		return
	}
	a, e, err := r.acquire(r.boxArgs(r.key(), r.longTTL(), true), r.cfg)
	err = must(e, err)
	if err == nil && a.Commit != r.opt.Commit {
		err = fmt.Errorf("acquire echoes commit %q, sent %q", a.Commit, r.opt.Commit)
	}
	var res execResult
	if err == nil {
		res, err = r.exec(a.Handle, []string{"git", "rev-parse", "HEAD"}, "", 0)
	}
	if err == nil && strings.TrimSpace(*res.Stdout) != r.opt.Commit {
		err = fmt.Errorf("HEAD in exec's default cwd is %q, sent %q (exit %d, stderr %q)",
			strings.TrimSpace(*res.Stdout), r.opt.Commit, *res.Exit, clip(*res.Stderr))
	}
	if err == nil {
		res, err = r.exec(a.Handle, []string{"git", "status", "--porcelain"}, "", 0)
		if err == nil && (*res.Exit != 0 || strings.TrimSpace(*res.Stdout) != "") {
			err = fmt.Errorf("the checkout is not clean: exit %d, %q", *res.Exit, clip(*res.Stdout))
		}
	}
	r.endHeld(held{acquired: a})
	why := r.unproven()
	switch {
	case err != nil:
		r.add(head, err)
	case why != "":
		r.skip(head, "passed, not proven: "+why)
	default:
		r.add(head, nil)
	}
}

// refuseBox: what a box must refuse that the other kinds have no word for.
func (r *run) refuseBox(refused func(json.RawMessage, map[string]string, ...string) error) {
	m := []any{"request_id", r.key(), "actor", actor, "ttl_s", r.shortTTL(), "image", "zmconf-no-such-image-" + r.tag}
	r.add("an image not in the binding's images answers bad_request", refused(args(m...), r.cfg, protocol.CodeBadRequest))
	const name = "exec refuses a shell string, an empty argv and a timeout_s past 600"
	h, err := r.acquireAny(r.longTTL())
	if err != nil {
		r.add(name, err)
		return
	}
	defer r.endHeld(h)
	err = r.expect(verbExec, h.Handle, args("argv", `touch "$HOME/zmconf-shell-ran"; true`), protocol.CodeBadRequest)
	if err == nil {
		err = r.expect(verbExec, h.Handle, args("argv", []string{}), protocol.CodeBadRequest)
	}
	if err == nil {
		err = r.expect(verbExec, h.Handle, args("argv", []string{"true"}, "timeout_s", 601), protocol.CodeBadRequest)
	}
	if err == nil {
		err = r.execExit(h.Handle, 0, "sh", "-c", `test ! -e "$HOME/zmconf-shell-ran"`)
	}
	r.add(name, err)
}

// capLines is how many numbered 8-byte lines overflow the 1 MiB cap.
const capLines = 140000

// numbered is the start of what `awk` prints below: its first n bytes.
func numbered(n int) string {
	var b strings.Builder
	for i := 0; b.Len() < n; i++ {
		fmt.Fprintf(&b, "%07d\n", i)
	}
	return b.String()[:n]
}

// execCaps sends more than the cap to stdout, then to stderr: each must keep
// exactly its first execCap bytes (numbered lines show which end was kept).
func (r *run) execCaps(h string) error {
	want := numbered(execCap)
	for _, stream := range []string{"stdout", "stderr"} {
		redirect := ""
		if stream == "stderr" {
			redirect = " >&2"
		}
		res, err := r.exec(h, []string{"sh", "-c", fmt.Sprintf(`awk 'BEGIN { for (i = 0; i < %d; i++) printf "%%07d\n", i }'%s`, capLines, redirect)}, "", 0)
		if err != nil {
			return err
		}
		got := *res.Stdout
		if stream == "stderr" {
			got = *res.Stderr
		}
		switch {
		case len(got) != execCap:
			return fmt.Errorf("%s is %d bytes for %d sent, want the cap, %d", stream, len(got), capLines*8, execCap)
		case got != want:
			return fmt.Errorf("%s is not the first %d bytes of what was sent", stream, execCap)
		case !*res.Truncated:
			return fmt.Errorf("truncated false with %s cut", stream)
		}
	}
	return nil
}

// secretsNames is the ZM_SECRETS name rule (SPEC 20 "Boxes", secrets).
const secretsCap = "secrets"

// boxSecrets checks renter secrets on exec: with the capability, each value
// is a 0600 file in a 0700 $ZM_SECRETS directory (or $POTLUCK_SECRETS), in no process's env or
// argv the box can see (best effort: a host-side leak is beyond a black
// box), and the directory is gone once the command ends or times out;
// without it, an exec carrying secrets is refused rather than run without
// them. The value never reaches the suite raw: the box answers its bytes
// in hex, and a mismatch is reported without them, so "no secret on stderr
// or in any answer" (finish) can hold. Bad names sent are watched the same
// way: a refusal names neither values nor names.
func (r *run) boxSecrets(h string) {
	const name = "exec secrets: a 0600 file in a 0700 $ZM_SECRETS, in no env or argv seen in the box, gone after the command"
	const refusals = "exec refuses bad secret names and more than 64 KiB, naming none of them"
	with := func(secrets map[string]string, argv ...string) json.RawMessage {
		return args("argv", argv, "secrets", secrets)
	}
	if r.desc.Capabilities == nil || !slices.Contains(*r.desc.Capabilities, secretsCap) {
		r.add("an exec with secrets on a driver without the secrets capability answers bad_request",
			r.expect(verbExec, h, with(map[string]string{"ZMCONF_A": "x"}, "true"), protocol.CodeBadRequest))
		r.skip(name, "describe lists no secrets capability")
		r.skip("a timed-out command's secrets are gone", "describe lists no secrets capability")
		r.skip(refusals, "describe lists no secrets capability")
		return
	}
	val := "zmconf-secret-" + r.tag + "-" + randomTag()
	r.secrets = append(r.secrets, val)
	r.sawSecret = true
	// ": zmconfmark" puts a marker in this command's own argv: a ps that
	// fails or sees nothing (no -ww/-e) cannot pass by printing "ps 0".
	line := `: zmconfmark; d=${POTLUCK_SECRETS:-$ZM_SECRETS}; f="$d/ZMCONF_A"; printf '%s\n' "$d"; od -An -tx1 "$f" | tr -d ' \n'; echo
ls -ld "$d" | cut -c1-10; ls -l "$f" | cut -c1-10
env | awk -v f="$f" 'BEGIN { getline v < f } index($0, v) { n++ } END { print "env " n+0 }'
p=$(ps -ww -eo args) || exit 9
printf '%s\n' "$p" | grep -q zmconfmark || exit 9
printf '%s\n' "$p" | awk -v f="$f" 'BEGIN { getline v < f } index($0, v) { n++ } END { print "ps " n+0 }'
cat /proc/[0-9]*/environ 2>/dev/null | tr '\0' '\n' | awk -v f="$f" 'BEGIN { getline v < f } index($0, v) { n++ } END { print "environ " n+0 }'`
	var res execResult
	err := must(r.call(verbExec, h, with(map[string]string{"ZMCONF_A": val}, "sh", "-c", line), &res))
	if err == nil && (res.Exit == nil || res.Stdout == nil) {
		err = fmt.Errorf("exec answers no exit or stdout")
	}
	var dir string
	if err == nil {
		got := strings.Split(strings.TrimRight(*res.Stdout, "\n"), "\n")
		want := []string{"", hex.EncodeToString([]byte(val)), "drwx------", "-rw-------", "env 0", "ps 0", "environ 0"}
		what := []string{"ZM_SECRETS", "the file's bytes", "the directory's mode", "the file's mode",
			"the value in the command's env", "the value in a process's argv", "the value in a process's environ"}
		switch {
		case *res.Exit == 9:
			err = fmt.Errorf("ps failed or did not show this command (the box needs ps -ww -eo args)")
		case *res.Exit != 0 || len(got) != len(want):
			err = fmt.Errorf("exit %d, %d lines of output, want 0 and %d (stderr %q)", *res.Exit, len(got), len(want), clip(deref3(res.Stderr)))
		case !strings.HasPrefix(got[0], "/"):
			err = fmt.Errorf("ZM_SECRETS is %q, want an absolute directory", got[0])
		default:
			dir = got[0]
			for i := 1; i < len(want) && err == nil; i++ {
				switch {
				case got[i] == want[i]:
				case i == 1:
					err = fmt.Errorf("the file does not hold the value sent")
				default:
					err = fmt.Errorf("%s: %q, want %q", what[i], got[i], want[i])
				}
			}
		}
	}
	if err == nil {
		if err = r.execExit(h, 0, "sh", "-c", `test ! -e "$1"`, "sh", dir); err != nil {
			err = fmt.Errorf("%s is still there after the command: %v", dir, err)
		}
	}
	if err == nil {
		if err = r.execExit(h, 0, "sh", "-c", `test -z "${ZM_SECRETS:-}${POTLUCK_SECRETS:-}"`); err != nil {
			err = fmt.Errorf("an exec without secrets has ZM_SECRETS or POTLUCK_SECRETS set: %v", err)
		}
	}
	r.add(name, err)

	res, err = r.exec2(h, with(map[string]string{"ZMCONF_A": val}, "sh", "-c", `printf '%s' "${POTLUCK_SECRETS:-$ZM_SECRETS}" > "$HOME/zmconf-sdir"; sleep 8`), 1)
	if err == nil && *res.Exit != exitExpired {
		err = fmt.Errorf("exit %d, want %d", *res.Exit, exitExpired)
	}
	if err == nil {
		if err = r.execExit(h, 0, "sh", "-c", `d=$(cat "$HOME/zmconf-sdir") && [ -n "$d" ] && test ! -e "$d"`); err != nil {
			err = fmt.Errorf("the directory outlived the timeout: %v", err)
		}
	}
	r.add("a timed-out command's secrets are gone", err)

	bad := []string{"zmconf bad " + r.tag, "", "Z" + strings.Repeat("A", 64), "ZMCONF_NL\n", "zmconf_lower"}
	r.secrets = append(r.secrets, bad[0], bad[2], "ZMCONF_NL", "zmconf_lower")
	err = nil
	for _, n := range bad {
		if err = r.expect(verbExec, h, with(map[string]string{n: "x"}, "true"), protocol.CodeBadRequest); err != nil {
			err = fmt.Errorf("name %q: %v", n, err)
			break
		}
	}
	full := strings.Repeat("y", 64<<10-len("ZMCONF_B"))
	if err == nil {
		err = r.expect(verbExec, h, with(map[string]string{"ZMCONF_B": full + "y"}, "true"), protocol.CodeBadRequest)
	}
	if err == nil {
		res, err = r.exec2(h, with(map[string]string{"ZMCONF_B": full}, "true"), 0)
		if err == nil && *res.Exit != 0 {
			err = fmt.Errorf("exactly 64 KiB of names and values: exit %d, want 0", *res.Exit)
		}
	}
	r.add(refusals, err)
}

// exec2 sends one exec with prepared args (and timeout_s when positive).
func (r *run) exec2(h string, a json.RawMessage, timeout int) (execResult, error) {
	if timeout > 0 {
		var m map[string]any
		_ = json.Unmarshal(a, &m)
		m["timeout_s"] = timeout
		a, _ = json.Marshal(m)
	}
	var res execResult
	if err := must(r.call(verbExec, h, a, &res)); err != nil {
		return res, err
	}
	if res.Exit == nil || res.Stdout == nil || res.Stderr == nil || res.Truncated == nil {
		return res, fmt.Errorf("exec answers no exit, stdout, stderr or truncated")
	}
	return res, nil
}

func deref3(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
