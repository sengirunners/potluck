package conformance

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/sengirunners/potluck/protocol"
)

// Keep at seal (capability volumes-keep): a release with seal may name the
// paths that are all its build volume keeps. Once the builder's processes
// have ended, the driver deletes every other entry before it seals, so
// nothing else the builder wrote (a .git, a config file, a link where a kept
// directory should be) reaches a reader.
func (r *run) keeps(owner string, fresh func() string) {
	const head = "a release with seal and keep seals only the kept trees, never a link or file in their place"
	if r.desc.Capabilities == nil || !slices.Contains(*r.desc.Capabilities, protocol.CapVolumesKeep) {
		r.skip(head, "describe lists no volumes-keep capability")
		return
	}
	r.add("keep without seal, or naming a path that is not a volume path, answers bad_request", r.keepBadArgs(owner, fresh()))
	r.add(head, r.keepOnly(owner, fresh()))
}

func (r *run) keepBadArgs(owner, name string) error {
	b, e, err := r.acquireVolWith(owner, name, protocol.VolumeBuild, false, r.longTTL())
	defer r.endHeld(b)
	if err = must(e, err); err != nil {
		return fmt.Errorf("build: %v", err)
	}
	for _, a := range []json.RawMessage{
		args("keep", []string{"node_modules"}),
		args("seal", true, "keep", []string{}),
		args("seal", true, "keep", []string{"../x"}),
		args("seal", true, "keep", []string{"/abs"}),
		args("seal", true, "keep", []string{"a//b"}),
	} {
		var rel releaseReport
		e, err := r.call(verbRelease, b.Handle, a, &rel)
		if err := wantCode(e, err, protocol.CodeBadRequest); err != nil {
			return fmt.Errorf("release %s: %v", a, err)
		}
	}
	return nil
}

// keepFill is what the builder writes: kept trees (with a .git and a link
// inside, which stay), and beside them a .git, plain files, a directory
// on the way holding other entries, a link and a file where a kept
// directory should be, and a link to a directory on the way (their target,
// zz, sorts after them, so a driver that follows them finds it there).
const keepFill = `set -e; cd "$HOME/$1"
mkdir -p node_modules/a/.git node_modules/.bin .git/hooks ws/node_modules/b ws/src zz/node_modules ws4
echo f > node_modules/a/f; echo g > node_modules/a/.git/config; ln -s ../a/f node_modules/.bin/x
echo c > .git/config; echo h > .git/hooks/post-checkout; echo x > x.txt; echo y > .npmrc; echo z > ..odd
echo b > ws/node_modules/b/f; echo s > ws/src/i.js; echo t > ws/.gitattributes
echo o > zz/node_modules/f; ln -s zz ws2; echo file > ws3; ln -s ../zz/node_modules ws4/node_modules
`

// keepList lists the volume's tree, links marked "@", one per line, sorted.
const keepList = `cd "$HOME/$1" || exit 3
find . ! -name . | while IFS= read -r p; do if [ -L "$p" ]; then printf '%s@\n' "$p"; else printf '%s\n' "$p"; fi; done | LC_ALL=C sort`

var keepPaths = []string{"node_modules", "ws/node_modules", "ws2/node_modules", "ws3/node_modules", "ws4/node_modules", "absent/node_modules"}

const keepWant = `./node_modules
./node_modules/.bin
./node_modules/.bin/x@
./node_modules/a
./node_modules/a/.git
./node_modules/a/.git/config
./node_modules/a/f
./ws
./ws/node_modules
./ws/node_modules/b
./ws/node_modules/b/f
./ws4
`

func (r *run) keepOnly(owner, name string) error {
	b, e, err := r.acquireVolWith(owner, name, protocol.VolumeBuild, false, r.longTTL())
	defer r.endHeld(b)
	if err = must(e, err); err != nil {
		return fmt.Errorf("build: %v", err)
	}
	res, err := r.sh(b.Handle, keepFill, volPath)
	if err == nil && *res.Exit != 0 {
		err = fmt.Errorf("exit %d: %s", *res.Exit, clip(*res.Stderr))
	}
	if err != nil {
		return fmt.Errorf("filling the build volume: %v", err)
	}
	var rel releaseReport
	if err := must(r.call(verbRelease, b.Handle, args("seal", true, "keep", keepPaths), &rel)); err != nil {
		return fmt.Errorf("release with seal and keep: %v", err)
	}
	r.noteSpend(b.Handle, rel.Spend)
	delete(r.live, b.Handle)

	ro, e, err := r.acquireVolWith(owner, name, protocol.VolumeRO, true, r.longTTL())
	defer r.endHeld(ro)
	if err = must(e, err); err != nil {
		return fmt.Errorf("ro with sealed after a release with seal and keep: %v", err)
	}
	res, err = r.sh(ro.Handle, keepList, volPath)
	if err == nil && *res.Exit != 0 {
		err = fmt.Errorf("listing the sealed volume exited %d", *res.Exit)
	}
	if err != nil {
		return err
	}
	if *res.Stdout != keepWant {
		return fmt.Errorf("the sealed volume holds %q, want only the kept trees %q", clip(strings.ReplaceAll(*res.Stdout, "\n", " ")), clip(strings.ReplaceAll(keepWant, "\n", " ")))
	}
	return nil
}
