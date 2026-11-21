package runners

import (
	"context"
	"database/sql"
	"fmt"
	projectpkg "github.com/sengirunners/potluck/kit/project"
	"strings"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/config"
)

// refuseRemove stops the removal of a pool (Registry.OnRemove) that
// GitHub Actions still runs on: the broker's pool in config.json, or a
// GitHub account linked to it. Each is the owner's to end first, and each
// refusal says how.
func (s *Service) refuseRemove(ctx context.Context, tx *sql.Tx, project string, _ projectpkg.RemoveOptions) error {
	if p, ok := s.d.Projects.Lookup(s.conf().Pool.Project); ok && p.ID == project {
		return api.Errorf(api.CodeConflict, "pool %s is the pool GitHub Actions jobs run on (runners.pool.project in config.json)", p.Slug).
			WithHint("take the runners block out of config.json and restart the daemon, then remove the pool")
	}
	rows, err := tx.QueryContext(ctx, `SELECT installation, account FROM runners_links WHERE project = ? ORDER BY installation`, project)
	if err != nil {
		return err
	}
	defer rows.Close()
	var links []string
	for rows.Next() {
		var id int64
		var account string
		if err := rows.Scan(&id, &account); err != nil {
			return err
		}
		links = append(links, fmt.Sprintf("%s (installation %d)", account, id))
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(links) > 0 {
		return api.Errorf(api.CodeConflict, "GitHub accounts are linked to this pool for Actions jobs: %s", strings.Join(links, ", ")).
			WithHint("unlink each with " + config.PotluckCommand("runners unlink <installation>") + ", then remove the pool")
	}
	return nil
}
