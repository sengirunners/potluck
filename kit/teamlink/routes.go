package teamlink

// Route rows (SPEC 17.3): how a member's proxy treats a call to each route,
// declared by the module that owns the route. teamlink holds the mechanism
// (proxy.go, hedge.go) and its own rows (kitRows); the layer above
// registers its rows with RegisterRoutes before Start. A route with no row
// is never forwarded: this machine's router answers it.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sengirunners/potluck/kit/api"
)

// Kind says how the proxy treats a call on a member's socket.
type Kind int

const (
	// KindLocal: served here, never forwarded (a row that keeps a route out
	// of a wider pattern's).
	KindLocal Kind = iota
	// KindList: served here and at every joined project, merged (List).
	KindList
	// KindItem: here first, then each joined project while the answer is 404.
	KindItem
	// KindWrite: to the project named (?project=, Zm-Project) or else the
	// joined project the caller's cwd belongs to.
	KindWrite
	// KindOwn: the row's Own hook answers it.
	KindOwn
)

func (k Kind) String() string {
	return [...]string{"local", "list", "item", "write", "own"}[k]
}

// HedgeMode says whether a forwarded call may be sent more than once.
type HedgeMode int

const (
	// HedgeUnset: no decision (refused for a forwarded row: CheckRoutes).
	HedgeUnset HedgeMode = iota
	// Hedged: it only reads at the authority, so a silent attempt is asked
	// again on a new connection (hedge.go).
	Hedged
	// Once: it goes out once and carries no write id (Why says why).
	Once
	// Write: a write. It goes out once with a write id the authority
	// records, so a second delivery never runs twice.
	Write
)

// RouteRule is one route's row.
type RouteRule struct {
	// Pattern is the router's pattern ("GET /v0/people", ServeMux syntax).
	Pattern string
	Kind    Kind
	Hedge   HedgeMode
	Why     string // Once: why it is not hedged
	Hook    *RouteHook
}

// RouteHook is a row's special handling. Every field may be nil (zero).
type RouteHook struct {
	// When: the row applies to r only when true; otherwise r is served here.
	// It decides the kind only, never the hedge or write-id decision.
	When func(r *http.Request) bool
	// Local: r stays on this machine (checked first).
	Local func(r *http.Request) bool
	// Before may rewrite r, or refuse it (the error answers), before the
	// dispatch.
	Before func(r *http.Request) (*http.Request, error)
	// Own answers r in place of the dispatch (KindOwn); false leaves r to
	// the router.
	Own func(w http.ResponseWriter, r *http.Request, next http.Handler) bool
	// Named may answer r for joined project x (named, or by the cwd when
	// !explicit) instead of a plain forward: true when it did. body is nil
	// while unread.
	Named func(w http.ResponseWriter, r *http.Request, x *Remote, area string, explicit bool, body []byte) bool
	// Unnamed answers r when it names no project, in place of the kind's
	// dispatch.
	Unnamed func(w http.ResponseWriter, r *http.Request, next http.Handler, body []byte)
	// Missing may answer an item that this machine and every joined project
	// answered 404 (an id that moved): true when it did.
	Missing func(w http.ResponseWriter, r *http.Request) bool
	// Refuse refuses forwarding r with body to the project of snap (checked
	// on every forward).
	Refuse func(r *http.Request, body []byte, snap api.Remote) error
	// BigBody: a body over MaxForwardBody, up to MaxForwardBigBody, is
	// forwarded when true (the authority still holds it to its own bounds).
	BigBody func(r *http.Request, body []byte) bool
	// Seal: body goes to the home only sealed to its key, on any transport.
	Seal func(body []byte) bool
	// AnswerWithin: how long the authority may be silent before it answers
	// (at least CallTimeout).
	AnswerWithin func(r *http.Request, body []byte) time.Duration
	// Thinks: a hedged read's silence before its answer starts may last
	// this many HedgeAfters (it may think for seconds first).
	Thinks int
	// NoCopy: the layer above's copy never holds this route's data, so the
	// authority is always asked, and one that does not answer is an error.
	NoCopy bool
	// IDNamesProject: an item's id names its own project, so $ZM_PROJECT
	// does not narrow it (-P does).
	IDNamesProject bool
	// List merges a KindList's answers.
	List *ListMerge
}

// ListMerge is how a list's answers are merged: each answer's Key array,
// "skipped" and "behind" are appended to this machine's.
type ListMerge struct {
	Key string
	// Share runs on each authority's answer before it is merged.
	Share func(merged, theirs map[string]json.RawMessage)
	// Done runs on the merged answer, with the authorities that did not
	// answer; it returns those still to be listed in "skipped".
	Done func(merged map[string]json.RawMessage, skipped []api.WorkSkipped) []api.WorkSkipped
}

func (r RouteRule) hook() *RouteHook {
	if r.Hook == nil {
		return &RouteHook{}
	}
	return r.Hook
}

// forwards reports whether the row's kind sends calls to an authority.
func (r RouteRule) forwards() bool { return r.Kind != KindLocal }

type ownedRule struct {
	owner string
	RouteRule
}

// routeTable is the rows matched as the router matches (every pattern, so
// a literal segment is never taken for a {wildcard}).
type routeTable struct {
	mux  *http.ServeMux
	rows map[string]RouteRule
	errs []error
}

type routes struct {
	mu    sync.Mutex
	rows  []ownedRule
	table atomic.Pointer[routeTable]
}

// RegisterRoutes adds owner's rows (before Start).
func (s *Service) RegisterRoutes(owner string, rows ...RouteRule) {
	s.routes.mu.Lock()
	defer s.routes.mu.Unlock()
	for _, r := range rows {
		s.routes.rows = append(s.routes.rows, ownedRule{owner, r})
	}
	s.routes.table.Store(nil)
}

// Rows is every registered row, by pattern.
func (s *Service) Rows() []RouteRule {
	s.routes.mu.Lock()
	defer s.routes.mu.Unlock()
	out := make([]RouteRule, 0, len(s.routes.rows))
	for _, r := range s.routes.rows {
		out = append(out, r.RouteRule)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Pattern < out[j].Pattern })
	return out
}

func (s *Service) routeTable() *routeTable {
	if t := s.routes.table.Load(); t != nil {
		return t
	}
	s.routes.mu.Lock()
	defer s.routes.mu.Unlock()
	t := &routeTable{mux: http.NewServeMux(), rows: map[string]RouteRule{}}
	owners := map[string]string{}
	for _, r := range s.routes.rows {
		if o, ok := owners[r.Pattern]; ok {
			t.errs = append(t.errs, fmt.Errorf("route %s: rows from %s and %s", r.Pattern, o, r.owner))
			continue
		}
		owners[r.Pattern] = r.owner
		if r.forwards() && r.Hedge == HedgeUnset {
			t.errs = append(t.errs, fmt.Errorf("route %s (%s) is forwarded with no hedge decision: Hedged (a pure read at the authority), Once (with Why) or Write", r.Pattern, r.owner))
		}
		if r.Hedge == Once && r.Why == "" {
			t.errs = append(t.errs, fmt.Errorf("route %s (%s) goes out once with no Why", r.Pattern, r.owner))
		}
		if r.Kind == KindOwn && r.hook().Own == nil {
			t.errs = append(t.errs, fmt.Errorf("route %s (%s) is KindOwn with no Own hook", r.Pattern, r.owner))
		}
		if err := handle(t.mux, r.Pattern); err != nil {
			t.errs = append(t.errs, fmt.Errorf("route %s (%s): %v", r.Pattern, r.owner, err))
			continue
		}
		t.rows[r.Pattern] = r.RouteRule
	}
	s.routes.table.Store(t)
	return t
}

func handle(m *http.ServeMux, pattern string) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("%v", p)
		}
	}()
	m.HandleFunc(pattern, func(http.ResponseWriter, *http.Request) {})
	return nil
}

// CheckRoutes reports a row that is not whole: forwarded with no hedge
// decision, a pattern two owners register, or one the router would refuse.
// Start runs it, so a daemon with such a row does not start.
func (s *Service) CheckRoutes() error { return errors.Join(s.routeTable().errs...) }

// rule is the row whose pattern matches r (ok false: none).
func (s *Service) rule(r *http.Request) (RouteRule, bool) {
	t := s.routeTable()
	_, pat := t.mux.Handler(r)
	row, ok := t.rows[pat] // a redirect's "pattern" is a path: no row
	return row, ok
}

// Route is the row that decides how the proxy treats r: none (ok false)
// when no row matches or its When says no.
func (s *Service) Route(r *http.Request) (RouteRule, bool) {
	row, ok := s.rule(r)
	if !ok || (row.Hook != nil && row.Hook.When != nil && !row.Hook.When(r)) {
		return RouteRule{}, false
	}
	return row, true
}

// Proxies implements core.ProxyLister: whether the proxy forwards method
// and path to a joined project's authority (or merges its answer). The
// swap preflight runs it over every route pattern (SPEC 2, "Swaps").
func (s *Service) Proxies(method, path string) bool {
	row, ok := s.Route(httptest.NewRequest(method, "http://zm"+path, nil))
	return ok && row.forwards()
}

// IsForwardedWrite reports whether req names a route a member forwards as
// a write: only those carry a write id and are recorded at the authority.
func (s *Service) IsForwardedWrite(req *http.Request) bool {
	row, ok := s.rule(req)
	return ok && row.Hedge == Write
}

// Hedgeable reports whether req may be sent more than once: a Hedged
// route, never a long poll or a stream.
func (s *Service) Hedgeable(req *http.Request) bool {
	q := req.URL.Query()
	if q.Has("wait") || q.Has("wait_ms") || req.Header.Get("Accept") == "text/event-stream" {
		return false
	}
	row, ok := s.rule(req)
	return ok && row.Hedge == Hedged
}

// kitRows are the KIT routes a member forwards: the projects lists and
// events (served here, see proxy.go and events_proxy.go), people, and the
// owners module's owner right and GitHub login (at the project's authority,
// SPEC 22).
func (s *Service) kitRows() []RouteRule {
	projects := &RouteHook{Own: s.projects}
	return []RouteRule{
		{Pattern: "GET /v0/projects", Kind: KindOwn, Hedge: Once, Why: "asked through CallRemote, not forwarded", Hook: projects},
		{Pattern: "GET /v0/projects/{rest...}", Kind: KindOwn, Hedge: Once, Why: "asked through CallRemote, not forwarded", Hook: projects},
		{Pattern: "GET /v0/events", Kind: KindOwn, Hedge: Once, Why: "a stream or a replay", Hook: &RouteHook{Own: s.eventsRoute}},
		{Pattern: "GET /v0/people", Kind: KindList, Hedge: Hedged, Hook: &RouteHook{List: &ListMerge{Key: "people",
			Done: func(merged map[string]json.RawMessage, sk []api.WorkSkipped) []api.WorkSkipped {
				merged["people"] = MergePeopleJSON(merged["people"])
				return sk
			}}}},
		{Pattern: "GET /v0/owner", Kind: KindWrite, Hedge: Hedged}, // the owner right is judged where choose is
		// An invite into a joined project is made at its authority, by an
		// owner's identity (authority.go inviter); a second copy would mint
		// a second code.
		{Pattern: "POST /v0/invites", Kind: KindWrite, Hedge: Once, Why: "it mints a one-time code: a second copy would mint a second"}, // the owner right is judged where choose is
		// A GitHub login runs at the project's authority, by the cwd or -P;
		// the poll collects (acts) once: never twice.
		{Pattern: "POST /v0/login/github/start", Kind: KindWrite, Hedge: Write},
		{Pattern: "POST /v0/login/github/{id}", Kind: KindWrite, Hedge: Write},
	}
}
