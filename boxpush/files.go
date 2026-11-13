package boxpush

// Questions about a checkout's files that a caller sending something built
// from them (a dependency layer) asks before the push.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
)

// Prefix is dir's path below the top of its git checkout ("" at the top, else
// "a/b", no slashes at the ends). Not in a checkout: "" and no error, since a
// directory outside one has no place in it.
func Prefix(ctx context.Context, dir string) string {
	out, err := git(ctx, dir, nil, nil, "rev-parse", "--show-prefix")
	if err != nil {
		return ""
	}
	return strings.Trim(line(out), "/")
}

// Mismatch lists the files (paths relative to dir, plain names) the push of
// ref would not carry as dir holds them. Without dirty that is each file whose
// content is not the commit's, or that the commit lacks (untracked and
// ignored files included). With dirty, tracked files' changes ride along in
// the second commit, and untracked ones when untracked is set (never an
// ignored one), so only a file neither carried nor in the commit is listed. An error: dir is no checkout or
// ref no commit, so nothing can be said.
func Mismatch(ctx context.Context, dir, ref string, dirty, untracked bool, paths []string) ([]string, error) {
	if ref == "" {
		ref = "HEAD"
	}
	env := []string{"GIT_LITERAL_PATHSPECS=1"}
	c, err := git(ctx, dir, env, nil, "rev-parse", "--verify", "--end-of-options", ref+"^{commit}")
	if err != nil {
		return nil, err
	}
	commit := line(c)
	var out []string
	for _, p := range paths {
		if dirty {
			if _, err := git(ctx, dir, env, nil, "ls-files", "--error-unmatch", "--", p); err == nil {
				continue // tracked: its changes are sent
			}
			if untracked {
				// Sent unless git ignores it (an ignored file is never sent).
				if out, err := git(ctx, dir, env, nil, "ls-files", "--others", "--exclude-standard", "--", p); err == nil && line(out) == p {
					continue
				}
			}
		}
		blob, err := git(ctx, dir, env, nil, "rev-parse", "--verify", "--end-of-options", commit+":./"+p)
		if err != nil {
			out = append(out, p) // the commit lacks it
			continue
		}
		if dirty {
			continue // in the commit, so carried
		}
		h, err := workHash(ctx, dir, env, p)
		if err != nil || line(h) != line(blob) {
			out = append(out, p)
		}
	}
	return out, nil
}

// workHash is the blob id git would give p as dir holds it: for a symlink,
// the blob of its target path, as a commit holds it.
func workHash(ctx context.Context, dir string, env []string, p string) ([]byte, error) {
	if target, err := os.Readlink(filepath.Join(dir, p)); err == nil {
		return git(ctx, dir, env, strings.NewReader(target), "hash-object", "--stdin")
	}
	return git(ctx, dir, env, nil, "hash-object", "--", p)
}
