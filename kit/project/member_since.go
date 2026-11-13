package project

import (
	"context"
	"database/sql"
	"time"

	"github.com/sengirunners/potluck/kit/store"
)

// MemberSince is when the oldest of identityID's current grants that reach
// area of project was made (area "": any grant in project; else one on the
// whole project or on that area); ok is false when it holds none. A grant's
// time survives a role change, so a later time means the identity left (the
// pool, or the area) and came back.
func (r *Registry) MemberSince(ctx context.Context, identityID, project, area string) (since time.Time, ok bool, err error) {
	q, args := `SELECT MIN(created) FROM grants WHERE subject = ? AND project = ?`, []any{identityID, project}
	if area != "" {
		q += ` AND (area = '' OR area = ?)`
		args = append(args, area)
	}
	var ms sql.NullInt64
	if err := r.st.Registry().QueryRowContext(ctx, q, args...).Scan(&ms); err != nil {
		return time.Time{}, false, err
	}
	if !ms.Valid {
		return time.Time{}, false, nil
	}
	return store.FromMillis(ms.Int64), true, nil
}
