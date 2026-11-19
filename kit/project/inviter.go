package project

// Inviters: an owner of the whole project may let a member invite guests
// (zm-drtr). The permission is a flag on the member's grant row, so it
// covers that grant's object (the project, or one area) and goes with the
// row: revoking the grant, lowering it or granting the plain role again
// ends it. An inviter's invite is held here, where invites are made and
// redeemed, to the one shape it may take:
//
//   - role guest, never member or owner;
//   - into the object of the inviter's own grant (a project-wide inviter
//     may name any area): the guest never sees more than the inviter;
//   - no machine class and no tagged node: those are the owner's to give;
//   - a new identity only, at creation and again at redemption, so the
//     code never speaks as an existing person;
//   - redeemable only while its inviter still holds the permission;
//   - the identity it creates stays a guest: no invite, join or system
//     grant raises it, only an owner's own grant (guestRaise), since its
//     name was the inviter's choice and may be one an owner or the system
//     meant for someone else.
//
// An inviter changes no grant and no role; Grant and Revoke stay an
// admin's.

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/store"
)

func init() {
	// grants.inviter is the permission (it counts only on a member's row);
	// invites.inviter is the identity id of the inviter who made the
	// invite under it ('' for an admin's invite); identities.guest_of is
	// that inviter's id on the identity such an invite created, until an
	// owner raises it by hand (guestRaise).
	store.RegisterRegistry("project/0009_inviters", `
ALTER TABLE grants ADD COLUMN inviter INTEGER NOT NULL DEFAULT 0;
ALTER TABLE invites ADD COLUMN inviter TEXT NOT NULL DEFAULT '';
ALTER TABLE identities ADD COLUMN guest_of TEXT NOT NULL DEFAULT '';`)
}

// RoleInviter is the grant role that makes a member an inviter: a member
// who may also make guest invites. Granting member (or any other role)
// again on the same object takes the permission away.
const RoleInviter = "inviter"

// grantRole parses a grant's role: owner, member, guest, or inviter
// (member with the inviter flag).
func grantRole(s string) (rel authz.Relation, inviter bool, err error) {
	if strings.EqualFold(strings.TrimSpace(s), RoleInviter) {
		return authz.Member, true, nil
	}
	rel, err = authz.ParseRelation(s)
	if err != nil {
		return authz.None, false, api.Errorf(api.CodeBadRequest, "role must be owner, member, guest or inviter, not %q", s)
	}
	return rel, false, nil
}

// inviterNamed refuses an inviter grant for a name no one has (to a
// non-admin, as inviterGrantAllowed would): an inviter is a member already
// (Grant checks the role), and a refused grant leaves no identity behind.
func (r *Registry) inviterNamed(ctx context.Context, sub *authz.Subject, pid, identity string) error {
	if _, err := r.identity(ctx, identity); api.IsCode(err, api.CodeNotFound) {
		if err := inviterGrantAllowed(sub, pid, r.Slug(pid)); err != nil {
			return err
		}
		return api.Errorf(api.CodeConflict, "%s is no member of %s: an inviter is a member an owner lets invite guests", identity, r.Slug(pid)).
			WithHint("make them a member there first")
	} else if err != nil {
		return err
	}
	return nil
}

// inviterGrantAllowed refuses setting the inviter flag to anyone but an
// admin of the whole project: an area's admin cannot invite, so cannot
// hand that on. Keeping a flag already set (an area admin minting the
// inviter a member token) is not setting it.
func inviterGrantAllowed(sub *authz.Subject, pid, project string) error {
	if sub.Can(authz.Admin, authz.Object{Project: pid}) {
		return nil
	}
	return api.Errorf(api.CodeForbidden, "only an owner of the whole project %s makes a member an inviter", project)
}

// daemonNames are identity names, and name prefixes, the daemon and the
// programs built on it give their own identities (the System subject's
// "zm", the runner broker's "zm-runners"): no inviter's guest takes one.
var daemonNames = []string{"zm", "potluck"}

// daemonName reports whether name is, or starts as, one of daemonNames.
func daemonName(name string) bool {
	for _, d := range daemonNames {
		if name == d || strings.HasPrefix(name, d+"-") {
			return true
		}
	}
	return false
}

// inviteTarget is CreateInvite's permission check: the project and area
// the invite goes into, and the inviter's identity id when it is made
// under the inviter permission ("" for an admin's invite).
func (r *Registry) inviteTarget(ctx context.Context, sub *authz.Subject, in InviteInput, rel authz.Relation) (pid, area, inviter string, err error) {
	pid, area, err = r.grantTarget(sub, in.Project, in.Area)
	if err == nil {
		if rel != authz.Guest {
			if err := heldAsGuest(ctx, r.st.Registry(), strings.ToLower(strings.TrimSpace(in.As)), rel); err != nil {
				return "", "", "", err
			}
		}
		return pid, area, "", nil
	}
	// A forbidden answer means the caller sees the target but is no admin
	// of it (authz.Subject.Check): it may still be an inviter there.
	// Only the inviter's own uncapped standing counts: the local socket's
	// owner, or a member's node-bound token as the network door rebuilds it
	// (teamlink); a token with a cap, a box's, never.
	if !api.IsCode(err, api.CodeForbidden) || sub == nil || sub.Identity.ID == "" || sub.Cap != nil || sub.BoxKey != "" {
		return "", "", "", err
	}
	r.mu.RLock()
	pid = r.idOf(in.Project)
	r.mu.RUnlock()
	area = strings.ToLower(strings.TrimSpace(in.Area))
	ok, err := invitesGuests(ctx, r.st.Registry(), sub.Identity.ID, pid, area)
	if err != nil {
		return "", "", "", err
	}
	if !ok {
		return "", "", "", api.Errorf(api.CodeForbidden, "only the project's owner (or an admin), or an inviter, makes an invite").
			WithHint("an owner can let a member invite guests by granting them the role inviter")
	}
	slug := r.Slug(pid)
	switch {
	case rel != authz.Guest:
		return "", "", "", api.Errorf(api.CodeForbidden, "an inviter makes guest invites only, not %s invites", rel).
			WithHint("ask an owner of " + slug + " to invite them as a " + string(rel))
	case strings.TrimSpace(in.Class) != "":
		return "", "", "", api.Errorf(api.CodeForbidden, "an inviter's invite carries no machine class: that is an owner's to give")
	case in.AllowTagged:
		return "", "", "", api.Errorf(api.CodeForbidden, "an inviter's invite cannot admit a tagged node: that is an owner's to allow")
	}
	name := strings.ToLower(strings.TrimSpace(in.As))
	if daemonName(name) {
		return "", "", "", api.Errorf(api.CodeBadRequest, "%s is a name the daemon keeps for its own identities: an inviter's guest takes another", name)
	}
	if free, err := nameFree(ctx, r.st.Registry(), name, ""); err != nil {
		return "", "", "", err
	} else if !free {
		return "", "", "", api.Errorf(api.CodeConflict, "an inviter invites new people only, under a name nobody here has or is invited as").
			WithHint("pick another name, or ask an owner of " + slug + " to invite them")
	}
	return pid, area, sub.Identity.ID, nil
}

// nameFree reports whether name is no identity's and no other live
// (unredeemed, unexpired) invite's but invite except's: an inviter's
// guest never takes the name of a person, nor of one an owner invited.
func nameFree(ctx context.Context, q querier, name, except string) (bool, error) {
	var one int
	err := q.QueryRowContext(ctx, `SELECT 1 FROM identities WHERE name = ?
		UNION ALL SELECT 1 FROM invites WHERE identity = ? AND id != ? AND node = '' AND expires > ? LIMIT 1`,
		name, name, except, store.Millis(time.Now())).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return true, nil
	}
	return false, err
}

// querier is a database or a transaction.
type querier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// invitesGuests reports whether identity holds the inviter permission
// covering project pid's area ("" = the whole project): on a member grant
// on the project, or on that area.
func invitesGuests(ctx context.Context, q querier, identity, pid, area string) (bool, error) {
	if identity == "" || pid == "" {
		return false, nil
	}
	objs := []any{authz.ProjectObject(pid), authz.ProjectObject(pid)}
	if area != "" {
		objs[1] = authz.AreaObject(pid, area)
	}
	var one int
	err := q.QueryRowContext(ctx, `SELECT 1 FROM grants WHERE subject = ? AND relation = ? AND inviter = 1 AND object IN (?, ?) LIMIT 1`,
		append([]any{identity, string(authz.Member)}, objs...)...).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// InvitesAnywhere reports whether identity is an inviter on project pid or
// any of its areas: the network door lets such a member's token ask for an
// invite, which CreateInvite then judges.
func (r *Registry) InvitesAnywhere(ctx context.Context, identity, pid string) (bool, error) {
	var one int
	err := r.st.Registry().QueryRowContext(ctx, `SELECT 1 FROM grants WHERE subject = ? AND project = ? AND relation = ? AND inviter = 1 LIMIT 1`,
		identity, pid, string(authz.Member)).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// inviterRedeemable reports whether invite id, made under the inviter
// permission, may still be redeemed: its inviter still holds the
// permission on its object, and its name is still free (nameFree).
func (r *Registry) inviterRedeemable(ctx context.Context, tx *sql.Tx, id, inviter, pid, area, name string) (bool, error) {
	ok, err := invitesGuests(ctx, tx, inviter, pid, area)
	if err != nil || !ok {
		return false, err
	}
	return nameFree(ctx, tx, name, id)
}

// endInviterInvites removes identity's unredeemed invites in project pid
// that its inviter permission no longer covers, in the transaction that
// took the permission away (a role change, a revoke, a lowering): giving
// it back later does not bring them back, and they leave the live-invite
// cap at once.
func endInviterInvites(ctx context.Context, tx *sql.Tx, identity, pid string) error {
	rows, err := tx.QueryContext(ctx, `SELECT id, area FROM invites WHERE inviter = ? AND project = ? AND node = ''`, identity, pid)
	if err != nil {
		return err
	}
	type inv struct{ id, area string }
	var all []inv
	for rows.Next() {
		var i inv
		if err := rows.Scan(&i.id, &i.area); err != nil {
			rows.Close()
			return err
		}
		all = append(all, i)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, i := range all {
		ok, err := invitesGuests(ctx, tx, identity, pid, i.area)
		if err != nil {
			return err
		}
		if ok {
			continue
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM invites WHERE id = ? AND node = ''`, i.id); err != nil {
			return err
		}
	}
	return nil
}

// inviterInviteTx re-checks, in the transaction that stores it, an
// inviter's invite: the permission still covers its object and the name is
// still free, so no revoke or second invite slips in between.
func inviterInviteTx(ctx context.Context, tx *sql.Tx, inviter, pid, area, name string) error {
	if ok, err := invitesGuests(ctx, tx, inviter, pid, area); err != nil {
		return err
	} else if !ok {
		return api.Errorf(api.CodeForbidden, "only the project's owner (or an admin), or an inviter, makes an invite")
	}
	if free, err := nameFree(ctx, tx, name, ""); err != nil {
		return err
	} else if !free {
		return api.Errorf(api.CodeConflict, "an inviter invites new people only, under a name nobody here has or is invited as")
	}
	return nil
}

// markGuestOf records that identity was created by inviter's guest invite.
func markGuestOf(ctx context.Context, tx *sql.Tx, identity, inviter string) error {
	_, err := tx.ExecContext(ctx, `UPDATE identities SET guest_of = ? WHERE id = ?`, inviter, identity)
	return err
}

// guestOf is the inviter whose guest invite created identity name ("" for
// any other identity, or a name no one has).
func guestOf(ctx context.Context, q querier, name string) (string, error) {
	var by string
	err := q.QueryRowContext(ctx, `SELECT guest_of FROM identities WHERE name = ?`, name).Scan(&by)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return by, err
}

// heldAsGuest refuses rel above guest for name when an inviter's guest
// invite created it: an invite or a join never raises such a guest.
func heldAsGuest(ctx context.Context, q querier, name string, rel authz.Relation) error {
	if authz.Max(rel, authz.Guest) == authz.Guest {
		return nil
	}
	by, err := guestOf(ctx, q, name)
	if err != nil || by == "" {
		return err
	}
	return api.Errorf(api.CodeConflict, "%s joined as a guest an inviter invited: an invite or a join does not make them a %s", name, rel).
		WithHint("if they are the person you mean, an owner grants them the role first; else pick another name")
}

// guestRaise is Grant's rule for such a guest: an owner's own grant (a
// person's, not the daemon's) may raise it, and clears the mark, since the
// owner chose this identity; the daemon's own grants (a runner link's)
// never do.
func guestRaise(ctx context.Context, tx *sql.Tx, sub *authz.Subject, identity, name string, rel authz.Relation) error {
	if authz.Max(rel, authz.Guest) == authz.Guest {
		return nil
	}
	by, err := guestOf(ctx, tx, name)
	if err != nil || by == "" {
		return err
	}
	if sub == nil || sub.Via == authz.ViaSystem {
		return api.Errorf(api.CodeConflict, "%s is a guest an inviter invited, not the identity the daemon meant", name).
			WithHint("if no person of that name is yours, revoke their grants")
	}
	_, err = tx.ExecContext(ctx, `UPDATE identities SET guest_of = '' WHERE id = ?`, identity)
	return err
}

// mintRole is the role Mint grants for role on identity name's object: an
// inviter's member grant stays an inviter's (minting a member token for
// them is no revoke).
func (r *Registry) mintRole(ctx context.Context, name, project, area, role string) string {
	if !strings.EqualFold(strings.TrimSpace(role), string(authz.Member)) {
		return role
	}
	r.mu.RLock()
	pid := r.idOf(project)
	r.mu.RUnlock()
	var one int
	err := r.st.Registry().QueryRowContext(ctx, `SELECT 1 FROM grants g JOIN identities i ON i.id = g.subject
		WHERE (i.name = ? OR i.id = ?) AND g.object = ? AND g.relation = ? AND g.inviter = 1`,
		strings.ToLower(strings.TrimSpace(name)), strings.ToUpper(strings.TrimSpace(name)),
		authz.AreaObject(pid, strings.ToLower(strings.TrimSpace(area))), string(authz.Member)).Scan(&one)
	if err == nil {
		return RoleInviter
	}
	return role
}
