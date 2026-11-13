package events

import (
	"context"
	"database/sql"
	"github.com/sengirunners/potluck/kit/store"
)

func init() {
	store.RegisterExportFilter(func(ctx context.Context, db *sql.DB, addr string) error {
		_, err := db.ExecContext(ctx, `DELETE FROM events WHERE json_extract(NULLIF(audience,''),'$.participants_only')=1 AND NOT EXISTS (SELECT 1 FROM json_each(json_extract(events.audience,'$.parties')) WHERE value=?)`, addr)
		if err != nil {
			return err
		}
		// Events that stay on the machine holding the log never leave in an
		// export either.
		home, args := authoritySQL()
		_, err = db.ExecContext(ctx, `DELETE FROM events WHERE `+home, args...)
		return err
	})
}
