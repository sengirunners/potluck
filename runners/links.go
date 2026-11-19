package runners

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sengirunners/potluck/ghapp"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/core"
)

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// Link is one installation serving one pool area (GET /v0/runners/links).
type Link struct {
	Installation int64  `json:"installation_id"`
	Account      string `json:"account"`
	AccountType  string `json:"account_type"`
	Area         string `json:"area"`
	LinkedBy     string `json:"linked_by"`
	Created      string `json:"created"`
}

type link struct {
	Link
	project, tokenID string
}

func (s *Service) link(ctx context.Context, installation int64) (link, error) {
	var l link
	var created int64
	err := s.db().QueryRowContext(ctx, `SELECT installation, account, account_type, project, area, linked_by, token_id, created
		FROM runners_links WHERE installation = ?`, installation).Scan(&l.Installation, &l.Account, &l.AccountType, &l.project,
		&l.Area, &l.LinkedBy, &l.tokenID, &created)
	l.Created = time.Unix(created, 0).UTC().Format(time.RFC3339)
	return l, err
}

// linkedInstallationOf is the installation linked here on GitHub account
// (any case), as the link recorded it; the newest if there are several.
// sql.ErrNoRows: none.
func (s *Service) linkedInstallationOf(ctx context.Context, account string) (int64, error) {
	var id int64
	err := s.db().QueryRowContext(ctx, `SELECT installation FROM runners_links WHERE account = ? COLLATE NOCASE
		ORDER BY created DESC, installation DESC LIMIT 1`, account).Scan(&id)
	return id, err
}

// linkStart is POST /v0/runners/links: an area admin asks to link an
// installation; the answer is GitHub's authorize URL, whose return proves the
// person can see the installation.
func (s *Service) linkStart(r *http.Request) (any, error) {
	ctx := r.Context()
	var req struct {
		Installation int64  `json:"installation_id"`
		Area         string `json:"area"`
	}
	if err := api.DecodeJSON(r, &req); err != nil {
		return nil, err
	}
	p, err := s.pool()
	if err != nil {
		return nil, err
	}
	sub := core.SubjectOf(ctx)
	if err := sub.Check(authz.Admin, authz.Object{Project: p.ID, Area: req.Area}, "project", p.Slug); err != nil {
		return nil, err
	}
	c := s.conf()
	if c.GitHub.ClientID == "" || c.GitHub.ClientSecret == "" || c.PublicURL == "" {
		return nil, s.off()
	}
	if req.Installation < 0 {
		return nil, api.Errorf(api.CodeBadRequest, "installation_id %d is not an installation", req.Installation)
	}
	if req.Installation == 0 {
		// Not installed yet (or on another account): the App's install URL,
		// whose return (setup_url) starts this link with no number to copy.
		u, exp, err := s.newInstall(ctx, req.Area, sub.Address, nil)
		if err != nil {
			return nil, err
		}
		return map[string]any{"install_url": u, "expires_at": exp.Format(time.RFC3339)}, nil
	}
	cl, err := s.client(ctx)
	if err != nil {
		return nil, err
	}
	in, err := cl.Installation(ctx, req.Installation)
	if err != nil {
		return nil, api.Errorf(api.CodeNotFound, "installation %d is not one of this App's", req.Installation)
	}
	if why := s.accountRefused(in.Account); why != "" {
		return nil, api.Errorf(api.CodeForbidden, "%s", why).WithHint("add it to runners.accounts in config.json; the broker reads it at its next start")
	}
	// Re-linking into another area takes admin of the area that has it now.
	var replaces any
	old, err := s.link(ctx, req.Installation)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if err == nil {
		if old.Area != req.Area && !sub.Can(authz.Admin, authz.Object{Project: old.project, Area: old.Area}) {
			return nil, api.Errorf(api.CodeForbidden, "installation %d serves another area: its admin unlinks it first", req.Installation)
		}
		replaces = old.Area
	}
	state, exp := newState(), s.now().Add(stateTTL)
	if _, err := s.db().ExecContext(ctx, `INSERT INTO runners_pending (state, kind, installation, area, replaces, by, expires) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		state, pendingLink, req.Installation, req.Area, replaces, sub.Address, exp.Unix()); err != nil {
		return nil, err
	}
	q := url.Values{"client_id": {c.GitHub.ClientID}, "state": {state}, "redirect_uri": {c.PublicURL + "/v0/runners/oauth"}}
	return map[string]any{"authorize_url": c.GitHub.WebURL + "/login/oauth/authorize?" + q.Encode(),
		"expires_at": exp.Format(time.RFC3339)}, nil
}

// oauth is GET /v0/runners/oauth: GitHub's return from the authorize URL.
// It proves the person administers the installation's account (an org's
// admin, or the user account itself), drops their token, and asks them to
// say yes to the link the requester asked for: a link reaches its area only
// with the consent of someone who could install the App there.
//
// An App that authorizes during installation (GitHub's "Install & Authorize"
// button) skips setup_url: GitHub returns here instead, with installation_id,
// setup_action, a code of its own and the install URL's state. That return
// goes to the setup_url step (installed), which takes only a live install
// state and sends the person on to a sign-in with a link state; GitHub's
// install-time code is never exchanged.
func (s *Service) oauth(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()
	p, err := s.takeState(ctx, q.Get("state"), pendingLink)
	if err != nil && (q.Has("installation_id") || q.Has("setup_action")) {
		s.installed(w, r)
		return
	}
	if err != nil {
		page(w, http.StatusNotFound, "This link expired or was used", "Start again: "+s.cmd("runners link")+" on the pool's machine.")
		return
	}
	cl, err := s.client(ctx)
	if err != nil {
		page(w, http.StatusServiceUnavailable, "The runner broker is not set up", err.Error())
		return
	}
	in, err := cl.Installation(ctx, p.installation)
	if err != nil {
		page(w, http.StatusBadGateway, "GitHub did not describe the installation", err.Error())
		return
	}
	tok, err := s.userToken(ctx, r.URL.Query().Get("code"))
	if err != nil {
		page(w, http.StatusBadGateway, "GitHub did not sign you in", err.Error())
		return
	}
	ok, login, err := s.administers(ctx, tok, in)
	tok = "" // dropped: never stored
	if errors.Is(err, ghapp.ErrNoMembersRead) {
		page(w, http.StatusBadGateway, "The runner App cannot check org admins", err.Error())
		return
	}
	if err != nil {
		page(w, http.StatusBadGateway, "GitHub did not answer", err.Error())
		return
	}
	if !ok {
		page(w, http.StatusForbidden, "Not linked", fmt.Sprintf("Only an admin of GitHub %s %s can link its installation; %s is not one.",
			strings.ToLower(in.AccountType), in.Account, login))
		return
	}
	state, exp := newState(), s.now().Add(stateTTL)
	if _, err := s.db().ExecContext(ctx, `INSERT INTO runners_pending (state, kind, installation, area, replaces, by, expires) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		state, pendingConfirm, p.installation, p.area, nullable(p), p.by, exp.Unix()); err != nil {
		page(w, http.StatusInternalServerError, "Not linked", err.Error())
		return
	}
	area := p.area
	if area == "" {
		area = "(the whole pool project)"
	}
	confirmPage(w, fmt.Sprintf("Run GitHub %s %s's jobs on %s's pool?", strings.ToLower(in.AccountType), in.Account, config.Program),
		fmt.Sprintf("%s asked to link this installation to pool area %s. Its Actions jobs labelled for the pool would then run in that area's throwaway VMs. Say yes only if you expected this.",
			p.by, area), state)
}

func nullable(p pending) any {
	if p.mayReplace {
		return p.replaces
	}
	return nil
}

// administers says whether the token's person may link the installation:
// an active admin of its org, or the very user account it is on.
func (s *Service) administers(ctx context.Context, tok string, in ghapp.Installation) (bool, string, error) {
	base := s.conf().GitHub.APIURL
	login, err := ghapp.UserLogin(ctx, s.HTTP, base, tok)
	if err != nil {
		return false, "", err
	}
	ids, err := ghapp.UserInstallations(ctx, s.HTTP, base, tok)
	if err != nil || !slices.Contains(ids, in.ID) {
		return false, login, err
	}
	if in.AccountType == "Organization" {
		ok, err := ghapp.UserOrgAdmin(ctx, s.HTTP, base, tok, in.Account)
		return ok, login, err
	}
	return strings.EqualFold(login, in.Account), login, nil
}

// confirm is POST /v0/runners/oauth: the person's yes.
func (s *Service) confirm(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	_ = r.ParseForm()
	p, err := s.takeState(ctx, r.PostForm.Get("state"), pendingConfirm)
	if err != nil {
		page(w, http.StatusNotFound, "This link expired or was used", "Start again: "+s.cmd("runners link")+" on the pool's machine.")
		return
	}
	l, err := s.doLink(ctx, p)
	if err != nil {
		page(w, http.StatusConflict, "Not linked", err.Error())
		return
	}
	page(w, http.StatusOK, "Linked", fmt.Sprintf("GitHub %s %s now runs its pool jobs in %s's pool, area %q. You can close this tab.",
		strings.ToLower(l.AccountType), l.Account, config.Program, l.Area))
}

// userToken exchanges an OAuth code for a user-to-server token.
func (s *Service) userToken(ctx context.Context, code string) (string, error) {
	c := s.conf()
	if code == "" || len(code) > 200 {
		return "", errors.New("no code")
	}
	secret, err := s.secret(ctx, c.GitHub.ClientSecret)
	if err != nil {
		return "", err
	}
	form := url.Values{"client_id": {c.GitHub.ClientID}, "client_secret": {secret}, "code": {code},
		"redirect_uri": {c.PublicURL + "/v0/runners/oauth"}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.GitHub.WebURL+"/login/oauth/access_token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := s.HTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var out struct {
		Token string `json:"access_token"`
		Error string `json:"error_description"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || out.Token == "" {
		if out.Error != "" {
			return "", errors.New(out.Error)
		}
		return "", fmt.Errorf("GitHub answered %d", resp.StatusCode)
	}
	return out.Token, nil
}

// doLink records the link and mints zm-runners' token for the area.
func (s *Service) doLink(ctx context.Context, p pending) (link, error) {
	pool, err := s.pool()
	if err != nil {
		return link{}, err
	}
	cl, err := s.client(ctx)
	if err != nil {
		return link{}, err
	}
	in, err := cl.Installation(ctx, p.installation)
	if err != nil {
		return link{}, err
	}
	if why := s.accountRefused(in.Account); why != "" {
		return link{}, errors.New(why)
	}
	s.linkMu.Lock() // one link change at a time: the check, grant, insert and revoke below
	defer s.linkMu.Unlock()
	old, oerr := s.link(ctx, p.installation)
	if oerr != nil && !errors.Is(oerr, sql.ErrNoRows) {
		return link{}, oerr
	}
	if oerr == nil && old.Area != p.area && (!p.mayReplace || old.Area != p.replaces) {
		return link{}, fmt.Errorf("installation %d was linked to another area meanwhile; ask again", p.installation)
	}
	sys := s.d.System()
	if _, err := s.d.Projects.Grant(ctx, sys, api.GrantRequest{Identity: Identity, Role: api.RoleMember, Project: pool.ID, Area: p.area}); err != nil {
		return link{}, err
	}
	tok, err := s.d.Projects.Mint(ctx, sys, api.MintTokenRequest{As: Identity, Project: pool.ID, Area: p.area, Role: api.RoleMember,
		NoGrant: true, Note: "runner broker: installation " + itoa(p.installation)})
	if err != nil {
		return link{}, err
	}
	// Only the token's id is kept: the broker acts as its subject
	// (core.Deps.SubscriptionSubject), so its secret is never needed or stored.
	l := link{Link: Link{Installation: p.installation, Account: in.Account, AccountType: in.AccountType, Area: p.area, LinkedBy: p.by},
		project: pool.ID, tokenID: tok.Token.ID}
	if _, err := s.db().ExecContext(ctx, `INSERT OR REPLACE INTO runners_links (installation, account, account_type, project, area, linked_by, token_id, created)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, l.Installation, l.Account, l.AccountType, l.project, l.Area, l.LinkedBy, l.tokenID, s.now().Unix()); err != nil {
		_, _ = s.d.Projects.RevokeToken(ctx, tok.Token.ID)
		return link{}, err
	}
	if oerr == nil { // the old link's token, and its area's grant if nothing else there needs it
		_, _ = s.d.Projects.RevokeToken(ctx, old.tokenID)
		if old.Area != l.Area {
			s.dropGrant(ctx, old.project, old.Area)
		}
	}
	return l, nil
}

// links is GET /v0/runners/links: the caller's readable areas only.
func (s *Service) links(r *http.Request) (any, error) {
	ctx := r.Context()
	p, err := s.readablePool(r)
	if err != nil {
		return nil, err
	}
	sub := core.SubjectOf(ctx)
	areaSQL, args := sub.Scope.AreaSQL(p.ID, "area", authz.Read)
	rows, err := s.db().QueryContext(ctx, `SELECT installation FROM runners_links WHERE project = ? AND `+areaSQL+` ORDER BY installation`,
		append([]any{p.ID}, args...)...)
	if err != nil {
		return nil, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	out := []Link{}
	for _, id := range ids {
		if l, err := s.link(ctx, id); err == nil {
			out = append(out, l.Link)
		}
	}
	return map[string]any{"links": out}, nil
}

// unlink is DELETE /v0/runners/links/{installation}.
func (s *Service) unlink(r *http.Request) (any, error) {
	ctx := r.Context()
	id, _ := strconv.ParseInt(r.PathValue("installation"), 10, 64)
	l, err := s.link(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, api.NotFound("link", r.PathValue("installation"))
	}
	if err != nil {
		return nil, err
	}
	if err := core.SubjectOf(ctx).Check(authz.Admin, authz.Object{Project: l.project, Area: l.Area}, "link", r.PathValue("installation")); err != nil {
		return nil, err
	}
	return map[string]any{"unlinked": true}, s.dropLink(ctx, l)
}

func (s *Service) dropLink(ctx context.Context, l link) error {
	if _, err := s.db().ExecContext(ctx, `DELETE FROM runners_links WHERE installation = ?`, l.Installation); err != nil {
		return err
	}
	if err := s.dropRepos(ctx, l.Installation); err != nil {
		return err
	}
	_, _ = s.d.Projects.RevokeToken(ctx, l.tokenID)
	s.dropGrant(ctx, l.project, l.Area)
	return nil
}

// dropGrant revokes zm-runners' grant on an area no link uses any more.
func (s *Service) dropGrant(ctx context.Context, project, area string) {
	var others int
	if err := s.db().QueryRowContext(ctx, `SELECT count(*) FROM runners_links WHERE project = ? AND area = ?`, project, area).Scan(&others); err != nil {
		s.logf("runners: grant of %s in area %q kept: %v", Identity, area, err)
		return
	}
	if others == 0 {
		if _, err := s.d.Projects.Revoke(ctx, s.d.System(), api.GrantRequest{Identity: Identity, Role: api.RoleMember, Project: project, Area: area}); err != nil {
			s.logf("runners: grant of %s in area %q kept: %v", Identity, area, err)
		}
	}
}

// confirmPage asks the person to say yes, by a POST back with state.
func confirmPage(w http.ResponseWriter, title, body, state string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; form-action 'self'; style-src 'unsafe-inline'")
	w.Header().Set("X-Frame-Options", "DENY")
	fmt.Fprintf(w, `<!doctype html><meta charset="utf-8"><meta name="viewport" content="width=device-width">
<title>%s</title><body style="font:16px system-ui;max-width:36rem;margin:3rem auto;padding:0 1rem"><h1>%s</h1><p>%s</p>
<form method="post" action="/v0/runners/oauth"><input type="hidden" name="state" value="%s"><button type="submit">Yes, link it</button></form>`,
		html.EscapeString(title), html.EscapeString(title), html.EscapeString(body), html.EscapeString(state))
}

// page answers a person's browser: plain HTML, escaped, nothing scoped.
func page(w http.ResponseWriter, code int, title, body string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'")
	w.Header().Set("X-Frame-Options", "DENY")
	w.WriteHeader(code)
	fmt.Fprintf(w, `<!doctype html><meta charset="utf-8"><meta name="viewport" content="width=device-width">
<title>%s</title><body style="font:16px system-ui;max-width:36rem;margin:3rem auto;padding:0 1rem"><h1>%s</h1><p>%s</p>`,
		html.EscapeString(title), html.EscapeString(title), html.EscapeString(body))
}
