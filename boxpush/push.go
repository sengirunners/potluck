package boxpush

// The box's side of a push: four kinds of exec, each a short sh script
// with literal arguments (no value is ever spliced into the script):
//
//	have:   the commits the box holds for dest (its last push's, and the
//	        base git directory's refs, read as files: the base's config is
//	        never loaded), for Make to cut a bundle against
//	probe:  git and a SHA-256 tool are there; which chunks already arrived
//	chunk:  stdin (base64 on the wire) to $HOME/.potluck/push/<key>/c.<i>, kept
//	        only when its SHA-256 matches (written to a temporary name, then
//	        renamed, so a resend or a crash never leaves half a chunk)
//	finish: join the chunks, check the whole bundle's SHA-256 (else refuse
//	        and drop them), fetch it into the push's own git directory, check
//	        the commit out in dest, verify HEAD, remove the chunks; answers
//	        dest's absolute path. Sent again after its answer was lost, it
//	        finds no chunks and HEAD at the commit, and answers the same
//
// The git directory is the push's own, $HOME/.potluck/push-git/<sha256 of
// dest's physical path>, made by the first push to dest in the box (a box
// that already has one under the old name, $HOME/.zm/push-git, keeps using
// it: the push is incremental on it and its first clean is done);
// with a base (PushOptions.Base) the git directory borrows the base's
// objects (git alternates), and a bundle built on commits the box does not
// hold is refused before anything is checked out (exit 67);
// dest/.git is replaced by a file pointing at it on every finish. A .git
// that was in dest before (a dependency layer's, the image's, a driver's
// checkout) is never read: its config (hooks, fsmonitor), alternates or
// replace refs cannot change what is checked out; a .git directory is
// moved aside next to the git directory, unread. With no index of its own
// to say which files an earlier checkout left, the first push to dest
// removes every untracked file there but ignored ones and node_modules (a
// dependency layer's), so dest holds the commit's tree.

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/vmhost/shelfhelper"
)

// ExecFunc runs one exec in the box (client.ExecRunTarget for one handle).
type ExecFunc func(ctx context.Context, req computeapi.ExecRunTargetRequest) (computeapi.RunTargetExec, error)

// PushOptions say where and how.
type PushOptions struct {
	Dest string // the checkout in the box, relative to its HOME (default DefaultDest) or absolute
	// Base is a git directory in the box (relative to its HOME, or
	// absolute) whose commits a bundle may build on: a volume of the
	// caller's that holds the repository, say. The checkout's git directory
	// borrows its objects (git alternates) from then on, so the base must
	// stay in the box as long as the checkout is used, and be trusted as
	// much as the checkout: git takes its objects as they are.
	Base string
	// NoBase disables automatic discovery of the host-mounted push base.
	NoBase    bool
	autoBase  bool
	ChunkSize int // default ChunkSize (tests use less)
	Parallel  int // chunks in flight (default 4)
	Retries   int // tries per call (default 4)
	// Progress, when set, hears each chunk as it lands (done of total).
	Progress func(done, total int)
}

// Result is what a push did.
type Result struct {
	Repo    string `json:"repo"`   // the renter's checkout it came from
	Dest    string `json:"dest"`   // the checkout's path in the box, as its shell has it
	Dir     string `json:"dir"`    // the checkout as asked: under the box's HOME unless absolute
	Commit  string `json:"commit"` // the commit checked out (HEAD there)
	Chunks  int    `json:"chunks"`
	Sent    int    `json:"sent"`    // chunks sent by this push
	Skipped int    `json:"skipped"` // chunks an earlier push left in the box
	Bytes   int64  `json:"bytes"`   // the bundle's size
	// Base are the commits the bundle built on: only what the commit adds
	// to them was sent.
	Base []string `json:"base,omitempty"`
}

// Exit codes of the box's scripts.
const (
	exitChecksum = 65 // a chunk or the bundle did not match its SHA-256
	exitMissing  = 66 // a chunk never arrived
	exitNoTool   = 69 // no git or no SHA-256 tool in the box
	exitBase     = 67 // the box lacks the base or a commit the bundle builds on
)

// errBaseGone is a refusal of a bundle built on commits the box does not
// hold: the same commit pushed whole goes in.
var errBaseGone = errors.New("the box does not hold the commits the bundle builds on")

// ChunkDir is where a box keeps the chunks of a push, as its shell has it:
// <ChunkDir>/<key>/c.<i>. Potluck before its rename kept them, and the push's
// git directories beside them, under $HOME/.zm.
const ChunkDir = `$HOME/.potluck/push`

// ShSum defines zmsum FILE: the SHA-256 of FILE, hex, with whichever tool
// the box has.
const ShSum = `zmsum() { if command -v sha256sum >/dev/null 2>&1; then sha256sum <"$1"; elif command -v shasum >/dev/null 2>&1; then shasum -a 256 <"$1"; else openssl dgst -sha256 <"$1"; fi 2>/dev/null | sed 's/^.*= *//; s/ .*//'; }
`

const shNeedGit = `command -v git >/dev/null 2>&1 || { echo "the box has no git: use an image that has it" >&2; exit 69; }
`

const shProbe = `[ -n "$(zmsum /dev/null)" ] || { echo "the box has no sha256sum, shasum or openssl" >&2; exit 69; }
d="` + ChunkDir + `/$1"
# A new send of these bytes: a refusal of an earlier one is no answer to it.
rm -f "$d.refused"
[ -d "$d" ] || exit 0
cd "$d"
for f in c.*; do case "$f" in c.|c.*[!0-9]*) ;; *) echo "${f#c.}" ;; esac; done
`

const shChunk = `set -e
d="` + ChunkDir + `/$1"
mkdir -p "$d"
t="$d/c.$2.$$"
cat >"$t"
got=$(zmsum "$t")
if [ "$got" != "$3" ]; then rm -f "$t"; echo "chunk $2 arrived with sha256 $got, not $3" >&2; exit 65; fi
mv -f "$t" "$d/c.$2"
`

// shGdir defines gdir, which sets g to the push's own git directory for
// dest ($asked: dest as the caller named it, for messages), named by dest's
// physical path: under HOME/.potluck, or where this box already has one made
// under the old name, HOME/.zm. A dest that is /, HOME or holds it, or is in
// either of them is refused: the first push cleans dest, and the push keeps
// its own files there. It also defines basedir, which sets base to the git
// directory its argument names, under HOME unless absolute ("" for "").
const shGdir = `gdir() {
  pd=$(cd "$dest" && pwd -P); h=$(cd "$HOME" && pwd -P)
  case "$pd" in /|"$h") echo "the checkout cannot be $asked: name a directory under HOME, outside .potluck and .zm" >&2; exit 64 ;; esac
  case "$h/" in "$pd"/*) echo "the checkout cannot be $asked, which holds HOME: name a directory under HOME" >&2; exit 64 ;; esac
  for z in "$HOME/.potluck" "$HOME/.zm"; do
    [ -d "$z" ] || continue
    z=$(cd "$z" && pwd -P)
    case "$pd" in "$z"|"$z"/*) echo "the checkout cannot be $asked: name a directory under HOME, outside .potluck and .zm" >&2; exit 64 ;; esac
    case "$z/" in "$pd"/*) echo "the checkout cannot be $asked, which holds $z: name a directory under HOME" >&2; exit 64 ;; esac
  done
  mkdir -p "$HOME/.potluck/push"
  p="$HOME/.potluck/push/.path.$$"; printf '%s' "$pd" >"$p"; k=$(zmsum "$p"); rm -f "$p"
  g="$HOME/.potluck/push-git/$k"
  [ -d "$g" ] || [ ! -d "$HOME/.zm/push-git/$k" ] || g="$HOME/.zm/push-git/$k"
}
basedir() { case "$1" in "") base= ;; /*) base="$1" ;; *) base="$HOME/$1" ;; esac; }
`

// shHave answers the commits the box holds for dest $1, one per line: its
// last push's, and with a base $2 the ids the base's HEAD and refs name,
// read as files (no git runs in the base, so its config is never read).
// What it cannot read is left out: the push then sends more.
const shHave = ShSum + shGdir + `asked="$1"
case "$1" in /*) dest="$1" ;; *) dest="$HOME/$1" ;; esac
if [ -d "$dest" ]; then ( gdir && [ -f "$g/HEAD" ] && git --git-dir="$g" rev-parse -q --verify refs/zm/push ) 2>/dev/null; fi
basedir "$2"
if [ -z "$base" ] && [ "$3" = auto ] && [ -d "$HOME/.potluck-base/objects" ]; then
  base=$HOME/.potluck-base
  echo potluck-host-base
fi
if [ -n "$base" ] && [ -d "$base" ]; then
  { cat "$base/HEAD" "$base/packed-refs"; find "$base/refs" -type f -exec cat {} +; } 2>/dev/null | while read -r a _; do
    case "$a" in ""|*[!0-9a-f]*) ;; *) echo "$a" ;; esac
  done | head -n 256
fi
exit 0
`

const shFinish = ShSum + shGdir + `set -e
d="` + ChunkDir + `/$1"
asked="$4"
case "$4" in /*) dest="$4" ;; *) dest="$HOME/$4" ;; esac
# dest/.git becomes a file naming g, whatever was there. A .git directory
# there (an earlier push's from before push-git, a layer's) is moved aside
# unread, never deleted: commits made in it are not lost.
own() {
  if [ -d "$dest/.git" ] && [ ! -L "$dest/.git" ]; then mv "$dest/.git" "$g.replaced.$$" || rm -rf "$dest/.git"; else rm -rf "$dest/.git"; fi
  printf 'gitdir: %s\n' "$g" >"$dest/.git"
}
# The package shelf's link step (docs/shelf.md), as the driver's own checkout
# runs it: in a box with the shelf mounted, the shelf's helper links the
# tarballs dest's lockfile names into the box's own npm cache and notes them
# in the box's wants. A box without the shelf has no helper and skips it. Its
# output is dropped and its failure never fails the push.
` + shelfhelper.LinkSh + `shelflink() { potluck_shelf_link "${POTLUCK_SHELF:-` + shelfhelper.BoxDir + `}" "$dest" >/dev/null 2>&1 || :; }
# refuse: the bundle builds on what the box does not hold (exit 67, the
# client then sends the commit whole). The chunks go, and a mark stays so the
# same call sent again (its answer lost) refuses again, once.
refuse() { rm -rf "$d"; : >"$d.refused"; echo "$1" >&2; exit 67; }
if [ -e "$d.refused" ]; then rm -f "$d.refused"; echo "the box does not hold what the bundle builds on" >&2; exit 67; fi
# Done already (this call's answer was lost and it is sent again): the
# chunks are gone and the push's own git directory is at the commit.
if [ ! -e "$d/c.0" ] && [ -d "$dest" ]; then
  mkdir -p "$HOME/.potluck/push"; gdir
  if [ "$(git --git-dir="$g" rev-parse -q --verify HEAD 2>/dev/null)" = "$6" ]; then own; shelflink; printf '%s\n' "$dest"; exit 0; fi
fi
mkdir -p "$d"
b="$d/bundle"
: >"$b"
i=0
while [ "$i" -lt "$2" ]; do
  [ -f "$d/c.$i" ] || { echo "chunk $i never arrived: push again to resume" >&2; exit 66; }
  cat "$d/c.$i" >>"$b"
  i=$((i + 1))
done
got=$(zmsum "$b")
if [ "$got" != "$3" ]; then rm -rf "$d"; echo "the bundle's sha256 is $got, not $3: refused, nothing checked out" >&2; exit 65; fi
mkdir -p "$dest" "$HOME/.potluck/push-git"
gdir
[ -f "$g/HEAD" ] || { rm -rf "$g"; git init -q --bare "$g"; }
git --git-dir="$g" config core.bare false
own
cd "$dest"
# The base's objects are borrowed (alternates), once per git directory. git
# never runs in the base: fetch would list its refs with git for-each-ref
# there (reading its config), and alternateRefsCommand true lists none.
basedir "$7"
# An alternate whose directory is gone (a base no longer mounted) is dropped:
# git refuses a repository naming one.
a="$g/objects/info/alternates"
if [ -f "$a" ]; then
  while read -r l; do [ -d "$l" ] && printf '%s\n' "$l"; done <"$a" >"$a.$$" || :
  mv -f "$a.$$" "$a"
fi
if [ -n "$base" ]; then
  git --git-dir="$g" config core.alternateRefsCommand true
  if [ ! -d "$base/objects" ]; then
    [ -z "$8" ] || refuse "the base $7 is no git directory in the box (it has no objects/)"
    echo "the base $7 is no git directory in the box (it has no objects/)" >&2; exit 64
  fi
  bo=$(cd "$base/objects" && pwd -P) || {
    [ -z "$8" ] || refuse "the base $7 cannot be read"
    echo "the base $7 cannot be read" >&2; exit 64
  }
  grep -qxF "$bo" "$a" 2>/dev/null || { mkdir -p "$g/objects/info"; printf '%s\n' "$bo" >>"$a"; }
fi
# The commits the bundle builds on ($8) must be here, or nothing is checked
# out and the chunks go: the same commit pushed whole is another bundle. git
# takes them only from its own refs, so each is named under refs/zm/base for
# the fetch (and marked shallow: the base's history may stop there).
for p in $8; do
  git cat-file -e "$p^{commit}" 2>/dev/null || refuse "the box does not hold $p, which the bundle builds on"
  grep -qx "$p" "$g/shallow" 2>/dev/null || echo "$p" >>"$g/shallow"
  git update-ref "refs/zm/base/$p" "$p"
done
for s in $5; do grep -qx "$s" "$g/shallow" 2>/dev/null || echo "$s" >>"$g/shallow"; done
# A bundle built on commits the box holds can still lack what they lead to
# (a base without its blobs): any failure of its fetch or checkout is a
# refusal, so the whole commit goes in instead.
if git -c maintenance.auto=false fetch -q "$b" "+refs/zm/push:refs/zm/push" && git -c advice.detachedHead=false checkout -q -f --detach "$6"; then :; else
  git for-each-ref --format='delete %(refname)' refs/zm/base | git update-ref --stdin
  [ -z "$8" ] || refuse "the bundle built on what the box holds did not check out"
  echo "the bundle did not check out" >&2; exit 1
fi
git for-each-ref --format='delete %(refname)' refs/zm/base | git update-ref --stdin
# Until one clean has finished (a first finish may die before it), remove
# what an earlier tree left: untracked files and nested repositories, but
# ignored ones and node_modules.
[ -f "$g/potluck-cleaned" ] || [ -f "$g/zm-cleaned" ] || { git clean -ffdq -e node_modules; : >"$g/potluck-cleaned"; }
[ "$(git rev-parse HEAD)" = "$6" ] || { echo "HEAD is not $6 after the checkout" >&2; exit 65; }
rm -rf "$d"
shelflink
printf '%s\n' "$dest"
`

// Call timeouts: a chunk's exec and a finishing one (inside the tunnel's 100 s).
const (
	ChunkTimeoutS  = 60
	FinishTimeoutS = 90
)

// Push sends b into the box exec reaches and checks it out at Dest. A call
// that fails on the way is tried again (a chunk resent is idempotent); one
// that still fails ends the push with what the box kept, so pushing the same
// bundle again resumes from there.
func Push(ctx context.Context, exec ExecFunc, b *Bundle, o PushOptions) (Result, error) {
	if o.Dest == "" {
		o.Dest = DefaultDest
	}
	if o.Retries <= 0 {
		o.Retries = 4
	}
	if strings.ContainsRune(o.Dest, 0) || len(o.Dest) > 1024 {
		return Result{}, fmt.Errorf("the box directory is over 1024 bytes or holds a NUL")
	}
	if strings.ContainsRune(o.Base, 0) || len(o.Base) > 1024 {
		return Result{}, fmt.Errorf("the base directory is over 1024 bytes or holds a NUL")
	}
	res := Result{Repo: b.Repo, Commit: b.Head, Bytes: b.Size, Dir: o.Dest, Base: b.Base}
	f, err := os.Open(b.Path)
	if err != nil {
		return res, err
	}
	defer f.Close()
	s, err := Send(ctx, exec, f, b.Size, b.Sum, SendOptions{ChunkSize: o.ChunkSize, Parallel: o.Parallel, Retries: o.Retries, Progress: o.Progress, Require: shNeedGit})
	res.Chunks, res.Sent, res.Skipped = s.Chunks, s.Sent, s.Skipped
	if err != nil {
		return res, err
	}
	x, err := Retry(ctx, o.Retries, func() (computeapi.RunTargetExec, error) {
		return exec(ctx, computeapi.ExecRunTargetRequest{Argv: Script(shFinish, s.Key, strconv.Itoa(s.Chunks), b.Sum, o.Dest, b.Shallow, b.Head, o.Base, strings.Join(b.Base, " ")), TimeoutS: FinishTimeoutS})
	})
	if err != nil {
		return res, fmt.Errorf("checking out in the box: %w", err)
	}
	if x.Exit == exitBase || (x.Exit == 64 && o.autoBase) {
		return res, fmt.Errorf("%w: %w", errBaseGone, BoxErr("the box refused the bundle", x))
	}
	if x.Exit != 0 {
		return res, BoxErr("the box refused the bundle", x)
	}
	res.Dest = strings.TrimSpace(x.Stdout)
	return res, nil
}

// Have answers the commits the box holds for o.Dest: its last push's there,
// and the ids o.Base's HEAD and refs name. A box that cannot say answers
// none (and an error), and a whole commit goes in.
func Have(ctx context.Context, exec ExecFunc, o PushOptions) ([]string, error) {
	o.NoBase = true // standalone Have names only the explicit base
	ids, _, err := haveInBox(ctx, exec, o)
	return ids, err
}

func haveInBox(ctx context.Context, exec ExecFunc, o PushOptions) ([]string, bool, error) {
	auto := ""
	if !o.NoBase && o.Base == "" {
		auto = "auto"
	}
	if o.Dest == "" {
		o.Dest = DefaultDest
	}
	x, err := Retry(ctx, max(o.Retries, 1), func() (computeapi.RunTargetExec, error) {
		return exec(ctx, computeapi.ExecRunTargetRequest{Argv: Script(shHave, o.Dest, o.Base, auto), TimeoutS: ChunkTimeoutS})
	})
	if err != nil {
		return nil, false, err
	}
	if x.Exit != 0 {
		return nil, false, BoxErr("the box cannot say what it holds", x)
	}
	var out []string
	for _, f := range strings.Fields(x.Stdout) {
		if isID(f) && len(out) < HaveMax {
			out = append(out, f)
		}
	}
	return out, strings.Contains(x.Stdout, "potluck-host-base"), nil
}

// MakeAndPush resolves mo (Resolve) and pushes it (PushSource).
func MakeAndPush(ctx context.Context, exec ExecFunc, mo MakeOptions, po PushOptions, made func(*Bundle)) (Result, error) {
	s, err := Resolve(ctx, mo)
	if err != nil {
		return Result{}, err
	}
	return PushSource(ctx, exec, s, po, made)
}

// PushSource asks the box what it holds (Have), bundles s against it (only
// what the commit adds) and pushes that (Push). made, when set, hears each
// bundle before it is sent. A bundle the box refuses for lack of its base,
// or one built on a base that is over the limit, is made again whole and
// pushed once more.
func PushSource(ctx context.Context, exec ExecFunc, s *Source, po PushOptions, made func(*Bundle)) (Result, error) {
	var have []string
	if !s.full {
		var automatic bool
		have, automatic, _ = haveInBox(ctx, exec, po) // none: the whole commit goes in
		if automatic {
			po.Base, po.autoBase = ".potluck-base", true
		}
	}
	res, err := pushBundle(ctx, exec, s, have, po, made)
	// A thin bundle carries the history since its base: over the limit (an old
	// base, a churning file), the whole commit may still fit.
	if (errors.Is(err, errBaseGone) && (len(have) > 0 || po.autoBase)) || (api.IsCode(err, api.CodeTooLarge) && len(have) > 0) {
		if po.autoBase {
			po.Base = ""
		}
		res, err = pushBundle(ctx, exec, s, nil, po, made)
	}
	return res, err
}

func pushBundle(ctx context.Context, exec ExecFunc, s *Source, have []string, po PushOptions, made func(*Bundle)) (Result, error) {
	b, err := s.Bundle(ctx, have)
	if err != nil {
		return Result{Repo: s.Repo, Commit: s.Head}, err
	}
	defer b.Close()
	if made != nil {
		made(b)
	}
	return Push(ctx, exec, b, po)
}

// Script is the argv of a box script with literal arguments ($1...): no
// value is ever spliced into the script.
func Script(s string, args ...string) []string {
	return append([]string{"sh", "-c", s, "zm-push"}, args...)
}

// SendOptions say how Send cuts and sends a file.
type SendOptions struct {
	ChunkSize int // default ChunkSize (tests use less)
	Parallel  int // chunks in flight (default 4)
	Retries   int // tries per call (default 4)
	// Progress, when set, hears each chunk as it lands (done of total).
	Progress func(done, total int)
	// Require is more sh the probe runs first (exit 69 with a line on
	// stderr when the box lacks a tool the caller needs after Send).
	Require string
	// Tag, when set, is added to the chunks' key ([0-9a-f]), so sends of
	// the same bytes for different ends keep apart.
	Tag string
}

// Sent is what Send left in the box: the chunks c.0 .. c.<Chunks-1> of a
// file under $HOME/.potluck/push/<Key>, each checked against its SHA-256.
type Sent struct {
	Key     string
	Chunks  int
	Sent    int // chunks this call sent
	Skipped int // chunks an earlier call left in the box
}

// Send puts the size bytes of f (whose SHA-256 is sum, hex) into the box
// in chunks of base64 stdin, skipping the chunks an earlier Send of the same
// bytes left there. Joining them is the caller's next exec (its script
// reads $HOME/.potluck/push/<Key>/c.<i>). A failure says how many arrived.
func Send(ctx context.Context, exec ExecFunc, f io.ReaderAt, size int64, sum string, o SendOptions) (Sent, error) {
	if o.ChunkSize <= 0 {
		o.ChunkSize = ChunkSize
	}
	if o.Parallel <= 0 {
		o.Parallel = 4
	}
	if o.Retries <= 0 {
		o.Retries = 4
	}
	if o.ChunkSize > ChunkSize {
		return Sent{}, fmt.Errorf("a chunk is at most %d bytes", ChunkSize)
	}
	if strings.Trim(o.Tag, "0123456789abcdef") != "" {
		return Sent{}, fmt.Errorf("a send's tag is lowercase hex")
	}
	n := int((size + int64(o.ChunkSize) - 1) / int64(o.ChunkSize))
	// key names the bytes and their cut, so chunks of another never mix in.
	res := Sent{Key: sum[:32] + "-" + strconv.Itoa(o.ChunkSize), Chunks: n}
	if o.Tag != "" {
		res.Key += "-" + o.Tag
	}

	x, err := Retry(ctx, o.Retries, func() (computeapi.RunTargetExec, error) {
		return exec(ctx, computeapi.ExecRunTargetRequest{Argv: Script(ShSum+o.Require+shProbe, res.Key), TimeoutS: ChunkTimeoutS})
	})
	if err != nil {
		return res, fmt.Errorf("reaching the box: %w", err)
	}
	if x.Exit != 0 {
		return res, BoxErr("the box cannot take the bytes", x)
	}
	have := map[int]bool{}
	for _, l := range strings.Fields(x.Stdout) {
		if i, err := strconv.Atoi(l); err == nil && i >= 0 && i < n {
			have[i] = true
		}
	}

	var (
		mu    sync.Mutex
		first error
		done  = len(have)
		wg    sync.WaitGroup
		next  = make(chan int)
	)
	res.Skipped = len(have)
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	for range min(o.Parallel, max(n-len(have), 1)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			buf := make([]byte, o.ChunkSize)
			for i := range next {
				k, err := f.ReadAt(buf, int64(i)*int64(o.ChunkSize))
				if err != nil && !errors.Is(err, io.EOF) {
					fail(&mu, &first, cancel, err)
					continue
				}
				k = int(min(int64(k), size-int64(i)*int64(o.ChunkSize)))
				sum := sha256.Sum256(buf[:k])
				req := computeapi.ExecRunTargetRequest{Argv: Script(ShSum+shChunk, res.Key, strconv.Itoa(i), hex.EncodeToString(sum[:])),
					Stdin: base64.StdEncoding.EncodeToString(buf[:k]), Encoding: computeapi.EncodingBase64, TimeoutS: ChunkTimeoutS}
				x, err := Retry(cctx, o.Retries, func() (computeapi.RunTargetExec, error) {
					x, err := exec(cctx, req)
					if err == nil && x.Exit == exitChecksum { // damaged on the way: send it again
						return x, ErrResend
					}
					return x, err
				})
				switch {
				case err != nil && !errors.Is(err, ErrResend):
					fail(&mu, &first, cancel, fmt.Errorf("chunk %d of %d: %w", i+1, n, err))
					continue
				case x.Exit != 0:
					fail(&mu, &first, cancel, BoxErr(fmt.Sprintf("chunk %d of %d was refused", i+1, n), x))
					continue
				}
				mu.Lock()
				done++
				res.Sent++
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
		if have[i] {
			continue
		}
		select {
		case next <- i:
		case <-cctx.Done():
			break feed
		}
	}
	close(next)
	wg.Wait()
	if first != nil {
		return res, fmt.Errorf("%w (%d of %d chunks are in the box: the same command again resumes)", first, done, n)
	}
	return res, ctx.Err()
}

// ErrResend is a call whose bytes the box refused as damaged on the way
// (exit 65): Retry sends it again.
var ErrResend = errors.New("resend")

func fail(mu *sync.Mutex, first *error, cancel func(), err error) {
	mu.Lock()
	if *first == nil {
		*first = err
	}
	mu.Unlock()
	cancel()
}

// Retry runs call up to tries times while it fails in a way a resend may
// cure (no answer, a dropped relay, a damaged chunk); a refusal by zm or
// the driver is final.
func Retry(ctx context.Context, tries int, call func() (computeapi.RunTargetExec, error)) (computeapi.RunTargetExec, error) {
	var x computeapi.RunTargetExec
	var err error
	for t := range tries {
		if x, err = call(); err == nil || !Transient(err) {
			return x, err
		}
		select {
		case <-ctx.Done():
			return x, ctx.Err()
		case <-time.After(time.Duration(t+1) * 200 * time.Millisecond):
		}
	}
	return x, err
}

// Transient says whether a failed call may succeed when sent again: no
// answer or a dropped relay, not a refusal by zm or the driver.
func Transient(err error) bool {
	if errors.Is(err, ErrResend) {
		return true
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	for _, c := range []string{api.CodeBadRequest, api.CodeForbidden, api.CodeNotFound, api.CodeConflict, api.CodeNotImplemented, api.CodeUnauthenticated, api.CodeTooLarge} {
		if api.IsCode(err, c) {
			return false
		}
	}
	return true
}

// BoxErr is a refusal from the box's script; its stderr is the box's text
// (data), cut to one short line.
func BoxErr(what string, x computeapi.RunTargetExec) error {
	msg := x.Stderr
	if x.Encoding == computeapi.EncodingBase64 {
		if b, err := base64.StdEncoding.DecodeString(msg); err == nil {
			msg = string(b)
		}
	}
	msg = strings.Join(strings.Fields(msg), " ")
	if r := []rune(msg); len(r) > 300 {
		msg = string(r[:300]) + "..."
	}
	switch x.Exit {
	case exitChecksum:
		what += " (checksum)"
	case exitMissing:
		what += " (incomplete)"
	case exitNoTool:
		what += " (missing tool)"
	}
	return fmt.Errorf("%s: exit %d: %s", what, x.Exit, strconv.Quote(msg))
}
