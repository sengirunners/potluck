package owners_test

import (
	"context"
	"fmt"
	"math/rand"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/owners"
	"github.com/sengirunners/potluck/kit/owners/githubfake"
)

// clock is the module's time, moved by tests.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

func (x *ox) clock() *clock {
	c := &clock{t: time.Now()}
	x.svc.Clock = c.now
	return c
}

// gh is a GitHub account as the owners brick sees it.
func acct(u githubfake.User, typ string) owners.Account {
	kind := owners.KindUser
	if typ == "Organization" {
		kind = owners.KindOrg
	}
	return owners.Account{ID: "github:" + strconv.FormatInt(u.ID, 10), Login: u.Login, Kind: kind}
}

// person is a full-scope subject (the socket's) for identity name, linked
// to the GitHub account u.
func (x *ox) person(t *testing.T, name string, u githubfake.User) *authz.Subject {
	t.Helper()
	sub := *x.dm.Deps.Projects.Local("")
	if name != "judy" {
		id, err := x.owner.AddIdentity(context.Background(), api.NewIdentityRequest{Name: name, Kind: api.IdentityHuman})
		if err != nil {
			t.Fatal(err)
		}
		sub.Identity, sub.Address = id, "human:"+name
	}
	if err := owners.LinkNow(x.svc, context.Background(), sub.Identity, acct(u, u.Type)); err != nil {
		t.Fatal(err)
	}
	return &sub
}

func (x *ox) id(t *testing.T, slug string) string {
	t.Helper()
	p, err := x.owner.GetProject(context.Background(), slug)
	if err != nil {
		t.Fatal(err)
	}
	return p.ID
}

// bind binds @owner/name to slug as sub, approved by holder (member: the
// live org membership answer).
func (x *ox) bind(t *testing.T, sub *authz.Subject, holder githubfake.User, slug, owner, name string, member bool) (api.NameBinding, error) {
	t.Helper()
	return owners.BindNow(x.svc, context.Background(), sub.Identity, x.id(t, slug), owner, name, acct(holder, holder.Type), member)
}

func (x *ox) resolve(t *testing.T, sub *authz.Subject, owner, name string) api.NameResolution {
	t.Helper()
	r, err := x.svc.Resolve(context.Background(), sub, owner, name)
	if err != nil {
		t.Fatalf("resolve @%s/%s: %v", owner, name, err)
	}
	return r
}

func TestBindRenameTransfer(t *testing.T) {
	x := newOX(t, "")
	x.projects(t, "orchid", "side")
	ana, _ := x.gh.AddUser("Ana", "User")
	me := x.person(t, "judy", ana)
	b, err := x.bind(t, me, ana, "orchid", "ana", "orchid", false)
	if err != nil || b.Path != "@Ana/orchid" || b.Project != x.id(t, "orchid") || !b.Current {
		t.Fatalf("bind = %+v %v", b, err)
	}
	if r := x.resolve(t, me, "ANA", "orchid"); r.Project != b.Project || r.Slug != "orchid" || r.Path != "@Ana/orchid" || r.Moved {
		t.Fatalf("resolve = %+v", r)
	}
	// A rename adds a binding; the old name answers moved.
	if _, err := x.bind(t, me, ana, "orchid", "ana", "or-chid", false); err != nil {
		t.Fatal(err)
	}
	if r := x.resolve(t, me, "ana", "orchid"); r.Project != b.Project || r.Path != "@Ana/or-chid" || !r.Moved {
		t.Fatalf("old name after a rename = %+v", r)
	}
	// Never moved to another project; renaming back is allowed.
	if _, err := x.bind(t, me, ana, "side", "ana", "orchid", false); !api.IsCode(err, api.CodeConflict) {
		t.Fatalf("rebinding a name: %v, want conflict", err)
	}
	if b2, err := x.bind(t, me, ana, "orchid", "ana", "orchid", false); err != nil || !b2.Bound.Equal(b.Bound) {
		t.Fatalf("rename back = %+v %v", b2, err)
	}
	// Orgs: only an active member (GitHub's live answer); a transfer leaves
	// the old path.
	acme, _ := x.gh.AddUser("acme", "Organization")
	_ = acme
	if _, err := x.bind(t, me, ana, "orchid", "acme", "orchid", false); !api.IsCode(err, api.CodeForbidden) {
		t.Fatalf("not a member: %v, want forbidden", err)
	}
	if _, err := x.bind(t, me, ana, "orchid", "acme", "orchid", true); err != nil {
		t.Fatal(err)
	}
	if r := x.resolve(t, me, "ana", "orchid"); r.Project != b.Project || r.Path != "@acme/orchid" || !r.Moved {
		t.Fatalf("after a transfer = %+v", r)
	}
	names, err := x.svc.Names(context.Background(), me, "orchid")
	if err != nil || len(names.Names) != 3 || names.Names[0].Path != "@acme/orchid" || !names.Names[0].Current || names.Names[1].Current {
		t.Fatalf("names = %+v %v", names, err)
	}
	// Refusals: another person's namespace, an approver not linked to the
	// binder, a bot owner, an unknown owner.
	bob, _ := x.gh.AddUser("bob", "User")
	x.gh.AddUser("dependabot", "Bot")
	for what, tc := range map[string]struct {
		holder      githubfake.User
		owner, code string
	}{
		"another's namespace": {ana, "bob", api.CodeForbidden},
		"not the linked one":  {bob, "bob", api.CodeForbidden},
		"bot owner":           {ana, "dependabot", api.CodeForbidden},
		"unknown owner":       {ana, "nobody-here", api.CodeNotFound},
	} {
		if _, err := x.bind(t, me, tc.holder, "side", tc.owner, "site", false); !api.IsCode(err, tc.code) {
			t.Errorf("%s: %v, want %s", what, err, tc.code)
		}
	}
}

// The bind login itself: names are checked before GitHub is asked, and the
// approver must be the binder's linked account.
func TestBindThroughLogin(t *testing.T) {
	x := newOX(t, "Iv1.fakeclient")
	ctx := context.Background()
	x.projects(t, "orchid")
	ana, _ := x.gh.AddUser("ana", "User")
	x.gh.Approver(ana.ID, 0)
	if _, _, err := login(t, x.owner); err != nil { // link judy to @ana
		t.Fatal(err)
	}
	for what, req := range map[string]api.GitHubLoginRequest{
		"reserved name": {Purpose: "bind", Project: "orchid", Owner: "ana", Name: "admin"},
		"no owner":      {Purpose: "bind", Project: "orchid", Name: "orchid"},
	} {
		if _, err := x.owner.StartGitHubLogin(ctx, req); !api.IsCode(err, api.CodeBadRequest) {
			t.Errorf("%s: %v, want bad_request", what, err)
		}
	}
	if _, err := x.owner.StartGitHubLogin(ctx, api.GitHubLoginRequest{Purpose: "bind", Project: "nope", Owner: "ana", Name: "x2"}); !api.IsCode(err, api.CodeNotFound) {
		t.Errorf("unknown project: %v", err)
	}
	run := func(approver int64) (api.GitHubLoginOutcome, error) {
		x.gh.Approver(approver, 0)
		st, err := x.owner.StartGitHubLogin(ctx, api.GitHubLoginRequest{Purpose: "bind", Project: "orchid", Owner: "@ana", Name: "orchid"})
		if err != nil {
			return api.GitHubLoginOutcome{}, err
		}
		if st.For != "identity judy to name a project @ana/orchid" {
			t.Errorf("who is asking: %q", st.For)
		}
		for {
			out, err := x.owner.PollGitHubLogin(ctx, st.ID, api.GitHubLoginPoll{Secret: st.Secret, WaitMS: 5000})
			if err != nil || !out.Pending {
				return out, err
			}
		}
	}
	bob, _ := x.gh.AddUser("bob", "User")
	if _, err := run(bob.ID); !api.IsCode(err, api.CodeForbidden) {
		t.Fatalf("approved by an account not linked to the binder: %v", err)
	}
	out, err := run(ana.ID)
	if err != nil || out.Bind == nil || out.Bind.Path != "@ana/orchid" {
		t.Fatalf("bind = %+v %v", out, err)
	}
	// Under an org: a member may not bind, an admin may (GitHub's live role).
	acme, _ := x.gh.AddUser("acme", "Organization")
	x.gh.AddMember(acme.ID, ana.ID, "active")
	org := func() error {
		x.gh.Approver(ana.ID, 0)
		st, err := x.owner.StartGitHubLogin(ctx, api.GitHubLoginRequest{Purpose: "bind", Project: "orchid", Owner: "acme", Name: "orchid"})
		if err != nil {
			return err
		}
		for {
			out, err := x.owner.PollGitHubLogin(ctx, st.ID, api.GitHubLoginPoll{Secret: st.Secret, WaitMS: 5000})
			if err != nil || !out.Pending {
				return err
			}
		}
	}
	if err := org(); !api.IsCode(err, api.CodeForbidden) || !strings.Contains(err.Error(), "admin") {
		t.Fatalf("an org member binding: %v, want forbidden", err)
	}
	x.gh.MakeAdmin(acme.ID, ana.ID)
	if err := org(); err != nil {
		t.Fatalf("an org admin binding: %v", err)
	}
	// A non-person identity never starts a bind (it could never collect).
	bot, _ := x.owner.AddIdentity(ctx, api.NewIdentityRequest{Name: "worker", Kind: api.IdentityAgent})
	asBot := *x.dm.Deps.Projects.Local("")
	asBot.Identity = bot
	if _, err := x.svc.StartLogin(ctx, &asBot, api.GitHubLoginRequest{Purpose: "bind", Project: "orchid", Owner: "ana", Name: "x2"}); !api.IsCode(err, api.CodeForbidden) {
		t.Fatalf("an agent's bind: %v", err)
	}
}

// Lookups for names cost GitHub calls: a login GitHub does not know is
// cached as missing, each caller has a budget, and with GitHub away an
// unseen login answers like a missing one (never an outage that tells).
func TestNameLookupBudget(t *testing.T) {
	x := newOX(t, "")
	ctx := context.Background()
	x.projects(t, "orchid")
	ana, _ := x.gh.AddUser("ana", "User")
	me := x.person(t, "judy", ana)
	if _, err := x.bind(t, me, ana, "orchid", "ana", "orchid", false); err != nil {
		t.Fatal(err)
	}
	before := x.gh.Lookups()
	for i := 0; i < 3; i++ {
		if _, err := x.svc.Resolve(ctx, me, "ghost-login", "orchid"); !api.IsCode(err, api.CodeNotFound) {
			t.Fatalf("an unknown login: %v", err)
		}
	}
	if n := x.gh.Lookups() - before; n != 1 {
		t.Fatalf("GitHub asked %d times for one unknown login, want 1 (cached as missing)", n)
	}
	x.svc.MaxLookups = 2
	mint, _ := x.owner.MintToken(ctx, api.MintTokenRequest{As: "carl", Project: "orchid", Role: "member"})
	c := client.WithHandler(x.dm.Handler)
	c.Token = mint.Secret
	var last error
	for i := 0; i < 3; i++ {
		_, last = c.ResolveName(ctx, "nobody-"+strconv.Itoa(i), "orchid")
	}
	if !api.IsCode(last, api.CodeRateLimited) {
		t.Fatalf("a caller's third uncached lookup this minute: %v, want rate_limited", last)
	}
	// GitHub away: a known (cached) owner still resolves; an unseen login
	// is missing, never unavailable.
	x.gh.Close()
	if r := x.resolve(t, me, "ana", "orchid"); r.Project != x.id(t, "orchid") {
		t.Fatalf("a cached owner with GitHub away: %+v", r)
	}
	if _, err := x.svc.Resolve(ctx, me, "never-seen", "orchid"); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("an unseen login with GitHub away: %v, want not_found", err)
	}
}

// A GitHub rename keeps every binding. When GitHub later gives the old
// login to another account, the original holder's old paths keep reaching
// its project (moved), and the newcomer can never bind them.
func TestRenamedOwnerKeepsOldPaths(t *testing.T) {
	x := newOX(t, "")
	clk := x.clock()
	x.projects(t, "orchid", "side")
	ana, _ := x.gh.AddUser("judy", "User")
	me := x.person(t, "judy", ana)
	if _, err := x.bind(t, me, ana, "orchid", "judy", "orchid", false); err != nil {
		t.Fatal(err)
	}
	orchid := x.id(t, "orchid")
	x.gh.Rename(ana.ID, "jd")
	clk.add(2 * time.Hour)
	if r := x.resolve(t, me, "judy", "orchid"); r.Project != orchid || r.Path != "@jd/orchid" || !r.Moved {
		t.Fatalf("old login after a GitHub rename = %+v", r)
	}
	// A newcomer takes "judy".
	mal, _ := x.gh.AddUser("judy", "User")
	them := x.person(t, "mallory", mal)
	clk.add(2 * time.Hour)
	if r := x.resolve(t, me, "judy", "orchid"); r.Project != orchid || r.Path != "@jd/orchid" || !r.Moved {
		t.Fatalf("@judy/orchid after GitHub gave the login to another account = %+v, want Judy's project", r)
	}
	if _, err := x.bind(t, them, mal, "side", "judy", "orchid", false); !api.IsCode(err, api.CodeConflict) {
		t.Fatalf("the newcomer binding an old path: %v, want conflict", err)
	}
	if _, err := x.bind(t, them, mal, "side", "judy", "fresh", false); err != nil {
		t.Fatal(err)
	}
	if r := x.resolve(t, me, "judy", "fresh"); r.Project != x.id(t, "side") {
		t.Fatalf("the newcomer's own name = %+v", r)
	}
}

// A project the caller cannot read answers exactly like a missing name.
func TestPrivateAnswersLikeMissing(t *testing.T) {
	x := newOX(t, "")
	ctx := context.Background()
	x.projects(t, "secret", "open")
	ana, _ := x.gh.AddUser("ana", "User")
	me := x.person(t, "judy", ana)
	for slug, name := range map[string]string{"secret": "merger", "open": "docs"} {
		if _, err := x.bind(t, me, ana, slug, "ana", name, false); err != nil {
			t.Fatal(err)
		}
	}
	mint, err := x.owner.MintToken(ctx, api.MintTokenRequest{As: "carl", Project: "open", Role: "member"})
	if err != nil {
		t.Fatal(err)
	}
	c := client.WithHandler(x.dm.Handler)
	c.Token = mint.Secret
	if _, err := c.ResolveName(ctx, "ana", "docs"); err != nil {
		t.Fatalf("a readable project: %v", err)
	}
	_, hidden := c.ResolveName(ctx, "ana", "merger")
	_, missing := c.ResolveName(ctx, "ana", "mergex")
	he, _ := hidden.(*api.Error)
	me2, _ := missing.(*api.Error)
	if he == nil || me2 == nil || he.Code != api.CodeNotFound || me2.Code != api.CodeNotFound ||
		he.Message != `no name "@ana/merger"` || me2.Message != `no name "@ana/mergex"` {
		t.Fatalf("hidden %v, missing %v: want the same not_found", hidden, missing)
	}
	if _, err := c.ProjectNames(ctx, "secret"); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("names of a hidden project: %v", err)
	}
}

func TestBindingsNeverRebound(t *testing.T) {
	x := newOX(t, "")
	x.projects(t, "orchid")
	ana, _ := x.gh.AddUser("ana", "User")
	me := x.person(t, "judy", ana)
	if _, err := x.bind(t, me, ana, "orchid", "ana", "orchid", false); err != nil {
		t.Fatal(err)
	}
	db := x.dm.Deps.Store.Registry()
	owner := "github:" + strconv.FormatInt(ana.ID, 10)
	for _, q := range []string{
		`UPDATE owner_names SET project = 'x'`, `UPDATE owner_names SET owner = 'x'`, `UPDATE owner_names SET name = 'x'`,
		`DELETE FROM owner_names`, `UPDATE login_paths SET owner = 'x'`, `DELETE FROM login_paths`,
		// REPLACE deletes the old row without the DELETE trigger: refused by
		// the INSERT triggers.
		`INSERT OR REPLACE INTO owner_names (owner, name, project, bound) VALUES ('` + owner + `', 'orchid', 'x', 0)`,
		`REPLACE INTO login_paths (login, name, owner) VALUES ('ana', 'orchid', 'x')`,
		`INSERT INTO owner_names (owner, name, project, bound) VALUES ('` + owner + `', 'orchid', 'x', 0) ON CONFLICT DO NOTHING`,
	} {
		if _, err := db.Exec(q); err == nil {
			t.Errorf("%s succeeded", q)
		}
	}
	if r := x.resolve(t, me, "ana", "orchid"); r.Project != x.id(t, "orchid") {
		t.Fatalf("after the attempts: %+v", r)
	}
}

// The never-rebind property: random binds, renames, transfers, GitHub
// renames, login reassignments, suspensions and cache expiry; every
// @login/name ever answered with a project keeps answering that project.
func TestNeverRebindProperty(t *testing.T) {
	x := newOX(t, "")
	clk := x.clock()
	x.svc.MaxLookups = 1 << 20
	seed := time.Now().UnixNano()
	rng := rand.New(rand.NewSource(seed))
	t.Logf("seed %d", seed)
	projects := []string{"p0", "p1", "p2"}
	x.projects(t, projects...)
	logins := []string{"l0", "l1", "l2", "l3", "l4"}
	names := []string{"aa", "bb", "cc"}
	type acc struct {
		u     githubfake.User
		login string
		sub   *authz.Subject
	}
	held := map[string]bool{}
	var accs []*acc
	newAcc := func(login string) {
		u, _ := x.gh.AddUser(login, "User")
		name := "judy"
		if len(accs) > 0 {
			name = fmt.Sprintf("u%d", len(accs))
		}
		accs = append(accs, &acc{u: u, login: login, sub: x.person(t, name, u)})
		held[login] = true
	}
	free := func() string {
		var out []string
		for _, l := range logins {
			if !held[l] {
				out = append(out, l)
			}
		}
		if len(out) == 0 {
			return ""
		}
		return out[rng.Intn(len(out))]
	}
	newAcc("l0")
	newAcc("l1")
	// An org (o0), its admin the first account; a caller who reads nothing
	// (its resolves must pin nothing and answer only missing).
	org, _ := x.gh.AddUser("o0", "Organization")
	held["o0"] = true
	stranger := &authz.Subject{Identity: api.Identity{ID: "01STRANGER", Name: "stranger", Kind: api.IdentityHuman}, Via: authz.ViaToken, Remote: true}
	logins = append(logins, "o0")
	first := map[string]string{} // @login/name -> the project first answered
	for step := 0; step < 150; step++ {
		switch op := rng.Intn(10); {
		case op < 5: // bind (a rename or a transfer when the project has names)
			a := accs[rng.Intn(len(accs))]
			u := a.u
			u.Login = a.login
			owner, admin := a.login, false
			if rng.Intn(3) == 0 { // under the org, as its admin or not
				owner, admin = "o0", a == accs[0]
			}
			_, _ = x.bind(t, a.sub, u, projects[rng.Intn(len(projects))], owner, names[rng.Intn(len(names))], admin)
			_ = org
		case op < 7: // a GitHub rename
			if l := free(); l != "" {
				a := accs[rng.Intn(len(accs))]
				held[a.login], held[l] = false, true
				x.gh.Rename(a.u.ID, l)
				a.login = l
			}
		case op < 8: // a new account, often under a released login
			if l := free(); l != "" && len(accs) < 6 {
				newAcc(l)
			}
		case op < 9: // GitHub suspends an account, or lifts it
			a := accs[rng.Intn(len(accs))]
			x.gh.Gone(a.u.ID, rng.Intn(2) == 0)
		default:
			clk.add(time.Duration(rng.Intn(3)) * time.Hour)
		}
		for _, l := range logins {
			for _, n := range names {
				key := "@" + l + "/" + n
				if rng.Intn(4) == 0 { // a caller who reads nothing: missing, and pins nothing
					if _, err := x.svc.Resolve(context.Background(), stranger, l, n); !api.IsCode(err, api.CodeNotFound) {
						t.Fatalf("seed %d step %d: %s for a stranger: %v", seed, step, key, err)
					}
				}
				r, err := x.svc.Resolve(context.Background(), accs[0].sub, l, n)
				if err == nil {
					// Path and Moved agree, and the current path reaches the
					// same project.
					if same := strings.EqualFold(r.Path, key); same == r.Moved {
						t.Fatalf("seed %d step %d: %s answered path %s moved=%v", seed, step, key, r.Path, r.Moved)
					}
					ol, on, _ := strings.Cut(strings.TrimPrefix(r.Path, "@"), "/")
					if cur, err := x.svc.Resolve(context.Background(), accs[0].sub, ol, on); err != nil || cur.Project != r.Project {
						t.Fatalf("seed %d step %d: %s's current path %s reaches %+v %v", seed, step, key, r.Path, cur, err)
					}
				}
				was, answered := first[key]
				switch {
				case api.IsCode(err, api.CodeNotFound) && !answered:
				case err != nil:
					t.Fatalf("seed %d step %d: %s answered %s before, now %v", seed, step, key, was, err)
				case !answered:
					first[key] = r.Project
				case was != r.Project:
					t.Fatalf("seed %d step %d: %s answered %s, now %s", seed, step, key, was, r.Project)
				}
			}
		}
	}
	if len(first) == 0 {
		t.Fatalf("seed %d: no name was ever answered", seed)
	}
}
