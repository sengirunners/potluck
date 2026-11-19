package teamlink

// Which authority a team project's id belongs to on this machine. The id is
// the authority's to choose: a join takes the id its authority answers with.
// What this machine keeps for a project outlives the join, and is keyed by
// the id: a host's shares and handle ledger (runtarget), the request key tag
// of its boxes and volumes. A left join's record is deleted; a revoked or
// purged one is replaced by the next join of its id. So the first join of an
// id that works binds the id to its authority's key: the fingerprint the
// join pinned and the transport it is pinned on, never the authority's
// address or machine name, which another authority can take. A leave, a
// revocation or a purge never removes the binding: a join that presents a
// bound id at another key is refused, and nothing this machine kept for
// that project passes to the other authority.
//
// Only this machine's owner removes a binding (zm forget): when the
// project's own authority got a new key (a new tailnet node) or is now
// reached another way (a tunnel code, a GitHub join over https). A forget
// is never automatic, is refused while this machine records a join of the
// id (in any state: zm leave first), names the key it forgets, and says
// what becomes of what this machine kept for the id: kept for whichever
// authority joins the id next, or dropped (Hooks.DropForgotten).

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"github.com/sengirunners/potluck/kit/config"
	"net/http"
	"strings"
	"time"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/ids"
	"github.com/sengirunners/potluck/kit/store"
)

var projectKeyMigrations = []store.Migration{
	// The joins recorded when this table was made bind their ids. Joins left
	// before then left no record: their ids are bound by the next join.
	{Kind: store.KindRegistry, Name: "team/0009_project_keys", Script: `
CREATE TABLE team_project_keys (
	project     TEXT PRIMARY KEY,
	via         TEXT NOT NULL,
	fingerprint TEXT NOT NULL,
	authority   TEXT NOT NULL,
	bound       INTEGER NOT NULL
);
INSERT INTO team_project_keys (project, via, fingerprint, authority, bound)
	SELECT project, via, fingerprint, authority, joined FROM team_remotes;`},
}

// checkProjectKey is a join's check before anything is written: the
// project its authority answered with (cap) must be a project id, not one
// of this machine's own projects, and not bound to another key than this
// authority's (via, fingerprint). authority is the authority's machine name,
// for the refusal's words only.
func (s *Service) checkProjectKey(ctx context.Context, cap api.TeamCap, via, fingerprint, authority string) error {
	if !ids.Valid(cap.Project) {
		return api.Errorf(api.CodeBadRequest, "the authority answered with project id %q, which is not a project id; this join is refused", cap.Project)
	}
	if p, ok := s.d.Projects.Lookup(cap.Project); ok && p.ID == cap.Project {
		return api.Errorf(api.CodeForbidden, "the authority answered with the id of %s, a project of this machine's own; this join is refused", p.Slug)
	}
	if fingerprint == "" {
		return api.Errorf(api.CodeBadRequest, "this join pins no key for the authority of %s; it is refused", cap.Slug)
	}
	return s.sameProjectKey(ctx, cap, via, fingerprint, authority)
}

// bindProjectKey binds cap's project to the authority's key, once the join
// has worked (its first call answered) and just before its record is saved:
// a join that fails binds nothing, so an authority cannot claim an id by
// answering with it and then failing. A join of the same id at another key
// that bound it since checkProjectKey is refused here.
func (s *Service) bindProjectKey(ctx context.Context, cap api.TeamCap, via, fingerprint, authority string) error {
	if _, err := s.d.Store.Registry().ExecContext(ctx, `INSERT INTO team_project_keys (project, via, fingerprint, authority, bound) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (project) DO NOTHING`, cap.Project, cmp.Or(via, api.ViaTailnet), fingerprint, authority, store.Millis(time.Now())); err != nil {
		return err
	}
	return s.sameProjectKey(ctx, cap, via, fingerprint, authority)
}

// sameProjectKey refuses a join when cap's project is bound to another key.
func (s *Service) sameProjectKey(ctx context.Context, cap api.TeamCap, via, fingerprint, authority string) error {
	via = cmp.Or(via, api.ViaTailnet) // as Save records it
	var bVia, bFP, bAuth string
	err := s.d.Store.Registry().QueryRowContext(ctx, `SELECT via, fingerprint, authority FROM team_project_keys WHERE project = ?`, cap.Project).
		Scan(&bVia, &bFP, &bAuth)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && bVia == via && bFP == fingerprint) {
		return nil
	}
	if err != nil {
		return err
	}
	return api.Errorf(api.CodeForbidden, "this machine first joined project %s at another authority (%s, key %s over %s); this join's authority (%s, key %s over %s) answers with the same project id, so the join is refused: the boxes, shares and records this machine keeps for that project stay with the authority it first joined",
		cap.Project, bAuth, bFP, bVia, authority, fingerprint, via).
		WithHint("ask the project's owner for a join code from the authority this machine first joined it at; " +
			"if that authority itself got a new key or is now reached another way, this machine's owner can forget the old key: " + config.Command("forget", cap.Project))
}

// bindAndSave binds cap's project to the authority's key, saves x and
// publishes it, under keyMu: a forget sees a join's binding and its live
// record together, or neither.
func (s *Service) bindAndSave(ctx context.Context, x *Remote, cap api.TeamCap, via, fingerprint, authority string) error {
	s.keyMu.Lock()
	defer s.keyMu.Unlock()
	if err := s.bindProjectKey(ctx, cap, via, fingerprint, authority); err != nil {
		return err
	}
	if err := s.Save(ctx, x); err != nil {
		return err
	}
	s.Add(x)
	return nil
}

func (s *Service) projectKeyRoutes(r *core.Router) {
	r.HandleFunc("GET /v0/team/project-keys/{project}", core.Local, "local: the authority key a project id is bound to", jsonH(func(r *http.Request) (any, error) {
		id, err := keyID(r)
		if err != nil {
			return nil, err
		}
		return s.projectKey(r.Context(), id)
	}))
	r.HandleFunc("POST /v0/team/project-keys/{project}/forget", core.Local, "local: this machine's owner forgets a project id's authority key", jsonH(s.handleForgetKey))
}

// noJoinRecorded refuses a forget while registry.db records a join of id,
// whatever its state: a revoked one turns live again by itself (role
// recovery), and any record loads at the next start, and would then hold
// no binding. It reads the table, not the joins in memory, which are empty
// before Load and can miss a row a failed delete left; and it refuses
// before the joins are loaded. Call with keyMu held: a join's save and its
// binding happen under it.
func (s *Service) noJoinRecorded(ctx context.Context, id string) error {
	if !s.Loaded() {
		return api.Errorf(api.CodeUnavailable, "this machine's joins are not loaded yet; nothing was forgotten").
			WithHint("try again once the daemon has started")
	}
	var slug, state string
	err := s.d.Store.Registry().QueryRowContext(ctx, `SELECT slug, state FROM team_remotes WHERE project = ?`, id).Scan(&slug, &state)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	hint := config.Command("leave", slug) + " first"
	if s.ByName(id) == nil { // a row a failed delete left: the next start loads it
		hint = "restart the daemon (" + config.Command("daemon stop") + ", then " + config.Command("daemon", "--detach") + "), then " + config.Command("leave", slug)
	}
	return api.Errorf(api.CodeConflict, "this machine has a join of %s (%s, %s); its authority key is not forgotten while the join is recorded", slug, id, state).
		WithHint(hint)
}

// DropForgotten runs every Hooks.DropForgotten for id in order, stopping at
// the first error; it returns what they dropped.
func (s *Service) DropForgotten(ctx context.Context, id string) ([]string, error) {
	var out []string
	for _, drop := range s.Hooks.DropForgotten {
		d, err := drop(ctx, id)
		out = append(out, d...)
		if err != nil {
			return out, err
		}
	}
	return out, nil
}

func keyID(r *http.Request) (string, error) {
	id := strings.ToUpper(r.PathValue("project"))
	if !ids.Valid(id) {
		return "", api.Errorf(api.CodeBadRequest, "%q is not a project id", r.PathValue("project")).
			WithHint("a project id is the 26-character id a refused join names")
	}
	return id, nil
}

func (s *Service) projectKey(ctx context.Context, id string) (api.ProjectKey, error) {
	k := api.ProjectKey{Project: id}
	var bound int64
	err := s.d.Store.Registry().QueryRowContext(ctx, `SELECT via, fingerprint, authority, bound FROM team_project_keys WHERE project = ?`, id).
		Scan(&k.Via, &k.Fingerprint, &k.Authority, &bound)
	if errors.Is(err, sql.ErrNoRows) {
		return k, api.NotFound("authority key for project", id)
	}
	k.Bound = store.FromMillis(bound)
	return k, err
}

// handleForgetKey is POST /v0/team/project-keys/{project}/forget: the
// binding goes, so the next join of the id binds it again, at whatever key
// that join pins. Refused while this machine records a join of the id in
// any state (leave first), without a choice for what this machine kept for
// the id, and when the request does not name the bound key (the one the
// owner was shown).
func (s *Service) handleForgetKey(r *http.Request) (any, error) {
	ctx := r.Context()
	id, err := keyID(r)
	if err != nil {
		return nil, err
	}
	var req api.ForgetProjectKeyRequest
	if err := api.DecodeJSON(r, &req); err != nil {
		return nil, err
	}
	if req.Fingerprint == "" {
		return nil, api.Errorf(api.CodeBadRequest, "a forget names the key it forgets (fingerprint): the one GET /v0/team/project-keys/%s shows", id)
	}
	if req.HostState != api.ForgetKeepHostState && req.HostState != api.ForgetDropHostState {
		return nil, api.Errorf(api.CodeBadRequest, "a forget says what becomes of what this machine kept for project %s (host shares, handles): host_state %q is neither %q nor %q",
			id, req.HostState, api.ForgetKeepHostState, api.ForgetDropHostState)
	}
	s.keyMu.Lock()
	defer s.keyMu.Unlock()
	k, err := s.projectKey(ctx, id)
	if err != nil {
		return nil, err
	}
	if req.Fingerprint != k.Fingerprint {
		return nil, api.Errorf(api.CodeConflict, "project %s is bound to key %s over %s, not %s; nothing was forgotten", id, k.Fingerprint, k.Via, req.Fingerprint)
	}
	if err := s.noJoinRecorded(ctx, id); err != nil {
		return nil, err
	}
	// An op still running for the old authority would, when it ends, record
	// what it did (a handle in the host's ledger) and report it to whichever
	// authority then holds the id. Only ops this process runs count: a row
	// left running by a process that stopped mid-op never ends, and the
	// forget deletes it with the rest.
	if running := s.runningOps(id); running > 0 {
		return nil, api.Errorf(api.CodeConflict, "%d machine ops for project %s are still running here; nothing was forgotten", running, id).
			WithHint("try again once they end (an op ends within its time limit)")
	}
	out := api.ForgetProjectKeyResponse{Key: k, HostState: req.HostState, Dropped: []string{}}
	if req.HostState == api.ForgetDropHostState {
		d, err := s.DropForgotten(ctx, id)
		out.Dropped = append(out.Dropped, d...)
		if err != nil {
			s.d.Log.Warn("team: forget a project key: drop failed; the key stays bound", "project", id, "dropped", out.Dropped, "err", err)
			return nil, api.Errorf(api.CodeUnavailable, "dropping what this machine kept for project %s failed (%v); its authority key stays bound; dropped before the failure: %s",
				id, err, cmp.Or(strings.Join(out.Dropped, ", "), "nothing")).
				WithHint("fix what the error names, then run the same forget again")
		}
	}
	// The ops this machine ran for the old authority, and their reports not
	// yet sent, are the old authority's in either mode: kept, the next
	// join's heartbeat would carry them to whichever authority holds the id.
	tx, err := s.d.Store.Registry().BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	for _, q := range []string{`DELETE FROM team_ops_run WHERE project = ?`, `DELETE FROM team_task_refs WHERE project = ?`} {
		if _, err := tx.ExecContext(ctx, q, id); err != nil {
			return nil, err
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM team_project_keys WHERE project = ? AND via = ? AND fingerprint = ?`,
		id, k.Via, k.Fingerprint); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	s.d.Log.Warn("team: this machine's owner forgot a project id's authority key: the next join of the id binds it again",
		"project", id, "via", k.Via, "fingerprint", k.Fingerprint, "authority", k.Authority, "host_state", req.HostState, "dropped", out.Dropped)
	return out, nil
}
