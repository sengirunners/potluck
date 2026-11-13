package project

import (
	"context"
	"database/sql"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
)

// SubscriptionSubject reconstructs a human subscription's current grants
// and immutable token cap. Expiry ends page sign-in, not an opted-in push;
// explicit token revocation or identity removal ends both.
func (r *Registry) SubscriptionSubject(ctx context.Context, tokenID string) (*authz.Subject, error) {
	var identityID, pid, area, role, node string
	var revoked sql.NullInt64
	err := r.st.Registry().QueryRowContext(ctx, `SELECT identity,project,area,role,node,revoked FROM tokens WHERE id=?`, tokenID).Scan(&identityID, &pid, &area, &role, &node, &revoked)
	if err != nil || revoked.Valid || node != "" {
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
