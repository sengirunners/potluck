package ghapp

import (
	"context"
	"net/http"
)

// GitHub's approval rules for workflow runs from outside contributors' pull
// requests on a public repository (Settings > Actions > General), weakest
// first. Only ApproveAll makes every outside contributor's run wait for a
// person who can write to the repository.
const (
	ApproveNewToGitHub = "first_time_contributors_new_to_github" // only accounts new to GitHub wait
	ApproveFirstTime   = "first_time_contributors"               // only first-time contributors wait
	ApproveAll         = "all_external_contributors"             // every outside contributor waits
)

// approvalPerms is what reading a repository's Actions settings needs.
var approvalPerms = Perms{"administration": "read", "metadata": "read"}

// ForkPRApproval reads a public repository's approval rule for outside
// contributors' runs: GET /repos/{o}/{r}/actions/permissions/fork-pr-contributor-approval.
// It answers GitHub's value as given (one of the Approve constants, or one
// this version does not know).
func (c *Client) ForkPRApproval(ctx context.Context, installation int64, fullName string) (string, error) {
	if err := validRepo(fullName); err != nil {
		return "", err
	}
	var r struct {
		Policy string `json:"approval_policy"`
	}
	path := "/repos/" + fullName + "/actions/permissions/fork-pr-contributor-approval"
	if err := c.asInstall(ctx, installation, fullName, approvalPerms, http.MethodGet, path, nil, &r, 200); err != nil {
		return "", err
	}
	return r.Policy, nil
}
