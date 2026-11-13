// Package store owns every SQLite handle the daemon has: registry.db (one per
// machine) and one project.db per project. Owned by the integrator.
//
// Layout ($ZM_HOME):
//
//	registry.db                  projects, identities, grants, tokens, sessions, leases, machine-level events
//	projects/<ulid>/project.db   that project's mail, local work items, comments and events
//	projects/<ulid>/project.toml that project's config (kit/project)
//
// A module adds its migrations when its daemon's wiring builds it, against
// one of the kinds (Migration, AddMigrations in migrations.go):
//
//	var migrations = []store.Migration{
//		{Kind: store.KindRegistry, Name: "usage/0001_init", Script: `CREATE TABLE usage (...);`},
//		{Kind: store.KindProject, Name: "grants/0001_init", Script: `CREATE TABLE grants (...);`},
//	}
//	func New(d *core.Deps) *Service { d.AddMigrations(migrations...); ... }
//
// The shell's own tables (kit/project, kit/events), which exist before any
// module is wired, are the base: registered from init() with
// RegisterRegistry and RegisterProject (a program on Potluck may still
// register its own modules' there too).
//
// Names are "<module>/<NNNN>_<what>"; per database, every migration of its
// kind runs once, in name order, each in its own transaction. A daemon runs
// the shell's first, then the base's and its modules' together in one name
// order pass once its wiring has built (Migrate). Never edit a migration
// that has shipped; add a new one.
//
// There is no cross-file transaction (modernc sqlite cannot ATTACH-commit
// atomically under WAL). A write that touches both files commits the
// authoritative row first and treats the other as derived (SPEC "Storage").
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/ids"
)

// Kind is which database a migration targets.
type Kind int

const (
	KindRegistry Kind = iota // registry.db
	KindProject              // every projects/<ulid>/project.db
	KindReplica              // a team replica's copy of a joined project: replicas/<ulid>.db (SPEC 17.6)
)

// File names.
const (
	FileRegistry = "registry.db"
	DirProjects  = "projects"
	FileProject  = "project.db"
)

var (
	regMu      sync.Mutex
	migrations = map[Kind]map[string]string{KindRegistry: {}, KindProject: {}, KindReplica: {}}
)

// RegisterRegistry adds a registry.db migration to the base. Panics on a bad
// name, an empty script or a duplicate (programming errors caught at
// start-up). A module uses AddMigrations through its wiring instead.
func RegisterRegistry(name, script string) { register(KindRegistry, name, script) }

// RegisterProject adds a project.db migration (applied to every project).
func RegisterProject(name, script string) { register(KindProject, name, script) }

// RegisterReplica adds a migration for a replica's copy (internal/team).
func RegisterReplica(name, script string) { register(KindReplica, name, script) }

// OpenReplica opens (creating) a replica's copy at path: WAL, IMMEDIATE
// write transactions and synchronous FULL, because its outbox holds writes
// the authority has not seen yet (a write acknowledged to an agent must
// survive a power cut). conns bounds the pool.
func OpenReplica(ctx context.Context, path string, conns int) (*sql.DB, error) {
	return openReplica(ctx, path, base(KindReplica), conns)
}

func openReplica(ctx context.Context, path string, scripts map[string]string, conns int) (*sql.DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	// A copy is walked whole by searches. Mapped, its pages cost no read()
	// each and no private memory; through the page cache, a walk over 11.5k
	// items spent most of its time in pread.
	return openDBSync(ctx, path, scripts, conns, soloBusyTimeout, "FULL", "mmap_size(1073741824)")
}

// OpenReplicaApply opens the same copy for what the replica applies from its
// authority (snapshots, changes, items fetched whole): one connection,
// synchronous NORMAL. Those rows can always be fetched again, so their
// commits need not wait for the disk; a power cut can only take the copy
// back to an earlier consistent state, cursor included (WAL), and the next
// sync brings it forward again. The outbox stays on OpenReplica's FULL, and
// its next commit makes everything before it durable too.
func OpenReplicaApply(ctx context.Context, path string) (*sql.DB, error) {
	return openReplicaApply(ctx, path, base(KindReplica))
}

func openReplicaApply(ctx context.Context, path string, scripts map[string]string) (*sql.DB, error) {
	return openDBSync(ctx, path, scripts, 1, soloBusyTimeout, "NORMAL", "mmap_size(1073741824)")
}

func register(k Kind, name, script string) {
	regMu.Lock()
	defer regMu.Unlock()
	if !strings.Contains(name, "/") || strings.TrimSpace(script) == "" {
		panic("store: want migration name <module>/<NNNN>_<what> and a script, got " + name)
	}
	if _, dup := migrations[k][name]; dup {
		panic("store: duplicate migration " + name)
	}
	migrations[k][name] = script
}

// Registered lists a kind's base migration names in apply order
// (Store.Migrations adds what the wiring added).
func Registered(k Kind) []string {
	regMu.Lock()
	defer regMu.Unlock()
	names := make([]string, 0, len(migrations[k]))
	for n := range migrations[k] {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Store holds every handle. Safe for concurrent use.
type Store struct {
	home string
	reg  *sql.DB

	mu       sync.RWMutex
	projects map[string]*sql.DB // solo: the project's 2-connection pool; team: its 4 readers
	writers  map[string]*sql.DB // team projects only: the one writer connection (SPEC 17.3)
	retired  []*sql.DB          // handles replaced by SetTeam, closed at Close (in-flight requests finish on them)

	addMu    sync.Mutex
	added    map[Kind]map[string]string // AddMigrations (migrations.go)
	deferred bool                       // OpenForWiring until Migrate: databases get the shell's migrations alone
}

// Team file pool (SPEC 17.3): one writer connection and four readers, WAL,
// and a busy_timeout below the claim budget (250 ms over the tailnet).
const (
	TeamReaders     = 4
	TeamBusyTimeout = 200 * time.Millisecond
	soloBusyTimeout = 5 * time.Second
)

// Open opens (creating) $home/registry.db and every existing project.db,
// migrating each (OpenForWiring, then Migrate). Tests pass a temp dir, never
// ~/.zm.
func Open(ctx context.Context, home string) (*Store, error) {
	s, err := OpenForWiring(ctx, home)
	if err != nil {
		return nil, err
	}
	if err := s.Migrate(ctx); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

// OpenForWiring opens the same databases with only the shell's migrations
// applied (Shell: the tables a daemon reads while it builds its wiring).
// The rest wait for Migrate, which the daemon calls once its wiring has
// added its modules' (migrations.go).
func OpenForWiring(ctx context.Context, home string) (*Store, error) {
	if err := os.MkdirAll(filepath.Join(home, DirProjects), 0o700); err != nil {
		return nil, err
	}
	s := &Store{home: home, projects: map[string]*sql.DB{}, writers: map[string]*sql.DB{}, deferred: true}
	reg, err := openDB(ctx, filepath.Join(home, FileRegistry), s.opening(KindRegistry), 4, soloBusyTimeout)
	if err != nil {
		return nil, err
	}
	s.reg = reg
	team := teamIDs(ctx, reg)
	ents, err := os.ReadDir(filepath.Join(home, DirProjects))
	if err != nil {
		s.Close()
		return nil, err
	}
	for _, e := range ents {
		if !e.IsDir() || !ids.Valid(e.Name()) {
			continue
		}
		p := filepath.Join(home, DirProjects, e.Name(), FileProject)
		if _, err := os.Stat(p); err != nil {
			continue
		}
		if team[e.Name()] {
			w, r, err := openTeam(ctx, p, s.opening(KindProject))
			if err != nil {
				s.Close()
				return nil, err
			}
			s.writers[e.Name()], s.projects[e.Name()] = w, r
			continue
		}
		db, err := openDB(ctx, p, s.opening(KindProject), 2, soloBusyTimeout)
		if err != nil {
			s.Close()
			return nil, err
		}
		s.projects[e.Name()] = db
	}
	return s, nil
}

// teamIDs reads which projects are team projects (projects.team, a column
// kit/project adds) so their files open with the team pool. A registry
// without the column yet (first start) has none.
func teamIDs(ctx context.Context, reg *sql.DB) map[string]bool {
	out := map[string]bool{}
	rows, err := reg.QueryContext(ctx, `SELECT id FROM projects WHERE team = 1`)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			out[id] = true
		}
	}
	return out
}

// openTeam opens a team project's file: one writer connection with
// TeamBusyTimeout (claims, leases, their events) and TeamReaders readers.
// The readers still open transactions with BEGIN IMMEDIATE, and other
// modules' writes (mail, comments, status, local items) commit on them
// (moving those onto the writer is round C), so "one writer" is a
// convention: such a write takes the file's write lock like the writer
// does. A claim waits at most TeamBusyTimeout behind one (they are short)
// and otherwise fails "database is locked"; a reader-side write waits the
// solo 5 s behind the writer rather than failing at 200 ms.
func openTeam(ctx context.Context, path string, scripts map[string]string) (writer, readers *sql.DB, err error) {
	if writer, err = openDB(ctx, path, scripts, 1, TeamBusyTimeout); err != nil {
		return nil, nil, err
	}
	if readers, err = openDB(ctx, path, scripts, TeamReaders, soloBusyTimeout); err != nil {
		writer.Close()
		return nil, nil, err
	}
	return writer, readers, nil
}

// SetTeam reopens project id's file with the team pool (one writer, four
// readers) or back with the solo pool. kit/project calls it when a
// project becomes team or solo (zm project team|solo). The old handles stay
// open until Close, so requests already holding them finish.
func (s *Store) SetTeam(ctx context.Context, id string, team bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	old, ok := s.projects[id]
	if !ok {
		return fmt.Errorf("store: no project %s", id)
	}
	_, isTeam := s.writers[id]
	if isTeam == team {
		return nil
	}
	path := filepath.Join(s.ProjectDir(id), FileProject)
	if team {
		w, r, err := openTeam(ctx, path, s.opening(KindProject))
		if err != nil {
			return err
		}
		s.retired = append(s.retired, old)
		s.writers[id], s.projects[id] = w, r
		return nil
	}
	db, err := openDB(ctx, path, s.opening(KindProject), 2, soloBusyTimeout)
	if err != nil {
		return err
	}
	s.retired = append(s.retired, old, s.writers[id])
	delete(s.writers, id)
	s.projects[id] = db
	return nil
}

// IsTeam reports whether project id's file has the team pool.
func (s *Store) IsTeam(id string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.writers[id]
	return ok
}

// Writer is the handle writes to project id go through when they must be
// ordered: a team project's single writer connection (leases, claims and
// their events commit there in one transaction, SPEC 17.3); a solo
// project's ordinary pool. nil for an unknown project. Daemon-internal: it
// takes no scope, so callers check the caller first.
func (s *Store) Writer(id string) *sql.DB {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if w, ok := s.writers[id]; ok {
		return w
	}
	return s.projects[id]
}

// TeamProjects lists the team projects' ids, sorted.
func (s *Store) TeamProjects() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, 0, len(s.writers))
	for id := range s.writers {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// Migrate opens and closes every database under home, applying migrations.
func Migrate(ctx context.Context, home string) error {
	s, err := Open(ctx, home)
	if err != nil {
		return err
	}
	return s.Close()
}

// Home is the directory the store lives in.
func (s *Store) Home() string { return s.home }

// Optimize runs PRAGMA optimize on every database (SQLite's advice for
// long-lived connections: refresh the planner's statistics now and then).
func (s *Store) Optimize(ctx context.Context) {
	s.mu.RLock()
	dbs := make([]*sql.DB, 0, len(s.projects)+1)
	if s.reg != nil {
		dbs = append(dbs, s.reg)
	}
	for _, db := range s.projects {
		dbs = append(dbs, db)
	}
	for _, db := range s.writers {
		dbs = append(dbs, db)
	}
	s.mu.RUnlock()
	for _, db := range dbs {
		_, _ = db.ExecContext(ctx, `PRAGMA optimize`)
	}
}

// Close closes every handle.
func (s *Store) Close() error {
	s.Optimize(context.Background())
	s.mu.Lock()
	defer s.mu.Unlock()
	var errs []error
	for id, db := range s.projects {
		errs = append(errs, db.Close())
		delete(s.projects, id)
	}
	for id, db := range s.writers {
		errs = append(errs, db.Close())
		delete(s.writers, id)
	}
	for _, db := range s.retired {
		errs = append(errs, db.Close())
	}
	s.retired = nil
	if s.reg != nil {
		errs = append(errs, s.reg.Close())
		s.reg = nil
	}
	return errors.Join(errs...)
}

// Registry is registry.db. Every query on a table with a project column that
// returns rows to a caller must filter with the caller's Scope.SQL.
func (s *Store) Registry() *sql.DB { return s.reg }

// ProjectDir is projects/<ulid>.
func (s *Store) ProjectDir(id string) string { return filepath.Join(s.home, DirProjects, id) }

// CreateProject creates and migrates projects/<id>/project.db. Only
// kit/project calls it.
func (s *Store) CreateProject(ctx context.Context, id string) (*sql.DB, error) {
	if !ids.Valid(id) {
		return nil, fmt.Errorf("store: bad project id %q", id)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if db, ok := s.projects[id]; ok {
		return db, nil
	}
	if err := os.MkdirAll(s.ProjectDir(id), 0o700); err != nil {
		return nil, err
	}
	db, err := openDB(ctx, filepath.Join(s.ProjectDir(id), FileProject), s.opening(KindProject), 2, soloBusyTimeout)
	if err != nil {
		return nil, err
	}
	s.projects[id] = db
	return db, nil
}

// Project returns a project's database if sc may do a somewhere in it, else
// api.NotFound("project", id), identical to a missing project. Area-level
// filtering is still the caller's job: use sc.AreaSQL on every query.
func (s *Store) Project(sc authz.Scope, a authz.Action, id string) (*sql.DB, error) {
	s.mu.RLock()
	db, ok := s.projects[id]
	s.mu.RUnlock()
	if !ok || !sc.Role(id).Allows(a) {
		return nil, api.NotFound("project", id)
	}
	return db, nil
}

// ProjectDB is one project's handle.
type ProjectDB struct {
	ID string
	DB *sql.DB
}

// Projects lists, sorted by id, the projects in which sc may do a. Use it for
// union reads ("zm inbox" across every project the caller sees).
func (s *Store) Projects(sc authz.Scope, a authz.Action) []ProjectDB {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]ProjectDB, 0, len(s.projects))
	for id, db := range s.projects {
		if sc.Role(id).Allows(a) {
			out = append(out, ProjectDB{ID: id, DB: db})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// CopyProject writes a consistent copy of a project's database to dst
// (VACUUM INTO), then runs each scrub on the copy (opened on its own, no
// pragmas), so what must not leave the daemon is gone before it is handed
// over.
func (s *Store) CopyProject(ctx context.Context, id, dst string, scrub ...func(context.Context, *sql.DB) error) error {
	db, err := s.Project(authz.All(), authz.Read, id)
	if err != nil {
		return err
	}
	if _, err = db.ExecContext(ctx, `VACUUM INTO ?`, dst); err != nil || len(scrub) == 0 {
		return err
	}
	conn, err := sqlite.NewConnector("file:" + dst)
	if err != nil {
		return err
	}
	cp := sql.OpenDB(conn)
	defer cp.Close()
	for _, f := range scrub {
		if err := f(ctx, cp); err != nil {
			_ = os.Remove(dst)
			return err
		}
	}
	return nil
}

// openDB opens path with WAL, synchronous=NORMAL (durable at checkpoints,
// never corrupt), a busy timeout (5 s; a team file's is TeamBusyTimeout),
// foreign keys and IMMEDIATE write transactions, then applies scripts (a
// kind's migrations by name) in name order.
func openDB(ctx context.Context, path string, scripts map[string]string, maxConns int, busy time.Duration) (*sql.DB, error) {
	return openDBSync(ctx, path, scripts, maxConns, busy, "NORMAL")
}

func openDBSync(ctx context.Context, path string, scripts map[string]string, maxConns int, busy time.Duration, sync string, pragmas ...string) (*sql.DB, error) {
	q := url.Values{}
	for _, p := range append([]string{"journal_mode(WAL)", fmt.Sprintf("busy_timeout(%d)", busy.Milliseconds()), "foreign_keys(1)", "synchronous(" + sync + ")"}, pragmas...) {
		q.Add("_pragma", p)
	}
	q.Set("_txlock", "immediate")
	base, err := sqlite.NewConnector("file:" + path + "?" + q.Encode())
	if err != nil {
		return nil, err
	}
	db := sql.OpenDB(cachingConnector{base}) // prepared statements per connection (stmtcache.go)
	// SQLite allows one writer; a small pool keeps readers concurrent under
	// WAL. Every connection stays open (idle ones included): a new one pays
	// the open, the pragmas and a cold statement cache.
	db.SetMaxOpenConns(maxConns)
	db.SetMaxIdleConns(maxConns)
	db.SetConnMaxLifetime(0)
	db.SetConnMaxIdleTime(0)
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	_ = os.Chmod(path, 0o600)
	if err := migrate(ctx, db, scripts); err != nil {
		db.Close()
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	// Planner statistics for tables that need them (cheap when none do);
	// Optimize refreshes them on a long-running daemon.
	_, _ = db.ExecContext(ctx, `PRAGMA optimize=0x10002`)
	return db, nil
}

func migrate(ctx context.Context, db *sql.DB, scripts map[string]string) error {
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		name TEXT PRIMARY KEY, applied_at INTEGER NOT NULL)`); err != nil {
		return err
	}
	// Read what is applied once: a daemon start opens every project.db, and
	// one transaction per registered migration per file adds up.
	applied := map[string]bool{}
	rows, err := db.QueryContext(ctx, `SELECT name FROM schema_migrations`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			return err
		}
		applied[n] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, name := range slices.Sorted(maps.Keys(scripts)) {
		if applied[name] {
			continue
		}
		script := scripts[name]
		err := Tx(ctx, db, func(tx *sql.Tx) error {
			var n int
			if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM schema_migrations WHERE name = ?`, name).Scan(&n); err != nil || n > 0 {
				return err
			}
			if _, err := tx.ExecContext(ctx, script); err != nil {
				return err
			}
			_, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations (name, applied_at) VALUES (?, ?)`, name, Millis(time.Now()))
			return err
		})
		if err != nil {
			return fmt.Errorf("migration %s: %w", name, err)
		}
	}
	return nil
}

// Tx runs fn in a transaction, committing on nil and rolling back otherwise.
// Transactions begin IMMEDIATE, so a read-then-write inside fn is atomic.
func Tx(ctx context.Context, db *sql.DB, fn func(*sql.Tx) error) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// Millis is t as Unix milliseconds, the storage format for every time column.
func Millis(t time.Time) int64 { return t.UnixMilli() }

// FromMillis turns a stored time back into a time.Time (UTC). 0 is the zero time.
func FromMillis(ms int64) time.Time {
	if ms == 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms).UTC()
}

// NullMillis turns a nullable stored time into a *time.Time.
func NullMillis(n sql.NullInt64) *time.Time {
	if !n.Valid {
		return nil
	}
	t := FromMillis(n.Int64)
	return &t
}

// IsUnique reports whether err is a UNIQUE or PRIMARY KEY constraint violation.
func IsUnique(err error) bool {
	var se *sqlite.Error
	if errors.As(err, &se) {
		c := se.Code()
		return c == sqlite3.SQLITE_CONSTRAINT_UNIQUE || c == sqlite3.SQLITE_CONSTRAINT_PRIMARYKEY
	}
	return false
}

// Compact rewrites project id's database and the registry without free
// pages and truncates their write-ahead logs, so text a redaction overwrote
// is gone from the files too (SQLite keeps freed pages and old frames).
func (s *Store) Compact(ctx context.Context, id string) error {
	var errs []error
	for _, db := range []*sql.DB{s.Writer(id), s.reg} {
		if db == nil {
			continue
		}
		if _, err := db.ExecContext(ctx, `VACUUM`); err != nil {
			errs = append(errs, err)
			continue
		}
		var busy, logFrames, done int
		if err := db.QueryRowContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`).Scan(&busy, &logFrames, &done); err != nil {
			errs = append(errs, err)
		} else if busy != 0 {
			errs = append(errs, fmt.Errorf("checkpoint busy (%d of %d frames folded)", done, logFrames))
		}
	}
	return errors.Join(errs...)
}
