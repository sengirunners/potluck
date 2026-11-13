package core

import (
	"context"

	"github.com/sengirunners/potluck/kit/api"
)

// Provide publishes v as the service name (a module's New, before Start):
// another module reaches it with Service(name) through the owner's typed
// helper (run targets: runtarget/rt.From). Providing a name twice
// panics.
func (d *Deps) Provide(name string, v any) {
	d.svcMu.Lock()
	defer d.svcMu.Unlock()
	if _, dup := d.services[name]; dup {
		panic("core: service " + name + " provided twice")
	}
	if d.services == nil {
		d.services = map[string]any{}
	}
	d.services[name] = v
}

// Service is the service name, or nil when no module provided it.
func (d *Deps) Service(name string) any {
	d.svcMu.Lock()
	defer d.svcMu.Unlock()
	return d.services[name]
}

// HolderCheck answers whether holder is live; known false when it does not
// know holder's kind.
type HolderCheck func(ctx context.Context, holder api.Address) (live, known bool)

// AddHolderCheck registers f (a module's New): the session module answers for
// sessions.
func (d *Deps) AddHolderCheck(f HolderCheck) {
	d.svcMu.Lock()
	defer d.svcMu.Unlock()
	d.holders = append(d.holders, f)
}

// HolderLive is the first registered check that knows holder; live and known
// false when none does (no check registered: nothing is kept alive).
func (d *Deps) HolderLive(ctx context.Context, holder api.Address) (live, known bool) {
	d.svcMu.Lock()
	hs := append([]HolderCheck(nil), d.holders...)
	d.svcMu.Unlock()
	for _, f := range hs {
		if live, known := f(ctx, holder); known {
			return live, true
		}
	}
	return false, false
}
