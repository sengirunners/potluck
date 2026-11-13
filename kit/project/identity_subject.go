package project

import (
	"context"

	"github.com/sengirunners/potluck/kit/authz"
)

// IdentitySubject is identity id's own subject, by its own grants, as a
// mapped tailnet login gets it (FromTailscale).
func (r *Registry) IdentitySubject(ctx context.Context, id string) (*authz.Subject, error) {
	who, err := r.identity(ctx, id)
	if err != nil {
		return nil, err
	}
	tuples, err := r.Tuples(ctx, who.ID)
	if err != nil {
		return nil, err
	}
	return &authz.Subject{Identity: who, Address: identityAddress(who), Via: authz.ViaTailnet, Remote: true, Scope: authz.FromTuples(tuples, nil)}, nil
}
