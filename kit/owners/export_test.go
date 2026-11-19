package owners

import (
	"context"
	"errors"
	"time"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/store"
)

// SetActFault makes each act call f first and fail with its error.
func SetActFault(s *Service, f func() error) { s.actFault = f }

// SetWithinFault makes a GitHub join fail after it created the identity,
// before it linked it (inside the admission's transaction).
func SetWithinFault(s *Service, f func() error) { s.withinFault = f }

// LinkNow links identity to acc as a collected link login would.
func LinkNow(s *Service, ctx context.Context, identity api.Identity, acc Account) error {
	_, err := s.link(ctx, identity, acc, "a test")
	return err
}

// BindNow binds as a collected bind login would, approved by holder, with
// member standing for GitHub's live answer on the owner org.
func BindNow(s *Service, ctx context.Context, identity api.Identity, project, owner, name string, holder Account, member bool) (api.NameBinding, error) {
	l := &login{purpose: PurposeBind, identity: identity, bindProject: project, bindOwner: owner, bindName: name}
	o, err := s.acc.ByLogin(ctx, owner)
	switch {
	case errors.Is(err, ErrNotFound):
		l.bindErr = s.missingOwner(ctx, owner)
	case err != nil:
		l.bindErr = providerDown(err)
	default:
		s.seen(ctx, o)
		l.bindOwnerAcc, l.bindMember = o, member
	}
	return s.bind(ctx, l, holder)
}

// StartsLen is how many owners the start limits remember.
func StartsLen(s *Service) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.starts)
}

// RetryAfterFor is when the grant queue asks again after GitHub named reset.
func RetryAfterFor(s *Service, reset time.Time) time.Time {
	return s.retryAfter(&RateLimited{ResetAt: reset})
}

// QueuedSnapshot reads the queued grant to login as a pass of the worker
// would, and answers what that pass would do next: claim the row and make
// the grant to acc (made false when the row has changed or gone since).
func QueuedSnapshot(s *Service, ctx context.Context, project, area, login string, acc Account) func() (bool, error) {
	var q queuedGrant
	var owner int
	var created int64
	err := s.d.Store.Registry().QueryRowContext(ctx, `SELECT project, area, login, role, identity_id, identity_name, address, machine_owner, token_id, created
		FROM github_grant_queue WHERE project = ? AND area = ? AND login = ?`, project, area, login).
		Scan(&q.project, &q.area, &q.login, &q.role, &q.granter.ID, &q.granter.Name, &q.address, &owner, &q.tokenID, &created)
	if err != nil {
		panic(err)
	}
	q.machineOwner, q.created = owner != 0, store.FromMillis(created)
	return func() (bool, error) { return s.makeQueued(ctx, q, s.d.Projects.Slug(project), acc) }
}
