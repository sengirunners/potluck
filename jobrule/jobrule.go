// Package jobrule is the one copy of the rules for a GitHub Actions job that
// may hold a secret: which events, which ref, which workflow files, and that
// the run's code is the repository's own. The grants check and the issuers
// both use it. It is a leaf: standard library only, no other Potluck package.
//
// A job is described by GitHub's own answers about its run (the run's
// repository, head repository, workflow path, head branch, event, id), never
// by what the box or the workflow file says. A field left empty refuses.
package jobrule

import (
	"errors"
	"regexp"
	"slices"
	"strings"
)

// Job is a GitHub Actions job as GitHub answered it.
type Job struct {
	Repo          string `json:"repo"`           // owner/name of the run's repository
	HeadRepo      string `json:"head_repo"`      // owner/name of the run's head repository: must be Repo
	RepoID        int64  `json:"repo_id"`        // GitHub's id of Repo
	HeadRepoID    int64  `json:"head_repo_id"`   // GitHub's id of the head repository: must be RepoID
	Workflow      string `json:"workflow"`       // .github/workflows/<file>
	Ref           string `json:"ref"`            // refs/heads/<branch>
	DefaultBranch string `json:"default_branch"` // the repository's default branch
	Event         string `json:"event"`
	RunID         int64  `json:"run_id"`
}

// Events. Default serves a rule that names none. Dispatch events serve only a
// rule that lists them: their inputs are chosen by whoever dispatches, that
// is anyone with write access to the repository or a token that can dispatch.
// Every other event (pull_request*, workflow_run, issues, merge_group,
// release, ...) never gets a secret.
var (
	Default  = []string{"push", "schedule"}
	Dispatch = []string{"workflow_dispatch", "repository_dispatch"}
)

// Allowed reports whether ev can ever be served.
func Allowed(ev string) bool { return slices.Contains(Default, ev) || slices.Contains(Dispatch, ev) }

// RuleEvents checks a rule's events and answers them with the default filled
// in.
func RuleEvents(evs []string) ([]string, error) {
	if len(evs) == 0 {
		return slices.Clone(Default), nil
	}
	for _, ev := range evs {
		if !Allowed(ev) {
			return nil, errors.New("event " + ev + ": push, schedule, workflow_dispatch or repository_dispatch")
		}
	}
	return slices.Clone(evs), nil
}

var (
	nameRE     = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}$`)
	workflowRE = regexp.MustCompile(`^\.github/workflows/[A-Za-z0-9._-]{1,200}\.ya?ml$`)
)

// ValidRepo: owner/name, GitHub's characters, no "..".
func ValidRepo(repo string) bool {
	o, n, ok := strings.Cut(repo, "/")
	return ok && nameRE.MatchString(o) && nameRE.MatchString(n) && !strings.Contains(repo, "..")
}

// ValidWorkflow: a file under .github/workflows/, .yml or .yaml.
func ValidWorkflow(w string) bool { return workflowRE.MatchString(w) && !strings.Contains(w, "..") }

// Check answers why j may never hold a secret, or "". It does not look at a
// rule: Serves does.
func Check(j Job) string {
	switch {
	case !ValidRepo(j.Repo) || !ValidRepo(j.HeadRepo) || !strings.EqualFold(j.HeadRepo, j.Repo),
		j.RepoID <= 0 || j.HeadRepoID != j.RepoID:
		return "a run whose head repository is not the repository (a fork's, or unknown) gets nothing"
	case j.RunID <= 0:
		return "no run id"
	case strings.HasPrefix(j.Event, "pull_request"):
		return "a pull request's run gets nothing"
	case !Allowed(j.Event):
		return "a run of this event gets nothing"
	case j.DefaultBranch == "" || j.Ref != "refs/heads/"+j.DefaultBranch:
		return "only a run on the repository's default branch gets a secret"
	case !ValidWorkflow(j.Workflow):
		return "the job's workflow is not a file under .github/workflows/"
	}
	return ""
}

// Serves reports whether a rule with these events (already through
// RuleEvents) and workflows (empty: any) serves j.
func Serves(events, workflows []string, j Job) bool {
	if !slices.Contains(events, j.Event) {
		return false
	}
	return len(workflows) == 0 || slices.Contains(workflows, j.Workflow)
}
