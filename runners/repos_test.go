package runners_test

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/store"
	"github.com/sengirunners/potluck/runners"
)

// allow is potluck runners allow on the machine's socket, as the pool's owner.
func (f *fixture) allow(repo string, public bool) (runners.Repo, error) {
	var rp runners.Repo
	return rp, f.owner.Do(context.Background(), "POST", "/v0/runners/repos", runners.AllowRequest{Repo: repo, Public: public}, &rp)
}

// repos is GET /v0/runners/repos by name.
func (f *fixture) repos(t *testing.T) map[string]runners.Repo {
	t.Helper()
	var out struct {
		Repos []runners.Repo `json:"repos"`
	}
	if err := f.owner.Do(context.Background(), "GET", "/v0/runners/repos", nil, &out); err != nil {
		t.Fatal(err)
	}
	m := map[string]runners.Repo{}
	for _, r := range out.Repos {
		m[r.Repo] = r
	}
	return m
}

// The matrix: a private and a public repository (both connected, the public
// one with its owner's yes), each run from the repository's own branch or a
// fork, of each event, with allow_fork_prs off and on. A public repository
// serves its own branches' trusted events only, whatever allow_fork_prs
// says, and every refusal says why in runners jobs.
func TestServedMatrix(t *testing.T) {
	trusted := []string{"push", "pull_request", "workflow_dispatch", "schedule", "merge_group", "release", "repository_dispatch"}
	untrusted := []string{"pull_request_target", "workflow_run", "issue_comment"}
	for _, forkPRs := range []bool{false, true} {
		t.Run(fmt.Sprintf("allow_fork_prs=%v", forkPRs), func(t *testing.T) {
			f := newFixtureWith(t, func(c *config.Runners) { c.AllowForkPRs = forkPRs })
			f.gh.mu.Lock()
			f.gh.publicRepo["acme/pub"] = true
			f.gh.mu.Unlock()
			if w := f.link(t, inst, "good"); w.Code != http.StatusOK {
				t.Fatalf("link: %d %s", w.Code, w.Body)
			}
			if rp, err := f.allow("acme/pub", true); err != nil || !rp.Public || rp.Visibility != "public" {
				t.Fatalf("allow acme/pub --public: %+v, %v", rp, err)
			}
			type cas struct {
				repo   string
				public bool
				fork   bool
				event  string
				served bool
				why    string
				job    int64
			}
			var cases []cas
			n := int64(0)
			for _, repo := range []string{"acme/pub", "acme/app"} {
				public := repo == "acme/pub"
				for _, fork := range []bool{false, true} {
					for _, ev := range append(append([]string{}, trusted...), untrusted...) {
						c := cas{repo: repo, public: public, fork: fork, event: ev}
						switch {
						case forkPRs && !public:
							c.served = true
						case fork:
							c.why = "a run from a fork"
						case !slices.Contains(trusted, ev):
							c.why = "a " + ev + " run"
						default:
							c.served = true
						}
						if !c.served {
							c.why += map[bool]string{true: ": never served for a public repository", false: ": this pool serves only its repositories' own branches"}[public]
						}
						n++
						c.job = 6000 + n
						cases = append(cases, c)
					}
				}
			}
			f.gh.mu.Lock()
			for _, c := range cases {
				f.gh.events[c.job] = c.event // run id = job id
				f.gh.forks[c.job] = c.fork
			}
			f.gh.mu.Unlock()
			// The public repository's push first, alone: its box is guarded
			// with the trusted events whatever allow_fork_prs says.
			deliver := func(c cas) {
				t.Helper()
				if code := f.deliver(t, "workflow_job", jobEvent("queued", c.job, c.job, c.repo, !c.public, "self-hosted", "sengi")); code != http.StatusAccepted {
					t.Fatalf("delivery %d", code)
				}
			}
			deliver(cases[0])
			guard := `"jit_guard":{"repo":"acme/pub","events":["push","pull_request","workflow_dispatch","schedule","merge_group","release","repository_dispatch"]}`
			eventually(t, "the public repository's box guarded", func() bool {
				b, _ := os.ReadFile(f.seen)
				return strings.Contains(string(b), guard)
			})
			for _, c := range cases[1:] {
				deliver(c)
			}
			// Settled: a refusal ignored, a served job past its run's checks
			// (serving, or waiting for the one box).
			settled := func(c cas, j runners.JobView) bool {
				if !c.served {
					return j.State == "ignored"
				}
				return j.State == "serving" || j.State == "waiting" && j.Why != "being checked with GitHub"
			}
			var js map[int64]runners.JobView
			eventually(t, "every job settled", func() bool {
				js = f.jobs(t)
				for _, c := range cases {
					if j, ok := js[c.job]; !ok || (!c.served && j.State != "ignored") || (c.served && j.State == "ignored") || !settled(c, j) {
						return false
					}
				}
				return true
			})
			for _, c := range cases {
				j := js[c.job]
				name := fmt.Sprintf("%s fork=%v %s", c.repo, c.fork, c.event)
				switch {
				case c.served && j.State == "ignored":
					t.Errorf("%s: refused (%s), want served", name, j.Why)
				case !c.served && j.State != "ignored":
					t.Errorf("%s: %s, want refused", name, j.State)
				case !c.served && !strings.Contains(j.Why, c.why):
					t.Errorf("%s: why %q, want %q", name, j.Why, c.why)
				}
			}
		})
	}
}

// Only a connected repository is served. A private one of a linked
// installation that is not connected is refused, saying the command that
// connects it; a public one needs its owner's yes, also when it was made
// public after it was connected; disconnecting (or unlinking) stops the next
// job. No config is edited for any of it.
func TestOnlyConnectedRepositoriesAreServed(t *testing.T) {
	f := pinned(t, "a", "b", "c")
	refused := func(job int64, repo string, private bool, want string) {
		t.Helper()
		f.deliver(t, "workflow_job", jobEvent("queued", job, job, repo, private, "pool:sa"))
		eventually(t, fmt.Sprintf("job %d refused (%s)", job, want), func() bool {
			j := f.jobs(t)[job]
			return j.State == "ignored" && strings.Contains(j.Why, want)
		})
	}
	served := func(job int64, repo string, private bool, box string) {
		t.Helper()
		f.deliver(t, "workflow_job", jobEvent("queued", job, job, repo, private, "pool:s"+box))
		eventually(t, fmt.Sprintf("job %d served on %s", job, box), func() bool {
			return f.jobs(t)[job].State == "serving" && f.liveOn(t, box) == 1
		})
	}
	f.gh.mu.Lock()
	for _, id := range []int64{7101, 7102, 7103, 7104, 7105, 7106, 7107, 7108} {
		f.gh.events[id] = "push"
	}
	f.gh.mu.Unlock()

	// A private repository of the installation, not connected.
	refused(7101, "acme/new", true, "acme/new is not connected to this pool (potluck runners allow acme/new)")
	if rp, err := f.allow("acme/new", false); err != nil || rp.Public || rp.Visibility != "private" || rp.By != "human:judy" {
		t.Fatalf("allow acme/new: %+v, %v", rp, err)
	}
	served(7102, "acme/new", true, "a")

	// A public repository: not connected without the yes, and its jobs say so.
	refused(7103, "acme/site", false, "potluck runners allow acme/site --public")
	if _, err := f.allow("acme/site", false); !api.IsCode(err, api.CodeBadRequest) || !strings.Contains(err.Error(), "public") {
		t.Fatalf("allow a public repository without --public: %v", err)
	}
	// The yes is for a public repository: never given ahead for a private one.
	if _, err := f.allow("acme/lib", true); !api.IsCode(err, api.CodeBadRequest) ||
		!strings.Contains(err.Error(), "acme/lib is private: --public is a yes given once it is public, not before") {
		t.Fatalf("allow a private repository with --public: %v", err)
	}
	if rp := f.repos(t)["acme/lib"]; rp.Public {
		t.Fatalf("a refused --public stored a yes: %+v", rp)
	}
	if _, ok := f.repos(t)["acme/site"]; ok {
		t.Fatal("a refused allow left a record")
	}

	// Made public after it was connected: refused until the yes.
	f.gh.mu.Lock()
	f.gh.publicRepo["acme/new"] = true
	f.gh.mu.Unlock()
	refused(7104, "acme/new", false, "a public repository its owner has not switched on: its own branches are served after potluck runners allow acme/new --public")
	if rp, err := f.allow("acme/new", true); err != nil || !rp.Public || rp.Visibility != "public" {
		t.Fatalf("allow acme/new --public: %+v, %v", rp, err)
	}
	served(7105, "acme/new", false, "b")

	// The yes stays on the record when the repository is made private and
	// public again: its next job is served without a new yes.
	f.gh.mu.Lock()
	f.gh.publicRepo["acme/new"] = false
	f.gh.mu.Unlock()
	f.svc.RepoCheckEvery = 0
	f.svc.Sweep(context.Background())
	if rp := f.repos(t)["acme/new"]; rp.Visibility != "private" || !rp.Public {
		t.Fatalf("acme/new after it was made private: %+v", rp)
	}
	f.gh.mu.Lock()
	f.gh.publicRepo["acme/new"] = true
	f.gh.mu.Unlock()
	served(7107, "acme/new", false, "c")

	// Neither another account's repository nor a bad name is connected.
	if _, err := f.allow("bob/tool", false); !api.IsCode(err, api.CodeNotFound) {
		t.Errorf("allow a repository the App is not installed on: %v", err)
	}
	if _, err := f.allow("acme", false); !api.IsCode(err, api.CodeBadRequest) {
		t.Errorf("allow acme: %v", err)
	}

	// Disconnected: the next job is refused, the one running keeps its box.
	if err := f.owner.Do(context.Background(), "DELETE", "/v0/runners/repos/acme/new", nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.repos(t)["acme/new"]; ok {
		t.Fatal("disallow left acme/new listed")
	}
	refused(7106, "acme/new", false, "acme/new is not connected")
	if j := f.jobs(t)[7105]; j.State != "serving" {
		t.Errorf("a running job of the disconnected repository: %+v", j)
	}
	if err := f.owner.Do(context.Background(), "DELETE", "/v0/runners/repos/acme/new", nil, nil); !api.IsCode(err, api.CodeNotFound) {
		t.Errorf("disallow twice: %v", err)
	}

	// Unlinking the installation disconnects its repositories.
	if len(f.repos(t)) == 0 {
		t.Fatal("no repositories before the unlink")
	}
	if err := f.owner.Do(context.Background(), "DELETE", fmt.Sprintf("/v0/runners/links/%d", inst), nil, nil); err != nil {
		t.Fatal(err)
	}
	if w := f.link(t, inst, "good"); w.Code != http.StatusOK { // links again; the helper connects acme/app, acme/lib, acme/other
		t.Fatalf("relink: %d", w.Code)
	}
	if _, ok := f.repos(t)["acme/new"]; ok {
		t.Error("acme/new still connected after its installation was unlinked")
	}

	// The old global switch is gone: potluck runners set says where it went.
	if _, err := f.set("allow_public_repos", "on"); err == nil || !strings.Contains(err.Error(), "potluck runners allow <owner/repo> --public") {
		t.Errorf("set allow_public_repos: %v", err)
	}
}

// A public repository's approval rule for outside contributors' runs is read
// when it is switched on and again each RepoCheckEvery; one weaker than "all
// outside contributors", or one GitHub does not answer, is said in plain
// words in its record. It never changes what is served.
func TestApprovalRuleIsReadAndWarned(t *testing.T) {
	f := pinned(t, "a")
	ctx := context.Background()
	f.gh.mu.Lock()
	f.gh.publicRepo["acme/pub"], f.gh.approval["acme/pub"] = true, "first_time_contributors"
	f.gh.mu.Unlock()
	rp, err := f.allow("acme/pub", true)
	if err != nil {
		t.Fatal(err)
	}
	if rp.Approval != "first_time_contributors" || !strings.Contains(rp.Warning, "without your approval") ||
		!strings.Contains(rp.Warning, "Require approval for all external contributors") || rp.Checked == "" {
		t.Fatalf("allow answered %+v", rp)
	}
	if got := f.repos(t)["acme/pub"]; got.Warning != rp.Warning {
		t.Errorf("listed %+v", got)
	}
	// A private repository has no such rule to warn about.
	if got := f.repos(t)["acme/app"]; got.Warning != "" || got.Approval != "" {
		t.Errorf("acme/app: %+v", got)
	}

	// Fixed on GitHub: not read again before RepoCheckEvery (an hour).
	f.gh.mu.Lock()
	f.gh.approval["acme/pub"] = "all_external_contributors"
	f.gh.mu.Unlock()
	f.svc.Sweep(ctx)
	if got := f.repos(t)["acme/pub"]; got.Warning == "" {
		t.Errorf("read again before it was due: %+v", got)
	}
	f.svc.RepoCheckEvery = 0 // due now
	f.svc.Sweep(ctx)
	if got := f.repos(t)["acme/pub"]; got.Warning != "" || got.Approval != "all_external_contributors" {
		t.Errorf("after the rule was fixed: %+v", got)
	}

	// Weaker still, then unreadable.
	f.gh.mu.Lock()
	f.gh.approval["acme/pub"] = "first_time_contributors_new_to_github"
	f.gh.mu.Unlock()
	f.svc.Sweep(ctx)
	if got := f.repos(t)["acme/pub"]; !strings.Contains(got.Warning, "unless their GitHub account is new") {
		t.Errorf("new to GitHub: %+v", got)
	}
	f.gh.mu.Lock()
	f.gh.approval["acme/pub"] = "error"
	f.gh.mu.Unlock()
	f.svc.Sweep(ctx)
	if got := f.repos(t)["acme/pub"]; got.Approval != "" || !strings.Contains(got.Warning, "GitHub did not say") {
		t.Errorf("unreadable: %+v", got)
	}

	// The warning does not stop its own branches' jobs.
	f.gh.mu.Lock()
	f.gh.events[7201] = "push"
	f.gh.mu.Unlock()
	f.deliver(t, "workflow_job", jobEvent("queued", 7201, 7201, "acme/pub", false, "pool:sa"))
	eventually(t, "the public repository's push served", func() bool {
		return f.jobs(t)[7201].State == "serving"
	})
	f.gh.mu.Lock()
	defer f.gh.mu.Unlock()
	if len(f.gh.misused) > 0 {
		t.Errorf("calls with the wrong token: %v", f.gh.misused)
	}
}

// A delivery names its repository as GitHub does, in any case: the
// connection is the same repository's.
func TestDeliveryNameCaseIsTheSameRepository(t *testing.T) {
	f := pinned(t, "a")
	f.gh.mu.Lock()
	f.gh.events[7401] = "push"
	f.gh.mu.Unlock()
	f.deliver(t, "workflow_job", jobEvent("queued", 7401, 7401, "ACME/App", true, "pool:sa"))
	eventually(t, "served", func() bool { return f.jobs(t)[7401].State == "serving" && f.liveOn(t, "a") == 1 })
}

// Who may change and read connections: connecting and disconnecting are the
// machine's owner's (the local socket), refused to any token, the pool
// project's owner's too; the list shows only the connections of links in the
// caller's readable areas.
func TestReposRoutesAuthority(t *testing.T) {
	f := pinned(t, "a")
	ctx := context.Background()
	if _, err := f.owner.AddArea(ctx, "pool", api.AddAreaRequest{Name: "b"}); err != nil {
		t.Fatal(err)
	}
	as := func(area, role string) *client.Client {
		tok, err := f.owner.MintToken(ctx, api.MintTokenRequest{As: "bo", Project: "pool", Area: area, Role: role})
		if err != nil {
			t.Fatal(err)
		}
		c := client.WithHandler(f.dm.Handler)
		c.Token, c.Project, c.ProjectFrom = tok.Secret, "pool", "flag"
		return c
	}
	list := func(c *client.Client) []string {
		t.Helper()
		var out struct {
			Repos []runners.Repo `json:"repos"`
		}
		if err := c.Do(ctx, "GET", "/v0/runners/repos", nil, &out); err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, r := range out.Repos {
			names = append(names, r.Repo)
		}
		return names
	}
	for _, role := range []string{"member", "owner"} {
		c := as("", role)
		if err := c.Do(ctx, "POST", "/v0/runners/repos", runners.AllowRequest{Repo: "acme/new"}, nil); !api.IsCode(err, api.CodeForbidden) {
			t.Errorf("a pool %s's token connecting: %v", role, err)
		}
		if err := c.Do(ctx, "DELETE", "/v0/runners/repos/acme/app", nil, nil); !api.IsCode(err, api.CodeForbidden) {
			t.Errorf("a pool %s's token disconnecting: %v", role, err)
		}
	}
	if _, ok := f.repos(t)["acme/app"]; !ok {
		t.Fatal("a refused disconnect took acme/app")
	}
	if got := list(as("", "member")); !slices.Contains(got, "acme/app") {
		t.Errorf("a pool member lists %v", got)
	}
	if got := list(as("b", "member")); len(got) != 0 {
		t.Errorf("a member of area b only lists the root area's connections: %v", got)
	}
	if err := client.WithHandler(f.dm.Handler).Do(ctx, "GET", "/v0/runners/repos", nil, nil); !api.IsCode(err, api.CodeUnauthenticated) {
		t.Errorf("anonymous list: %v", err)
	}
}

// The upgrade (runners/0008) connects each repository with a job the broker
// took through a linked installation, once whatever the case of its name,
// without a yes for public use and without an id; a repository whose jobs
// were all ignored, or whose installation is not linked, is not connected.
func TestUpgradeConnectsRepositoriesWithJobs(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	st, err := store.Open(ctx, home)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.AddMigrations(ctx, runners.Migrations...); err != nil {
		t.Fatal(err)
	}
	// The registry as it was before runners/0008, with jobs.
	for _, q := range []string{
		`DROP TABLE runners_repos`,
		`DELETE FROM schema_migrations WHERE name = 'runners/0008_repos'`,
		`INSERT INTO runners_links (installation, account, account_type, project, area, linked_by, token_id, created) VALUES (7, 'acme', 'Organization', 'p', '', 'human:judy', 't', 1)`,
		`INSERT INTO runners_jobs (job, installation, repo, run, labels, state, created, updated) VALUES
			(1, 7, 'acme/app', 1, '[]', 'done', 10, 10),
			(2, 7, 'ACME/App', 2, '[]', 'serving', 20, 20),
			(3, 7, 'acme/site', 3, '[]', 'done', 5, 5),
			(4, 7, 'acme/never', 4, '[]', 'ignored', 5, 5),
			(5, 9, 'bob/tool', 5, '[]', 'done', 5, 5)`,
	} {
		if _, err := st.Registry().ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	st.Close()
	if st, err = store.Open(ctx, home); err != nil {
		t.Fatal(err)
	}
	if err := st.AddMigrations(ctx, runners.Migrations...); err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	rows, err := st.Registry().QueryContext(ctx, `SELECT lower(repo), repo_id, installation, public, by, created FROM runners_repos ORDER BY lower(repo)`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var repo, by string
		var id, inst, created int64
		var public bool
		if err := rows.Scan(&repo, &id, &inst, &public, &by, &created); err != nil {
			t.Fatal(err)
		}
		got = append(got, fmt.Sprintf("%s id=%d inst=%d public=%v by=%s created=%d", repo, id, inst, public, by, created))
	}
	want := []string{"acme/app id=0 inst=7 public=false by=upgrade created=20", "acme/site id=0 inst=7 public=false by=upgrade created=5"}
	if !slices.Equal(got, want) {
		t.Errorf("connected by the upgrade:\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// A path that is not a repository's disconnects nothing: the owner and the
// name are escaped by the client, and what the daemon gets that is not
// owner/name is bad_request. acme/app stays connected.
func TestDisallowRefusesWhatIsNotARepository(t *testing.T) {
	f := pinned(t, "a")
	if _, ok := f.repos(t)["acme/app"]; !ok {
		t.Fatal("acme/app is not connected to begin with")
	}
	for _, bad := range []string{"acme/app%3Fx", "acme/app%23x", "acme/app%2Fx", "acme/app%20x", "acme/%2E%2E", "acme%2Fapp/x"} {
		err := f.owner.Do(context.Background(), "DELETE", "/v0/runners/repos/"+bad, nil, nil)
		if !api.IsCode(err, api.CodeBadRequest) && !api.IsCode(err, api.CodeNotFound) {
			t.Errorf("DELETE %s: %v", bad, err)
		}
		if _, ok := f.repos(t)["acme/app"]; !ok {
			t.Fatalf("DELETE %s disconnected acme/app", bad)
		}
	}
	err := f.owner.Do(context.Background(), "DELETE", "/v0/runners/repos/acme/app%3Fx", nil, nil)
	if !api.IsCode(err, api.CodeBadRequest) || !strings.Contains(err.Error(), "owner/name") {
		t.Errorf("DELETE acme/app?x: %v", err)
	}
}
