package core

import (
	"context"

	"github.com/sengirunners/potluck/kit/authz"
)

// AreaRelation is sub's relation to area of project ("" = the whole
// project), judged as ProjectAdmin judges admin: a member machine's
// node-bound token (capped at member) by its identity's current grants here
// (none once they are gone), and only for the project its cap names;
// everyone else by its scope, so a minted token keeps its cap.
func (d *Deps) AreaRelation(ctx context.Context, sub *authz.Subject, project, area string) (authz.Relation, error) {
	o := authz.Object{Project: project, Area: area}
	switch {
	case sub == nil || project == "":
		return authz.None, nil
	case sub.Via == authz.ViaToken && sub.Node != "" && sub.Machine != "" && sub.Identity.ID != "" && d.Projects != nil &&
		(sub.Cap == nil || sub.Cap.Project == "" || sub.Cap.Project == project):
		tuples, err := d.Projects.Tuples(ctx, sub.Identity.ID)
		if err != nil {
			return authz.None, err
		}
		return authz.FromTuples(tuples, nil).Relation(o), nil
	}
	return sub.Scope.Relation(o), nil
}
