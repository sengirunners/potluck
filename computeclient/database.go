package computeclient

import (
	"context"
	"net/http"
	"net/url"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/kit/client"
)

// Databases is GET /v0/databases: the project's database recipes.
func Databases(ctx context.Context, c *client.Client) (computeapi.DatabaseRecipes, error) {
	var v computeapi.DatabaseRecipes
	return v, c.Do(ctx, http.MethodGet, "/v0/databases", nil, &v)
}

// SetDatabase is PUT /v0/databases/{name} (an owner or admin of the whole
// project): create or replace that recipe.
func SetDatabase(ctx context.Context, c *client.Client, name string, req computeapi.SetDatabaseRecipeRequest) (computeapi.DatabaseRecipeSet, error) {
	var v computeapi.DatabaseRecipeSet
	return v, c.Do(ctx, http.MethodPut, "/v0/databases/"+url.PathEscape(name), req, &v)
}

// RemoveDatabase is DELETE /v0/databases/{name} (an owner or admin of the
// whole project): the recipes left.
func RemoveDatabase(ctx context.Context, c *client.Client, name string) (computeapi.DatabaseRecipes, error) {
	var v computeapi.DatabaseRecipes
	return v, c.Do(ctx, http.MethodDelete, "/v0/databases/"+url.PathEscape(name), nil, &v)
}
