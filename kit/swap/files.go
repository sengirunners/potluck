package swap

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// PrevSuffix names the previous binary kept beside each one placed.
const PrevSuffix = ".prev"

// Installable lists the files of a zm build in dir that an upgrade places:
// zm itself, bd-zm and every zm-* executable, and potluck and every
// potluck-* one (the programs' new names: potluck-driver-*, ...).
func Installable(dir string) ([]string, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range ents {
		n := e.Name()
		if !(n == "zm" || n == "bd-zm" || strings.HasPrefix(n, "zm-") || n == "potluck" || strings.HasPrefix(n, "potluck-")) || strings.HasSuffix(n, PrevSuffix) || strings.HasPrefix(n, ".") {
			continue
		}
		if st, err := os.Stat(filepath.Join(dir, n)); err == nil && st.Mode().IsRegular() && st.Mode()&0o111 != 0 {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	if !contains(out, "zm") {
		return nil, fmt.Errorf("no zm executable in %s", dir)
	}
	return out, nil
}

// Placed are files swapped into Dir; Restore puts the previous ones back.
type Placed struct {
	Dir   string
	Names []string
	// Sums is each placed file's sha256 (hex), of the bytes copied from
	// src: an updater re-pins the drivers it placed with them
	// (runtarget.RepinPlaced, DRIVERS.md).
	Sums map[string]string
	prev map[string]bool // name -> a previous file was kept
}

// ErrNotRestored marks a Place failure whose restore failed too: dst may
// mix new and previous files. Any other Place error left dst as it was.
var ErrNotRestored = errors.New("the previous files are not all back")

// Place copies each name from src into dst. A file already there is kept
// as <name>.prev (a hard link, so the name never goes missing), then the
// new one is renamed over it. A failure part way restores what was done
// (else the error is ErrNotRestored).
func Place(src, dst string, names []string) (*Placed, error) {
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return nil, err
	}
	p := &Placed{Dir: dst, Sums: map[string]string{}, prev: map[string]bool{}}
	for _, n := range names {
		if err := p.one(filepath.Join(src, n), n); err != nil {
			if rerr := p.Restore(); rerr != nil {
				err = errors.Join(err, fmt.Errorf("%w: %w", ErrNotRestored, rerr))
			}
			return nil, err
		}
	}
	return p, nil
}

func (p *Placed) one(from, name string) error {
	to := filepath.Join(p.Dir, name)
	tmp := filepath.Join(p.Dir, "."+name+".new")
	sum, err := copyFile(from, tmp)
	if err != nil {
		return err
	}
	if _, err := os.Lstat(to); err == nil {
		prev := to + PrevSuffix
		_ = os.Remove(prev)
		if err := os.Link(to, prev); err != nil {
			if _, err := copyFile(to, prev); err != nil {
				os.Remove(tmp)
				return fmt.Errorf("keep %s: %w", prev, err)
			}
		}
		p.prev[name] = true
	}
	if err := os.Rename(tmp, to); err != nil {
		os.Remove(tmp)
		return err
	}
	p.Names = append(p.Names, name)
	p.Sums[name] = sum
	return nil
}

// Restore puts each previous file back (or removes a file that had none).
func (p *Placed) Restore() error {
	var errs []error
	for _, n := range p.Names {
		to := filepath.Join(p.Dir, n)
		if p.prev[n] {
			errs = append(errs, os.Rename(to+PrevSuffix, to))
		} else {
			errs = append(errs, os.Remove(to))
		}
	}
	p.Names = nil
	return errors.Join(errs...)
}

// copyFile copies from to a new file to, mode 0755 whatever the umask (a
// group-writable driver is refused: DRIVERS.md), and answers its sha256.
func copyFile(from, to string) (string, error) {
	in, err := os.Open(from)
	if err != nil {
		return "", err
	}
	defer in.Close()
	out, err := os.OpenFile(to, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	_, err = io.Copy(io.MultiWriter(out, h), in)
	if err == nil {
		err = out.Chmod(0o755)
	}
	if err == nil {
		err = out.Sync()
	}
	if err != nil {
		out.Close()
		os.Remove(to)
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), out.Close()
}

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}
