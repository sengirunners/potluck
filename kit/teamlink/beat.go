package teamlink

// The heartbeat (SPEC 17.3, "presence"): a member machine posts one beat per
// joined project to its authority (POST /v0/team/presence) every BeatEvery,
// every FastBeatEvery while something asked for fast beats (BeatFast), and
// soon after a kick (KickBeat, or an event a part named). The beat itself
// carries the machine's op reports and the authority's answer the ops
// pending for it (ops.go); everything else in it is a registered part's
// (RegisterBeatPart): each part puts one raw JSON value under its name in
// the request and may answer one under the same name.

import (
	"context"
	"encoding/json"
	"github.com/sengirunners/potluck/kit/config"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/events"
)

// Tunables (tests shorten them).
var (
	// BeatEvery is the heartbeat period (SPEC: 15 s).
	BeatEvery = 15 * time.Second
	// FastBeatEvery is the period while BeatFast was called within the last
	// two BeatEvery (a part asked, or an op runs here for an authority).
	FastBeatEvery = 2 * time.Second
	// BeatTimeout bounds one heartbeat.
	BeatTimeout = 5 * time.Second
	// BeatsOff stops the loop's own beats (tests that beat by hand with
	// Heartbeat).
	BeatsOff atomic.Bool
	// beatKickDelay batches the kicks and events that trigger an early beat.
	beatKickDelay = 100 * time.Millisecond
	// beatMinGap spaces beats that kicks trigger.
	beatMinGap = time.Second
)

const (
	// MaxBeatBody is a heartbeat's body limit (larger than the global one:
	// a part may be one complete inventory).
	MaxBeatBody = 8 << 20
	// maxBeatOps bounds the op reports one beat carries, and its Took.
	maxBeatOps = 200
)

// BeatPart is one module's share of the heartbeat, under its name. Every
// func may be nil.
type BeatPart struct {
	// Kick are event type prefixes whose events send a beat soon.
	Kick []string
	// Request (the member's side) is the part's value in one beat to b's
	// authority (nil: left out) and what to do with the authority's answer
	// to that beat (its value under the part's name, nil when none). The
	// answer func runs only when the beat was answered and the remote's
	// access and scope did not move meanwhile. An error skips the beat.
	Request func(ctx context.Context, b Beat) (json.RawMessage, func(ctx context.Context, answer json.RawMessage), error)
	// Serve (the authority's side) takes the part's value from the calling
	// machine's beat for p (nil when the beat had none) and answers it (nil:
	// nothing). The caller is core.SubjectOf(ctx). Parts are served in
	// registration order, before the beat's op reports are applied; an error
	// (CodeTooLarge for a size) refuses the beat, keeping what earlier parts
	// did.
	Serve func(ctx context.Context, p api.Project, v json.RawMessage) (json.RawMessage, error)
	// Gone (the member's side) runs when a beat finds project's access ended.
	Gone func(project string)
}

// Beat is one heartbeat to a remote's authority, as a part sees it.
type Beat struct {
	Remote *Remote
	Snap   api.Remote     // the remote when the beat started
	Fence  RoleReplyFence // its role lifetime: Fence.Current() before admitting an answer's effects
}

// beatReg is a Service's beat parts and loops.
type beatReg struct {
	mu     sync.RWMutex
	names  []string // registration order: requests are built and served in it
	by     map[string]BeatPart
	kick   chan struct{}
	fastAt atomic.Int64 // BeatFast (UnixNano)
	lctx   context.Context
	stop   context.CancelFunc
	loops  map[*Remote]context.CancelFunc // per remote: its ops poll
	wg     sync.WaitGroup

	heardMu sync.Mutex
	heard   map[string]map[string]time.Time // project -> machine -> its last beat here (the authority)
}

// noteBeat records that machine beat for project now.
func (s *Service) noteBeat(project, machine string) {
	s.beat.heardMu.Lock()
	defer s.beat.heardMu.Unlock()
	if s.beat.heard == nil {
		s.beat.heard = map[string]map[string]time.Time{}
	}
	if s.beat.heard[project] == nil {
		s.beat.heard[project] = map[string]time.Time{}
	}
	s.beat.heard[project][machine] = time.Now()
}

// LastBeat is when machine last beat for project at this authority (zero:
// never since this daemon started). A relay asks it before waiting on a
// machine that may have gone quiet.
func (s *Service) LastBeat(project, machine string) time.Time {
	s.beat.heardMu.Lock()
	defer s.beat.heardMu.Unlock()
	return s.beat.heard[project][machine]
}

// RegisterBeatPart registers part under name. Registering a name twice
// panics. Register before Start.
func (s *Service) RegisterBeatPart(name string, part BeatPart) {
	if name == "" {
		panic("teamlink: a beat part needs a name")
	}
	s.beat.mu.Lock()
	defer s.beat.mu.Unlock()
	if s.beat.by == nil {
		s.beat.by = map[string]BeatPart{}
	}
	if _, dup := s.beat.by[name]; dup {
		panic("teamlink: beat part " + name + " registered twice")
	}
	s.beat.by[name] = part
	s.beat.names = append(s.beat.names, name)
}

type namedPart struct {
	name string
	BeatPart
}

func (s *Service) beatParts() []namedPart {
	s.beat.mu.RLock()
	defer s.beat.mu.RUnlock()
	out := make([]namedPart, 0, len(s.beat.names))
	for _, n := range s.beat.names {
		out = append(out, namedPart{n, s.beat.by[n]})
	}
	return out
}

// KickBeat asks for a heartbeat soon (never blocks).
func (s *Service) KickBeat() {
	select {
	case s.beat.kick <- struct{}{}:
	default:
	}
}

// BeatFast makes the heartbeat beat every FastBeatEvery for the next two
// BeatEvery.
func (s *Service) BeatFast() { s.beat.fastAt.Store(time.Now().UnixNano()) }

// Heartbeat sends one heartbeat to every joined authority now and applies
// the answers (the loop does this every BeatEvery; tests call it).
func (s *Service) Heartbeat(ctx context.Context) { s.beatAll(ctx) }

// startLoops starts the heartbeat and, per joined remote, its ops poll;
// they stop with ctx or StopLoops.
func (s *Service) startLoops(ctx context.Context) {
	s.beat.mu.Lock()
	if s.beat.lctx != nil {
		s.beat.mu.Unlock()
		return
	}
	s.beat.lctx, s.beat.stop = context.WithCancel(ctx)
	lctx := s.beat.lctx
	s.beat.wg.Add(1)
	s.beat.mu.Unlock()
	go func() {
		defer s.beat.wg.Done()
		s.beatLoop(lctx)
	}()
	for _, x := range s.List() {
		s.watch(x)
	}
}

// StopLoops stops the heartbeat, the ops polls and the GitHub joins'
// watches (join_github.go) and waits for them.
func (s *Service) StopLoops() {
	defer s.stopGitHubJoins()
	s.beat.mu.Lock()
	stop := s.beat.stop
	s.beat.lctx, s.beat.stop, s.beat.loops = nil, nil, nil
	s.beat.mu.Unlock()
	if stop != nil {
		stop()
	}
	s.beat.wg.Wait()
}

// watch starts x's ops poll (OpsLoop) while the loops run.
func (s *Service) watch(x *Remote) {
	s.beat.mu.Lock()
	defer s.beat.mu.Unlock()
	if s.beat.lctx == nil || s.beat.loops[x] != nil {
		return
	}
	if s.beat.loops == nil {
		s.beat.loops = map[*Remote]context.CancelFunc{}
	}
	ctx, cancel := context.WithCancel(s.beat.lctx)
	s.beat.loops[x] = cancel
	s.beat.wg.Add(1)
	go func() {
		defer s.beat.wg.Done()
		s.OpsLoop(ctx, x)
	}()
}

// unwatch stops x's ops poll.
func (s *Service) unwatch(x *Remote) {
	s.beat.mu.Lock()
	defer s.beat.mu.Unlock()
	if c := s.beat.loops[x]; c != nil {
		c()
		delete(s.beat.loops, x)
	}
}

// beatLoop sends heartbeats until ctx ends: every beatEvery, and
// beatKickDelay after a kick or an event a part named.
func (s *Service) beatLoop(ctx context.Context) {
	var types []string
	for _, p := range s.beatParts() {
		types = append(types, p.Kick...)
	}
	var evs <-chan api.Event
	cancel := func() {}
	subscribe := func() {
		if len(types) > 0 && s.d.Events != nil {
			evs, cancel = s.d.Events.Subscribe(s.d.System(), events.Filter{Types: types}, 256)
		}
	}
	subscribe()
	defer func() { cancel() }()
	t := time.NewTimer(BeatEvery)
	defer t.Stop()
	var last time.Time
	s.KickBeat() // the first heartbeat right after start
	for {
		t.Reset(s.beatEvery())
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-s.beat.kick:
			s.waitKick(ctx, evs, last)
		case _, ok := <-evs:
			if !ok { // fell behind: resubscribe; a heartbeat covers what was missed
				subscribe()
			}
			if s.Count() == 0 {
				continue
			}
			s.waitKick(ctx, evs, last)
		}
		if ctx.Err() == nil && s.Count() > 0 && !BeatsOff.Load() {
			last = time.Now()
			s.beatAll(ctx)
		}
	}
}

// beatEvery is the heartbeat period: BeatEvery, or FastBeatEvery for two
// BeatEvery after BeatFast.
func (s *Service) beatEvery() time.Duration {
	if at := s.beat.fastAt.Load(); at != 0 && time.Since(time.Unix(0, at)) < 2*BeatEvery && FastBeatEvery < BeatEvery {
		return FastBeatEvery
	}
	return BeatEvery
}

// waitKick waits beatKickDelay (and at least beatMinGap since the last
// heartbeat), draining kicks and events, so a burst is one heartbeat.
func (s *Service) waitKick(ctx context.Context, evs <-chan api.Event, last time.Time) {
	d := time.NewTimer(max(beatKickDelay, beatMinGap-time.Since(last)))
	defer d.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-d.C:
			return
		case <-s.beat.kick:
		case _, ok := <-evs:
			if !ok {
				return
			}
		}
	}
}

// beatAll sends one heartbeat to every joined authority, in parallel.
func (s *Service) beatAll(ctx context.Context) {
	var wg sync.WaitGroup
	for _, x := range s.List() {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.BeatRemote(ctx, x)
		}()
	}
	wg.Wait()
}

type partAnswer struct {
	name  string
	apply func(context.Context, json.RawMessage)
}

// BeatRemote sends one heartbeat to x's authority and applies the answer.
func (s *Service) BeatRemote(ctx context.Context, x *Remote) {
	x.BeatMu.Lock()
	defer x.BeatMu.Unlock()
	if ctx.Err() != nil {
		return
	}
	fence := x.RoleReply()
	snap := fence.Snap
	parts := s.beatParts()
	if Ended(snap) != nil {
		for _, p := range parts {
			if p.Gone != nil {
				p.Gone(snap.Project.ID)
			}
		}
		if snap.State == api.RemoteRevoked {
			s.ProbeRole(ctx, x)
		}
		return
	}
	x.Mu.Lock()
	noCopy := x.Copy == nil
	x.Mu.Unlock()
	if noCopy {
		s.QueueRoleReconcile(x) // a transient recovery save/open failure still owes a copy
	}
	b := Beat{Remote: x, Snap: snap, Fence: fence}
	req := api.PresenceRequest{Project: snap.Project.ID}
	behind := x.Home().Behind // a home that lost its term hears only whether it is current again (successor.go)
	var answers []partAnswer
	for _, p := range parts {
		if p.Request == nil || behind && p.name != AuthorityPart {
			continue
		}
		v, apply, err := p.Request(ctx, b)
		if err != nil {
			s.d.Log.Warn("teamlink: heartbeat not sent", "project", snap.Project.Slug, "part", p.name, "err", err)
			return
		}
		if v != nil {
			if req.Parts == nil {
				req.Parts = map[string]json.RawMessage{}
			}
			req.Parts[p.name] = v
		}
		if apply != nil {
			answers = append(answers, partAnswer{p.name, apply})
		}
	}
	var finals []string
	if !behind {
		req.Ops, finals = s.OpReports(ctx, snap.Project.ID) // machine ops' reports (SPEC 8 "Machine choice")
		req.Took = s.TookOps(ctx, snap.Project.ID)          // the ops this machine has: never sent again
	}
	if len(req.Ops) > len(finals) {
		s.BeatFast() // a task runs here for an authority: beat fast
	}
	body, err := json.Marshal(req)
	if err != nil || len(body) > MaxBeatBody {
		s.d.Log.Warn("teamlink: heartbeat body exceeds its limit", "project", snap.Project.Slug, "limit", MaxBeatBody)
		return
	}
	cctx, cancel := context.WithTimeout(ctx, BeatTimeout)
	defer cancel()
	var resp api.PresenceResponse
	if err := s.CallRemote(cctx, x, http.MethodPost, "/v0/team/presence", json.RawMessage(body), &resp); err != nil {
		if ctx.Err() == nil {
			s.d.Log.Debug("teamlink: heartbeat", "project", snap.Project.Slug, "authority", snap.Authority, "err", err)
			s.homeSilent(ctx, x, err) // the successor its home named may serve now (successor.go)
		}
		return
	}
	if current := x.Snapshot(); Ended(current) != nil || current.ScopeVersion != snap.ScopeVersion {
		return // an answer from before revocation cannot repopulate anything
	}
	if !s.homeCurrent(ctx, x, resp.Parts[AuthorityPart]) {
		return // a home below the epoch this machine recorded (successor.go)
	}
	s.AckOps(ctx, finals)
	for _, a := range answers {
		a.apply(ctx, resp.Parts[a.name])
	}
	for _, op := range resp.Ops {
		if !fence.Current() {
			break // a later role lifetime cannot admit an older answer's operations
		}
		s.TakeOps(ctx, snap.Project.ID, []api.MachineOp{op})
	}
	s.followIfAway(ctx, x) // the home answered it does not serve (successor.go)
	s.reportMachine(ctx, x, snap)
}

// beatRoutes registers the authority's heartbeat and machine routes.
func (s *Service) beatRoutes(r *core.Router) {
	r.HandleFunc("POST /v0/team/presence", core.Authed, "a replica's node-bound token only; at most 8 MiB and 200 op reports; the project visible to it and team; each registered part checks its own value", jsonH(s.HandleBeat))
	r.HandleFunc("POST /v0/team/machine", core.Authed, "a replica's node-bound token only; the project visible to it and team; recorded only when it reads the project whole (a member's machine, never an area guest's)", jsonH(s.handleMachine))
	r.HandleFunc("GET /v0/machines", core.Authed, "this machine: its owner or a whole-project reader of a team project it hosts; joined machines: whole-project readers of their projects", jsonH(s.handleMachines))
}

// HandleBeat is POST /v0/team/presence (tests call it directly).
func (s *Service) HandleBeat(r *http.Request) (any, error) {
	ctx := r.Context()
	sub := core.SubjectOf(ctx)
	if sub.Node == "" || sub.Machine == "" {
		return nil, api.Errorf(api.CodeForbidden, "presence comes from a team replica's daemon (its node-bound token)").
			WithHint("a replica sends it by itself every 15 s once it joined: " + config.Command("join", "<code>"))
	}
	// Unknown fields and parts are tolerated (unlike other routes).
	body, err := io.ReadAll(io.LimitReader(r.Body, MaxBeatBody+1))
	if err != nil {
		return nil, api.Errorf(api.CodeBadRequest, "reading presence body: %v", err)
	}
	if len(body) > MaxBeatBody {
		return nil, api.Errorf(api.CodeTooLarge, "presence body exceeds %d bytes", MaxBeatBody)
	}
	var req api.PresenceRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, api.Errorf(api.CodeBadRequest, "invalid JSON body: %v", err)
	}
	if len(req.Ops) > maxBeatOps {
		return nil, api.Errorf(api.CodeTooLarge, "presence carries more than %d op reports", maxBeatOps)
	}
	if len(req.Took) > maxBeatOps {
		return nil, api.Errorf(api.CodeTooLarge, "presence names more than %d taken ops", maxBeatOps)
	}
	parts := s.beatParts()
	p, err := s.d.Projects.Get(sub.Scope, req.Project)
	if err != nil {
		return nil, err
	}
	if !s.d.Store.IsTeam(p.ID) {
		return nil, api.NotFound("project", req.Project)
	}
	s.noteBeat(p.ID, sub.Machine)
	var out api.PresenceResponse
	for _, part := range parts {
		if part.Serve == nil {
			continue
		}
		v, err := part.Serve(ctx, p, req.Parts[part.name])
		if err != nil {
			return nil, err
		}
		if v != nil {
			if out.Parts == nil {
				out.Parts = map[string]json.RawMessage{}
			}
			out.Parts[part.name] = v
		}
	}
	// Machine ops (SPEC 8 "Machine choice"): what the machine has and its
	// reports first, then what is pending for it.
	s.MarkTook(ctx, p.ID, sub.Machine, req.Took)
	s.ApplyOpReports(ctx, p.ID, sub.Machine, req.Ops)
	out.Ops = s.PendingOps(ctx, p.ID, sub.Machine)
	return out, nil
}
