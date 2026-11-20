// Package owners is the owner-directory brick (SPEC 22): who an owner is (a GitHub user or
// org, bound by its immutable numeric account id, never its login), which
// zm identity a person's account is linked to on this authority, and the
// sign-in that proves it.
//
// The interfaces are Accounts (looking accounts up) and Device (the
// provider's device flow, which the authority runs itself, so no client
// ever handles a provider token). v1's driver is GitHub
// (kit/owners/github); the second expected is GitLab. Every driver
// passes kit/owners/conformance. Owner: zm lead (the names lane).
//
// A token reaches the authority from the provider, is used for the calls
// one login needs, and is dropped: never stored, logged or echoed.
package owners

import (
	"context"
	"errors"
	"time"
)

// Account kinds.
const (
	KindUser = "user"
	KindOrg  = "org"
	KindBot  = "bot"
)

// Account is an owner as its provider knows it.
type Account struct {
	// ID is the provider's immutable account id, namespaced by driver
	// ("github:583231"), so two drivers' ids never meet. Never reused.
	ID string
	// Login is the current name: display and lookup only, it can change.
	Login string
	Kind  string // KindUser, KindOrg or KindBot
}

// Accounts is what zm needs from an account provider.
type Accounts interface {
	// Verify asks the provider who holds token. It answers ErrBadToken
	// when the provider refuses it, and any other error when the provider
	// could not be asked.
	Verify(ctx context.Context, token string) (Account, error)
	// ByLogin is the account that holds login now (logins compare without
	// case), or ErrNotFound when none does: a released or renamed login.
	// A provider that refuses for its rate limit answers *RateLimited from
	// any call here.
	ByLogin(ctx context.Context, login string) (Account, error)
	// ByID is the account with id, under its current login, or
	// ErrNotFound when the provider no longer answers for it.
	ByID(ctx context.Context, id string) (Account, error)
	// Member reports whether token's holder is an active member of the
	// org account named org (ErrBadToken when the token is refused).
	// Checked live, never cached.
	Member(ctx context.Context, token, org string) (bool, error)
	// OrgRole is the role of token's holder in the org: "admin" or
	// "member" for an active membership, "" otherwise. Binding a name under
	// an org takes an admin (a name is never rebound, so a member could
	// spend the org's names for good).
	OrgRole(ctx context.Context, token, org string) (string, error)
	// OrgMember reports whether login is a member of org now, asked with
	// the authority's own lookup token (never a person's): how a join from
	// an org grant is re-checked after the person's token is gone.
	// ErrNoView when the authority cannot see the org's members;
	// ErrLookupRefused when the provider refused its token.
	OrgMember(ctx context.Context, org, login string) (bool, error)
}

// DeviceCode is a started device flow: what the person enters, where.
type DeviceCode struct {
	UserCode        string
	VerificationURI string
	Expires         time.Time
	// Handle is the driver's own state for Wait.
	Handle any
}

// Device is the provider's device flow, run by the authority: Start asks
// for a code, Wait polls until the person approves and answers the token.
type Device interface {
	Start(ctx context.Context) (DeviceCode, error)
	// Wait answers the access token once approved, ErrDenied when the
	// person declined, ErrExpired when the code ran out, or the context's
	// error.
	Wait(ctx context.Context, code DeviceCode) (string, error)
}

// Errors drivers answer.
var (
	ErrBadToken = errors.New("owners: the provider refused the token")
	ErrDenied   = errors.New("owners: the person declined the login")
	ErrExpired  = errors.New("owners: the login code expired")
	ErrNotFound = errors.New("owners: no such account")
	ErrNoView   = errors.New("owners: the authority's lookup token cannot see the org's members")
	// ErrLookupRefused is the provider refusing the authority's own lookup
	// token (GitHub's 401): a minted one is minted anew on the next call; a
	// configured one must be replaced.
	ErrLookupRefused = errors.New("owners: the provider refused the authority's lookup token")
)

// RateLimited is what a driver answers when the provider refused a call for
// its rate limit: nothing is known about the account, and asking again
// before ResetAt (zero when the provider did not say) is refused again.
type RateLimited struct{ ResetAt time.Time }

func (e *RateLimited) Error() string {
	if e.ResetAt.IsZero() {
		return "owners: the provider's rate limit refused the call"
	}
	return "owners: the provider's rate limit refused the call until " + e.ResetAt.UTC().Format(time.RFC3339)
}
