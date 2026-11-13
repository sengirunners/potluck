// Package github is the owners brick's GitHub driver: an account is a
// GitHub user or org, and its id is GitHub's numeric account id
// ("github:<id>"). The API and web base URLs are configuration (tests point
// them at kit/owners/githubfake; GitHub Enterprise at its own host).
//
// Device is "Sign in with GitHub": GitHub's OAuth device flow through
// golang.org/x/oauth2 (an OAuth app's client id, no secret), run by the
// authority, never by a client.
package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"golang.org/x/oauth2"

	"github.com/sengirunners/potluck/kit/owners"
)

// Defaults for github.com.
const (
	DefaultAPI = "https://api.github.com"
	DefaultWeb = "https://github.com"
)

// Scopes the device flow asks for: who the person is, and their org
// memberships (binding a project under an org, SPEC 22).
var Scopes = []string{"read:user", "read:org"}

// IDPrefix namespaces GitHub's account ids.
const IDPrefix = "github:"

// Driver implements owners.Accounts against a GitHub API.
type Driver struct {
	api string
	hc  *http.Client
	// LookupToken, when set, is the authority's own token for lookups by
	// login and id (GitHub allows 60 unauthenticated calls an hour per
	// address) and for re-checking org memberships (OrgMember: it must be
	// an org member's to see private members). Never a person's login
	// token; an error or "" goes without one.
	LookupToken func(ctx context.Context) (string, error)
}

// New is a driver for the API at base (DefaultAPI when empty). hc nil is a
// client with a 10-second timeout.
func New(base string, hc *http.Client) *Driver {
	if base == "" {
		base = DefaultAPI
	}
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Second}
	}
	return &Driver{api: strings.TrimRight(base, "/"), hc: hc}
}

// user is the part of GitHub's user object zm reads.
type user struct {
	ID    int64  `json:"id"`
	Login string `json:"login"`
	Type  string `json:"type"` // User, Organization, Bot
}

func (u user) account() owners.Account {
	kind := owners.KindUser
	switch u.Type {
	case "Organization":
		kind = owners.KindOrg
	case "Bot":
		kind = owners.KindBot
	}
	return owners.Account{ID: IDPrefix + strconv.FormatInt(u.ID, 10), Login: u.Login, Kind: kind}
}

// get GETs path, with token when it is set, decoding a 200 into out. It
// answers the status; a transport error never names the token (a header).
func (d *Driver) get(ctx context.Context, path, token string, out any) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.api+path, nil)
	if err != nil {
		return 0, err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := d.hc.Do(req)
	if err != nil {
		return 0, fmt.Errorf("asking GitHub: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return resp.StatusCode, nil
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out); err != nil {
		return 0, fmt.Errorf("reading GitHub's %s: %w", path, err)
	}
	return resp.StatusCode, nil
}

// lookup is a user object at path: ErrNotFound on 404.
func (d *Driver) lookup(ctx context.Context, path, token string) (owners.Account, error) {
	var u user
	switch st, err := d.get(ctx, path, token, &u); {
	case err != nil:
		return owners.Account{}, err
	case st == http.StatusUnauthorized && token != "":
		return owners.Account{}, owners.ErrBadToken
	case st == http.StatusNotFound:
		return owners.Account{}, owners.ErrNotFound
	case st != http.StatusOK:
		return owners.Account{}, fmt.Errorf("GitHub answered %d for %s", st, path)
	case u.ID <= 0 || u.Login == "":
		return owners.Account{}, fmt.Errorf("GitHub's %s has no id or login", path)
	}
	return u.account(), nil
}

// Verify is GET /user with the token: the holder's numeric id and login.
func (d *Driver) Verify(ctx context.Context, token string) (owners.Account, error) {
	if token = strings.TrimSpace(token); token == "" {
		return owners.Account{}, owners.ErrBadToken
	}
	a, err := d.lookup(ctx, "/user", token)
	if errors.Is(err, owners.ErrNotFound) {
		return owners.Account{}, fmt.Errorf("GitHub answered 404 for /user")
	}
	return a, err
}

// ByLogin is GET /users/{login}.
func (d *Driver) ByLogin(ctx context.Context, login string) (owners.Account, error) {
	if !validLogin(login) {
		return owners.Account{}, owners.ErrNotFound
	}
	return d.authorityLookup(ctx, "/users/"+login)
}

// authorityLookup is lookup with the authority's own token: GitHub
// refusing it is an outage (the operator's to fix), not ErrBadToken.
func (d *Driver) authorityLookup(ctx context.Context, path string) (owners.Account, error) {
	a, err := d.lookup(ctx, path, d.lookupToken(ctx))
	if errors.Is(err, owners.ErrBadToken) {
		return a, fmt.Errorf("GitHub refused the authority's lookup token (github.lookup_token)")
	}
	return a, err
}

func (d *Driver) lookupToken(ctx context.Context) string {
	if d.LookupToken == nil {
		return ""
	}
	tok, err := d.LookupToken(ctx)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(tok)
}

// OrgMember is GET /orgs/{org}/members/{login} with the authority's lookup
// token: 204 a member, 404 not; a 302 means the token cannot see the org's
// members (it is no member's): ErrNoView.
func (d *Driver) OrgMember(ctx context.Context, org, login string) (bool, error) {
	if !validLogin(org) || !validLogin(login) {
		return false, nil
	}
	tok := d.lookupToken(ctx)
	if tok == "" {
		return false, owners.ErrNoView
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.api+"/orgs/"+org+"/members/"+login, nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	hc := *d.hc
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := hc.Do(req)
	if err != nil {
		return false, fmt.Errorf("asking GitHub: %w", err)
	}
	resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusNoContent:
		return true, nil
	case http.StatusNotFound:
		return false, nil
	case http.StatusFound, http.StatusUnauthorized, http.StatusForbidden:
		return false, owners.ErrNoView
	}
	return false, fmt.Errorf("GitHub answered %d for an org's member", resp.StatusCode)
}

// ByID is GET /user/{id}, GitHub's lookup by numeric account id.
func (d *Driver) ByID(ctx context.Context, id string) (owners.Account, error) {
	n, err := strconv.ParseInt(strings.TrimPrefix(id, IDPrefix), 10, 64)
	if !strings.HasPrefix(id, IDPrefix) || err != nil || n <= 0 {
		return owners.Account{}, owners.ErrNotFound
	}
	return d.authorityLookup(ctx, "/user/"+strconv.FormatInt(n, 10))
}

// Member is GET /user/memberships/orgs/{org} with the holder's token
// (scope read:org): an active membership only, never a pending invite.
func (d *Driver) Member(ctx context.Context, token, org string) (bool, error) {
	role, err := d.OrgRole(ctx, token, org)
	return role != "", err
}

// OrgRole is the same call's role for an active membership: "admin" or
// "member"; "" when the holder is not an active member.
func (d *Driver) OrgRole(ctx context.Context, token, org string) (string, error) {
	if token = strings.TrimSpace(token); token == "" {
		return "", owners.ErrBadToken
	}
	if !validLogin(org) {
		return "", nil
	}
	var m struct {
		State string `json:"state"`
		Role  string `json:"role"`
	}
	switch st, err := d.get(ctx, "/user/memberships/orgs/"+org, token, &m); {
	case err != nil:
		return "", err
	case st == http.StatusUnauthorized:
		return "", owners.ErrBadToken
	case st == http.StatusNotFound || st == http.StatusForbidden:
		return "", nil
	case st != http.StatusOK:
		return "", fmt.Errorf("GitHub answered %d for an org membership", st)
	}
	if m.State != "active" {
		return "", nil
	}
	if m.Role == "admin" {
		return "admin", nil
	}
	return "member", nil
}

// validLogin is GitHub's login alphabet (letters, digits and hyphens, not
// first, at most 39; older logins may hold a double or a trailing hyphen),
// so a login never adds a path segment.
func validLogin(s string) bool {
	if s == "" || len(s) > 39 || s[0] == '-' {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-') {
			return false
		}
	}
	return true
}

// Device is GitHub's OAuth device flow for an OAuth app's client id, run
// by the authority (owners.Device). No client secret: the device flow
// takes the client id alone (the app must have "Enable Device Flow" set).
type Device struct {
	cfg *oauth2.Config
	hc  *http.Client
}

// NewDevice is the device flow at web (DefaultWeb when empty) for
// clientID. hc nil is a client with a 30-second timeout per request.
func NewDevice(web, clientID string, hc *http.Client) *Device {
	if web == "" {
		web = DefaultWeb
	}
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	web = strings.TrimRight(web, "/")
	return &Device{hc: hc, cfg: &oauth2.Config{
		ClientID: clientID,
		Scopes:   Scopes,
		Endpoint: oauth2.Endpoint{
			DeviceAuthURL: web + "/login/device/code",
			TokenURL:      web + "/login/oauth/access_token",
			AuthStyle:     oauth2.AuthStyleInParams,
		},
	}}
}

func (d *Device) ctx(ctx context.Context) context.Context {
	return context.WithValue(ctx, oauth2.HTTPClient, d.hc)
}

// Start asks GitHub for a device and user code.
func (d *Device) Start(ctx context.Context) (owners.DeviceCode, error) {
	da, err := d.cfg.DeviceAuth(d.ctx(ctx))
	if err != nil {
		return owners.DeviceCode{}, fmt.Errorf("starting GitHub's device flow: %w", err)
	}
	if da.DeviceCode == "" || da.UserCode == "" {
		return owners.DeviceCode{}, fmt.Errorf("GitHub started no device flow for client id %q (is \"Enable Device Flow\" set on the OAuth app?)", d.cfg.ClientID)
	}
	exp := da.Expiry
	if exp.IsZero() {
		exp = time.Now().Add(15 * time.Minute)
	}
	return owners.DeviceCode{UserCode: da.UserCode, VerificationURI: da.VerificationURI, Expires: exp, Handle: da}, nil
}

// Wait polls GitHub (at the interval it asked for) until the person
// approves, declines or the code expires.
func (d *Device) Wait(ctx context.Context, code owners.DeviceCode) (string, error) {
	da, ok := code.Handle.(*oauth2.DeviceAuthResponse)
	if !ok {
		return "", errors.New("not a GitHub device code")
	}
	tok, err := d.cfg.DeviceAccessToken(d.ctx(ctx), da)
	var re *oauth2.RetrieveError
	switch {
	case err == nil && tok.AccessToken != "":
		return tok.AccessToken, nil
	case err == nil:
		return "", errors.New("GitHub answered no access token")
	case errors.As(err, &re) && re.ErrorCode == "access_denied":
		return "", owners.ErrDenied
	case errors.As(err, &re) && re.ErrorCode == "expired_token":
		return "", owners.ErrExpired
	case ctx.Err() != nil:
		return "", ctx.Err()
	}
	return "", fmt.Errorf("waiting for GitHub's approval: %w", err)
}
