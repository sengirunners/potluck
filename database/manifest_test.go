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
		"undeclared config root": func(d *protocol.Database) {
			d.Manifest = strings.Replace(d.Manifest, `"sources": [`, `"sources": ["/etc/postgresql-custom/supautils.conf",`, 1)
		},
		"config root outside /etc/postgresql": func(d *protocol.Database) {
			d.Manifest = strings.Replace(d.Manifest, `"/etc/postgresql": {`, `"/etc/postgresql": {"archive": "a.tar.gz", "sha256": "`+strings.Repeat("d", 64)+`"}, "/etc": {`, 1)
		},
		"config archive without sha256": func(d *protocol.Database) {
			d.Manifest = strings.Replace(d.Manifest, `"sha256": "dddd`, `"sha256": "xddd`, 1)
		},
		"config root is PGDATA": func(d *protocol.Database) {
			d.Manifest = strings.ReplaceAll(d.Manifest, `"pgdata_path": "/var/lib/postgresql/data"`, `"pgdata_path": "/etc/postgresql"`)
		},
		"PGDATA inside a config root": func(d *protocol.Database) {
			d.Manifest = strings.ReplaceAll(d.Manifest, `"pgdata_path": "/var/lib/postgresql/data"`, `"pgdata_path": "/etc/postgresql/data"`)
		},
		"no ledger digest": func(d *protocol.Database) {
			d.Manifest = strings.Replace(d.Manifest, `"migration_versions_sha256": "3737`, `"migration_versions_sha256": "x737`, 1)
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
	// A source under a declared sibling root, as Supabase's /etc/postgresql-custom, is packaged.
	custom := identity
	custom.Manifest = strings.Replace(strings.Replace(identity.Manifest, `"sources": [`, `"sources": ["/etc/postgresql-custom/supautils.conf",`, 1),
		`"config_archives": {`, `"config_archives": {"/etc/postgresql-custom": {"archive": "postgres-custom-config.tar.gz", "sha256": "`+strings.Repeat("d", 64)+`"},`, 1)
	if _, err := database.Check(&custom); err != nil {
		t.Fatalf("sibling configuration root: %v", err)
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
