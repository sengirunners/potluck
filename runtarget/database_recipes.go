package runtarget

import (
	"context"
	"net/http"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/database"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/core"
)

// A project's database recipes, read and set live: the database package
// owns the recipe and its rules; an acquire's databaseFor resolves the
// project.toml these write on its next call.

// listDatabases is GET /v0/databases: read on the project. A recipe and its
// manifest are not secret.
func (s *Service) listDatabases(r *http.Request) (any, error) {
	t, err := s.d.WriteTarget(r.Context(), "", authz.Read)
	if err != nil {
		return nil, err
	}
	out := computeapi.DatabaseRecipes{Project: t.Project.Slug, Databases: []computeapi.DatabaseRecipe{}}
	for _, x := range database.Recipes(s.d.Projects, t.Project.ID) {
		out.Databases = append(out.Databases, computeapi.DatabaseRecipe{Name: x.Name, Image: x.Image, Manifest: x.Manifest})
	}
	return out, nil
}

// setDatabase is PUT /v0/databases/{name}: an owner or admin of the whole
// project creates or replaces that recipe.
func (s *Service) setDatabase(r *http.Request) (any, error) {
	t, err := s.databaseAdmin(r)
	if err != nil {
		return nil, err
	}
	var req computeapi.SetDatabaseRecipeRequest
	if err := api.DecodeJSON(r, &req); err != nil {
		return nil, err
	}
	rec := database.Recipe{Name: r.PathValue("name"), Image: req.Image, Manifest: req.Manifest}
	changed, err := database.Put(r.Context(), s.d.Projects, t.Project.ID, rec, req.IfImage)
	if err != nil {
		return nil, err
	}
	if changed {
		s.d.Log.Info("database recipe set", "project", t.Project.Slug, "name", rec.Name, "image", rec.Image, "by", askedBy(r.Context()))
	}
	return computeapi.DatabaseRecipeSet{Project: t.Project.Slug, Changed: changed,
		Recipe: computeapi.DatabaseRecipe{Name: rec.Name, Image: rec.Image, Manifest: rec.Manifest}}, nil
}

// removeDatabase is DELETE /v0/databases/{name}: an owner or admin of the
// whole project; answers the recipes left.
func (s *Service) removeDatabase(r *http.Request) (any, error) {
	t, err := s.databaseAdmin(r)
	if err != nil {
		return nil, err
	}
	name := r.PathValue("name")
	if err := database.Remove(r.Context(), s.d.Projects, t.Project.ID, name); err != nil {
		return nil, err
	}
	s.d.Log.Info("database recipe removed", "project", t.Project.Slug, "name", name, "by", askedBy(r.Context()))
	return s.listDatabases(r)
}

// databaseAdmin is the project a recipe edit is for: its caller reads the
// project and is an owner or admin of all of it (an area's own admin may
// not change what every area's sandboxes boot).
func (s *Service) databaseAdmin(r *http.Request) (core.Target, error) {
	ctx := r.Context()
	t, err := s.d.WriteTarget(ctx, "", authz.Read)
	if err != nil {
		return core.Target{}, err
	}
	// The caller already reads the project (above), so saying why leaks nothing.
	if !core.SubjectOf(ctx).Can(authz.Admin, authz.Object{Project: t.Project.ID}) {
		return core.Target{}, api.Errorf(api.CodeForbidden, "only an owner or admin of the whole project %s sets its database recipes", t.Project.Slug)
	}
	return t, nil
}

// askedBy names who asked, for a log or an event: the identity, else the machine.
func askedBy(ctx context.Context) string {
	sub := core.SubjectOf(ctx)
	if sub.Identity.Name != "" {
		return sub.Identity.Name
	}
	return sub.Machine
}
