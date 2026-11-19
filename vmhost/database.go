package vmhost

import (
	"context"
	_ "embed"
	"strings"
	"time"

	"github.com/sengirunners/potluck/database"
	"github.com/sengirunners/potluck/protocol"
)

//go:embed database_start.py
var databaseStart string

func (d *drv) databaseSupported() bool { return d.cfg.Kind == "box" && d.cfg.VM == "oci" }
func (d *drv) checkDatabase(db *protocol.Database, image string, persist, jit bool, volumes []volReq) error {
	if db == nil {
		return nil
	}
	if !d.databaseSupported() || persist || jit {
		return refuse("bad_request", "database needs an OCI box without persist or runner")
	}
	if _, err := database.Check(db); err != nil {
		return refuse("bad_request", "%v", err)
	}
	for _, v := range volumes {
		if protocol.VolumePathsOverlap(protocol.DatabaseHome, v.Path) {
			return refuse("bad_request", "volume %s overlaps private database directory", v.Name)
		}
	}
	if image != db.Image {
		return refuse("bad_request", "database image does not match its immutable recipe")
	}
	return nil
}
func sameDatabase(a, b *protocol.Database) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}
func (s *sup) startDatabase(r record, bootBy time.Time) error {
	if r.Database == nil {
		return nil
	}
	if _, err := database.Check(r.Database); err != nil {
		return err
	}
	ctx, cancel := context.WithDeadline(context.Background(), bootBy.Add(10*time.Second))
	defer cancel()
	return s.guest(ctx, strings.NewReader(databaseStart), "database snapshot", "python3", "-", database.Digest(r.Database.Manifest))
}

// The credential stays inside the private guest. All command entry points use
// this wrapper; none puts DATABASE_URL in driver state, argv or metadata.
func databaseWrapped(r record, argv []string) []string {
	if r.Database == nil {
		return argv
	}
	return append([]string{"sh", "-c", `set -eu
` + guestHome + `f="$HOME/.potluck-database/url"
[ -f "$f" ] && [ ! -L "$f" ] || { echo 'database is not ready' >&2; exit 125; }
DATABASE_URL=$(cat "$f"); DB_URL=$DATABASE_URL; export DATABASE_URL DB_URL
exec "$@"`, "sh"}, argv...)
}
