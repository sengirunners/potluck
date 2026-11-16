package runtarget

// A pool's sealing mode (zm-eakh.12.48.2, SPEC 17.11 "Sealed secrets"). Every
// daemon seals secrets and a jit_config to a peer that names a seal key, on
// every leg. The mode says what the pool's machine does with a peer that
// names none, which over the tailnet is a Potluck (or zm) from before
// sealing there (over the tunnel such a peer is refused in both modes, as
// before):
//
//   - preferred (what a pool with no row has, as every pool from before this
//     rule does): send to it as before, in the clear over the tailnet, and
//     take its clear secrets, as before; one Warn per peer per start names
//     it and says to update it.
//   - required: refuse both ways.
//
// In both modes secrets in the clear from a peer that names a key are
// refused, and a host refuses a clear op its authority marked as sealing:
// stripping cannot turn a capable pair to the clear.
//
// A pool started by this build begins required (startSealing); a pool with no
// row, which is every pool started before, stays preferred until its admin
// sets it.
//
// The mode is a row of this brick's run_settings table in the pool's
// project.db at its machine, read on every use (a change takes effect at
// once, no restart), set by an admin of the pool (PUT /v0/sealing, potluck
// sealing set).

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/events"
	"github.com/sengirunners/potluck/kit/sealbox"
	"github.com/sengirunners/potluck/kit/store"
)

var settingsMigrations = []store.Migration{
	{Kind: store.KindProject, Name: "runtarget/0007_settings", Script: `CREATE TABLE run_settings (key TEXT PRIMARY KEY, value TEXT NOT NULL);`},
}

// sealState is what the pool's machine heard of its peers' sealing since it
// started, and whom it warned.
type sealState struct {
	mu     sync.Mutex
	since  time.Time
	peers  map[string]map[string]computeapi.SealingPeer // project -> machine -> peer
	warned map[string]bool                              // project + "\n" + machine
	// sealed: machines heard naming a seal key since the start. One of them
	// that stops is a downgrade signal, not version skew: warned every time.
	sealed map[string]bool // project + "\n" + machine
	// reported is ReportedSealing's cache.
	reported   string
	reportedAt time.Time
	keyLogged  atomic.Int64 // when "cannot read its seal key" was last logged
}

// heard records machine's beat to project: whether it named a seal key and
// lends boxes.
func (s *Service) heardSeal(project, machine string, seals, lends bool) {
	s.seal.mu.Lock()
	defer s.seal.mu.Unlock()
	if s.seal.peers == nil {
		s.seal.peers = map[string]map[string]computeapi.SealingPeer{}
	}
	if s.seal.peers[project] == nil {
		s.seal.peers[project] = map[string]computeapi.SealingPeer{}
	}
	s.seal.peers[project][machine] = computeapi.SealingPeer{Machine: machine, Seals: seals, Lends: lends, Seen: time.Now().UTC()}
	k := project + "\n" + machine
	if s.seal.sealed == nil {
		s.seal.sealed = map[string]bool{}
	}
	if seals {
		s.seal.sealed[k] = true
	}
}

// stoppedSealing reports (and warns, every time it is asked) whether
// machine named a seal key to this pool's machine since it started and names
// none now.
func (s *Service) stoppedSealing(project, machine, how string) bool {
	s.seal.mu.Lock()
	was := s.seal.sealed[project+"\n"+machine]
	s.seal.mu.Unlock()
	if was {
		s.d.Log.Warn("sealing: a machine that sealed before names no seal key now ("+how+"): a downgrade, or it was put back to an older build; check it",
			"machine", machine, "project", project)
	}
	return was
}

// ownKey is this daemon's seal key id; "" with no error while sealing is off
// (a test standing in for a build from before sealing). A key that cannot
// be read is an error, logged loudly: a fault to fix, never a reason to send
// in the clear.
func (s *Service) ownKey() (string, error) {
	sl, ok := s.d.Transport(core.ViaTunnel).(core.Sealer)
	if !ok {
		return "", errors.New("this daemon has no seal key module")
	}
	idB64, cert, err := sl.SealIdentity()
	if err == nil && idB64 == "" {
		return "", nil
	}
	id, _, ok := sealbox.Peer(idB64, cert)
	if err == nil && !ok {
		err = errors.New("this daemon's own seal certificate does not verify")
	}
	if err != nil {
		// At most once a minute.
		if now := time.Now().UnixNano(); now-s.seal.keyLogged.Load() >= int64(time.Minute) {
			s.seal.keyLogged.Store(now)
			s.d.Log.Error("sealing: this machine cannot read its seal key; it sends no secrets until fixed", "err", err)
		}
		return "", err
	}
	return id, nil
}

// errOwnKey is the refusal while this daemon's seal key cannot be read:
// why is in this machine's log, never in the answer (a caller may be
// another member).
func errOwnKey(error) *api.Error {
	return api.Errorf(api.CodeUnavailable, "this machine cannot read its seal key, so it passes on no secrets").
		WithHint("its owner sees why in its log; calls without secrets still work")
}

// ReportedSealing implements core.SealingReporter: "required" when any pool
// this machine serves requires sealing, else "preferred". Cached for a few
// seconds (every answer of the tailnet listener asks); a change here drops
// the cache.
func (s *Service) ReportedSealing(ctx context.Context) string {
	s.seal.mu.Lock()
	defer s.seal.mu.Unlock()
	if s.seal.reported != "" && time.Since(s.seal.reportedAt) < reportTTL {
		return s.seal.reported
	}
	// Not the request's context: a caller that goes away mid-query must not
	// turn a failed read (counted as required) into what members record.
	qctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), reportQueryMax)
	defer cancel()
	out, failed := computeapi.SealingPreferred, false
	for _, p := range s.d.Store.Projects(s.d.System().Scope, authz.Read) {
		if !s.d.Store.IsTeam(p.ID) {
			continue // a pool is a team project: no other project relays anything
		}
		mode, err := s.readSealingMode(qctx, p.ID)
		if err != nil {
			failed = true
		}
		if mode == computeapi.SealingRequired {
			out = computeapi.SealingRequired
			break
		}
	}
	if !failed {
		// A result from a failed read is answered (fail closed) but never
		// cached: the next answer reads again.
		s.seal.reported, s.seal.reportedAt = out, time.Now()
	}
	return out
}

// reportQueryMax bounds ReportedSealing's own reads.
var reportQueryMax = 2 * time.Second

// reportTTL bounds how stale ReportedSealing may be.
var reportTTL = 5 * time.Second

// warnUnsealed logs, once per peer per start, that machine got or sent
// secrets in the clear because it cannot seal.
func (s *Service) warnUnsealed(project, machine, how string) {
	s.seal.mu.Lock()
	k := project + "\n" + machine
	first := !s.seal.warned[k]
	if s.seal.warned == nil {
		s.seal.warned = map[string]bool{}
	}
	s.seal.warned[k] = true
	s.seal.mu.Unlock()
	if first {
		s.d.Log.Warn("sealing: a machine that cannot seal "+how+" in the clear over the tailnet (pool sealing mode preferred): update it, then set sealing to required",
			"machine", machine, "project", project)
	}
}

// startSealing is a teamlink PoolStarting hook: the pool this build starts
// begins required. It keeps a row already there (a retry, or a project that
// went solo and team again), and never touches a pool that is already team.
func (s *Service) startSealing(ctx context.Context, project string) error {
	db, err := s.d.Store.Project(s.d.System().Scope, authz.Write, project)
	if err != nil {
		return err
	}
	if _, err = db.ExecContext(ctx, `INSERT INTO run_settings (key, value) VALUES ('sealing', ?) ON CONFLICT (key) DO NOTHING`, computeapi.SealingRequired); err != nil {
		return err
	}
	return nil
}

// startedSealing is a teamlink PoolStarted hook: the project is team now, so
// the machine's cached report (which skips solo projects) is read again.
func (s *Service) startedSealing(context.Context, string) {
	s.seal.mu.Lock()
	s.seal.reported = ""
	s.seal.mu.Unlock()
}

// sealingMode is project's mode at this machine. A row it cannot read
// counts as required (fail closed); no row is the default, preferred.
func (s *Service) sealingMode(ctx context.Context, projectID string) string {
	mode, _ := s.readSealingMode(ctx, projectID)
	return mode
}

// readSealingMode is sealingMode, and the error when the row could not be
// read (the mode is then required).
func (s *Service) readSealingMode(ctx context.Context, projectID string) (string, error) {
	db, err := s.d.Store.Project(s.d.System().Scope, authz.Read, projectID)
	if err != nil {
		return computeapi.SealingRequired, err
	}
	var v string
	err = db.QueryRowContext(ctx, `SELECT value FROM run_settings WHERE key='sealing'`).Scan(&v)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return computeapi.SealingPreferred, nil
	case err != nil:
		return computeapi.SealingRequired, err
	case v != computeapi.SealingPreferred && v != computeapi.SealingRequired:
		return computeapi.SealingRequired, errors.New("unknown sealing mode " + v)
	}
	return v, nil
}

// callerSealed refuses what (secrets, a jit_config) that arrived in the
// clear over the tailnet when the pool requires sealing; under preferred it
// warns (every time for a machine that sealed before). Clear secrets reach a
// route only from a caller that names no seal key: one that names a key had
// its body opened (or its clear secrets refused) by the tailnet listener; the
// socket is this machine's own, and the tunnel never carries them clear.
func (s *Service) callerSealed(ctx context.Context, what string) error {
	rm := core.RemoteOf(ctx)
	if rm == nil || rm.Via == core.ViaTunnel {
		return nil
	}
	own, err := s.ownKey()
	if err != nil {
		return errOwnKey(err)
	}
	if own == "" || len(rm.SealKey) > 0 {
		return nil // sealing off: a build from before sealing checked nothing
	}
	t, err := s.d.WriteTarget(ctx, "", authz.Read)
	if err != nil {
		return err
	}
	machine := core.SubjectOf(ctx).Machine
	if machine == "" {
		machine = rm.Node
	}
	stopped := s.stoppedSealing(t.Project.ID, machine, "it sent "+what+" in the clear")
	if s.sealingMode(ctx, t.Project.ID) == computeapi.SealingRequired {
		return api.Errorf(api.CodeForbidden, "this pool requires sealing, and the calling machine runs a Potluck (or zm) too old to seal %s: update it", what).
			WithHint("update the daemon on the calling machine (potluck daemon update, or zm daemon update); calls without secrets still work")
	}
	if !stopped {
		s.warnUnsealed(t.Project.ID, machine, "sent "+what)
	}
	return nil
}

// sealingView is project's mode and peers.
func (s *Service) sealingView(ctx context.Context, p api.ProjectRef) computeapi.Sealing {
	v := computeapi.Sealing{Project: p.Slug, Mode: s.sealingMode(ctx, p.ID), Peers: []computeapi.SealingPeer{}, CannotSeal: []string{}}
	s.seal.mu.Lock()
	v.Since = s.seal.since
	for _, x := range s.seal.peers[p.ID] {
		v.Peers = append(v.Peers, x)
		if !x.Seals {
			v.CannotSeal = append(v.CannotSeal, x.Machine)
		}
	}
	s.seal.mu.Unlock()
	sort.Slice(v.Peers, func(i, j int) bool { return v.Peers[i].Machine < v.Peers[j].Machine })
	sort.Strings(v.CannotSeal)
	return v
}

// getSealing is GET /v0/sealing. The machines heard from are for a
// project-wide operator; an area-scoped reader gets the mode only.
func (s *Service) getSealing(r *http.Request) (any, error) {
	t, err := s.d.WriteTarget(r.Context(), "", authz.Read)
	if err != nil {
		return nil, err
	}
	v := s.sealingView(r.Context(), t.Project)
	if !core.SubjectOf(r.Context()).Can(authz.Operate, authz.Object{Project: t.Project.ID}) {
		v.Peers, v.CannotSeal = nil, nil
	}
	return v, nil
}

// setSealing is PUT /v0/sealing: admin on the pool. required is refused
// while a machine heard from cannot seal, unless forced.
func (s *Service) setSealing(r *http.Request) (any, error) {
	t, err := s.d.WriteTarget(r.Context(), "", authz.Admin)
	if err != nil {
		return nil, err
	}
	// The mode is the whole pool's: an admin of one area may not set it.
	sub := core.SubjectOf(r.Context())
	if t.Area != "" || !sub.Can(authz.Admin, authz.Object{Project: t.Project.ID}) {
		return nil, api.Errorf(api.CodeForbidden, "the pool's sealing mode is set by an admin of the whole pool, for the whole pool: run it without an area").
			WithHint("potluck sealing set <mode> -P <pool> (not <pool>/<area>)")
	}
	var req computeapi.SetSealingRequest
	if err := api.DecodeJSON(r, &req); err != nil {
		return nil, err
	}
	if req.Mode != computeapi.SealingPreferred && req.Mode != computeapi.SealingRequired {
		return nil, api.Errorf(api.CodeBadRequest, "sealing is preferred or required")
	}
	ctx := r.Context()
	if v := s.sealingView(ctx, t.Project); req.Mode == computeapi.SealingRequired && len(v.CannotSeal) > 0 && !req.Force {
		return nil, api.Errorf(api.CodeConflict, "these machines cannot seal yet and would get no secrets or runner jobs: %v", v.CannotSeal).
			WithHint("update the daemon on each, wait for its next beat (potluck sealing lists them), then set required again; --force sets it now")
	}
	from := s.sealingMode(ctx, t.Project.ID)
	db, err := s.d.Store.Project(s.d.System().Scope, authz.Write, t.Project.ID)
	if err != nil {
		return nil, err
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO run_settings (key, value) VALUES ('sealing', ?) ON CONFLICT (key) DO UPDATE SET value=excluded.value`, req.Mode); err != nil {
		return nil, err
	}
	s.seal.mu.Lock()
	s.seal.reported = "" // the next answer reports the new mode
	s.seal.mu.Unlock()
	by := sub.Identity.Name
	if by == "" {
		by = sub.Machine
	}
	ch := computeapi.SealingChanged{Project: t.Project.Slug, From: from, To: req.Mode, By: by, Forced: req.Force}
	s.d.Log.Warn("sealing: the pool's mode changed", "project", ch.Project, "from", ch.From, "to", ch.To, "by", ch.By, "forced", ch.Forced)
	if s.d.Events != nil {
		if _, err := s.d.Events.Publish(ctx, events.Input{Type: computeapi.EventSealingChanged, Subject: t.Project.ID, Project: t.Project.ID, Data: ch}); err != nil {
			s.d.Log.Warn("sealing: publish the mode change", "err", err)
		}
	}
	return s.sealingView(ctx, t.Project), nil
}

// forgetSealedHome is POST /v0/sealing/forget: this machine's owner, on the
// socket, forgets that a pool's machine named a seal key, so it may be sent
// secrets in the clear again (under preferred) after its owner put it back
// to a build from before sealing. Logged.
func (s *Service) forgetSealedHome(r *http.Request) (any, error) {
	if !owner(r.Context()) {
		return nil, api.Errorf(api.CodeForbidden, "only this machine's owner, on the socket without a token, forgets a pool's machine's seal key")
	}
	var req computeapi.ForgetSealedHomeRequest
	if err := api.DecodeJSON(r, &req); err != nil {
		return nil, err
	}
	sl, ok := s.d.Transport(core.ViaTunnel).(core.Sealer)
	if !ok || s.link == nil {
		return nil, api.Errorf(api.CodeUnavailable, "this machine joins no pools")
	}
	for _, x := range s.link.List() {
		snap := x.Snapshot()
		if req.Pool == "" || (snap.Project.Slug != req.Pool && snap.Project.ID != req.Pool) || snap.Via == api.ViaTunnel {
			continue
		}
		forgot, err := sl.ForgetHomeSealed(snap.Fingerprint)
		if err != nil {
			return nil, err
		}
		s.d.Log.Warn("sealing: the owner forgot that this pool's machine named a seal key; secrets may go to it in the clear over the tailnet again",
			"pool", snap.Project.Slug, "authority", snap.Authority, "forgot", forgot)
		return computeapi.ForgetSealedHome{Pool: snap.Project.Slug, Forgot: forgot}, nil
	}
	return nil, api.Errorf(api.CodeNotFound, "this machine joined no pool %q over the tailnet", req.Pool)
}
