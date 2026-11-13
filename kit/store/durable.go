package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// DurableTx commits a reservation with synchronous FULL before a caller may
// write a replayable sink in another database. Ordinary Tx is atomic within
// one file but NORMAL does not promise this cross-file power-loss ordering.
func DurableTx(ctx context.Context, db *sql.DB, fn func(*sql.Tx) error) error {
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	var prior int
	if err := conn.QueryRowContext(ctx, "PRAGMA synchronous").Scan(&prior); err != nil {
		return err
	}
	if prior < 2 {
		if _, err := conn.ExecContext(ctx, "PRAGMA synchronous = FULL"); err != nil {
			return err
		}
		defer func() {
			cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			// If restoring fails the pool retains FULL, which is safe.
			_, _ = conn.ExecContext(cleanup, fmt.Sprintf("PRAGMA synchronous = %d", prior))
		}()
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}
