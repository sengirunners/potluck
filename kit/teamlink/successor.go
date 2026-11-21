package teamlink

// A home's successor and a member's follow (home moves; kit/hometerm). A
// home keeps the same team projects when it moves to another machine, but
// the new machine has its own key: its tailnet node, or its tunnel home key,
// never a copy of the old one. A member does not follow it by copying keys;
// it follows by introduction:
//
//   - The serving home's owner names the home that serves after it
//     (Designate): its term (hometerm) records the successor.
//   - Every heartbeat's "authority" part answers the home's epoch, state
//     and named successor. The member records them: the epoch in
//     team_remotes.term, the named successor in team_successors, and when
//     the home last answered (heard), so it can say how long its home has
//     been silent.
//   - When its home has not answered for HomeSilentAfter (no answer at all,
//     never a refusal in its own words), or answers that it no longer
//     serves (its authority part, or a parked daemon's answer), the member asks the named successor, over a client pinned to
//     the key the home named, for its own term. Only when that home answers
//     for the same project that it serves at an epoch higher than the one
//     the member recorded does the member repoint (repoint): the project key
//     (project_keys.go) is rebound to the successor's key, the remote's
//     address, key and transport change, and nothing else: the token file,
//     the copy and what this machine keeps for the project as a host stay.
//
// No other home can take a project over: the introduction comes over the
// channel already pinned to the bound key, and the member reaches the
// successor only at the key the home named (the pin is the proof; the
// member's token goes to no other key). Without an introduction, only this
// machine's owner repoints, naming the bound key (Follow), and the
// successor must still serve the project at a higher epoch. A home that
// answers an epoch below the one recorded (it lost its term) is not
// believed (homeCurrent): nothing of that answer is taken, no call but the
// heartbeat and a leave goes to it (Send), no answer asked before is
// admitted (the role fence), and the remote says so, until it answers the
// recorded epoch again, the owner takes its epoch (Follow with
// ResetEpoch), or this machine joins again.

import (
	"bytes"
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/hometerm"
	"github.com/sengirunners/potluck/kit/secrets"
	"github.com/sengirunners/potluck/kit/store"
)

var successorMigrations = []store.Migration{
	// A member's side: the epoch of each joined project's home it recorded,
	// when that home last answered a heartbeat, the lower epoch it answered
	// when it lost its term (behind; NULL while it is current), and the
	// successor it named.
	// A home's side: the machine name of a successor its owner designated
	// (display only; the term file is the record of who is designated).
	{Kind: store.KindRegistry, Name: "team/0010_home_term", Script: `
ALTER TABLE team_remotes ADD COLUMN term INTEGER NOT NULL DEFAULT 0;
ALTER TABLE team_remotes ADD COLUMN heard INTEGER NOT NULL DEFAULT 0;
ALTER TABLE team_remotes ADD COLUMN behind INTEGER;
CREATE TABLE team_successors (
	project     TEXT PRIMARY KEY,
	via         TEXT NOT NULL,
	url         TEXT NOT NULL,
	fingerprint TEXT NOT NULL,
	machine     TEXT NOT NULL,
	epoch       INTEGER NOT NULL,
	named       INTEGER NOT NULL
);
CREATE TABLE team_designated (
	fingerprint TEXT PRIMARY KEY,
	machine     TEXT NOT NULL,
	designated  INTEGER NOT NULL
);`},
}

// AuthorityPart is the heartbeat part that carries a home's term.
const AuthorityPart = "authority"

// heardEvery spaces the saves of a home's last answer when nothing else in
// what the member knows of it changed.
const heardEvery = time.Minute

// HomeSilentAfter is how long a member's home goes without answering a
// beat at all, counted in this process from the first beat it did not
// answer, before the member asks the successor it named (tests shorten
// it): one lost beat, or a daemon that was itself down, is not a moved
// home.
var HomeSilentAfter = 2 * time.Minute

// homeNow is the clock silence is measured on (tests move it).
var homeNow = time.Now

// Successor is a home named to serve a team project after its current
// home: the peer a member pins (transport, address, key) and its machine
// name, shown to people only.
type Successor struct {
	hometerm.Peer
	Machine string `json:"machine,omitempty"`
}

// check refuses a successor a home could not designate (hometerm's rule for
// a peer).
func (c Successor) check() error {
	_, err := hometerm.Initial().Designate(c.Peer)
	return err
}

// AuthorityAnswer is a home's answer in a heartbeat's authority part: the
// project it answers for, its machine name, its term, and the successor it
// names.
type AuthorityAnswer struct {
	Project   string         `json:"project"`
	Machine   string         `json:"machine"`
	Epoch     uint64         `json:"epoch"`
	State     hometerm.State `json:"state"`
	Successor *Successor     `json:"successor,omitempty"`
}

// HomeView is what this machine knows of a joined project's home without
// asking it: the epoch it recorded, when the home last answered a
// heartbeat (zero: not since the join), and the successor it named (nil:
// none), with the home's epoch when it named it and when it last did.
// Behind is a home that answered BehindEpoch, below Epoch: it lost its
// term, so nothing it answers is taken and no call but the heartbeat and a
// leave goes to it, until it answers Epoch again or this machine's owner
// takes its epoch (Follow with ResetEpoch) or joins again.
type HomeView struct {
	Epoch       uint64
	Heard       time.Time
	Successor   *Successor
	NamedEpoch  uint64
	NamedAt     time.Time
	Behind      bool
	BehindEpoch uint64
}

// memberHome is a remote's HomeView, when heard was last saved, whether
// what is saved of its successor is known to match (synced: false until a
// save succeeds), whether the home's last answer said it does not serve
// (away), and since when this process's beats to it got no answer at all
// (silentSince, on homeNow; zero: it answered the last one); guarded by
// Remote.Mu, changed only under Remote.BeatMu.
type memberHome struct {
	HomeView
	heardSaved  time.Time
	synced      bool
	away        bool
	silentSince time.Time
	changed     chan struct{} // homeChanged
}

// homeChanged is closed when x stops talking to the home it talks to now:
// a repoint, or the home falling behind. A forwarded call (a live stream)
// ends with it (ForwardResp).
func (x *Remote) homeChanged() <-chan struct{} {
	x.Mu.Lock()
	defer x.Mu.Unlock()
	if x.home.changed == nil {
		x.home.changed = make(chan struct{})
	}
	return x.home.changed
}

// homeMoved closes homeChanged's channel; x.Mu is held.
func (x *Remote) homeMoved() {
	if x.home.changed != nil {
		close(x.home.changed)
		x.home.changed = nil
	}
}

// behindError is why nothing goes to a home that is behind (h.Behind).
func behindError(slug string, h HomeView) *api.Error {
	return api.Errorf(api.CodeUnavailable, "the home of %s is behind: it answers epoch %d, this machine recorded %d (it lost its term); "+
		"nothing but the heartbeat is sent to it until it answers epoch %d again", slug, h.BehindEpoch, h.Epoch, h.Epoch)
}

// homeRefusal is why req may not go to x's home (nil: it may): only the
// heartbeat, which hears whether the home is current again, and a leave go
// to a home that is behind. Send asks it with x.Mu held.
func (x *Remote) homeRefusal(req *http.Request) error {
	if !x.home.Behind || isBeat(req) || req.Method == http.MethodPost && req.URL.Path == "/v0/team/leave" {
		return nil
	}
	return behindError(x.R.Project.Slug, x.home.HomeView)
}

// Home is what this machine knows of x's home.
func (x *Remote) Home() HomeView {
	x.Mu.Lock()
	defer x.Mu.Unlock()
	v := x.home.HomeView
	if v.Successor != nil {
		c := *v.Successor
		v.Successor = &c
	}
	return v
}

// Client is x's client to its home (Repoint replaces it).
func (x *Remote) Client() *http.Client {
	x.Mu.Lock()
	defer x.Mu.Unlock()
	return x.HC
}

// ---- the home's side ----

// Designate names succ as the home that serves this home's team projects
// after it (hometerm Designate), keeping its machine name for display.
// Members learn it with their next heartbeat. The change goes through the
// daemon's one door for term changes (core.Deps.ApplyTerm).
func (s *Service) Designate(ctx context.Context, succ Successor) (hometerm.Term, error) {
	succ.Machine = strings.TrimSpace(succ.Machine)
	if err := succ.check(); err != nil {
		return hometerm.Term{}, api.Errorf(api.CodeBadRequest, "%v", err)
	}
	t, err := s.d.ApplyTerm(func(t hometerm.Term) (hometerm.Term, error) { return t.Designate(succ.Peer) })
	if err != nil {
		if errors.Is(err, hometerm.ErrTransition) {
			return hometerm.Term{}, api.Errorf(api.CodeConflict, "%v", err)
		}
		return hometerm.Term{}, err
	}
	if _, err := s.d.Store.Registry().ExecContext(ctx, `INSERT INTO team_designated (fingerprint, machine, designated) VALUES (?, ?, ?)
		ON CONFLICT (fingerprint) DO UPDATE SET machine = excluded.machine, designated = excluded.designated`,
		succ.Fingerprint, succ.Machine, store.Millis(time.Now())); err != nil {
		return t, err // designated; only its display name is not kept
	}
	s.d.Log.Warn("team: this home named its successor", "via", succ.Via, "url", succ.URL, "fingerprint", succ.Fingerprint,
		"machine", succ.Machine, "epoch", t.Epoch)
	return t, nil
}

// Undesignate withdraws the successor this home named (hometerm
// Undesignate); members drop it with their next heartbeat.
func (s *Service) Undesignate(ctx context.Context) (hometerm.Term, error) {
	t, err := s.d.ApplyTerm(hometerm.Term.Undesignate)
	if err != nil {
		if errors.Is(err, hometerm.ErrTransition) {
			return hometerm.Term{}, api.Errorf(api.CodeConflict, "%v", err)
		}
		return hometerm.Term{}, err
	}
	s.d.Log.Warn("team: this home withdrew its successor", "epoch", t.Epoch)
	return t, nil
}

// serveAuthority is the authority part's answer at the home: its term for
// p, read from the home (a term that cannot be read refuses the beat).
func (s *Service) serveAuthority(ctx context.Context, p api.Project, _ json.RawMessage) (json.RawMessage, error) {
	t, err := hometerm.Load(s.d.Config.Home)
	if err != nil {
		return nil, err
	}
	a := AuthorityAnswer{Project: p.ID, Machine: s.d.Config.Machine, Epoch: t.Epoch, State: t.State}
	if t.Successor != nil {
		c := Successor{Peer: *t.Successor}
		_ = s.d.Store.Registry().QueryRowContext(ctx, `SELECT machine FROM team_designated WHERE fingerprint = ?`, c.Fingerprint).Scan(&c.Machine)
		a.Successor = &c
	}
	return json.Marshal(a)
}

// ---- the member's side ----

// requestAuthority is the authority part of a beat to b's home: no value,
// and the answer recorded (heardHome).
func (s *Service) requestAuthority(_ context.Context, b Beat) (json.RawMessage, func(context.Context, json.RawMessage), error) {
	return nil, func(ctx context.Context, raw json.RawMessage) { s.heardHome(ctx, b.Remote, raw) }, nil
}

// heardHome records x's home's answer to a beat (the caller holds
// x.BeatMu): when it answered, a higher epoch, the successor it names, and
// whether it serves (followIfAway acts on that once the whole answer is
// applied). An answer for another project, or at an epoch below the one
// recorded, is not taken.
func (s *Service) heardHome(ctx context.Context, x *Remote, raw json.RawMessage) {
	if raw == nil {
		return // a home without the part (an older Potluck)
	}
	snap := x.Snapshot()
	a, err := parseAnswer(raw)
	if err != nil {
		s.d.Log.Warn("team: the home's term is not taken", "project", snap.Project.Slug, "err", err)
		return
	}
	if a.Project != snap.Project.ID {
		s.d.Log.Warn("team: the home answered its term for another project; not taken", "project", snap.Project.ID, "answered", a.Project)
		return
	}
	if a.Successor != nil {
		if err := a.Successor.check(); err != nil {
			s.d.Log.Warn("team: the home named a successor that is not whole; not taken", "project", snap.Project.Slug, "err", err)
			a.Successor = nil
		}
	}
	now := time.Now()
	x.Mu.Lock()
	h := &x.home
	if a.Epoch < h.Epoch {
		x.Mu.Unlock()
		s.d.Log.Warn("team: the home answered an epoch below the one this machine recorded; not taken",
			"project", snap.Project.Slug, "epoch", a.Epoch, "recorded", h.Epoch)
		return
	}
	// A fenced home answers its successor's epoch, which is the one to
	// follow at, not the one recorded for the home being followed.
	epoch := h.Epoch
	if a.State != hometerm.Fenced {
		epoch = a.Epoch
	}
	moved := epoch != h.Epoch
	named := !sameSuccessor(a.Successor, h.Successor)
	successor := named || a.Successor != nil || !h.synced
	h.Epoch, h.Heard, h.away = epoch, now, a.State != hometerm.Serving
	if a.Successor == nil {
		h.Successor, h.NamedEpoch, h.NamedAt = nil, 0, time.Time{}
	} else {
		c := *a.Successor
		h.Successor, h.NamedEpoch, h.NamedAt = &c, a.Epoch, now
	}
	save := moved || named || !h.synced || now.Sub(h.heardSaved) >= heardEvery
	if save {
		h.heardSaved = now
	}
	x.Mu.Unlock()
	if save {
		if err := s.saveHome(ctx, snap, epoch, a, now, successor); err != nil {
			s.d.Log.Warn("team: save what this machine knows of the home", "project", snap.Project.Slug, "err", err)
			x.Mu.Lock()
			x.home.synced = false // the next answer saves its successor again
			x.Mu.Unlock()
		} else if successor {
			x.Mu.Lock()
			x.home.synced = true
			x.Mu.Unlock()
		}
	}
	if named && a.Successor != nil {
		s.d.Log.Info("team: the project's home named its successor", "project", snap.Project.Slug,
			"url", a.Successor.URL, "fingerprint", a.Successor.Fingerprint, "machine", a.Successor.Machine, "epoch", a.Epoch)
	}
}

// saveHome writes a home's answer to r's join: the epoch recorded and
// heard on its row (neither goes down within a join) and, with successor,
// its named successor (removed when it names none). Nothing is written
// when the row is another join's.
func (s *Service) saveHome(ctx context.Context, r api.Remote, epoch uint64, a AuthorityAnswer, heard time.Time, successor bool) error {
	project := r.Project.ID
	tx, err := s.d.Store.Registry().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx, `UPDATE team_remotes SET term = MAX(term, ?), heard = MAX(heard, ?) WHERE project = ? AND joined = ?`,
		epoch, store.Millis(heard), project, store.Millis(r.Joined))
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return nil // left, or joined again meanwhile: the answer was the old join's
	}
	if successor {
		if a.Successor == nil {
			_, err = tx.ExecContext(ctx, `DELETE FROM team_successors WHERE project = ?`, project)
		} else {
			c := a.Successor
			_, err = tx.ExecContext(ctx, `INSERT INTO team_successors (project, via, url, fingerprint, machine, epoch, named) VALUES (?, ?, ?, ?, ?, ?, ?)
				ON CONFLICT (project) DO UPDATE SET via = excluded.via, url = excluded.url, fingerprint = excluded.fingerprint,
					machine = excluded.machine, epoch = excluded.epoch, named = excluded.named`,
				project, c.Via, c.URL, c.Fingerprint, c.Machine, a.Epoch, store.Millis(heard))
		}
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

// heardMillis is t for a row: 0 when never.
func heardMillis(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return store.Millis(t)
}

// parseAnswer reads an authority part's answer; an epoch a registry row
// cannot hold (above MaxInt64) is refused.
func parseAnswer(raw json.RawMessage) (AuthorityAnswer, error) {
	var a AuthorityAnswer
	if err := json.Unmarshal(raw, &a); err != nil {
		return a, fmt.Errorf("it does not parse: %v", err)
	}
	if a.Epoch > hometerm.MaxEpoch {
		return a, fmt.Errorf("epoch %d is out of range", a.Epoch)
	}
	return a, nil
}

// homeCurrent reports whether x's home's answer to a beat may be taken
// (BeatRemote asks before it applies any of it), and counts the answer as
// the home's (silence ends). Not when its authority part says it serves
// below the epoch this machine recorded for it, which a home that lost its
// term (a restored copy, a missing term file) would: the remote is marked
// behind (HomeView.Behind, its last error, the registry) until the home
// answers the recorded epoch again. A fenced home answers its successor's
// epoch, never lower. An answer that says nothing of the epoch (no part: an
// older Potluck; one that does not parse; another project's) leaves it as
// it was.
func (s *Service) homeCurrent(ctx context.Context, x *Remote, raw json.RawMessage) bool {
	a, err := parseAnswer(raw)
	x.Mu.Lock()
	h := &x.home
	h.silentSince = time.Time{}
	was, wasEpoch := h.Behind, h.BehindEpoch
	if raw == nil || err != nil || a.Project != x.R.Project.ID {
		x.Mu.Unlock()
		return !was
	}
	h.Behind = a.Epoch < h.Epoch
	h.BehindEpoch = 0
	if h.Behind {
		h.BehindEpoch = a.Epoch
		if !was {
			x.homeMoved()
		}
	}
	if x.R.State == api.RemoteOK {
		x.R.LastError = ""
		if h.Behind {
			x.R.LastError = behindError(x.R.Project.Slug, h.HomeView).Message
		}
	}
	behind, view, snap := h.Behind, h.HomeView, x.R
	x.Mu.Unlock()
	if behind == was && view.BehindEpoch == wasEpoch {
		return !behind
	}
	var col any
	if behind {
		col = int64(view.BehindEpoch)
	}
	if _, err := s.d.Store.Registry().ExecContext(ctx, `UPDATE team_remotes SET behind = ?, last_error = ? WHERE project = ? AND joined = ?`,
		col, snap.LastError, snap.Project.ID, store.Millis(snap.Joined)); err != nil {
		s.d.Log.Warn("team: save whether the home is behind", "project", snap.Project.Slug, "err", err)
	}
	if behind {
		s.d.Log.Warn("team: the project's home answered an epoch below the one this machine recorded; nothing of its answers is taken",
			"project", snap.Project.Slug, "url", snap.URL, "epoch", view.BehindEpoch, "state", a.State, "recorded", view.Epoch)
	} else {
		s.d.Log.Warn("team: the project's home answers the epoch this machine recorded again", "project", snap.Project.Slug, "epoch", a.Epoch)
	}
	return !behind
}

// unheard is whether resp, x's home's answer to req, is kept from Observe
// (Send): every answer while the home is behind, and a heartbeat's answer
// whose authority part shows it behind now (BeatRemote marks it), so a home
// that lost its term revokes, rescopes or purges nothing here.
func (s *Service) unheard(x *Remote, req *http.Request, resp *http.Response) bool {
	x.Mu.Lock()
	behind, epoch, id := x.home.Behind, x.home.Epoch, x.R.Project.ID
	x.Mu.Unlock()
	if behind {
		return true
	}
	if !isBeat(req) || resp.StatusCode != http.StatusOK || epoch == 0 {
		return false
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, MaxBeatBody+1))
	resp.Body.Close()
	resp.Body = io.NopCloser(bytes.NewReader(b))
	if err != nil {
		return false // Decode fails on it too
	}
	var pr api.PresenceResponse
	if json.Unmarshal(b, &pr) != nil || pr.Parts[AuthorityPart] == nil {
		return false
	}
	a, err := parseAnswer(pr.Parts[AuthorityPart])
	return err == nil && a.Project == id && a.Epoch < epoch
}

func sameSuccessor(a, b *Successor) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// homeSilent is a beat to x's home that failed with err (the caller holds
// x.BeatMu): when the home did not answer at all (awayErr), and no beat of
// this process got an answer for HomeSilentAfter since the first that did
// not, the member asks the successor it named, if any. An answer in the
// home's own words ends the silence; one saying it is parked (it does not
// serve now: kit/server term.go) asks the successor at once, as an
// authority part saying so does (followIfAway).
func (s *Service) homeSilent(ctx context.Context, x *Remote, err error) {
	now := homeNow()
	x.Mu.Lock()
	h := &x.home
	if !awayErr(err) {
		h.silentSince = time.Time{}
		x.Mu.Unlock()
		if api.IsCode(err, api.CodeParked) {
			s.followNamed(ctx, x)
		}
		return
	}
	if h.silentSince.IsZero() {
		h.silentSince = now
	}
	follow := h.Successor != nil && now.Sub(h.silentSince) >= HomeSilentAfter
	x.Mu.Unlock()
	if follow {
		s.followNamed(ctx, x)
	}
}

// awayErr reports whether a call's error says its home did not answer:
// unreachable, a key at its address that is not the pinned one, a status
// from something between. Every answer in the home's own words (a refusal
// of this machine's token, a rescope, unavailable) is the home itself.
func awayErr(err error) bool {
	var ae *api.Error
	return !errors.As(err, &ae) || ae.HTTPStatus == 0
}

// followIfAway follows x's named successor when x's home's last answer said
// it does not serve (heardHome); BeatRemote calls it once the whole answer
// is applied, so nothing of that answer lands after a repoint. The caller
// holds x.BeatMu.
func (s *Service) followIfAway(ctx context.Context, x *Remote) {
	x.Mu.Lock()
	away := x.home.away
	x.Mu.Unlock()
	if away {
		s.followNamed(ctx, x)
	}
}

// followNamed follows x's named successor when it serves x's project at a
// higher epoch than this machine recorded (the caller holds x.BeatMu).
func (s *Service) followNamed(ctx context.Context, x *Remote) {
	h, snap := x.Home(), x.Snapshot()
	if h.Successor == nil || joinEnded(x) != nil {
		return // a join being left refuses its own calls: that is not its home's silence
	}
	a, err := s.probe(ctx, x, *h.Successor)
	if err == nil {
		err = followable(snap.Project.ID, h.Epoch, a)
	}
	if err != nil {
		s.d.Log.Debug("team: not following the named successor", "project", snap.Project.Slug, "url", h.Successor.URL, "err", err)
		return
	}
	if err := s.repoint(ctx, x, *h.Successor, a, "", "its home named it"); err != nil {
		s.d.Log.Warn("team: following the named successor failed", "project", snap.Project.Slug, "url", h.Successor.URL, "err", err)
	}
}

// followable is nil when a home that answered a with its term may become
// project's home on a machine that recorded epoch: it serves project
// (serves), at a higher epoch.
func followable(project string, epoch uint64, a AuthorityAnswer) error {
	if err := serves(project, a); err != nil {
		return err
	}
	if a.Epoch <= epoch {
		return api.Errorf(api.CodeConflict, "the successor serves at epoch %d, not above the %d this machine recorded for the project's home", a.Epoch, epoch)
	}
	return nil
}

// serves is nil when a home that answered a with its term serves project.
func serves(project string, a AuthorityAnswer) error {
	switch {
	case a.Project != project:
		return api.Errorf(api.CodeConflict, "the successor answered for project %q, not %s", a.Project, project)
	case a.State != hometerm.Serving:
		return api.Errorf(api.CodeConflict, "the successor is %s at epoch %d; it does not serve", a.State, a.Epoch)
	}
	return nil
}

// resettable is nil when a, the answer of succ, lets the owner lower the
// epoch recorded for r's home (h) to a.Epoch: succ is that home, it is
// behind, and it serves at the epoch it fell behind to.
func resettable(r api.Remote, h HomeView, succ Successor, a AuthorityAnswer) error {
	switch {
	case !h.Behind:
		return api.Errorf(api.CodeConflict, "the home of %s is not behind; its epoch %d stays", r.Project.Slug, h.Epoch)
	case succ.URL != r.URL || succ.Fingerprint != r.Fingerprint || succ.Via != cmp.Or(r.Via, api.ViaTailnet):
		return api.Errorf(api.CodeConflict, "an epoch is taken only from the home itself (%s, key %s), not %s", r.URL, r.Fingerprint, succ.URL)
	case a.Epoch != h.BehindEpoch:
		return api.Errorf(api.CodeConflict, "the home answers epoch %d now, not the %d it fell behind to; nothing was taken", a.Epoch, h.BehindEpoch)
	}
	return serves(r.Project.ID, a)
}

// joinEnded is why x's join takes no new home: it was left, is being
// left, or its access ended.
func joinEnded(x *Remote) error {
	x.Mu.Lock()
	left := x.Left || x.Recovery.Closing
	r := x.R
	x.Mu.Unlock()
	if err := Ended(r); err != nil || left {
		return api.Errorf(api.CodeConflict, "this machine's join of %s was left or ended; nothing was repointed", r.Project.Slug)
	}
	return nil
}

// successorClient is a client to succ for x, pinned to succ's key: it
// carries the headers stored for x's home (a service token) only when succ
// is at the same address and transport, since a token given for one address
// is not sent to another.
func (s *Service) successorClient(x *Remote, succ Successor) *http.Client {
	return s.client(core.JoinTarget{Via: succ.Via, URL: succ.URL, Authority: succ.Machine, Fingerprint: succ.Fingerprint}, s.headersFor(x, succ))
}

// headersFor is the stored headers of x's home when succ is at its address
// and transport, else nil.
func (s *Service) headersFor(x *Remote, succ Successor) http.Header {
	r := x.Snapshot()
	if succ.URL != r.URL || succ.Via != cmp.Or(r.Via, api.ViaTailnet) {
		return nil
	}
	v, err := secrets.Get(context.Background(), secrets.File(s.headersPath(r.Project.ID)))
	if err != nil {
		return nil
	}
	return decodeHeaders(v.Reveal())
}

// SuccessorClient is a client pinned to the successor that project's home
// named, and that successor, for a caller that asks the successor something
// on this machine's behalf. Nil and an error when none is named.
func (s *Service) SuccessorClient(project string) (*http.Client, Successor, error) {
	x := s.ByName(project)
	if x == nil {
		return nil, Successor{}, api.NotFound("joined project", project)
	}
	h := x.Home()
	if h.Successor == nil {
		return nil, Successor{}, api.Errorf(api.CodeNotFound, "the home of %s names no successor", x.Slug())
	}
	return s.successorClient(x, *h.Successor), *h.Successor, nil
}

// probe asks succ for its term for x's project: a heartbeat that carries
// only the authority part (no op reports, nothing taken; an op it answers
// goes again with the next beat), with x's token, over a client pinned to
// succ, so the token reaches succ's key or nothing. A home restored from
// x's home's data knows the token.
func (s *Service) probe(ctx context.Context, x *Remote, succ Successor) (AuthorityAnswer, error) {
	hc := s.successorClient(x, succ)
	defer hc.CloseIdleConnections()
	x.Mu.Lock()
	token, id := x.Token, x.R.Project.ID
	x.Mu.Unlock()
	cctx, cancel := context.WithTimeout(ctx, BeatTimeout)
	defer cancel()
	req := api.PresenceRequest{Project: id, Parts: map[string]json.RawMessage{AuthorityPart: json.RawMessage(`{}`)}}
	var resp api.PresenceResponse
	if err := s.call(cctx, succ.Via, hc, http.MethodPost, succ.URL+"/v0/team/presence", token, req, &resp); err != nil {
		return AuthorityAnswer{}, err
	}
	raw := resp.Parts[AuthorityPart]
	if raw == nil {
		return AuthorityAnswer{}, api.Errorf(api.CodeUnavailable, "the successor at %s answered no term", succ.URL)
	}
	a, err := parseAnswer(raw)
	if err != nil {
		return AuthorityAnswer{}, api.Errorf(api.CodeUnavailable, "the successor's term is not taken: %v", err)
	}
	return a, nil
}

// FollowOptions are a Follow's choices.
type FollowOptions struct {
	// ResetEpoch is the owner's way back to a home that lost its term: succ
	// must be x's home itself, marked behind (HomeView.Behind), serving
	// the project at exactly the epoch it answered when it fell behind;
	// the recorded epoch is lowered to that one. Logged.
	ResetEpoch bool
}

// Follow is this machine's owner repointing joined project to succ when its
// home did not name it: bound must be the key the project id is bound to
// now (GET /v0/team/project-keys/{project}), and succ must answer, over a
// client pinned to its key and with this machine's token, that it serves
// the project at an epoch above the one recorded (with opt.ResetEpoch: the
// home itself, at the epoch it fell behind to). The same repoint as a named
// follow.
func (s *Service) Follow(ctx context.Context, project string, succ Successor, bound string, opt FollowOptions) (api.Remote, error) {
	x := s.ByName(project)
	if x == nil {
		return api.Remote{}, api.NotFound("joined project", project)
	}
	succ.Machine = strings.TrimSpace(succ.Machine)
	if err := succ.check(); err != nil {
		return api.Remote{}, api.Errorf(api.CodeBadRequest, "%v", err)
	}
	snap := x.Snapshot()
	if bound == "" {
		return api.Remote{}, api.Errorf(api.CodeBadRequest, "a follow names the key project %s is bound to now (fingerprint): the one GET /v0/team/project-keys/%s shows",
			snap.Project.ID, snap.Project.ID)
	}
	x.BeatMu.Lock()
	defer x.BeatMu.Unlock()
	if err := joinEnded(x); err != nil { // after the lock: a leave may have finished while it waited
		return api.Remote{}, err
	}
	if k, err := s.projectKey(ctx, snap.Project.ID); err != nil {
		return api.Remote{}, err
	} else if k.Fingerprint != bound {
		return api.Remote{}, api.Errorf(api.CodeConflict, "project %s is bound to key %s over %s, not %s; nothing was repointed", snap.Project.ID, k.Fingerprint, k.Via, bound)
	}
	a, err := s.probe(ctx, x, succ)
	if err != nil {
		return api.Remote{}, err
	}
	h, why := x.Home(), "this machine's owner named it"
	recorded := h.Epoch
	if opt.ResetEpoch {
		err, why = resettable(x.Snapshot(), h, succ, a), "this machine's owner took the epoch its home fell behind to"
	} else {
		err = followable(snap.Project.ID, recorded, a)
	}
	if err != nil {
		return api.Remote{}, err
	}
	if err := s.repoint(ctx, x, succ, a, bound, why); err != nil {
		return api.Remote{}, err
	}
	if a.Epoch < recorded {
		s.d.Log.Warn("team: this machine's owner lowered the recorded epoch of the project's home", "project", snap.Project.Slug,
			"from_epoch", recorded, "to_epoch", a.Epoch, "url", succ.URL, "fingerprint", succ.Fingerprint)
	}
	return x.Snapshot(), nil
}

// Repoint makes succ x's home at the epoch it answered (a), under keyMu (a
// forget sees the old binding with the old record, or the new with the
// new): the project key is rebound from x's current key to succ's, and x's
// row gets succ's address, key and transport, the machine name succ answered
// with (the name its join codes carry; the designated one when it gave none)
// and the new epoch, in one transaction;
// the named successor is used up. In memory x gets a client pinned to succ
// and its 401 count is cleared (the old home's refusals say nothing of the
// new one). The token file, the copy and the host state stay; the old
// home's stored headers stay only when succ is at the same address and
// transport (headersFor). A remote left or ended meanwhile is refused.
// bound, when set,
// is the key the caller saw bound (Follow); either way the binding must be
// x's current key. The caller holds x.BeatMu and has checked succ
// (followable). Hooks.Repointed runs after.
func (s *Service) repoint(ctx context.Context, x *Remote, succ Successor, a AuthorityAnswer, bound, why string) error {
	machine, epoch := cmp.Or(a.Machine, succ.Machine), a.Epoch
	extra := s.headersFor(x, succ)
	old, err := s.rebind(ctx, x, succ, machine, epoch, bound, extra)
	if err != nil {
		return err
	}
	if extra == nil {
		_ = removeFile(s.headersPath(old.Project.ID))
	}
	s.d.Log.Warn("team: repointed a joined project to its new home; its project key is rebound",
		"project", old.Project.ID, "slug", old.Project.Slug, "why", why,
		"from_url", old.URL, "from_fingerprint", old.Fingerprint, "from_via", cmp.Or(old.Via, api.ViaTailnet), "from_machine", old.Authority,
		"to_url", succ.URL, "to_fingerprint", succ.Fingerprint, "to_via", succ.Via, "to_machine", machine, "epoch", epoch)
	if s.Hooks.Repointed != nil {
		s.Hooks.Repointed(x, old)
	}
	s.KickBeat() // the new home hears from this machine now
	return nil
}

// rebind is repoint's durable part and its swap in memory; it answers the
// remote as it was.
func (s *Service) rebind(ctx context.Context, x *Remote, succ Successor, machine string, epoch uint64, bound string, extra http.Header) (api.Remote, error) {
	s.keyMu.Lock()
	defer s.keyMu.Unlock()
	x.saveMu.Lock() // no save of the old address lands after this one
	defer x.saveMu.Unlock()
	old := x.Snapshot()
	if err := joinEnded(x); err != nil {
		return old, err
	}
	oldVia := cmp.Or(old.Via, api.ViaTailnet)
	k, err := s.projectKey(ctx, old.Project.ID)
	if err != nil {
		return old, err
	}
	if k.Via != oldVia || k.Fingerprint != old.Fingerprint {
		return old, api.Errorf(api.CodeConflict, "project %s is bound to key %s over %s, not its home's (%s over %s); nothing was repointed",
			old.Project.ID, k.Fingerprint, k.Via, old.Fingerprint, oldVia)
	}
	if bound != "" && bound != k.Fingerprint {
		return old, api.Errorf(api.CodeConflict, "project %s is bound to key %s, not %s; nothing was repointed", old.Project.ID, k.Fingerprint, bound)
	}
	now := time.Now()
	tx, err := s.d.Store.Registry().BeginTx(ctx, nil)
	if err != nil {
		return old, err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx, `UPDATE team_project_keys SET via = ?, fingerprint = ?, authority = ?, bound = ? WHERE project = ? AND via = ? AND fingerprint = ?`,
		succ.Via, succ.Fingerprint, machine, store.Millis(now), old.Project.ID, k.Via, k.Fingerprint)
	if err != nil {
		return old, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return old, api.Errorf(api.CodeConflict, "project %s's key changed meanwhile; nothing was repointed", old.Project.ID)
	}
	res, err = tx.ExecContext(ctx, `UPDATE team_remotes SET url = ?, fingerprint = ?, via = ?, authority = ?, term = ?, heard = ?, behind = NULL,
		last_error = CASE WHEN state = ? THEN '' ELSE last_error END WHERE project = ? AND joined = ?`,
		succ.URL, succ.Fingerprint, succ.Via, machine, epoch, store.Millis(now), api.RemoteOK, old.Project.ID, store.Millis(old.Joined))
	if err != nil {
		return old, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return old, api.Errorf(api.CodeConflict, "this machine records no join of %s any more; nothing was repointed", old.Project.Slug)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM team_successors WHERE project = ?`, old.Project.ID); err != nil {
		return old, err
	}
	if err := tx.Commit(); err != nil {
		return old, err
	}
	hc := s.client(core.JoinTarget{Via: succ.Via, URL: succ.URL, Authority: machine, Fingerprint: succ.Fingerprint}, extra)
	x.Mu.Lock()
	prev := x.HC
	x.R.URL, x.R.Fingerprint, x.R.Via, x.R.Authority = succ.URL, succ.Fingerprint, succ.Via, machine
	x.HC, x.FreshHC, x.Unauth = hc, nil, nil
	if x.home.Behind && x.R.State == api.RemoteOK {
		x.R.LastError = ""
	}
	x.homeMoved()
	x.home = memberHome{HomeView: HomeView{Epoch: epoch, Heard: now}, heardSaved: now, synced: true}
	x.Mu.Unlock()
	if prev != nil {
		prev.CloseIdleConnections()
	}
	return old, nil
}

// loadHomes reads what this machine knows of each joined project's home
// into m (Load): the epoch, when it last answered, whether it is behind,
// and the successor it named while this join stood (a row an earlier join
// left is not this join's).
func (s *Service) loadHomes(ctx context.Context, m map[string]*Remote) error {
	rows, err := s.d.Store.Registry().QueryContext(ctx, `SELECT r.project, r.term, r.heard, r.behind,
		c.via, c.url, c.fingerprint, c.machine, c.epoch, c.named
		FROM team_remotes r LEFT JOIN team_successors c ON c.project = r.project AND c.named >= r.joined`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var project string
		var epoch uint64
		var heard int64
		var behind, cEpoch, named sql.NullInt64
		var via, url, fp, machine sql.NullString
		if err := rows.Scan(&project, &epoch, &heard, &behind, &via, &url, &fp, &machine, &cEpoch, &named); err != nil {
			return err
		}
		x := m[project]
		if x == nil {
			continue
		}
		h := &x.home
		h.Epoch = epoch
		if heard > 0 {
			h.Heard = store.FromMillis(heard)
			h.heardSaved = h.Heard
		}
		if behind.Valid {
			h.Behind, h.BehindEpoch = true, uint64(behind.Int64)
			if x.R.State == api.RemoteOK {
				x.R.LastError = behindError(x.R.Project.Slug, h.HomeView).Message
			}
		}
		if url.Valid {
			c := Successor{Peer: hometerm.Peer{Via: via.String, URL: url.String, Fingerprint: fp.String}, Machine: machine.String}
			h.Successor, h.NamedEpoch, h.NamedAt = &c, uint64(cEpoch.Int64), store.FromMillis(named.Int64)
		}
	}
	return rows.Err()
}
