package runners

import (
	"context"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"time"

	"github.com/sengirunners/potluck/ghapp"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/core"
)

var orgRe = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,38})$`)

// appStart is POST /v0/runners/app: the pool's owner asks for the one-time
// page that creates the App from its manifest.
func (s *Service) appStart(r *http.Request) (any, error) {
	ctx := r.Context()
	var req struct {
		Org  string `json:"org"`
		Name string `json:"name"`
		Area string `json:"area"`
	}
	if err := api.DecodeJSON(r, &req); err != nil {
		return nil, err
	}
	p, err := s.pool()
	if err != nil {
		return nil, err
	}
	u, exp, err := s.newApp(ctx, core.SubjectOf(ctx), p, req.Org, req.Name, req.Area, nil)
	if err != nil {
		return nil, err
	}
	return map[string]any{"url": u, "expires_at": exp.Format(time.RFC3339)}, nil
}

// newApp mints the App creation page's state for an owner of pool p. area
// is where the first installation links once GitHub returns from installing.
func (s *Service) newApp(ctx context.Context, sub *authz.Subject, p api.ProjectRef, org, name, area string, pre *Preselect) (string, time.Time, error) {
	if err := sub.Check(authz.Read, authz.Object{Project: p.ID}, "project", p.Slug); err != nil {
		return "", time.Time{}, err
	}
	if sub.Scope.Role(p.ID) != authz.Owner {
		return "", time.Time{}, api.Errorf(api.CodeForbidden, "creating the runner App takes an owner of %s", p.Slug)
	}
	name, err := appName(org, name)
	if err != nil {
		return "", time.Time{}, err
	}
	c := s.conf()
	if err := s.checkPublicURL(c.PublicURL); err != nil {
		return "", time.Time{}, err
	}
	state, exp := newState(), s.now().Add(appStateTTL)
	if _, err := s.db().ExecContext(ctx, `INSERT INTO runners_pending (state, kind, name, area, org, by, preselect, expires) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		state, pendingApp, name, area, org, sub.Address, pre.encode(), exp.Unix()); err != nil {
		return "", time.Time{}, err
	}
	return c.PublicURL + "/v0/runners/app/new?state=" + state, exp, nil
}

// appName checks an App's org and name, answering the name (default
// "Z3 Compute").
func appName(org, name string) (string, error) {
	if org != "" && !orgRe.MatchString(org) {
		return "", api.Errorf(api.CodeBadRequest, "org %q is not a GitHub account name", org)
	}
	if name == "" {
		name = "Z3 Compute"
	}
	if len(name) > 34 {
		return "", api.Errorf(api.CodeBadRequest, "a GitHub App name is at most 34 characters")
	}
	return name, nil
}

// checkPublicURL refuses a public_url GitHub cannot call back: https, a
// host, nothing else (the tunnel routes /v0/runners/... from the root).
func (s *Service) checkPublicURL(v string) error {
	if u, err := url.Parse(v); err != nil || u.Scheme != "https" || u.Host == "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil ||
		(u.Path != "" && u.Path != "/") {
		return api.Errorf(api.CodeUnavailable, "runners.public_url %q is not an https URL GitHub can reach", v).
			WithHint(s.cmd("runners setup <pool> --url https://<host>") + ": a host forwarding only /v0/runners/{hook,oauth,app/new,app/created,app/installed} to runners.listen")
	}
	return nil
}

// manifest is the App the broker needs (SPEC 20).
func (s *Service) manifest(name, org string) ghapp.Manifest {
	c := s.conf()
	return ghapp.Manifest{Name: name, URL: c.PublicURL, HookURL: c.PublicURL + "/v0/runners/hook",
		RedirectURL: c.PublicURL + "/v0/runners/app/created", CallbackURLs: []string{c.PublicURL + "/v0/runners/oauth"},
		SetupURL:    c.PublicURL + "/v0/runners/app/installed",
		Description: "Runs this team's GitHub Actions jobs in zm's throwaway VMs (SPEC 20 runner broker)."}
}

// appPage is GET /v0/runners/app/new: posts the manifest to GitHub. The
// state is only looked at here; GitHub's return consumes it.
func (s *Service) appPage(w http.ResponseWriter, r *http.Request) {
	state := r.URL.Query().Get("state")
	var name, area, org string
	err := s.db().QueryRowContext(r.Context(), `SELECT name, area, org FROM runners_pending WHERE state = ? AND kind = ? AND expires > ?`,
		state, pendingApp, s.now().Unix()).Scan(&name, &area, &org)
	if err != nil || len(state) != 48 {
		page(w, http.StatusNotFound, "This link expired or was used", "Ask for a new one: "+s.cmd("runners setup <pool>")+".")
		return
	}
	if name == "" { // a row from before 0003
		name = area
	}
	m, err := s.manifest(name, org).JSON()
	if err != nil {
		page(w, http.StatusInternalServerError, "No manifest", err.Error())
		return
	}
	action := s.conf().GitHub.WebURL + "/settings/apps/new"
	if org != "" {
		action = s.conf().GitHub.WebURL + "/organizations/" + org + "/settings/apps/new"
	}
	action += "?state=" + url.QueryEscape(state)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; form-action "+s.conf().GitHub.WebURL+"; style-src 'unsafe-inline'")
	fmt.Fprintf(w, `<!doctype html><meta charset="utf-8"><meta name="viewport" content="width=device-width"><title>Create the runner App</title>
<body style="font:16px system-ui;max-width:36rem;margin:3rem auto;padding:0 1rem"><h1>Create %s</h1>
<p>GitHub will show the App zm needs to run this team's Actions jobs in throwaway VMs: runner registration, read and write access to Actions
(write only to re-run a job whose box its lender took back), read access to org members (to check who may link an org), the workflow_job event. Its key and secrets come back to zm directly; nobody copies them.</p>
<form method="post" action="%s"><input type="hidden" name="manifest" value="%s"><button type="submit">Create on GitHub</button></form>`,
		html.EscapeString(name), html.EscapeString(action), html.EscapeString(string(m)))
}

// appCreated is GET /v0/runners/app/created: GitHub's return with the
// manifest code. The authority converts it and keeps the App's secrets.
func (s *Service) appCreated(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	p, err := s.takeState(ctx, r.URL.Query().Get("state"), pendingApp)
	if err != nil {
		page(w, http.StatusNotFound, "This link expired or was used", "Ask for a new one: "+s.cmd("runners setup <pool>")+".")
		return
	}
	c := s.conf()
	app, err := ghapp.Convert(ctx, s.HTTP, c.GitHub.APIURL, r.URL.Query().Get("code"))
	if err != nil {
		page(w, http.StatusBadGateway, "GitHub did not hand the App over", err.Error())
		return
	}
	// New files per App, written before config.json names them, so a failed
	// save leaves the old config pointing at the old App's files.
	dir := filepath.Join(s.d.Config.Home, "secrets", "runners")
	prefix := "app-" + itoa(app.ID) + "-"
	files := map[string]string{"key.pem": string(app.PEM), "client_secret": app.ClientSecret, "webhook_secret": app.WebhookSecret}
	for name, v := range files {
		if err := writeSecret(filepath.Join(dir, prefix+name), v); err != nil {
			page(w, http.StatusInternalServerError, "The App was made but not kept", err.Error())
			return
		}
	}
	err = s.updateConf(func(f *config.File) error {
		gh := &f.Runners.GitHub
		gh.AppID, gh.ClientID, gh.Slug = app.ID, app.ClientID, app.Slug
		gh.PrivateKey = "file:" + filepath.Join(dir, prefix+"key.pem")
		gh.ClientSecret = "file:" + filepath.Join(dir, prefix+"client_secret")
		gh.WebhookSecret = "file:" + filepath.Join(dir, prefix+"webhook_secret")
		return nil
	})
	if err != nil {
		page(w, http.StatusInternalServerError, "The App was made but config.json was not saved", err.Error())
		return
	}
	// Installing it comes back to setup_url with this state: the link
	// starts there, for the owner who asked for the App, into the area they
	// named, still behind the sign-in, admin check and yes of a link.
	install, _, err := s.newInstall(ctx, p.area, p.by, p.preselect)
	if err != nil {
		page(w, http.StatusOK, "Created "+app.Slug, "Install it on the org whose jobs should run on the pool, then link it: "+s.cmd("runners link")+".")
		return
	}
	linkPage(w, "Created "+app.Slug, "Next, install it on the org (or account) whose Actions jobs should run on the pool. "+
		"GitHub then brings you back here to sign in and say yes to the link.", install, "Install "+app.Slug+" on GitHub")
}
