package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/spf13/cobra"
)

// sandbox database set sends the manifest byte for byte (from stdin here)
// with the pin and --if-image to PUT /v0/databases/{name}; sandbox database
// --json prints GET /v0/databases as the daemon answered it.
func TestSandboxDatabaseCLI(t *testing.T) {
	recipe := computeapi.DatabaseRecipe{Name: "app-db", Image: "db@sha256:" + strings.Repeat("a", 64), Manifest: "{\"format\": \"x\"}\n"}
	var got computeapi.SetDatabaseRecipeRequest
	var gotPath string
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(api.HeaderProtocol, api.Protocol)
		switch r.Method + " " + r.URL.Path {
		case "PUT /v0/databases/app-db":
			gotPath = r.URL.Path
			if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
				t.Error(err)
			}
			api.WriteJSON(w, 200, computeapi.DatabaseRecipeSet{Project: "pool", Recipe: recipe, Changed: true})
		case "GET /v0/databases":
			api.WriteJSON(w, 200, computeapi.DatabaseRecipes{Project: "pool", Databases: []computeapi.DatabaseRecipe{recipe}})
		default:
			t.Errorf("unexpected route %s %s", r.Method, r.URL.Path)
			api.WriteError(w, api.ErrNotImplemented(r.URL.Path))
		}
	})
	run := func(asJSON bool, args ...string) string {
		t.Helper()
		c := client.WithHandler(handler)
		c.Project, c.ProjectFrom = "pool", "flag"
		cfg, err := config.LoadFrom(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		var stdout, stderr bytes.Buffer
		root := &cobra.Command{Use: "example", SilenceErrors: true, SilenceUsage: true}
		root.AddCommand(Commands(Host{Prefix: "example", Client: func() (*client.Client, *config.Config, error) { return c, cfg, nil },
			JSON: func() bool { return asJSON }, Stdin: strings.NewReader(recipe.Manifest), Stdout: &stdout, Stderr: &stderr})...)
		root.SetArgs(args)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := root.ExecuteContext(ctx); err != nil {
			t.Fatalf("%v: %v %s", args, err, &stderr)
		}
		return stdout.String()
	}

	out := run(false, "sandbox", "database", "set", "app-db", "--image", recipe.Image, "--manifest-file", "-", "--if-image", "db@sha256:"+strings.Repeat("b", 64))
	if gotPath == "" || got.Image != recipe.Image || got.Manifest != recipe.Manifest || got.IfImage != "db@sha256:"+strings.Repeat("b", 64) {
		t.Fatalf("PUT %q sent %+v", gotPath, got)
	}
	if !strings.Contains(out, "app-db now boots "+recipe.Image) {
		t.Fatalf("set printed %q", out)
	}
	var l computeapi.DatabaseRecipes
	if err := json.Unmarshal([]byte(run(true, "sandbox", "database")), &l); err != nil || len(l.Databases) != 1 || l.Databases[0] != recipe {
		t.Fatalf("--json list: %+v %v", l, err)
	}
	if out := run(false, "sandbox", "database"); strings.TrimSpace(out) != "app-db "+recipe.Image {
		t.Fatalf("list printed %q", out)
	}
}
