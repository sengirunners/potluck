package boxpush_test

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

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

// refBox is a box on the reference driver, bound without a repo (as every
// remote box is), and its holder's client.
func refBox(t *testing.T) (*client.Client, string, string) {
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
	return c, "box", h.Handle[strings.LastIndex(h.Handle, "/")+1:]
}

// repo is a checkout with three commits and a file of random bytes, so its
// bundle spans several small chunks.
func repo(t *testing.T, random int) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@x", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@x", "GIT_CONFIG_GLOBAL=/dev/null")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q")
	for i, v := range []string{"one", "two", "three"} {
		write(t, filepath.Join(dir, "v.txt"), v)
		if i == 2 {
			b := make([]byte, random)
			rand.Read(b)
			write(t, filepath.Join(dir, "data", "blob.bin"), string(b))
		}
		run("add", "-A")
		run("commit", "-qm", v)
	}
	return dir
}

func write(t *testing.T, path, s string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}

func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return strings.TrimSpace(string(out))
}

func boxSh(t *testing.T, c *client.Client, target, id, script string) computeapi.RunTargetExec {
	t.Helper()
	x, err := computeclient.ExecRunTarget(context.Background(), c, target, id, computeapi.ExecRunTargetRequest{Argv: []string{"sh", "-c", script}})
	if err != nil {
		t.Fatal(err)
	}
	return x
}

// A bundle round trip on the reference driver: one commit in many chunks,
// checked out in the box at the renter's commit (shallow), then the renter's
// uncommitted changes as a second commit on top; no remote and no token in
// the box. A push cut off midway resumes, sending only what is missing.
func TestPushRoundTrip(t *testing.T) {
	ctx := context.Background()
	c, target, id := refBox(t)
	dir := repo(t, 64<<10)
	head := gitOut(t, dir, "rev-parse", "HEAD")
	b, err := boxpush.Make(ctx, boxpush.MakeOptions{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if b.Commit != head || b.Head != head || b.Shallow != head || b.Size < 64<<10 {
		t.Fatalf("bundle %+v", b)
	}
	if out, err := exec.Command("git", "bundle", "list-heads", b.Path).Output(); err != nil || !strings.HasPrefix(string(out), head+" "+boxpush.PushRef) {
		t.Fatalf("not a git bundle: %s %v", out, err)
	}

	// Cut off after three chunks: the push fails cleanly, saying it resumes.
	var calls atomic.Int32
	cut := func(ctx context.Context, req computeapi.ExecRunTargetRequest) (computeapi.RunTargetExec, error) {
		if req.Encoding == computeapi.EncodingBase64 && calls.Add(1) > 3 {
			return computeapi.RunTargetExec{}, api.Errorf(api.CodeUnavailable, "the relay dropped")
		}
		return computeclient.ExecRunTarget(ctx, c, target, id, req)
	}
	o := boxpush.PushOptions{ChunkSize: 8 << 10, Parallel: 1, Retries: 2}
	if _, err := boxpush.Push(ctx, cut, b, o); err == nil || !strings.Contains(err.Error(), "3 of") || !strings.Contains(err.Error(), "resumes") {
		t.Fatalf("a dropped push: %v", err)
	}
	var sent atomic.Int32
	count := func(ctx context.Context, req computeapi.ExecRunTargetRequest) (computeapi.RunTargetExec, error) {
		if req.Encoding == computeapi.EncodingBase64 {
			sent.Add(1)
		}
		return computeclient.ExecRunTarget(ctx, c, target, id, req)
	}
	start := time.Now()
	res, err := boxpush.Push(ctx, count, b, o)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%d bytes in %d chunks (%d resent) in %v", b.Size, res.Chunks, res.Sent, time.Since(start))
	if res.Chunks < 8 || res.Skipped != 3 || res.Sent != res.Chunks-3 || int(sent.Load()) != res.Sent || res.Commit != head {
		t.Fatalf("resume: %+v (sent %d)", res, sent.Load())
	}
	x := boxSh(t, c, target, id, `cd checkout && git rev-parse HEAD && cat v.txt && git rev-parse --is-shallow-repository && git remote | wc -l | tr -d ' ' && git log --oneline | wc -l | tr -d ' ' && git hash-object data/blob.bin && ls "$HOME/.potluck/push" | wc -l | tr -d ' '`)
	want := head + "\nthreetrue\n0\n1\n" + gitOut(t, dir, "hash-object", "data/blob.bin") + "\n0\n"
	if x.Exit != 0 || x.Stdout != want {
		t.Fatalf("the box's checkout: %+v, want %q", x, want)
	}
	if !strings.HasSuffix(res.Dest, "/checkout") {
		t.Errorf("dest %q", res.Dest)
	}

	// Uncommitted changes: a second commit on the renter's with the changes
	// to tracked files only (an untracked .env stays home), nothing of the
	// renter's checkout touched.
	write(t, filepath.Join(dir, "v.txt"), "four (not committed)")
	write(t, filepath.Join(dir, ".env"), "API_TOKEN=hunter2")
	write(t, filepath.Join(dir, "sub", "new.txt"), "untracked")
	write(t, filepath.Join(dir, ".gitignore"), "ignored.txt\n")
	write(t, filepath.Join(dir, "ignored.txt"), "ignored")
	status := gitOut(t, dir, "status", "--porcelain")
	res, err = boxpush.Into(ctx, c, "box:"+target+"/"+id, boxpush.MakeOptions{Dir: filepath.Join(dir, "data"), Dirty: true}, boxpush.PushOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Commit == head || res.Repo != gitOut(t, dir, "rev-parse", "--show-toplevel") || gitOut(t, dir, "rev-parse", "HEAD") != head || gitOut(t, dir, "status", "--porcelain") != status {
		t.Fatalf("dirty push %+v; the renter's checkout changed", res)
	}
	x = boxSh(t, c, target, id, `cd checkout && git rev-parse HEAD HEAD^ && cat v.txt && echo && ls -A | sort | tr '\n' ' '`)
	if want := res.Commit + "\n" + head + "\nfour (not committed)\n.git data v.txt "; x.Exit != 0 || x.Stdout != want {
		t.Fatalf("the dirty checkout (tracked only): %+v, want %q", x, want)
	}
	// Untracked files only when asked, each listed; ignored ones never.
	b2, err := boxpush.Make(ctx, boxpush.MakeOptions{Dir: dir, Dirty: true, Untracked: true})
	if err != nil {
		t.Fatal(err)
	}
	defer b2.Close()
	if strings.Join(b2.Untracked, " ") != ".env .gitignore sub/new.txt" {
		t.Fatalf("untracked listed %q", b2.Untracked)
	}
	if res, err = boxpush.Push(ctx, boxpush.Exec(c, target, id), b2, boxpush.PushOptions{}); err != nil {
		t.Fatal(err)
	}
	x = boxSh(t, c, target, id, `cd checkout && cat .env sub/new.txt && test ! -e ignored.txt && echo && git status --porcelain | wc -l | tr -d ' '`)
	if want := "API_TOKEN=hunter2untracked\n0\n"; x.Exit != 0 || x.Stdout != want {
		t.Fatalf("the dirty checkout with untracked files: %+v, want %q", x, want)
	}
	if _, err := boxpush.Make(ctx, boxpush.MakeOptions{Dir: dir, Ref: "HEAD^", Dirty: true}); err == nil {
		t.Error("--dirty on another ref than HEAD was taken")
	}
}

// The box refuses a bundle whose bytes do not match its SHA-256, even when
// each chunk matches the checksum it came with: nothing is checked out and
// the chunks are dropped. A chunk damaged on the way is refused in the box
// and sent again.
func TestPushChecksumRefusal(t *testing.T) {
	ctx := context.Background()
	c, target, id := refBox(t)
	dir := repo(t, 20<<10)
	b, err := boxpush.Make(ctx, boxpush.MakeOptions{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	o := boxpush.PushOptions{ChunkSize: 4 << 10, Parallel: 2}

	// A middleman rewrites chunk 1 and the checksum beside it.
	forged := func(ctx context.Context, req computeapi.ExecRunTargetRequest) (computeapi.RunTargetExec, error) {
		if req.Encoding == computeapi.EncodingBase64 && req.Argv[5] == "1" {
			bad := []byte("not the renter's bytes")
			s := sha256.Sum256(bad)
			req.Stdin, req.Argv[6] = base64.StdEncoding.EncodeToString(bad), hex.EncodeToString(s[:])
		}
		return computeclient.ExecRunTarget(ctx, c, target, id, req)
	}
	_, err = boxpush.Push(ctx, forged, b, o)
	if err == nil || !strings.Contains(err.Error(), "refused the bundle (checksum)") || !strings.Contains(err.Error(), "nothing checked out") {
		t.Fatalf("a forged chunk: %v", err)
	}
	if x := boxSh(t, c, target, id, `ls checkout 2>/dev/null | wc -l | tr -d ' '; ls "$HOME/.potluck/push" | wc -l | tr -d ' '`); x.Stdout != "0\n0\n" {
		t.Fatalf("after a refused bundle: %+v", x)
	}

	// Damaged on the way (the checksum is the renter's): refused per chunk,
	// resent, and the push lands.
	var damaged atomic.Int32
	flaky := func(ctx context.Context, req computeapi.ExecRunTargetRequest) (computeapi.RunTargetExec, error) {
		if req.Encoding == computeapi.EncodingBase64 && req.Argv[5] == "2" && damaged.Add(1) == 1 {
			raw, _ := base64.StdEncoding.DecodeString(req.Stdin)
			raw[0] ^= 0xff
			req.Stdin = base64.StdEncoding.EncodeToString(raw)
		}
		return computeclient.ExecRunTarget(ctx, c, target, id, req)
	}
	res, err := boxpush.Push(ctx, flaky, b, o)
	if err != nil || damaged.Load() != 2 {
		t.Fatalf("a damaged chunk: %+v %v (sent chunk 2 %d times)", res, err, damaged.Load())
	}
	if x := boxSh(t, c, target, id, `cd checkout && git rev-parse HEAD`); strings.TrimSpace(x.Stdout) != b.Head {
		t.Fatalf("checkout: %+v", x)
	}

	// Always damaged: a clear refusal after the tries.
	always := func(ctx context.Context, req computeapi.ExecRunTargetRequest) (computeapi.RunTargetExec, error) {
		if req.Encoding == computeapi.EncodingBase64 {
			req.Stdin = base64.StdEncoding.EncodeToString([]byte("x"))
		}
		return computeclient.ExecRunTarget(ctx, c, target, id, req)
	}
	b2, err := boxpush.Make(ctx, boxpush.MakeOptions{Dir: dir, Full: true})
	if err != nil {
		t.Fatal(err)
	}
	defer b2.Close()
	if _, err := boxpush.Push(ctx, always, b2, o); err == nil || !strings.Contains(err.Error(), "(checksum)") {
		t.Fatalf("a chunk never intact: %v", err)
	}
}

// The finishing call's answer is lost after it checked out: sent again, it
// answers the checkout (the chunks are gone by then), and the push succeeds.
func TestPushFinishAnswerLost(t *testing.T) {
	ctx := context.Background()
	c, target, id := refBox(t)
	b, err := boxpush.Make(ctx, boxpush.MakeOptions{Dir: repo(t, 10<<10)})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	var finishes atomic.Int32
	lose := func(ctx context.Context, req computeapi.ExecRunTargetRequest) (computeapi.RunTargetExec, error) {
		x, err := computeclient.ExecRunTarget(ctx, c, target, id, req)
		if len(req.Argv) == 10 && finishes.Add(1) == 1 { // finish: key n sum dest shallow head
			return computeapi.RunTargetExec{}, api.Errorf(api.CodeUnavailable, "the relay dropped the answer")
		}
		return x, err
	}
	res, err := boxpush.Push(ctx, lose, b, boxpush.PushOptions{ChunkSize: 4 << 10})
	if err != nil || finishes.Load() != 2 || !strings.HasSuffix(res.Dest, "/checkout") {
		t.Fatalf("a lost finish answer: %+v %v (finish sent %d times)", res, err, finishes.Load())
	}
	if x := boxSh(t, c, target, id, `cd checkout && git rev-parse HEAD`); strings.TrimSpace(x.Stdout) != b.Head {
		t.Fatalf("checkout: %+v", x)
	}
}

// The size limit is checked on the renter's machine, before anything is sent.
func TestPushSizeLimit(t *testing.T) {
	dir := repo(t, 200<<10)
	_, err := boxpush.Make(context.Background(), boxpush.MakeOptions{Dir: dir, Max: 100 << 10})
	if !api.IsCode(err, api.CodeTooLarge) || !strings.Contains(err.Error(), "over 0.1 MiB") {
		t.Fatalf("over the limit: %v", err)
	}
	if _, err := boxpush.Make(context.Background(), boxpush.MakeOptions{Dir: t.TempDir()}); err == nil || !strings.Contains(err.Error(), "not in a git checkout") {
		t.Fatalf("outside a checkout: %v", err)
	}
}

// A .git already in the box's checkout directory (a dependency layer's, the
// image's) is never trusted: its hooks never run and its replace refs never
// change the tree, so the box holds exactly the pushed commit; a second push
// to the same directory fetches on top of the push's own history.
func TestPushNeverTrustsAPlantedGit(t *testing.T) {
	ctx := context.Background()
	c, target, id := refBox(t)
	dir := repo(t, 1<<10)
	head := gitOut(t, dir, "rev-parse", "HEAD")
	three := gitOut(t, dir, "rev-parse", "HEAD:v.txt")
	plant := `set -e; mkdir -p checkout; cd checkout; git init -q .; git config core.hooksPath .git/h; mkdir -p .git/h
printf '#!/bin/sh\necho hooked > "$HOME/hooked"\n' > .git/h/post-checkout; chmod +x .git/h/post-checkout
git update-ref refs/replace/` + three + ` "$(printf evil | git hash-object -w --stdin)"`
	if x := boxSh(t, c, target, id, plant); x.Exit != 0 {
		t.Fatalf("planting a .git: %+v", x)
	}
	b, err := boxpush.Make(ctx, boxpush.MakeOptions{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if _, err := boxpush.Push(ctx, boxpush.Exec(c, target, id), b, boxpush.PushOptions{}); err != nil {
		t.Fatal(err)
	}
	look := `cd checkout && git rev-parse HEAD && cat v.txt && echo && git config --get core.hooksPath; git for-each-ref refs/replace | wc -l | tr -d ' '; test -e "$HOME/hooked" && echo hooked; true`
	if x := boxSh(t, c, target, id, look); x.Exit != 0 || x.Stdout != head+"\nthree\n0\n" {
		t.Fatalf("the checkout after a planted .git: %+v, want %q", x, head+"\nthree\n0\n")
	}
	write(t, filepath.Join(dir, "v.txt"), "four")
	b2, err := boxpush.Make(ctx, boxpush.MakeOptions{Dir: dir, Dirty: true})
	if err != nil {
		t.Fatal(err)
	}
	defer b2.Close()
	res, err := boxpush.Push(ctx, boxpush.Exec(c, target, id), b2, boxpush.PushOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if x := boxSh(t, c, target, id, `cd checkout && git rev-parse HEAD HEAD^ && cat v.txt`); x.Exit != 0 || x.Stdout != res.Commit+"\n"+head+"\nfour" {
		t.Fatalf("a second push: %+v", x)
	}
}

// The first push into a directory that already holds a tree (another git's
// checkout, a dependency layer) leaves it exactly the commit's: a file the
// commit does not hold goes, node_modules and ignored files stay, even after
// a first finish that died before its clean. The .git that was there is
// moved aside, its commits kept. The same directory spelled another way is
// the same git directory.
func TestPushFirstIntoAUsedDirectory(t *testing.T) {
	ctx := context.Background()
	c, target, id := refBox(t)
	dir := repo(t, 1<<10)
	write(t, filepath.Join(dir, ".gitignore"), "*.log\n")
	cmd := exec.Command("git", "-c", "user.name=t", "-c", "user.email=t@x", "-c", "commit.gpgsign=false", "commit", "-qam", "x")
	cmd.Dir = dir
	_ = exec.Command("git", "-C", dir, "add", ".gitignore").Run()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	// A first finish that died after making its git directory, before its clean.
	prep := boxpush.ShSum + `set -e; mkdir -p checkout/node_modules/x checkout/old; cd checkout; git init -q .; echo stale > stale.txt; echo o > old/f; git init -q old; echo m > node_modules/x/f; echo l > keep.log
git -c user.name=t -c user.email=t@x -c commit.gpgsign=false commit -q --allow-empty -m mine; pwd -P | tr -d '\n' > "$HOME/p"; mkdir -p "$HOME/.potluck/push-git"; git init -q --bare "$HOME/.potluck/push-git/$(zmsum "$HOME/p")"`
	if x := boxSh(t, c, target, id, prep); x.Exit != 0 {
		t.Fatalf("prep: %+v", x)
	}
	b, err := boxpush.Make(ctx, boxpush.MakeOptions{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if _, err := boxpush.Push(ctx, boxpush.Exec(c, target, id), b, boxpush.PushOptions{}); err != nil {
		t.Fatal(err)
	}
	if x := boxSh(t, c, target, id, `cd checkout && ls -A | sort | tr '\n' ' ' && { git status --porcelain | grep -vc node_modules; true; }`); x.Exit != 0 || x.Stdout != ".git .gitignore data keep.log node_modules v.txt 0\n" {
		t.Fatalf("the first push into a used directory: %+v", x)
	}
	if x := boxSh(t, c, target, id, `git --git-dir="$(ls -d "$HOME"/.potluck/push-git/*.replaced.*)" log --format=%s`); x.Stdout != "mine\n" {
		t.Errorf("the .git that was there: %+v, want it moved aside with its commit", x)
	}
	if _, err := boxpush.Push(ctx, boxpush.Exec(c, target, id), b, boxpush.PushOptions{Dest: "./checkout/"}); err != nil {
		t.Fatal(err)
	}
	if x := boxSh(t, c, target, id, `ls "$HOME/.potluck/push-git" | grep -vc replaced; ls "$HOME/.zm" 2>/dev/null | wc -l | tr -d ' '`); x.Stdout != "1\n0\n" {
		t.Errorf("checkout and ./checkout/ have %q git directories (and nothing under the old .zm), want one", x.Stdout)
	}
}

// A box that was pushed into before the rename keeps its git directory under
// HOME/.zm/push-git: the next push uses it (history there, its clean done),
// neither making a second one nor cleaning the checkout again.
func TestPushKeepsAnOldGitDirectory(t *testing.T) {
	ctx := context.Background()
	c, target, id := refBox(t)
	dir := repo(t, 1<<10)
	prep := boxpush.ShSum + `set -e; mkdir -p checkout; cd checkout; echo mine > untracked.txt
pwd -P | tr -d '\n' > "$HOME/p"; g="$HOME/.zm/push-git/$(zmsum "$HOME/p")"; mkdir -p "$HOME/.zm/push-git"; git init -q --bare "$g"; : > "$g/zm-cleaned"`
	if x := boxSh(t, c, target, id, prep); x.Exit != 0 {
		t.Fatalf("prep: %+v", x)
	}
	b, err := boxpush.Make(ctx, boxpush.MakeOptions{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if _, err := boxpush.Push(ctx, boxpush.Exec(c, target, id), b, boxpush.PushOptions{}); err != nil {
		t.Fatal(err)
	}
	if x := boxSh(t, c, target, id, `cat checkout/untracked.txt; cd checkout && git rev-parse HEAD; ls "$HOME/.zm/push-git" | wc -l | tr -d ' '; ls "$HOME/.potluck/push-git" | wc -l | tr -d ' '`); x.Exit != 0 || x.Stdout != "mine\n"+b.Head+"\n1\n0\n" {
		t.Errorf("a push into a box with an old git directory: %+v", x)
	}
}

// A checkout that is HOME, holds it, or is in HOME/.potluck or HOME/.zm is refused before
// anything is checked out (the first push cleans its checkout).
func TestPushRefusesHomeAsTheCheckout(t *testing.T) {
	ctx := context.Background()
	c, target, id := refBox(t)
	b, err := boxpush.Make(ctx, boxpush.MakeOptions{Dir: repo(t, 1<<10)})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if x := boxSh(t, c, target, id, `echo mine > "$HOME/dot"; mkdir -p "$HOME/.zm/x" "$HOME/.potluck/x"`); x.Exit != 0 {
		t.Fatal(x)
	}
	home := strings.TrimSpace(boxSh(t, c, target, id, `cd "$HOME" && pwd -P`).Stdout)
	for _, dest := range []string{".", "/", home, ".zm", ".zm/x", ".potluck", ".potluck/x", filepath.Dir(home)} {
		if _, err := boxpush.Push(ctx, boxpush.Exec(c, target, id), b, boxpush.PushOptions{Dest: dest}); err == nil || !strings.Contains(err.Error(), "cannot be") {
			t.Errorf("dest %q: %v, want refused", dest, err)
		}
	}
	// HOME/.zm a link elsewhere: that place, and what holds it, are refused too.
	zm := strings.TrimSpace(boxSh(t, c, target, id, `set -e; mv "$HOME/.zm" "$HOME/zmreal"; ln -s "$HOME/zmreal" "$HOME/.zm"; cd "$HOME/zmreal" && pwd -P`).Stdout)
	for _, dest := range []string{zm, "zmreal/x"} {
		if _, err := boxpush.Push(ctx, boxpush.Exec(c, target, id), b, boxpush.PushOptions{Dest: dest}); err == nil || !strings.Contains(err.Error(), "cannot be") {
			t.Errorf("dest %q: %v, want refused", dest, err)
		}
	}
	if x := boxSh(t, c, target, id, `cat "$HOME/dot"; ls -A "$HOME" | grep -c '^v.txt$'; true`); x.Stdout != "mine\n0\n" {
		t.Errorf("HOME after refused pushes: %q", x.Stdout)
	}
}
