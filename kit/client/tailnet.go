package client

// Owned by the tailnet builder. One method per /v0/tailnet and /v0/peers endpoint in API.md.

import (
	"context"
	"net/http"

	"github.com/sengirunners/potluck/kit/api"
)

// Tailnet is GET /v0/tailnet.
func (c *Client) Tailnet(ctx context.Context) (api.TailnetStatus, error) {
	var s api.TailnetStatus
	return s, c.Do(ctx, http.MethodGet, "/v0/tailnet", nil, &s)
}

// TailnetUp is POST /v0/tailnet/up.
func (c *Client) TailnetUp(ctx context.Context, req api.TailnetUpRequest) (api.TailnetStatus, error) {
	var s api.TailnetStatus
	return s, c.Do(ctx, http.MethodPost, "/v0/tailnet/up", req, &s)
}

// TailnetDown is POST /v0/tailnet/down.
func (c *Client) TailnetDown(ctx context.Context) (api.TailnetStatus, error) {
	var s api.TailnetStatus
	return s, c.Do(ctx, http.MethodPost, "/v0/tailnet/down", nil, &s)
}

// Peers is GET /v0/peers.
func (c *Client) Peers(ctx context.Context) ([]api.Peer, error) {
	var l api.PeerList
	return l.Peers, c.Do(ctx, http.MethodGet, "/v0/peers", nil, &l)
}
