package vmhost

import (
	"strings"

	"github.com/sengirunners/potluck/protocol"
)

// The cleanup at seal (docs/protocol.md, Sealed volumes): once every process
// of the builder has ended and nothing else references the volume, the
// driver cleans it before it seals; a volume that could not be cleaned is
// never sealed (endBuilds), so the sweep deletes it. No link is ever
// followed. Two steps:
//
//   - Keep (capability volumes-keep): a release with seal may name the paths
//     (relative to the volume's root) that are all the volume keeps. Every
//     other entry is deleted: what the builder's processes wrote anywhere
//     else (a .git, a config file) never reaches a reader. A directory on the
//     way to a kept path stays as a directory with only those entries; a kept
//     path or a directory on the way that is not a real directory (a
//     symlink, a file) is deleted.
//   - Normalise, always: every setuid and setgid bit is cleared and every
//     device node, FIFO and socket deleted, then the volume is walked again
//     and any left (or a walk that cannot finish) fails the seal. A build box
//     on a binding with root runs sudo, so its install scripts could leave a
//     setuid-root file that every later box of the owner mounts. This is
//     what holds on every runtime: on gVisor the views of a sealed volume
//     carry no nosuid or nodev in the box (oci_volume.go). File
//     capabilities (security.capability) are not removed.

// sealCleanScript cleans the directory $1: with $2 "keep", first prunes it to
// the paths in $3 (one per line), with $4 the directories on the way to them
// (protocol.KeepDirs); then normalises it. An entry whose name a volume path
// cannot hold is never one of the kept. rm -rf removes a link, never what it
// points at; find walks without following links, and chmod is never given a
// link. PATH is the system's own: the image's may name a relative directory
// (./node_modules/.bin), where a build could have put a find that lies. A
// find that cannot walk the volume fails the script (set -e), and the last
// check reads what find prints, never another command's exit through
// -exec. It runs as root in the volume's helper (ociCLI.CleanSeal) or on the
// host (tests' fake layer), never in a box, and only once no process can
// change the volume under it.
const sealCleanScript = `set -eu
PATH=/usr/sbin:/usr/bin:/sbin:/bin; export PATH
cd "$1"
nl='
'
K="$nl${3-}$nl" D="$nl${4-}$nl"
prune() {
  for e in "$1"/* "$1"/.[!.]* "$1"/..?*; do
    [ -e "$e" ] || [ -L "$e" ] || continue
    r=${e#./}
    case $r in *[!A-Za-z0-9._/@+-]*) rm -rf -- "$e"; continue ;; esac
    case $K in *"$nl$r$nl"*) if [ ! -d "$e" ] || [ -L "$e" ]; then rm -rf -- "$e"; fi; continue ;; esac
    case $D in *"$nl$r$nl"*) if [ -d "$e" ] && [ ! -L "$e" ]; then prune "$e"; else rm -rf -- "$e"; fi; continue ;; esac
    rm -rf -- "$e"
  done
}
[ "$2" != keep ] || prune .
find . \( -type b -o -type c -o -type p -o -type s \) -exec rm -f {} +
find . ! -type l \( -perm -4000 -o -perm -2000 \) -exec chmod u-s,g-s {} +
left=$(find . \( -type b -o -type c -o -type p -o -type s -o \( ! -type l \( -perm -4000 -o -perm -2000 \) \) \) -print)
if [ -n "$left" ]; then
  echo "a device node, FIFO, socket or setuid or setgid file is left" >&2
  exit 1
fi
`

// sealCleanArgs are sealCleanScript's arguments after the directory: keep's
// paths and the directories on the way to them, or none to only normalise.
func sealCleanArgs(keep []string) []string {
	if len(keep) == 0 {
		return []string{"all"}
	}
	return []string{"keep", strings.Join(keep, "\n"), strings.Join(protocol.KeepDirs(keep), "\n")}
}
