package database_test

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/sengirunners/potluck/database"
	"github.com/sengirunners/potluck/kit/project"
	"github.com/sengirunners/potluck/protocol"
)

func TestSnapshotManifestAndProjectScope(t *testing.T) {
	raw, err := os.ReadFile("testdata/manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	identity := protocol.Database{Project: "pool-project-id", Name: "example-db", Image: "db@sha256:" + strings.Repeat("a", 64), Manifest: string(raw)}
	if _, err := database.Check(&identity); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*protocol.Database){
		"mutable image":    func(d *protocol.Database) { d.Image = "db:latest" },
		"missing scope":    func(d *protocol.Database) { d.Project = "" },
		"missing manifest": func(d *protocol.Database) { d.Manifest = "{}" },
		"running snapshot": func(d *protocol.Database) { d.Manifest = strings.Replace(d.Manifest, "shut down", "in production", 1) },
		"external config": func(d *protocol.Database) {
			d.Manifest = strings.ReplaceAll(d.Manifest, "/etc/postgresql/postgresql.conf", "/tmp/outside.conf")
		},
		"bad PGDATA": func(d *protocol.Database) {
			d.Manifest = strings.ReplaceAll(d.Manifest, "/var/lib/postgresql/data", "/var/../data")
		},
		"oversized manifest": func(d *protocol.Database) {
			d.Manifest = strings.Repeat(" ", database.ManifestMax) + d.Manifest
		},
		"bad runtime": func(d *protocol.Database) {
			d.Manifest = strings.ReplaceAll(d.Manifest, "/nix/postgres/bindir", "/tmp/bin")
		},
	} {
		t.Run(name, func(t *testing.T) {
			d := identity
			mutate(&d)
			if _, err := database.Check(&d); err == nil {
				t.Fatal("accepted invalid recipe")
			}
		})
	}
	// A refusal names the rule, so an owner can fix the manifest.
	noPGTAP := identity
	noPGTAP.Manifest = strings.Replace(identity.Manifest, `"pgtap"`, `"pgtap_absent"`, 1)
	if _, err := database.Check(&noPGTAP); err == nil || !strings.Contains(err.Error(), "pgtap") {
		t.Fatalf("missing pgtap: %v", err)
	}
	// A pool project has no source repository remote: the owner's recipe is the
	// authority to lend this artifact; manifest.project names its source only.
	src := "id = \"pool-project-id\"\nslug = \"pool\"\nname = \"Pool\"\n[[databases]]\nname = \"example-db\"\nimage = " + project.TOMLString(identity.Image) + "\nmanifest = " + project.TOMLString(identity.Manifest) + "\n"
	file, err := project.ParseFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Validate(); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(file.Section("databases"))
	if !strings.Contains(string(b), "example-db") {
		t.Fatalf("recipe absent: %s", b)
	}
}
