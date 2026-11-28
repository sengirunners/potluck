package boxcp_test

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sengirunners/potluck/boxcp"
	"github.com/sengirunners/potluck/boxpush"
	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/computeclient"
	"github.com/sengirunners/potluck/conformance/refdriver"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/server"
	"github.com/sengirunners/potluck/runtarget"
	"github.com/sengirunners/potluck/wiring"
)

func TestMain(m *testing.M) {
	if code, ok := refdriver.Main(); ok { // the reference driver runs as this binary
		os.Exit(code)
	}
	os.Exit(m.Run())
}

const pin = "ref-image@sha256:0000000000000000000000000000000000000000000000000000000000000001"

// refBox is a box on the reference driver and its holder's exec.
func refBox(t *testing.T) (boxpush.ExecFunc, func(script string) computeapi.RunTargetExec) {
	t.Helper()
	home, err := os.MkdirTemp("", "zm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	t.Setenv("HOME", home)
	cfg, err := config.LoadFrom(home)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Human, cfg.Machine = "judy", "mini"
	dm, err := server.New(context.Background(), cfg, nil, wiring.Compute)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { dm.Close() })
	fix, _ := filepath.EvalSymlinks(t.TempDir())
	state := filepath.Join(fix, "state")
	drv, err := refdriver.Install(fix, state, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { refdriver.KillAll(state) })
	for _, m := range dm.Modules() {
		if s, ok := m.(*runtarget.Service); ok {
			s.SetLookPath(func(string) (string, error) { return drv, nil })
		}
	}
	ctx := context.Background()
	c := client.WithHandler(dm.SocketHandler())
	c.Project, c.ProjectFrom = "acme", "flag"
	if _, err := c.CreateProject(ctx, api.NewProjectRequest{Slug: "acme"}); err != nil {
		t.Fatal(err)
	}
	if _, err := computeclient.BindRunTarget(ctx, c, computeapi.BindRunTargetRequest{Target: computeapi.RunTarget{Name: "box", Driver: "ref", Kind: "box", Isolation: "ephemeral", Images: []string{pin}}}); err != nil {
		t.Fatal(err)
	}
	h, err := computeclient.AcquireRunTarget(ctx, c, "box", computeapi.AcquireRunTargetRequest{RequestID: "p1", TTLS: 300})
	if err != nil {
		t.Fatal(err)
	}
	id := h.Handle[strings.LastIndex(h.Handle, "/")+1:]
	sh := func(script string) computeapi.RunTargetExec {
		t.Helper()
		x, err := computeclient.ExecRunTarget(ctx, c, "box", id, computeapi.ExecRunTargetRequest{Argv: []string{"sh", "-c", script}})
		if err != nil {
			t.Fatal(err)
		}
		return x
	}
	return boxpush.Exec(c, "box", id), sh
}

func write(t *testing.T, p string, b []byte, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, b, mode); err != nil {
		t.Fatal(err)
	}
}

func sum(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }

func random(n int) []byte { b := make([]byte, n); rand.Read(b); return b }

// A file up and down on the reference driver: every byte value, many
// chunks, into a directory by its name, back with its executable bit; a
// copy cut off resumes; the box keeps no chunk or snapshot after.
func TestFileRoundTrip(t *testing.T) {
	ctx := context.Background()
	exec, sh := refBox(t)
	local := t.TempDir()
	data := random(100 << 10)
	write(t, filepath.Join(local, "blob.bin"), data, 0o755)
	o := boxcp.Options{ChunkSize: 8 << 10, Parallel: 2, Retries: 2}

	var calls atomic.Int32
	cut := func(ctx context.Context, req computeapi.ExecRunTargetRequest) (computeapi.RunTargetExec, error) {
		if req.Encoding == computeapi.EncodingBase64 && calls.Add(1) > 3 {
			return computeapi.RunTargetExec{}, api.Errorf(api.CodeUnavailable, "the relay dropped")
		}
		return exec(ctx, req)
	}
	if _, err := boxcp.Upload(ctx, cut, filepath.Join(local, "blob.bin"), "in/", o); err == nil || !strings.Contains(err.Error(), "resumes") {
		t.Fatalf("a dropped copy: %v", err)
	}
	res, err := boxcp.Upload(ctx, exec, filepath.Join(local, "blob.bin"), "in/", o)
	if err != nil {
		t.Fatal(err)
	}
	if res.Kind != "file" || res.Chunks != 13 || res.Skipped != 3 || !strings.HasSuffix(res.To, "/in/blob.bin") || res.Bytes != int64(len(data)) {
		t.Fatalf("upload %+v", res)
	}
	x := sh(`f=in/blob.bin; (sha256sum <"$f" 2>/dev/null || shasum -a 256 <"$f") | cut -c1-64; test -x "$f" && echo x; ls -A "$HOME/.potluck/push" | wc -l | tr -d ' '`)
	if want := sum(data) + "\nx\n0\n"; x.Stdout != want {
		t.Fatalf("the box's copy: %+v, want %q", x, want)
	}
	// Onto an existing file: replaced.
	write(t, filepath.Join(local, "small.txt"), []byte("hello\n"), 0o644)
	if res, err = boxcp.Upload(ctx, exec, filepath.Join(local, "small.txt"), "in/blob.bin", o); err != nil || sh(`cat in/blob.bin`).Stdout != "hello\n" {
		t.Fatalf("over a file: %+v %v", res, err)
	}

	sh(`mkdir -p tools && printf '#!/bin/sh\n' >tools/run && chmod 755 tools/run`)
	if _, err := boxcp.Upload(ctx, exec, filepath.Join(local, "blob.bin"), "/"+strings.TrimPrefix(filepath.Dir(res.To), "/")+"/big.bin", o); err != nil {
		t.Fatal(err)
	}
	down := t.TempDir()
	got, err := boxcp.Download(ctx, exec, "in/big.bin", down+"/", o)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(down, "big.bin"))
	if got.Kind != "file" || got.Chunks != 13 || sum(b) != sum(data) || got.To != filepath.Join(down, "big.bin") {
		t.Fatalf("download %+v (%d bytes)", got, len(b))
	}
	if _, err := boxcp.Download(ctx, exec, "tools/run", filepath.Join(down, "run.sh"), o); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(filepath.Join(down, "run.sh")); err != nil || fi.Mode().Perm() != 0o755 {
		t.Fatalf("an executable came down as %v %v", fi, err)
	}
	if x := sh(`ls "${TMPDIR:-/tmp}" | grep -c '^zm-cp-' || true`); strings.TrimSpace(x.Stdout) != "0" {
		t.Fatalf("snapshots left in the box: %+v", x)
	}
	if _, err := boxcp.Download(ctx, exec, "no/such", down, o); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("a missing path: %v", err)
	}
	ents, _ := os.ReadDir(down)
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), ".zm-cp-") {
			t.Fatalf("a temporary file left locally: %s", e.Name())
		}
	}
}

// A directory up and down: nested files, an empty directory and modes
// cross; a symlink goes up as a link and is never written coming down (left
// out, listed); a directory already at the destination is never merged.
func TestDirRoundTrip(t *testing.T) {
	ctx := context.Background()
	exec, sh := refBox(t)
	src := filepath.Join(t.TempDir(), "proj")
	write(t, filepath.Join(src, "a.txt"), []byte("a"), 0o644)
	write(t, filepath.Join(src, "sub", "deep", "b.bin"), random(30<<10), 0o600)
	write(t, filepath.Join(src, "bin", "tool"), []byte("#!/bin/sh\n"), 0o755)
	if err := os.MkdirAll(filepath.Join(src, "empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/passwd", filepath.Join(src, "link")); err != nil {
		t.Fatal(err)
	}
	o := boxcp.Options{ChunkSize: 8 << 10}
	res, err := boxcp.Upload(ctx, exec, src, "work", o)
	if err != nil {
		t.Fatal(err)
	}
	if res.Kind != "dir" || res.Files != 3 || !strings.HasSuffix(res.To, "/work") {
		t.Fatalf("upload %+v", res)
	}
	x := sh(`cd work && cat a.txt && test -d empty && test -x bin/tool && test -L link && readlink link && wc -c <sub/deep/b.bin | tr -d ' '`)
	if want := "a/etc/passwd\n30720\n"; x.Stdout != want {
		t.Fatalf("the box's tree: %+v, want %q", x, want)
	}
	// Into an existing directory: by its name.
	if res, err = boxcp.Upload(ctx, exec, src, "work", o); err != nil || !strings.HasSuffix(res.To, "/work/proj") {
		t.Fatalf("into a directory: %+v %v", res, err)
	}

	down := t.TempDir()
	got, err := boxcp.Download(ctx, exec, "work", down, o)
	if err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(down, "work")
	if got.Kind != "dir" || got.To != dst || got.Files != 6 || len(got.Left) != 2 || !strings.Contains(got.Left[0], "symlink") {
		t.Fatalf("download %+v", got)
	}
	for _, f := range []string{"a.txt", "sub/deep/b.bin", "proj/a.txt", "empty"} {
		if _, err := os.Stat(filepath.Join(dst, f)); err != nil {
			t.Fatalf("%s did not come down: %v", f, err)
		}
	}
	if _, err := os.Lstat(filepath.Join(dst, "link")); !os.IsNotExist(err) {
		t.Fatalf("a symlink was written: %v", err)
	}
	if fi, _ := os.Stat(filepath.Join(dst, "bin", "tool")); fi == nil || fi.Mode().Perm() != 0o755 {
		t.Fatalf("modes: %v", fi)
	}
	if _, err := boxcp.Download(ctx, exec, "work", down, o); !api.IsCode(err, api.CodeConflict) {
		t.Fatalf("over an existing directory: %v", err)
	}
	// HOME itself holds the box's TMPDIR only when TMPDIR is under it; a
	// copy of / always does: refused, never a tar of its own snapshot.
	if _, err := boxcp.Download(ctx, exec, "/", filepath.Join(down, "root"), o); err == nil || !strings.Contains(err.Error(), "temporary directory") {
		t.Fatalf("a copy of /: %v", err)
	}
}

// forged answers the box's side of a download with archive, as a box
// whose holder lost it to an attacker could: the stage line, then pages.
func forged(archive []byte, chunk int) boxpush.ExecFunc {
	return func(_ context.Context, req computeapi.ExecRunTargetRequest) (computeapi.RunTargetExec, error) {
		script := req.Argv[2]
		switch {
		case strings.Contains(script, "zmsum"): // stage
			return computeapi.RunTargetExec{Stdout: fmt.Sprintf("dir %d %s 0\n", len(archive), sum(archive))}, nil
		case strings.HasPrefix(script, "dd "):
			i, _ := strconv.Atoi(req.Argv[6])
			lo, hi := min(i*chunk, len(archive)), min((i+1)*chunk, len(archive))
			return computeapi.RunTargetExec{Stdout: base64.StdEncoding.EncodeToString(archive[lo:hi]), Encoding: computeapi.EncodingBase64}, nil
		}
		return computeapi.RunTargetExec{}, nil // unstage
	}
}

type entry struct {
	name, link string
	typ        byte
	body       string
}

func tarOf(t *testing.T, es ...entry) []byte {
	t.Helper()
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	for _, e := range es {
		h := &tar.Header{Name: e.name, Typeflag: e.typ, Linkname: e.link, Mode: 0o4777, Size: int64(len(e.body))}
		if e.typ != tar.TypeReg {
			h.Size = 0
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(e.body)); err != nil && e.typ == tar.TypeReg {
			t.Fatal(err)
		}
	}
	tw.Close()
	return b.Bytes()
}

// TestDownloadRefusesTraversal: what comes down is data. A tar naming an
// absolute path or climbing out with ".." is refused whole, nothing written
// (not even its harmless entries); a symlink is never written, so a file
// "through" it lands inside the destination as a plain file; set-id bits
// never survive.
func TestDownloadRefusesTraversal(t *testing.T) {
	ctx := context.Background()
	o := boxcp.Options{ChunkSize: 1 << 10}
	for name, archive := range map[string][]byte{
		"dot-dot":       tarOf(t, entry{name: "ok.txt", typ: tar.TypeReg, body: "fine"}, entry{name: "../escape.txt", typ: tar.TypeReg, body: "pwned"}),
		"inner dot-dot": tarOf(t, entry{name: "a/../../escape.txt", typ: tar.TypeReg, body: "pwned"}),
		"absolute":      tarOf(t, entry{name: "/tmp/escape.txt", typ: tar.TypeReg, body: "pwned"}),
		"dir dot-dot":   tarOf(t, entry{name: "../evil/", typ: tar.TypeDir}),
		"link dot-dot":  tarOf(t, entry{name: "../l", typ: tar.TypeSymlink, link: "/etc"}),
	} {
		t.Run(name, func(t *testing.T) {
			parent := t.TempDir()
			dest := filepath.Join(parent, "out", "copy")
			_, err := boxcp.Download(ctx, forged(archive, o.ChunkSize), "x", dest, o)
			if !api.IsCode(err, api.CodeForbidden) || !strings.Contains(err.Error(), "nothing written") {
				t.Fatalf("taken: %v", err)
			}
			for _, p := range []string{dest, filepath.Join(parent, "out", "escape.txt"), filepath.Join(parent, "escape.txt"), filepath.Join(parent, "out", "evil"), filepath.Join(parent, "out", "l")} {
				if _, err := os.Lstat(p); !os.IsNotExist(err) {
					t.Fatalf("%s was written", p)
				}
			}
			if ents, _ := os.ReadDir(filepath.Join(parent, "out")); len(ents) != 0 {
				t.Fatalf("left behind: %v", ents)
			}
		})
	}

	// A symlink out, then a file through it: the link is left out, the file
	// is a plain file inside the destination; the target is untouched.
	outside := t.TempDir()
	dest := filepath.Join(t.TempDir(), "copy")
	archive := tarOf(t,
		entry{name: "./", typ: tar.TypeDir},
		entry{name: "./link", typ: tar.TypeSymlink, link: outside},
		entry{name: "./link/pwned.txt", typ: tar.TypeReg, body: "pwned"},
		entry{name: "./hard", typ: tar.TypeLink, link: "/etc/passwd"},
		entry{name: "./suid", typ: tar.TypeReg, body: "x"})
	res, err := boxcp.Download(ctx, forged(archive, o.ChunkSize), "x", dest, o)
	if err != nil {
		t.Fatal(err)
	}
	if ents, _ := os.ReadDir(outside); len(ents) != 0 {
		t.Fatalf("written outside through a symlink: %v", ents)
	}
	if fi, err := os.Lstat(filepath.Join(dest, "link")); err != nil || !fi.IsDir() {
		t.Fatalf("link: %v %v", fi, err)
	}
	if fi, _ := os.Stat(filepath.Join(dest, "suid")); fi == nil || fi.Mode()&(os.ModeSetuid|0o022) != 0 {
		t.Fatalf("modes: %v", fi)
	}
	if len(res.Left) != 2 || !strings.Contains(res.Left[0], "symlink") || !strings.Contains(res.Left[1], "hard link") {
		t.Fatalf("left out: %q", res.Left)
	}

	// Bytes that do not match the box's own checksum: refused, nothing written.
	bad := forged(archive, o.ChunkSize)
	lying := func(ctx context.Context, req computeapi.ExecRunTargetRequest) (computeapi.RunTargetExec, error) {
		x, err := bad(ctx, req)
		if strings.HasPrefix(req.Argv[2], "dd ") {
			raw, _ := base64.StdEncoding.DecodeString(x.Stdout)
			raw[0] ^= 0xff
			x.Stdout = base64.StdEncoding.EncodeToString(raw)
		}
		return x, err
	}
	dest2 := filepath.Join(t.TempDir(), "copy")
	if _, err := boxcp.Download(ctx, lying, "x", dest2, o); err == nil || !strings.Contains(err.Error(), "sha256") {
		t.Fatalf("damaged bytes: %v", err)
	}
	if _, err := os.Lstat(dest2); !os.IsNotExist(err) {
		t.Fatal("damaged bytes were written")
	}
}

// The limit: a file or tree over it is refused before anything is sent
// up, or by the box before anything comes down; the limit has a ceiling.
func TestSizeLimit(t *testing.T) {
	ctx := context.Background()
	var calls atomic.Int32
	none := func(context.Context, computeapi.ExecRunTargetRequest) (computeapi.RunTargetExec, error) {
		calls.Add(1)
		return computeapi.RunTargetExec{}, nil
	}
	dir := t.TempDir()
	write(t, filepath.Join(dir, "big"), random(200<<10), 0o644)
	o := boxcp.Options{Max: 100 << 10}
	for _, p := range []string{filepath.Join(dir, "big"), dir} {
		if _, err := boxcp.Upload(ctx, none, p, "x", o); !api.IsCode(err, api.CodeTooLarge) || !strings.Contains(err.Error(), "nothing was copied") {
			t.Fatalf("%s over the limit: %v", p, err)
		}
	}
	if _, err := boxcp.WriteFile(ctx, none, "x", random(200<<10), o); !api.IsCode(err, api.CodeTooLarge) {
		t.Fatalf("content over the limit: %v", err)
	}
	if calls.Load() != 0 {
		t.Fatalf("%d execs before the refusal", calls.Load())
	}
	if _, err := boxcp.Upload(ctx, none, dir, "x", boxcp.Options{Max: boxcp.HardMax + 1}); !api.IsCode(err, api.CodeBadRequest) {
		t.Fatalf("past the ceiling: %v", err)
	}

	exec, sh := refBox(t)
	sh(`head -c 204800 /dev/zero >big && mkdir -p tree && cp big tree/`)
	for _, p := range []string{"big", "tree"} {
		if _, err := boxcp.Download(ctx, exec, p, filepath.Join(t.TempDir(), "x"), o); !api.IsCode(err, api.CodeTooLarge) {
			t.Fatalf("%s over the limit down: %v", p, err)
		}
	}
	if x := sh(`ls "${TMPDIR:-/tmp}" | grep -c '^zm-cp-' || true`); strings.TrimSpace(x.Stdout) != "0" {
		t.Fatalf("snapshots left: %+v", x)
	}
}

// WriteFile and ReadPage, the MCP tools' halves: exactly the path named
// (a directory refused), pages from offsets, text cut at a whole character.
func TestWriteFileReadPage(t *testing.T) {
	ctx := context.Background()
	exec, sh := refBox(t)
	text := strings.Repeat("héllo wörld\n", 100)
	res, err := boxcp.WriteFile(ctx, exec, "notes/a.txt", []byte(text), boxcp.Options{})
	if err != nil || !strings.HasSuffix(res.To, "/notes/a.txt") {
		t.Fatalf("write %+v %v", res, err)
	}
	if _, err := boxcp.WriteFile(ctx, exec, "notes", []byte("x"), boxcp.Options{}); err == nil || !strings.Contains(err.Error(), "is a directory") {
		t.Fatalf("onto a directory: %v", err)
	}
	var got strings.Builder
	for off := int64(0); ; {
		pg, err := boxcp.ReadPage(ctx, exec, "notes/a.txt", off, 100)
		if err != nil {
			t.Fatal(err)
		}
		s, ok := pg.Text()
		if !ok || pg.Size != int64(len(text)) {
			t.Fatalf("page at %d: %+v", off, pg)
		}
		got.WriteString(s)
		if pg.EOF {
			break
		}
		off = pg.Next
	}
	if got.String() != text {
		t.Fatal("pages do not join to the file")
	}
	sh(`printf '\000\377' >bin`)
	pg, err := boxcp.ReadPage(ctx, exec, "bin", 0, 10)
	if _, ok := pg.Text(); err != nil || ok || !bytes.Equal(pg.Data, []byte{0, 0xff}) || !pg.EOF {
		t.Fatalf("binary page %+v %v", pg, err)
	}
	if _, err := boxcp.ReadPage(ctx, exec, "nope", 0, 10); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("missing: %v", err)
	}
}

func TestSplit(t *testing.T) {
	for in, want := range map[string]string{
		"box:linux/b3:/home/u/f": "box:linux/b3 /home/u/f true",
		"box:linux@lender/b3:":   "box:linux@lender/b3  true",
		"./box:linux/b3":         "  false",
		"rel/file":               "  false",
	} {
		h, p, box, err := boxcp.Split(in)
		if err != nil || fmt.Sprintf("%s %s %v", h, p, box) != want {
			t.Errorf("%q: %q %q %v %v", in, h, p, box, err)
		}
	}
	if _, _, _, err := boxcp.Split("box:linux/b3"); err == nil {
		t.Error("a handle with no path was taken")
	}
}

// sparseTar is a 3 KiB tar whose one entry, by GNU sparse 0.1 PAX
// records, reads back as size bytes of zeros (archive/tar honours them).
func sparseTar(t *testing.T, size int64) []byte {
	t.Helper()
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	if err := tw.WriteHeader(&tar.Header{Name: "big", Typeflag: tar.TypeReg, Size: 1, Mode: 0o644, Format: tar.FormatPAX,
		PAXRecords: map[string]string{"XXX.sparse.size": strconv.FormatInt(size, 10), "XXX.sparse.numblocks": "1", "XXX.sparse.map": "0,1"}}); err != nil {
		t.Fatal(err)
	}
	tw.Write([]byte("x"))
	tw.Close()
	return bytes.ReplaceAll(b.Bytes(), []byte("XXX.sparse."), []byte("GNU.sparse.")) // the writer will not write GNU.sparse itself
}

// A small tar that expands on unpacking (GNU sparse records) is refused,
// nothing written: a box cannot fill this disk past the copy's limit.
func TestDownloadRefusesSparse(t *testing.T) {
	o := boxcp.Options{ChunkSize: 1 << 10}
	archive := sparseTar(t, 200<<20)
	if len(archive) > 4<<10 {
		t.Fatalf("the forged tar is %d bytes", len(archive))
	}
	dest := filepath.Join(t.TempDir(), "copy")
	if _, err := boxcp.Download(context.Background(), forged(archive, o.ChunkSize), "x", dest, o); !api.IsCode(err, api.CodeForbidden) || !strings.Contains(err.Error(), "nothing written") {
		t.Fatalf("a sparse entry: %v", err)
	}
	if ents, _ := os.ReadDir(filepath.Dir(dest)); len(ents) != 0 {
		t.Fatalf("left behind: %v", ents)
	}
}

// The finishing exec's answer is lost after it wrote: sent again, it
// writes the same path (the destination is resolved once, before), never a
// second copy inside the first.
func TestUploadFinishAnswerLost(t *testing.T) {
	ctx := context.Background()
	exec, sh := refBox(t)
	src := filepath.Join(t.TempDir(), "data")
	write(t, filepath.Join(src, "a.txt"), []byte("a"), 0o644)
	var finishes atomic.Int32
	lose := func(ctx context.Context, req computeapi.ExecRunTargetRequest) (computeapi.RunTargetExec, error) {
		x, err := exec(ctx, req)
		if strings.Contains(req.Argv[2], "joined") && finishes.Add(1) == 1 {
			return computeapi.RunTargetExec{}, api.Errorf(api.CodeUnavailable, "the relay dropped the answer")
		}
		return x, err
	}
	res, err := boxcp.Upload(ctx, lose, src, "work/new", boxcp.Options{})
	if err != nil || finishes.Load() != 2 || !strings.HasSuffix(res.To, "/work/new") {
		t.Fatalf("a lost finish answer: %+v %v (sent %d times)", res, err, finishes.Load())
	}
	if x := sh(`cd work/new && ls -A | tr '\n' ' '`); x.Stdout != "a.txt " {
		t.Fatalf("the box's tree after a resent finish: %q", x.Stdout)
	}
}

// Two copies of the same bytes to different places keep their chunks
// apart (one's cleanup never takes the other's); the same copy again
// resumes from its own.
func TestUploadKeysPerDestination(t *testing.T) {
	ctx := context.Background()
	exec, _ := refBox(t)
	f := filepath.Join(t.TempDir(), "f")
	write(t, f, random(20<<10), 0o644)
	keys := map[string]bool{}
	rec := func(ctx context.Context, req computeapi.ExecRunTargetRequest) (computeapi.RunTargetExec, error) {
		if req.Encoding == computeapi.EncodingBase64 && len(req.Argv) > 4 && !strings.HasPrefix(req.Argv[2], "dd ") {
			keys[req.Argv[4]] = true
		}
		return exec(ctx, req)
	}
	o := boxcp.Options{ChunkSize: 8 << 10, Parallel: 1}
	for _, dest := range []string{"one", "two"} {
		if _, err := boxcp.Upload(ctx, rec, f, dest, o); err != nil {
			t.Fatal(err)
		}
	}
	if len(keys) != 2 {
		t.Fatalf("chunk keys %v", keys)
	}
}

// A refusal by the finishing exec drops the chunks it would have used; a
// directory holding this machine's temporary directory is refused before
// anything is sent (never a tar of its own tar).
func TestUploadRefusals(t *testing.T) {
	ctx := context.Background()
	exec, sh := refBox(t)
	f := filepath.Join(t.TempDir(), "f")
	write(t, f, random(20<<10), 0o644) // over a chunk: the chunked copy
	refuse := func(ctx context.Context, req computeapi.ExecRunTargetRequest) (computeapi.RunTargetExec, error) {
		if strings.Contains(req.Argv[2], "joined") {
			return computeapi.RunTargetExec{Exit: 64, Stderr: "refused"}, nil
		}
		return exec(ctx, req)
	}
	if _, err := boxcp.Upload(ctx, refuse, f, "x", boxcp.Options{ChunkSize: 8 << 10}); err == nil {
		t.Fatal("a refused finish was taken")
	}
	if x := sh(`ls -A "$HOME/.potluck/push" 2>/dev/null | wc -l | tr -d ' '`); x.Stdout != "0\n" {
		t.Fatalf("chunks kept after a refusal: %q", x.Stdout)
	}

	src := t.TempDir()
	if err := os.MkdirAll(filepath.Join(src, "tmp"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", filepath.Join(src, "tmp"))
	var calls atomic.Int32
	count := func(ctx context.Context, req computeapi.ExecRunTargetRequest) (computeapi.RunTargetExec, error) {
		calls.Add(1)
		return exec(ctx, req)
	}
	if _, err := boxcp.Upload(ctx, count, src, "y", boxcp.Options{}); err == nil || !strings.Contains(err.Error(), "temporary directory") || calls.Load() != 0 {
		t.Fatalf("a source holding the temporary directory: %v (%d execs)", err, calls.Load())
	}
}

// A file that fits one chunk goes in one exec: written where cp says, with
// its mode, over a file already there, refused onto a directory; bytes
// damaged on the way are sent again, never written; nothing is left for a
// resume. One byte over a chunk takes the chunked copy.
func TestUploadWholeFile(t *testing.T) {
	ctx := context.Background()
	exec, sh := refBox(t)
	var calls atomic.Int32
	count := func(ctx context.Context, req computeapi.ExecRunTargetRequest) (computeapi.RunTargetExec, error) {
		calls.Add(1)
		return exec(ctx, req)
	}
	o := boxcp.Options{ChunkSize: 8 << 10}
	local := t.TempDir()
	data := random(8 << 10)
	write(t, filepath.Join(local, "tool"), data, 0o755)
	res, err := boxcp.Upload(ctx, count, filepath.Join(local, "tool"), "bin/", o)
	if err != nil || calls.Load() != 1 || res.Chunks != 1 || res.Sent != 1 || !strings.HasSuffix(res.To, "/bin/tool") || res.Bytes != int64(len(data)) {
		t.Fatalf("one chunk's file: %+v %v (%d execs)", res, err, calls.Load())
	}
	x := sh(`f=bin/tool; (sha256sum <"$f" 2>/dev/null || shasum -a 256 <"$f") | cut -c1-64; test -x "$f" && echo x; ls -A bin | wc -l | tr -d ' '; ls -A "$HOME/.potluck/push" 2>/dev/null | wc -l | tr -d ' '`)
	if want := sum(data) + "\nx\n1\n0\n"; x.Stdout != want {
		t.Fatalf("the box's copy: %+v, want %q", x, want)
	}
	calls.Store(0)
	if res, err = boxcp.WriteFile(ctx, count, "bin/tool", []byte("again\n"), o); err != nil || calls.Load() != 1 || sh(`cat bin/tool`).Stdout != "again\n" {
		t.Fatalf("over a file: %+v %v (%d execs)", res, err, calls.Load())
	}
	if _, err := boxcp.WriteFile(ctx, exec, "bin", []byte("x"), o); err == nil || !strings.Contains(err.Error(), "is a directory") {
		t.Fatalf("onto a directory: %v", err)
	}

	var damaged atomic.Int32
	damage := func(ctx context.Context, req computeapi.ExecRunTargetRequest) (computeapi.RunTargetExec, error) {
		if req.Encoding == computeapi.EncodingBase64 && damaged.Add(1) == 1 {
			req.Stdin = base64.StdEncoding.EncodeToString([]byte("not the bytes"))
		}
		return exec(ctx, req)
	}
	if _, err := boxcp.WriteFile(ctx, damage, "notes/n.txt", []byte("the bytes\n"), o); err != nil || damaged.Load() != 2 || sh(`cat notes/n.txt`).Stdout != "the bytes\n" {
		t.Fatalf("damaged once: %v (%d sends)", err, damaged.Load())
	}
	always := func(ctx context.Context, req computeapi.ExecRunTargetRequest) (computeapi.RunTargetExec, error) {
		req.Stdin = base64.StdEncoding.EncodeToString([]byte("not the bytes"))
		return exec(ctx, req)
	}
	if _, err := boxcp.WriteFile(ctx, always, "notes/m.txt", []byte("x"), boxcp.Options{Retries: 2}); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("always damaged: %v", err)
	}
	if x := sh(`ls -A notes | tr '\n' ' '`); x.Stdout != "n.txt " {
		t.Fatalf("the box's notes after a damaged write: %q", x.Stdout)
	}

	// The answer lost after it wrote: sent again, the same path, one file,
	// into an existing directory and at a new name alike.
	for dest, want := range map[string]string{"bin/": "/bin/once", "fresh": "/fresh"} {
		var sends atomic.Int32
		lose := func(ctx context.Context, req computeapi.ExecRunTargetRequest) (computeapi.RunTargetExec, error) {
			x, err := exec(ctx, req)
			if sends.Add(1) == 1 {
				return computeapi.RunTargetExec{}, api.Errorf(api.CodeUnavailable, "the relay dropped the answer")
			}
			return x, err
		}
		write(t, filepath.Join(local, "once"), []byte("once\n"), 0o644)
		res, err := boxcp.Upload(ctx, lose, filepath.Join(local, "once"), dest, o)
		if err != nil || sends.Load() != 2 || !strings.HasSuffix(res.To, want) {
			t.Fatalf("%s, its answer lost: %+v %v (%d sends)", dest, res, err, sends.Load())
		}
		if x := sh(`cat "$HOME` + want + `"; ls -A "$(dirname "$HOME` + want + `")" | grep -c potluck-cp || true`); x.Stdout != "once\n0\n" {
			t.Fatalf("%s after a resend: %q", dest, x.Stdout)
		}
	}
	if x := sh(`ls -A bin | tr '\n' ' '`); x.Stdout != "once tool " {
		t.Fatalf("bin after the resends: %q", x.Stdout)
	}

	calls.Store(0)
	write(t, filepath.Join(local, "big"), random(8<<10+1), 0o644)
	if res, err := boxcp.Upload(ctx, count, filepath.Join(local, "big"), "big", o); err != nil || res.Chunks != 2 || calls.Load() < 4 {
		t.Fatalf("one byte over a chunk: %+v %v (%d execs)", res, err, calls.Load())
	}
}
