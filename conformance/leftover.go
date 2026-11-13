package conformance

import (
	"fmt"
	"slices"
	"strings"
)

// The leftover search: what an earlier box wrote must not be in a later box.
// A driver that hands a VM from box to box, or a pool that wipes HOME but
// keeps the rest, could leave it anywhere the box wrote; the search covers
// the listed places only (leftoverPlaces), not every writable path in a
// guest (a shared cache mount, a root box's whole disk).

// DefaultLeftoverRoots are the places most boxes can write besides their own
// HOME, TMPDIR, working directory and runtime directory: the shared and
// lock directories of Linux and macOS guests.
var DefaultLeftoverRoots = []string{"/tmp", "/var/tmp", "/dev/shm", "/run/user", "/run/lock", "/var/lock", "/Users/Shared"}

// leftoverPlaces is the one list of places, as sh words: HOME, TMPDIR, the
// working directory (a checkout), the runtime directory, then the roots
// (the script's remaining arguments). An unset one is empty and skipped.
const leftoverPlaces = `"$HOME" "${TMPDIR:-}" "$PWD" "${XDG_RUNTIME_DIR:-}" "$@"`

// roots are LeftoverRoots, or DefaultLeftoverRoots when none are given,
// then extra.
func (r *run) roots(extra []string) []string {
	rs := r.opt.LeftoverRoots
	if len(rs) == 0 {
		rs = DefaultLeftoverRoots
	}
	return append(slices.Clip(rs), extra...)
}

// leaveMark writes a file only-<mark>, and a directory only-<mark>.d holding
// another (a cleanup that removes files but keeps directories is caught),
// into each of leftoverPlaces that is a directory box h may write (HOME
// must be one) and answers those
// directories as the box resolves them. A later box's search walks them too
// (noLeftover), so a pool that gives each box its own HOME or checkout path
// but keeps the last one's is caught. It then searches box h itself: a
// search that cannot find a marker it wrote (no find, a tree it cannot
// walk) fails here rather than passing every later box.
func (r *run) leaveMark(h, mark string) ([]string, error) {
	argv := append([]string{"sh", "-c", `m=$1; shift
[ -d "$HOME" ] && [ -w "$HOME" ] || { echo "no HOME the box can write" >&2; exit 3; }
for d in ` + leftoverPlaces + `; do
	[ -n "$d" ] && [ -d "$d" ] && [ -w "$d" ] || continue
	printf x > "$d/only-$m" && mkdir -p "$d/only-$m.d" && printf x > "$d/only-$m.d/only-$m" || { echo "cannot write $d/only-$m" >&2; exit 1; }
	(cd "$d" && pwd -P) || exit 1
done`, "sh", mark}, r.roots(nil)...)
	res, err := r.exec(h, argv, "", 0)
	if err == nil && *res.Exit != 0 {
		err = fmt.Errorf("exit %d %s", *res.Exit, clip(*res.Stderr))
	}
	if err != nil {
		return nil, fmt.Errorf("could not leave a marker: %w", err)
	}
	var dirs []string
	for _, d := range strings.Split(*res.Stdout, "\n") {
		if d != "" {
			dirs = append(dirs, d)
		}
	}
	got, err := r.leftovers(h, mark, "", nil)
	if err != nil {
		return nil, fmt.Errorf("could not search the box that left a marker: %w", err)
	}
	for _, d := range dirs {
		for _, f := range []string{d + "/only-" + mark, d + "/only-" + mark + ".d/only-" + mark} {
			if !slices.Contains(got, f) {
				return nil, fmt.Errorf("the leftover search does not find the marker %s in the box that wrote it (found %q)", f, got)
			}
		}
	}
	return dirs, nil
}

// noLeftover says box h holds no file named only-<mark> in any of
// leftoverPlaces or in dirs (where leaveMark wrote it in an earlier box).
func (r *run) noLeftover(h, mark string, dirs []string) error {
	got, err := r.leftovers(h, mark, "", dirs)
	if err == nil && len(got) > 0 {
		err = fmt.Errorf("the box holds an earlier box's files: %q", got)
	}
	return err
}

// nothingAt says box h has nothing at the volume path (volPath under HOME)
// and no file named only-<mark> in any of leftoverPlaces.
func (r *run) nothingAt(h, mark string) error {
	got, err := r.leftovers(h, mark, volPath, nil)
	if err == nil && len(got) > 0 {
		err = fmt.Errorf("the box holds an earlier box's volume: %q", got)
	}
	return err
}

// leftovers is what box h holds of a mark: each file named only-<mark> in
// each of leftoverPlaces (and extra) that is there, as the box resolves its
// directory, and, when at is set, what is at HOME's at. Each place is
// entered through a trailing slash, so a symlinked root (macOS /tmp) is
// walked; find's own exit is not read, since a tree may hold directories
// the box cannot enter (macOS guests). The search is bounded by leftoverS;
// a box that cannot be searched in that time fails.
func (r *run) leftovers(h, mark, at string, extra []string) ([]string, error) {
	argv := append([]string{"sh", "-c", `command -v find >/dev/null || { echo "no find in the box" >&2; exit 3; }
[ -d "$HOME" ] || { echo "no HOME" >&2; exit 3; }
if [ -n "$1" ] && { [ -e "$HOME/$1" ] || [ -L "$HOME/$1" ]; }; then echo "$1 is there:"; ls -A "$HOME/$1"; fi
m=$2; shift 2
for d in ` + leftoverPlaces + `; do
	[ -n "$d" ] && [ -d "$d" ] || continue
	find "$d/" -name "only-$m" 2>/dev/null | while IFS= read -r f; do
		printf '%s/only-%s\n' "$(cd "${f%/*}" && pwd -P)" "$m"
	done
done; true`,
		"sh", at, mark}, r.roots(extra)...)
	res, err := r.exec(h, argv, "", leftoverS)
	switch {
	case err != nil:
		return nil, err
	case *res.Exit != 0:
		return nil, fmt.Errorf("the box could not be searched: exit %d %s", *res.Exit, clip(*res.Stderr))
	}
	var got []string
	for _, l := range strings.Split(*res.Stdout, "\n") {
		if l = strings.TrimSpace(l); l != "" && !slices.Contains(got, l) {
			got = append(got, l)
		}
	}
	return got, nil
}

// leftoverS bounds the leftover search, in seconds.
const leftoverS = 120
