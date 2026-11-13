package owners

import (
	"context"
	"errors"

	"github.com/sengirunners/potluck/kit/api"
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
