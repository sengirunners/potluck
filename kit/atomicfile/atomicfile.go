// Package atomicfile writes a private file so that a crash leaves either the
// old file or the new one, never a part of either.
package atomicfile

import (
	"os"
	"path/filepath"
)

// WritePrivate writes b to path (mode 0600) through a temp file in the same
// directory, which must exist (a missing one is an error, never made here: a
// directory removed on purpose is not brought back): the temp file is
// synced, renamed over path, and the directory synced. A directory that
// cannot be synced (some file systems refuse) does not fail the write: the
// file is already replaced.
func WritePrivate(path string, b []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		d.Close()
	}
	return nil
}
