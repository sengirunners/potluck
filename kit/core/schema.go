package core

import (
	"context"
	"errors"
	"sync"

	"github.com/sengirunners/potluck/kit/store"
)

// schemas are migration lists the store does not register: a module with
// databases of its own (the issues module's issues.db) adds its list here,
// so the daemon's build manifest names them without importing the module.
type schemas struct {
	mu    sync.Mutex
	kinds map[string][]func() []string
	err   error // what AddMigrations could not apply
}

// AddMigrations adds a module's migrations of the store's databases
// (registry.db, every project.db, a replica's copy) to this daemon
// (store.Store.AddMigrations). A module's New calls it, so a daemon has the
// tables of the modules its wiring builds and no others. In a daemon they
// run with every other migration once the wiring has built (kit/server
// calls store.Store.Migrate), so New must not read its tables; on a store
// from store.Open (a module's tests) they run now. A migration that fails
// is kept for MigrationsErr: the daemon does not start. With no store it
// does nothing.
func (d *Deps) AddMigrations(ms ...store.Migration) {
	if d.Store == nil {
		return
	}
	if err := d.Store.AddMigrations(context.Background(), ms...); err != nil {
		d.schema.mu.Lock()
		d.schema.err = errors.Join(d.schema.err, err)
		d.schema.mu.Unlock()
	}
}

// MigrationsErr is every error AddMigrations met, or nil.
func (d *Deps) MigrationsErr() error {
	d.schema.mu.Lock()
	defer d.schema.mu.Unlock()
	return d.schema.err
}

// AddSchema registers list as migrations of database kind (swap.Kind*);
// the wiring calls it for a module whose databases the store does not open.
func (d *Deps) AddSchema(kind string, list func() []string) {
	d.schema.mu.Lock()
	defer d.schema.mu.Unlock()
	if d.schema.kinds == nil {
		d.schema.kinds = map[string][]func() []string{}
	}
	d.schema.kinds[kind] = append(d.schema.kinds[kind], list)
}

// Schemas lists the migrations AddSchema registered, per database kind.
func (d *Deps) Schemas() map[string][]string {
	d.schema.mu.Lock()
	defer d.schema.mu.Unlock()
	out := map[string][]string{}
	for k, fns := range d.schema.kinds {
		for _, fn := range fns {
			out[k] = append(out[k], fn()...)
		}
	}
	return out
}

// ProxyLister is implemented by teamlink's proxy (d.Team): whether this
// build's proxy forwards method and path to a joined project's authority.
// The swap preflight runs it over every route; a daemon without it
// forwards nothing.
type ProxyLister interface {
	Proxies(method, path string) bool
}
