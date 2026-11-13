package vmhost

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/sengirunners/potluck/internal/tarsafe"
)

// Go modules on the shelf. A box reads them through GOPROXY (ociCacheGoproxy,
// set on every box by cacheArgs): the shelf's goproxy/ tree first, then the
// public proxy, then direct. Go checks every module it reads against the
// box's own go.sum (and the checksum database for one go.sum lacks): a
// missing file costs the box a download; a wrong one fails its build with
// Go's checksum error, never passes as the module (unless the box turned
// the checks off with GONOSUMDB or GOSUMDB=off).
//
// A Go want names a module version (`go <module> <version>`) or only its
// go.mod (`go <module> <version>/go.mod`), as go.sum lines do. The host
// fills one as follows:
//
//  1. A throwaway container of the binding's own pinned image, on the box
//     network (so the owner's egress filter and bandwidth cap apply), runs the
//     newest Go toolchain on the shelf (tools/go/<version>/<arch>, the owner's
//     fill script's) with GOPROXY=https://proxy.golang.org (never direct, so
//     the host never contacts a host a tenant named), the public checksum
//     database on and nothing private: `go mod download` for a version,
//     `go list -m` for a go.mod. Go verifies what it downloads against the
//     checksum database before it writes it.
//  2. The container sends back, as a tar on stdout, only the module cache's
//     .info, .mod, .zip and .ziphash files. The host keeps only files of the
//     versions it asked for, under size caps, in a directory under
//     shelfFetching; it checks the .info names the version and the .zip's
//     hash (Go's h1, over the zip's files) is the .ziphash; then it renames
//     the version's files into staging (under the shelf lock).
//  3. The publisher (shelf_publish.go) checks the same again and renames
//     each file onto the shelf, mode 0444: every .zip first, then .ziphash,
//     .mod, and each .info last. A version is on the shelf when its .info and
//     its .zip (or, for a go.mod want, its .mod) are.
//
// Wants of private modules never reach the host from the helper (it leaves
// out what the box's GOPRIVATE, GONOPROXY, GONOSUMDB or GOINSECURE match,
// writes no Go wants at all unless the box's GOPROXY goes first to the
// public proxy and its GOSUMDB is the public database, and writes none from
// the driver's own link step, which runs before the job could set them). A box can still write any
// line, so a private name it writes on purpose is asked of the public proxy
// anonymously, which has nothing and stores nothing. Logs count; no module
// path is printed.

// ociCacheGoproxy is GOPROXY in every box with the shelf: the shelf's tree,
// then what Go uses by default.
const ociCacheGoproxy = "file://" + ociCacheDir + "/" + shelfGoproxyDir + ",https://proxy.golang.org,direct"

// shelfGoproxyDir is the GOPROXY tree's directory in the volume.
const shelfGoproxyDir = "goproxy"

// Size caps on one version's files, as the host keeps them.
const (
	goInfoMax    = 64 << 10
	goModMax     = 16 << 20
	goZipMax     = 512 << 20 // Go's own limit on a module zip is 500 MiB
	goZipHashMax = 256
	goUnzipMax   = 1 << 30 // what hashing a zip may read: Go's limit on a module's files is 500 MiB
)

// Why a Go want was dropped (beside shelf_npm.go's reasons).
const (
	dropToolchain = "toolchain" // the shelf has no Go toolchain for the host's arch
	dropNetwork   = "network"   // the binding's boxes have no network to fetch on
	dropMissing   = "missing"   // the proxy or the checksum database gave nothing for it
)

// shelfGoFetchFor bounds one fetch container.
var shelfGoFetchFor = 30 * time.Minute

// goVersionRe is a canonical Go module version: v and a semantic version
// whose only build metadata may be +incompatible.
var goVersionRe = regexp.MustCompile(`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)` +
	`(?:-(?:0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*)(?:\.(?:0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*))*)?` +
	`(?:\+incompatible)?$`)

// goModulePath says whether p is a module path Go accepts (its CheckPath,
// less the major-version suffix rule, which the proxy enforces): elements of
// ASCII letters, digits and - . _ ~, none empty, all dots, or starting or
// ending with a dot; a first element of lower case letters, digits, - and .
// holding a dot and not starting with -.
func goModulePath(p string) bool {
	if p == "" || len(p) > 400 {
		return false
	}
	for i, e := range strings.Split(p, "/") {
		if e == "" || strings.Count(e, ".") == len(e) || e[0] == '.' || e[len(e)-1] == '.' {
			return false
		}
		for j := 0; j < len(e); j++ {
			c := e[j]
			lower := 'a' <= c && c <= 'z' || '0' <= c && c <= '9' || c == '-' || c == '.'
			if i == 0 && !lower || !lower && !('A' <= c && c <= 'Z' || c == '_' || c == '~') {
				return false
			}
		}
		if i == 0 && (!strings.Contains(e, ".") || e[0] == '-') {
			return false
		}
	}
	return true
}

// goVersion says whether v is a canonical module version.
func goVersion(v string) bool { return len(v) <= 128 && goVersionRe.MatchString(v) }

// parseGoWant is parseShelfWant's go line: a module path and a version,
// with /go.mod after the version for the go.mod alone.
func parseGoWant(mod, ver string) (shelfWant, bool) {
	v, goMod := strings.CutSuffix(ver, "/go.mod")
	if !goModulePath(mod) || !goVersion(v) {
		return shelfWant{}, false
	}
	return shelfWant{Kind: shelfKindGo, Name: mod, Version: v, GoMod: goMod}, true
}

// goVer is a Go want's version as go.sum writes it: /go.mod after it for
// the go.mod alone.
func (w shelfWant) goVer() string {
	if w.GoMod {
		return w.Version + "/go.mod"
	}
	return w.Version
}

// goLine is a Go want as a wants line.
func (w shelfWant) goLine() string { return shelfKindGo + " " + w.Name + " " + w.goVer() }

// goEscape is Go's case encoding for module cache and proxy paths: each
// upper case letter becomes ! and its lower case.
func goEscape(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if c := s[i]; 'A' <= c && c <= 'Z' {
			b.WriteByte('!')
			b.WriteByte(c + 'a' - 'A')
		} else {
			b.WriteByte(c)
		}
	}
	return b.String()
}

// goUnescape undoes goEscape, refusing what goEscape could not have made.
func goUnescape(s string) (string, bool) {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case 'A' <= c && c <= 'Z':
			return "", false
		case c == '!':
			if i+1 == len(s) || s[i+1] < 'a' || s[i+1] > 'z' {
				return "", false
			}
			i++
			b.WriteByte(s[i] - 'a' + 'A')
		default:
			b.WriteByte(c)
		}
	}
	return b.String(), true
}

// goFile is one file of a version in a GOPROXY tree, relative to its root:
// <escaped module>/@v/<escaped version>.<ext>.
func goFile(mod, ver, ext string) string {
	return goEscape(mod) + "/@v/" + goEscape(ver) + "." + ext
}

// goFileRe splits a GOPROXY tree path; '@' is in no module path, so the
// split is the only one.
var goFileRe = regexp.MustCompile(`^([^@]+)/@v/([^/]+)\.(info|mod|zip|ziphash)$`)

// parseGoFile reads a path made by goFile back, refusing any other.
func parseGoFile(p string) (mod, ver, ext string, ok bool) {
	m := goFileRe.FindStringSubmatch(p)
	if m == nil {
		return "", "", "", false
	}
	mod, ok1 := goUnescape(m[1])
	ver, ok2 := goUnescape(m[2])
	if !ok1 || !ok2 || !goModulePath(mod) || !goVersion(ver) {
		return "", "", "", false
	}
	return mod, ver, m[3], true
}

// goFileMax is the size cap for a file of type ext.
func goFileMax(ext string) int64 {
	switch ext {
	case "info":
		return goInfoMax
	case "mod":
		return goModMax
	case "zip":
		return goZipMax
	}
	return goZipHashMax
}

// goFiles are the files a want needs, relative to the GOPROXY tree.
func (w shelfWant) goFiles() []string {
	if w.GoMod {
		return []string{goFile(w.Name, w.Version, "info"), goFile(w.Name, w.Version, "mod")}
	}
	return []string{goFile(w.Name, w.Version, "info"), goFile(w.Name, w.Version, "mod"),
		goFile(w.Name, w.Version, "zip"), goFile(w.Name, w.Version, "ziphash")}
}

// goPresent are the files whose presence on the shelf means the want is
// there: the .info, published last, and the .zip (or .mod) it vouches for.
func (w shelfWant) goPresent() []string {
	if w.GoMod {
		return []string{goFile(w.Name, w.Version, "info"), goFile(w.Name, w.Version, "mod")}
	}
	return []string{goFile(w.Name, w.Version, "info"), goFile(w.Name, w.Version, "zip")}
}

// goZipHash is Go's h1 hash of a module zip (golang.org/x/mod/sumdb/dirhash's
// HashZip): base64 of the sha256 of the lines "<sha256 hex>  <name>\n" for
// each file, sorted by name. Reading stops past goUnzipMax.
func goZipHash(r io.ReaderAt, size int64) (string, error) {
	z, err := zip.NewReader(r, size)
	if err != nil {
		return "", err
	}
	names := make([]string, 0, len(z.File))
	byName := map[string]*zip.File{}
	for _, f := range z.File {
		if strings.Contains(f.Name, "\n") {
			return "", errors.New("a file name with a newline")
		}
		if _, dup := byName[f.Name]; dup {
			return "", errors.New("a file named twice")
		}
		byName[f.Name] = f
		names = append(names, f.Name)
	}
	sort.Strings(names)
	sum := sha256.New()
	left := int64(goUnzipMax)
	for _, n := range names {
		rc, err := byName[n].Open()
		if err != nil {
			return "", err
		}
		h := sha256.New()
		c, err := io.Copy(h, io.LimitReader(rc, left+1))
		rc.Close()
		if err != nil {
			return "", err
		}
		if left -= c; left < 0 {
			return "", errors.New("the zip's files are too big")
		}
		fmt.Fprintf(sum, "%x  %s\n", h.Sum(nil), n)
	}
	return "h1:" + base64.StdEncoding.EncodeToString(sum.Sum(nil)), nil
}

// goInfoOK says whether b is a .info naming version ver.
func goInfoOK(b []byte, ver string) bool {
	var info struct{ Version string }
	return json.Unmarshal(b, &info) == nil && info.Version == ver
}

// goPrivate says whether module mod matches any of the comma-separated
// path prefix globs (GOPRIVATE's syntax; Go's module.MatchPrefixPatterns).
func goPrivate(globs, mod string) bool {
	for _, g := range strings.Split(globs, ",") {
		g = strings.TrimSuffix(g, "/")
		if g == "" {
			continue
		}
		n := strings.Count(g, "/")
		prefix := mod
		for i := 0; i < len(mod); i++ {
			if mod[i] == '/' {
				if n == 0 {
					prefix = mod[:i]
					break
				}
				n--
			}
		}
		if n > 0 {
			continue
		}
		if ok, _ := path.Match(g, prefix); ok {
			return true
		}
	}
	return false
}

// shelfGoSumMax bounds a go.sum the host reads.
const shelfGoSumMax = 64 << 20

// goSumWants reads a go.sum as the in-box helper does: each line names a
// module version (or its go.mod) to want, unless the module matches private
// (GOPRIVATE's syntax). A version wanted whole is not wanted again for its
// go.mod. Every line passes parseShelfWant; others are left out.
func goSumWants(r io.Reader, private string) ([]shelfWant, error) {
	b, err := io.ReadAll(io.LimitReader(r, shelfGoSumMax+1))
	if err != nil {
		return nil, err
	}
	if len(b) > shelfGoSumMax {
		return nil, fmt.Errorf("over %d MiB", shelfGoSumMax>>20)
	}
	var out []shelfWant
	whole := map[string]bool{}
	seen := map[string]bool{}
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) != 3 || !strings.HasPrefix(f[2], "h1:") || goPrivate(private, f[0]) {
			continue
		}
		w, ok := parseShelfWant(shelfKindGo + " " + f[0] + " " + f[1])
		if !ok || seen[w.goLine()] {
			continue
		}
		seen[w.goLine()] = true
		if !w.GoMod {
			whole[w.Name+" "+w.Version] = true
		}
		out = append(out, w)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return slices.DeleteFunc(out, func(w shelfWant) bool { return w.GoMod && whole[w.Name+" "+w.Version] }), nil
}

// goFetched is what fillGo did.
type goFetched struct {
	present, touched, fetched int
	bytes                     int64
	dropped                   map[string]int
}

// goRoom is what a Go fill may stage: at most limit bytes (the fill's cap;
// the last version may pass it) and room (under shelf_max_gb; a version that
// would pass it is dropped as full), and nothing while floor answers an
// error.
type goRoom struct {
	limit, room int64
	floor       func() error
}

// shelfGoStaged says whether want w's version waits in staging whole.
func shelfGoStaged(stateDir string, w shelfWant) bool {
	for _, f := range w.goPresent() {
		fi, err := os.Lstat(filepath.Join(shelfStaging(stateDir), shelfGoproxyDir, filepath.FromSlash(f)))
		if err != nil || !fi.Mode().IsRegular() {
			return false
		}
	}
	return true
}

// fillGo fetches the Go wants neither on the shelf nor staged into staging,
// within r, and moves forward the recency of those on the shelf. Each
// distinct want counts once.
func fillGo(ctx context.Context, cfg config, sh shelfer, wants []shelfWant, r goRoom) (res goFetched, wanted int, err error) {
	res.dropped = map[string]int{}
	var uniq []shelfWant
	seen, whole := map[string]bool{}, map[string]bool{}
	for _, w := range wants {
		if !w.GoMod {
			whole[w.Name+" "+w.Version] = true
		}
	}
	for _, w := range wants {
		// A version wanted whole brings its go.mod: the go.mod want is the same want.
		if !seen[w.goLine()] && !(w.GoMod && whole[w.Name+" "+w.Version]) {
			seen[w.goLine()] = true
			uniq = append(uniq, w)
		}
	}
	if len(uniq) == 0 {
		return res, 0, nil
	}
	var rels []string
	for _, w := range uniq {
		for _, f := range w.goPresent() {
			rels = append(rels, shelfGoproxyDir+"/"+f)
		}
	}
	has, err := sh.ShelfHasFiles(cfg.Image, rels)
	if err != nil {
		return res, len(uniq), err
	}
	var absent []shelfWant
	var touch []string
	for _, w := range uniq {
		all := true
		for _, f := range w.goPresent() {
			all = all && has[shelfGoproxyDir+"/"+f]
		}
		switch {
		case all:
			res.present++
			touch = append(touch, shelfGoproxyDir+"/"+goFile(w.Name, w.Version, "info"))
		case !shelfGoStaged(cfg.StateDir, w):
			absent = append(absent, w)
		}
	}
	// A missed touch only lets the version go sooner, so it does not stop the fill.
	res.touched, _ = sh.TouchShelfFiles(cfg.Image, touch, cfg.Shelf.KeepS)
	// In batches, so the cap, the room and the floor hold between them: one
	// batch downloads at most shelfGoBatch versions past them.
	for len(absent) > 0 {
		n := min(len(absent), shelfGoBatch)
		batch := absent[:n]
		absent = absent[n:]
		why := ""
		switch {
		case ctx.Err() != nil:
			return res, len(uniq), ctx.Err()
		case res.bytes >= r.limit:
			why = dropCap
		case res.bytes >= r.room:
			why = dropFull
		case r.floor() != nil:
			why = dropFloor
		}
		if why != "" {
			res.dropped[why] += n + len(absent)
			break
		}
		if err := fetchGoBatch(ctx, cfg, sh, batch, r, &res); err != nil {
			res.dropped[dropError] += len(absent)
			return res, len(uniq), err
		}
	}
	return res, len(uniq), nil
}

// shelfGoBatch is the most versions one fetch container fetches.
const shelfGoBatch = 8

// fetchGoBatch runs one fetch container for batch and keeps what it brought
// that is whole and checked, within r. A shelfDrop from FetchGo (no network,
// no toolchain) drops the batch by its reason and is no error.
func fetchGoBatch(ctx context.Context, cfg config, sh shelfer, batch []shelfWant, r goRoom, res *goFetched) error {
	dir := shelfFetching(cfg.StateDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(dir, "go-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	var in strings.Builder
	for _, w := range batch {
		in.WriteString(w.Name + " " + w.goVer() + "\n")
	}
	pr, pw := io.Pipe()
	got := make(chan map[string]int64, 1)
	budget := min(r.limit, r.room) - res.bytes + goZipMax // what ingest keeps at most: the last version may pass
	go func() {
		kept, err := ingestGoTar(pr, tmp, batch, budget)
		pr.CloseWithError(err)
		got <- kept
	}()
	ferr := sh.FetchGo(ctx, cfg.Image, strings.NewReader(in.String()), pw)
	pw.CloseWithError(ferr)
	sizes := <-got
	if ferr != nil {
		why := dropError
		var d shelfDrop
		if errors.As(ferr, &d) {
			why = d.reason
		}
		res.dropped[why] += len(batch)
		if why != dropError {
			return nil // the binding's shape, not a failure
		}
		return ferr
	}
	for _, w := range batch {
		if res.bytes >= r.limit {
			res.dropped[dropCap]++
			continue
		}
		if res.bytes+goSize(w, sizes) > r.room {
			res.dropped[dropFull]++
			continue
		}
		n, why := keepGoVersion(cfg.StateDir, tmp, w, sizes)
		if why != "" {
			res.dropped[why]++
			continue
		}
		res.fetched++
		res.bytes += n
	}
	return nil
}

// ingestGoTar writes the files of the fetch container's tar that belong to
// a want into dir, under the size caps, and answers each kept file's size.
// Other entries are skipped; a link, a name outside the tree or a sparse
// file ends the read (nothing of it is kept).
func ingestGoTar(r io.Reader, dir string, wants []shelfWant, budget int64) (map[string]int64, error) {
	ok := map[string]bool{}
	for _, w := range wants {
		for _, f := range w.goFiles() {
			ok[f] = true
		}
	}
	kept := map[string]int64{}
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			_, err = io.Copy(io.Discard, r) // tar's padding after its end, so the container's write ends cleanly
			return kept, err
		}
		if err != nil {
			return nil, err
		}
		name, safe := tarsafe.Name(hdr.Name)
		if !safe || tarsafe.Sparse(hdr) {
			return nil, fmt.Errorf("the fetch container sent a link, a sparse file or a name outside its tree: %w", errShelfRefused)
		}
		switch hdr.Typeflag {
		case tar.TypeDir, tar.TypeXGlobalHeader:
			continue
		case tar.TypeReg, '\x00':
		default:
			return nil, fmt.Errorf("the fetch container sent an entry that is not a regular file (%s): %w", tarsafe.TypeName(hdr.Typeflag), errShelfRefused)
		}
		_, _, ext, isGo := parseGoFile(name)
		if !isGo || !ok[name] || hdr.Size > goFileMax(ext) || hdr.Size > budget {
			continue // the tar reader skips its bytes
		}
		budget -= hdr.Size
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			return nil, err
		}
		f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return nil, err
		}
		n, err := io.Copy(f, io.LimitReader(tr, hdr.Size))
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil || n != hdr.Size {
			return nil, fmt.Errorf("the fetch container's tar was cut: %v", err)
		}
		kept[name] = n
	}
}

// goSize is the bytes of w's files that came.
func goSize(w shelfWant, sizes map[string]int64) (n int64) {
	for _, f := range w.goFiles() {
		n += sizes[f]
	}
	return n
}

// keepGoVersion checks one want's files in dir (all there, the .info naming
// the version, the .zip's h1 the .ziphash) and renames them into staging,
// under the shelf lock. It answers their bytes, or why the want is dropped.
func keepGoVersion(stateDir, dir string, w shelfWant, sizes map[string]int64) (int64, string) {
	var n int64
	for _, f := range w.goFiles() {
		s, ok := sizes[f]
		if !ok {
			return 0, dropMissing
		}
		n += s
	}
	info, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(goFile(w.Name, w.Version, "info"))))
	if err != nil || !goInfoOK(info, w.Version) {
		return 0, dropMismatch
	}
	if !w.GoMod {
		if why := goZipMatches(filepath.Join(dir, filepath.FromSlash(goFile(w.Name, w.Version, "zip"))),
			filepath.Join(dir, filepath.FromSlash(goFile(w.Name, w.Version, "ziphash")))); why != "" {
			return 0, why
		}
	}
	err = flocked(shelfLock(stateDir), func() error {
		for _, f := range w.goFiles() {
			dst := filepath.Join(shelfStaging(stateDir), shelfGoproxyDir, filepath.FromSlash(f))
			if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
				return err
			}
			if err := os.Rename(filepath.Join(dir, filepath.FromSlash(f)), dst); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return 0, dropError
	}
	return n, ""
}

// goZipMatches says why the zip at zp is not the one its .ziphash at hp
// names, or "" when it is.
func goZipMatches(zp, hp string) string {
	want, err := os.ReadFile(hp)
	if err != nil {
		return dropError
	}
	z, err := os.Open(zp)
	if err != nil {
		return dropError
	}
	defer z.Close()
	fi, err := z.Stat()
	if err != nil {
		return dropError
	}
	got, err := goZipHash(z, fi.Size())
	if err != nil || got != strings.TrimSpace(string(want)) {
		return dropMismatch
	}
	return ""
}

// shelfGoFetchSh is the fetch container's script: $1 is the shelf, mounted
// read-only; stdin is lines "<module> <version>[/go.mod]" the host made from
// validated wants; stdout is a tar of the module cache's .info, .mod, .zip
// and .ziphash files. It exits 3 when the shelf has no Go toolchain for this
// machine's arch. Go's own output is dropped (it names modules).
const shelfGoFetchSh = `set -u
umask 077
case $(uname -m) in x86_64) a=x64 ;; aarch64|arm64) a=arm64 ;; *) a= ;; esac
go=
if [ -n "$a" ]; then
  go=$(for c in "$1"/tools/go/*/"$a".complete; do
    d=${c%.complete}; [ -f "$c" ] && [ -x "$d/bin/go" ] || continue
    v=${d%/*}; echo "${v##*/} $d/bin/go"
  done | sort -t. -k1,1n -k2,2n -k3,3n | tail -n 1 | cut -d' ' -f2-)
fi
[ -n "$go" ] || { echo "no Go toolchain on the shelf for $(uname -m)" >&2; exit 3; }
w=$(mktemp -d) || exit 1
export HOME="$w/home" GOPATH="$w/path" GOMODCACHE="$w/mod" GOCACHE="$w/build" GOENV=off GO111MODULE=on \
  GOPROXY=https://proxy.golang.org GOSUMDB=sum.golang.org GONOSUMDB= GOPRIVATE= GONOPROXY= GOINSECURE= \
  GOFLAGS=-mod=mod GOTOOLCHAIN=local
mkdir -p "$HOME" && cd "$w" || exit 1
while read -r m v; do
  case $v in
  */go.mod) "$go" list -m -json "$m@${v%/go.mod}" </dev/null >/dev/null 2>&1 ;;
  *) "$go" mod download "$m@$v" </dev/null >/dev/null 2>&1 ;;
  esac
done
if cd "$GOMODCACHE/cache/download" 2>/dev/null; then
  find . -path ./sumdb -prune -o -type f \( -name '*.info' -o -name '*.mod' -o -name '*.zip' -o -name '*.ziphash' \) -print >"$w/list" 2>/dev/null
  tar -cf - -T "$w/list" 2>/dev/null
else
  tar -cf - -T /dev/null
fi
`

// FetchGo runs the fetch container (shelfGoFetchSh) with lines on stdin and
// its tar to out. It is a box of the binding's own: image is config.Image,
// the runtime and network are the boxes', and the shelf is mounted
// read-only. A binding with no network, or a shelf without a toolchain, is a
// shelfDrop.
func (o ociCLI) FetchGo(ctx context.Context, image string, lines io.Reader, out io.Writer) error {
	if !ociNetHasDNS(o.network) || o.network == "" {
		return drop(dropNetwork)
	}
	if err := ociNetReady(o.network); err != nil {
		return err
	}
	o, err := o.outerVolumeEngine() // as every helper that mounts the shelf
	if err != nil {
		return err
	}
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	name := "potluck-gofetch-" + hex.EncodeToString(b)
	dns, err := o.dnsArgs(name)
	if err != nil {
		return err
	}
	defer os.Remove(o.resolvPath(name))
	a := append([]string{"run", "--rm", "-i", "--name", name, "--runtime", o.runtime, "--network", o.network}, dns...)
	a = append(a, "--user", "65534:65534", "--security-opt", "no-new-privileges", "--cap-drop", "ALL",
		"--pids-limit", "256", "--memory", "2048m",
		"--mount", "type=volume,source="+ShelfVolume+",target=/shelf,readonly,volume-nocopy",
		"--entrypoint", "sh", image, "-c", shelfGoFetchSh, "sh", "/shelf")
	err = o.shelfExec(ctx, name, a, lines, out, shelfGoFetchFor)
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() == 3 {
		return drop(dropToolchain)
	}
	return err
}

// shelfHasFilesSh prints each line of stdin (a path under the shelf the host
// built) that is a regular file on the shelf; $1 is the shelf.
const shelfHasFilesSh = `while IFS= read -r p; do
  f="$1/$p"
  if [ -f "$f" ] && [ ! -L "$f" ]; then echo "$p"; fi
done
exit 0
`

// shelfRelRe is a path ShelfHasFiles takes: plain elements of the
// characters a GOPROXY tree's paths use.
var shelfRelRe = regexp.MustCompile(`^[A-Za-z0-9._~!@+-]+(/[A-Za-z0-9._~!@+-]+)*$`)

// shelfRelOKPath says whether p is a path the shelf's scripts take: plain
// elements, none . or ..
func shelfRelOKPath(p string) bool {
	es := strings.Split(p, "/")
	return shelfRelRe.MatchString(p) && !slices.Contains(es, "..") && !slices.Contains(es, ".")
}

// ShelfHasFiles asks a read-only helper which of the paths (relative to
// the shelf) are regular files there.
func (o ociCLI) ShelfHasFiles(image string, rels []string) (map[string]bool, error) {
	has := map[string]bool{}
	if len(rels) == 0 {
		return has, nil
	}
	var in strings.Builder
	want := map[string]bool{}
	for _, p := range rels {
		if !shelfRelOKPath(p) {
			return nil, fmt.Errorf("not a plain path in the shelf: %q", p)
		}
		want[p] = true
		in.WriteString(p + "\n")
	}
	out, err := o.shelfRun(image, false, []string{"DAC_READ_SEARCH"}, shelfHasFilesSh, strings.NewReader(in.String()), shelfMeasureFor)
	if err != nil {
		return nil, err
	}
	for _, l := range strings.Split(out, "\n") {
		if l = strings.TrimSpace(l); want[l] {
			has[l] = true
		}
	}
	return has, nil
}

// shelfGoDeleteSh deletes each Go module version stdin names as
// "<mtime> <base>" (base: goproxy/<module>/@v/<version>, as the listing
// gave it) whose recency (its .info's mtime, else its newest file's) is
// still mtime: its .info first, so no reader finds a version whose files
// are going, then .mod, .ziphash and .zip. It prints "gone <base>" for
// each; $1 is the shelf, read-write. It refuses a symlink on the way.
const shelfGoDeleteSh = `set -eu
s=$1
[ ! -L "$s/` + shelfGoproxyDir + `" ] || { echo "goproxy/ is a symlink: nothing deleted" >&2; exit 5; }
[ -d "$s/` + shelfGoproxyDir + `" ] || exit 0
while IFS=' ' read -r m b; do
  case $b in (` + shelfGoproxyDir + `/?*/@v/?*) ;; (*) continue ;; esac
  case $b in (*/../*|*/./*|*//*|*/..|*/.) continue ;; esac
  c=$s; ok=1
  set -f; IFS=/
  for e in ${b%/*}; do c=$c/$e; [ ! -L "$c" ] || ok=; done
  unset IFS; set +f
  [ -n "$ok" ] && [ -d "$s/${b%/*}" ] || continue
  if [ -f "$s/$b.info" ]; then cur=$(stat -c %Y "$s/$b.info" 2>/dev/null) || continue
  else
    cur=0
    for e in mod ziphash zip; do
      if [ -f "$s/$b.$e" ]; then t=$(stat -c %Y "$s/$b.$e" 2>/dev/null) || continue; [ "$t" -le "$cur" ] || cur=$t; fi
    done
  fi
  [ "$cur" = "$m" ] || continue
  for e in info mod ziphash zip; do
    if [ -L "$s/$b.$e" ] || [ -f "$s/$b.$e" ]; then rm -f "$s/$b.$e" 2>/dev/null || :; fi
  done
  echo "gone $b"
done
`

// DeleteShelfGo deletes the versions through the publisher's container,
// under the publisher's lock, and answers which went.
func (o ociCLI) DeleteShelfGo(image string, gos []shelfGoAt) (map[string]bool, error) {
	var in strings.Builder
	want := map[string]bool{}
	for _, g := range gos {
		rel, ok := strings.CutPrefix(g.Base, shelfGoproxyDir+"/")
		if _, _, _, valid := parseGoFile(rel + ".info"); !ok || !valid {
			return nil, fmt.Errorf("not a Go module version on the shelf: %q", g.Base)
		}
		want[g.Base] = true
		fmt.Fprintf(&in, "%d %s\n", g.Mtime, g.Base)
	}
	gone := map[string]bool{}
	err := o.shelfWrite(func() error {
		out, err := o.shelfRun(image, true, []string{"DAC_OVERRIDE", "FOWNER"}, shelfGoDeleteSh, strings.NewReader(in.String()), shelfEvictFor)
		for _, l := range strings.Split(out, "\n") {
			if b, ok := strings.CutPrefix(l, "gone "); ok && want[b] {
				gone[b] = true
			}
		}
		return err
	})
	return gone, err
}

// shelfTouchFilesSh moves forward the mtime of each file stdin names
// (relative to the shelf $1, read-write, one a line) that is a regular file
// reached through no symlink and more than ageMin minutes old. It prints
// "touched N".
func shelfTouchFilesSh(ageMin int) string {
	return `set -eu
s=$1; n=0
while IFS= read -r p; do
  case $p in (*/../*|*/./*|*//*|../*|./*|/*) continue ;; esac
  c=$s; ok=1
  set -f; IFS=/
  for e in ${p%/*}; do c=$c/$e; [ ! -L "$c" ] || ok=; done
  unset IFS; set +f
  f=$s/$p
  [ -n "$ok" ] && [ -f "$f" ] && [ ! -L "$f" ] || continue
  [ -n "$(find "$f" -mmin +` + strconv.Itoa(ageMin) + `)" ] || continue
  touch -c "$f" && n=$((n+1))
done
echo "touched $n"
`
}

// TouchShelfFiles moves forward the mtime of each file named (relative to
// the shelf, as ShelfHasFiles takes them) older than shelfTouchAge(keepS),
// through the publisher's container under its lock.
func (o ociCLI) TouchShelfFiles(image string, rels []string, keepS int) (int, error) {
	if len(rels) == 0 {
		return 0, nil
	}
	var in strings.Builder
	for _, p := range rels {
		if !shelfRelOKPath(p) {
			return 0, fmt.Errorf("not a plain path in the shelf: %q", p)
		}
		in.WriteString(p + "\n")
	}
	n := 0
	err := o.shelfWrite(func() error {
		out, err := o.shelfRun(image, true, []string{"DAC_OVERRIDE", "FOWNER"}, shelfTouchFilesSh(max(1, int(shelfTouchAge(keepS)/time.Minute))), strings.NewReader(in.String()), shelfEvictFor)
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
