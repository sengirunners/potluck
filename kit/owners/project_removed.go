package owners

import (
	"context"
	"database/sql"

	"github.com/sengirunners/potluck/kit/project"
)

// dropRemoved deletes what this module keeps for a project its owner removes
// (Registry.OnRemove): the GitHub grants, the grants still waiting for
// GitHub's rate limit, the admissions made from them and the org check
// state. Not here: owner_names and login_paths, which are append-only by
// rule (triggers refuse a delete): a @login/name bound to the removed
// project stays bound to it, so no other project takes that name.
func dropRemoved(ctx context.Context, tx *sql.Tx, id string, _ project.RemoveOptions) error {
	for _, q := range []string{
		`DELETE FROM github_grant_queue WHERE project = ?`,
		`DELETE FROM github_admissions WHERE project = ?`,
		`DELETE FROM github_org_checks WHERE project = ?`,
		`DELETE FROM github_grants WHERE project = ?`,
	} {
		if _, err := tx.ExecContext(ctx, q, id); err != nil {
			return err
		}
	}
	return nil
}
