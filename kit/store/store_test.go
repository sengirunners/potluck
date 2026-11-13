package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/ids"
)

func init() {
	RegisterRegistry("storetest/0001_init", `CREATE TABLE reg_t (k TEXT PRIMARY KEY, v TEXT NOT NULL);`)
	RegisterProject("storetest/0001_init", `CREATE TABLE proj_t (k TEXT PRIMARY KEY, area TEXT NOT NULL DEFAULT '');`)
	RegisterProject("storetest/0002_more", `ALTER TABLE proj_t ADD COLUMN v TEXT NOT NULL DEFAULT '';`)
}

func TestOpenMigrateReopen(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	s, err := Open(ctx, home)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Registry().ExecContext(ctx, `INSERT INTO reg_t VALUES ('a', 'b')`); err != nil {
		t.Fatal(err)
	}
	p1, p2 := ids.New(), ids.New()
	for _, p := range []string{p1, p2} {
		db, err := s.CreateProject(ctx, p)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO proj_t (k, area, v) VALUES ('x', 'sprout', 'y')`); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.CreateProject(ctx, "not-a-ulid"); err == nil {
		t.Fatal("bad id accepted")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(ctx, home); err != nil { // idempotent
		t.Fatal(err)
	}
	s, err = Open(ctx, home)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if got := len(s.Projects(authz.All(), authz.Read)); got != 2 {
		t.Fatalf("reopened %d projects", got)
	}
	var n int
	db, _ := s.Project(authz.All(), authz.Read, p2)
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM schema_migrations`).Scan(&n); err != nil || n != len(Registered(KindProject)) {
		t.Fatalf("project migrations %d %v", n, err)
	}
}

func TestProjectScope(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	orchid, side := ids.New(), ids.New()
	for _, p := range []string{orchid, side} {
		if _, err := s.CreateProject(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	guest := authz.FromTuples([]authz.Tuple{{Relation: authz.Guest, Object: authz.AreaObject(orchid, "sprout")}}, nil)
	if _, err := s.Project(guest, authz.Read, orchid); err != nil {
		t.Fatal(err)
	}
	_, hidden := s.Project(guest, authz.Read, side)
	_, missing := s.Project(guest, authz.Read, side+"X")
	if !api.IsCode(hidden, api.CodeNotFound) || hidden.Error() != api.NotFound("project", side).Error() || missing == nil {
		t.Fatalf("hidden %v missing %v", hidden, missing)
	}
	if _, err := s.Project(guest, authz.Operate, orchid); err == nil {
		t.Fatal("guest got operate")
	}
	if ps := s.Projects(guest, authz.Read); len(ps) != 1 || ps[0].ID != orchid {
		t.Fatalf("projects %v", ps)
	}
	dst := t.TempDir() + "/copy.db"
	if err := s.CopyProject(ctx, orchid, dst); err != nil {
		t.Fatal(err)
	}
	cp, err := sql.Open("sqlite", dst)
	if err != nil {
		t.Fatal(err)
	}
	defer cp.Close()
	if err := cp.QueryRow(`SELECT count(*) FROM proj_t`).Scan(new(int)); err != nil {
		t.Fatal(err)
	}
}

func TestRegisterPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("no panic on duplicate")
		}
	}()
	RegisterProject("storetest/0001_init", "SELECT 1")
}

// A write on a team file's reader pool (mail, comments: not yet on the
// writer) waits behind the writer's transaction for the solo busy timeout,
// not TeamBusyTimeout: a lease transaction longer than 200 ms must not make
// a mail commit fail "database is locked".
func TestTeamReaderWriteWaitsForWriter(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	p := ids.New()
	if _, err := s.CreateProject(ctx, p); err != nil {
		t.Fatal(err)
	}
	if err := s.SetTeam(ctx, p, true); err != nil {
		t.Fatal(err)
	}
	tx, err := s.Writer(p).BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO proj_t (k) VALUES ('w')`); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		time.Sleep(3 * TeamBusyTimeout)
		_ = tx.Commit()
		close(done)
	}()
	db, err := s.Project(authz.All(), authz.Write, p)
	if err != nil {
		t.Fatal(err)
	}
	err = Tx(ctx, db, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO proj_t (k) VALUES ('r')`)
		return err
	})
	<-done
	if err != nil {
		t.Fatalf("a reader-side write behind a %v writer transaction: %v", 3*TeamBusyTimeout, err)
	}
}

// A replica's copy: the outbox's connection waits on the disk at commit
// (FULL), the one for what can be had again does not (NORMAL).
func TestReplicaSyncLevels(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "copy.db")
	full, err := OpenReplica(ctx, path, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer full.Close()
	apply, err := OpenReplicaApply(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer apply.Close()
	for _, c := range []struct {
		db   *sql.DB
		want int
	}{{full, 2}, {apply, 1}} {
		var n int
		if err := c.db.QueryRowContext(ctx, `PRAGMA synchronous`).Scan(&n); err != nil || n != c.want {
			t.Fatalf("synchronous %d %v, want %d", n, err, c.want)
		}
	}
}

// A module's migrations reach the databases already open, every project
// created later, and the build's schema; the base never lists them.
func TestAddMigrations(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	before, team := ids.New(), ids.New()
	for _, p := range []string{before, team} {
		if _, err := s.CreateProject(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.SetTeam(ctx, team, true); err != nil {
		t.Fatal(err)
	}
	ms := []Migration{
		{Kind: KindRegistry, Name: "addtest/0001_reg", Script: `CREATE TABLE add_reg (k TEXT PRIMARY KEY);`},
		{Kind: KindProject, Name: "addtest/0001_proj", Script: `CREATE TABLE add_proj (k TEXT PRIMARY KEY);`},
	}
	if err := s.AddMigrations(ctx, ms...); err != nil {
		t.Fatal(err)
	}
	if err := s.AddMigrations(ctx, ms...); err != nil { // the same again: nothing
		t.Fatal(err)
	}
	after := ids.New()
	if _, err := s.CreateProject(ctx, after); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Registry().ExecContext(ctx, `INSERT INTO add_reg VALUES ('a')`); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{before, team, after} {
		if _, err := s.Writer(p).ExecContext(ctx, `INSERT INTO add_proj VALUES ('a')`); err != nil {
			t.Fatalf("project %s: %v", p, err)
		}
	}
	if err := s.SetTeam(ctx, team, false); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(s.Migrations(KindProject), "addtest/0001_proj") || slices.Contains(Registered(KindProject), "addtest/0001_proj") {
		t.Fatalf("schema %v, base %v", s.Migrations(KindProject), Registered(KindProject))
	}
	other, err := Open(ctx, t.TempDir()) // another daemon's store: not wired with them
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if slices.Contains(other.Migrations(KindRegistry), "addtest/0001_reg") {
		t.Fatal("a migration added to one store reached another")
	}
	if _, err := other.Registry().ExecContext(ctx, `INSERT INTO add_reg VALUES ('a')`); err == nil {
		t.Fatal("another store has the added table")
	}
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("a name reused for another script was accepted")
			}
		}()
		_ = s.AddMigrations(ctx, Migration{Kind: KindProject, Name: "addtest/0001_proj", Script: `CREATE TABLE x (k TEXT);`})
	}()
}
