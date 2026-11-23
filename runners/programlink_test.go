package runners_test

import (
	"context"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/sengirunners/potluck/computeclient"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/runners"
	"github.com/sengirunners/potluck/wiring"
)

// program is a module of a program built on Potluck: it serves no route of
// its own and holds the broker its wiring found, as such a program's code
// compiled into the daemon would.
type program struct{ broker *runners.Service }

func (*program) Routes(*core.Router) {}

// programWiring is wiring.Compute plus prog, given the broker Compute made.
func programWiring(prog *program) func(d *core.Deps) ([]core.Module, error) {
	return func(d *core.Deps) ([]core.Module, error) {
		mods, err := wiring.Compute(d)
		for _, m := range mods {
			if s, ok := m.(*runners.Service); ok {
				prog.broker = s
			}
		}
		return append(mods, prog), err
	}
}

// brokerRoutes are the daemon's runner broker routes, as "METHOD /path".
func brokerRoutes(f *fixture) []string {
	var out []string
	for _, r := range f.dm.Router.Routes() {
		if _, path, _ := strings.Cut(r.Pattern, " "); strings.HasPrefix(path, "/v0/runners/") {
			out = append(out, r.Pattern)
		}
	}
	slices.Sort(out)
	return out
}

// An embedding program that verified an installation's account itself
// links it in-process: no GitHub sign-in, and no Members read on the App.
// Its job is then served in the area it was linked to. Another App's
// installation and one linked to another area are refused, and the daemon
// has no new route.
func TestProgramLinksAnInstallationItVerified(t *testing.T) {
	prog := &program{}
	f := newPoolFixtureWired(t, func(r *config.Runners) {
		r.Label, r.Pool.Sizes, r.Pool.DefaultSize = "sengi", map[string]string{"small": "small"}, "small"
	}, nil, programWiring(prog), box("small", "small", "linux", "1"))
	if prog.broker != f.svc || prog.broker == nil {
		t.Fatal("the program's wiring did not find the daemon's broker")
	}
	ctx := context.Background()
	// The App cannot read org members, and nobody is signed in: the page's
	// admin check could not pass.
	f.gh.mu.Lock()
	f.gh.orgRole, f.gh.userSee = "no-members-read", nil
	f.gh.mu.Unlock()
	if _, err := f.owner.AddArea(ctx, "pool", api.AddAreaRequest{Name: "b"}); err != nil {
		t.Fatal(err)
	}

	l, err := prog.broker.LinkVerified(ctx, inst, "b", "program:supper")
	if err != nil {
		t.Fatalf("link: %v", err)
	}
	if l.Installation != inst || l.Account != "acme" || l.AccountType != "Organization" || l.Area != "b" || l.LinkedBy != "program:supper" {
		t.Fatalf("link %+v", l)
	}
	f.gh.mu.Lock()
	for _, c := range f.gh.calls {
		if strings.Contains(c, "/login/oauth") || strings.Contains(c, " /user") {
			t.Errorf("a program's link called GitHub's sign-in: %s", c)
		}
	}
	f.gh.mu.Unlock()
	var links struct {
		Links []runners.Link `json:"links"`
	}
	if err := f.owner.Do(ctx, "GET", "/v0/runners/links", nil, &links); err != nil || len(links.Links) != 1 || links.Links[0] != l {
		t.Fatalf("links: %+v %v, want %+v", links, err, l)
	}
	// Linked again to the same area: the same link.
	if again, err := prog.broker.LinkVerified(ctx, inst, "B", "program:supper"); err != nil || again != l {
		t.Errorf("the same link again: %+v %v, want %+v", again, err, l)
	}

	// Its job is served, in area b, as the broker's identity.
	if _, err := f.allow("acme/app", false); err != nil {
		t.Fatal(err)
	}
	if code := f.deliver(t, "workflow_job", jobEvent("queued", 3001, 90, "acme/app", true, "self-hosted", "sengi")); code != 202 {
		t.Fatalf("delivery: %d", code)
	}
	eventuallyBox(t, "the job's box in area b", func() bool {
		hs, err := computeclient.RunTargetHandles(ctx, f.owner, "small", false)
		return err == nil && len(hs) == 1 && hs[0].State == "ready" && hs[0].Actor == "human:"+runners.Identity && hs[0].Area == "b"
	})
	if b, _ := os.ReadFile(f.seen); !strings.Contains(string(b), `"jit_config":"`+jitValue+`"`) {
		t.Errorf("the box was not given the job's runner: %s", b)
	}

	// Another App's installation is refused, and so is a move to another area.
	if _, err := prog.broker.LinkVerified(ctx, 9, "b", "program:supper"); !api.IsCode(err, api.CodeNotFound) {
		t.Errorf("another App's installation: %v, want not_found", err)
	}
	if _, err := prog.broker.LinkVerified(ctx, inst, "", "program:supper"); !api.IsCode(err, api.CodeConflict) {
		t.Errorf("an installation linked to another area: %v, want conflict", err)
	}

	// An area the pool project does not have is refused, and nothing is linked.
	if _, err := prog.broker.LinkVerified(ctx, 11, "nowhere", "program:supper"); !api.IsCode(err, api.CodeNotFound) {
		t.Errorf("an unknown area: %v, want not_found", err)
	}
	// A link names who verified it, and an installation id is positive.
	if _, err := prog.broker.LinkVerified(ctx, 11, "", " "); !api.IsCode(err, api.CodeBadRequest) {
		t.Errorf("a link naming nobody: %v, want bad_request", err)
	}
	if _, err := prog.broker.LinkVerified(ctx, 0, "", "program:supper"); !api.IsCode(err, api.CodeBadRequest) {
		t.Errorf("installation 0: %v, want bad_request", err)
	}
	if err := f.owner.Do(ctx, "GET", "/v0/runners/links", nil, &links); err != nil || len(links.Links) != 1 || links.Links[0].Area != "b" {
		t.Errorf("after the refusals: %+v %v", links, err)
	}

	// No route was added: these are the broker's routes, as before.
	want := []string{
		"DELETE /v0/runners/links/{installation}",
		"DELETE /v0/runners/repos/{owner}/{name}",
		"GET /v0/runners/app/created",
		"GET /v0/runners/app/installed",
		"GET /v0/runners/app/new",
		"GET /v0/runners/capacity/{host}",
		"GET /v0/runners/doctor",
		"GET /v0/runners/jobs",
		"GET /v0/runners/links",
		"GET /v0/runners/oauth",
		"GET /v0/runners/pool",
		"GET /v0/runners/repos",
		"GET /v0/runners/setup",
		"POST /v0/runners/app",
		"POST /v0/runners/hook",
		"POST /v0/runners/links",
		"POST /v0/runners/oauth",
		"POST /v0/runners/repos",
		"POST /v0/runners/settings",
		"POST /v0/runners/setup",
		"PUT /v0/runners/capacity/{host}",
	}
	if got := brokerRoutes(f); !slices.Equal(got, want) {
		t.Errorf("the broker's routes:\n got %q\nwant %q", got, want)
	}
}

// A program's link is refused for an account runners.accounts leaves out,
// as the page's is.
func TestProgramLinkKeepsTheAccountList(t *testing.T) {
	prog := &program{}
	f := newPoolFixtureWired(t, func(r *config.Runners) { r.Accounts = []string{"someone-else"} }, nil, programWiring(prog))
	if _, err := prog.broker.LinkVerified(context.Background(), inst, "", "program:supper"); !api.IsCode(err, api.CodeForbidden) {
		t.Errorf("an account not in runners.accounts: %v, want forbidden", err)
	}
	var links struct {
		Links []runners.Link `json:"links"`
	}
	if err := f.owner.Do(context.Background(), "GET", "/v0/runners/links", nil, &links); err != nil || len(links.Links) != 0 {
		t.Errorf("links: %+v %v", links, err)
	}
}

// An installation already linked to the area (here by the page, which keeps
// the area as typed) is answered as it is, whatever the area's case, but only
// while it is still this App's and its account still allowed.
func TestProgramLinkOfALinkedInstallation(t *testing.T) {
	prog := &program{}
	f := newPoolFixtureWired(t, func(*config.Runners) {}, nil, programWiring(prog))
	ctx := context.Background()
	if _, err := f.owner.AddArea(ctx, "pool", api.AddAreaRequest{Name: "b"}); err != nil {
		t.Fatal(err)
	}
	var start struct {
		URL string `json:"authorize_url"`
	}
	if err := f.owner.Do(ctx, "POST", "/v0/runners/links", map[string]any{"installation_id": inst, "area": "B"}, &start); err != nil {
		t.Fatal(err)
	}
	if w := f.linkAs(t, start.URL, "good"); w.Code != http.StatusOK {
		t.Fatalf("the page's link: %d %s", w.Code, w.Body)
	}
	var links struct {
		Links []runners.Link `json:"links"`
	}
	if err := f.owner.Do(ctx, "GET", "/v0/runners/links", nil, &links); err != nil || len(links.Links) != 1 {
		t.Fatalf("links: %+v %v", links, err)
	}
	page := links.Links[0]
	if l, err := prog.broker.LinkVerified(ctx, inst, "b", "program:supper"); err != nil || l != page {
		t.Fatalf("the program's link of the page's: %+v %v, want %+v", l, err, page)
	}

	// GitHub no longer has it as this App's installation.
	f.gh.mu.Lock()
	f.gh.extra = func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/app/installations/7" {
			return false
		}
		w.WriteHeader(http.StatusNotFound)
		return true
	}
	f.gh.mu.Unlock()
	if _, err := prog.broker.LinkVerified(ctx, inst, "b", "program:supper"); !api.IsCode(err, api.CodeNotFound) {
		t.Errorf("a linked installation the App no longer has: %v, want not_found", err)
	}
	f.gh.mu.Lock()
	f.gh.extra = nil
	f.gh.mu.Unlock()

	// Its account is no longer in runners.accounts.
	f.restart(t, func(c *config.Config) { c.File.Runners.Accounts = []string{"someone-else"} })
	if _, err := prog.broker.LinkVerified(ctx, inst, "b", "program:supper"); !api.IsCode(err, api.CodeForbidden) {
		t.Errorf("a linked installation of an account no longer allowed: %v, want forbidden", err)
	}
	if err := f.owner.Do(ctx, "GET", "/v0/runners/links", nil, &links); err != nil || len(links.Links) != 1 || links.Links[0] != page {
		t.Errorf("the link after the refusals: %+v %v, want %+v", links, err, page)
	}
}
