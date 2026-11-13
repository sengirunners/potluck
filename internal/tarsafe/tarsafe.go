// Package tarsafe holds the rules every tar reader here applies to an entry
// it did not write itself: a name must stay inside the destination, and a
// sparse entry (which archive/tar would expand to any size) is refused. Each
// reader turns a refusal into its own error and decides what it does with
// links and devices.
package tarsafe

import (
	"archive/tar"
	"path"
	"strconv"
	"strings"
)

// Name is a tar entry's name cleaned, or ok false when it is empty, holds a
// NUL, is absolute or has a ".." part (it would land outside the
// destination).
func Name(n string) (clean string, ok bool) {
	if n == "" || strings.ContainsRune(n, 0) || strings.HasPrefix(n, "/") {
		return "", false
	}
	for _, c := range strings.Split(n, "/") {
		if c == ".." {
			return "", false
		}
	}
	return path.Clean(n), true
}

// Sparse says whether a tar entry is a GNU sparse file, in any of its
// formats (an old GNU 'S' entry, or PAX GNU.sparse records).
func Sparse(h *tar.Header) bool {
	if h.Typeflag == tar.TypeGNUSparse {
		return true
	}
	for k := range h.PAXRecords {
		if strings.HasPrefix(k, "GNU.sparse.") {
			return true
		}
	}
	return false
}

// TypeName names a tar entry type, for a refusal or a list of what was
// left out.
func TypeName(t byte) string {
	switch t {
	case tar.TypeSymlink:
		return "symlink"
	case tar.TypeLink:
		return "hard link"
	case tar.TypeChar, tar.TypeBlock:
		return "device"
	case tar.TypeFifo:
		return "fifo"
	}
	return "type " + strconv.Quote(string(t))
}
