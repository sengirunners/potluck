package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/sengirunners/potluck/agenttasks"
	"github.com/sengirunners/potluck/boxcp"
	"github.com/sengirunners/potluck/boxpush"
	"github.com/sengirunners/potluck/kit/client"
)

type taskChanges struct {
	ref     string
	commits int
	state   string
}

// bringBack copies the task's out directory from the box and fetches its
// bundle into AgentTaskRefs+<box id> in b's checkout, with hooks off and
// objects checked: answers the ref and how many commits it holds past the
// pushed commit ("" and 0 when the agent made none).
func bringBack(ctx context.Context, c *client.Client, bx acquiredBox, b *boxpush.Source) (taskChanges, error) {
	tmp, err := os.MkdirTemp("", "potluck-agent-task-*")
	if err != nil {
		return taskChanges{}, err
	}
	defer os.RemoveAll(tmp)
	local := filepath.Join(tmp, agenttasks.OutDir)
	if _, err := boxcp.Download(ctx, boxpush.Exec(c, bx.target, bx.id), agentTaskDir(bx.key)+"/"+agenttasks.OutDir, local, boxcp.Options{}); err != nil {
		return taskChanges{}, fmt.Errorf("copy the agent's commits back: %w", err)
	}
	statusPath := filepath.Join(local, agenttasks.Status)
	fi, err := os.Lstat(statusPath)
	if err != nil {
		return taskChanges{}, fmt.Errorf("read the task's completion status: %w", err)
	}
	if !fi.Mode().IsRegular() {
		return taskChanges{}, errors.New("the task's completion status is not a regular file")
	}
	f, err := os.Open(statusPath)
	if err != nil {
		return taskChanges{}, fmt.Errorf("read the task's completion status: %w", err)
	}
	status, err := io.ReadAll(io.LimitReader(f, 32))
	_ = f.Close()
	if err != nil {
		return taskChanges{}, fmt.Errorf("read the task's completion status: %w", err)
	}
	state := strings.TrimSpace(string(status))
	if state != "exited" && state != "timeout" {
		return taskChanges{}, fmt.Errorf("the task returned invalid completion status %q", state)
	}
	if _, err := os.Stat(filepath.Join(local, agenttasks.NoChanges)); err == nil {
		return taskChanges{state: state}, nil
	}
	// The bundle is the box's data: a regular file git checks as a bundle
	// before fetching it. A directory there would be fetched as a repository
	// the box built (its config read), so it is refused.
	bundle := filepath.Join(local, agenttasks.Bundle)
	if fi, err := os.Lstat(bundle); err != nil {
		return taskChanges{}, errors.New("the sandbox sent back no bundle of the agent's commits (its lines above say why)")
	} else if !fi.Mode().IsRegular() {
		return taskChanges{}, fmt.Errorf("the sandbox sent back a %s as the bundle of the agent's commits, not a file: refused", fi.Mode().Type())
	}
	ref := AgentTaskRefs + bx.id
	git := func(args ...string) (string, error) {
		cmd := exec.CommandContext(ctx, "git", append([]string{"-C", b.Repo, "-c", "core.hooksPath=/dev/null", "-c", "fetch.fsckObjects=true"}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			return "", fmt.Errorf("git %s: %v: %s", args[0], err, strings.TrimSpace(stderr.String()))
		}
		return strings.TrimSpace(string(out)), nil
	}
	if _, err := git("bundle", "verify", "--quiet", bundle); err != nil {
		return taskChanges{}, fmt.Errorf("the sandbox's bundle of the agent's commits is not one git takes: %w", err)
	}
	if _, err := git("fetch", "--quiet", "--no-tags", "--no-write-fetch-head", bundle, agenttasks.BundleRef+":"+ref); err != nil {
		return taskChanges{}, fmt.Errorf("fetch the agent's commits: %w", err)
	}
	n, err := git("rev-list", "--count", b.Head+".."+ref)
	if err != nil {
		return taskChanges{ref: ref}, err
	}
	count, err := strconv.Atoi(n)
	if err != nil {
		return taskChanges{ref: ref}, fmt.Errorf("count the commits at %s: git rev-list answered %q", ref, n)
	}
	return taskChanges{ref: ref, commits: count, state: state}, nil
}
