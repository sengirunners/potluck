package core

import (
	"context"
	"net/http"

	"github.com/sengirunners/potluck/kit/api"
)

// ParkedRoutesService names the routes a parked daemon's wiring provides
// (Provide, from server.ParkedListeners' modules' New) beside the term's: a
// ParkedRoutes. A parked daemon answers a call on this machine's socket
// that they Serve with them, and nothing else of them (kit/server term.go).
const ParkedRoutesService = "parked-routes"

// StandbyCopyService is a func(context.Context, uint64) *api.StandbyCopy
// supplied by a parked standby's wiring. It answers only for the given term
// epoch and returns nil when no copy is known.
const StandbyCopyService = "standby-copy"

// ParkedRoutes are routes a parked daemon answers to its machine's owner on
// its socket (ParkedRoutesService): teamlink's HQ routes there.
type ParkedRoutes interface {
	http.Handler
	// Serves reports whether r is one of them.
	Serves(r *http.Request) bool
}

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
