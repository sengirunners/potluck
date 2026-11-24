package softnet

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"
)

// Archive pins the gzipped tar file a build is published in: its SHA-256
// and size, and the name of the executable inside it.
type Archive struct {
	SHA256 string
	Bytes  int64
	Member string
}

// Unpack writes build b's executable from the archive at src (b.Archive)
// to dst, a new file. Nothing in the archive is read before the whole
// file has the pinned SHA-256 and size; then only the one regular file
// named b.Archive.Member is written, at most b.Bytes+1 bytes of it. The
// caller still checks dst against b's SHA-256 and size, and the root half
// of an install checks its own copy again; Unpack never runs as root.
func (b Build) Unpack(src, dst string) error {
	a := b.Archive
	if a == nil {
		return fmt.Errorf("Softnet %s is not published in an archive", b.ID)
	}
	f, err := os.OpenFile(src, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(f, a.Bytes+1))
	if err != nil {
		return fmt.Errorf("cannot read %s: %w", src, err)
	}
	if got := hex.EncodeToString(h.Sum(nil)); n != a.Bytes || got != a.SHA256 {
		return fmt.Errorf("%s is not Softnet %s's archive: it has %d bytes and SHA-256 %s; the pinned archive has %d bytes and SHA-256 %s. Nothing was installed", src, b.ID, n, got, a.Bytes, a.SHA256)
	}
	// Read the same open file again, so the bytes unpacked are the bytes
	// hashed even if src is replaced meanwhile.
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	zr, err := gzip.NewReader(io.LimitReader(f, a.Bytes))
	if err != nil {
		return fmt.Errorf("Softnet %s's archive: %w", b.ID, err)
	}
	tr := tar.NewReader(zr)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return fmt.Errorf("Softnet %s's archive has no %s", b.ID, a.Member)
		}
		if err != nil {
			return fmt.Errorf("Softnet %s's archive: %w", b.ID, err)
		}
		if hdr.Name != a.Member {
			continue
		}
		if hdr.Typeflag != tar.TypeReg {
			return fmt.Errorf("%s in Softnet %s's archive is not a regular file", a.Member, b.ID)
		}
		out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o700)
		if err != nil {
			return err
		}
		_, err = io.Copy(out, io.LimitReader(tr, b.Bytes+1))
		if cerr := out.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			os.Remove(dst)
			return fmt.Errorf("cannot unpack Softnet %s: %w", b.ID, err)
		}
		return nil
	}
}
