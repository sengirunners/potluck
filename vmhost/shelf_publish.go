package vmhost

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha512"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/sengirunners/potluck/internal/tarsafe"
	"github.com/sengirunners/potluck/vmhost/shelfhelper"
)

// The publisher is the one piece of the driver that mounts the shelf
// writable (the owner's scripts/box-cache-fill.sh is the other writer).
// One publish, under the host's shelf lock:
//
//  1. The host packs its staging directory as a tar and reads it back
//     through shelfSpool, which refuses the whole publish (nothing sent) on
//     a link, a device, a name outside the tree, a sparse entry or any file
//     that is neither an npm blob nor a GOPROXY file by its path, re-hashes
//     each blob, and leaves out one whose sha512 is not its name; of Go
//     files (shelf_go.go) it leaves out a .info not naming its version and
//     a .zip whose Go hash is not the .ziphash staged with it. What passes
//     is written again with fixed headers into a spool file in the host's
//     shelf directory, followed by the helper (shelfHelper) and an end
//     marker.
//  2. A throwaway container from the binding's image under its runtime, with
//     no network, a read-only root and the shelf mounted read-write, reads
//     the spool on stdin (shelfPublishSh): it unpacks into a fresh directory
//     under npm/.tmp, publishes nothing unless the end marker arrived,
//     re-hashes every blob again, sets its mtime to now (its recency:
//     shelf_evict.go) and its mode to 0444, and renames it into place;
//     then the Go files the same way, every .zip first, then .ziphash, .mod
//     and .info last, so a version's .info never names a file not yet there.
//
// A file's bytes are complete before its name appears, so a publish killed
// at any point leaves only whole, verified files visible; what it left in
// npm/.tmp (root's, 0700) a later publish removes once it is stale
// (shelfStaleMin).

// shelfHelper is the in-box helper the publisher writes at shelfHelperPath
// with every publish (tests swap it).
var shelfHelper = shelfhelper.Script

// shelfBlobMax bounds one npm tarball on the shelf.
const shelfBlobMax = 256 << 20

// shelfEnd is the spool's last entry: the container publishes nothing
// unless it unpacked it, so a stream cut off anywhere publishes nothing.
const shelfEnd = ".potluck-end"

// shelfStaleMin is the age, in minutes, past which a directory in npm/.tmp
// is a dead publish's: twice shelfPublishFor.
const shelfStaleMin = 60

// shelfMeasureFor bounds one measurement (describe waits for it).
var shelfMeasureFor = 30 * time.Second

// shelfPublishFor bounds one publish.
var shelfPublishFor = 30 * time.Minute

// shelfPublished is what one publish did.
type shelfPublished struct {
	Published int // blobs renamed into place
	Present   int // blobs whose name was already there (taken as the same bytes; not re-read)
	Dropped   int // blobs left out because their bytes are not their name (host or container)
	Stats     shelfStats
}

// shelfStatsSh prints the volume's disk use in bytes (du's KiB, staging
// included) and its regular files (staging left out), or nothing when du
// gives no number as "bytes N files N";
// $1 is the shelf.
const shelfStatsSh = `k=$(du -skx "$1" | cut -f1)
case $k in ""|*[!0-9]*) echo "du said \"$k\"" >&2; exit 0 ;; esac
echo "bytes $((k * 1024)) files $(find "$1" \( -path "$1/npm/.tmp" \) -prune -o -type f -print | wc -l)"
`

// shelfPublishSh is the publisher's script; $1 is the shelf, mounted
// read-write, and stdin the spool. Bindings with other state directories
// (other host locks) share one engine's volume, and a lock inside it would
// not hold across sandboxed kernels, so publishers may overlap: each works
// in its own npm/.tmp directory, removes only directories there older than
// shelfStaleMin (a publish is bounded well below it), and renames only
// verified blobs, which are the same bytes whoever renames them. It refuses
// a symlink where it makes or enters a directory (the owner's fill script
// also writes the volume). It prints "published N present N
// dropped N", then shelfStatsSh's line.
var shelfPublishSh = `set -eu
umask 022
s=$1; h=$s/` + shelfBlobDir + `; tmp=$s/` + shelfTmpDir + `
nolink() { for l; do if [ -L "$l" ]; then echo "$l is a symlink: nothing published" >&2; exit 5; fi; done; }
nolink "$s/npm" "$s/npm/content-v2" "$h" "$tmp" "$s/bin"
mkdir -p "$tmp" "$h"
chmod 0700 "$tmp"
chmod 0755 "$s/npm" "$s/npm/content-v2" "$h"
find "$tmp" -mindepth 1 -maxdepth 1 -mmin +` + strconv.Itoa(shelfStaleMin) + ` -exec rm -rf {} + ||
  echo "clearing stale npm/.tmp directories failed (another publisher's went first?)" >&2
t=$(mktemp -d "$tmp/p.XXXXXX")
tar -xf - -C "$t"
[ -f "$t/` + shelfEnd + `" ] || { echo "the spool ended early: nothing published" >&2; exit 3; }
cd "$t"
n=$(find . -type f ! -name ` + shelfEnd + ` ! -path './` + shelfGoproxyDir + `/*' | {
p=0 k=0 x=0
while IFS= read -r f; do
  case $f in
  (./` + shelfBlobDir + `/[0-9a-f][0-9a-f]/[0-9a-f][0-9a-f]/*)
    r=${f#./` + shelfBlobDir + `/}; a=${r%%/*}; r=${r#*/}; b=${r%%/*}; r=${r#*/}
    case $r in (""|*[!0-9a-f]*) rm -f "$f"; x=$((x+1)); continue ;; esac
    if [ ${#r} -ne 124 ]; then rm -f "$f"; x=$((x+1)); continue; fi
    sum=$(sha512sum < "$f"); sum=${sum%% *}
    if [ "$sum" != "$a$b$r" ]; then rm -f "$f"; x=$((x+1)); continue; fi
    if [ -e "$h/$a/$b/$r" ] || [ -L "$h/$a/$b/$r" ]; then rm -f "$f"; k=$((k+1)); continue; fi
    nolink "$h/$a" "$h/$a/$b"
    mkdir -p "$h/$a/$b"
    touch "$f"
    chmod 0444 "$f"
    mv -f "$f" "$h/$a/$b/$r"
    p=$((p+1)) ;;
  (./` + shelfHelperPath + `)
    mkdir -p "$s/bin"
    if [ -d "$s/` + shelfHelperPath + `" ]; then echo "` + shelfHelperPath + ` is a directory" >&2; exit 4; fi
    chmod 0555 "$f"
    mv -f "$f" "$s/` + shelfHelperPath + `" ;;
  (*) rm -f "$f"; x=$((x+1)) ;;
  esac
done
echo "$p $k $x"
})
g=$(for e in zip ziphash mod info; do [ ! -d ./` + shelfGoproxyDir + ` ] || find ./` + shelfGoproxyDir + ` -type f -name "*.$e"; done | {
p=0 k=0 x=0
while IFS= read -r f; do
  case $f in
  (*/../*|*/./*|*//*) rm -f "$f"; x=$((x+1)); continue ;;
  (./` + shelfGoproxyDir + `/?*/@v/?*.zip|./` + shelfGoproxyDir + `/?*/@v/?*.ziphash|./` + shelfGoproxyDir + `/?*/@v/?*.mod|./` + shelfGoproxyDir + `/?*/@v/?*.info) ;;
  (*) rm -f "$f"; x=$((x+1)); continue ;;
  esac
  r=${f#./}; d=${r%/*}; c=$s
  set -f; IFS=/
  for e in $d; do c=$c/$e; if [ -L "$c" ]; then echo "a symlink in ` + shelfGoproxyDir + `/: nothing published" >&2; exit 5; fi; done
  unset IFS; set +f
  if [ -e "$s/$r" ] || [ -L "$s/$r" ]; then rm -f "$f"; k=$((k+1)); continue; fi
  { mkdir -p "$s/$d" && touch "$f" && chmod 0444 "$f" && mv -f "$f" "$s/$r"; } 2>/dev/null ||
    { echo "a move into ` + shelfGoproxyDir + `/ failed: nothing more published" >&2; exit 6; }
  p=$((p+1))
done
echo "$p $k $x"
})
if [ -d "$s/` + shelfGoproxyDir + `" ] && [ ! -L "$s/` + shelfGoproxyDir + `" ]; then
  # An older fill's version lists, locks and checksum database cache: Go would
  # answer @latest from a list naming versions the shelf no longer has.
  rm -rf "$s/` + shelfGoproxyDir + `/sumdb" 2>/dev/null || :
  find "$s/` + shelfGoproxyDir + `" -type f -path '*/@v/*' \( -name list -o -name '*.lock' \) -exec rm -f {} + 2>/dev/null || :
fi
echo "$n $g" | { read -r p k x gp gk gx; echo "published $((p+gp)) present $((k+gk)) dropped $((x+gx))"; }
cd /
rm -rf "$t"
` + shelfStatsSh

// errShelfRefused is a staging tree the publisher will not take at all.
var errShelfRefused = errors.New("refused, nothing published")

// shelfSpool reads the tar r (the host's staging) and writes to spool what
// may go on the shelf, with fixed headers: only npm blobs whose sha512 is
// their name, then the helper (if any) and the end marker. Any entry that is
// not a directory or a regular file, has a name outside the tree, is sparse,
// or is a file at any path but a blob's refuses the whole tar. sent lists
// the staging names written; dropped counts blobs left out (bytes that are
// not their name, or over shelfBlobMax).
func shelfSpool(r io.Reader, spool *os.File) (sent []string, dropped int, err error) {
	tw := tar.NewWriter(spool)
	tr := tar.NewReader(r)
	gs := goSpool{dir: filepath.Dir(spool.Name())}
	defer gs.settle()
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			dropped += gs.settle()
			break
		}
		if err != nil {
			return nil, 0, fmt.Errorf("the staging tar: %v: %w", err, errShelfRefused)
		}
		name, ok := tarsafe.Name(hdr.Name)
		if !ok {
			return nil, 0, fmt.Errorf("staging names %s, outside the shelf: %w", strconv.Quote(hdr.Name), errShelfRefused)
		}
		if tarsafe.Sparse(hdr) {
			return nil, 0, fmt.Errorf("staging holds %s as a sparse file: %w", strconv.Quote(name), errShelfRefused)
		}
		switch hdr.Typeflag {
		case tar.TypeDir, tar.TypeXGlobalHeader:
			continue
		case tar.TypeReg, '\x00': // '\x00': an old tar's regular file
		default:
			return nil, 0, fmt.Errorf("staging holds %s, which is not a regular file (%s): %w", strconv.Quote(name), tarsafe.TypeName(hdr.Typeflag), errShelfRefused)
		}
		if rel, isGo := strings.CutPrefix(name, shelfGoproxyDir+"/"); isGo {
			_, ver, ext, ok := parseGoFile(rel)
			if !ok {
				return nil, 0, fmt.Errorf("staging holds a file under %s/ that is not a GOPROXY file's path: %w", shelfGoproxyDir, errShelfRefused)
			}
			sent = append(sent, name)
			n, err := gs.add(tw, tr, hdr.Size, name, ver, ext)
			if err != nil {
				return nil, 0, err
			}
			dropped += n
			continue
		}
		want := shelfBlobHex(name)
		if want == "" {
			return nil, 0, fmt.Errorf("staging holds %s, which is not an npm blob's path (%s/<aa>/<bb>/<rest of its sha512>) or a GOPROXY file's: %w", strconv.Quote(name), shelfBlobDir, errShelfRefused)
		}
		sent = append(sent, name)
		if hdr.Size > shelfBlobMax {
			dropped++
			continue
		}
		// Written first, taken back if its bytes are not its name: the
		// writer holds nothing between entries once flushed.
		at, err := spool.Seek(0, io.SeekCurrent)
		if err != nil {
			return nil, 0, err
		}
		h := sha512.New()
		if err := tw.WriteHeader(shelfHeader(name, 0o444, hdr.Size)); err != nil {
			return nil, 0, err
		}
		if _, err := io.CopyN(io.MultiWriter(tw, h), tr, hdr.Size); err != nil {
			return nil, 0, fmt.Errorf("staging %s: %w", strconv.Quote(name), err)
		}
		if err := tw.Flush(); err != nil {
			return nil, 0, err
		}
		if hex.EncodeToString(h.Sum(nil)) != want {
			dropped++
			if err := spool.Truncate(at); err != nil {
				return nil, 0, err
			}
			if _, err := spool.Seek(at, io.SeekStart); err != nil {
				return nil, 0, err
			}
		}
	}
	if shelfHelper != nil {
		if err := tw.WriteHeader(shelfHeader(shelfHelperPath, 0o555, int64(len(shelfHelper)))); err != nil {
			return nil, 0, err
		}
		if _, err := tw.Write(shelfHelper); err != nil {
			return nil, 0, err
		}
	}
	if err := tw.WriteHeader(shelfHeader(shelfEnd, 0o444, 0)); err != nil {
		return nil, 0, err
	}
	return sent, dropped, tw.Close()
}

// goSpool is shelfSpool's state for Go files. A .zip is copied aside and
// hashed (Go's h1) when it comes, and goes into the spool only when its
// .ziphash comes and names that hash; a zip whose .ziphash never comes is
// dropped at the end. Pairing is by name, not by order: another version's
// files may sort between them (v1.0.0-a.zip's files lie between
// v1.0.0-a's .zip and .ziphash).
type goSpool struct {
	dir     string // where zips wait: the spool's directory
	pending map[string]goHeld
}

type goHeld struct {
	f    *os.File
	size int64
	h1   string
}

func (g *goSpool) drop(base string) {
	if h, ok := g.pending[base]; ok {
		h.f.Close()
		os.Remove(h.f.Name())
		delete(g.pending, base)
	}
}

// settle drops every held zip (their .ziphash never came) and answers how
// many.
func (g *goSpool) settle() int {
	n := len(g.pending)
	for b := range g.pending {
		g.drop(b)
	}
	return n
}

// add spools one Go file (name in staging, ver its version, ext its type)
// of size bytes from tr, or drops it: over its cap, a .info not naming the
// version, a .zip Go cannot hash, a .ziphash not its zip's. It answers how
// many files it dropped.
func (g *goSpool) add(tw *tar.Writer, tr io.Reader, size int64, name, ver, ext string) (int, error) {
	base := strings.TrimSuffix(name, "."+ext)
	if size > goFileMax(ext) {
		if ext == "ziphash" && g.pending[base].f != nil {
			g.drop(base)
			return 2, nil
		}
		return 1, nil
	}
	write := func(n string, r io.Reader, size int64) error {
		if err := tw.WriteHeader(shelfHeader(n, 0o444, size)); err != nil {
			return err
		}
		if _, err := io.CopyN(tw, r, size); err != nil {
			return fmt.Errorf("staging a GOPROXY file: %w", err)
		}
		return tw.Flush()
	}
	switch ext {
	case "zip":
		g.drop(base) // a name once
		f, err := os.CreateTemp(g.dir, "zip-*")
		if err != nil {
			return 0, err
		}
		if _, err := io.CopyN(f, tr, size); err != nil {
			f.Close()
			os.Remove(f.Name())
			return 0, fmt.Errorf("staging a GOPROXY file: %w", err)
		}
		h1, err := goZipHash(f, size)
		if err != nil {
			f.Close()
			os.Remove(f.Name())
			return 1, nil
		}
		if g.pending == nil {
			g.pending = map[string]goHeld{}
		}
		g.pending[base] = goHeld{f: f, size: size, h1: h1}
		return 0, nil
	case "info", "ziphash":
		b, err := io.ReadAll(io.LimitReader(tr, size))
		if err != nil {
			return 0, fmt.Errorf("staging a GOPROXY file: %w", err)
		}
		if ext == "info" {
			if !goInfoOK(b, ver) {
				return 1, nil
			}
			return 0, write(name, bytes.NewReader(b), int64(len(b)))
		}
		h, held := g.pending[base]
		if !held {
			return 1, nil // its zip was dropped, or never staged
		}
		defer g.drop(base)
		if strings.TrimSpace(string(b)) != h.h1 {
			return 2, nil
		}
		if err := write(base+".zip", io.NewSectionReader(h.f, 0, h.size), h.size); err != nil {
			return 0, err
		}
		return 0, write(name, bytes.NewReader(b), int64(len(b)))
	}
	return 0, write(name, tr, size) // mod
}

func shelfHeader(name string, mode, size int64) *tar.Header {
	return &tar.Header{Typeflag: tar.TypeReg, Name: name, Mode: mode, Size: size, Format: tar.FormatGNU}
}

func (o ociCLI) HasShelf() bool {
	on, err := o.ShelfMounted()
	return err == nil && on
}

// ShelfMounted says whether a box created now gets the shelf: false on a
// layer that mounts no outer volumes (outerVolumeEngine) or when the volume
// is absent. A volume that inspect cannot read but the volume list names, or
// a list that fails too, is an error, never false: a create fails rather
// than drop the mount, and describe does not list what it cannot vouch for.
func (o ociCLI) ShelfMounted() (bool, error) {
	o, err := o.outerVolumeEngine()
	if errors.Is(err, errNoOuterVolumes) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	_, ierr := o.run("volume", "inspect", ShelfVolume)
	if ierr == nil {
		return true, nil
	}
	out, err := o.run("volume", "ls", "-q")
	if err != nil {
		return false, fmt.Errorf("the shelf's state is unknown: %v; %v", ierr, err)
	}
	for _, v := range strings.Fields(out) {
		if v == ShelfVolume {
			return false, fmt.Errorf("the shelf's volume exists but cannot be read: %v", ierr)
		}
	}
	return false, nil
}

// errShelfMountWrong is BoxShelf's refusal of a shelf mount that is not
// ociCacheMount's.
var errShelfMountWrong = errors.New("the shelf is mounted unlike every box's")

// BoxShelf says whether box vm has the shelf, and refuses (an error) a
// mount of the volume that is not ociCacheMount's: read-only at ociCacheDir.
func (o ociCLI) BoxShelf(vm string) (bool, error) {
	out, err := o.run("container", "inspect", "--format",
		`{{range .Mounts}}{{if eq .Type "volume"}}{{.Name}} {{.Destination}} {{.RW}}{{println}}{{end}}{{end}}`, vm)
	if err != nil {
		return false, err
	}
	on := false
	for _, l := range strings.Split(out, "\n") {
		f := strings.Fields(l)
		if len(f) == 0 || f[0] != ShelfVolume {
			continue
		}
		if on || len(f) != 3 || f[1] != ociCacheDir || f[2] != "false" {
			return false, fmt.Errorf("%w: box %s mounts it as %q, not read-only at %s", errShelfMountWrong, vm, strings.TrimSpace(l), ociCacheDir)
		}
		on = true
	}
	return on, nil
}

// shelfRun runs script in a throwaway helper container from image under the
// binding's runtime: no network, a read-only root, every capability dropped
// but those listed, the shelf mounted at /shelf (read-only unless rw), the
// script's $1 that path, stdin in. Bounded by limit; a CLI killed at the
// limit leaves its container, which is removed.
func (o ociCLI) shelfRun(image string, rw bool, caps []string, script string, in io.Reader, limit time.Duration) (string, error) {
	// Shelf helpers need the same admitted lifecycle as outer-volume helpers.
	var err error
	o, err = o.outerVolumeEngine()
	if err != nil {
		return "", err
	}
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	name := "potluck-shelf-" + hex.EncodeToString(b)
	mnt := "type=volume,source=" + ShelfVolume + ",target=/shelf,volume-nocopy"
	if !rw {
		mnt += ",readonly"
	}
	a := []string{"run", "--rm", "-i", "--name", name, "--network", "none", "--runtime", o.runtime, "--read-only", "--user", "0",
		"--security-opt", "no-new-privileges", "--cap-drop", "ALL"}
	for _, c := range caps {
		a = append(a, "--cap-add", c)
	}
	a = append(a, "--pids-limit", "64", "--mount", mnt, "--entrypoint", "sh", image, "-c", script, "sh", "/shelf")
	var out strings.Builder
	if err := o.shelfExec(context.Background(), name, a, in, &out, limit); err != nil {
		return "", err
	}
	return out.String(), nil
}

// shelfExec runs the container CLI with args (a run --rm of a container
// called name), stdin in and stdout to out, bounded by ctx and limit; a CLI
// killed at the limit leaves its container, which is removed. Its error
// wraps the CLI's *exec.ExitError.
func (o ociCLI) shelfExec(ctx context.Context, name string, args []string, in io.Reader, out io.Writer, limit time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	cmd := exec.CommandContext(ctx, o.bin, args...)
	cmd.Env = dockerEngineEnv(o.engineHost)
	cmd.Stdin, cmd.Stdout = in, out
	cmd.WaitDelay = 10 * time.Second // a child still holding stdout after the kill
	var stderr capped
	stderr.max = 4 << 10
	cmd.Stderr = &stderr
	err := cmd.Run()
	if ctx.Err() != nil {
		_, _ = o.run("rm", "-f", name) // the CLI is gone; its container is not
	}
	if err != nil {
		return fmt.Errorf("the shelf helper container: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// MeasureShelf counts the shelf through a read-only helper (DAC_READ_SEARCH,
// to count what the owner's fill script made private).
func (o ociCLI) MeasureShelf(image string) (shelfStats, error) {
	out, err := o.shelfRun(image, false, []string{"DAC_READ_SEARCH"}, shelfStatsSh, nil, shelfMeasureFor)
	if err != nil {
		return shelfStats{}, err
	}
	return parseShelfStats(out)
}

func parseShelfStats(out string) (shelfStats, error) {
	for _, l := range strings.Split(out, "\n") {
		var s shelfStats
		if n, _ := fmt.Sscanf(strings.TrimSpace(l), "bytes %d files %d", &s.Bytes, &s.Files); n == 2 {
			s.At = time.Now().Unix()
			return s, nil
		}
	}
	return shelfStats{}, fmt.Errorf("the shelf's measurement said %q", strings.TrimSpace(out))
}

// PublishShelf publishes the state directory's staging, under the shelf
// lock. image must be the binding's own pinned image (config.Image), never
// one a renter named: its sh runs as root with the shelf writable. The image
// needs sh, tar, sha512sum, find (-mmin, -prune, -exec +), mktemp -d, du -x,
// touch -c and stat -c (shelf_evict.go too). Staging files it took (sent, or dropped for their bytes) are
// removed from staging once the container has finished; a refused or failed
// publish leaves staging as it was.
func (o ociCLI) PublishShelf(image string) (res shelfPublished, err error) {
	if o.stateDir == "" {
		return res, errors.New("no state directory for the shelf's staging")
	}
	if !o.HasShelf() {
		return res, fmt.Errorf("no %s volume: the owner has not made the shelf", ShelfVolume)
	}
	staging := shelfStaging(o.stateDir)
	if err := os.MkdirAll(staging, 0o700); err != nil {
		return res, err
	}
	err = flocked(shelfLock(o.stateDir), func() error {
		res, err = o.publishLocked(image, staging)
		return err
	})
	return res, err
}

func (o ociCLI) publishLocked(image, staging string) (res shelfPublished, err error) {
	spool, err := os.CreateTemp(shelfDir(o.stateDir), "spool-*.tar")
	if err != nil {
		return res, err
	}
	defer os.Remove(spool.Name())
	defer spool.Close()
	pr, pw := io.Pipe()
	go func() {
		tw := tar.NewWriter(pw)
		err := tw.AddFS(os.DirFS(staging))
		if err == nil {
			err = tw.Close()
		}
		pw.CloseWithError(err)
	}()
	sent, dropped, err := shelfSpool(pr, spool)
	pr.CloseWithError(errShelfRefused) // stops the packer if the spool refused early
	if err != nil {
		return res, err
	}
	if _, err := spool.Seek(0, io.SeekStart); err != nil {
		return res, err
	}
	out, err := o.shelfRun(image, true, []string{"DAC_OVERRIDE", "FOWNER"}, shelfPublishSh, spool, shelfPublishFor)
	if err != nil {
		return res, err
	}
	var gone int
	ok := false
	for _, l := range strings.Split(out, "\n") {
		if n, _ := fmt.Sscanf(strings.TrimSpace(l), "published %d present %d dropped %d", &res.Published, &res.Present, &gone); n == 3 {
			ok = true
		}
	}
	if !ok {
		return res, fmt.Errorf("the publisher said %q", strings.TrimSpace(out))
	}
	res.Dropped = dropped + gone
	if res.Stats, err = parseShelfStats(out); err == nil { // a failed count leaves describe to measure
		_ = saveShelfStats(o.stateDir, res.Stats)
	}
	for _, n := range sent { // published or dropped: never sent again
		p := filepath.Join(staging, filepath.FromSlash(n))
		_ = os.Remove(p)
		for d := filepath.Dir(p); d != staging && os.Remove(d) == nil; d = filepath.Dir(d) { // empty directories it leaves
		}
	}
	return res, nil
}

// shelfHasSh prints each line of stdin (a blob's path under the blob
// directory, aa/bb/rest) that is a regular file on the shelf; $1 is the
// shelf. The host sends only paths it made from hex.
const shelfHasSh = `while IFS= read -r p; do
  f="$1/` + shelfBlobDir + `/$p"
  if [ -f "$f" ] && [ ! -L "$f" ]; then echo "$p"; fi
done
exit 0
`

// ShelfHas asks a read-only helper which of the blobs are on the shelf.
func (o ociCLI) ShelfHas(image string, hexes []string) (map[string]bool, error) {
	has := map[string]bool{}
	if len(hexes) == 0 {
		return has, nil
	}
	var in strings.Builder
	byPath := map[string]string{}
	for _, h := range hexes {
		p, err := shelfBlobRel(h)
		if err != nil {
			return nil, err
		}
		byPath[p] = h
		in.WriteString(p + "\n")
	}
	out, err := o.shelfRun(image, false, []string{"DAC_READ_SEARCH"}, shelfHasSh, strings.NewReader(in.String()), shelfMeasureFor)
	if err != nil {
		return nil, err
	}
	for _, l := range strings.Split(out, "\n") {
		if h, ok := byPath[strings.TrimSpace(l)]; ok {
			has[h] = true
		}
	}
	return has, nil
}

var _ shelfer = ociCLI{}
