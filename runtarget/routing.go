package runtarget

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/kit/api"
)

// "Code, not callers, decides where a run goes" (SPEC 20): zm resolves a
// ref by a live lookup on the binding's remote, never a local tracking ref.

const lsRemoteLimit = 20 * time.Second

var (
	refName = regexp.MustCompile(`^[A-Za-z0-9._/-]{1,200}$`)
	sha     = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)
)

// lsRemote answers ref's commit on url: a branch first (isHead), then a tag
// (peeled). "" with no error means the remote has no such ref.
func lsRemote(ctx context.Context, url, ref string) (commit string, isHead bool, err error) {
	if !refName.MatchString(ref) || strings.HasPrefix(ref, "-") || strings.Contains(ref, "..") {
		return "", false, api.Errorf(api.CodeBadRequest, "ref %q is not a branch or tag name", ref)
	}
	ctx, cancel := context.WithTimeout(ctx, lsRemoteLimit)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "-c", "core.fsmonitor=false", "ls-remote", "--", url,
		"refs/heads/"+ref, "refs/tags/"+ref, "refs/tags/"+ref+"^{}")
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=/bin/false", "SSH_ASKPASS=/bin/false")
	cmd.Dir = os.TempDir()
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return "", false, api.Errorf(api.CodeUnavailable, "git ls-remote on the binding's repo failed: %v", err)
	}
	var head, tag, peeled string
	for _, line := range strings.Split(out.String(), "\n") {
		id, name, ok := strings.Cut(strings.TrimSpace(line), "\t")
		if !ok || !sha.MatchString(id) {
			continue
		}
		switch name {
		case "refs/heads/" + ref:
			head = id
		case "refs/tags/" + ref:
			tag = id
		case "refs/tags/" + ref + "^{}":
			peeled = id
		}
	}
	switch {
	case head != "":
		return head, true, nil
	case peeled != "":
		return peeled, false, nil
	}
	return tag, false, nil
}

// stricter is the class zm uses: the binding's or describe's, whichever is
// less safe (none over ephemeral, credentials over none).
func stricter(t computeapi.RunTarget, d *computeapi.RunTargetDescribe) computeapi.RunTargetClass {
	c := computeapi.RunTargetClass{Isolation: t.Isolation, Credentials: t.Credentials}
	if d != nil {
		if d.Isolation != computeapi.RunTargetIsolationEphemeral {
			c.Isolation = computeapi.RunTargetIsolationNone
		}
		c.Credentials = c.Credentials || d.Credentials
	}
	return c
}

// route applies SPEC 20's code rule and answers the commit to send.
func (s *Service) route(ctx context.Context, b computeapi.RunTarget, eff computeapi.RunTargetClass, req computeapi.AcquireRunTargetRequest) (string, error) {
	if b.Kind == computeapi.RunTargetKindBox {
		// A box is untrusted-capable by its bind rule: any pushed commit, or none.
		if req.Ref == "" {
			if req.Commit != "" {
				return "", api.Errorf(api.CodeBadRequest, "commit without ref")
			}
			return "", nil
		}
		commit, _, err := lsRemote(ctx, b.Repo, req.Ref)
		if err != nil {
			return "", err
		}
		if commit == "" {
			return "", api.Errorf(api.CodeBadRequest, "ref %s is not on the binding's remote (push it first)", req.Ref)
		}
		if req.Commit != "" && req.Commit != commit {
			return "", api.Errorf(api.CodeForbidden, "ref %s is at %s on the remote, not %s", req.Ref, commit, req.Commit)
		}
		return commit, nil
	}
	if b.Kind == computeapi.RunTargetKindDB && req.Ref == "" {
		if req.Commit != "" {
			return "", api.Errorf(api.CodeBadRequest, "commit without ref")
		}
		if !eff.UntrustedCapable() && !b.AllowAnyRef {
			return "", api.Errorf(api.CodeForbidden, "a db acquire without a ref counts as untrusted, and %s", eff.CannotIsolate("run target "+b.Name))
		}
		return "", nil
	}
	if req.Ref == "" {
		return "", api.Errorf(api.CodeBadRequest, "ref is required: a pushed branch or tag")
	}
	commit, isHead, err := lsRemote(ctx, b.Repo, req.Ref)
	if err != nil {
		return "", err
	}
	if commit == "" {
		if b.Kind == computeapi.RunTargetKindDB {
			if !eff.UntrustedCapable() && !b.AllowAnyRef {
				return "", api.Errorf(api.CodeForbidden, "ref %s does not resolve on the remote, so it counts as untrusted, and %s", req.Ref, eff.CannotIsolate("run target "+b.Name))
			}
			return "", nil
		}
		return "", api.Errorf(api.CodeBadRequest, "ref %s is not on the binding's remote (push it first)", req.Ref)
	}
	if req.Commit != "" && req.Commit != commit {
		return "", api.Errorf(api.CodeForbidden, "ref %s is at %s on the remote, not %s", req.Ref, commit, req.Commit)
	}
	trusted := false
	if req.Ref == b.Branch {
		trusted = isHead // a tag named like the branch is not the branch
	} else {
		head, headIsBranch, err := lsRemote(ctx, b.Repo, b.Branch)
		if err != nil {
			return "", err
		}
		trusted = headIsBranch && head == commit
	}
	if !trusted && !eff.UntrustedCapable() && !b.AllowAnyRef {
		return "", api.Errorf(api.CodeForbidden, "%s is not the head of %s, so it counts as untrusted, and %s", commit[:12], b.Branch, eff.CannotIsolate("run target "+b.Name))
	}
	if b.Kind == computeapi.RunTargetKindRun && (eff.Guarded() || len(b.Scripts) > 0) && !slices.Contains(b.Scripts, req.Script) {
		return "", api.Errorf(api.CodeForbidden, "script %s is not in run target %s's scripts", req.Script, b.Name)
	}
	return commit, nil
}
