package boxcp

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/sengirunners/potluck/boxpush"
	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/kit/api"
)

// shStage snapshots $1 in the box into $TMPDIR/zm-cp-$2/data, at most $3
// bytes (the limit + 1, to see it passed), and answers "kind size sha256
// x" (x: 1 when a file is executable). The snapshot is what is read, so a
// file that changes meanwhile never arrives torn.
const shStage = boxpush.ShSum + `case "$1" in /*) p="$1" ;; "") p="$HOME" ;; *) p="$HOME/$1" ;; esac
[ -e "$p" ] || { echo "no such file or directory in the box" >&2; exit 67; }
d="${TMPDIR:-/tmp}/zm-cp-$2"
mkdir -p "$d" || exit 70
o="$d/data"
if [ -d "$p" ]; then
  command -v tar >/dev/null 2>&1 || { rm -rf "$d"; echo "the box has no tar: copy files one at a time, or use an image that has it" >&2; exit 69; }
  s=$(cd -- "$d" && pwd -P); r=$(cd -- "$p" && pwd -P) || { rm -rf "$d"; exit 70; }
  [ "$r" = / ] && r=
  case "$s/" in "$r"/*) rm -rf "$d"; echo "that directory holds the box's temporary directory, where the copy is staged: copy a directory beneath it" >&2; exit 64 ;; esac
  kind=dir
  { (cd -- "$p" && COPYFILE_DISABLE=1 tar -cf - .) || : >"$d/failed"; } | head -c "$3" >"$o"
elif [ -f "$p" ]; then
  kind=file
  head -c "$3" <"$p" >"$o" || : >"$d/failed"
else
  rm -rf "$d"; echo "not a regular file or a directory" >&2; exit 64
fi
n=$(wc -c <"$o" | tr -d ' ')
if [ "$n" -ge "$3" ]; then rm -rf "$d"; echo "over the limit" >&2; exit 68; fi
if [ -e "$d/failed" ]; then rm -rf "$d"; echo "the box could not read all of it" >&2; exit 70; fi
x=0
[ "$kind" = file ] && [ -x "$p" ] && x=1
printf '%s %s %s %s\n' "$kind" "$n" "$(zmsum "$o")" "$x"
`

const shRead = `dd if="${TMPDIR:-/tmp}/zm-cp-$1/data" bs="$2" skip="$3" count=1 2>/dev/null`

const shUnstage = `rm -rf "${TMPDIR:-/tmp}/zm-cp-$1"`

var stageRe = regexp.MustCompile(`^(file|dir) ([0-9]{1,12}) ([0-9a-f]{64}) ([01])$`)

// Download copies src in the box exec reaches (a file, or a directory with
// all beneath it) to local, as cp has it: an existing directory takes it by
// its name, a path ending in / is made a directory first, else local is the
// copy's own name. A file replaces one there; a directory is never merged
// into one there (refused), and appears whole or not at all.
func Download(ctx context.Context, exec boxpush.ExecFunc, src, local string, o Options) (Result, error) {
	if err := o.defaults(); err != nil {
		return Result{}, err
	}
	if err := checkBoxPath(src); err != nil {
		return Result{}, err
	}
	res := Result{From: src}
	final, err := localDest(src, local)
	if err != nil {
		return res, err
	}
	var k [12]byte
	_, _ = rand.Read(k[:])
	key := hex.EncodeToString(k[:])

	x, err := boxpush.Retry(ctx, o.Retries, func() (computeapi.RunTargetExec, error) {
		return exec(ctx, computeapi.ExecRunTargetRequest{Argv: boxpush.Script(shStage, src, key, strconv.FormatInt(o.Max+1, 10)), TimeoutS: boxpush.FinishTimeoutS})
	})
	if err != nil {
		return res, fmt.Errorf("reading in the box: %w", err)
	}
	defer func() { // the snapshot goes, whatever happened; else it ends with the box
		_, _ = boxpush.Retry(context.WithoutCancel(ctx), o.Retries, func() (computeapi.RunTargetExec, error) {
			return exec(context.WithoutCancel(ctx), computeapi.ExecRunTargetRequest{Argv: boxpush.Script(shUnstage, key), TimeoutS: boxpush.ChunkTimeoutS})
		})
	}()
	switch x.Exit {
	case 0:
	case exitNotFound:
		return res, api.Errorf(api.CodeNotFound, "%s: no such file or directory in the box", src)
	case exitTooLarge:
		return res, api.Errorf(api.CodeTooLarge, "%s is over the %s limit: nothing was copied (raise it up to %s, or copy less)", src, mib(o.Max), mib(HardMax))
	default:
		return res, boxpush.BoxErr("the box did not read it", x)
	}
	m := stageRe.FindStringSubmatch(strings.TrimSpace(x.Stdout))
	if m == nil {
		return res, fmt.Errorf("the box's answer is not a staged copy: refused")
	}
	res.Kind, res.Bytes = m[1], mustInt(m[2])
	sum, exe := m[3], m[4] == "1"
	if res.Bytes > o.Max {
		return res, fmt.Errorf("the box staged %s, over the %s limit: refused", mib(res.Bytes), mib(o.Max))
	}
	if fi, err := os.Lstat(final); err == nil && (res.Kind == KindDir || fi.IsDir()) {
		return res, api.Errorf(api.CodeConflict, "%s already exists: remove it or name another destination", final)
	}
	if err := os.MkdirAll(filepath.Dir(final), 0o755); err != nil {
		return res, err
	}

	// The bytes, in pages, into a file beside the destination.
	tmp, err := os.CreateTemp(filepath.Dir(final), ".zm-cp-*.part")
	if err != nil {
		return res, err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	if err := fetch(ctx, exec, key, res.Bytes, tmp, &res, o); err != nil {
		return res, err
	}
	h := sha256.New()
	if _, err := io.Copy(h, io.NewSectionReader(tmp, 0, res.Bytes)); err != nil {
		return res, err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != sum {
		return res, fmt.Errorf("the copy's sha256 is %s, not the box's %s: refused, nothing written", got, sum)
	}
	if res.Kind == KindDir {
		if res.Files, res.Left, err = unpack(io.NewSectionReader(tmp, 0, res.Bytes), final, o.Max); err != nil {
			return res, err
		}
		res.To = final
		return res, nil
	}
	mode := os.FileMode(0o644)
	if exe {
		mode = 0o755
	}
	if err := tmp.Chmod(mode); err != nil {
		return res, err
	}
	if err := tmp.Close(); err != nil {
		return res, err
	}
	if err := os.Rename(tmp.Name(), final); err != nil {
		return res, err
	}
	res.To, res.Files = final, 1
	return res, nil
}

// localDest is where a download of src lands: inside an existing local
// directory (or one named with a trailing /) by src's name, else local.
func localDest(src, local string) (string, error) {
	if local == "" {
		return "", api.Errorf(api.CodeBadRequest, "name a local destination")
	}
	name := path.Base(path.Clean("/" + src))
	if src == "" || name == "/" {
		name = ""
	}
	into := strings.HasSuffix(local, string(filepath.Separator))
	if fi, err := os.Stat(local); err == nil && fi.IsDir() {
		into = true
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if !into {
		return filepath.Clean(local), nil
	}
	if name == "" {
		return "", api.Errorf(api.CodeBadRequest, "%q has no name to take inside %s: name the destination itself", src, local)
	}
	return filepath.Join(local, name), nil
}

// fetch reads the staged snapshot's size bytes in pages into w.
func fetch(ctx context.Context, exec boxpush.ExecFunc, key string, size int64, w io.WriterAt, res *Result, o Options) error {
	n := int((size + int64(o.ChunkSize) - 1) / int64(o.ChunkSize))
	res.Chunks = n
	var (
		mu    sync.Mutex
		first error
		done  int
		wg    sync.WaitGroup
		next  = make(chan int)
	)
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	failed := func(err error) {
		mu.Lock()
		if first == nil {
			first = err
		}
		mu.Unlock()
		cancel()
	}
	for range min(o.Parallel, max(n, 1)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range next {
				want := min(int64(o.ChunkSize), size-int64(i)*int64(o.ChunkSize))
				req := computeapi.ExecRunTargetRequest{Argv: boxpush.Script(shRead, key, strconv.Itoa(o.ChunkSize), strconv.Itoa(i)), Encoding: computeapi.EncodingBase64, TimeoutS: boxpush.ChunkTimeoutS}
				x, err := boxpush.Retry(cctx, o.Retries, func() (computeapi.RunTargetExec, error) { return exec(cctx, req) })
				if err != nil {
					failed(fmt.Errorf("page %d of %d: %w", i+1, n, err))
					continue
				}
				b, err := page(x)
				if err == nil && int64(len(b)) != want {
					err = fmt.Errorf("%d bytes, not %d", len(b), want)
				}
				if err != nil {
					failed(fmt.Errorf("page %d of %d: %w", i+1, n, err))
					continue
				}
				if _, err := w.WriteAt(b, int64(i)*int64(o.ChunkSize)); err != nil {
					failed(err)
					continue
				}
				mu.Lock()
				done++
				d := done
				mu.Unlock()
				if o.Progress != nil {
					o.Progress(d, n)
				}
			}
		}()
	}
feed:
	for i := range n {
		select {
		case next <- i:
		case <-cctx.Done():
			break feed
		}
	}
	close(next)
	wg.Wait()
	if first != nil {
		return first
	}
	return ctx.Err()
}

// page is one exec's stdout as bytes (exact: base64 on the wire).
func page(x computeapi.RunTargetExec) ([]byte, error) {
	switch {
	case x.Exit != 0:
		return nil, boxpush.BoxErr("the box did not read it", x)
	case x.Encoding != computeapi.EncodingBase64:
		return nil, fmt.Errorf("the box's driver did not answer the bytes as base64: it cannot carry files")
	case x.Truncated:
		return nil, fmt.Errorf("a page passed exec's output cap")
	}
	return base64.StdEncoding.DecodeString(x.Stdout)
}

func mustInt(s string) int64 {
	n, _ := strconv.ParseInt(s, 10, 64)
	return n
}
