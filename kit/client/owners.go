package client

// Owned by the owners brick. One method per route in API.md "Owners".

import (
	"context"
	"net/http"
	"net/url"

	"github.com/sengirunners/potluck/kit/api"
)

// StartGitHubLogin is POST /v0/login/github/start.
func (c *Client) StartGitHubLogin(ctx context.Context, req api.GitHubLoginRequest) (api.GitHubLoginStart, error) {
	var out api.GitHubLoginStart
	return out, c.Do(ctx, http.MethodPost, "/v0/login/github/start", req, &out)
}

// PollGitHubLogin is POST /v0/login/github/{id}.
func (c *Client) PollGitHubLogin(ctx context.Context, id string, req api.GitHubLoginPoll) (api.GitHubLoginOutcome, error) {
	var out api.GitHubLoginOutcome
	return out, c.Do(ctx, http.MethodPost, "/v0/login/github/"+url.PathEscape(id), req, &out)
}

// UnlinkGitHubIdentity is DELETE /v0/identities/{identity}/github[?revoke=1].
func (c *Client) UnlinkGitHubIdentity(ctx context.Context, identity string, revoke bool) (api.GitHubUnlink, error) {
	q := ""
	if revoke {
		q = "?revoke=1"
	}
	var out api.GitHubUnlink
	return out, c.Do(ctx, http.MethodDelete, "/v0/identities/"+url.PathEscape(identity)+"/github"+q, nil, &out)
}

// Owner is GET /v0/owner?project=.
func (c *Client) Owner(ctx context.Context, project string) (api.OwnerStatus, error) {
	var out api.OwnerStatus
	return out, c.Do(ctx, http.MethodGet, "/v0/owner?project="+url.QueryEscape(project), nil, &out)
}

// GrantGitHub is POST /v0/grants/github.
func (c *Client) GrantGitHub(ctx context.Context, req api.GitHubGrantRequest) (api.GitHubGrant, error) {
	var out api.GitHubGrant
	return out, c.Do(ctx, http.MethodPost, "/v0/grants/github", req, &out)
}

// RevokeGitHub is DELETE /v0/grants/github.
func (c *Client) RevokeGitHub(ctx context.Context, req api.GitHubGrantRequest) (api.GitHubRevoke, error) {
	var out api.GitHubRevoke
	return out, c.Do(ctx, http.MethodDelete, "/v0/grants/github", req, &out)
}

// GitHubGrants is GET /v0/grants/github?project=.
func (c *Client) GitHubGrants(ctx context.Context, project string) (api.GitHubGrantList, error) {
	var out api.GitHubGrantList
	return out, c.Do(ctx, http.MethodGet, "/v0/grants/github?project="+url.QueryEscape(project), nil, &out)
}

// GitHubAuthority is GET /v0/join/github (at an authority).
func (c *Client) GitHubAuthority(ctx context.Context) (api.GitHubAuthority, error) {
	var out api.GitHubAuthority
	return out, c.Do(ctx, http.MethodGet, "/v0/join/github", nil, &out)
}

// StartGitHubJoin is POST /v0/join/github (at an authority, from a tailnet node).
func (c *Client) StartGitHubJoin(ctx context.Context, req api.GitHubJoinRequest) (api.GitHubLoginStart, error) {
	var out api.GitHubLoginStart
	return out, c.Do(ctx, http.MethodPost, "/v0/join/github", req, &out)
}

// ProjectNames is GET /v0/projects/{project}/names.
func (c *Client) ProjectNames(ctx context.Context, project string) (api.NameBindingList, error) {
	var out api.NameBindingList
	return out, c.Do(ctx, http.MethodGet, "/v0/projects/"+url.PathEscape(project)+"/names", nil, &out)
}

// ResolveName is GET /v0/names/{owner}/{name}.
func (c *Client) ResolveName(ctx context.Context, owner, name string) (api.NameResolution, error) {
	var out api.NameResolution
	return out, c.Do(ctx, http.MethodGet, "/v0/names/"+url.PathEscape(owner)+"/"+url.PathEscape(name), nil, &out)
}
