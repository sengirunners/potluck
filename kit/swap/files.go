package swap

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"syscall"
	"time"
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
	kept map[string]kept // what Placing says of Dir, by name
	lock *os.File        // Dir's, from Begin to Finish or Restore
}

// ErrNotRestored marks a Place failure whose restore failed too: dst may
// mix new and previous files. Any other Place error left dst as it was, or
// as it was before a Place that was cut off there.
var ErrNotRestored = errors.New("the previous files are not all back")

// Place copies each name from src into dst: all of them beside where they
// go first, so a full disk changes nothing, then each into place, in the
// order given (the last is there only once the others are). A file already
// there is kept as <name>.prev (a hard link, so the name never goes
// missing), then the new one is renamed over it. A failure part way restores
// what was done (else the error is ErrNotRestored).
//
// Cut off part way, dst says so (Placing), and the next Place keeps the
// .prev files of the first: they, not a mix, are what it replaces. Two at
// once in dst take turns.
func Place(src, dst string, names []string) (*Placed, error) {
	p, err := Begin(context.Background(), src, dst, names, func() {})
	if err == nil {
		err = p.Finish()
	}
	return p, err
}

// Begin is Place for a caller with more to do before the files are what dst
// runs (a daemon to restart on them): dst says it is unfinished, and is this
// caller's, until Finish or Restore. With every file of src's in place already and no such word, it
// changes nothing, and Restore has nothing to put back. While another has dst
// it waits, after waiting(), until ctx ends.
func Begin(ctx context.Context, src, dst string, names []string, waiting func()) (_ *Placed, err error) {
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return nil, err
	}
	lock, err := os.Open(dst)
	if err != nil {
		return nil, err
	}
	p := &Placed{Dir: dst, Sums: map[string]string{}, lock: lock}
	defer func() {
		if err != nil {
			p.unlock()
		}
	}()
	if err := lockDir(ctx, lock, waiting); err != nil {
		return nil, err
	}
	defer func() {
		for _, n := range names {
			_ = os.Remove(p.at(stagedName(n))) // gone once placed
		}
	}()
	there := true // all of it is
	for _, n := range names {
		step()
		sum, err := copyFile(filepath.Join(src, n), p.at(stagedName(n)))
		if err != nil {
			return nil, err
		}
		p.Sums[n] = sum
		there = there && placedSum(p.at(n), sum)
	}
	if there && !Placing(dst) {
		return p, nil
	}
	if err := p.keepPrev(names); err != nil {
		return nil, err
	}
	for _, n := range names {
		step()
		if err := os.Rename(p.at(stagedName(n)), p.at(n)); err != nil {
			if rerr := p.Restore(); rerr != nil {
				err = errors.Join(err, fmt.Errorf("%w: %w", ErrNotRestored, rerr))
			}
			return nil, err
		}
		p.Names = append(p.Names, n)
	}
	step()
	return p, nil
}

// lockDir takes dir's lock, or ends with ctx.
func lockDir(ctx context.Context, dir *os.File, waiting func()) error {
	for first := true; ; first = false {
		err := syscall.Flock(int(dir.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err != syscall.EWOULDBLOCK && err != syscall.EINTR {
			return err
		}
		if first {
			waiting()
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// step is called before each change of dst; a test ends the process in it.
var step = func() {}

// placingName is the file in dst that says a Place there was begun and
// has not ended: each name on a line, with "+" before one whose .prev is the
// file it replaced, "-" when there was none, and the sha256 of the file it
// puts there after it, then of those a Place cut off before it put there.
const placingName = ".placing"

// kept is what a Place there does with a name: whether the file it replaces
// is the name's .prev (else there was none), and the sha256 (hex) of the one
// it puts there, then of those a Place cut off before it put there.
type kept struct {
	prev bool
	sums []string
}

// keepPrev keeps what names replace as .prev, and then says so in dst. A
// name an unfinished Place already kept stays as that left it, unless its file
// was put there some other way since.
func (p *Placed) keepPrev(names []string) error {
	was, err := readPlacing(p.Dir)
	if err != nil {
		return err
	}
	had := len(was)
	if len(names) > 0 {
		if last := names[len(names)-1]; p.others(last, was[last]) { // so what is there is what dst ran
			clear(was)
		}
	}
	maps.DeleteFunc(was, p.others)
	if len(was) < had { // said first: a .prev made below is the sign of others' no more
		if err := p.writePlacing(was); err != nil {
			return err
		}
	}
	for _, n := range names {
		k, ok := was[n]
		if ok && (!k.prev || p.has(n+PrevSuffix)) {
			was[n] = kept{k.prev, slices.Insert(slices.DeleteFunc(k.sums, func(s string) bool { return s == p.Sums[n] }), 0, p.Sums[n])}
			continue
		}
		was[n] = kept{p.has(n), []string{p.Sums[n]}}
		if !was[n].prev {
			continue
		}
		step()
		_ = os.Remove(p.at(n + PrevSuffix))
		if err := os.Link(p.at(n), p.at(n+PrevSuffix)); err != nil {
			if _, err := copyFile(p.at(n), p.at(n+PrevSuffix)); err != nil {
				return fmt.Errorf("keep %s: %w", p.at(n+PrevSuffix), err)
			}
		}
	}
	if err := p.writePlacing(was); err != nil {
		return err
	}
	p.kept = was
	return nil
}

// writePlacing says in dst that a Place that keeps was is unfinished.
func (p *Placed) writePlacing(was map[string]kept) error {
	var b strings.Builder
	for _, n := range slices.Sorted(maps.Keys(was)) {
		b.WriteString(map[bool]string{true: "+", false: "-"}[was[n].prev] + n + " " + strings.Join(was[n].sums, " ") + "\n")
	}
	step()
	tmp := p.at(placingName + ".new")
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	_, err = f.WriteString(b.String())
	if err == nil {
		err = f.Sync() // it must outlive a kill of the machine too
	}
	if err := errors.Join(err, f.Close()); err != nil {
		return err
	}
	return os.Rename(tmp, p.at(placingName))
}

// others reports that name's file is none that the unfinished Places that kept
// k put there, nor what they replaced.
func (p *Placed) others(name string, k kept) bool {
	if !p.has(name) || len(k.sums) == 0 {
		return false
	}
	now, err := fileSum(p.at(name))
	if err != nil {
		return true
	}
	if slices.Contains(k.sums, now) {
		return false
	}
	was, err := fileSum(p.at(name + PrevSuffix))
	return !k.prev || err == nil && was != now
}

// readPlacing is what dir's unfinished Place kept (none: empty).
func readPlacing(dir string) (map[string]kept, error) {
	out := map[string]kept{}
	b, err := os.ReadFile(filepath.Join(dir, placingName))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	for l := range strings.Lines(string(b)) {
		if f := strings.Fields(l); len(f) > 1 && len(f[0]) > 1 {
			out[f[0][1:]] = kept{f[0][0] == '+', f[1:]}
		}
	}
	return out, nil
}

// Placing reports that a Place in dir, or what comes after a Begin there, goes
// on or was cut off.
func Placing(dir string) bool {
	_, err := os.Lstat(filepath.Join(dir, placingName))
	return err == nil
}

// Settle says that last, the file in dir a Place there puts last, runs. So
// what is in dir is what the next Place replaces, when it is a whole build
// (the cut-off Place's, or one put there some other way), or there was none
// before it to go back to. Else, or while a Place goes on, it stays as it is.
func Settle(dir, last string) error {
	lock, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err == syscall.EWOULDBLOCK {
		return nil
	} else if err != nil {
		return err
	}
	names, err := readPlacing(dir)
	if err != nil {
		return err
	}
	k, ok := names[last]
	whole := ok
	for n, k := range names {
		whole = whole && placedSum(filepath.Join(dir, n), k.sums[0])
	}
	if !ok || !whole && k.prev && !(&Placed{Dir: dir}).others(last, k) {
		return nil
	}
	return endPlacing(dir)
}

// endPlacing says that no Place in dir goes on or was cut off.
func endPlacing(dir string) error {
	if err := os.Remove(filepath.Join(dir, placingName)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// Leftovers are the files in dir that a Place of names there makes and one
// cut off leaves, beside names themselves and their .prev.
func Leftovers(dir string, names []string) (files []string) {
	all := []string{placingName, placingName + ".new"}
	for _, n := range names {
		all = append(all, stagedName(n))
	}
	for _, n := range all {
		if p := filepath.Join(dir, n); lexists(p) {
			files = append(files, p)
		}
	}
	return files
}

func (p *Placed) at(name string) string { return filepath.Join(p.Dir, name) }
func (p *Placed) has(name string) bool  { return lexists(p.at(name)) }

func lexists(p string) bool { _, err := os.Lstat(p); return err == nil }

// stagedName is name's copy before it goes into place.
func stagedName(name string) string { return "." + name + ".new" }

// placedSum reports that the file at path is one a Place put there: a regular
// file of its mode, with this sha256 (hex).
func placedSum(path, sum string) bool {
	if fi, err := os.Lstat(path); err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm() != placedMode {
		return false
	}
	now, err := fileSum(path)
	return err == nil && now == sum
}

// fileSum is the sha256 (hex) of the file at path.
func fileSum(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	_, err = io.Copy(h, f)
	return hex.EncodeToString(h.Sum(nil)), err
}

// Finish ends a Begin: the files in place are what dst runs.
func (p *Placed) Finish() error {
	defer p.unlock()
	step()
	return endPlacing(p.Dir)
}

// unlock lets others place in Dir.
func (p *Placed) unlock() {
	if p.lock != nil {
		p.lock.Close()
		p.lock = nil
	}
}

// Restore puts each previous file back (or removes a file that had none): of
// what a Place cut off in Dir put there too, else the files would be a mix.
func (p *Placed) Restore() error {
	defer p.unlock()
	var errs []error
	for _, n := range slices.Sorted(maps.Keys(p.kept)) {
		step()
		from := p.at(n + PrevSuffix)
		if !p.kept[n].prev {
			if err := os.Remove(p.at(n)); !errors.Is(err, fs.ErrNotExist) {
				errs = append(errs, err)
			}
		} else if p.has(n + PrevSuffix) { // else a Restore cut off put it back
			errs = append(errs, os.Rename(from, p.at(n)))
			_ = os.Remove(from) // a link to the file in place: the rename changed nothing
		}
	}
	p.Names, p.kept = nil, nil
	if err := errors.Join(errs...); err != nil {
		return err
	}
	return p.Finish()
}

// placedMode is a placed file's.
const placedMode = 0o755

// copyFile copies from to a new file to, mode 0755 whatever the umask (a
// group-writable driver is refused: DRIVERS.md), and answers its sha256.
func copyFile(from, to string) (string, error) {
	in, err := os.Open(from)
	if err != nil {
		return "", err
	}
	defer in.Close()
	out, err := os.OpenFile(to, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, placedMode)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	_, err = io.Copy(io.MultiWriter(out, h), in)
	if err == nil {
		err = out.Chmod(placedMode)
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
