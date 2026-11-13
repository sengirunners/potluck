package store

import (
	"context"
	"database/sql"
	"errors"
	"maps"
	"slices"
	"strings"
)

// Migration is one migration of one kind of database. Name is
// "<module>/<NNNN>_<what>"; every migration of a kind runs once per
// database, in name order, each in its own transaction. Never edit one that
// has shipped; add a new one.
type Migration struct {
	Kind   Kind
	Name   string
	Script string
}

// The base is what is registered from init() (RegisterRegistry,
// RegisterProject, RegisterReplica): the tables kit/project and kit/events
// need before any module is wired, and whatever a program on Potluck still
// registers there for its own modules. A module brings its own through its
// daemon's wiring instead (AddMigrations).

// shell are the modules whose tables a daemon reads while it builds its
// wiring (kit/events and kit/project: the event log, the projects). Their
// migrations depend on no module's, so they run first; everything else
// waits for Migrate.
var shell = []string{"events/", "project/"}

// Shell reports whether migration name belongs to the shell (OpenForWiring
// applies these alone).
func Shell(name string) bool {
	return slices.ContainsFunc(shell, func(p string) bool { return strings.HasPrefix(name, p) })
}

// base is kind k's registered migrations by name (a copy).
func base(k Kind) map[string]string {
	regMu.Lock()
	defer regMu.Unlock()
	return maps.Clone(migrations[k])
}

// scripts is kind k's migrations for s's databases: the base plus what
// AddMigrations added.
func (s *Store) scripts(k Kind) map[string]string {
	out := base(k)
	s.addMu.Lock()
	defer s.addMu.Unlock()
	maps.Copy(out, s.added[k])
	return out
}

// opening is kind k's migrations a database s opens now gets: the shell's
// alone until Migrate, then all of them.
func (s *Store) opening(k Kind) map[string]string {
	if !s.waiting() {
		return s.scripts(k)
	}
	out := base(k)
	maps.DeleteFunc(out, func(name, _ string) bool { return !Shell(name) })
	return out
}

// Migrations lists kind k's migration names for s's databases, in apply
// order: the base and what the wired modules added (the build manifest's
// schema).
func (s *Store) Migrations(k Kind) []string {
	names := slices.Sorted(maps.Keys(s.scripts(k)))
	if names == nil {
		names = []string{} // a kind with none is [] in the manifest, never null
	}
	return names
}

// AddMigrations makes ms part of s's schema: each runs on every open
// database of its kind (registry.db, every project.db) that has not had it,
// and on every database of its kind s opens later (CreateProject, SetTeam,
// s.OpenReplica). On a store from Open they run now; on one from
// OpenForWiring they wait for Migrate. A module's New adds its own through
// core.Deps.AddMigrations, so a daemon has the tables of the modules its
// wiring builds and no others.
//
// Adding a migration again with the same script does nothing (a module
// built twice on one daemon). A bad name, an empty script, or a name already
// used for another script panics: those are programming errors.
func (s *Store) AddMigrations(ctx context.Context, ms ...Migration) error {
	kinds := map[Kind]bool{}
	s.addMu.Lock()
	for _, m := range ms {
		if m.Kind != KindRegistry && m.Kind != KindProject && m.Kind != KindReplica {
			s.addMu.Unlock()
			panic("store: migration " + m.Name + " has no database kind")
		}
		if !strings.Contains(m.Name, "/") || strings.TrimSpace(m.Script) == "" {
			s.addMu.Unlock()
			panic("store: want migration name <module>/<NNNN>_<what> and a script, got " + m.Name)
		}
		old, ok := s.added[m.Kind][m.Name]
		if !ok {
			regMu.Lock()
			old, ok = migrations[m.Kind][m.Name]
			regMu.Unlock()
		}
		if ok {
			if old != m.Script {
				s.addMu.Unlock()
				panic("store: duplicate migration " + m.Name)
			}
			continue
		}
		if s.added == nil {
			s.added = map[Kind]map[string]string{}
		}
		if s.added[m.Kind] == nil {
			s.added[m.Kind] = map[string]string{}
		}
		s.added[m.Kind][m.Name] = m.Script
		kinds[m.Kind] = true
	}
	deferred := s.deferred
	s.addMu.Unlock()
	if deferred {
		return nil
	}
	return s.migrateOpen(ctx, kinds)
}

// Migrate runs, on registry.db and every open project.db, each migration
// not yet applied (the base's and what AddMigrations added) in one name
// order pass; from then on AddMigrations applies at once and every database
// s opens gets all of them. A daemon opened with OpenForWiring calls it once
// its wiring has built (kit/server), so a migration registered from init()
// that uses a module's table runs after the module's migration that makes
// it, whatever the order the wiring added them in. Open calls it itself.
func (s *Store) Migrate(ctx context.Context) error {
	s.addMu.Lock()
	s.deferred = false
	s.addMu.Unlock()
	return s.migrateOpen(ctx, map[Kind]bool{KindRegistry: true, KindProject: true})
}

// migrateOpen applies kinds' migrations to s's open databases of those
// kinds. The scripts are read before the handles: a project created in
// between opened with them already.
func (s *Store) migrateOpen(ctx context.Context, kinds map[Kind]bool) error {
	var errs []error
	if kinds[KindRegistry] {
		scripts := s.scripts(KindRegistry)
		s.mu.RLock()
		reg := s.reg
		s.mu.RUnlock()
		if reg != nil {
			if err := migrate(ctx, reg, scripts); err != nil {
				errs = append(errs, errors.New(FileRegistry+": "+err.Error()))
			}
		}
	}
	if kinds[KindProject] {
		scripts := s.scripts(KindProject)
		s.mu.RLock()
		dbs := map[string]*sql.DB{}
		for id, db := range s.projects {
			dbs[id] = db
		}
		for id, w := range s.writers { // a team project's writes go through its writer
			dbs[id] = w
		}
		s.mu.RUnlock()
		for _, id := range slices.Sorted(maps.Keys(dbs)) {
			if err := migrate(ctx, dbs[id], scripts); err != nil {
				errs = append(errs, errors.New(DirProjects+"/"+id+"/"+FileProject+": "+err.Error()))
			}
		}
	}
	return errors.Join(errs...)
}

// errReplicaEarly: a replica's copy is not tracked by s, so Migrate could
// not bring one opened before it up to date.
var errReplicaEarly = errors.New("store: a replica opened before Migrate (the wiring's migrations have not run)")

// OpenReplica is the package's OpenReplica with s's replica migrations (the
// base and what AddMigrations added).
func (s *Store) OpenReplica(ctx context.Context, path string, conns int) (*sql.DB, error) {
	if s.waiting() {
		return nil, errReplicaEarly
	}
	return openReplica(ctx, path, s.scripts(KindReplica), conns)
}

// OpenReplicaApply is the package's OpenReplicaApply with s's replica
// migrations.
func (s *Store) OpenReplicaApply(ctx context.Context, path string) (*sql.DB, error) {
	if s.waiting() {
		return nil, errReplicaEarly
	}
	return openReplicaApply(ctx, path, s.scripts(KindReplica))
}

// waiting reports whether s is still waiting for Migrate.
func (s *Store) waiting() bool {
	s.addMu.Lock()
	defer s.addMu.Unlock()
	return s.deferred
}
