package vmhost

import (
	"cmp"
	"crypto/sha512"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Shelf limits and eviction. A blob's recency is its mtime: the publisher
// sets it when it publishes the blob and moves it forward (shelfTouchSh)
// when a want names a blob already there, at most once per shelfTouchAge.
// After each publish, and when the filler starts (the sweep starts one at
// least every shelfEvictEvery), the host lists the shelf through a
// read-only helper (shelfListSh) and deletes, through the publisher's
// read-write container (shelfDeleteSh) under the publisher's lock:
//
//  1. every npm blob older than shelf_keep_s;
//  2. then the oldest blobs until the shelf's bytes are at most shelf_max_gb.
//
// Only npm blobs (shelfBlobName) and whole Go module versions (shelf_go.go:
// a version's .info, .mod, .ziphash and .zip, gone together, .info first)
// are ever deleted. A version's recency is its .info's mtime (else its
// newest file's). bin/, tools/ and the rest of goproxy/ (an older fill's
// list and lock files) count toward the shelf's bytes but are never touched:
// tools/ is the owner's. A fill stages no more than the room under
// shelf_max_gb, and nothing below the free-disk floor (persist_min_free_gb).
//
// A delete is an unlink: a box reading the blob keeps its open file, and a
// box's cache link to it dangles, which npm reads as a cache miss
// (docs/shelf.md).

// shelfEvictEvery is how often the sweep starts a filler that evicts by
// keep time when nothing was published.
var shelfEvictEvery = 6 * time.Hour

// shelfEvictStamp is when the sweep last started a filler for eviction.
func shelfEvictStamp(stateDir string) string { return filepath.Join(shelfDir(stateDir), "evicted") }

// shelfTouchAge is how old a blob's mtime must be before a want moves it
// forward: a day, or half the keep time when that is shorter, so a blob
// wanted within every keep time stays.
func shelfTouchAge(keepS int) time.Duration {
	return min(24*time.Hour, time.Duration(keepS)*time.Second/2)
}

// shelfBlobAt is one blob as the listing saw it.
type shelfBlobAt struct {
	Hex   string
	Mtime int64 // unix seconds
	Size  int64
}

// shelfGoAt is one Go module version as the listing saw it.
type shelfGoAt struct {
	Base  string // goproxy/<escaped module>/@v/<escaped version>, its files' path less the extension
	Mtime int64  // its .info's, else its newest file's (unix seconds)
	Size  int64  // its files' bytes
}

// shelfListing is the shelf by file: its npm blobs, its Go module versions,
// and the bytes of every other regular file (tools, bin, goproxy's list
// files; the publisher's staging left out).
type shelfListing struct {
	Blobs []shelfBlobAt
	Gos   []shelfGoAt
	Other int64
}

func (l shelfListing) bytes() int64 {
	n := l.Other
	for _, b := range l.Blobs {
		n += b.Size
	}
	for _, g := range l.Gos {
		n += g.Size
	}
	return n
}

// shelfEvictPlan is what eviction deletes from l at now: every blob and Go
// version last set before now-keep, then the oldest (by mtime, then name)
// until the shelf is at most max bytes. When the other files alone reach
// max nothing deletable can bring it there, so only the keep time deletes.
func shelfEvictPlan(l shelfListing, now time.Time, keep time.Duration, max int64) ([]shelfBlobAt, []shelfGoAt) {
	type item struct {
		mtime, size int64
		key         string
		blob        *shelfBlobAt
		gov         *shelfGoAt
	}
	var items []item
	for i, b := range l.Blobs {
		items = append(items, item{b.Mtime, b.Size, b.Hex, &l.Blobs[i], nil})
	}
	for i, g := range l.Gos {
		items = append(items, item{g.Mtime, g.Size, g.Base, nil, &l.Gos[i]})
	}
	slices.SortFunc(items, func(a, b item) int {
		return cmp.Or(cmp.Compare(a.mtime, b.mtime), strings.Compare(a.key, b.key))
	})
	total, cut := l.bytes(), now.Add(-keep).Unix()
	sized := l.Other < max // else the size step cannot reach max
	var blobs []shelfBlobAt
	var gos []shelfGoAt
	for _, it := range items {
		if it.mtime >= cut && (!sized || total <= max) {
			break
		}
		if it.blob != nil {
			blobs = append(blobs, *it.blob)
		} else {
			gos = append(gos, *it.gov)
		}
		total -= it.size
	}
	return blobs, gos
}

// shelfEvicted is what one eviction did.
type shelfEvicted struct {
	Deleted int   // blobs and Go module versions deleted
	Bytes   int64 // the shelf's bytes after
}

// evictShelf lists the shelf, deletes what shelfEvictPlan says, and answers
// the shelf's bytes after. It does nothing to a shelf the owner has not
// made. A blob whose mtime moved since the listing (a
// touch, or a publish by another binding sharing the volume) is left.
func evictShelf(cfg config, sh shelfer) (shelfEvicted, error) {
	if !sh.HasShelf() { // a helper's mount would make the volume
		return shelfEvicted{}, fmt.Errorf("no %s volume", ShelfVolume)
	}
	l, err := sh.ListShelf(cfg.Image)
	if err != nil {
		return shelfEvicted{}, err
	}
	res := shelfEvicted{Bytes: l.bytes()}
	plan, gos := shelfEvictPlan(l, time.Now(), time.Duration(cfg.Shelf.KeepS)*time.Second, int64(cfg.Shelf.MaxGB)*shelfGiB)
	var err1, err2 error
	if len(plan) > 0 {
		var gone map[string]bool
		gone, err1 = sh.DeleteShelf(cfg.Image, plan)
		for _, b := range plan {
			if gone[b.Hex] {
				res.Deleted++
				res.Bytes -= b.Size
			}
		}
	}
	if len(gos) > 0 {
		var gone map[string]bool
		gone, err2 = sh.DeleteShelfGo(cfg.Image, gos)
		for _, g := range gos {
			if gone[g.Base] {
				res.Deleted++
				res.Bytes -= g.Size
			}
		}
	}
	return res, errors.Join(err1, err2)
}

// String is the eviction as one line, with no names.
func (e shelfEvicted) String() string {
	return fmt.Sprintf("shelf: evicted %d blobs and module versions; %d bytes on the shelf", e.Deleted, e.Bytes)
}

// shelfFloor answers, each time it is called, whether filling must stop
// for the free-disk floor (persist_min_free_gb): under the layer's disk
// directory (where the volume lives) or the state directory (where
// fetches land).
func shelfFloor(cfg config, sh shelfer) func() error {
	layer := diskFloorOf(cfg, sh, "filling the shelf")
	return func() error {
		if err := layer(); err != nil {
			return err
		}
		if free := freeGiB(cfg.StateDir); cfg.PersistMinFreeGB > 0 && free >= 0 && free < float64(cfg.PersistMinFreeGB) {
			return fmt.Errorf("%.1f GiB free under the state directory, below persist_min_free_gb %d", free, cfg.PersistMinFreeGB)
		}
		return nil
	}
}

// shelfBlobRel is a blob's path under the blob directory (aa/bb/rest), from
// its sha512 in lowercase hex; the scripts take only these.
func shelfBlobRel(h string) (string, error) {
	if len(h) != 2*sha512.Size || strings.Trim(h, "0123456789abcdef") != "" {
		return "", fmt.Errorf("not a sha512 in hex: %q", h)
	}
	return h[:2] + "/" + h[2:4] + "/" + h[4:], nil
}

// shelfListSh prints "<mtime> <size> <path>" for each regular file on the
// shelf ($1) but the publisher's staging, paths starting "./", then "end".
// A file that goes between find and stat (another size evicting, the
// owner's fill script) is left out, not an error. The image needs find and
// stat -c (GNU or busybox).
const shelfListSh = `cd "$1" || exit 1
find . -path ./` + shelfTmpDir + ` -prune -o -type f -exec stat -c '%Y %s %n' {} + 2>/dev/null
echo end
`

// shelfEvictFor bounds one listing, delete or touch: a shelf holds tens of
// thousands of files.
var shelfEvictFor = 5 * time.Minute

// shelfIsStaged says whether the blob h (hex) waits in staging.
func shelfIsStaged(stateDir, h string) bool {
	fi, err := os.Lstat(filepath.Join(shelfStaging(stateDir), filepath.FromSlash(shelfWant{Hex: h}.blob())))
	return err == nil && fi.Mode().IsRegular()
}

// shelfStagedBytes is the bytes waiting in staging for the next publish.
func shelfStagedBytes(stateDir string) (n int64) {
	_ = filepath.WalkDir(shelfStaging(stateDir), func(_ string, d os.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			if fi, err := d.Info(); err == nil {
				n += fi.Size()
			}
		}
		return nil
	})
	return n
}

// shelfRelOK is the scripts' check of one aa/bb/rest line (in $p) naming a
// blob in a real directory: it sets f to the blob's path or answers false.
// $h is the blob directory.
const shelfRelOK = `rel_ok() {
  case $p in [0-9a-f][0-9a-f]/[0-9a-f][0-9a-f]/*) ;; *) return 1 ;; esac
  a=${p%%/*}; r=${p#*/}; b=${r%%/*}; r=${r#*/}
  case $r in ""|*[!0-9a-f]*) return 1 ;; esac
  [ ${#r} -eq 124 ] || return 1
  [ ! -L "$h/$a" ] && [ ! -L "$h/$a/$b" ] || return 1
  f=$h/$a/$b/$r
  [ -f "$f" ] && [ ! -L "$f" ]
}
`

// shelfDeleteSh deletes each blob stdin names as "<mtime> <aa/bb/rest>"
// that is still a regular file with that mtime, printing "gone <aa/bb/rest>"
// for each; $1 is the shelf, read-write. It refuses a symlink on the way to
// the blob directory (the owner's fill script also writes the volume).
const shelfDeleteSh = `set -eu
s=$1; h=$s/` + shelfBlobDir + `
for l in "$s/npm" "$s/npm/content-v2" "$h"; do
  if [ -L "$l" ]; then echo "$l is a symlink: nothing deleted" >&2; exit 5; fi
done
[ -d "$h" ] || exit 0
` + shelfRelOK + `while IFS=' ' read -r m p; do
  rel_ok || continue
  [ "$(stat -c %Y "$f")" = "$m" ] || continue
  rm -f "$f" && echo "gone $p"
done
`

// shelfTouchSh moves forward the mtime of each blob stdin names (aa/bb/rest,
// one a line) whose mtime is more than $AGE minutes old; $1 is the shelf,
// read-write. It prints "touched N".
func shelfTouchSh(ageMin int) string {
	return `set -eu
s=$1; h=$s/` + shelfBlobDir + `
for l in "$s/npm" "$s/npm/content-v2" "$h"; do
  if [ -L "$l" ]; then echo "$l is a symlink: nothing touched" >&2; exit 5; fi
done
n=0
` + shelfRelOK + `while IFS= read -r p; do
  rel_ok || continue
  [ -n "$(find "$f" -mmin +` + strconv.Itoa(ageMin) + `)" ] || continue
  touch -c "$f" && n=$((n+1))
done
echo "touched $n"
`
}

// ListShelf lists the shelf through a read-only helper.
func (o ociCLI) ListShelf(image string) (shelfListing, error) {
	out, err := o.shelfRun(image, false, []string{"DAC_READ_SEARCH"}, shelfListSh, nil, shelfEvictFor)
	if err != nil {
		return shelfListing{}, err
	}
	return parseShelfListing(out)
}

// parseShelfListing reads shelfListSh's output; one without its end line
// was cut short and is refused.
func parseShelfListing(out string) (shelfListing, error) {
	var l shelfListing
	gos := map[string]*shelfGoAt{}
	var bases []string
	info := map[string]bool{}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) == 0 || lines[len(lines)-1] != "end" {
		return l, fmt.Errorf("the shelf's listing ended early")
	}
	for _, line := range lines[:len(lines)-1] {
		f := strings.SplitN(line, " ", 3)
		if len(f) != 3 {
			continue // a name with a newline in it: not a blob, and its size is lost
		}
		mt, err1 := strconv.ParseInt(f[0], 10, 64)
		size, err2 := strconv.ParseInt(f[1], 10, 64)
		if err1 != nil || err2 != nil || size < 0 {
			continue
		}
		rel, dotted := strings.CutPrefix(f[2], "./")
		if hx := shelfBlobHex(rel); hx != "" && dotted {
			l.Blobs = append(l.Blobs, shelfBlobAt{Hex: hx, Mtime: mt, Size: size})
			continue
		}
		if g, isGo := strings.CutPrefix(rel, shelfGoproxyDir+"/"); isGo && dotted {
			if _, _, ext, ok := parseGoFile(g); ok {
				base := shelfGoproxyDir + "/" + strings.TrimSuffix(g, "."+ext)
				v := gos[base]
				if v == nil {
					v = &shelfGoAt{Base: base}
					gos[base] = v
					bases = append(bases, base)
				}
				v.Size += size
				switch {
				case ext == "info":
					v.Mtime, info[base] = mt, true
				case !info[base]:
					v.Mtime = max(v.Mtime, mt)
				}
				continue
			}
		}
		l.Other += size
	}
	for _, b := range bases {
		l.Gos = append(l.Gos, *gos[b])
	}
	return l, nil
}

// DeleteShelf deletes the blobs through the publisher's container, under
// the publisher's lock, and answers which went.
func (o ociCLI) DeleteShelf(image string, blobs []shelfBlobAt) (map[string]bool, error) {
	var in strings.Builder
	byRel := map[string]string{}
	for _, b := range blobs {
		rel, err := shelfBlobRel(b.Hex)
		if err != nil {
			return nil, err
		}
		byRel[rel] = b.Hex
		fmt.Fprintf(&in, "%d %s\n", b.Mtime, rel)
	}
	gone := map[string]bool{}
	err := o.shelfWrite(func() error {
		out, err := o.shelfRun(image, true, []string{"DAC_OVERRIDE", "FOWNER"}, shelfDeleteSh, strings.NewReader(in.String()), shelfEvictFor)
		for _, l := range strings.Split(out, "\n") {
			if h, ok := byRel[strings.TrimPrefix(l, "gone ")]; ok && strings.HasPrefix(l, "gone ") {
				gone[h] = true
			}
		}
		return err
	})
	return gone, err
}

// TouchShelf moves forward the mtime of each blob named (sha512s in hex)
// that is older than shelfTouchAge(keepS), through the publisher's
// container under its lock, and answers how many it moved.
func (o ociCLI) TouchShelf(image string, hexes []string, keepS int) (int, error) {
	if len(hexes) == 0 {
		return 0, nil
	}
	var in strings.Builder
	for _, h := range hexes {
		rel, err := shelfBlobRel(h)
		if err != nil {
			return 0, err
		}
		in.WriteString(rel + "\n")
	}
	n := 0
	err := o.shelfWrite(func() error {
		out, err := o.shelfRun(image, true, []string{"DAC_OVERRIDE", "FOWNER"}, shelfTouchSh(max(1, int(shelfTouchAge(keepS)/time.Minute))), strings.NewReader(in.String()), shelfEvictFor)
		if err != nil {
			return err
		}
		for _, l := range strings.Split(out, "\n") {
			if c, _ := fmt.Sscanf(strings.TrimSpace(l), "touched %d", &n); c == 1 {
				return nil
			}
		}
		return fmt.Errorf("the touch said %q", strings.TrimSpace(out))
	})
	return n, err
}

// shelfWrite runs f under the publisher's lock: every change to the shelf
// from this state directory goes through it.
func (o ociCLI) shelfWrite(f func() error) error {
	if o.stateDir == "" {
		return fmt.Errorf("no state directory for the shelf's lock")
	}
	if err := os.MkdirAll(shelfDir(o.stateDir), 0o700); err != nil {
		return err
	}
	return flocked(shelfLock(o.stateDir), f)
}

// stampShelfEvict records that the sweep started a filler to evict.
func stampShelfEvict(stateDir string) {
	_ = os.MkdirAll(shelfDir(stateDir), 0o700)
	_ = os.WriteFile(shelfEvictStamp(stateDir), nil, 0o600)
	now := time.Now()
	_ = os.Chtimes(shelfEvictStamp(stateDir), now, now)
}

// shelfEvictDue says whether the sweep should start a filler to evict: the
// owner set shelf_max_gb, and the last start is shelfEvictEvery old.
func shelfEvictDue(cfg config) bool {
	if cfg.Shelf.MaxGB <= 0 {
		return false
	}
	fi, err := os.Stat(shelfEvictStamp(cfg.StateDir))
	return err != nil || time.Since(fi.ModTime()) >= shelfEvictEvery
}

// logShelfEvict runs one eviction and logs it by counts.
func logShelfEvict(cfg config, sh shelfer, log io.Writer) {
	res, err := evictShelf(cfg, sh)
	if err != nil {
		fmt.Fprintln(log, "potluck-driver-tart: shelf eviction:", err)
		return
	}
	fmt.Fprintln(log, "potluck-driver-tart:", res)
}
