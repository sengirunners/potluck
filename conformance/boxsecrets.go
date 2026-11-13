package conformance

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/sengirunners/potluck/protocol"
)

// Group box-secrets (docs/protocol.md, Box secrets): the secrets verb puts
// a secret in a box for the box's life. Each value is a 0600 file in a 0700
// directory on a memory filesystem, which every later exec finds in
// $POTLUCK_SECRETS; a renewal replaces a file whole; unset removes one; the
// directory goes with the box. What the box sees is seen through exec; what
// the host keeps, through the driver's state directory (Options.StateDirs).
// Values never reach the suite raw: the box answers their bytes in hex.

const (
	bsRefused  = "a driver without capability box-secrets refuses the secrets verb and lands nothing"
	bsModes    = "box secrets: each value a 0600 file in a 0700 directory, read back whole"
	bsMemory   = "box secrets: the directory is on a memory filesystem (tmpfs or ramfs)"
	bsSwap     = "box secrets: the guest swaps nowhere but zram, so the memory filesystem never reaches a disk"
	bsOwner    = "box secrets: the directory belongs to the user commands run as"
	bsPlanted  = "box secrets: a symlink planted at the directory's path is refused, its target gets nothing, and no command is pointed at it"
	bsForeign  = "box secrets: another user's directory planted at the path is refused, and no command is pointed at it"
	bsStop     = "box secrets: a kept box's stop drops them, and it starts with none"
	bsVisible  = "box secrets: a later exec finds them at $POTLUCK_SECRETS, the dir secrets answered"
	bsNoEnv    = "box secrets: the value is in no process's environment or argv in the box"
	bsNoState  = "box secrets: the value is in no file under the driver's state directory"
	bsAtomic   = "box secrets: a renewal replaces the file whole, never seen half written"
	bsUnset    = "box secrets: unset removes one name and keeps the others"
	bsOwn      = "box secrets: an exec's own secrets get a directory of their own and leave the box's in place"
	bsRefuse   = "box secrets: bad names, an empty call and more than 64 KiB held answer bad_request and change nothing"
	bsConflict = "box secrets: refused with conflict once the box is released"
	bsGone     = "box secrets: gone with the box, and a new box does not see them"
)

// bsChecks are group box-secrets' checks (isolation: what share's quick
// check runs), bsOpsChecks group box-secrets-ops' (what the verb does).
var (
	bsChecks    = []string{bsVisible, bsModes, bsMemory, bsOwner, bsSwap, bsNoEnv, bsPlanted, bsForeign, bsNoState, bsGone}
	bsOpsChecks = []string{bsUnset, bsOwn, bsAtomic, bsRefuse, bsConflict}
)

// boxSecretsAnswer is the secrets verb's answer.
type boxSecretsAnswer struct {
	Dir   *string `json:"dir"`
	Count *int    `json:"count"`
}

// setBoxSecrets sends one secrets call and checks the answer's shape.
func (r *run) setBoxSecrets(h string, a map[string]any) (boxSecretsAnswer, error) {
	var res boxSecretsAnswer
	if err := must(r.call(protocol.VerbSecrets, h, args(flat(a)...), &res)); err != nil {
		return res, err
	}
	if res.Dir == nil || res.Count == nil || !strings.HasPrefix(*res.Dir, "/") {
		return res, fmt.Errorf("secrets answers no absolute dir and count")
	}
	return res, nil
}

func flat(m map[string]any) []any {
	var kv []any
	for k, v := range m {
		kv = append(kv, k, v)
	}
	return kv
}

// boxSecretsProbe runs in the box with $1 the directory secrets answered.
// Its lines: $POTLUCK_SECRETS; ZMCONF_A's bytes in hex; the directory's and
// the file's modes; the filesystem's type (stat -f, as the guest reports it:
// a driver that fakes it inside the guest is beyond a black box); whether
// the directory's owner is the user commands run as; how many swap areas
// the guest has that are not zram (/proc/swaps, else meminfo's SwapTotal);
// and how many places in the box hold the value: env, any process's argv
// (ps), /proc/*/environ. ": zmconfmark" puts a marker in its own argv, so a
// ps that sees nothing cannot pass.
const boxSecretsProbe = `: zmconfmark; printf '%s\n' "${POTLUCK_SECRETS:-}"; d=$1; f="$d/ZMCONF_A"
od -An -tx1 "$f" | tr -d ' \n'; echo
ls -ld "$d" | cut -c1-10; ls -l "$f" | cut -c1-10
t=$(stat -f -c %T "$d" 2>/dev/null) || t=
printf 'fs %s\n' "${t:-unknown}"
o=$(ls -ldn "$d" | awk '{ print $3 }'); u=$(id -u)
if [ -n "$o" ] && [ "$o" = "$u" ]; then echo "owner yes"; else echo "owner $o, commands run as $u"; fi
if s=$(cat /proc/swaps 2>/dev/null); then
	printf '%s\n' "$s" | awk 'NR > 1 && $1 !~ /^\/dev\/zram[0-9]+$/ { n++ } END { print "swap " n+0 }'
else
	awk '/^SwapTotal:/ { print ($2 == 0 ? "swap 0" : "swap total " $2 " kB"); f = 1 } END { if (!f) print "swap unknown" }' /proc/meminfo 2>/dev/null || echo "swap unknown"
fi
env | awk -v f="$f" 'BEGIN { getline v < f } index($0, v) { n++ } END { print "env " n+0 }'
p=$(ps -ww -eo args) || exit 9
printf '%s\n' "$p" | grep -q zmconfmark || exit 9
printf '%s\n' "$p" | awk -v f="$f" 'BEGIN { getline v < f } index($0, v) { n++ } END { print "ps " n+0 }'
cat /proc/[0-9]*/environ 2>/dev/null | tr '\0' '\n' | awk -v f="$f" 'BEGIN { getline v < f } index($0, v) { n++ } END { print "environ " n+0 }'`

// boxSecretsReader reads $1/ZMCONF_R for 5 s while the suite replaces it
// with 24 KiB of a, then of b, and back: every read must be one whole value.
const boxSecretsReader = `f=$1/ZMCONF_R; n=0; a=0; b=0; bad=0
A=$(dd if=/dev/zero bs=1024 count=24 2>/dev/null | tr '\0' a); B=$(dd if=/dev/zero bs=1024 count=24 2>/dev/null | tr '\0' b)
end=$(( $(date +%s) + 5 ))
while [ $((n % 20)) -ne 0 ] || [ "$(date +%s)" -lt "$end" ]; do
	v=$(cat "$f" 2>/dev/null); n=$((n + 1))
	if [ "$v" = "$A" ]; then a=$((a + 1)); elif [ "$v" = "$B" ]; then b=$((b + 1)); else bad=$((bad + 1)); fi
done
echo "reads $n a $a b $b bad $bad"`

const bsReaderKiB = 24

func (r *run) boxSecretsGroup() {
	if r.kind != "box" { // as pty: nothing to say for a run or a db
		return
	}
	caps := boxCaps(r)
	if !slices.Contains(caps, protocol.CapBoxSecrets) {
		r.boxSecretsRefused()
		for _, c := range bsChecks {
			r.skip(c, "describe lists no box-secrets capability")
		}
		return
	}
	r.skip(bsRefused, "the driver lists box-secrets")
	h, dir, val, other, ok := r.boxSecretsBox(bsChecks)
	if !ok {
		return
	}
	defer func() { r.endHeld(h) }()
	r.boxSecretsLanding(h.Handle, dir, val)
	leak := r.stateHolds(dir, val, other)
	r.boxSecretsPlanted(h.Handle, dir, caps)
	_, held := r.setBoxSecrets(h.Handle, map[string]any{"secrets": map[string]string{"ZMCONF_A": val}}) // the box holds a value as it ends

	rel, err := r.release(h.Handle)
	if err == nil {
		err = wantRelease(rel, true, nil)
	}
	if err != nil {
		r.add(bsGone, fmt.Errorf("release: %v", err))
		r.addStateCheck(leak, nil)
		return
	}
	delete(r.live, h.Handle)
	after := r.stateHolds(dir, val, other)
	r.addStateCheck(leak, &after)

	next, err := r.acquireAny(r.longTTL())
	if err == nil {
		err = r.execExit(next.Handle, 0, "sh", "-c", `test ! -e "$1" && test -z "${POTLUCK_SECRETS:-}${ZM_SECRETS:-}"`, "sh", dir)
		r.endHeld(next)
	}
	if held != nil {
		err = fmt.Errorf("setting a value to hold at the release: %v", held)
	}
	if err != nil {
		err = fmt.Errorf("after release a new box finds %s or a secrets variable: %v", dir, err)
	}
	r.add(bsGone, err)
}

// boxSecretsOpsGroup is group box-secrets-ops: what the verb does beyond
// isolation (unset, an exec's own secrets beside the box's, atomic replace,
// refusals, conflict after release). Slower (a 5 s reader), and the
// driver's own code on every size.
func (r *run) boxSecretsOpsGroup() {
	if r.kind != "box" {
		return
	}
	caps := boxCaps(r)
	if !slices.Contains(caps, protocol.CapBoxSecrets) {
		for _, c := range bsOpsChecks {
			r.skip(c, "describe lists no box-secrets capability")
		}
		return
	}
	h, dir, _, _, ok := r.boxSecretsBox(bsOpsChecks)
	if !ok {
		return
	}
	defer func() { r.endHeld(h) }()
	r.boxSecretsUnset(h.Handle, dir)
	r.boxSecretsOwn(h.Handle, dir, caps)
	r.boxSecretsAtomic(h.Handle, dir)
	r.boxSecretsRefusals(h.Handle, dir)
	rel, err := r.release(h.Handle)
	if err == nil {
		err = wantRelease(rel, true, nil)
	}
	if err != nil {
		r.add(bsConflict, fmt.Errorf("release: %v", err))
		return
	}
	delete(r.live, h.Handle)
	r.add(bsConflict, r.expect(protocol.VerbSecrets, h.Handle, args("secrets", map[string]string{"ZMCONF_A": "x"}), protocol.CodeConflict))
}

func boxCaps(r *run) []string {
	if r.desc.Capabilities != nil {
		return *r.desc.Capabilities
	}
	return []string{}
}

// boxSecretsBox acquires a box and sets ZMCONF_A and ZMCONF_B in it,
// failing checks when it cannot.
func (r *run) boxSecretsBox(checks []string) (held, string, string, string, bool) {
	h, err := r.acquireAny(r.longTTL())
	if err != nil {
		for _, c := range checks {
			r.add(c, fmt.Errorf("acquire a box for box secrets: %v", err))
		}
		return h, "", "", "", false
	}
	val := "zmconf-boxsecret-" + r.tag + "-" + randomTag()
	other := "zmconf-boxother-" + r.tag + "-" + randomTag()
	r.secrets = append(r.secrets, val, other)
	r.sawSecret = true // finish: no value on stderr or in any answer
	res, err := r.setBoxSecrets(h.Handle, map[string]any{"secrets": map[string]string{"ZMCONF_A": val, "ZMCONF_B": other}})
	if err == nil && *res.Count != 2 {
		err = fmt.Errorf("count %d after setting 2 names, want 2", *res.Count)
	}
	if err != nil {
		for _, c := range checks {
			r.add(c, fmt.Errorf("secrets: %v", err))
		}
		r.endHeld(h)
		return h, "", "", "", false
	}
	return h, *res.Dir, val, other, true
}

// boxSecretsRefused: a driver that does not list box-secrets refuses the
// verb (not_implemented, or bad_request), and no later exec sees a secrets
// directory.
func (r *run) boxSecretsRefused() {
	h, err := r.acquireAny(r.longTTL())
	if err != nil {
		r.add(bsRefused, fmt.Errorf("acquire: %v", err))
		return
	}
	defer r.endHeld(h)
	val := "zmconf-unlisted-" + r.tag + "-" + randomTag()
	r.secrets = append(r.secrets, val)
	r.sawSecret = true
	err = r.expect(protocol.VerbSecrets, h.Handle, args("secrets", map[string]string{"ZMCONF_A": val}), protocol.CodeNotImplemented, protocol.CodeBadRequest)
	if err == nil {
		if err = r.execExit(h.Handle, 0, "sh", "-c", `test -z "${POTLUCK_SECRETS:-}${ZM_SECRETS:-}"`); err != nil {
			err = fmt.Errorf("after a refused secrets call an exec has a secrets variable: %v", err)
		}
	}
	r.add(bsRefused, err)
}

// boxSecretsLanding runs the probe once and judges its lines.
func (r *run) boxSecretsLanding(h, dir, val string) {
	res, err := r.sh(h, boxSecretsProbe, dir)
	var got []string
	if err == nil {
		got = strings.Split(strings.TrimRight(*res.Stdout, "\n"), "\n")
		switch {
		case *res.Exit == 9:
			err = fmt.Errorf("ps failed or did not show this command (the box needs ps -ww -eo args)")
		case *res.Exit != 0 || len(got) != 10:
			err = fmt.Errorf("the probe: exit %d, %d lines, want 0 and 10 (stderr %q)", *res.Exit, len(got), clip(deref3(res.Stderr)))
		}
	}
	if err != nil {
		for _, c := range []string{bsVisible, bsModes, bsMemory, bsOwner, bsSwap, bsNoEnv} {
			r.add(c, err)
		}
		return
	}
	var visible error
	if got[0] != dir {
		visible = fmt.Errorf("POTLUCK_SECRETS is %q in a later exec, secrets answered %q", got[0], dir)
	}
	r.add(bsVisible, visible)
	var modes error
	switch {
	case got[1] != hex.EncodeToString([]byte(val)):
		modes = fmt.Errorf("the file does not hold the value sent")
	case got[2] != "drwx------":
		modes = fmt.Errorf("the directory's mode is %q, want drwx------", got[2])
	case got[3] != "-rw-------":
		modes = fmt.Errorf("the file's mode is %q, want -rw-------", got[3])
	}
	r.add(bsModes, modes)
	var mem error
	if fs := strings.TrimPrefix(got[4], "fs "); fs != "tmpfs" && fs != "ramfs" {
		mem = fmt.Errorf("the directory is on %q, not a memory filesystem", fs)
	}
	r.add(bsMemory, mem)
	var owner error
	if got[5] != "owner yes" {
		owner = fmt.Errorf("%s", got[5])
	}
	r.add(bsOwner, owner)
	var swap error
	if got[6] != "swap 0" {
		swap = fmt.Errorf("%s: a tmpfs page can be swapped out to a disk", got[6])
	}
	r.add(bsSwap, swap)
	var env error
	for i, want := range []string{"env 0", "ps 0", "environ 0"} {
		if got[7+i] != want {
			env = fmt.Errorf("%s: want %q (the value is held there)", got[7+i], want)
			break
		}
	}
	r.add(bsNoEnv, env)
}

// boxSecretsUnset removes ZMCONF_B and keeps ZMCONF_A.
func (r *run) boxSecretsUnset(h, dir string) {
	res, err := r.setBoxSecrets(h, map[string]any{"unset": []string{"ZMCONF_B"}})
	if err == nil && *res.Count != 1 {
		err = fmt.Errorf("count %d after unsetting 1 of 2, want 1", *res.Count)
	}
	if err == nil {
		if err = r.execExit(h, 0, "sh", "-c", `test ! -e "$1/ZMCONF_B" && test -f "$1/ZMCONF_A"`, "sh", dir); err != nil {
			err = fmt.Errorf("after unset: %v", err)
		}
	}
	r.add(bsUnset, err)
}

// boxSecretsOwn: an exec with secrets of its own gets its own directory,
// and the box's stays, with its file, for the next.
func (r *run) boxSecretsOwn(h, dir string, caps []string) {
	if !slices.Contains(caps, secretsCap) {
		r.skip(bsOwn, "describe lists no secrets capability: an exec takes none of its own")
		return
	}
	own := "zmconf-own-" + r.tag + "-" + randomTag()
	r.secrets = append(r.secrets, own)
	res, err := r.exec2(h, args("argv", []string{"sh", "-c", `[ "$POTLUCK_SECRETS" != "$1" ] && test -f "$POTLUCK_SECRETS/ZMCONF_O" && test ! -e "$1/ZMCONF_O"`, "sh", dir},
		"secrets", map[string]string{"ZMCONF_O": own}), 0)
	if err == nil && *res.Exit != 0 {
		err = fmt.Errorf("an exec with its own secrets found them in the box's directory, or not at all (exit %d)", *res.Exit)
	}
	if err == nil {
		if err = r.execExit(h, 0, "sh", "-c", `[ "$POTLUCK_SECRETS" = "$1" ] && test -f "$1/ZMCONF_A"`, "sh", dir); err != nil {
			err = fmt.Errorf("after an exec with its own secrets the box's are gone: %v", err)
		}
	}
	r.add(bsOwn, err)
}

// boxSecretsAtomic replaces ZMCONF_R again and again while a reader in the
// box reads it: every read is one whole value, and both values are seen.
func (r *run) boxSecretsAtomic(h, dir string) {
	a, b := strings.Repeat("a", bsReaderKiB<<10), strings.Repeat("b", bsReaderKiB<<10)
	if _, err := r.setBoxSecrets(h, map[string]any{"secrets": map[string]string{"ZMCONF_R": a}}); err != nil {
		r.add(bsAtomic, err)
		return
	}
	type read struct {
		res execResult
		err error
	}
	done := make(chan read, 1)
	go func() {
		res, err := r.exec(h, []string{"sh", "-c", boxSecretsReader, "sh", dir}, "", 20)
		done <- read{res, err}
	}()
	time.Sleep(700 * time.Millisecond) // the reader is reading the first value
	writes, i := 0, 0
	var werr error
	var got read
	for {
		select {
		case got = <-done:
		default:
			v := b
			if i%2 == 1 {
				v = a
			}
			i++
			if _, err := r.setBoxSecrets(h, map[string]any{"secrets": map[string]string{"ZMCONF_R": v}}); err != nil && werr == nil {
				werr = err
			}
			writes++
			time.Sleep(100 * time.Millisecond)
			continue
		}
		break
	}
	err := got.err
	switch {
	case err != nil:
	case werr != nil:
		err = fmt.Errorf("a renewal: %v", werr)
	case *got.res.Exit != 0:
		err = fmt.Errorf("the reader: exit %d (stderr %q)", *got.res.Exit, clip(deref3(got.res.Stderr)))
	default:
		var n, na, nb, bad int
		if _, serr := fmt.Sscanf(strings.TrimSpace(*got.res.Stdout), "reads %d a %d b %d bad %d", &n, &na, &nb, &bad); serr != nil {
			err = fmt.Errorf("the reader answered %q", clip(*got.res.Stdout))
		} else if bad > 0 || na == 0 || nb == 0 {
			err = fmt.Errorf("%d reads over %d renewals: %d of one value, %d of the other, %d neither (missing or half written)", n, writes, na, nb, bad)
		}
	}
	r.add(bsAtomic, err)
}

// boxSecretsRefusals: bad names (set and unset), an empty call, one call
// past 64 KiB, and a call within it that takes the box past 64 KiB held,
// all bad_request; the last leaves nothing behind. The names sent count as
// secrets: finish fails a driver that quotes them.
func (r *run) boxSecretsRefusals(h, dir string) {
	bad := []string{"zmconf bad " + r.tag, "", "Z" + strings.Repeat("A", 64), "ZMCONF_NL\n", "zmconf_lower"}
	r.secrets = append(r.secrets, bad[0], bad[2], "ZMCONF_NL", "zmconf_lower")
	var err error
	for _, n := range bad {
		if err = r.expect(protocol.VerbSecrets, h, args("secrets", map[string]string{n: "x"}), protocol.CodeBadRequest); err != nil {
			err = fmt.Errorf("name %q: %v", n, err)
			break
		}
	}
	if err == nil {
		if err = r.expect(protocol.VerbSecrets, h, args("unset", []string{"zmconf_lower"}), protocol.CodeBadRequest); err != nil {
			err = fmt.Errorf("unset of a bad name: %v", err)
		}
	}
	if err == nil {
		if err = r.expect(protocol.VerbSecrets, h, args(), protocol.CodeBadRequest); err != nil {
			err = fmt.Errorf("an empty call: %v", err)
		}
	}
	if err == nil {
		if err = r.expect(protocol.VerbSecrets, h, args("secrets", map[string]string{"ZMCONF_BIG": strings.Repeat("y", 64<<10)}), protocol.CodeBadRequest); err != nil {
			err = fmt.Errorf("one call past 64 KiB: %v", err)
		}
	}
	if err == nil { // ZMCONF_A and the reader's 24 KiB are held: 48 KiB more is past 64 KiB in all
		if err = r.expect(protocol.VerbSecrets, h, args("secrets", map[string]string{"ZMCONF_BIG": strings.Repeat("y", 48<<10)}), protocol.CodeBadRequest); err != nil {
			err = fmt.Errorf("a call that takes the box past 64 KiB held: %v", err)
		}
	}
	if err == nil {
		if err = r.execExit(h, 0, "sh", "-c", `test ! -e "$1/ZMCONF_BIG" && test -f "$1/ZMCONF_A"`, "sh", dir); err != nil {
			err = fmt.Errorf("a refused call changed the directory: %v", err)
		}
	}
	r.add(bsRefuse, err)
}

// stateDirs are the host directories the driver keeps its records and logs
// in: Options.StateDirs, else config state_dir and the conventional
// ~/.potluck-driver-<name> (or ~/.zm-run-<name>) that exist.
func (r *run) stateDirs() []string {
	ds := r.opt.StateDirs
	if ds == nil {
		if s := r.cfg["state_dir"]; s != "" {
			ds = append(ds, s)
		}
		if home, err := os.UserHomeDir(); err == nil {
			if name := protocol.DriverProgram(filepath.Base(r.path)); name != "" {
				ds = append(ds, protocol.DriverStateDir(home, name))
			}
		}
	}
	var out []string
	for _, d := range ds {
		if st, err := os.Stat(d); err == nil && st.IsDir() && !slices.Contains(out, d) {
			out = append(out, d)
		}
	}
	return out
}

// stateScanMax bounds what stateHolds reads: per file and in all.
const (
	stateFileMax  = 16 << 20
	stateTotalMax = 512 << 20
)

// stateHolds looks for vals in every file under the driver's state
// directories, outside skip (the box's own secrets directory: a driver whose
// guest is this machine keeps it there), and names the first holding one.
func (r *run) stateHolds(skip string, vals ...string) stateScan {
	dirs := r.stateDirs()
	if len(dirs) == 0 {
		return stateScan{}
	}
	total := int64(0)
	sc := stateScan{looked: true}
	for _, d := range dirs {
		_ = filepath.WalkDir(d, func(p string, e fs.DirEntry, err error) error {
			if err != nil || sc.found != "" {
				return nil
			}
			if p == skip || strings.HasPrefix(p, skip+"/") {
				if e.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if !e.Type().IsRegular() {
				return nil
			}
			fi, err := e.Info()
			if err != nil || fi.Size() > stateFileMax || total+fi.Size() > stateTotalMax {
				return nil
			}
			total += fi.Size()
			b, err := os.ReadFile(p)
			if err != nil {
				return nil
			}
			for _, v := range vals {
				if bytes.Contains(b, []byte(v)) {
					sc.found = p
				}
			}
			return nil
		})
	}
	return sc
}

type stateScan struct {
	looked bool
	found  string
}

// addStateCheck judges the state scans made while the box held its
// secrets and after its release.
func (r *run) addStateCheck(live stateScan, after *stateScan) {
	switch {
	case !live.looked:
		r.skip(bsNoState, "no state directory to look in: give --state-dir (or config state_dir)")
	case live.found != "":
		r.add(bsNoState, fmt.Errorf("%s holds a value while the box lives", live.found))
	case after != nil && after.found != "":
		r.add(bsNoState, fmt.Errorf("%s holds a value after the release", after.found))
	default:
		r.add(bsNoState, nil)
	}
}

// boxSecretsPlanted plants what a box's own commands could put at the
// directory's path (its path is fixed for the box's life) once the box
// holds no name: a symlink to a directory of the box user's, then, with
// root, a directory of root's. A secrets call must be refused, write nothing
// through the link, and no later exec may be pointed at either.
func (r *run) boxSecretsPlanted(h, dir string, caps []string) {
	clear := func() error {
		_, err := r.setBoxSecrets(h, map[string]any{"unset": []string{"ZMCONF_A", "ZMCONF_B"}})
		if err == nil {
			err = r.execExit(h, 0, "sh", "-c", `rm -rf "$1" && test ! -e "$1"`, "sh", dir)
		}
		return err
	}
	unused := func(e *protocol.Error, err error, what string) error {
		switch {
		case err != nil:
			return err
		case e == nil:
			return fmt.Errorf("a secrets call with %s at the path answered ok", what)
		}
		if err := r.execExit(h, 0, "sh", "-c", `[ "${POTLUCK_SECRETS:-}" != "$1" ] && [ "${ZM_SECRETS:-}" != "$1" ]`, "sh", dir); err != nil {
			return fmt.Errorf("a later exec is pointed at %s: %v", what, err)
		}
		return nil
	}
	val := "zmconf-planted-" + r.tag + "-" + randomTag()
	r.secrets = append(r.secrets, val)
	set := args("secrets", map[string]string{"ZMCONF_P": val})
	err := clear()
	if err == nil {
		err = r.execExit(h, 0, "sh", "-c", `mkdir -p "$HOME/zmconf-target" && ln -s "$HOME/zmconf-target" "$1"`, "sh", dir)
	}
	if err == nil {
		e, cerr := r.call(protocol.VerbSecrets, h, set, nil)
		err = unused(e, cerr, "a symlink")
		if lerr := r.execExit(h, 0, "sh", "-c", `test -z "$(ls -A "$HOME/zmconf-target")"`); err == nil && lerr != nil {
			err = fmt.Errorf("the symlink's target got a file: %v", lerr)
		}
	}
	if cerr := r.execExit(h, 0, "sh", "-c", `rm -f "$1"; rm -rf "$HOME/zmconf-target"`, "sh", dir); err == nil {
		err = cerr
	}
	r.add(bsPlanted, err)

	if !slices.Contains(caps, protocol.CapRoot) {
		r.skip(bsForeign, "describe lists no root capability: the box user cannot make another user's directory")
		return
	}
	if res, err := r.sh(h, `id -u`, ""); err != nil || strings.TrimSpace(deref3(res.Stdout)) == "0" {
		r.skip(bsForeign, "commands run as root: no other user to plant a directory for")
		return
	}
	err = r.execExit(h, 0, "sh", "-c", `sudo -n mkdir -m 0700 "$1" && sudo -n chown 0:0 "$1"`, "sh", dir)
	if err == nil {
		e, cerr := r.call(protocol.VerbSecrets, h, set, nil)
		err = unused(e, cerr, "root's directory")
	}
	if cerr := r.execExit(h, 0, "sh", "-c", `sudo -n rm -rf "$1"`, "sh", dir); err == nil {
		err = cerr
	}
	r.add(bsForeign, err)
}

// boxSecretsStopped: with box-secrets and persist both listed, a kept box
// set a secret before its stop starts again with none (its guest's memory
// went with the stop). Called by persist around the first stop and start.
func (r *run) boxSecretsBeforeStop(h string) (string, error) {
	if r.desc.Capabilities == nil || !slices.Contains(*r.desc.Capabilities, protocol.CapBoxSecrets) {
		return "", nil
	}
	val := "zmconf-kept-" + r.tag + "-" + randomTag()
	r.secrets = append(r.secrets, val)
	r.sawSecret = true
	res, err := r.setBoxSecrets(h, map[string]any{"secrets": map[string]string{"ZMCONF_K": val}})
	if err != nil {
		return "", err
	}
	return *res.Dir, nil
}

func (r *run) boxSecretsAfterStart(h, dir string, before error) {
	if dir == "" && before == nil {
		r.skip(bsStop, "describe lists no box-secrets capability")
		return
	}
	err := before
	if err == nil {
		if err = r.execExit(h, 0, "sh", "-c", `test ! -e "$1/ZMCONF_K" && test -z "${POTLUCK_SECRETS:-}${ZM_SECRETS:-}"`, "sh", dir); err != nil {
			err = fmt.Errorf("after stop and start the box still holds its secrets: %v", err)
		}
	}
	r.add(bsStop, err)
}
