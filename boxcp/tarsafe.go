package boxcp

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/sengirunners/potluck/internal/tarsafe"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/config"
)

type packed struct {
	path  string // the tar, alone in a temporary directory
	size  int64
	sum   string
	files int
	left  []string
	creds []string // regular files whose names look like credentials
}

// packDir writes dir's tree as a tar in a temporary file: directories,
// regular files and symlinks as links (never followed), names relative to
// dir, no owner names or ids; anything else is left out and listed. Over
// max it stops and refuses before anything is sent; a dir holding the
// temporary directory the tar is written in is refused (never a tar of
// its own tar).
func packDir(dir string, max int64) (p packed, err error) {
	tmp, err := os.MkdirTemp("", "zm-cp")
	if err != nil {
		return p, err
	}
	defer func() {
		if err != nil {
			os.RemoveAll(tmp)
		}
	}()
	if within(tmp, dir) {
		return p, api.Errorf(api.CodeBadRequest, "%s holds this machine's temporary directory, where %s writes the copy's tar: copy a directory beneath it", dir, config.Program)
	}
	p.path = filepath.Join(tmp, "up.tar")
	f, err := os.OpenFile(p.path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return p, err
	}
	defer f.Close()
	h := sha256.New()
	lw := &limitWriter{w: io.MultiWriter(f, h), max: max}
	tw := tar.NewWriter(lw)
	err = filepath.WalkDir(dir, func(full string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, full)
		if err != nil || rel == "." {
			return err
		}
		name := filepath.ToSlash(rel)
		info, err := d.Info()
		if err != nil {
			return err
		}
		link := ""
		switch t := info.Mode().Type(); {
		case t == fs.ModeSymlink:
			if link, err = os.Readlink(full); err != nil {
				return err
			}
		case t != 0 && !info.IsDir():
			p.left = append(p.left, name+" ("+kindOf(info.Mode())+")")
			return nil
		}
		hdr, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return err
		}
		hdr.Name, hdr.Uid, hdr.Gid, hdr.Uname, hdr.Gname = name, 0, 0, "", ""
		if info.IsDir() {
			hdr.Name += "/"
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		src, err := os.Open(full)
		if err != nil {
			return err
		}
		defer src.Close()
		if _, err := io.CopyN(tw, src, hdr.Size); err != nil {
			return fmt.Errorf("%s: %w (did it change while it was read?)", name, err)
		}
		p.files++
		if CredentialLike(name) {
			p.creds = append(p.creds, name)
		}
		return nil
	})
	if err == nil {
		err = tw.Close()
	}
	if lw.over {
		return p, api.Errorf(api.CodeTooLarge, "%s is over the %s limit as a tar: nothing was copied (raise it up to %s, or copy less)", dir, mib(max), mib(HardMax))
	}
	if err != nil {
		return p, err
	}
	p.size, p.sum = lw.n, hex.EncodeToString(h.Sum(nil))
	return p, f.Close()
}

// within says whether p is dir or beneath it, symlinks resolved.
func within(p, dir string) bool {
	rp, err1 := filepath.EvalSymlinks(p)
	rd, err2 := filepath.EvalSymlinks(dir)
	if err1 != nil || err2 != nil {
		return true // unsure: refuse
	}
	rel, err := filepath.Rel(rd, rp)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func kindOf(m fs.FileMode) string {
	switch t := m.Type(); {
	case t&fs.ModeSymlink != 0:
		return "symlink"
	case t&fs.ModeNamedPipe != 0:
		return "fifo"
	case t&fs.ModeSocket != 0:
		return "socket"
	case t&fs.ModeDevice != 0:
		return "device"
	}
	return "special file"
}

type limitWriter struct {
	w    io.Writer
	n    int64
	max  int64
	over bool
}

func (l *limitWriter) Write(p []byte) (int, error) {
	if l.n+int64(len(p)) > l.max {
		l.over = true
		return 0, errors.New("over the limit")
	}
	n, err := l.w.Write(p)
	l.n += int64(n)
	return n, err
}

// unpack writes the box's tar r as the new directory final, all or
// nothing: it is unpacked into a fresh directory beside final under
// os.Root (no name, symlink or ".." reaches outside it), then renamed. A
// name that is absolute or climbs out with ".." refuses the whole copy;
// only directories and regular files are written (modes at most 0755, no
// set-id bits, no owners); symlinks, hard links and devices are left out
// and listed. A sparse entry (GNU sparse records, which archive/tar would
// expand to any size) refuses the copy, and so does writing past limit
// bytes in all. Every name is the box's: data, quoted when shown.
func unpack(r io.Reader, final string, limit int64) (files int, left []string, err error) {
	tmp, err := os.MkdirTemp(filepath.Dir(final), ".zm-cp-*")
	if err != nil {
		return 0, nil, err
	}
	defer func() {
		if err != nil {
			os.RemoveAll(tmp)
		}
	}()
	root, err := os.OpenRoot(tmp)
	if err != nil {
		return 0, nil, err
	}
	defer root.Close()
	top, more := os.FileMode(0o755), 0
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return 0, nil, fmt.Errorf("the box's archive is not a tar: %v: refused, nothing written", err)
		}
		name, err := entryName(hdr.Name)
		if err != nil {
			return 0, nil, err
		}
		if tarsafe.Sparse(hdr) {
			return 0, nil, api.Errorf(api.CodeForbidden, "the box's archive holds %s as a sparse file (it would unpack past its size): refused, nothing written", strconv.Quote(name))
		}
		perm := os.FileMode(hdr.Mode).Perm() & 0o755
		switch hdr.Typeflag {
		case tar.TypeDir:
			if name == "." {
				top = perm | 0o700
				continue
			}
			if err := root.MkdirAll(name, perm|0o700); err != nil {
				return 0, nil, fmt.Errorf("writing %s: %w", strconv.Quote(name), err)
			}
		case tar.TypeReg, '\x00':
			if name == "." {
				return 0, nil, fmt.Errorf("the box's archive names a file at its top: refused, nothing written")
			}
			if err := writeEntry(root, name, perm|0o600, tr, &limit); err != nil {
				return 0, nil, err
			}
			files++
		case tar.TypeXGlobalHeader:
		default:
			if more++; len(left) < maxListed {
				left = append(left, strconv.Quote(name)+" ("+tarsafe.TypeName(hdr.Typeflag)+")")
			}
		}
	}
	if more > len(left) {
		left = append(left, fmt.Sprintf("and %d more", more-len(left)))
	}
	if err := root.Chmod(".", top); err != nil {
		return 0, nil, err
	}
	if _, err := os.Lstat(final); err == nil {
		return 0, nil, api.Errorf(api.CodeConflict, "%s appeared while copying: nothing written", final)
	}
	return files, left, os.Rename(tmp, final)
}

// maxListed bounds the entries a copy lists as left out.
const maxListed = 50

// writeEntry writes r to name, taking its bytes from *left (the copy's
// bytes still allowed) and refusing past it.
func writeEntry(root *os.Root, name string, perm os.FileMode, r io.Reader, left *int64) error {
	if d := path.Dir(name); d != "." {
		if err := root.MkdirAll(d, 0o755); err != nil {
			return fmt.Errorf("writing %s: %w", strconv.Quote(name), err)
		}
	}
	f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return fmt.Errorf("writing %s: %w", strconv.Quote(name), err)
	}
	n, err := io.CopyN(f, r, *left+1)
	if *left -= n; *left < 0 {
		f.Close()
		return api.Errorf(api.CodeTooLarge, "the box's archive unpacks past the copy's limit: refused, nothing written")
	}
	if err != nil && !errors.Is(err, io.EOF) {
		f.Close()
		return err
	}
	if err := f.Chmod(perm); err != nil { // past the umask, as tar does
		f.Close()
		return err
	}
	return f.Close()
}

// entryName is a tar entry's name inside the destination, or a refusal
// when it is absolute or climbs out with "..".
func entryName(n string) (string, error) {
	clean, ok := tarsafe.Name(n)
	if !ok {
		q := []rune(strconv.Quote(n))
		if len(q) > 200 {
			q = append(q[:200], []rune("...")...)
		}
		return "", api.Errorf(api.CodeForbidden, "the box's archive names %s, outside the destination: refused, nothing written", string(q))
	}
	return clean, nil
}
