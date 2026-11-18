// Package boxpush puts the renter's code into a box without a credential
// (SPEC 20 "Boxes", "Code from the renter"; zm-eakh.12.5.16). The renter's
// client makes a git bundle of one commit from its own checkout and sends it
// into the box in chunks through exec (base64 stdin), the one verb every box
// driver has, so it works on every layer and across machines within the
// relay's caps; the box joins the chunks, checks the whole bundle's SHA-256,
// fetches it and checks the commit out. Nothing but the bundle's bytes
// enters the box: no token, no remote, no key.
//
// Why exec and not a new verb: a 512 KiB chunk's request is about 700 KB of
// JSON, inside the 1 MiB body every door and the team proxy take, and each
// call answers in well under a second, inside a relayed call's limits and
// the tunnel's 100 s. A put verb would add a driver obligation for no
// capability exec lacks. Each chunk is checked against its own SHA-256 in
// the box before it is kept, so a resend after a dropped call is
// idempotent and a later push of the same bundle skips what arrived.
//
// The bundle is git's v2 format (v3 for SHA-256 repositories) holding only
// the commit and its tree (a shallow, single-commit bundle) unless Full: the
// box marks the commit shallow before fetching, as a clone with --depth 1
// would. Given the commits the box already holds (Have: an earlier push's,
// a base git directory's), the bundle carries the same commits less every
// object those of them that are its ancestors here hold in their trees, as
// a thin pack (a changed file as its delta from the base's copy), and names
// them as its prerequisites: a repeat push of a big checkout sends what
// changed since, not the whole tree. It never carries the history between. Uncommitted changes to tracked files (Dirty; untracked files only
// with Untracked, each listed) go as a second commit on top, written
// with a temporary index: the renter's refs, index and working tree are
// never touched (its object store gains the unreachable objects, which git
// prunes). Owned by sandboxes (zm-eakh.12.5).
package boxpush

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/sengirunners/potluck/kit/api"
)

// Limits.
const (
	// DefaultMax bounds a bundle unless the caller says otherwise.
	DefaultMax = 128 << 20
	// ChunkSize is one exec's stdin: its base64 and JSON stay under the
	// 1 MiB request body (api.MaxBody, the team proxy's forward limit).
	ChunkSize = 512 << 10
	// PushRef is the ref the bundle names and the box fetches.
	PushRef = "refs/zm/push"
	// DefaultDest is where the box checks out, relative to its HOME.
	DefaultDest = "checkout"
)

// MakeOptions say what to bundle.
type MakeOptions struct {
	Dir   string // the renter's checkout (default: the working directory)
	Ref   string // a commit-ish there (default HEAD)
	Dirty bool   // also the working tree's changes to tracked files, as a second commit (Ref must be HEAD)
	// Untracked (with Dirty) adds the files git neither tracks nor ignores
	// (an untracked .env among them): Bundle.Untracked lists each.
	Untracked bool
	Full      bool  // the whole history, not one commit
	Max       int64 // bundle size limit in bytes (default DefaultMax)
	// Have are commits the box holds (Have answers them). Those that are
	// here and ancestors of the commit sent become the bundle's base: it
	// carries only what the commit adds to them. Not with Full.
	Have []string
}

// Bundle is a bundle made on the renter's machine, in a temporary file.
type Bundle struct {
	Path    string // the file (Close removes it)
	Size    int64
	Sum     string // its SHA-256, hex
	Commit  string // the commit Ref named
	Head    string // the commit the box checks out: Commit, or the dirty commit on top
	Shallow string // the commits the box marks shallow, space-separated: Commit, or a full bundle's from a shallow checkout
	Repo    string // the checkout's top-level directory
	Objects int    // git objects in it
	// Base are the commits the bundle builds on (its prerequisites): the box
	// must hold them, or the push is refused before anything is checked out.
	Base []string
	// Untracked are the untracked files the dirty commit carries (Untracked).
	Untracked []string
}

// Close removes the bundle's file.
func (b *Bundle) Close() error { return os.RemoveAll(filepath.Dir(b.Path)) }

// git runs git in dir with a clean, explicit environment overlay.
func git(ctx context.Context, dir string, env []string, stdin io.Reader, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdin = stdin
	var errb bytes.Buffer
	cmd.Stderr = &errb
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = err.Error()
		}
		return out, fmt.Errorf("git %s: %s", args[0], msg)
	}
	return out, nil
}

func line(b []byte) string { return strings.TrimSpace(string(b)) }

// Make bundles Ref (and with Dirty the uncommitted changes) of the checkout
// at Dir into a temporary file, refusing one over Max before anything is
// sent.
func Make(ctx context.Context, o MakeOptions) (*Bundle, error) {
	if o.Ref == "" {
		o.Ref = "HEAD"
	}
	if o.Max <= 0 {
		o.Max = DefaultMax
	}
	if o.Dir == "" {
		o.Dir = "."
	}
	top, err := git(ctx, o.Dir, nil, nil, "rev-parse", "--show-toplevel")
	if err != nil {
		return nil, fmt.Errorf("not in a git checkout (%v): run this from your repository, or pass the ref of one", err)
	}
	b := &Bundle{Repo: line(top)}
	c, err := git(ctx, b.Repo, nil, nil, "rev-parse", "--verify", "--end-of-options", o.Ref+"^{commit}")
	if err != nil {
		return nil, fmt.Errorf("%q names no commit here: %v", o.Ref, err)
	}
	b.Commit, b.Head = line(c), line(c)
	if o.Dirty {
		if h, err := git(ctx, b.Repo, nil, nil, "rev-parse", "HEAD"); err != nil || line(h) != b.Commit {
			return nil, fmt.Errorf("uncommitted changes sit on HEAD: --dirty needs the ref to be HEAD, not %s", o.Ref)
		}
		if b.Head, b.Untracked, err = dirtyCommit(ctx, b.Repo, b.Commit, o.Untracked); err != nil {
			return nil, err
		}
	}
	format := "sha1"
	if f, err := git(ctx, b.Repo, nil, nil, "rev-parse", "--show-object-format"); err == nil && line(f) != "" {
		format = line(f)
	}
	// The objects: the commits and their trees only, or the whole history.
	walk := []string{"rev-list", "--objects", "--no-walk", b.Commit}
	if b.Head != b.Commit {
		walk = append(walk, b.Head)
	}
	if o.Full {
		walk = []string{"rev-list", "--objects", b.Head}
		// A shallow checkout's history stops where its own does.
		if p, err := git(ctx, b.Repo, nil, nil, "rev-parse", "--path-format=absolute", "--git-path", "shallow"); err == nil {
			if sh, err := os.ReadFile(line(p)); err == nil {
				b.Shallow = strings.Join(strings.Fields(string(sh)), " ")
			}
		}
	} else {
		b.Shallow = b.Commit
	}
	if !o.Full {
		if b.Base, err = usable(ctx, b.Repo, b.Head, o.Have); err != nil {
			return nil, err
		}
	}
	objs, err := git(ctx, b.Repo, nil, nil, walk...)
	if err != nil {
		return nil, err
	}
	// On a base: a thin pack of the same commits and trees, its walk cut at
	// Commit as git serves a shallow client, so its deltas may lean on the
	// base's objects but it never holds the history between: what the box
	// names as its base cannot draw out commits (or files deleted since)
	// that the push itself would not send.
	pack, in := []string{"pack-objects", "--stdout", "-q"}, objs
	if len(b.Base) > 0 {
		held, err := git(ctx, b.Repo, nil, nil, append([]string{"rev-list", "--objects", "--no-walk"}, b.Base...)...)
		if err != nil {
			return nil, err
		}
		objs = without(objs, held)
		pack = append(pack, "--revs", "--thin", "--shallow")
		in = []byte("--shallow " + b.Commit + "\n" + b.Head + "\n")
		for _, c := range b.Base {
			in = append(in, "^"+c+"\n"...)
		}
	}
	tmp, err := os.MkdirTemp("", "zm-push")
	if err != nil {
		return nil, err
	}
	b.Path = filepath.Join(tmp, "push.bundle")
	ok := false
	defer func() {
		if !ok {
			b.Close()
		}
	}()
	f, err := os.OpenFile(b.Path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	h := sha256.New()
	w := &limitWriter{w: io.MultiWriter(f, h), max: o.Max}
	head := "# v2 git bundle\n"
	if format != "sha1" {
		head = "# v3 git bundle\n@object-format=" + format + "\n"
	}
	for _, c := range b.Base {
		head += "-" + c + "\n"
	}
	fmt.Fprintf(w, "%s%s %s\n\n", head, b.Head, PushRef)
	cmd := exec.CommandContext(ctx, "git", pack...)
	cmd.Dir, cmd.Stdin, cmd.Stdout = b.Repo, bytes.NewReader(in), w
	var errb bytes.Buffer
	cmd.Stderr = &errb
	err = cmd.Run()
	if w.over {
		return nil, api.Errorf(api.CodeTooLarge, "the bundle of %s is over %s (the limit): commit fewer large files, or raise the limit", short(b.Head), mib(o.Max))
	}
	if w.err != nil { // pack-objects then dies of SIGPIPE, which says nothing
		return nil, fmt.Errorf("writing the bundle to %s: %w", b.Path, w.err)
	}
	if err != nil {
		return nil, fmt.Errorf("git pack-objects: %s", cmdErr(err, &errb))
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	b.Size, b.Sum, b.Objects = w.n, hex.EncodeToString(h.Sum(nil)), bytes.Count(objs, []byte("\n"))
	ok = true
	return b, nil
}

// without is the lines of list (rev-list --objects: "<id>[ <path>]") whose
// id no line of held has: what a bundle on a base holds, for its count.
func without(list, held []byte) []byte {
	have := map[string]bool{}
	for _, l := range strings.Split(string(held), "\n") {
		if f := strings.Fields(l); len(f) > 0 {
			have[f[0]] = true
		}
	}
	var out []byte
	for _, l := range strings.SplitAfter(string(list), "\n") {
		if f := strings.Fields(l); len(f) > 0 && !have[f[0]] {
			out = append(out, l...)
		}
	}
	return out
}

// Bounds on a box's answer: the ids looked at (each costs git a lookup
// here), and the commits kept as a base.
const (
	HaveMax = 256
	baseMax = 64
)

// usable are the commits that the ids of have (as the box answered them: a
// tag is peeled to its commit) name in this checkout and that are ancestors
// of head: a bundle built on them carries only head's history since. An id
// the checkout lacks, or that is not head's ancestor, is dropped; a git
// failure other than "no" is an error. Only the first HaveMax ids are read.
func usable(ctx context.Context, repo, head string, have []string) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	for _, id := range have[:min(len(have), HaveMax)] {
		if len(out) == baseMax {
			break
		}
		if !isID(id) {
			continue
		}
		p, err := git(ctx, repo, nil, nil, "rev-parse", "-q", "--verify", "--end-of-options", id+"^{commit}")
		c := line(p)
		if err != nil || seen[c] {
			continue // not here (a shallow checkout, another repository), or a commit already kept
		}
		seen[c] = true
		cmd := exec.CommandContext(ctx, "git", "merge-base", "--is-ancestor", c, head)
		cmd.Dir = repo
		err = cmd.Run()
		var ee *exec.ExitError
		switch {
		case err == nil:
			out = append(out, c)
		case errors.As(err, &ee) && ee.ExitCode() == 1: // not an ancestor
		default:
			return nil, fmt.Errorf("git merge-base: %v", err)
		}
	}
	return out, nil
}

// isID says whether s is a full object id: 40 (SHA-1) or 64 (SHA-256)
// lowercase hex digits.
func isID(s string) bool {
	return (len(s) == 40 || len(s) == 64) && strings.Trim(s, "0123456789abcdef") == ""
}

// dirtyCommit writes the working tree's changes to tracked files (and
// with untracked the files git neither tracks nor ignores, listed in the
// answer) as a commit on parent, through a copy of the index, and answers
// its id. Its dates are parent's, so the same changes make the same commit
// and a resent push resumes.
func dirtyCommit(ctx context.Context, repo, parent string, untracked bool) (string, []string, error) {
	tmp, err := os.MkdirTemp("", "zm-push-index")
	if err != nil {
		return "", nil, err
	}
	defer os.RemoveAll(tmp)
	idx := filepath.Join(tmp, "index")
	env := []string{"GIT_INDEX_FILE=" + idx}
	if p, err := git(ctx, repo, nil, nil, "rev-parse", "--path-format=absolute", "--git-path", "index"); err == nil {
		if b, err := os.ReadFile(line(p)); err == nil {
			_ = os.WriteFile(idx, b, 0o600) // keeps stat data: only changed files are hashed again
		}
	}
	if _, err := os.Stat(idx); err != nil {
		if _, err := git(ctx, repo, env, nil, "read-tree", parent); err != nil {
			return "", nil, err
		}
	}
	var added []string
	if untracked {
		out, err := git(ctx, repo, env, nil, "ls-files", "--others", "--exclude-standard", "-z", "--full-name", "--", ":/")
		if err != nil {
			return "", nil, err
		}
		for _, f := range strings.Split(string(out), "\x00") {
			if f != "" {
				added = append(added, f)
			}
		}
	}
	mode := "-u"
	if untracked {
		mode = "-A"
	}
	if _, err := git(ctx, repo, env, nil, "add", mode, "--", ":/"); err != nil {
		return "", nil, err
	}
	tree, err := git(ctx, repo, env, nil, "write-tree")
	if err != nil {
		return "", nil, err
	}
	when, err := git(ctx, repo, nil, nil, "show", "-s", "--format=%ct %cz", parent)
	if err != nil {
		return "", nil, err
	}
	id := []string{"GIT_AUTHOR_NAME=zm", "GIT_AUTHOR_EMAIL=zm@localhost", "GIT_COMMITTER_NAME=zm", "GIT_COMMITTER_EMAIL=zm@localhost",
		"GIT_AUTHOR_DATE=@" + line(when), "GIT_COMMITTER_DATE=@" + line(when)}
	c, err := git(ctx, repo, id, strings.NewReader("zm: uncommitted changes\n"), "commit-tree", line(tree), "-p", parent)
	if err != nil {
		return "", nil, err
	}
	return line(c), added, nil
}

type limitWriter struct {
	w    io.Writer
	n    int64
	max  int64
	over bool
	err  error
}

func (l *limitWriter) Write(p []byte) (int, error) {
	if l.n+int64(len(p)) > l.max {
		l.over = true
		return 0, fmt.Errorf("over the limit")
	}
	n, err := l.w.Write(p)
	l.n += int64(n)
	if err != nil {
		l.err = err
	}
	return n, err
}

func cmdErr(err error, stderr *bytes.Buffer) string {
	if s := strings.TrimSpace(stderr.String()); s != "" {
		return s
	}
	return err.Error()
}

func short(c string) string {
	if len(c) > 12 {
		return c[:12]
	}
	return c
}

func mib(n int64) string { return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20)) }
