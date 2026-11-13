package client

// Teams (SPEC 17.3, 17.4): one method per /v0/invites, /v0/team and
// project team|solo route in API.md.

import (
	"context"
	"net/http"
	"net/url"

	"github.com/sengirunners/potluck/kit/api"
)

// ProjectTeam is POST /v0/projects/{project}/team (this machine becomes its authority).
func (c *Client) ProjectTeam(ctx context.Context, project string) (api.ProjectModeResponse, error) {
	var r api.ProjectModeResponse
	return r, c.Do(ctx, http.MethodPost, "/v0/projects/"+Seg(project)+"/team", struct{}{}, &r)
}

// ProjectSolo is POST /v0/projects/{project}/solo.
func (c *Client) ProjectSolo(ctx context.Context, project string) (api.ProjectModeResponse, error) {
	var r api.ProjectModeResponse
	return r, c.Do(ctx, http.MethodPost, "/v0/projects/"+Seg(project)+"/solo", struct{}{}, &r)
}

// Invite is POST /v0/invites.
func (c *Client) Invite(ctx context.Context, req api.InviteRequest) (api.InviteResponse, error) {
	var r api.InviteResponse
	return r, c.Do(ctx, http.MethodPost, "/v0/invites", req, &r)
}

// Invites is GET /v0/invites.
func (c *Client) Invites(ctx context.Context) ([]api.Invite, error) {
	var l api.InviteList
	return l.Invites, c.Do(ctx, http.MethodGet, "/v0/invites", nil, &l)
}

// RemoveInvite is DELETE /v0/invites/{id}.
func (c *Client) RemoveInvite(ctx context.Context, id string) (api.Invite, error) {
	var r api.Invite
	return r, c.Do(ctx, http.MethodDelete, "/v0/invites/"+Seg(id), nil, &r)
}

// Join is POST /v0/team/join on this machine's daemon.
func (c *Client) Join(ctx context.Context, req api.JoinRequest) (api.JoinResponse, error) {
	var r api.JoinResponse
	return r, c.Do(ctx, http.MethodPost, "/v0/team/join", req, &r)
}

// Remotes is GET /v0/team/remotes.
func (c *Client) Remotes(ctx context.Context) ([]api.Remote, error) {
	var l api.RemoteList
	return l.Remotes, c.Do(ctx, http.MethodGet, "/v0/team/remotes", nil, &l)
}

// Outbox is GET /v0/team/outbox: a replica's queued writes.
func (c *Client) Outbox(ctx context.Context, project string, all bool) ([]api.OutboxEntry, error) {
	q := url.Values{}
	if project != "" {
		q.Set("project", project)
	}
	if all {
		q.Set("all", "1")
	}
	path := "/v0/team/outbox"
	if enc := q.Encode(); enc != "" {
		path += "?" + enc
	}
	var l api.OutboxList
	return l.Entries, c.Do(ctx, http.MethodGet, path, nil, &l)
}

// Leave is DELETE /v0/team/remotes/{project}.
func (c *Client) Leave(ctx context.Context, project string) (api.LeaveResponse, error) {
	var r api.LeaveResponse
	return r, c.Do(ctx, http.MethodDelete, "/v0/team/remotes/"+Seg(project), nil, &r)
}

// SetMachineClass is POST /v0/team/machines/{machine}/class: set (or, with
// class "", clear) a machine's operator class in a team project you own.
func (c *Client) SetMachineClass(ctx context.Context, machine string, req api.MachineClassRequest) (api.MachineClass, error) {
	var r api.MachineClass
	err := c.Do(ctx, http.MethodPost, "/v0/team/machines/"+Seg(machine)+"/class", req, &r)
	return r, err
}

// MachineClasses is GET /v0/team/machines/class: every machine's operator
// class in a team project you own.
func (c *Client) MachineClasses(ctx context.Context, project string) (api.MachineClassList, error) {
	var r api.MachineClassList
	err := c.Do(ctx, http.MethodGet, Query("/v0/team/machines/class", "project", project), nil, &r)
	return r, err
}

// CancelWrite is POST /v0/team/writes/{id}/cancel: settle a write id at the
// authority of a joined project (it ran, or it never will).
func (c *Client) CancelWrite(ctx context.Context, project, id string) (api.WriteCancelResponse, error) {
	var r api.WriteCancelResponse
	return r, c.Do(ctx, http.MethodPost, Query("/v0/team/writes/"+Seg(id)+"/cancel", "project", project), struct{}{}, &r)
}

// ProjectKey is GET /v0/team/project-keys/{project}.
func (c *Client) ProjectKey(ctx context.Context, project string) (api.ProjectKey, error) {
	var r api.ProjectKey
	return r, c.Do(ctx, http.MethodGet, "/v0/team/project-keys/"+Seg(project), nil, &r)
}

// ForgetProjectKey is POST /v0/team/project-keys/{project}/forget.
func (c *Client) ForgetProjectKey(ctx context.Context, project string, req api.ForgetProjectKeyRequest) (api.ForgetProjectKeyResponse, error) {
	var r api.ForgetProjectKeyResponse
	return r, c.Do(ctx, http.MethodPost, "/v0/team/project-keys/"+Seg(project)+"/forget", req, &r)
}
