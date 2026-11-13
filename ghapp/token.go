package ghapp

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Token is an installation access token minted for one holder (a box), never
// taken from or put in the Client's cache, so revoking it ends that holder
// alone. Printing a Token never prints its value: Reveal is the one way out.
type Token struct {
	value       string
	ExpiresAt   time.Time
	Repo        string // owner/name, as GitHub answered
	Permissions Perms  // as GitHub answered
}

// Reveal is the token's value.
func (t Token) Reveal() string { return t.value }

// HashedToken is the token as GitHub's audit log names it (its hashed_token
// field): the base64 (standard, padded) of the value's SHA-256. Installation
// tokens are among the tokens the audit log records. Docs: "Identifying
// audit log events performed by an access token",
// https://docs.github.com/en/organizations/keeping-your-organization-secure/managing-security-settings-for-your-organization/identifying-audit-log-events-performed-by-an-access-token
func (t Token) HashedToken() string {
	if t.value == "" {
		return ""
	}
	s := sha256.Sum256([]byte(t.value))
	return base64.StdEncoding.EncodeToString(s[:])
}

func (t Token) String() string {
	return fmt.Sprintf("ghapp.Token{repo %s, %s, expires %s}", t.Repo, t.Permissions.key(), t.ExpiresAt.UTC().Format(time.RFC3339))
}

// GoString and Format keep the value out of every fmt verb (%v, %+v, %#v).
func (t Token) GoString() string             { return t.String() }
func (t Token) Format(f fmt.State, _ rune)   { _, _ = f.Write([]byte(t.String())) }
func (t Token) MarshalText() ([]byte, error) { return []byte(t.String()), nil }

// ErrBroader is GitHub answering a token mint with more than was asked: another
// repository, more repositories, the whole installation, or other permissions.
var ErrBroader = errors.New("github answered a token broader than asked; it was refused")

// ErrBadExpiry is GitHub answering a token whose expires_at is not within
// (now, now + 1 hour + maxSkew]: an expired token, or a longer life than
// GitHub documents (one hour), or a clock this far off.
var ErrBadExpiry = errors.New("github answered a token whose expiry is not within the next hour; it was refused")

// maxSkew is how far past now + 1 hour an answer's expiry may be.
const maxSkew = 5 * time.Minute

// AnswerError is a minted token refused by the answer check (Err is ErrBroader
// or ErrBadExpiry). MintToken revokes it first; when that revoke failed,
// Unrevoked holds it so the caller can keep trying. Printing it never prints
// the value.
type AnswerError struct {
	Err       error
	Unrevoked Token
	RevokeErr error
}

func (e *AnswerError) Error() string {
	if e.RevokeErr != nil {
		return e.Err.Error() + " (revoking it failed: " + e.RevokeErr.Error() + ")"
	}
	return e.Err.Error() + " (revoked)"
}

func (e *AnswerError) Unwrap() error { return e.Err }

// Retry bounds for MintToken, RevokeToken and RepoInstallation: a 5xx, a 429,
// a 403 that is a rate limit, or a network error is tried again up to
// retryTries times in all, never waiting longer than retryMaxWait; a limit that
// resets later than that fails at once (closed).
const (
	retryTries   = 3
	retryBase    = time.Second
	retryMaxWait = 30 * time.Second
)

// MintToken mints a token for exactly one repository (owner/name, of the
// installation's account) with exactly the permissions p, uncached. It checks
// GitHub's answer: the selection must be "selected", the repositories exactly
// repo, the permissions exactly p (else ErrBroader), and expires_at within
// (now, now + 1 hour + 5 minutes] (else ErrBadExpiry). A refused token is
// revoked; the error is an *AnswerError.
//
// The request and answer fields are GitHub's "Create an installation access
// token for an app" (https://docs.github.com/en/rest/apps/apps#create-an-installation-access-token-for-an-app):
// repositories, permissions, repository_selection, expires_at; tokens
// "expire one hour from the time you create them".
// GitHub adds metadata read to every repository token, so callers that do not
// want ErrBroader for it ask for it.
//
// A 5xx or a rate limit is retried (bounded). A retried POST GitHub had in fact
// served leaves a token nobody holds; it lapses at GitHub's expiry (1 hour).
func (c *Client) MintToken(ctx context.Context, installation int64, repo string, p Perms) (Token, error) {
	if installation <= 0 {
		return Token{}, errors.New("ghapp: installation must be positive")
	}
	if err := validRepo(repo); err != nil {
		return Token{}, err
	}
	if err := p.validate(); err != nil {
		return Token{}, err
	}
	_, name, _ := strings.Cut(repo, "/")
	var r struct {
		Token        string            `json:"token"`
		ExpiresAt    time.Time         `json:"expires_at"`
		Permissions  map[string]string `json:"permissions"`
		Selection    string            `json:"repository_selection"`
		Repositories []struct {
			FullName string `json:"full_name"`
		} `json:"repositories"`
	}
	in := map[string]any{"repositories": []string{name}, "permissions": p}
	path := "/app/installations/" + strconv.FormatInt(installation, 10) + "/access_tokens"
	err := c.retry(ctx, func() (http.Header, error) {
		j, err := c.jwt()
		if err != nil {
			return nil, err
		}
		return callH(ctx, c.strict, http.MethodPost, c.api, path, j, in, &r, http.StatusCreated)
	})
	if err != nil {
		return Token{}, err
	}
	if r.Token == "" {
		return Token{}, fmt.Errorf("github POST %s: response has no token", path)
	}
	t := Token{value: r.Token, ExpiresAt: r.ExpiresAt, Permissions: Perms(maps.Clone(r.Permissions))}
	if len(r.Repositories) == 1 {
		t.Repo = r.Repositories[0].FullName
	}
	var bad error
	now := c.now()
	switch {
	case r.Selection != "selected" || len(r.Repositories) != 1 || !strings.EqualFold(t.Repo, repo) ||
		!maps.Equal(map[string]string(p), r.Permissions):
		bad = ErrBroader
	case !r.ExpiresAt.After(now) || r.ExpiresAt.After(now.Add(time.Hour+maxSkew)):
		bad = ErrBadExpiry
	}
	if bad != nil {
		// Revoke what GitHub handed over before refusing it.
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		if rerr := c.RevokeToken(rctx, t); rerr != nil {
			return Token{}, &AnswerError{Err: bad, Unrevoked: t, RevokeErr: rerr}
		}
		return Token{}, &AnswerError{Err: bad}
	}
	return t, nil
}

// RevokeToken ends t at GitHub (DELETE /installation/token, authenticated by
// t itself: "Revoke an installation access token",
// https://docs.github.com/en/rest/apps/installations#revoke-an-installation-access-token). A token GitHub no longer accepts (401: revoked or expired) counts
// as revoked. 5xx and rate limits are retried (bounded).
func (c *Client) RevokeToken(ctx context.Context, t Token) error {
	return c.revoke(ctx, t.value)
}

func (c *Client) revoke(ctx context.Context, tok string) error {
	if tok == "" {
		return errors.New("ghapp: no token to revoke")
	}
	err := c.retry(ctx, func() (http.Header, error) {
		return callH(ctx, c.strict, http.MethodDelete, c.api, "/installation/token", tok, nil, nil, http.StatusNoContent)
	})
	var se *statusError
	if errors.As(err, &se) && se.status == http.StatusUnauthorized {
		return nil
	}
	return err
}

// RevokeCached revokes every unexpired token in the Client's cache and empties
// it, answering how many GitHub ended and the first error. The broker calls it
// when it stops, so its own tokens do not outlive it by up to an hour.
func (c *Client) RevokeCached(ctx context.Context) (int, error) {
	c.mu.Lock()
	es := make([]*tokEntry, 0, len(c.tokens))
	for k, e := range c.tokens {
		es = append(es, e)
		delete(c.tokens, k)
	}
	c.mu.Unlock()
	n := 0
	var first error
	for _, e := range es {
		e.mu.Lock()
		tok, exp := e.tok, e.exp
		e.tok, e.exp = "", time.Time{}
		e.mu.Unlock()
		if tok == "" || !c.now().Before(exp) {
			continue
		}
		if err := c.revoke(ctx, tok); err != nil {
			if first == nil {
				first = err
			}
			continue
		}
		n++
	}
	return n, first
}

// RepoInstallation is the App's installation on repo's account
// (GET /repos/{owner}/{repo}/installation, App JWT): 0 and an error when the
// App is not installed there.
func (c *Client) RepoInstallation(ctx context.Context, repo string) (int64, error) {
	if err := validRepo(repo); err != nil {
		return 0, err
	}
	var r struct {
		ID int64 `json:"id"`
	}
	err := c.retry(ctx, func() (http.Header, error) {
		j, err := c.jwt()
		if err != nil {
			return nil, err
		}
		return callH(ctx, c.strict, http.MethodGet, c.api, "/repos/"+repo+"/installation", j, nil, &r, http.StatusOK)
	})
	if err != nil {
		return 0, err
	}
	if r.ID <= 0 {
		return 0, fmt.Errorf("github GET /repos/%s/installation: no installation id", repo)
	}
	return r.ID, nil
}

// retry runs f until it succeeds, fails in a way not worth retrying, or has
// run retryTries times.
func (c *Client) retry(ctx context.Context, f func() (http.Header, error)) error {
	for try := 1; ; try++ {
		hdr, err := f()
		if err == nil {
			return nil
		}
		wait, again := c.retryAfter(err, hdr, try)
		if !again || try >= retryTries {
			return err
		}
		if err := c.sleep(ctx, wait); err != nil {
			return err
		}
	}
}

// retryAfter says whether err is worth another try and after how long.
func (c *Client) retryAfter(err error, hdr http.Header, try int) (time.Duration, bool) {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return 0, false
	}
	backoff := retryBase << (try - 1)
	var se *statusError
	if !errors.As(err, &se) {
		// A network error (no status). A decoding error of a 2xx is not retried.
		if strings.Contains(err.Error(), "decoding response") || strings.HasPrefix(err.Error(), "ghapp:") {
			return 0, false
		}
		return backoff, true
	}
	if se.status < 500 && !rateLimited(err, hdr) {
		return 0, false
	}
	wait := backoff
	if hdr != nil {
		if ra, err := strconv.Atoi(hdr.Get("Retry-After")); err == nil && ra >= 0 {
			wait = time.Duration(ra) * time.Second
		} else if hdr.Get("X-RateLimit-Remaining") == "0" {
			if reset, err := strconv.ParseInt(hdr.Get("X-RateLimit-Reset"), 10, 64); err == nil {
				wait = time.Unix(reset, 0).Sub(c.now())
			}
		}
	}
	if wait > retryMaxWait {
		return 0, false // a limit that resets later than we wait: fail closed now
	}
	return max(wait, 0), true
}

func (c *Client) sleep(ctx context.Context, d time.Duration) error {
	if c.sleepFn != nil {
		return c.sleepFn(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
