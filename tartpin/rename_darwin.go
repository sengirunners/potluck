package tartpin

import "golang.org/x/sys/unix"

// renameNew renames from to to, failing if anything is at to.
func renameNew(from, to string) error { return unix.RenamexNp(from, to, unix.RENAME_EXCL) }

// swap exchanges from and to in one step: to gets the new bundle and from
// the old one.
func swap(from, to string) error { return unix.RenamexNp(from, to, unix.RENAME_SWAP) }
