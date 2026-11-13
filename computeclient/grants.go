package computeclient

import (
	"context"
	"net/http"
	"net/url"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/kit/client"
)

// SecretGrants is GET /v0/secret_grants: the default, then the grants in
// the areas the caller reads.
func SecretGrants(ctx context.Context, c *client.Client) (computeapi.SecretGrantList, error) {
	var l computeapi.SecretGrantList
	return l, c.Do(ctx, http.MethodGet, "/v0/secret_grants", nil, &l)
}

// AddSecretGrant is POST /v0/secret_grants (an admin of the grant's area).
func AddSecretGrant(ctx context.Context, c *client.Client, req computeapi.AddSecretGrantRequest) (computeapi.SecretGrant, error) {
	var g computeapi.SecretGrant
	return g, c.Do(ctx, http.MethodPost, "/v0/secret_grants", req, &g)
}

// RemoveSecretGrant is DELETE /v0/secret_grants/{id}.
func RemoveSecretGrant(ctx context.Context, c *client.Client, id string) (computeapi.SecretGrant, error) {
	var g computeapi.SecretGrant
	return g, c.Do(ctx, http.MethodDelete, "/v0/secret_grants/"+url.PathEscape(id), nil, &g)
}

// SetSecretGrantDefault is POST /v0/secret_grants/default (an admin of the
// whole pool): the built-in grant on or off.
func SetSecretGrantDefault(ctx context.Context, c *client.Client, on bool) (computeapi.SecretGrant, error) {
	var g computeapi.SecretGrant
	return g, c.Do(ctx, http.MethodPost, "/v0/secret_grants/default", computeapi.SecretGrantDefaultRequest{On: on}, &g)
}
