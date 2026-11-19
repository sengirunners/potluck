// Package pushbase reuses a holder's sealed repository snapshot when a
// host supports it. A base is an optimization; an unavailable base leaves
// the caller free to send the commit whole.
package pushbase

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os/exec"
	"slices"
	"strings"

	"github.com/sengirunners/potluck/boxpush"
)

// Repository identifies the history shared by a checkout and the commit
// to seed on a host. Key deliberately contains no remote URL or credential.
// Forks with the same roots share a key; the daemon separately scopes it to
// the holder and project.
type Repository struct {
	Key    string
	Commit string
}

// Identify uses the resolved source, not a moving HEAD. Shallow checkouts
// cannot identify their real roots and return no base. A remote default
// branch, when recorded locally, supplies a common ancestor to seed;
// otherwise the source commit is the base. This never fetches a remote.
func Identify(ctx context.Context, src *boxpush.Source) (*Repository, error) {
	git := func(args ...string) (string, error) {
		out, err := exec.CommandContext(ctx, "git", append([]string{"-C", src.Repo}, args...)...).Output()
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(string(out)), nil
	}
	shallow, err := git("rev-parse", "--is-shallow-repository")
	if err != nil {
		return nil, fmt.Errorf("check push base history: %w", err)
	}
	if shallow == "true" {
		return nil, nil
	}
	roots, err := git("rev-list", "--max-parents=0", src.Commit, "--")
	if err != nil {
		return nil, fmt.Errorf("identify push base roots: %w", err)
	}
	ids := strings.Fields(roots)
	if len(ids) == 0 {
		return nil, fmt.Errorf("push base commit has no history roots")
	}
	slices.Sort(ids)
	sum := sha256.Sum256([]byte(strings.Join(ids, "\n")))
	r := &Repository{Key: hex.EncodeToString(sum[:]), Commit: src.Commit}
	refs, err := git("for-each-ref", "--format=%(refname) %(symref)", "refs/remotes/")
	if err != nil {
		return nil, fmt.Errorf("find push base default branch: %w", err)
	}
	for _, line := range strings.Split(refs, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || !strings.HasSuffix(fields[0], "/HEAD") {
			continue
		}
		if base, err := git("merge-base", src.Commit, fields[1]); err == nil && base != "" {
			r.Commit = base
			break
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return r, nil
}
