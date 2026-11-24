//go:build !darwin

package tartpin

import (
	"fmt"
	"os"
)

// renameNew renames from to to, failing if anything is at to. Tart runs
// only on a Mac; this copy lets the package's tests run elsewhere.
func renameNew(from, to string) error {
	if lexists(to) {
		return fmt.Errorf("%s appeared meanwhile", to)
	}
	return os.Rename(from, to)
}

// swap exchanges from and to: to gets the new bundle and from the old one.
// Off a Mac it is three renames, put back on failure.
func swap(from, to string) error {
	aside := from + ".old"
	if err := os.Rename(to, aside); err != nil {
		return err
	}
	if err := os.Rename(from, to); err != nil {
		_ = os.Rename(aside, to)
		return err
	}
	return os.Rename(aside, from)
}
