package vmhost

import (
	"context"
	"crypto/rand"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// The npm fetcher puts a want's tarball in the shelf's staging only when its
// bytes are exactly the registry's for the want's hash. For one want:
//
//  1. GET <registry>/<name>/<version>, the URL built from the validated name
//     and version alone (a scoped name's slash written %2f). No credentials,
//     no cookies, no proxy, redirects refused.
//  2. The version document must name the same package and version, and its
//     dist.integrity must hold a sha512 equal to the want's; else the want
//     is dropped.
//  3. dist.tarball must be on the registry's own scheme and host (port
//     included) with no user info; else dropped. A tenant names a package,
//     never a URL.
//  4. The tarball streams into a file under shelfFetching while it is hashed,
//     at most shelfFetchMax bytes, paced by the fill's token bucket. Only a
//     file whose sha512 is the want's is renamed into staging (under the
//     shelf lock); every other is removed.
//
// A dropped want is counted by its reason and forgotten: nothing is stored
// and its name is never printed or logged.

// shelfRegistry is where npm tarballs come from (tests swap it for an
// httptest server).
var shelfRegistry = "https://registry.npmjs.org"

// shelfDocMax bounds a version document.
const shelfDocMax = 8 << 20

// shelfFetchMax bounds one tarball the fetcher keeps (shelfBlobMax; tests
// lower it).
var shelfFetchMax int64 = shelfBlobMax

// shelfFetchFor bounds one request (the tarball's whole body included).
var shelfFetchFor = 30 * time.Minute

// Why a want was dropped. The reason is all that is kept.
const (
	dropStatus    = "status"    // the registry answered other than 200 (404, 401, ...)
	dropRedirect  = "redirect"  // the registry answered a redirect
	dropDoc       = "document"  // the version document is unreadable, too big, or names another package
	dropIntegrity = "integrity" // dist.integrity has no sha512, or not the want's
	dropHost      = "host"      // dist.tarball is not on the registry's scheme and host
	dropOversize  = "oversize"  // the tarball is over shelfFetchMax
	dropMismatch  = "mismatch"  // the tarball's bytes are not the want's hash
	dropError     = "error"     // the network or the disk failed
	dropCap       = "cap"       // the fill already staged shelf_max_gb
	dropFull      = "full"      // the shelf is at shelf_max_gb
	dropFloor     = "floor"     // the host's free disk is below persist_min_free_gb
)

// shelfDrop is a want the fetcher dropped, by reason only.
type shelfDrop struct{ reason string }

func (d shelfDrop) Error() string { return "dropped: " + d.reason }

func drop(reason string) error { return shelfDrop{reason} }

// shelfNpm fetches npm wants into a state directory's staging.
type shelfNpm struct {
	base     *url.URL
	client   *http.Client
	bucket   *shelfBucket
	stateDir string
	kept     int64 // bytes renamed into staging by this fetcher
	room     int64 // the most kept may reach: a tarball past it is dropped as full
}

func newShelfNpm(stateDir string, mbit int) (*shelfNpm, error) {
	base, err := url.Parse(shelfRegistry)
	if err != nil || (base.Scheme != "https" && base.Scheme != "http") || base.Host == "" || base.User != nil {
		return nil, fmt.Errorf("the registry %q is not a plain http(s) URL", shelfRegistry)
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = nil               // no proxy from the environment: the host talks to the registry itself
	tr.DisableCompression = true // the bytes hashed are the bytes sent
	return &shelfNpm{
		room: math.MaxInt64,
		base: base,
		client: &http.Client{
			Transport: tr,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse // a redirect is answered as is, and dropped
			},
			Timeout: shelfFetchFor,
		},
		bucket:   newShelfBucket(mbit),
		stateDir: stateDir,
	}, nil
}

// get GETs u with no credentials; the caller closes the body of a 200.
func (f *shelfNpm) get(ctx context.Context, u string, accept string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, drop(dropError)
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("User-Agent", "potluck-shelf")
	res, err := f.client.Do(req)
	if err != nil {
		return nil, drop(dropError)
	}
	if res.StatusCode != http.StatusOK {
		res.Body.Close()
		if res.StatusCode >= 300 && res.StatusCode < 400 {
			return nil, drop(dropRedirect)
		}
		return nil, drop(dropStatus)
	}
	return res, nil
}

// docURL is the version document's URL for w.
func (f *shelfNpm) docURL(w shelfWant) string {
	return f.base.Scheme + "://" + f.base.Host + strings.TrimSuffix(f.base.Path, "/") + "/" +
		strings.Replace(w.Name, "/", "%2f", 1) + "/" + w.Version
}

// fetch puts w's tarball into staging, or returns a shelfDrop.
func (f *shelfNpm) fetch(ctx context.Context, w shelfWant) error {
	res, err := f.get(ctx, f.docURL(w), "application/json")
	if err != nil {
		return err
	}
	var doc struct {
		Name    string `json:"name"`
		Version string `json:"version"`
		Dist    struct {
			Integrity string `json:"integrity"`
			Tarball   string `json:"tarball"`
		} `json:"dist"`
	}
	err = json.NewDecoder(io.LimitReader(f.bucket.reader(ctx, res.Body), shelfDocMax)).Decode(&doc)
	res.Body.Close()
	if err != nil || doc.Name != w.Name || doc.Version != w.Version {
		return drop(dropDoc)
	}
	if !integrityHas(doc.Dist.Integrity, w.Hex) {
		return drop(dropIntegrity)
	}
	tb, err := url.Parse(doc.Dist.Tarball)
	if err != nil || tb.Scheme != f.base.Scheme || tb.Host != f.base.Host || tb.User != nil || tb.Opaque != "" {
		return drop(dropHost)
	}
	res, err = f.get(ctx, tb.String(), "application/octet-stream")
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.ContentLength > shelfFetchMax {
		return drop(dropOversize)
	}
	if res.ContentLength > 0 && f.kept+res.ContentLength > f.room { // keep checks the bytes too
		return drop(dropFull)
	}
	return f.keep(ctx, w, res.Body)
}

// integrityHas says whether an SRI string lists the sha512 hx.
func integrityHas(sri, hx string) bool {
	_, h := sriSHA512(sri, hx)
	return h != ""
}

// sriSHA512 is an SRI string's sha512 part (`sha512-<b64>`, options cut)
// and its hex: the one equal to hx when hx is given, else the first. Both are
// "" when there is none.
func sriSHA512(sri, hx string) (part, h string) {
	for _, p := range strings.Fields(sri) {
		p, _, _ = strings.Cut(p, "?") // SRI options
		if x := shelfSRIHex(p); x != "" && (hx == "" || x == hx) {
			return p, x
		}
	}
	return "", ""
}

// shelfFetching is where tarballs land while they download: beside staging,
// never in it (the publisher refuses anything in staging that is not a
// verified blob's path).
func shelfFetching(stateDir string) string { return filepath.Join(shelfDir(stateDir), "fetching") }

// keep streams body into a file under shelfFetching while hashing it and
// renames it into staging at w's blob path only when it is at most
// shelfFetchMax bytes and its sha512 is w's.
func (f *shelfNpm) keep(ctx context.Context, w shelfWant, body io.Reader) error {
	dir := shelfFetching(f.stateDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return drop(dropError)
	}
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	part := filepath.Join(dir, hex.EncodeToString(b)+".part")
	defer os.Remove(part) // a no-op once renamed
	out, err := os.OpenFile(part, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return drop(dropError)
	}
	h := sha512.New()
	n, err := io.Copy(io.MultiWriter(out, h), io.LimitReader(f.bucket.reader(ctx, body), shelfFetchMax+1))
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	switch {
	case err != nil:
		return drop(dropError)
	case n > shelfFetchMax:
		return drop(dropOversize)
	case hex.EncodeToString(h.Sum(nil)) != w.Hex:
		return drop(dropMismatch)
	case f.kept+n > f.room:
		return drop(dropFull)
	}
	dst := filepath.Join(shelfStaging(f.stateDir), filepath.FromSlash(w.blob()))
	err = flocked(shelfLock(f.stateDir), func() error {
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			return err
		}
		return os.Rename(part, dst)
	})
	if err != nil {
		return drop(dropError)
	}
	f.kept += n
	return nil
}

// clearFetching removes what a killed fill left under shelfFetching that is
// older than an hour (a fill running beside this one may own the rest).
func clearFetching(stateDir string) {
	es, _ := os.ReadDir(shelfFetching(stateDir))
	for _, e := range es {
		if fi, err := e.Info(); err == nil && time.Since(fi.ModTime()) > time.Hour {
			_ = os.RemoveAll(filepath.Join(shelfFetching(stateDir), e.Name())) // a Go fetch's is a directory
		}
	}
}

// shelfBucket paces the fill's downloads: rate bytes a second, with a burst
// of one read.
type shelfBucket struct {
	rate   float64 // bytes a second
	tokens float64
	last   time.Time
}

// shelfBucketChunk is the most one read takes, and the bucket's burst.
const shelfBucketChunk = 32 << 10

func newShelfBucket(mbit int) *shelfBucket {
	return &shelfBucket{rate: float64(mbit) * 1e6 / 8, tokens: shelfBucketChunk, last: time.Now()}
}

// take waits until n bytes may pass.
func (b *shelfBucket) take(ctx context.Context, n int) error {
	now := time.Now()
	b.tokens = min(b.tokens+now.Sub(b.last).Seconds()*b.rate, shelfBucketChunk)
	b.last = now
	b.tokens -= float64(n)
	if b.tokens >= 0 {
		return nil
	}
	t := time.NewTimer(time.Duration(-b.tokens / b.rate * float64(time.Second)))
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// reader is r paced by the bucket.
func (b *shelfBucket) reader(ctx context.Context, r io.Reader) io.Reader {
	return &pacedReader{ctx: ctx, b: b, r: r}
}

type pacedReader struct {
	ctx context.Context
	b   *shelfBucket
	r   io.Reader
}

func (p *pacedReader) Read(buf []byte) (int, error) {
	if len(buf) > shelfBucketChunk {
		buf = buf[:shelfBucketChunk]
	}
	n, err := p.r.Read(buf)
	if n > 0 {
		if werr := p.b.take(p.ctx, n); werr != nil {
			return n, werr
		}
	}
	return n, err
}

// shelfLockMax bounds a package-lock.json the host reads.
const shelfLockMax = 64 << 20

// shelfNpmRegistry is the only registry a lockfile entry may come from to be
// wanted (the helper's REGISTRY).
const shelfNpmRegistry = "https://registry.npmjs.org/"

// npmLockWants reads a package-lock.json as the in-box helper does
// (vmhost/shelfhelper/potluck-deps): each package resolved from
// registry.npmjs.org with a sha512 integrity is a want, its name and version
// taken from the tarball URL; every line passes parseShelfWant. Entries from
// other registries, git, files or links, and those without a sha512, are left
// out. Each want appears once.
func npmLockWants(r io.Reader) ([]shelfWant, error) {
	b, err := io.ReadAll(io.LimitReader(r, shelfLockMax+1))
	if err != nil {
		return nil, err
	}
	if len(b) > shelfLockMax {
		return nil, fmt.Errorf("over %d MiB", shelfLockMax>>20)
	}
	// One decode into plain values, linear in the file whatever its
	// nesting; then a walk that skips any value of the wrong type, as the
	// helper's JavaScript does.
	var doc any
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, errors.New("not a package-lock.json")
	}
	lock, _ := doc.(map[string]any)
	type entry struct{ resolved, integrity any }
	var es []entry
	switch pkgs := lock["packages"].(type) {
	case []any: // an object to JavaScript, with no install paths
	case map[string]any:
		for key, v := range pkgs {
			e, ok := v.(map[string]any)
			if !ok || truthy(e["link"]) || !strings.Contains(key, "node_modules/") {
				continue
			}
			es = append(es, entry{e["resolved"], e["integrity"]})
		}
	default:
		stack := []any{lock["dependencies"]}
		for len(stack) > 0 {
			deps, _ := stack[len(stack)-1].(map[string]any)
			stack = stack[:len(stack)-1]
			for _, v := range deps {
				e, ok := v.(map[string]any)
				if !ok {
					continue
				}
				es = append(es, entry{e["resolved"], e["integrity"]})
				stack = append(stack, e["dependencies"])
			}
		}
	}
	var out []shelfWant
	seen := map[shelfWant]bool{}
	for _, e := range es {
		resolved, _ := e.resolved.(string)
		integrity, _ := e.integrity.(string)
		name, version, ok := npmFromURL(resolved)
		if !ok {
			continue
		}
		b64, _ := sriSHA512(integrity, "")
		if b64 == "" {
			continue
		}
		w, ok := parseShelfWant("npm " + name + " " + version + " " + b64)
		if !ok || seen[w] {
			continue
		}
		seen[w] = true
		out = append(out, w)
	}
	return out, nil
}

// truthy is JavaScript's truthiness for a JSON value, as the helper tests
// v.link.
func truthy(v any) bool {
	switch v := v.(type) {
	case nil:
		return false
	case bool:
		return v
	case string:
		return v != ""
	case float64:
		return v != 0
	}
	return true
}

// npmFromURL is the name and version npm fetches from a registry tarball
// URL, <registry><name>/-/<base>-<version>.tgz, where name is base or
// @scope/base (its slash may be written %2f), as the helper's fromURL.
func npmFromURL(resolved string) (name, version string, ok bool) {
	rest, ok := strings.CutPrefix(resolved, shelfNpmRegistry)
	if !ok {
		return "", "", false
	}
	pkg, file, ok := strings.Cut(rest, "/-/")
	if !ok || strings.ContainsAny(file, "/?#%") {
		return "", "", false
	}
	file, ok = strings.CutSuffix(file, ".tgz")
	if !ok {
		return "", "", false
	}
	base := pkg
	if strings.HasPrefix(pkg, "@") {
		scope, b, found := strings.Cut(pkg, "/")
		if !found {
			scope, b, found = cutFold(pkg, "%2f")
		}
		if !found {
			return "", "", false
		}
		name, base = scope+"/"+b, b
	} else {
		name = pkg
	}
	if base == "" || strings.ContainsAny(base, "/%") {
		return "", "", false
	}
	version, ok = strings.CutPrefix(file, base+"-")
	return name, version, ok && version != ""
}

// cutFold is strings.Cut with sep matched without regard to case.
func cutFold(s, sep string) (before, after string, found bool) {
	if i := strings.Index(strings.ToLower(s), strings.ToLower(sep)); i >= 0 {
		return s[:i], s[i+len(sep):], true
	}
	return s, "", false
}
