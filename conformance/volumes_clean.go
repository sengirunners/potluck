package conformance

import (
	"fmt"
	"slices"
	"strings"

	"github.com/sengirunners/potluck/protocol"
)

// A sealed volume carries no setuid or setgid file and no device node, FIFO
// or socket (docs/protocol.md, Sealed volumes): once the builder's processes
// have ended, the driver clears those bits and deletes those entries before
// it seals, so nothing a build box made (with root, a setuid-root file) gives
// a later box of the owner anything a fresh box lacks. What the files hold
// stays.

// cleanFill is what the builder writes into the volume ($1): a setuid file,
// a setgid file and directory, and a FIFO; with root ($2 "root"), also a
// setuid file of root's and a device node (either may be refused: the
// driver may hold MKNOD back).
const cleanFill = `set -e; cd "$HOME/$1"
mkdir -p node_modules/.bin node_modules/d
echo u > node_modules/.bin/u; chmod 4755 node_modules/.bin/u
echo g > node_modules/g; chmod 2755 node_modules/g; chmod 2775 node_modules/d
mkfifo node_modules/fifo
n=$(find node_modules \( -type p -o -perm -4000 -o -perm -2000 \) -print | wc -l)
[ "$n" -ge 4 ] || { echo "the box made only $n of its setuid, setgid and FIFO entries" >&2; exit 1; }
if [ "$2" = root ]; then
  echo r > node_modules/.bin/r; sudo -n chown 0:0 node_modules/.bin/r; sudo -n chmod 4755 node_modules/.bin/r
  sudo -n mknod node_modules/null c 1 3 2>/dev/null || true
fi
`

// cleanList lists, in the volume ($1), every special entry and setuid or
// setgid one, then each file the builder wrote ($2...) with what it holds.
const cleanList = `cd "$HOME/$1" || exit 3; shift
find . \( -type b -o -type c -o -type p -o -type s -o -perm -4000 -o -perm -2000 \) -print | LC_ALL=C sort | sed 's/^/special /'
for p; do printf '%s %s\n' "$p" "$(cat "$p" 2>&1)"; done`

func (r *run) sealClean(owner, name string) error {
	b, e, err := r.acquireVolWith(owner, name, protocol.VolumeBuild, false, r.longTTL())
	defer r.endHeld(b)
	if err = must(e, err); err != nil {
		return fmt.Errorf("build: %v", err)
	}
	root := ""
	if r.desc.Capabilities != nil && slices.Contains(*r.desc.Capabilities, protocol.CapRoot) {
		root = "root"
	}
	res, err := r.exec(b.Handle, []string{"sh", "-c", cleanFill, "sh", volPath, root}, "", 0)
	if err == nil && *res.Exit != 0 {
		err = fmt.Errorf("exit %d: %s", *res.Exit, clip(*res.Stderr))
	}
	if err != nil {
		return fmt.Errorf("filling the build volume: %v", err)
	}
	if err := r.releaseSeal(b.Handle, true); err != nil {
		return err
	}
	files := []string{"node_modules/.bin/u", "node_modules/g"}
	if root != "" {
		files = append(files, "node_modules/.bin/r")
	}
	want := ""
	for _, f := range files {
		want += f + " " + f[strings.LastIndex(f, "/")+1:] + "\n"
	}
	for _, mode := range []string{protocol.VolumeRO, protocol.VolumeCOW} {
		h, e, err := r.acquireVolWith(owner, name, mode, true, r.longTTL())
		if err = must(e, err); err != nil {
			r.endHeld(h)
			return fmt.Errorf("%s with sealed after a release with seal: %v", mode, err)
		}
		res, err := r.exec(h.Handle, append([]string{"sh", "-c", cleanList, "sh", volPath}, files...), "", 0)
		r.endHeld(h)
		if err == nil && *res.Exit != 0 {
			err = fmt.Errorf("listing the sealed volume exited %d", *res.Exit)
		}
		if err != nil {
			return err
		}
		if *res.Stdout != want {
			return fmt.Errorf("%s of the sealed volume lists %q, want only the files, with no setuid or setgid bit and no device node, FIFO or socket: %q", mode, clip(strings.ReplaceAll(*res.Stdout, "\n", " | ")), strings.ReplaceAll(want, "\n", " | "))
		}
	}
	return nil
}
