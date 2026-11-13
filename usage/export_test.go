package usage

import (
	"context"
	"time"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/kit/authz"
)

// ForgetForTest deletes one row, as a failed write would have left it.
func (s *Service) ForgetForTest(ctx context.Context, project, handle string) error {
	db, err := s.d.Store.Project(s.d.System().Scope, authz.Write, project)
	if err != nil {
		return err
	}
	_, err = db.ExecContext(ctx, `DELETE FROM box_usage WHERE handle = ?`, handle)
	return err
}

// BreakLedgerForTest renames the ledger's table, so every read of it fails.
func (s *Service) BreakLedgerForTest(ctx context.Context, project string) error {
	db, err := s.d.Store.Project(s.d.System().Scope, authz.Write, project)
	if err != nil {
		return err
	}
	_, err = db.ExecContext(ctx, `ALTER TABLE box_usage RENAME TO box_usage_broken`)
	return err
}

// SoFar is a live box's minutes so far (soFar).
var SoFar = soFar

// SetNowForTest sets the clock the limits read.
func (s *Service) SetNowForTest(f func() time.Time) { s.now = f }

// PlantLimitForTest writes a limit without asking that its area exist, as
// one left behind by an area removed from project.toml.
func (s *Service) PlantLimitForTest(ctx context.Context, project string, l computeapi.AreaMinuteLimit) error {
	_, err := s.d.Projects.SetSection(ctx, project, limitsSection, func(old any) (any, error) {
		ls, _ := old.([]computeapi.AreaMinuteLimit)
		return append(append([]computeapi.AreaMinuteLimit(nil), ls...), l), nil
	})
	return err
}

// SetMaxEndPagesForTest sets how many pages of ended boxes a limit reads; it
// answers a function that restores the old figure.
func SetMaxEndPagesForTest(n int) (restore func()) {
	old := maxEndPages
	maxEndPages = n
	return func() { maxEndPages = old }
}
