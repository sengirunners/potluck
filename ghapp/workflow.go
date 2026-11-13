package ghapp

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

var ErrWorkflowContentsRead = errors.New("GitHub App needs Contents: read; existing installations may need the permission update")

// WorkflowFile reads a workflow file only at an immutable commit. A separate scoped
// Contents token keeps workflow reads out of runner-administration grants.
func (c *Client) WorkflowFile(ctx context.Context, installation int64, repo, path, sha string) ([]byte, error) {
	if !strings.HasPrefix(path, ".github/workflows/") {
		return nil, fmt.Errorf("workflow source is not an immutable workflow file")
	}
	return c.FileAt(ctx, installation, repo, path, sha)
}

// FileAt reads one file of repo at commit sha (a full commit id, so what it
// reads cannot move), with a Contents read token of its own.
func (c *Client) FileAt(ctx context.Context, installation int64, repo, path, sha string) ([]byte, error) {
	if err := validRepo(repo); err != nil {
		return nil, err
	}
	if len(sha) != 40 || strings.Trim(sha, "0123456789abcdef") != "" || path == "" || strings.HasPrefix(path, "/") || strings.Contains(path, "..") || strings.ContainsAny(path, "?#@\\") {
		return nil, fmt.Errorf("workflow source is not an immutable workflow file")
	}
	var v struct{ Encoding, Content string }
	err := c.asInstall(ctx, installation, repo, Perms{"contents": "read", "metadata": "read"}, http.MethodGet, "/repos/"+repo+"/contents/"+path+"?ref="+url.QueryEscape(sha), nil, &v, 200)
	if err != nil {
		var status *statusError
		if errors.As(err, &status) && (status.status == http.StatusForbidden || status.status == http.StatusUnprocessableEntity) {
			return nil, fmt.Errorf("%w: %v", ErrWorkflowContentsRead, err)
		}
		return nil, err
	}
	if v.Encoding != "base64" {
		return nil, fmt.Errorf("workflow contents are not base64")
	}
	b, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(v.Content, "\n", ""))
	if len(b) > 1<<20 {
		return nil, fmt.Errorf("workflow exceeds 1 MiB")
	}
	return b, err
}
