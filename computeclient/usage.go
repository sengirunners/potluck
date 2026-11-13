package computeclient

import (
	"context"
	"net/http"
	"net/url"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/kit/client"
)

// Usage is GET /v0/usage: ended boxes' usage rows the caller may read,
// newest first; since (RFC 3339) keeps rows that ended at or after it.
func Usage(ctx context.Context, c *client.Client, since string) (computeapi.UsageList, error) {
	p := "/v0/usage"
	if since != "" {
		p += "?since=" + url.QueryEscape(since)
	}
	var l computeapi.UsageList
	return l, c.Do(ctx, http.MethodGet, p, nil, &l)
}

// AreaMinuteLimits is GET /v0/usage/limits: the project's per-area sandbox
// minute limits.
func AreaMinuteLimits(ctx context.Context, c *client.Client) (computeapi.AreaMinuteLimits, error) {
	var l computeapi.AreaMinuteLimits
	return l, c.Do(ctx, http.MethodGet, "/v0/usage/limits", nil, &l)
}

// SetAreaMinuteLimit is PUT /v0/usage/limits/{area} (a whole-project owner
// or admin): minutes 0 removes the limit.
func SetAreaMinuteLimit(ctx context.Context, c *client.Client, area string, req computeapi.SetAreaMinuteLimitRequest) (computeapi.AreaMinuteLimits, error) {
	var l computeapi.AreaMinuteLimits
	return l, c.Do(ctx, http.MethodPut, "/v0/usage/limits/"+url.PathEscape(area), req, &l)
}
