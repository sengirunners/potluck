package core

import (
	"net/http"
	"sort"
	"sync/atomic"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
)

// Access is the coarse guard on a route. The fine check (which project or
// area, which action) is the handler's job, through Subject.Check,
// Deps.WriteTarget or Scope.SQL.
type Access string

const (
	// Public routes answer anyone, even unauthenticated network callers
	// (only GET /v0/health). They must return nothing scoped.
	Public Access = "public"
	// Authed routes need an authenticated subject (socket, token or tailnet).
	Authed Access = "authed"
	// Local routes need the machine owner on the Unix socket (no token): spawning, attaching, typing into
	// sessions, tokens, identities, tailnet control.
	Local Access = "local"
	// Device routes answer only a tailnet device identified by WhoIs (stable
	// node ID + node key), never the socket and never a token: approvals
	// (SPEC section 16). Whether that node is a paired approver is the
	// handler's check. Unmarked network callers get 401 like everywhere.
	Device Access = "device"
)

// Route describes one registered route (API.md and the leak test use it).
type Route struct {
	Pattern string // Go 1.22 pattern, e.g. "POST /v0/mail/{id}/ack"
	Access  Access
	// Check documents the fine-grained authz check, e.g. "write: resolved project/area".
	Check  string
	Module string
}

// Router is a ServeMux that records every route with its guard and enforces
// the coarse part (authenticated, local only).
type Router struct {
	mux    *http.ServeMux
	routes *[]Route
	// used: pattern -> answered since start (the swap preflight's "used"
	// routes). Written only while routes register, before serving.
	used   map[string]*atomic.Bool
	module string
}

// NewRouter returns an empty router.
func NewRouter() *Router {
	return &Router{mux: http.NewServeMux(), routes: &[]Route{}, used: map[string]*atomic.Bool{}}
}

// For returns a view of r that tags routes with a module name.
func (r *Router) For(module string) *Router {
	return &Router{mux: r.mux, routes: r.routes, used: r.used, module: module}
}

// Handle registers h for pattern behind access; check documents the
// fine-grained check the handler does.
func (r *Router) Handle(pattern string, access Access, check string, h http.Handler) {
	*r.routes = append(*r.routes, Route{Pattern: pattern, Access: access, Check: check, Module: r.module})
	flag := &atomic.Bool{}
	r.used[pattern] = flag
	g := guard(access, h)
	r.mux.Handle(pattern, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if !flag.Load() {
			flag.Store(true)
		}
		g.ServeHTTP(w, req)
	}))
}

// MarkUsed records req's route as answered when something other than the
// router answered it (the team proxy).
func (r *Router) MarkUsed(req *http.Request) {
	if _, pat := r.mux.Handler(req); pat != "" {
		if f := r.used[pat]; f != nil {
			f.Store(true)
		}
	}
}

// Used lists the route patterns answered since start, sorted.
func (r *Router) Used() []string {
	var out []string
	for p, f := range r.used {
		if f.Load() {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

// HandleFunc is Handle for a func.
func (r *Router) HandleFunc(pattern string, access Access, check string, h http.HandlerFunc) {
	r.Handle(pattern, access, check, h)
}

// Stub registers a 501 route, keeping its guard (module stubs use it).
func (r *Router) Stub(pattern string, access Access, check string) {
	r.Handle(pattern, access, check, api.NotImplemented(pattern))
}

// Fallback handles every request no route matches (not recorded).
func (r *Router) Fallback(h http.Handler) { r.mux.Handle("/", h) }

// Routes lists every registered route, sorted by pattern.
func (r *Router) Routes() []Route {
	out := append([]Route(nil), *r.routes...)
	sort.Slice(out, func(i, j int) bool { return out[i].Pattern < out[j].Pattern })
	return out
}

// ServeHTTP dispatches.
func (r *Router) ServeHTTP(w http.ResponseWriter, req *http.Request) { r.mux.ServeHTTP(w, req) }

func guard(access Access, h http.Handler) http.Handler {
	if access == Public {
		return h
	}
	if access == Device {
		return deviceGuard(h)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := authErrOf(r.Context()); err != nil {
			api.WriteError(w, err)
			return
		}
		sub := SubjectOf(r.Context())
		if sub == nil {
			api.WriteError(w, api.Errorf(api.CodeUnauthenticated, "authentication required").
				WithHint("send Authorization: Bearer <token> (zm token mint) or call from the machine's socket"))
			return
		}
		// Local routes are the machine owner's: the OS user on the socket,
		// without a token (a token always narrows, even on the socket).
		if access == Local && (sub.Remote || sub.Via != authz.ViaLocal) {
			api.WriteError(w, api.Errorf(api.CodeForbidden, "this works only for the machine's owner on its own socket"))
			return
		}
		h.ServeHTTP(w, r)
	})
}

// deviceGuard admits only a tailnet device that WhoIs identified. It runs
// before (and ignores) token or login authentication: the decide right is
// bound to a node, so a token, a login or the socket can never carry it. The
// answer does not depend on which object the path names (no oracle).
func deviceGuard(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		switch rm := RemoteOf(ctx); {
		case IsLocal(ctx):
			api.WriteError(w, api.Errorf(api.CodeForbidden, "no socket caller holds the decide right").
				WithHint("the project's owner answers with zm ask answer <id> <choice>; a paired device decides on its approvals page"))
		case r.Header.Get(api.HeaderAuthorization) != "":
			api.WriteError(w, api.Errorf(api.CodeForbidden, "approvals take no token: the right is bound to a paired device's tailnet node"))
		case rm == nil || rm.NodeID == "" || rm.NodeKey == "" || rm.Via != "":
			api.WriteError(w, api.Errorf(api.CodeUnauthenticated, "unknown tailnet device").
				WithHint("approvals answer only devices on this daemon's tailnet"))
		default:
			h.ServeHTTP(w, r)
		}
	})
}
