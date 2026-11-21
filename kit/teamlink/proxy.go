package teamlink

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"time"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/ids"
	"github.com/sengirunners/potluck/kit/project"
)

// The proxy (SPEC 17.3, round B). On this machine's socket, a call to a
// joined team project goes to its authority, as the route's row says
// (routes.go):
//
//   - a project named by ?project= or Zm-Project (slug or the authority's
//     ULID, with an optional /area): that project, local or joined;
//   - a list naming none: this machine's answer merged with every joined
//     project's; an authority that does not answer is named in "skipped"
//     (a partial list never looks complete);
//   - one item naming none: this machine first, then each joined project
//     while the answer is 404;
//   - a write naming none: the joined project whose .zm/project.toml or
//     recorded checkout contains the cwd, unless a local rule named a
//     project explicitly.
//
// Forwarded calls carry the replica's node-bound token, its scope_version,
// Zm-As when the caller is a session on this machine, and never Zm-Cwd.
// The layer above takes part through Hooks (its copy, its live writes) and
// its rows' hooks.

// MaxForwardBody bounds a forwarded request body; MaxForwardBigBody one a
// row's BigBody admits, the most a transport to an authority carries
// (tunnel.MaxBody; the tailnet's door reads as much).
const (
	MaxForwardBody    = 1 << 20
	MaxForwardBigBody = 16 << 20
)

// ReadBody reads a request body of at most MaxForwardBody, refusing a
// larger one (never cut short): readBody for a route with no BigBody.
func ReadBody(r *http.Request) ([]byte, error) { return readBody(r, &RouteHook{}) }

// readBody reads a body to forward, refusing one over MaxForwardBody unless
// h's BigBody admits it, and any over MaxForwardBigBody (a body is never
// forwarded cut short).
func readBody(r *http.Request, h *RouteHook) ([]byte, error) {
	limit := MaxForwardBody
	if h.BigBody != nil {
		limit = MaxForwardBigBody
	}
	b, err := io.ReadAll(io.LimitReader(r.Body, int64(limit)+1))
	if err != nil {
		return nil, api.Errorf(api.CodeBadRequest, "reading the body: %v", err)
	}
	switch {
	case len(b) <= MaxForwardBody:
	case h.BigBody == nil:
		return nil, api.Errorf(api.CodeBadRequest, "body over 1 MiB").WithHint("a call to a team project carries at most 1 MiB")
	case len(b) > limit:
		return nil, api.Errorf(api.CodeBadRequest, "body over %d MiB", limit>>20).WithHint("this call to a team project carries at most 16 MiB")
	case !h.BigBody(r, b):
		return nil, api.Errorf(api.CodeBadRequest, "body over 1 MiB").WithHint("a call to a team project carries at most 1 MiB")
	}
	return b, nil
}

// copyState is what the layer above's copy of x says: ready (it holds a
// copy that serves while the authority is away), reachable (the authority
// answered last) and the last error. No copy: not ready, reachable.
func (s *Service) copyState(x *Remote) (ready, reachable bool, lastErr string) {
	if s.Hooks.Copy == nil {
		return false, true, ""
	}
	return s.Hooks.Copy(x)
}

// away reports whether x's copy serves and its authority is away (no wait
// for it).
func (s *Service) away(x *Remote) (bool, string) {
	ready, reachable, e := s.copyState(x)
	return ready && !reachable, e
}

// Proxy implements core.TeamProxy.
func (s *Service) Proxy(w http.ResponseWriter, r *http.Request, next http.Handler) bool {
	if !core.IsLocal(r.Context()) {
		// The authority's side: a replica's live write is answered once per
		// write id (the layer above's).
		return s.Hooks.FromMember != nil && s.Hooks.FromMember(w, r, next)
	}
	if s.Count() == 0 {
		return false
	}
	sub := core.SubjectOf(r.Context())
	if sub == nil || sub.Via != authz.ViaLocal {
		return false // a token on the socket acts in this machine's projects only
	}
	row, ok := s.Route(r)
	if !ok || row.Kind == KindLocal {
		return false
	}
	h := row.hook()
	if row.Kind == KindOwn {
		return h.Own(w, r, next)
	}
	if h.Local != nil && h.Local(r) {
		return false
	}
	if h.Before != nil {
		r2, err := h.Before(r)
		if err != nil {
			api.WriteError(w, err)
			return true
		}
		r = r2
	}
	named, fromEnv := r.URL.Query().Get("project"), false
	if named == "" {
		named = api.GetHeader(r.Header, api.HeaderProject)
		fromEnv = api.GetHeader(r.Header, api.HeaderProjectFrom) == project.RuleEnv
	}
	list := row.Kind == KindList
	if fromEnv && row.Kind == KindItem && h.IDNamesProject {
		named = "" // $ZM_PROJECT narrows no item whose id names its project; -P does
	}
	if named != "" {
		slug, area, _ := strings.Cut(named, "/")
		if _, local := s.d.Projects.Lookup(slug); local {
			return false
		}
		x := s.ByName(slug)
		if x == nil {
			return false // not found locally either: the router answers
		}
		if !(list && fromEnv) { // $ZM_PROJECT does not narrow lists
			if s.Hooks.ServeCopy != nil && s.Hooks.ServeCopy(w, r, x, area, nil) {
				return true // the copy answers
			}
			if h.Named != nil && h.Named(w, r, x, area, true, nil) {
				return true
			}
			s.Forward(w, r, x, area, true)
			return true
		}
	}
	body, err := readBody(r, h)
	if err != nil {
		api.WriteError(w, err)
		return true
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	if h.Unnamed != nil {
		h.Unnamed(w, r, next, body)
		return true
	}
	switch row.Kind {
	case KindList:
		s.union(w, r, next, body, row)
		return true
	case KindItem:
		s.LocalFirst(w, r, next, body)
		return true
	}
	// A write: by where the caller is.
	if x, area := s.ByCwd(r); x != nil {
		if s.Hooks.ServeCopy != nil && s.Hooks.ServeCopy(w, r, x, area, body) {
			return true
		}
		if h.Named != nil && h.Named(w, r, x, area, false, body) {
			return true
		}
		s.Forward(w, r, x, area, false)
		return true
	}
	return false
}

// projects adds the joined projects to GET /v0/projects (as their
// authorities show them to this replica), and answers GET
// /v0/projects/{slug}[/areas[/{area}]] for a joined slug from its authority,
// so the CLI names team projects by slug.
func (s *Service) projects(w http.ResponseWriter, r *http.Request, next http.Handler) bool {
	// Display metadata must not depend on joined authorities. The core
	// handler still applies the caller's scope to the local registry.
	if r.URL.Path == "/v0/projects" && r.URL.Query().Get("local") == "1" {
		return false
	}
	if r.URL.Query().Get("project") != "" {
		return false
	}
	if r.URL.Path == "/v0/projects/resolve" {
		return s.resolve(w, r)
	}
	if r.URL.Path != "/v0/projects" {
		rest := strings.TrimPrefix(r.URL.Path, "/v0/projects/")
		name, tail, _ := strings.Cut(rest, "/")
		if _, local := s.d.Projects.Lookup(name); local {
			return false
		}
		x := s.ByName(name)
		if x == nil {
			return false
		}
		if tail != "" {
			tail = "/" + tail
		}
		if err := Ended(x.Snapshot()); err != nil {
			api.WriteError(w, err)
			return true
		}
		var v json.RawMessage
		if away, _ := s.away(x); tail == "" && away {
			// The authority is away and the copy serves; the project is named
			// from what the replica holds, so offline commands resolve.
			api.WriteJSON(w, http.StatusOK, offlineProject(x.Snapshot()))
			return true
		}
		if err := s.CallRemote(r.Context(), x, http.MethodGet, "/v0/projects/"+x.Snapshot().Project.ID+tail, nil, &v); err != nil {
			if ready, _, _ := s.copyState(x); tail == "" && ready && api.IsCode(err, api.CodeUnavailable) {
				api.WriteJSON(w, http.StatusOK, offlineProject(x.Snapshot()))
				return true
			}
			api.WriteError(w, err)
			return true
		}
		api.WriteJSON(w, http.StatusOK, v)
		return true
	}
	rec := httptest.NewRecorder()
	next.ServeHTTP(rec, r)
	var pl api.ProjectList
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &pl) != nil {
		CopyRecorded(w, rec)
		return true
	}
	xs := s.List()
	answers := FanOut(xs, func(x *Remote) (api.Project, error) {
		var p api.Project
		if err := Ended(x.Snapshot()); err != nil {
			return p, err
		}
		if away, e := s.away(x); away {
			return p, api.Errorf(api.CodeUnavailable, "authority unreachable (%s)", e) // no wait: the replica saw it go
		}
		ctx, cancel := context.WithTimeout(r.Context(), CallTimeout)
		defer cancel()
		if err := s.CallRemote(ctx, x, http.MethodGet, "/v0/projects/"+x.Snapshot().Project.ID, nil, &p); err != nil {
			return p, err
		}
		return p, Ended(x.Snapshot()) // this answer may have revoked it
	})
	for i, x := range xs {
		snap := x.Snapshot()
		p, err := answers[i].V, answers[i].Err
		if err != nil {
			// A partial list never looks complete. A project whose copy
			// serves is still listed (offline commands resolve it), and named
			// in skipped too.
			msg := snap.Project.Slug + ": " + err.Error()
			if ready, _, _ := s.copyState(x); ready && snap.State == api.RemoteOK {
				pl.Projects = append(pl.Projects, offlineProject(snap))
				msg += "; served from this replica's copy (stale)"
			}
			pl.Skipped = append(pl.Skipped, api.WorkSkipped{Project: snap.Project.ID, Source: "authority", Error: msg})
			continue
		}
		pl.Projects = append(pl.Projects, p)
	}
	api.WriteJSON(w, http.StatusOK, pl)
	return true
}

// offlineProject is a joined project as the replica knows it (its header),
// for GET /v0/projects while the authority is away and the copy serves.
func offlineProject(r api.Remote) api.Project {
	p := api.Project{ID: r.Project.ID, Slug: r.Project.Slug, Name: r.Project.Slug, Role: r.Role}
	for _, a := range r.Areas {
		p.Areas = append(p.Areas, api.Area{Name: a})
	}
	return p
}

// resolve answers GET /v0/projects/resolve (zm project why) when a joined
// project takes writes from here (TeamWhy: a .zm/project.toml naming the
// joined slug, its path or remote rules, the calling session's team
// project): the same rule the proxy applies to a write, shown as the step
// "team" after the local steps. Dogfood: a guest who ran `zm project use acme` was told
// "no project: writes from here are refused" while `zm work new` worked.
func (s *Service) resolve(w http.ResponseWriter, r *http.Request) bool {
	x, area, why := s.TeamWhy(r.Context(), api.GetHeader(r.Header, api.HeaderCwd))
	if x == nil {
		return false
	}
	res := s.d.Resolve(r.Context())
	snap := x.Snapshot()
	res.Project, res.Area, res.Rule = &snap.Project, area, "team"
	in := snap.Project.Slug
	if area != "" {
		in += "/" + area
	}
	res.Steps = append(res.Steps, api.ResolveStep{Rule: "team", Input: in,
		Result: "joined project (authority " + snap.Authority + ") by " + why})
	api.WriteJSON(w, http.StatusOK, res)
	return true
}

// ByCwd is the joined project a write from the caller's cwd goes to
// (TeamFor), when no local rule names a project explicitly.
func (s *Service) ByCwd(r *http.Request) (*Remote, string) {
	return s.TeamFor(r.Context(), api.GetHeader(r.Header, api.HeaderCwd))
}

// TeamFor is the joined project a write (or a session) from cwd goes to,
// when no local rule names a project explicitly (TeamWhy).
func (s *Service) TeamFor(ctx context.Context, cwd string) (*Remote, string) {
	x, area, _ := s.TeamWhy(ctx, cwd)
	return x, area
}

// TeamWhy is TeamFor with the rule that chose it: a .zm/project.toml naming
// a joined slug; else a joined project's path rules (its recorded checkouts,
// the project's and its visible areas' paths from the authority's header)
// matched like a local project's, the longest path winning against a local
// one too; else, when no path matched, its remotes; else (when no local
// rule matched by path or remote) the calling session's own team project.
// Local data only: the header is cached from the authority's answers.
func (s *Service) TeamWhy(ctx context.Context, cwd string) (*Remote, string, string) {
	if cwd != "" {
		if slug, area, ok := project.RepoFileProject(cwd); ok {
			if _, local := s.d.Projects.Lookup(slug); local {
				return nil, "", ""
			}
			if x := s.ByName(slug); x != nil {
				return x, area, project.RuleCwdFile
			}
		}
	}
	res := s.d.Resolve(ctx)
	if res.Project != nil && (res.Rule == project.RuleFlag || res.Rule == project.RuleEnv || res.Rule == project.RuleCwdFile) {
		return nil, "", ""
	}
	sub := core.SubjectOf(ctx)
	if dir := project.ExpandPath(cwd); dir != "" {
		var best *Remote
		var bb project.Binding
		bestLen := -1
		for _, x := range s.List() {
			if b, n, ok := project.BestPath(dir, x.Bindings()); ok && (n > bestLen || (n == bestLen && b.Area != "" && bb.Area == "")) {
				best, bb, bestLen = x, b, n
			}
		}
		local := -1
		if res.Project != nil && res.Rule == project.RuleCwdPath && sub != nil {
			_, _, local = s.d.Projects.MatchPath(sub.Scope, dir)
		}
		switch {
		case best != nil && bestLen > local: // a tie goes to the local project
			return best, bb.Area, project.RuleCwdPath
		case local >= 0:
			return nil, "", ""
		}
		if res.Project != nil && res.Rule == project.RuleCwdRemote {
			return nil, "", ""
		}
		if root := project.GitRoot(dir); root != "" {
			if origin := project.NormalizeRemote(project.OriginURL(root)); origin != "" {
				for _, x := range s.List() {
					if b, ok := project.BestRemote(origin, x.Bindings()); ok && (best == nil || (b.Area != "" && bb.Area == "")) {
						best, bb = x, b
					}
				}
				if best != nil {
					return best, bb.Area, project.RuleCwdRemote
				}
			}
		}
	}
	if res.Project != nil && res.Rule != project.RuleSession && res.Rule != project.RuleDefaultProject {
		return nil, "", ""
	}
	if sub != nil && sub.Session != "" {
		if p, area, ok := s.d.SessionPlace(ctx, sub.Session); ok {
			if x := s.ByName(p); x != nil {
				return x, area, project.RuleSession
			}
		}
	}
	return nil, "", ""
}

// Forward sends r to x's authority and copies the answer back. area, when
// set, narrows the forwarded project (slug/area). explicit keeps the
// caller's ?project= / Zm-Project naming x (rewritten to the authority's
// ULID); otherwise Zm-Project names x.
func (s *Service) Forward(w http.ResponseWriter, r *http.Request, x *Remote, area string, explicit bool) {
	resp, err := s.ForwardResp(r, x, area, explicit, nil)
	if err != nil {
		api.WriteError(w, err)
		return
	}
	defer resp.Body.Close()
	s.relay(w, r, resp)
}

// relay copies an authority's answer to the caller, through Hooks.Relay
// when the layer above reads it.
func (s *Service) relay(w http.ResponseWriter, r *http.Request, resp *http.Response) {
	if s.Hooks.Relay != nil {
		s.Hooks.Relay(w, r, resp)
		return
	}
	CopyResponse(w, resp)
}

// ForwardResp builds and sends the forwarded request. body, when not nil,
// is the already-read request body.
func (s *Service) ForwardResp(r *http.Request, x *Remote, area string, explicit bool, body []byte) (*http.Response, error) {
	moved := x.homeChanged() // before the fence: a home left after it ends this call
	fence := x.RoleReply()
	snap := fence.Snap
	if err := Ended(snap); err != nil {
		return nil, err
	}
	row, _ := s.rule(r)
	h := row.hook()
	if body == nil {
		var err error
		if body, err = readBody(r, h); err != nil {
			return nil, err
		}
	}
	if h.Refuse != nil {
		if err := h.Refuse(r, body, snap); err != nil {
			return nil, err
		}
	}
	// Secrets go to the home only sealed to its key, over every transport
	// (SPEC 17.11 "Sealed secrets"): the transport seals them for this one
	// request, or refuses (a home too old to open them).
	seal := h.Seal != nil && h.Seal(body)
	target := snap.Project.ID
	if area != "" {
		target += "/" + area
	}
	q := r.URL.Query()
	if q.Get("project") != "" {
		q.Set("project", target)
	}
	u := snap.URL + r.URL.EscapedPath()
	if enc := q.Encode(); enc != "" {
		u += "?" + enc
	}
	// CallTimeout bounds waiting for the answer and each silence in it, not
	// the whole transfer: every native issue over a home uplink takes longer.
	// A row may wait longer for its answer to start (AnswerWithin).
	wait := CallTimeout
	if h.AnswerWithin != nil {
		wait = max(wait, h.AnswerWithin(r, body))
	}
	ctx, cancel := context.WithCancel(r.Context())
	done := ctx.Done() // read here: ctx is reassigned below while the goroutine runs
	go func() {        // a live answer (an event stream) ends when this machine leaves the home or it falls behind
		select {
		case <-moved:
			cancel()
		case <-done:
		}
	}()
	timer := time.AfterFunc(wait, cancel)
	stop := func() { timer.Stop(); cancel() }
	ctx = core.WithReadProgress(ctx, func() { timer.Reset(wait) })
	out, err := http.NewRequestWithContext(ctx, r.Method, u, bytes.NewReader(body))
	if err != nil {
		stop()
		return nil, err
	}
	if ct := r.Header.Get("Content-Type"); ct != "" {
		out.Header.Set("Content-Type", ct)
	}
	if seal {
		api.SetHeader(out.Header, api.HeaderSeal, SealedBody)
	}
	if row.Hedge == Write {
		// A live write carries a write id like a queued one (SPEC 17.6): the
		// authority runs it once, so a second delivery (the rescope retry, a
		// request resent by whoever carried it) never runs twice. Reads
		// (a read by POST included) carry none and are never recorded.
		api.SetHeader(out.Header, api.HeaderWriteID, ids.New())
	}
	if api.GetHeader(r.Header, api.HeaderProject) != "" || !explicit {
		api.SetHeader(out.Header, api.HeaderProject, target)
		api.SetHeader(out.Header, api.HeaderProjectFrom, project.RuleFlag)
		if api.GetHeader(r.Header, api.HeaderProjectFrom) == project.RuleEnv && explicit {
			api.SetHeader(out.Header, api.HeaderProjectFrom, project.RuleEnv)
		}
	}
	if as := core.SubjectOf(r.Context()).Address; as != "" {
		if a, err := api.ParseAddress(as); err == nil && (a.Kind == api.KindSession || a.Kind == api.KindMachine) && a.Machine == s.d.Config.Machine {
			api.SetHeader(out.Header, api.HeaderAs, a.String())
		}
	}
	s.d.Log.Debug("request proxied", "method", r.Method, "path", r.URL.Path, "project", snap.Project.Slug, "authority", snap.Authority)
	resp, err := s.sendForward(ctx, x, out, body)
	if err != nil {
		stop()
		return nil, err
	}
	if err := Ended(x.Snapshot()); err != nil {
		// This answer revoked (or purged) the replica: say so, not what the
		// authority answers a member with no grant.
		resp.Body.Close()
		stop()
		return nil, err
	}
	resp.Body = &watchedBody{ReadCloser: resp.Body, timer: timer, stop: stop}
	resp.Request = out.WithContext(WithReplyFence(out.Context(), fence))
	return resp, nil
}

// SealedBody is the Zm-Seal value asking the transport to seal a request's
// body to the home's key.
const SealedBody = "body"

type watchedBody struct {
	io.ReadCloser
	timer *time.Timer
	stop  func()
}

func (b *watchedBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		b.timer.Reset(CallTimeout)
	}
	return n, err
}

func (b *watchedBody) Close() error { err := b.ReadCloser.Close(); b.stop(); return err }

type replyFenceKey struct{}

// WithReplyFence carries the role fence a forwarded answer was asked under.
func WithReplyFence(ctx context.Context, f RoleReplyFence) context.Context {
	return context.WithValue(ctx, replyFenceKey{}, f)
}

// ReplyFence is the role fence resp was asked under (ForwardResp): false
// for an answer that did not come from it.
func ReplyFence(resp *http.Response) (RoleReplyFence, bool) {
	if resp.Request == nil {
		return RoleReplyFence{}, false
	}
	f, ok := resp.Request.Context().Value(replyFenceKey{}).(RoleReplyFence)
	return f, ok
}

// LocalFirst serves an item route here first and, while the answer is 404,
// at each joined project.
func (s *Service) LocalFirst(w http.ResponseWriter, r *http.Request, next http.Handler, body []byte) {
	rec := httptest.NewRecorder()
	next.ServeHTTP(rec, r)
	if rec.Code != http.StatusNotFound {
		CopyRecorded(w, rec)
		return
	}
	xs := s.List()
	if s.Hooks.ServeHeld != nil {
		for _, x := range xs {
			// A copy that holds the item answers it here.
			if s.Hooks.ServeHeld(w, r, x, body) {
				return
			}
		}
	}
	row, _ := s.rule(r)
	h := row.hook()
	var away error
	for _, x := range xs {
		if gone, _ := s.away(x); !h.NoCopy && gone {
			continue // its copy does not hold it and the authority is away: no wait
		}
		resp, err := s.ForwardResp(r, x, "", false, body)
		if err != nil {
			if api.IsCode(err, api.CodeForbidden) { // a row's refusal: say so whatever the id
				api.WriteError(w, err)
				return
			}
			if h.NoCopy {
				away = err // no copy to ask: "not found" would be a guess
			}
			continue
		}
		if resp.StatusCode == http.StatusNotFound {
			resp.Body.Close()
			continue
		}
		s.relay(w, r, resp)
		resp.Body.Close()
		return
	}
	if away != nil {
		api.WriteError(w, away)
		return
	}
	if h.Missing != nil && h.Missing(w, r) {
		return
	}
	CopyRecorded(w, rec)
}

// ErrBadAnswer is an authority's answer that does not decode.
var ErrBadAnswer = errors.New("bad answer")

// Answer is one authority's share of a merged list.
type Answer[T any] struct {
	V   T
	Err error
}

// FanOut runs call for every joined project at once (an unreachable
// authority costs a merged list one DialTimeout, not one per authority)
// and returns the answers in xs's order.
func FanOut[T any](xs []*Remote, call func(*Remote) (T, error)) []Answer[T] {
	out := make([]Answer[T], len(xs))
	var wg sync.WaitGroup
	for i, x := range xs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out[i].V, out[i].Err = call(x)
		}()
	}
	wg.Wait()
	return out
}

// union serves a list here and at every joined project and merges them.
func (s *Service) union(w http.ResponseWriter, r *http.Request, next http.Handler, body []byte, row RouteRule) {
	rec := httptest.NewRecorder()
	next.ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		CopyRecorded(w, rec)
		return
	}
	var merged map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &merged); err != nil {
		CopyRecorded(w, rec)
		return
	}
	h := row.hook()
	lm := h.List
	if lm == nil {
		lm = &ListMerge{}
	}
	var skipped []api.WorkSkipped
	xs := s.List()
	answers := FanOut(xs, func(x *Remote) (map[string]json.RawMessage, error) {
		if err := Ended(x.Snapshot()); err != nil {
			return nil, err // revoked or purged: its copy (going) answers nothing
		}
		if s.Hooks.ListShare != nil {
			if theirs, ok, err := s.Hooks.ListShare(r, x); ok {
				return theirs, err // the layer above answers (its copy, a cache)
			}
		}
		// A route with no copy always asks (the flag is false for a while
		// after this daemon starts).
		if gone, e := s.away(x); !h.NoCopy && gone {
			return nil, api.Errorf(api.CodeUnavailable, "authority unreachable (%s)", e)
		}
		resp, err := s.ForwardResp(r, x, "", false, body)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, Decode(resp, nil)
		}
		var theirs map[string]json.RawMessage
		if err := json.NewDecoder(resp.Body).Decode(&theirs); err != nil {
			return nil, ErrBadAnswer
		}
		return theirs, nil
	})
	for i, x := range xs {
		snap := x.Snapshot()
		theirs, err := answers[i].V, answers[i].Err
		if err != nil {
			skipped = append(skipped, api.WorkSkipped{Project: snap.Project.ID, Source: "authority", Error: snap.Project.Slug + ": " + err.Error()})
			continue
		}
		if lm.Share != nil {
			lm.Share(merged, theirs)
		}
		for _, k := range []string{lm.Key, "skipped", "behind"} {
			merged[k] = AppendArrays(merged[k], theirs[k])
		}
	}
	if lm.Done != nil {
		skipped = lm.Done(merged, skipped)
	}
	if len(skipped) > 0 {
		js, _ := json.Marshal(skipped)
		merged["skipped"] = AppendArrays(merged["skipped"], js)
	}
	for k, v := range merged {
		if v == nil {
			delete(merged, k)
		}
	}
	api.WriteJSON(w, http.StatusOK, merged)
}

// AppendArrays joins two JSON arrays.
func AppendArrays(a, b json.RawMessage) json.RawMessage {
	var xs, ys []json.RawMessage
	_ = json.Unmarshal(a, &xs)
	_ = json.Unmarshal(b, &ys)
	if xs == nil && ys == nil {
		return a
	}
	out, _ := json.Marshal(append(nonNil(xs), ys...))
	return out
}

// CopyResponse copies an authority's answer to the caller.
func CopyResponse(w http.ResponseWriter, resp *http.Response) {
	// An issue attachment's bytes keep the headers that stop a browser
	// second-guessing them (SPEC "Native issues", attachments).
	for _, h := range []string{"Content-Type", "X-Content-Type-Options", "Content-Disposition", "Content-Security-Policy", "Cache-Control", "ETag"} {
		if v := resp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	if _, err := io.Copy(w, resp.Body); err != nil {
		// The status is gone: drop the connection, so the caller sees a failed
		// call and never a short answer that ends cleanly.
		panic(http.ErrAbortHandler)
	}
}

// CopyRecorded copies a recorded answer to w.
func CopyRecorded(w http.ResponseWriter, rec *httptest.ResponseRecorder) {
	for k, vs := range rec.Header() {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(rec.Code)
	_, _ = w.Write(rec.Body.Bytes())
}
