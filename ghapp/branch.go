package ghapp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
)

// callsOwnOnly: every referenced workflow (GitHub's referenced_workflows:
// path owner/name/.github/workflows/<file>@<ref or sha>, and sha, the
// commit it ran at) is under repo's .github/workflows/ at sha head. One
// GitHub did not describe that way counts as another's.
func callsOwnOnly(refs []json.RawMessage, repo, head string) bool {
	for _, raw := range refs {
		var c struct {
			Path string `json:"path"`
			SHA  string `json:"sha"`
		}
		if json.Unmarshal(raw, &c) != nil || head == "" || c.SHA != head ||
			!strings.HasPrefix(strings.ToLower(c.Path), strings.ToLower(repo)+"/.github/workflows/") || strings.Contains(c.Path, "..") {
			return false
		}
	}
	return true
}

var (
	branchRE = regexp.MustCompile(`^[A-Za-z0-9._/-]{1,200}$`)
	shaRE    = regexp.MustCompile(`^[0-9a-f]{40}$`)
)

// OnBranch reports whether commit sha is on branch (its tip or one of its
// ancestors) and whether it is the tip, as GitHub compares them now (GET
// /repos/{o}/{r}/compare/{branch}...{sha}: "identical" is the tip, "behind"
// an ancestor). A head_branch alone cannot
// say a run is on a branch: a tag may carry the branch's name. It needs
// Contents read, asked for in a token of its own.
func (c *Client) OnBranch(ctx context.Context, installation int64, repo, branch, sha string) (on, tip bool, err error) {
	if err := validRepo(repo); err != nil {
		return false, false, err
	}
	if !branchRE.MatchString(branch) || strings.Contains(branch, "..") || !shaRE.MatchString(sha) {
		return false, false, fmt.Errorf("ghapp: compare needs a branch name and a full commit id")
	}
	var v struct {
		Status string `json:"status"`
	}
	path := "/repos/" + repo + "/compare/" + branch + "..." + sha
	if err := c.asInstall(ctx, installation, repo, Perms{"contents": "read", "metadata": "read"}, http.MethodGet, path, nil, &v, 200); err != nil {
		return false, false, err
	}
	switch v.Status {
	case "identical":
		return true, true, nil
	case "behind":
		return true, false, nil
	case "ahead", "diverged":
		return false, false, nil
	}
	return false, false, fmt.Errorf("github: compare answered no known status")
}

// TagExists reports whether repo has a tag named name (GET /repos/{o}/{r}/
// git/ref/tags/{name}: GitHub answers that exact ref or 404). A run's
// head_branch is a branch or a tag name and GitHub does not say which, so a
// run is on the default branch only when no tag carries that name. Contents
// read, in a token of its own.
func (c *Client) TagExists(ctx context.Context, installation int64, repo, name string) (bool, error) {
	if err := validRepo(repo); err != nil {
		return false, err
	}
	if !branchRE.MatchString(name) || strings.Contains(name, "..") {
		return false, fmt.Errorf("ghapp: not a tag name")
	}
	var v struct {
		Ref string `json:"ref"`
	}
	err := c.asInstall(ctx, installation, repo, Perms{"contents": "read", "metadata": "read"}, http.MethodGet, "/repos/"+repo+"/git/ref/tags/"+name, nil, &v, 200)
	switch {
	case NotFound(err):
		return false, nil
	case err != nil:
		return false, err
	}
	return true, nil
}

// Reviewed reports whether a change reaches branch of repo only through a
// pull request this App cannot skip, as GitHub's rulesets say now: an
// active rule of type pull_request asking for at least one approval, of the
// last push by someone else (require_last_push_approval), applies to the
// branch (GET
// /repos/{o}/{r}/rules/branches/{branch}) in a ruleset GitHub says this
// App can never bypass (GET /repos/{o}/{r}/rulesets/{id}:
// current_user_can_bypass "never", enforcement "active"). why says what is
// missing when it does not. Classic branch protection is not read (it needs
// Administration read, which the App is not given), so a branch guarded
// only that way is not reviewed here. Metadata read, in a token of its own.
func (c *Client) Reviewed(ctx context.Context, installation int64, repo, branch string) (ok bool, why string, err error) {
	if err := validRepo(repo); err != nil {
		return false, "", err
	}
	if !branchRE.MatchString(branch) || strings.Contains(branch, "..") {
		return false, "", fmt.Errorf("ghapp: not a branch name")
	}
	meta := Perms{"metadata": "read"}
	var rules []struct {
		Type       string `json:"type"`
		RulesetID  int64  `json:"ruleset_id"`
		Parameters struct {
			Approvals int  `json:"required_approving_review_count"`
			LastPush  bool `json:"require_last_push_approval"`
		} `json:"parameters"`
	}
	if err := c.asInstall(ctx, installation, repo, meta, http.MethodGet, "/repos/"+repo+"/rules/branches/"+branch+"?per_page=100", nil, &rules, 200); err != nil {
		return false, "", err
	}
	seen := map[int64]bool{}
	for _, r := range rules {
		p := r.Parameters
		if r.Type != "pull_request" || r.RulesetID <= 0 || p.Approvals < 1 || !p.LastPush || seen[r.RulesetID] {
			// No approval, or one the last pusher may give (dismissing stale
			// reviews alone lets a pusher approve their own push), is not a
			// review of what merges.
			continue
		}
		seen[r.RulesetID] = true
		var rs struct {
			Enforcement string `json:"enforcement"`
			CanBypass   string `json:"current_user_can_bypass"`
		}
		if err := c.asInstall(ctx, installation, repo, meta, http.MethodGet, "/repos/"+repo+"/rulesets/"+strconv.FormatInt(r.RulesetID, 10), nil, &rs, 200); err != nil {
			return false, "", err
		}
		if rs.Enforcement == "active" && rs.CanBypass == "never" {
			return true, "", nil
		}
	}
	if len(seen) == 0 {
		return false, "no active ruleset requires a pull request approved after its last push on " + branch, nil
	}
	return false, "every ruleset requiring a pull request on " + branch + " can be bypassed by this App", nil
}
