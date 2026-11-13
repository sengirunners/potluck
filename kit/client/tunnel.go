package client

// Owned by the tunnel lane. One method per /v0/tunnel endpoint in API.md.

import (
	"context"
	"net/http"

	"github.com/sengirunners/potluck/kit/api"
)

// Tunnel is GET /v0/tunnel.
func (c *Client) Tunnel(ctx context.Context) (api.TunnelStatus, error) {
	var s api.TunnelStatus
	return s, c.Do(ctx, http.MethodGet, "/v0/tunnel", nil, &s)
}

// TunnelUp is POST /v0/tunnel/up.
func (c *Client) TunnelUp(ctx context.Context, req api.TunnelUpRequest) (api.TunnelStatus, error) {
	var s api.TunnelStatus
	return s, c.Do(ctx, http.MethodPost, "/v0/tunnel/up", req, &s)
}

// TunnelDown is POST /v0/tunnel/down.
func (c *Client) TunnelDown(ctx context.Context) (api.TunnelStatus, error) {
	var s api.TunnelStatus
	return s, c.Do(ctx, http.MethodPost, "/v0/tunnel/down", nil, &s)
}

// TunnelKey is POST /v0/tunnel/key: name (ref "" clears) the secret
// reference that holds this daemon's key outside the home.
func (c *Client) TunnelKey(ctx context.Context, ref string) (api.TunnelStatus, error) {
	var st api.TunnelStatus
	return st, c.Do(ctx, http.MethodPost, "/v0/tunnel/key", api.TunnelKeyRequest{Ref: ref}, &st)
}
