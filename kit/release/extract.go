package release

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// binaryRE names the programs a release installs (zm, bd-zm, zm-work-*,
// zm-*-conformance, and under their new names potluck, potluck-driver-*,
// potluck-conformance, potluck-e2b); docs and anything else in the tarball
// stay out.
var binaryRE = regexp.MustCompile(`^(zm|bd-zm|zm-[a-z]+(-[a-z]+)*|potluck(-[a-z0-9]+)*)$`)

// maxBinary bounds one extracted program.
const maxBinary = 512 << 20

// Extract writes the programs in a zm release tarball into dir (Format.Extract).
func Extract(tarball []byte, version, goos, goarch, dir string) ([]string, error) {
	return ZM.Extract(tarball, version, goos, goarch, dir)
}

// Extract writes the release programs in tarball (verified by CheckFile
// first) into dir, which must exist: only regular files directly under the
// tarball's own <name>_<version>_<os>_<arch>/ prefix, never a link, a path
// that leaves it, or a name twice. The tarball must carry f's main program.
// It returns the names written.
func (f Format) Extract(tarball []byte, version, goos, goarch, dir string) ([]string, error) {
	prefix := strings.TrimSuffix(f.Tarball(version, goos, goarch), ".tar.gz") + "/"
	gz, err := gzip.NewReader(bytes.NewReader(tarball))
	if err != nil {
		return nil, fmt.Errorf("release tarball: %w", err)
	}
	tr := tar.NewReader(gz)
	seen := map[string]bool{}
	var out []string
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("release tarball: %w", err)
		}
		if h.Typeflag == tar.TypeDir && h.Name == prefix {
			continue
		}
		rest, ok := strings.CutPrefix(h.Name, prefix)
		if !ok || rest == "" || strings.Contains(rest, "/") || rest == "." || rest == ".." {
			return nil, fmt.Errorf("release tarball entry %q is outside %s", h.Name, prefix)
		}
		if h.Typeflag != tar.TypeReg {
			return nil, fmt.Errorf("release tarball entry %q is not a regular file", h.Name)
		}
		if seen[rest] {
			return nil, fmt.Errorf("release tarball names %q twice", rest)
		}
		seen[rest] = true
		if !binaryRE.MatchString(rest) {
			continue // README, SPEC, API
		}
		if h.Size > maxBinary {
			return nil, fmt.Errorf("release program %s is over %d bytes", rest, maxBinary)
		}
		f, err := os.OpenFile(filepath.Join(dir, rest), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o755)
		if err != nil {
			return nil, err
		}
		_, err = io.Copy(f, io.LimitReader(tr, maxBinary))
		if err == nil {
			err = f.Chmod(0o755) // OpenFile's mode is cut by the umask (077 on CI runners)
		}
		if e := f.Close(); err == nil {
			err = e
		}
		if err != nil {
			return nil, err
		}
		out = append(out, rest)
	}
	if !seen[f.Name] {
		return nil, fmt.Errorf("release tarball has no %s", f.Name)
	}
	return out, nil
}
