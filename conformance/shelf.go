package conformance

import (
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sengirunners/potluck/protocol"
)

// The package shelf (docs/protocol.md, The package shelf; zm-eakh.12.30): a
// read-only tree every box of the binding sees at describe.shelf.path. Only
// the host writes it. A box may name what it wants in $HOME/.potluck/wants;
// it never places a byte. The group proves what a box cannot do to it: write
// it, reach another box through it, get its own bytes onto it, or leave its
// links and wants to a later box.

// shelfLook resolves the shelf path given as $1 to $S, a physical path
// (relative paths are under HOME; a symlink is followed, so find walks the
// tree it names), and stops with exit 3 when there is no shelf there or the
// box cannot list it. Writes are read from what a script prints, never from
// its exit, so a setup failure is never taken for a refused write.
const shelfLook = `case "$1" in /*) S=$1 ;; *) S=$HOME/$1 ;; esac
[ -d "$S" ] || { echo "no shelf at $S" >&2; exit 3; }
[ -r "$S" ] && [ -x "$S" ] || { echo "the shelf at $S cannot be listed" >&2; exit 3; }
S=$(cd "$S" && pwd -P) || exit 3
`

// shelfWants is the wants file, relative to HOME.
const shelfWants = protocol.ShelfWants

// shelfOn says whether describe lists capability shelf; when it does, path
// is describe.shelf.path, or an error when describe gives none or a path
// that is not clean (relative ones may not climb out of HOME).
func (r *run) shelfOn() (on bool, p string, err error) {
	if r.desc.Capabilities == nil || !slices.Contains(*r.desc.Capabilities, protocol.CapShelf) {
		return false, "", nil
	}
	if r.desc.Shelf == nil || r.desc.Shelf.Path == "" {
		return true, "", fmt.Errorf("describe lists capability shelf, with shelf %+v: want shelf.path, where a box finds it", r.desc.Shelf)
	}
	p = r.desc.Shelf.Path
	if path.Clean(p) != p || p == "/" || strings.ContainsAny(p, "\x00\n'\"$`\\*?[") || !path.IsAbs(p) && (p == "." || p == ".." || strings.HasPrefix(p, "../")) {
		return true, "", fmt.Errorf("describe.shelf.path %q: want a clean absolute path, or one under HOME", p)
	}
	return true, p, nil
}

func (r *run) shelf() {
	if r.kind != "box" {
		return
	}
	on, p, err := r.shelfOn()
	if !on {
		r.skip("the package shelf", "describe lists no shelf capability")
		return
	}
	if !r.add("describe's shelf says where a box finds it", err) {
		return
	}
	// The shared check first: on a driver whose boxes share one writable
	// copy, the refusal check's writes would leave it nothing to compare.
	r.add("a write to the shelf path reaches no other box (a live one, nor a later one)", r.shelfShared(p))
	r.add("the shelf refuses writes (create, overwrite, delete, chmod; as root too where root is on)", r.shelfRefuses(p))
	r.add("the shelf takes nothing a box wrote (a blob in its own cache, named in its wants)", r.shelfHarvest(p))
	r.add("a shelf box leaves nothing behind (no links from the shelf, no wants file)", r.shelfLeaves(p))
}

// shelfSh runs a shell line in box h with $1 the shelf path and $2.. args.
func (r *run) shelfSh(h, line, p string, a ...string) (execResult, error) {
	res, err := r.exec(h, append([]string{"sh", "-c", shelfLook + line, "sh", p}, a...), "", 0)
	if err == nil && *res.Exit != 0 {
		err = fmt.Errorf("a shelf command exited %d: %s", *res.Exit, clip(strings.TrimSpace(*res.Stderr)))
	}
	return res, err
}

// shelfFile is a regular file on the shelf, relative to it, with its mode
// and checksum: what writes must leave as they are.
type shelfFile struct{ rel, state string }

// shelfPicks are the files writes are tried on: the first file under each
// entry at the shelf's top (each kind of content: npm, goproxy, bin, ...),
// at most 16. Links are followed: a write through one is a write to what it
// names.
func (r *run) shelfPicks(h, p string) ([]shelfFile, error) {
	res, err := r.shelfSh(h, `cd "$S" && for t in * .[!.]*; do
  if [ -f "$t" ]; then echo "$t"; elif [ -d "$t" ]; then find -L "$t" -type f -print 2>/dev/null | head -n 1; fi
done | head -n 16`, p)
	if err != nil {
		return nil, err
	}
	var out []shelfFile
	for _, rel := range strings.Split(strings.TrimSuffix(*res.Stdout, "\n"), "\n") {
		if rel == "" {
			continue
		}
		st, err := r.shelfState(h, p, rel)
		if err != nil {
			return nil, err
		}
		out = append(out, shelfFile{rel, st})
	}
	return out, nil
}

// shelfState is a shelf file's mode and checksum as box h sees them.
func (r *run) shelfState(h, p, rel string) (string, error) {
	res, err := r.shelfSh(h, `[ -f "$S/$2" ] || { echo "$2 is gone from the shelf" >&2; exit 4; }
ls -ld "$S/$2" | cut -c1-10; cksum < "$S/$2"`, p, rel)
	if err != nil {
		return "", err
	}
	return *res.Stdout, nil
}

// shelfUnchanged says box h sees each file as it was and no name carrying
// mark anywhere on the shelf.
func (r *run) shelfUnchanged(h, p string, files []shelfFile, mark string) error {
	res, err := r.shelfSh(h, `find "$S" -name "*$2*" -print 2>/dev/null; true`, p, mark)
	if err != nil {
		return err
	}
	if s := strings.TrimSpace(*res.Stdout); s != "" {
		return fmt.Errorf("the shelf holds what a box wrote: %q", clip(s))
	}
	for _, f := range files {
		got, err := r.shelfState(h, p, f.rel)
		if err == nil && got != f.state {
			err = fmt.Errorf("shelf file %s was %q, now %q", f.rel, clip(f.state), clip(got))
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// shelfOp is one write a box tries: op is a shell line with $1 the target
// (each directory: the shelf, its top-level directories, each picked file's
// directory and up to 64 more; or each picked file) and $2 the mark.
type shelfOp struct{ what, on, op string }

var shelfWrites = []shelfOp{
	{"create a file", "d", `: > "$1/$2"`},
	{"make a directory", "d", `mkdir "$1/$2.d"`},
	{"overwrite a file", "f", `printf '%s' "$2" > "$1"`},
	{"append to a file", "f", `printf '%s' "$2" >> "$1"`},
	{"delete a file", "f", `rm -f "$1" && [ ! -e "$1" ]`},
	{"rename a file", "f", `mv "$1" "$1.$2"`},
}

// shelfChmods change modes. A chmod must fail, or leave nothing the box's
// next command can write.
var shelfChmods = []shelfOp{
	{"chmod a file", "f", `chmod 0666 "$1"`},
	{"chmod a directory", "d", `chmod 0777 "$1"`},
}

// shelfTry runs op on every target of its kind, as the box user, or under
// sudo -n; it prints each target the op worked on. The op sees the shelf's
// top as $S. $2 sudo (0 or 1), $3 the
// kind, $4 the op, $5 the mark, then the picked files.
const shelfTry = `sudo=$2 on=$3 op=$4 mark=$5; shift 5
if [ "$on" = d ]; then { echo "$S"; for t in "$S"/* "$S"/.[!.]*; do [ -d "$t" ] && echo "$t"; done; for f; do dirname "$S/$f"; done; find "$S" -type d 2>/dev/null | head -n 64; } | sort -u; else for f; do printf '%s\n' "$S/$f"; done; fi | while IFS= read -r t; do
  if [ "$sudo" = 1 ]; then sudo -n env S="$S" sh -c "$op" sh "$t" "$mark"; else S=$S sh -c "$op" sh "$t" "$mark"; fi </dev/null >/dev/null 2>&1 && echo "$t"
done; true`

// shelfAttempt runs o in box h as the box user, then as root where root is
// on; it answers how the first write that worked was made ("" for none).
func (r *run) shelfAttempt(h, p string, o shelfOp, mark string, files []shelfFile) (string, error) {
	rels := make([]string, len(files))
	for i, f := range files {
		rels[i] = f.rel
	}
	for _, sudo := range []string{"0", "1"} {
		if sudo == "1" && !r.rootOn() {
			break
		}
		res, err := r.shelfSh(h, shelfTry, p, append([]string{sudo, o.on, o.op, mark}, rels...)...)
		if err != nil {
			return "", err
		}
		if s := strings.TrimSpace(*res.Stdout); s != "" {
			return fmt.Sprintf("%s%s: %s", o.what, map[string]string{"1": " as root"}[sudo], clip(s)), nil
		}
	}
	return "", nil
}

func (r *run) rootOn() bool {
	return r.desc.Capabilities != nil && slices.Contains(*r.desc.Capabilities, protocol.CapRoot)
}

// shelfRoot says sudo works in box h where root is on, so a write refused
// under sudo is the shelf's refusal, not sudo's.
func (r *run) shelfRoot(h string) error {
	if !r.rootOn() {
		return nil
	}
	res, err := r.sh(h, `sudo -n sh -c 'exit 0'`, "")
	if err == nil && *res.Exit != 0 {
		err = fmt.Errorf("describe lists root, but sudo -n exits %d: %s", *res.Exit, clip(*res.Stderr))
	}
	return err
}

// shelfBox acquires a box and picks the shelf's files in it.
func (r *run) shelfBox(p string) (held, []shelfFile, error) {
	a, err := r.acquireAny(r.longTTL())
	if err != nil {
		return a, nil, err
	}
	if err := r.shelfRoot(a.Handle); err != nil {
		return a, nil, err
	}
	files, err := r.shelfPicks(a.Handle, p)
	if err == nil && len(files) == 0 {
		r.note("the package shelf", "the shelf holds no file: only creating on it is tried, not overwrite, delete or a file's chmod")
	}
	return a, files, err
}

// shelfRefuses: every write a box tries on the shelf fails, as root too
// where root is on, and the shelf is as it was after them.
func (r *run) shelfRefuses(p string) error {
	a, files, err := r.shelfBox(p)
	defer r.endHeld(a)
	if err != nil {
		return err
	}
	mark := "zmconf-shelf-" + r.tag
	for _, o := range shelfWrites {
		if did, err := r.shelfAttempt(a.Handle, p, o, mark, files); err != nil || did != "" {
			return orSay(err, "a box could "+did)
		}
	}
	for _, c := range shelfChmods {
		if _, err := r.shelfAttempt(a.Handle, p, c, mark, files); err != nil {
			return err
		}
		// A chmod that took leaves the shelf writable: the next command
		// must find it as unwritable as before.
		for _, o := range []shelfOp{shelfWrites[0], shelfWrites[3]} {
			if did, err := r.shelfAttempt(a.Handle, p, o, mark, files); err != nil || did != "" {
				return orSay(err, "after a box tried to "+c.what+", it could "+did)
			}
		}
	}
	return r.shelfUnchanged(a.Handle, p, files, mark)
}

// orSay is err, else a new error saying s.
func orSay(err error, s string) error {
	if err != nil {
		return err
	}
	return errors.New(s)
}

// shelfShared: box A tries every write, then a chmod and writes in one
// command, then replacing the shelf path itself, while B is live; B, and C
// acquired after A ends, see the shelf as it was.
func (r *run) shelfShared(p string) error {
	b, files, err := r.shelfBox(p)
	defer r.endHeld(b)
	if err != nil {
		return err
	}
	a, err := r.acquireAny(r.longTTL())
	defer r.endHeld(a)
	if err != nil {
		return err
	}
	mark := "zmconf-shelf-shared-" + r.tag
	for _, o := range append(append([]shelfOp{}, shelfChmods...), shelfWrites...) { // chmod first: a write after it is the one that could land
		if _, err := r.shelfAttempt(a.Handle, p, o, mark, files); err != nil {
			return err
		}
	}
	if err := r.shelfLimit(a.Handle); err != nil {
		return err
	}
	for _, o := range []shelfOp{
		{"chmod and write in one command", "d", `chmod -R u+w "$1" 2>/dev/null; : > "$1/$2"`},
		{"replace the shelf with a directory of the box's", "d", shelfReplace},
	} {
		if _, err := r.shelfAttempt(a.Handle, p, o, mark, files); err != nil {
			return err
		}
	}
	if err := r.shelfUnchanged(b.Handle, p, files, mark); err != nil {
		return fmt.Errorf("a live box, while another wrote: %w", err)
	}
	r.endHeld(a)
	r.endHeld(b)
	c, err := r.acquireAny(r.longTTL())
	defer r.endHeld(c)
	if err != nil {
		return err
	}
	if err := r.shelfUnchanged(c.Handle, p, files, mark); err != nil {
		return fmt.Errorf("a box after the writer ended: %w", err)
	}
	return nil
}

// shelfReplace renames the shelf aside and makes a directory of the box's
// in its place, with a file named for the mark. It must be a rename, never a
// copy: a copy of the shelf lands in the box's own filesystem, which under
// gVisor is box memory, so a shelf bigger than the box gets the box killed.
// mv copies when rename(2) answers EXDEV. And under gVisor, with root,
// rename(2) of the shelf's mount point itself fills the box's rootfs overlay
// with the tree before it answers (Linux answers EBUSY for a mount point).
// With the file size limit at zero both stop at the first non-empty file,
// and a rename in place, mkdir and an empty file still work. The limit holds
// only in the op's own sh -c. So a replace that only a copy could make is
// left untried: a shelf a box can copy over is one it can write, which the
// writes before this op show.
const shelfReplace = `[ "$1" = "$S" ] && ulimit -f 0 && mv "$1" "$1.$2" && mkdir "$1" && : > "$1/$2"`

// shelfLimit says box h's sh can set the file size limit shelfReplace needs.
// A box that cannot would skip the replace unseen, since an op that fails
// reads as a write refused.
func (r *run) shelfLimit(h string) error {
	res, err := r.sh(h, `ulimit -f 0`, "")
	if err == nil && *res.Exit != 0 {
		err = fmt.Errorf("the box's sh cannot set a file size limit (ulimit -f 0 exits %d: %s): replacing the shelf cannot be tried without copying it", *res.Exit, clip(strings.TrimSpace(*res.Stderr)))
	}
	return err
}

// npmBlob is a sha512's path on the shelf and in npm's cache (under
// content-v2), and its integrity.
func npmBlob(sum []byte) (rel, integrity string) {
	hx := hex.EncodeToString(sum)
	return "npm/content-v2/sha512/" + hx[:2] + "/" + hx[2:4] + "/" + hx[4:], "sha512-" + base64.StdEncoding.EncodeToString(sum)
}

// shelfMarker is a blob only this run makes: its bytes, the shelf path it
// would have and its integrity.
func shelfMarker(tag string) (blob, rel, integrity string) {
	line := "zmconf shelf marker " + tag + "\n"
	blob = strings.Repeat(line, 4099/len(line)+1)[:4099]
	sum := sha512.Sum512([]byte(blob))
	rel, integrity = npmBlob(sum[:])
	return blob, rel, integrity
}

// shelfHarvestWait is how long after the writer ends the shelf is looked at
// again, so a driver that copies an ended box's cache in soon after is
// caught too. A fill that comes minutes later (shelf_fill_delay_s) is past
// what a conformance run waits for; the host's fill takes only what it
// fetched (vmhost's tests).
var shelfHarvestWait = 5 * time.Second

// shelfHarvest: box A puts a marker blob in its own npm cache at the path
// the shelf would hold it at, and in its Go module cache, and names it in
// its wants; after A ends, no box finds it on the shelf, at any path.
func (r *run) shelfHarvest(p string) error {
	a, err := r.acquireAny(r.longTTL())
	defer r.endHeld(a)
	if err != nil {
		return err
	}
	blob, rel, integ := shelfMarker(r.tag)
	want := "npm zmconf-shelf-" + r.tag + " 1.0.0 " + integ
	res, err := r.exec(a.Handle, []string{"sh", "-c", `cat > "$HOME/zmconf-marker"`}, blob, 0)
	if err == nil && *res.Exit != 0 {
		err = fmt.Errorf("writing the marker exited %d", *res.Exit)
	}
	if err != nil {
		return err
	}
	// The blob goes where npm's cache would hold it (~/.npm, or
	// npm_config_cache when the box sets it), at the shelf's own path, and
	// where Go keeps a downloaded module.
	res, err = r.exec(a.Handle, []string{"sh", "-c", `set -e
for c in "$HOME/.npm/_cacache" "${npm_config_cache:-$HOME/.npm}/_cacache"; do
  b="$c/${1#npm/}"; mkdir -p "$(dirname "$b")"; cat "$HOME/zmconf-marker" > "$b"
done
g="${GOMODCACHE:-${GOPATH:-$HOME/go}/pkg/mod}/cache/download/zmconf.example/shelf-$4/@v"
mkdir -p "$g"; cat "$HOME/zmconf-marker" > "$g/v1.0.0.zip"
mkdir -p "$(dirname "$HOME/$3")"; printf '%s\n' "$2" >> "$HOME/$3"`, "sh", rel, want, shelfWants, r.tag}, "", 0)
	if err == nil && *res.Exit != 0 {
		err = fmt.Errorf("writing a box's own caches and wants exited %d: %s", *res.Exit, clip(*res.Stderr))
	}
	if err != nil {
		return err
	}
	r.endHeld(a)
	for i, wait := range []time.Duration{0, shelfHarvestWait} {
		time.Sleep(wait)
		b, err := r.acquireAny(r.longTTL())
		if err == nil {
			err = r.shelfNoMarker(b.Handle, p, blob, rel)
		}
		r.endHeld(b)
		if err != nil {
			return fmt.Errorf("%s the box ended: %w", map[int]string{0: "right after", 1: shelfHarvestWait.String() + " after"}[i], err)
		}
	}
	return nil
}

// shelfNoMarker says box h finds the marker on the shelf neither at rel nor
// at any other path (every file of its size is compared).
func (r *run) shelfNoMarker(h, p, blob, rel string) error {
	res, err := r.exec(h, []string{"sh", "-c", shelfLook + `cat > "$HOME/zmconf-marker"
[ -e "$S/$2" ] && echo "$2"
find "$S" -type f -size "$3"c 2>/dev/null | while IFS= read -r f; do cmp -s "$f" "$HOME/zmconf-marker" && echo "$f"; done; true`,
		"sh", p, rel, strconv.Itoa(len(blob))}, blob, 0)
	switch {
	case err != nil:
	case *res.Exit != 0:
		err = fmt.Errorf("the shelf could not be searched: exit %d %s", *res.Exit, clip(*res.Stderr))
	case strings.TrimSpace(*res.Stdout) != "":
		err = fmt.Errorf("the shelf holds a blob a box wrote: %q", clip(*res.Stdout))
	}
	return err
}

// shelfUse is what a box does with the shelf, as potluck-deps link does: a
// link to a shelf file in its own npm cache (an npm blob at its own cache
// path, with a valid want for it), a link carrying mark, a wants line and a
// file in HOME. It answers the files it saw on the shelf and the cache
// paths it linked.
func (r *run) shelfUse(h, p, mark string) ([]shelfFile, []string, error) {
	files, err := r.shelfPicks(h, p)
	if err != nil {
		return nil, nil, err
	}
	target, links, wants := "", []string{".npm/_cacache/content-v2/sha512/zm/cf/" + mark}, []string{"npm " + mark + " 1.0.0 sha512-AAAA"}
	for _, f := range files {
		hx := strings.ReplaceAll(strings.TrimPrefix(f.rel, "npm/content-v2/sha512/"), "/", "")
		sum, derr := hex.DecodeString(hx)
		if !strings.HasPrefix(f.rel, "npm/content-v2/sha512/") || derr != nil || len(sum) != sha512.Size {
			continue
		}
		_, integ := npmBlob(sum)
		links = append(links, ".npm/_cacache/"+strings.TrimPrefix(f.rel, "npm/"))
		wants = append(wants, "npm zmconf-shelf-pkg 1.0.0 "+integ)
		target = f.rel
		break
	}
	if target == "" && len(files) > 0 {
		target = files[0].rel
	}
	res, err := r.exec(h, append([]string{"sh", "-c", shelfLook + `set -e
t="$S"; [ -n "$2" ] && t="$S/$2"
mark=$3; shift 3
for l; do mkdir -p "$(dirname "$HOME/$l")"; ln -s "$t" "$HOME/$l"; done
printf x > "$HOME/$mark"`, "sh", p, target, mark}, links...), "", 0)
	if err == nil && *res.Exit != 0 {
		err = fmt.Errorf("linking from the shelf exited %d: %s", *res.Exit, clip(*res.Stderr))
	}
	if err != nil {
		return nil, nil, err
	}
	res, err = r.exec(h, []string{"sh", "-c", `mkdir -p "$(dirname "$HOME/$1")" && cat >> "$HOME/$1"`, "sh", shelfWants}, strings.Join(wants, "\n")+"\n", 0)
	if err == nil && *res.Exit != 0 {
		err = fmt.Errorf("writing wants exited %d: %s", *res.Exit, clip(*res.Stderr))
	}
	return files, links, err
}

// shelfLeftovers says box h holds nothing shelfUse left: none of its links,
// no other link under HOME into a shelf at p, no wants file, no file mark.
func (r *run) shelfLeftovers(h, p, mark string, links []string) error {
	res, err := r.exec(h, append([]string{"sh", "-c", shelfLook + `p=$1 w=$2 mark=$3; shift 3
for l; do { [ -e "$HOME/$l" ] || [ -L "$HOME/$l" ]; } && echo "the link $l"; done
{ [ -e "$HOME/$w" ] || [ -L "$HOME/$w" ]; } && echo "a wants file: $(head -c 200 "$HOME/$w")"
{ [ -e "$HOME/$mark" ] || [ -L "$HOME/$mark" ]; } && echo "the file $mark"
H=$(cd "$HOME" && pwd -P); find "$H" -path "$S" -prune -o -type l -print 2>/dev/null | while IFS= read -r l; do
  case "$(readlink "$l")" in *"$p"*) echo "a link into a shelf: $l" ;; esac
done; true`, "sh", p, shelfWants, mark}, links...), "", 0)
	switch {
	case err != nil:
	case *res.Exit != 0:
		err = fmt.Errorf("the box could not be searched: exit %d %s", *res.Exit, clip(*res.Stderr))
	case strings.TrimSpace(*res.Stdout) != "":
		err = fmt.Errorf("the box holds an earlier box's: %s", clip(strings.TrimSpace(*res.Stdout)))
	}
	return err
}

// shelfLeaves: box A links from the shelf and writes its wants and HOME;
// a later box has none of it.
func (r *run) shelfLeaves(p string) error {
	a, err := r.acquireAny(r.longTTL())
	defer r.endHeld(a)
	if err != nil {
		return err
	}
	mark := "zmconf-shelf-left-" + r.tag
	_, links, err := r.shelfUse(a.Handle, p, mark)
	if err != nil {
		return err
	}
	r.endHeld(a)
	b, err := r.acquireAny(r.longTTL())
	defer r.endHeld(b)
	if err != nil {
		return err
	}
	return r.shelfLeftovers(b.Handle, p, mark, links)
}

// warmShelf: a warm box links from the shelf, writes its wants and HOME and
// is released; the next warm box has none of it and sees the same shelf.
func (r *run) warmShelf(p string) error {
	a, err := r.acquireAny(r.longTTL())
	defer r.endHeld(a)
	if err != nil {
		return err
	}
	mark := "zmconf-shelf-warm-" + r.tag
	files, links, err := r.shelfUse(a.Handle, p, mark)
	if err != nil {
		return err
	}
	r.endHeld(a)
	if err := r.waitWarmReady(); err != nil {
		return err
	}
	b, err := r.acquireAny(r.longTTL())
	defer r.endHeld(b)
	if err != nil {
		return err
	}
	if err := r.shelfLeftovers(b.Handle, p, mark, links); err != nil {
		return err
	}
	return r.shelfUnchanged(b.Handle, p, files, mark)
}
