package runners

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"github.com/sengirunners/potluck/kit/api"
)

// LinkVerified links installation to area of the pool project for a program
// that embeds the broker (its daemon is kit/server with wiring.Compute and
// modules of its own) and has verified the installation's GitHub account
// itself, for example through its own sign-in when the App was installed.
// It is a Go method with no route: only code compiled into the daemon can
// call it, so neither a network caller nor a pool area's admin reaches it.
//
// The link page's GitHub sign-in, org admin check and confirmation are the
// caller's to have done; everything else of a link is unchanged (doLink):
// the installation must be this App's (read with the App's JWT) and its
// account allowed by runners.accounts, and the link mints the same
// zm-runners member token capped to the pool project and area. An
// installation linked to another area is refused (its admin unlinks it
// first); one already linked to area is answered as it is, once the App and
// account rules are checked again. by names who
// verified the account, as the link's linked_by.
func (s *Service) LinkVerified(ctx context.Context, installation int64, area, by string) (Link, error) {
	area, by = strings.ToLower(strings.TrimSpace(area)), strings.TrimSpace(by)
	if installation <= 0 {
		return Link{}, api.Errorf(api.CodeBadRequest, "installation %d is not an installation", installation)
	}
	if by == "" {
		return Link{}, api.Errorf(api.CodeBadRequest, "a link names who verified the installation's account")
	}
	old, err := s.link(ctx, installation)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return Link{}, err
	case !strings.EqualFold(old.Area, area):
		return Link{}, api.Errorf(api.CodeConflict, "installation %d serves area %q: its admin unlinks it first", installation, old.Area)
	default:
		// Already linked here: the App and account rules still hold for it.
		cl, err := s.client(ctx)
		if err != nil {
			return Link{}, err
		}
		if _, err := s.linkable(ctx, cl, installation); err != nil {
			return Link{}, err
		}
		return old.Link, nil
	}
	l, err := s.doLink(ctx, pending{installation: installation, area: area, by: by})
	if err != nil {
		return Link{}, err
	}
	return l.Link, nil
}
