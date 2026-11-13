package runners_test

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/runners"
)

// readRepos answers GET /repos/<owner>/<repo> as the machine owner's gh would, from a table of "owner/repo" -> id
// (the owner's id is 100 + the length of its login, so two accounts differ).
func readRepos(ids map[string]int64) runners.PersonRead {
	return func(_ context.Context, path string, out any) error {
		repo := strings.TrimPrefix(path, "/repos/")
		id, ok := ids[repo]
		if !ok {
			return fmt.Errorf("not found")
		}
		login := strings.SplitN(repo, "/", 2)[0]
		typ := "User"
		if login == "abcd" {
			typ = "Organization"
		}
		b, _ := json.Marshal(map[string]any{"id": id, "full_name": repo, "owner": map[string]any{"id": 100 + len(login), "login": login, "type": typ}})
		return json.Unmarshal(b, out)
	}
}

func freshFixture(t *testing.T) *fixture {
	return newFixtureWith(t, func(r *config.Runners) {
		*r = config.Runners{GitHub: config.RunnersGitHub{APIURL: r.GitHub.APIURL, WebURL: r.GitHub.WebURL}}
	})
}

// The install URL names the account and the repositories to tick, and keeps its state, both when the App is made
// by this setup (the choice rides through GitHub's creation return) and when the App exists already.
func TestSetupPreselectsReposOnTheInstallURL(t *testing.T) {
	f := freshFixture(t)
	f.svc.DoctorRead = readRepos(map[string]int64{"acme/app": 4001, "Acme/web": 4002})
	ctx := context.Background()
	var st runners.Setup
	req := runners.SetupRequest{Pool: "pool", PublicURL: "https://pool.example", Repos: []string{"acme/app", "Acme/web", "acme/app"}}
	if err := f.owner.Do(ctx, "POST", "/v0/runners/setup", req, &st); err != nil || st.Next != "create_app" {
		t.Fatalf("setup: %+v %v", st, err)
	}
	check := func(raw string) {
		t.Helper()
		u, err := url.Parse(html.UnescapeString(raw))
		if err != nil || u.Path != "/apps/z3-compute/installations/new/permissions" {
			t.Fatalf("install URL %q: %v", raw, err)
		}
		q := u.Query()
		if q.Get("target_id") != "104" || strings.Join(q["repository_ids[]"], ",") != "4001,4002" || len(q.Get("state")) != 48 {
			t.Errorf("install URL query %v", q)
		}
	}

	// The App is made: GitHub's return offers the install, with the repositories chosen.
	u, _ := url.Parse(st.URL)
	resp := get(t, "http://"+st.Listen, "/v0/runners/app/created?"+u.RawQuery+"&code=mcode")
	b := body(t, resp)
	m := hrefRe.FindStringSubmatch(b)
	if resp.StatusCode != 200 || m == nil {
		t.Fatalf("created: %d %s", resp.StatusCode, b)
	}
	check(m[1])

	// The App exists: setup again mints the install URL the same way, and that state is a real one.
	if err := f.owner.Do(ctx, "POST", "/v0/runners/setup", req, &st); err != nil || st.Next != "install" {
		t.Fatalf("setup again: %+v %v", st, err)
	}
	check(st.URL)
	iu, _ := url.Parse(st.URL)
	resp = get(t, "http://"+st.Listen, "/v0/runners/app/installed?installation_id=7&state="+iu.Query().Get("state"))
	if resp.StatusCode != http.StatusSeeOther {
		t.Errorf("the pre-selecting install URL's state was not accepted: %d", resp.StatusCode)
	}

	// With no repos the page is the plain one.
	if err := f.owner.Do(ctx, "POST", "/v0/runners/setup", runners.SetupRequest{Pool: "pool"}, &st); err != nil ||
		!strings.Contains(st.URL, "/apps/z3-compute/installations/new?state=") || strings.Contains(st.URL, "target_id") {
		t.Errorf("setup with no repos: %s %v", st.URL, err)
	}
}

// A repository it cannot name is refused, and a refusal changes nothing (no listener, no pool).
func TestSetupRefusesWhatItCannotPreselect(t *testing.T) {
	f := freshFixture(t)
	f.svc.DoctorRead = readRepos(map[string]int64{"acme/app": 4001, "other/app": 4003, "abcd/app": 4004, "solo/app": 4005})
	ctx := context.Background()
	for _, tt := range []struct {
		repos []string
		code  string
	}{
		{[]string{"acme/app", "other/app"}, api.CodeBadRequest}, // two accounts
		{[]string{"acme/app", "abcd/app"}, api.CodeBadRequest},  // two logins with one id (their lengths agree)
		{[]string{"acme"}, api.CodeBadRequest},
		{[]string{"acme/nope"}, api.CodeUnavailable}, // gh cannot read it
		{[]string{"acme/app/../x"}, api.CodeBadRequest},
		{twentyOne(), api.CodeBadRequest},
	} {
		err := f.owner.Do(ctx, "POST", "/v0/runners/setup", runners.SetupRequest{Pool: "pool", PublicURL: "https://pool.example", Repos: tt.repos}, nil)
		if !api.IsCode(err, tt.code) {
			t.Errorf("repos %v: %v, want %s", tt.repos, err, tt.code)
		}
	}
	if err := f.owner.Do(ctx, "POST", "/v0/runners/setup", runners.SetupRequest{Pool: "pool", PublicURL: "https://pool.example", Repos: []string{"acme/app"}, Org: "other"}, nil); !api.IsCode(err, api.CodeBadRequest) {
		t.Errorf("a repo of another account than --org: %v, want bad_request", err)
	}
	if err := f.owner.Do(ctx, "POST", "/v0/runners/setup", runners.SetupRequest{Pool: "pool", PublicURL: "https://pool.example", Repos: []string{"abcd/app"}}, nil); !api.IsCode(err, api.CodeBadRequest) || !strings.Contains(err.Error(), "--org abcd") {
		t.Errorf("an org's repo with no --org: %v, want bad_request naming --org abcd", err)
	}
	var st runners.Setup
	if err := f.owner.Do(ctx, "GET", "/v0/runners/setup", nil, &st); err != nil || st.Listening != "" || st.Pool != "" {
		t.Fatalf("a refused setup changed things: %+v %v", st, err)
	}
}

// twentyOne is one more repository than a setup may name, each a valid name.
func twentyOne() []string {
	var out []string
	for i := 0; i < 21; i++ {
		out = append(out, fmt.Sprintf("acme/r%d", i))
	}
	return out
}
