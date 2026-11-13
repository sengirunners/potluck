// Package boxcp copies files and directories between the renter's machine
// and a box they hold (SPEC 20 "Boxes", Files in and out; zm-eakh.12.5.22),
// over exec alone, as boxpush sends a git bundle: the one verb every box
// driver has, relayed across machines and the tunnel within the same caps,
// and the holder's alone (exec's checks). No new verb, no new route.
//
// Up: a file goes as its bytes, a directory as a tar the client makes (no
// symlink is followed out of it; devices, sockets and FIFOs are left out and
// listed). A first exec resolves the destination to one absolute path;
// boxpush.Send puts the bytes into the box in 512 KiB chunks of base64
// stdin, each checked against its SHA-256 there; one more exec joins them,
// checks the whole SHA-256 and writes the file (or untars with the box's
// tar) at exactly that path, so sent again it does the same. A copy cut
// off resumes when run again.
//
// Down: one exec stages a snapshot in the box's TMPDIR (the file's bytes,
// or a tar of the directory by the box's tar), capped at the limit, and
// answers its kind, size and SHA-256; 512 KiB pages are read back with dd
// through exec's base64 output; the client checks the SHA-256 and writes
// the file or unpacks the tar itself, with Go's archive/tar under os.Root:
// the box's bytes are data, never run or interpreted, and nothing is
// written outside the destination. An entry naming an absolute path or a
// ".." is refused with nothing written (the destination appears whole or
// not at all), as is a sparse entry or more bytes than the limit in all;
// symlinks, hard links and devices are never written, only listed.
// Owned by sandboxes (zm-eakh.12.5).
package boxcp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/sengirunners/potluck/boxpush"
	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/kit/api"
)

// Limits.
const (
	// DefaultMax bounds one copy (a file, or a directory's tar) unless the
	// caller says otherwise; HardMax is the most it may say.
	DefaultMax = 128 << 20
	HardMax    = 1 << 30
	// ChunkSize is one exec's stdin up or stdout down: its base64 and JSON
	// stay under the 1 MiB request body and exec's 1 MiB output cap.
	ChunkSize = boxpush.ChunkSize
)

// Kinds of what is copied.
const (
	KindFile = "file"
	KindDir  = "dir"
)

// Exit codes of the box's scripts beside boxpush's (65 checksum, 66 a
// chunk missing, 69 a missing tool).
const (
	exitRefused  = 64 // not a file or directory, or the wrong kind at the destination
	exitMissing  = 66 // a chunk never arrived (boxpush's)
	exitNotFound = 67 // no such path in the box
	exitTooLarge = 68 // over the limit
	exitUnread   = 70 // the box could not read all of it
)

// Options say how.
type Options struct {
	Max       int64 // bytes (default DefaultMax, at most HardMax)
	ChunkSize int   // default ChunkSize (tests use less)
	Parallel  int   // chunks in flight (default 4)
	Retries   int   // tries per call (default 4)
	// Progress, when set, hears each chunk as it lands (done of total).
	Progress func(done, total int)
}

func (o *Options) defaults() error {
	if o.Max == 0 {
		o.Max = DefaultMax
	}
	if o.ChunkSize <= 0 {
		o.ChunkSize = ChunkSize
	}
	if o.Retries <= 0 {
		o.Retries = 4
	}
	if o.Parallel <= 0 {
		o.Parallel = 4
	}
	switch {
	case o.Max < 0 || o.Max > HardMax:
		return api.Errorf(api.CodeBadRequest, "the limit is at most %s", mib(HardMax))
	case o.ChunkSize > ChunkSize:
		return fmt.Errorf("a chunk is at most %d bytes", ChunkSize)
	}
	return nil
}

// Result is what a copy did.
type Result struct {
	From  string `json:"from"`
	To    string `json:"to"`    // where it landed: the box's path as its shell has it, or the local path
	Kind  string `json:"kind"`  // file or dir
	Bytes int64  `json:"bytes"` // bytes carried (a directory's tar)
	Files int    `json:"files"` // regular files written
	// Left are entries not copied (a symlink, a device...), each with why;
	// on a download their names are the box's (data).
	Left    []string `json:"left_out,omitempty"`
	Chunks  int      `json:"chunks"`
	Sent    int      `json:"sent,omitempty"`    // chunks this copy sent up
	Skipped int      `json:"skipped,omitempty"` // chunks an earlier copy left in the box
	// CredentialLike are paths copied up (relative to a directory copied)
	// whose names look like credentials files (credentials.go).
	CredentialLike []string `json:"credential_like,omitempty"`
}

// Split parses a cp argument: "box:<target>/<id>:<path>" is a box's path
// (an empty path is its HOME, a relative one is under it); anything else is
// local ("./box:x" names a local file of that name).
func Split(arg string) (handle, path string, box bool, err error) {
	rest, ok := strings.CutPrefix(arg, "box:")
	if !ok {
		return "", "", false, nil
	}
	h, p, ok := strings.Cut(rest, ":")
	if !ok {
		return "", "", true, api.Errorf(api.CodeBadRequest, "%q names a box but no path in it: box:<target>/<id>:<path> (an empty path is its HOME)", arg)
	}
	return "box:" + h, p, true, checkBoxPath(p)
}

func checkBoxPath(p string) error {
	if strings.ContainsRune(p, 0) || len(p) > 4096 {
		return api.Errorf(api.CodeBadRequest, "the box path is over 4096 bytes or holds a NUL")
	}
	return nil
}

// shResolve is the first exec of an upload, before any byte is sent: dest
// ($1) as cp has it for a copy of kind $2 named $3 ("": dest is the file
// itself), answered as the one absolute path every later exec writes, so
// a finish sent again after a lost answer writes the same place. A dest
// ending in / is made a directory here.
const shResolve = `case "$1" in /*) dest="$1" ;; "") dest="$HOME" ;; *) dest="$HOME/$1" ;; esac
case "$dest" in /*) ;; *) echo "the box's HOME is not an absolute path" >&2; exit 64 ;; esac
case "$1" in */) mkdir -p -- "$dest" || exit 70 ;; esac
if [ -n "$3" ] && [ -d "$dest" ]; then dest="${dest%/}/$3"; fi
if [ "$2" = dir ]; then
  if [ -e "$dest" ] && [ ! -d "$dest" ]; then echo "$dest is a file, not a directory" >&2; exit 64; fi
elif [ -d "$dest" ]; then echo "$dest is a directory" >&2; exit 64; fi
printf '%s\n' "$dest"
`

// shPut is the box's side of an upload once boxpush.Send left the chunks:
// join, check, write at $4, the path shResolve answered (a directory
// merges into one there). $1 key, $2 chunks, $3 sha256, $5 kind, $6 a
// file's mode. The chunks stay until the cleanup exec, so a finish sent
// again after its answer was lost does exactly the same again.
const shPut = boxpush.ShSum + `d="` + boxpush.ChunkDir + `/$1"
dest=$4
b="$d/joined.$$"
: >"$b" || exit 70
i=0
while [ "$i" -lt "$2" ]; do
  [ -f "$d/c.$i" ] || { rm -f "$b"; echo "chunk $i never arrived: copy again to resume" >&2; exit 66; }
  cat "$d/c.$i" >>"$b" || { rm -f "$b"; exit 70; }
  i=$((i + 1))
done
got=$(zmsum "$b")
if [ "$got" != "$3" ]; then rm -rf "$d"; echo "the upload's sha256 is $got, not $3: refused, nothing written" >&2; exit 65; fi
if [ "$5" = dir ]; then
  if [ -e "$dest" ] && [ ! -d "$dest" ]; then rm -f "$b"; echo "$dest is a file, not a directory" >&2; exit 64; fi
  mkdir -p -- "$dest" && tar -x -o -f "$b" -C "$dest" || { s=$?; rm -f "$b"; exit "$s"; }
  rm -f "$b"
else
  if [ -d "$dest" ]; then rm -f "$b"; echo "$dest is a directory" >&2; exit 64; fi
  mkdir -p -- "$(dirname -- "$dest")" && chmod "$6" "$b" && mv -f -- "$b" "$dest" || { s=$?; rm -f "$b"; exit "$s"; }
fi
cd -- "$(dirname -- "$dest")" && printf '%s/%s\n' "$(pwd -P)" "$(basename -- "$dest")"
`

const shNeedTar = `command -v tar >/dev/null 2>&1 || { echo "the box has no tar: copy files one at a time, or use an image that has it" >&2; exit 69; }
`

// Upload copies local (a file, or a directory with all beneath it) to dest
// in the box exec reaches. dest as cp has it: an existing directory there
// takes it by its name; a path ending in / is made a directory first; else
// dest is the copy's own name (a directory merges into one already there).
func Upload(ctx context.Context, exec boxpush.ExecFunc, local, dest string, o Options) (Result, error) {
	if err := o.defaults(); err != nil {
		return Result{}, err
	}
	if err := checkBoxPath(dest); err != nil {
		return Result{}, err
	}
	abs, err := filepath.Abs(local)
	if err != nil {
		return Result{}, err
	}
	fi, err := os.Stat(abs) // a symlink named on the command line is followed, as cp -H
	if err != nil {
		return Result{}, err
	}
	res := Result{From: local, Kind: KindFile}
	var f *os.File
	var size int64
	var sum string
	switch {
	case fi.IsDir():
		res.Kind = KindDir
		p, err := packDir(abs, o.Max)
		if err != nil {
			return res, err
		}
		defer os.RemoveAll(filepath.Dir(p.path))
		res.Files, res.Left, res.CredentialLike, size, sum = p.files, p.left, p.creds, p.size, p.sum
		if f, err = os.Open(p.path); err != nil {
			return res, err
		}
	case fi.Mode().IsRegular():
		if fi.Size() > o.Max {
			return res, tooLarge(local, fi.Size(), o.Max)
		}
		if f, err = os.Open(abs); err != nil {
			return res, err
		}
		h := sha256.New()
		if size, err = io.Copy(h, io.LimitReader(f, o.Max+1)); err != nil {
			f.Close()
			return res, err
		}
		if size > o.Max {
			f.Close()
			return res, tooLarge(local, size, o.Max)
		}
		sum, res.Files = hex.EncodeToString(h.Sum(nil)), 1
		if CredentialLike(filepath.ToSlash(abs)) {
			res.CredentialLike = []string{local}
		}
	default:
		return res, api.Errorf(api.CodeBadRequest, "%s is not a regular file or a directory", local)
	}
	defer f.Close()
	return put(ctx, exec, io.NewSectionReader(f, 0, size), size, sum, dest, filepath.Base(abs), fi.Mode().Perm(), res, o)
}

// WriteFile writes data to the file p in the box (exactly p: a directory
// there is refused), mode 0644.
func WriteFile(ctx context.Context, exec boxpush.ExecFunc, p string, data []byte, o Options) (Result, error) {
	if err := o.defaults(); err != nil {
		return Result{}, err
	}
	if err := checkBoxPath(p); err != nil {
		return Result{}, err
	}
	if strings.HasSuffix(p, "/") || p == "" {
		return Result{}, api.Errorf(api.CodeBadRequest, "name the file, not a directory: %q", p)
	}
	if int64(len(data)) > o.Max {
		return Result{}, tooLarge("the content", int64(len(data)), o.Max)
	}
	s := sha256.Sum256(data)
	res := Result{From: "content", Kind: KindFile, Files: 1}
	if CredentialLike(p) {
		res.CredentialLike = []string{p}
	}
	return put(ctx, exec, bytes.NewReader(data), int64(len(data)), hex.EncodeToString(s[:]), p, "", 0o644, res, o)
}

func put(ctx context.Context, exec boxpush.ExecFunc, r io.ReaderAt, size int64, sum, dest, name string, mode os.FileMode, res Result, o Options) (Result, error) {
	res.Bytes = size
	x, err := boxpush.Retry(ctx, o.Retries, func() (computeapi.RunTargetExec, error) {
		return exec(ctx, computeapi.ExecRunTargetRequest{Argv: boxpush.Script(shResolve, dest, res.Kind, name), TimeoutS: boxpush.ChunkTimeoutS})
	})
	if err != nil {
		return res, fmt.Errorf("reaching the box: %w", err)
	}
	if x.Exit != 0 {
		return res, boxpush.BoxErr("the box refused the destination", x)
	}
	final := strings.TrimSuffix(x.Stdout, "\n")
	if !strings.HasPrefix(final, "/") || checkBoxPath(final) != nil {
		return res, fmt.Errorf("the box's answer is not a destination path: refused")
	}
	need := ""
	if res.Kind == KindDir {
		need = shNeedTar
	}
	// The chunks' key carries the destination: two copies of the same bytes
	// to two places never share (or clean up) each other's chunks, and the
	// same copy again resumes from its own.
	tag := sha256.Sum256([]byte(final + "\x00" + res.Kind))
	s, err := boxpush.Send(ctx, exec, r, size, sum, boxpush.SendOptions{ChunkSize: o.ChunkSize, Parallel: o.Parallel, Retries: o.Retries, Progress: o.Progress, Require: need, Tag: hex.EncodeToString(tag[:6])})
	res.Chunks, res.Sent, res.Skipped = s.Chunks, s.Sent, s.Skipped
	if err != nil {
		return res, err
	}
	x, err = boxpush.Retry(ctx, o.Retries, func() (computeapi.RunTargetExec, error) {
		return exec(ctx, computeapi.ExecRunTargetRequest{Argv: boxpush.Script(shPut, s.Key, strconv.Itoa(s.Chunks), sum, final, res.Kind, strconv.FormatUint(uint64(mode&0o777), 8)), TimeoutS: boxpush.FinishTimeoutS})
	})
	if err != nil { // no answer: the chunks stay, so the same copy again resumes
		return res, fmt.Errorf("writing in the box: %w (the same copy again resumes)", err)
	}
	if x.Exit != exitMissing { // written, or refused: the chunks go (a failure leaves them until the box ends)
		_, _ = boxpush.Retry(ctx, o.Retries, func() (computeapi.RunTargetExec, error) {
			return exec(context.WithoutCancel(ctx), computeapi.ExecRunTargetRequest{Argv: boxpush.Script(`rm -rf "`+boxpush.ChunkDir+`/$1"`, s.Key), TimeoutS: boxpush.ChunkTimeoutS})
		})
	}
	if x.Exit != 0 {
		return res, boxpush.BoxErr("the box did not write it", x)
	}
	res.To = strings.TrimSpace(x.Stdout)
	return res, nil
}

func tooLarge(what string, n, max int64) error {
	return api.Errorf(api.CodeTooLarge, "%s is %s, over the %s limit: nothing was copied (raise it up to %s, or copy less)", what, mib(n), mib(max), mib(HardMax))
}

func mib(n int64) string { return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20)) }
