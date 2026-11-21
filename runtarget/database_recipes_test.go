package runtarget_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/computeclient"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/project"
	"github.com/sengirunners/potluck/protocol"
)

// TestDatabaseRecipeSetLive: a project owner or admin sets a database recipe
// through the API and the very next acquire naming it resolves it, with no
// daemon restart; a replacement likewise. A member reads the recipes but
// sets none; an invalid recipe is refused naming its rule and leaves
// project.toml as it was; setting the same recipe writes nothing; a stale
// --if-image is a conflict; rm removes it.
func TestDatabaseRecipeSetLive(t *testing.T) {
	f := newDatabaseDaemonFixture(t, "")
	ctx := context.Background()
	path := filepath.Join(f.dm.Deps.Store.ProjectDir(f.project), project.FileName)
	read := func() string {
		t.Helper()
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	acquire := func(id, db string) protocol.Database {
		t.Helper()
		if _, err := computeclient.AcquireRunTarget(ctx, f.member, "dbbox", computeapi.AcquireRunTargetRequest{RequestID: id, Database: db}); err != nil {
			t.Fatalf("acquire %s: %v", db, err)
		}
		calls := f.requests(t, "acquire")
		var a struct {
			Database *protocol.Database `json:"database"`
		}
		if err := json.Unmarshal(calls[len(calls)-1].Args, &a); err != nil || a.Database == nil {
			t.Fatalf("driver args: %v", err)
		}
		return *a.Database
	}
	set := func(c *client.Client, name string, req computeapi.SetDatabaseRecipeRequest) (computeapi.DatabaseRecipeSet, error) {
		return computeclient.SetDatabase(ctx, c, name, req)
	}
	tok, err := f.owner.MintToken(ctx, api.MintTokenRequest{As: "wa", Project: "acme", Role: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	admin := client.WithHandler(f.dm.Handler)
	admin.Token, admin.Project, admin.ProjectFrom = tok.Secret, "acme", "flag"

	// Create: a name no recipe had resolves on the next acquire.
	if _, err := computeclient.AcquireRunTarget(ctx, f.member, "dbbox", computeapi.AcquireRunTargetRequest{RequestID: "before", Database: "second-db"}); !api.IsCode(err, api.CodeBadRequest) {
		t.Fatalf("acquire of an unset recipe: %v", err)
	}
	add := computeapi.SetDatabaseRecipeRequest{Image: f.recipe.Image, Manifest: f.recipe.Manifest}
	if _, err := set(f.member, "second-db", add); !api.IsCode(err, api.CodeForbidden) {
		t.Fatalf("a member set a recipe: %v", err)
	}
	v, err := set(admin, "second-db", add)
	if err != nil || !v.Changed || v.Recipe.Name != "second-db" || v.Recipe.Image != f.recipe.Image {
		t.Fatalf("a whole-project owner sets a recipe: %+v %v", v, err)
	}
	if got := acquire("created", "second-db"); got.Name != "second-db" || got.Manifest != f.recipe.Manifest || got.Project != f.project {
		t.Fatalf("the next acquire resolved %+v", got)
	}
	if !strings.Contains(read(), `name = "second-db"`) {
		t.Fatalf("project.toml was not rewritten:\n%s", read())
	}

	// Replace: the next acquire boots the new manifest bytes.
	replaced := computeapi.SetDatabaseRecipeRequest{Image: f.recipe.Image, Manifest: f.recipe.Manifest + "\n", IfImage: f.recipe.Image}
	if v, err := set(f.owner, "example-db", replaced); err != nil || !v.Changed {
		t.Fatalf("replace: %+v %v", v, err)
	}
	if got := acquire("replaced", "example-db"); got.Manifest != replaced.Manifest {
		t.Fatalf("the next acquire kept the old manifest")
	}
	l, err := computeclient.Databases(ctx, f.member)
	if err != nil || len(l.Databases) != 2 || l.Project != "acme" || l.Databases[0] != (computeapi.DatabaseRecipe{Name: "example-db", Image: f.recipe.Image, Manifest: replaced.Manifest}) {
		t.Fatalf("a member lists the recipes: %+v %v", l, err)
	}

	// The same recipe again writes nothing.
	before := read()
	if v, err := set(f.owner, "example-db", replaced); err != nil || v.Changed {
		t.Fatalf("an identical set: %+v %v", v, err)
	}
	// A stale --if-image, an unpinned image and a broken manifest are refused,
	// and project.toml is left as it was.
	// A retry of a set that already landed (its answer lost) names the old pin
	// in --if-image; the recipe is already what it asks for, so it succeeds
	// with nothing changed rather than answering a conflict.
	retried := replaced
	retried.IfImage = "snapshot@sha256:" + strings.Repeat("c", 64)
	if v, err := set(f.owner, "example-db", retried); err != nil || v.Changed {
		t.Fatalf("a retried set that already landed: %+v %v", v, err)
	}
	stale := retried
	stale.Manifest += "\n"
	if _, err := set(f.owner, "example-db", stale); !api.IsCode(err, api.CodeConflict) {
		t.Fatalf("a stale --if-image: %v", err)
	}
	if _, err := set(f.owner, "third-db", computeapi.SetDatabaseRecipeRequest{Image: f.recipe.Image, Manifest: f.recipe.Manifest, IfImage: f.recipe.Image}); !api.IsCode(err, api.CodeConflict) {
		t.Fatalf("--if-image on no recipe: %v", err)
	}
	for name, req := range map[string]computeapi.SetDatabaseRecipeRequest{
		"mutable image": {Image: "snapshot:latest", Manifest: f.recipe.Manifest},
		"no pgtap":      {Image: f.recipe.Image, Manifest: strings.Replace(f.recipe.Manifest, `"pgtap"`, `"pgtap_absent"`, 1)},
	} {
		if _, err := set(f.owner, "example-db", req); !api.IsCode(err, api.CodeBadRequest) {
			t.Errorf("%s: %v", name, err)
		}
	}
	var e *api.Error
	if _, err := set(f.owner, "example-db", computeapi.SetDatabaseRecipeRequest{Image: f.recipe.Image, Manifest: "{}"}); !errors.As(err, &e) || !strings.Contains(e.Message, "manifest") {
		t.Fatalf("a refusal names its rule: %v", err)
	}
	if read() != before {
		t.Fatal("a refused or identical set rewrote project.toml")
	}

	// rm: a member may not; the owner may; the name then resolves to nothing.
	if _, err := computeclient.RemoveDatabase(ctx, f.member, "second-db"); !api.IsCode(err, api.CodeForbidden) {
		t.Fatalf("a member removed a recipe: %v", err)
	}
	if l, err := computeclient.RemoveDatabase(ctx, f.owner, "second-db"); err != nil || len(l.Databases) != 1 || l.Databases[0].Name != "example-db" {
		t.Fatalf("rm: %+v %v", l, err)
	}
	if _, err := computeclient.RemoveDatabase(ctx, f.owner, "second-db"); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("rm of no recipe: %v", err)
	}
	if _, err := computeclient.AcquireRunTarget(ctx, f.member, "dbbox", computeapi.AcquireRunTargetRequest{RequestID: "removed", Database: "second-db"}); !api.IsCode(err, api.CodeBadRequest) {
		t.Fatalf("acquire of a removed recipe: %v", err)
	}
}
