package core

import (
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
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
	// Limits are the token route limits this route admits (Router.Admit),
	// sorted; AnyLimit admits every limit. A token with a limit reaches no
	// route that does not admit it.
	Limits []string
}

// AnyLimit, admitted on a route, admits a token of every limit (the kit's
// GET /v0/whoami: a client asks what it holds).
const AnyLimit = "*"

// LimitReason is the data reason of the 403 a limited token gets on a route
// that does not admit its limit.
const LimitReason = "token-limit"

// ErrTokenLimit is the refusal of a limited token on a route that does not
// admit its limit (or on no route).
func ErrTokenLimit() *api.Error {
	return api.Errorf(api.CodeForbidden, "this token does not reach this route").
		WithData(map[string]string{"reason": LimitReason}).
		WithHint("the token is limited to a few routes; use a token without a limit for the rest")
}

// Router is a ServeMux that records every route with its guard and enforces
// the coarse part (authenticated, local only).
type Router struct {
	mux    *http.ServeMux
	routes *[]Route
	// used: pattern -> answered since start (the swap preflight's "used"
	// routes). Written only while routes register, before serving.
	used map[string]*atomic.Bool
	// limits: pattern -> the token limits it admits (Admit). Written only
	// while routes register, before serving.
	limits map[string]map[string]bool
	module string
}

// NewRouter returns an empty router.
func NewRouter() *Router {
	return &Router{mux: http.NewServeMux(), routes: &[]Route{}, used: map[string]*atomic.Bool{}, limits: map[string]map[string]bool{}}
}

// For returns a view of r that tags routes with a module name.
func (r *Router) For(module string) *Router {
	return &Router{mux: r.mux, routes: r.routes, used: r.used, limits: r.limits, module: module}
}

// Admit lets a token limited to limit reach the routes with these exact
// patterns (as registered; a GET pattern also answers HEAD). Every other
// route refuses such a token (ErrTokenLimit) before its gates, the team
// proxy or its handler see the request. Call while routes register, before
// serving; CheckAdmits, at start, refuses a pattern no route has. limit is
// a name (authz.ValidLimit) or AnyLimit.
func (r *Router) Admit(limit string, patterns ...string) {
	for _, p := range patterns {
		if r.limits[p] == nil {
			r.limits[p] = map[string]bool{}
		}
		r.limits[p][limit] = true
	}
}

// CheckAdmits is the start-up check of every Admit: the limit is a valid
// name or AnyLimit, and the pattern is a registered authed route (a local
// or device route never takes a token, and a public one needs no limit).
func (r *Router) CheckAdmits() error {
	byPattern := map[string]Route{}
	for _, rt := range *r.routes {
		byPattern[rt.Pattern] = rt
	}
	var errs []error
	for _, p := range slices.Sorted(maps.Keys(r.limits)) {
		for _, l := range slices.Sorted(maps.Keys(r.limits[p])) {
			if l != AnyLimit && !authz.ValidLimit(l) {
				errs = append(errs, fmt.Errorf("route %q admits %q, which is no token limit name", p, l))
			}
		}
		rt, ok := byPattern[p]
		switch {
		case !ok:
			errs = append(errs, fmt.Errorf("a token limit admits route %q, which is not registered", p))
		case rt.Access != Authed:
			errs = append(errs, fmt.Errorf("a token limit admits route %q, which is %s (only an authed route takes a limited token)", p, rt.Access))
		}
	}
	return errors.Join(errs...)
}

// admits reports whether the route registered as pattern admits limit.
func (r *Router) admits(pattern, limit string) bool {
	m := r.limits[pattern]
	return limit != "" && (m[limit] || m[AnyLimit])
}

// Limits lists the limits some route admits by name (not AnyLimit), sorted:
// the only limits a token may be minted with.
func (r *Router) Limits() []string {
	seen := map[string]bool{}
	for _, m := range r.limits {
		for l := range m {
			if l != AnyLimit {
				seen[l] = true
			}
		}
	}
	return slices.Sorted(maps.Keys(seen))
}

// LimitRoute is how the route req would reach treats a token limited to
// limit: public (it runs, with no subject), admitted, or neither (refused,
// also when no route matches and the fallback would answer).
func (r *Router) LimitRoute(req *http.Request, limit string) (public, admitted bool) {
	// The handler the mux would serve, not only its pattern: for a path it
	// redirects (a doubled slash) it names the target's pattern but serves
	// a redirect, and a 405 or the fallback is no route either.
	h, _ := r.mux.Handler(req)
	rh, ok := h.(*routeHandler)
	if !ok {
		return false, false
	}
	return rh.access == Public, r.admits(rh.pattern, limit)
}

// routeHandler is what Handle registers on the mux: the route's pattern
// and access travel with its handler, so LimitRoute knows exactly which
// route a request reaches.
type routeHandler struct {
	pattern string
	access  Access
	serve   http.HandlerFunc
}

func (h *routeHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) { h.serve(w, r) }

// Handle registers h for pattern behind access; check documents the
// fine-grained check the handler does.
func (r *Router) Handle(pattern string, access Access, check string, h http.Handler) {
	*r.routes = append(*r.routes, Route{Pattern: pattern, Access: access, Check: check, Module: r.module})
	flag := &atomic.Bool{}
	r.used[pattern] = flag
	g := r.guard(pattern, access, h)
	r.mux.Handle(pattern, &routeHandler{pattern: pattern, access: access, serve: func(w http.ResponseWriter, req *http.Request) {
		if !flag.Load() {
			flag.Store(true)
		}
		g.ServeHTTP(w, req)
	}})
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
	for i := range out {
		if m := r.limits[out[i].Pattern]; len(m) > 0 {
			out[i].Limits = slices.Sorted(maps.Keys(m))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Pattern < out[j].Pattern })
	return out
}

// ServeHTTP dispatches.
func (r *Router) ServeHTTP(w http.ResponseWriter, req *http.Request) { r.mux.ServeHTTP(w, req) }

func (rt *Router) guard(pattern string, access Access, h http.Handler) http.Handler {
	if access == Public {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if SubjectOf(r.Context()).Limit() != "" {
				r = WithoutLimited(r)
			}
			h.ServeHTTP(w, r)
		})
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
		// The server refused a limited token on a route that does not admit
		// it before anything else saw the request; this is the second line,
		// for a router served without that middleware.
		if l := sub.Limit(); l != "" && !rt.admits(pattern, l) {
			api.WriteError(w, ErrTokenLimit())
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

// WithoutLimited is r as a public route sees a limited token's request: no
// subject and no Authorization header, like a caller that sent none, so no
// public handler can act on the token or read it again.
func WithoutLimited(r *http.Request) *http.Request {
	c := r.Clone(WithSubject(r.Context(), nil))
	c.Header.Del(api.HeaderAuthorization)
	return c
}
