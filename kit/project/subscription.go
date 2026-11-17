package project

import (
	"context"
	"database/sql"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
)

// SubscriptionSubject reconstructs a human subscription's current grants
// and immutable token cap (never a replica's, a box's or a limited token). Expiry ends page sign-in, not an opted-in push;
// explicit token revocation or identity removal ends both.
func (r *Registry) SubscriptionSubject(ctx context.Context, tokenID string) (*authz.Subject, error) {
	var identityID, pid, area, role, node, boxKey, limit string
	var revoked sql.NullInt64
	err := r.st.Registry().QueryRowContext(ctx, `SELECT identity,project,area,role,node,revoked,box_key,route_limit FROM tokens WHERE id=?`, tokenID).Scan(&identityID, &pid, &area, &role, &node, &revoked, &boxKey, &limit)
	// A box's token is no subscriber: what it holds would be read later
	// with no key to sign for it. Nor is a limited token: a subscriber is
	// read with no route and no device to check.
	if err != nil || revoked.Valid || node != "" || boxKey != "" || limit != "" {
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
