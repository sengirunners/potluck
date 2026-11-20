package ghapp

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"
)

// AccountToken returns a cached installation token that names no repository,
// for calls about the installation's account rather than one of its
// repositories: lookups by login or id, and an organization's members
// (members read). It is refreshed when fewer than 5 minutes remain, like
// InstallationToken. Naming no repository, it reaches every repository of
// the installation at the levels in p, so it refuses any write: GitHub's
// metadata read on those repositories is the most it can add. RevokeCached
// ends it.
func (c *Client) AccountToken(ctx context.Context, installation int64, p Perms) (string, error) {
	if err := p.validate(); err != nil {
		return "", err
	}
	for k, v := range p {
		if v != "read" {
			return "", fmt.Errorf("ghapp: an account token is read only, not %s=%s", k, v)
		}
	}
	return c.cachedToken(ctx, installation, "", p)
}

// AppOwner is the login of the user or organization that owns this App
// (GET /app).
func (c *Client) AppOwner(ctx context.Context) (string, error) {
	var r struct {
		Owner struct {
			Login string `json:"login"`
		} `json:"owner"`
	}
	if err := c.asApp(ctx, http.MethodGet, "/app", nil, &r, http.StatusOK); err != nil {
		return "", err
	}
	if !validName(r.Owner.Login) {
		return "", errors.New("GitHub supplied no owner for the App")
	}
	return r.Owner.Login, nil
}

// ForgetToken drops tok from the token cache, so the next call for its
// installation, repository and permissions mints a new one: for a token
// GitHub refused (401) before its expiry, as after the App was uninstalled
// and installed again.
func (c *Client) ForgetToken(tok string) {
	if tok == "" {
		return
	}
	c.mu.Lock()
	es := make([]*tokEntry, 0, len(c.tokens))
	for _, e := range c.tokens {
		es = append(es, e)
	}
	c.mu.Unlock()
	for _, e := range es { // e.mu is never held while c.mu is taken
		e.mu.Lock()
		if e.tok == tok {
			e.tok, e.exp = "", time.Time{}
		}
		e.mu.Unlock()
	}
}
