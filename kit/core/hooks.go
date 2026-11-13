package core

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/swap"
)

// Hooks are how the base asks the modules built on it (zm's mail, sessions
// and homes) something, or tells them, without naming their types
// (zm-eakh.12.35.11, design items 2 to 4). The module that answers
// registers its hook at wiring time. With none registered the base answers
// as a daemon without that module: its health has no such section, a
// notice is only logged, and every session is unknown.

type hooks struct {
	mu       sync.Mutex
	parts    []healthPart
	probes   []swap.Part
	gates    []RequestGate
	notify   []Notifier
	sessions func() *SessionHooks
}

type healthPart struct {
	key string
	f   func(context.Context) any
}

// AddHealthPart registers one section of the full health answer (GET
// /v0/health on the socket, and the daemon_started event): key is its JSON
// name at the top level of the answer, and f gives its value on every such
// answer. A nil value, or one that encodes as null, leaves the section out.
// key must not be the name of a field the base fills (api.HealthKeyTaken);
// such a key, or one registered twice, panics.
func (d *Deps) AddHealthPart(key string, f func(ctx context.Context) any) {
	if key == "" || api.HealthKeyTaken(key) {
		panic("core: health part " + key + " is the name of a health field")
	}
	d.hooks.mu.Lock()
	defer d.hooks.mu.Unlock()
	for _, p := range d.hooks.parts {
		if p.key == key {
			panic("core: health part " + key + " added twice")
		}
	}
	d.hooks.parts = append(d.hooks.parts, healthPart{key, f})
}

// HealthParts is every registered health part's value, encoded, by key
// (for api.Health.Parts). Parts whose value is nil are left out; nil when
// none has a value.
func (d *Deps) HealthParts(ctx context.Context) map[string]json.RawMessage {
	d.hooks.mu.Lock()
	ps := append([]healthPart(nil), d.hooks.parts...)
	d.hooks.mu.Unlock()
	var out map[string]json.RawMessage
	for _, p := range ps {
		v := p.f(ctx)
		if v == nil {
			continue
		}
		b, err := json.Marshal(v)
		if err != nil {
			d.logger().Warn("health: a part did not encode", "part", p.key, "err", err)
			continue
		}
		if string(b) == "null" {
			continue
		}
		if out == nil {
			out = map[string]json.RawMessage{}
		}
		out[p.key] = b
	}
	return out
}

// AddProbe adds a part to this daemon's self-check probe (health's
// self_check, compared with what the last build on this home served): it
// asks the daemon, through its own socket, whether the module still serves
// what it served (SPEC 2, "Swaps"). Parts run after the base
// (health, projects, joined) and those swap.RegisterProbe added, in the
// order added. A name used twice, or one a base or registered part has
// (swap.Taken), panics: two parts of one name would leave one unchecked.
func (d *Deps) AddProbe(name string, part swap.ProbePart) {
	if name == "" || part == nil {
		panic("core: probe part " + name + " is empty")
	}
	if swap.Taken(name) {
		panic("core: probe part " + name + " is a base part's name or one swap.RegisterProbe registered")
	}
	d.hooks.mu.Lock()
	defer d.hooks.mu.Unlock()
	for _, p := range d.hooks.probes {
		if p.Name == name {
			panic("core: probe part " + name + " added twice")
		}
	}
	d.hooks.probes = append(d.hooks.probes, swap.Part{Name: name, Run: part})
}

// Probes are the parts AddProbe added, in order (for swap.Probe).
func (d *Deps) Probes() []swap.Part {
	d.hooks.mu.Lock()
	defer d.hooks.mu.Unlock()
	return slices.Clone(d.hooks.probes)
}

// RequestGate sees every request to the daemon's API after the caller is
// resolved (Authenticate) and before the team proxy and the router, so
// before the route's auth class (Authed, Public) is checked: it sees
// requests to public routes and unauthenticated ones too, where SubjectOf
// is nil (no token, or one that failed). It returns true when it has
// answered: written a refusal itself, or served the request through serve
// (the proxy and router, with any later gate), perhaps with a wrapped writer
// or request. false: it does not take the request, which goes on as if it
// were not there.
type RequestGate func(w http.ResponseWriter, r *http.Request, serve func(http.ResponseWriter, *http.Request)) bool

// AddRequestGate adds g in front of the proxy and router (a module's New:
// zm's session module gates a session's safe outputs). Gates run in the
// order added; each one's serve runs the next.
func (d *Deps) AddRequestGate(g RequestGate) {
	d.hooks.mu.Lock()
	defer d.hooks.mu.Unlock()
	d.hooks.gates = append(d.hooks.gates, g)
}

// Gate runs r through the gates AddRequestGate added and serves it with
// serve (the daemon's middleware calls it once per request).
func (d *Deps) Gate(w http.ResponseWriter, r *http.Request, serve func(http.ResponseWriter, *http.Request)) {
	d.hooks.mu.Lock()
	gs := slices.Clone(d.hooks.gates)
	d.hooks.mu.Unlock()
	for i := len(gs) - 1; i >= 0; i-- {
		g, inner := gs[i], serve
		serve = func(w http.ResponseWriter, r *http.Request) {
			if !g(w, r, inner) {
				inner(w, r)
			}
		}
	}
	serve(w, r)
}

// Notice is something a person should see (a GitHub link or join they may
// not have expected, an org check that keeps failing). To is an address in
// Project ("owners", "human:<name>"); Body is plain text. The daemon tells
// it as itself.
type Notice struct {
	Project api.ProjectRef
	To      string
	Body    string
}

// Notifier delivers a notice (zm's mail module mails it).
type Notifier func(ctx context.Context, n Notice) error

// ErrNoNotifier is Notify's answer when no notifier is registered: the
// notice was only logged.
var ErrNoNotifier = errors.New("no notifier registered: the notice was logged")

// AddNotifier registers f (at wiring time).
func (d *Deps) AddNotifier(f Notifier) {
	d.hooks.mu.Lock()
	defer d.hooks.mu.Unlock()
	d.hooks.notify = append(d.hooks.notify, f)
}

// Notify hands n to every registered notifier; their errors are joined.
// With none registered it logs n (project and recipient at Warn, the body
// only at Debug) and returns ErrNoNotifier.
func (d *Deps) Notify(ctx context.Context, n Notice) error {
	d.hooks.mu.Lock()
	fs := append([]Notifier(nil), d.hooks.notify...)
	d.hooks.mu.Unlock()
	if len(fs) == 0 {
		d.logger().Warn("notice for a person (no notifier to send it)", "project", n.Project.Slug, "to", n.To)
		d.logger().Debug("notice for a person: body", "project", n.Project.Slug, "to", n.To, "body", n.Body)
		return ErrNoNotifier
	}
	var errs []error
	for _, f := range fs {
		if err := f(ctx, n); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// SessionHooks are how the base asks the module that runs this machine's
// sessions (zm's session module) about one: Zm-As, project resolution,
// visibility, the people list, machine numbers and the daemon stop guards.
// A nil field answers as for a session the module does not know: no name
// resolves, no session is capped, placed, visible or seen, none is live,
// none blocks a stop.
type SessionHooks struct {
	// Name is the address of the live session on this machine that ref
	// (<machine>/<name>) names now, ignoring scope (SPEC 8 "Names").
	Name func(ctx context.Context, ref string) (address string, ok bool)
	// Cap is the project a session acts in at most, and its role there
	// (SPEC 1.3: a session started for another machine's launcher); ok
	// false: not capped. A cap only narrows a scope.
	Cap func(address string) (project string, role authz.Relation, ok bool)
	// Place is the project (its ID) and area a session belongs to,
	// ignoring scope: project resolution's session rule.
	Place func(ctx context.Context, address string) (project, area string, ok bool)
	// Visible reports whether a reader with scope sc may see the session.
	Visible func(ctx context.Context, sc authz.Scope, address string) bool
	// Reap ends the presence rows no replica stands behind any more (after
	// a node-bound token was revoked).
	Reap func(ctx context.Context)
	// Seen is, for each session a reader with scope sc may see that has a
	// person, that person's name, the session's machine and when it was
	// last seen (the people list).
	Seen func(ctx context.Context, sc authz.Scope) ([]SessionSeen, error)
	// Machines keeps machine numbers (SPEC 8 "Machine numbers"); nil:
	// teamlink keeps them in memory.
	Machines MachinePresence
	// Live counts the managed sessions a daemon stop would end.
	Live func(ctx context.Context) int
	// StopBlocked is the refusal of a daemon stop, restart or upgrade
	// without force that would end sessions; nil when none would.
	StopBlocked func(ctx context.Context) error
	// End ends the managed sessions before a forced stop answers: how many
	// ended and those that did not.
	End func(ctx context.Context) (int, []api.DaemonSessionFailure, error)
	// Keep makes sessions outlive this daemon: their holders keep running
	// when it stops, and the next daemon adopts them (SPEC 8).
	Keep func()
}

// SessionSeen is one session as the people list counts it.
type SessionSeen struct {
	Person  string // the session's person (identity name)
	Machine string // "" when unknown
	Seen    time.Time
}

// SetSessionHooks registers the session module's hooks: f is asked on
// every use and answers nil while no session module is wired (a test
// harness may swap the module on a running daemon). A later call replaces
// f.
func (d *Deps) SetSessionHooks(f func() *SessionHooks) {
	d.hooks.mu.Lock()
	defer d.hooks.mu.Unlock()
	d.hooks.sessions = f
}

// SessionHooks is the session module's hooks, or nil when no session module
// is wired.
func (d *Deps) SessionHooks() *SessionHooks {
	d.hooks.mu.Lock()
	f := d.hooks.sessions
	d.hooks.mu.Unlock()
	if f != nil {
		return f()
	}
	return nil
}

// SessionPlace is the project (its ID) and area session address belongs
// to, ignoring scope; ok false when no session module knows it.
func (d *Deps) SessionPlace(ctx context.Context, address string) (project, area string, ok bool) {
	if h := d.SessionHooks(); h != nil && h.Place != nil {
		return h.Place(ctx, address)
	}
	return "", "", false
}

func (d *Deps) logger() *slog.Logger {
	if d.Log != nil {
		return d.Log
	}
	return slog.Default()
}
