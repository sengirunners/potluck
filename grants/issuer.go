package grants

import (
	"context"
	"database/sql"
	"encoding/json"
	"sync"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/kit/core"
)

// Issuer is what grants needs from a kind the authority mints per box
// (github-token, potluck-token): reading a grant's ceiling and judging a
// request's params against it. Minting, renewal, revocation and their
// ledger events are the issuer's own (its package); grants never sees a
// value. An issuer registers with ProvideIssuer; until one has, no grant of
// its kind can be added and a request for it is refused.
//
// value and github-jit need no issuer: the caller holds the secret.
type Issuer interface {
	// Kind is computeapi.SecretKindGitHubToken or SecretKindPotluckToken.
	Kind() string
	// Ceiling checks a grant's ceiling when it is added and answers it in
	// its stored form; an error is the add's bad_request.
	Ceiling(raw json.RawMessage) (json.RawMessage, error)
	// Within reports whether a request's params sit inside a stored
	// ceiling (nil: they do).
	Within(ceiling, params json.RawMessage) error
}

// issuerService is the Deps service name an issuer of kind registers under.
func issuerService(kind string) string { return "grants.issuer." + kind }

// ProvideIssuer registers iss on d for its kind (once per kind; before the
// daemon serves).
func ProvideIssuer(d *core.Deps, iss Issuer) { d.Provide(issuerService(iss.Kind()), iss) }

// IssuerOf is the issuer registered for kind, or nil.
func IssuerOf(d *core.Deps, kind string) Issuer {
	i, _ := d.Service(issuerService(kind)).(Issuer)
	return i
}

// RemovedFunc is told that grant id of project is gone (removed, or for
// the defaults turned off), after the change committed, so an issuer can
// end what it issued under it. Each call runs on its own goroutine.
type RemovedFunc func(ctx context.Context, project, id string)

const removedService = "grants.removed"

type removedHooks struct {
	mu sync.Mutex
	fs []RemovedFunc
}

// OnGrantRemoved registers f (before the daemon serves). Nothing is called
// when no issuer registered one.
func OnGrantRemoved(d *core.Deps, f RemovedFunc) {
	h := hooksOf(d)
	h.mu.Lock()
	h.fs = append(h.fs, f)
	h.mu.Unlock()
}

// hooksOf is d's hook list, provided on first use (wiring is sequential).
func hooksOf(d *core.Deps) *removedHooks {
	if h, ok := d.Service(removedService).(*removedHooks); ok {
		return h
	}
	h := &removedHooks{}
	d.Provide(removedService, h)
	return h
}

// grantRemoved calls every hook for project's grant id.
func grantRemoved(ctx context.Context, d *core.Deps, project, id string) {
	h, ok := d.Service(removedService).(*removedHooks)
	if !ok {
		return
	}
	h.mu.Lock()
	fs := append([]RemovedFunc(nil), h.fs...)
	h.mu.Unlock()
	for _, f := range fs {
		go func() {
			defer func() {
				if r := recover(); r != nil && d.Log != nil {
					d.Log.Error("grants: a grant-removed hook panicked", "panic", r)
				}
			}()
			f(context.WithoutCancel(ctx), project, id)
		}()
	}
}

// ReserveFunc is asked, inside the transaction that adds a grant reserving
// name for an issuer in area ("" every area) of the project tx writes, to
// refuse the add while a live box there holds a value its holder set under
// name: that value would stay in the box, and the holder could no longer
// unset it (Reserved). An error refuses the add. It runs after the grant's
// row is written in tx, so a holder's write that commits later sees the
// grant and is refused.
type ReserveFunc func(ctx context.Context, tx *sql.Tx, area, name string) error

const reserveService = "grants.reserve"

type reserveHooks struct {
	mu sync.Mutex
	fs []ReserveFunc
}

// OnReserve registers f (before the daemon serves).
func OnReserve(d *core.Deps, f ReserveFunc) {
	h, ok := d.Service(reserveService).(*reserveHooks)
	if !ok {
		h = &reserveHooks{}
		d.Provide(reserveService, h)
	}
	h.mu.Lock()
	h.fs = append(h.fs, f)
	h.mu.Unlock()
}

// reserving asks every ReserveFunc about g, which reserves its name when
// its kind is an issuer's.
func reserving(ctx context.Context, d *core.Deps, tx *sql.Tx, g computeapi.SecretGrant) error {
	if !reserves(g) {
		return nil
	}
	h, ok := d.Service(reserveService).(*reserveHooks)
	if !ok {
		return nil
	}
	h.mu.Lock()
	fs := append([]ReserveFunc(nil), h.fs...)
	h.mu.Unlock()
	for _, f := range fs {
		if err := f(ctx, tx, g.Area, g.Name); err != nil {
			return err
		}
	}
	return nil
}
