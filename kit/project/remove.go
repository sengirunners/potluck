package project

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"time"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/store"
)

// Removing a project: the registry's rows for it, its folder (project.db
// and project.toml) and every token it held. A joined machine's token ends
// with the pool_removed mark, so its next call is refused with the pool's
// name (FromTokenCaller) and its replica says why it ended, not only that
// the token was refused.
func init() {
	store.RegisterRegistry("project/0010_token_pool_removed", `
ALTER TABLE tokens ADD COLUMN pool_removed TEXT NOT NULL DEFAULT '';`)
}

// PoolRemovedPrefix starts the message of the refusal a joined machine gets
// when its pool was removed (the pool's slug follows): teamlink matches it.
const PoolRemovedPrefix = "pool removed by its owner: "

// RemoveOptions are what the caller of Remove chose.
type RemoveOptions struct {
	// Force removes the project although work still runs in it (a hook that
	// refuses for that reason lets it through): the work goes on until its
	// lease ends, out of its members' reach.
	Force bool
}

// RemoveHook runs inside Remove's transaction before the project's own rows
// go: a module deletes what it keeps in registry.db for project (a ULID), or
// refuses the removal with an error that says what to end first.
type RemoveHook func(ctx context.Context, tx *sql.Tx, project string, opts RemoveOptions) error

// OnRemove adds a hook to every later Remove. Set before serving.
func (r *Registry) OnRemove(f RemoveHook) {
	r.mu.Lock()
	r.removers = append(r.removers, f)
	r.mu.Unlock()
}

// RemovedHook runs after the registry's transaction committed, for what a
// module keeps outside registry.db (a file, a project's own database). It
// cannot refuse: the project is gone. An error it returns is reported as a
// leftover of the removal, with the hook's own words for what is still there.
type RemovedHook func(ctx context.Context, project api.ProjectRef) error

// OnRemoved adds a hook to every later Remove, run after it committed. Set
// before serving.
func (r *Registry) OnRemoved(f RemovedHook) {
	r.mu.Lock()
	r.removedHooks = append(r.removedHooks, f)
	r.mu.Unlock()
}

// Remove deletes project ref from this machine: its grants, invites and
// ceilings, its tokens (ended, with the project's name), what the modules
// kept for it (OnRemove) and its folder. sub must be an admin of it. All the
// registry's rows go in one transaction, so a refusal or a failure leaves the
// project as it was. Once that committed the project is gone: a step after
// it (OnRemoved, the folder) that fails is listed in the answer's Leftovers,
// not returned as an error, so a caller never reports failure for a removal
// that happened.
func (r *Registry) Remove(ctx context.Context, sub *authz.Subject, ref string, opts RemoveOptions) (api.RemoveProjectResponse, error) {
	// One removal at a time. r.mu is not held while the hooks run: they ask
	// the registry (Lookup) like any module.
	r.removeMu.Lock()
	defer r.removeMu.Unlock()
	r.mu.RLock()
	id := r.idOf(ref)
	var slug string
	if id != "" {
		slug = r.byID[id].file.Slug
	}
	hooks := slices.Clone(r.removers)
	after := slices.Clone(r.removedHooks)
	r.mu.RUnlock()
	if id == "" {
		return api.RemoveProjectResponse{}, api.NotFound("project", ref)
	}
	if err := sub.Check(authz.Admin, authz.Object{Project: id}, "project", ref); err != nil {
		return api.RemoveProjectResponse{}, err
	}
	var machines []string
	err := store.Tx(ctx, r.st.Registry(), func(tx *sql.Tx) error {
		for _, h := range hooks {
			if err := h(ctx, tx, id, opts); err != nil {
				return err
			}
		}
		rows, err := tx.QueryContext(ctx, `SELECT DISTINCT machine FROM tokens WHERE project = ? AND node != '' AND machine != '' AND revoked IS NULL
			AND (expires IS NULL OR expires > ?) ORDER BY machine`, id, store.Millis(time.Now()))
		if err != nil {
			return err
		}
		for rows.Next() {
			var m string
			if err := rows.Scan(&m); err != nil {
				rows.Close()
				return err
			}
			machines = append(machines, m)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE tokens SET revoked = ?, pool_removed = ? WHERE project = ? AND revoked IS NULL`,
			store.Millis(time.Now()), slug, id); err != nil {
			return err
		}
		for _, q := range []string{
			`DELETE FROM invites WHERE project = ?`,
			`DELETE FROM grant_ceilings WHERE project = ?`,
			`DELETE FROM grants WHERE project = ?`,
			`DELETE FROM projects WHERE id = ?`,
		} {
			if _, err := tx.ExecContext(ctx, q, id); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return api.RemoveProjectResponse{}, err
	}
	r.mu.Lock()
	delete(r.byID, id)
	delete(r.bySlug, slug)
	r.mu.Unlock()
	r.forgetMachines()
	// The registry no longer knows the project, so nothing below can undo
	// the removal: each failure is a leftover the owner can finish by hand.
	// A folder that stays holds files nothing reads (no grant, token or row).
	removed := api.ProjectRef{ID: id, Slug: slug}
	leftovers := []string{}
	for _, h := range after {
		if err := h(ctx, removed); err != nil {
			leftovers = append(leftovers, err.Error())
		}
	}
	if err := r.st.DropProject(id); err != nil {
		leftovers = append(leftovers, fmt.Sprintf("the project's folder %s could not be removed: %v", r.st.ProjectDir(id), err))
	}
	return api.RemoveProjectResponse{Project: removed, Machines: append([]string{}, machines...), Leftovers: leftovers}, nil
}
