// Package sqlbackup is the SQLite backup brick: a consistent compact copy of
// a live database (VACUUM INTO), a check that a copy is a whole database
// (integrity_check, and a size that matches its pages), and retention that
// never removes the newest copy.
// Directories, schedules and redaction are the caller's (internal/homes);
// this package knows no module.
//
// Why VACUUM INTO, not Litestream: a redaction must make text stop existing.
// A WAL-shipping replica keeps every frame until its retention expires, so
// the text would outlive the redaction; a VACUUM INTO copy holds only live
// rows, and removing the older copies removes the text. Why our own code: the
// pieces are one SQL statement and one pragma.
//
// A file is published (renamed to its final name) only after it passed its
// check, so a name that exists is a verified copy. Leftovers are always named
// *.tmp. Names are the caller's and must sort by time.
package sqlbackup

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	_ "modernc.org/sqlite" // the "sqlite" database/sql driver
)

const tmpSuffix = ".tmp"

// roomFactor is the free space a copy asks for, as a share of the source
// (ten percent over: VACUUM INTO's output can only be smaller than the file,
// and a disk filled to the last byte hurts everything else on it).
const roomFactor = 1.1

// ErrNoRoom is a target without the space for a copy.
var ErrNoRoom = errors.New("not enough free space")

// freeBytes is what dir's disk has free (a test seam).
var freeBytes = func(dir string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, err
	}
	return uint64(st.Bavail) * uint64(st.Bsize), nil
}

// ShrinkFloor: a copy under 1/ShrinkFloor of the last good one is not
// plausible (a store emptied by mistake must not rotate the good copies out).
const ShrinkFloor = 4

// Info describes a checked copy.
type Info struct {
	Bytes int64 // the file on disk
	Pages int64 // database pages
}

// DB is what Snapshot needs of a database handle. The handle must be allowed
// to write (a query_only one refuses VACUUM INTO). VACUUM INTO only reads the
// source, so it does not block that database's writer.
type DB interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Snapshot writes db to dst and publishes it once it passes Check and
// Plausible (prev: the last good copy's size, 0 = none). It fails before
// writing anything when dst's disk lacks the room (ErrNoRoom).
func Snapshot(ctx context.Context, db DB, dst string, prev int64) (Info, error) {
	tmp := dst + tmpSuffix
	if err := os.Remove(tmp); err != nil && !errors.Is(err, os.ErrNotExist) {
		return Info{}, err
	}
	if _, err := os.Stat(dst); err == nil {
		return Info{}, fmt.Errorf("%s already exists", dst)
	}
	if err := checkRoom(ctx, db, filepath.Dir(dst)); err != nil {
		return Info{}, err
	}
	// VACUUM INTO takes an empty file as it finds it, so a whole store is
	// never on disk with the default mode, not even while it is written.
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return Info{}, err
	}
	f.Close()
	if _, err := db.ExecContext(ctx, `VACUUM INTO ?`, tmp); err != nil {
		os.Remove(tmp)
		return Info{}, fmt.Errorf("VACUUM INTO: %w", err)
	}
	info, err := Check(ctx, tmp)
	if err == nil {
		err = Plausible(info.Bytes, prev)
	}
	if err == nil {
		err = publish(tmp, dst)
	}
	if err != nil {
		os.Remove(tmp)
		return Info{}, err
	}
	return info, nil
}

// checkRoom fails fast when dir cannot hold a copy of db. A filesystem that
// will not say is no reason to refuse: the copy then fails on its own.
func checkRoom(ctx context.Context, db DB, dir string) error {
	var pages, pageSize int64
	if err := db.QueryRowContext(ctx, `PRAGMA page_count`).Scan(&pages); err != nil {
		return fmt.Errorf("reading the source's size: %w", err)
	}
	if err := db.QueryRowContext(ctx, `PRAGMA page_size`).Scan(&pageSize); err != nil {
		return fmt.Errorf("reading the source's size: %w", err)
	}
	free, err := freeBytes(dir)
	if err != nil {
		return nil
	}
	need := uint64(float64(pages*pageSize) * roomFactor)
	if free < need {
		return fmt.Errorf("%w: a copy needs about %d bytes, %s has %d", ErrNoRoom, need, dir, free)
	}
	return nil
}

// Check proves path is a whole database: PRAGMA integrity_check says ok, it
// has a schema, and its size is its pages' (a truncated file fails even where
// the pages it kept are fine).
func Check(ctx context.Context, path string) (Info, error) {
	st, err := os.Stat(path)
	if err != nil {
		return Info{}, err
	}
	if !st.Mode().IsRegular() {
		return Info{}, fmt.Errorf("%s is not a regular file", path)
	}
	u := url.URL{Scheme: "file", Path: path, RawQuery: "mode=ro&immutable=1"}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return Info{}, err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	var pages, pageSize, tables int64
	if err := db.QueryRowContext(ctx, `PRAGMA page_count`).Scan(&pages); err != nil {
		return Info{}, fmt.Errorf("%s: %w", path, err)
	}
	if err := db.QueryRowContext(ctx, `PRAGMA page_size`).Scan(&pageSize); err != nil {
		return Info{}, fmt.Errorf("%s: %w", path, err)
	}
	if pages*pageSize != st.Size() {
		return Info{}, fmt.Errorf("%s is %d bytes, its header says %d pages of %d: not a whole database", path, st.Size(), pages, pageSize)
	}
	rows, err := db.QueryContext(ctx, `PRAGMA integrity_check`)
	if err != nil {
		return Info{}, fmt.Errorf("%s: %w", path, err)
	}
	defer rows.Close()
	var first string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			return Info{}, fmt.Errorf("%s: %w", path, err)
		}
		if first == "" {
			first = line
		}
	}
	if err := rows.Err(); err != nil {
		return Info{}, fmt.Errorf("%s: %w", path, err)
	}
	rows.Close()
	if first != "ok" {
		return Info{}, fmt.Errorf("%s fails integrity_check: %s", path, first)
	}
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master`).Scan(&tables); err != nil {
		return Info{}, fmt.Errorf("%s: %w", path, err)
	}
	if tables == 0 {
		return Info{}, fmt.Errorf("%s has no schema", path)
	}
	return Info{Bytes: st.Size(), Pages: pages}, nil
}

// Plausible refuses an empty copy, and one far under the last good size prev
// (0 = none: the first copy has nothing to be compared with).
func Plausible(n, prev int64) error {
	if n <= 0 {
		return fmt.Errorf("the copy is empty")
	}
	if prev > 0 && n < prev/ShrinkFloor {
		return fmt.Errorf("the copy is %d bytes, under 1/%d of the last good one (%d bytes)", n, ShrinkFloor, prev)
	}
	return nil
}

// Copy copies src into dst (mode 0600), which it replaces.
func Copy(ctx context.Context, src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + tmpSuffix
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, ctxReader{ctx, in})
	if err == nil {
		err = out.Sync()
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, dst)
	}
	if err != nil {
		os.Remove(tmp)
	}
	return err
}

// publish makes tmp, a finished copy, dst: owner-only, on disk, then named.
func publish(tmp, dst string) error {
	if err := os.Chmod(tmp, 0o600); err != nil {
		return err
	}
	f, err := os.Open(tmp)
	if err != nil {
		return err
	}
	err = f.Sync()
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		return err
	}
	return SyncDir(filepath.Dir(dst))
}

// SyncDir makes a rename in dir durable.
func SyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// ctxReader stops a long copy when ctx ends.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

// List is dir's copies named prefix*suffix, oldest first; a missing directory
// has none. Leftovers (*.tmp) are not copies.
func List(dir, prefix, suffix string) ([]string, error) {
	des, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, de := range des {
		if n := de.Name(); strings.HasPrefix(n, prefix) && strings.HasSuffix(n, suffix) && !strings.HasSuffix(n, tmpSuffix) {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out, nil
}

// Prune keeps the newest keep copies named prefix*suffix and removes the
// rest. keep below 1 is treated as 1: retention never removes the newest
// copy.
func Prune(dir, prefix, suffix string, keep int) error {
	names, err := List(dir, prefix, suffix)
	if err != nil {
		return err
	}
	var errs []error
	for len(names) > max(keep, 1) {
		errs = append(errs, os.Remove(filepath.Join(dir, names[0])))
		names = names[1:]
	}
	return errors.Join(errs...)
}

// KeepOnly removes every copy named prefix*suffix except name, which must be
// there (the guard against ending with no copy at all). A file that cannot be
// removed is an error: the caller must know a copy survived.
func KeepOnly(dir, prefix, suffix, name string) error {
	if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
		return fmt.Errorf("the copy to keep is not there: %w", err)
	}
	names, err := List(dir, prefix, suffix)
	if err != nil {
		return err
	}
	var errs []error
	for _, n := range names {
		if n != name {
			errs = append(errs, os.Remove(filepath.Join(dir, n)))
		}
	}
	return errors.Join(errs...)
}

// Sweep removes leftovers (*.tmp) named prefix*: a run cut off by a crash. A
// leftover holds the same text a copy does, so nothing may keep one. The
// caller holds whatever keeps another run out of dir.
func Sweep(dir, prefix string) error {
	des, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var errs []error
	for _, de := range des {
		if n := de.Name(); strings.HasPrefix(n, prefix) && strings.HasSuffix(n, tmpSuffix) {
			errs = append(errs, os.Remove(filepath.Join(dir, n)))
		}
	}
	return errors.Join(errs...)
}
