package wiring

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/sengirunners/potluck/ghapp"
	"github.com/sengirunners/potluck/kit/config"
	ghaccounts "github.com/sengirunners/potluck/kit/owners/github"
	"github.com/sengirunners/potluck/kit/secrets"
)

// setLookup gives the owners GitHub driver its lookup token from
// config.json, read once at start:
//
//   - github.lookup_token set: that secret, for every call. A value that is
//     not a secret reference is not used (a warning says so, never quoting
//     it), and lookups go unauthenticated.
//   - unset, and runners.github names an App (app_id and private_key) on the
//     same GitHub as github.api_url: tokens minted from that App (appLookup).
//   - otherwise none: lookups go unauthenticated and org grants cannot be
//     checked.
func setLookup(drv *ghaccounts.Driver, f config.File, log *slog.Logger, sec secrets.Driver) {
	if f.GitHub.LookupToken != "" {
		ref, err := secrets.Parse(f.GitHub.LookupToken)
		if err != nil || ref.Name == "" {
			// The error would quote the value: a token pasted in config.json.
			log.Warn("owners: github.lookup_token is not a secret reference (file:, keychain: or env:); looking up without it")
			return
		}
		drv.LookupToken = func(ctx context.Context, _ string) (string, error) {
			v, err := sec.Get(ctx, ref)
			return v.Reveal(), err
		}
		return
	}
	app := f.Runners.GitHub
	if app.AppID <= 0 || app.PrivateKey == "" {
		return
	}
	if apiBase(app.APIURL) != apiBase(f.GitHub.APIURL) {
		log.Warn("owners: runners.github's App is on another GitHub than github.api_url; looking up without a token",
			"app_api", apiBase(app.APIURL), "api", apiBase(f.GitHub.APIURL))
		return
	}
	a := &appLookup{app: app, sec: sec, log: log, inst: map[string]installation{}}
	drv.LookupToken, drv.LookupRefused, drv.LookupSource = a.token, a.refused, "minted from runners.github's App"
}

// apiBase is a GitHub API base URL as compared: the default when empty, no
// trailing slash.
func apiBase(u string) string {
	if u == "" {
		u = ghaccounts.DefaultAPI
	}
	return strings.TrimRight(u, "/")
}

// Permissions of the minted lookup tokens: the least each call needs.
var (
	lookupPerms  = ghapp.Perms{"metadata": "read"}
	membersPerms = ghapp.Perms{"members": "read", "metadata": "read"}
)

// noInstallAgain is how long an account with no installation of the App is
// not asked about again.
const noInstallAgain = 10 * time.Minute

// errNotInstalled is an account with no installation of the App: there is
// no token for it, and the driver goes without one (an org's members then
// answer owners.ErrNoView).
var errNotInstalled = errors.New("the runners App is not installed there")

// appLookup mints lookup tokens from the runner broker's GitHub App:
//
//   - a lookup by login or id (org "") takes metadata read from the
//     installation on the App's owner. Never another account's: its token
//     would spend that account's API limit, which its runner jobs share.
//     With the owner's missing, lookups go unauthenticated (warned once);
//   - org's members take members and metadata read from the installation on
//     org itself, the only one whose token sees them.
//
// The client and each installation are found on first use and kept. An
// installation that is missing or that GitHub refuses (suspended, or the
// App's key not accepted) is asked about again after noInstallAgain, so it
// costs no call per lookup. ghapp caches each token and mints a new one when
// fewer than 5 minutes of its hour remain, or after GitHub refused it
// (refused).
type appLookup struct {
	app config.RunnersGitHub
	sec secrets.Driver
	log *slog.Logger

	build sync.Mutex // held while the client is built (reading the key may prompt)
	mu    sync.Mutex // the fields below
	cl    *ghapp.Client
	look  installation            // the owner's installation, which lookups by login or id use
	inst  map[string]installation // lower-cased org login
	// warned is whether the owner's missing installation was warned about.
	warned bool
}

// installation is an installation id, or none until retry.
type installation struct {
	id    int64
	retry time.Time // id 0: not installed; ask again from then
}

// token is a token for a lookup by login or id (org "") or for org's
// members. Failures other than errNotInstalled are logged: the driver drops
// the error and goes without a token.
func (a *appLookup) token(ctx context.Context, org string) (string, error) {
	tok, err := a.mint(ctx, org)
	if err != nil && !errors.Is(err, errNotInstalled) {
		a.log.Warn("owners: minting a GitHub lookup token from runners.github's App", "org", org, "err", err)
	}
	return tok, err
}

// refused drops a token GitHub answered 401 from the client's cache.
func (a *appLookup) refused(tok string) {
	a.mu.Lock()
	cl := a.cl
	a.mu.Unlock()
	if cl != nil {
		cl.ForgetToken(tok)
	}
}

func (a *appLookup) mint(ctx context.Context, org string) (string, error) {
	cl, err := a.client(ctx)
	if err != nil {
		return "", err
	}
	if org == "" {
		id, err := a.ownerInstallation(ctx, cl)
		if err != nil {
			return "", err
		}
		tok, err := cl.AccountToken(ctx, id, lookupPerms)
		if ghapp.WholeInstallation(err) {
			a.mu.Lock()
			a.look = afterRefusal(err)
			a.mu.Unlock()
		}
		return tok, err
	}
	id, err := a.orgInstallation(ctx, cl, org)
	if err != nil {
		return "", err
	}
	tok, err := cl.AccountToken(ctx, id, membersPerms)
	if ghapp.WholeInstallation(err) {
		a.mu.Lock()
		a.inst[strings.ToLower(org)] = afterRefusal(err)
		a.mu.Unlock()
	}
	return tok, err
}

// afterRefusal is what an installation whose token GitHub refused as a
// whole becomes: unknown (found again on the next call: it was uninstalled,
// and may be installed again under a new id), or for a refusal of the
// installation itself (suspended, or the key not accepted), none until
// noInstallAgain has passed.
func afterRefusal(err error) installation {
	if errors.Is(err, ghapp.ErrNoInstallation) {
		return installation{}
	}
	return installation{retry: time.Now().Add(noInstallAgain)}
}

// client builds the App's client on first use: the key is read then, not
// at start, so a keychain prompt waits for a lookup.
func (a *appLookup) client(ctx context.Context) (*ghapp.Client, error) {
	a.build.Lock()
	defer a.build.Unlock()
	a.mu.Lock()
	cl := a.cl
	a.mu.Unlock()
	if cl != nil {
		return cl, nil
	}
	ref, err := secrets.Parse(a.app.PrivateKey)
	if err != nil {
		return nil, fmt.Errorf("runners.github.private_key: %w", err)
	}
	key, err := a.sec.Get(ctx, ref)
	if err != nil {
		return nil, fmt.Errorf("runners.github.private_key: %w", err)
	}
	cl, err = ghapp.New(ghapp.Config{AppID: a.app.AppID, PrivateKeyPEM: []byte(strings.TrimSpace(key.Reveal())), APIURL: apiBase(a.app.APIURL)})
	if err != nil {
		return nil, err
	}
	a.mu.Lock()
	a.cl = cl
	a.mu.Unlock()
	return cl, nil
}

// ownerInstallation is the App's installation on its own owner, which
// lookups by login or id mint from: errNotInstalled when there is none
// (warned once; asked again after noInstallAgain).
func (a *appLookup) ownerInstallation(ctx context.Context, cl *ghapp.Client) (int64, error) {
	a.mu.Lock()
	look := a.look
	a.mu.Unlock()
	switch {
	case look.id > 0:
		return look.id, nil
	case time.Now().Before(look.retry):
		return 0, errNotInstalled
	}
	owner, err := cl.AppOwner(ctx)
	if err != nil {
		return 0, err
	}
	id, err := cl.OwnerInstallation(ctx, owner)
	switch {
	case ghapp.NotFound(err):
		a.mu.Lock()
		a.look = installation{retry: time.Now().Add(noInstallAgain)}
		warn := !a.warned
		a.warned = true
		a.mu.Unlock()
		if warn {
			a.log.Warn("owners: runners.github's App is not installed on its owner; looking GitHub logins up without a token until it is", "owner", owner)
		}
		return 0, errNotInstalled
	case err != nil:
		return 0, err
	}
	a.mu.Lock()
	a.look = installation{id: id}
	a.mu.Unlock()
	return id, nil
}

// orgInstallation is the App's installation on org: errNotInstalled when
// there is none (asked again after noInstallAgain).
func (a *appLookup) orgInstallation(ctx context.Context, cl *ghapp.Client, org string) (int64, error) {
	k := strings.ToLower(org)
	a.mu.Lock()
	in, ok := a.inst[k]
	a.mu.Unlock()
	switch {
	case ok && in.id > 0:
		return in.id, nil
	case ok && time.Now().Before(in.retry):
		return 0, errNotInstalled
	}
	id, err := cl.OwnerInstallation(ctx, org)
	switch {
	case ghapp.NotFound(err):
		a.mu.Lock()
		a.inst[k] = installation{retry: time.Now().Add(noInstallAgain)}
		a.mu.Unlock()
		return 0, errNotInstalled
	case err != nil:
		return 0, err
	}
	a.mu.Lock()
	a.inst[k] = installation{id: id}
	a.mu.Unlock()
	return id, nil
}
