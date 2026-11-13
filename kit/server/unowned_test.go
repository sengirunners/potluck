package server_test

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/project"
	"github.com/sengirunners/potluck/kit/server"
	"github.com/sengirunners/potluck/wiring"
)

// A daemon that starts on a project.toml with settings nothing in it reads
// says so once per project, naming them; a project without any is not named.
func TestStartWarnsAboutUnownedProjectSettings(t *testing.T) {
	home := shortHome(t)
	cfg, err := config.LoadFrom(home)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	dm, err := server.New(ctx, cfg, nil, wiring.Kit)
	if err != nil {
		t.Fatal(err)
	}
	reg := dm.Deps.Projects
	var odd api.Project
	for _, slug := range []string{"odd", "plain"} {
		p, err := reg.Create(ctx, reg.Local(""), api.NewProjectRequest{Slug: slug})
		if err != nil {
			t.Fatal(err)
		}
		if slug == "odd" {
			odd = p
		}
	}
	dm.Close()
	path := filepath.Join(home, "projects", odd.ID, project.FileName)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("zz_future = true\n"+string(b)+"\n[[zz_widgets]]\nname = \"w\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	dm, err = server.New(ctx, cfg, slog.New(slog.NewTextHandler(&out, nil)), wiring.Kit)
	if err != nil {
		t.Fatal(err)
	}
	dm.Close()
	var warns []string
	for _, l := range strings.Split(out.String(), "\n") {
		if strings.Contains(l, "nothing in this build reads") {
			warns = append(warns, l)
		}
	}
	if len(warns) != 1 || !strings.Contains(warns[0], "level=WARN") || !strings.Contains(warns[0], "project=odd") || !strings.Contains(warns[0], `names="zz_future, zz_widgets"`) {
		t.Fatalf("warnings: %q\n%s", warns, out.String())
	}
}
