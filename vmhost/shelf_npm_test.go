package vmhost

import (
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// line is the want as a wants line.
func (w shelfWant) line() string {
	b, _ := hex.DecodeString(w.Hex)
	return "npm " + w.Name + " " + w.Version + " sha512-" + base64.StdEncoding.EncodeToString(b)
}

func sri512(b []byte) string {
	h := sha512.Sum512(b)
	return "sha512-" + base64.StdEncoding.EncodeToString(h[:])
}

// fakeRegistry serves version documents and tarballs by escaped path and
// records every request it got.
type fakeRegistry struct {
	t     *testing.T
	srv   *httptest.Server
	mu    sync.Mutex
	paths map[string]http.HandlerFunc
	hits  []string
}

func newFakeRegistry(t *testing.T) *fakeRegistry {
	r := &fakeRegistry{t: t, paths: map[string]http.HandlerFunc{}}
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		r.mu.Lock()
		r.hits = append(r.hits, q.URL.EscapedPath())
		h := r.paths[q.URL.EscapedPath()]
		r.mu.Unlock()
		for _, k := range []string{"Authorization", "Cookie", "Npm-Auth-Type"} {
			if q.Header.Get(k) != "" {
				t.Errorf("the fetcher sent %s", k)
			}
		}
		if h == nil {
			http.NotFound(w, q)
			return
		}
		h(w, q)
	}))
	t.Cleanup(r.srv.Close)
	return r
}

func (r *fakeRegistry) handle(path string, h http.HandlerFunc) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.paths[path] = h
}

func (r *fakeRegistry) got(path string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, h := range r.hits {
		if h == path {
			return true
		}
	}
	return false
}

// publish serves name@version with integrity and tarball URL as given, and
// the tarball bytes at the registry's own tarball path.
func (r *fakeRegistry) publish(name, version, integrity, tarball string, body []byte) {
	esc := strings.Replace(name, "/", "%2f", 1)
	base := name[strings.LastIndex(name, "/")+1:]
	tpath := "/" + name + "/-/" + base + "-" + version + ".tgz"
	if tarball == "" {
		tarball = r.srv.URL + tpath
	}
	doc, _ := json.Marshal(map[string]any{"name": name, "version": version, "dist": map[string]string{"integrity": integrity, "tarball": tarball}})
	r.handle("/"+esc+"/"+version, func(w http.ResponseWriter, _ *http.Request) { w.Write(doc) })
	r.handle(tpath, func(w http.ResponseWriter, _ *http.Request) { w.Write(body) })
}

func shelfFillCfg(o ociCLI, mbit int) config {
	return config{VM: "oci", StateDir: o.stateDir, Image: "img", Shelf: shelfCfg{MaxGB: 1, Fill: "seed", FillMbit: mbit, KeepS: shelfKeepDefault}}
}

func want(t *testing.T, name, version string, body []byte) shelfWant {
	t.Helper()
	w, ok := parseShelfWant("npm " + name + " " + version + " " + sri512(body))
	if !ok {
		t.Fatalf("want %s %s does not parse", name, version)
	}
	return w
}

// nothingStored fails unless the shelf, its staging and its fetch directory
// hold no blob.
func nothingStored(t *testing.T, o ociCLI, vol string) {
	t.Helper()
	if b := blobsOn(t, vol); len(b) > 0 {
		t.Errorf("on the shelf: %v", b)
	}
	for _, d := range []string{shelfStaging(o.stateDir), shelfFetching(o.stateDir)} {
		_ = filepath.WalkDir(d, func(p string, e os.DirEntry, err error) error {
			if err == nil && !e.IsDir() {
				t.Errorf("left behind: %s", p)
			}
			return nil
		})
	}
}

func useRegistry(t *testing.T, r *fakeRegistry) {
	old := shelfRegistry
	shelfRegistry = r.srv.URL
	t.Cleanup(func() { shelfRegistry = old })
}

func TestShelfFillGood(t *testing.T) {
	o, _, vol := newShelfOCI(t)
	r := newFakeRegistry(t)
	useRegistry(t, r)
	a, b := []byte("tarball a"), []byte("tarball b")
	r.publish("left-pad", "1.3.0", sri512(a), "", a)
	r.publish("@scope/pkg", "2.0.0-beta.1", "sha1-xxxx "+sri512(b), "", b)
	ws := []shelfWant{want(t, "left-pad", "1.3.0", a), want(t, "@scope/pkg", "2.0.0-beta.1", b), want(t, "left-pad", "1.3.0", a)}
	res, err := shelfFill(context.Background(), shelfFillCfg(o, 1000), o, ws)
	if err != nil {
		t.Fatal(err)
	}
	if res.Wanted != 2 || res.Fetched != 2 || res.Published.Published != 2 || len(res.Dropped) != 0 {
		t.Fatalf("fill: %+v", res)
	}
	if !r.got("/@scope%2fpkg/2.0.0-beta.1") {
		t.Errorf("the scoped name was not asked for as @scope%%2fpkg: %v", r.hits)
	}
	on := blobsOn(t, vol)
	if len(on) != 2 {
		t.Fatalf("on the shelf: %v", on)
	}
	for _, body := range [][]byte{a, b} {
		p := filepath.Join(vol, filepath.FromSlash(blobPath(sum512(body))))
		if got, err := os.ReadFile(p); err != nil || !bytes.Equal(got, body) {
			t.Errorf("%s: %q %v", p, got, err)
		}
	}
	if s := res.String(); strings.Contains(s, "left-pad") || strings.Contains(s, "scope") {
		t.Errorf("the summary names a package: %s", s)
	}

	// Again: both are present, nothing is fetched.
	r.hits = nil
	res, err = shelfFill(context.Background(), shelfFillCfg(o, 1000), o, ws)
	if err != nil || res.Present != 2 || res.Fetched != 0 || len(r.hits) != 0 {
		t.Fatalf("second fill: %+v %v, requests %v", res, err, r.hits)
	}
}

func TestShelfFillDrops(t *testing.T) {
	good := []byte("the registry's tarball")
	other := []byte("someone else's bytes")
	sha1sum := sha1.Sum(good)
	cases := []struct {
		name   string
		reason string
		setup  func(r *fakeRegistry, elsewhere *fakeRegistry)
		check  func(t *testing.T, r *fakeRegistry, elsewhere *fakeRegistry)
	}{
		{"tarball bytes differ", dropMismatch, func(r, _ *fakeRegistry) {
			r.publish("pkg", "1.0.0", sri512(good), "", other)
		}, nil},
		{"registry hash differs from the want", dropIntegrity, func(r, _ *fakeRegistry) {
			r.publish("pkg", "1.0.0", sri512(other), "", good)
		}, func(t *testing.T, r, _ *fakeRegistry) {
			if r.got("/pkg/-/pkg-1.0.0.tgz") {
				t.Error("fetched a tarball whose registry hash is not the want's")
			}
		}},
		{"registry has only sha1", dropIntegrity, func(r, _ *fakeRegistry) {
			r.publish("pkg", "1.0.0", "sha1-"+base64.StdEncoding.EncodeToString(sha1sum[:]), "", good)
		}, nil},
		{"version document redirects", dropRedirect, func(r, elsewhere *fakeRegistry) {
			elsewhere.publish("pkg", "1.0.0", sri512(good), "", good)
			r.handle("/pkg/1.0.0", func(w http.ResponseWriter, q *http.Request) {
				http.Redirect(w, q, elsewhere.srv.URL+"/pkg/1.0.0", http.StatusFound)
			})
		}, func(t *testing.T, _, elsewhere *fakeRegistry) {
			if len(elsewhere.hits) > 0 {
				t.Errorf("followed a redirect: %v", elsewhere.hits)
			}
		}},
		{"tarball redirects", dropRedirect, func(r, elsewhere *fakeRegistry) {
			r.publish("pkg", "1.0.0", sri512(good), "", good)
			elsewhere.publish("pkg", "1.0.0", sri512(good), "", good)
			r.handle("/pkg/-/pkg-1.0.0.tgz", func(w http.ResponseWriter, q *http.Request) {
				http.Redirect(w, q, elsewhere.srv.URL+"/pkg/-/pkg-1.0.0.tgz", http.StatusMovedPermanently)
			})
		}, func(t *testing.T, _, elsewhere *fakeRegistry) {
			if len(elsewhere.hits) > 0 {
				t.Errorf("followed a redirect: %v", elsewhere.hits)
			}
		}},
		{"401", dropStatus, func(r, _ *fakeRegistry) {
			r.handle("/pkg/1.0.0", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusUnauthorized) })
		}, nil},
		{"404", dropStatus, func(*fakeRegistry, *fakeRegistry) {}, nil},
		{"tarball 401", dropStatus, func(r, _ *fakeRegistry) {
			r.publish("pkg", "1.0.0", sri512(good), "", good)
			r.handle("/pkg/-/pkg-1.0.0.tgz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusUnauthorized) })
		}, nil},
		{"tarball on another host", dropHost, func(r, elsewhere *fakeRegistry) {
			elsewhere.publish("pkg", "1.0.0", sri512(good), "", good)
			r.publish("pkg", "1.0.0", sri512(good), elsewhere.srv.URL+"/pkg/-/pkg-1.0.0.tgz", good)
		}, func(t *testing.T, _, elsewhere *fakeRegistry) {
			if len(elsewhere.hits) > 0 {
				t.Errorf("fetched from the host the document named: %v", elsewhere.hits)
			}
		}},
		{"tarball on another scheme", dropHost, func(r, _ *fakeRegistry) {
			r.publish("pkg", "1.0.0", sri512(good), strings.Replace(r.srv.URL, "http:", "https:", 1)+"/pkg/-/pkg-1.0.0.tgz", good)
		}, nil},
		{"tarball URL with user info", dropHost, func(r, _ *fakeRegistry) {
			r.publish("pkg", "1.0.0", sri512(good), strings.Replace(r.srv.URL, "http://", "http://u:p@", 1)+"/pkg/-/pkg-1.0.0.tgz", good)
		}, nil},
		{"document names another package", dropDoc, func(r, _ *fakeRegistry) {
			doc, _ := json.Marshal(map[string]any{"name": "evil", "version": "1.0.0", "dist": map[string]string{"integrity": sri512(good), "tarball": r.srv.URL + "/pkg/-/pkg-1.0.0.tgz"}})
			r.handle("/pkg/1.0.0", func(w http.ResponseWriter, _ *http.Request) { w.Write(doc) })
			r.handle("/pkg/-/pkg-1.0.0.tgz", func(w http.ResponseWriter, _ *http.Request) { w.Write(good) })
		}, nil},
		{"oversize, with a length", dropOversize, func(r, _ *fakeRegistry) {
			r.publish("pkg", "1.0.0", sri512(good), "", good)
		}, nil},
		{"oversize, streamed", dropOversize, func(r, _ *fakeRegistry) {
			r.publish("pkg", "1.0.0", sri512(good), "", good)
			r.handle("/pkg/-/pkg-1.0.0.tgz", func(w http.ResponseWriter, _ *http.Request) {
				for i := 0; i < len(good); i++ { // flushed byte by byte: no Content-Length
					w.Write(good[i : i+1])
					w.(http.Flusher).Flush()
				}
			})
		}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			o, _, vol := newShelfOCI(t)
			r, elsewhere := newFakeRegistry(t), newFakeRegistry(t)
			useRegistry(t, r)
			if strings.HasPrefix(c.name, "oversize") {
				old := shelfFetchMax
				shelfFetchMax = int64(len(good)) - 1
				t.Cleanup(func() { shelfFetchMax = old })
			}
			c.setup(r, elsewhere)
			res, err := shelfFill(context.Background(), shelfFillCfg(o, 1000), o, []shelfWant{want(t, "pkg", "1.0.0", good)})
			if err != nil {
				t.Fatal(err)
			}
			if res.Fetched != 0 || res.Dropped[c.reason] != 1 || len(res.Dropped) != 1 {
				t.Fatalf("want one drop for %s: %+v", c.reason, res)
			}
			if res.Published != (shelfPublished{}) {
				t.Errorf("a publish ran: %+v", res.Published)
			}
			if strings.Contains(res.String(), "pkg") {
				t.Errorf("the summary names the package: %s", res)
			}
			nothingStored(t, o, vol)
			if c.check != nil {
				c.check(t, r, elsewhere)
			}
		})
	}
}

func TestShelfFillOff(t *testing.T) {
	o, _, vol := newShelfOCI(t)
	r := newFakeRegistry(t)
	useRegistry(t, r)
	body := []byte("x")
	r.publish("pkg", "1.0.0", sri512(body), "", body)
	for _, cfg := range []func(*config){
		func(c *config) { c.Shelf.MaxGB = 0 },
		func(c *config) { c.Shelf.Fill = "off" },
	} {
		c := shelfFillCfg(o, 1000)
		cfg(&c)
		res, err := shelfFill(context.Background(), c, o, []shelfWant{want(t, "pkg", "1.0.0", body)})
		if err != nil || res.Skipped == "" || res.Fetched != 0 {
			t.Fatalf("%+v %v", res, err)
		}
	}
	if len(r.hits) > 0 {
		t.Errorf("asked the registry: %v", r.hits)
	}
	nothingStored(t, o, vol)
}

func TestShelfFillRate(t *testing.T) {
	o, _, vol := newShelfOCI(t)
	r := newFakeRegistry(t)
	useRegistry(t, r)
	body := bytes.Repeat([]byte("0123456789abcdef"), 200<<10/16) // 200 KiB at 1 Mbit/s (125,000 B/s)
	r.publish("big", "1.0.0", sri512(body), "", body)
	start := time.Now()
	res, err := shelfFill(context.Background(), shelfFillCfg(o, 1), o, []shelfWant{want(t, "big", "1.0.0", body)})
	if err != nil || res.Fetched != 1 {
		t.Fatalf("%+v %v", res, err)
	}
	// Less the one-read burst: (204800 - 32768) / 125000 s.
	if el := time.Since(start); el < 1300*time.Millisecond {
		t.Errorf("200 KiB at 1 Mbit/s took %s", el)
	}
	if len(blobsOn(t, vol)) != 1 {
		t.Error("the paced tarball did not reach the shelf")
	}
}

func TestParseShelfWant(t *testing.T) {
	h := sri512([]byte("x"))
	good := []string{
		"npm left-pad 1.3.0 " + h,
		"npm @types/node 20.11.5 " + h,
		"npm JSONStream 1.3.5 " + h,
		"npm pkg 1.0.0-rc.1+build.5 " + h,
	}
	for _, l := range good {
		w, ok := parseShelfWant(l)
		if !ok {
			t.Errorf("refused %q", l)
			continue
		}
		if w.line() != l {
			t.Errorf("%q came back as %q", l, w.line())
		}
	}
	bad := []string{
		"",
		"npm left-pad 1.3.0",
		"npm left-pad 1.3.0 " + h + " extra",
		"npm  left-pad 1.3.0 " + h,
		"go left-pad 1.3.0 " + h,
		"npm ../etc 1.0.0 " + h,
		"npm .hidden 1.0.0 " + h,
		"npm _under 1.0.0 " + h,
		"npm a/b 1.0.0 " + h,
		"npm @s/a/b 1.0.0 " + h,
		"npm a%2fb 1.0.0 " + h,
		"npm a?b 1.0.0 " + h,
		"npm a#b 1.0.0 " + h,
		"npm left-pad 1.3 " + h,
		"npm left-pad 01.3.0 " + h,
		"npm left-pad 1.3.0/../x " + h,
		"npm left-pad 1.3.0 sha1-AAAA",
		"npm left-pad 1.3.0 sha512-" + strings.Repeat("A", 86),
		"npm left-pad 1.3.0 " + h[:len(h)-3] + "B==", // bits past the 64 bytes
		"npm left-pad 1.3.0 " + strings.Replace(h, "sha512-", "sha512-", 1) + "?x",
		"npm " + strings.Repeat("a", 215) + " 1.0.0 " + h,
	}
	for _, l := range bad {
		if _, ok := parseShelfWant(l); ok {
			t.Errorf("took %q", l)
		}
	}
}

func TestNpmLockWants(t *testing.T) {
	a, b, c := sri512([]byte("a")), sri512([]byte("b")), sri512([]byte("c"))
	s1 := sha1.Sum([]byte("d"))
	lock := `{"lockfileVersion": 3, "packages": {
	  "": {"name": "root", "dependencies": {"left-pad": "^1.3.0"}, "devDependencies": {"x": "1"}},
	  "node_modules/str": "not an object",
	  "packages/ws": {"version": "1.0.0"},
	  "node_modules/left-pad": {"version": "1.3.0", "dependencies": {"ms": "^2"}, "resolved": "https://registry.npmjs.org/left-pad/-/left-pad-1.3.0.tgz", "integrity": "` + a + `"},
	  "node_modules/x/node_modules/left-pad": {"version": "1.3.0", "resolved": "https://registry.npmjs.org/left-pad/-/left-pad-1.3.0.tgz", "integrity": "` + a + `"},
	  "node_modules/@types/node": {"resolved": "https://registry.npmjs.org/@types%2fnode/-/node-20.0.0.tgz", "integrity": "sha1-x ` + b + `"},
	  "node_modules/alias": {"resolved": "https://registry.npmjs.org/real/-/real-2.0.0.tgz", "integrity": "` + c + `"},
	  "node_modules/other": {"resolved": "https://npm.example.com/other/-/other-1.0.0.tgz", "integrity": "` + c + `"},
	  "node_modules/git": {"resolved": "git+ssh://git@github.com/a/b.git#abc", "integrity": "` + c + `"},
	  "node_modules/old": {"resolved": "https://registry.npmjs.org/old/-/old-1.0.0.tgz", "integrity": "sha1-` + base64.StdEncoding.EncodeToString(s1[:]) + `"},
	  "node_modules/linked": {"resolved": "packages/ws", "link": true},
	  "node_modules/bad": {"resolved": "https://registry.npmjs.org/bad/-/other-1.0.0.tgz", "integrity": "` + c + `"}
	}}`
	ws, err := npmLockWants(strings.NewReader(lock))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, w := range ws {
		got[w.line()] = true
	}
	expect := []string{"npm left-pad 1.3.0 " + a, "npm @types/node 20.0.0 " + b, "npm real 2.0.0 " + c}
	if len(ws) != len(expect) {
		t.Errorf("wants %v", got)
	}
	for _, e := range expect {
		if !got[e] {
			t.Errorf("missing %q in %v", e, got)
		}
	}

	v1 := `{"lockfileVersion": 1, "dependencies": {"a": {"resolved": "https://registry.npmjs.org/a/-/a-1.0.0.tgz", "integrity": "` + a + `",
	  "dependencies": {"b": {"resolved": "https://registry.npmjs.org/b/-/b-2.0.0.tgz", "integrity": "` + b + `"}}}}}`
	ws, err = npmLockWants(strings.NewReader(v1))
	if err != nil || len(ws) != 2 {
		t.Errorf("v1: %v %v", ws, err)
	}
	if _, err := npmLockWants(strings.NewReader("not json")); err == nil {
		t.Error("took a file that is not JSON")
	}
}

// TestShelfHostCmd runs the driver's host shelf fill end to end with a
// lockfile and the binding's config on stdin.
func TestShelfHostCmd(t *testing.T) {
	o, dir, vol := newShelfOCI(t)
	r := newFakeRegistry(t)
	useRegistry(t, r)
	body := []byte("tarball")
	r.publish("left-pad", "1.3.0", sri512(body), "", body)
	lock := filepath.Join(t.TempDir(), "package-lock.json")
	if err := os.WriteFile(lock, []byte(`{"packages": {"node_modules/left-pad": {"resolved": "https://registry.npmjs.org/left-pad/-/left-pad-1.3.0.tgz", "integrity": "`+sri512(body)+`"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = dir
	req, _ := json.Marshal(map[string]any{"config": map[string]string{
		"target": "b", "kind": "box", "vm": "oci", "image": "img@sha256:" + strings.Repeat("a", 64), "images": "img@sha256:" + strings.Repeat("a", 64),
		"size": "c1m2", "cpu": "1", "memory": "2048", "state_dir": o.stateDir, "oci_cli": o.bin, "oci_runtime": o.runtime, "shelf_max_gb": "1",
	}})
	var out, errs bytes.Buffer
	code := hostCmd([]string{"shelf", "fill", "--npm-lock", lock}, bytes.NewReader(req), &out, &errs)
	if code != 0 {
		t.Fatalf("exit %d: %s %s", code, out.String(), errs.String())
	}
	if !strings.Contains(out.String(), "1 fetched") || strings.Contains(out.String()+errs.String(), "left-pad") {
		t.Errorf("out %q err %q", out.String(), errs.String())
	}
	if len(blobsOn(t, vol)) != 1 {
		t.Error("the blob is not on the shelf")
	}
	if code := hostCmd([]string{"shelf", "fill"}, bytes.NewReader(req), &out, &errs); code != 2 {
		t.Errorf("no --npm-lock: exit %d", code)
	}
}

// A fill stages no tarball past shelf_max_gb (shelf_evict.go has the rest
// of the limits).
func TestShelfFillCap(t *testing.T) {
	o, _, vol := newShelfOCI(t)
	r := newFakeRegistry(t)
	useRegistry(t, r)
	old := shelfGiB
	shelfGiB = 20 // shelf_max_gb 1 is 20 bytes here
	oldHelper := shelfHelper
	shelfHelper = nil // the helper's bytes count too
	t.Cleanup(func() { shelfGiB, shelfHelper = old, oldHelper })
	a, b := []byte("twelve bytes"), []byte("more bytes!!")
	r.publish("a", "1.0.0", sri512(a), "", a)
	r.publish("b", "1.0.0", sri512(b), "", b)
	res, err := shelfFill(context.Background(), shelfFillCfg(o, 1000), o, []shelfWant{want(t, "a", "1.0.0", a), want(t, "b", "1.0.0", b)})
	if err != nil || res.Fetched != 1 || res.Dropped[dropFull] != 1 {
		t.Fatalf("%+v %v", res, err)
	}
	if len(blobsOn(t, vol)) != 1 {
		t.Errorf("want one blob on the shelf: %+v", res)
	}
}

// A deeply nested v1 lockfile parses in one pass.
func TestNpmLockWantsDeep(t *testing.T) {
	a := sri512([]byte("a"))
	const depth = 4000 // two JSON levels each; Go's decoder stops at 10000
	var sb strings.Builder
	sb.WriteString(`{"dependencies": `)
	for i := 0; i < depth; i++ {
		sb.WriteString(`{"d": {"resolved": "https://registry.npmjs.org/a/-/a-1.0.0.tgz", "integrity": "` + a + `", "dependencies": `)
	}
	sb.WriteString(`{}`)
	for i := 0; i < depth; i++ {
		sb.WriteString(`}}`)
	}
	sb.WriteString(`}`)
	start := time.Now()
	ws, err := npmLockWants(strings.NewReader(sb.String()))
	if err != nil || len(ws) != 1 {
		t.Fatalf("%v %v", ws, err)
	}
	if el := time.Since(start); el > 5*time.Second {
		t.Errorf("took %s", el)
	}
}

// A lockfile with a wrong-typed value is read around it, as the helper does.
func TestNpmLockWantsLenient(t *testing.T) {
	a := sri512([]byte("a"))
	for lock, n := range map[string]int{
		`{"packages": []}`: 0,
		`{"packages": "x", "dependencies": {"a": {"resolved": "https://registry.npmjs.org/a/-/a-1.0.0.tgz", "integrity": "` + a + `"}}}`:             1,
		`{"dependencies": {"x": 1, "a": {"resolved": "https://registry.npmjs.org/a/-/a-1.0.0.tgz", "integrity": "` + a + `", "dependencies": "y"}}}`: 1,
		`[]`: 0,
	} {
		ws, err := npmLockWants(strings.NewReader(lock))
		if err != nil || len(ws) != n {
			t.Errorf("%s: %v %v", lock, ws, err)
		}
	}
}

// A want naming the wrong package for a hash does not keep the right one
// from filling it.
func TestShelfFillOtherName(t *testing.T) {
	o, _, vol := newShelfOCI(t)
	r := newFakeRegistry(t)
	useRegistry(t, r)
	body := []byte("lodash")
	r.publish("lodash", "4.17.21", sri512(body), "", body)
	ws := []shelfWant{want(t, "bogus", "1.0.0", body), want(t, "lodash", "4.17.21", body)}
	res, err := shelfFill(context.Background(), shelfFillCfg(o, 1000), o, ws)
	if err != nil || res.Wanted != 1 || res.Fetched != 1 || len(res.Dropped) != 0 {
		t.Fatalf("%+v %v", res, err)
	}
	if len(blobsOn(t, vol)) != 1 {
		t.Error("not on the shelf")
	}
}
