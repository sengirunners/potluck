package refdriver

import (
	"github.com/sengirunners/potluck/database"
	"github.com/sengirunners/potluck/protocol"
)

// The reference driver has no baked artifact. With database=1 it exercises
// boundary validation; valid identities still refuse unavailable, never fall
// through into an ordinary box. The deliberate bug forgets that boundary.
func (d *drv) databaseCaps(caps []string) []string {
	if d.cfg["database"] == "1" || d.is("database-ignored") {
		return append(caps, protocol.CapDatabase)
	}
	return caps
}
func (d *drv) databaseAcquire(db *protocol.Database) error {
	if db == nil || d.is("database-ignored") {
		return nil
	}
	if d.cfg["database"] != "1" {
		return refuse("bad_request", "no database capability")
	}
	if _, err := database.Check(db); err != nil {
		return refuse("bad_request", "%v", err)
	}
	return refuse("unavailable", "reference driver has no database artifact")
}
