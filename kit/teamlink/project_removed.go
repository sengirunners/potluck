package teamlink

import (
	"context"
	"database/sql"
	projectpkg "github.com/sengirunners/potluck/kit/project"
)

// dropRemoved deletes what the authority keeps in registry.db for a pool its
// owner removes (Registry.OnRemove): the machine classes the owner set, the
// machine ops still queued and the task references. A replica's own
// records of the pool (team_remotes) are not here: they belong to the
// machines that joined, which end theirs when the authority says it is gone
// (Observe).
func dropRemoved(ctx context.Context, tx *sql.Tx, project string, _ projectpkg.RemoveOptions) error {
	for _, q := range []string{
		`DELETE FROM team_machine_class WHERE project = ?`,
		`DELETE FROM team_ops WHERE project = ?`,
		`DELETE FROM team_ops_run WHERE project = ?`,
		`DELETE FROM team_task_refs WHERE project = ?`,
	} {
		if _, err := tx.ExecContext(ctx, q, project); err != nil {
			return err
		}
	}
	return nil
}
