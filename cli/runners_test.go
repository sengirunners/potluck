package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/tailnet/tailnettest"
	"github.com/sengirunners/potluck/runners"
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
	m.ok(t, "runners", "setup", "pool", "--url", "https://pool.example")
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
	if out = m.ok(t, "runners", "repos"); !strings.Contains(out, "no repositories connected: potluck runners allow <owner/repo>") {
		t.Errorf("runners repos with none:\n%s", out)
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
	a := &app{stdout: &out}
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
