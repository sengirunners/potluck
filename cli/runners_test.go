package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/tailnet/tailnettest"
	"github.com/sengirunners/potluck/runners"
	"github.com/spf13/cobra"
)

// potluck runners set changes a broker setting on the running daemon, and
// potluck runners ls prints the settings: the job limit among them.
func TestRunnersSetAndLs(t *testing.T) {
	m := startMachine(t, tailnettest.New(), "mini", "judy@example.com")
	if _, err := client.NewUnix(m.dm.Deps.Config.SocketPath()).CreateProject(context.Background(), api.NewProjectRequest{Slug: "pool"}); err != nil {
		t.Fatal(err)
	}
	if _, e, code := m.run(t, "runners", "set", "job_ttl_s", "5400"); code == 0 || !strings.Contains(e, "not set up") {
		t.Fatalf("set before setup: exit %d, %s", code, e)
	}
	if out := m.ok(t, "runners", "setup", "pool", "--url", "https://pool.example"); !strings.Contains(out, "(listener ") || !strings.Contains(out, "live); forward that URL to the listener") {
		t.Fatalf("setup with no host route:\n%s", out) // potluck's own words, unchanged by the host hook
	}
	out := m.ok(t, "runners", "ls")
	if !strings.Contains(out, "job limit   3600 s") {
		t.Fatalf("runners ls before any change:\n%s", out)
	}
	out = m.ok(t, "runners", "set", "job_ttl_s", "5400")
	if !strings.Contains(out, "job limit   5400 s") {
		t.Errorf("runners set job_ttl_s 5400 printed:\n%s", out)
	}
	if _, e, code := m.run(t, "runners", "set", "allow_public_repos", "on"); code == 0 || !strings.Contains(e, "potluck runners allow <owner/repo> --public") {
		t.Errorf("set allow_public_repos: exit %d, %s", code, e)
	}
	if out = m.ok(t, "runners", "ls"); !strings.Contains(out, "machines    none listed (potluck host pool on a machine to lend it") {
		t.Errorf("runners ls with no machine lent:\n%s", out)
	}
	m.ok(t, "runners", "set", "label", "ci")
	out = m.ok(t, "runners", "ls")
	for _, want := range []string{"job limit   5400 s", "runs-on: [self-hosted, ci]"} {
		if !strings.Contains(out, want) {
			t.Errorf("runners ls lacks %q:\n%s", want, out)
		}
	}
	m.ok(t, "runners", "set", "label", "default")
	if out = m.ok(t, "runners", "ls"); !strings.Contains(out, "runs-on: [self-hosted, pool]") {
		t.Errorf("runners ls after label default:\n%s", out)
	}
	if _, e, code := m.run(t, "runners", "set", "job_ttl_s", "soon"); code == 0 || !strings.Contains(e, "whole seconds") {
		t.Errorf("a bad value: exit %d, %s", code, e)
	}
	if out = m.ok(t, "runners", "repos"); !strings.Contains(out, "repos       none connected: no job is served (potluck runners allow <owner/repo>)") {
		t.Errorf("runners repos with none:\n%s", out)
	}
	if out = m.ok(t, "runners", "ls"); !strings.Contains(out, "repos       none connected") {
		t.Errorf("runners ls lists no repositories:\n%s", out)
	}
	if _, e, code := m.run(t, "runners", "disallow", "acme"); code == 0 || !strings.Contains(e, "not owner/repo") {
		t.Errorf("disallow acme: exit %d, %s", code, e)
	}
	if _, e, code := m.run(t, "runners", "disallow", "acme/app"); code == 0 || !strings.Contains(e, `no connected repository "acme/app"`) {
		t.Errorf("disallow a repository not connected: exit %d, %s", code, e)
	}
}

func TestRunnersJobsPrintsSelectedLimit(t *testing.T) {
	m := startMachine(t, tailnettest.New(), "mini", "judy@example.com")
	cl := client.NewUnix(m.dm.Deps.Config.SocketPath())
	p, err := cl.CreateProject(context.Background(), api.NewProjectRequest{Slug: "pool"})
	if err != nil {
		t.Fatal(err)
	}
	m.ok(t, "runners", "setup", "pool", "--url", "https://pool.example")
	db := m.dm.Deps.Store.Registry()
	_, err = db.Exec(`INSERT INTO runners_links (installation,account,account_type,project,area,linked_by,token_id,created) VALUES (7,'acme','Organization',?,'','judy','test',1)`, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO runners_jobs (job,installation,repo,run,labels,state,why,created,updated,limit_s,limit_source,started_at,deadline) VALUES (9,7,'acme/app',8,'[]','done','stopped at its job time limit',1,2,6000,'workflow .github/workflows/ci.yml@abc job native',3,6003)`)
	if err != nil {
		t.Fatal(err)
	}
	out := m.ok(t, "runners", "jobs")
	for _, want := range []string{"LIMIT", "SOURCE", "6000s", "workflow .github/workflows/ci.yml@abc job native", "stopped at its job time limit"} {
		if !strings.Contains(out, want) {
			t.Fatalf("jobs output missing %q: %s", want, out)
		}
	}
	out = m.ok(t, "runners", "jobs", "--json")
	for _, want := range []string{`"limit_s": 6000`, `"limit_source": "workflow`, `"started_at": 3`, `"deadline": 6003`} {
		if !strings.Contains(out, want) {
			t.Fatalf("jobs JSON missing %q: %s", want, out)
		}
	}
}

// runners disallow sends the repository the person typed or nothing: a name
// that is not owner/repo fails before a request, and one the daemon holds
// stays connected (the '?' used to start a query, so the daemon deleted
// acme/app while the command reported acme/app?x).
func TestRunnersDisallowNamesWhatItDeletes(t *testing.T) {
	m := startMachine(t, tailnettest.New(), "mini", "judy@example.com")
	p, err := client.NewUnix(m.dm.Deps.Config.SocketPath()).CreateProject(context.Background(), api.NewProjectRequest{Slug: "pool"})
	if err != nil {
		t.Fatal(err)
	}
	m.ok(t, "runners", "setup", "pool", "--url", "https://pool.example")
	db := m.dm.Deps.Store.Registry()
	if _, err := db.Exec(`INSERT INTO runners_links (installation,account,account_type,project,area,linked_by,token_id,created) VALUES (7,'acme','Organization',?,'','judy','test',1)`, p.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO runners_repos (repo, repo_id, installation, by, created) VALUES ('acme/app', 5, 7, 'judy', 1)`); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"acme/app?x", "acme/app#x", "acme/app/x", "acme/app%3Fx", "acme/", "/app", "acme", "acme/a b", "acme/../app"} {
		if _, e, code := m.run(t, "runners", "disallow", bad); code == 0 || !strings.Contains(e, "not owner/repo") {
			t.Errorf("disallow %q: exit %d, %s", bad, code, e)
		}
	}
	if out := m.ok(t, "runners", "repos"); !strings.Contains(out, "acme/app") {
		t.Fatalf("a refused disallow disconnected acme/app:\n%s", out)
	}
	for _, bad := range []string{"0", "7?x", "+7", "07", "7/links"} {
		if _, e, code := m.run(t, "runners", "unlink", bad); code == 0 || !strings.Contains(e, "not an installation id") {
			t.Errorf("unlink %q: exit %d, %s", bad, code, e)
		}
	}
	m.ok(t, "runners", "disallow", "acme/app")
	if out := m.ok(t, "runners", "repos"); strings.Contains(out, "acme/app") {
		t.Errorf("disallow acme/app left it:\n%s", out)
	}
}

// repoPath escapes each part, so no character of a valid name changes the
// route, and refuses what is not owner/repo.
func TestRepoPath(t *testing.T) {
	if got, err := repoPath("acme/app.js"); err != nil || got != "/v0/runners/repos/acme/app.js" {
		t.Errorf("repoPath(acme/app.js) = %q, %v", got, err)
	}
	for _, bad := range []string{"acme/app?x", "acme/app#x", "acme/app/x", "acme/%2e", "", "acme", "a b/c"} {
		if got, err := repoPath(bad); err == nil {
			t.Errorf("repoPath(%q) = %q", bad, got)
		}
	}
}

// runners ls shows what config.json holds that does nothing any more.
func TestPrintSettingsShowsNotices(t *testing.T) {
	var out strings.Builder
	a := programApp(Program{}, nil, &out, io.Discard)
	a.printSettings(&runners.Setup{JobTTLS: 3600, Notices: []string{"allow_public_repos in config.json does nothing any more"}})
	if !strings.Contains(out.String(), "notice      allow_public_repos in config.json does nothing any more") {
		t.Errorf("settings:\n%s", out.String())
	}
	out.Reset()
	a.printSettings(&runners.Setup{JobTTLS: 3600})
	if strings.Contains(out.String(), "notice") {
		t.Errorf("settings with no notice:\n%s", out.String())
	}
}

// potluck runners ls says a lent machine that is away is waiting, with the
// driver's reason, rather than that nothing is lent.
func TestPrintMachines(t *testing.T) {
	var out strings.Builder
	a := programApp(Program{}, nil, &out, io.Discard)
	st := &runners.Setup{Pool: "home"}
	a.printMachines(st, &runners.PoolStatus{
		RunsOn:  `runs-on: ["self-hosted","macos","home:c4m16"]`,
		Waiting: []string{`mac-c4m16: busy or away: "reclaimed: the owner is active"`},
		Unknown: []string{"dev-c1m2: describe failed"},
		Refused: []string{"old-c1m2: serves no GitHub runner"},
	})
	for _, want := range []string{
		`machines    runs-on: ["self-hosted","macos","home:c4m16"]`,
		`  waiting   mac-c4m16: busy or away: "reclaimed: the owner is active"`,
		"  unknown   dev-c1m2: describe failed",
		"  refused   old-c1m2: serves no GitHub runner",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("lacks %q:\n%s", want, out.String())
		}
	}
	out.Reset()
	a.printMachines(st, &runners.PoolStatus{Refused: []string{"x: serves no GitHub runner"}})
	if got := out.String(); !strings.HasPrefix(got, "machines    none can serve now\n") || strings.Contains(got, "pool host") {
		t.Errorf("only refused machines:\n%s", got)
	}
	out.Reset()
	a.printMachines(st, &runners.PoolStatus{NotReady: "pool label and effective job_ttl_s must be set"})
	if got := out.String(); !strings.Contains(got, "not judged: pool label") {
		t.Errorf("not ready:\n%s", got)
	}
}

// The repository list says what keeps a connected repository from being
// served, in the host's words, and what the upgrade left for its owner.
func TestPrintReposSaysWhatIsNotServed(t *testing.T) {
	var out strings.Builder
	a := newApp(Host{Prefix: "supper pool", Client: func() (*client.Client, *config.Config, error) { return nil, nil, errors.New("unused") },
		Stdout: &out, Stderr: io.Discard})
	a.printRepos([]runners.Repo{
		{Repo: "acme/open", Visibility: "public", By: "upgrade"},
		{Repo: "acme/yes", Visibility: "public", Public: true, By: "judy", Warning: "outside contributors' runs start without approval"},
		{Repo: "acme/unread", By: "upgrade"},
		{Repo: "acme/shut", Visibility: "private", By: "judy", Warning: "w"},
		{Repo: "corp/inner", Visibility: "internal", By: "upgrade"},
	})
	got := out.String()
	for _, want := range []string{
		"not served until supper pool runners allow acme/open --public",
		"outside contributors' runs start without approval",
		"visibility not read yet: if public or internal, not served until supper pool runners allow acme/unread --public",
		"not served until supper pool runners allow corp/inner --public",
		`"upgrade" connected only repositories`, "any other repository you serve: supper pool runners allow <owner/repo>",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "acme/yes --public") || strings.Contains(got, "acme/shut --public") {
		t.Errorf("a repository with its yes is said to need one:\n%s", got)
	}
	if w := a.repoWarning(runners.Repo{Repo: "acme/both", Visibility: "public", Warning: "w"}); w != "not served until supper pool runners allow acme/both --public; w" {
		t.Errorf("both warnings: %q", w)
	}
	out.Reset()
	a.printRepos([]runners.Repo{{Repo: "acme/a", Visibility: "private", By: "judy"}})
	if strings.Contains(out.String(), "upgrade") {
		t.Errorf("no repository from the upgrade, yet:\n%s", out.String())
	}
}

// runners ls says how the broker learns of jobs and how its last round of
// asking GitHub went.
func TestPrintCheck(t *testing.T) {
	var out strings.Builder
	a := programApp(Program{}, nil, &out, io.Discard)
	a.printCheck(nil)
	if out.Len() != 0 {
		t.Fatalf("no App yet: %q", out.String())
	}
	a.printCheck(&runners.Checking{Mode: "check", EveryS: 60, Error: "boom", ErrorAt: "t1",
		Installations: []runners.CheckedInstallation{{Installation: 7, Repos: 3, LastOK: "t0", Error: "rate", ErrorAt: "t2", Cut: "too many", CutAt: "t3"}}})
	for _, want := range []string{
		"check       asking GitHub every 60 s (no public url: no webhook arrives)",
		"could not check at t1: boom",
		"installation 7: 3 repositories, last complete round t0",
		"errors since (t2): rate", "cut short (t3): too many",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("lacks %q:\n%s", want, out.String())
		}
	}
	out.Reset()
	a.printCheck(&runners.Checking{Mode: "webhook", EveryS: 900})
	if !strings.Contains(out.String(), "GitHub's webhooks serve; a catch-up check every 900 s") {
		t.Errorf("webhook mode:\n%s", out.String())
	}
}

// remoteBroker is a fake daemon over HTTP: it answers the runner routes a
// remote caller may read, refuses GET /v0/runners/setup as the owner-only
// route does away from the socket, and answers POST /v0/runners/setup (the
// hook's tests) unless the body names the pool "refused".
func remoteBroker(t *testing.T) (*httptest.Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var asked []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		asked = append(asked, r.Method+" "+r.URL.Path)
		mu.Unlock()
		w.Header().Set(api.HeaderProtocol, api.Protocol)
		switch r.URL.Path {
		case "/v0/runners/links":
			_, _ = io.WriteString(w, `{"links":[{"installation_id":7,"account":"acme","account_type":"Organization"}]}`)
		case "/v0/runners/repos":
			_, _ = io.WriteString(w, `{"repos":[{"repo":"acme/app","visibility":"private","by":"judy"}]}`)
		case "/v0/runners/setup":
			if r.Method == http.MethodPost && !strings.Contains(readBody(r), `"refused"`) {
				_, _ = io.WriteString(w, `{"pool":"pool","public_url":"https://pool.example","listening":"127.0.0.1:4000","next":"none"}`)
				return
			}
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `{"error":{"code":"forbidden","message":"this works only for the machine's owner on its own socket"}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), asked...) }
}

// remoteHost is a host whose client reaches srv over HTTP.
func remoteHost(t *testing.T, srv *httptest.Server, out, errw *bytes.Buffer, asJSON bool) Host {
	t.Helper()
	home, err := os.MkdirTemp("", "pr")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	return Host{Prefix: "supper pool", JSON: func() bool { return asJSON }, Stdout: out, Stderr: errw,
		Client: func() (*client.Client, *config.Config, error) {
			cfg, err := config.LoadClientFrom(home)
			return client.NewRemote(srv.URL, srv.Client()), cfg, err
		}}
}

// runCmd runs one of h's commands (words under Potluck's root).
func runCmd(h Host, words ...string) error {
	root := &cobra.Command{Use: "supper", SilenceUsage: true, SilenceErrors: true}
	root.AddCommand(Commands(h)...)
	root.SetArgs(words)
	return root.Execute()
}

// Away from the daemon's own socket (a remote client, a sandbox) runners ls
// lists the links and repositories the caller may read, without asking for
// the owner's setup.
func TestRunnersLsAwayFromTheSocket(t *testing.T) {
	srv, asked := remoteBroker(t)
	var out, errw bytes.Buffer
	if err := runCmd(remoteHost(t, srv, &out, &errw, false), "runners", "ls"); err != nil {
		t.Fatalf("ls: %v\n%s%s", err, out.String(), errw.String())
	}
	for _, want := range []string{"7          acme (organization)", "repos", "acme/app"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("lacks %q:\n%s", want, out.String())
		}
	}
	for _, a := range asked() {
		if strings.HasSuffix(a, "/v0/runners/setup") {
			t.Errorf("asked for the owner's setup away from the socket: %v", asked())
		}
	}
	out.Reset()
	if err := runCmd(remoteHost(t, srv, &out, &errw, true), "runners", "ls"); err != nil {
		t.Fatal(err)
	}
	var got struct {
		Setup *runners.Setup `json:"setup"`
		Repos []runners.Repo `json:"repos"`
		Links []runners.Link `json:"links"`
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil || got.Setup != nil || len(got.Repos) != 1 || len(got.Links) != 1 {
		t.Errorf("--json: %v %s", err, out.String())
	}
}

// A host whose own proxy fronts the broker routes it after setup: its line
// follows setup's, its error fails the command, and --json carries it.
func TestRunnersSetupRunsTheHostsRoute(t *testing.T) {
	srv, _ := remoteBroker(t)
	var out, errw bytes.Buffer
	h := remoteHost(t, srv, &out, &errw, false)
	var gotHome string
	var gotSt runners.Setup
	h.RunnersRoute = func(_ context.Context, home string, st runners.Setup) (string, error) {
		gotHome, gotSt = home, st
		return "proxy now sends the broker's paths to " + st.Listening, nil
	}
	if err := runCmd(h, "runners", "setup", "pool"); err != nil {
		t.Fatalf("setup: %v\n%s%s", err, out.String(), errw.String())
	}
	if gotHome == "" || gotSt.Listening != "127.0.0.1:4000" {
		t.Errorf("hook got home %q setup %+v", gotHome, gotSt)
	}
	if o := out.String(); !strings.Contains(o, "supper: proxy now sends the broker's paths to 127.0.0.1:4000") || strings.Contains(o, "forward that URL") {
		t.Errorf("setup printed:\n%s", o)
	}
	out.Reset()
	h.JSON = func() bool { return true }
	if err := runCmd(h, "runners", "setup", "pool"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"route": "proxy now sends the broker's paths to 127.0.0.1:4000"`) {
		t.Errorf("--json:\n%s", out.String())
	}
	h.JSON = func() bool { return false }
	h.RunnersRoute = func(context.Context, string, runners.Setup) (string, error) {
		return "", errors.New("a quick tunnel cannot route paths")
	}
	for _, asJSON := range []bool{false, true} {
		h.JSON = func() bool { return asJSON }
		if err := runCmd(h, "runners", "setup", "pool"); err == nil || !strings.Contains(err.Error(), "routing GitHub's calls to the broker (the setup is saved") ||
			!strings.Contains(err.Error(), "a quick tunnel cannot route paths") {
			t.Errorf("a failed route (json %v): %v", asJSON, err)
		}
	}
	// A setup the daemon refuses routes nothing.
	called := false
	h.RunnersRoute = func(context.Context, string, runners.Setup) (string, error) { called = true; return "", nil }
	if err := runCmd(h, "runners", "setup", "refused"); err == nil || called {
		t.Errorf("a refused setup: %v, route called %v", err, called)
	}
}

func readBody(r *http.Request) string {
	b, _ := io.ReadAll(r.Body)
	return string(b)
}

// A token narrows even on the socket: runners ls then does not ask for the
// owner's setup, which would be refused.
func TestRunnersListWithATokenOnTheSocket(t *testing.T) {
	srv, asked := remoteBroker(t)
	cl := client.NewRemote(srv.URL, srv.Client())
	cl.Socket, cl.Token = "/nowhere.sock", "zmt_x" // as a socket client with a token: only these fields are read
	if st, links, err := runnersList(context.Background(), cl); err != nil || st != nil || len(links) != 1 {
		t.Fatalf("setup %+v links %v err %v", st, links, err)
	}
	for _, a := range asked() {
		if strings.HasSuffix(a, "/v0/runners/setup") {
			t.Errorf("asked for the owner's setup with a token: %v", asked())
		}
	}
}

// runners capacity publishes stdin as the host's report; the broker then
// serves it without a token.
func TestRunnersCapacityPublishes(t *testing.T) {
	m := startMachine(t, tailnettest.New(), "mini", "judy@example.com")
	if _, err := client.NewUnix(m.dm.Deps.Config.SocketPath()).CreateProject(context.Background(), api.NewProjectRequest{Slug: "pool"}); err != nil {
		t.Fatal(err)
	}
	m.ok(t, "runners", "setup", "pool", "--url", "https://pool.example")
	if _, e, code := m.runIn(t, "not json", "runners", "capacity", "mini"); code == 0 || !strings.Contains(e, "not JSON") {
		t.Fatalf("a report that is not JSON: exit %d, %s", code, e)
	}
	o, e, code := m.runIn(t, `{"schema": "example-capacity-v1", "ready": true}`, "runners", "capacity", "mini", "--ttl-s", "30")
	if code != 0 || !strings.Contains(o+e, "published until") {
		t.Fatalf("publish: exit %d\n%s%s", code, o, e)
	}
	var got map[string]any
	if err := client.WithHandler(m.dm.Handler).Do(context.Background(), "GET", "/v0/runners/capacity/mini", nil, &got); err != nil || got["schema"] != "example-capacity-v1" {
		t.Fatalf("served: %v %v", got, err)
	}
}
