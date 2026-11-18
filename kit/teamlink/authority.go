package teamlink

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/events"
	"github.com/sengirunners/potluck/kit/project"
)

// Routes registers the teamlink routes (API.md "Teams"): being joined, on
// both sides.
func (s *Service) Routes(r *core.Router) {
	r = r.For("teamlink")
	// The authority's side.
	r.HandleFunc("POST /v0/projects/{project}/team", core.Local, "local; admin on the project; refused while a bound lease is live", jsonH(s.handleTeam))
	r.HandleFunc("POST /v0/projects/{project}/solo", core.Local, "local; admin on the project; refused while a replica is joined", jsonH(s.handleSolo))
	r.HandleFunc("POST /v0/invites", core.Authed, "the socket: admin on the project/area; a member's daemon (a replica's node-bound token, forwarded): its identity owns or administers the whole project by its own grants here, never a minted token or a tailnet login; the project must be team; a class only from the project's owner or admin; add_area: an owner or admin of the whole project gets the area made first", jsonH(s.handleInvite))
	r.HandleFunc("GET /v0/invites", core.Local, "local", jsonH(func(r *http.Request) (any, error) {
		ls, err := s.d.Projects.Invites(r.Context())
		return api.InviteList{Invites: ls}, err
	}))
	r.HandleFunc("DELETE /v0/invites/{id}", core.Local, "local", jsonH(func(r *http.Request) (any, error) {
		return s.d.Projects.RemoveInvite(r.Context(), r.PathValue("id"))
	}))
	r.HandleFunc("POST /v0/invites/redeem", core.Public, "none: a tailnet node with a join code, or over the tunnel a key with a proof of one (not this machine; tagged only with --allow-tagged); rate-limited; one answer for wrong, used and expired codes", s.handleRedeem)
	r.HandleFunc("POST /v0/team/leave", core.Authed, "a replica's node-bound token only: revokes itself", jsonH(func(r *http.Request) (any, error) {
		sub := core.SubjectOf(r.Context())
		if err := s.d.Projects.RevokeOwn(r.Context(), sub); err != nil {
			return nil, err
		}
		s.d.ReapPresence(r.Context()) // its sessions leave the authority's lists now
		return map[string]bool{"revoked": true}, nil
	}))
	// The member's side.
	r.HandleFunc("POST /v0/team/join", core.Local, "local: redeem a join code at its authority", jsonH(s.handleJoin))
	r.HandleFunc("GET /v0/team/remotes", core.Local, "local", jsonH(func(r *http.Request) (any, error) {
		out := api.RemoteList{Remotes: []api.Remote{}}
		for _, x := range s.List() {
			rm := x.Snapshot()
			if s.Hooks.View != nil {
				s.Hooks.View(x, &rm)
			}
			out.Remotes = append(out.Remotes, rm)
		}
		return out, nil
	}))
	r.HandleFunc("DELETE /v0/team/remotes/{project}", core.Local, "local: leave a joined team project", jsonH(s.handleLeave))
	s.projectKeyRoutes(r) // the authority key each joined project id is bound to (project_keys.go)
	s.peopleRoutes(r)     // both sides: a list the proxy merges
	s.classRoutes(r)      // operator classes (SPEC 8, zm-eakh.12.3)
	s.opsRoutes(r)        // machine ops without the heartbeat's wait (ops.go)
	s.beatRoutes(r)       // the heartbeat and machine numbers (beat.go, machine.go)
}

// ---- zm project team | solo ----

// moveLeases records project's mode, through Hooks.MoveLeases when the
// layer above keeps leases.
func (s *Service) moveLeases(ctx context.Context, project string, toTeam bool) (int, error) {
	if s.Hooks.MoveLeases != nil {
		return s.Hooks.MoveLeases(ctx, project, toTeam)
	}
	if s.d.Projects.IsTeam(project) == toTeam {
		return 0, nil
	}
	return 0, s.d.Projects.SetTeam(ctx, project, toTeam)
}

func (s *Service) projectFor(r *http.Request) (api.Project, *authz.Subject, error) {
	sub := core.SubjectOf(r.Context())
	p, err := s.d.Projects.Get(sub.Scope, r.PathValue("project"))
	if err != nil {
		return p, sub, err
	}
	return p, sub, sub.Check(authz.Admin, authz.Object{Project: p.ID}, "project", r.PathValue("project"))
}

func (s *Service) handleTeam(r *http.Request) (any, error) {
	p, _, err := s.projectFor(r)
	if err != nil {
		return nil, err
	}
	if s.ByName(p.Slug) != nil {
		return nil, api.Errorf(api.CodeConflict, "%s is also a team project this machine joined", p.Slug)
	}
	starting := !s.d.Projects.IsTeam(p.ID)
	if starting {
		for _, f := range s.Hooks.PoolStarting {
			if err := f(r.Context(), p.ID); err != nil {
				return nil, err
			}
		}
	}
	n, err := s.moveLeases(r.Context(), p.ID, true)
	if err != nil {
		return nil, err
	}
	if starting {
		for _, f := range s.Hooks.PoolStarted {
			f(r.Context(), p.ID)
		}
	}
	s.Publish(r.Context(), p)
	return api.ProjectModeResponse{Project: api.ProjectRef{ID: p.ID, Slug: p.Slug}, Team: true, Moved: n}, nil
}

func (s *Service) handleSolo(r *http.Request) (any, error) {
	p, _, err := s.projectFor(r)
	if err != nil {
		return nil, err
	}
	if n, err := s.d.Projects.JoinedNodes(r.Context(), p.ID); err != nil {
		return nil, err
	} else if n > 0 {
		return nil, api.Errorf(api.CodeConflict, "%d replicas are joined to %s", n, p.Slug).
			WithHint("each member runs zm leave " + p.Slug + ", or revoke their tokens: zm token ls, zm token revoke <id>")
	}
	n, err := s.moveLeases(r.Context(), p.ID, false)
	if err != nil {
		return nil, err
	}
	s.d.ReapPresence(r.Context()) // replicas' sessions from before end with the team
	s.Publish(r.Context(), p)
	return api.ProjectModeResponse{Project: api.ProjectRef{ID: p.ID, Slug: p.Slug}, Team: false, Moved: n}, nil
}

// Publish announces p changed (project.updated).
func (s *Service) Publish(ctx context.Context, p api.Project) {
	if _, err := s.d.Events.Publish(ctx, events.Input{Type: api.EventProjectUpdated, Subject: p.ID, Project: p.ID, Data: p}); err != nil {
		s.d.Log.Warn("team: publish", "err", err)
	}
}

// ---- invites ----

func (s *Service) handleInvite(r *http.Request) (any, error) {
	var req api.InviteRequest
	if err := api.DecodeJSON(r, &req); err != nil {
		return nil, err
	}
	if p, a, ok := strings.Cut(req.Project, "/"); ok && req.Area == "" {
		req.Project, req.Area = p, a
	}
	sub, err := s.inviter(r.Context(), req.Project)
	if err != nil {
		return nil, err
	}
	if req.AddArea && strings.TrimSpace(req.Area) != "" {
		if err := s.ensureArea(r.Context(), sub, req.Project, req.Area); err != nil {
			return nil, err
		}
	}
	if strings.TrimSpace(req.Class) != "" {
		// An operator class is the whole project's owner's or admin's to give
		// (as POST /v0/team/machines/{machine}/class): an area's admin may
		// invite into it, not trust a machine with the project's secrets.
		if err := s.classInviter(r.Context(), sub, req.Project); err != nil {
			return nil, err
		}
	}
	in := project.InviteInput{InviteRequest: req}
	// The code names a transport (core.TeamTransport.Invite). Without a
	// via, each registered transport in turn (the tailnet first), skipping
	// one that is not serving here now (unavailable).
	ts := s.d.Transports()
	if via := strings.TrimSpace(req.Via); via != "" {
		t := s.d.Transport(via)
		if t == nil {
			var names []string
			for _, x := range ts {
				names = append(names, x.Via())
			}
			return nil, api.Errorf(api.CodeBadRequest, "via %q: one of %s", req.Via, strings.Join(names, ", "))
		}
		ts = []core.TeamTransport{t}
	}
	var first error
	for _, t := range ts {
		it, err := t.Invite(r.Context())
		if err == nil {
			in.Authority, in.Host, in.Node, in.Fingerprint, in.URL = it.Authority, it.Host, it.Node, it.Fingerprint, it.URL
			return s.d.Projects.CreateInvite(r.Context(), sub, in)
		}
		if first == nil {
			first = err
		}
		if !api.IsCode(err, api.CodeUnavailable) {
			return nil, err
		}
	}
	// None serves here: the project's own refusals (solo, a bad role) come
	// first, then the first transport's reason.
	if _, err := s.d.Projects.CreateInvite(r.Context(), sub, in); err != nil && !api.IsCode(err, api.CodeUnavailable) {
		return nil, err
	}
	if first == nil {
		first = api.Errorf(api.CodeUnavailable, "this daemon has no team transport")
	}
	return nil, first
}

// inviter is who makes an invite into project ref. On this machine's
// socket only its owner, as a local route admits (no token: a token always
// narrows), held to CreateInvite's own checks. From the network only a
// member machine's node-bound token (whatever area it joined) whose
// identity owns or administers the whole project by its own grants here
// (as core.Deps.ProjectAdmin judges): that token is capped at member, so
// the invite is judged by those grants, uncapped. A minted token, a box's
// token and a tailnet login never make one.
func (s *Service) inviter(ctx context.Context, ref string) (*authz.Subject, error) {
	sub := core.SubjectOf(ctx)
	refuse := api.Errorf(api.CodeForbidden, "only the project's owner (or an admin of the whole project) makes an invite").
		WithHint("run it on the machine that runs the project, or on an owner's own machine that joined it")
	if sub == nil {
		return nil, refuse
	}
	if core.IsLocal(ctx) {
		if sub.Remote || sub.Via != authz.ViaLocal || sub.TokenID != "" {
			return nil, refuse
		}
		return sub, nil
	}
	if sub.Via != authz.ViaToken || sub.Node == "" || sub.Machine == "" || sub.Identity.ID == "" {
		return nil, refuse
	}
	p, err := s.d.Projects.Get(sub.Scope, ref)
	if err != nil {
		return nil, err
	}
	tuples, err := s.d.Projects.Tuples(ctx, sub.Identity.ID)
	if err != nil {
		return nil, err
	}
	own := authz.FromTuples(tuples, nil)
	if !own.Can(authz.Admin, authz.Object{Project: p.ID}) {
		return nil, refuse
	}
	x := *sub
	x.Scope, x.Cap = own, nil
	return &x, nil
}

// ensureArea makes area in team project ref when it has none by that name,
// for an owner or admin of the whole project; anyone else, and a solo
// project, are left to CreateInvite's own refusals. The area may stay when
// the invite is refused after it (a bad name, the live-invite cap): it is
// one the project's program always gives it.
func (s *Service) ensureArea(ctx context.Context, sub *authz.Subject, ref, area string) error {
	p, err := s.d.Projects.Get(sub.Scope, ref)
	if err != nil {
		return nil // CreateInvite answers a project the caller cannot see
	}
	area = strings.ToLower(strings.TrimSpace(area))
	has := func(p api.Project) bool {
		return slices.ContainsFunc(p.Areas, func(a api.Area) bool { return a.Name == area })
	}
	if !s.d.Projects.IsTeam(p.ID) || has(p) || !sub.Can(authz.Admin, authz.Object{Project: p.ID}) {
		return nil
	}
	added, err := s.d.Projects.AddArea(ctx, sub, ref, api.Area{Name: area})
	if err != nil {
		// Another invite added it meanwhile: done.
		if now, gerr := s.d.Projects.Get(sub.Scope, ref); gerr == nil && has(now) {
			return nil
		}
		return err
	}
	s.Publish(ctx, added)
	return nil
}

// classInviter refuses sub an invite carrying an operator class unless it
// owns or administers the whole project.
func (s *Service) classInviter(ctx context.Context, sub *authz.Subject, ref string) error {
	p, err := s.d.Projects.Get(sub.Scope, ref)
	if err != nil {
		return err
	}
	admin, err := s.d.ProjectAdmin(ctx, sub, p.ID)
	if err != nil {
		return err
	}
	if !admin {
		return api.Errorf(api.CodeForbidden, "only the project's owner (or an admin) gives an invite a machine class")
	}
	return nil
}

// handleRedeem is POST /v0/invites/redeem. Its refusals do not depend on
// the code until the network checks pass: the socket (403), a caller WhoIs
// did not identify (401), this machine (403); then every code problem and a
// rate-limited caller get ErrBadCode.
func (s *Service) handleRedeem(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	rm := core.RemoteOf(ctx)
	var t core.TeamTransport
	if rm != nil {
		t = s.d.Transport(rm.TransportVia())
	}
	switch {
	case core.IsLocal(ctx):
		api.WriteError(w, api.Errorf(api.CodeForbidden, "a join code is redeemed from the member's machine").WithHint("run zm join <code> on the member's machine"))
		return
	case rm == nil || rm.NodeID == "" || t == nil:
		api.WriteError(w, api.Errorf(api.CodeUnauthenticated, "unknown caller").WithHint("redeem with zm join <code>"))
		return
	case rm.Self:
		api.WriteError(w, api.Errorf(api.CodeForbidden, "a join code is not redeemed from the authority's own machine").
			WithHint("run zm join on the member's machine"))
		return
	}
	var req api.RedeemRequest
	if err := api.DecodeJSON(r, &req); err != nil {
		api.WriteError(w, err)
		return
	}
	// Each transport has its own budget and bucket (the tunnel's is the
	// client address: anyone can make a key), so noise on one cannot lock
	// the others' joins out.
	lim, who := s.limiterFor(t.Via()), t.RateKey(rm)
	if !lim.allow(who, time.Now()) {
		api.WriteError(w, project.ErrBadCode)
		return
	}
	in := t.RedeemInput(ctx, rm, req)
	out, err := s.d.Projects.Redeem(ctx, in)
	if err != nil {
		if err == project.ErrBadCode {
			lim.fail(who, time.Now())
		}
		api.WriteError(w, err)
		return
	}
	if _, err := s.d.Events.Publish(ctx, events.Input{Type: api.EventTeamJoined, Subject: out.Machine, Data: api.TeamJoined{
		Project: api.ProjectRef{ID: out.Cap.Project, Slug: out.Cap.Slug}, Area: out.Cap.Area, Role: out.Cap.Role,
		Identity: out.Identity, Machine: out.Machine, Node: rm.NodeID}}); err != nil {
		s.d.Log.Warn("team: publish joined", "err", err)
	}
	api.WriteJSON(w, http.StatusOK, out)
}

// limiterFor is via's redemption limiter.
func (s *Service) limiterFor(via string) *limiter {
	s.limMu.Lock()
	defer s.limMu.Unlock()
	if s.lims == nil {
		s.lims = map[string]*limiter{}
	}
	l := s.lims[via]
	if l == nil {
		l = &limiter{}
		s.lims[via] = l
	}
	return l
}

// limiter counts wrong join codes per node and overall over RedeemWindow.
type limiter struct {
	mu     sync.Mutex
	nodes  map[string][]time.Time
	all    []time.Time
	pruned time.Time
}

func recent(ts []time.Time, now time.Time) []time.Time {
	i := 0
	for i < len(ts) && now.Sub(ts[i]) > RedeemWindow {
		i++
	}
	return ts[i:]
}

func (l *limiter) allow(node string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.nodes == nil {
		l.nodes = map[string][]time.Time{}
	}
	if now.Sub(l.pruned) > RedeemWindow {
		for n, ts := range l.nodes {
			if ts = recent(ts, now); len(ts) == 0 {
				delete(l.nodes, n)
			} else {
				l.nodes[n] = ts
			}
		}
		l.pruned = now
	}
	l.all = recent(l.all, now)
	l.nodes[node] = recent(l.nodes[node], now)
	return len(l.nodes[node]) < RedeemPerNode && len(l.all) < RedeemPerDaemon
}

func (l *limiter) fail(node string, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.nodes == nil {
		l.nodes = map[string][]time.Time{}
	}
	l.nodes[node] = append(l.nodes[node], now)
	l.all = append(l.all, now)
}
