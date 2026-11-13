package runners

// Setup from zm commands (zm runners setup|link|ls): config.json's runners
// block written through the daemon, runners.listen opened live, and the App's
// setup_url, which starts a link from GitHub's return after an install.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"html"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/core"
)

// updateConf applies fn to config.json as it is on disk now (other modules
// write it, and a person may have edited it since the daemon started) and
// saves it; the daemon then serves the runners block that was saved. Nothing
// changes in memory when the save fails.
func (s *Service) updateConf(fn func(*config.File) error) error {
	s.cfgMu.Lock()
	defer s.cfgMu.Unlock()
	fresh, err := config.LoadFrom(s.d.Config.Home)
	if err != nil {
		return err
	}
	if err := fn(&fresh.File); err != nil {
		return err
	}
	if err := fresh.Save(); err != nil {
		return err
	}
	// The cached App client is keyed by its config: a new App rebuilds it.
	was := s.d.Config.File.Runners.AllowForkPRs
	s.d.Config.File.Runners = fresh.File.Runners
	if was && !fresh.File.Runners.AllowForkPRs { // switched off by hand, served from this save on
		s.retireOpen(context.Background(), "", retireOff)
	}
	return nil
}

// SetupRequest is POST /v0/runners/setup.
type SetupRequest struct {
	Pool      string `json:"pool"`
	PublicURL string `json:"public_url,omitempty"`
	Org       string `json:"org,omitempty"`
	Name      string `json:"name,omitempty"`
	Area      string `json:"area,omitempty"`
	// Repos (owner/repo, all of one account) are pre-selected on GitHub's install page: the install URL names that
	// account and these repositories' ids, so the person confirms instead of finding them.
	Repos []string `json:"repos,omitempty"`
}

// Setup is the broker's setup as GET /v0/runners/setup answers it, and
// POST's answer with the next step: Next is "create_app" (open URL to make
// the App) or "install" (open URL to install it and link).
type Setup struct {
	Pool        string            `json:"pool,omitempty"`
	PublicURL   string            `json:"public_url,omitempty"`
	Listen      string            `json:"listen,omitempty"`
	Listening   string            `json:"listening,omitempty"` // the address served now ("" none)
	Label       string            `json:"label"`
	JobTTLS     int               `json:"job_ttl_s"` // as a job gets it (the default when unset); with Label, what potluck runners set changes
	Sizes       map[string]string `json:"sizes,omitempty"`
	DefaultSize string            `json:"default_size,omitempty"`
	App         *SetupApp         `json:"app,omitempty"`
	SetupURL    string            `json:"setup_url,omitempty"` // the App's Setup URL on GitHub (an App made before it existed needs it set)
	Next        string            `json:"next,omitempty"`
	URL         string            `json:"url,omitempty"`
	ExpiresAt   string            `json:"expires_at,omitempty"`
	Check       *Checking         `json:"check,omitempty"` // how jobs arrive, and the checker's last round per installation (once there is an App)
	// Notices are things the pool's owner should know about config.json, in
	// plain words (a key that no longer does anything).
	Notices []string `json:"notices,omitempty"`
}

// SetupApp is the App as config.json names it (secret references, never
// values, and only whether each is set).
type SetupApp struct {
	ID         int64  `json:"id"`
	Slug       string `json:"slug,omitempty"`
	ClientID   string `json:"client_id,omitempty"`
	Secrets    bool   `json:"secrets"` // private key, client and webhook secrets all named
	InstallURL string `json:"install_url,omitempty"`
}

// setup is POST /v0/runners/setup (local): the pool's owner on this
// machine's socket turns the broker on.
func (s *Service) setup(r *http.Request) (any, error) {
	ctx := r.Context()
	var req SetupRequest
	if err := api.DecodeJSON(r, &req); err != nil {
		return nil, err
	}
	if req.Pool == "" {
		return nil, api.Errorf(api.CodeBadRequest, "pool is required (the pool project's slug)")
	}
	p, ok := s.d.Projects.Lookup(req.Pool)
	sub := core.SubjectOf(ctx)
	if !ok {
		return nil, api.NotFound("project", req.Pool)
	}
	if err := sub.Check(authz.Read, authz.Object{Project: p.ID}, "project", req.Pool); err != nil {
		return nil, err
	}
	if sub.Scope.Role(p.ID) != authz.Owner {
		return nil, api.Errorf(api.CodeForbidden, "setting up the runner broker takes an owner of %s", p.Slug)
	}
	if err := sub.Check(authz.Admin, authz.Object{Project: p.ID, Area: req.Area}, "area", req.Area); err != nil {
		return nil, err
	}
	var elsewhere int
	if err := s.db().QueryRowContext(ctx, `SELECT count(*) FROM runners_links WHERE project != ?`, p.ID).Scan(&elsewhere); err != nil {
		return nil, err
	}
	if elsewhere > 0 {
		return nil, api.Errorf(api.CodeConflict, "%d installation(s) are linked to another pool project; unlink them first (%s)", elsewhere, s.cmd("runners unlink"))
	}
	if req.PublicURL != "" {
		req.PublicURL = strings.TrimRight(req.PublicURL, "/")
		if err := s.checkPublicURL(req.PublicURL); err != nil {
			return nil, api.Errorf(api.CodeBadRequest, "--url %q: an https URL (https://<host>)", req.PublicURL)
		}
	}
	// Everything checked before anything changes: a refused setup leaves
	// config.json and the listener as they were.
	pre, err := s.preselect(ctx, req.Repos)
	if err != nil {
		return nil, err
	}
	if pre != nil && s.conf().GitHub.AppID == 0 {
		// A private App installs only on the account that owns it: a link to another account is a dead end.
		switch {
		case req.Org != "" && !strings.EqualFold(pre.Owner, req.Org):
			return nil, api.Errorf(api.CodeBadRequest, "repos: %s belongs to %s, but the App is made under --org %s and installs only there", req.Repos[0], pre.Owner, req.Org)
		case req.Org == "" && pre.Org:
			return nil, api.Errorf(api.CodeBadRequest, "repos: %s belongs to the organization %s: pass --org %s, so the App is made there", req.Repos[0], pre.Owner, pre.Owner)
		}
	}
	if s.conf().GitHub.AppID == 0 {
		if _, err := appName(req.Org, req.Name); err != nil {
			return nil, err
		}
	} else if c := s.conf().GitHub; c.Slug == "" || c.ClientID == "" {
		return nil, api.Errorf(api.CodeUnavailable, "config.json runners.github names App %d without its slug and client id", c.AppID)
	}

	// The listener first: config.json names only an address being served.
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ctx == nil || s.ctx.Err() != nil {
		return nil, api.Errorf(api.CodeUnavailable, "the runner broker is not running")
	}
	cur := s.conf().Listen
	if cur == "" {
		if fresh, err := config.LoadFrom(s.d.Config.Home); err == nil {
			cur = fresh.File.Runners.Listen
		}
	}
	addr := cur
	var ln net.Listener
	if s.public == nil || s.public.addr != cur {
		var err error
		ln, err = openListen(cur)
		if err != nil { // none set, or taken: any free one
			ln, err = openListen("127.0.0.1:0")
		}
		if err != nil {
			return nil, err
		}
		addr = ln.Addr().String()
	}
	err = s.updateConf(func(f *config.File) error {
		rc := &f.Runners
		rc.Listen = addr
		switch {
		case req.PublicURL != "" && rc.GitHub.AppID != 0 && rc.PublicURL != "" && req.PublicURL != strings.TrimRight(rc.PublicURL, "/"):
			// The App's webhook and callbacks were registered at this URL.
			return api.Errorf(api.CodeConflict, "the App %s calls %s, not %s", rc.GitHub.Slug, rc.PublicURL, req.PublicURL).
				WithHint("change the App's URLs on GitHub (Settings > Developer settings > GitHub Apps) and runners.public_url together, or drop --url")
		case req.PublicURL != "":
			rc.PublicURL = req.PublicURL
		case rc.PublicURL != "":
		case f.Tunnel.URL != "":
			rc.PublicURL = strings.TrimRight(f.Tunnel.URL, "/")
		default:
			hint := "set tunnel.url in config.json, or run " + s.cmd("runners setup <pool> --url https://<host>") + " for your own proxy to " + addr
			if config.Has("tunnel setup") {
				hint = "run " + s.cmd("tunnel setup <hostname>") + " first, or " + hint
			}
			return api.Errorf(api.CodeBadRequest, "no public URL: GitHub must reach this machine").WithHint(hint)
		}
		if err := s.checkPublicURL(rc.PublicURL); err != nil {
			return err
		}
		if old := rc.Pool.Project; old != "" && old != p.Slug {
			// States minted for the old pool's areas never land in this one.
			if _, err := s.db().ExecContext(ctx, `DELETE FROM runners_pending`); err != nil {
				return err
			}
		}
		rc.Pool.Project = p.Slug
		return nil
	})
	if err != nil {
		if ln != nil {
			ln.Close()
		}
		return nil, err
	}
	if ln != nil {
		s.servePublic(s.ctx, ln) // replaces (and stops) the one served before
	}
	out := s.status()
	if out.App == nil {
		u, exp, err := s.newApp(ctx, sub, p, req.Org, req.Name, req.Area, pre)
		if err != nil {
			return nil, err
		}
		out.Next, out.URL, out.ExpiresAt = "create_app", u, exp.Format(time.RFC3339)
		return out, nil
	}
	u, exp, err := s.newInstall(ctx, req.Area, sub.Address, pre)
	if err != nil {
		return nil, err
	}
	out.Next, out.URL, out.ExpiresAt = "install", u, exp.Format(time.RFC3339)
	return out, nil
}

// setupStatus is GET /v0/runners/setup (local).
func (s *Service) setupStatus(r *http.Request) (any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status(), nil
}

// status describes the setup. Callers hold s.mu.
func (s *Service) status() Setup {
	c := s.conf()
	out := Setup{Pool: c.Pool.Project, PublicURL: c.PublicURL, Listen: c.Listen, Label: c.Label, Sizes: c.Pool.Sizes, DefaultSize: c.Pool.DefaultSize,
		JobTTLS: s.jobTTL(), Notices: s.notices()}
	if s.public != nil {
		out.Listening = s.public.addr
	}
	if c.PublicURL != "" {
		out.SetupURL = c.PublicURL + "/v0/runners/app/installed"
	}
	if gh := c.GitHub; gh.AppID != 0 {
		out.App = &SetupApp{ID: gh.AppID, Slug: gh.Slug, ClientID: gh.ClientID,
			Secrets: gh.PrivateKey != "" && gh.ClientSecret != "" && gh.WebhookSecret != ""}
		if gh.Slug != "" {
			out.App.InstallURL = gh.WebURL + "/apps/" + gh.Slug + "/installations/new"
		}
		if ch, err := s.Checked(context.Background()); err == nil {
			out.Check = &ch
		}
	}
	return out
}

// SetRequest is POST /v0/runners/settings: one setting and its new value.
type SetRequest struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// Settings a pool's owner changes with potluck runners set, by their
// config.json names.
const (
	setLabel  = "label"
	setJobTTL = "job_ttl_s"

	labelDefault = "default" // set label default: back to the pool project's slug

	jobTTLMax = 5 * 24 * 3600 // GitHub ends a self-hosted job after 5 days
)

var labelRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)

// set is POST /v0/runners/settings (local): the pool's owner changes one
// setting. It is written into config.json as it is on disk and served from
// then on: conf is read on every use, so the next job gets it (no restart).
func (s *Service) set(r *http.Request) (any, error) {
	var req SetRequest
	if err := api.DecodeJSON(r, &req); err != nil {
		return nil, err
	}
	if _, _, err := s.poolOwner(r, "changing the runner broker's settings"); err != nil {
		return nil, err
	}
	var apply func(*config.Runners)
	switch v := strings.TrimSpace(req.Value); req.Key {
	case setLabel:
		l := strings.ToLower(v)
		if l == labelDefault {
			apply = func(c *config.Runners) { c.Label = "" }
			break
		}
		if !labelRe.MatchString(v) || l == "self-hosted" || slices.Contains(osLabels, l) || slices.Contains(archLabels, l) {
			return nil, api.Errorf(api.CodeBadRequest, "%s %q: a name of letters, digits, '.', '_' and '-' that is not one of GitHub's own labels (jobs ask runs-on: <label>), or %s for the pool's own name", setLabel, req.Value, labelDefault)
		}
		apply = func(c *config.Runners) { c.Label = v }
	case setJobTTL:
		n, err := strconv.Atoi(v)
		if err != nil || n < s.jobTTLMin() || n > jobTTLMax {
			return nil, api.Errorf(api.CodeBadRequest, "%s %q: whole seconds from %d to %d (5400 is 90 minutes)", setJobTTL, req.Value, s.jobTTLMin(), jobTTLMax)
		}
		apply = func(c *config.Runners) { c.JobTTLS = n }
	case "allow_public_repos":
		return nil, api.Errorf(api.CodeBadRequest, "no setting %q: a public repository is switched on by itself", req.Key).
			WithHint(s.cmd("runners allow <owner/repo> --public"))
	default:
		return nil, api.Errorf(api.CodeBadRequest, "no setting %q: %s or %s", req.Key, setLabel, setJobTTL)
	}
	if err := s.updateConf(func(f *config.File) error { apply(&f.Runners); return nil }); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status(), nil
}

// newInstall mints an install URL: installing the App from it returns to
// setup_url with the state, which starts the link into area as by asked. With
// pre the URL is GitHub's install page for pre's account with pre's
// repositories chosen (the same state rides along); without, the plain page.
func (s *Service) newInstall(ctx context.Context, area, by string, pre *Preselect) (string, time.Time, error) {
	c := s.conf()
	if c.GitHub.Slug == "" || c.GitHub.ClientID == "" {
		return "", time.Time{}, s.off()
	}
	state, exp := newState(), s.now().Add(installTTL)
	if _, err := s.db().ExecContext(ctx, `INSERT INTO runners_pending (state, kind, area, by, expires) VALUES (?, ?, ?, ?, ?)`,
		state, pendingInstall, area, by, exp.Unix()); err != nil {
		return "", time.Time{}, err
	}
	base := c.GitHub.WebURL + "/apps/" + url.PathEscape(c.GitHub.Slug) + "/installations/new"
	if pre == nil {
		return base + "?state=" + state, exp, nil
	}
	q := url.Values{"state": {state}}
	pre.query(q)
	return base + "/permissions?" + q.Encode(), exp, nil
}

// installed is GET /v0/runners/app/installed, the App's setup_url: GitHub's
// return after an install, with installation_id and the install URL's
// state (also GET /v0/runners/oauth's, for an App that authorizes during
// installation). Anyone can call it with any number, so it decides nothing: it
// spends the state (bound to the zm person who asked and their area) and
// hands over to the link flow, whose GitHub sign-in proves the person
// administers that installation's account and whose page asks their yes.
func (s *Service) installed(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()
	expired := func() {
		link := s.cmd("runners link")
		if id, err := strconv.ParseInt(q.Get("installation_id"), 10, 64); err == nil && id > 0 {
			link += " " + itoa(id) // GitHub's claim, only echoed: that link's sign-in decides
		}
		page(w, http.StatusNotFound, "This link expired or was used",
			"If the App is installed, link it: run "+link+" on the pool's machine and open the URL it prints.")
	}
	var live int
	if st := q.Get("state"); len(st) != 48 || s.db().QueryRowContext(ctx, `SELECT count(*) FROM runners_pending WHERE state = ? AND kind = ? AND expires > ?`,
		st, pendingInstall, s.now().Unix()).Scan(&live) != nil || live == 0 {
		expired()
		return
	}
	id, err := strconv.ParseInt(q.Get("installation_id"), 10, 64)
	if err != nil || id <= 0 {
		// The state stays: an install an org admin has still to approve
		// (setup_action=request) comes back without an installation.
		page(w, http.StatusBadRequest, "Nothing installed yet",
			"GitHub sent no installation (an org admin may still have to approve the request). Once the App is installed, open the install link again.")
		return
	}
	c := s.conf()
	cl, err := s.client(ctx)
	if err != nil || c.GitHub.ClientID == "" || c.GitHub.ClientSecret == "" || c.PublicURL == "" {
		page(w, http.StatusServiceUnavailable, "The runner broker is not set up", s.cmd("runners setup <pool>")+" on the pool's machine.")
		return
	}
	p, err := s.takeState(ctx, q.Get("state"), pendingInstall)
	if err != nil {
		expired()
		return
	}
	if _, err := cl.Installation(ctx, id); err != nil {
		page(w, http.StatusNotFound, "Not linked", fmt.Sprintf("Installation %d is not one of this App's.", id))
		return
	}
	// An installation already serving another area moves only through
	// zm runners link by an admin of both (linkStart).
	old, err := s.link(ctx, id)
	switch {
	case err == nil && old.Area != p.area:
		page(w, http.StatusConflict, "Not linked", fmt.Sprintf("Installation %d is linked elsewhere in the pool; its admin unlinks it first.", id))
		return
	case err != nil && !errors.Is(err, sql.ErrNoRows):
		page(w, http.StatusInternalServerError, "Not linked", err.Error())
		return
	}
	state, exp := newState(), s.now().Add(stateTTL)
	if _, err := s.db().ExecContext(ctx, `INSERT INTO runners_pending (state, kind, installation, area, by, expires) VALUES (?, ?, ?, ?, ?, ?)`,
		state, pendingLink, id, p.area, p.by, exp.Unix()); err != nil {
		page(w, http.StatusInternalServerError, "Not linked", err.Error())
		return
	}
	qv := url.Values{"client_id": {c.GitHub.ClientID}, "state": {state}, "redirect_uri": {c.PublicURL + "/v0/runners/oauth"}}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	http.Redirect(w, r, c.GitHub.WebURL+"/login/oauth/authorize?"+qv.Encode(), http.StatusSeeOther)
}

// linkPage is page with one link onward (to GitHub).
func linkPage(w http.ResponseWriter, title, body, href, label string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Cache-Control", "no-store")
	fmt.Fprintf(w, `<!doctype html><meta charset="utf-8"><meta name="viewport" content="width=device-width">
<title>%s</title><body style="font:16px system-ui;max-width:36rem;margin:3rem auto;padding:0 1rem"><h1>%s</h1><p>%s</p>
<p><a href="%s">%s</a></p>`,
		html.EscapeString(title), html.EscapeString(title), html.EscapeString(body), html.EscapeString(href), html.EscapeString(label))
}
