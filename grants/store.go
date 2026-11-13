package grants

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/kit/store"
)

// Migrations are the grants module's tables (New adds them).
var Migrations = []store.Migration{
	{Kind: store.KindProject, Name: "grants/0001_secret_grants", Script: ddl},
}

func init() {
	// A project's export carries no secret name: the grant rows stay, their
	// names blanked (the events naming them are dropped by kit/events).
	store.RegisterExportFilter(func(ctx context.Context, db *sql.DB, _ string) error {
		_, err := db.ExecContext(ctx, `UPDATE secret_grants SET name = '' WHERE name <> ''`)
		return err
	})
}

// secret_grants is the table (project.db of the pool project that owns the
// boxes); secret_grant_settings holds the defaults' switch ("default" =
// "off" when the owner turned them off; no row is on).
const ddl = `
CREATE TABLE secret_grants (
  id          TEXT PRIMARY KEY,
  area        TEXT NOT NULL DEFAULT '',
  to_json     TEXT NOT NULL,
  kind        TEXT NOT NULL,
  name        TEXT NOT NULL,
  ceiling     TEXT NOT NULL DEFAULT '{}',
  host_class  TEXT NOT NULL,
  scopes      TEXT NOT NULL,
  targets     TEXT NOT NULL DEFAULT '[]',
  created_by  TEXT NOT NULL DEFAULT '',
  created     TEXT NOT NULL
);
CREATE INDEX secret_grants_area ON secret_grants (area);
CREATE TABLE secret_grant_settings (key TEXT PRIMARY KEY, value TEXT NOT NULL);
`

// db is what the table's functions run on: the database or a transaction.
type db interface {
	ExecContext(ctx context.Context, q string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, q string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, q string, args ...any) *sql.Row
}

const cols = `id, area, to_json, kind, name, ceiling, host_class, scopes, targets, created_by, created`

// scanGrant reads a row. A row whose JSON does not decode is answered with
// Corrupt set (it lists, so an admin can see and remove it; it matches
// nothing).
func scanGrant(sc interface{ Scan(...any) error }) (computeapi.SecretGrant, error) {
	var g computeapi.SecretGrant
	var to, ceiling, scopes, targets string
	if err := sc.Scan(&g.ID, &g.Area, &to, &g.Kind, &g.Name, &ceiling, &g.HostClass, &scopes, &targets, &g.CreatedBy, &g.Created); err != nil {
		return g, err
	}
	if json.Unmarshal([]byte(to), &g.To) != nil || json.Unmarshal([]byte(targets), &g.Targets) != nil || !json.Valid([]byte(ceiling)) {
		g.Corrupt = true
	}
	if json.Valid([]byte(ceiling)) {
		g.Ceiling = json.RawMessage(ceiling)
	}
	if scopes != "" {
		g.Scopes = strings.Split(scopes, ",")
	}
	return g, nil
}

// load reads every row matching where (oldest first) and the defaults'
// switch.
func load(ctx context.Context, q db, where string, args ...any) ([]computeapi.SecretGrant, bool, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+cols+` FROM secret_grants WHERE `+where+` ORDER BY created, id`, args...)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	var out []computeapi.SecretGrant
	for rows.Next() {
		g, err := scanGrant(rows)
		if err != nil {
			return nil, false, err
		}
		out = append(out, g)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	on, err := defaultOn(ctx, q)
	return out, on, err
}

func defaultOn(ctx context.Context, q db) (bool, error) {
	var v string
	err := q.QueryRowContext(ctx, `SELECT value FROM secret_grant_settings WHERE key='default'`).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return v != "off", nil
}

func insert(ctx context.Context, q db, g computeapi.SecretGrant) error {
	to, err := json.Marshal(g.To)
	if err != nil {
		return err
	}
	targets := []byte("[]")
	if g.Targets != nil {
		if targets, err = json.Marshal(g.Targets); err != nil {
			return err
		}
	}
	_, err = q.ExecContext(ctx, `INSERT INTO secret_grants (`+cols+`) VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		g.ID, g.Area, string(to), g.Kind, g.Name, string(g.Ceiling), g.HostClass, strings.Join(g.Scopes, ","), string(targets), g.CreatedBy, g.Created)
	return err
}

func byID(ctx context.Context, q db, id string) (computeapi.SecretGrant, bool, error) {
	g, err := scanGrant(q.QueryRowContext(ctx, `SELECT `+cols+` FROM secret_grants WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return g, false, nil
	}
	return g, err == nil, err
}

func remove(ctx context.Context, q db, id string) error {
	_, err := q.ExecContext(ctx, `DELETE FROM secret_grants WHERE id=?`, id)
	return err
}

func setDefault(ctx context.Context, q db, on bool) error {
	if on {
		_, err := q.ExecContext(ctx, `DELETE FROM secret_grant_settings WHERE key='default'`)
		return err
	}
	_, err := q.ExecContext(ctx, `INSERT INTO secret_grant_settings (key, value) VALUES ('default', 'off')
  ON CONFLICT(key) DO UPDATE SET value='off'`)
	return err
}
