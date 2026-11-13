package client

// Core endpoints: projects, areas, resolution, identities, grants, tokens.
// Owned by the integrator.

import (
	"context"
	"net/http"

	"github.com/sengirunners/potluck/kit/api"
)

// Projects is GET /v0/projects.
func (c *Client) Projects(ctx context.Context) ([]api.Project, error) {
	var l api.ProjectList
	return l.Projects, c.Do(ctx, http.MethodGet, "/v0/projects", nil, &l)
}

// LocalProjects is GET /v0/projects?local=1: scoped registry metadata
// without joined-authority discovery on servers supporting the parameter.
func (c *Client) LocalProjects(ctx context.Context) ([]api.Project, error) {
	var l api.ProjectList
	err := c.Do(ctx, http.MethodGet, "/v0/projects?local=1", nil, &l)
	return l.Projects, err
}

// ProjectList is GET /v0/projects with its skipped entries (a replica's
// joined projects whose authority did not answer).
func (c *Client) ProjectList(ctx context.Context) (api.ProjectList, error) {
	var l api.ProjectList
	return l, c.Do(ctx, http.MethodGet, "/v0/projects", nil, &l)
}

// GetProject is GET /v0/projects/{project} (slug or ULID).
func (c *Client) GetProject(ctx context.Context, ref string) (api.Project, error) {
	var p api.Project
	return p, c.Do(ctx, http.MethodGet, "/v0/projects/"+Seg(ref), nil, &p)
}

// CreateProject is POST /v0/projects.
func (c *Client) CreateProject(ctx context.Context, req api.NewProjectRequest) (api.Project, error) {
	var p api.Project
	return p, c.Do(ctx, http.MethodPost, "/v0/projects", req, &p)
}

// UpdateProject is PATCH /v0/projects/{project}.
func (c *Client) UpdateProject(ctx context.Context, ref string, req api.UpdateProjectRequest) (api.Project, error) {
	var p api.Project
	return p, c.Do(ctx, http.MethodPatch, "/v0/projects/"+Seg(ref), req, &p)
}

// Areas is GET /v0/projects/{project}/areas.
func (c *Client) Areas(ctx context.Context, ref string) ([]api.Area, error) {
	var l api.AreaList
	return l.Areas, c.Do(ctx, http.MethodGet, "/v0/projects/"+Seg(ref)+"/areas", nil, &l)
}

// AddArea is POST /v0/projects/{project}/areas.
func (c *Client) AddArea(ctx context.Context, ref string, a api.AddAreaRequest) (api.Project, error) {
	var p api.Project
	return p, c.Do(ctx, http.MethodPost, "/v0/projects/"+Seg(ref)+"/areas", a, &p)
}

// GetArea is GET /v0/projects/{project}/areas/{area}.
func (c *Client) GetArea(ctx context.Context, ref, area string) (api.AreaView, error) {
	var v api.AreaView
	return v, c.Do(ctx, http.MethodGet, "/v0/projects/"+Seg(ref)+"/areas/"+Seg(area), nil, &v)
}

// UpdateArea is PATCH /v0/projects/{project}/areas/{area}.
func (c *Client) UpdateArea(ctx context.Context, ref, area string, req api.UpdateAreaRequest) (api.AreaView, error) {
	var v api.AreaView
	return v, c.Do(ctx, http.MethodPatch, "/v0/projects/"+Seg(ref)+"/areas/"+Seg(area), req, &v)
}

// ExportProject is POST /v0/projects/{project}/export.
func (c *Client) ExportProject(ctx context.Context, ref string, req api.ExportRequest) (api.ExportResponse, error) {
	var r api.ExportResponse
	return r, c.Do(ctx, http.MethodPost, "/v0/projects/"+Seg(ref)+"/export", req, &r)
}

// Resolve is GET /v0/projects/resolve ("zm project why").
func (c *Client) Resolve(ctx context.Context) (api.Resolution, error) {
	var r api.Resolution
	return r, c.Do(ctx, http.MethodGet, "/v0/projects/resolve", nil, &r)
}

// Identities is GET /v0/identities.
func (c *Client) Identities(ctx context.Context) ([]api.Identity, error) {
	var l api.IdentityList
	return l.Identities, c.Do(ctx, http.MethodGet, "/v0/identities", nil, &l)
}

// AddIdentity is POST /v0/identities.
func (c *Client) AddIdentity(ctx context.Context, req api.NewIdentityRequest) (api.Identity, error) {
	var id api.Identity
	return id, c.Do(ctx, http.MethodPost, "/v0/identities", req, &id)
}

// Grants is GET /v0/grants (?project=).
func (c *Client) Grants(ctx context.Context, project string) ([]api.Grant, error) {
	l, err := c.GrantList(ctx, project)
	return l.Grants, err
}

// GrantList is GET /v0/grants (?project=) whole: the grants and the
// owners' ceilings.
func (c *Client) GrantList(ctx context.Context, project string) (api.GrantList, error) {
	var l api.GrantList
	return l, c.Do(ctx, http.MethodGet, Query("/v0/grants", "project", project), nil, &l)
}

// Grant is POST /v0/grants.
func (c *Client) Grant(ctx context.Context, req api.GrantRequest) (api.Grant, error) {
	var g api.Grant
	return g, c.Do(ctx, http.MethodPost, "/v0/grants", req, &g)
}

// Revoke is DELETE /v0/grants.
func (c *Client) Revoke(ctx context.Context, req api.GrantRequest) (api.Grant, error) {
	var g api.Grant
	return g, c.Do(ctx, http.MethodDelete, "/v0/grants", req, &g)
}

// Tokens is GET /v0/tokens.
func (c *Client) Tokens(ctx context.Context) ([]api.Token, error) {
	var l api.TokenList
	return l.Tokens, c.Do(ctx, http.MethodGet, "/v0/tokens", nil, &l)
}

// MintToken is POST /v0/tokens.
func (c *Client) MintToken(ctx context.Context, req api.MintTokenRequest) (api.MintTokenResponse, error) {
	var r api.MintTokenResponse
	return r, c.Do(ctx, http.MethodPost, "/v0/tokens", req, &r)
}

// RevokeToken is DELETE /v0/tokens/{id}.
func (c *Client) RevokeToken(ctx context.Context, id string) (api.Token, error) {
	var t api.Token
	return t, c.Do(ctx, http.MethodDelete, "/v0/tokens/"+Seg(id), nil, &t)
}
