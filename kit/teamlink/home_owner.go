package teamlink

// Who pressed an HQ action (home_control.go): the one permission check of
// every press, on a serving daemon and a parked one.

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/hometerm"
	"github.com/sengirunners/potluck/kit/project"
	"github.com/sengirunners/potluck/kit/secrets"
	"github.com/sengirunners/potluck/kit/store"
)

// pressedBy is who pressed an HQ action on this daemon's home (r's caller),
// or the refusal: the one permission check of every press, on a serving
// daemon and a parked one. person: the press must be a person's own, never
// a session's.
//
//   - On a parked daemon (d.Parked): on its socket only, with no token, as
//     this home's owner by its registry.db (on a standby: by the tokens it
//     holds), who must own every team project it holds (parkedOwner).
//   - On a serving daemon's socket: its owner with no token (a token always
//     narrows), as a local route admits. A press there acts on this
//     daemon's own home only.
//   - From the network, on a serving daemon: a member machine's node-bound
//     token whose identity is owner of every team project this home holds,
//     by its own grants here (the token itself is capped at member; a
//     grant on an area counts for nothing). A minted token, a box's token, a
//     limited token and a tailnet login never pass, nor does anyone when
//     the home holds no team project. The refusal names each project the
//     caller can see but does not own, with its owners, and counts the rest.
func pressedBy(r *http.Request, d *core.Deps, person bool) (api.MoveActor, error) {
	now := time.Now().UTC()
	if d.Parked {
		return parkedPress(r, d, person, now)
	}
	ctx := r.Context()
	sub := core.SubjectOf(ctx)
	if core.IsLocal(ctx) {
		switch {
		case sub == nil || sub.Remote || sub.Via != authz.ViaLocal || sub.TokenID != "":
			return api.MoveActor{}, notOwner()
		case person && !personal(sub):
			return api.MoveActor{}, notPerson()
		}
		return api.MoveActor{Identity: sub.Identity.Name, Machine: d.Config.Machine, At: now}, nil
	}
	if d.Store == nil || sub == nil || !nodeBound(sub) || sub.Machine == "" || sub.Identity.ID == "" {
		return api.MoveActor{}, notOwner()
	}
	if person && !personal(sub) {
		return api.MoveActor{}, notPerson()
	}
	if err := ownsEveryTeamProject(ctx, d.Store.Registry(), sub.Identity.ID, d.Log); err != nil {
		return api.MoveActor{}, err
	}
	return api.MoveActor{Identity: sub.Identity.Name, Machine: sub.Machine, At: now}, nil
}

// parkedPress is pressedBy on a parked daemon. It authenticates no one: a
// call on its socket is this machine's owner's, a token there narrows, as
// on any socket, and a session names itself in its Zm-As. Who that owner
// is in this home, and that they own every team project it holds, is read
// from a private copy of the home's registry.db (parkedOwner).
func parkedPress(r *http.Request, d *core.Deps, person bool, now time.Time) (api.MoveActor, error) {
	if !core.IsLocal(r.Context()) || r.Header.Get(api.HeaderAuthorization) != "" {
		return api.MoveActor{}, notOwner()
	}
	if as := strings.TrimSpace(api.GetHeader(r.Header, api.HeaderAs)); person && as != "" && as != d.Config.HumanAddress() {
		return api.MoveActor{}, notPerson()
	}
	return parkedOwner(r.Context(), d, now)
}

// parkedOwner is who presses on this parked home: an identity of its
// registry.db (a private copy, store.OpenCopy: the home is never written)
// that owns every team project the registry holds (ownsEveryTeamProject).
//
// A standby's registry.db is its restored copy of HQ's, so an owner HQ
// added after the newest set it restored counts for nothing here, and its
// local identity is HQ's owner, not this machine's. The owner here is the
// one identity whose live node-bound tokens (HQ's rows: unrevoked,
// unexpired, no box's, never limited) the standby home holds the secrets
// of, in its remotes directory (heldTokens): a token proves the identity,
// a machine name does not. Tokens of two identities, or bound to two
// nodes, refuse; so does a home that holds none. Any other parked home
// (frozen, fenced) served once, and its owner is the identity its registry
// marks local and names as this machine's human.
//
// What this check promises: a press is pressed in a registry's name only
// as an identity that owns every team project there. It cannot stop the
// machine's own administrator, who can edit the home's files (registry.db,
// the token files) and the daemon itself; and who pressed, as the engine
// records it, is that owner's identity on this machine's word. A registry,
// term or token file that cannot be read refuses.
func parkedOwner(ctx context.Context, d *core.Deps, now time.Time) (api.MoveActor, error) {
	t, err := hometerm.Load(d.Config.Home)
	if err != nil {
		d.Log.Warn("HQ: this parked home's term cannot be read to check who pressed", "err", err)
		return api.MoveActor{}, unreadable()
	}
	db, done, err := store.OpenCopy(ctx, d.Config.RegistryPath())
	if err != nil {
		d.Log.Warn("HQ: this parked home's registry cannot be opened to check who pressed", "err", err)
		return api.MoveActor{}, unreadable()
	}
	defer done()
	by := api.MoveActor{Machine: d.Config.Machine, At: now}
	var id api.Identity
	if t.State == hometerm.Standby {
		id, by.Machine, err = standbyOwner(ctx, db, d)
	} else {
		id, err = localOwner(ctx, db, d)
	}
	if err != nil {
		return api.MoveActor{}, err
	}
	if err := ownsEveryTeamProject(ctx, db, id.ID, d.Log); err != nil {
		return api.MoveActor{}, err
	}
	by.Identity = id.Name
	return by, nil
}

// standbyOwner is the identity whose live node-bound tokens this standby
// home holds (parkedOwner), and the machine HQ bound them to.
func standbyOwner(ctx context.Context, db *sql.DB, d *core.Deps) (api.Identity, string, error) {
	held, err := heldTokens(ctx, d.Config.Home)
	if err != nil {
		d.Log.Warn("HQ: the tokens this standby holds cannot be read to check who pressed", "err", err)
		return api.Identity{}, "", unreadable()
	}
	rows, err := project.HeldTokensIn(ctx, db, held)
	if err != nil {
		d.Log.Warn("HQ: this parked home's registry cannot be read to check who pressed", "err", err)
		return api.Identity{}, "", unreadable()
	}
	if len(rows) == 0 {
		return api.Identity{}, "", api.RefuseHQ(api.CodeForbidden, api.ReasonNotOwner, "this standby holds no live token HQ issued a member machine, so who presses here cannot be told").
			WithHint("an owner of every team project presses it on HQ, or on a standby that holds their machine's tokens")
	}
	for _, h := range rows[1:] {
		if h.Identity.ID != rows[0].Identity.ID {
			return api.Identity{}, "", api.RefuseHQ(api.CodeForbidden, api.ReasonNotOwner, "this standby holds the tokens of more than one identity, so who presses here cannot be told").
				WithHint("an owner of every team project presses it on HQ, or on a standby that holds only their tokens")
		}
		if h.Node != rows[0].Node {
			return api.Identity{}, "", api.RefuseHQ(api.CodeForbidden, api.ReasonNotOwner, "this standby holds tokens of more than one machine, so who presses here cannot be told").
				WithHint("an owner of every team project presses it on HQ, or on a standby that holds only their machine's tokens")
		}
	}
	return rows[0].Identity, rows[0].Machine, nil
}

// localOwner is a frozen or fenced home's owner (parkedOwner): the
// identity its registry marks local, named as this machine's human.
func localOwner(ctx context.Context, db *sql.DB, d *core.Deps) (api.Identity, error) {
	var id api.Identity
	switch err := db.QueryRowContext(ctx, `SELECT id, name FROM identities WHERE local = 1 AND name = ?`, d.Config.Human).Scan(&id.ID, &id.Name); {
	case errors.Is(err, sql.ErrNoRows):
		return api.Identity{}, notOwner()
	case err != nil:
		d.Log.Warn("HQ: this parked home's registry cannot be read to check who pressed", "err", err)
		return api.Identity{}, unreadable()
	}
	return id, nil
}

// heldTokens are the token secrets home keeps in its remotes directory
// (DirRemotes, one <project>.token each, as a member home keeps the tokens
// it joined with): none when there is no such directory.
func heldTokens(ctx context.Context, home string) ([]string, error) {
	files, err := filepath.Glob(filepath.Join(home, DirRemotes, "*.token"))
	if err != nil {
		return nil, err
	}
	var out []string
	for _, f := range files {
		v, err := secrets.Get(ctx, secrets.File(f))
		if err != nil {
			return nil, err
		}
		out = append(out, strings.TrimSpace(v.Reveal()))
	}
	return out, nil
}

// nodeBound reports whether sub came with a member machine's node-bound
// token: never a box's, never a limited one.
func nodeBound(sub *authz.Subject) bool {
	return sub.Via == authz.ViaToken && sub.Remote && sub.Node != "" && sub.BoxKey == "" && sub.Limit() == ""
}

// memberCaller reports whether r's caller may read HQ's status: this
// machine's owner on its socket, or a member machine's node-bound token.
func memberCaller(r *http.Request) bool {
	sub := core.SubjectOf(r.Context())
	if sub == nil {
		return false
	}
	if core.IsLocal(r.Context()) {
		return !sub.Remote && sub.Via == authz.ViaLocal && sub.TokenID == ""
	}
	return nodeBound(sub)
}

// personal reports whether sub is a person's own call: no session, and a
// human's address.
func personal(sub *authz.Subject) bool {
	if sub.Session != "" {
		return false
	}
	a, err := api.ParseAddress(sub.Address)
	return err == nil && a.Kind == api.KindHuman
}

func notOwner() *api.Error {
	return api.RefuseHQ(api.CodeForbidden, api.ReasonNotOwner, "only an owner of every team project this home holds moves HQ").
		WithHint("press it on the HQ machine's own socket, or from an owner's machine that joined its projects")
}

func notPerson() *api.Error {
	return api.RefuseHQ(api.CodeForbidden, api.ReasonNotOwner, "this is an owner's own press, never a session's").
		WithHint("an owner presses it themselves; a session may abort or resume a stopped move")
}

// unreadable refuses a press whose caller cannot be checked: who owns this
// home's projects cannot be read now.
func unreadable() *api.Error {
	return api.RefuseHQ(api.CodeForbidden, api.ReasonNotOwner, "who owns this home's projects cannot be read on this machine now, so no one may press here").
		WithHint("this machine's log says why")
}

// ownsEveryTeamProject is nil when identity is owner of every team project
// db (a registry.db: this serving daemon's, or a parked home's opened read
// only) holds, by its own grants there.
func ownsEveryTeamProject(ctx context.Context, db *sql.DB, identity string, log *slog.Logger) error {
	team, err := project.TeamProjectsIn(ctx, db)
	if err != nil {
		log.Warn("HQ: the team projects cannot be read to check who pressed", "err", err)
		return unreadable()
	}
	if len(team) == 0 {
		return notOwner()
	}
	tuples, err := project.TuplesIn(ctx, db, identity)
	if err != nil {
		log.Warn("HQ: the caller's grants cannot be read", "err", err)
		return unreadable()
	}
	own := authz.FromTuples(tuples, nil)
	var short []api.ProjectRef
	for _, p := range team {
		if own.Relation(authz.Object{Project: p.ID}) != authz.Owner {
			short = append(short, p)
		}
	}
	if len(short) == 0 {
		return nil
	}
	pids := make([]string, len(short))
	for i, p := range short {
		pids[i] = p.ID
	}
	members, err := project.MembersIn(ctx, db, pids)
	if err != nil {
		log.Warn("HQ: the projects' owners cannot be read", "err", err)
		return unreadable()
	}
	owners := map[string][]string{}
	for _, m := range members {
		if m.Area == "" && m.Relation == authz.Owner {
			owners[m.Project] = append(owners[m.Project], m.Identity.Name)
		}
	}
	var named []string
	hidden := 0
	for _, p := range short {
		if !own.SeesProject(p.ID) {
			hidden++ // a project the caller cannot see is counted, never named
			continue
		}
		os := owners[p.ID]
		slices.Sort(os)
		named = append(named, fmt.Sprintf("%s (owners: %s)", p.Slug, cmp.Or(strings.Join(os, ", "), "none")))
	}
	if hidden > 0 {
		named = append(named, fmt.Sprintf("%d more you are not a member of", hidden))
	}
	return api.RefuseHQ(api.CodeForbidden, api.ReasonNotOwner,
		"only an owner of every team project this home holds moves HQ; you do not own %s", strings.Join(named, "; ")).
		WithHint("an owner of each makes you an owner there, or presses it themselves")
}
