package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"modernc.org/sqlite"
)

// OpenFile opens a SQLite file that is not registry.db or a project.db (a
// module's own brick, e.g. the native issue store's issues.db) with the same
// settings as every other handle: WAL, synchronous=NORMAL, foreign keys,
// IMMEDIATE write transactions, busy timeout, and the per-connection
// prepared-statement cache. maxConns connections stay open. No migrations
// run: the caller owns the schema. pragmas are added to every connection
// (e.g. "wal_autocheckpoint(0)"). readOnly opens a reader pool: query_only,
// and transactions that start deferred (a consistent snapshot, no lock).
func OpenFile(ctx context.Context, path string, maxConns int, busy time.Duration, readOnly bool, pragmas ...string) (*sql.DB, error) {
	q := url.Values{}
	for _, p := range []string{"journal_mode(WAL)", fmt.Sprintf("busy_timeout(%d)", busy.Milliseconds()), "foreign_keys(1)", "synchronous(NORMAL)"} {
		q.Add("_pragma", p)
	}
	for _, p := range pragmas {
		q.Add("_pragma", p)
	}
	if readOnly {
		q.Add("_pragma", "query_only(1)")
		q.Set("_txlock", "deferred")
	} else {
		q.Set("_txlock", "immediate")
	}
	base, err := sqlite.NewConnector("file:" + path + "?" + q.Encode())
	if err != nil {
		return nil, err
	}
	db := sql.OpenDB(cachingConnector{base})
	db.SetMaxOpenConns(maxConns)
	db.SetMaxIdleConns(maxConns)
	db.SetConnMaxLifetime(0)
	db.SetConnMaxIdleTime(0)
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	_ = os.Chmod(path, 0o600)
	return db, nil
}

// OpenCopy opens a private copy of the SQLite file at path, which must be
// a regular file: path and its -wal, when there is one, are copied into a
// new directory of the system's temporary directory (0700, files 0600) and
// the copy is opened read only (query_only). Nothing is created, changed or
// locked beside path: SQLite's own -wal and -shm, even for a read, are the
// copy's. A daemon that opened no store (a parked one) reads its home's
// registry.db this way. The copy is as consistent as the files were while
// copied, so it is for a file nothing writes now. done closes the copy and
// removes its directory; the caller calls it once.
func OpenCopy(ctx context.Context, path string) (db *sql.DB, done func(), err error) {
	st, err := os.Lstat(path)
	if err != nil {
		return nil, nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, nil, fmt.Errorf("%s is not a regular file", path)
	}
	dir, err := os.MkdirTemp("", "potluck-copy-*")
	if err != nil {
		return nil, nil, err
	}
	gone := func() { _ = os.RemoveAll(dir) }
	cp := filepath.Join(dir, filepath.Base(path))
	for _, suffix := range []string{"", "-wal"} {
		if err := copyRegular(path+suffix, cp+suffix); err != nil {
			if suffix != "" && errors.Is(err, os.ErrNotExist) {
				continue
			}
			gone()
			return nil, nil, err
		}
	}
	db, err = OpenFile(ctx, cp, 1, time.Second, true)
	if err != nil {
		gone()
		return nil, nil, err
	}
	return db, func() { db.Close(); gone() }, nil
}

// copyRegular copies the regular file from to a new file to (0600).
func copyRegular(from, to string) error {
	in, err := os.Open(from)
	if err != nil {
		return err
	}
	defer in.Close()
	if st, err := in.Stat(); err != nil {
		return err
	} else if !st.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", from)
	}
	out, err := os.OpenFile(to, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// AppliedIn lists the migrations recorded in path's schema_migrations,
// opened read only (no migration runs, a live daemon's file is safe to
// read). A file without the table has none. The swap preflight compares
// them with what a build registers (SPEC 2, "Swaps").
func AppliedIn(ctx context.Context, path string) ([]string, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, err
	}
	db, err := OpenFile(ctx, path, 1, time.Second, true)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'schema_migrations'`).Scan(&n); err != nil || n == 0 {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, `SELECT name FROM schema_migrations ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}
