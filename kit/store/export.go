package store

import (
	"context"
	"database/sql"
)

// ExportFilter is registered at init by the owner of copied tables. It runs
// only on a disposable export copy, with the authenticated exporter's address.
type ExportFilter func(context.Context, *sql.DB, string) error

var exportFilters []ExportFilter

func RegisterExportFilter(f ExportFilter) { exportFilters = append(exportFilters, f) }
func FilterExport(ctx context.Context, db *sql.DB, address string) error {
	for _, f := range exportFilters {
		if err := f(ctx, db, address); err != nil {
			return err
		}
	}
	_, err := db.ExecContext(ctx, "VACUUM")
	return err
}
