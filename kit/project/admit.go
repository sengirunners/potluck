package project

// Admission: what a join gives a member (SPEC 17.4, and the GitHub join of
// SPEC 22): the identity (if new), the grant (never a demotion), the node's
// machine name and a node-bound token, in the caller's transaction. Invite
// redemption and the owners brick's GitHub join share it.

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/ids"
	"github.com/sengirunners/potluck/kit/store"
)

// admission is one member admitted from one tailnet node.
type admission struct {
	node, machine string // the caller's stable node ID; its machine name here
	identity      string // the identity's name (created if missing, kind human)
	project, area string
	role          string
	class         string // the token's operator class (SPEC 8): the invite's, never the caller's; "" for the role's default
	by, note      string // the grant's created_by; the token's note
	invite        bool   // an invite's redemption: an owner's act, so its raise moves the grant's ceiling (setCeiling)
}

// admitTx admits a in tx: it answers the identity's id and the new token's
// id and secret.
func (r *Registry) admitTx(ctx context.Context, tx *sql.Tx, now time.Time, a admission) (who, tid, secret string, err error) {
	machine := a.machine
	note := a.note
	if note == "" {
		note = "joined from " + machine
	}
	// The authority's own machine name is never a replica's: its token
	// could then answer as every session on this machine (Zm-As
	// mini/<s8>) and as its human.
	if machine == r.cfg.Machine {
		return "", "", "", api.Errorf(api.CodeConflict, "machine name %s is taken at this authority by another node", machine).
			WithHint("join with --machine <another name>")
	}
	// One machine name per node, one node per machine name. A node that
	// holds no live node-bound token any more (it left, or its token was
	// revoked) no longer holds its name: a reimaged box (a new node ID)
	// rejoins under it, and the old node may rejoin under another.
	var held, heldBy string
	switch err := tx.QueryRowContext(ctx, `SELECT machine FROM team_nodes WHERE node = ?`, a.node).Scan(&held); {
	case err == nil && held != machine:
		live, err := nodeLive(ctx, tx, a.node)
		if err != nil {
			return "", "", "", err
		}
		if live {
			return "", "", "", api.Errorf(api.CodeConflict, "this node already joined as machine %s", held).WithHint("join with --machine " + held)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM team_nodes WHERE node = ?`, a.node); err != nil {
			return "", "", "", err
		}
	case err != nil && !errors.Is(err, sql.ErrNoRows):
		return "", "", "", err
	}
	switch err := tx.QueryRowContext(ctx, `SELECT node FROM team_nodes WHERE machine = ?`, machine).Scan(&heldBy); {
	case err == nil && heldBy != a.node:
		live, err := nodeLive(ctx, tx, heldBy)
		if err != nil {
			return "", "", "", err
		}
		if live {
			return "", "", "", api.Errorf(api.CodeConflict, "machine name %s is taken at this authority by another node", machine).
				WithHint("join with --machine <another name>")
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM team_nodes WHERE node = ?`, heldBy); err != nil {
			return "", "", "", err
		}
	case err != nil && !errors.Is(err, sql.ErrNoRows):
		return "", "", "", err
	}
	if err := heldAsGuest(ctx, tx, a.identity, authz.Relation(a.role)); err != nil {
		return "", "", "", err
	}
	who, err = ensureIdentityTx(ctx, tx, a.identity, now)
	if err != nil {
		return "", "", "", err
	}
	obj := authz.AreaObject(a.project, a.area)
	var prev string
	switch err := tx.QueryRowContext(ctx, `SELECT relation FROM grants WHERE subject = ? AND object = ?`, who, obj).Scan(&prev); {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return "", "", "", err
	}
	if authz.Max(authz.Relation(prev), authz.Relation(a.role)) != authz.Relation(prev) {
		// A stronger grant already there is kept (an invite never demotes).
		// An invite's raise is an owner's act and moves the ceiling; a
		// GitHub join's role is already capped by it (ownerCap).
		// A raise ends an inviter flag (an owner invites anyway), and with
		// it the invites the flag alone allowed.
		if _, err := tx.ExecContext(ctx, `INSERT INTO grants (subject, relation, object, project, area, created, created_by)
			VALUES (?, ?, ?, ?, ?, ?, ?) ON CONFLICT (subject, object) DO UPDATE SET relation = excluded.relation, inviter = 0`,
			who, a.role, obj, a.project, a.area, store.Millis(now), a.by); err != nil {
			return "", "", "", err
		}
		if err := endInviterInvites(ctx, tx, who, a.project); err != nil {
			return "", "", "", err
		}
		if a.invite {
			if err := setCeiling(ctx, tx, who, a.project, a.area, authz.Relation(prev), authz.Relation(a.role), a.by, now); err != nil {
				return "", "", "", err
			}
		}
		if err := bumpScope(ctx, tx, who); err != nil {
			return "", "", "", err
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO team_nodes (node, machine, identity, created) VALUES (?, ?, ?, ?)
		ON CONFLICT (node) DO NOTHING`, a.node, machine, who, store.Millis(now)); err != nil {
		if store.IsUnique(err) {
			return "", "", "", api.Errorf(api.CodeConflict, "machine name %s is taken at this authority by another node", machine)
		}
		return "", "", "", err
	}
	secret = authz.NewTokenSecret()
	tid = ids.New()
	if _, err := tx.ExecContext(ctx, `INSERT INTO tokens (id, hash, identity, project, area, role, note, created, node, machine, class)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, tid, authz.HashToken(secret), who, a.project, a.area, a.role,
		note, store.Millis(now), a.node, machine, a.class); err != nil {
		return "", "", "", err
	}
	return who, tid, secret, nil
}

// AdmitInput is a member admitted without an invite: the owners brick's
// GitHub join (SPEC 22), after GitHub proved who the person is and a
// GitHub grant gave the role.
type AdmitInput struct {
	Node     string // the caller's stable tailnet node ID
	Machine  string // its machine name at this authority
	Identity string // the identity's name (created if missing)
	Project  string // ULID
	Area     string
	Role     string
	By, Note string
	// Account is the GitHub account joining ('' for none): the ceilings of
	// every identity it was before an unlink hold this join too (ownerCap).
	Account string
	// MustCreate refuses (conflict) an Identity name that already exists:
	// the GitHub join creates the identity it links, and a name match
	// must never link (owners-2026-11-01 Q2).
	MustCreate bool
	// Within runs inside the admission's transaction with the identity's
	// id (the owners brick records its link there, so an identity is never
	// created without it). Its error rolls everything back.
	Within func(ctx context.Context, tx *sql.Tx, identityID string) error
}

// Admitted is Admit's answer: redemption's, and whether the node is new to
// the identity in this project (it held no live token there), which is when
// the owners are told.
type Admitted struct {
	api.RedeemResponse
	NewNode bool
	TokenID string // the new token (the owners brick revokes it when its GitHub grant goes)
}

// Admit admits in like a redeemed invite: a team project only, the same
// machine-name rules, a grant that never demotes and never rises over an
// owner's lowering or removal (ownerCap: refused when nothing is left), a
// node-bound token. A
// live node-bound token the identity already holds from this node in this
// project is revoked: joining again replaces it, keeping the stronger of
// in's role and what the old token counts for now (tokenStanding): a GitHub
// join never downgrades a node that holds owner, and never restores a role
// lowered since; the cap's area is then the stronger token's.
func (r *Registry) Admit(ctx context.Context, in AdmitInput) (Admitted, error) {
	if in.Node == "" {
		return Admitted{}, api.Errorf(api.CodeUnauthenticated, "unknown tailnet node")
	}
	if !api.ValidName(in.Machine) || in.Machine != strings.ToLower(in.Machine) {
		return Admitted{}, api.Errorf(api.CodeBadRequest, "bad machine name %q", in.Machine)
	}
	if _, err := authz.ParseRelation(in.Role); err != nil {
		return Admitted{}, err
	}
	slug := r.Slug(in.Project)
	if slug == "" || !r.IsTeam(in.Project) {
		return Admitted{}, api.NotFound("project", in.Project)
	}
	if _, err := r.identity(ctx, in.Identity); api.IsCode(err, api.CodeNotFound) {
		r.mu.RLock()
		guard := r.guard
		r.mu.RUnlock()
		if guard != nil {
			if err := guard(ctx, in.Identity); err != nil {
				return Admitted{}, err
			}
		}
	}
	var out Admitted
	out.RedeemResponse = api.RedeemResponse{Cap: api.TeamCap{Project: in.Project, Slug: slug, Area: in.Area, Role: in.Role}, Machine: in.Machine,
		Identity: in.Identity, Authority: r.cfg.Machine}
	now := time.Now().UTC().Truncate(time.Millisecond)
	err := store.Tx(ctx, r.st.Registry(), func(tx *sql.Tx) error {
		var existed int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM identities WHERE name = ?`, in.Identity).Scan(&existed); err != nil {
			return err
		}
		if in.MustCreate && existed > 0 {
			return errIdentityTaken
		}
		if err := r.freshJoin(ctx, tx, in.Project, in.Machine, in.Node, now); err != nil {
			return err
		}
		// in's role is capped by an owner's lowering or removal (ownerCap);
		// an old token's standing (below) is already bounded by its own grant.
		role, cappedBy, err := ownerCap(ctx, tx, in.Identity, in.Account, in.Project, in.Area, authz.Relation(in.Role))
		if err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, `SELECT t.role, t.area, t.class, `+heldGrant+`
			FROM tokens t WHERE t.revoked IS NULL AND (t.expires IS NULL OR t.expires > ?) AND t.node = ? AND t.project = ?
			AND t.identity = (SELECT id FROM identities WHERE name = ?)`, store.Millis(now), in.Node, in.Project, in.Identity)
		if err != nil {
			return err
		}
		// A replaced token carries over what it counts for now
		// (tokenStanding, as MachineJoin reads it): a member since lowered
		// to guest does not get member back by joining again. Its operator
		// class (an invite's, SPEC 8) carries over, the stricter winning:
		// joining again never loosens what the owner chose for this machine
		// (a public invite's machine does not become trusted by a stronger
		// role), and a trusted class whose grant was revoked or lowered does
		// not come back.
		area, class, prior := in.Area, "", 0
		for rows.Next() {
			var pr, pa, pc, held string
			if err := rows.Scan(&pr, &pa, &pc, &held); err != nil {
				rows.Close()
				return err
			}
			prior++
			counts, own := tokenStanding(pr, held)
			if authz.Max(role, counts) != role {
				role, area = counts, pa
			}
			if pc == api.MachinePublic || (own && class == "") {
				class = pc
			}
		}
		rows.Close()
		if role == authz.None {
			return errOwnerRemoved(cappedBy, slug)
		}
		out.NewNode = prior == 0
		out.Cap.Role, out.Cap.Area = string(role), area
		if _, err := tx.ExecContext(ctx, `UPDATE tokens SET revoked = ? WHERE revoked IS NULL AND node = ? AND project = ?
			AND identity = (SELECT id FROM identities WHERE name = ?)`, store.Millis(now), in.Node, in.Project, in.Identity); err != nil {
			return err
		}
		who, tid, secret, err := r.admitTx(ctx, tx, now, admission{node: in.Node, machine: in.Machine, identity: in.Identity,
			project: in.Project, area: area, role: string(role), class: class, by: in.By, note: in.Note})
		if err != nil {
			return err
		}
		if in.Within != nil {
			if err := in.Within(ctx, tx, who); err != nil {
				return err
			}
		}
		out.Token, out.TokenID = secret, tid
		return tx.QueryRowContext(ctx, `SELECT scope_version FROM identities WHERE id = ?`, who).Scan(&out.ScopeVersion)
	})
	r.forgetMachines()
	if err != nil {
		return Admitted{}, err
	}
	return out, nil
}

// freshJoin runs the OnFreshJoin hook when node holds no live join of machine
// to project (liveJoinWhere). Admit and Redeem call it before they replace
// any token, so every way a node joins goes through it.
func (r *Registry) freshJoin(ctx context.Context, tx *sql.Tx, project, machine, node string, now time.Time) error {
	r.mu.RLock()
	fresh := r.fresh
	r.mu.RUnlock()
	if fresh == nil {
		return nil
	}
	var live int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM tokens t WHERE `+liveJoinWhere+` AND t.node = ?`,
		machine, project, store.Millis(now), node).Scan(&live); err != nil {
		return err
	}
	if live > 0 {
		return nil
	}
	return fresh(ctx, tx, project, machine)
}

// errIdentityTaken is Admit's answer to MustCreate for a name that exists.
var errIdentityTaken = api.Errorf(api.CodeConflict, "that identity name is taken")

// IsIdentityTaken reports Admit's MustCreate refusal (pick another name).
func IsIdentityTaken(err error) bool { return errors.Is(err, errIdentityTaken) }

// RevokeAdmitted removes the grant identityID holds on project/area when
// by made it (an admission's source: the owners brick revoking a GitHub
// grant); a grant from anywhere else stays, and so does an owner's
// ceiling (setCeiling), which another GitHub grant's join still meets. It
// reports whether one went.
func (r *Registry) RevokeAdmitted(ctx context.Context, identityID, project, area, by string) (bool, error) {
	gone := false
	err := store.Tx(ctx, r.st.Registry(), func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM grants WHERE subject = ? AND object = ? AND created_by = ?`,
			identityID, authz.AreaObject(project, area), by)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			gone = true
			if err := endInviterInvites(ctx, tx, identityID, project); err != nil {
				return err
			}
			return bumpScope(ctx, tx, identityID)
		}
		return nil
	})
	return gone, err
}
