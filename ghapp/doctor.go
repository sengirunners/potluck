package ghapp

import (
	"context"
	"fmt"
	"net/http"
)

// CheckApp verifies that GitHub accepts the configured App's signed key.
func (c *Client) CheckApp(ctx context.Context) error {
	return c.asApp(ctx, http.MethodGet, "/app", nil, nil, 200)
}

// OwnerInstallation finds this App's installation on a user or organization.
func (c *Client) OwnerInstallation(ctx context.Context, owner string) (int64, error) {
	if !validName(owner) {
		return 0, validRepo(owner)
	}
	var r struct {
		ID int64 `json:"id"`
	}
	err := c.asApp(ctx, http.MethodGet, "/users/"+owner+"/installation", nil, &r, 200)
	if NotFound(err) {
		err = c.asApp(ctx, http.MethodGet, "/orgs/"+owner+"/installation", nil, &r, 200)
	}
	if err == nil && r.ID <= 0 {
		err = fmt.Errorf("GitHub supplied no installation id")
	}
	return r.ID, err
}
