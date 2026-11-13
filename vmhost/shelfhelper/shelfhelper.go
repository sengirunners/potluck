// Package shelfhelper holds potluck-deps, the shell helper a box runs to use the package shelf
// (docs/shelf.md). The publisher installs Script on the shelf as bin/potluck-deps.
package shelfhelper

import _ "embed"

// Name is the helper's file name on the shelf, under bin/.
const Name = "potluck-deps"

// Script is the helper itself: POSIX sh, with node for the lockfile.
//
//go:embed potluck-deps
var Script []byte
