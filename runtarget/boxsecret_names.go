package runtarget

// Holder-set names (secrets door MUST 17). A box holder's secrets call may
// not touch a name an issuer's grant reserves (grants.Reserved). A grant
// added after a holder set a value under its name would leave that value in
// the box, unremovable by its holder and taken for the authority's own. So
// the authority keeps, per live box, the names its holder set (names only,
// never a value), and a grant reserving one of them is refused
// (grants.OnReserve) until the holder unsets it.
//
// A holder's name is recorded before the reserved check, and the grant's
// check runs after its row is written in its own transaction: whichever
// commits second sees the other, so one of the two is always refused.

import (
	"context"
	"database/sql"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/store"
	"github.com/sengirunners/potluck/runtarget/rt"
)

var boxSecretNameMigrations = []store.Migration{
	{Kind: store.KindProject, Name: "runtarget/0009_box_secret_names", Script: `CREATE TABLE run_box_secret_names (
  handle TEXT NOT NULL,
  name   TEXT NOT NULL,
  PRIMARY KEY (handle, name)
);`},
}

// holderNames records names as set by handle's holder. It answers the ones
// it added, for forgetNames when the call turns out to have written
// nothing.
func (s *Service) holderNames(ctx context.Context, project, handle string, names []string) ([]string, error) {
	if len(names) == 0 {
		return nil, nil
	}
	db, err := s.d.Store.Project(s.d.System().Scope, authz.Write, project)
	if err != nil {
		return nil, err
	}
	var added []string
	for _, n := range names {
		res, err := db.ExecContext(ctx, `INSERT INTO run_box_secret_names (handle, name) VALUES (?, ?) ON CONFLICT DO NOTHING`, handle, n)
		if err != nil {
			s.forgetNames(ctx, project, handle, added)
			return nil, err
		}
		if k, _ := res.RowsAffected(); k == 1 {
			added = append(added, n)
		}
	}
	return added, nil
}

// forgetNames removes handle's names: unset by its holder, or recorded for
// a call that wrote nothing.
func (s *Service) forgetNames(ctx context.Context, project, handle string, names []string) {
	if len(names) == 0 {
		return
	}
	db, err := s.d.Store.Project(s.d.System().Scope, authz.Write, project)
	if err == nil {
		for _, n := range names {
			if _, err = db.ExecContext(context.WithoutCancel(ctx), `DELETE FROM run_box_secret_names WHERE handle = ? AND name = ?`, handle, n); err != nil {
				break
			}
		}
	}
	if err != nil && s.d.Log != nil {
		s.d.Log.Warn("run targets: a box's holder-set names were not updated; a grant reserving one stays refused", "handle", handle, "err", err)
	}
}

// forgetBox drops an ended box's holder-set names and its secrets lock: an
// ended box holds nothing a grant must wait for.
func (s *Service) forgetBox(ctx context.Context, project, handle string) {
	s.secretsMu.Delete(project + "/" + handle)
	db, err := s.d.Store.Project(s.d.System().Scope, authz.Write, project)
	if err == nil {
		_, err = db.ExecContext(context.WithoutCancel(ctx), `DELETE FROM run_box_secret_names WHERE handle = ?`, handle)
	}
	if err != nil && s.d.Log != nil {
		s.d.Log.Warn("run targets: an ended box's holder-set names were not dropped (they hold nothing up)", "handle", handle, "err", err)
	}
}

// refuseReserved is a grants.ReserveFunc: a grant reserving name in area
// ("" every area) is refused while a live box there holds a value its
// holder set under name.
func refuseReserved(ctx context.Context, tx *sql.Tx, area, name string) error {
	var n int
	err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM run_box_secret_names n JOIN run_handles h ON h.handle = n.handle
		WHERE n.name = ? AND h.ended = '' AND (? = '' OR h.area = ?)`, name, area, area).Scan(&n)
	if err != nil {
		return err
	}
	if n > 0 {
		return api.Errorf(api.CodeConflict, "%d live box(es) hold a value their holder set under %s; an issuer's grant for that name would leave it there, out of the holder's reach", n, name).
			WithHint("its holder unsets " + name + " in those boxes first (the box secrets call with unset), or the boxes end")
	}
	return nil
}

// notWritten reports whether a failed secrets call certainly wrote nothing
// (a refusal, runtarget.go): one that timed out or lost its host may have.
func notWritten(err error) bool {
	return refusal(err) && !api.IsCode(err, rt.CodeDriverTimeout) && !api.IsCode(err, api.CodeUnavailable)
}
