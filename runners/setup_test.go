package runners_test

import (
	"context"
	"encoding/json"
	"errors"
	"html"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/runners"
)

// Finishing App creation writes the App into config.json as it is on disk:
// what was edited there since the daemon started (another block, or the
// runners block itself) survives.
func TestAppCreatedKeepsEditsMadeSinceStart(t *testing.T) {
	f := newFixture(t)
	path := filepath.Join(f.home, "config.json")
	raw, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	doc := map[string]any{}
	_ = json.Unmarshal(raw, &doc)
	// The daemon's own config with two edits made after it started.
	b, _ := json.Marshal(f.dm.Deps.Config.File)
	_ = json.Unmarshal(b, &doc)
	doc["web"] = map[string]any{"vapid_subject": "https://example.com/zm"}
	r := doc["runners"].(map[string]any)
	r["allow_fork_prs"] = true
	out, _ := json.Marshal(doc)
	if err := os.WriteFile(path, out, 0o600); err != nil {
		t.Fatal(err)
	}

	var start struct{ URL string }
	if err := f.owner.Do(context.Background(), "POST", "/v0/runners/app", map[string]any{}, &start); err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(start.URL)
	if w := f.public(t, "GET", "/v0/runners/app/created?state="+u.Query().Get("state")+"&code=mcode", nil, nil); w.Code != 200 {
		t.Fatalf("created: %d %s", w.Code, w.Body)
	}
	raw, _ = os.ReadFile(path)
	var got struct {
		Web struct {
			VAPIDSubject string `json:"vapid_subject"`
		} `json:"web"`
		Runners struct {
			AllowForkPRs bool `json:"allow_fork_prs"`
			GitHub       struct {
				AppID int64 `json:"app_id"`
			} `json:"github"`
		} `json:"runners"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.Runners.GitHub.AppID != 99 {
		t.Fatalf("the new App is not in config.json: %s", raw)
	}
	if got.Web.VAPIDSubject != "https://example.com/zm" || !got.Runners.AllowForkPRs {
		t.Errorf("edits made since the daemon started were overwritten: %s", raw)
	}
}

// get calls the broker's own listener (what public_url's proxy reaches).
func get(t *testing.T, base, path string) *http.Response {
	t.Helper()
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.Get(base + path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func body(t *testing.T, resp *http.Response) string {
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

var hrefRe = regexp.MustCompile(`href="([^"]+)"`)

// From nothing to a linked org with zm's setup route and browser steps only:
// no config.json edit, no daemon restart, no installation id copied.
func TestSetupFromNothingToALinkedOrg(t *testing.T) {
	f := newFixtureWith(t, func(r *config.Runners) {
		*r = config.Runners{GitHub: config.RunnersGitHub{APIURL: r.GitHub.APIURL, WebURL: r.GitHub.WebURL}}
	})
	ctx := context.Background()
	var st runners.Setup
	err := f.owner.Do(ctx, "POST", "/v0/runners/setup", runners.SetupRequest{Pool: "pool"}, &st)
	if !api.IsCode(err, api.CodeBadRequest) || !strings.Contains(err.Error(), "public URL") {
		t.Fatalf("setup with no tunnel and no --url: %v", err)
	}
	if err := f.owner.Do(ctx, "POST", "/v0/runners/setup", runners.SetupRequest{Pool: "nope", PublicURL: "https://pool.example"}, nil); !api.IsCode(err, api.CodeNotFound) {
		t.Errorf("setup of a missing pool: %v, want not_found", err)
	}
	if err := f.owner.Do(ctx, "POST", "/v0/runners/setup", runners.SetupRequest{Pool: "pool", PublicURL: "http://pool.example"}, nil); !api.IsCode(err, api.CodeBadRequest) {
		t.Errorf("setup with a plain-http url: %v, want bad_request", err)
	}
	if err := f.owner.Do(ctx, "POST", "/v0/runners/setup", runners.SetupRequest{Pool: "pool", PublicURL: "https://pool.example/zm"}, nil); !api.IsCode(err, api.CodeBadRequest) {
		t.Errorf("setup with a url path: %v, want bad_request", err)
	}
	// A refusal changes nothing: no listener, nothing in config.json.
	if err := f.owner.Do(ctx, "POST", "/v0/runners/setup", runners.SetupRequest{Pool: "pool", PublicURL: "https://pool.example", Org: "not an org"}, nil); !api.IsCode(err, api.CodeBadRequest) {
		t.Errorf("setup with a bad org: %v, want bad_request", err)
	}
	if err := f.owner.Do(ctx, "GET", "/v0/runners/setup", nil, &st); err != nil || st.Listening != "" || st.Pool != "" {
		t.Fatalf("a refused setup changed things: %+v %v", st, err)
	}
	if err := f.owner.Do(ctx, "POST", "/v0/runners/setup", runners.SetupRequest{Pool: "pool", PublicURL: "https://pool.example/"}, &st); err != nil {
		t.Fatal(err)
	}
	if st.Next != "create_app" || !strings.HasPrefix(st.URL, "https://pool.example/v0/runners/app/new?state=") ||
		st.Listen == "" || st.Listening != st.Listen || st.PublicURL != "https://pool.example" || st.Pool != "pool" {
		t.Fatalf("setup: %+v", st)
	}
	raw, _ := os.ReadFile(filepath.Join(f.home, "config.json"))
	var disk struct{ Runners config.Runners }
	_ = json.Unmarshal(raw, &disk)
	if disk.Runners.Listen != st.Listen || disk.Runners.Pool.Project != "pool" || disk.Runners.PublicURL != "https://pool.example" {
		t.Fatalf("config.json runners: %s", raw)
	}

	// The listener is live: the manifest page names setup_url.
	base := "http://" + st.Listen
	u, _ := url.Parse(st.URL)
	resp := get(t, base, "/v0/runners/app/new?"+u.RawQuery)
	if b := body(t, resp); resp.StatusCode != 200 || !strings.Contains(b, "https://pool.example/v0/runners/app/installed") {
		t.Fatalf("manifest page: %d %s", resp.StatusCode, b)
	}
	resp = get(t, base, "/v0/runners/app/created?"+u.RawQuery+"&code=mcode")
	b := body(t, resp)
	m := hrefRe.FindStringSubmatch(b)
	if resp.StatusCode != 200 || m == nil {
		t.Fatalf("created: %d %s", resp.StatusCode, b)
	}
	install, _ := url.Parse(html.UnescapeString(m[1]))
	if install.Path != "/apps/z3-compute/installations/new" || len(install.Query().Get("state")) != 48 {
		t.Fatalf("install link %s", install)
	}
	state := install.Query().Get("state")

	// setup_url decides nothing alone: a made-up or missing state, HEAD, and
	// another App's installation are refused; no installation_id keeps the state.
	for _, c := range []struct {
		q    string
		want int
	}{
		{"installation_id=7&setup_action=install", http.StatusNotFound},
		{"installation_id=7&setup_action=install&state=" + strings.Repeat("a", 48), http.StatusNotFound},
		{"setup_action=request&state=" + state, http.StatusBadRequest}, // the state stays for the real return
		{"installation_id=-1&state=" + state, http.StatusBadRequest},
		{"installation_id=7&state=" + strings.Repeat("a", 47), http.StatusNotFound},
	} {
		if resp := get(t, base, "/v0/runners/app/installed?"+c.q); resp.StatusCode != c.want {
			t.Errorf("installed?%s: %d, want %d", c.q, resp.StatusCode, c.want)
		}
	}
	req, _ := http.NewRequest("HEAD", base+"/v0/runners/app/installed?installation_id=7&state="+state, nil)
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("HEAD installed: %v %v", resp, err)
	}

	// GitHub's return after the install: on to the sign-in, then the yes.
	resp = get(t, base, "/v0/runners/app/installed?installation_id=7&setup_action=install&state="+state)
	loc, _ := url.Parse(resp.Header.Get("Location"))
	if resp.StatusCode != http.StatusSeeOther || loc.Path != "/login/oauth/authorize" || !strings.HasPrefix(loc.String(), f.gh.srv.URL) ||
		loc.Query().Get("client_id") != "Iv1.new" || loc.Query().Get("redirect_uri") != "https://pool.example/v0/runners/oauth" {
		t.Fatalf("installed: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	if resp := get(t, base, "/v0/runners/app/installed?installation_id=7&setup_action=install&state="+state); resp.StatusCode != http.StatusNotFound {
		t.Errorf("the same install state twice: %d, want 404", resp.StatusCode)
	}
	if w := f.linkAs(t, loc.String(), "good"); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "acme") {
		t.Fatalf("link: %d %s", w.Code, w.Body)
	}
	var links struct{ Links []runners.Link }
	if err := f.owner.Do(ctx, "GET", "/v0/runners/links", nil, &links); err != nil || len(links.Links) != 1 || links.Links[0].Installation != inst {
		t.Fatalf("links: %+v %v", links, err)
	}

	// A typed installation_id the signed-in person does not administer goes
	// nowhere: the sign-in's check decides, as for any link.
	var more struct {
		URL string `json:"install_url"`
	}
	if err := f.owner.Do(ctx, "POST", "/v0/runners/links", map[string]any{}, &more); err != nil {
		t.Fatal(err)
	}
	mu, _ := url.Parse(more.URL)
	f.gh.mu.Lock()
	f.gh.userSee = []int64{inst, 12}
	f.gh.mu.Unlock()
	resp = get(t, base, "/v0/runners/app/installed?installation_id=12&state="+mu.Query().Get("state"))
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("installed (12): %d", resp.StatusCode)
	}
	if w := f.linkAs(t, resp.Header.Get("Location"), "good"); w.Code != http.StatusForbidden {
		t.Fatalf("a link of someone else's installation through setup_url: %d %s", w.Code, w.Body)
	}
	if err := f.owner.Do(ctx, "POST", "/v0/runners/links", map[string]any{}, &more); err != nil {
		t.Fatal(err)
	}
	mu, _ = url.Parse(more.URL)
	if resp := get(t, base, "/v0/runners/app/installed?installation_id=9&state="+mu.Query().Get("state")); resp.StatusCode != http.StatusNotFound {
		t.Errorf("installed with another App's installation: %d, want 404", resp.StatusCode)
	}

	// A re-run keeps the listener and offers the install link.
	var again runners.Setup
	if err := f.owner.Do(ctx, "POST", "/v0/runners/setup", runners.SetupRequest{Pool: "pool"}, &again); err != nil {
		t.Fatal(err)
	}
	if again.Listen != st.Listen || again.Next != "install" || !strings.Contains(again.URL, "/apps/z3-compute/installations/new?state=") ||
		again.App == nil || again.App.ID != 99 || !again.App.Secrets {
		t.Fatalf("setup again: %+v", again)
	}
	if err := f.owner.Do(ctx, "POST", "/v0/runners/setup", runners.SetupRequest{Pool: "pool", PublicURL: "https://elsewhere.example"}, nil); !api.IsCode(err, api.CodeConflict) {
		t.Errorf("moving public_url under an App registered at the old one: %v, want conflict", err)
	}
	var status runners.Setup
	if err := f.owner.Do(ctx, "GET", "/v0/runners/setup", nil, &status); err != nil || status.Listening != st.Listen || status.App.Slug != "z3-compute" {
		t.Fatalf("status: %+v %v", status, err)
	}
	// Only the machine's owner on its socket.
	tok, err := f.owner.MintToken(ctx, api.MintTokenRequest{As: "bo", Project: "pool", Role: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	bo := client.WithHandler(f.dm.Handler)
	bo.Token, bo.Project, bo.ProjectFrom = tok.Secret, "pool", "flag"
	if err := bo.Do(ctx, "POST", "/v0/runners/setup", runners.SetupRequest{Pool: "pool", PublicURL: "https://evil.example"}, nil); !api.IsCode(err, api.CodeForbidden) {
		t.Errorf("setup with a project owner's token: %v, want forbidden", err)
	}
}

// browse follows a browser's redirects from start across the fake GitHub
// and the broker's public routes (public_url pool.example), answering the
// last page and the URL it came from.
func (f *fixture) browse(t *testing.T, start string) (*httptest.ResponseRecorder, *url.URL) {
	t.Helper()
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	for at, n := start, 0; n < 10; n++ {
		u, err := url.Parse(at)
		if err != nil {
			t.Fatal(err)
		}
		w := httptest.NewRecorder()
		if u.Host == "pool.example" {
			w = f.public(t, "GET", u.RequestURI(), nil, nil)
		} else {
			resp, err := c.Get(at)
			if err != nil {
				t.Fatal(err)
			}
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			w.Code = resp.StatusCode
			maps.Copy(w.Header(), resp.Header)
			w.Body.Write(b)
		}
		if loc := w.Header().Get("Location"); w.Code/100 == 3 && loc != "" {
			at = loc
			continue
		}
		return w, u
	}
	t.Fatalf("too many redirects from %s", start)
	return nil, nil
}

func (f *fixture) pending(t *testing.T) int {
	t.Helper()
	return f.svc.Pending(context.Background())
}

// Installing from zm's install link ends on the confirmation page whichever
// way GitHub returns: to setup_url, or (the App authorizing during
// installation, GitHub's "Install & Authorize") straight to the callback URL
// with its own code and the install link's state. GitHub's install-time code
// is never exchanged: the link goes on only through a sign-in zm started.
func TestInstallReturnsLandOnTheConfirmPage(t *testing.T) {
	for _, mode := range []string{"setup_url", "callback"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixtureWith(t, func(r *config.Runners) { r.GitHub.Slug = "z3-compute" })
			f.gh.mu.Lock()
			f.gh.installReturn = mode
			f.gh.mu.Unlock()
			var start struct {
				URL string `json:"install_url"`
			}
			if err := f.owner.Do(context.Background(), "POST", "/v0/runners/links", map[string]any{"area": ""}, &start); err != nil {
				t.Fatal(err)
			}
			w, at := f.browse(t, start.URL)
			m := regexp.MustCompile(`name="state" value="([0-9a-f]{48})"`).FindStringSubmatch(w.Body.String())
			if w.Code != http.StatusOK || m == nil || at.Path != "/v0/runners/oauth" || !strings.Contains(w.Body.String(), "human:judy asked") {
				t.Fatalf("after the install: %d at %s: %s", w.Code, at, w.Body)
			}
			// The install link is spent: following it again links nothing new.
			if w, _ := f.browse(t, start.URL); w.Code != http.StatusNotFound {
				t.Errorf("the install link again: %d %s", w.Code, w.Body)
			}
			c := f.public(t, "POST", "/v0/runners/oauth", []byte("state="+m[1]), map[string]string{"Content-Type": "application/x-www-form-urlencoded"})
			var links struct{ Links []runners.Link }
			if err := f.owner.Do(context.Background(), "GET", "/v0/runners/links", nil, &links); c.Code != 200 || err != nil || len(links.Links) != 1 {
				t.Fatalf("yes: %d %s; links %+v %v", c.Code, c.Body, links, err)
			}
		})
	}
}

// A callback carrying an install but no state of zm's links nothing and
// mints nothing: it says how to go on (zm runners link <id>, whose sign-in
// and yes decide). A made-up state, another App's installation and an
// install still waiting for an org admin's approval are refused alike, and
// the admin check still decides after a callback that carried the state.
func TestInstallCallbackWithoutAStateLinksNothing(t *testing.T) {
	f := newFixtureWith(t, func(r *config.Runners) { r.GitHub.Slug = "z3-compute" })
	f.gh.mu.Lock()
	f.gh.installReturn = "callback-no-state"
	f.gh.mu.Unlock()
	ctx := context.Background()
	var start struct {
		URL string `json:"install_url"`
	}
	if err := f.owner.Do(ctx, "POST", "/v0/runners/links", map[string]any{}, &start); err != nil {
		t.Fatal(err)
	}
	before := f.pending(t)
	w, at := f.browse(t, start.URL)
	if w.Code != http.StatusNotFound || at.Path != "/v0/runners/oauth" || !strings.Contains(w.Body.String(), "potluck runners link 7") {
		t.Fatalf("a stateless install callback: %d at %s: %s", w.Code, at, w.Body)
	}
	if n := f.pending(t); n != before {
		t.Errorf("a stateless callback changed the pending states: %d, was %d", n, before)
	}
	for _, q := range []string{
		"code=good&installation_id=7&setup_action=install&state=" + strings.Repeat("a", 48),
		"code=good&installation_id=7&setup_action=install&state=" + strings.Repeat("a", 47),
		"code=good&installation_id=x&setup_action=install",
		"code=good",
	} {
		if w := f.public(t, "GET", "/v0/runners/oauth?"+q, nil, nil); w.Code != http.StatusNotFound || w.Header().Get("Location") != "" {
			t.Errorf("oauth?%s: %d %s", q, w.Code, w.Header().Get("Location"))
		}
	}
	st, _ := url.Parse(start.URL)
	state := st.Query().Get("state")
	// Waiting for approval: no installation yet, the state stays.
	if w := f.public(t, "GET", "/v0/runners/oauth?code=good&setup_action=request&state="+state, nil, nil); w.Code != http.StatusBadRequest {
		t.Errorf("an install waiting for approval: %d %s", w.Code, w.Body)
	}
	if w := f.public(t, "GET", "/v0/runners/oauth?code=good&installation_id=9&setup_action=install&state="+state, nil, nil); w.Code != http.StatusNotFound {
		t.Errorf("another App's installation: %d %s", w.Code, w.Body)
	}
	// A real install state with a person who is not acme's admin: the
	// sign-in it goes on to refuses, as for any link.
	if err := f.owner.Do(ctx, "POST", "/v0/runners/links", map[string]any{}, &start); err != nil {
		t.Fatal(err)
	}
	st, _ = url.Parse(start.URL)
	f.gh.mu.Lock()
	f.gh.orgRole = "member"
	f.gh.mu.Unlock()
	w = f.public(t, "GET", "/v0/runners/oauth?code=good&installation_id=7&setup_action=install&state="+st.Query().Get("state"), nil, nil)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("install callback with its state: %d %s", w.Code, w.Body)
	}
	if w := f.linkAs(t, w.Header().Get("Location"), "good"); w.Code != http.StatusForbidden {
		t.Fatalf("a non-admin through the install callback: %d %s", w.Code, w.Body)
	}
	var links struct{ Links []runners.Link }
	if err := f.owner.Do(ctx, "GET", "/v0/runners/links", nil, &links); err != nil || len(links.Links) != 0 {
		t.Fatalf("links: %+v %v", links, err)
	}
}

// The callback loaded twice (a reload, a prefetch) links at most once: the
// second load finds its state spent and the first page's yes still links.
func TestOAuthCallbackLoadedTwice(t *testing.T) {
	f := newFixture(t)
	var out struct {
		URL string `json:"authorize_url"`
	}
	if err := f.owner.Do(context.Background(), "POST", "/v0/runners/links", map[string]any{"installation_id": inst}, &out); err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(out.URL)
	cb := "/v0/runners/oauth?code=good&state=" + u.Query().Get("state")
	first := f.public(t, "GET", cb, nil, nil)
	if w := f.public(t, "GET", cb, nil, nil); w.Code != http.StatusNotFound {
		t.Errorf("second load: %d %s", w.Code, w.Body)
	}
	m := regexp.MustCompile(`name="state" value="([0-9a-f]{48})"`).FindStringSubmatch(first.Body.String())
	if first.Code != 200 || m == nil {
		t.Fatalf("first load: %d %s", first.Code, first.Body)
	}
	if w := f.public(t, "POST", "/v0/runners/oauth", []byte("state="+m[1]), map[string]string{"Content-Type": "application/x-www-form-urlencoded"}); w.Code != 200 {
		t.Fatalf("the first page's yes: %d %s", w.Code, w.Body)
	}
}

// With no tunnel and no --url, the hint names the program's own tunnel command when it has one (zm's
// tunnel setup), and only the commands it has when it has none (potluck's).
func TestSetupWithNoPublicURLNamesTheTunnelCommandTheProgramHas(t *testing.T) {
	for _, c := range []struct {
		program  string
		lacks    func(string) bool
		want     string
		wantNone string
	}{
		{"zm", nil, "run zm tunnel setup <hostname> first", ""},
		{"potluck", func(w string) bool { return w == "tunnel setup" }, "set tunnel.url in config.json", "tunnel setup"},
	} {
		t.Run(c.program, func(t *testing.T) {
			prog, lacks := config.Program, config.Lacks
			config.Program, config.Lacks = c.program, c.lacks
			t.Cleanup(func() { config.Program, config.Lacks = prog, lacks })
			f := newFixtureWith(t, func(r *config.Runners) {
				*r = config.Runners{GitHub: config.RunnersGitHub{APIURL: r.GitHub.APIURL, WebURL: r.GitHub.WebURL}}
			})
			f.svc.Command = c.program
			err := f.owner.Do(context.Background(), "POST", "/v0/runners/setup", runners.SetupRequest{Pool: "pool"}, nil)
			var ae *api.Error
			if !errors.As(err, &ae) || !strings.Contains(ae.Hint, c.want) || !strings.Contains(ae.Hint, c.program+" runners setup <pool> --url") ||
				(c.wantNone != "" && strings.Contains(ae.Hint, c.wantNone)) {
				t.Fatalf("hint: %v", err)
			}
		})
	}
}
