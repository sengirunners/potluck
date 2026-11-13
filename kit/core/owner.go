package core

import (
	"context"

	"github.com/sengirunners/potluck/kit/authz"
)

// ProjectAdmin reports whether sub's identity owns or administers project
// (whole, not one area), judged by its grants: the one check for an owner's
// rights that a member machine's link would otherwise lose (deciding asks,
// setting inbox aliases).
//
//   - By sub's scope: this machine's owner on its Unix socket (every process
//     of the OS user, sessions too), or a token or login whose scope reaches
//     admin there.
//   - A member machine's node-bound token (SPEC 17.4), by its identity's own
//     grants here. That token is capped at member, so its scope cannot say
//     this; the cap still bounds everything else the token does. Only the
//     node-bound token: a minted token keeps its cap (an agent's, a CI's).
func (d *Deps) ProjectAdmin(ctx context.Context, sub *authz.Subject, project string) (bool, error) {
	whole := authz.Object{Project: project}
	switch {
	case sub == nil || project == "":
		return false, nil
	case sub.Can(authz.Admin, whole):
		return true, nil
	case sub.Via == authz.ViaToken && sub.Node != "" && sub.Machine != "" && sub.Identity.ID != "" && d.Projects != nil:
		tuples, err := d.Projects.Tuples(ctx, sub.Identity.ID)
		if err != nil {
			return false, err
		}
		return authz.FromTuples(tuples, nil).Can(authz.Admin, whole), nil
	}
	return false, nil
}
