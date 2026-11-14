// Package shelfhelper holds potluck-deps, the shell helper a box runs to use the package shelf
// (docs/shelf.md). The publisher installs Script on the shelf as bin/potluck-deps.
package shelfhelper

import _ "embed"

// Name is the helper's file name on the shelf, under bin/.
const Name = "potluck-deps"

// BoxDir is where a box with the shelf has it mounted, read-only, and
// BoxPath the helper there. Every in-box link step (the driver's checkout,
// a push's checkout, a layer build's recipe) runs the helper from BoxPath.
const (
	BoxDir  = "/opt/potluck-cache"
	BoxPath = BoxDir + "/bin/" + Name
)

// Script is the helper itself: POSIX sh, with node for the lockfile.
//
//go:embed potluck-deps
var Script []byte

// LinkS bounds one in-box link step, in seconds.
const LinkS = "20"

// LinkSh defines the sh function `potluck_shelf_link SHELF DIR`, the one
// link step a box runs after a checkout lands (the driver's checkout and a
// push's): the helper at SHELF/bin/potluck-deps runs `link DIR` with Go
// naming off (the step runs before the box's own commands, which may set
// GOPRIVATE) and stdin closed, and is killed after LinkS seconds without
// needing timeout(1). With no helper there (a box without the shelf) it does
// nothing and returns 0; otherwise it returns the helper's status. The
// helper's output is the caller's to keep or drop.
const LinkSh = `potluck_shelf_link() {
  [ -f "$1/bin/` + Name + `" ] && [ -x "$1/bin/` + Name + `" ] || return 0
  POTLUCK_DEPS_GO=off "$1/bin/` + Name + `" link "$2" </dev/null &
  _psl=$!
  ( sleep ` + LinkS + `; kill -9 "$_psl" ) </dev/null >/dev/null 2>&1 &
  _psk=$!
  _psr=0; wait "$_psl" || _psr=$?
  kill "$_psk" 2>/dev/null || :
  return "$_psr"
}
`
