package project

import (
	"context"
	"database/sql"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
)

// SubscriptionSubject reconstructs a human subscription's current grants
// and immutable token cap (never a replica's or a box's token). Expiry ends page sign-in, not an opted-in push;
// explicit token revocation or identity removal ends both.
func (r *Registry) SubscriptionSubject(ctx context.Context, tokenID string) (*authz.Subject, error) {
	var identityID, pid, area, role, node, boxKey string
	var revoked sql.NullInt64
	err := r.st.Registry().QueryRowContext(ctx, `SELECT identity,project,area,role,node,revoked,box_key FROM tokens WHERE id=?`, tokenID).Scan(&identityID, &pid, &area, &role, &node, &revoked, &boxKey)
	// A box's token is no subscriber: what it holds would be read later
	// with no key to sign for it.
	if err != nil || revoked.Valid || node != "" || boxKey != "" {
		return nil, api.Errorf(api.CodeUnauthenticated, "invalid subscription")
	}
	who, _, err := r.identityVersion(ctx, identityID)
	if err != nil || who.Kind != api.IdentityHuman {
		return nil, api.Errorf(api.CodeUnauthenticated, "invalid subscription")
	}
	tuples, err := r.Tuples(ctx, who.ID)
	if err != nil {
		return nil, err
	}
	cp := &authz.Cap{Project: pid, Area: area, Role: authz.Relation(role)}
	sc := authz.FromTuples(tuples, cp)
	if who.ID == r.local.ID && pid == "" && area == "" && cp.Role == authz.Owner {
		sc = sc.WithMachine()
	}
	return &authz.Subject{Identity: who, Address: identityAddress(who), Via: authz.ViaSystem, TokenID: tokenID, Scope: sc, Cap: cp}, nil
}
